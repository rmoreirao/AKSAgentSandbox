package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryCredentials struct {
	mu      sync.Mutex
	value   string
	stored  []string
	loadErr error
}

func (c *memoryCredentials) Load(context.Context, string) (string, error) {
	if c.loadErr != nil {
		return "", c.loadErr
	}
	if c.value == "" {
		return "", cliError(ExitAuthentication, "authentication_required", "login required", nil)
	}
	return c.value, nil
}

func (c *memoryCredentials) Store(_ context.Context, _ string, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = value
	c.stored = append(c.stored, value)
	return nil
}

func (c *memoryCredentials) Delete(context.Context, string) error { return nil }

type staticResolver struct {
	target RepositoryTarget
	err    error
}

func (r staticResolver) Current(context.Context, *Prompter) (RepositoryTarget, error) {
	return r.target, r.err
}

func testOptions(server *httptest.Server, in string, terminal bool) (Options, *bytes.Buffer, *bytes.Buffer) {
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	return Options{
		In:          strings.NewReader(in),
		Out:         out,
		Err:         errOut,
		HTTPClient:  server.Client(),
		Credentials: &memoryCredentials{value: "platform-session"},
		IsTerminal:  func() bool { return terminal },
		Sleep:       func(context.Context, time.Duration) error { return nil },
		Resolver: staticResolver{target: RepositoryTarget{
			ID: "acme/repo", URL: "https://github.com/acme/repo.git",
		}},
		Random: bytes.NewReader(make([]byte, 64)),
	}, out, errOut
}

func writeResponse(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if value != nil {
		_ = json.NewEncoder(writer).Encode(value)
	}
}

func TestAPIHostHeaderFromEnvironment(t *testing.T) {
	t.Setenv("DEVSANDBOX_API_HOST_HEADER", "api.devsandbox.invalid")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Host != "api.devsandbox.invalid" {
			t.Fatalf("host = %q, want api.devsandbox.invalid", request.Host)
		}
		writeResponse(writer, http.StatusOK, map[string]any{"items": []Template{}})
	}))
	defer server.Close()
	options, _, errOut := testOptions(server, "", false)

	code := Execute([]string{"--api-url", server.URL, "templates"}, options)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
}

func TestValidationTLSOptionsFromEnvironment(t *testing.T) {
	t.Setenv("DEVSANDBOX_API_HOST_HEADER", "api.devsandbox.invalid")
	t.Setenv("DEVSANDBOX_SKIP_TLS_VERIFY", "true")

	app := newApplication(Options{})
	transport, ok := app.options.HTTPClient.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		t.Fatal("validation TLS transport was not configured")
	}
	if transport.TLSClientConfig.ServerName != "api.devsandbox.invalid" {
		t.Fatalf("server name = %q", transport.TLSClientConfig.ServerName)
	}
	if !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("TLS verification was not disabled")
	}
	if app.dialer == nil || app.dialer.TLSClientConfig == nil ||
		app.dialer.TLSClientConfig.ServerName != "api.devsandbox.invalid" {
		t.Fatal("WebSocket validation TLS was not configured")
	}
}

func TestMissingTemplateNeverPromptsNonInteractive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/templates" {
			t.Fatalf("unexpected request %s", request.URL.Path)
		}

		writeResponse(writer, http.StatusOK, map[string]any{"items": []Template{
			{Name: "standard", DefaultProfile: "small"},
			{Name: "vscode", DefaultProfile: "small"},
		}})
	}))
	defer server.Close()
	options, _, errOut := testOptions(server, "", false)

	code := Execute([]string{"--api-url", server.URL, "up", "--empty", "--no-attach"}, options)
	if code != ExitInvalid {
		t.Fatalf("exit = %d, want %d; stderr=%s", code, ExitInvalid, errOut.String())
	}
	if !strings.Contains(errOut.String(), "standard") || !strings.Contains(errOut.String(), "vscode") {
		t.Fatalf("choices missing from error: %s", errOut.String())
	}
}

func TestTemplatePromptOnTTY(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/templates":
			writeResponse(writer, http.StatusOK, map[string]any{"items": []Template{
				{Name: "standard", DefaultProfile: "small", EntryAction: "shell"},
			}})
		case "/v1/sandboxes":
			if request.Method == http.MethodGet {
				writeResponse(writer, http.StatusOK, map[string]any{"items": []Sandbox{}})
			} else {
				writeResponse(writer, http.StatusCreated, Sandbox{Name: "standard-aaaaaaaa", Phase: "Running"})
			}
		default:
			t.Fatalf("unexpected request %s", request.URL.Path)
		}
	}))
	defer server.Close()
	options, out, errOut := testOptions(server, "1\n", true)

	code := Execute([]string{"--api-url", server.URL, "up", "--empty", "--no-attach"}, options)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "choose a template") || !strings.Contains(out.String(), "is Running") {
		t.Fatalf("prompt/output missing: %s", out.String())
	}
}

func TestExplicitRepositoryIsResolvedBeforeCreation(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"
	var created CreateSandboxRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/templates":
			writeResponse(writer, http.StatusOK, map[string]any{"items": []Template{{
				Name: "standard", DefaultProfile: "small", EntryAction: "shell",
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/repositories/resolve":
			if request.URL.Query().Get("repository") != "external/public" ||
				request.URL.Query().Get("ref") != "release" {
				t.Errorf("unexpected resolution query %s", request.URL.RawQuery)
			}
			writeResponse(writer, http.StatusOK, map[string]any{
				"repositoryId": "external/public", "repositoryUrl": "https://github.com/external/public.git",
				"refName": "release", "commitSha": sha, "authorName": "User",
				"authorEmail": "user@example.test", "primaryOrg": "primary", "readOnly": true,
			})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/sandboxes":
			writeResponse(writer, http.StatusOK, map[string]any{"items": []Sandbox{}})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/sandboxes":
			if err := json.NewDecoder(request.Body).Decode(&created); err != nil {
				t.Fatal(err)
			}
			writeResponse(writer, http.StatusCreated, Sandbox{Name: "public-aaaaaaaa", Phase: "Running"})
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	options, _, errOut := testOptions(server, "", false)
	code := Execute([]string{
		"--api-url", server.URL, "up", "--repo", "External/Public", "--ref", "release",
		"--template", "standard", "--no-attach",
	}, options)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if created.Source.CommitSHA != sha || !created.Source.ReadOnly ||
		!created.Source.LFS || !created.Source.Submodules ||
		created.Source.AuthorEmail != "user@example.test" {
		t.Fatalf("created source = %#v", created.Source)
	}
}

func TestStoppedEquivalentRequiresChoiceAndResumeFlagResumes(t *testing.T) {
	resumeCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/templates":
			writeResponse(writer, http.StatusOK, map[string]any{"items": []Template{
				{Name: "standard", DefaultProfile: "small", EntryAction: "shell"},
			}})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/sandboxes":
			writeResponse(writer, http.StatusOK, map[string]any{"items": []Sandbox{{
				Name: "stopped-one", Phase: "Stopped", Source: SandboxSource{Type: "empty"},
				Template: SandboxTemplate{Name: "standard"}, Profile: SandboxProfile{Name: "small"},
			}}})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/sandboxes/stopped-one/resume":
			resumeCalls++
			writeResponse(writer, http.StatusAccepted, Sandbox{Name: "stopped-one", Phase: "Running"})
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()

	options, _, errOut := testOptions(server, "", false)
	code := Execute([]string{
		"--api-url", server.URL, "up", "--empty", "--template", "standard", "--no-attach",
	}, options)
	if code != ExitInvalid || !strings.Contains(errOut.String(), "resume stopped-one") ||
		!strings.Contains(errOut.String(), "create new") {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	options, _, errOut = testOptions(server, "", false)
	code = Execute([]string{
		"--api-url", server.URL, "up", "--empty", "--template", "standard", "--resume-existing", "--no-attach",
	}, options)
	if code != 0 || resumeCalls != 1 {
		t.Fatalf("exit=%d resumeCalls=%d stderr=%s", code, resumeCalls, errOut.String())
	}
}

func TestAmbiguousSandboxFailsWithChoicesNonInteractive(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeResponse(writer, http.StatusOK, map[string]any{"items": []Sandbox{
			{Name: "one", Source: SandboxSource{Type: "git", RepositoryURL: "https://github.com/acme/repo.git"}},
			{Name: "two", Source: SandboxSource{Type: "git", RepositoryURL: "git@github.com:acme/repo.git"}},
		}})
	}))
	defer server.Close()
	options, _, errOut := testOptions(server, "", false)

	code := Execute([]string{"--api-url", server.URL, "status"}, options)
	if code != ExitInvalid {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "one") || !strings.Contains(errOut.String(), "two") {
		t.Fatalf("available sandboxes missing: %s", errOut.String())
	}
}

func TestListFiltersRetainedUnlessAll(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeResponse(writer, http.StatusOK, map[string]any{"items": []Sandbox{
			{Name: "active", Phase: "Running"},
			{Name: "retained", Phase: "Stopped"},
			{Name: "failed", Phase: "Failed"},
		}})
	}))
	defer server.Close()
	options, out, errOut := testOptions(server, "", false)
	if code := Execute([]string{"--api-url", server.URL, "--json", "list"}, options); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if strings.Contains(out.String(), "retained") || strings.Contains(out.String(), "failed") {
		t.Fatalf("default list contains retained values: %s", out.String())
	}
	options, out, errOut = testOptions(server, "", false)
	if code := Execute([]string{"--api-url", server.URL, "--json", "list", "--all"}, options); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "retained") || !strings.Contains(out.String(), "failed") {
		t.Fatalf("--all omitted retained values: %s", out.String())
	}
}

func TestGeneratedNamesAreStableAndDNSSafe(t *testing.T) {
	name, err := GenerateName("My_REALLY.long Repository!!!", bytes.NewReader(make([]byte, 5)))
	if err != nil {
		t.Fatal(err)
	}

	if name != "my-really-long-repository-aaaaaaaa" || !dnsNamePattern.MatchString(name) {
		t.Fatalf("name = %q", name)
	}

	longName, err := GenerateName(strings.Repeat("x", 100), bytes.NewReader(make([]byte, 5)))
	if err != nil {
		t.Fatal(err)
	}
	if len(longName) != 63 || !dnsNamePattern.MatchString(longName) {
		t.Fatalf("long name = %q (%d)", longName, len(longName))
	}
	if prefix := namePrefix(SandboxSource{Type: "git", RepositoryURL: "https://github.com/acme/my-repo.git"}, "standard"); prefix != "my-repo" {
		t.Fatalf("repository prefix = %q", prefix)
	}
}

func TestTemplateShowJSONHasStableFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writeResponse(writer, http.StatusOK, Template{
			Name: "standard", DisplayName: "Standard", Description: "Shell",
			Version: "1.2.3", ImageDigest: "sha256:abc", DefaultProfile: "small",
			EntryAction: "shell", Capabilities: TemplateCapabilities{},
		})
	}))
	defer server.Close()
	options, out, errOut := testOptions(server, "", false)

	code := Execute([]string{"--api-url", server.URL, "--json", "templates", "show", "standard"}, options)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"name", "displayName", "description", "version", "imageDigest", "defaultProfile", "entryAction", "capabilities"} {
		if _, ok := value[field]; !ok {
			t.Errorf("stable field %q missing from %s", field, out.String())
		}
	}
}

func TestHTTPExitCodeMapping(t *testing.T) {
	tests := []struct {
		status int
		exit   int
	}{
		{http.StatusBadRequest, ExitInvalid},
		{http.StatusUnauthorized, ExitAuthentication},
		{http.StatusNotFound, ExitNotFound},
		{http.StatusConflict, ExitConflict},
		{http.StatusServiceUnavailable, ExitProvisioning},
		{http.StatusInternalServerError, ExitInternal},
	}
	for _, test := range tests {
		t.Run(http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writeResponse(writer, test.status, apiErrorBody{Code: "test_error", Message: "failed"})
			}))
			defer server.Close()
			options, _, _ := testOptions(server, "", false)
			if code := Execute([]string{"--api-url", server.URL, "status", "box"}, options); code != test.exit {
				t.Fatalf("status %d mapped to %d, want %d", test.status, code, test.exit)
			}
		})
	}
	if code := exitCode(RemoteExitError{Code: 23}); code != 23 {
		t.Fatalf("remote exit mapped to %d", code)
	}
}

func TestStopAndDeleteAreIdempotent(t *testing.T) {
	var mu sync.Mutex
	deleted := false
	stopCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/sandboxes/box/stop":
			stopCalls++
			writeResponse(writer, http.StatusAccepted, Sandbox{Name: "box", Phase: "Stopping"})
		case request.Method == http.MethodDelete && request.URL.Path == "/v1/sandboxes/box":
			if deleted {
				writeResponse(writer, http.StatusNotFound, apiErrorBody{Code: "sandbox_not_found", Message: "not found"})
				return
			}
			deleted = true
			writer.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()

	for index := 0; index < 2; index++ {
		options, _, errOut := testOptions(server, "", false)
		if code := Execute([]string{"--api-url", server.URL, "stop", "box"}, options); code != 0 {
			t.Fatalf("stop %d exit=%d stderr=%s", index, code, errOut.String())
		}
		options, _, errOut = testOptions(server, "", false)
		if code := Execute([]string{"--api-url", server.URL, "delete", "box", "--yes"}, options); code != 0 {
			t.Fatalf("delete %d exit=%d stderr=%s", index, code, errOut.String())
		}
	}
	if stopCalls != 2 {
		t.Fatalf("stop calls = %d", stopCalls)
	}
}

func TestLoginStoresOnlyPlatformSession(t *testing.T) {
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/auth/device/start":
			writeResponse(writer, http.StatusOK, DeviceStart{
				State: "opaque", UserCode: "ABCD", VerificationURI: "https://github.com/login/device",
				ExpiresIn: 600, Interval: 1,
			})
		case "/v1/auth/device/poll":
			polls++
			if polls == 1 {
				writeResponse(writer, http.StatusAccepted, apiErrorBody{
					Code: "authorization_pending", Message: "authorization is pending",
				})
				return
			}
			writeResponse(writer, http.StatusOK, map[string]any{
				"sessionToken": "platform-only", "expiresAt": time.Unix(1000, 0).UTC(),
				"identity": map[string]string{"userId": "1", "login": "octocat", "org": "acme"},
			})
		default:
			t.Fatalf("unexpected request %s", request.URL.Path)
		}
	}))
	defer server.Close()
	credentials := &memoryCredentials{}
	options, _, errOut := testOptions(server, "", false)
	options.Credentials = credentials

	if code := Execute([]string{"--api-url", server.URL, "login"}, options); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if len(credentials.stored) != 1 || credentials.stored[0] != "platform-only" {
		t.Fatalf("stored values = %#v", credentials.stored)
	}
	if polls != 2 {
		t.Fatalf("polls = %d", polls)
	}
}

func TestStaticModeBootstrapsAndStoresOnlyPlatformSession(t *testing.T) {
	t.Setenv("DEVSANDBOX_AUTH_MODE", authModeGitHubCLIStatic)
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/auth/static/bootstrap":
			authorization = request.Header.Get("Authorization")
			writeResponse(writer, http.StatusOK, map[string]any{
				"sessionToken": "platform-only",
				"expiresAt":    time.Unix(2000, 0).UTC(),
				"identity":     map[string]string{"userId": "42", "login": "owner", "org": "owner"},
			})
		case "/v1/templates":
			if request.Header.Get("Authorization") != "Bearer platform-only" {
				t.Fatalf("platform authorization = %q", request.Header.Get("Authorization"))
			}
			writeResponse(writer, http.StatusOK, map[string]any{"items": []Template{}})
		default:
			t.Fatalf("unexpected request %s", request.URL.Path)
		}
	}))
	defer server.Close()
	credentials := &memoryCredentials{}
	options, _, errOut := testOptions(server, "", false)
	options.Credentials = credentials
	options.BootstrapCredential = func(context.Context) (string, error) {
		return "github-bootstrap", nil
	}

	if code := Execute([]string{"--api-url", server.URL, "templates"}, options); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if authorization != "Bearer github-bootstrap" {
		t.Fatalf("bootstrap authorization = %q", authorization)
	}
	if len(credentials.stored) != 1 || credentials.stored[0] != "platform-only" {
		t.Fatalf("stored values = %#v", credentials.stored)
	}
}

func TestStaticModeRefreshesExpiredCachedSession(t *testing.T) {
	t.Setenv("DEVSANDBOX_AUTH_MODE", authModeGitHubCLIStatic)
	bootstrapCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/me":
			if request.Header.Get("Authorization") != "Bearer expired-platform" {
				t.Fatalf("cached authorization = %q", request.Header.Get("Authorization"))
			}
			writeResponse(writer, http.StatusUnauthorized, apiErrorBody{
				Code: "expired_session", Message: "valid platform session required",
			})
		case "/v1/auth/static/bootstrap":
			bootstrapCalls++
			writeResponse(writer, http.StatusOK, map[string]any{
				"sessionToken": "refreshed-platform",
				"expiresAt":    time.Unix(3000, 0).UTC(),
				"identity":     map[string]string{"userId": "42", "login": "owner"},
			})
		case "/v1/templates":
			if request.Header.Get("Authorization") != "Bearer refreshed-platform" {
				t.Fatalf("refreshed authorization = %q", request.Header.Get("Authorization"))
			}
			writeResponse(writer, http.StatusOK, map[string]any{"items": []Template{}})
		default:
			t.Fatalf("unexpected request %s", request.URL.Path)
		}
	}))
	defer server.Close()
	credentials := &memoryCredentials{value: "expired-platform"}
	options, _, errOut := testOptions(server, "", false)
	options.Credentials = credentials
	options.BootstrapCredential = func(context.Context) (string, error) {
		return "github-bootstrap", nil
	}

	if code := Execute([]string{"--api-url", server.URL, "templates"}, options); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if bootstrapCalls != 1 || credentials.value != "refreshed-platform" {
		t.Fatalf("bootstrapCalls=%d credentials=%q", bootstrapCalls, credentials.value)
	}
}

func TestStaticModeReusesValidCachedSession(t *testing.T) {
	t.Setenv("DEVSANDBOX_AUTH_MODE", authModeGitHubCLIStatic)
	bootstrapCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/me":
			writeResponse(writer, http.StatusOK, map[string]string{"userId": "42", "login": "owner"})
		case "/v1/templates":
			if request.Header.Get("Authorization") != "Bearer cached-platform" {
				t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
			}
			writeResponse(writer, http.StatusOK, map[string]any{"items": []Template{}})
		default:
			t.Fatalf("unexpected request %s", request.URL.Path)
		}
	}))
	defer server.Close()
	options, _, errOut := testOptions(server, "", false)
	options.Credentials = &memoryCredentials{value: "cached-platform"}
	options.BootstrapCredential = func(context.Context) (string, error) {
		bootstrapCalls++
		return "github-bootstrap", nil
	}

	if code := Execute([]string{"--api-url", server.URL, "templates"}, options); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	if bootstrapCalls != 0 {
		t.Fatalf("bootstrap called %d times", bootstrapCalls)
	}
}

func TestConfiguredDefaultTemplateAvoidsPrompt(t *testing.T) {
	t.Setenv("DEVSANDBOX_DEFAULT_TEMPLATE", "standard")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/v1/templates":
			writeResponse(writer, http.StatusOK, map[string]any{"items": []Template{{
				Name: "standard", DefaultProfile: "small", EntryAction: "shell",
			}}})
		case request.Method == http.MethodGet && request.URL.Path == "/v1/sandboxes":
			writeResponse(writer, http.StatusOK, map[string]any{"items": []Sandbox{}})
		case request.Method == http.MethodPost && request.URL.Path == "/v1/sandboxes":
			writeResponse(writer, http.StatusCreated, Sandbox{Name: "standard-aaaaaaaa", Phase: "Running"})
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	options, _, errOut := testOptions(server, "", false)

	if code := Execute([]string{"--api-url", server.URL, "up", "--empty", "--no-attach"}, options); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
}

func TestDoctorChecksAPIAuthenticationAndRepository(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/me":
			writeResponse(writer, http.StatusOK, map[string]string{"userId": "42", "login": "owner"})
		case "/v1/templates":
			writeResponse(writer, http.StatusOK, map[string]any{"items": []Template{{Name: "standard"}}})
		default:
			t.Fatalf("unexpected request %s", request.URL.Path)
		}
	}))
	defer server.Close()
	options, out, errOut := testOptions(server, "", false)
	options.Resolver = staticResolver{target: RepositoryTarget{
		ID: "acme/repo", URL: "https://github.com/acme/repo.git", CommitSHA: strings.Repeat("1", 40),
	}}

	if code := Execute([]string{"--api-url", server.URL, "doctor"}, options); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, errOut.String())
	}
	for _, expected := range []string{"Authenticated as: owner", "Repository: acme/repo", "Ready: devsandbox up"} {
		if !strings.Contains(out.String(), expected) {
			t.Fatalf("doctor output missing %q: %s", expected, out.String())
		}
	}
}

func TestDeleteRequiresYesWithoutTTY(t *testing.T) {
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requested = true
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	options, _, errOut := testOptions(server, "", false)
	code := Execute([]string{"--api-url", server.URL, "delete", "box"}, options)
	if code != ExitInvalid || requested {
		t.Fatalf("exit=%d requested=%v stderr=%s", code, requested, errOut.String())
	}
}

func TestCredentialErrorsRemainAuthenticationErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("API must not be called without credentials")
	}))
	defer server.Close()
	options, _, _ := testOptions(server, "", false)
	options.Credentials = &memoryCredentials{loadErr: cliError(ExitAuthentication, "authentication_required", "login", nil)}
	if code := Execute([]string{"--api-url", server.URL, "list"}, options); code != ExitAuthentication {
		t.Fatalf("exit=%d", code)
	}
}
