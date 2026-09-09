package templates

import (
	"strings"
	"testing"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func validTemplate() *devsandboxv1alpha1.DevSandboxTemplate {
	return &devsandboxv1alpha1.DevSandboxTemplate{
		Spec: devsandboxv1alpha1.DevSandboxTemplateSpec{
			DisplayName: "Standard",
			Description: "Standard tools",
			Version:     "1.0.0",
			Image: devsandboxv1alpha1.TemplateImage{
				Repository: "example.azurecr.io/devsandbox/standard",
				Digest:     "sha256:" + strings.Repeat("a", 64),
			},
			DefaultProfile: devsandboxv1alpha1.ProfileSmall,
			EntryAction:    devsandboxv1alpha1.EntryActionShell,
			Capabilities:   devsandboxv1alpha1.TemplateCapabilities{},
		},
	}
}

func TestTemplateSpecIsImmutable(t *testing.T) {
	oldTemplate := validTemplate()
	newTemplate := oldTemplate.DeepCopy()
	newTemplate.Spec.Description = "changed"
	if err := ValidateUpdate(oldTemplate, newTemplate); err == nil {
		t.Fatal("expected immutable template update to fail")
	}
}

func TestTemplateRejectsSecretLikeAndOwnedBlueprintFields(t *testing.T) {
	for name, blueprint := range map[string]string{
		"secret": `{"containers":[{"env":{"accessToken":"nope"}}]}`,
		"pvc":    `{"spec":{"volumeClaimTemplates":[]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			template := validTemplate()
			template.Spec.SandboxBlueprint = runtime.RawExtension{Raw: []byte(blueprint)}
			if err := Validate(template); err == nil {
				t.Fatal("expected blueprint validation failure")
			}
		})
	}
}

func TestTemplateDefaults(t *testing.T) {
	template := validTemplate()
	template.Spec.DefaultProfile = ""
	template.Spec.EntryAction = ""
	Default(template)
	if template.Spec.DefaultProfile != devsandboxv1alpha1.ProfileSmall ||
		template.Spec.EntryAction != devsandboxv1alpha1.EntryActionShell {
		t.Fatalf("defaults not applied: %#v", template.Spec)
	}
}

func TestVSCodeTemplateRequiresStartupContract(t *testing.T) {
	template := validTemplate()
	template.Spec.Capabilities.VSCode = true
	if err := Validate(template); err == nil || !strings.Contains(err.Error(), "entryAction vscode") ||
		!strings.Contains(err.Error(), "code-server TCP service port") {
		t.Fatalf("missing VS Code contract was accepted: %v", err)
	}
	template.Spec.EntryAction = devsandboxv1alpha1.EntryActionVSCode
	template.Spec.ServicePorts = []devsandboxv1alpha1.ServicePort{{
		Name: "code-server", Port: 13337, Protocol: corev1.ProtocolTCP,
	}}
	if err := Validate(template); err != nil {
		t.Fatalf("valid VS Code contract rejected: %v", err)
	}
}

func TestVSCodeAITemplateSupportsCombinedCapabilities(t *testing.T) {
	template := validTemplate()
	template.Spec.EntryAction = devsandboxv1alpha1.EntryActionVSCode
	template.Spec.ServicePorts = []devsandboxv1alpha1.ServicePort{{
		Name: "code-server", Port: 13337, Protocol: corev1.ProtocolTCP,
	}}
	template.Spec.Capabilities = devsandboxv1alpha1.TemplateCapabilities{
		VSCode: true, Copilot: true, OpenCode: true,
	}
	if err := Validate(template); err != nil {
		t.Fatalf("valid VS Code AI contract rejected: %v", err)
	}
}
