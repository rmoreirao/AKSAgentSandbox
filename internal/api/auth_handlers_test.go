package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
)

func TestMeHandlerRejectsExpiredSession(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	sessions := auth.SessionManager{
		Signer:   auth.HMACSigner{Key: []byte("01234567890123456789012345678901")},
		Audience: "api", Lifetime: time.Minute, Now: func() time.Time { return now },
	}
	token, _, err := sessions.Issue(context.Background(), auth.Identity{UserID: "1", Login: "user", Org: "primary"})
	if err != nil {
		t.Fatal(err)
	}

	sessions.Now = func() time.Time { return now.Add(time.Minute) }
	handler := AuthHandler{Sessions: sessions}
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["code"] != "expired_session" || strings.Contains(response.Body.String(), token) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestMeHandlerUsesPublishedUserSchema(t *testing.T) {
	sessions := auth.SessionManager{
		Signer:   auth.HMACSigner{Key: []byte("01234567890123456789012345678901")},
		Audience: "api", Lifetime: time.Minute,
	}
	token, _, err := sessions.Issue(context.Background(), auth.Identity{UserID: "42", Login: "owner", Org: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	handler := AuthHandler{Sessions: sessions}
	request := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["githubUserId"] != "42" || body["githubLogin"] != "owner" ||
		body["userId"] != nil || body["login"] != nil {
		t.Fatalf("body = %#v", body)
	}
}

func TestStaticBootstrapRequiresExactCredentialAndIssuesSession(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("bootstrap-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions := auth.SessionManager{
		Signer:   auth.HMACSigner{Key: []byte("01234567890123456789012345678901")},
		Audience: "api", Lifetime: time.Hour,
	}
	handler := AuthHandler{
		Sessions: sessions, StaticTokenFile: tokenPath,
		StaticIdentity: auth.Identity{UserID: "42", Login: "owner", Org: "owner"},
	}
	for credential, want := range map[string]int{"wrong": http.StatusUnauthorized, "bootstrap-secret": http.StatusOK} {
		request := httptest.NewRequest(http.MethodPost, "/v1/auth/static/bootstrap", nil)
		request.Header.Set("Authorization", "Bearer "+credential)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("credential %q status = %d, want %d", credential, response.Code, want)
		}
		if credential == "bootstrap-secret" {
			var result auth.LoginResult
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil ||
				result.SessionToken == "" || result.Identity.UserID != "42" {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/auth/static/bootstrap", nil)
			request.Header.Set("Authorization", "Bearer bootstrap-secret")
			request.Header.Set("X-DevSandbox-Validation-Identity", "secondary")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			var secondary auth.LoginResult
			if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &secondary) != nil ||
				secondary.Identity.UserID != "42-secondary" || secondary.Identity.Login != "validation-secondary" {
				t.Fatalf("secondary result = %#v, status = %d", secondary, response.Code)
			}
		}
	}
}
