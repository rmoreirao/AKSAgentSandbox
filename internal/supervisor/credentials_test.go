package supervisor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCredentialFileLifecycle(t *testing.T) {
	file := CredentialFile{RuntimeDir: t.TempDir(), Name: "github-token"}
	if err := file.Write([]byte("short-lived-value")); err != nil {
		t.Fatal(err)
	}
	value, err := file.Read()
	if err != nil || string(value) != "short-lived-value" {
		t.Fatalf("read value=%q error=%v", value, err)
	}
	if err := file.Write([]byte("rotated-value")); err != nil {
		t.Fatal(err)
	}
	value, err = file.Read()
	if err != nil || string(value) != "rotated-value" {
		t.Fatalf("read rotated value=%q error=%v", value, err)
	}
	if err := file.Remove(); err != nil {
		t.Fatal(err)
	}

	if _, err := file.Read(); err == nil {
		t.Fatal("expected removed credential to be unavailable")
	}
}

func TestBrokerClientUsesProjectedTokenWithoutLeakingIt(t *testing.T) {
	const projected = "projected-secret-value"
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer "+projected {
			t.Errorf("authorization header = %q", got)
		}
		fmt.Fprint(writer, `{"token":"current-runtime-value"}`)
	}))
	defer server.Close()
	tokenPath := filepath.Join(t.TempDir(), "projected-token")
	if err := os.WriteFile(tokenPath, []byte(projected+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := BrokerClient{
		Config: RuntimeIdentityConfig{
			ServiceAccountName: "devsandbox-uid",
			BrokerAudience:     "devsandbox-broker",
			BrokerTokenFile:    tokenPath,
			BrokerURL:          server.URL,
		},
		Client: server.Client(),
	}
	value, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != "current-runtime-value" {
		t.Fatalf("credential = %q", value)
	}
}

func TestCredentialFileRejectsWorkspaceAndSymlink(t *testing.T) {
	workspacePath := "/workspace/.credentials"
	if runtime.GOOS == "windows" {
		workspacePath = `\workspace\.credentials`
	}
	if err := (CredentialFile{RuntimeDir: workspacePath, Name: "token"}).Write([]byte("value")); err == nil {
		t.Fatal("expected workspace path rejection")
	}

	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires optional Windows privileges")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "token")); err != nil {
		t.Fatal(err)
	}
	if err := (CredentialFile{RuntimeDir: dir, Name: "token"}).Write([]byte("new")); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestRuntimeIdentityValidation(t *testing.T) {
	valid := RuntimeIdentityConfig{
		ServiceAccountName: "devsandbox-uid",
		BrokerAudience:     "devsandbox-credential-broker",
		BrokerTokenFile:    DefaultBrokerTokenFile,
		BrokerURL:          "https://devsandbox-broker.dev.svc/token",
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	valid.AutomountToken = true
	if err := valid.Validate(); err == nil {
		t.Fatal("expected automatic token mount rejection")
	}
}

func TestEnsureBearerTokenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-token")
	if err := EnsureBearerToken(path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBearerToken(path); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) || len(first) < 32 {
		t.Fatal("agent token was changed or is too short")
	}
}
