package operator

import (
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	Finalizer                = "devsandbox.io/workspace-cleanup"
	SandboxLabel             = "devsandbox.io/sandbox"
	SandboxUIDLabel          = "devsandbox.io/sandbox-uid"
	ManagedLabel             = "devsandbox.io/managed"
	ActivityLeasePrefix      = "activity-"
	StandardSSDStorageClass  = "devsandbox-standard-ssd"
	WorkspacePVAnnotation    = "devsandbox.io/workspace-pv"
	KataRuntimeClass         = "kata-vm-isolation"
	BrokerAudience           = "devsandbox-credential-broker"
	DefaultRequeue           = 30 * time.Second
	ActivityStatusCoalesce   = 60 * time.Second
	ConditionReady           = "Ready"
	ConditionQuotaReady      = "QuotaReady"
	ConditionResourcesReady  = "ResourcesReady"
	ConditionTransitionValid = "TransitionValid"
)

var SandboxGVK = schema.GroupVersionKind{
	Group: "agents.x-k8s.io", Version: "v1beta1", Kind: "Sandbox",
}

const (
	PhasePending  = "Pending"
	PhaseCreating = "Creating"
	PhaseRunning  = "Running"
	PhaseStopped  = "Stopped"
	PhaseFailed   = "Failed"
	PhaseDeleting = "Deleting"
)
