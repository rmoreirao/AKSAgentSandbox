package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

type AzureTokenSource interface {
	Token(ctx context.Context) (string, error)
}

// WorkloadIdentityTokenSource exchanges a projected federated token for an
// Azure access token using only net/http. API and broker processes configure
// distinct client IDs.
type WorkloadIdentityTokenSource struct {
	TenantID           string
	ClientID           string
	FederatedTokenFile string
	Scope              string
	Client             *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func (s *WorkloadIdentityTokenSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && s.expires.After(time.Now().Add(time.Minute)) {
		return s.token, nil
	}
	assertion, err := readSmallFile(s.FederatedTokenFile)
	if err != nil {
		return "", errors.New("read workload identity assertion")
	}
	scope := s.Scope
	if scope == "" {
		scope = "https://vault.azure.net/.default"
	}
	form := url.Values{
		"client_id":             {s.ClientID},
		"scope":                 {scope},
		"grant_type":            {"client_credentials"},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	endpoint := "https://login.microsoftonline.com/" + url.PathEscape(s.TenantID) + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", errors.New("create workload identity token request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := s.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange workload identity assertion: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("workload identity exchange returned status %d", response.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body); err != nil || body.AccessToken == "" {
		return "", errors.New("workload identity exchange returned an invalid response")
	}
	s.token = body.AccessToken
	s.expires = time.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	return s.token, nil
}

func (s *WorkloadIdentityTokenSource) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

type KeyVaultRefreshCredentialStore struct {
	VaultURL string
	Tokens   AzureTokenSource
	Client   *http.Client
}

func (s *KeyVaultRefreshCredentialStore) Read(ctx context.Context, userID string) (VersionedRefreshCredential, error) {
	endpoint, err := s.secretURL(userID)
	if err != nil {
		return VersionedRefreshCredential{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return VersionedRefreshCredential{}, errors.New("create key vault read request")
	}
	if err := s.authorize(ctx, req); err != nil {
		return VersionedRefreshCredential{}, err
	}
	response, err := s.client().Do(req)
	if err != nil {
		return VersionedRefreshCredential{}, fmt.Errorf("read key vault refresh credential: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return VersionedRefreshCredential{}, ErrCredentialNotFound
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return VersionedRefreshCredential{}, fmt.Errorf("key vault read returned status %d", response.StatusCode)
	}
	var body struct {
		Value string `json:"value"`
		ID    string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body); err != nil {
		return VersionedRefreshCredential{}, errors.New("decode key vault refresh credential")
	}
	version := path.Base(strings.TrimSuffix(body.ID, "/"))
	if body.Value == "" || version == "" || version == "secrets" {
		return VersionedRefreshCredential{}, errors.New("key vault returned an incomplete refresh credential")
	}
	return VersionedRefreshCredential{Value: body.Value, Version: version}, nil
}

func (s *KeyVaultRefreshCredentialStore) Write(ctx context.Context, userID, refreshCredential string) (string, error) {
	if refreshCredential == "" {
		return "", errors.New("refresh credential is empty")
	}
	endpoint, err := s.secretURL(userID)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]interface{}{
		"value": refreshCredential,
		"tags":  map[string]string{"purpose": "github-app-user-refresh"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", errors.New("create key vault write request")
	}
	req.Header.Set("Content-Type", "application/json")
	if err := s.authorize(ctx, req); err != nil {
		return "", err
	}
	response, err := s.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("write key vault refresh credential: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return "", fmt.Errorf("key vault write returned status %d", response.StatusCode)
	}
	var responseBody struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&responseBody); err != nil {
		return "", errors.New("decode key vault write response")
	}
	version := path.Base(strings.TrimSuffix(responseBody.ID, "/"))
	if version == "" || version == "secrets" {
		return "", errors.New("key vault returned no secret version")
	}
	return version, nil
}

func (s *KeyVaultRefreshCredentialStore) secretURL(userID string) (string, error) {
	if userID == "" || s.VaultURL == "" || s.Tokens == nil {
		return "", errors.New("key vault credential store is not configured")
	}
	base, err := url.Parse(s.VaultURL)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return "", errors.New("key vault URL must be absolute HTTPS")
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + "/secrets/" + keyVaultSecretName(userID)
	base.RawQuery = "api-version=7.4"
	return base.String(), nil
}

func (s *KeyVaultRefreshCredentialStore) authorize(ctx context.Context, req *http.Request) error {
	token, err := s.Tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("get key vault access token: %w", err)
	}
	if token == "" {
		return errors.New("key vault access token is empty")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (s *KeyVaultRefreshCredentialStore) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

func keyVaultSecretName(userID string) string {
	digest := sha256.Sum256([]byte(userID))
	return "github-refresh-" + hex.EncodeToString(digest[:])
}

func readSmallFile(name string) (string, error) {
	if name == "" {
		return "", errors.New("file name is empty")
	}
	file, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", errors.New("file is empty")
	}
	return value, nil
}
