package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
)

// AuthHandler owns only the MVP-07 authentication endpoints and can be mounted
// into the management API's router without coupling it to other API work.
type AuthHandler struct {
	Login           *auth.LoginService
	Sessions        auth.SessionManager
	Audit           auth.AuditSink
	StaticTokenFile string
	StaticIdentity  auth.Identity
}

func (h AuthHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/v1/auth/device/start":
		h.start(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/v1/auth/device/poll":
		h.poll(writer, request)
	case request.Method == http.MethodPost && request.URL.Path == "/v1/auth/static/bootstrap":
		h.staticBootstrap(writer, request)
	case request.Method == http.MethodGet && request.URL.Path == "/v1/me":
		h.me(writer, request)
	default:
		authAPIError(writer, http.StatusNotFound, "not_found", "endpoint not found")
	}
}

func (h AuthHandler) staticBootstrap(writer http.ResponseWriter, request *http.Request) {
	if h.StaticTokenFile == "" || h.StaticIdentity.UserID == "" || h.StaticIdentity.Login == "" {
		authAPIError(writer, http.StatusNotFound, "not_found", "endpoint not found")
		return
	}
	token, ok := authBearerToken(request)
	if !ok {
		authAPIError(writer, http.StatusUnauthorized, "unauthenticated", "valid bootstrap credential required")
		return
	}
	expected, err := os.ReadFile(h.StaticTokenFile)
	expectedToken := strings.TrimSpace(string(expected))
	if err != nil || expectedToken == "" ||
		subtle.ConstantTimeCompare([]byte(token), []byte(expectedToken)) != 1 {
		authAPIError(writer, http.StatusUnauthorized, "unauthenticated", "valid bootstrap credential required")
		return
	}
	identity := h.StaticIdentity
	if request.Header.Get("X-DevSandbox-Validation-Identity") == "secondary" {
		identity.UserID += "-secondary"
		identity.Login = "validation-secondary"
	}
	session, expiresAt, err := h.Sessions.Issue(request.Context(), identity)
	if err != nil {
		authAPIError(writer, http.StatusServiceUnavailable, "auth_unavailable", "authentication is unavailable")
		return
	}
	h.audit("login", "success", http.StatusOK, identity)
	_ = json.NewEncoder(writer).Encode(auth.LoginResult{
		SessionToken: session, ExpiresAt: expiresAt, Identity: identity,
	})
}

func (h AuthHandler) start(writer http.ResponseWriter, request *http.Request) {
	if h.Login == nil {
		authAPIError(writer, http.StatusServiceUnavailable, "auth_unavailable", "authentication is unavailable")
		h.audit("login", "failure", http.StatusServiceUnavailable, auth.Identity{})
		return
	}
	start, err := h.Login.Start(request.Context())
	if err != nil {
		authAPIError(writer, http.StatusBadGateway, "github_unavailable", "GitHub authentication is unavailable")
		h.audit("login", "failure", http.StatusBadGateway, auth.Identity{})
		return
	}
	_ = json.NewEncoder(writer).Encode(start)
}

func (h AuthHandler) poll(writer http.ResponseWriter, request *http.Request) {
	if h.Login == nil {
		authAPIError(writer, http.StatusServiceUnavailable, "auth_unavailable", "authentication is unavailable")
		h.audit("login", "failure", http.StatusServiceUnavailable, auth.Identity{})
		return
	}
	var body struct {
		State string `json:"state"`
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || strings.TrimSpace(body.State) == "" {
		authAPIError(writer, http.StatusBadRequest, "invalid_request", "device-flow state is required")
		h.audit("login", "failure", http.StatusBadRequest, auth.Identity{})
		return
	}
	result, err := h.Login.Poll(request.Context(), body.State)
	if err != nil {
		status, code, message := loginError(err)
		authAPIError(writer, status, code, message)
		if status != http.StatusAccepted {
			h.audit("login", "failure", status, auth.Identity{})
		}
		return
	}
	h.audit("login", "success", http.StatusOK, result.Identity)
	_ = json.NewEncoder(writer).Encode(result)
}

func (h AuthHandler) me(writer http.ResponseWriter, request *http.Request) {
	token, ok := authBearerToken(request)
	if !ok {
		authAPIError(writer, http.StatusUnauthorized, "unauthenticated", "valid platform session required")
		return
	}
	claims, err := h.Sessions.Verify(request.Context(), token)
	if err != nil {
		code := "invalid_session"
		if errors.Is(err, auth.ErrExpiredSession) {
			code = "expired_session"
		}
		authAPIError(writer, http.StatusUnauthorized, code, "valid platform session required")
		h.audit("session_validate", "failure", http.StatusUnauthorized, auth.Identity{})
		return
	}
	identity := auth.Identity{UserID: claims.Subject, Login: claims.Login, Org: claims.Org}
	h.audit("session_validate", "success", http.StatusOK, identity)
	_ = json.NewEncoder(writer).Encode(struct {
		GitHubUserID string `json:"githubUserId"`
		GitHubLogin  string `json:"githubLogin"`
	}{
		GitHubUserID: identity.UserID,
		GitHubLogin:  identity.Login,
	})
}

func loginError(err error) (int, string, string) {
	switch {
	case errors.Is(err, auth.ErrAuthorizationPending):
		return http.StatusAccepted, "authorization_pending", "authorization is pending"
	case errors.Is(err, auth.ErrSlowDown):
		return http.StatusTooManyRequests, "slow_down", "polling too quickly"
	case errors.Is(err, auth.ErrInvalidState), errors.Is(err, auth.ErrDeviceFlowExpired):
		return http.StatusBadRequest, "invalid_state", "device-flow state is invalid or expired"
	case errors.Is(err, auth.ErrMembershipRequired):
		return http.StatusForbidden, "organization_membership_required", "active primary-organization membership required"
	default:
		return http.StatusBadGateway, "github_unavailable", "GitHub authentication is unavailable"
	}
}

func authBearerToken(request *http.Request) (string, bool) {
	values := request.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if token == "" || token != strings.TrimSpace(token) || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

func authAPIError(writer http.ResponseWriter, status int, code, message string) {
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"code": code, "message": message})
}

func (h AuthHandler) audit(event, outcome string, status int, identity auth.Identity) {
	if h.Audit == nil {
		return
	}
	h.Audit.Record(auth.AuditEvent{
		Event: event, Outcome: outcome, Status: status, UserID: identity.UserID,
	})
}
