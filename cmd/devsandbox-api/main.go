package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	apihttp "github.com/rmoreirao/AKSAgentSandbox/internal/api"
	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
	"github.com/rmoreirao/AKSAgentSandbox/internal/gateway"
	githubclient "github.com/rmoreirao/AKSAgentSandbox/internal/github"
	"github.com/rmoreirao/AKSAgentSandbox/internal/observability"
	"github.com/rmoreirao/AKSAgentSandbox/internal/version"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	if err := run(); err != nil {
		log.Printf("devsandbox-api failed: %v", err)
		os.Exit(1)
	}
}

func run() error {
	required := []string{
		"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_FEDERATED_TOKEN_FILE",
		"DEVSANDBOX_KEY_VAULT_URL", "DEVSANDBOX_KEY_VAULT_SIGNING_KEY_ID",
		"DEVSANDBOX_WEB_BASE_URL", "DEVSANDBOX_ROUTER_URL",
	}
	if missing := missingEnvironment(required); len(missing) > 0 {
		return fmt.Errorf("required configuration is absent: %v", missing)
	}
	authMode := os.Getenv("DEVSANDBOX_GITHUB_AUTH_MODE")
	if authMode != "github-app" && authMode != "static-validation" {
		return errors.New("DEVSANDBOX_GITHUB_AUTH_MODE must be github-app or static-validation")
	}
	if authMode == "github-app" &&
		(os.Getenv("DEVSANDBOX_GITHUB_APP_CLIENT_ID") == "" ||
			os.Getenv("DEVSANDBOX_GITHUB_APP_CLIENT_SECRET") == "") {
		return errors.New("GitHub App credentials are required in github-app mode")
	}
	if authMode == "static-validation" &&
		(os.Getenv("DEVSANDBOX_GITHUB_STATIC_TOKEN_FILE") == "" ||
			os.Getenv("DEVSANDBOX_GITHUB_STATIC_USER_ID") == "" ||
			os.Getenv("DEVSANDBOX_GITHUB_STATIC_LOGIN") == "") {
		return errors.New("complete static validation credentials are required")
	}
	routerURL, err := gateway.ParseRouterURL(os.Getenv("DEVSANDBOX_ROUTER_URL"))
	if err != nil {
		return err
	}
	webURL, err := url.Parse(os.Getenv("DEVSANDBOX_WEB_BASE_URL"))
	if err != nil || webURL.Scheme != "https" || webURL.Host == "" {
		return errors.New("DEVSANDBOX_WEB_BASE_URL must be an absolute HTTPS URL")
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return errors.New("load in-cluster Kubernetes configuration")
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		devsandboxv1alpha1.AddToScheme, corev1.AddToScheme, coordinationv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return err
		}
	}
	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return errors.New("create scoped Kubernetes client")
	}
	namespace := envOr("DEVSANDBOX_SYSTEM_NAMESPACE", "devsandbox-system")
	workloads := envOr("DEVSANDBOX_WORKLOAD_NAMESPACE", "devsandbox-workloads")
	holder, err := os.Hostname()
	if err != nil || holder == "" {
		return errors.New("determine API replica identity")
	}
	externalHTTP := &http.Client{Timeout: 20 * time.Second}
	tokens := &auth.WorkloadIdentityTokenSource{
		TenantID: os.Getenv("AZURE_TENANT_ID"), ClientID: os.Getenv("AZURE_CLIENT_ID"),
		FederatedTokenFile: os.Getenv("AZURE_FEDERATED_TOKEN_FILE"), Client: externalHTTP,
	}
	github := &githubclient.Client{
		ClientID:     os.Getenv("DEVSANDBOX_GITHUB_APP_CLIENT_ID"),
		ClientSecret: os.Getenv("DEVSANDBOX_GITHUB_APP_CLIENT_SECRET"),
		HTTPClient:   externalHTTP,
	}
	credentials := &auth.CredentialManager{
		Store: &auth.KeyVaultRefreshCredentialStore{
			VaultURL: os.Getenv("DEVSANDBOX_KEY_VAULT_URL"), Tokens: tokens, Client: externalHTTP,
		},
		Refresher: github,
		Locker: &auth.KubernetesLeaseLocker{
			Client: kube, Namespace: namespace, HolderID: holder, LeaseDuration: 2 * time.Minute,
		},
	}
	var accessTokens interface {
		AccessToken(context.Context, string) (string, time.Time, error)
	} = credentials
	if tokenFile := os.Getenv("DEVSANDBOX_GITHUB_STATIC_TOKEN_FILE"); authMode == "static-validation" {
		accessTokens = auth.StaticTokenProvider{
			UserID: os.Getenv("DEVSANDBOX_GITHUB_STATIC_USER_ID"), TokenFile: tokenFile,
		}
	}
	signer := auth.KeyVaultSigner{
		Client: apihttp.KeyVaultSigningClient{Tokens: tokens, Client: externalHTTP},
		KeyID:  os.Getenv("DEVSANDBOX_KEY_VAULT_SIGNING_KEY_ID"),
	}
	sessionLifetime := 15 * time.Minute
	staticTokenFile := ""
	if authMode == "static-validation" {
		staticTokenFile = os.Getenv("DEVSANDBOX_GITHUB_STATIC_TOKEN_FILE")
		sessionLifetime = 4 * time.Hour
	}
	sessions := auth.SessionManager{Signer: signer, Audience: "devsandbox-api", Lifetime: sessionLifetime}
	login := &auth.LoginService{
		GitHub:      github,
		States:      apihttp.KubernetesDeviceStateStore{Client: kube, Namespace: namespace},
		Credentials: credentials, Sessions: sessions,
		PrimaryOrg: os.Getenv("DEVSANDBOX_PRIMARY_GITHUB_ORG"),
	}
	claims := gateway.ClaimManager{Signer: signer, Lifetime: 5 * time.Minute}
	oneTime := &gateway.KubernetesCredentialStore{Client: kube, Namespace: namespace}
	router := gateway.DirectRouter{BaseURL: routerURL}
	kubernetesStore := apihttp.KubernetesStore{Client: kube, Namespace: workloads}
	audit := observability.NewAuditor(os.Stdout, "api")
	metrics := observability.NewMetrics("api")
	handler := apihttp.Handler{
		Auth: apihttp.AuthHandler{
			Login: login, Sessions: sessions, Audit: audit,
			StaticTokenFile: staticTokenFile,
			StaticIdentity: auth.Identity{
				UserID: os.Getenv("DEVSANDBOX_GITHUB_STATIC_USER_ID"),
				Login:  os.Getenv("DEVSANDBOX_GITHUB_STATIC_LOGIN"),
				Org:    os.Getenv("DEVSANDBOX_GITHUB_STATIC_LOGIN"),
			},
		},
		Sessions: sessions, Store: kubernetesStore, Activity: kubernetesStore,
		Router: router, WebSockets: router, Claims: claims, Credentials: oneTime,
		WebBaseURL: webURL.String(), WorkloadNamespace: workloads,
		AgentPort: intEnv("DEVSANDBOX_AGENT_PORT", 8081), VSCodePort: intEnv("DEVSANDBOX_VSCODE_PORT", 13337),
		RepositoryResolver: github, AccessTokens: accessTokens,
		PrimaryOrg: os.Getenv("DEVSANDBOX_PRIMARY_GITHUB_ORG"),
		Audit:      audit, Metrics: metrics,
	}
	routerHealth := *routerURL
	routerHealth.Path = "/healthz"
	gatewayHealthURL := os.Getenv("DEVSANDBOX_WEB_READINESS_URL")
	if gatewayHealthURL == "" {
		gatewayHealth := *webURL
		gatewayHealth.Path = "/healthz"
		gatewayHealthURL = gatewayHealth.String()
	}
	probes := observability.Probes{Checks: []observability.Check{
		{Name: "kubernetes_api", Run: func(ctx context.Context) error {
			return kube.List(ctx, &devsandboxv1alpha1.DevSandboxList{}, client.InNamespace(workloads), client.Limit(1))
		}},
		observability.HTTPCheck("sandbox_router", routerHealth.String(), externalHTTP),
		observability.HTTPCheck("key_vault", os.Getenv("DEVSANDBOX_KEY_VAULT_URL"), externalHTTP),
		observability.HTTPCheck("gateway", gatewayHealthURL, externalHTTP),
	}}
	publicHandler := probes.Handler(handler, metrics.Handler())
	public := server(envOr("DEVSANDBOX_API_ADDRESS", ":8080"), publicHandler)
	exchange := server(envOr("DEVSANDBOX_EXCHANGE_ADDRESS", ":8081"), apihttp.ExchangeHandler{Credentials: oneTime})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := handler.RecoverManagedJobs(ctx); err != nil {
		return errors.New("recover managed job monitoring")
	}
	failures := make(chan error, 2)
	go listen(public, failures)
	go listen(exchange, failures)
	log.Printf("devsandbox API started version=%s", version.String())
	select {
	case <-ctx.Done():
	case err := <-failures:
		return err
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = public.Shutdown(shutdown)
	_ = exchange.Shutdown(shutdown)
	return nil
}

func server(address string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 0, WriteTimeout: 0, IdleTimeout: 2 * time.Minute,
		MaxHeaderBytes: 32 << 10,
	}
}

func listen(server *http.Server, failures chan<- error) {
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		failures <- err
	}
}

func missingEnvironment(names []string) []string {
	var result []string
	for _, name := range names {
		if os.Getenv(name) == "" {
			result = append(result, name)
		}
	}
	return result
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func intEnv(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value < 1 || value > 65535 {
		return fallback
	}
	return value
}
