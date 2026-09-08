package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStaticTokenProviderBindsImmutableUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := StaticTokenProvider{UserID: "42", TokenFile: path}
	token, expires, err := provider.AccessToken(context.Background(), "42")
	if err != nil || token != "test-token" || expires.IsZero() {
		t.Fatalf("AccessToken() = %q, %v, %v", token, expires, err)
	}
	if _, _, err := provider.AccessToken(context.Background(), "99"); err == nil {
		t.Fatal("static token accepted a different immutable user ID")
	}
}
