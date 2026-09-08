package api

import (
	"context"
	"strings"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Store interface {
	ListTemplates(context.Context) ([]devsandboxv1alpha1.DevSandboxTemplate, error)
	GetTemplate(context.Context, string) (*devsandboxv1alpha1.DevSandboxTemplate, error)
	CreateSandbox(context.Context, *devsandboxv1alpha1.DevSandbox) error
	ListSandboxes(context.Context) ([]devsandboxv1alpha1.DevSandbox, error)
	GetSandbox(context.Context, string) (*devsandboxv1alpha1.DevSandbox, error)
	UpdateSandbox(context.Context, *devsandboxv1alpha1.DevSandbox) error
	DeleteSandbox(context.Context, *devsandboxv1alpha1.DevSandbox) error
	ListEvents(context.Context, types.UID) ([]corev1.Event, error)
}

type JobStore interface {
	CreateJob(context.Context, *devsandboxv1alpha1.DevSandboxJob) error
	ListJobs(context.Context) ([]devsandboxv1alpha1.DevSandboxJob, error)
	UpdateJobStatus(context.Context, string, devsandboxv1alpha1.DevSandboxJobStatus) error
}

func (s KubernetesStore) RenewActivity(ctx context.Context, sandbox *devsandboxv1alpha1.DevSandbox, holder string, now time.Time) error {
	if sandbox == nil || sandbox.UID == "" {
		return nil
	}
	uid := strings.ToLower(strings.ReplaceAll(string(sandbox.UID), "_", "-"))
	if len(uid) > 48 {
		uid = uid[:48]
	}
	key := types.NamespacedName{Namespace: sandbox.Namespace, Name: "activity-" + uid}
	for attempts := 0; attempts < 5; attempts++ {
		var lease coordinationv1.Lease
		if err := s.Client.Get(ctx, key, &lease); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if lease.Labels["devsandbox.io/sandbox-uid"] != string(sandbox.UID) {
			return nil
		}
		renewed := metav1.NewMicroTime(now.UTC())
		lease.Spec.RenewTime = &renewed
		lease.Spec.HolderIdentity = &holder
		if err := s.Client.Update(ctx, &lease); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return err
		}
	}
	return apierrors.NewConflict(coordinationv1.Resource("leases"), key.Name, nil)
}

// KubernetesStore intentionally exposes only the exact domain operations used
// by the management service.
type KubernetesStore struct {
	Client    client.Client
	Namespace string
}

func (s KubernetesStore) ListTemplates(ctx context.Context) ([]devsandboxv1alpha1.DevSandboxTemplate, error) {
	var list devsandboxv1alpha1.DevSandboxTemplateList
	if err := s.Client.List(ctx, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (s KubernetesStore) GetTemplate(ctx context.Context, name string) (*devsandboxv1alpha1.DevSandboxTemplate, error) {
	var value devsandboxv1alpha1.DevSandboxTemplate
	if err := s.Client.Get(ctx, types.NamespacedName{Name: name}, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s KubernetesStore) CreateSandbox(ctx context.Context, value *devsandboxv1alpha1.DevSandbox) error {
	value.Namespace = s.Namespace
	return s.Client.Create(ctx, value)
}

func (s KubernetesStore) ListSandboxes(ctx context.Context) ([]devsandboxv1alpha1.DevSandbox, error) {
	var list devsandboxv1alpha1.DevSandboxList
	if err := s.Client.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (s KubernetesStore) GetSandbox(ctx context.Context, name string) (*devsandboxv1alpha1.DevSandbox, error) {
	var value devsandboxv1alpha1.DevSandbox
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s KubernetesStore) UpdateSandbox(ctx context.Context, value *devsandboxv1alpha1.DevSandbox) error {
	return s.Client.Update(ctx, value)
}

func (s KubernetesStore) DeleteSandbox(ctx context.Context, value *devsandboxv1alpha1.DevSandbox) error {
	return s.Client.Delete(ctx, value)
}

func (s KubernetesStore) ListEvents(ctx context.Context, uid types.UID) ([]corev1.Event, error) {
	var list corev1.EventList
	if err := s.Client.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}

	result := make([]corev1.Event, 0)
	for _, event := range list.Items {
		if event.InvolvedObject.UID == uid {
			result = append(result, event)
		}
	}
	return result, nil
}

func (s KubernetesStore) CreateJob(ctx context.Context, value *devsandboxv1alpha1.DevSandboxJob) error {
	value.Namespace = s.Namespace
	status := value.DeepCopy().Status
	value.Status = devsandboxv1alpha1.DevSandboxJobStatus{}
	if err := s.Client.Create(ctx, value); err != nil {
		return err
	}
	return s.UpdateJobStatus(ctx, value.Name, status)
}

func (s KubernetesStore) ListJobs(ctx context.Context) ([]devsandboxv1alpha1.DevSandboxJob, error) {
	var list devsandboxv1alpha1.DevSandboxJobList
	if err := s.Client.List(ctx, &list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (s KubernetesStore) UpdateJobStatus(ctx context.Context, name string, status devsandboxv1alpha1.DevSandboxJobStatus) error {
	key := types.NamespacedName{Namespace: s.Namespace, Name: name}
	for attempts := 0; attempts < 5; attempts++ {
		var value devsandboxv1alpha1.DevSandboxJob
		if err := s.Client.Get(ctx, key, &value); err != nil {
			return err
		}
		copy := (&devsandboxv1alpha1.DevSandboxJob{Status: status}).DeepCopy()
		value.Status = copy.Status
		if err := s.Client.Status().Update(ctx, &value); err == nil {
			return nil
		} else if !apierrors.IsConflict(err) {
			return err
		}
	}
	return apierrors.NewConflict(devsandboxv1alpha1.GroupVersion.WithResource("devsandboxjobs").GroupResource(), name, nil)
}
