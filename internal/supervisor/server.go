package supervisor

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rmoreirao/AKSAgentSandbox/internal/observability"
)

const DefaultPingInterval = 30 * time.Second

type Authenticator interface {
	Authenticate(*http.Request) bool
}

type TokenFileAuthenticator struct {
	Path string
}

func (a TokenFileAuthenticator) Authenticate(request *http.Request) bool {
	value, err := os.ReadFile(a.Path)
	if err != nil {
		return false
	}
	expected := strings.TrimSpace(string(value))
	provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	if expected == "" || provided == request.Header.Get("Authorization") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}

type StaticTokenAuthenticator string

func (a StaticTokenAuthenticator) Authenticate(request *http.Request) bool {
	provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	if provided == request.Header.Get("Authorization") || len(a) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(provided)) == 1
}

// RouterAuthenticator accepts the sandbox identity that the trusted Sandbox
// Router adds after owner authorization.
type RouterAuthenticator string

func (a RouterAuthenticator) Authenticate(request *http.Request) bool {
	provided := request.Header.Get("X-Sandbox-Uid")
	return len(a) != 0 && request.Header.Get("X-Sandbox-Id") != "" &&
		subtle.ConstantTimeCompare([]byte(a), []byte(provided)) == 1
}

type AnyAuthenticator []Authenticator

func (a AnyAuthenticator) Authenticate(request *http.Request) bool {
	for _, authenticator := range a {
		if authenticator != nil && authenticator.Authenticate(request) {
			return true
		}
	}
	return false
}

type Server struct {
	Manager      *Manager
	Auth         Authenticator
	Logger       *slog.Logger
	PingInterval time.Duration
	OnShutdown   func()
	Audit        observability.AuditSink
	Metrics      *observability.Metrics
	upgrader     websocket.Upgrader
}

func NewServer(manager *Manager, auth Authenticator) (*Server, error) {
	if manager == nil {
		return nil, errors.New("manager is required")
	}
	if auth == nil {
		return nil, errors.New("authenticator is required")
	}
	return &Server{
		Manager:      manager,
		Auth:         auth,
		Logger:       slog.Default(),
		PingInterval: DefaultPingInterval,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(request *http.Request) bool {
				return request.Header.Get("Origin") == ""
			},
		},
	}, nil
}

func (s *Server) Handler() (http.Handler, error) {
	if s.PingInterval <= 0 || s.PingInterval > 60*time.Second {
		return nil, errors.New("WebSocket ping interval must be between zero and 60 seconds")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", s.authenticated(s.health))
	mux.HandleFunc("/v1/activity", s.authenticated(s.activity))
	mux.HandleFunc("/v1/exec", s.authenticated(s.session("exec", s.exec)))
	mux.HandleFunc("/v1/exec/stream", s.authenticated(s.session("exec", s.execStream)))
	mux.HandleFunc("/v1/shell", s.authenticated(s.session("shell", s.shell)))
	mux.HandleFunc("/v1/jobs", s.authenticated(s.jobs))
	mux.HandleFunc("/v1/jobs/", s.authenticated(s.job))
	mux.HandleFunc("/v1/tunnel", s.authenticated(s.session("tunnel", s.tunnel)))
	mux.HandleFunc("/v1/shutdown", s.authenticated(s.shutdown))
	return securityHeaders(mux), nil
}

func (s *Server) session(kind string, next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		s.recordSession("session.start", kind)
		if s.Metrics != nil {
			s.Metrics.ActiveConnections.WithLabelValues(kind).Inc()
			defer s.Metrics.ActiveConnections.WithLabelValues(kind).Dec()
		}
		defer s.recordSession("session.end", kind)
		next(writer, request)
	}
}

func (s *Server) recordSession(event, kind string) {
	if s.Audit != nil {
		s.Audit.Record(observability.AuditEvent{Event: event, Outcome: "success", Session: kind})
	}
}

func (s *Server) authenticated(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if !s.Auth.Authenticate(request) {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			writeError(writer, http.StatusUnauthorized, "authentication required")
			return
		}
		next(writer, request)
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(writer, request)
	})
}

func (s *Server) health(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) activity(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		writeJSON(writer, http.StatusOK, s.Manager.ActivityStatus())
	case http.MethodPost:
		s.Manager.Activity()
		writer.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
	}
}

type execRequest struct {
	Command Command `json:"command"`
	Detach  bool    `json:"detach,omitempty"`
}

type streamEvent struct {
	Type     string `json:"type"`
	Data     []byte `json:"data,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Canceled bool   `json:"canceled,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (s *Server) exec(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	var input execRequest
	if err := decodeJSON(request.Body, &input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid execution request")
		return
	}
	if input.Detach {
		if input.Command.Directory == "" {
			input.Command.Directory = defaultWorkingDirectory()
		}
		job, err := s.Manager.Start(input.Command)
		if err != nil {
			writeError(writer, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(writer, http.StatusAccepted, job)
		return
	}

	writer.Header().Set("Content-Type", "application/x-ndjson")
	writer.WriteHeader(http.StatusOK)
	flusher, _ := writer.(http.Flusher)
	stream := &eventStream{output: writer, flusher: flusher}
	if input.Command.Directory == "" {
		input.Command.Directory = defaultWorkingDirectory()
	}
	result, err := s.Manager.Run(
		request.Context(), input.Command, request.Body,
		stream.writer("stdout"), stream.writer("stderr"),
	)
	if err != nil {
		stream.write(streamEvent{Type: "error", Error: err.Error()})
		return
	}
	stream.write(streamEvent{
		Type: "exit", ExitCode: &result.ExitCode, Canceled: result.Canceled,
	})
}

func (s *Server) execStream(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	connection, err := s.upgrader.Upgrade(writer, request, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	connection.SetReadLimit(1 << 20)
	_ = connection.SetReadDeadline(time.Now().Add(3 * s.PingInterval))
	wsWriter := &websocketEventWriter{connection: connection, timeout: s.PingInterval}
	connection.SetPongHandler(func(string) error {
		s.Manager.Activity()
		return connection.SetReadDeadline(time.Now().Add(3 * s.PingInterval))
	})

	messageType, data, err := connection.ReadMessage()
	if err != nil || messageType != websocket.TextMessage {
		_ = wsWriter.write(streamEvent{Type: "error", Error: "invalid execution request"})
		return
	}
	var input execRequest
	if json.Unmarshal(data, &input) != nil || input.Detach {
		_ = wsWriter.write(streamEvent{Type: "error", Error: "invalid execution request"})
		return
	}
	if input.Command.Directory == "" {
		input.Command.Directory = defaultWorkingDirectory()
	}

	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	readDone := make(chan struct{})
	go s.readExecInput(connection, cancel, readDone)
	pingDone := make(chan struct{})
	go s.ping(wsWriter, pingDone, cancel)

	resultDone := make(chan ExitResult, 1)
	go func() {
		result, runErr := s.Manager.Run(
			ctx, input.Command, nil,
			wsWriter.writer("stdout"), wsWriter.writer("stderr"),
		)
		if runErr != nil {
			_ = wsWriter.write(streamEvent{Type: "error", Error: runErr.Error()})
			cancel()
		}
		resultDone <- result
	}()
	result := <-resultDone
	close(pingDone)
	_ = wsWriter.write(streamEvent{
		Type: "exit", ExitCode: &result.ExitCode, Canceled: result.Canceled,
	})
	cancel()
	select {
	case <-readDone:
	case <-time.After(100 * time.Millisecond):
	}
}

func (s *Server) readExecInput(connection *websocket.Conn, cancel context.CancelFunc, done chan<- struct{}) {
	defer close(done)
	for {
		messageType, data, err := connection.ReadMessage()
		if err != nil {
			cancel()
			return
		}
		_ = connection.SetReadDeadline(time.Now().Add(3 * s.PingInterval))
		switch messageType {
		case websocket.BinaryMessage:
		case websocket.TextMessage:
			var control terminalControl
			if json.Unmarshal(data, &control) == nil && control.Type == "eof" {
				return
			}
		}
		s.Manager.Activity()
	}
}

type eventStream struct {
	mu      sync.Mutex
	output  io.Writer
	flusher http.Flusher
}

func (s *eventStream) writer(kind string) io.Writer {
	return writerFunc(func(data []byte) (int, error) {
		copied := append([]byte(nil), data...)
		if err := s.write(streamEvent{Type: kind, Data: copied}); err != nil {
			return 0, err
		}
		return len(data), nil
	})
}

func (s *eventStream) write(event streamEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := json.NewEncoder(s.output).Encode(event); err != nil {
		return err
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return nil
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(data []byte) (int, error) { return f(data) }

func (s *Server) jobs(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	writeJSON(writer, http.StatusOK, s.Manager.Jobs())
}

func (s *Server) job(writer http.ResponseWriter, request *http.Request) {
	id := strings.TrimPrefix(request.URL.Path, "/v1/jobs/")
	if id == "" || strings.Contains(id, "/") {
		writeError(writer, http.StatusNotFound, "job not found")
		return
	}
	switch request.Method {
	case http.MethodGet:
		job, ok := s.Manager.Job(id)
		if !ok {
			writeError(writer, http.StatusNotFound, "job not found")
			return
		}
		writeJSON(writer, http.StatusOK, job)
	case http.MethodDelete:
		job, err := s.Manager.StopJob(request.Context(), id)
		if errors.Is(err, os.ErrNotExist) {
			writeError(writer, http.StatusNotFound, "job not found")
			return
		}
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "unable to stop job")
			return
		}
		writeJSON(writer, http.StatusOK, job)
	default:
		methodNotAllowed(writer, http.MethodGet+", "+http.MethodDelete)
	}
}

func (s *Server) shutdown(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	writer.WriteHeader(http.StatusAccepted)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = s.Manager.Shutdown(ctx)
		if s.OnShutdown != nil {
			s.OnShutdown()
		}
	}()
}

func (s *Server) shell(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	connection, err := s.upgrader.Upgrade(writer, request, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	connection.SetReadLimit(1 << 20)
	_ = connection.SetReadDeadline(time.Now().Add(3 * s.PingInterval))
	connection.SetPongHandler(func(string) error {
		s.Manager.Activity()
		return connection.SetReadDeadline(time.Now().Add(3 * s.PingInterval))
	})
	wsWriter := &websocketEventWriter{connection: connection, timeout: s.PingInterval}
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	terminal, err := s.Manager.StartTerminal(ctx, defaultShell(), TerminalSize{Rows: 24, Cols: 80})
	if err != nil {
		_ = wsWriter.write(streamEvent{Type: "error", Error: err.Error()})
		return
	}
	defer terminal.Close()
	readDone := make(chan struct{})
	go s.readTerminalInput(connection, terminal, cancel, readDone)
	pingDone := make(chan struct{})
	go s.ping(wsWriter, pingDone, cancel)
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(wsWriter.writer("stdout"), terminal.Output())
		copyDone <- copyErr
	}()
	result := terminal.Wait()
	close(pingDone)
	cancel()
	<-copyDone
	_ = wsWriter.write(streamEvent{
		Type: "exit", ExitCode: &result.ExitCode, Canceled: result.Canceled,
	})
	select {
	case <-readDone:
	case <-time.After(100 * time.Millisecond):
	}
}

type terminalControl struct {
	Type string `json:"type"`
	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
}

func (s *Server) readTerminalInput(connection *websocket.Conn, terminal *Terminal, cancel context.CancelFunc, done chan<- struct{}) {
	defer close(done)
	for {
		messageType, data, err := connection.ReadMessage()
		if err != nil {
			cancel()
			return
		}
		_ = connection.SetReadDeadline(time.Now().Add(3 * s.PingInterval))
		switch messageType {
		case websocket.BinaryMessage:
			if _, err := terminal.Input().Write(data); err != nil {
				cancel()
				return
			}
		case websocket.TextMessage:
			var control terminalControl
			if json.Unmarshal(data, &control) != nil {
				continue
			}
			switch control.Type {
			case "resize":
				if err := terminal.Resize(TerminalSize{Rows: control.Rows, Cols: control.Cols}); err != nil {
					continue
				}
			case "eof":
				if closer, ok := terminal.Input().(io.Closer); ok {
					_ = closer.Close()
				}
			}
		default:
			continue
		}
		s.Manager.Activity()
	}
}

type websocketEventWriter struct {
	mu         sync.Mutex
	connection *websocket.Conn
	timeout    time.Duration
}

func (w *websocketEventWriter) writer(kind string) io.Writer {
	return writerFunc(func(data []byte) (int, error) {
		event := streamEvent{Type: kind, Data: append([]byte(nil), data...)}
		if err := w.write(event); err != nil {
			return 0, err
		}
		return len(data), nil
	})
}

func (w *websocketEventWriter) write(event streamEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timeout > 0 {
		_ = w.connection.SetWriteDeadline(time.Now().Add(w.timeout))
	}
	return w.connection.WriteJSON(event)
}

func (w *websocketEventWriter) binary(data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timeout > 0 {
		_ = w.connection.SetWriteDeadline(time.Now().Add(w.timeout))
	}
	return w.connection.WriteMessage(websocket.BinaryMessage, data)
}

func (w *websocketEventWriter) control(kind int, data []byte, deadline time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.connection.WriteControl(kind, data, deadline)
}

func (s *Server) ping(writer *websocketEventWriter, done <-chan struct{}, cancel func()) {
	ticker := time.NewTicker(s.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			deadline := time.Now().Add(s.PingInterval)
			if err := writer.control(websocket.PingMessage, nil, deadline); err != nil {
				cancel()
				return
			}
			s.Manager.Activity()
		}
	}
}

func (s *Server) tunnel(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	port, err := strconv.Atoi(request.URL.Query().Get("port"))
	if err != nil || port < 1 || port > 65535 {
		writeError(writer, http.StatusBadRequest, "port must be between 1 and 65535")
		return
	}
	destination, err := (&net.Dialer{}).DialContext(
		request.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
	)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "local destination is unavailable")
		return
	}
	defer destination.Close()
	connection, err := s.upgrader.Upgrade(writer, request, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	connection.SetReadLimit(4 << 20)
	_ = connection.SetReadDeadline(time.Now().Add(3 * s.PingInterval))
	connection.SetPongHandler(func(string) error {
		s.Manager.Activity()
		return connection.SetReadDeadline(time.Now().Add(3 * s.PingInterval))
	})
	wsWriter := &websocketEventWriter{connection: connection, timeout: s.PingInterval}
	ctx, release, err := s.Manager.ConnectionContext(request.Context())
	if err != nil {
		_ = connection.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "sandbox stopping"), time.Now().Add(time.Second))
		return
	}
	defer release()
	s.Manager.Activity()
	pingDone := make(chan struct{})
	go s.ping(wsWriter, pingDone, release)
	defer close(pingDone)
	go func() {
		<-ctx.Done()
		_ = wsWriter.control(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "sandbox stopping"),
			time.Now().Add(time.Second))
		_ = connection.Close()
		_ = destination.Close()
	}()
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			messageType, data, readErr := connection.ReadMessage()
			if readErr != nil {
				return
			}
			if messageType != websocket.BinaryMessage {
				continue
			}
			if _, writeErr := destination.Write(data); writeErr != nil {
				return
			}
			s.Manager.Activity()
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		buffer := make([]byte, 32<<10)
		for {
			count, readErr := destination.Read(buffer)
			if count > 0 {
				if writeErr := wsWriter.binary(buffer[:count]); writeErr != nil {
					return
				}
				s.Manager.Activity()
			}
			if readErr != nil {
				return
			}
		}
	}()
	<-done
	release()
	_ = connection.Close()
	_ = destination.Close()
	<-done
}

func defaultShell() Command {
	if runtime.GOOS == "windows" {
		return Command{Executable: "cmd.exe", Directory: defaultWorkingDirectory()}
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return Command{Executable: shell, Directory: defaultWorkingDirectory()}
}

func defaultWorkingDirectory() string {
	for _, candidate := range []string{"/workspace/repo", "/workspace"} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return ""
}

func decodeJSON(reader io.Reader, destination any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing content")
	}
	return nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

func methodNotAllowed(writer http.ResponseWriter, allow string) {
	writer.Header().Set("Allow", allow)
	writeError(writer, http.StatusMethodNotAllowed, "method not allowed")
}
