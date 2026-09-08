package gateway

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketProxyPreservesRawBinaryBytes(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		messageType, data, err := connection.ReadMessage()
		if err == nil {
			_ = connection.WriteMessage(messageType, data)
		}
	}))
	defer backend.Close()
	routerURL, _ := url.Parse(backend.URL)
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		WebSocketProxy{Router: DirectRouter{BaseURL: routerURL}}.ServeHTTP(
			writer, request, RouteTarget{ID: "sandbox", Namespace: "workloads", Port: 8081},
		)
	}))
	defer proxy.Close()
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxy.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	payload := []byte{0x00, 0xff, 0x01, '\r', '\n', 'N', 'O', 'T', ' ', 'H', 'T', 'T', 'P'}
	if err := connection.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatal(err)
	}
	messageType, echoed, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if messageType != websocket.BinaryMessage || !bytes.Equal(echoed, payload) {
		t.Fatalf("echoed type=%d bytes=%v", messageType, echoed)
	}
}

func TestWebSocketProxyPreservesStopCloseCode(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		_ = connection.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "sandbox stopping"),
			time.Now().Add(time.Second),
		)
	}))
	defer backend.Close()
	routerURL, _ := url.Parse(backend.URL)
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		WebSocketProxy{Router: DirectRouter{BaseURL: routerURL}}.ServeHTTP(
			writer, request, RouteTarget{ID: "sandbox", Namespace: "workloads", Port: 8081},
		)
	}))
	defer proxy.Close()
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(proxy.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_, _, err = connection.ReadMessage()
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseGoingAway {
		t.Fatalf("close error = %v", err)
	}
}
