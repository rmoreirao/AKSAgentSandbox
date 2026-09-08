package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type MemoryLocker struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

func (l *MemoryLocker) Lock(ctx context.Context, userID string) (UnlockFunc, error) {
	if userID == "" {
		return nil, errors.New("user ID is required")
	}
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]chan struct{})
	}
	gate, ok := l.locks[userID]
	if !ok {
		gate = make(chan struct{}, 1)
		gate <- struct{}{}
		l.locks[userID] = gate
	}
	l.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-gate:
	}
	var once sync.Once
	return func(context.Context) error {
		once.Do(func() { gate <- struct{}{} })
		return nil
	}, nil
}

type KubernetesLeaseLocker struct {
	Client        client.Client
	Namespace     string
	HolderID      string
	LeaseDuration time.Duration
	PollInterval  time.Duration
	Now           func() time.Time
	counter       uint64
}

func (l *KubernetesLeaseLocker) Lock(ctx context.Context, userID string) (UnlockFunc, error) {
	if l.Client == nil || l.Namespace == "" || l.HolderID == "" || userID == "" {
		return nil, errors.New("kubernetes credential lease locker is not configured")
	}
	name := credentialLeaseName(userID)
	holder := fmt.Sprintf("%s-%d", l.HolderID, atomic.AddUint64(&l.counter, 1))
	poll := l.PollInterval
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	for {
		acquired, err := l.tryAcquire(ctx, name, holder)
		if err != nil {
			return nil, err
		}
		if acquired {
			var once sync.Once
			var releaseErr error
			return func(releaseCtx context.Context) error {
				once.Do(func() { releaseErr = l.release(releaseCtx, name, holder) })
				return releaseErr
			}, nil
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *KubernetesLeaseLocker) tryAcquire(ctx context.Context, name, holder string) (bool, error) {
	key := types.NamespacedName{Namespace: l.Namespace, Name: name}
	var lease coordinationv1.Lease
	err := l.Client.Get(ctx, key, &lease)
	now := metav1.NewMicroTime(l.now())
	seconds := int32(l.duration() / time.Second)
	if apierrors.IsNotFound(err) {
		lease = coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: l.Namespace,
				Labels: map[string]string{"devsandbox.io/credential-refresh-lock": "true"},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       stringPointer(holder),
				LeaseDurationSeconds: int32Pointer(seconds),
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		}
		err = l.Client.Create(ctx, &lease)
		if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
			return false, nil
		}
		return err == nil, err
	}
	if err != nil {
		return false, fmt.Errorf("read credential lease: %w", err)
	}

	expired := l.expired(lease, now.Time)
	if lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" &&
		*lease.Spec.HolderIdentity != holder && !expired {
		return false, nil
	}
	lease.Spec.HolderIdentity = stringPointer(holder)
	lease.Spec.LeaseDurationSeconds = int32Pointer(seconds)
	lease.Spec.RenewTime = &now
	if lease.Spec.AcquireTime == nil || expired {
		lease.Spec.AcquireTime = &now
	}
	if err := l.Client.Update(ctx, &lease); err != nil {
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return false, fmt.Errorf("acquire credential lease: %w", err)
	}
	return true, nil
}

func (l *KubernetesLeaseLocker) release(ctx context.Context, name, holder string) error {
	var lease coordinationv1.Lease
	key := types.NamespacedName{Namespace: l.Namespace, Name: name}
	if err := l.Client.Get(ctx, key, &lease); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read credential lease for release: %w", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holder {
		return nil
	}
	lease.Spec.HolderIdentity = stringPointer("")
	now := metav1.NewMicroTime(l.now())
	lease.Spec.RenewTime = &now
	if err := l.Client.Update(ctx, &lease); err != nil {
		return fmt.Errorf("release credential lease: %w", err)
	}
	return nil
}

func (l *KubernetesLeaseLocker) expired(lease coordinationv1.Lease, now time.Time) bool {
	renewed := lease.Spec.RenewTime
	if renewed == nil {
		renewed = lease.Spec.AcquireTime
	}
	if renewed == nil {
		return true
	}
	duration := l.duration()
	if lease.Spec.LeaseDurationSeconds != nil && *lease.Spec.LeaseDurationSeconds > 0 {
		duration = time.Duration(*lease.Spec.LeaseDurationSeconds) * time.Second
	}
	return !renewed.Add(duration).After(now)
}

func (l *KubernetesLeaseLocker) duration() time.Duration {
	if l.LeaseDuration > 0 {
		return l.LeaseDuration
	}
	return 30 * time.Second
}

func (l *KubernetesLeaseLocker) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

func credentialLeaseName(userID string) string {
	digest := sha256.Sum256([]byte(userID))
	return "github-refresh-" + hex.EncodeToString(digest[:16])
}

func stringPointer(value string) *string { return &value }
func int32Pointer(value int32) *int32    { return &value }
