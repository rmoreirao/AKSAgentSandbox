package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rmoreirao/AKSAgentSandbox/internal/observability"
	"github.com/rmoreirao/AKSAgentSandbox/internal/supervisor"
)

func main() {
	address := flag.String("listen", envOr("DEVSANDBOX_AGENT_LISTEN", ":8081"), "internal listen address")
	tokenFile := flag.String("auth-token-file", envOr("DEVSANDBOX_AGENT_TOKEN_FILE", supervisor.DefaultRuntimeDir+"/agent-token"), "agent bearer-token file")
	flag.Parse()

	if err := supervisor.EnsureNonRoot(); err != nil {
		slog.Error("unsafe supervisor identity", "error", err)
		os.Exit(1)
	}
	if err := supervisor.EnsureBearerToken(*tokenFile); err != nil {
		slog.Error("initialize supervisor authentication", "error", err)
		os.Exit(1)
	}
	audit := observability.NewAuditor(os.Stdout, "supervisor")
	metrics := observability.NewMetrics("supervisor")
	manager := supervisor.NewManager(func(event supervisor.AuditEvent) {
		action := "session.end"
		delta := -1.0
		if event.Action == "job.started" {
			action = "session.start"
			delta = 1
		}
		metrics.ActiveJobs.Add(delta)
		audit.Record(observability.AuditEvent{
			Time: event.Time, Event: action, Outcome: "success", Session: "job", JobID: event.JobID,
		})
	})
	authenticator := supervisor.Authenticator(supervisor.TokenFileAuthenticator{Path: *tokenFile})
	if sandboxUID := os.Getenv("DEVSANDBOX_SANDBOX_UID"); sandboxUID != "" {
		authenticator = supervisor.AnyAuthenticator{
			supervisor.RouterAuthenticator(sandboxUID),
			supervisor.TokenFileAuthenticator{Path: *tokenFile},
		}
	}
	api, err := supervisor.NewServer(manager, authenticator)
	if err != nil {
		slog.Error("configure supervisor", "error", err)
		os.Exit(1)
	}
	api.Audit = audit
	api.Metrics = metrics
	handler, err := api.Handler()
	if err != nil {
		slog.Error("configure supervisor HTTP handler", "error", err)
		os.Exit(1)
	}
	checks := []observability.Check{}
	brokerURL := os.Getenv("DEVSANDBOX_BROKER_URL")
	if brokerURL != "" {
		brokerHTTP, err := supervisor.BrokerHTTPClient(os.Getenv("DEVSANDBOX_BROKER_CA_FILE"))
		if err != nil {
			slog.Error("configure broker readiness", "error", err)
			os.Exit(1)
		}
		brokerHTTP.Timeout = 3 * time.Second
		checks = append(checks, observability.HTTPCheck("broker", brokerURL, brokerHTTP))
	}
	handler = (observability.Probes{Checks: checks}).Handler(handler, metrics.Handler())
	server := &http.Server{
		Addr:              *address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	credentialFile, err := configureRuntimeCredential(ctx, audit, metrics)
	if err != nil {
		slog.Error("initialize runtime credential", "error", err)
		os.Exit(1)
	}
	if credentialFile != nil {
		defer credentialFile.Remove()
	}
	templateConfig, templateEnabled, err := supervisor.TemplateServiceConfigFromEnvironment(os.Getenv)
	if err != nil {
		slog.Error("configure template service", "error", err)
		os.Exit(1)
	}
	var templateService *supervisor.TemplateService
	if templateEnabled {
		templateService, err = supervisor.StartTemplateService(ctx, templateConfig, os.Stderr)
		if err != nil {
			slog.Error("start template service", "error", err)
			os.Exit(1)
		}
		defer templateService.Close()
		checks = append(checks, observability.Check{Name: "template_service", Run: templateService.Check})
		handler = (observability.Probes{Checks: checks}).Handler(handler, metrics.Handler())
		server.Handler = handler
	}
	shutdown := make(chan struct{}, 1)
	api.OnShutdown = func() {
		select {
		case shutdown <- struct{}{}:
		default:
		}
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-shutdown:
		}
		stop()
		timeout, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = manager.Shutdown(timeout)
		_ = server.Shutdown(timeout)
	}()

	slog.Info("devsandbox agent listening", "address", *address)
	err = server.ListenAndServe()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("supervisor HTTP server failed", "error", err)
		os.Exit(1)
	}
}

func configureRuntimeCredential(ctx context.Context, audit observability.AuditSink, metrics *observability.Metrics) (*supervisor.CredentialFile, error) {
	brokerURL := os.Getenv("DEVSANDBOX_BROKER_URL")
	if brokerURL == "" {
		return nil, nil
	}
	runtimeDir := envOr("DEVSANDBOX_RUNTIME_DIR", supervisor.DefaultRuntimeDir)
	credential := &supervisor.CredentialFile{RuntimeDir: runtimeDir, Name: "github-token"}
	client := supervisor.BrokerClient{Config: supervisor.RuntimeIdentityConfig{
		ServiceAccountName: os.Getenv("DEVSANDBOX_SERVICE_ACCOUNT_NAME"),
		BrokerAudience:     envOr("DEVSANDBOX_BROKER_AUDIENCE", "devsandbox-credential-broker"),
		BrokerTokenFile:    envOr("DEVSANDBOX_BROKER_TOKEN_FILE", supervisor.DefaultBrokerTokenFile),
		BrokerURL:          brokerURL,
		BrokerCAFile:       os.Getenv("DEVSANDBOX_BROKER_CA_FILE"),
	}}
	value, err := client.Fetch(ctx)
	if err != nil {
		metrics.TokenRefreshFailures.Inc()
		audit.Record(observability.AuditEvent{Event: "credential.refresh", Outcome: "failure"})
		return nil, err
	}
	if err := credential.Write(value); err != nil {
		return nil, err
	}
	audit.Record(observability.AuditEvent{Event: "credential.refresh", Outcome: "success"})
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				value, err := client.Fetch(ctx)
				if err != nil {
					slog.Warn("runtime credential refresh failed")
					metrics.TokenRefreshFailures.Inc()
					audit.Record(observability.AuditEvent{Event: "credential.refresh", Outcome: "failure"})
					continue
				}
				if err := credential.Write(value); err != nil {
					slog.Warn("runtime credential update failed")
					metrics.TokenRefreshFailures.Inc()
					audit.Record(observability.AuditEvent{Event: "credential.refresh", Outcome: "failure"})
					continue
				}
				audit.Record(observability.AuditEvent{Event: "credential.refresh", Outcome: "success"})
			}
		}
	}()
	return credential, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
