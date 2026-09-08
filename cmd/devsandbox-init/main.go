package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"

	"github.com/rmoreirao/AKSAgentSandbox/internal/repository"
	"github.com/rmoreirao/AKSAgentSandbox/internal/supervisor"
)

func main() {
	var source repository.Source
	workspace := flag.String("workspace", envOr("DEVSANDBOX_WORKSPACE", "/workspace"), "workspace mount path")
	flag.StringVar(&source.Type, "source-type", envOr("DEVSANDBOX_SOURCE_TYPE", "empty"), "source type: empty or git")
	flag.StringVar(&source.RepositoryURL, "repository-url", os.Getenv("DEVSANDBOX_REPOSITORY_URL"), "HTTPS repository URL")
	flag.StringVar(&source.CommitSHA, "commit-sha", os.Getenv("DEVSANDBOX_COMMIT_SHA"), "exact Git commit")
	flag.StringVar(&source.RefName, "ref", os.Getenv("DEVSANDBOX_REF_NAME"), "source ref name")
	flag.StringVar(&source.AuthorName, "author-name", os.Getenv("DEVSANDBOX_GIT_AUTHOR_NAME"), "Git author name")
	flag.StringVar(&source.AuthorEmail, "author-email", os.Getenv("DEVSANDBOX_GIT_AUTHOR_EMAIL"), "Git author email")
	flag.StringVar(&source.PrimaryOrg, "primary-org", os.Getenv("DEVSANDBOX_PRIMARY_GITHUB_ORG"), "primary GitHub organization")
	readOnlyDefault, _ := strconv.ParseBool(os.Getenv("DEVSANDBOX_REPOSITORY_READ_ONLY"))
	flag.BoolVar(&source.ReadOnly, "read-only", readOnlyDefault, "disable pushes to the source repository")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	hook := repository.Hook(repository.DirectoryHook{})
	if source.Type == "git" {
		credential, credentialErr := configureRuntimeCredential(ctx)
		if credentialErr != nil {
			slog.Error("obtain repository credential", "error", credentialErr)
			os.Exit(1)
		}
		if credential != nil {
			defer credential.Remove()
		}
		hook = repository.GitHook{Reporter: os.Stdout}
	}

	result, err := (repository.Initializer{
		WorkspaceDir: *workspace,
		Hook:         hook,
	}).Run(ctx, source)
	if err != nil {
		slog.Error("workspace initialization failed", "error", err)
		os.Exit(1)
	}
	state := "resumed"
	if result.FirstInitialization {
		state = "initialized"
	}
	fmt.Fprintln(os.Stdout, state)
}

func configureRuntimeCredential(ctx context.Context) (*supervisor.CredentialFile, error) {
	brokerURL := os.Getenv("DEVSANDBOX_BROKER_URL")
	if brokerURL == "" {
		return nil, nil
	}
	credential := &supervisor.CredentialFile{
		RuntimeDir: envOr("DEVSANDBOX_RUNTIME_DIR", supervisor.DefaultRuntimeDir),
		Name:       "github-token",
	}
	value, err := (supervisor.BrokerClient{Config: supervisor.RuntimeIdentityConfig{
		ServiceAccountName: os.Getenv("DEVSANDBOX_SERVICE_ACCOUNT_NAME"),
		BrokerAudience:     envOr("DEVSANDBOX_BROKER_AUDIENCE", "devsandbox-credential-broker"),
		BrokerTokenFile:    envOr("DEVSANDBOX_BROKER_TOKEN_FILE", supervisor.DefaultBrokerTokenFile),
		BrokerURL:          brokerURL,
		BrokerCAFile:       os.Getenv("DEVSANDBOX_BROKER_CA_FILE"),
	}}).Fetch(ctx)
	if err != nil {
		return nil, err
	}
	if err := credential.Write(value); err != nil {
		return nil, err
	}
	return credential, nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
