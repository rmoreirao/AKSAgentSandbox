package operator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	"github.com/rmoreirao/AKSAgentSandbox/internal/lifecycle"
	"github.com/rmoreirao/AKSAgentSandbox/internal/observability"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

func AddSchemes(scheme *runtime.Scheme) error {
	if scheme == nil {
		return errors.New("scheme is required")
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		return err
	}
	return devsandboxv1alpha1.AddToScheme(scheme)
}

func SetupControllers(manager ctrl.Manager) error {
	return SetupControllersWithObservability(manager, nil, nil)
}

func SetupControllersWithObservability(manager ctrl.Manager, audit observability.AuditSink, metrics *observability.Metrics) error {
	upstream := &unstructured.Unstructured{}
	upstream.SetGroupVersionKind(SandboxGVK)
	devsandbox := &DevSandboxReconciler{Client: manager.GetClient(), Scheme: manager.GetScheme(), Audit: audit, Metrics: metrics}
	if err := ctrl.NewControllerManagedBy(manager).
		Named("devsandbox").
		For(&devsandboxv1alpha1.DevSandbox{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(upstream).
		Watches(
			&source.Kind{Type: &coordinationv1.Lease{}},
			handler.EnqueueRequestsFromMapFunc(mapLeaseToSandbox),
			builder.WithPredicates(managedLeasePredicate{}),
		).
		Watches(
			&source.Kind{Type: &devsandboxv1alpha1.DevSandboxJob{}},
			handler.EnqueueRequestsFromMapFunc(mapJobToSandbox),
		).
		Complete(observedReconciler{next: devsandbox, metrics: metrics}); err != nil {
		return fmt.Errorf("set up DevSandbox controller: %w", err)
	}
	if err := ctrl.NewControllerManagedBy(manager).
		Named("devsandbox-template").
		For(&devsandboxv1alpha1.DevSandboxTemplate{}).
		Complete(observedReconciler{next: &TemplateReconciler{Client: manager.GetClient()}, metrics: metrics}); err != nil {
		return fmt.Errorf("set up template controller: %w", err)
	}
	if err := ctrl.NewControllerManagedBy(manager).
		Named("devsandbox-quota").
		For(&devsandboxv1alpha1.DevSandboxUserQuota{}).
		Complete(observedReconciler{next: &QuotaReconciler{Client: manager.GetClient()}, metrics: metrics}); err != nil {
		return fmt.Errorf("set up quota controller: %w", err)
	}
	if err := ctrl.NewControllerManagedBy(manager).
		Named("devsandbox-job").
		For(&devsandboxv1alpha1.DevSandboxJob{}).
		Complete(observedReconciler{next: &JobReconciler{Client: manager.GetClient()}, metrics: metrics}); err != nil {
		return fmt.Errorf("set up job controller: %w", err)
	}
	if err := ctrl.NewControllerManagedBy(manager).
		Named("devsandbox-activity-lease").
		For(&coordinationv1.Lease{}, builder.WithPredicates(managedLeasePredicate{})).
		Complete(observedReconciler{next: &ActivityLeaseReconciler{Client: manager.GetClient()}, metrics: metrics}); err != nil {
		return fmt.Errorf("set up activity Lease controller: %w", err)
	}

	return nil
}

type observedReconciler struct {
	next    reconcile.Reconciler
	metrics *observability.Metrics
}

func (r observedReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	result, err := r.next.Reconcile(ctx, request)
	if err != nil && r.metrics != nil {
		r.metrics.ReconciliationFailures.Inc()
	}
	return result, err
}

type TemplateReconciler struct {
	client.Client
	Now func() time.Time
}

func (r *TemplateReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var template devsandboxv1alpha1.DevSandboxTemplate
	if err := r.Get(ctx, request.NamespacedName, &template); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	status := metav1.ConditionTrue
	reason := "Valid"
	message := "template is valid for operator rendering"
	if template.Spec.Image.Repository == "" || !strings.HasPrefix(template.Spec.Image.Digest, "sha256:") {
		status, reason, message = metav1.ConditionFalse, "InvalidImage", "template image must have a repository and digest"
	} else if spec, err := blueprintSpec(template.Spec.SandboxBlueprint); err != nil {
		status, reason, message = metav1.ConditionFalse, "InvalidBlueprint", err.Error()
	} else if _, found := spec["volumeClaimTemplates"]; found {
		status, reason, message = metav1.ConditionFalse, "OwnedStorage", "volumeClaimTemplates are not permitted"
	}
	before := template.Status
	setCondition(&template.Status.Conditions, "Valid", status, reason, message, template.Generation, now)
	if reflect.DeepEqual(before, template.Status) {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, r.Status().Update(ctx, &template)
}

type QuotaReconciler struct {
	client.Client
	Now func() time.Time
}

func (r *QuotaReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var quota devsandboxv1alpha1.DevSandboxUserQuota
	if err := r.Get(ctx, request.NamespacedName, &quota); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	var sandboxes devsandboxv1alpha1.DevSandboxList
	if err := r.List(ctx, &sandboxes, client.InNamespace(quota.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	expected := make(map[string]lifecycle.ReservationRequest)
	for i := range sandboxes.Items {
		sandbox := &sandboxes.Items[i]
		if sandbox.Spec.Owner.GitHubUserID != quota.Spec.GitHubUserID || !sandbox.DeletionTimestamp.IsZero() {
			continue
		}
		if conditionReason(sandbox.Status.Conditions, ConditionQuotaReady) == "QuotaExceeded" {
			continue
		}
		requested := lifecycle.ReservationRequest{ID: reservationID(sandbox), SandboxName: sandbox.Name}
		requested.Retained = 1
		if sandbox.Status.Phase != PhaseStopped && sandbox.Status.Phase != PhaseFailed &&
			sandbox.Spec.DesiredState != devsandboxv1alpha1.DesiredStateStopped {
			requested.Active = 1
		}
		var pvc corev1.PersistentVolumeClaim
		pvcErr := r.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: workspacePVCName(sandbox)}, &pvc)
		if pvcErr == nil || (!apierrors.IsNotFound(pvcErr) && pvcErr != nil) {
			if pvcErr != nil {
				return ctrl.Result{}, pvcErr
			}
			requested.StorageGiB, _ = storageGiB(pvc.Spec.Resources.Requests.Storage().String())
		} else if sandbox.Status.Phase != PhaseFailed {
			requested.StorageGiB, _ = storageGiB(sandbox.Spec.Profile.Storage)
		}
		expected[requested.ID] = requested
	}

	manager := lifecycle.QuotaManager{Store: clientQuotaStore{Client: r.Client}, MaxAttempts: 12, Now: r.Now}
	for _, reservation := range quota.Status.Reservations {
		if _, found := expected[reservation.ID]; !found {
			if err := manager.Release(ctx, quota.Namespace, quota.Name, reservation.ID); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	for _, reservation := range expected {
		if err := manager.ReconcileReservation(ctx, quota.Namespace, quota.Name, reservation); err != nil {
			if errors.Is(err, lifecycle.ErrQuotaExceeded) {
				return ctrl.Result{RequeueAfter: DefaultRequeue}, nil
			}
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

type JobReconciler struct {
	client.Client
	Now func() time.Time
}

func (r *JobReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var job devsandboxv1alpha1.DevSandboxJob
	if err := r.Get(ctx, request.NamespacedName, &job); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if job.Status.State != devsandboxv1alpha1.JobStateRunning &&
		job.Status.State != devsandboxv1alpha1.JobStatePending {
		return ctrl.Result{}, nil
	}
	var sandbox devsandboxv1alpha1.DevSandbox
	if err := r.Get(ctx, types.NamespacedName{Namespace: job.Namespace, Name: job.Spec.SandboxName}, &sandbox); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if sandbox.Spec.DesiredState != devsandboxv1alpha1.DesiredStateStopped && sandbox.Status.Phase != PhaseStopped {
		return ctrl.Result{}, nil
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	job.Status.State = devsandboxv1alpha1.JobStateStopped
	job.Status.Reason = "SandboxStopped"
	job.Status.CompletedAt = lifecycle.TimePointer(now)
	setCondition(&job.Status.Conditions, "Complete", metav1.ConditionTrue, "SandboxStopped",
		"managed job was terminated because its sandbox stopped", job.Generation, now)
	return ctrl.Result{}, r.Status().Update(ctx, &job)
}

type ActivityLeaseReconciler struct {
	client.Client
}

func (r *ActivityLeaseReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var lease coordinationv1.Lease
	if err := r.Get(ctx, request.NamespacedName, &lease); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	name := lease.Labels[SandboxLabel]
	if name == "" {
		return ctrl.Result{}, nil
	}
	var sandbox devsandboxv1alpha1.DevSandbox
	if err := r.Get(ctx, types.NamespacedName{Namespace: lease.Namespace, Name: name}, &sandbox); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{RequeueAfter: DefaultRequeue}, nil
}

func mapLeaseToSandbox(object client.Object) []reconcile.Request {
	name := object.GetLabels()[SandboxLabel]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: object.GetNamespace(), Name: name}}}
}

func mapJobToSandbox(object client.Object) []reconcile.Request {
	job, ok := object.(*devsandboxv1alpha1.DevSandboxJob)
	if !ok || job.Spec.SandboxName == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: job.Namespace, Name: job.Spec.SandboxName}}}
}
