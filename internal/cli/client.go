package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	BaseURL    *url.URL
	HTTPClient *http.Client
	Token      string
	Dialer     *websocket.Dialer
	HostHeader string
}

func NewClient(endpoint string, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, invalid("--api-url must be an absolute HTTP(S) URL")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{BaseURL: parsed, HTTPClient: httpClient}, nil
}

func (c *Client) StartDeviceFlow(ctx context.Context) (DeviceStart, error) {
	var result DeviceStart
	return result, c.do(ctx, http.MethodPost, "/v1/auth/device/start", struct{}{}, &result, false)
}

func (c *Client) PollDeviceFlow(ctx context.Context, state string) (LoginResult, bool, error) {
	var result LoginResult
	encoded, err := json.Marshal(map[string]string{"state": state})
	if err != nil {
		return result, false, fmt.Errorf("encode device poll: %w", err)
	}
	endpoint := *c.BaseURL
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + "/v1/auth/device/poll"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return result, false, fmt.Errorf("create device poll: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	if c.HostHeader != "" {
		request.Host = c.HostHeader
	}
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return result, false, cliError(ExitInternal, "api_unavailable",
			fmt.Sprintf("management API request failed: %v", err), nil)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusTooManyRequests {
		var body apiErrorBody
		_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body)
		if body.Code == "authorization_pending" || body.Code == "slow_down" {
			return result, true, nil
		}
		return result, false, cliError(ExitConflict, body.Code, body.Message, body.Details)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return result, false, errorFromResponse(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return result, false, cliError(ExitInternal, "invalid_api_response",
			"management API returned an invalid response", nil)
	}
	return result, false, nil
}

func (c *Client) BootstrapStatic(ctx context.Context, credential string) (LoginResult, error) {
	var result LoginResult
	if credential == "" || credential != strings.TrimSpace(credential) ||
		strings.ContainsAny(credential, " \t\r\n") {
		return result, cliError(ExitAuthentication, "invalid_bootstrap_credential",
			"GitHub CLI returned an invalid bootstrap credential", nil)
	}
	bootstrapClient := *c
	bootstrapClient.Token = credential
	err := bootstrapClient.do(ctx, http.MethodPost, "/v1/auth/static/bootstrap", nil, &result, true)
	return result, err
}

func (c *Client) Me(ctx context.Context) (Identity, error) {
	var result Identity
	return result, c.do(ctx, http.MethodGet, "/v1/me", nil, &result, true)
}

func (c *Client) Templates(ctx context.Context) ([]Template, error) {
	var result listResponse[Template]
	return result.Items, c.do(ctx, http.MethodGet, "/v1/templates", nil, &result, true)
}

func (c *Client) Template(ctx context.Context, name string) (Template, error) {
	var result Template
	return result, c.do(ctx, http.MethodGet, "/v1/templates/"+url.PathEscape(name), nil, &result, true)
}

func (c *Client) Sandboxes(ctx context.Context, includeRetained ...bool) ([]Sandbox, error) {
	var result listResponse[Sandbox]
	requestPath := "/v1/sandboxes"
	if len(includeRetained) > 0 && includeRetained[0] {
		requestPath += "?includeRetained=true"
	}
	return result.Items, c.do(ctx, http.MethodGet, requestPath, nil, &result, true)
}

func (c *Client) Sandbox(ctx context.Context, name string) (Sandbox, error) {
	var result Sandbox
	return result, c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(name), nil, &result, true)
}

func (c *Client) CreateSandbox(ctx context.Context, input CreateSandboxRequest) (Sandbox, error) {
	var result Sandbox
	return result, c.do(ctx, http.MethodPost, "/v1/sandboxes", input, &result, true)
}

func (c *Client) ResolveRepository(ctx context.Context, repositoryID, ref string) (RepositoryTarget, error) {
	var result struct {
		RepositoryID  string `json:"repositoryId"`
		RepositoryURL string `json:"repositoryUrl"`
		RefName       string `json:"refName"`
		CommitSHA     string `json:"commitSha"`
		AuthorName    string `json:"authorName"`
		AuthorEmail   string `json:"authorEmail"`
		PrimaryOrg    string `json:"primaryOrg"`
		ReadOnly      bool   `json:"readOnly"`
	}
	query := url.Values{"repository": {repositoryID}}
	if ref != "" {
		query.Set("ref", ref)
	}
	err := c.do(ctx, http.MethodGet, "/v1/repositories/resolve?"+query.Encode(), nil, &result, true)
	return RepositoryTarget{
		ID: result.RepositoryID, URL: result.RepositoryURL, Ref: result.RefName,
		CommitSHA: result.CommitSHA, AuthorName: result.AuthorName, AuthorEmail: result.AuthorEmail,
		PrimaryOrg: result.PrimaryOrg, ReadOnly: result.ReadOnly,
	}, err
}

func (c *Client) StopSandbox(ctx context.Context, name string) (Sandbox, error) {
	var result Sandbox
	return result, c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(name)+"/stop", struct{}{}, &result, true)
}

func (c *Client) ResumeSandbox(ctx context.Context, name string) (Sandbox, error) {
	var result Sandbox
	return result, c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(name)+"/resume", struct{}{}, &result, true)
}

func (c *Client) DeleteSandbox(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(name), nil, nil, true)
}

func (c *Client) Events(ctx context.Context, name string) ([]SandboxEvent, error) {
	var result listResponse[SandboxEvent]
	return result.Items, c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(name)+"/events", nil, &result, true)
}

func (c *Client) VSCodeURL(ctx context.Context, name string) (string, error) {
	var result struct {
		URL string `json:"url"`
	}
	return result.URL, c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(name)+"/vscode-url", struct{}{}, &result, true)
}

func (c *Client) DialShell(ctx context.Context, name string) (*websocket.Conn, error) {
	return c.dialWebSocket(ctx, "/v1/sandboxes/"+url.PathEscape(name)+"/shell")
}

func (c *Client) dialWebSocket(ctx context.Context, requestPath string) (*websocket.Conn, error) {
	endpoint := *c.BaseURL
	if endpoint.Scheme == "https" {
		endpoint.Scheme = "wss"
	} else {
		endpoint.Scheme = "ws"
	}
	relative, err := url.Parse(requestPath)
	if err != nil {
		return nil, invalid("invalid WebSocket path")
	}
	endpoint.Path = path.Join(strings.TrimSuffix(endpoint.Path, "/"), relative.Path)
	endpoint.RawQuery = relative.RawQuery
	header := http.Header{"Authorization": []string{"Bearer " + c.Token}}
	if c.HostHeader != "" {
		header.Set("Host", c.HostHeader)
	}
	dialer := c.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	connection, response, err := dialer.DialContext(ctx, endpoint.String(), header)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		if response != nil {
			responseErr := errorFromResponse(response)
			if response.StatusCode >= 500 {
				var detail *Error
				if errors.As(responseErr, &detail) {
					detail.ExitCode = ExitTransport
				}
			}
			return nil, responseErr
		}
		return nil, cliError(ExitTransport, "remote_transport_failed", "sandbox connection failed before an exit code was available", nil)
	}
	return connection, nil
}

func (c *Client) do(ctx context.Context, method, requestPath string, input, output any, authenticated bool) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	endpoint := *c.BaseURL
	relative, parseErr := url.Parse(requestPath)
	if parseErr != nil {
		return fmt.Errorf("parse request path: %w", parseErr)
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + relative.Path
	endpoint.RawQuery = relative.RawQuery
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if c.HostHeader != "" {
		request.Host = c.HostHeader
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authenticated {
		if c.Token == "" {
			return cliError(ExitAuthentication, "authentication_required", "run 'devsandbox login' to authenticate", nil)
		}
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return cliError(ExitInternal, "api_unavailable", fmt.Sprintf("management API request failed: %v", err), nil)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errorFromResponse(response)
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(output); err != nil {
		return cliError(ExitInternal, "invalid_api_response", "management API returned an invalid response", nil)
	}
	return nil
}
