package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeDeviceClient struct {
	member           bool
	pollErr          error
	identity         GitHubUser
	polledCode       string
	accessToken      string
	refreshToken     string
	membershipChecks int
}

func (f *fakeDeviceClient) StartDeviceFlow(context.Context) (DeviceAuthorization, error) {
	return DeviceAuthorization{
		DeviceCode: "internal-device-code", UserCode: "ABCD-EFGH",
		VerificationURI: "https://github.com/login/device",
		ExpiresIn:       10 * time.Minute, Interval: time.Second,
	}, nil
}

func (f *fakeDeviceClient) PollDeviceFlow(_ context.Context, code string) (DeviceToken, error) {
	f.polledCode = code
	if f.pollErr != nil {
		return DeviceToken{}, f.pollErr
	}
	return DeviceToken{AccessToken: f.accessToken, RefreshToken: f.refreshToken}, nil
}

func (f *fakeDeviceClient) CurrentUser(context.Context, string) (GitHubUser, error) {
	return f.identity, nil
}

func (f *fakeDeviceClient) IsActiveOrgMember(context.Context, string, string) (bool, error) {
	f.membershipChecks++
	return f.member, nil
}

type recordingCredentialWriter struct {
	userID string
	value  string
}

func (w *recordingCredentialWriter) StoreRefresh(_ context.Context, userID, value string) error {
	w.userID, w.value = userID, value
	return nil
}

func TestDeviceFlowStateAndImmutableIdentity(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	github := &fakeDeviceClient{
		member: true, identity: GitHubUser{ID: "987654", Login: "mutable-login"},
		accessToken: "access", refreshToken: "refresh",
	}
	writer := &recordingCredentialWriter{}
	states := &MemoryDeviceStateStore{Now: func() time.Time { return now }}
	service := &LoginService{
		GitHub: github, States: states, Credentials: writer, PrimaryOrg: "primary",
		Now: func() time.Time { return now },
		Random: func(value []byte) (int, error) {
			for i := range value {
				value[i] = byte(i + 1)
			}
			return len(value), nil
		},
		Sessions: SessionManager{
			Signer:   HMACSigner{Key: []byte("01234567890123456789012345678901")},
			Audience: "devsandbox-api", Lifetime: 15 * time.Minute,
			Now: func() time.Time { return now },
		},
	}
	start, err := service.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if start.State == "" || start.State == "internal-device-code" {
		t.Fatalf("unsafe state %q", start.State)
	}
	if _, err := service.Poll(context.Background(), "attacker-state"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid state error = %v", err)
	}
	result, err := service.Poll(context.Background(), start.State)
	if err != nil {
		t.Fatal(err)
	}
	if github.polledCode != "internal-device-code" {
		t.Fatalf("polled code = %q", github.polledCode)
	}
	if writer.userID != "987654" || result.Identity.UserID != "987654" {
		t.Fatalf("credential/session did not use immutable ID: writer=%q identity=%q", writer.userID, result.Identity.UserID)
	}
	if _, err := service.Poll(context.Background(), start.State); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("successful state was reusable: %v", err)
	}
}

func TestDeviceFlowRejectsInactiveMembership(t *testing.T) {
	now := time.Now()
	github := &fakeDeviceClient{
		member: false, identity: GitHubUser{ID: "1", Login: "user"},
		accessToken: "access", refreshToken: "refresh",
	}

	service := &LoginService{
		GitHub: github, States: &MemoryDeviceStateStore{}, Credentials: &recordingCredentialWriter{},
		PrimaryOrg: "primary", Now: func() time.Time { return now },
		Sessions: SessionManager{Signer: HMACSigner{Key: make([]byte, 32)}, Audience: "api"},
	}
	start, err := service.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Poll(context.Background(), start.State); !errors.Is(err, ErrMembershipRequired) {
		t.Fatalf("membership error = %v", err)
	}
}

func TestDeviceFlowPersonalModeUsesLoginBoundary(t *testing.T) {
	now := time.Now()
	github := &fakeDeviceClient{
		identity:    GitHubUser{ID: "1", Login: "personal-owner"},
		accessToken: "access", refreshToken: "refresh",
	}
	service := &LoginService{
		GitHub: github, States: &MemoryDeviceStateStore{}, Credentials: &recordingCredentialWriter{},
		Now:      func() time.Time { return now },
		Sessions: SessionManager{Signer: HMACSigner{Key: make([]byte, 32)}, Audience: "api"},
	}
	start, err := service.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Poll(context.Background(), start.State)
	if err != nil {
		t.Fatal(err)
	}
	if result.Identity.Org != "personal-owner" || github.membershipChecks != 0 {
		t.Fatalf("personal identity = %#v, membership checks = %d", result.Identity, github.membershipChecks)
	}
}

func TestSessionExpirationAndAudience(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	manager := SessionManager{
		Signer:   HMACSigner{Key: []byte("01234567890123456789012345678901")},
		Audience: "devsandbox-api", Lifetime: time.Minute, Now: func() time.Time { return now },
	}

	token, _, err := manager.Issue(context.Background(), Identity{UserID: "42", Login: "user", Org: "primary"})
	if err != nil {
		t.Fatal(err)
	}
	if claims, err := manager.Verify(context.Background(), token); err != nil || claims.Subject != "42" {
		t.Fatalf("Verify() = %#v, %v", claims, err)
	}
	manager.Now = func() time.Time { return now.Add(time.Minute) }
	if _, err := manager.Verify(context.Background(), token); !errors.Is(err, ErrExpiredSession) {
		t.Fatalf("expiration error = %v", err)
	}
	manager.Audience = "different"
	if _, err := manager.Verify(context.Background(), token); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("audience error = %v", err)
	}
}

func TestOwnerAuthorizationUsesImmutableID(t *testing.T) {
	claims := SessionClaims{Subject: "42", Login: "same-login"}
	if err := AuthorizeOwner(claims, "42"); err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeOwner(claims, "99"); !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("owner mismatch error = %v", err)
	}
}
