package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
	"github.com/rmoreirao/AKSAgentSandbox/internal/gateway"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

type testStore struct {
	templates map[string]devsandboxv1alpha1.DevSandboxTemplate
	sandboxes map[string]devsandboxv1alpha1.DevSandbox
	jobs      map[string]devsandboxv1alpha1.DevSandboxJob
	mu        sync.Mutex
}

func (s *testStore) ListTemplates(context.Context) ([]devsandboxv1alpha1.DevSandboxTemplate, error) {
	var result []devsandboxv1alpha1.DevSandboxTemplate
	for _, value := range s.templates {
		result = append(result, value)
	}
	return result, nil
}
func (s *testStore) GetTemplate(_ context.Context, name string) (*devsandboxv1alpha1.DevSandboxTemplate, error) {
	value, ok := s.templates[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "devsandbox.io", Resource: "templates"}, name)
	}
	return value.DeepCopy(), nil
}
func (s *testStore) CreateSandbox(_ context.Context, value *devsandboxv1alpha1.DevSandbox) error {
	s.sandboxes[value.Name] = *value.DeepCopy()
	return nil
}
func (s *testStore) ListSandboxes(context.Context) ([]devsandboxv1alpha1.DevSandbox, error) {
	var result []devsandboxv1alpha1.DevSandbox
	for _, value := range s.sandboxes {
		result = append(result, value)
	}
	return result, nil
}
func (s *testStore) GetSandbox(_ context.Context, name string) (*devsandboxv1alpha1.DevSandbox, error) {
	value, ok := s.sandboxes[name]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "devsandbox.io", Resource: "sandboxes"}, name)
	}
	return value.DeepCopy(), nil
}
func (s *testStore) UpdateSandbox(_ context.Context, value *devsandboxv1alpha1.DevSandbox) error {
	s.sandboxes[value.Name] = *value.DeepCopy()
	return nil
}
func (s *testStore) DeleteSandbox(_ context.Context, value *devsandboxv1alpha1.DevSandbox) error {
	delete(s.sandboxes, value.Name)
	return nil
}
func (*testStore) ListEvents(context.Context, types.UID) ([]corev1.Event, error) {
	return nil, nil
}
func (s *testStore) CreateJob(_ context.Context, value *devsandboxv1alpha1.DevSandboxJob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs == nil {
		s.jobs = make(map[string]devsandboxv1alpha1.DevSandboxJob)
	}
	s.jobs[value.Name] = *value.DeepCopy()
	return nil
}
func (s *testStore) ListJobs(context.Context) ([]devsandboxv1alpha1.DevSandboxJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]devsandboxv1alpha1.DevSandboxJob, 0, len(s.jobs))
	for _, value := range s.jobs {
		result = append(result, *value.DeepCopy())
	}
	return result, nil
}
func (s *testStore) UpdateJobStatus(_ context.Context, name string, status devsandboxv1alpha1.DevSandboxJobStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.jobs[name]
	if !ok {
		return apierrors.NewNotFound(schema.GroupResource{Group: "devsandbox.io", Resource: "devsandboxjobs"}, name)
	}
	value.Status = status
	s.jobs[name] = value
	return nil
}
func (s *testStore) storedJob(name string) devsandboxv1alpha1.DevSandboxJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := s.jobs[name]
	return *value.DeepCopy()
}

type managedJobRouter struct{}

func (managedJobRouter) ServeRoute(writer http.ResponseWriter, request *http.Request, _ gateway.RouteTarget) {
	writer.Header().Set("Content-Type", "application/json")
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/v1/exec":
		writer.WriteHeader(http.StatusAccepted)
		_, _ = writer.Write([]byte(`{"id":"job-persisted","state":"Running","startedAt":"2026-09-02T16:00:00Z"}`))
	case request.Method == http.MethodGet && request.URL.Path == "/v1/jobs/job-persisted":
		_, _ = writer.Write([]byte(`{"id":"job-persisted","state":"Completed","startedAt":"2026-09-02T16:00:00Z","endedAt":"2026-09-02T16:00:01Z","exitCode":0}`))
	default:
		http.NotFound(writer, request)
	}
}

func testAPI(t *testing.T, owner string, store *testStore) (Handler, string) {
	t.Helper()
	sessions := auth.SessionManager{
		Signer:   auth.HMACSigner{Key: []byte("01234567890123456789012345678901")},
		Audience: "api", Lifetime: time.Hour,
	}
	token, _, err := sessions.Issue(context.Background(), auth.Identity{UserID: owner, Login: "owner", Org: "org"})
	if err != nil {
		t.Fatal(err)
	}
	return Handler{Sessions: sessions, Store: store}, token
}

func doAPI(handler Handler, token, method, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestOwnerIsolation(t *testing.T) {
	store := &testStore{sandboxes: map[string]devsandboxv1alpha1.DevSandbox{
		"mine": {ObjectMeta: metav1.ObjectMeta{Name: "mine"}, Spec: devsandboxv1alpha1.DevSandboxSpec{
			Owner: devsandboxv1alpha1.SandboxOwner{GitHubUserID: "owner"},
		}},
		"theirs": {ObjectMeta: metav1.ObjectMeta{Name: "theirs"}, Spec: devsandboxv1alpha1.DevSandboxSpec{
			Owner: devsandboxv1alpha1.SandboxOwner{GitHubUserID: "other"},
		}},
	}}
	handler, token := testAPI(t, "owner", store)
	if response := doAPI(handler, token, http.MethodGet, "/v1/sandboxes/mine"); response.Code != http.StatusOK {
		t.Fatalf("owner status = %d", response.Code)
	}
	response := doAPI(handler, token, http.MethodGet, "/v1/sandboxes/theirs")
	if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "other") {
		t.Fatalf("isolation response = %d %s", response.Code, response.Body.String())
	}
	list := doAPI(handler, token, http.MethodGet, "/v1/sandboxes")
	if strings.Contains(list.Body.String(), "theirs") {
		t.Fatalf("list leaked another owner's sandbox: %s", list.Body.String())
	}
}

func TestInvalidResumeState(t *testing.T) {
	store := &testStore{sandboxes: map[string]devsandboxv1alpha1.DevSandbox{
		"mine": {
			ObjectMeta: metav1.ObjectMeta{Name: "mine"},
			Spec: devsandboxv1alpha1.DevSandboxSpec{
				Owner: devsandboxv1alpha1.SandboxOwner{GitHubUserID: "owner"},
			},
			Status: devsandboxv1alpha1.DevSandboxStatus{Phase: "Running"},
		},
	}}
	handler, token := testAPI(t, "owner", store)
	response := doAPI(handler, token, http.MethodPost, "/v1/sandboxes/mine/resume")
	var body Error
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if response.Code != http.StatusConflict || body.Code != "invalid_state" {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

func TestVSCodeCredentialOnlyInFragment(t *testing.T) {
	key := auth.HMACSigner{Key: []byte("01234567890123456789012345678901")}
	template := devsandboxv1alpha1.DevSandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "vscode"},
		Spec:       devsandboxv1alpha1.DevSandboxTemplateSpec{Capabilities: devsandboxv1alpha1.TemplateCapabilities{VSCode: true}},
	}

	sandbox := devsandboxv1alpha1.DevSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "mine", UID: "uid"},
		Spec: devsandboxv1alpha1.DevSandboxSpec{
			Owner:    devsandboxv1alpha1.SandboxOwner{GitHubUserID: "owner"},
			Template: devsandboxv1alpha1.SandboxTemplateReference{Name: "vscode"},
		},
		Status: devsandboxv1alpha1.DevSandboxStatus{Phase: "Running", UpstreamSandboxName: "upstream"},
	}
	store := &testStore{
		templates: map[string]devsandboxv1alpha1.DevSandboxTemplate{"vscode": template},
		sandboxes: map[string]devsandboxv1alpha1.DevSandbox{"mine": sandbox},
	}
	handler, token := testAPI(t, "owner", store)
	handler.Claims = gateway.ClaimManager{Signer: key}
	handler.Credentials = &gateway.MemoryCredentialStore{}
	handler.WebBaseURL = "https://sandbox.example"
	response := doAPI(handler, token, http.MethodPost, "/v1/sandboxes/mine/vscode-url")
	var body struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if response.Code != http.StatusOK || !strings.Contains(body.URL, "/bootstrap#credential=") ||
		strings.Contains(strings.Split(body.URL, "#")[0], "credential") {
		t.Fatalf("unsafe URL response = %d %s", response.Code, response.Body.String())
	}
	credential := strings.TrimPrefix(strings.SplitN(body.URL, "#", 2)[1], "credential=")
	claimValue, err := handler.Credentials.Consume(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := handler.Claims.Verify(context.Background(), claimValue)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Port != 13337 {
		t.Fatalf("VS Code route port = %d, want 13337", claim.Port)
	}
}
