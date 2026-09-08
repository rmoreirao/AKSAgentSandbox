package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
	"github.com/rmoreirao/AKSAgentSandbox/internal/broker"
	githubclient "github.com/rmoreirao/AKSAgentSandbox/internal/github"
	"github.com/rmoreirao/AKSAgentSandbox/internal/observability"
	"github.com/rmoreirao/AKSAgentSandbox/internal/version"
	authenticationv1 "k8s.io/api/authentication/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	logger := auth.NewRedactingLogger(os.Stderr, "devsandbox-broker: ", log.LstdFlags|log.LUTC)
	if err := run(logger); err != nil {
		logger.Fatal(err)
	}
}

func run(logger *log.Logger) error {
	for _, name := range []string{
		"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_FEDERATED_TOKEN_FILE",
		"DEVSANDBOX_KEY_VAULT_URL",
		"DEVSANDBOX_BROKER_TLS_CERT_FILE", "DEVSANDBOX_BROKER_TLS_KEY_FILE",
	} {
		if os.Getenv(name) == "" {
			return errors.New("required broker configuration is absent: " + name)
		}
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
			os.Getenv("DEVSANDBOX_GITHUB_STATIC_USER_ID") == "") {
		return errors.New("complete static validation credentials are required")
	}
	config, err := rest.InClusterConfig()
	if err != nil {
		return errors.New("load in-cluster Kubernetes configuration")
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return errors.New("create Kubernetes token review client")
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := authenticationv1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := devsandboxv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return errors.New("create scoped Kubernetes reader")
	}

	holderID, err := os.Hostname()
	if err != nil || holderID == "" {
		return errors.New("determine broker replica identity")
	}
	tokenSource := &auth.WorkloadIdentityTokenSource{
		TenantID: os.Getenv("AZURE_TENANT_ID"), ClientID: os.Getenv("AZURE_CLIENT_ID"),
		FederatedTokenFile: os.Getenv("AZURE_FEDERATED_TOKEN_FILE"),
	}
	externalHTTP := &http.Client{Timeout: 20 * time.Second}
	tokenSource.Client = externalHTTP
	store := &auth.KeyVaultRefreshCredentialStore{
		VaultURL: os.Getenv("DEVSANDBOX_KEY_VAULT_URL"), Tokens: tokenSource, Client: externalHTTP,
	}
	github := &githubclient.Client{
		ClientID: os.Getenv("DEVSANDBOX_GITHUB_APP_CLIENT_ID"), ClientSecret: os.Getenv("DEVSANDBOX_GITHUB_APP_CLIENT_SECRET"),
		HTTPClient: externalHTTP,
	}
	credentials := &auth.CredentialManager{
		Store: store, Refresher: github,
		Locker: &auth.KubernetesLeaseLocker{
			Client: kubeClient, Namespace: envOr("DEVSANDBOX_SYSTEM_NAMESPACE", "devsandbox-system"),
			HolderID: holderID, LeaseDuration: 2 * time.Minute,
		},
	}
	var credentialProvider broker.CredentialProvider = credentials
	if tokenFile := os.Getenv("DEVSANDBOX_GITHUB_STATIC_TOKEN_FILE"); authMode == "static-validation" {
		credentialProvider = auth.StaticTokenProvider{
			UserID: os.Getenv("DEVSANDBOX_GITHUB_STATIC_USER_ID"), TokenFile: tokenFile,
		}
	}
	audit := observability.NewAuditor(os.Stdout, "broker")
	metrics := observability.NewMetrics("broker")
	service := &broker.Service{
		Audience: envOr("DEVSANDBOX_BROKER_AUDIENCE", broker.DefaultAudience),
		Reviewer: broker.KubernetesTokenReviewer{
			Client: clientset.AuthenticationV1().TokenReviews(),
		},
		Bindings: broker.KubernetesBindingReader{Client: kubeClient}, Credentials: credentialProvider,
	}
	probes := observability.Probes{Checks: []observability.Check{
		{Name: "kubernetes_api", Run: func(ctx context.Context) error {
			_, err := clientset.Discovery().RESTClient().Get().AbsPath("/readyz").DoRaw(ctx)
			return err
		}},
		observability.HTTPCheck("key_vault", os.Getenv("DEVSANDBOX_KEY_VAULT_URL"), externalHTTP),
	}}
	brokerHandler := broker.Handler{Service: service, Audit: audit, Metrics: metrics}
	handler := auth.RedactionMiddleware(probes.Handler(brokerHandler, metrics.Handler()), audit)
	server := &http.Server{
		Addr:              envOr("DEVSANDBOX_BROKER_ADDRESS", ":8443"),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          logger,
		MaxHeaderBytes:    32 << 10,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	certFile := os.Getenv("DEVSANDBOX_BROKER_TLS_CERT_FILE")
	keyFile := os.Getenv("DEVSANDBOX_BROKER_TLS_KEY_FILE")
	if certFile == "" || keyFile == "" {
		return errors.New("broker TLS certificate and key files are required")
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(stop)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	logger.Printf("starting version=%s address=%s", version.String(), server.Addr)
	err = server.ListenAndServeTLS(certFile, keyFile)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
