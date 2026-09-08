package auth

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedactionRemovesTokensFromLogsAndAudit(t *testing.T) {
	const access = "ghu_very-secret-access-token"
	const refresh = "ghr_very-secret-refresh-token"
	var output bytes.Buffer
	logger := NewRedactingLogger(&output, "", 0)
	logger.Printf(`Authorization: Bearer %s {"refresh_token":"%s"}`, access, refresh)
	audit := NewJSONAuditLogger(&output)
	handler := RedactionMiddleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "done", http.StatusUnauthorized)
	}), audit)
	request := httptest.NewRequest(http.MethodGet, "https://api.example/v1/me?token="+access, nil)
	request.Header.Set("Authorization", "Bearer "+access)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	logged := output.String()
	if strings.Contains(logged, access) || strings.Contains(logged, refresh) {
		t.Fatalf("secret appeared in output: %s", logged)
	}
	if !strings.Contains(logged, "[REDACTED]") {
		t.Fatalf("redaction marker missing: %s", logged)
	}
	if strings.Contains(logged, request.URL.RawQuery) {
		t.Fatalf("audit included query string: %s", logged)
	}
}
