package templates

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	"github.com/rmoreirao/AKSAgentSandbox/internal/lifecycle"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var (
	versionPattern     = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
	imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)
)

func Default(template *devsandboxv1alpha1.DevSandboxTemplate) {
	if template.Spec.DefaultProfile == "" {
		template.Spec.DefaultProfile = devsandboxv1alpha1.ProfileSmall
	}
	if template.Spec.EntryAction == "" {
		template.Spec.EntryAction = devsandboxv1alpha1.EntryActionShell
	}
	for i := range template.Spec.ServicePorts {
		if template.Spec.ServicePorts[i].Protocol == "" {
			template.Spec.ServicePorts[i].Protocol = corev1.ProtocolTCP
		}
	}
}

func Validate(template *devsandboxv1alpha1.DevSandboxTemplate) error {
	if template == nil {
		return errors.New("template is required")
	}
	var problems []string
	spec := template.Spec
	if spec.DisplayName == "" {
		problems = append(problems, "spec.displayName is required")
	}
	if !versionPattern.MatchString(spec.Version) {
		problems = append(problems, "spec.version must be a semantic version")
	}
	if spec.Image.Repository == "" {
		problems = append(problems, "spec.image.repository is required")
	}
	if !imageDigestPattern.MatchString(spec.Image.Digest) {
		problems = append(problems, "spec.image.digest must be a sha256 digest")
	}
	if spec.DefaultProfile != devsandboxv1alpha1.ProfileSmall &&
		spec.DefaultProfile != devsandboxv1alpha1.ProfileMedium &&
		spec.DefaultProfile != devsandboxv1alpha1.ProfileLarge {
		problems = append(problems, "spec.defaultProfile must be small, medium, or large")
	}
	if spec.EntryAction != devsandboxv1alpha1.EntryActionShell &&
		spec.EntryAction != devsandboxv1alpha1.EntryActionVSCode &&
		spec.EntryAction != devsandboxv1alpha1.EntryActionCopilot {
		problems = append(problems, "spec.entryAction must be shell, vscode, or copilot")
	}
	ports := make(map[int32]struct{}, len(spec.ServicePorts))
	names := make(map[string]struct{}, len(spec.ServicePorts))
	for _, port := range spec.ServicePorts {
		if port.Name == "" || port.Port < 1 || port.Port > 65535 {
			problems = append(problems, "service ports require a name and port between 1 and 65535")
		}
		if port.Protocol != corev1.ProtocolTCP && port.Protocol != corev1.ProtocolUDP {
			problems = append(problems, "service port protocol must be TCP or UDP")
		}
		if _, duplicate := ports[port.Port]; duplicate {
			problems = append(problems, fmt.Sprintf("service port %d is duplicated", port.Port))
		}
		if _, duplicate := names[port.Name]; duplicate {
			problems = append(problems, fmt.Sprintf("service port name %q is duplicated", port.Name))
		}
		ports[port.Port] = struct{}{}
		names[port.Name] = struct{}{}
	}
	if spec.Capabilities.VSCode {
		if spec.EntryAction != devsandboxv1alpha1.EntryActionVSCode {
			problems = append(problems, "VS Code capability requires spec.entryAction vscode")
		}
		foundCodeServer := false
		for _, port := range spec.ServicePorts {
			if port.Name == "code-server" && port.Protocol == corev1.ProtocolTCP {
				foundCodeServer = true
			}
		}
		if !foundCodeServer {
			problems = append(problems, "VS Code capability requires a code-server TCP service port")
		}
	}
	if path, found := lifecycle.SecretLikeField(spec); found {
		problems = append(problems, path+" is secret-like and cannot be persisted")
	}
	if path, found, err := blueprintField(spec.SandboxBlueprint, "volumeClaimTemplates"); err != nil {
		problems = append(problems, "spec.sandboxBlueprint must contain valid JSON")
	} else if found {
		problems = append(problems, path+" is operator-owned and cannot be set")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func ValidateUpdate(oldTemplate, newTemplate *devsandboxv1alpha1.DevSandboxTemplate) error {
	if oldTemplate == nil || newTemplate == nil {
		return errors.New("old and new templates are required")
	}
	if !reflect.DeepEqual(oldTemplate.Spec, newTemplate.Spec) {
		return errors.New("published template spec is immutable; publish a new version")
	}
	return Validate(newTemplate)
}

func blueprintField(extension runtime.RawExtension, wanted string) (string, bool, error) {
	raw := extension.Raw
	if len(raw) == 0 && extension.Object != nil {
		var err error
		raw, err = json.Marshal(extension.Object)
		if err != nil {
			return "", false, err
		}
	}
	if len(raw) == 0 {
		return "", false, nil
	}
	var value interface{}
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false, err
	}
	path, found := findField(value, "", wanted)
	return path, found, nil
}

func findField(value interface{}, path, wanted string) (string, bool) {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			next := key
			if path != "" {
				next = path + "." + key
			}
			if strings.EqualFold(key, wanted) {
				return next, true
			}
			if foundPath, found := findField(child, next, wanted); found {
				return foundPath, true
			}
		}
	case []interface{}:
		for i, child := range typed {
			if foundPath, found := findField(child, fmt.Sprintf("%s[%d]", path, i), wanted); found {
				return foundPath, true
			}
		}
	}
	return "", false
}
