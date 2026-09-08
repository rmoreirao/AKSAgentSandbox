package api

import (
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type CreateSandboxRequest struct {
	Name               string                           `json:"name,omitempty"`
	Source             devsandboxv1alpha1.SandboxSource `json:"source"`
	Template           string                           `json:"template"`
	Profile            devsandboxv1alpha1.ProfileName   `json:"profile,omitempty"`
	IdleTimeoutSeconds int32                            `json:"idleTimeoutSeconds,omitempty"`
	ResumeExisting     bool                             `json:"resumeExisting,omitempty"`
	CreateNew          bool                             `json:"createNew,omitempty"`
}

type User struct {
	GitHubUserID string `json:"githubUserId"`
	GitHubLogin  string `json:"githubLogin"`
}

type Template struct {
	Name           string                                  `json:"name"`
	DisplayName    string                                  `json:"displayName"`
	Description    string                                  `json:"description"`
	Version        string                                  `json:"version"`
	ImageDigest    string                                  `json:"imageDigest"`
	DefaultProfile devsandboxv1alpha1.ProfileName          `json:"defaultProfile"`
	EntryAction    devsandboxv1alpha1.EntryAction          `json:"entryAction"`
	ServicePorts   []int32                                 `json:"servicePorts,omitempty"`
	Capabilities   devsandboxv1alpha1.TemplateCapabilities `json:"capabilities"`
}

type Sandbox struct {
	Name                 string                                      `json:"name"`
	Owner                User                                        `json:"owner"`
	Source               devsandboxv1alpha1.SandboxSource            `json:"source"`
	Template             devsandboxv1alpha1.SandboxTemplateReference `json:"template"`
	Profile              devsandboxv1alpha1.SandboxProfile           `json:"profile"`
	DesiredState         devsandboxv1alpha1.DesiredState             `json:"desiredState"`
	Phase                string                                      `json:"phase"`
	InitializedCommitSHA string                                      `json:"initializedCommitSha,omitempty"`
	CreatedAt            time.Time                                   `json:"createdAt"`
	LastActivityTime     *metav1.Time                                `json:"lastActivityTime,omitempty"`
	IdleDeadline         *metav1.Time                                `json:"idleDeadline,omitempty"`
	StoppedAt            *metav1.Time                                `json:"stoppedAt,omitempty"`
	RetentionDeadline    *metav1.Time                                `json:"retentionDeadline,omitempty"`
	Conditions           []metav1.Condition                          `json:"conditions,omitempty"`
}

type SandboxEvent struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	OccurredAt time.Time `json:"occurredAt"`
	Message    string    `json:"message"`
}

func templateView(value devsandboxv1alpha1.DevSandboxTemplate) Template {
	ports := make([]int32, 0, len(value.Spec.ServicePorts))
	for _, port := range value.Spec.ServicePorts {
		ports = append(ports, port.Port)
	}
	return Template{
		Name: value.Name, DisplayName: value.Spec.DisplayName, Description: value.Spec.Description,
		Version: value.Spec.Version, ImageDigest: value.Spec.Image.Digest,
		DefaultProfile: value.Spec.DefaultProfile, EntryAction: value.Spec.EntryAction,
		ServicePorts: ports, Capabilities: value.Spec.Capabilities,
	}
}

func sandboxView(value devsandboxv1alpha1.DevSandbox) Sandbox {
	phase := value.Status.Phase
	if phase == "" {
		phase = "Pending"
	}
	return Sandbox{
		Name:   value.Name,
		Owner:  User{GitHubUserID: value.Spec.Owner.GitHubUserID, GitHubLogin: value.Spec.Owner.GitHubLogin},
		Source: value.Spec.Source, Template: value.Spec.Template, Profile: value.Spec.Profile,
		DesiredState: value.Spec.DesiredState, Phase: phase,
		InitializedCommitSHA: value.Status.InitializedCommitSHA, CreatedAt: value.CreationTimestamp.Time,
		LastActivityTime: value.Status.LastActivityTime, IdleDeadline: value.Status.IdleDeadline,
		StoppedAt: value.Status.StoppedAt, RetentionDeadline: value.Status.RetentionDeadline,
		Conditions: value.Status.Conditions,
	}
}

func eventView(value corev1.Event) SandboxEvent {
	occurred := value.EventTime.Time
	if occurred.IsZero() {
		occurred = value.LastTimestamp.Time
	}
	if occurred.IsZero() {
		occurred = value.CreationTimestamp.Time
	}
	return SandboxEvent{
		ID: string(value.UID), Type: value.Reason, OccurredAt: occurred, Message: value.Message,
	}
}
