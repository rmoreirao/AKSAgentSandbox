package copilotruntime

import (
	"context"
	"encoding/json"
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
		Prepare(context.Background(), []string{"--version"}, []string{
			"PATH=/bin", "COPILOT_GITHUB_TOKEN=host-secret", "OPENCODE_AUTH_CONTENT=host-secret",
		})
	if err != nil {
		t.Fatal(err)
	}
	assertEnvironmentAbsent(t, environ, "COPILOT_GITHUB_TOKEN")
	assertEnvironmentAbsent(t, environ, "OPENCODE_AUTH_CONTENT")
}

func TestPrepareOpenCodeInjectsInMemoryCopilotAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	runtimeDir := t.TempDir()
	tokenFile := filepath.Join(runtimeDir, "github-token")
	openCodeRuntimeDir := filepath.Join(runtimeDir, "opencode-runtime")
	if err := os.WriteFile(tokenFile, []byte("current-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	environ, err := (Config{
		TokenFile: tokenFile, OpenCodeRuntimeDir: openCodeRuntimeDir,
		APIURL: server.URL, Client: server.Client(),
	}).
		PrepareOpenCode(context.Background(), nil, []string{
			"PATH=/usr/bin",
			"OPENCODE_AUTH_CONTENT=stale",
			"XDG_DATA_HOME=/workspace/persisted",
			"OPENCODE_CONFIG=/workspace/repo/opencode.json",
			"OPENCODE_DB=/workspace/opencode.db",
			"OPENCODE_TEST_HOME=/workspace/repo",
			"HOME=/home/devsandbox",
			"GH_TOKEN=different-user",
		})
	if err != nil {
		t.Fatal(err)
	}

	var auth map[string]struct {
		Type    string `json:"type"`
		Refresh string `json:"refresh"`
		Access  string `json:"access"`
		Expires int    `json:"expires"`
	}
	if err := json.Unmarshal([]byte(environmentValue(t, environ, "OPENCODE_AUTH_CONTENT")), &auth); err != nil {
		t.Fatal(err)
	}
	copilot := auth["github-copilot"]
	if copilot.Type != "oauth" || copilot.Refresh != "current-token" ||
		copilot.Access != "current-token" || copilot.Expires != 0 {
		t.Fatalf("unexpected OpenCode auth: %#v", copilot)
	}
	assertEnvironmentValue(t, environ, "HOME", filepath.Join(openCodeRuntimeDir, "home"))
	assertEnvironmentValue(t, environ, "XDG_DATA_HOME", filepath.Join(openCodeRuntimeDir, "data"))
	assertEnvironmentValue(t, environ, "XDG_CACHE_HOME", filepath.Join(openCodeRuntimeDir, "cache"))
	assertEnvironmentValue(t, environ, "XDG_CONFIG_HOME", filepath.Join(openCodeRuntimeDir, "config"))
	assertEnvironmentValue(t, environ, "XDG_STATE_HOME", filepath.Join(openCodeRuntimeDir, "state"))
	assertEnvironmentValue(t, environ, "TMPDIR", filepath.Join(openCodeRuntimeDir, "tmp"))
	assertEnvironmentValue(t, environ, "OPENCODE_DISABLE_PROJECT_CONFIG", "1")
	assertEnvironmentValue(t, environ, "OPENCODE_DISABLE_AUTOUPDATE", "1")
	assertEnvironmentAbsent(t, environ, "OPENCODE_CONFIG")
	assertEnvironmentAbsent(t, environ, "OPENCODE_DB")
	assertEnvironmentAbsent(t, environ, "OPENCODE_TEST_HOME")
	assertEnvironmentAbsent(t, environ, "GH_TOKEN")

	entries, err := os.ReadDir(filepath.Join(openCodeRuntimeDir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("wrapper persisted OpenCode state: %#v", entries)
	}
}

func TestPrepareOpenCodeVersionUsesIsolatedRuntimeWithoutCredential(t *testing.T) {
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	environ, err := (Config{
		TokenFile: filepath.Join(t.TempDir(), "missing"), OpenCodeRuntimeDir: runtimeDir,
	}).PrepareOpenCode(context.Background(), []string{"--version"}, []string{
		"OPENCODE_AUTH_CONTENT=stale",
		"OPENCODE_CONFIG_CONTENT=untrusted",
		"OPENCODE_DB=/workspace/opencode.db",
		"XDG_CACHE_HOME=/workspace/cache",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertEnvironmentAbsent(t, environ, "OPENCODE_AUTH_CONTENT")
	assertEnvironmentAbsent(t, environ, "OPENCODE_CONFIG_CONTENT")
	assertEnvironmentAbsent(t, environ, "OPENCODE_DB")
	assertEnvironmentValue(t, environ, "XDG_CACHE_HOME", filepath.Join(runtimeDir, "cache"))
	assertEnvironmentValue(t, environ, "OPENCODE_DISABLE_PROJECT_CONFIG", "1")
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

func environmentValue(t *testing.T, environ []string, name string) string {
	t.Helper()
	for _, value := range environ {
		if actualName, actualValue, found := strings.Cut(value, "="); found && strings.EqualFold(actualName, name) {
			return actualValue
		}
	}
	t.Fatalf("%s missing from environment", name)
	return ""
}

func assertEnvironmentAbsent(t *testing.T, environ []string, name string) {
	t.Helper()
	for _, value := range environ {
		if strings.HasPrefix(strings.ToUpper(value), strings.ToUpper(name)+"=") {
			t.Fatalf("%s unexpectedly present", name)
		}
	}
}
