package operator

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	"github.com/rmoreirao/AKSAgentSandbox/internal/lifecycle"
	"github.com/rmoreirao/AKSAgentSandbox/internal/observability"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type DevSandboxReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Now     func() time.Time
	Audit   observability.AuditSink
	Metrics *observability.Metrics
}

func (r *DevSandboxReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var sandbox devsandboxv1alpha1.DevSandbox
	if err := r.Get(ctx, request.NamespacedName, &sandbox); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := r.now()

	if !sandbox.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &sandbox)
	}
	if !controllerutil.ContainsFinalizer(&sandbox, Finalizer) {
		controllerutil.AddFinalizer(&sandbox, Finalizer)
		if err := r.Update(ctx, &sandbox); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if sandbox.Status.Phase == PhaseFailed {
		if sandbox.Spec.DesiredState == devsandboxv1alpha1.DesiredStateRunning {
			setCondition(&sandbox.Status.Conditions, ConditionTransitionValid, metav1.ConditionFalse,
				"FailedSandbox", "a failed sandbox cannot be resumed", sandbox.Generation, now)
		}
		if expired(sandbox.Status.RetentionDeadline, now) {
			if err := r.Delete(ctx, &sandbox); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		return r.updateStatus(ctx, &sandbox, requeueAt(now, sandbox.Status.RetentionDeadline))
	}
	if sandbox.Status.Phase == PhaseStopped && expired(sandbox.Status.RetentionDeadline, now) {
		setCondition(&sandbox.Status.Conditions, ConditionTransitionValid, metav1.ConditionFalse,
			"RetentionExpired", "the stopped retention deadline has elapsed", sandbox.Generation, now)
		if _, err := r.updateStatus(ctx, &sandbox, ctrl.Result{}); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Delete(ctx, &sandbox); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	lifecycle.DefaultDevSandbox(&sandbox)
	if err := lifecycle.ValidateDevSandbox(&sandbox); err != nil {
		return r.fail(ctx, &sandbox, "InvalidSpec", err.Error(), now)
	}
	var template devsandboxv1alpha1.DevSandboxTemplate
	if err := r.Get(ctx, types.NamespacedName{Name: sandbox.Spec.Template.Name}, &template); err != nil {
		if apierrors.IsNotFound(err) {
			return r.fail(ctx, &sandbox, "TemplateNotFound", err.Error(), now)
		}
		return ctrl.Result{}, err
	}
	if template.Spec.Version != sandbox.Spec.Template.Version ||
		template.Spec.Image.Digest != sandbox.Spec.Template.ImageDigest {
		return r.fail(ctx, &sandbox, "TemplateMismatch", "template version or image digest does not match the immutable reference", now)
	}
	blueprint, err := blueprintSpec(template.Spec.SandboxBlueprint)
	if err != nil {
		return r.fail(ctx, &sandbox, "InvalidTemplate", err.Error(), now)
	}
	if _, found := blueprint["volumeClaimTemplates"]; found {
		return r.fail(ctx, &sandbox, "InvalidTemplate", "template sandboxBlueprint cannot define volumeClaimTemplates", now)
	}

	lease, err := r.getActivityLease(ctx, &sandbox)
	if err != nil {
		return ctrl.Result{}, err
	}
	jobs, err := r.listJobs(ctx, &sandbox)
	if err != nil {
		return ctrl.Result{}, err
	}
	baseline := sandbox.CreationTimestamp.Time
	if baseline.IsZero() {
		baseline = now
	}
	if sandbox.Status.LastActivityTime != nil {
		baseline = sandbox.Status.LastActivityTime.Time
	}
	activity := lifecycle.EvaluateActivity(
		now, baseline, time.Duration(sandbox.Spec.Lifecycle.IdleTimeoutSeconds)*time.Second, lease, jobs,
	)
	effectiveState := sandbox.Spec.DesiredState
	if effectiveState == devsandboxv1alpha1.DesiredStateRunning &&
		sandbox.Status.Phase == PhaseRunning && activity.ShouldIdleStop {
		sandbox.Spec.DesiredState = devsandboxv1alpha1.DesiredStateStopped
		effectiveState = devsandboxv1alpha1.DesiredStateStopped
		if err := r.Update(ctx, &sandbox); err != nil {
			return ctrl.Result{}, err
		}
		if r.Metrics != nil {
			r.Metrics.IdleStops.Inc()
		}
	}

	storage, err := storageGiB(sandbox.Spec.Profile.Storage)
	if err != nil {
		return r.fail(ctx, &sandbox, "InvalidStorage", err.Error(), now)
	}
	quota, err := r.ensureQuota(ctx, &sandbox)
	if err != nil {
		return ctrl.Result{}, err
	}
	reservation := lifecycle.ReservationRequest{
		ID: reservationID(&sandbox), SandboxName: sandbox.Name, Retained: 1, StorageGiB: storage,
	}
	if effectiveState == devsandboxv1alpha1.DesiredStateRunning {
		reservation.Active = 1
	}
	manager := lifecycle.QuotaManager{Store: clientQuotaStore{Client: r.Client}, MaxAttempts: 12, Now: r.Now}
	if err := manager.ReconcileReservation(ctx, quota.Namespace, quota.Name, reservation); err != nil {
		if errors.Is(err, lifecycle.ErrQuotaExceeded) {
			r.audit(&sandbox, "quota.rejection", "failure", "quota_exceeded")
			setCondition(&sandbox.Status.Conditions, ConditionQuotaReady, metav1.ConditionFalse,
				"QuotaExceeded", err.Error(), sandbox.Generation, now)
			setCondition(&sandbox.Status.Conditions, ConditionTransitionValid, metav1.ConditionFalse,
				"QuotaExceeded", "the requested lifecycle transition was rejected", sandbox.Generation, now)
			if sandbox.Status.Phase == PhaseStopped &&
				sandbox.Spec.DesiredState == devsandboxv1alpha1.DesiredStateRunning {
				sandbox.Spec.DesiredState = devsandboxv1alpha1.DesiredStateStopped
				if updateErr := r.Update(ctx, &sandbox); updateErr != nil {
					return ctrl.Result{}, updateErr
				}
			}
			if sandbox.Status.Phase == "" {
				sandbox.Status.Phase = PhasePending
			}
			return r.updateStatus(ctx, &sandbox, ctrl.Result{RequeueAfter: DefaultRequeue})
		}
		return ctrl.Result{}, err
	}
	setCondition(&sandbox.Status.Conditions, ConditionQuotaReady, metav1.ConditionTrue,
		"Reserved", "quota is atomically reserved", sandbox.Generation, now)
	setCondition(&sandbox.Status.Conditions, ConditionTransitionValid, metav1.ConditionTrue,
		"Accepted", "the requested lifecycle transition is valid", sandbox.Generation, now)

	pvc, err := r.ensurePVC(ctx, &sandbox)
	if err != nil {
		return r.fail(ctx, &sandbox, "PVCFailed", err.Error(), now)
	}
	account, err := r.ensureServiceAccount(ctx, &sandbox)
	if err != nil {
		return r.fail(ctx, &sandbox, "ServiceAccountFailed", err.Error(), now)
	}
	if effectiveState == devsandboxv1alpha1.DesiredStateRunning {
		if _, err := r.ensureActivityLease(ctx, &sandbox); err != nil {
			return r.fail(ctx, &sandbox, "ActivityLeaseFailed", err.Error(), now)
		}
	} else {
		stale := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: activityLeaseName(&sandbox), Namespace: sandbox.Namespace},
		}
		if err := r.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	mode := "Running"
	shutdown := activity.IdleDeadline
	if effectiveState == devsandboxv1alpha1.DesiredStateRunning &&
		sandbox.Status.Phase != PhaseRunning {
		shutdown = now.Add(time.Duration(sandbox.Spec.Lifecycle.IdleTimeoutSeconds) * time.Second)
	}
	if activity.HasLiveLease && !shutdown.After(now) {
		shutdown = now.Add(time.Duration(sandbox.Spec.Lifecycle.IdleTimeoutSeconds) * time.Second)
	}
	if effectiveState == devsandboxv1alpha1.DesiredStateStopped {
		mode = "Suspended"
		if sandbox.Status.StoppedAt == nil {
			sandbox.Status.StoppedAt = lifecycle.TimePointer(now)
		}
		if sandbox.Status.RetentionDeadline == nil {
			sandbox.Status.RetentionDeadline = lifecycle.TimePointer(
				sandbox.Status.StoppedAt.Add(time.Duration(sandbox.Spec.Lifecycle.StoppedRetentionSeconds) * time.Second),
			)
		}
		shutdown = now
	} else {
		sandbox.Status.StoppedAt = nil
		sandbox.Status.RetentionDeadline = nil
		if activity.HasRunningJob {
			shutdown = now.Add(lifecycle.ConnectionStaleAfter +
				time.Duration(sandbox.Spec.Lifecycle.IdleTimeoutSeconds)*time.Second)
		}
	}
	rendered, err := RenderUpstreamSandbox(&sandbox, &template, RenderOptions{
		Now: now, OperatingMode: mode, ShutdownTime: shutdown,
		ServiceAccount: account.Name, PVCName: pvc.Name, UpstreamName: upstreamSandboxName(&sandbox),
	})
	if err != nil {
		return r.fail(ctx, &sandbox, "RenderFailed", err.Error(), now)
	}
	if err := controllerutil.SetControllerReference(&sandbox, rendered, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	upstream, err := r.applyUpstream(ctx, rendered)
	if err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return r.fail(ctx, &sandbox, "UpstreamFailed", err.Error(), now)
	}
	if reason, message, failed := upstreamFailure(upstream); failed {
		return r.fail(ctx, &sandbox, reason, message, now)
	}

	sandbox.Status.UpstreamSandboxName = upstream.GetName()
	if shouldProjectActivity(&sandbox, activity.LastActivity, activity.IdleDeadline) {
		sandbox.Status.LastActivityTime = lifecycle.TimePointer(activity.LastActivity)
		sandbox.Status.IdleDeadline = lifecycle.TimePointer(activity.IdleDeadline)
	}
	setCondition(&sandbox.Status.Conditions, ConditionResourcesReady, metav1.ConditionTrue,
		"Reconciled", "workspace PVC, service account, and upstream Sandbox are reconciled", sandbox.Generation, now)
	r.projectUpstreamConditions(&sandbox, upstream, now)
	if effectiveState == devsandboxv1alpha1.DesiredStateStopped {
		sandbox.Status.Phase = PhaseStopped
		setCondition(&sandbox.Status.Conditions, ConditionReady, metav1.ConditionFalse,
			"Suspended", "sandbox is stopped and its workspace is retained", sandbox.Generation, now)
		return r.updateStatus(ctx, &sandbox, requeueAt(now, sandbox.Status.RetentionDeadline))
	}

	if upstreamReady(upstream) {
		if sandbox.Status.Phase != PhaseRunning {
			idleTimeout := time.Duration(sandbox.Spec.Lifecycle.IdleTimeoutSeconds) * time.Second
			sandbox.Status.LastActivityTime = lifecycle.TimePointer(now)
			sandbox.Status.IdleDeadline = lifecycle.TimePointer(now.Add(idleTimeout))
		}
		sandbox.Status.Phase = PhaseRunning
		setCondition(&sandbox.Status.Conditions, ConditionReady, metav1.ConditionTrue,
			"UpstreamReady", "upstream Sandbox is ready", sandbox.Generation, now)
	} else {
		sandbox.Status.Phase = PhaseCreating
		setCondition(&sandbox.Status.Conditions, ConditionReady, metav1.ConditionFalse,
			"UpstreamPending", "waiting for the upstream Sandbox", sandbox.Generation, now)
	}
	next := activity.NextCheck
	if next.Before(now) {
		next = now.Add(time.Second)
	}
	return r.updateStatus(ctx, &sandbox, ctrl.Result{RequeueAfter: next.Sub(now)})
}

func (r *DevSandboxReconciler) reconcileDelete(ctx context.Context, sandbox *devsandboxv1alpha1.DevSandbox) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(sandbox, Finalizer) {
		return ctrl.Result{}, nil
	}
	workspaceKey := types.NamespacedName{Namespace: sandbox.Namespace, Name: workspacePVCName(sandbox)}
	var workspace corev1.PersistentVolumeClaim
	if err := r.Get(ctx, workspaceKey, &workspace); err == nil && workspace.Spec.VolumeName != "" &&
		sandbox.Annotations[WorkspacePVAnnotation] == "" {
		if sandbox.Annotations == nil {
			sandbox.Annotations = make(map[string]string)
		}
		sandbox.Annotations[WorkspacePVAnnotation] = workspace.Spec.VolumeName
		if err := r.Update(ctx, sandbox); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	} else if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	children := []client.Object{
		&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: activityLeaseName(sandbox), Namespace: sandbox.Namespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: serviceAccountName(sandbox), Namespace: sandbox.Namespace}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: workspacePVCName(sandbox), Namespace: sandbox.Namespace}},
	}
	upstream := &unstructured.Unstructured{}
	upstream.SetGroupVersionKind(SandboxGVK)
	upstream.SetName(upstreamSandboxName(sandbox))
	upstream.SetNamespace(sandbox.Namespace)
	children = append(children, upstream)
	for _, child := range children {
		if err := r.Delete(ctx, child); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	var jobs devsandboxv1alpha1.DevSandboxJobList
	if err := r.List(ctx, &jobs, client.InNamespace(sandbox.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for i := range jobs.Items {
		if jobs.Items[i].Spec.SandboxName == sandbox.Name {
			if err := r.Delete(ctx, &jobs.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, workspaceKey, &pvc); err == nil {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if volumeName := sandbox.Annotations[WorkspacePVAnnotation]; volumeName != "" {
		var volume corev1.PersistentVolume
		if err := r.Get(ctx, types.NamespacedName{Name: volumeName}, &volume); err == nil {
			if err := r.Delete(ctx, &volume); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	quotaName := quotaNameForUser(sandbox.Spec.Owner.GitHubUserID)
	manager := lifecycle.QuotaManager{Store: clientQuotaStore{Client: r.Client}, MaxAttempts: 12, Now: r.Now}
	if err := manager.Release(ctx, sandbox.Namespace, quotaName, reservationID(sandbox)); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(sandbox, Finalizer)
	r.audit(sandbox, "sandbox.delete", "success", "")
	return ctrl.Result{}, r.Update(ctx, sandbox)
}

func (r *DevSandboxReconciler) ensureQuota(ctx context.Context, sandbox *devsandboxv1alpha1.DevSandbox) (*devsandboxv1alpha1.DevSandboxUserQuota, error) {
	name := quotaNameForUser(sandbox.Spec.Owner.GitHubUserID)
	var quota devsandboxv1alpha1.DevSandboxUserQuota
	err := r.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: name}, &quota)
	if err == nil {
		if quota.Spec.GitHubUserID != sandbox.Spec.Owner.GitHubUserID {
			return nil, errors.New("quota identity does not match sandbox owner")
		}
		return &quota, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	quota = devsandboxv1alpha1.DevSandboxUserQuota{
		TypeMeta:   metav1.TypeMeta{APIVersion: devsandboxv1alpha1.GroupVersion.String(), Kind: "DevSandboxUserQuota"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sandbox.Namespace},
		Spec: devsandboxv1alpha1.DevSandboxUserQuotaSpec{
			GitHubUserID: sandbox.Spec.Owner.GitHubUserID,
			Limits:       devsandboxv1alpha1.QuotaLimits{Active: 3, Retained: 10, StorageGiB: 200},
		},
	}
	if err := r.Create(ctx, &quota); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		if err := r.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: name}, &quota); err != nil {
			return nil, err
		}
	}
	return &quota, nil
}

func (r *DevSandboxReconciler) ensurePVC(ctx context.Context, sandbox *devsandboxv1alpha1.DevSandbox) (*corev1.PersistentVolumeClaim, error) {
	desired, err := NewWorkspacePVC(sandbox)
	if err != nil {
		return nil, err
	}
	if err := controllerutil.SetControllerReference(sandbox, desired, r.Scheme); err != nil {
		return nil, err
	}
	var current corev1.PersistentVolumeClaim
	err = r.Get(ctx, objectKey(desired), &current)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return nil, err
		}
		return desired, nil
	}
	if err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(&current, sandbox) {
		return nil, errors.New("workspace PVC exists but is not controlled by this DevSandbox")
	}
	return &current, nil
}

func (r *DevSandboxReconciler) ensureServiceAccount(ctx context.Context, sandbox *devsandboxv1alpha1.DevSandbox) (*corev1.ServiceAccount, error) {
	desired := NewSandboxServiceAccount(sandbox)
	if err := controllerutil.SetControllerReference(sandbox, desired, r.Scheme); err != nil {
		return nil, err
	}
	var current corev1.ServiceAccount
	err := r.Get(ctx, objectKey(desired), &current)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return nil, err
		}
		return desired, nil
	}
	if err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(&current, sandbox) {
		return nil, errors.New("sandbox service account exists but is not controlled by this DevSandbox")
	}
	before := current.DeepCopy()
	current.Labels = desired.Labels
	current.AutomountServiceAccountToken = desired.AutomountServiceAccountToken
	if !reflect.DeepEqual(before.Labels, current.Labels) ||
		!reflect.DeepEqual(before.AutomountServiceAccountToken, current.AutomountServiceAccountToken) {
		if err := r.Update(ctx, &current); err != nil {
			return nil, err
		}
	}
	return &current, nil
}

func (r *DevSandboxReconciler) ensureActivityLease(ctx context.Context, sandbox *devsandboxv1alpha1.DevSandbox) (*coordinationv1.Lease, error) {
	duration := int32(lifecycle.ConnectionStaleAfter / time.Second)
	desired := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name: activityLeaseName(sandbox), Namespace: sandbox.Namespace, Labels: labelsFor(sandbox),
		},
		Spec: coordinationv1.LeaseSpec{LeaseDurationSeconds: &duration},
	}
	if err := controllerutil.SetControllerReference(sandbox, desired, r.Scheme); err != nil {
		return nil, err
	}
	var current coordinationv1.Lease
	err := r.Get(ctx, objectKey(desired), &current)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return nil, err
		}
		return desired, nil
	}
	if err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(&current, sandbox) {
		return nil, errors.New("activity Lease exists but is not controlled by this DevSandbox")
	}
	before := current.DeepCopy()
	current.Labels = desired.Labels
	current.Spec.LeaseDurationSeconds = &duration
	if !reflect.DeepEqual(before.Labels, current.Labels) ||
		!reflect.DeepEqual(before.Spec.LeaseDurationSeconds, current.Spec.LeaseDurationSeconds) {
		if err := r.Update(ctx, &current); err != nil {
			return nil, err
		}
	}
	return &current, nil
}

func (r *DevSandboxReconciler) applyUpstream(ctx context.Context, desired *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(SandboxGVK)
	err := r.Get(ctx, objectKey(desired), current)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return nil, err
		}
		return desired, nil
	}
	if err != nil {
		return nil, err
	}
	desired.SetResourceVersion(current.GetResourceVersion())
	desired.Object["status"] = current.Object["status"]
	if reflect.DeepEqual(current.Object["spec"], desired.Object["spec"]) &&
		reflect.DeepEqual(current.GetLabels(), desired.GetLabels()) &&
		reflect.DeepEqual(current.GetOwnerReferences(), desired.GetOwnerReferences()) {
		return current, nil
	}
	if err := r.Update(ctx, desired); err != nil {
		return nil, err
	}
	return desired, nil
}

func (r *DevSandboxReconciler) getActivityLease(ctx context.Context, sandbox *devsandboxv1alpha1.DevSandbox) (*coordinationv1.Lease, error) {
	var lease coordinationv1.Lease
	err := r.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: activityLeaseName(sandbox)}, &lease)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return &lease, err
}

func (r *DevSandboxReconciler) listJobs(ctx context.Context, sandbox *devsandboxv1alpha1.DevSandbox) ([]devsandboxv1alpha1.DevSandboxJob, error) {
	var list devsandboxv1alpha1.DevSandboxJobList
	if err := r.List(ctx, &list, client.InNamespace(sandbox.Namespace)); err != nil {
		return nil, err
	}
	result := make([]devsandboxv1alpha1.DevSandboxJob, 0)
	for i := range list.Items {
		if list.Items[i].Spec.SandboxName == sandbox.Name {
			result = append(result, list.Items[i])
		}
	}
	return result, nil
}

func (r *DevSandboxReconciler) projectUpstreamConditions(sandbox *devsandboxv1alpha1.DevSandbox, upstream *unstructured.Unstructured, now time.Time) {
	raw, found, _ := unstructured.NestedSlice(upstream.Object, "status", "conditions")
	if !found {
		return
	}
	for _, item := range raw {
		values, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		conditionType, _, _ := unstructured.NestedString(values, "type")
		status, _, _ := unstructured.NestedString(values, "status")
		reason, _, _ := unstructured.NestedString(values, "reason")
		message, _, _ := unstructured.NestedString(values, "message")
		setCondition(&sandbox.Status.Conditions, "Upstream"+conditionType, metav1.ConditionStatus(status),
			reason, message, sandbox.Generation, now)
	}
}

func upstreamReady(upstream *unstructured.Unstructured) bool {
	raw, found, _ := unstructured.NestedSlice(upstream.Object, "status", "conditions")
	if !found {
		return false
	}
	for _, item := range raw {
		values, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		conditionType, _, _ := unstructured.NestedString(values, "type")
		status, _, _ := unstructured.NestedString(values, "status")
		if conditionType == "Ready" && status == string(metav1.ConditionTrue) {
			return true
		}
	}
	return false
}

func upstreamFailure(upstream *unstructured.Unstructured) (string, string, bool) {
	raw, found, _ := unstructured.NestedSlice(upstream.Object, "status", "conditions")
	if !found {
		return "", "", false
	}
	for _, item := range raw {
		values, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		conditionType, _, _ := unstructured.NestedString(values, "type")
		status, _, _ := unstructured.NestedString(values, "status")
		reason, _, _ := unstructured.NestedString(values, "reason")
		message, _, _ := unstructured.NestedString(values, "message")
		lower := strings.ToLower(reason + " " + message)
		if conditionType == "Ready" && status == string(metav1.ConditionFalse) &&
			(strings.Contains(lower, "fail") || strings.Contains(lower, "error") || strings.Contains(lower, "invalid")) {
			if reason == "" {
				reason = "UpstreamFailed"
			}
			return reason, message, true
		}
	}
	return "", "", false
}

func (r *DevSandboxReconciler) fail(
	ctx context.Context, sandbox *devsandboxv1alpha1.DevSandbox,
	reason, message string, now time.Time,
) (ctrl.Result, error) {
	sandbox.Status.Phase = PhaseFailed
	sandbox.Status.IdleDeadline = nil
	if sandbox.Status.RetentionDeadline == nil {
		retention := time.Duration(sandbox.Spec.Lifecycle.FailedRetentionSeconds) * time.Second
		if retention <= 0 {
			retention = time.Duration(lifecycle.DefaultFailedRetentionSeconds) * time.Second
		}
		sandbox.Status.RetentionDeadline = lifecycle.TimePointer(now.Add(retention))
	}
	setCondition(&sandbox.Status.Conditions, ConditionReady, metav1.ConditionFalse, reason, message, sandbox.Generation, now)
	setCondition(&sandbox.Status.Conditions, ConditionResourcesReady, metav1.ConditionFalse, reason, message, sandbox.Generation, now)
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: workspacePVCName(sandbox)}, &pvc); err == nil {
		if storage, storageErr := storageGiB(pvc.Spec.Resources.Requests.Storage().String()); storageErr == nil {
			manager := lifecycle.QuotaManager{Store: clientQuotaStore{Client: r.Client}, MaxAttempts: 12, Now: r.Now}
			_ = manager.ReconcileReservation(ctx, sandbox.Namespace, quotaNameForUser(sandbox.Spec.Owner.GitHubUserID),
				lifecycle.ReservationRequest{
					ID: reservationID(sandbox), SandboxName: sandbox.Name, Retained: 1, StorageGiB: storage,
				})
		}
	}
	return r.updateStatus(ctx, sandbox, requeueAt(now, sandbox.Status.RetentionDeadline))
}

func (r *DevSandboxReconciler) updateStatus(
	ctx context.Context, sandbox *devsandboxv1alpha1.DevSandbox, result ctrl.Result,
) (ctrl.Result, error) {
	var current devsandboxv1alpha1.DevSandbox
	if err := r.Get(ctx, objectKey(sandbox), &current); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if reflect.DeepEqual(current.Status, sandbox.Status) {
		r.refreshGauges(ctx)
		return result, nil
	}
	oldPhase := current.Status.Phase
	current.Status = sandbox.Status
	if err := r.Status().Update(ctx, &current); err != nil {
		return ctrl.Result{}, err
	}
	r.auditTransition(&current, oldPhase)
	r.refreshGauges(ctx)
	return result, nil
}

func (r *DevSandboxReconciler) auditTransition(sandbox *devsandboxv1alpha1.DevSandbox, oldPhase string) {
	if oldPhase == sandbox.Status.Phase {
		return
	}
	switch sandbox.Status.Phase {
	case PhaseRunning:
		event := "sandbox.ready"
		if oldPhase == PhaseStopped {
			event = "sandbox.resume"
		}
		r.audit(sandbox, event, "success", "")
		if r.Metrics != nil && event == "sandbox.ready" && !sandbox.CreationTimestamp.IsZero() {
			r.Metrics.LifecycleDuration.WithLabelValues("create").Observe(r.now().Sub(sandbox.CreationTimestamp.Time).Seconds())
		}
	case PhaseStopped:
		r.audit(sandbox, "sandbox.stop", "success", "")
	case PhaseFailed:
		r.audit(sandbox, "sandbox.failure", "failure", conditionReason(sandbox.Status.Conditions, ConditionReady))
	}
}

func (r *DevSandboxReconciler) audit(sandbox *devsandboxv1alpha1.DevSandbox, event, outcome, reason string) {
	if r.Audit == nil || sandbox == nil {
		return
	}
	repositoryID := sandbox.Spec.Source.RepositoryID
	if repositoryID == "" {
		repositoryID = sandbox.Spec.Source.RepositoryURL
	}
	r.Audit.Record(observability.AuditEvent{
		Event: event, Outcome: outcome, UserID: sandbox.Spec.Owner.GitHubUserID,
		Namespace: sandbox.Namespace, SandboxUID: string(sandbox.UID), SandboxName: sandbox.Name,
		Template: sandbox.Spec.Template.Name, Profile: string(sandbox.Spec.Profile.Name),
		Repository: repositoryID, CommitSHA: sandbox.Spec.Source.CommitSHA, Reason: reason,
	})
}

func (r *DevSandboxReconciler) refreshGauges(ctx context.Context) {
	if r.Metrics == nil {
		return
	}
	var sandboxes devsandboxv1alpha1.DevSandboxList
	if err := r.List(ctx, &sandboxes); err == nil {
		r.Metrics.ActiveSandboxes.Reset()
		counts := make(map[[3]string]float64)
		for i := range sandboxes.Items {
			item := &sandboxes.Items[i]
			state := item.Status.Phase
			if state == "" {
				state = PhasePending
			}
			counts[[3]string{item.Spec.Template.Name, string(item.Spec.Profile.Name), state}]++
		}
		for labels, count := range counts {
			r.Metrics.ActiveSandboxes.WithLabelValues(labels[0], labels[1], labels[2]).Set(count)
		}
	}
	var pvcs corev1.PersistentVolumeClaimList
	if err := r.List(ctx, &pvcs); err == nil {
		var count, gib float64
		for i := range pvcs.Items {
			if pvcs.Items[i].Labels[SandboxLabel] == "" {
				continue
			}
			count++
			size, err := storageGiB(pvcs.Items[i].Spec.Resources.Requests.Storage().String())
			if err == nil {
				gib += float64(size)
			}
		}
		r.Metrics.RetainedPVCs.Set(count)
		r.Metrics.ProvisionedGiB.Set(gib)
	}
}

func (r *DevSandboxReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func setCondition(
	conditions *[]metav1.Condition, conditionType string, status metav1.ConditionStatus,
	reason, message string, generation int64, now time.Time,
) {
	if reason == "" {
		reason = "Unknown"
	}
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type: conditionType, Status: status, Reason: reason, Message: message,
		ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(now.UTC()),
	})
	sort.SliceStable(*conditions, func(i, j int) bool { return (*conditions)[i].Type < (*conditions)[j].Type })
}

func expired(deadline *metav1.Time, now time.Time) bool {
	return deadline != nil && !now.Before(deadline.Time)
}

func shouldProjectActivity(sandbox *devsandboxv1alpha1.DevSandbox, activity, deadline time.Time) bool {
	if sandbox.Status.LastActivityTime == nil || sandbox.Status.IdleDeadline == nil {
		return true
	}
	expectedTimeout := time.Duration(sandbox.Spec.Lifecycle.IdleTimeoutSeconds) * time.Second
	reportedTimeout := sandbox.Status.IdleDeadline.Sub(sandbox.Status.LastActivityTime.Time)
	return activity.Sub(sandbox.Status.LastActivityTime.Time) >= ActivityStatusCoalesce ||
		reportedTimeout != expectedTimeout ||
		!deadline.Equal(activity.Add(expectedTimeout))
}

func requeueAt(now time.Time, deadline *metav1.Time) ctrl.Result {
	if deadline == nil {
		return ctrl.Result{RequeueAfter: DefaultRequeue}
	}
	wait := deadline.Sub(now)
	if wait <= 0 {
		wait = time.Second
	}
	return ctrl.Result{RequeueAfter: wait}
}

func reservationID(sandbox *devsandboxv1alpha1.DevSandbox) string {
	if sandbox.UID != "" {
		return string(sandbox.UID)
	}
	return sandbox.Namespace + "/" + sandbox.Name
}

func quotaNameForUser(userID string) string {
	value := strings.ToLower(userID)
	var result strings.Builder
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
			result.WriteRune(char)
		} else {
			result.WriteByte('-')
		}
	}
	cleaned := strings.Trim(result.String(), "-")
	if cleaned == "" {
		cleaned = "unknown"
	}
	if len(cleaned) > 40 {
		cleaned = cleaned[:40]
	}
	digest := sha256.Sum256([]byte(userID))
	return fmt.Sprintf("user-%s-%x", cleaned, digest[:6])
}

type clientQuotaStore struct {
	client.Client
}

func (s clientQuotaStore) Get(ctx context.Context, namespace, name string) (*devsandboxv1alpha1.DevSandboxUserQuota, error) {
	var quota devsandboxv1alpha1.DevSandboxUserQuota
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &quota); err != nil {
		return nil, err
	}
	return &quota, nil
}

func (s clientQuotaStore) UpdateStatus(ctx context.Context, quota *devsandboxv1alpha1.DevSandboxUserQuota) error {
	return s.Client.Status().Update(ctx, quota)
}

func conditionReason(conditions []metav1.Condition, conditionType string) string {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return conditions[i].Reason
		}
	}
	return ""
}
