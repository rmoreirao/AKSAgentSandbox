package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type VersionedRefreshCredential struct {
	Value   string
	Version string
}

// RefreshCredentialStore models version-aware Key Vault secret operations. A
// store implementation must key secrets from immutable GitHub user IDs.
type RefreshCredentialStore interface {
	Read(ctx context.Context, userID string) (VersionedRefreshCredential, error)
	Write(ctx context.Context, userID, refreshCredential string) (string, error)
}

type RefreshResult struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
}

type GitHubTokenRefresher interface {
	Refresh(ctx context.Context, refreshCredential string) (RefreshResult, error)
}

type UnlockFunc func(context.Context) error

// CredentialLocker is implemented by a Kubernetes Lease locker in production.
// API and broker instances must use the same lease namespace and name scheme.
type CredentialLocker interface {
	Lock(ctx context.Context, userID string) (UnlockFunc, error)
}

type cachedAccess struct {
	token         string
	expiresAt     time.Time
	sourceVersion string
}

// CredentialManager is the only supported path for replacing refresh
// credentials or obtaining current access tokens.
type CredentialManager struct {
	Store     RefreshCredentialStore
	Refresher GitHubTokenRefresher
	Locker    CredentialLocker
	Now       func() time.Time
	Skew      time.Duration

	cacheMu sync.Mutex
	cache   map[string]cachedAccess
}

func (m *CredentialManager) StoreRefresh(ctx context.Context, userID, refreshCredential string) error {
	if userID == "" {
		return errors.New("immutable GitHub user ID is required")
	}
	if m.Store == nil || m.Locker == nil {
		return errors.New("credential manager is not configured")
	}
	if refreshCredential == "" {
		return errors.New("refresh credential is empty")
	}
	unlock, err := m.Locker.Lock(ctx, userID)
	if err != nil {
		return fmt.Errorf("acquire credential lease: %w", err)
	}
	released := false
	defer func() {
		if !released {
			_ = releaseLease(unlock)
		}
	}()

	// Re-read after the lock even though login always replaces the credential.
	if _, err := m.Store.Read(ctx, userID); err != nil && !errors.Is(err, ErrCredentialNotFound) {
		return fmt.Errorf("read current refresh credential: %w", err)
	}
	if _, err := m.Store.Write(ctx, userID, refreshCredential); err != nil {
		return fmt.Errorf("store refresh credential: %w", err)
	}
	m.cacheMu.Lock()
	delete(m.cache, userID)
	m.cacheMu.Unlock()
	err = releaseLease(unlock)
	released = true
	if err != nil {
		return fmt.Errorf("release credential lease: %w", err)
	}
	return nil
}

func (m *CredentialManager) AccessToken(ctx context.Context, userID string) (string, time.Time, error) {
	if err := m.validate(userID); err != nil {
		return "", time.Time{}, err
	}
	unlock, err := m.Locker.Lock(ctx, userID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("acquire credential lease: %w", err)
	}
	released := false
	defer func() {
		if !released {
			_ = releaseLease(unlock)
		}
	}()

	current, err := m.Store.Read(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrCredentialNotFound) {
			return "", time.Time{}, ErrReauthentication
		}
		return "", time.Time{}, fmt.Errorf("read refresh credential: %w", err)
	}
	if current.Value == "" || current.Version == "" {
		return "", time.Time{}, errors.New("key vault returned an incomplete refresh credential")
	}

	if cached, ok := m.cached(userID, current.Version); ok {
		err = releaseLease(unlock)
		released = true
		if err != nil {
			return "", time.Time{}, fmt.Errorf("release credential lease: %w", err)
		}
		return cached.token, cached.expiresAt, nil
	}

	result, err := m.Refresher.Refresh(ctx, current.Value)
	if errors.Is(err, ErrInvalidGrant) {
		latest, readErr := m.Store.Read(ctx, userID)
		if readErr != nil {
			return "", time.Time{}, fmt.Errorf("re-read refresh credential after invalid_grant: %w", readErr)
		}
		if latest.Version == "" || latest.Version == current.Version {
			return "", time.Time{}, ErrReauthentication
		}
		current = latest
		result, err = m.Refresher.Refresh(ctx, current.Value)
		if errors.Is(err, ErrInvalidGrant) {
			return "", time.Time{}, ErrReauthentication
		}
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("refresh GitHub access token: %w", err)
	}
	if result.AccessToken == "" || !result.AccessExpiresAt.After(m.now()) {
		return "", time.Time{}, errors.New("github returned an empty or expired access token")
	}

	sourceVersion := current.Version
	if result.RefreshToken != "" && result.RefreshToken != current.Value {
		version, writeErr := m.Store.Write(ctx, userID, result.RefreshToken)
		if writeErr != nil {
			return "", time.Time{}, fmt.Errorf("store rotated refresh credential: %w", writeErr)
		}
		if version == "" {
			return "", time.Time{}, errors.New("key vault returned an empty rotated credential version")
		}
		sourceVersion = version
	}
	m.remember(userID, result.AccessToken, result.AccessExpiresAt, sourceVersion)
	err = releaseLease(unlock)
	released = true
	if err != nil {
		return "", time.Time{}, fmt.Errorf("release credential lease: %w", err)
	}
	return result.AccessToken, result.AccessExpiresAt, nil
}

func (m *CredentialManager) validate(userID string) error {
	if userID == "" {
		return errors.New("immutable GitHub user ID is required")
	}
	if m.Store == nil || m.Refresher == nil || m.Locker == nil {
		return errors.New("credential manager is not configured")
	}
	return nil
}

func (m *CredentialManager) cached(userID, version string) (cachedAccess, bool) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	value, ok := m.cache[userID]
	if !ok || value.sourceVersion != version || !value.expiresAt.After(m.now().Add(m.skew())) {
		return cachedAccess{}, false
	}
	return value, true
}

func (m *CredentialManager) remember(userID, token string, expiresAt time.Time, version string) {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	if m.cache == nil {
		m.cache = make(map[string]cachedAccess)
	}
	m.cache[userID] = cachedAccess{token: token, expiresAt: expiresAt, sourceVersion: version}
}

func (m *CredentialManager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *CredentialManager) skew() time.Duration {
	if m.Skew > 0 {
		return m.Skew
	}
	return 30 * time.Second
}

func releaseLease(unlock UnlockFunc) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return unlock(ctx)
}
