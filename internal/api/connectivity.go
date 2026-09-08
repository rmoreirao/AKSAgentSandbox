package api

import (
	"context"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
)

const activityRenewInterval = 30 * time.Second

type ActivityRenewer interface {
	RenewActivity(context.Context, *devsandboxv1alpha1.DevSandbox, string, time.Time) error
}

func (h Handler) renewActivity(ctx context.Context, sandbox *devsandboxv1alpha1.DevSandbox, holder string) {
	if h.Activity == nil {
		return
	}
	_ = h.Activity.RenewActivity(ctx, sandbox, holder, time.Now().UTC())
}

func (h Handler) startActivity(ctx context.Context, sandbox *devsandboxv1alpha1.DevSandbox, holder string) func() {
	if h.Activity == nil {
		return func() {}
	}
	done := make(chan struct{})
	h.renewActivity(ctx, sandbox, holder)
	go func() {
		ticker := time.NewTicker(activityRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				h.renewActivity(ctx, sandbox, holder)
			}
		}
	}()
	return func() { close(done) }
}
