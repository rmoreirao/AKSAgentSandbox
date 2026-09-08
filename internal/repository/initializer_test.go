package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFirstInitializationAndResume(t *testing.T) {
	workspace := t.TempDir()
	var calls atomic.Int32
	hook := HookFunc(func(_ context.Context, repoDir string, _ Source) error {
		calls.Add(1)
		if err := os.Mkdir(repoDir, 0o750); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(repoDir, "preserve"), []byte("original"), 0o600)
	})
	source := Source{Type: "empty"}
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	initializer := Initializer{WorkspaceDir: workspace, Hook: hook, Now: func() time.Time { return now }}

	first, err := initializer.Run(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if !first.FirstInitialization || calls.Load() != 1 {
		t.Fatalf("first initialization = %v, hook calls = %d", first.FirstInitialization, calls.Load())
	}

	resumed, err := initializer.Run(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.FirstInitialization || calls.Load() != 1 {
		t.Fatalf("resume initialized = %v, hook calls = %d", resumed.FirstInitialization, calls.Load())
	}
	content, err := os.ReadFile(filepath.Join(workspace, "repo", "preserve"))
	if err != nil || string(content) != "original" {
		t.Fatalf("resume modified repository: content=%q error=%v", content, err)
	}
	if resumed.Marker.InitializedAt != now {
		t.Fatalf("marker time = %v, want %v", resumed.Marker.InitializedAt, now)
	}
}

func TestResumeRejectsSourceMismatch(t *testing.T) {
	workspace := t.TempDir()
	initializer := Initializer{WorkspaceDir: workspace}
	if _, err := initializer.Run(context.Background(), Source{Type: "empty"}); err != nil {
		t.Fatal(err)
	}

	gitSource := Source{
		Type: "git", RepositoryURL: "https://github.com/example/repo.git",
		CommitSHA: "0123456789012345678901234567890123456789",
	}
	if _, err := initializer.Run(context.Background(), gitSource); !errors.Is(err, ErrMarkerMismatch) {
		t.Fatalf("error = %v, want ErrMarkerMismatch", err)
	}
}

func TestConcurrentInitializationRunsHookOnce(t *testing.T) {
	workspace := t.TempDir()
	var calls atomic.Int32
	initializer := Initializer{
		WorkspaceDir: workspace,
		Hook: HookFunc(func(_ context.Context, repoDir string, _ Source) error {
			calls.Add(1)
			time.Sleep(50 * time.Millisecond)
			return os.Mkdir(repoDir, 0o750)
		}),
	}
	var wait sync.WaitGroup
	errors := make(chan error, 6)
	for range 6 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := initializer.Run(context.Background(), Source{Type: "empty"})
			errors <- err
		}()
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("concurrent initialization: %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("hook calls = %d, want 1", calls.Load())
	}
}

func TestFailedHookCanRetry(t *testing.T) {
	workspace := t.TempDir()
	hook := HookFunc(func(_ context.Context, repoDir string, _ Source) error {
		if err := os.Mkdir(repoDir, 0o750); err != nil {
			return err
		}
		return errors.New("injected failure")
	})
	initializer := Initializer{WorkspaceDir: workspace, Hook: hook}
	if _, err := initializer.Run(context.Background(), Source{Type: "empty"}); err == nil {
		t.Fatal("expected initialization failure")
	}
	if _, err := os.Stat(filepath.Join(workspace, "repo")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial repository remains: %v", err)
	}
	initializer.Hook = DirectoryHook{}
	if _, err := initializer.Run(context.Background(), Source{Type: "empty"}); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
}

func TestInvalidMarkerDoesNotModifyRepository(t *testing.T) {
	workspace := t.TempDir()
	stateDir := filepath.Join(workspace, ".devsandbox")
	repoDir := filepath.Join(workspace, "repo")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(repoDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, markerName), []byte(`{"version":99}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var called bool
	_, err := (Initializer{
		WorkspaceDir: workspace,
		Hook: HookFunc(func(context.Context, string, Source) error {
			called = true
			return nil
		}),
	}).Run(context.Background(), Source{Type: "empty"})
	if err == nil {
		t.Fatal("expected invalid marker error")
	}
	if called {
		t.Fatal("hook ran for invalid marker")
	}
	if _, err := os.Stat(repoDir); err != nil {
		t.Fatalf("repository was modified: %v", err)
	}
}
