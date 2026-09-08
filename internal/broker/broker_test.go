package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeReviewer struct {
	result TokenReviewResult
	err    error
	token  string
	aud    string
}

func (r *fakeReviewer) Review(_ context.Context, token, audience string) (TokenReviewResult, error) {
	r.token, r.aud = token, audience
	return r.result, r.err
}

type fakeBindings struct {
	accounts  map[string]ServiceAccountBinding
	pods      map[string]PodBinding
	sandboxes map[string]SandboxBinding
}

func (b fakeBindings) GetServiceAccount(_ context.Context, namespace, name string) (ServiceAccountBinding, error) {
	value, ok := b.accounts[namespace+"/"+name]
	if !ok {
		return ServiceAccountBinding{}, errors.New("service account not found")
	}
	return value, nil
}

func (b fakeBindings) GetPod(_ context.Context, namespace, name string) (PodBinding, error) {
	value, ok := b.pods[namespace+"/"+name]
	if !ok {
		return PodBinding{}, errors.New("pod not found")
	}
	return value, nil
}

func (b fakeBindings) GetDevSandbox(_ context.Context, namespace, name string) (SandboxBinding, error) {
	value, ok := b.sandboxes[namespace+"/"+name]
	if !ok {
		return SandboxBinding{}, errors.New("sandbox not found")
	}
	return value, nil
}

type fakeCredentials struct {
	userID string
	token  string
	err    error
}

func (c *fakeCredentials) AccessToken(_ context.Context, userID string) (string, time.Time, error) {
	c.userID = userID
	return c.token, time.Now().Add(time.Hour), c.err
}

func validFixture() (*Service, *fakeReviewer, *fakeCredentials) {
	reviewer := &fakeReviewer{result: TokenReviewResult{
		Authenticated: true, Audiences: []string{DefaultAudience},
		Username: "system:serviceaccount:workloads:sa-a", ServiceAccountUID: "sa-uid-a",
		Extra: map[string][]string{
			"authentication.kubernetes.io/pod-name": {"pod-a"},
			"authentication.kubernetes.io/pod-uid":  {"pod-uid-a"},
		},
	}}
	labelsA := map[string]string{SandboxNameLabel: "sandbox-a", SandboxUIDLabel: "sandbox-uid-a"}
	bindings := fakeBindings{
		accounts: map[string]ServiceAccountBinding{
			"workloads/sa-a": {UID: "sa-uid-a", Labels: labelsA},
			"workloads/sa-b": {UID: "sa-uid-b", Labels: map[string]string{SandboxNameLabel: "sandbox-b", SandboxUIDLabel: "sandbox-uid-b"}},
		},
		pods: map[string]PodBinding{
			"workloads/pod-a": {UID: "pod-uid-a", ServiceAccountName: "sa-a", Labels: labelsA},
			"workloads/pod-b": {UID: "pod-uid-b", ServiceAccountName: "sa-b", Labels: map[string]string{SandboxNameLabel: "sandbox-b", SandboxUIDLabel: "sandbox-uid-b"}},
		},
		sandboxes: map[string]SandboxBinding{
			"workloads/sandbox-a": {UID: "sandbox-uid-a", OwnerUserID: "owner-a"},
			"workloads/sandbox-b": {UID: "sandbox-uid-b", OwnerUserID: "owner-b"},
		},
	}
	credentials := &fakeCredentials{token: "access-for-a"}
	return &Service{Reviewer: reviewer, Bindings: bindings, Credentials: credentials}, reviewer, credentials
}

func TestBrokerBindsAllProjectedIdentityFields(t *testing.T) {
	service, reviewer, credentials := validFixture()
	token, _, identity, err := service.Credential(context.Background(), "projected")
	if err != nil {
		t.Fatal(err)
	}
	if token != "access-for-a" || credentials.userID != "owner-a" {
		t.Fatalf("credential=%q user=%q", token, credentials.userID)
	}
	if reviewer.aud != DefaultAudience || identity.ServiceAccountUID != "sa-uid-a" ||
		identity.PodUID != "pod-uid-a" || identity.SandboxUID != "sandbox-uid-a" {
		t.Fatalf("incomplete binding: %#v", identity)
	}

	tests := []struct {
		name   string
		mutate func(*fakeReviewer)
	}{
		{"wrong audience", func(r *fakeReviewer) { r.result.Audiences = []string{"other"} }},
		{"multiple audiences", func(r *fakeReviewer) { r.result.Audiences = []string{DefaultAudience, "other"} }},
		{"service account UID", func(r *fakeReviewer) { r.result.ServiceAccountUID = "wrong" }},
		{"pod UID", func(r *fakeReviewer) { r.result.Extra["authentication.kubernetes.io/pod-uid"] = []string{"wrong"} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate, r, _ := validFixture()
			test.mutate(r)
			_, _, _, err := candidate.Credential(context.Background(), "projected")
			if err == nil {
				t.Fatal("expected binding rejection")
			}
		})
	}
}

func TestSandboxAIdentityCannotObtainSandboxBToken(t *testing.T) {
	service, reviewer, credentials := validFixture()
	reviewer.result.Extra["authentication.kubernetes.io/pod-name"] = []string{"pod-b"}
	reviewer.result.Extra["authentication.kubernetes.io/pod-uid"] = []string{"pod-uid-b"}
	_, _, _, err := service.Credential(context.Background(), "projected-a")
	if !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("cross-sandbox error = %v", err)
	}
	if credentials.userID != "" {
		t.Fatalf("credential lookup reached owner %q", credentials.userID)
	}
}

func TestBrokerHandlerIgnoresCallerOwnerInput(t *testing.T) {
	service, _, credentials := validFixture()
	handler := Handler{Service: service}
	request := httptest.NewRequest(http.MethodPost, "/v1/credential", strings.NewReader(`{"owner":"owner-b","sandbox":"sandbox-b"}`))
	request.Header.Set("Authorization", "Bearer projected")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["token"] != "access-for-a" || credentials.userID != "owner-a" {
		t.Fatalf("caller influenced owner: body=%v owner=%q", body, credentials.userID)
	}
}
