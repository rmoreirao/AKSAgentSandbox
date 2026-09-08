package lifecycle

import (
	"testing"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIdleDeadlineCalculation(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	baseline := now.Add(-5 * time.Minute)
	result := EvaluateActivity(now, baseline, 10*time.Minute, nil, nil)
	if !result.IdleDeadline.Equal(now.Add(5*time.Minute)) || result.ShouldIdleStop {
		t.Fatalf("unexpected activity result: %#v", result)
	}
}

func TestRunningManagedJobSuppressesIdleStop(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	result := EvaluateActivity(now, now.Add(-time.Hour), time.Minute, nil, []devsandboxv1alpha1.DevSandboxJob{
		{Status: devsandboxv1alpha1.DevSandboxJobStatus{State: devsandboxv1alpha1.JobStateRunning}},
	})
	if result.ShouldIdleStop || !result.HasRunningJob {
		t.Fatalf("running job did not suppress idle stop: %#v", result)
	}
}

func TestStaleConnectionDoesNotSuppressIdleStop(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	renewed := metav1.NewMicroTime(now.Add(-ConnectionStaleAfter - time.Second))
	lease := &coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{RenewTime: &renewed}}
	result := EvaluateActivity(now, now.Add(-time.Hour), time.Minute, lease, nil)
	if !result.ShouldIdleStop || result.HasLiveLease {
		t.Fatalf("stale lease incorrectly suppressed idle stop: %#v", result)
	}
}

func TestCompletedJobRestartsIdleClock(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	completed := metav1.NewTime(now.Add(-2 * time.Minute))
	result := EvaluateActivity(now, now.Add(-time.Hour), 5*time.Minute, nil, []devsandboxv1alpha1.DevSandboxJob{
		{Status: devsandboxv1alpha1.DevSandboxJobStatus{
			State: devsandboxv1alpha1.JobStateCompleted, CompletedAt: &completed,
		}},
	})
	if result.ShouldIdleStop || !result.LastActivity.Equal(completed.Time) {
		t.Fatalf("completion did not restart idle clock: %#v", result)
	}
}
