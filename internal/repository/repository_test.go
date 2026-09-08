package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRunner struct {
	output func(Command) ([]byte, error)
	run    func(Command) error
	calls  []Command
}

func (r *fakeRunner) Output(_ context.Context, command Command) ([]byte, error) {
	r.calls = append(r.calls, command)
	if r.output == nil {
		return nil, nil
	}
	return r.output(command)
}

func (r *fakeRunner) Run(_ context.Context, command Command) error {
	r.calls = append(r.calls, command)
	if r.run == nil {
		return nil
	}
	return r.run(command)
}

func TestInspectorRejectsTrackedDirtyWorktree(t *testing.T) {
	runner := &fakeRunner{output: func(command Command) ([]byte, error) {
		switch strings.Join(command.Args, " ") {
		case "rev-parse --show-toplevel":
			return []byte("/repo\n"), nil
		case "status --porcelain=v1 --untracked-files=no":
			return []byte(" M tracked.txt\n"), nil
		default:
			return nil, errors.New("unexpected command")
		}
	}}
	_, err := (Inspector{Directory: t.TempDir(), Runner: runner}).Inspect(context.Background(), nil)
	if !errors.Is(err, ErrDirty) {
		t.Fatalf("error = %v, want ErrDirty", err)
	}
}

func TestInspectorRejectsUnpushedHead(t *testing.T) {
	const head = "1111111111111111111111111111111111111111"
	const remote = "2222222222222222222222222222222222222222"
	runner := &fakeRunner{
		output: func(command Command) ([]byte, error) {
			switch strings.Join(command.Args, " ") {
			case "rev-parse --show-toplevel":
				return []byte("/repo\n"), nil
			case "status --porcelain=v1 --untracked-files=no":
				return nil, nil
			case "remote":
				return []byte("origin\n"), nil
			case "remote get-url --all origin":
				return []byte("git@github.com:Primary/Fixture.git\n"), nil
			case "rev-parse --verify HEAD^{commit}":
				return []byte(head + "\n"), nil
			case "ls-remote --heads --tags --refs https://github.com/primary/fixture.git":
				return []byte(remote + "\trefs/heads/main\n"), nil
			case "cat-file -e " + remote + "^{commit}":
				return nil, nil
			default:
				return nil, errors.New("not available")
			}
		},
		run: func(command Command) error {
			if strings.Contains(strings.Join(command.Args, " "), "merge-base --is-ancestor") {
				return errors.New("not an ancestor")
			}
			return nil
		},
	}
	_, err := (Inspector{Runner: runner}).Inspect(context.Background(), nil)
	if !errors.Is(err, ErrUnpushed) {
		t.Fatalf("error = %v, want ErrUnpushed", err)
	}
}

func TestCanonicalGitHubURL(t *testing.T) {
	for _, value := range []string{
		"Owner/Repo", "https://github.com/Owner/Repo.git", "git@github.com:Owner/Repo.git",
		"ssh://git@github.com/Owner/Repo.git",
	} {
		id, cloneURL, err := CanonicalGitHubURL(value)
		if err != nil || id != "owner/repo" || cloneURL != "https://github.com/owner/repo.git" {
			t.Errorf("CanonicalGitHubURL(%q) = %q, %q, %v", value, id, cloneURL, err)
		}
	}
	for _, value := range []string{
		"http://github.com/owner/repo", "file:///repo", "../repo", "https://example.com/owner/repo",
		"https://user@github.com/owner/repo", "https://github.com/owner/repo/issues",
	} {
		if _, _, err := CanonicalGitHubURL(value); err == nil {
			t.Errorf("CanonicalGitHubURL(%q) succeeded", value)
		}
	}
}

func TestGitHookKeepsExactSHAWhenBranchAdvances(t *testing.T) {
	const wanted = "1111111111111111111111111111111111111111"
	const advanced = "2222222222222222222222222222222222222222"
	runner := &fakeRunner{}
	runner.run = func(command Command) error {
		if commandName(command.Args) == "clone" {
			return os.Mkdir(command.Args[len(command.Args)-1], 0o750)
		}
		if commandName(command.Args) == "cat-file" {
			return errors.New("no .gitmodules")
		}
		return nil
	}
	runner.output = func(command Command) ([]byte, error) {
		if commandName(command.Args) == "ls-remote" {
			return []byte(advanced + "\trefs/heads/main\n"), nil
		}
		return nil, errors.New("unexpected output command")
	}
	repoDir := filepath.Join(t.TempDir(), "repo")
	err := (GitHook{Runner: runner}).Initialize(context.Background(), repoDir, Source{
		Type: "git", RepositoryURL: "https://github.com/primary/repo.git",
		CommitSHA: wanted, RefName: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	var detached, attached bool
	for _, call := range runner.calls {
		joined := strings.Join(call.Args, " ")
		detached = detached || strings.Contains(joined, "checkout --detach --force "+wanted)
		attached = attached || strings.Contains(joined, "checkout -B main")
	}
	if !detached || attached {
		t.Fatalf("exact checkout not retained: detached=%v attached=%v calls=%v", detached, attached, runner.calls)
	}
}

func TestGitHookRejectsInvalidSubmoduleURLBeforeUpdate(t *testing.T) {
	const commit = "1111111111111111111111111111111111111111"
	runner := &fakeRunner{}
	runner.run = func(command Command) error {
		if commandName(command.Args) == "clone" {
			return os.Mkdir(command.Args[len(command.Args)-1], 0o750)
		}
		return nil
	}
	runner.output = func(command Command) ([]byte, error) {
		joined := strings.Join(command.Args, " ")
		switch {
		case strings.Contains(joined, `--get-regexp ^submodule\..*\.url$`):
			return []byte("submodule.bad.url file:///etc/passwd\n"), nil
		case strings.Contains(joined, `--get-regexp ^submodule\..*\.path$`):
			return []byte("submodule.bad.path deps/bad\n"), nil
		case commandName(command.Args) == "ls-remote":
			return nil, nil
		default:
			return nil, errors.New("unexpected output command")
		}
	}
	err := (GitHook{Runner: runner}).Initialize(context.Background(), filepath.Join(t.TempDir(), "repo"), Source{
		Type: "git", RepositoryURL: "https://github.com/primary/repo.git", CommitSHA: commit,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported URL") {
		t.Fatalf("error = %v, want unsupported URL", err)
	}
	for _, call := range runner.calls {
		if commandName(call.Args) == "submodule" && strings.Contains(strings.Join(call.Args, " "), " update ") {
			t.Fatal("submodule update ran before recursive validation completed")
		}
	}
}

func commandName(arguments []string) string {
	for index, argument := range arguments {
		if argument == "-c" {
			index++
			continue
		}
		if index > 0 && arguments[index-1] == "-c" {
			continue
		}
		return argument
	}
	return ""
}
