package lifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strings"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	DefaultIdleTimeoutSeconds      int32 = 7200
	DefaultStoppedRetentionSeconds int32 = 604800
	DefaultFailedRetentionSeconds  int32 = 86400
)

var (
	shaPattern    = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-fA-F]{64}$`)
	secretTerms   = []string{
		"password", "passwd", "secret", "token", "credential", "privatekey",
		"clientsecret", "accesskey", "authorization", "bearer",
	}
)

func DefaultDevSandbox(sandbox *devsandboxv1alpha1.DevSandbox) {
	if sandbox.Spec.DesiredState == "" {
		sandbox.Spec.DesiredState = devsandboxv1alpha1.DesiredStateRunning
	}
	if sandbox.Spec.Lifecycle.IdleTimeoutSeconds == 0 {
		sandbox.Spec.Lifecycle.IdleTimeoutSeconds = DefaultIdleTimeoutSeconds
	}
	if sandbox.Spec.Lifecycle.StoppedRetentionSeconds == 0 {
		sandbox.Spec.Lifecycle.StoppedRetentionSeconds = DefaultStoppedRetentionSeconds
	}
	if sandbox.Spec.Lifecycle.FailedRetentionSeconds == 0 {
		sandbox.Spec.Lifecycle.FailedRetentionSeconds = DefaultFailedRetentionSeconds
	}
	_ = ApplyProfileDefaults(&sandbox.Spec.Profile)
}

func ValidateDevSandbox(sandbox *devsandboxv1alpha1.DevSandbox) error {
	if sandbox == nil {
		return errors.New("sandbox is required")
	}
	var problems []string
	spec := sandbox.Spec
	if spec.Owner.GitHubUserID == "" {
		problems = append(problems, "spec.owner.githubUserId is required")
	}
	if spec.Owner.GitHubLogin == "" {
		problems = append(problems, "spec.owner.githubLogin is required")
	}
	if err := validateSource(spec.Source); err != nil {
		problems = append(problems, err.Error())
	}
	if spec.Template.Name == "" || spec.Template.Version == "" {
		problems = append(problems, "spec.template name and version are required")
	}
	if !digestPattern.MatchString(spec.Template.ImageDigest) {
		problems = append(problems, "spec.template.imageDigest must be a sha256 digest")
	}
	if err := ValidateProfile(spec.Profile); err != nil {
		problems = append(problems, err.Error())
	}
	idle := spec.Lifecycle.IdleTimeoutSeconds
	if idle < 1 || idle > 7200 {
		problems = append(problems, "spec.lifecycle.idleTimeoutSeconds must be between 1 and 7200")
	}
	if spec.Lifecycle.StoppedRetentionSeconds < 0 || spec.Lifecycle.FailedRetentionSeconds < 0 {
		problems = append(problems, "retention seconds cannot be negative")
	}
	if spec.DesiredState != devsandboxv1alpha1.DesiredStateRunning &&
		spec.DesiredState != devsandboxv1alpha1.DesiredStateStopped {
		problems = append(problems, "spec.desiredState must be Running or Stopped")
	}
	if path, found := SecretLikeField(spec); found {
		problems = append(problems, path+" is secret-like and cannot be persisted")
	}
	return problemError(problems)
}

func ValidateDevSandboxUpdate(oldSandbox, newSandbox *devsandboxv1alpha1.DevSandbox) error {
	if oldSandbox == nil || newSandbox == nil {
		return errors.New("old and new sandboxes are required")
	}
	if !reflect.DeepEqual(oldSandbox.Spec.Template, newSandbox.Spec.Template) {
		return errors.New("spec.template is immutable")
	}
	if !reflect.DeepEqual(oldSandbox.Spec.Profile, newSandbox.Spec.Profile) {
		return errors.New("spec.profile is immutable")
	}
	return ValidateDevSandbox(newSandbox)
}

func ValidateProfile(profile devsandboxv1alpha1.SandboxProfile) error {
	canonical, err := CanonicalProfile(profile.Name)
	if err != nil {
		return errors.New("spec.profile.name must be small, medium, or large")
	}
	actual := []string{profile.CPU, profile.Memory, profile.Storage}
	expected := []string{canonical.CPU, canonical.Memory, canonical.Storage}
	for i, value := range actual {
		quantity, err := resource.ParseQuantity(value)
		if err != nil {
			return fmt.Errorf("spec.profile contains invalid resource quantity %q", value)
		}
		expectedQuantity := resource.MustParse(expected[i])
		if quantity.Cmp(expectedQuantity) != 0 {
			return fmt.Errorf("spec.profile values do not match the %s profile", profile.Name)
		}
	}
	return nil
}

func CanonicalProfile(name devsandboxv1alpha1.ProfileName) (devsandboxv1alpha1.SandboxProfile, error) {
	switch name {
	case devsandboxv1alpha1.ProfileSmall:
		return devsandboxv1alpha1.SandboxProfile{Name: name, CPU: "2", Memory: "4Gi", Storage: "20Gi"}, nil
	case devsandboxv1alpha1.ProfileMedium:
		return devsandboxv1alpha1.SandboxProfile{Name: name, CPU: "4", Memory: "8Gi", Storage: "40Gi"}, nil
	case devsandboxv1alpha1.ProfileLarge:
		return devsandboxv1alpha1.SandboxProfile{Name: name, CPU: "8", Memory: "16Gi", Storage: "80Gi"}, nil
	default:
		return devsandboxv1alpha1.SandboxProfile{}, fmt.Errorf("unsupported profile %q", name)
	}
}

func ApplyProfileDefaults(profile *devsandboxv1alpha1.SandboxProfile) error {
	if profile == nil {
		return errors.New("profile is required")
	}
	canonical, err := CanonicalProfile(profile.Name)
	if err != nil {
		return err
	}
	if profile.CPU == "" {
		profile.CPU = canonical.CPU
	}
	if profile.Memory == "" {
		profile.Memory = canonical.Memory
	}
	if profile.Storage == "" {
		profile.Storage = canonical.Storage
	}
	return nil
}

func SecretLikeField(value interface{}) (string, bool) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	var decoded interface{}
	if json.Unmarshal(data, &decoded) != nil {
		return "", false
	}
	return findSecretLikeField(decoded, "")
}

func findSecretLikeField(value interface{}, path string) (string, bool) {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			normalized := normalizeKey(key)
			for _, term := range secretTerms {
				if strings.Contains(normalized, term) {
					return joinPath(path, key), true
				}
			}
			if foundPath, found := findSecretLikeField(child, joinPath(path, key)); found {
				return foundPath, true
			}
		}
	case []interface{}:
		for i, child := range typed {
			if foundPath, found := findSecretLikeField(child, fmt.Sprintf("%s[%d]", path, i)); found {
				return foundPath, true
			}
		}
	}
	return "", false
}

func validateSource(source devsandboxv1alpha1.SandboxSource) error {
	switch source.Type {
	case devsandboxv1alpha1.SourceTypeEmpty:
		if source.RepositoryID != "" || source.RepositoryURL != "" || source.RefName != "" ||
			source.CommitSHA != "" || source.LFS || source.Submodules || source.AuthorName != "" ||
			source.AuthorEmail != "" || source.PrimaryOrg != "" || source.ReadOnly {
			return errors.New("empty source cannot contain git repository fields")
		}
	case devsandboxv1alpha1.SourceTypeGit:
		if source.RepositoryID == "" || source.RepositoryURL == "" {
			return errors.New("git source requires repositoryId and repositoryUrl")
		}
		parsed, err := url.Parse(source.RepositoryURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			return errors.New("git source repositoryUrl must be an absolute HTTPS URL")
		}
		if parsed.User != nil {
			return errors.New("git source repositoryUrl cannot contain credentials")
		}
		if !shaPattern.MatchString(source.CommitSHA) {
			return errors.New("git source requires a 40-character hexadecimal commitSha")
		}
		if invalidSourceText(source.AuthorName, 256) || invalidSourceText(source.AuthorEmail, 320) ||
			invalidSourceText(source.PrimaryOrg, 100) {
			return errors.New("git source contains invalid identity metadata")
		}
	default:
		return errors.New("spec.source.type must be git or empty")
	}
	return nil
}

func invalidSourceText(value string, limit int) bool {
	return len(value) > limit || strings.ContainsAny(value, "\r\n\x00")
}

func normalizeKey(key string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		if r >= 'a' && r <= 'z' {
			return r
		}
		return -1
	}, key)
}

func joinPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

func problemError(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}
