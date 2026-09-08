package lifecycle

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type memoryQuotaStore struct {
	mu    sync.Mutex
	quota *devsandboxv1alpha1.DevSandboxUserQuota
}

func (s *memoryQuotaStore) Get(context.Context, string, string) (*devsandboxv1alpha1.DevSandboxUserQuota, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quota.DeepCopy(), nil
}

func (s *memoryQuotaStore) UpdateStatus(_ context.Context, candidate *devsandboxv1alpha1.DevSandboxUserQuota) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if candidate.ResourceVersion != s.quota.ResourceVersion {
		return apierrors.NewConflict(
			schema.GroupResource{Group: devsandboxv1alpha1.GroupName, Resource: "devsandboxuserquotas"},
			candidate.Name,
			fmt.Errorf("resource version changed"),
		)
	}
	version, _ := strconv.Atoi(s.quota.ResourceVersion)
	candidate.ResourceVersion = strconv.Itoa(version + 1)
	s.quota = candidate.DeepCopy()
	return nil
}

func TestConcurrentReservationsCannotExceedQuota(t *testing.T) {
	store := &memoryQuotaStore{quota: &devsandboxv1alpha1.DevSandboxUserQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "user-123", ResourceVersion: "1"},
		Spec: devsandboxv1alpha1.DevSandboxUserQuotaSpec{
			GitHubUserID: "123",
			Limits:       devsandboxv1alpha1.QuotaLimits{Active: 3, Retained: 10, StorageGiB: 200},
		},
	}}
	manager := QuotaManager{Store: store, MaxAttempts: 64}
	const workers = 24
	var wg sync.WaitGroup
	results := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- manager.Reserve(context.Background(), "", "user-123", ReservationRequest{
				ID:          fmt.Sprintf("reservation-%d", i),
				SandboxName: fmt.Sprintf("sandbox-%d", i),
				Active:      1,
				StorageGiB:  20,
			})
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 3 {
		t.Fatalf("expected exactly 3 successful reservations, got %d", successes)
	}
	current, _ := store.Get(context.Background(), "", "")
	if current.Status.ReservedActive != 3 || len(current.Status.Reservations) != 3 {
		t.Fatalf("quota exceeded: %#v", current.Status)
	}
}

func TestReservationIsIdempotentAndReleasable(t *testing.T) {
	store := &memoryQuotaStore{quota: &devsandboxv1alpha1.DevSandboxUserQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "user-123", ResourceVersion: "1"},
		Spec: devsandboxv1alpha1.DevSandboxUserQuotaSpec{
			Limits: devsandboxv1alpha1.QuotaLimits{Active: 3, Retained: 10, StorageGiB: 200},
		},
	}}
	manager := QuotaManager{Store: store}
	request := ReservationRequest{ID: "same", SandboxName: "sandbox", Active: 1, StorageGiB: 20}
	if err := manager.Reserve(context.Background(), "", "user-123", request); err != nil {
		t.Fatal(err)
	}

	if err := manager.Reserve(context.Background(), "", "user-123", request); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(context.Background(), "", "user-123", request.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(context.Background(), "", "")
	if current.Status.ReservedActive != 0 || current.Status.ProvisionedStorageGiB != 0 ||
		len(current.Status.Reservations) != 0 {
		t.Fatalf("reservation was not released: %#v", current.Status)
	}
}

func TestReservationTransitionsUseCompareAndSwap(t *testing.T) {
	store := &memoryQuotaStore{quota: &devsandboxv1alpha1.DevSandboxUserQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "user-123", ResourceVersion: "1"},
		Spec: devsandboxv1alpha1.DevSandboxUserQuotaSpec{
			Limits: devsandboxv1alpha1.QuotaLimits{Active: 3, Retained: 10, StorageGiB: 200},
		},
	}}
	manager := QuotaManager{Store: store}
	active := ReservationRequest{ID: "sandbox-uid", SandboxName: "sandbox", Active: 1, StorageGiB: 20}
	if err := manager.ReconcileReservation(context.Background(), "", "user-123", active); err != nil {
		t.Fatal(err)
	}
	stopped := ReservationRequest{ID: "sandbox-uid", SandboxName: "sandbox", Retained: 1, StorageGiB: 20}
	if err := manager.ReconcileReservation(context.Background(), "", "user-123", stopped); err != nil {
		t.Fatal(err)
	}
	current, _ := store.Get(context.Background(), "", "")
	if current.Status.ReservedActive != 0 || current.Status.Retained != 1 ||
		current.Status.ProvisionedStorageGiB != 20 || len(current.Status.Reservations) != 1 {
		t.Fatalf("reservation transition was not atomic: %#v", current.Status)
	}
}
