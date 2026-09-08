package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVSCodeUpOpensButNeverPrintsOneTimeURL(t *testing.T) {
	const oneTimeURL = "https://sandbox.example/bootstrap#credential=super-secret"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/templates":
			writeResponse(writer, http.StatusOK, map[string]any{"items": []Template{{
				Name: "vscode", DefaultProfile: "medium", EntryAction: "vscode",
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/sandboxes":
			writeResponse(writer, http.StatusOK, map[string]any{"items": []Sandbox{}})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/sandboxes":
			writeResponse(writer, http.StatusCreated, Sandbox{Name: "vscode-test", Phase: "Running"})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/sandboxes/vscode-test/vscode-url":
			writeResponse(writer, http.StatusOK, map[string]string{"url": oneTimeURL})
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	options, out, errOut := testOptions(server, "", false)
	var opened string
	options.OpenURL = func(value string) error {
		opened = value
		return nil
	}
	code := Execute([]string{"--api-url", server.URL, "up", "--empty", "--template", "vscode", "--name", "vscode-test"}, options)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if opened != oneTimeURL {
		t.Fatal("authenticated URL was not passed directly to the browser")
	}
	if strings.Contains(out.String(), oneTimeURL) || strings.Contains(out.String(), "super-secret") ||
		strings.Contains(errOut.String(), oneTimeURL) || strings.Contains(errOut.String(), "super-secret") {
		t.Fatal("one-time browser URL was printed")
	}
}
