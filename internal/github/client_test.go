package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
	"github.com/rmoreirao/AKSAgentSandbox/internal/repository"
)

func TestClientDeviceFlowIdentityMembershipAndRefreshErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/device":
			_, _ = writer.Write([]byte(`{"device_code":"device-secret","user_code":"ABCD","verification_uri":"https://github.example/device","expires_in":600,"interval":5}`))
		case "/token":
			_ = request.ParseForm()
			switch request.Form.Get("grant_type") {
			case "refresh_token":
				if request.Form.Get("refresh_token") == "invalid" {
					_, _ = writer.Write([]byte(`{"error":"invalid_grant"}`))
					return
				}
				_, _ = writer.Write([]byte(`{"access_token":"new-access","expires_in":3600,"refresh_token":"rotated","refresh_token_expires_in":7200}`))
			default:
				_, _ = writer.Write([]byte(`{"error":"authorization_pending"}`))
			}
		case "/user":
			if request.Header.Get("Authorization") != "Bearer access" {
				t.Errorf("authorization header = %q", request.Header.Get("Authorization"))
			}
			_, _ = writer.Write([]byte(`{"id":987654321,"login":"renameable"}`))
		case "/user/memberships/orgs/primary":
			_, _ = writer.Write([]byte(`{"state":"active"}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	client := &Client{
		ClientID: "app-id", APIURL: server.URL, DeviceURL: server.URL + "/device",
		TokenURL: server.URL + "/token", HTTPClient: server.Client(),
	}

	start, err := client.StartDeviceFlow(context.Background())
	if err != nil || start.DeviceCode != "device-secret" {
		t.Fatalf("StartDeviceFlow() = %#v, %v", start, err)
	}
	if _, err := client.PollDeviceFlow(context.Background(), "device-secret"); !errors.Is(err, auth.ErrAuthorizationPending) {
		t.Fatalf("pending error = %v", err)
	}
	user, err := client.CurrentUser(context.Background(), "access")
	if err != nil || user.ID != "987654321" {
		t.Fatalf("CurrentUser() = %#v, %v", user, err)
	}
	active, err := client.IsActiveOrgMember(context.Background(), "access", "primary")
	if err != nil || !active {
		t.Fatalf("IsActiveOrgMember() = %v, %v", active, err)
	}
	if _, err := client.Refresh(context.Background(), "invalid"); !errors.Is(err, auth.ErrInvalidGrant) {
		t.Fatalf("invalid grant error = %v", err)
	}
	result, err := client.Refresh(context.Background(), "valid")
	if err != nil || result.AccessToken != "new-access" || result.RefreshToken != "rotated" {
		t.Fatalf("Refresh() = %#v, %v", result, err)
	}
}

func TestClientPropagatesGitHubAPIErrorWithoutResponseBody(t *testing.T) {
	const secretBody = "must-not-appear"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, secretBody, http.StatusInternalServerError)
	}))
	defer server.Close()
	client := &Client{ClientID: "id", DeviceURL: server.URL, HTTPClient: server.Client()}
	_, err := client.StartDeviceFlow(context.Background())
	if err == nil || strings.Contains(err.Error(), secretBody) {
		t.Fatalf("unsafe GitHub error = %v", err)
	}
}

func TestResolveRepositoryRejectsExternalPrivate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path != "/repos/external/private" {
			t.Fatalf("unexpected request %s", request.URL.Path)
		}
		_, _ = writer.Write([]byte(`{
				"id":123,"full_name":"external/private","private":true,
				"default_branch":"main","owner":{"login":"external"}
			}`))
	}))
	defer server.Close()
	client := &Client{APIURL: server.URL, HTTPClient: server.Client()}
	_, err := client.ResolveRepository(context.Background(), "access", "external/private", "", "primary")
	if !errors.Is(err, repository.ErrExternalPrivate) {
		t.Fatalf("error = %v, want ErrExternalPrivate", err)
	}
}

func TestResolveRepositoryMarksExternalPublicReadOnly(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/external/public":
			_, _ = writer.Write([]byte(`{
					"id":123,"full_name":"External/Public","private":false,
					"default_branch":"main","owner":{"login":"External"}
				}`))
		case "/repos/external/public/git/ref/heads/main":
			_, _ = writer.Write([]byte(`{"object":{"sha":"` + sha + `"}}`))
		case "/user":
			_, _ = writer.Write([]byte(`{"id":42,"login":"octocat","name":"","email":null}`))
		default:
			t.Fatalf("unexpected request %s", request.URL.Path)
		}
	}))
	defer server.Close()
	client := &Client{APIURL: server.URL, HTTPClient: server.Client()}
	value, err := client.ResolveRepository(context.Background(), "access", "external/public", "", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if !value.ReadOnly || value.Private || value.CommitSHA != sha || value.RefName != "main" {
		t.Fatalf("unexpected repository: %#v", value)
	}
	if value.AuthorName != "octocat" || value.AuthorEmail != "42+octocat@users.noreply.github.com" {
		t.Fatalf("unexpected fallback author: %#v", value)
	}
}
