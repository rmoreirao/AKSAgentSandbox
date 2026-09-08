package cli

import (
	"encoding/json"
	"time"
)

type DeviceStart struct {
	State           string `json:"state"`
	UserCode        string `json:"userCode"`
	VerificationURI string `json:"verificationUri"`
	ExpiresIn       int64  `json:"expiresIn"`
	Interval        int64  `json:"interval"`
}

type Identity struct {
	GitHubUserID string `json:"githubUserId"`
	GitHubLogin  string `json:"githubLogin"`
	Org          string `json:"org,omitempty"`
}

func (i *Identity) UnmarshalJSON(data []byte) error {
	var value struct {
		GitHubUserID string `json:"githubUserId"`
		GitHubLogin  string `json:"githubLogin"`
		UserID       string `json:"userId"`
		Login        string `json:"login"`
		Org          string `json:"org"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	i.GitHubUserID = value.GitHubUserID
	if i.GitHubUserID == "" {
		i.GitHubUserID = value.UserID
	}
	i.GitHubLogin = value.GitHubLogin
	if i.GitHubLogin == "" {
		i.GitHubLogin = value.Login
	}
	i.Org = value.Org
	return nil
}

type LoginResult struct {
	SessionToken string    `json:"sessionToken"`
	ExpiresAt    time.Time `json:"expiresAt"`
	Identity     Identity  `json:"identity"`
}

type TemplateCapabilities struct {
	VSCode     bool `json:"vscode"`
	Copilot    bool `json:"copilot"`
	Playwright bool `json:"playwright"`
}

type Template struct {
	Name           string               `json:"name"`
	DisplayName    string               `json:"displayName"`
	Description    string               `json:"description"`
	Version        string               `json:"version"`
	ImageDigest    string               `json:"imageDigest"`
	DefaultProfile string               `json:"defaultProfile"`
	EntryAction    string               `json:"entryAction"`
	ServicePorts   []int32              `json:"servicePorts,omitempty"`
	Capabilities   TemplateCapabilities `json:"capabilities"`
}

type SandboxSource struct {
	Type          string `json:"type"`
	RepositoryID  string `json:"repositoryId,omitempty"`
	RepositoryURL string `json:"repositoryUrl,omitempty"`
	RefName       string `json:"refName,omitempty"`
	CommitSHA     string `json:"commitSha,omitempty"`
	LFS           bool   `json:"lfs,omitempty"`
	Submodules    bool   `json:"submodules,omitempty"`
	AuthorName    string `json:"authorName,omitempty"`
	AuthorEmail   string `json:"authorEmail,omitempty"`
	PrimaryOrg    string `json:"primaryOrg,omitempty"`
	ReadOnly      bool   `json:"readOnly,omitempty"`
}

type SandboxTemplate struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	ImageDigest string `json:"imageDigest"`
}

type SandboxProfile struct {
	Name    string `json:"name"`
	CPU     string `json:"cpu"`
	Memory  string `json:"memory"`
	Storage string `json:"storage"`
}

type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	ObservedGeneration int64     `json:"observedGeneration,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
	Reason             string    `json:"reason"`
	Message            string    `json:"message"`
}

type Sandbox struct {
	Name                 string          `json:"name"`
	Owner                Identity        `json:"owner"`
	Source               SandboxSource   `json:"source"`
	Template             SandboxTemplate `json:"template"`
	Profile              SandboxProfile  `json:"profile"`
	DesiredState         string          `json:"desiredState"`
	Phase                string          `json:"phase"`
	InitializedCommitSHA string          `json:"initializedCommitSha,omitempty"`
	CreatedAt            time.Time       `json:"createdAt"`
	LastActivityTime     *time.Time      `json:"lastActivityTime,omitempty"`
	IdleDeadline         *time.Time      `json:"idleDeadline,omitempty"`
	StoppedAt            *time.Time      `json:"stoppedAt,omitempty"`
	RetentionDeadline    *time.Time      `json:"retentionDeadline,omitempty"`
	Conditions           []Condition     `json:"conditions,omitempty"`
}

type SandboxEvent struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurredAt"`
	Message    string    `json:"message"`
}

type CreateSandboxRequest struct {
	Name               string        `json:"name,omitempty"`
	Source             SandboxSource `json:"source"`
	Template           string        `json:"template"`
	Profile            string        `json:"profile,omitempty"`
	IdleTimeoutSeconds int32         `json:"idleTimeoutSeconds,omitempty"`
	ResumeExisting     bool          `json:"resumeExisting,omitempty"`
	CreateNew          bool          `json:"createNew,omitempty"`
}

type listResponse[T any] struct {
	Items []T `json:"items"`
}

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}
