package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTemplateServiceStartupConfiguration(t *testing.T) {
	for name, environment := range map[string]map[string]string{
		"missing readiness": {
			templateServiceCommandEnv: "/usr/local/bin/devsandbox-code-server",
		},
		"shell command": {
			templateServiceCommandEnv:  "/bin/sh -c code-server",
			templateServiceReadyURLEnv: "http://127.0.0.1:13337/healthz",
		},
		"external readiness": {
			templateServiceCommandEnv:  "/usr/local/bin/devsandbox-code-server",
			templateServiceReadyURLEnv: "https://sandbox.example/healthz",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := TemplateServiceConfigFromEnvironment(func(key string) string { return environment[key] })
			if err == nil {
				t.Fatal("unsafe or incomplete template service configuration was accepted")
			}
		})
	}

	environment := map[string]string{
		templateServiceCommandEnv:  "/usr/local/bin/devsandbox-code-server",
		templateServiceReadyURLEnv: "http://127.0.0.1:13337/healthz",
	}
	config, enabled, err := TemplateServiceConfigFromEnvironment(func(key string) string { return environment[key] })
	if err != nil || !enabled {
		t.Fatalf("valid startup configuration rejected: enabled=%v err=%v", enabled, err)
	}
	if config.Executable != environment[templateServiceCommandEnv] || config.ReadyURL != environment[templateServiceReadyURLEnv] {
		t.Fatalf("startup configuration changed: %#v", config)
	}
}

func TestTemplateServiceReadinessTracksProcess(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer ready.Close()

	config := TemplateServiceConfig{
		Executable: os.Args[0],
		Arguments:  []string{"-test.run=TestTemplateServiceHelperProcess", "--"},
		ReadyURL:   ready.URL, StartupTimeout: 3 * time.Second, PollInterval: 10 * time.Millisecond,
	}
	service, err := StartTemplateService(context.Background(), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Check(context.Background()); err != nil {
		t.Fatalf("ready service failed its check: %v", err)
	}
	service.Close()
	if err := service.Check(context.Background()); err == nil {
		t.Fatal("stopped service remained ready")
	}
}

func TestTemplateServiceHelperProcess(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "-test.run=TestTemplateServiceHelperProcess") {
		return
	}
	select {}
}
