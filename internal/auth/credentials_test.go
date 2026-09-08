package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryCredentialStore struct {
	mu      sync.Mutex
	value   string
	version string
	readErr error
	writes  []string
}

func (s *memoryCredentialStore) Read(context.Context, string) (VersionedRefreshCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return VersionedRefreshCredential{}, s.readErr
	}
	if s.value == "" {
		return VersionedRefreshCredential{}, ErrCredentialNotFound
	}
	return VersionedRefreshCredential{Value: s.value, Version: s.version}, nil
}

func (s *memoryCredentialStore) Write(_ context.Context, _ string, value string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = value
	s.version = s.version + "n"
	s.writes = append(s.writes, value)
	return s.version, nil
}

func (s *memoryCredentialStore) replace(value, version string) {
	s.mu.Lock()
	s.value, s.version = value, version
	s.mu.Unlock()
}

type refreshFunc func(context.Context, string) (RefreshResult, error)

func (f refreshFunc) Refresh(ctx context.Context, value string) (RefreshResult, error) {
	return f(ctx, value)
}

func TestCredentialManagerSerializesConcurrentRefresh(t *testing.T) {
	store := &memoryCredentialStore{value: "refresh-1", version: "v1"}
	var active, maximum, calls int32
	refresher := refreshFunc(func(context.Context, string) (RefreshResult, error) {
		current := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maximum)
			if current <= old || atomic.CompareAndSwapInt32(&maximum, old, current) {
				break
			}
		}
		atomic.AddInt32(&calls, 1)
		time.Sleep(10 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return RefreshResult{AccessToken: "access", AccessExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	manager := &CredentialManager{Store: store, Refresher: refresher, Locker: &MemoryLocker{}}

	const workers = 12
	var group sync.WaitGroup
	group.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer group.Done()
			token, _, err := manager.AccessToken(context.Background(), "1234")
			if err != nil || token != "access" {
				t.Errorf("AccessToken() = %q, %v", token, err)
			}
		}()
	}
	group.Wait()
	if maximum != 1 {
		t.Fatalf("maximum concurrent refresh calls = %d", maximum)
	}
	if calls != 1 {
		t.Fatalf("refresh calls = %d, want one cached refresh", calls)
	}
}

func TestCredentialManagersShareLeaseAcrossAPIAndBroker(t *testing.T) {
	store := &memoryCredentialStore{value: "refresh-1", version: "v1"}
	locker := &MemoryLocker{}
	var active, maximum int32
	refresher := refreshFunc(func(context.Context, string) (RefreshResult, error) {
		current := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maximum)
			if current <= old || atomic.CompareAndSwapInt32(&maximum, old, current) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		return RefreshResult{AccessToken: "access", AccessExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	apiManager := &CredentialManager{Store: store, Refresher: refresher, Locker: locker}
	brokerManager := &CredentialManager{Store: store, Refresher: refresher, Locker: locker}
	start := make(chan struct{})
	var group sync.WaitGroup
	for _, manager := range []*CredentialManager{apiManager, brokerManager} {
		group.Add(1)
		go func(manager *CredentialManager) {
			defer group.Done()
			<-start
			if _, _, err := manager.AccessToken(context.Background(), "1234"); err != nil {
				t.Error(err)
			}
		}(manager)
	}
	close(start)
	group.Wait()
	if maximum != 1 {
		t.Fatalf("API and broker refreshed concurrently: maximum=%d", maximum)
	}
}

func TestCredentialManagerWritesRotationBeforeUnlock(t *testing.T) {
	store := &memoryCredentialStore{value: "old-refresh", version: "v1"}
	locker := &observingLocker{}
	storeWithObservation := &observingStore{memoryCredentialStore: store, locked: &locker.locked}
	manager := &CredentialManager{
		Store: storeWithObservation, Locker: locker,
		Refresher: refreshFunc(func(context.Context, string) (RefreshResult, error) {
			return RefreshResult{
				AccessToken: "access", AccessExpiresAt: time.Now().Add(time.Hour),
				RefreshToken: "rotated-refresh",
			}, nil
		}),
	}
	if _, _, err := manager.AccessToken(context.Background(), "1234"); err != nil {
		t.Fatal(err)
	}
	if !storeWithObservation.writeWhileLocked {
		t.Fatal("rotated refresh credential was not written while lease was held")
	}
}

type observingLocker struct{ locked int32 }

func (l *observingLocker) Lock(context.Context, string) (UnlockFunc, error) {
	atomic.StoreInt32(&l.locked, 1)
	return func(context.Context) error {
		atomic.StoreInt32(&l.locked, 0)
		return nil
	}, nil
}

type observingStore struct {
	*memoryCredentialStore
	locked           *int32
	writeWhileLocked bool
}

func (s *observingStore) Write(ctx context.Context, userID, value string) (string, error) {
	s.writeWhileLocked = atomic.LoadInt32(s.locked) == 1
	return s.memoryCredentialStore.Write(ctx, userID, value)
}

func TestCredentialManagerInvalidGrantRetriesOnlyNewVersion(t *testing.T) {
	store := &memoryCredentialStore{value: "stale", version: "v1"}
	var calls int
	manager := &CredentialManager{
		Store: store, Locker: &MemoryLocker{},
		Refresher: refreshFunc(func(_ context.Context, credential string) (RefreshResult, error) {
			calls++
			if credential == "stale" {
				store.replace("newer", "v2")
				return RefreshResult{}, ErrInvalidGrant
			}
			return RefreshResult{AccessToken: "access", AccessExpiresAt: time.Now().Add(time.Hour)}, nil
		}),
	}
	if token, _, err := manager.AccessToken(context.Background(), "1234"); err != nil || token != "access" {
		t.Fatalf("new version retry = %q, %v", token, err)
	}
	if calls != 2 {
		t.Fatalf("refresh calls = %d, want 2", calls)
	}

	store.replace("still-stale", "v3")
	manager.cache = nil
	calls = 0
	manager.Refresher = refreshFunc(func(context.Context, string) (RefreshResult, error) {
		calls++
		return RefreshResult{}, ErrInvalidGrant
	})
	_, _, err := manager.AccessToken(context.Background(), "1234")
	if !errors.Is(err, ErrReauthentication) || calls != 1 {
		t.Fatalf("same version invalid_grant = %v, calls %d", err, calls)
	}
}

func TestCredentialManagerPropagatesKeyVaultError(t *testing.T) {
	storeErr := errors.New("vault unavailable")
	manager := &CredentialManager{
		Store: &memoryCredentialStore{readErr: storeErr}, Locker: &MemoryLocker{},
		Refresher: refreshFunc(func(context.Context, string) (RefreshResult, error) {
			t.Fatal("refresh must not be called")
			return RefreshResult{}, nil
		}),
	}
	_, _, err := manager.AccessToken(context.Background(), "1234")
	if !errors.Is(err, storeErr) {
		t.Fatalf("AccessToken() error = %v", err)
	}
}
