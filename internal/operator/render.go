package operator

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

type RenderOptions struct {
	Now            time.Time
	OperatingMode  string
	ShutdownTime   time.Time
	ServiceAccount string
	PVCName        string
	UpstreamName   string
}

func resourceName(prefix string, sandbox *devsandboxv1alpha1.DevSandbox) string {
	uid := strings.ToLower(string(sandbox.UID))
	if uid == "" {
		uid = strings.ToLower(sandbox.Name)
	}
	uid = strings.ReplaceAll(uid, "_", "-")
	if len(uid) > 48 {
		uid = uid[:48]
	}
	return prefix + uid
}

func serviceAccountName(sandbox *devsandboxv1alpha1.DevSandbox) string {
	return resourceName("devsandbox-", sandbox)
}

func workspacePVCName(sandbox *devsandboxv1alpha1.DevSandbox) string {
	return resourceName("workspace-", sandbox)
}

func upstreamSandboxName(sandbox *devsandboxv1alpha1.DevSandbox) string {
	return resourceName("devsandbox-", sandbox)
}

func activityLeaseName(sandbox *devsandboxv1alpha1.DevSandbox) string {
	return resourceName(ActivityLeasePrefix, sandbox)
}

func labelsFor(sandbox *devsandboxv1alpha1.DevSandbox) map[string]string {
	return map[string]string{
		ManagedLabel:    "true",
		SandboxLabel:    sandbox.Name,
		SandboxUIDLabel: string(sandbox.UID),
	}
}

func NewWorkspacePVC(sandbox *devsandboxv1alpha1.DevSandbox) (*corev1.PersistentVolumeClaim, error) {
	storage, err := resource.ParseQuantity(sandbox.Spec.Profile.Storage)
	if err != nil {
		return nil, fmt.Errorf("parse workspace storage: %w", err)
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: workspacePVCName(sandbox), Namespace: sandbox.Namespace, Labels: labelsFor(sandbox),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: storage},
			},
			StorageClassName: pointer(StandardSSDStorageClass),
			VolumeMode:       volumeModePointer(corev1.PersistentVolumeFilesystem),
		},
	}, nil
}

func NewSandboxServiceAccount(sandbox *devsandboxv1alpha1.DevSandbox) *corev1.ServiceAccount {
	automount := false
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: serviceAccountName(sandbox), Namespace: sandbox.Namespace, Labels: labelsFor(sandbox),
		},
		AutomountServiceAccountToken: &automount,
	}
}

func RenderUpstreamSandbox(
	sandbox *devsandboxv1alpha1.DevSandbox,
	template *devsandboxv1alpha1.DevSandboxTemplate,
	options RenderOptions,
) (*unstructured.Unstructured, error) {
	if sandbox == nil || template == nil {
		return nil, errors.New("sandbox and template are required")
	}
	image := template.Spec.Image.Repository + "@" + sandbox.Spec.Template.ImageDigest
	if template.Spec.Image.Repository == "" || template.Spec.Image.Digest != sandbox.Spec.Template.ImageDigest {
		return nil, errors.New("template image digest does not match the pinned sandbox digest")
	}

	spec, err := blueprintSpec(template.Spec.SandboxBlueprint)
	if err != nil {
		return nil, err
	}
	if _, found := spec["volumeClaimTemplates"]; found {
		return nil, errors.New("template sandboxBlueprint cannot define volumeClaimTemplates")
	}

	ephemeral, shm, pids := profileSizing(sandbox.Spec.Profile.Name)
	resources := map[string]interface{}{
		"requests": map[string]interface{}{
			"cpu": sandbox.Spec.Profile.CPU, "memory": sandbox.Spec.Profile.Memory,
			"ephemeral-storage": ephemeral,
		},
		"limits": map[string]interface{}{
			"cpu": sandbox.Spec.Profile.CPU, "memory": sandbox.Spec.Profile.Memory,
			"ephemeral-storage": ephemeral,
		},
	}
	containerSecurity := map[string]interface{}{
		"allowPrivilegeEscalation": false,
		"runAsNonRoot":             true,
		"capabilities":             map[string]interface{}{"drop": []interface{}{"ALL"}},
		"seccompProfile":           map[string]interface{}{"type": "RuntimeDefault"},
	}
	mounts := []interface{}{
		map[string]interface{}{"name": "workspace", "mountPath": "/workspace"},
		map[string]interface{}{"name": "runtime", "mountPath": "/run/devsandbox"},
		map[string]interface{}{"name": "dev-shm", "mountPath": "/dev/shm"},
		map[string]interface{}{"name": "broker-identity", "mountPath": "/var/run/secrets/devsandbox/broker", "readOnly": true},
		map[string]interface{}{"name": "broker-ca", "mountPath": "/etc/devsandbox/broker", "readOnly": true},
	}
	volumes := []interface{}{
		map[string]interface{}{"name": "workspace", "persistentVolumeClaim": map[string]interface{}{"claimName": options.PVCName}},
		map[string]interface{}{"name": "runtime", "emptyDir": map[string]interface{}{"medium": "Memory", "sizeLimit": "16Mi"}},
		map[string]interface{}{"name": "dev-shm", "emptyDir": map[string]interface{}{"medium": "Memory", "sizeLimit": shm}},
		map[string]interface{}{"name": "broker-identity", "projected": map[string]interface{}{
			"defaultMode": int64(256),
			"sources": []interface{}{map[string]interface{}{"serviceAccountToken": map[string]interface{}{
				"audience": BrokerAudience, "expirationSeconds": int64(600), "path": "token",
			}}},
		}},
		map[string]interface{}{"name": "broker-ca", "configMap": map[string]interface{}{
			"name": "devsandbox-broker-ca",
		}},
	}
	env := []interface{}{
		map[string]interface{}{"name": "DEVSANDBOX_SERVICE_ACCOUNT_NAME", "value": options.ServiceAccount},
		map[string]interface{}{"name": "DEVSANDBOX_SANDBOX_UID", "value": string(sandbox.UID)},
		map[string]interface{}{"name": "DEVSANDBOX_BROKER_AUDIENCE", "value": BrokerAudience},
		map[string]interface{}{"name": "DEVSANDBOX_BROKER_URL", "value": "https://devsandbox-broker.devsandbox-system.svc.cluster.local:8443/v1/credential"},
		map[string]interface{}{"name": "DEVSANDBOX_BROKER_CA_FILE", "value": "/etc/devsandbox/broker/ca.crt"},
		map[string]interface{}{"name": "DEVSANDBOX_PID_LIMIT", "value": pids},
		map[string]interface{}{"name": "DEVSANDBOX_SOURCE_TYPE", "value": string(sandbox.Spec.Source.Type)},
		map[string]interface{}{"name": "DEVSANDBOX_REPOSITORY_URL", "value": sandbox.Spec.Source.RepositoryURL},
		map[string]interface{}{"name": "DEVSANDBOX_COMMIT_SHA", "value": sandbox.Spec.Source.CommitSHA},
		map[string]interface{}{"name": "DEVSANDBOX_REF_NAME", "value": sandbox.Spec.Source.RefName},
		map[string]interface{}{"name": "DEVSANDBOX_GIT_AUTHOR_NAME", "value": sandbox.Spec.Source.AuthorName},
		map[string]interface{}{"name": "DEVSANDBOX_GIT_AUTHOR_EMAIL", "value": sandbox.Spec.Source.AuthorEmail},
		map[string]interface{}{"name": "DEVSANDBOX_PRIMARY_GITHUB_ORG", "value": sandbox.Spec.Source.PrimaryOrg},
		map[string]interface{}{"name": "DEVSANDBOX_REPOSITORY_READ_ONLY", "value": strconv.FormatBool(sandbox.Spec.Source.ReadOnly)},
	}
	ports := []interface{}{map[string]interface{}{"name": "agent", "containerPort": int64(8081), "protocol": "TCP"}}
	for _, servicePort := range template.Spec.ServicePorts {
		ports = append(ports, map[string]interface{}{
			"name": servicePort.Name, "containerPort": int64(servicePort.Port), "protocol": string(servicePort.Protocol),
		})
	}
	if template.Spec.Capabilities.VSCode {
		vscodePort, found := templateServicePort(template)
		if !found {
			return nil, errors.New("VS Code template requires a code-server TCP service port")
		}
		env = append(env,
			map[string]interface{}{"name": "DEVSANDBOX_TEMPLATE_SERVICE_COMMAND", "value": "/usr/local/bin/devsandbox-code-server"},
			map[string]interface{}{"name": "DEVSANDBOX_TEMPLATE_SERVICE_READY_URL", "value": fmt.Sprintf("http://127.0.0.1:%d/healthz", vscodePort)},
			map[string]interface{}{"name": "DEVSANDBOX_CODE_SERVER_BIND", "value": fmt.Sprintf("0.0.0.0:%d", vscodePort)},
			map[string]interface{}{"name": "DEVSANDBOX_VSCODE_STATE_DIR", "value": "/workspace/.devsandbox/vscode"},
		)
	}
	if template.Spec.Capabilities.OpenCode {
		mounts = append(mounts, map[string]interface{}{
			"name": "opencode-runtime", "mountPath": "/run/devsandbox-opencode",
		})
		volumes = append(volumes, map[string]interface{}{
			"name":     "opencode-runtime",
			"emptyDir": map[string]interface{}{"medium": "Memory", "sizeLimit": "256Mi"},
		})
		env = append(env, map[string]interface{}{
			"name": "DEVSANDBOX_OPENCODE_RUNTIME_DIR", "value": "/run/devsandbox-opencode",
		})
	}
	podSpec := map[string]interface{}{
		"serviceAccountName":            options.ServiceAccount,
		"automountServiceAccountToken":  false,
		"runtimeClassName":              KataRuntimeClass,
		"nodeSelector":                  map[string]interface{}{"devsandbox.github.com/runtime": "kata"},
		"tolerations":                   []interface{}{map[string]interface{}{"key": "devsandbox.github.com/runtime", "operator": "Equal", "value": "kata", "effect": "NoSchedule"}},
		"hostNetwork":                   false,
		"hostPID":                       false,
		"hostIPC":                       false,
		"shareProcessNamespace":         false,
		"terminationGracePeriodSeconds": int64(30),
		"securityContext": map[string]interface{}{
			"runAsNonRoot": true, "runAsUser": int64(1000), "runAsGroup": int64(1000), "fsGroup": int64(1000),
			"seccompProfile": map[string]interface{}{"type": "RuntimeDefault"},
		},
		"volumes": volumes,
		"initContainers": []interface{}{map[string]interface{}{
			"name": "devsandbox-init", "image": image,
			"command":         []interface{}{"/usr/local/bin/devsandbox-init"},
			"securityContext": containerSecurity,
			"resources": map[string]interface{}{
				"requests": map[string]interface{}{"cpu": "100m", "memory": "128Mi", "ephemeral-storage": "1Gi"},
				"limits":   map[string]interface{}{"cpu": "500m", "memory": "512Mi", "ephemeral-storage": "1Gi"},
			},
			"volumeMounts": mounts, "env": env,
		}},
		"containers": []interface{}{map[string]interface{}{
			"name": "workspace", "image": image,
			"command":         []interface{}{"/usr/local/bin/devsandbox-agent"},
			"securityContext": containerSecurity, "resources": resources,
			"volumeMounts": mounts, "env": env,
			"ports": ports,
			"livenessProbe": map[string]interface{}{
				"exec": map[string]interface{}{"command": []interface{}{
					"/bin/sh", "-c", "test -s /run/devsandbox/agent-token && test -d /proc/1",
				}},
				"timeoutSeconds": int64(3),
			},
			"readinessProbe": map[string]interface{}{
				"exec": map[string]interface{}{"command": []interface{}{
					"/bin/sh", "-c", "curl --fail --silent --show-error http://127.0.0.1:8081/readyz >/dev/null",
				}},
				"timeoutSeconds": int64(3),
			},
		}},
	}
	spec["operatingMode"] = options.OperatingMode
	spec["shutdownPolicy"] = "Retain"
	spec["shutdownTime"] = options.ShutdownTime.UTC().Format(time.RFC3339)
	spec["podTemplate"] = map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": labelsFor(sandbox),
			"annotations": map[string]interface{}{
				"devsandbox.io/pid-limit": pids,
				"devsandbox.io/profile":   string(sandbox.Spec.Profile.Name),
			},
		},
		"spec": podSpec,
	}

	result := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": SandboxGVK.GroupVersion().String(),
		"kind":       SandboxGVK.Kind,
		"metadata": map[string]interface{}{
			"name": options.UpstreamName, "namespace": sandbox.Namespace, "labels": labelsFor(sandbox),
		},
		"spec": spec,
	}}
	result.SetGroupVersionKind(SandboxGVK)
	return result, nil
}

func templateServicePort(template *devsandboxv1alpha1.DevSandboxTemplate) (int32, bool) {
	for _, port := range template.Spec.ServicePorts {
		if port.Name == "code-server" && port.Protocol == corev1.ProtocolTCP {
			return port.Port, true
		}
	}
	return 0, false
}

func blueprintSpec(raw runtime.RawExtension) (map[string]interface{}, error) {
	if len(raw.Raw) == 0 && raw.Object == nil {
		return map[string]interface{}{}, nil
	}
	data := raw.Raw
	if len(data) == 0 {
		var err error
		data, err = json.Marshal(raw.Object)
		if err != nil {
			return nil, fmt.Errorf("marshal sandbox blueprint: %w", err)
		}
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("decode sandbox blueprint: %w", err)
	}
	if nested, ok := decoded["spec"].(map[string]interface{}); ok {
		decoded = nested
	}
	return decoded, nil
}

func profileSizing(profile devsandboxv1alpha1.ProfileName) (ephemeral, shm, pids string) {
	switch profile {
	case devsandboxv1alpha1.ProfileLarge:
		return "16Gi", "4Gi", "4096"
	case devsandboxv1alpha1.ProfileMedium:
		return "8Gi", "2Gi", "2048"
	default:
		return "4Gi", "1Gi", "1024"
	}
}

func storageGiB(value string) (int64, error) {
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, err
	}
	bytes := quantity.Value()
	const gib = int64(1024 * 1024 * 1024)
	return (bytes + gib - 1) / gib, nil
}

func objectKey(object metav1.Object) types.NamespacedName {
	return types.NamespacedName{Namespace: object.GetNamespace(), Name: object.GetName()}
}

func pointer[T any](value T) *T {
	return &value
}

func volumeModePointer(value corev1.PersistentVolumeMode) *corev1.PersistentVolumeMode {
	return &value
}
