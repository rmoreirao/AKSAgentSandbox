package copilotruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	DefaultTokenFile          = "/run/devsandbox/github-token"
	DefaultOpenCodeRuntimeDir = "/run/devsandbox-opencode"
	DefaultAPIURL             = "https://api.github.com"
)

type Config struct {
	TokenFile          string
	OpenCodeRuntimeDir string
	APIURL             string
	Client             *http.Client
}

// Prepare obtains the current runtime credential for one Copilot process,
// verifies its GitHub identity, and returns a process-only environment.
func (c Config) Prepare(ctx context.Context, args, environ []string) ([]string, error) {
	clean := withoutAuthentication(environ)
	if informationalInvocation(args) {
		return clean, nil
	}

	raw, tokenFile, err := c.validatedToken(ctx)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	token := strings.TrimSpace(string(raw))

	home := filepath.Join(filepath.Dir(tokenFile), "copilot")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, errors.New("Copilot authentication unavailable: runtime configuration directory could not be created")
	}
	return append(clean,
		"COPILOT_GITHUB_TOKEN="+token,
		"COPILOT_HOME="+home,
	), nil
}

// PrepareOpenCode injects the broker credential through OpenCode's documented
// in-memory auth contract and keeps any runtime state off the workspace PVC.
func (c Config) PrepareOpenCode(ctx context.Context, args, environ []string) ([]string, error) {
	clean := withoutOpenCodeAuthentication(environ)
	runtimeDir := c.OpenCodeRuntimeDir
	if runtimeDir == "" {
		runtimeDir = DefaultOpenCodeRuntimeDir
	}
	if !filepath.IsAbs(runtimeDir) {
		return nil, errors.New("OpenCode authentication unavailable: runtime directory must be absolute")
	}
	paths := []string{"data", "cache", "config", "state", "tmp", "home"}
	for _, name := range paths {
		if err := os.MkdirAll(filepath.Join(runtimeDir, name), 0o700); err != nil {
			return nil, errors.New("OpenCode authentication unavailable: runtime directory could not be created")
		}
	}
	clean = append(clean,
		"HOME="+filepath.Join(runtimeDir, "home"),
		"XDG_DATA_HOME="+filepath.Join(runtimeDir, "data"),
		"XDG_CACHE_HOME="+filepath.Join(runtimeDir, "cache"),
		"XDG_CONFIG_HOME="+filepath.Join(runtimeDir, "config"),
		"XDG_STATE_HOME="+filepath.Join(runtimeDir, "state"),
		"TMPDIR="+filepath.Join(runtimeDir, "tmp"),
		"OPENCODE_DISABLE_PROJECT_CONFIG=1",
		"OPENCODE_DISABLE_AUTOUPDATE=1",
	)
	if informationalInvocation(args) {
		return clean, nil
	}

	raw, _, err := c.validatedToken(ctx)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	token := strings.TrimSpace(string(raw))
	auth, err := json.Marshal(map[string]interface{}{
		"github-copilot": map[string]interface{}{
			"type": "oauth", "refresh": token, "access": token, "expires": 0,
		},
	})
	if err != nil {
		return nil, errors.New("OpenCode authentication unavailable: runtime configuration could not be created")
	}
	defer clear(auth)

	return append(clean,
		"OPENCODE_AUTH_CONTENT="+string(auth),
	), nil
}

func (c Config) validatedToken(ctx context.Context) ([]byte, string, error) {
	tokenFile := c.TokenFile
	if tokenFile == "" {
		tokenFile = DefaultTokenFile
	}
	raw, err := readRuntimeToken(tokenFile)
	if err != nil {
		return nil, "", errors.New("Copilot authentication unavailable: current runtime credential could not be read")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsRune(token, '\x00') {
		clear(raw)
		return nil, "", errors.New("Copilot authentication unavailable: current runtime credential is invalid")
	}
	if err := c.preflight(ctx, token); err != nil {
		clear(raw)
		return nil, "", err
	}
	return raw, tokenFile, nil
}

func (c Config) preflight(ctx context.Context, token string) error {
	base := c.APIURL
	if base == "" {
		base = DefaultAPIURL
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("Copilot authentication unavailable: GitHub API endpoint is invalid")
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	status, err := requestStatus(ctx, client, strings.TrimRight(base, "/")+"/user", "Bearer", token)
	if err != nil {
		return errors.New("Copilot authentication unavailable: GitHub identity could not be verified")
	}
	if status != http.StatusOK {
		return fmt.Errorf("Copilot authentication unavailable: GitHub rejected the runtime credential (status %d)", status)
	}

	return nil
}

func requestStatus(ctx context.Context, client *http.Client, endpoint, scheme, token string) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", scheme+" "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "devsandbox-copilot-wrapper")
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return response.StatusCode, nil
}

func readRuntimeToken(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("runtime credential path must be absolute")
	}
	cleaned := filepath.Clean(path)
	normalized := strings.ReplaceAll(cleaned, `\`, "/")
	if runtime.GOOS == "windows" {
		normalized = strings.TrimPrefix(normalized, strings.ReplaceAll(filepath.VolumeName(cleaned), `\`, "/"))
	}
	if normalized == "/workspace" || strings.HasPrefix(normalized, "/workspace/") {
		return nil, errors.New("runtime credential cannot be stored on the workspace")
	}
	info, err := os.Lstat(cleaned)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("runtime credential must be a regular file")
	}
	return os.ReadFile(cleaned)
}

func withoutAuthentication(environ []string) []string {
	result := make([]string, 0, len(environ))
	for _, value := range environ {
		name, _, _ := strings.Cut(value, "=")
		switch strings.ToUpper(name) {
		case "COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "COPILOT_HOME", "OPENCODE_AUTH_CONTENT":
			continue
		default:
			result = append(result, value)
		}
	}
	return result
}

func withoutOpenCodeAuthentication(environ []string) []string {
	clean := withoutAuthentication(environ)
	result := make([]string, 0, len(clean))
	for _, value := range clean {
		name, _, _ := strings.Cut(value, "=")
		upper := strings.ToUpper(name)
		switch upper {
		case "HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "TMPDIR":
			continue
		}
		if strings.HasPrefix(upper, "OPENCODE_") {
			continue
		}
		result = append(result, value)
	}
	return result
}

func informationalInvocation(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "--version", "--help", "-h", "version", "help", "completion":
		return true
	default:
		return false
	}
}

func clear(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
