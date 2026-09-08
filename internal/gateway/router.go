package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	HeaderSandboxID        = "X-Sandbox-Id"
	HeaderSandboxUID       = "X-Sandbox-Uid"
	HeaderSandboxNamespace = "X-Sandbox-Namespace"
	HeaderSandboxPort      = "X-Sandbox-Port"
)

type RouteTarget struct {
	ID        string
	UID       string
	Namespace string
	Port      int
	Path      string
	RawQuery  string
}

func (t RouteTarget) validate() error {
	if t.ID == "" || t.Namespace == "" || t.Port < 1 || t.Port > 65535 {
		return errors.New("invalid sandbox route target")
	}
	return nil
}

type HTTPRouter interface {
	ServeRoute(http.ResponseWriter, *http.Request, RouteTarget)
}

type WebSocketRouter interface {
	Dial(context.Context, RouteTarget, http.Header) (*websocket.Conn, *http.Response, error)
}

type DirectRouter struct {
	BaseURL   *url.URL
	Transport http.RoundTripper
	Dialer    *websocket.Dialer
}

func (r DirectRouter) ServeRoute(writer http.ResponseWriter, request *http.Request, target RouteTarget) {
	if r.BaseURL == nil || target.validate() != nil {
		http.Error(writer, "sandbox route unavailable", http.StatusBadGateway)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(r.BaseURL)
	proxy.Transport = r.Transport
	original := proxy.Director
	proxy.Director = func(out *http.Request) {
		original(out)
		setRouteHeaders(out.Header, target)
		out.Header.Del("Authorization")
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "sandbox route unavailable", http.StatusBadGateway)
	}
	proxy.ServeHTTP(writer, request)
}

func (r DirectRouter) Dial(ctx context.Context, target RouteTarget, header http.Header) (*websocket.Conn, *http.Response, error) {
	if r.BaseURL == nil || target.validate() != nil {
		return nil, nil, errors.New("sandbox route unavailable")
	}
	endpoint := *r.BaseURL
	switch endpoint.Scheme {
	case "http":
		endpoint.Scheme = "ws"
	case "https":
		endpoint.Scheme = "wss"
	}
	if target.Path != "" {
		endpoint.Path = target.Path
		endpoint.RawPath = ""
	}
	endpoint.RawQuery = target.RawQuery
	outHeader := header.Clone()
	outHeader.Del("Authorization")
	setRouteHeaders(outHeader, target)
	dialer := r.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	return dialer.DialContext(ctx, endpoint.String(), outHeader)
}

func setRouteHeaders(header http.Header, target RouteTarget) {
	for _, name := range []string{HeaderSandboxID, HeaderSandboxUID, HeaderSandboxNamespace, HeaderSandboxPort} {
		header.Del(name)
	}
	header.Set(HeaderSandboxID, target.ID)
	header.Set(HeaderSandboxNamespace, target.Namespace)
	header.Set(HeaderSandboxPort, strconv.Itoa(target.Port))
	if target.UID != "" {
		header.Set(HeaderSandboxUID, target.UID)
	}
}

type WebSocketProxy struct {
	Router       WebSocketRouter
	Upgrader     websocket.Upgrader
	PingInterval time.Duration
	OnActivity   func()
	OnClose      func()
}

func (p WebSocketProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request, target RouteTarget) {
	if p.Router == nil || target.validate() != nil {
		http.Error(writer, "sandbox route unavailable", http.StatusBadGateway)
		return
	}
	upgrader := p.Upgrader
	if upgrader.CheckOrigin == nil {
		upgrader.CheckOrigin = func(r *http.Request) bool { return r.Header.Get("Origin") == "" }
	}
	client, err := upgrader.Upgrade(writer, request, nil)
	if err != nil {
		return
	}
	defer client.Close()
	defer func() {
		if p.OnClose != nil {
			p.OnClose()
		}
	}()
	headers := request.Header.Clone()
	for _, name := range []string{"Connection", "Upgrade", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions"} {
		headers.Del(name)
	}
	backend, response, err := p.Router.Dial(request.Context(), target, headers)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		_ = client.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "sandbox unavailable"),
			time.Now().Add(time.Second))
		return
	}
	defer backend.Close()

	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			cancel()
			_ = client.Close()
			_ = backend.Close()
		})
	}
	done := make(chan struct{}, 2)
	go copyWebSocket(ctx, backend, client, closeBoth, done)
	go copyWebSocket(ctx, client, backend, closeBoth, done)
	pingInterval := p.PingInterval
	if pingInterval <= 0 {
		pingInterval = 30 * time.Second
	}
	if pingInterval > 60*time.Second {
		pingInterval = 60 * time.Second
	}
	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-pingDone:
				return
			case <-ticker.C:
				deadline := time.Now().Add(pingInterval)
				if client.WriteControl(websocket.PingMessage, nil, deadline) != nil ||
					backend.WriteControl(websocket.PingMessage, nil, deadline) != nil {
					closeBoth()
					return
				}
				if p.OnActivity != nil {
					p.OnActivity()
				}
			}
		}
	}()
	if p.OnActivity != nil {
		p.OnActivity()
	}
	<-done
	closeBoth()
	<-done
}

func copyWebSocket(ctx context.Context, destination, source *websocket.Conn, closeBoth func(), done chan<- struct{}) {
	defer func() { done <- struct{}{} }()
	for {
		messageType, reader, err := source.NextReader()
		if err != nil {
			var closeErr *websocket.CloseError
			if errors.As(err, &closeErr) {
				_ = destination.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(closeErr.Code, closeErr.Text),
					time.Now().Add(time.Second),
				)
			}
			return
		}
		writer, err := destination.NextWriter(messageType)
		if err != nil {
			return
		}
		_, copyErr := io.Copy(writer, reader)
		closeErr := writer.Close()
		if copyErr != nil || closeErr != nil {
			return
		}
		select {
		case <-ctx.Done():
			closeBoth()
			return
		default:
		}
	}
}

func ParseRouterURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("router URL must be an absolute HTTP(S) URL")
	}
	return parsed, nil
}
