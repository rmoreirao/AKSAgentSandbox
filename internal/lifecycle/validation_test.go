package lifecycle

import (
	"strings"
	"testing"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
)

func validSandbox() *devsandboxv1alpha1.DevSandbox {
	return &devsandboxv1alpha1.DevSandbox{
		Spec: devsandboxv1alpha1.DevSandboxSpec{
			Owner:    devsandboxv1alpha1.SandboxOwner{GitHubUserID: "123", GitHubLogin: "octocat"},
			Source:   devsandboxv1alpha1.SandboxSource{Type: devsandboxv1alpha1.SourceTypeEmpty},
			Template: devsandboxv1alpha1.SandboxTemplateReference{Name: "standard", Version: "1.0.0", ImageDigest: "sha256:" + strings.Repeat("a", 64)},
			Profile:  devsandboxv1alpha1.SandboxProfile{Name: devsandboxv1alpha1.ProfileSmall, CPU: "2", Memory: "4Gi", Storage: "20Gi"},
			Lifecycle: devsandboxv1alpha1.SandboxLifecycle{
				IdleTimeoutSeconds:      7200,
				StoppedRetentionSeconds: DefaultStoppedRetentionSeconds,
				FailedRetentionSeconds:  DefaultFailedRetentionSeconds,
			},
			DesiredState: devsandboxv1alpha1.DesiredStateRunning,
		},
	}
}

func TestInvalidIdleTimeout(t *testing.T) {
	for _, timeout := range []int32{-1, 0, 7201} {
		sandbox := validSandbox()
		sandbox.Spec.Lifecycle.IdleTimeoutSeconds = timeout
		if err := ValidateDevSandbox(sandbox); err == nil {
			t.Errorf("expected timeout %d to fail", timeout)
		}
	}
}

func TestInvalidProfile(t *testing.T) {
	sandbox := validSandbox()
	sandbox.Spec.Profile.Name = "xlarge"
	if err := ValidateDevSandbox(sandbox); err == nil {
		t.Fatal("expected unsupported profile to fail")
	}
	sandbox = validSandbox()
	sandbox.Spec.Profile.CPU = "3"
	if err := ValidateDevSandbox(sandbox); err == nil {
		t.Fatal("expected modified profile resources to fail")
	}
}

func TestInvalidSource(t *testing.T) {
	sandbox := validSandbox()
	sandbox.Spec.Source.Type = "archive"
	if err := ValidateDevSandbox(sandbox); err == nil {
		t.Fatal("expected unsupported source to fail")
	}
}

func TestSecretLikeFieldsDetected(t *testing.T) {
	value := map[string]interface{}{
		"spec": map[string]interface{}{
			"nested": []interface{}{map[string]interface{}{"clientSecret": "nope"}},
		},
	}
	path, found := SecretLikeField(value)
	if !found || path != "spec.nested[0].clientSecret" {
		t.Fatalf("expected nested secret field, got %q, %t", path, found)
	}
}

func TestSandboxTemplateAndProfileAreImmutable(t *testing.T) {
	oldSandbox := validSandbox()
	newSandbox := oldSandbox.DeepCopy()
	newSandbox.Spec.Profile = devsandboxv1alpha1.SandboxProfile{Name: devsandboxv1alpha1.ProfileMedium, CPU: "4", Memory: "8Gi", Storage: "40Gi"}
	if err := ValidateDevSandboxUpdate(oldSandbox, newSandbox); err == nil {
		t.Fatal("expected profile update to fail")
	}
}

func TestProfileAndLifecycleDefaults(t *testing.T) {
	sandbox := validSandbox()
	sandbox.Spec.Profile.CPU = ""
	sandbox.Spec.Profile.Memory = ""
	sandbox.Spec.Profile.Storage = ""
	sandbox.Spec.Lifecycle = devsandboxv1alpha1.SandboxLifecycle{}
	sandbox.Spec.DesiredState = ""
	DefaultDevSandbox(sandbox)
	if sandbox.Spec.Profile.CPU != "2" || sandbox.Spec.Profile.Memory != "4Gi" ||
		sandbox.Spec.Profile.Storage != "20Gi" ||
		sandbox.Spec.Lifecycle.IdleTimeoutSeconds != DefaultIdleTimeoutSeconds ||
		sandbox.Spec.DesiredState != devsandboxv1alpha1.DesiredStateRunning {
		t.Fatalf("defaults not applied: %#v", sandbox.Spec)
	}
}
