package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	"github.com/rmoreirao/AKSAgentSandbox/internal/gateway"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type shutdownRouter struct {
	called bool
}

func (r *shutdownRouter) ServeRoute(writer http.ResponseWriter, request *http.Request, _ gateway.RouteTarget) {
	if request.Method == http.MethodPost && request.URL.Path == "/v1/shutdown" {
		r.called = true
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	http.NotFound(writer, request)
}

func TestDetachedJobSummaryPersistsAndCompletesAfterClientDisconnect(t *testing.T) {
	sandbox := devsandboxv1alpha1.DevSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: "workloads", UID: "uid"},
		Spec: devsandboxv1alpha1.DevSandboxSpec{
			Owner: devsandboxv1alpha1.SandboxOwner{GitHubUserID: "owner"},
		},
		Status: devsandboxv1alpha1.DevSandboxStatus{Phase: "Running", UpstreamSandboxName: "upstream"},
	}
	store := &testStore{
		sandboxes: map[string]devsandboxv1alpha1.DevSandbox{"mine": sandbox},
		jobs:      make(map[string]devsandboxv1alpha1.DevSandboxJob),
	}
	handler, token := testAPI(t, "owner", store)
	handler.Router = managedJobRouter{}
	body := bytes.NewBufferString(`{"command":{"executable":"ignored-secret","arguments":["not-persisted"]},"detach":true}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/mine/exec", body)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for store.storedJob("job-persisted").Status.State != devsandboxv1alpha1.JobStateCompleted && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	job := store.storedJob("job-persisted")
	if job.Spec.SandboxName != "mine" || job.Spec.GitHubUserID != "owner" ||
		job.Status.State != devsandboxv1alpha1.JobStateCompleted ||
		job.Status.ExitCode == nil || *job.Status.ExitCode != 0 {
		t.Fatalf("persisted job = %#v", job)
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("ignored-secret")) || bytes.Contains(encoded, []byte("not-persisted")) {
		t.Fatalf("job persisted command details: %s", encoded)
	}
}

func TestStopSignalsAgentToCloseConnections(t *testing.T) {
	sandbox := devsandboxv1alpha1.DevSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: "workloads", UID: "uid"},
		Spec: devsandboxv1alpha1.DevSandboxSpec{
			Owner:        devsandboxv1alpha1.SandboxOwner{GitHubUserID: "owner"},
			DesiredState: devsandboxv1alpha1.DesiredStateRunning,
		},
		Status: devsandboxv1alpha1.DevSandboxStatus{Phase: "Running", UpstreamSandboxName: "upstream"},
	}
	store := &testStore{sandboxes: map[string]devsandboxv1alpha1.DevSandbox{"mine": sandbox}}
	handler, token := testAPI(t, "owner", store)
	router := &shutdownRouter{}
	handler.Router = router
	response := doAPI(handler, token, http.MethodPost, "/v1/sandboxes/mine/stop")
	if response.Code != http.StatusAccepted || !router.called {
		t.Fatalf("status=%d shutdownCalled=%v body=%s", response.Code, router.called, response.Body.String())
	}
}
