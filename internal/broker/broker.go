package broker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
)

const (
	DefaultAudience  = "devsandbox-credential-broker"
	SandboxNameLabel = "devsandbox.io/sandbox"
	SandboxUIDLabel  = "devsandbox.io/sandbox-uid"
)

var (
	ErrUnauthenticated = errors.New("projected identity is not authenticated")
	ErrBindingMismatch = errors.New("projected identity does not match sandbox binding")
)

type TokenReviewResult struct {
	Authenticated     bool
	Audiences         []string
	Username          string
	ServiceAccountUID string
	Extra             map[string][]string
}

type TokenReviewer interface {
	Review(ctx context.Context, token, audience string) (TokenReviewResult, error)
}

type ServiceAccountBinding struct {
	UID    string
	Labels map[string]string
}

type PodBinding struct {
	UID                string
	ServiceAccountName string
	Labels             map[string]string
}

type SandboxBinding struct {
	UID         string
	OwnerUserID string
}

// BindingReader exposes only the reads required by the broker.
type BindingReader interface {
	GetServiceAccount(ctx context.Context, namespace, name string) (ServiceAccountBinding, error)
	GetPod(ctx context.Context, namespace, name string) (PodBinding, error)
	GetDevSandbox(ctx context.Context, namespace, name string) (SandboxBinding, error)
}

type CredentialProvider interface {
	AccessToken(ctx context.Context, userID string) (string, time.Time, error)
}

type RequestIdentity struct {
	Namespace          string
	ServiceAccountName string
	ServiceAccountUID  string
	PodName            string
	PodUID             string
	SandboxName        string
	SandboxUID         string
	OwnerUserID        string
}

type Service struct {
	Audience    string
	Reviewer    TokenReviewer
	Bindings    BindingReader
	Credentials CredentialProvider
}

func (s *Service) Credential(ctx context.Context, projectedToken string) (string, time.Time, RequestIdentity, error) {
	var identity RequestIdentity
	if s.Reviewer == nil || s.Bindings == nil || s.Credentials == nil {
		return "", time.Time{}, identity, errors.New("broker service is not configured")
	}
	audience := s.Audience
	if audience == "" {
		audience = DefaultAudience
	}
	if projectedToken == "" {
		return "", time.Time{}, identity, ErrUnauthenticated
	}
	review, err := s.Reviewer.Review(ctx, projectedToken, audience)
	if err != nil {
		return "", time.Time{}, identity, fmt.Errorf("review projected identity: %w", err)
	}
	if !review.Authenticated || len(review.Audiences) != 1 || review.Audiences[0] != audience {
		return "", time.Time{}, identity, ErrUnauthenticated
	}
	namespace, serviceAccount, ok := parseServiceAccountUsername(review.Username)
	if !ok || review.ServiceAccountUID == "" {
		return "", time.Time{}, identity, ErrUnauthenticated
	}
	podName, okName := exactlyOne(review.Extra["authentication.kubernetes.io/pod-name"])
	podUID, okUID := exactlyOne(review.Extra["authentication.kubernetes.io/pod-uid"])
	if !okName || !okUID {
		return "", time.Time{}, identity, ErrBindingMismatch
	}

	identity.Namespace = namespace
	identity.ServiceAccountName = serviceAccount
	identity.ServiceAccountUID = review.ServiceAccountUID
	identity.PodName = podName
	identity.PodUID = podUID

	account, err := s.Bindings.GetServiceAccount(ctx, namespace, serviceAccount)
	if err != nil {
		return "", time.Time{}, identity, fmt.Errorf("read bound service account: %w", err)
	}
	if account.UID == "" || account.UID != review.ServiceAccountUID {
		return "", time.Time{}, identity, ErrBindingMismatch
	}
	pod, err := s.Bindings.GetPod(ctx, namespace, podName)
	if err != nil {
		return "", time.Time{}, identity, fmt.Errorf("read bound pod: %w", err)
	}
	if pod.UID == "" || pod.UID != podUID || pod.ServiceAccountName != serviceAccount {
		return "", time.Time{}, identity, ErrBindingMismatch
	}
	sandboxName := pod.Labels[SandboxNameLabel]
	sandboxUID := pod.Labels[SandboxUIDLabel]
	if sandboxName == "" || sandboxUID == "" ||
		account.Labels[SandboxNameLabel] != sandboxName ||
		account.Labels[SandboxUIDLabel] != sandboxUID {
		return "", time.Time{}, identity, ErrBindingMismatch
	}
	sandbox, err := s.Bindings.GetDevSandbox(ctx, namespace, sandboxName)
	if err != nil {
		return "", time.Time{}, identity, fmt.Errorf("read bound DevSandbox: %w", err)
	}
	if sandbox.UID == "" || sandbox.UID != sandboxUID || sandbox.OwnerUserID == "" {
		return "", time.Time{}, identity, ErrBindingMismatch
	}
	identity.SandboxName = sandboxName
	identity.SandboxUID = sandboxUID
	identity.OwnerUserID = sandbox.OwnerUserID

	token, expiresAt, err := s.Credentials.AccessToken(ctx, sandbox.OwnerUserID)
	if err != nil {
		return "", time.Time{}, identity, err
	}
	if token == "" || !expiresAt.After(time.Now()) {
		return "", time.Time{}, identity, errors.New("credential provider returned an empty or expired token")
	}
	return token, expiresAt, identity, nil
}

func parseServiceAccountUsername(username string) (string, string, bool) {
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(username, prefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(username, prefix), ":")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func exactlyOne(values []string) (string, bool) {
	returnValue := ""
	if len(values) == 1 {
		returnValue = strings.TrimSpace(values[0])
	}
	return returnValue, returnValue != ""
}

var _ CredentialProvider = (*auth.CredentialManager)(nil)
