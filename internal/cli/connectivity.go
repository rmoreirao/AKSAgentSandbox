package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const connectivityPingInterval = 30 * time.Second

var errSandboxStopped = errors.New("sandbox stopped")

type remoteCommand struct {
	Executable string   `json:"executable"`
	Arguments  []string `json:"arguments,omitempty"`
}

type execRequest struct {
	Command remoteCommand `json:"command"`
	Detach  bool          `json:"detach,omitempty"`
}

type Job struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	ExitCode  *int       `json:"exitCode,omitempty"`
}

type terminalControl struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

type synchronizedWebSocket struct {
	connection *websocket.Conn
	mu         sync.Mutex
}

func (w *synchronizedWebSocket) message(kind int, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.connection.WriteMessage(kind, data)
}

func (w *synchronizedWebSocket) json(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return w.message(websocket.TextMessage, data)
}

func attachShell(ctx context.Context, client *Client, name string, initial []byte, in io.Reader, out, errOut io.Writer) error {
	connection, err := client.DialShell(ctx, name)
	if err != nil {
		return err
	}
	defer connection.Close()
	writer := &synchronizedWebSocket{connection: connection}
	stopClose := make(chan struct{})
	defer close(stopClose)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-stopClose:
		}
	}()

	restore := func() {}
	if file, ok := in.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		state, rawErr := term.MakeRaw(int(file.Fd()))
		if rawErr != nil {
			return cliError(ExitTransport, "terminal_setup_failed", rawErr.Error(), nil)
		}
		restore = func() { _ = term.Restore(int(file.Fd()), state) }
		lastCols, lastRows := 0, 0
		sendSize := func() {
			cols, rows, sizeErr := term.GetSize(int(file.Fd()))
			if sizeErr == nil && rows > 0 && cols > 0 && (rows != lastRows || cols != lastCols) {
				lastCols, lastRows = cols, rows
				_ = writer.json(terminalControl{Type: "resize", Rows: uint16(rows), Cols: uint16(cols)})
			}
		}
		sendSize()
		stopResize := watchTerminalResize(sendSize)
		defer stopResize()
	}
	defer restore()
	if len(initial) > 0 {
		if err := writer.message(websocket.BinaryMessage, initial); err != nil {
			return transportClosed()
		}
	}

	inputDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 32<<10)
		for {
			count, readErr := in.Read(buffer)
			if count > 0 {
				if writeErr := writer.message(websocket.BinaryMessage, buffer[:count]); writeErr != nil {
					inputDone <- writeErr
					return
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					_ = writer.json(terminalControl{Type: "eof"})
				}
				inputDone <- readErr
				return
			}
		}
	}()
	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		ticker := time.NewTicker(connectivityPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ticker.C:
				writer.mu.Lock()
				err := connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
				writer.mu.Unlock()
				if err != nil {
					_ = connection.Close()
					return
				}
			}
		}
	}()

	for {
		var event streamEvent
		if err := connection.ReadJSON(&event); err != nil {
			select {
			case <-ctx.Done():
				return RemoteExitError{Code: 130}
			default:
				return transportClosed()
			}
		}
		switch event.Type {
		case "stdout":
			if _, err := out.Write(event.Data); err != nil {
				return err
			}
		case "stderr":
			if _, err := errOut.Write(event.Data); err != nil {
				return err
			}
		case "error":
			return cliError(ExitTransport, "remote_execution_failed", event.Error, nil)
		case "exit":
			if event.ExitCode != nil && *event.ExitCode != 0 {
				return RemoteExitError{Code: *event.ExitCode}
			}
			return nil
		}
	}
}

func transportClosed() error {
	return cliError(ExitTransport, "remote_transport_failed", "sandbox connection closed before an exit code was available", nil)
}

func (a *application) shellCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "shell [name]",
		Short: "Open an interactive shell in a running sandbox",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := a.client(command.Context(), true)
			if err != nil {
				return err
			}
			name, err := a.resolveSandboxName(command.Context(), client, args)
			if err != nil {
				return err
			}
			return attachShell(command.Context(), client, name, nil, a.options.In, a.options.Out, a.options.Err)
		},
	}
}

func (a *application) execCommand() *cobra.Command {
	var detach bool
	command := &cobra.Command{
		Use:   "exec [name] -- command [arguments...]",
		Short: "Execute a command in a running sandbox",
		Args: func(command *cobra.Command, args []string) error {
			dash := command.ArgsLenAtDash()
			if dash < 0 || dash > 1 || len(args)-dash < 1 {
				return invalid("usage: devsandbox exec [NAME] [--detach] -- COMMAND [ARGUMENTS...]")
			}
			return nil
		},
		RunE: func(command *cobra.Command, args []string) error {
			client, err := a.client(command.Context(), true)
			if err != nil {
				return err
			}
			dash := command.ArgsLenAtDash()
			nameArgs := args[:dash]
			name, err := a.resolveSandboxName(command.Context(), client, nameArgs)
			if err != nil {
				return err
			}
			input := execRequest{
				Command: remoteCommand{Executable: args[dash], Arguments: append([]string(nil), args[dash+1:]...)},
				Detach:  detach,
			}
			if detach {
				job, err := client.StartJob(command.Context(), name, input)
				if err != nil {
					return err
				}
				if a.json {
					return writeJSON(a.options.Out, job)
				}
				_, _ = fmt.Fprintln(a.options.Out, job.ID)
				return nil
			}
			return client.Exec(command.Context(), name, input, a.options.Out, a.options.Err)
		},
	}
	command.Flags().BoolVarP(&detach, "detach", "d", false, "run as a managed detached job")
	return command
}

func (a *application) jobsCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "jobs [name]",
		Short: "List managed detached jobs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := a.client(command.Context(), true)
			if err != nil {
				return err
			}
			name, err := a.resolveSandboxName(command.Context(), client, args)
			if err != nil {
				return err
			}
			jobs, err := client.Jobs(command.Context(), name)
			if err != nil {
				return err
			}
			if a.json {
				return writeJSON(a.options.Out, map[string]any{"items": jobs})
			}
			writeJobs(a.options.Out, jobs)
			return nil
		},
	}
	command.AddCommand(&cobra.Command{
		Use:   "stop [name] job-id",
		Short: "Stop a managed detached job",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := a.client(command.Context(), true)
			if err != nil {
				return err
			}
			nameArgs := []string(nil)
			jobID := args[0]
			if len(args) == 2 {
				nameArgs = args[:1]
				jobID = args[1]
			}
			name, err := a.resolveSandboxName(command.Context(), client, nameArgs)
			if err != nil {
				return err
			}
			job, err := client.StopJob(command.Context(), name, jobID)
			if err != nil {
				return err
			}
			if a.json {
				return writeJSON(a.options.Out, job)
			}
			_, _ = fmt.Fprintf(a.options.Out, "%s %s\n", job.ID, job.State)
			return nil
		},
	})
	return command
}

func writeJobs(output io.Writer, jobs []Job) {
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "ID\tSTATE\tSTARTED\tENDED\tEXIT")
	for _, job := range jobs {
		ended, exit := "-", "-"
		if job.EndedAt != nil {
			ended = job.EndedAt.Format(time.RFC3339)
		}
		if job.ExitCode != nil {
			exit = strconv.Itoa(*job.ExitCode)
		}
		_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
			job.ID, job.State, job.StartedAt.Format(time.RFC3339), ended, exit)
	}
	_ = writer.Flush()
}

func (a *application) portCommand() *cobra.Command {
	var localPort int
	command := &cobra.Command{
		Use:   "port [name] remote-port",
		Short: "Forward a local TCP port to a running sandbox",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := a.client(command.Context(), true)
			if err != nil {
				return err
			}
			nameArgs := []string(nil)
			portValue := args[0]
			if len(args) == 2 {
				nameArgs = args[:1]
				portValue = args[1]
			}
			remotePort, err := parsePort(portValue)
			if err != nil {
				return err
			}
			if !command.Flags().Changed("local-port") {
				localPort = remotePort
			}
			if localPort < 0 || localPort > 65535 {
				return invalid("--local-port must be between 0 and 65535")
			}
			name, err := a.resolveSandboxName(command.Context(), client, nameArgs)
			if err != nil {
				return err
			}
			return a.serveTunnel(command.Context(), client, name, remotePort, localPort)
		},
	}
	command.Flags().IntVar(&localPort, "local-port", 0, "local TCP port (0 selects an available port)")
	return command
}

func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, invalid("REMOTE_PORT must be between 1 and 65535")
	}
	return port, nil
}

func (a *application) serveTunnel(ctx context.Context, client *Client, name string, remotePort, localPort int) error {
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))
	if err != nil {
		return cliError(ExitTransport, "local_port_unavailable", err.Error(), nil)
	}
	defer listener.Close()
	actualPort := listener.Addr().(*net.TCPAddr).Port
	if a.json {
		if err := writeJSON(a.options.Out, map[string]any{
			"name": name, "localAddress": listener.Addr().String(), "localPort": actualPort, "remotePort": remotePort,
		}); err != nil {
			return err
		}
	} else {
		_, _ = fmt.Fprintf(a.options.Out, "Forwarding 127.0.0.1:%d to %s:%d\n", actualPort, name, remotePort)
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	var wait sync.WaitGroup
	defer wait.Wait()
	stopped := make(chan error, 1)
	for {
		local, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			select {
			case tunnelErr := <-stopped:
				return cliError(ExitTransport, "sandbox_stopped", tunnelErr.Error(), nil)
			default:
			}
			return cliError(ExitTransport, "local_tunnel_failed", acceptErr.Error(), nil)
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			defer local.Close()
			if forwardErr := client.forwardConnection(ctx, name, remotePort, local); errors.Is(forwardErr, errSandboxStopped) {
				select {
				case stopped <- forwardErr:
					_ = listener.Close()
				default:
				}
			}
		}()
	}
}

func (c *Client) Exec(ctx context.Context, name string, input execRequest, stdout, stderr io.Writer) error {
	connection, err := c.dialWebSocket(ctx,
		"/v1/sandboxes/"+url.PathEscape(name)+"/exec/stream")
	if err != nil {
		return err
	}
	defer connection.Close()
	writer := &synchronizedWebSocket{connection: connection}
	if err := writer.json(input); err != nil {
		return transportClosed()
	}
	stopClose := make(chan struct{})
	defer close(stopClose)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-stopClose:
		}
	}()
	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		ticker := time.NewTicker(connectivityPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ticker.C:
				writer.mu.Lock()
				pingErr := connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
				writer.mu.Unlock()
				if pingErr != nil {
					_ = connection.Close()
					return
				}
			}
		}
	}()
	for {
		var event streamEvent
		if err := connection.ReadJSON(&event); err != nil {
			if ctx.Err() != nil {
				return RemoteExitError{Code: 130}
			}
			return transportClosed()
		}
		switch event.Type {
		case "stdout":
			if _, err := stdout.Write(event.Data); err != nil {
				return err
			}
		case "stderr":
			if _, err := stderr.Write(event.Data); err != nil {
				return err
			}
		case "error":
			return cliError(ExitTransport, "remote_execution_failed", event.Error, nil)
		case "exit":
			if event.Canceled && ctx.Err() != nil {
				return RemoteExitError{Code: 130}
			}
			if event.ExitCode != nil && *event.ExitCode != 0 {
				return RemoteExitError{Code: *event.ExitCode}
			}
			return nil
		}
	}
}

func (c *Client) StartJob(ctx context.Context, name string, input execRequest) (Job, error) {
	var job Job
	return job, c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(name)+"/exec", input, &job, true)
}

func (c *Client) Jobs(ctx context.Context, name string) ([]Job, error) {
	var jobs []Job
	return jobs, c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(name)+"/jobs", nil, &jobs, true)
}

func (c *Client) StopJob(ctx context.Context, name, jobID string) (Job, error) {
	var job Job
	return job, c.do(ctx, http.MethodDelete,
		"/v1/sandboxes/"+url.PathEscape(name)+"/jobs/"+url.PathEscape(jobID), nil, &job, true)
}

func (c *Client) forwardConnection(ctx context.Context, name string, remotePort int, local net.Conn) error {
	path := "/v1/sandboxes/" + url.PathEscape(name) + "/ports/" + strconv.Itoa(remotePort)
	connection, err := c.dialWebSocket(ctx, path)
	if err != nil {
		return err
	}
	defer connection.Close()
	writer := &synchronizedWebSocket{connection: connection}
	done := make(chan error, 2)
	go func() {
		buffer := make([]byte, 32<<10)
		for {
			count, readErr := local.Read(buffer)
			if count > 0 {
				if writeErr := writer.message(websocket.BinaryMessage, buffer[:count]); writeErr != nil {
					done <- writeErr
					return
				}
			}
			if readErr != nil {
				done <- readErr
				return
			}
		}
	}()
	go func() {
		for {
			messageType, data, readErr := connection.ReadMessage()
			if readErr != nil {
				var closeErr *websocket.CloseError
				if errors.As(readErr, &closeErr) && closeErr.Code == websocket.CloseGoingAway {
					done <- errSandboxStopped
					return
				}
				done <- readErr
				return
			}
			if messageType == websocket.BinaryMessage {
				if writeErr := writeAll(local, data); writeErr != nil {
					done <- writeErr
					return
				}

			}
		}
	}()
	ping := time.NewTicker(connectivityPingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			return err
		case <-ping.C:
			writer.mu.Lock()
			err := connection.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second))
			writer.mu.Unlock()
			if err != nil {
				return err
			}
		}
	}
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		count, err := writer.Write(data)
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
		data = data[count:]
	}
	return nil
}

func stringsTrimSuffixSlash(value string) string {
	for len(value) > 0 && value[len(value)-1] == '/' {
		value = value[:len(value)-1]
	}
	return value
}
