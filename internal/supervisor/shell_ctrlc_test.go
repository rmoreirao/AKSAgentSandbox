package supervisor

import (
	"bytes"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestShellCtrlCInterruptsForegroundCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows fallback terminal cannot synthesize console Ctrl+C")
	}
	httpServer, header := newWebSocketTestServer(t, NewManager(nil))
	defer httpServer.Close()
	endpoint := "ws" + strings.TrimPrefix(httpServer.URL, "http") + "/v1/shell"
	connection, _, err := websocket.DefaultDialer.Dial(endpoint, header)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.WriteMessage(websocket.BinaryMessage, []byte("sleep 30\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := connection.WriteMessage(websocket.BinaryMessage, []byte{3}); err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteMessage(websocket.BinaryMessage, []byte("echo ctrl-c-ok\nexit\n")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	for {
		var event streamEvent
		if err := connection.ReadJSON(&event); err != nil {
			t.Fatal(err)
		}
		output.Write(event.Data)
		if event.Type == "exit" {
			break
		}
	}
	if !strings.Contains(output.String(), "ctrl-c-ok") {
		t.Fatalf("Ctrl+C did not return control to shell: %q", output.String())
	}
}
