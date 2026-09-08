package broker

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
	"github.com/rmoreirao/AKSAgentSandbox/internal/observability"
)

type Handler struct {
	Service *Service
	Audit   auth.AuditSink
	Metrics *observability.Metrics
}

func (h Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
		_ = json.NewEncoder(writer).Encode(map[string]string{"status": "ok"})
		return
	}
	if request.Method != http.MethodPost || request.URL.Path != "/v1/credential" {
		writeError(writer, http.StatusNotFound, "not_found", "endpoint not found")
		return
	}
	projected, ok := bearerToken(request)
	if !ok {
		writeError(writer, http.StatusUnauthorized, "unauthenticated", "valid projected identity required")
		return
	}
	if h.Service == nil {
		writeError(writer, http.StatusServiceUnavailable, "broker_unavailable", "credential broker is unavailable")
		return
	}
	token, _, identity, err := h.Service.Credential(request.Context(), projected)
	if err != nil {
		status, code, message := brokerError(err)
		writeError(writer, status, code, message)
		if h.Audit != nil {
			h.Audit.Record(auth.AuditEvent{
				Event: "credential.refresh", Outcome: "failure", Status: status,
				Namespace: identity.Namespace, SandboxUID: identity.SandboxUID,
			})
		}
		if h.Metrics != nil {
			h.Metrics.TokenRefreshFailures.Inc()
		}
		return
	}
	if h.Audit != nil {
		h.Audit.Record(auth.AuditEvent{
			Event: "credential.refresh", Outcome: "success", Status: http.StatusOK,
			UserID: identity.OwnerUserID, Namespace: identity.Namespace, SandboxUID: identity.SandboxUID,
		})
	}
	_ = json.NewEncoder(writer).Encode(map[string]string{"token": token})
}

func bearerToken(request *http.Request) (string, bool) {
	values := request.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

func brokerError(err error) (int, string, string) {
	switch {
	case errors.Is(err, ErrUnauthenticated):
		return http.StatusUnauthorized, "unauthenticated", "valid projected identity required"
	case errors.Is(err, ErrBindingMismatch):
		return http.StatusForbidden, "binding_mismatch", "projected identity does not match a sandbox"
	case errors.Is(err, auth.ErrReauthentication), errors.Is(err, auth.ErrCredentialNotFound):
		return http.StatusUnauthorized, "reauthentication_required", "GitHub reauthentication required"
	default:
		return http.StatusBadGateway, "credential_unavailable", "GitHub credential is temporarily unavailable"
	}
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]string{"code": code, "message": message})
}
