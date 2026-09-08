package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type staticAzureToken string

func (t staticAzureToken) Token(context.Context) (string, error) { return string(t), nil }

func TestKeyVaultStoreUsesHashedUserKeyAndVersions(t *testing.T) {
	const userID = "987654321"
	const refresh = "sensitive-refresh"
	var secretPath string
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		secretPath = request.URL.Path
		if request.Header.Get("Authorization") != "Bearer vault-access" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case http.MethodGet:
			fmt.Fprintf(writer, `{"value":%q,"id":"https://vault.example%s/version-1"}`, refresh, request.URL.Path)
		case http.MethodPut:
			fmt.Fprintf(writer, `{"id":"https://vault.example%s/version-2"}`, request.URL.Path)
		}
	}))
	defer server.Close()
	store := &KeyVaultRefreshCredentialStore{
		VaultURL: server.URL, Tokens: staticAzureToken("vault-access"), Client: server.Client(),
	}
	value, err := store.Read(context.Background(), userID)
	if err != nil || value.Value != refresh || value.Version != "version-1" {
		t.Fatalf("Read() = %#v, %v", value, err)
	}
	if strings.Contains(secretPath, userID) {
		t.Fatalf("raw immutable user ID appeared in Key Vault name: %s", secretPath)
	}
	version, err := store.Write(context.Background(), userID, "rotated")
	if err != nil || version != "version-2" {
		t.Fatalf("Write() = %q, %v", version, err)
	}
}

func TestKeyVaultStorePropagatesSafeHTTPError(t *testing.T) {
	const responseSecret = "response-must-not-leak"
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, responseSecret, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	store := &KeyVaultRefreshCredentialStore{
		VaultURL: server.URL, Tokens: staticAzureToken("vault-access"), Client: server.Client(),
	}
	_, err := store.Read(context.Background(), "1")
	if err == nil || strings.Contains(err.Error(), responseSecret) {
		t.Fatalf("unsafe Key Vault error = %v", err)
	}
}
