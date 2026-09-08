package observability

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)("(?:access_token|refresh_token|sessionToken|token|device_code|client_secret)"\s*:\s*")[^"]*(")`),
	regexp.MustCompile(`(?i)((?:access_token|refresh_token|sessionToken|token|device_code|client_secret)=)[^&\s]*`),
}

func Redact(value string) string {
	redacted := secretPatterns[0].ReplaceAllString(value, "******")
	for _, pattern := range secretPatterns[1:] {
		redacted = pattern.ReplaceAllString(redacted, "${1}[REDACTED]${2}")
	}
	return redacted
}

// AuditEvent contains an allow-list of operational metadata. It deliberately
// cannot represent tokens, commands, process output, or file content.
type AuditEvent struct {
	Time        time.Time
	Event       string
	Outcome     string
	Status      int
	RequestID   string
	UserID      string
	Namespace   string
	SandboxUID  string
	SandboxName string
	Template    string
	Profile     string
	Repository  string
	CommitSHA   string
	Session     string
	JobID       string
	Reason      string
}

type AuditSink interface {
	Record(AuditEvent)
}

type Auditor struct {
	logger *slog.Logger
	now    func() time.Time
}

func NewAuditor(writer io.Writer, service string) *Auditor {
	handler := slog.NewJSONHandler(writer, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if value, ok := attr.Value.Any().(string); ok {
				attr.Value = slog.StringValue(Redact(value))
			}
			return attr
		},
	})
	return &Auditor{logger: slog.New(handler).With("service", service)}
}

func (a *Auditor) Record(event AuditEvent) {
	if a == nil || a.logger == nil || strings.TrimSpace(event.Event) == "" {
		return
	}
	if event.Time.IsZero() {
		event.Time = time.Now().UTC()
		if a.now != nil {
			event.Time = a.now().UTC()
		}
	}
	attrs := []slog.Attr{
		slog.Time("event_time", event.Time),
		slog.String("event", event.Event),
		slog.String("outcome", event.Outcome),
	}
	attrs = appendString(attrs, "request_id", event.RequestID)
	attrs = appendString(attrs, "user_id", event.UserID)
	attrs = appendString(attrs, "namespace", event.Namespace)
	attrs = appendString(attrs, "sandbox_uid", event.SandboxUID)
	attrs = appendString(attrs, "sandbox_name", event.SandboxName)
	attrs = appendString(attrs, "template", event.Template)
	attrs = appendString(attrs, "profile", event.Profile)
	attrs = appendString(attrs, "repository", event.Repository)
	attrs = appendString(attrs, "commit_sha", event.CommitSHA)
	attrs = appendString(attrs, "session", event.Session)
	attrs = appendString(attrs, "job_id", event.JobID)
	attrs = appendString(attrs, "reason", event.Reason)
	if event.Status != 0 {
		attrs = append(attrs, slog.Int("status", event.Status))
	}
	a.logger.LogAttrs(context.Background(), slog.LevelInfo, "audit", attrs...)
}

func appendString(attrs []slog.Attr, key, value string) []slog.Attr {
	if value != "" {
		return append(attrs, slog.String(key, value))
	}
	return attrs
}
