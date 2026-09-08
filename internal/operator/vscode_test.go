package operator

import (
	"testing"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestVSCodeTemplateRendersSupervisedInternalService(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	sandbox, template := testObjects(now)
	sandbox.Spec.Template.Name = "vscode"
	template.Name = "vscode"
	template.Spec.EntryAction = devsandboxv1alpha1.EntryActionVSCode
	template.Spec.Capabilities.VSCode = true
	template.Spec.ServicePorts = []devsandboxv1alpha1.ServicePort{{
		Name: "code-server", Port: 13337, Protocol: corev1.ProtocolTCP,
	}}
	upstream, err := RenderUpstreamSandbox(sandbox, template, RenderOptions{
		Now: now, OperatingMode: "Running", ShutdownTime: now.Add(time.Hour),
		ServiceAccount: "sandbox", PVCName: "workspace", UpstreamName: "upstream",
	})
	if err != nil {
		t.Fatal(err)
	}
	containers, _, _ := unstructured.NestedSlice(upstream.Object, "spec", "podTemplate", "spec", "containers")
	workload := containers[0].(map[string]interface{})
	ports := workload["ports"].([]interface{})
	if len(ports) != 2 || ports[1].(map[string]interface{})["containerPort"] != int64(13337) {
		t.Fatalf("code-server internal port not rendered: %#v", ports)
	}
	environment := workload["env"].([]interface{})
	assertVSCodeEnvironment(t, environment, "DEVSANDBOX_TEMPLATE_SERVICE_COMMAND", "/usr/local/bin/devsandbox-code-server")
	assertVSCodeEnvironment(t, environment, "DEVSANDBOX_TEMPLATE_SERVICE_READY_URL", "http://127.0.0.1:13337/healthz")
	assertVSCodeEnvironment(t, environment, "DEVSANDBOX_CODE_SERVER_BIND", "0.0.0.0:13337")
	assertVSCodeEnvironment(t, environment, "DEVSANDBOX_VSCODE_STATE_DIR", "/workspace/.devsandbox/vscode")
}

func assertVSCodeEnvironment(t *testing.T, environment []interface{}, name, expected string) {
	t.Helper()
	for _, raw := range environment {
		value := raw.(map[string]interface{})
		if value["name"] == name {
			if value["value"] != expected {
				t.Fatalf("%s = %v, want %s", name, value["value"], expected)
			}
			return
		}
	}
	t.Fatalf("environment variable %s not rendered", name)
}
