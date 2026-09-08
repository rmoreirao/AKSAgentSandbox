package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/rmoreirao/AKSAgentSandbox/internal/gateway"
	"github.com/rmoreirao/AKSAgentSandbox/internal/supervisor"
)

func TestCLIConnectivityHelperProcess(t *testing.T) {
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}
	switch os.Args[separator+1] {
	case "exit":
		code, _ := strconv.Atoi(os.Args[separator+2])
		os.Exit(code)
	case "stream":
		_, _ = os.Stdout.Write([]byte{0x00, 0xff, 'O', 'K'})
		_, _ = os.Stderr.Write([]byte("text-error"))
	case "block":
		for {
			time.Sleep(time.Hour)
		}
	}
}

func cliHelperCommand(mode string, arguments ...string) execRequest {
	return execRequest{Command: remoteCommand{
		Executable: os.Args[0],
		Arguments:  append([]string{"-test.run=TestCLIConnectivityHelperProcess", "--", mode}, arguments...),
	}}
}

func connectivityTestClient(t *testing.T) (*Client, func()) {
	t.Helper()
	manager := supervisor.NewManager(nil)
	agent, err := supervisor.NewServer(manager, supervisor.RouterAuthenticator("sandbox-uid"))
	if err != nil {
		t.Fatal(err)
	}
	agentHandler, err := agent.Handler()
	if err != nil {
		t.Fatal(err)
	}
	agentServer := httptest.NewServer(agentHandler)
	routerURL, err := url.Parse(agentServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	apiServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/sandboxes/box/exec/stream" {
			http.NotFound(writer, request)
			return
		}
		gateway.WebSocketProxy{Router: gateway.DirectRouter{BaseURL: routerURL}}.ServeHTTP(
			writer, request,
			gateway.RouteTarget{
				ID: "upstream", UID: "sandbox-uid", Namespace: "workloads",
				Port: 8081, Path: "/v1/exec/stream",
			},
		)
	}))
	client, err := NewClient(apiServer.URL, apiServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.Token = "platform-session"
	return client, func() {
		apiServer.Close()
		agentServer.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	}
}

func TestExecEndToEndPreservesStreamsAndRemoteExit(t *testing.T) {
	client, cleanup := connectivityTestClient(t)
	defer cleanup()
	var stdout, stderr bytes.Buffer
	err := client.Exec(context.Background(), "box", cliHelperCommand("stream"), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(stdout.Bytes(), []byte{0x00, 0xff, 'O', 'K'}) || stderr.String() != "text-error" {
		t.Fatalf("stdout=%v stderr=%q", stdout.Bytes(), stderr.String())
	}
	err = client.Exec(context.Background(), "box", cliHelperCommand("exit", "37"), &stdout, &stderr)
	var remote RemoteExitError
	if !errors.As(err, &remote) || remote.Code != 37 {
		t.Fatalf("error = %v, want remote exit 37", err)
	}
}

func TestExecCancellationReturnsShellInterruptCode(t *testing.T) {
	client, cleanup := connectivityTestClient(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := client.Exec(ctx, "box", cliHelperCommand("block"), io.Discard, io.Discard)
	var remote RemoteExitError
	if !errors.As(err, &remote) || remote.Code != 130 {
		t.Fatalf("error = %v, want remote exit 130", err)
	}
}
