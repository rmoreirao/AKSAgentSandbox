package auth

import (
	"io"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/rmoreirao/AKSAgentSandbox/internal/observability"
)

func RedactSecrets(value string) string {
	return observability.Redact(value)
}

type redactingWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *redactingWriter) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err := io.WriteString(w.writer, RedactSecrets(string(value)))
	if err != nil {
		return 0, err
	}
	return len(value), nil
}

func NewRedactingLogger(writer io.Writer, prefix string, flags int) *log.Logger {
	return log.New(&redactingWriter{writer: writer}, prefix, flags)
}

type AuditEvent = observability.AuditEvent
type AuditSink = observability.AuditSink
type JSONAuditLogger = observability.Auditor

func NewJSONAuditLogger(writer io.Writer) *JSONAuditLogger {
	return observability.NewAuditor(&redactingWriter{writer: writer}, "devsandbox")
}

type auditResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditResponseWriter) Write(value []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(value)
}

// RedactionMiddleware records only fixed metadata. It intentionally excludes
// headers, query strings, and request/response bodies.
func RedactionMiddleware(next http.Handler, audit AuditSink) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		wrapped := &auditResponseWriter{ResponseWriter: writer}
		next.ServeHTTP(wrapped, request)
		if audit == nil {
			return
		}
		status := wrapped.status
		if status == 0 {
			status = http.StatusOK
		}
		outcome := "success"
		if status >= 400 {
			outcome = "failure"
		}
		audit.Record(AuditEvent{
			Event: "http.request", Outcome: outcome, Status: status,
			RequestID: strings.TrimSpace(request.Header.Get("X-Request-ID")),
		})
	})
}
