package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	"github.com/rmoreirao/AKSAgentSandbox/internal/repository"
)

type testAccessTokens struct {
	token string
	err   error
}

func (p testAccessTokens) AccessToken(context.Context, string) (string, time.Time, error) {
	return p.token, time.Now().Add(time.Hour), p.err
}

type testRepositoryResolver struct {
	value         repository.RemoteRepository
	err           error
	token         string
	ownerBoundary string
}

func (r *testRepositoryResolver) ResolveRepository(
	_ context.Context, token, _, _, ownerBoundary string,
) (repository.RemoteRepository, error) {
	r.token = token
	r.ownerBoundary = ownerBoundary
	return r.value, r.err
}

func TestRepositoryResolutionPersonalModeUsesSessionLogin(t *testing.T) {
	handler, session := testAPI(t, "owner", &testStore{
		templates: map[string]devsandboxv1alpha1.DevSandboxTemplate{},
		sandboxes: map[string]devsandboxv1alpha1.DevSandbox{},
	})
	resolver := &testRepositoryResolver{value: repository.RemoteRepository{
		ID: "owner/repo", URL: "https://github.com/owner/repo.git",
		CommitSHA: "1111111111111111111111111111111111111111",
	}}
	handler.RepositoryResolver = resolver
	handler.AccessTokens = testAccessTokens{token: "managed-github-token"}
	response := doAPI(handler, session, http.MethodGet, "/v1/repositories/resolve?repository=owner%2Frepo")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if resolver.ownerBoundary != "owner" {
		t.Fatalf("owner boundary = %q", resolver.ownerBoundary)
	}
}

func TestRepositoryResolutionUsesManagedCredential(t *testing.T) {
	handler, session := testAPI(t, "owner", &testStore{
		templates: map[string]devsandboxv1alpha1.DevSandboxTemplate{},
		sandboxes: map[string]devsandboxv1alpha1.DevSandbox{},
	})
	resolver := &testRepositoryResolver{value: repository.RemoteRepository{
		ID: "external/public", URL: "https://github.com/external/public.git",
		CommitSHA: "1111111111111111111111111111111111111111", ReadOnly: true,
	}}
	handler.RepositoryResolver = resolver
	handler.AccessTokens = testAccessTokens{token: "managed-github-token"}
	handler.PrimaryOrg = "primary"
	response := doAPI(handler, session, http.MethodGet, "/v1/repositories/resolve?repository=external%2Fpublic")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if resolver.token != "managed-github-token" {
		t.Fatalf("resolver token = %q", resolver.token)
	}
	var body repository.RemoteRepository
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || !body.ReadOnly {
		t.Fatalf("body=%#v error=%v", body, err)
	}
}

func TestRepositoryResolutionRejectsExternalPrivate(t *testing.T) {
	handler, session := testAPI(t, "owner", &testStore{
		templates: map[string]devsandboxv1alpha1.DevSandboxTemplate{},
		sandboxes: map[string]devsandboxv1alpha1.DevSandbox{},
	})
	handler.RepositoryResolver = &testRepositoryResolver{err: repository.ErrExternalPrivate}
	handler.AccessTokens = testAccessTokens{token: "managed-github-token"}
	handler.PrimaryOrg = "primary"
	response := doAPI(handler, session, http.MethodGet, "/v1/repositories/resolve?repository=external%2Fprivate")
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
