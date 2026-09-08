package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rmoreirao/AKSAgentSandbox/internal/gateway"
	"github.com/rmoreirao/AKSAgentSandbox/internal/observability"
	"github.com/rmoreirao/AKSAgentSandbox/internal/version"
)

func main() {
	if err := run(); err != nil {
		log.Printf("devsandbox-web failed: %v", err)
		os.Exit(1)
	}
}

func run() error {
	for _, name := range []string{
		"DEVSANDBOX_ROUTER_URL", "DEVSANDBOX_INTERNAL_EXCHANGE_URL", "DEVSANDBOX_ROUTE_PUBLIC_KEY_FILE",
	} {
		if os.Getenv(name) == "" {
			return fmt.Errorf("required configuration is absent: %s", name)
		}
	}
	publicKey, err := os.ReadFile(os.Getenv("DEVSANDBOX_ROUTE_PUBLIC_KEY_FILE"))
	if err != nil {
		return errors.New("read route verification public key")
	}
	verifier, err := gateway.ParseRSAPublicKeyPEM(publicKey)
	if err != nil {
		return err
	}
	routerURL, err := gateway.ParseRouterURL(os.Getenv("DEVSANDBOX_ROUTER_URL"))
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	metrics := observability.NewMetrics("gateway")
	handler := gateway.WebHandler{
		Claims: gateway.ClaimManager{Signer: verifier},
		Exchange: gateway.HTTPExchanger{
			URL:    os.Getenv("DEVSANDBOX_INTERNAL_EXCHANGE_URL"),
			Client: client,
		},
		Router: gateway.DirectRouter{BaseURL: routerURL}, Audit: observability.NewAuditor(os.Stdout, "gateway"), Metrics: metrics,
	}
	routerHealth := *routerURL
	routerHealth.Path = "/healthz"
	probes := observability.Probes{Checks: []observability.Check{
		observability.HTTPCheck("sandbox_router", routerHealth.String(), client),
		observability.HTTPCheck("api_exchange", os.Getenv("DEVSANDBOX_INTERNAL_EXCHANGE_URL"), client),
	}}
	server := &http.Server{
		Addr: envOr("DEVSANDBOX_WEB_ADDRESS", ":8080"), Handler: probes.Handler(handler, metrics.Handler()),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 0,
		WriteTimeout: 0, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 32 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("devsandbox web gateway started version=%s", version.String())
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
