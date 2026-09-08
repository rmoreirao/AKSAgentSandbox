package supervisor

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestServerRequiresAuthentication(t *testing.T) {
	server, err := NewServer(NewManager(nil), StaticTokenAuthenticator("correct-token"))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.Handler()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	request.Header.Set("Authorization", "Bearer correct-token")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
}

func TestExecStreamsAndPropagatesExit(t *testing.T) {
	server, err := NewServer(NewManager(nil), StaticTokenAuthenticator("token"))
	if err != nil {
		t.Fatal(err)
	}

	handler, err := server.Handler()
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(execRequest{Command: helperCommand("exit", "19")})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/exec", bytes.NewReader(input))
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	var final streamEvent
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		if err := json.Unmarshal(scanner.Bytes(), &final); err != nil {
			t.Fatal(err)
		}
	}
	if final.Type != "exit" || final.ExitCode == nil || *final.ExitCode != 19 {
		t.Fatalf("final event = %+v", final)
	}
}

func TestWebSocketPingUpdatesActivity(t *testing.T) {
	manager := NewManager(nil)
	server, err := NewServer(manager, StaticTokenAuthenticator("token"))
	if err != nil {
		t.Fatal(err)
	}
	server.PingInterval = 20 * time.Millisecond
	handler, err := server.Handler()
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()

	header := http.Header{}
	header.Set("Authorization", "Bearer token")
	url := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/shell"
	connection, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	go func() {
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for manager.LastActivity().IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("shell did not register activity")
		}
		time.Sleep(10 * time.Millisecond)
	}
	initial := manager.LastActivity()
	time.Sleep(80 * time.Millisecond)
	if !manager.LastActivity().After(initial) {
		t.Fatal("WebSocket ping did not renew activity")
	}
	_ = connection.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
}

func TestPingIntervalCannotExceedSixtySeconds(t *testing.T) {
	server, err := NewServer(NewManager(nil), StaticTokenAuthenticator("token"))
	if err != nil {
		t.Fatal(err)
	}
	server.PingInterval = 61 * time.Second
	if _, err := server.Handler(); err == nil {
		t.Fatal("expected ping interval validation error")
	}
}

func TestActivityAndShutdownInterfaces(t *testing.T) {
	manager := NewManager(nil)
	server, err := NewServer(manager, StaticTokenAuthenticator("token"))
	if err != nil {
		t.Fatal(err)
	}
	shutdown := make(chan struct{}, 1)
	server.OnShutdown = func() { shutdown <- struct{}{} }
	handler, err := server.Handler()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/activity", "/v1/shutdown"} {
		request := httptest.NewRequest(http.MethodPost, path, nil)
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent && response.Code != http.StatusAccepted {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("%s status=%d body=%s", path, response.Code, body)
		}
	}
	if manager.LastActivity().IsZero() {
		t.Fatal("activity endpoint did not update activity")
	}
	select {
	case <-shutdown:
	case <-time.After(time.Second):
		t.Fatal("shutdown callback was not invoked")
	}
}
