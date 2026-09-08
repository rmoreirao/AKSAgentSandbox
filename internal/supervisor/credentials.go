package supervisor

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultRuntimeDir      = "/run/devsandbox"
	DefaultBrokerTokenFile = "/var/run/secrets/devsandbox/broker/token"
)

// RuntimeIdentityConfig describes the zero-RBAC sandbox identity and its
// explicitly projected, audience-bound broker token.
type RuntimeIdentityConfig struct {
	ServiceAccountName string `json:"serviceAccountName"`
	AutomountToken     bool   `json:"automountServiceAccountToken"`
	BrokerAudience     string `json:"brokerAudience"`
	BrokerTokenFile    string `json:"brokerTokenFile"`
	BrokerURL          string `json:"brokerUrl"`
	BrokerCAFile       string `json:"brokerCaFile,omitempty"`
}

func (c RuntimeIdentityConfig) Validate() error {
	if strings.TrimSpace(c.ServiceAccountName) == "" {
		return errors.New("service account name is required")
	}
	if c.AutomountToken {
		return errors.New("automatic service-account token mounting must be disabled")
	}
	if strings.TrimSpace(c.BrokerAudience) == "" {
		return errors.New("broker audience is required")
	}
	if !isAbsoluteRuntimePath(c.BrokerTokenFile) {
		return errors.New("broker token file must be absolute")
	}
	u, err := url.Parse(c.BrokerURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("broker URL must be an absolute HTTPS URL")
	}
	return nil
}

// CredentialFile stores a short-lived token under memory-backed runtime
// storage. RuntimeDir must be mounted as tmpfs/emptyDir medium Memory in
// production; it must never point into /workspace.
type CredentialFile struct {
	RuntimeDir string
	Name       string
}

func (f CredentialFile) path() (string, error) {
	root := f.RuntimeDir
	if root == "" {
		root = DefaultRuntimeDir
	}
	if !isAbsoluteRuntimePath(root) {
		return "", errors.New("runtime credential directory must be absolute")
	}
	root = filepath.Clean(root)
	if isWorkspacePath(root) {
		return "", errors.New("runtime credentials cannot be stored on the workspace")
	}
	if f.Name == "" || filepath.Base(f.Name) != f.Name {
		return "", errors.New("credential file name must be a base name")
	}
	return filepath.Join(root, f.Name), nil
}

func (f CredentialFile) Write(value []byte) error {
	if len(value) == 0 {
		return errors.New("credential is empty")
	}
	path, err := f.path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create runtime credential directory: %w", err)
	}
	if err := rejectCredentialSymlink(path); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".credential-*")
	if err != nil {
		return fmt.Errorf("create credential file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure credential file: %w", err)
	}
	if _, err := temp.Write(value); err != nil {
		temp.Close()
		return fmt.Errorf("write credential file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close credential file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			return fmt.Errorf("replace credential file: %w", err)
		}
		if retryErr := os.Rename(tempPath, path); retryErr != nil {
			return fmt.Errorf("publish credential file: %w", retryErr)
		}
	}
	return nil
}

func (f CredentialFile) Read() ([]byte, error) {
	path, err := f.path()
	if err != nil {
		return nil, err
	}
	if err := rejectCredentialSymlink(path); err != nil {
		return nil, err
	}
	value, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credential file: %w", err)
	}
	if len(value) == 0 {
		return nil, errors.New("credential file is empty")
	}
	return value, nil
}

func (f CredentialFile) Remove() error {
	path, err := f.path()
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func rejectCredentialSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect credential file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("credential file must not be a symbolic link")
	}
	return nil
}

type BrokerClient struct {
	Config RuntimeIdentityConfig
	Client *http.Client
}

func BrokerHTTPClient(caFile string) (*http.Client, error) {
	if caFile == "" {
		return &http.Client{Timeout: 20 * time.Second}, nil
	}
	certificate, err := os.ReadFile(caFile)
	if err != nil {
		return nil, errors.New("read broker CA certificate")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, errors.New("load system certificate roots")
	}
	if !roots.AppendCertsFromPEM(certificate) {
		return nil, errors.New("broker CA certificate is invalid")
	}
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		}},
	}, nil
}

// EnsureBearerToken creates the internal agent token once in runtime storage.
// It returns no token so callers cannot accidentally log it.
func EnsureBearerToken(path string) error {
	if !isAbsoluteRuntimePath(path) {
		return errors.New("agent token file must be absolute")
	}
	path = filepath.Clean(path)
	if isWorkspacePath(path) {
		return errors.New("agent token cannot be stored on the workspace")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create agent token directory: %w", err)
	}
	if err := rejectCredentialSymlink(path); err != nil {
		return err
	}
	if value, err := os.ReadFile(path); err == nil {
		if len(strings.TrimSpace(string(value))) < 32 {
			return errors.New("existing agent token is invalid")
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return errors.New("inspect agent token")
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return errors.New("generate agent token")
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw[:])
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return EnsureBearerToken(path)
	}
	if err != nil {
		return errors.New("create agent token")
	}
	if _, err := io.WriteString(file, encoded+"\n"); err != nil {
		file.Close()
		_ = os.Remove(path)
		return errors.New("write agent token")
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return errors.New("close agent token")
	}
	return nil
}

func isAbsoluteRuntimePath(path string) bool {
	return filepath.IsAbs(path) || strings.HasPrefix(path, "/")
}

func isWorkspacePath(path string) bool {
	normalized := strings.ReplaceAll(filepath.Clean(path), `\`, "/")
	if volume := filepath.VolumeName(path); volume != "" {
		normalized = strings.TrimPrefix(normalized, strings.ReplaceAll(volume, `\`, "/"))
	}
	return normalized == "/workspace" || strings.HasPrefix(normalized, "/workspace/")
}

// Fetch exchanges the projected identity for a current credential without
// including either token in URLs or errors.
func (b BrokerClient) Fetch(ctx context.Context) ([]byte, error) {
	if err := b.Config.Validate(); err != nil {
		return nil, err
	}
	projected, err := os.ReadFile(b.Config.BrokerTokenFile)
	if err != nil {
		return nil, errors.New("read projected broker identity")
	}
	projectedToken := strings.TrimSpace(string(projected))
	if projectedToken == "" {
		return nil, errors.New("projected broker identity is empty")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.Config.BrokerURL, nil)
	if err != nil {
		return nil, errors.New("create broker request")
	}
	req.Header.Set("Authorization", "Bearer "+projectedToken)
	req.Header.Set("Content-Type", "application/json")
	client := b.Client
	if client == nil {
		client, err = BrokerHTTPClient(b.Config.BrokerCAFile)
		if err != nil {
			return nil, err
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("request runtime credential")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("credential broker returned status %d", resp.StatusCode)
	}
	var body struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	if err := decoder.Decode(&body); err != nil || body.Token == "" {
		return nil, errors.New("credential broker returned an invalid response")
	}
	return []byte(body.Token), nil
}
