package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRouteClaimTamperingAndSandboxBinding(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	manager := ClaimManager{
		Signer: auth.HMACSigner{Key: []byte("01234567890123456789012345678901")},
		Now:    func() time.Time { return now },
	}
	token, err := manager.Issue(context.Background(), RouteClaim{
		OwnerID: "1", SandboxName: "mine", SandboxUID: "uid-a",
		RouterID: "upstream", Namespace: "workloads", Port: 8080,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifySandbox(context.Background(), token, "mine", "uid-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.VerifySandbox(context.Background(), token, "other", "uid-b"); err == nil {
		t.Fatal("cross-sandbox claim was accepted")
	}
	parts := strings.Split(token, ".")
	parts[1] = "e30"
	if _, err := manager.Verify(context.Background(), strings.Join(parts, ".")); err == nil {
		t.Fatal("tampered route claim was accepted")
	}
}

func TestKubernetesCredentialConsumptionIsAtomicAcrossReplicas(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	first := &KubernetesCredentialStore{Client: client, Namespace: "system"}
	second := &KubernetesCredentialStore{Client: client, Namespace: "system"}
	credential, _, err := first.Issue(context.Background(), "signed-route", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wait sync.WaitGroup
	for _, store := range []*KubernetesCredentialStore{first, second} {
		wait.Add(1)
		go func(store *KubernetesCredentialStore) {
			defer wait.Done()
			if claim, err := store.Consume(context.Background(), credential); err == nil && claim == "signed-route" {
				successes.Add(1)
			}
		}(store)
	}
	wait.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful consumers = %d, want 1", successes.Load())
	}
	if _, err := first.Consume(context.Background(), credential); err == nil {
		t.Fatal("credential reuse succeeded")
	}
}

func TestBootstrapSecurityAndHistoryCleanup(t *testing.T) {
	response := httptest.NewRecorder()
	serveBootstrap(response)
	body := response.Body.String()
	if response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("Referrer-Policy") != "no-referrer" ||
		response.Header().Get("Permissions-Policy") == "" ||
		response.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.Contains(response.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("insecure headers: %v", response.Header())
	}
	if strings.Contains(body, "http://") || strings.Contains(body, "https://") {
		t.Fatal("bootstrap includes a third-party resource")
	}
	history := strings.Index(body, "history.replaceState")
	exchange := strings.Index(body, `fetch("/exchange"`)
	if history < 0 || exchange < 0 || history > exchange {
		t.Fatal("fragment is not removed from history before exchange")
	}
	if !strings.Contains(body, `credentials:"same-origin"`) ||
		!strings.Contains(body, `referrerPolicy:"no-referrer"`) {
		t.Fatal("bootstrap exchange does not constrain credentials and referrers")
	}
}

func TestBootstrapDoesNotAcceptCredentialOutsideFragment(t *testing.T) {
	handler := WebHandler{}
	for _, target := range []string{
		"https://sandbox.example/bootstrap?credential=must-not-be-consumed",
		"https://sandbox.example/credential/must-not-be-consumed",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if strings.Contains(response.Body.String(), "must-not-be-consumed") {
			t.Fatal("credential from a logged URL surface was reflected")
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("browser response is cacheable")
		}
	}
}

type staticExchanger string

func (s staticExchanger) Exchange(context.Context, string) (string, error) { return string(s), nil }

func TestWebExchangeCookieIsHostOnly(t *testing.T) {
	claims := ClaimManager{Signer: auth.HMACSigner{Key: []byte("01234567890123456789012345678901")}}
	token, err := claims.Issue(context.Background(), RouteClaim{
		OwnerID: "1", SandboxName: "mine", SandboxUID: "uid",
		RouterID: "upstream", Namespace: "workloads", Port: 8080,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := WebHandler{Claims: claims, Exchange: staticExchanger(token)}
	request := httptest.NewRequest(http.MethodPost, "https://sandbox.example/exchange", strings.NewReader("credential"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Domain != "" || cookie.Path != "/" || !cookie.Secure || !cookie.HttpOnly ||
		cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("insecure cookie: %#v", cookie)
	}
}

func TestWebSocketDisconnectCleansBothSides(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	backendClosed := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}

		defer connection.Close()
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				close(backendClosed)
				return
			}
		}
	}))
	defer backend.Close()
	routerURL, _ := url.Parse(backend.URL)
	var cleanup atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		WebSocketProxy{
			Router:  DirectRouter{BaseURL: routerURL},
			OnClose: func() { cleanup.Add(1) },
		}.ServeHTTP(writer, request, RouteTarget{ID: "sandbox", Namespace: "workloads", Port: 8081})
	}))
	defer proxy.Close()
	endpoint := "ws" + strings.TrimPrefix(proxy.URL, "http")
	connection, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	select {
	case <-backendClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("backend connection was not closed")
	}
	deadline := time.Now().Add(time.Second)
	for cleanup.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cleanup.Load() != 1 {
		t.Fatalf("cleanup calls = %d", cleanup.Load())
	}
}
