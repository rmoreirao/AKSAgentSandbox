package v1alpha1

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

func TestSchemeAndJSONSerialization(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("register scheme: %v", err)
	}
	object := &DevSandbox{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "DevSandbox"},
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "devsandbox-workloads"},
		Spec: DevSandboxSpec{
			Owner:    SandboxOwner{GitHubUserID: "123", GitHubLogin: "octocat"},
			Source:   SandboxSource{Type: SourceTypeEmpty},
			Template: SandboxTemplateReference{Name: "standard", Version: "1.0.0", ImageDigest: "sha256:" + strings.Repeat("a", 64)},
			Profile:  SandboxProfile{Name: ProfileSmall, CPU: "2", Memory: "4Gi", Storage: "20Gi"},
			Lifecycle: SandboxLifecycle{
				IdleTimeoutSeconds: 7200,
			},
			DesiredState: DesiredStateRunning,
		},
	}
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded DevSandbox
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Spec.Owner.GitHubUserID != "123" || !strings.Contains(string(data), `"idleTimeoutSeconds":7200`) {
		t.Fatalf("unexpected JSON: %s", data)
	}
	kinds, _, err := scheme.ObjectKinds(object)
	if err != nil || len(kinds) != 1 || kinds[0] != GroupVersion.WithKind("DevSandbox") {
		t.Fatalf("unexpected scheme kinds %v: %v", kinds, err)
	}
}

func TestJobSummaryContainsNoCommandOrOutput(t *testing.T) {
	job := DevSandboxJob{
		Spec:   DevSandboxJobSpec{SandboxName: "sample", GitHubUserID: "123"},
		Status: DevSandboxJobStatus{State: JobStateRunning},
	}
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"command", "stdout", "stderr", "output"} {
		if strings.Contains(strings.ToLower(string(data)), forbidden) {
			t.Fatalf("job persisted forbidden field %q: %s", forbidden, data)
		}
	}
}

func TestOpenAPICoversSectionTenOperations(t *testing.T) {
	data, err := os.ReadFile("../openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		OpenAPI    string                            `yaml:"openapi"`
		Paths      map[string]map[string]interface{} `yaml:"paths"`
		Components struct {
			Schemas map[string]interface{} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	expected := map[string][]string{
		"/v1/auth/device/start":             {"post"},
		"/v1/auth/device/poll":              {"post"},
		"/v1/me":                            {"get"},
		"/v1/templates":                     {"get"},
		"/v1/templates/{name}":              {"get"},
		"/v1/sandboxes":                     {"get", "post"},
		"/v1/sandboxes/{name}":              {"get", "delete"},
		"/v1/sandboxes/{name}/stop":         {"post"},
		"/v1/sandboxes/{name}/resume":       {"post"},
		"/v1/sandboxes/{name}/events":       {"get"},
		"/v1/sandboxes/{name}/exec":         {"post"},
		"/v1/sandboxes/{name}/jobs":         {"get"},
		"/v1/sandboxes/{name}/jobs/{jobId}": {"delete"},
		"/v1/sandboxes/{name}/vscode-url":   {"post"},
		"/v1/sandboxes/{name}/shell":        {"get"},
		"/v1/sandboxes/{name}/exec/stream":  {"get"},
		"/v1/sandboxes/{name}/ports/{port}": {"get"},
	}
	for path, methods := range expected {
		item, ok := document.Paths[path]
		if !ok {
			t.Errorf("missing OpenAPI path %s", path)
			continue
		}
		for _, method := range methods {
			if _, ok := item[method]; !ok {
				t.Errorf("missing %s %s", strings.ToUpper(method), path)
			}
		}
	}
	if !strings.HasPrefix(document.OpenAPI, "3.") {
		t.Errorf("expected OpenAPI 3 document, got %q", document.OpenAPI)
	}
	if _, ok := document.Components.Schemas["Error"]; !ok {
		t.Error("stable Error schema is missing")
	}
}
