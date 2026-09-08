package repository

import (
	"context"
	"errors"
)

var (
	ErrRepositoryNotFound = errors.New("GitHub repository or ref was not found")
	ErrExternalPrivate    = errors.New("private repositories outside the primary organization are unsupported")
)

type RemoteRepository struct {
	ID            string `json:"repositoryId"`
	URL           string `json:"repositoryUrl"`
	Owner         string `json:"owner"`
	DefaultBranch string `json:"defaultBranch"`
	RefName       string `json:"refName"`
	CommitSHA     string `json:"commitSha"`
	AuthorName    string `json:"authorName"`
	AuthorEmail   string `json:"authorEmail"`
	PrimaryOrg    string `json:"primaryOrg"`
	Private       bool   `json:"private"`
	ReadOnly      bool   `json:"readOnly"`
}

// RemoteResolver resolves mutable GitHub names to an immutable commit using a
// current user credential obtained by the management service.
type RemoteResolver interface {
	ResolveRepository(context.Context, string, string, string, string) (RemoteRepository, error)
}
