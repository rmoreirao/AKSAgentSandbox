package operator

import (
	"context"
	"strings"
	"testing"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	"github.com/rmoreirao/AKSAgentSandbox/internal/lifecycle"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testNamespace = "devsandbox-workloads"

func TestCreateReconciliationRendersHardenedSandbox(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	sandbox, template := testObjects(now)
	reconciler := newTestReconciler(t, now, sandbox, template)
	reconcileTimes(t, reconciler, sandbox, 2)

	var pvc corev1.PersistentVolumeClaim
	mustGet(t, reconciler.Client, types.NamespacedName{Namespace: testNamespace, Name: workspacePVCName(sandbox)}, &pvc)
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != StandardSSDStorageClass ||
		len(pvc.OwnerReferences) != 1 || pvc.OwnerReferences[0].UID != sandbox.UID {
		t.Fatalf("PVC is not operator-owned Standard SSD RWO storage: %#v", pvc.Spec)
	}

	var account corev1.ServiceAccount
	mustGet(t, reconciler.Client, types.NamespacedName{Namespace: testNamespace, Name: serviceAccountName(sandbox)}, &account)
	if account.AutomountServiceAccountToken == nil || *account.AutomountServiceAccountToken {
		t.Fatal("sandbox service account automatically mounts a token")
	}

	var activityLease coordinationv1.Lease
	mustGet(t, reconciler.Client,
		types.NamespacedName{Namespace: testNamespace, Name: activityLeaseName(sandbox)}, &activityLease)
	if activityLease.Spec.LeaseDurationSeconds == nil ||
		*activityLease.Spec.LeaseDurationSeconds != int32(lifecycle.ConnectionStaleAfter/time.Second) {
		t.Fatalf("activity Lease has the wrong stale duration: %#v", activityLease.Spec)
	}

	upstream := getUpstream(t, reconciler.Client, sandbox)
	assertNestedString(t, upstream.Object, KataRuntimeClass, "spec", "podTemplate", "spec", "runtimeClassName")
	assertNestedString(t, upstream.Object, "Running", "spec", "operatingMode")
	assertNestedString(t, upstream.Object, "Retain", "spec", "shutdownPolicy")
	assertNestedString(t, upstream.Object, now.Add(10*time.Minute).Format(time.RFC3339), "spec", "shutdownTime")
	if _, found, _ := unstructured.NestedFieldNoCopy(upstream.Object, "spec", "volumeClaimTemplates"); found {
		t.Fatal("rendered Sandbox contains volumeClaimTemplates")
	}
	containers, found, err := unstructured.NestedSlice(upstream.Object, "spec", "podTemplate", "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		t.Fatalf("missing workload container: %v %#v", err, containers)
	}
	workload := containers[0].(map[string]interface{})
	if workload["image"] != template.Spec.Image.Repository+"@"+template.Spec.Image.Digest {
		t.Fatalf("image is not pinned to exact digest: %v", workload["image"])
	}
	assertContainerResources(t, workload, "2", "4Gi", "4Gi")
	security := workload["securityContext"].(map[string]interface{})
	if security["allowPrivilegeEscalation"] != false || security["runAsNonRoot"] != true {
		t.Fatalf("unsafe container security context: %#v", security)
	}
	volumes, _, _ := unstructured.NestedSlice(upstream.Object, "spec", "podTemplate", "spec", "volumes")
	assertProjectedBrokerIdentity(t, volumes)
	assertExistingWorkspaceClaim(t, volumes, pvc.Name)
	assertMemoryVolume(t, volumes, "dev-shm", "1Gi")

	var quota devsandboxv1alpha1.DevSandboxUserQuota
	mustGet(t, reconciler.Client, types.NamespacedName{Namespace: testNamespace, Name: quotaNameForUser("123")}, &quota)
	if quota.Status.ReservedActive != 1 || quota.Status.Retained != 1 ||
		quota.Status.ProvisionedStorageGiB != 20 {
		t.Fatalf("unexpected quota reservation: %#v", quota.Status)
	}
}

func TestStopAndResumeReconciliation(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	sandbox, template := testObjects(now)
	reconciler := newTestReconciler(t, now, sandbox, template)
	reconcileTimes(t, reconciler, sandbox, 2)

	current := getSandbox(t, reconciler.Client, sandbox.Name)
	current.Spec.DesiredState = devsandboxv1alpha1.DesiredStateStopped
	mustUpdate(t, reconciler.Client, current)
	reconcileTimes(t, reconciler, sandbox, 1)
	current = getSandbox(t, reconciler.Client, sandbox.Name)
	if current.Status.Phase != PhaseStopped || current.Status.StoppedAt == nil ||
		current.Status.RetentionDeadline == nil ||
		current.Status.RetentionDeadline.Sub(current.Status.StoppedAt.Time) != 7*24*time.Hour {
		t.Fatalf("stop status not projected: %#v", current.Status)
	}
	assertNestedString(t, getUpstream(t, reconciler.Client, sandbox).Object, "Suspended", "spec", "operatingMode")
	assertQuota(t, reconciler.Client, 0, 1, 20)

	current.Spec.DesiredState = devsandboxv1alpha1.DesiredStateRunning
	mustUpdate(t, reconciler.Client, current)
	reconcileTimes(t, reconciler, sandbox, 1)
	current = getSandbox(t, reconciler.Client, sandbox.Name)
	if current.Status.StoppedAt != nil || current.Status.RetentionDeadline != nil {
		t.Fatalf("resume did not clear retention: %#v", current.Status)
	}
	assertNestedString(t, getUpstream(t, reconciler.Client, sandbox).Object, "Running", "spec", "operatingMode")
	assertQuota(t, reconciler.Client, 1, 1, 20)
}

func TestIdleReconciliationStopsSandbox(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	sandbox, template := testObjects(now)
	sandbox.CreationTimestamp = metav1.NewTime(now.Add(-10 * time.Minute))
	sandbox.Spec.Lifecycle.IdleTimeoutSeconds = 60
	sandbox.Status.Phase = PhaseRunning
	reconciler := newTestReconciler(t, now, sandbox, template)
	reconcileTimes(t, reconciler, sandbox, 2)
	current := getSandbox(t, reconciler.Client, sandbox.Name)
	if current.Spec.DesiredState != devsandboxv1alpha1.DesiredStateStopped ||
		current.Status.Phase != PhaseStopped {
		t.Fatalf("idle sandbox was not stopped: spec=%s status=%#v", current.Spec.DesiredState, current.Status)
	}
}

func TestIdleReconciliationDoesNotStopCreatingSandbox(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	sandbox, template := testObjects(now)
	sandbox.CreationTimestamp = metav1.NewTime(now.Add(-10 * time.Minute))
	sandbox.Spec.Lifecycle.IdleTimeoutSeconds = 60
	reconciler := newTestReconciler(t, now, sandbox, template)
	reconcileTimes(t, reconciler, sandbox, 2)
	current := getSandbox(t, reconciler.Client, sandbox.Name)
	if current.Spec.DesiredState != devsandboxv1alpha1.DesiredStateRunning ||
		current.Status.Phase != PhaseCreating {
		t.Fatalf("creating sandbox was idle-stopped: spec=%s status=%#v", current.Spec.DesiredState, current.Status)
	}
}

func TestRetentionDeletesStoppedAndFailedSandboxes(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	for _, phase := range []string{PhaseStopped, PhaseFailed} {
		t.Run(phase, func(t *testing.T) {
			sandbox, template := testObjects(now)
			sandbox.Name = strings.ToLower(phase)
			sandbox.UID = types.UID(strings.ToLower(phase) + "-uid")
			sandbox.Finalizers = []string{Finalizer}
			sandbox.Status.Phase = phase
			sandbox.Status.RetentionDeadline = timePointer(now.Add(-time.Second))
			if phase == PhaseStopped {
				sandbox.Spec.DesiredState = devsandboxv1alpha1.DesiredStateStopped
			}

			reconciler := newTestReconciler(t, now, sandbox, template)
			reconcileTimes(t, reconciler, sandbox, 3)
			var current devsandboxv1alpha1.DevSandbox
			err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: sandbox.Name}, &current)
			if !apierrors.IsNotFound(err) {
				t.Fatalf("retained %s sandbox was not deleted: %v", phase, err)
			}
		})
	}
}

func TestFailedCreateGets24HourRetention(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	sandbox, template := testObjects(now)
	template.Spec.Image.Digest = "sha256:" + strings.Repeat("b", 64)
	reconciler := newTestReconciler(t, now, sandbox, template)
	reconcileTimes(t, reconciler, sandbox, 2)
	current := getSandbox(t, reconciler.Client, sandbox.Name)
	if current.Status.Phase != PhaseFailed || current.Status.RetentionDeadline == nil ||
		current.Status.RetentionDeadline.Sub(now) != 24*time.Hour {
		t.Fatalf("failed create retention is not 24 hours: %#v", current.Status)
	}
}

func TestQuotaRejectionCreatesNoOwnedResources(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	sandbox, template := testObjects(now)
	quota := &devsandboxv1alpha1.DevSandboxUserQuota{
		ObjectMeta: metav1.ObjectMeta{Name: quotaNameForUser("123"), Namespace: testNamespace},
		Spec: devsandboxv1alpha1.DevSandboxUserQuotaSpec{
			GitHubUserID: "123", Limits: devsandboxv1alpha1.QuotaLimits{Active: 0, Retained: 10, StorageGiB: 200},
		},
	}
	reconciler := newTestReconciler(t, now, sandbox, template, quota)
	reconcileTimes(t, reconciler, sandbox, 2)
	current := getSandbox(t, reconciler.Client, sandbox.Name)
	if conditionReason(current.Status.Conditions, ConditionQuotaReady) != "QuotaExceeded" {
		t.Fatalf("quota rejection condition missing: %#v", current.Status.Conditions)
	}
	var pvc corev1.PersistentVolumeClaim
	err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: workspacePVCName(sandbox)}, &pvc)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("PVC created despite quota rejection: %v", err)
	}
}

func TestDeletionCleansPVCAndIsIdempotent(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	sandbox, template := testObjects(now)
	reconciler := newTestReconciler(t, now, sandbox, template)
	reconcileTimes(t, reconciler, sandbox, 2)
	current := getSandbox(t, reconciler.Client, sandbox.Name)
	var pvc corev1.PersistentVolumeClaim
	mustGet(t, reconciler.Client,
		types.NamespacedName{Namespace: testNamespace, Name: workspacePVCName(sandbox)}, &pvc)
	pvc.Spec.VolumeName = "pvc-managed-disk"
	mustUpdate(t, reconciler.Client, &pvc)
	volume := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: pvc.Spec.VolumeName}}
	if err := reconciler.Create(context.Background(), volume); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Delete(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	reconcileTimes(t, reconciler, sandbox, 4)
	err := reconciler.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: workspacePVCName(sandbox)}, &pvc)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("PVC remains after deletion: %v", err)
	}
	err = reconciler.Get(context.Background(), types.NamespacedName{Name: volume.Name}, volume)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("managed-disk PV remains after deletion: %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: sandbox.Name},
	}); err != nil {
		t.Fatalf("idempotent deletion failed: %v", err)
	}
	assertQuota(t, reconciler.Client, 0, 0, 0)
}

func TestFailedSandboxRejectsResume(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	sandbox, template := testObjects(now)
	sandbox.Finalizers = []string{Finalizer}
	sandbox.Status.Phase = PhaseFailed
	sandbox.Status.RetentionDeadline = timePointer(now.Add(24 * time.Hour))
	reconciler := newTestReconciler(t, now, sandbox, template)
	reconcileTimes(t, reconciler, sandbox, 1)
	current := getSandbox(t, reconciler.Client, sandbox.Name)
	if conditionReason(current.Status.Conditions, ConditionTransitionValid) != "FailedSandbox" {
		t.Fatalf("invalid resume was not rejected: %#v", current.Status.Conditions)
	}
}

func TestUpstreamStateAndConditionsAreProjected(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	sandbox, template := testObjects(now)
	reconciler := newTestReconciler(t, now, sandbox, template)
	reconcileTimes(t, reconciler, sandbox, 2)
	upstream := getUpstream(t, reconciler.Client, sandbox)
	upstream.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
		"type": "Ready", "status": "True", "reason": "PodReady", "message": "ready",
	}}}
	mustUpdate(t, reconciler.Client, upstream)
	reconcileTimes(t, reconciler, sandbox, 1)
	current := getSandbox(t, reconciler.Client, sandbox.Name)
	if current.Status.Phase != PhaseRunning || conditionReason(current.Status.Conditions, "UpstreamReady") != "PodReady" {
		t.Fatalf("upstream state was not projected: %#v", current.Status)
	}
}

func newTestReconciler(t *testing.T, now time.Time, objects ...client.Object) *DevSandboxReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := AddSchemes(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(SandboxGVK, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(SandboxGVK.GroupVersion().WithKind("SandboxList"), &unstructured.UnstructuredList{})
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &DevSandboxReconciler{Client: fakeClient, Scheme: scheme, Now: func() time.Time { return now }}
}

func testObjects(now time.Time) (*devsandboxv1alpha1.DevSandbox, *devsandboxv1alpha1.DevSandboxTemplate) {
	digest := "sha256:" + strings.Repeat("a", 64)
	sandbox := &devsandboxv1alpha1.DevSandbox{
		TypeMeta: metav1.TypeMeta{APIVersion: devsandboxv1alpha1.GroupVersion.String(), Kind: "DevSandbox"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "sample", Namespace: testNamespace, UID: types.UID("11111111-2222-3333-4444-555555555555"),
			CreationTimestamp: metav1.NewTime(now),
		},
		Spec: devsandboxv1alpha1.DevSandboxSpec{
			Owner:    devsandboxv1alpha1.SandboxOwner{GitHubUserID: "123", GitHubLogin: "octocat"},
			Source:   devsandboxv1alpha1.SandboxSource{Type: devsandboxv1alpha1.SourceTypeEmpty},
			Template: devsandboxv1alpha1.SandboxTemplateReference{Name: "standard", Version: "1.0.0", ImageDigest: digest},
			Profile:  devsandboxv1alpha1.SandboxProfile{Name: devsandboxv1alpha1.ProfileSmall, CPU: "2", Memory: "4Gi", Storage: "20Gi"},
			Lifecycle: devsandboxv1alpha1.SandboxLifecycle{
				IdleTimeoutSeconds: 600, StoppedRetentionSeconds: 604800, FailedRetentionSeconds: 86400,
			},
			DesiredState: devsandboxv1alpha1.DesiredStateRunning,
		},
	}
	template := &devsandboxv1alpha1.DevSandboxTemplate{
		TypeMeta:   metav1.TypeMeta{APIVersion: devsandboxv1alpha1.GroupVersion.String(), Kind: "DevSandboxTemplate"},
		ObjectMeta: metav1.ObjectMeta{Name: "standard"},
		Spec: devsandboxv1alpha1.DevSandboxTemplateSpec{
			DisplayName: "Standard", Version: "1.0.0",
			Image: devsandboxv1alpha1.TemplateImage{Repository: "example.azurecr.io/devsandbox-standard", Digest: digest},
		},
	}
	return sandbox, template
}

func reconcileTimes(t *testing.T, reconciler *DevSandboxReconciler, sandbox *devsandboxv1alpha1.DevSandbox, count int) {
	t.Helper()
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Name}}
	for i := 0; i < count; i++ {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
	}
}

func getSandbox(t *testing.T, kubeClient client.Client, name string) *devsandboxv1alpha1.DevSandbox {
	t.Helper()
	var sandbox devsandboxv1alpha1.DevSandbox
	mustGet(t, kubeClient, types.NamespacedName{Namespace: testNamespace, Name: name}, &sandbox)
	return &sandbox
}

func getUpstream(t *testing.T, kubeClient client.Client, sandbox *devsandboxv1alpha1.DevSandbox) *unstructured.Unstructured {
	t.Helper()
	upstream := &unstructured.Unstructured{}
	upstream.SetGroupVersionKind(SandboxGVK)
	mustGet(t, kubeClient, types.NamespacedName{Namespace: sandbox.Namespace, Name: upstreamSandboxName(sandbox)}, upstream)
	return upstream
}

func mustGet(t *testing.T, kubeClient client.Client, key types.NamespacedName, object client.Object) {
	t.Helper()
	if err := kubeClient.Get(context.Background(), key, object); err != nil {
		t.Fatalf("get %T %s: %v", object, key, err)
	}
}

func mustUpdate(t *testing.T, kubeClient client.Client, object client.Object) {
	t.Helper()
	if err := kubeClient.Update(context.Background(), object); err != nil {
		t.Fatalf("update %T: %v", object, err)
	}
}

func assertNestedString(t *testing.T, object map[string]interface{}, expected string, fields ...string) {
	t.Helper()
	actual, found, err := unstructured.NestedString(object, fields...)
	if err != nil || !found || actual != expected {
		t.Fatalf("%s: got %q, found=%v, err=%v; want %q", strings.Join(fields, "."), actual, found, err, expected)
	}

}

func assertContainerResources(t *testing.T, container map[string]interface{}, cpu, memory, ephemeral string) {
	t.Helper()
	resources := container["resources"].(map[string]interface{})
	for _, key := range []string{"requests", "limits"} {
		values := resources[key].(map[string]interface{})
		if values["cpu"] != cpu || values["memory"] != memory || values["ephemeral-storage"] != ephemeral {
			t.Fatalf("unexpected %s: %#v", key, values)
		}
	}
}

func assertProjectedBrokerIdentity(t *testing.T, volumes []interface{}) {
	t.Helper()
	for _, raw := range volumes {
		volume := raw.(map[string]interface{})
		if volume["name"] != "broker-identity" {
			continue
		}
		projected := volume["projected"].(map[string]interface{})
		sources := projected["sources"].([]interface{})
		token := sources[0].(map[string]interface{})["serviceAccountToken"].(map[string]interface{})
		if token["audience"] != BrokerAudience || token["expirationSeconds"] != int64(600) {
			t.Fatalf("incorrect projected identity: %#v", token)
		}
		return
	}
	t.Fatal("projected broker identity volume not found")
}

func assertExistingWorkspaceClaim(t *testing.T, volumes []interface{}, claim string) {
	t.Helper()
	for _, raw := range volumes {
		volume := raw.(map[string]interface{})
		if volume["name"] != "workspace" {
			continue
		}

		pvc := volume["persistentVolumeClaim"].(map[string]interface{})
		if pvc["claimName"] != claim {
			t.Fatalf("wrong existing workspace claim: %#v", pvc)
		}
		return
	}
	t.Fatal("workspace PVC volume not found")
}

func assertMemoryVolume(t *testing.T, volumes []interface{}, name, size string) {
	t.Helper()
	for _, raw := range volumes {
		volume := raw.(map[string]interface{})
		if volume["name"] != name {
			continue
		}
		emptyDir := volume["emptyDir"].(map[string]interface{})
		if emptyDir["medium"] != "Memory" || emptyDir["sizeLimit"] != size {
			t.Fatalf("unexpected memory volume sizing: %#v", emptyDir)
		}
		return
	}
	t.Fatalf("%s volume not found", name)
}

func assertQuota(t *testing.T, kubeClient client.Client, active, retained int32, storage int64) {
	t.Helper()
	var quota devsandboxv1alpha1.DevSandboxUserQuota
	mustGet(t, kubeClient, types.NamespacedName{Namespace: testNamespace, Name: quotaNameForUser("123")}, &quota)
	if quota.Status.ReservedActive != active || quota.Status.Retained != retained ||
		quota.Status.ProvisionedStorageGiB != storage {
		t.Fatalf("quota got active=%d retained=%d storage=%d; want %d/%d/%d",
			quota.Status.ReservedActive, quota.Status.Retained, quota.Status.ProvisionedStorageGiB,
			active, retained, storage)
	}
}

func timePointer(value time.Time) *metav1.Time {
	result := metav1.NewTime(value)
	return &result
}
