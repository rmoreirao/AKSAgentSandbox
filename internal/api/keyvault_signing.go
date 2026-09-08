package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
)

// KeyVaultSigningClient implements the narrow auth signing adapter without
// exposing key material to the management process.
type KeyVaultSigningClient struct {
	Tokens auth.AzureTokenSource
	Client *http.Client
}

func (c KeyVaultSigningClient) Sign(ctx context.Context, keyID string, digest []byte) ([]byte, error) {
	var body struct {
		Value string `json:"value"`
	}
	if err := c.request(ctx, keyID, "sign", map[string]string{
		"alg": "RS256", "value": base64.RawURLEncoding.EncodeToString(digest),
	}, &body); err != nil {
		return nil, err
	}
	value, err := base64.RawURLEncoding.DecodeString(body.Value)
	if err != nil || len(value) == 0 {
		return nil, errors.New("Key Vault returned an invalid signature")
	}
	return value, nil
}

func (c KeyVaultSigningClient) Verify(ctx context.Context, keyID string, digest, signature []byte) error {
	var body struct {
		Value bool `json:"value"`
	}
	if err := c.request(ctx, keyID, "verify", map[string]string{
		"alg":    "RS256",
		"digest": base64.RawURLEncoding.EncodeToString(digest),
		"value":  base64.RawURLEncoding.EncodeToString(signature),
	}, &body); err != nil {
		return err
	}
	if !body.Value {
		return errors.New("Key Vault rejected signature")
	}
	return nil
}

func (c KeyVaultSigningClient) request(ctx context.Context, keyID, operation string, input interface{}, output interface{}) error {
	if c.Tokens == nil || keyID == "" {
		return errors.New("Key Vault signing client is not configured")
	}
	endpoint, err := url.Parse(keyID)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || !strings.Contains(endpoint.Path, "/keys/") {
		return errors.New("Key Vault signing key ID must be an HTTPS key URL")
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + "/" + operation
	endpoint.RawQuery = "api-version=7.4"
	data, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	token, err := c.Tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("get Key Vault access token: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := c.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("Key Vault %s returned status %d", operation, response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(output); err != nil {
		return errors.New("Key Vault returned an invalid signing response")
	}
	return nil
}
