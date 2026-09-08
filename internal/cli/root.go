package cli

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type Options struct {
	In                  io.Reader
	Out                 io.Writer
	Err                 io.Writer
	HTTPClient          *http.Client
	Credentials         CredentialStore
	BootstrapCredential func(context.Context) (string, error)
	Resolver            RepositoryResolver
	Random              io.Reader
	IsTerminal          func() bool
	Sleep               func(context.Context, time.Duration) error
	PollInterval        time.Duration
	OpenURL             func(string) error
	Attach              func(context.Context, *Client, string, string, io.Reader, io.Writer, io.Writer) error
	Version             string
	APIHostHeader       string
	SkipTLSVerify       bool
}

type application struct {
	options  Options
	apiURL   string
	authMode string
	json     bool
	prompt   *Prompter
	dialer   *websocket.Dialer
}

func Execute(args []string, options Options) int {
	restoreEnvironment, err := loadLocalEnvironment()
	if err != nil {
		output := options.Err
		if output == nil {
			output = os.Stderr
		}
		detail := cliError(ExitInvalid, "invalid_environment",
			fmt.Sprintf("local DevSandbox configuration is invalid: %v", err), nil)
		writeError(output, detail, false)
		return detail.ExitCode
	}
	defer restoreEnvironment()
	app := newApplication(options)
	command := app.rootCommand()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	command.SetContext(ctx)
	command.SetArgs(args)
	err = command.Execute()
	if err != nil {
		var remote RemoteExitError
		if errors.As(err, &remote) {
			return remote.Code
		}
		if !errors.As(err, new(*Error)) {
			err = invalid(err.Error())
		}
		writeError(app.options.Err, err, app.json)
	}
	return exitCode(err)
}

func NewRootCommand(options Options) *cobra.Command {
	return newApplication(options).rootCommand()
}

func newApplication(options Options) *application {
	if options.In == nil {
		options.In = os.Stdin
	}
	if options.Out == nil {
		options.Out = os.Stdout
	}
	if options.Err == nil {
		options.Err = os.Stderr
	}
	if options.Credentials == nil {
		options.Credentials = OSKeyring{}
	}
	if options.BootstrapCredential == nil {
		options.BootstrapCredential = githubCLIBootstrapCredential
	}
	if options.Random == nil {
		options.Random = rand.Reader
	}
	if options.IsTerminal == nil {
		options.IsTerminal = func() bool {
			file, ok := options.In.(*os.File)
			return ok && term.IsTerminal(int(file.Fd()))
		}
	}
	if options.Sleep == nil {
		options.Sleep = sleepContext
	}
	if options.PollInterval <= 0 {
		options.PollInterval = time.Second
	}
	if options.OpenURL == nil {
		options.OpenURL = openBrowser
	}
	if options.Attach == nil {
		options.Attach = attachEntry
	}
	if options.Resolver == nil {
		directory, _ := os.Getwd()
		options.Resolver = GitRepositoryResolver{Directory: directory}
	}
	if options.APIHostHeader == "" {
		options.APIHostHeader = strings.TrimSpace(os.Getenv("DEVSANDBOX_API_HOST_HEADER"))
	}
	if !options.SkipTLSVerify {
		options.SkipTLSVerify = strings.EqualFold(
			strings.TrimSpace(os.Getenv("DEVSANDBOX_SKIP_TLS_VERIFY")),
			"true",
		)
	}
	var dialer *websocket.Dialer
	if options.HTTPClient == nil {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: options.SkipTLSVerify, //nolint:gosec // Explicit validation-profile opt-in.
		}
		if options.APIHostHeader != "" {
			tlsConfig.ServerName = options.APIHostHeader
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = tlsConfig
		options.HTTPClient = &http.Client{Transport: transport, Timeout: 30 * time.Second}
		configuredDialer := *websocket.DefaultDialer
		configuredDialer.TLSClientConfig = tlsConfig.Clone()
		dialer = &configuredDialer
	}
	app := &application{
		options:  options,
		dialer:   dialer,
		authMode: strings.ToLower(strings.TrimSpace(os.Getenv("DEVSANDBOX_AUTH_MODE"))),
	}
	app.prompt = NewPrompter(options.In, options.Out, options.IsTerminal())
	return app
}

func (a *application) rootCommand() *cobra.Command {
	command := &cobra.Command{
		Use:           "devsandbox",
		Short:         "Create and manage isolated AKS development sandboxes",
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       a.options.Version,
	}
	command.SetIn(a.options.In)
	command.SetOut(a.options.Out)
	command.SetErr(a.options.Err)
	command.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return invalid(err.Error()) })
	defaultURL := os.Getenv("DEVSANDBOX_API_URL")
	if defaultURL == "" {
		defaultURL = "http://localhost:8080"
	}
	command.PersistentFlags().StringVar(&a.apiURL, "api-url", defaultURL, "management API URL (or DEVSANDBOX_API_URL)")
	command.PersistentFlags().BoolVar(&a.json, "json", false, "emit stable JSON output")
	command.AddCommand(
		a.loginCommand(),
		a.doctorCommand(),
		a.templatesCommand(),
		a.upCommand(),
		a.listCommand(),
		a.statusCommand(),
		a.shellCommand(),
		a.execCommand(),
		a.jobsCommand(),
		a.portCommand(),
		a.stopCommand(),
		a.resumeCommand(),
		a.deleteCommand(),
	)
	return command
}

func (a *application) doctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check local configuration, authentication, API access, and repository readiness",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			client, err := a.client(command.Context(), true)
			if err != nil {
				return err
			}
			identity, err := client.Me(command.Context())
			if err != nil {
				return err
			}
			templates, err := client.Templates(command.Context())
			if err != nil {
				return err
			}
			repository, err := a.options.Resolver.Current(command.Context(), a.prompt)
			if err != nil {
				return err
			}
			if a.json {
				return writeJSON(a.options.Out, map[string]any{
					"apiUrl":   a.apiURL,
					"authMode": a.authMode,
					"identity": identity,
					"repository": map[string]string{
						"id": repository.ID, "url": repository.URL, "ref": repository.Ref,
						"commitSha": repository.CommitSHA,
					},
					"templateCount": len(templates),
					"ready":         true,
				})
			}
			_, _ = fmt.Fprintf(a.options.Out,
				"API: %s\nAuthenticated as: %s\nRepository: %s @ %s\nTemplates: %d\nReady: devsandbox up\n",
				a.apiURL, identity.GitHubLogin, repository.ID, repository.CommitSHA, len(templates))
			return nil
		},
	}
}

func (a *application) client(ctx context.Context, authenticated bool) (*Client, error) {
	client, err := NewClient(a.apiURL, a.options.HTTPClient)
	if err != nil {
		return nil, err
	}
	client.HostHeader = a.options.APIHostHeader
	client.Dialer = a.dialer
	if authenticated {
		if a.authMode != "" && a.authMode != authModeDeviceFlow &&
			a.authMode != authModeGitHubCLIStatic {
			return nil, invalid("DEVSANDBOX_AUTH_MODE must be device-flow or github-cli-static")
		}
		token, loadErr := a.options.Credentials.Load(ctx, credentialAccount(a.apiURL))
		if a.authMode != authModeGitHubCLIStatic {
			if loadErr != nil {
				return nil, loadErr
			}
			client.Token = token
			return client, nil
		}
		if loadErr == nil {
			client.Token = token
			if _, validateErr := client.Me(ctx); validateErr == nil {
				return client, nil
			} else if !isAuthenticationError(validateErr) {
				return nil, validateErr
			}
		} else if !isAuthenticationError(loadErr) {
			return nil, loadErr
		}
		if _, bootstrapErr := a.bootstrapStatic(ctx, client); bootstrapErr != nil {
			return nil, bootstrapErr
		}
	}
	return client, nil
}

func (a *application) bootstrapStatic(ctx context.Context, client *Client) (LoginResult, error) {
	var result LoginResult
	credential, err := a.options.BootstrapCredential(ctx)
	if err != nil {
		return result, err
	}
	result, err = client.BootstrapStatic(ctx, credential)
	if err != nil {
		return result, err
	}
	if result.SessionToken == "" {
		return result, cliError(ExitInternal, "invalid_api_response",
			"static bootstrap response contained no platform session", nil)
	}
	if err := a.options.Credentials.Store(ctx, credentialAccount(a.apiURL), result.SessionToken); err != nil {
		return result, err
	}
	client.Token = result.SessionToken
	return result, nil
}

func (a *application) loginCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Authenticate DevSandbox with GitHub",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			client, err := a.client(command.Context(), false)
			if err != nil {
				return err
			}
			if a.authMode == authModeGitHubCLIStatic {
				result, err := a.bootstrapStatic(command.Context(), client)
				if err != nil {
					return err
				}
				return a.writeLoginResult(result)
			}
			if a.authMode != "" && a.authMode != authModeDeviceFlow {
				return invalid("DEVSANDBOX_AUTH_MODE must be device-flow or github-cli-static")
			}
			start, err := client.StartDeviceFlow(command.Context())
			if err != nil {
				return err
			}
			if a.json {
				if err := writeJSON(a.options.Out, start); err != nil {
					return err
				}
			} else {
				_, _ = fmt.Fprintf(a.options.Out, "Open %s and enter code %s\n", start.VerificationURI, start.UserCode)
			}
			interval := time.Duration(start.Interval) * time.Second
			if interval <= 0 {
				interval = 5 * time.Second
			}
			deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
			for {
				result, pending, pollErr := client.PollDeviceFlow(command.Context(), start.State)
				if pollErr != nil {
					return pollErr
				}
				if !pending {
					if result.SessionToken == "" {
						return cliError(ExitInternal, "invalid_api_response", "login response contained no platform session", nil)
					}
					if storeErr := a.options.Credentials.Store(command.Context(), credentialAccount(a.apiURL), result.SessionToken); storeErr != nil {
						return storeErr
					}
					return a.writeLoginResult(result)
				}
				if !deadline.IsZero() && time.Now().Add(interval).After(deadline) {
					return cliError(ExitAuthentication, "device_flow_expired", "device authorization expired; run login again", nil)
				}
				if err := a.options.Sleep(command.Context(), interval); err != nil {
					return err
				}
			}
		},
	}
}

func (a *application) writeLoginResult(result LoginResult) error {
	if a.json {
		return writeJSON(a.options.Out, map[string]any{
			"authenticated": true,
			"expiresAt":     result.ExpiresAt,
			"identity":      result.Identity,
		})
	}
	_, _ = fmt.Fprintf(a.options.Out, "Logged in as %s\n", result.Identity.GitHubLogin)
	return nil
}

func (a *application) templatesCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "templates",
		Short: "List available sandbox templates",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			client, err := a.client(command.Context(), true)
			if err != nil {
				return err
			}
			items, err := client.Templates(command.Context())
			if err != nil {
				return err
			}
			if a.json {
				return writeJSON(a.options.Out, map[string]any{"items": items})
			}
			writer := tabwriter.NewWriter(a.options.Out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(writer, "NAME\tVERSION\tDEFAULT PROFILE\tENTRY\tDESCRIPTION")
			for _, item := range items {
				_, _ = fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n",
					item.Name, item.Version, item.DefaultProfile, item.EntryAction, item.Description)
			}
			return writer.Flush()
		},
	}
	show := &cobra.Command{
		Use:   "show <name>",
		Short: "Show an available sandbox template",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			client, err := a.client(command.Context(), true)
			if err != nil {
				return err
			}
			item, err := client.Template(command.Context(), args[0])
			if err != nil {
				return err
			}
			if a.json {
				return writeJSON(a.options.Out, item)
			}
			_, _ = fmt.Fprintf(a.options.Out,
				"Name: %s\nDisplay name: %s\nVersion: %s\nDescription: %s\nDefault profile: %s\nEntry action: %s\nImage digest: %s\n",
				item.Name, item.DisplayName, item.Version, item.Description, item.DefaultProfile, item.EntryAction, item.ImageDigest)
			return nil
		},
	}
	command.AddCommand(show)
	return command
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func openBrowser(target string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		command = exec.Command("open", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	if err := command.Start(); err != nil {
		return cliError(ExitInternal, "browser_open_failed",
			"unable to open the authenticated browser URL; it was not printed because it contains a one-time credential", nil)
	}
	return nil
}

type streamEvent struct {
	Type     string `json:"type"`
	Data     []byte `json:"data,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Canceled bool   `json:"canceled,omitempty"`
	Error    string `json:"error,omitempty"`
}

func attachEntry(ctx context.Context, client *Client, name, action string, in io.Reader, out, errOut io.Writer) error {
	return attachShell(ctx, client, name, entryInput(action), in, out, errOut)
}

func entryInput(action string) []byte {
	if action == "copilot" {
		return []byte("exec copilot\n")
	}
	return nil
}
