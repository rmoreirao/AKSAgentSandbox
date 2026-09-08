package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type KubernetesDeviceStateStore struct {
	Client    client.Client
	Namespace string
	Now       func() time.Time
}

const deviceStateAnnotation = "devsandbox.io/device-state"

func (s KubernetesDeviceStateStore) Put(ctx context.Context, state string, value auth.DeviceState) error {
	if err := s.validate(state, value); err != nil {
		return err
	}
	data, _ := json.Marshal(value)
	seconds := int32(value.ExpiresAt.Sub(s.now()) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	now := metav1.NewMicroTime(s.now())
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name: deviceLeaseName(state), Namespace: s.Namespace,
			Labels: map[string]string{"devsandbox.io/device-flow": "true"},
			Annotations: map[string]string{
				deviceStateAnnotation: base64.RawURLEncoding.EncodeToString(data),
				expiresAnnotationAPI:  strconv.FormatInt(value.ExpiresAt.Unix(), 10),
			},
		},
		Spec: coordinationv1.LeaseSpec{LeaseDurationSeconds: &seconds, AcquireTime: &now, RenewTime: &now},
	}
	return s.Client.Create(ctx, lease)
}

func (s KubernetesDeviceStateStore) Get(ctx context.Context, state string) (auth.DeviceState, error) {
	lease, value, err := s.get(ctx, state)
	if err != nil {
		return value, err
	}
	if !value.ExpiresAt.After(s.now()) {
		_ = s.Client.Delete(ctx, lease)
		return auth.DeviceState{}, auth.ErrInvalidState
	}
	return value, nil
}

func (s KubernetesDeviceStateStore) Update(ctx context.Context, state string, value auth.DeviceState) error {
	if err := s.validate(state, value); err != nil {
		return err
	}
	lease, _, err := s.get(ctx, state)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(value)
	lease.Annotations[deviceStateAnnotation] = base64.RawURLEncoding.EncodeToString(data)
	lease.Annotations[expiresAnnotationAPI] = strconv.FormatInt(value.ExpiresAt.Unix(), 10)
	now := metav1.NewMicroTime(s.now())
	lease.Spec.RenewTime = &now
	if err := s.Client.Update(ctx, lease); err != nil {
		if apierrors.IsConflict(err) {
			return auth.ErrSlowDown
		}
		return err
	}
	return nil
}

func (s KubernetesDeviceStateStore) Delete(ctx context.Context, state string) error {
	lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: deviceLeaseName(state), Namespace: s.Namespace}}
	if err := s.Client.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (s KubernetesDeviceStateStore) get(ctx context.Context, state string) (*coordinationv1.Lease, auth.DeviceState, error) {
	var lease coordinationv1.Lease
	if s.Client == nil || s.Namespace == "" || state == "" {
		return nil, auth.DeviceState{}, auth.ErrInvalidState
	}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: deviceLeaseName(state)}, &lease); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, auth.DeviceState{}, auth.ErrInvalidState
		}
		return nil, auth.DeviceState{}, err
	}
	encoded := lease.Annotations[deviceStateAnnotation]
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	var value auth.DeviceState
	if err != nil || json.Unmarshal(data, &value) != nil {
		return nil, value, auth.ErrInvalidState
	}
	return &lease, value, nil
}

func (s KubernetesDeviceStateStore) validate(state string, value auth.DeviceState) error {
	if s.Client == nil || s.Namespace == "" || state == "" || value.DeviceCode == "" || value.ExpiresAt.IsZero() {
		return errors.New("Kubernetes device state store is not configured")
	}
	return nil
}

func (s KubernetesDeviceStateStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func deviceLeaseName(state string) string {
	sum := sha256.Sum256([]byte(state))
	return "device-" + hex.EncodeToString(sum[:16])
}

const expiresAnnotationAPI = "devsandbox.io/expires-at"
