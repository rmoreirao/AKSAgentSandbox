package operator

import (
	"context"
	"testing"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestQuotaReconcilerReleasesStaleReservation(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	quota := &devsandboxv1alpha1.DevSandboxUserQuota{
		ObjectMeta: metav1.ObjectMeta{Name: quotaNameForUser("123"), Namespace: testNamespace},
		Spec: devsandboxv1alpha1.DevSandboxUserQuotaSpec{
			GitHubUserID: "123", Limits: devsandboxv1alpha1.QuotaLimits{Active: 3, Retained: 10, StorageGiB: 200},
		},
		Status: devsandboxv1alpha1.DevSandboxUserQuotaStatus{
			ReservedActive: 1, ProvisionedStorageGiB: 20,
			Reservations: []devsandboxv1alpha1.QuotaReservation{{
				ID: "deleted-uid", SandboxName: "deleted", Active: 1, StorageGiB: 20,
				CreatedAt: metav1.NewTime(now.Add(-time.Hour)),
			}},
		},
	}
	base := newTestReconciler(t, now, quota)
	reconciler := &QuotaReconciler{Client: base.Client, Now: func() time.Time { return now }}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: quota.Name},
	}); err != nil {
		t.Fatal(err)
	}
	assertQuota(t, base.Client, 0, 0, 0)
}

func TestJobReconcilerStopsManagedJobWithSandbox(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	sandbox, _ := testObjects(now)
	sandbox.Spec.DesiredState = devsandboxv1alpha1.DesiredStateStopped
	job := &devsandboxv1alpha1.DevSandboxJob{
		ObjectMeta: metav1.ObjectMeta{Name: "job-1", Namespace: testNamespace},
		Spec:       devsandboxv1alpha1.DevSandboxJobSpec{SandboxName: sandbox.Name, GitHubUserID: "123"},
		Status:     devsandboxv1alpha1.DevSandboxJobStatus{State: devsandboxv1alpha1.JobStateRunning},
	}
	base := newTestReconciler(t, now, sandbox, job)
	reconciler := &JobReconciler{Client: base.Client, Now: func() time.Time { return now }}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: job.Name},
	}); err != nil {
		t.Fatal(err)
	}
	var current devsandboxv1alpha1.DevSandboxJob
	mustGet(t, base.Client, types.NamespacedName{Namespace: testNamespace, Name: job.Name}, &current)
	if current.Status.State != devsandboxv1alpha1.JobStateStopped || current.Status.CompletedAt == nil {
		t.Fatalf("managed job was not stopped: %#v", current.Status)
	}
}
