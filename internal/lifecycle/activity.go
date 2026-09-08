package lifecycle

import (
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const ConnectionStaleAfter = 90 * time.Second

// ActivityResult is the coalesced lifecycle view used by the operator.
type ActivityResult struct {
	LastActivity   time.Time
	IdleDeadline   time.Time
	HasLiveLease   bool
	HasRunningJob  bool
	NextCheck      time.Time
	ShouldIdleStop bool
}

// EvaluateActivity combines lease heartbeats and restart-safe managed-job
// summaries. An active managed job suppresses idle stopping; a completed job
// restarts the idle clock at its completion time.
func EvaluateActivity(
	now, baseline time.Time,
	idleTimeout time.Duration,
	lease *coordinationv1.Lease,
	jobs []devsandboxv1alpha1.DevSandboxJob,
) ActivityResult {
	last := baseline.UTC()
	liveLease := false
	if lease != nil && lease.Spec.RenewTime != nil {
		renewed := lease.Spec.RenewTime.Time.UTC()
		if renewed.After(last) {
			last = renewed
		}
		liveLease = now.Sub(renewed) <= ConnectionStaleAfter
	}

	running := false
	for i := range jobs {
		switch jobs[i].Status.State {
		case devsandboxv1alpha1.JobStatePending, devsandboxv1alpha1.JobStateRunning:
			running = true
		case devsandboxv1alpha1.JobStateCompleted, devsandboxv1alpha1.JobStateFailed, devsandboxv1alpha1.JobStateStopped:
			if jobs[i].Status.CompletedAt != nil && jobs[i].Status.CompletedAt.Time.After(last) {
				last = jobs[i].Status.CompletedAt.Time.UTC()
			}
		}
	}

	deadline := last.Add(idleTimeout)
	next := deadline
	if liveLease {
		leaseStale := lease.Spec.RenewTime.Add(ConnectionStaleAfter)
		if leaseStale.Before(next) {
			next = leaseStale
		}
	}
	if running {
		next = now.Add(ConnectionStaleAfter)
	}
	return ActivityResult{
		LastActivity: last, IdleDeadline: deadline, HasLiveLease: liveLease,
		HasRunningJob: running, NextCheck: next,
		ShouldIdleStop: !running && !liveLease && !now.Before(deadline),
	}
}

func TimePointer(value time.Time) *metav1.Time {
	result := metav1.NewTime(value.UTC())
	return &result
}
