package copilotruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareInjectsCurrentTokenPerProcessWithoutPersistingIt(t *testing.T) {
	var expectedToken string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/user" {
			t.Errorf("unexpected private endpoint probe: %s", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer "+expectedToken {
			t.Errorf("authorization header = %q", got)
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	runtimeDir := t.TempDir()
	tokenFile := filepath.Join(runtimeDir, "github-token")
	config := Config{TokenFile: tokenFile, APIURL: server.URL, Client: server.Client()}
	for _, token := range []string{"current-one", "rotated-two"} {
		expectedToken = token
		if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		environ, err := config.Prepare(context.Background(), nil, []string{
			"PATH=/usr/bin",
			"COPILOT_GITHUB_TOKEN=stale",
			"GH_TOKEN=different-user",
			"GITHUB_TOKEN=different-user",
		})
		if err != nil {
			t.Fatal(err)
		}
		assertEnvironmentValue(t, environ, "COPILOT_GITHUB_TOKEN", token)
		assertEnvironmentValue(t, environ, "COPILOT_HOME", filepath.Join(runtimeDir, "copilot"))
		assertEnvironmentAbsent(t, environ, "GH_TOKEN")
		assertEnvironmentAbsent(t, environ, "GITHUB_TOKEN")
	}

	entries, err := os.ReadDir(filepath.Join(runtimeDir, "copilot"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("wrapper persisted Copilot state: %#v", entries)
	}
	value, err := os.ReadFile(tokenFile)
	if err != nil || string(value) != "rotated-two\n" {
		t.Fatalf("wrapper modified the broker-owned credential: %q, %v", value, err)
	}
}

func TestPrepareDoesNotProbePrivateEntitlementEndpoint(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/user" {
			t.Fatalf("unexpected request: %s", request.URL.Path)
		}
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	if err := os.WriteFile(tokenFile, []byte("supported-oauth-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := (Config{TokenFile: tokenFile, APIURL: server.URL, Client: server.Client()}).
		Prepare(context.Background(), []string{"-p", "hello"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestPrepareVersionNeedsNoCredentialAndRemovesInheritedAuthentication(t *testing.T) {
	environ, err := (Config{TokenFile: filepath.Join(t.TempDir(), "missing")}).
		Prepare(context.Background(), []string{"--version"}, []string{"PATH=/bin", "COPILOT_GITHUB_TOKEN=host-secret"})
	if err != nil {
		t.Fatal(err)
	}
	assertEnvironmentAbsent(t, environ, "COPILOT_GITHUB_TOKEN")
}

func assertEnvironmentValue(t *testing.T, environ []string, name, expected string) {
	t.Helper()
	for _, value := range environ {
		if value == name+"="+expected {
			return
		}
	}
	t.Fatalf("%s=%q missing from environment", name, expected)
}

func assertEnvironmentAbsent(t *testing.T, environ []string, name string) {
	t.Helper()
	for _, value := range environ {
		if strings.HasPrefix(strings.ToUpper(value), strings.ToUpper(name)+"=") {
			t.Fatalf("%s unexpectedly present", name)
		}
	}
}
