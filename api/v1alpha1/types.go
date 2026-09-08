package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type ProfileName string

const (
	ProfileSmall  ProfileName = "small"
	ProfileMedium ProfileName = "medium"
	ProfileLarge  ProfileName = "large"
)

type EntryAction string

const (
	EntryActionShell   EntryAction = "shell"
	EntryActionVSCode  EntryAction = "vscode"
	EntryActionCopilot EntryAction = "copilot"
)

type SourceType string

const (
	SourceTypeGit   SourceType = "git"
	SourceTypeEmpty SourceType = "empty"
)

type DesiredState string

const (
	DesiredStateRunning DesiredState = "Running"
	DesiredStateStopped DesiredState = "Stopped"
)

type DevSandboxJobState string

const (
	JobStatePending   DevSandboxJobState = "Pending"
	JobStateRunning   DevSandboxJobState = "Running"
	JobStateCompleted DevSandboxJobState = "Completed"
	JobStateFailed    DevSandboxJobState = "Failed"
	JobStateStopped   DevSandboxJobState = "Stopped"
)

type TemplateImage struct {
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
}

type ServicePort struct {
	Name string `json:"name"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// +kubebuilder:validation:Enum=TCP;UDP
	// +kubebuilder:default=TCP
	Protocol corev1.Protocol `json:"protocol,omitempty"`
}

type TemplateCapabilities struct {
	VSCode     bool `json:"vscode"`
	Copilot    bool `json:"copilot"`
	Playwright bool `json:"playwright"`
}

type DevSandboxTemplateSpec struct {
	DisplayName string        `json:"displayName"`
	Description string        `json:"description"`
	Version     string        `json:"version"`
	Image       TemplateImage `json:"image"`
	// +kubebuilder:validation:Enum=small;medium;large
	// +kubebuilder:default=small
	DefaultProfile ProfileName `json:"defaultProfile,omitempty"`
	// +kubebuilder:validation:Enum=shell;vscode;copilot
	// +kubebuilder:default=shell
	EntryAction  EntryAction   `json:"entryAction,omitempty"`
	ServicePorts []ServicePort `json:"servicePorts,omitempty"`
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Schemaless
	SandboxBlueprint runtime.RawExtension `json:"sandboxBlueprint,omitempty"`
	Capabilities     TemplateCapabilities `json:"capabilities"`
}

type DevSandboxTemplateStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=dst
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec) || self.spec == oldSelf.spec",message="published template specs are immutable"
type DevSandboxTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DevSandboxTemplateSpec   `json:"spec"`
	Status            DevSandboxTemplateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DevSandboxTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DevSandboxTemplate `json:"items"`
}

type SandboxOwner struct {
	GitHubUserID string `json:"githubUserId"`
	GitHubLogin  string `json:"githubLogin"`
}

type SandboxSource struct {
	// +kubebuilder:validation:Enum=git;empty
	Type          SourceType `json:"type"`
	RepositoryID  string     `json:"repositoryId,omitempty"`
	RepositoryURL string     `json:"repositoryUrl,omitempty"`
	RefName       string     `json:"refName,omitempty"`
	CommitSHA     string     `json:"commitSha,omitempty"`
	LFS           bool       `json:"lfs,omitempty"`
	Submodules    bool       `json:"submodules,omitempty"`
	AuthorName    string     `json:"authorName,omitempty"`
	AuthorEmail   string     `json:"authorEmail,omitempty"`
	PrimaryOrg    string     `json:"primaryOrg,omitempty"`
	ReadOnly      bool       `json:"readOnly,omitempty"`
}

type SandboxTemplateReference struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	ImageDigest string `json:"imageDigest"`
}

type SandboxProfile struct {
	// +kubebuilder:validation:Enum=small;medium;large
	Name    ProfileName `json:"name"`
	CPU     string      `json:"cpu"`
	Memory  string      `json:"memory"`
	Storage string      `json:"storage"`
}

type SandboxLifecycle struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=7200
	// +kubebuilder:default=7200
	IdleTimeoutSeconds int32 `json:"idleTimeoutSeconds,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=604800
	StoppedRetentionSeconds int32 `json:"stoppedRetentionSeconds,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=86400
	FailedRetentionSeconds int32 `json:"failedRetentionSeconds,omitempty"`
}

type DevSandboxSpec struct {
	Owner     SandboxOwner             `json:"owner"`
	Source    SandboxSource            `json:"source"`
	Template  SandboxTemplateReference `json:"template"`
	Profile   SandboxProfile           `json:"profile"`
	Lifecycle SandboxLifecycle         `json:"lifecycle"`
	// +kubebuilder:validation:Enum=Running;Stopped
	// +kubebuilder:default=Running
	DesiredState DesiredState `json:"desiredState,omitempty"`
}

type DevSandboxStatus struct {
	Phase                string             `json:"phase,omitempty"`
	UpstreamSandboxName  string             `json:"upstreamSandboxName,omitempty"`
	InitializedCommitSHA string             `json:"initializedCommitSha,omitempty"`
	LastActivityTime     *metav1.Time       `json:"lastActivityTime,omitempty"`
	IdleDeadline         *metav1.Time       `json:"idleDeadline,omitempty"`
	StoppedAt            *metav1.Time       `json:"stoppedAt,omitempty"`
	RetentionDeadline    *metav1.Time       `json:"retentionDeadline,omitempty"`
	Conditions           []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=ds
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec) || self.spec.template == oldSelf.spec.template",message="template is immutable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec) || self.spec.profile == oldSelf.spec.profile",message="profile is immutable"
type DevSandbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DevSandboxSpec   `json:"spec"`
	Status            DevSandboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DevSandboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DevSandbox `json:"items"`
}

type QuotaLimits struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=3
	Active int32 `json:"active,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=10
	Retained int32 `json:"retained,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=200
	StorageGiB int64 `json:"storageGiB,omitempty"`
}

type DevSandboxUserQuotaSpec struct {
	GitHubUserID string      `json:"githubUserId"`
	Limits       QuotaLimits `json:"limits"`
}

type QuotaReservation struct {
	ID          string      `json:"id"`
	SandboxName string      `json:"sandboxName"`
	Active      int32       `json:"active,omitempty"`
	Retained    int32       `json:"retained,omitempty"`
	StorageGiB  int64       `json:"storageGiB,omitempty"`
	CreatedAt   metav1.Time `json:"createdAt"`
}

type DevSandboxUserQuotaStatus struct {
	ReservedActive        int32              `json:"reservedActive,omitempty"`
	Retained              int32              `json:"retained,omitempty"`
	ProvisionedStorageGiB int64              `json:"provisionedStorageGiB,omitempty"`
	Reservations          []QuotaReservation `json:"reservations,omitempty"`
	Conditions            []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=dsuq
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.githubUserId) || self.spec.githubUserId == oldSelf.spec.githubUserId",message="githubUserId is immutable"
type DevSandboxUserQuota struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DevSandboxUserQuotaSpec   `json:"spec"`
	Status            DevSandboxUserQuotaStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DevSandboxUserQuotaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DevSandboxUserQuota `json:"items"`
}

type DevSandboxJobSpec struct {
	SandboxName  string `json:"sandboxName"`
	GitHubUserID string `json:"githubUserId"`
}

type DevSandboxJobStatus struct {
	// +kubebuilder:validation:Enum=Pending;Running;Completed;Failed;Stopped
	State       DevSandboxJobState `json:"state,omitempty"`
	StartedAt   *metav1.Time       `json:"startedAt,omitempty"`
	CompletedAt *metav1.Time       `json:"completedAt,omitempty"`
	ExitCode    *int32             `json:"exitCode,omitempty"`
	Reason      string             `json:"reason,omitempty"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
}

// DevSandboxJob stores restart-safe job identity and lifecycle summaries. Commands
// and output intentionally remain in the sandbox supervisor and are never persisted.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced,shortName=dsj
// +kubebuilder:subresource:status
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec) || self.spec == oldSelf.spec",message="job specs are immutable"
type DevSandboxJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DevSandboxJobSpec   `json:"spec"`
	Status            DevSandboxJobStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DevSandboxJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DevSandboxJob `json:"items"`
}

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&DevSandboxTemplate{}, &DevSandboxTemplateList{},
		&DevSandbox{}, &DevSandboxList{},
		&DevSandboxUserQuota{}, &DevSandboxUserQuotaList{},
		&DevSandboxJob{}, &DevSandboxJobList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
