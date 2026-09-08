package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var ErrQuotaExceeded = errors.New("user quota exceeded")

type QuotaStore interface {
	Get(ctx context.Context, namespace, name string) (*devsandboxv1alpha1.DevSandboxUserQuota, error)
	UpdateStatus(ctx context.Context, quota *devsandboxv1alpha1.DevSandboxUserQuota) error
}

type ReservationRequest struct {
	ID          string
	SandboxName string
	Active      int32
	Retained    int32
	StorageGiB  int64
}

type QuotaManager struct {
	Store       QuotaStore
	MaxAttempts int
	Now         func() time.Time
}

func (m QuotaManager) Reserve(ctx context.Context, namespace, name string, request ReservationRequest) error {
	if err := validateReservation(request); err != nil {
		return err
	}
	return m.retry(ctx, namespace, name, func(quota *devsandboxv1alpha1.DevSandboxUserQuota) (bool, error) {
		for _, reservation := range quota.Status.Reservations {
			if reservation.ID == request.ID {
				return false, nil
			}
		}
		nextActive := quota.Status.ReservedActive + request.Active
		nextRetained := quota.Status.Retained + request.Retained
		nextStorage := quota.Status.ProvisionedStorageGiB + request.StorageGiB
		if nextActive > quota.Spec.Limits.Active ||
			nextRetained > quota.Spec.Limits.Retained ||
			nextStorage > quota.Spec.Limits.StorageGiB {
			return false, fmt.Errorf("%w: active %d/%d, retained %d/%d, storage %d/%d GiB",
				ErrQuotaExceeded,
				nextActive, quota.Spec.Limits.Active,
				nextRetained, quota.Spec.Limits.Retained,
				nextStorage, quota.Spec.Limits.StorageGiB)
		}
		now := time.Now
		if m.Now != nil {
			now = m.Now
		}
		quota.Status.ReservedActive = nextActive
		quota.Status.Retained = nextRetained
		quota.Status.ProvisionedStorageGiB = nextStorage
		quota.Status.Reservations = append(quota.Status.Reservations, devsandboxv1alpha1.QuotaReservation{
			ID:          request.ID,
			SandboxName: request.SandboxName,
			Active:      request.Active,
			Retained:    request.Retained,
			StorageGiB:  request.StorageGiB,
			CreatedAt:   metav1.NewTime(now().UTC()),
		})
		return true, nil
	})
}

// ReconcileReservation atomically moves a reservation between active and retained
// states. Passing zero values for every dimension releases the reservation.
func (m QuotaManager) ReconcileReservation(ctx context.Context, namespace, name string, request ReservationRequest) error {
	if request.ID == "" || request.SandboxName == "" {
		return errors.New("reservation ID and sandbox name are required")
	}
	if request.Active < 0 || request.Retained < 0 || request.StorageGiB < 0 {
		return errors.New("reservation values cannot be negative")
	}
	return m.retry(ctx, namespace, name, func(quota *devsandboxv1alpha1.DevSandboxUserQuota) (bool, error) {
		var previous devsandboxv1alpha1.QuotaReservation
		index := -1
		for i := range quota.Status.Reservations {
			if quota.Status.Reservations[i].ID == request.ID {
				previous = quota.Status.Reservations[i]
				index = i
				break
			}
		}
		if previous.Active == request.Active && previous.Retained == request.Retained &&
			previous.StorageGiB == request.StorageGiB &&
			(index < 0 || previous.SandboxName == request.SandboxName) {
			return false, nil
		}

		nextActive := quota.Status.ReservedActive - previous.Active + request.Active
		nextRetained := quota.Status.Retained - previous.Retained + request.Retained
		nextStorage := quota.Status.ProvisionedStorageGiB - previous.StorageGiB + request.StorageGiB
		if nextActive > quota.Spec.Limits.Active ||
			nextRetained > quota.Spec.Limits.Retained ||
			nextStorage > quota.Spec.Limits.StorageGiB {
			return false, fmt.Errorf("%w: active %d/%d, retained %d/%d, storage %d/%d GiB",
				ErrQuotaExceeded,
				nextActive, quota.Spec.Limits.Active,
				nextRetained, quota.Spec.Limits.Retained,
				nextStorage, quota.Spec.Limits.StorageGiB)
		}
		quota.Status.ReservedActive = max32(0, nextActive)
		quota.Status.Retained = max32(0, nextRetained)
		quota.Status.ProvisionedStorageGiB = max64(0, nextStorage)

		if request.Active == 0 && request.Retained == 0 && request.StorageGiB == 0 {
			if index >= 0 {
				quota.Status.Reservations = append(quota.Status.Reservations[:index], quota.Status.Reservations[index+1:]...)
			}
			return index >= 0, nil
		}
		now := time.Now
		if m.Now != nil {
			now = m.Now
		}
		next := devsandboxv1alpha1.QuotaReservation{
			ID: request.ID, SandboxName: request.SandboxName, Active: request.Active,
			Retained: request.Retained, StorageGiB: request.StorageGiB,
			CreatedAt: metav1.NewTime(now().UTC()),
		}
		if index >= 0 {
			next.CreatedAt = previous.CreatedAt
			quota.Status.Reservations[index] = next
		} else {
			quota.Status.Reservations = append(quota.Status.Reservations, next)
		}
		return true, nil
	})
}

func (m QuotaManager) Release(ctx context.Context, namespace, name, reservationID string) error {
	if reservationID == "" {
		return errors.New("reservation ID is required")
	}
	return m.retry(ctx, namespace, name, func(quota *devsandboxv1alpha1.DevSandboxUserQuota) (bool, error) {
		for i, reservation := range quota.Status.Reservations {
			if reservation.ID != reservationID {
				continue
			}
			quota.Status.ReservedActive = max32(0, quota.Status.ReservedActive-reservation.Active)
			quota.Status.Retained = max32(0, quota.Status.Retained-reservation.Retained)
			quota.Status.ProvisionedStorageGiB = max64(0, quota.Status.ProvisionedStorageGiB-reservation.StorageGiB)
			quota.Status.Reservations = append(quota.Status.Reservations[:i], quota.Status.Reservations[i+1:]...)
			return true, nil
		}
		return false, nil
	})
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (m QuotaManager) retry(
	ctx context.Context,
	namespace, name string,
	mutate func(*devsandboxv1alpha1.DevSandboxUserQuota) (bool, error),
) error {
	if m.Store == nil {
		return errors.New("quota store is required")
	}
	attempts := m.MaxAttempts
	if attempts <= 0 {
		attempts = 8
	}
	var lastRetryable error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := m.Store.Get(ctx, namespace, name)
		if err != nil {
			if apierrors.IsNotFound(err) {
				lastRetryable = err
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(100 * time.Millisecond):
					continue
				}
			}
			return err
		}
		candidate := current.DeepCopy()
		changed, err := mutate(candidate)
		if err != nil || !changed {
			return err
		}
		err = m.Store.UpdateStatus(ctx, candidate)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
		lastRetryable = err
	}
	return fmt.Errorf("quota update did not converge after %d attempts: %w", attempts, lastRetryable)
}

func validateReservation(request ReservationRequest) error {
	if request.ID == "" || request.SandboxName == "" {
		return errors.New("reservation ID and sandbox name are required")
	}
	if request.Active < 0 || request.Retained < 0 || request.StorageGiB < 0 {
		return errors.New("reservation values cannot be negative")
	}
	if request.Active == 0 && request.Retained == 0 && request.StorageGiB == 0 {
		return errors.New("reservation must reserve at least one quota dimension")
	}
	return nil
}
