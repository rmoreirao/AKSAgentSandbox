package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rmoreirao/AKSAgentSandbox/internal/observability"
)

const CookieName = "__Host-devsandbox-route"

type CredentialExchanger interface {
	Exchange(context.Context, string) (string, error)
}

type HTTPExchanger struct {
	URL    string
	Client *http.Client
}

func (e HTTPExchanger) Exchange(ctx context.Context, credential string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, bytes.NewBufferString(credential))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "text/plain")
	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return "", errors.New("credential exchange rejected")
	}
	claim, err := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	if err != nil || len(claim) == 0 {
		return "", errors.New("credential exchange returned no route claim")
	}
	return string(claim), nil
}

type WebHandler struct {
	Claims    ClaimManager
	Exchange  CredentialExchanger
	Router    HTTPRouter
	CookieTTL time.Duration
	Audit     observability.AuditSink
	Metrics   *observability.Metrics
}

func (h WebHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/healthz":
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusOK)
	case request.Method == http.MethodGet && request.URL.Path == "/bootstrap":
		serveBootstrap(writer)
	case request.Method == http.MethodPost && request.URL.Path == "/exchange":
		h.exchange(writer, request)
	default:
		h.proxy(writer, request)
	}
}

func serveBootstrap(writer http.ResponseWriter) {
	nonceRaw := make([]byte, 18)
	if _, err := rand.Read(nonceRaw); err != nil {
		http.Error(writer, "bootstrap unavailable", http.StatusServiceUnavailable)
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceRaw)
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	writer.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'nonce-"+nonce+"'; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(writer, `<!doctype html><meta charset="utf-8"><title>Opening sandbox</title>`+
		`<script nonce="`+nonce+`">`+
		`const p=new URLSearchParams(location.hash.slice(1));const c=p.get("credential");`+
		`history.replaceState(null,"","/bootstrap");`+
		`if(!c){document.body.textContent="Invalid or expired link";}else{`+
		`fetch("/exchange",{method:"POST",headers:{"Content-Type":"text/plain"},body:c,credentials:"same-origin",referrerPolicy:"no-referrer"})`+
		`.then(r=>{if(!r.ok)throw 0;location.replace("/")}).catch(()=>document.body.textContent="Invalid or expired link");}`+
		`</script>`)
}

func (h WebHandler) exchange(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if h.Exchange == nil {
		http.Error(writer, "exchange unavailable", http.StatusServiceUnavailable)
		return
	}
	credential, err := io.ReadAll(io.LimitReader(request.Body, 1024))
	if err != nil || len(credential) == 0 || len(credential) > 512 {
		http.Error(writer, "invalid credential", http.StatusBadRequest)
		return
	}
	claim, err := h.Exchange.Exchange(request.Context(), string(credential))
	if err != nil {
		http.Error(writer, "invalid or expired credential", http.StatusUnauthorized)
		return
	}
	claims, err := h.Claims.Verify(request.Context(), claim)
	if err != nil {
		http.Error(writer, "invalid route claim", http.StatusUnauthorized)
		return
	}
	ttl := h.CookieTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	http.SetCookie(writer, &http.Cookie{
		Name: CookieName, Value: claim, Path: "/", MaxAge: int(ttl.Seconds()),
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	h.audit(claims, "session.start", "vscode")
	writer.WriteHeader(http.StatusNoContent)
}

func (h WebHandler) proxy(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	cookie, err := request.Cookie(CookieName)
	if err != nil {
		http.Error(writer, "route authentication required", http.StatusUnauthorized)
		return
	}
	claim, err := h.Claims.Verify(request.Context(), cookie.Value)
	if err != nil {
		http.Error(writer, "route authentication required", http.StatusUnauthorized)
		return
	}
	if h.Router == nil {
		http.Error(writer, "sandbox unavailable", http.StatusBadGateway)
		return
	}
	removeCookie(request.Header, CookieName)
	h.audit(claim, "session.start", "vscode")
	if h.Metrics != nil {
		h.Metrics.ActiveConnections.WithLabelValues("vscode").Inc()
		defer h.Metrics.ActiveConnections.WithLabelValues("vscode").Dec()
	}
	defer h.audit(claim, "session.end", "vscode")
	h.Router.ServeRoute(writer, request, RouteTarget{
		ID: claim.RouterID, UID: claim.SandboxUID, Namespace: claim.Namespace, Port: claim.Port,
	})
}

func (h WebHandler) audit(claim RouteClaim, event, session string) {
	if h.Audit == nil {
		return
	}
	h.Audit.Record(observability.AuditEvent{
		Event: event, Outcome: "success", UserID: claim.OwnerID,
		Namespace: claim.Namespace, SandboxUID: claim.SandboxUID,
		SandboxName: claim.SandboxName, Session: session,
	})
}

func removeCookie(header http.Header, name string) {
	var kept []string
	for _, cookie := range strings.Split(header.Get("Cookie"), ";") {
		cookie = strings.TrimSpace(cookie)
		if cookie == "" || strings.HasPrefix(cookie, name+"=") {
			continue
		}
		kept = append(kept, cookie)
	}
	if len(kept) == 0 {
		header.Del("Cookie")
		return
	}
	header.Set("Cookie", strings.Join(kept, "; "))
}
