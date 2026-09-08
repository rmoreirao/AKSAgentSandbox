package supervisor

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newWebSocketTestServer(t *testing.T, manager *Manager) (*httptest.Server, http.Header) {
	t.Helper()
	server, err := NewServer(manager, StaticTokenAuthenticator("token"))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := server.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(handler), http.Header{"Authorization": []string{"Bearer token"}}
}

func TestExecWebSocketExitCodeAndCancellation(t *testing.T) {
	manager := NewManager(nil)
	httpServer, header := newWebSocketTestServer(t, manager)
	defer httpServer.Close()
	endpoint := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/exec/stream"
	connection, _, err := websocket.DefaultDialer.Dial(endpoint, header)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteJSON(execRequest{Command: helperCommand("exit", "29")}); err != nil {
		t.Fatal(err)
	}
	var final streamEvent
	for final.Type != "exit" {
		if err := connection.ReadJSON(&final); err != nil {
			t.Fatal(err)
		}
	}
	_ = connection.Close()
	if final.ExitCode == nil || *final.ExitCode != 29 {
		t.Fatalf("exit event = %+v", final)
	}

	connection, _, err = websocket.DefaultDialer.Dial(endpoint, header)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteJSON(execRequest{Command: helperCommand("block")}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for manager.ActivityStatus().ForegroundActive == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = connection.Close()
	for manager.ActivityStatus().ForegroundActive != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if manager.ActivityStatus().ForegroundActive != 0 {
		t.Fatal("closing exec WebSocket did not cancel the remote process")
	}
}

func TestShellResizeControlAndInput(t *testing.T) {
	httpServer, header := newWebSocketTestServer(t, NewManager(nil))
	defer httpServer.Close()
	endpoint := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/shell"
	connection, _, err := websocket.DefaultDialer.Dial(endpoint, header)
	if err != nil {
		t.Fatal(err)
	}

	defer connection.Close()
	if err := connection.WriteJSON(terminalControl{Type: "resize", Rows: 40, Cols: 120}); err != nil {
		t.Fatal(err)
	}

	if err := connection.WriteMessage(websocket.BinaryMessage, []byte("echo resized-shell\r\nexit\r\n")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	for {
		var event streamEvent
		if err := connection.ReadJSON(&event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "stdout" {
			output.Write(event.Data)
		}
		if event.Type == "exit" {
			if event.ExitCode == nil || *event.ExitCode != 0 {
				t.Fatalf("exit event = %+v", event)
			}
			break
		}
	}
	if !strings.Contains(output.String(), "resized-shell") {
		t.Fatalf("shell output = %q", output.String())
	}
}

func TestTunnelCarriesRawBytesAndClosesOnShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(connection, connection)
	}()
	manager := NewManager(nil)
	httpServer, header := newWebSocketTestServer(t, manager)
	defer httpServer.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	endpoint := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/tunnel?port=" + strconv.Itoa(port)
	connection, _, err := websocket.DefaultDialer.Dial(endpoint, header)
	if err != nil {
		t.Fatal(err)
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := connection.ReadMessage(); err == nil {
		t.Fatal("tunnel remained connected after supervisor shutdown")
	}
}

func TestSilentShellRemainsConnectedForTenMinutes(t *testing.T) {
	if os.Getenv("DEVSANDBOX_TEST_SILENT_SHELL_10M") != "1" {
		t.Skip("set DEVSANDBOX_TEST_SILENT_SHELL_10M=1 to run")
	}
	manager := NewManager(nil)
	server, err := NewServer(manager, StaticTokenAuthenticator("token"))
	if err != nil {
		t.Fatal(err)
	}
	server.PingInterval = 30 * time.Second
	handler, err := server.Handler()
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	header := http.Header{"Authorization": []string{"Bearer token"}}
	endpoint := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/shell"
	connection, _, err := websocket.DefaultDialer.Dial(endpoint, header)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	done := make(chan error, 1)
	go func() {
		for {
			if _, _, readErr := connection.ReadMessage(); readErr != nil {
				done <- readErr
				return
			}
		}
	}()
	select {
	case err := <-done:
		t.Fatalf("silent shell disconnected early: %v", err)
	case <-time.After(10 * time.Minute):
	}
}
