package api

import (
	"io"
	"net/http"

	"github.com/rmoreirao/AKSAgentSandbox/internal/gateway"
)

// ExchangeHandler is mounted only on the NetworkPolicy-protected internal
// service. It is deliberately not registered on the public API handler.
type ExchangeHandler struct {
	Credentials gateway.CredentialStore
}

func (h ExchangeHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
		writer.WriteHeader(http.StatusOK)
		return
	}
	if request.Method != http.MethodPost || request.URL.Path != "/v1/vscode/exchange" {
		writeAPIError(writer, http.StatusNotFound, "not_found", "endpoint not found", nil)
		return
	}
	value, err := io.ReadAll(io.LimitReader(request.Body, 1024))
	if err != nil || len(value) == 0 || len(value) > 512 {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "one-time credential is required", nil)
		return
	}
	claim, err := h.Credentials.Consume(request.Context(), string(value))
	if err != nil {
		writeAPIError(writer, http.StatusUnauthorized, "invalid_one_time_credential", "one-time credential is invalid or expired", nil)
		return
	}
	writer.Header().Set("Content-Type", "text/plain")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, claim)
}
