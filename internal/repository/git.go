package repository

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// GitHook is trusted platform code. It never runs a hook, command, or
// executable supplied by a repository.
type GitHook struct {
	GitPath          string
	CredentialHelper string
	Runner           CommandRunner
	Reporter         io.Writer
}

func (h GitHook) Initialize(ctx context.Context, repoDir string, source Source) error {
	if source.Type == "empty" {
		return DirectoryHook{}.Initialize(ctx, repoDir, source)
	}
	remote, err := url.Parse(source.RepositoryURL)
	if err != nil || remote.Scheme != "https" || remote.Host == "" {
		return errors.New("repository URL must be an absolute HTTPS URL")
	}
	if remote.User != nil || remote.RawQuery != "" || remote.Fragment != "" {
		return errors.New("repository URL cannot contain credentials, query, or fragment")
	}
	configDir, err := os.MkdirTemp(filepath.Dir(repoDir), ".git-runtime-")
	if err != nil {
		return fmt.Errorf("create isolated Git configuration: %w", err)
	}
	defer os.RemoveAll(configDir)
	hooksDir := filepath.Join(configDir, "hooks")
	if err := os.Mkdir(hooksDir, 0o700); err != nil {
		return fmt.Errorf("create empty Git hooks directory: %w", err)
	}
	env := append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME="+configDir,
		"XDG_CONFIG_HOME="+configDir,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LFS_SKIP_SMUDGE=1",
	)
	sourceCredentials := !source.ReadOnly
	parent := filepath.Dir(repoDir)
	if err := h.run(ctx, parent, env, hooksDir, sourceCredentials,
		"clone", "--filter=blob:none", "--no-checkout", source.RepositoryURL, repoDir); err != nil {
		return fmt.Errorf("partial clone: %w", err)
	}
	if err := h.run(ctx, repoDir, env, hooksDir, sourceCredentials,
		"fetch", "--quiet", "--filter=blob:none", "--no-tags", "--no-recurse-submodules",
		"origin", source.CommitSHA); err != nil {
		return fmt.Errorf("fetch exact commit: %w", err)
	}
	if err := h.run(ctx, repoDir, env, hooksDir, true, "checkout", "--detach", "--force", source.CommitSHA); err != nil {
		return fmt.Errorf("checkout exact commit: %w", err)
	}
	attached := false
	if validBranchName(source.RefName) {
		output, outputErr := h.output(ctx, repoDir, env, hooksDir, sourceCredentials,
			"ls-remote", "--heads", "origin", "refs/heads/"+source.RefName)
		if outputErr == nil && advertisedSHA(output) == strings.ToLower(source.CommitSHA) {
			if err := h.run(ctx, repoDir, env, hooksDir, true,
				"checkout", "-B", source.RefName, source.CommitSHA); err != nil {
				return fmt.Errorf("attach requested branch: %w", err)
			}
			if err := h.run(ctx, repoDir, env, hooksDir, true,
				"branch", "--set-upstream-to=origin/"+source.RefName, source.RefName); err != nil {
				return fmt.Errorf("configure branch upstream: %w", err)
			}
			attached = true
		}
	}
	if h.Reporter != nil {
		if attached {
			_, _ = fmt.Fprintf(h.Reporter, "checked out %s at %s\n", source.RefName, source.CommitSHA)
		} else {
			_, _ = fmt.Fprintf(h.Reporter, "checked out detached HEAD at %s; requested branch moved or was not a branch\n", source.CommitSHA)
		}
	}
	if source.AuthorName != "" {
		if err := h.run(ctx, repoDir, env, hooksDir, true, "config", "--local", "user.name", source.AuthorName); err != nil {
			return fmt.Errorf("configure Git author name: %w", err)
		}
	}
	if source.AuthorEmail != "" {
		if err := h.run(ctx, repoDir, env, hooksDir, true, "config", "--local", "user.email", source.AuthorEmail); err != nil {
			return fmt.Errorf("configure Git author email: %w", err)
		}
	}
	if err := h.validateSubmoduleGraph(ctx, repoDir, source.RepositoryURL, source.CommitSHA,
		source.PrimaryOrg, env, hooksDir, map[string]bool{}); err != nil {
		return err
	}
	if err := h.run(ctx, repoDir, env, hooksDir, true, "submodule", "sync", "--recursive"); err != nil {
		return fmt.Errorf("synchronize submodules: %w", err)
	}
	if err := h.run(ctx, repoDir, env, hooksDir, true,
		"submodule", "update", "--init", "--recursive", "--filter=blob:none"); err != nil {
		return fmt.Errorf("update submodules: %w", err)
	}
	if err := h.run(ctx, repoDir, env, hooksDir, sourceCredentials, "lfs", "pull", "origin"); err != nil {
		return fmt.Errorf("pull Git LFS objects: %w", err)
	}
	if source.ReadOnly {
		id, _, _ := CanonicalGitHubURL(source.RepositoryURL)
		if err := h.run(ctx, repoDir, env, hooksDir, true,
			"remote", "set-url", "--push", "origin", "devsandbox-read-only://"+id); err != nil {
			return fmt.Errorf("make external repository read-only: %w", err)
		}
	}
	return nil
}

func (h GitHook) validateSubmoduleGraph(
	ctx context.Context, repoDir, parentURL, commit, primaryOrg string,
	env []string, hooksDir string, seen map[string]bool,
) error {
	if err := h.run(ctx, repoDir, env, hooksDir, true, "cat-file", "-e", commit+":.gitmodules"); err != nil {
		return nil
	}
	urlOutput, err := h.output(ctx, repoDir, env, hooksDir, true,
		"config", "--blob="+commit+":.gitmodules", "--get-regexp", `^submodule\..*\.url$`)
	if err != nil {
		return errors.New("invalid .gitmodules URL configuration")
	}
	pathOutput, err := h.output(ctx, repoDir, env, hooksDir, true,
		"config", "--blob="+commit+":.gitmodules", "--get-regexp", `^submodule\..*\.path$`)
	if err != nil {
		return errors.New("invalid .gitmodules path configuration")
	}
	paths := parseSubmoduleConfig(pathOutput, ".path")
	urls := parseSubmoduleConfig(urlOutput, ".url")
	for name, rawURL := range urls {
		submodulePath, ok := paths[name]
		if !ok || submodulePath == "" || filepath.IsAbs(submodulePath) ||
			strings.HasPrefix(filepath.Clean(submodulePath), "..") {
			return fmt.Errorf("submodule %q has an invalid path", name)
		}
		id, canonical, canonicalErr := ResolveRelativeGitHubURL(parentURL, rawURL)
		if canonicalErr != nil {
			return fmt.Errorf("submodule %q has unsupported URL %q", name, rawURL)
		}
		owner := strings.SplitN(id, "/", 2)[0]
		useCredential := primaryOrg != "" && strings.EqualFold(owner, primaryOrg)
		if !useCredential {
			if _, err := h.output(ctx, repoDir, env, hooksDir, false, "ls-remote", "--exit-code", canonical, "HEAD"); err != nil {
				return fmt.Errorf("submodule %q is not a public GitHub repository or a private repository in the primary organization", name)
			}
		}
		tree, err := h.output(ctx, repoDir, env, hooksDir, true,
			"ls-tree", commit, "--", filepath.ToSlash(submodulePath))
		if err != nil {
			return fmt.Errorf("resolve submodule %q commit: %w", name, err)
		}
		submoduleCommit := gitlinkCommit(tree)
		if submoduleCommit == "" {
			return fmt.Errorf("submodule %q does not reference a commit", name)
		}
		key := canonical + "@" + submoduleCommit
		if seen[key] {
			continue
		}
		seen[key] = true
		validationDir, err := os.MkdirTemp(filepath.Dir(repoDir), ".submodule-validate-")
		if err != nil {
			return fmt.Errorf("prepare submodule validation: %w", err)
		}
		if err := os.Remove(validationDir); err != nil {
			return err
		}
		cloneErr := h.run(ctx, filepath.Dir(repoDir), env, hooksDir, useCredential,
			"clone", "--filter=blob:none", "--no-checkout", canonical, validationDir)
		if cloneErr == nil {
			cloneErr = h.run(ctx, validationDir, env, hooksDir, useCredential,
				"fetch", "--quiet", "--filter=blob:none", "--no-tags", "--no-recurse-submodules",
				"origin", submoduleCommit)
		}
		if cloneErr == nil {
			cloneErr = h.validateSubmoduleGraph(ctx, validationDir, canonical, submoduleCommit,
				primaryOrg, env, hooksDir, seen)
		}
		_ = os.RemoveAll(validationDir)
		if cloneErr != nil {
			return fmt.Errorf("validate submodule %q: %w", name, cloneErr)
		}
	}
	return nil
}

func (h GitHook) run(
	ctx context.Context, directory string, env []string, hooksDir string, credentials bool, arguments ...string,
) error {
	return h.runner().Run(ctx, Command{
		Name: h.gitPath(), Args: h.arguments(hooksDir, credentials, arguments...), Dir: directory, Env: env,
	})
}

func (h GitHook) output(
	ctx context.Context, directory string, env []string, hooksDir string, credentials bool, arguments ...string,
) (string, error) {
	output, err := h.runner().Output(ctx, Command{
		Name: h.gitPath(), Args: h.arguments(hooksDir, credentials, arguments...), Dir: directory, Env: env,
	})
	return string(output), err
}

func (h GitHook) arguments(hooksDir string, credentials bool, arguments ...string) []string {
	helper := h.CredentialHelper
	if helper == "" {
		helper = "devsandbox"
	}
	base := []string{"-c", "core.hooksPath=" + hooksDir, "-c", "protocol.file.allow=never"}
	if credentials {
		base = append(base, "-c", "credential.helper="+helper, "-c", "credential.useHttpPath=true")
	} else {
		base = append(base, "-c", "credential.helper=")
	}
	return append(base, arguments...)
}

func (h GitHook) runner() CommandRunner {
	if h.Runner != nil {
		return h.Runner
	}
	return ExecCommandRunner{}
}

func (h GitHook) gitPath() string {
	if h.GitPath != "" {
		return h.GitPath
	}
	return "git"
}

func validBranchName(value string) bool {
	return value != "" && !strings.HasPrefix(value, "-") && !strings.ContainsAny(value, " \t\r\n~^:?*[\\") &&
		!strings.Contains(value, "..") && !strings.HasSuffix(value, ".lock") && !strings.HasSuffix(value, "/")
}

func advertisedSHA(output string) string {
	fields := strings.Fields(output)
	if len(fields) < 2 {
		return ""
	}
	return strings.ToLower(fields[0])
}

func parseSubmoduleConfig(output, suffix string) map[string]string {
	result := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "submodule.") || !strings.HasSuffix(fields[0], suffix) {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(fields[0], "submodule."), suffix)
		result[name] = strings.Join(fields[1:], " ")
	}
	return result
}

func gitlinkCommit(output string) string {
	fields := strings.Fields(output)
	if len(fields) >= 3 && fields[0] == "160000" && fields[1] == "commit" {
		return strings.ToLower(fields[2])
	}
	return ""
}
