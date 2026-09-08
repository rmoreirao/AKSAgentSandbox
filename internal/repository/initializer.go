package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	MarkerVersion = 1
	markerName    = "initialized.json"
)

var ErrMarkerMismatch = errors.New("workspace initialization marker does not match requested source")

var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}([0-9a-fA-F]{24})?$`)

// Source is the immutable, non-secret description of a workspace source.
type Source struct {
	Type          string `json:"type"`
	RepositoryURL string `json:"repositoryUrl,omitempty"`
	CommitSHA     string `json:"commitSha,omitempty"`
	RefName       string `json:"refName,omitempty"`
	AuthorName    string `json:"authorName,omitempty"`
	AuthorEmail   string `json:"authorEmail,omitempty"`
	PrimaryOrg    string `json:"primaryOrg,omitempty"`
	ReadOnly      bool   `json:"readOnly,omitempty"`
}

func (s Source) validate() error {
	switch s.Type {
	case "empty":
		if s.RepositoryURL != "" || s.CommitSHA != "" || s.RefName != "" ||
			s.AuthorName != "" || s.AuthorEmail != "" || s.PrimaryOrg != "" || s.ReadOnly {
			return errors.New("empty source cannot contain repository fields")
		}
	case "git":
		remote, err := url.Parse(s.RepositoryURL)
		if err != nil || remote.Scheme != "https" || remote.Host == "" {
			return errors.New("git source requires an absolute HTTPS repository URL")
		}
		if remote.User != nil || remote.RawQuery != "" || remote.Fragment != "" {
			return errors.New("repository URL cannot contain credentials, query, or fragment")
		}
		if !commitPattern.MatchString(s.CommitSHA) {
			return errors.New("git source requires an exact commit SHA")
		}
		if invalidIdentityValue(s.AuthorName, 256) || invalidIdentityValue(s.AuthorEmail, 320) ||
			invalidIdentityValue(s.PrimaryOrg, 100) {
			return errors.New("git source contains invalid identity metadata")
		}
	default:
		return fmt.Errorf("unsupported source type %q", s.Type)
	}
	return nil
}

func invalidIdentityValue(value string, limit int) bool {
	return len(value) > limit || strings.ContainsAny(value, "\r\n\x00")
}

// Marker is persisted on the workspace PVC. It deliberately contains no
// credential, command, or repository content.
type Marker struct {
	Version       int       `json:"version"`
	Source        Source    `json:"source"`
	InitializedAt time.Time `json:"initializedAt"`
}

// Hook is trusted platform code used to populate repoDir. Hook configuration is
// supplied by the platform; no executable or hook is loaded from the repository.
type Hook interface {
	Initialize(context.Context, string, Source) error
}

// HookFunc adapts a function into a Hook.
type HookFunc func(context.Context, string, Source) error

func (f HookFunc) Initialize(ctx context.Context, repoDir string, source Source) error {
	return f(ctx, repoDir, source)
}

// DirectoryHook creates an empty repository directory. It is the safe default
// for empty workspaces and for images whose trusted provisioning hook populates
// the directory separately.
type DirectoryHook struct{}

func (DirectoryHook) Initialize(_ context.Context, repoDir string, _ Source) error {
	return os.Mkdir(repoDir, 0o750)
}

type Initializer struct {
	WorkspaceDir string
	Hook         Hook
	Now          func() time.Time
	LockTimeout  time.Duration
}

type Result struct {
	FirstInitialization bool
	Marker              Marker
}

// Run initializes a workspace once. A valid marker makes the operation a
// read-only resume; the repository directory is never recreated or changed.
func (i Initializer) Run(ctx context.Context, source Source) (Result, error) {
	if err := source.validate(); err != nil {
		return Result{}, err
	}
	workspace, err := cleanAbsolute(i.WorkspaceDir)
	if err != nil {
		return Result{}, err
	}
	if i.Hook == nil {
		i.Hook = DirectoryHook{}
	}
	if i.Now == nil {
		i.Now = time.Now
	}
	if i.LockTimeout <= 0 {
		i.LockTimeout = 2 * time.Minute
	}

	stateDir := filepath.Join(workspace, ".devsandbox")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("create state directory: %w", err)
	}
	if err := rejectSymlink(stateDir); err != nil {
		return Result{}, err
	}

	unlock, err := acquireLock(ctx, filepath.Join(stateDir, "initialize.lock"), i.LockTimeout)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	markerPath := filepath.Join(stateDir, markerName)
	pendingPath := filepath.Join(stateDir, "initializing.json")
	marker, err := readMarker(markerPath)
	if err == nil {
		if !sameSource(marker.Source, source) {
			return Result{}, ErrMarkerMismatch
		}
		repoDir := filepath.Join(workspace, "repo")
		info, statErr := os.Lstat(repoDir)
		if statErr != nil {
			return Result{}, fmt.Errorf("initialized workspace repository is unavailable: %w", statErr)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return Result{}, errors.New("initialized workspace repository is not a directory")
		}
		return Result{Marker: marker}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return Result{}, err
	}

	repoDir := filepath.Join(workspace, "repo")
	if _, err := os.Lstat(repoDir); err == nil {
		pending, pendingErr := readMarker(pendingPath)
		if pendingErr != nil || !sameSource(pending.Source, source) {
			return Result{}, errors.New("repository exists without an initialization marker")
		}
		if err := os.RemoveAll(repoDir); err != nil {
			return Result{}, fmt.Errorf("remove interrupted repository initialization: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Result{}, fmt.Errorf("inspect repository: %w", err)
	}
	staleStages, _ := filepath.Glob(filepath.Join(workspace, ".repo-initialize-*"))
	for _, stale := range staleStages {
		_ = os.RemoveAll(stale)
	}
	_ = os.Remove(pendingPath)

	pending := Marker{Version: MarkerVersion, Source: source, InitializedAt: i.Now().UTC()}
	if err := writeMarker(pendingPath, pending); err != nil {
		return Result{}, fmt.Errorf("record initialization transaction: %w", err)
	}
	stageDir, err := uniqueStagePath(workspace)
	if err != nil {
		return Result{}, err
	}
	if err := i.Hook.Initialize(ctx, stageDir, source); err != nil {
		_ = os.RemoveAll(stageDir)
		_ = os.Remove(pendingPath)
		return Result{}, fmt.Errorf("initialize repository: %w", err)
	}
	info, err := os.Stat(stageDir)
	if err != nil || !info.IsDir() {
		_ = os.RemoveAll(stageDir)
		_ = os.Remove(pendingPath)
		return Result{}, errors.New("initialization hook did not create a repository directory")
	}
	if err := os.Rename(stageDir, repoDir); err != nil {
		_ = os.RemoveAll(stageDir)
		return Result{}, fmt.Errorf("publish initialized repository: %w", err)
	}

	marker = Marker{
		Version:       MarkerVersion,
		Source:        source,
		InitializedAt: pending.InitializedAt,
	}
	if err := writeMarker(markerPath, marker); err != nil {
		return Result{}, err
	}
	_ = os.Remove(pendingPath)
	return Result{FirstInitialization: true, Marker: marker}, nil
}

func cleanAbsolute(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("workspace directory is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve workspace directory: %w", err)
	}
	return filepath.Clean(path), nil
}

func sameSource(a, b Source) bool {
	return a.Type == b.Type &&
		a.RepositoryURL == b.RepositoryURL &&
		a.CommitSHA == b.CommitSHA &&
		a.RefName == b.RefName &&
		a.AuthorName == b.AuthorName &&
		a.AuthorEmail == b.AuthorEmail &&
		a.PrimaryOrg == b.PrimaryOrg &&
		a.ReadOnly == b.ReadOnly
}

func readMarker(path string) (Marker, error) {
	if err := rejectSymlink(path); err != nil {
		return Marker{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Marker{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return Marker{}, errors.New("initialization marker is not a valid regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return Marker{}, err
	}
	defer f.Close()
	var marker Marker
	decoder := json.NewDecoder(io.LimitReader(f, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&marker); err != nil {
		return Marker{}, fmt.Errorf("decode initialization marker: %w", err)
	}
	if marker.Version != MarkerVersion || marker.InitializedAt.IsZero() {
		return Marker{}, errors.New("invalid initialization marker")
	}
	if err := marker.Source.validate(); err != nil {
		return Marker{}, fmt.Errorf("invalid initialization marker source: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Marker{}, errors.New("initialization marker has trailing content")
	}
	return marker, nil
}

func writeMarker(path string, marker Marker) error {
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return fmt.Errorf("encode initialization marker: %w", err)
	}

	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".initialized-*.tmp")
	if err != nil {
		return fmt.Errorf("create initialization marker: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure initialization marker: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write initialization marker: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync initialization marker: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close initialization marker: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish initialization marker: %w", err)
	}
	return nil
}

func uniqueStagePath(workspace string) (string, error) {
	file, err := os.CreateTemp(workspace, ".repo-initialize-")
	if err != nil {
		return "", fmt.Errorf("reserve repository staging path: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close repository staging reservation: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("prepare repository staging path: %w", err)
	}
	return path, nil
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must not be a symbolic link", path)
	}
	return nil
}

func acquireLock(ctx context.Context, path string, timeout time.Duration) (func(), error) {
	deadline := time.Now().Add(timeout)
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("acquire initialization lock: %w", err)
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > timeout {
			if removeErr := os.Remove(path); removeErr == nil {
				continue
			}
		}
		if time.Now().After(deadline) {
			return nil, errors.New("timed out waiting for workspace initialization lock")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
