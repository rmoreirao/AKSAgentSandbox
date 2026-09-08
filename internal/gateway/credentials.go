package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrCredentialNotFound = errors.New("one-time credential not found")
	ErrCredentialUsed     = errors.New("one-time credential already used")
	ErrCredentialExpired  = errors.New("one-time credential expired")
)

type CredentialStore interface {
	Issue(ctx context.Context, routeClaim string, lifetime time.Duration) (string, time.Time, error)
	Consume(ctx context.Context, credential string) (string, error)
}

type memoryCredential struct {
	claim   string
	expires time.Time
	used    bool
}

type MemoryCredentialStore struct {
	mu     sync.Mutex
	values map[string]memoryCredential
	Now    func() time.Time
	Random io.Reader
}

func (s *MemoryCredentialStore) Issue(_ context.Context, claim string, lifetime time.Duration) (string, time.Time, error) {
	if claim == "" || lifetime <= 0 {
		return "", time.Time{}, errors.New("route claim and positive lifetime are required")
	}
	credential, err := randomCredential(s.Random)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := s.now().Add(lifetime)
	s.mu.Lock()
	if s.values == nil {
		s.values = make(map[string]memoryCredential)
	}
	s.values[credential] = memoryCredential{claim: claim, expires: expires}
	s.mu.Unlock()
	return credential, expires, nil
}

func (s *MemoryCredentialStore) Consume(_ context.Context, credential string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[credential]
	if !ok {
		return "", ErrCredentialNotFound
	}
	if !value.expires.After(s.now()) {
		delete(s.values, credential)
		return "", ErrCredentialExpired
	}
	if value.used {
		return "", ErrCredentialUsed
	}
	value.used = true
	s.values[credential] = value
	return value.claim, nil
}

func (s *MemoryCredentialStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

type KubernetesCredentialStore struct {
	Client    client.Client
	Namespace string
	Now       func() time.Time
	Random    io.Reader
}

const (
	claimAnnotation   = "devsandbox.io/route-claim"
	expiresAnnotation = "devsandbox.io/expires-at"
	credentialLabel   = "devsandbox.io/one-time-credential"
)

func (s *KubernetesCredentialStore) Issue(ctx context.Context, claim string, lifetime time.Duration) (string, time.Time, error) {
	if s.Client == nil || s.Namespace == "" || claim == "" || lifetime <= 0 {
		return "", time.Time{}, errors.New("Kubernetes one-time credential store is not configured")
	}
	for attempts := 0; attempts < 3; attempts++ {
		credential, err := randomCredential(s.Random)
		if err != nil {
			return "", time.Time{}, err
		}
		expires := s.now().Add(lifetime)
		unused := "unused"
		seconds := int32(lifetime / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		now := metav1.NewMicroTime(s.now())
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name: credentialLeaseName(credential), Namespace: s.Namespace,
				Labels: map[string]string{credentialLabel: "true"},
				Annotations: map[string]string{
					claimAnnotation: claim, expiresAnnotation: strconv.FormatInt(expires.Unix(), 10),
				},
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity: &unused, LeaseDurationSeconds: &seconds,
				AcquireTime: &now, RenewTime: &now,
			},
		}
		if err := s.Client.Create(ctx, lease); err == nil {
			return credential, expires, nil
		} else if !apierrors.IsAlreadyExists(err) {
			return "", time.Time{}, err
		}
	}
	return "", time.Time{}, errors.New("could not allocate a unique one-time credential")
}

func (s *KubernetesCredentialStore) Consume(ctx context.Context, credential string) (string, error) {
	if s.Client == nil || s.Namespace == "" || credential == "" {
		return "", ErrCredentialNotFound
	}
	key := types.NamespacedName{Namespace: s.Namespace, Name: credentialLeaseName(credential)}
	for attempts := 0; attempts < 5; attempts++ {
		var lease coordinationv1.Lease
		if err := s.Client.Get(ctx, key, &lease); err != nil {
			if apierrors.IsNotFound(err) {
				return "", ErrCredentialNotFound
			}
			return "", err
		}
		expires, err := strconv.ParseInt(lease.Annotations[expiresAnnotation], 10, 64)
		if err != nil || expires <= s.now().Unix() {
			return "", ErrCredentialExpired
		}
		if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != "unused" {
			return "", ErrCredentialUsed
		}
		claim := lease.Annotations[claimAnnotation]
		if claim == "" {
			return "", ErrCredentialNotFound
		}

		// Deletion is the consume transition. UID and resourceVersion are API
		// server preconditions, making the successful delete the linearization
		// point across replicas. A concurrent modification is re-read and
		// retried; a concurrent delete means another consumer won.
		uid := lease.UID
		resourceVersion := lease.ResourceVersion
		preconditions := client.Preconditions{ResourceVersion: &resourceVersion}
		if uid != "" {
			preconditions.UID = &uid
		}
		if err := s.Client.Delete(ctx, &lease, preconditions); err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			if apierrors.IsNotFound(err) {
				return "", ErrCredentialUsed
			}
			return "", err
		}
		return claim, nil
	}
	return "", ErrCredentialUsed
}

func (s *KubernetesCredentialStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func randomCredential(reader io.Reader) (string, error) {
	if reader == nil {
		reader = rand.Reader
	}
	raw := make([]byte, 32)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func credentialLeaseName(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return "vscode-" + hex.EncodeToString(sum[:16])
}
