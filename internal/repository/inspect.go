package repository

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

var githubPartPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

var (
	ErrNotRepository = errors.New("current directory is not inside a Git repository")
	ErrDirty         = errors.New("working tree has staged or unstaged tracked changes")
	ErrUnpushed      = errors.New("HEAD is not reachable from the selected GitHub remote")
	ErrRemoteChoice  = errors.New("multiple GitHub remotes require a selection")
)

type GitRemote struct {
	Name string
	ID   string
	URL  string
}

type LocalRepository struct {
	Remote      GitRemote
	Remotes     []GitRemote
	RefName     string
	CommitSHA   string
	AuthorName  string
	AuthorEmail string
}

type RemoteChooser func([]GitRemote) (GitRemote, error)

type Inspector struct {
	Directory string
	GitPath   string
	Runner    CommandRunner
}

// Inspect discovers a local repository, chooses a GitHub remote, and proves
// that the clean local HEAD is already reachable from that remote.
func (i Inspector) Inspect(ctx context.Context, choose RemoteChooser) (LocalRepository, error) {
	if _, err := i.gitOutput(ctx, "rev-parse", "--show-toplevel"); err != nil {
		return LocalRepository{}, ErrNotRepository
	}
	status, err := i.gitOutput(ctx, "status", "--porcelain=v1", "--untracked-files=no")
	if err != nil {
		return LocalRepository{}, fmt.Errorf("inspect working tree: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return LocalRepository{}, ErrDirty
	}
	remotes, err := i.remotes(ctx)
	if err != nil {
		return LocalRepository{}, err
	}
	if len(remotes) == 0 {
		return LocalRepository{}, errors.New("current Git repository has no GitHub remote")
	}
	selected, err := selectGitRemote(remotes, choose)
	if err != nil {
		return LocalRepository{}, err
	}
	head, err := i.gitOutput(ctx, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return LocalRepository{}, fmt.Errorf("resolve local HEAD: %w", err)
	}
	head = strings.ToLower(strings.TrimSpace(head))
	if err := i.requireReachable(ctx, selected, head); err != nil {
		return LocalRepository{}, err
	}
	result := LocalRepository{Remote: selected, Remotes: remotes, CommitSHA: head}
	if branch, branchErr := i.gitOutput(ctx, "symbolic-ref", "--quiet", "--short", "HEAD"); branchErr == nil {
		result.RefName = strings.TrimSpace(branch)
	}
	if name, nameErr := i.gitOutput(ctx, "config", "--get", "user.name"); nameErr == nil {
		result.AuthorName = strings.TrimSpace(name)
	}
	if email, emailErr := i.gitOutput(ctx, "config", "--get", "user.email"); emailErr == nil {
		result.AuthorEmail = strings.TrimSpace(email)
	}
	return result, nil
}

func (i Inspector) remotes(ctx context.Context) ([]GitRemote, error) {
	names, err := i.gitOutput(ctx, "remote")
	if err != nil {
		return nil, fmt.Errorf("inspect Git remotes: %w", err)
	}
	byID := map[string]GitRemote{}
	for _, name := range strings.Fields(names) {
		values, getErr := i.gitOutput(ctx, "remote", "get-url", "--all", name)
		if getErr != nil {
			continue
		}
		for _, raw := range strings.Fields(values) {
			id, canonical, canonicalErr := CanonicalGitHubURL(raw)
			if canonicalErr != nil {
				continue
			}
			candidate := GitRemote{Name: name, ID: id, URL: canonical}
			current, exists := byID[id]
			if !exists || (name == "origin" && current.Name != "origin") {
				byID[id] = candidate
			}
			break
		}
	}
	result := make([]GitRemote, 0, len(byID))
	for _, remote := range byID {
		result = append(result, remote)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Name == "origin" {
			return true
		}
		if result[right].Name == "origin" {
			return false
		}
		return result[left].ID < result[right].ID
	})
	return result, nil
}

func selectGitRemote(remotes []GitRemote, choose RemoteChooser) (GitRemote, error) {
	for _, remote := range remotes {
		if remote.Name == "origin" {
			return remote, nil
		}
	}
	if len(remotes) == 1 {
		return remotes[0], nil
	}
	if choose == nil {
		return GitRemote{}, ErrRemoteChoice
	}
	return choose(remotes)
}

func (i Inspector) requireReachable(ctx context.Context, remote GitRemote, head string) error {
	advertised, err := i.gitOutput(ctx, "ls-remote", "--heads", "--tags", "--refs", remote.URL)
	if err != nil {
		return fmt.Errorf("query selected GitHub remote: %w", err)
	}
	tips := map[string]bool{}
	for _, line := range strings.Split(advertised, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			tips[strings.ToLower(fields[0])] = true
		}
	}
	for tip := range tips {
		if tip == head {
			return nil
		}
		if _, err := i.gitOutput(ctx, "cat-file", "-e", tip+"^{commit}"); err != nil {
			if runErr := i.gitRun(ctx, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head",
				"--no-recurse-submodules", remote.URL, tip); runErr != nil {
				continue
			}
		}
		if err := i.gitRun(ctx, "merge-base", "--is-ancestor", head, tip); err == nil {
			return nil
		}
	}
	return ErrUnpushed
}

func (i Inspector) gitOutput(ctx context.Context, arguments ...string) (string, error) {
	output, err := i.runner().Output(ctx, Command{
		Name: i.gitPath(), Args: arguments, Dir: i.Directory,
	})
	return string(output), err
}

func (i Inspector) gitRun(ctx context.Context, arguments ...string) error {
	return i.runner().Run(ctx, Command{Name: i.gitPath(), Args: arguments, Dir: i.Directory})
}

func (i Inspector) runner() CommandRunner {
	if i.Runner != nil {
		return i.Runner
	}
	return ExecCommandRunner{}
}

func (i Inspector) gitPath() string {
	if i.GitPath != "" {
		return i.GitPath
	}
	return "git"
}

// CanonicalGitHubURL accepts GitHub HTTPS and SSH syntax and returns a stable
// lower-case repository ID plus a credential-free HTTPS clone URL.
func CanonicalGitHubURL(value string) (string, string, error) {
	value = strings.TrimSpace(value)
	switch {
	case strings.HasPrefix(value, "git@github.com:"):
		value = strings.TrimPrefix(value, "git@github.com:")
	case strings.HasPrefix(value, "ssh://git@github.com/"):
		value = strings.TrimPrefix(value, "ssh://git@github.com/")
	case strings.Contains(value, "://"):
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") ||
			parsed.User != nil || parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", "", errors.New("repository must be OWNER/REPOSITORY or a GitHub HTTPS or SSH URL")
		}
		value = strings.TrimPrefix(parsed.EscapedPath(), "/")
		if decoded, decodeErr := url.PathUnescape(value); decodeErr == nil {
			value = decoded
		}
	}
	value = strings.TrimSuffix(strings.TrimSuffix(value, "/"), ".git")
	parts := strings.Split(value, "/")
	if len(parts) != 2 || !validGitHubPart(parts[0]) || !validGitHubPart(parts[1]) {
		return "", "", errors.New("repository must be OWNER/REPOSITORY or a GitHub HTTPS or SSH URL")
	}
	id := strings.ToLower(parts[0] + "/" + parts[1])
	return id, "https://github.com/" + id + ".git", nil
}

func validGitHubPart(value string) bool {
	return value != "" && value != "." && value != ".." && githubPartPattern.MatchString(value)
}

// ResolveRelativeGitHubURL applies Git's relative-submodule URL semantics.
func ResolveRelativeGitHubURL(parentURL, childURL string) (string, string, error) {
	if !strings.HasPrefix(childURL, "./") && !strings.HasPrefix(childURL, "../") {
		if !strings.Contains(childURL, "://") && !strings.HasPrefix(childURL, "git@github.com:") {
			return "", "", errors.New("submodule URL must be an absolute GitHub URL or a relative URL")
		}
		return CanonicalGitHubURL(childURL)
	}
	parent, err := url.Parse(parentURL)
	if err != nil {
		return "", "", errors.New("invalid parent repository URL")
	}
	parent.Path = path.Clean(parent.Path) + "/"
	resolved := parent.ResolveReference(&url.URL{Path: childURL})
	return CanonicalGitHubURL(resolved.String())
}
