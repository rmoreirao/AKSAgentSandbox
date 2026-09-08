package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"sync"
	"time"
)

type DeviceAuthorization struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresIn       time.Duration
	Interval        time.Duration
}

type DeviceToken struct {
	AccessToken  string
	RefreshToken string
}

type GitHubUser struct {
	ID    string
	Login string
}

type DeviceFlowClient interface {
	StartDeviceFlow(ctx context.Context) (DeviceAuthorization, error)
	PollDeviceFlow(ctx context.Context, deviceCode string) (DeviceToken, error)
	CurrentUser(ctx context.Context, accessToken string) (GitHubUser, error)
	IsActiveOrgMember(ctx context.Context, accessToken, org string) (bool, error)
}

type DeviceState struct {
	DeviceCode string
	ExpiresAt  time.Time
	NextPollAt time.Time
	PollEvery  time.Duration
}

type DeviceStateStore interface {
	Put(ctx context.Context, state string, value DeviceState) error
	Get(ctx context.Context, state string) (DeviceState, error)
	Update(ctx context.Context, state string, value DeviceState) error
	Delete(ctx context.Context, state string) error
}

type MemoryDeviceStateStore struct {
	mu     sync.Mutex
	states map[string]DeviceState
	Now    func() time.Time
}

func (s *MemoryDeviceStateStore) Put(_ context.Context, state string, value DeviceState) error {
	if state == "" || value.DeviceCode == "" {
		return errors.New("device state is incomplete")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.states == nil {
		s.states = make(map[string]DeviceState)
	}
	s.states[state] = value
	return nil
}

func (s *MemoryDeviceStateStore) Get(_ context.Context, state string) (DeviceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.states[state]
	if !ok || !value.ExpiresAt.After(s.now()) {
		delete(s.states, state)
		return DeviceState{}, ErrInvalidState
	}
	return value, nil
}

func (s *MemoryDeviceStateStore) Update(_ context.Context, state string, value DeviceState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.states[state]; !ok {
		return ErrInvalidState
	}
	s.states[state] = value
	return nil
}

func (s *MemoryDeviceStateStore) Delete(_ context.Context, state string) error {
	s.mu.Lock()
	delete(s.states, state)
	s.mu.Unlock()
	return nil
}

func (s *MemoryDeviceStateStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

type DeviceStart struct {
	State           string        `json:"state"`
	UserCode        string        `json:"userCode"`
	VerificationURI string        `json:"verificationUri"`
	ExpiresIn       time.Duration `json:"-"`
	Interval        time.Duration `json:"-"`
	ExpiresSeconds  int64         `json:"expiresIn"`
	IntervalSeconds int64         `json:"interval"`
}

type LoginResult struct {
	SessionToken string    `json:"sessionToken"`
	ExpiresAt    time.Time `json:"expiresAt"`
	Identity     Identity  `json:"identity"`
}

type RefreshCredentialWriter interface {
	StoreRefresh(ctx context.Context, userID, refreshCredential string) error
}

type LoginService struct {
	GitHub      DeviceFlowClient
	States      DeviceStateStore
	Credentials RefreshCredentialWriter
	Sessions    SessionManager
	PrimaryOrg  string
	Now         func() time.Time
	Random      func([]byte) (int, error)
}

func (s *LoginService) Start(ctx context.Context) (DeviceStart, error) {
	if err := s.validate(); err != nil {
		return DeviceStart{}, err
	}
	authorization, err := s.GitHub.StartDeviceFlow(ctx)
	if err != nil {
		return DeviceStart{}, err
	}
	if authorization.DeviceCode == "" || authorization.UserCode == "" ||
		authorization.VerificationURI == "" || authorization.ExpiresIn <= 0 {
		return DeviceStart{}, errors.New("github returned an incomplete device authorization")
	}
	state, err := s.randomState()
	if err != nil {
		return DeviceStart{}, err
	}
	now := s.now()
	interval := authorization.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if err := s.States.Put(ctx, state, DeviceState{
		DeviceCode: authorization.DeviceCode, ExpiresAt: now.Add(authorization.ExpiresIn),
		NextPollAt: now, PollEvery: interval,
	}); err != nil {
		return DeviceStart{}, err
	}
	return DeviceStart{
		State: state, UserCode: authorization.UserCode, VerificationURI: authorization.VerificationURI,
		ExpiresIn: authorization.ExpiresIn, Interval: interval,
		ExpiresSeconds:  int64(authorization.ExpiresIn / time.Second),
		IntervalSeconds: int64(interval / time.Second),
	}, nil
}

func (s *LoginService) Poll(ctx context.Context, state string) (LoginResult, error) {
	if err := s.validate(); err != nil {
		return LoginResult{}, err
	}
	value, err := s.States.Get(ctx, state)
	if err != nil {
		return LoginResult{}, err
	}
	now := s.now()
	if value.NextPollAt.After(now) {
		return LoginResult{}, ErrSlowDown
	}
	value.NextPollAt = now.Add(value.PollEvery)
	if err := s.States.Update(ctx, state, value); err != nil {
		return LoginResult{}, err
	}
	token, err := s.GitHub.PollDeviceFlow(ctx, value.DeviceCode)
	if errors.Is(err, ErrSlowDown) {
		value.PollEvery += 5 * time.Second
		value.NextPollAt = now.Add(value.PollEvery)
		_ = s.States.Update(ctx, state, value)
		return LoginResult{}, err
	}
	if errors.Is(err, ErrAuthorizationPending) {
		return LoginResult{}, err
	}
	if err != nil {
		_ = s.States.Delete(ctx, state)
		return LoginResult{}, err
	}
	_ = s.States.Delete(ctx, state)
	if token.AccessToken == "" || token.RefreshToken == "" {
		return LoginResult{}, errors.New("github did not return expiring user credentials")
	}
	user, err := s.GitHub.CurrentUser(ctx, token.AccessToken)
	if err != nil {
		return LoginResult{}, err
	}
	if user.ID == "" {
		return LoginResult{}, errors.New("github returned no immutable user ID")
	}
	identityBoundary := s.PrimaryOrg
	if identityBoundary != "" {
		active, err := s.GitHub.IsActiveOrgMember(ctx, token.AccessToken, identityBoundary)
		if err != nil {
			return LoginResult{}, err
		}
		if !active {
			return LoginResult{}, ErrMembershipRequired
		}
	} else {
		identityBoundary = user.Login
	}
	if err := s.Credentials.StoreRefresh(ctx, user.ID, token.RefreshToken); err != nil {
		return LoginResult{}, err
	}
	identity := Identity{UserID: user.ID, Login: user.Login, Org: identityBoundary}
	session, expiresAt, err := s.Sessions.Issue(ctx, identity)
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{SessionToken: session, ExpiresAt: expiresAt, Identity: identity}, nil
}

func (s *LoginService) validate() error {
	if s.GitHub == nil || s.States == nil || s.Credentials == nil {
		return errors.New("login service is not configured")
	}
	return nil
}

func (s *LoginService) randomState() (string, error) {
	var raw [32]byte
	random := s.Random
	if random == nil {
		random = rand.Read
	}
	if n, err := random(raw[:]); err != nil {
		return "", err
	} else if n != len(raw) {
		return "", io.ErrUnexpectedEOF
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func (s *LoginService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
