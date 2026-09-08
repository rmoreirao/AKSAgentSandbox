package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const defaultJobPollInterval = 5 * time.Second

type agentJob struct {
	ID        string     `json:"id"`
	State     string     `json:"state"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	ExitCode  *int       `json:"exitCode,omitempty"`
}

func (h Handler) execRequest(writer http.ResponseWriter, request *http.Request, sandbox *devsandboxv1alpha1.DevSandbox) {
	if request.Method != http.MethodPost {
		methodError(writer, http.MethodPost)
		return
	}
	data, err := io.ReadAll(io.LimitReader(request.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "request body is invalid", nil)
		return
	}
	request.Body = io.NopCloser(bytes.NewReader(data))
	var envelope struct {
		Detach bool `json:"detach"`
	}
	if json.Unmarshal(data, &envelope) != nil || !envelope.Detach {
		h.agentHTTP(writer, request, sandbox, http.MethodPost, "/v1/exec")
		return
	}
	h.startManagedJob(writer, request, sandbox)
}

func (h Handler) startManagedJob(writer http.ResponseWriter, request *http.Request, sandbox *devsandboxv1alpha1.DevSandbox) {
	store, ok := h.Store.(JobStore)
	if !ok {
		writeAPIError(writer, http.StatusServiceUnavailable, "job_store_unavailable", "managed job storage is unavailable", nil)
		return
	}
	response := httptest.NewRecorder()
	h.agentHTTP(response, request, sandbox, http.MethodPost, "/v1/exec")
	if response.Code < 200 || response.Code >= 300 {
		copyRecordedResponse(writer, response)
		return
	}
	var summary agentJob
	if json.Unmarshal(response.Body.Bytes(), &summary) != nil || summary.ID == "" {
		writeAPIError(writer, http.StatusBadGateway, "invalid_agent_response", "sandbox returned an invalid job summary", nil)
		return
	}
	resource := jobResource(sandbox, summary)
	if err := store.CreateJob(request.Context(), resource); err != nil {
		h.stopUnpersistedJob(sandbox, summary.ID)
		storeError(writer, err)
		return
	}
	h.auditSandbox("job.start", "success", sandbox, "job")
	go h.monitorJob(context.Background(), sandbox.DeepCopy(), summary.ID)
	copyRecordedResponse(writer, response)
}

func (h Handler) stopManagedJob(writer http.ResponseWriter, request *http.Request, sandbox *devsandboxv1alpha1.DevSandbox, jobID string) {
	response := httptest.NewRecorder()
	h.agentHTTP(response, request, sandbox, http.MethodDelete, "/v1/jobs/"+url.PathEscape(jobID))
	if response.Code >= 200 && response.Code < 300 {
		var summary agentJob
		if json.Unmarshal(response.Body.Bytes(), &summary) == nil {
			if store, ok := h.Store.(JobStore); ok {
				_ = store.UpdateJobStatus(request.Context(), summary.ID, jobStatus(summary))
			}
		}
		h.auditSandbox("job.stop", "success", sandbox, "job")
	}
	copyRecordedResponse(writer, response)
}

func (h Handler) stopUnpersistedJob(sandbox *devsandboxv1alpha1.DevSandbox, jobID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodDelete, "/v1/jobs/"+url.PathEscape(jobID), nil)
	h.Router.ServeRoute(ioDiscardResponse{}, request, h.routeTarget(sandbox, h.agentPort(), ""))
}

func (h Handler) shutdownAgent(sandbox *devsandboxv1alpha1.DevSandbox) {
	if h.Router == nil || requireRunning(sandbox) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/shutdown", nil)
	if err != nil {
		return
	}
	response := httptest.NewRecorder()
	h.Router.ServeRoute(response, request, h.routeTarget(sandbox, h.agentPort(), ""))
}

func (h Handler) monitorJob(ctx context.Context, sandbox *devsandboxv1alpha1.DevSandbox, jobID string) {
	store, ok := h.Store.(JobStore)
	if !ok {
		return
	}
	for {
		summary, err := h.fetchAgentJob(ctx, sandbox, jobID)
		if err == nil {
			if updateErr := store.UpdateJobStatus(ctx, jobID, jobStatus(summary)); updateErr == nil && jobTerminal(summary.State) {
				h.renewActivity(ctx, sandbox, "managed-job")
				h.auditSandbox("job.finish", "success", sandbox, "job")
				return
			}
		} else {
			current, getErr := h.Store.GetSandbox(ctx, sandbox.Name)
			if getErr != nil || current.Spec.DesiredState == devsandboxv1alpha1.DesiredStateStopped ||
				current.Status.Phase != "Running" {
				return
			}
		}
		timer := time.NewTimer(defaultJobPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (h Handler) fetchAgentJob(ctx context.Context, sandbox *devsandboxv1alpha1.DevSandbox, jobID string) (agentJob, error) {
	var summary agentJob
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "/v1/jobs/"+url.PathEscape(jobID), nil)
	if err != nil {
		return summary, err
	}
	response := httptest.NewRecorder()
	h.Router.ServeRoute(response, request, h.routeTarget(sandbox, h.agentPort(), ""))
	if response.Code < 200 || response.Code >= 300 {
		return summary, errors.New("agent job query failed")
	}
	if json.Unmarshal(response.Body.Bytes(), &summary) != nil || summary.ID != jobID {
		return summary, errors.New("invalid agent job summary")
	}
	return summary, nil
}

// RecoverManagedJobs resumes status polling after a management API restart.
func (h Handler) RecoverManagedJobs(ctx context.Context) error {
	store, ok := h.Store.(JobStore)
	if !ok {
		return nil
	}
	jobs, err := store.ListJobs(ctx)
	if err != nil {
		return err
	}
	sandboxes, err := h.Store.ListSandboxes(ctx)
	if err != nil {
		return err
	}
	byName := make(map[string]*devsandboxv1alpha1.DevSandbox, len(sandboxes))
	for i := range sandboxes {
		byName[sandboxes[i].Name] = sandboxes[i].DeepCopy()
	}
	for i := range jobs {
		if jobs[i].Status.State != devsandboxv1alpha1.JobStatePending &&
			jobs[i].Status.State != devsandboxv1alpha1.JobStateRunning {
			continue
		}
		if sandbox := byName[jobs[i].Spec.SandboxName]; sandbox != nil && requireRunning(sandbox) == nil {
			go h.monitorJob(ctx, sandbox, jobs[i].Name)
		}
	}
	return nil
}

func jobResource(sandbox *devsandboxv1alpha1.DevSandbox, summary agentJob) *devsandboxv1alpha1.DevSandboxJob {
	return &devsandboxv1alpha1.DevSandboxJob{
		TypeMeta: metav1.TypeMeta{APIVersion: devsandboxv1alpha1.GroupVersion.String(), Kind: "DevSandboxJob"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      summary.ID,
			Namespace: sandbox.Namespace,
			Labels: map[string]string{
				"devsandbox.io/sandbox-name": sandbox.Name,
				"devsandbox.io/sandbox-uid":  string(sandbox.UID),
			},
		},
		Spec: devsandboxv1alpha1.DevSandboxJobSpec{
			SandboxName: sandbox.Name, GitHubUserID: sandbox.Spec.Owner.GitHubUserID,
		},
		Status: jobStatus(summary),
	}
}

func jobStatus(summary agentJob) devsandboxv1alpha1.DevSandboxJobStatus {
	status := devsandboxv1alpha1.DevSandboxJobStatus{State: devsandboxv1alpha1.DevSandboxJobState(summary.State)}
	if !summary.StartedAt.IsZero() {
		status.StartedAt = lifecycleTime(summary.StartedAt)
	}
	if summary.EndedAt != nil {
		status.CompletedAt = lifecycleTime(*summary.EndedAt)
	}
	if summary.ExitCode != nil {
		value := int32(*summary.ExitCode)
		status.ExitCode = &value
	}
	return status
}

func lifecycleTime(value time.Time) *metav1.Time {
	result := metav1.NewTime(value.UTC())
	return &result
}

func jobTerminal(state string) bool {
	switch devsandboxv1alpha1.DevSandboxJobState(state) {
	case devsandboxv1alpha1.JobStateCompleted, devsandboxv1alpha1.JobStateFailed, devsandboxv1alpha1.JobStateStopped:
		return true
	default:
		return false
	}
}

func copyRecordedResponse(writer http.ResponseWriter, response *httptest.ResponseRecorder) {
	for name, values := range response.Header() {
		for _, value := range values {
			writer.Header().Add(name, value)
		}
	}
	writer.WriteHeader(response.Code)
	_, _ = writer.Write(response.Body.Bytes())
}

type ioDiscardResponse struct{}

func (ioDiscardResponse) Header() http.Header            { return make(http.Header) }
func (ioDiscardResponse) Write(data []byte) (int, error) { return len(data), nil }
func (ioDiscardResponse) WriteHeader(int)                {}
