package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAuditAllowListAndRedaction(t *testing.T) {
	var output bytes.Buffer
	NewAuditor(&output, "test").Record(AuditEvent{
		Event: "credential.refresh", Outcome: "failure", UserID: "user",
		Reason: `Bearer secret-value refresh_token=also-secret`,
	})
	logged := output.String()
	if strings.Contains(logged, "secret-value") || strings.Contains(logged, "also-secret") {
		t.Fatalf("audit leaked secret: %s", logged)
	}
	var event map[string]any
	if err := json.Unmarshal(output.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "command", "output", "content"} {
		if _, found := event[forbidden]; found {
			t.Fatalf("forbidden field %q is representable", forbidden)
		}
	}
}

func TestLivenessIgnoresDependenciesAndReadinessChecksThem(t *testing.T) {
	probes := Probes{Checks: []Check{{Name: "router", Run: func(context.Context) error {
		return errors.New("down")
	}}}}
	handler := probes.Handler(http.NotFoundHandler(), http.NotFoundHandler())
	for path, want := range map[string]int{"/healthz": http.StatusOK, "/readyz": http.StatusServiceUnavailable} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != want {
			t.Fatalf("%s status = %d, want %d", path, response.Code, want)
		}
	}
}

func TestMetricsArePrometheusCompatible(t *testing.T) {
	metrics := NewMetrics("test")
	metrics.TokenRefreshFailures.Inc()
	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "devsandbox_test_token_refresh_failures_total 1") {
		t.Fatalf("unexpected metrics: %d %s", response.Code, response.Body.String())
	}
}
