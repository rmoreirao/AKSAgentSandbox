package cli

import (
	"context"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	repositorypkg "github.com/rmoreirao/AKSAgentSandbox/internal/repository"
)

var invalidDNSCharacters = regexp.MustCompile(`[^a-z0-9]+`)
var dnsNamePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

type RepositoryTarget struct {
	ID          string
	URL         string
	Ref         string
	CommitSHA   string
	AuthorName  string
	AuthorEmail string
	PrimaryOrg  string
	ReadOnly    bool
}

type RepositoryResolver interface {
	Current(context.Context, *Prompter) (RepositoryTarget, error)
}

type GitRepositoryResolver struct {
	Directory string
	GitPath   string
	Runner    repositorypkg.CommandRunner
}

func (r GitRepositoryResolver) Current(ctx context.Context, prompt *Prompter) (RepositoryTarget, error) {
	value, err := (repositorypkg.Inspector{
		Directory: r.Directory, GitPath: r.GitPath, Runner: r.Runner,
	}).Inspect(ctx, func(remotes []repositorypkg.GitRemote) (repositorypkg.GitRemote, error) {
		choices := make([]string, 0, len(remotes))
		byID := make(map[string]repositorypkg.GitRemote, len(remotes))
		for _, remote := range remotes {
			choices = append(choices, remote.ID)
			byID[remote.ID] = remote
		}
		choice, selectErr := prompt.Select("Multiple GitHub remotes are available; choose a repository", choices)
		return byID[choice], selectErr
	})
	if err != nil {
		switch {
		case errors.Is(err, repositorypkg.ErrNotRepository):
			return RepositoryTarget{}, cliError(ExitNotFound, "repository_not_found", err.Error(), nil)
		case errors.Is(err, repositorypkg.ErrDirty):
			return RepositoryTarget{}, cliError(ExitInvalid, "repository_dirty", err.Error(), nil)
		case errors.Is(err, repositorypkg.ErrUnpushed):
			return RepositoryTarget{}, cliError(ExitInvalid, "repository_unpushed", err.Error(), nil)
		default:
			var cliErr *Error
			if errors.As(err, &cliErr) {
				return RepositoryTarget{}, err
			}
			return RepositoryTarget{}, cliError(ExitInvalid, "git_inspection_failed", err.Error(), nil)
		}
	}
	return RepositoryTarget{
		ID: value.Remote.ID, URL: value.Remote.URL, Ref: value.RefName, CommitSHA: value.CommitSHA,
		AuthorName: value.AuthorName, AuthorEmail: value.AuthorEmail,
	}, nil
}

func ParseRepository(value string) (RepositoryTarget, error) {
	id, canonical, err := repositorypkg.CanonicalGitHubURL(value)
	if err != nil {
		return RepositoryTarget{}, invalid("repository must be OWNER/REPOSITORY or a GitHub URL")
	}
	return RepositoryTarget{ID: id, URL: canonical}, nil
}

func GenerateName(prefix string, random io.Reader) (string, error) {
	prefix = strings.ToLower(prefix)
	prefix = invalidDNSCharacters.ReplaceAllString(prefix, "-")
	prefix = strings.Trim(prefix, "-")
	if prefix == "" {
		prefix = "sandbox"
	}
	const suffixBytes = 5
	data := make([]byte, suffixBytes)
	if _, err := io.ReadFull(random, data); err != nil {
		return "", fmt.Errorf("generate sandbox name: %w", err)
	}
	suffix := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(data))
	maxPrefix := 63 - 1 - len(suffix)
	if len(prefix) > maxPrefix {
		prefix = strings.Trim(prefix[:maxPrefix], "-")
	}
	return prefix + "-" + suffix, nil
}

func namePrefix(source SandboxSource, template string) string {
	if source.Type == "git" {
		return strings.TrimSuffix(path.Base(source.RepositoryURL), ".git")
	}
	return template
}

func sameRepository(left, right string) bool {
	leftTarget, leftErr := ParseRepository(left)
	rightTarget, rightErr := ParseRepository(right)
	return leftErr == nil && rightErr == nil && leftTarget.ID == rightTarget.ID
}

func sandboxMatchesRepository(value Sandbox, repository RepositoryTarget) bool {
	return value.Source.Type == "git" && sameRepository(value.Source.RepositoryURL, repository.URL)
}

func equivalentSandbox(value Sandbox, source SandboxSource, template, profile string) bool {
	if value.Phase != "Stopped" || value.Template.Name != template || value.Profile.Name != profile ||
		value.Source.Type != source.Type {
		return false
	}
	if source.Type == "empty" {
		return true
	}
	if !sameRepository(value.Source.RepositoryURL, source.RepositoryURL) {
		return false
	}
	if source.CommitSHA != "" && value.Source.CommitSHA != "" {
		return strings.EqualFold(source.CommitSHA, value.Source.CommitSHA)
	}
	return source.RefName == "" || value.Source.RefName == source.RefName
}
