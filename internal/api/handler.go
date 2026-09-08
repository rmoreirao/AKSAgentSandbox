package api

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	devsandboxv1alpha1 "github.com/rmoreirao/AKSAgentSandbox/api/v1alpha1"
	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
	"github.com/rmoreirao/AKSAgentSandbox/internal/gateway"
	"github.com/rmoreirao/AKSAgentSandbox/internal/lifecycle"
	"github.com/rmoreirao/AKSAgentSandbox/internal/observability"
	"github.com/rmoreirao/AKSAgentSandbox/internal/repository"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var dnsName = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$`)

type Handler struct {
	Auth               AuthHandler
	Sessions           auth.SessionManager
	Store              Store
	Router             gateway.HTTPRouter
	WebSockets         gateway.WebSocketRouter
	Claims             gateway.ClaimManager
	Credentials        gateway.CredentialStore
	WebBaseURL         string
	WorkloadNamespace  string
	AgentPort          int
	VSCodePort         int
	CredentialTTL      time.Duration
	Activity           ActivityRenewer
	RepositoryResolver repository.RemoteResolver
	AccessTokens       interface {
		AccessToken(context.Context, string) (string, time.Time, error)
	}
	PrimaryOrg string
	Audit      observability.AuditSink
	Metrics    *observability.Metrics
}

func (h Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Cache-Control", "no-store")
	if request.URL.Path == "/healthz" || request.URL.Path == "/readyz" {
		if request.Method != http.MethodGet {
			writeAPIError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", nil)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if request.URL.Path == "/v1/auth/device/start" || request.URL.Path == "/v1/auth/device/poll" ||
		request.URL.Path == "/v1/auth/static/bootstrap" {
		h.Auth.ServeHTTP(writer, request)
		return
	}
	sessionMiddleware(h.Sessions, http.HandlerFunc(h.authenticated)).ServeHTTP(writer, request)
}

func (h Handler) authenticated(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/v1/me" {
		h.me(writer, request)
		return
	}
	if h.Store == nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "service_unavailable", "management service is unavailable", nil)
		return
	}
	segments := splitPath(request.URL.Path)
	switch {
	case len(segments) == 2 && segments[0] == "v1" && segments[1] == "templates":
		h.templates(writer, request)
	case len(segments) == 3 && segments[0] == "v1" && segments[1] == "templates":
		h.template(writer, request, segments[2])
	case len(segments) == 2 && segments[0] == "v1" && segments[1] == "sandboxes":
		h.sandboxes(writer, request)
	case len(segments) == 3 && segments[0] == "v1" && segments[1] == "repositories" && segments[2] == "resolve":
		h.resolveRepository(writer, request)
	case len(segments) >= 3 && segments[0] == "v1" && segments[1] == "sandboxes":
		h.sandboxRoute(writer, request, segments[2:])
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "endpoint not found", nil)
	}
}

func (h Handler) resolveRepository(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodError(writer, http.MethodGet)
		return
	}
	if h.RepositoryResolver == nil || h.AccessTokens == nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "repository_resolution_unavailable",
			"repository resolution service is unavailable", nil)
		return
	}
	id, _, err := repository.CanonicalGitHubURL(request.URL.Query().Get("repository"))
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_repository",
			"repository must be OWNER/REPOSITORY or a GitHub URL", nil)
		return
	}
	token, _, err := h.AccessTokens.AccessToken(request.Context(), requestClaims(request).Subject)
	if err != nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "repository_resolution_unavailable",
			"repository access could not be validated", nil)
		return
	}
	ownerBoundary := h.PrimaryOrg
	if ownerBoundary == "" {
		ownerBoundary = requestClaims(request).Login
	}
	value, err := h.RepositoryResolver.ResolveRepository(
		request.Context(), token, id, request.URL.Query().Get("ref"), ownerBoundary,
	)
	switch {
	case errors.Is(err, repository.ErrRepositoryNotFound):
		writeAPIError(writer, http.StatusNotFound, "repository_not_found", err.Error(), nil)
	case errors.Is(err, repository.ErrExternalPrivate):
		writeAPIError(writer, http.StatusForbidden, "external_private_repository", err.Error(), nil)
	case err != nil:
		writeAPIError(writer, http.StatusBadGateway, "repository_resolution_failed",
			"GitHub repository resolution failed", nil)
	default:
		writeJSON(writer, http.StatusOK, value)
	}
}

func (h Handler) me(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodError(writer, http.MethodGet)
		return
	}
	claims := requestClaims(request)
	writeJSON(writer, http.StatusOK, User{GitHubUserID: claims.Subject, GitHubLogin: claims.Login})
}

func (h Handler) templates(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodError(writer, http.MethodGet)
		return
	}
	values, err := h.Store.ListTemplates(request.Context())
	if err != nil {
		storeError(writer, err)
		return
	}
	items := make([]Template, 0, len(values))
	for _, value := range values {
		items = append(items, templateView(value))
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{"items": items})
}

func (h Handler) template(writer http.ResponseWriter, request *http.Request, name string) {
	if request.Method != http.MethodGet {
		methodError(writer, http.MethodGet)
		return
	}
	value, err := h.Store.GetTemplate(request.Context(), name)
	if err != nil {
		storeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, templateView(*value))
}

func (h Handler) sandboxes(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		values, err := h.Store.ListSandboxes(request.Context())
		if err != nil {
			storeError(writer, err)
			return
		}
		owner := requestClaims(request).Subject
		items := make([]Sandbox, 0, len(values))
		for _, value := range values {
			if value.Spec.Owner.GitHubUserID == owner {
				items = append(items, sandboxView(value))
			}
		}
		writeJSON(writer, http.StatusOK, map[string]interface{}{"items": items})
	case http.MethodPost:
		h.createSandbox(writer, request)
	default:
		methodError(writer, http.MethodGet+", "+http.MethodPost)
	}
}

func (h Handler) createSandbox(writer http.ResponseWriter, request *http.Request) {
	started := time.Now()
	var input CreateSandboxRequest
	if err := decodeJSON(request.Body, &input); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "request body is invalid", nil)
		return
	}
	if input.Template == "" || (input.ResumeExisting && input.CreateNew) {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request", "template is required and create options must not conflict", nil)
		return
	}
	template, err := h.Store.GetTemplate(request.Context(), input.Template)
	if err != nil {
		storeError(writer, err)
		return
	}
	profileName := input.Profile
	if profileName == "" {
		profileName = template.Spec.DefaultProfile
	}
	profile, err := lifecycle.CanonicalProfile(profileName)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_profile", "profile must be small, medium, or large", nil)
		return
	}
	name := strings.ToLower(input.Name)
	if name == "" {
		name = generatedSandboxName(requestClaims(request).Login)
	}
	if !dnsName.MatchString(name) {
		writeAPIError(writer, http.StatusBadRequest, "invalid_name", "sandbox name must be a valid DNS label", nil)
		return
	}
	claims := requestClaims(request)
	value := &devsandboxv1alpha1.DevSandbox{
		TypeMeta:   metav1.TypeMeta{APIVersion: devsandboxv1alpha1.GroupVersion.String(), Kind: "DevSandbox"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: devsandboxv1alpha1.DevSandboxSpec{
			Owner:  devsandboxv1alpha1.SandboxOwner{GitHubUserID: claims.Subject, GitHubLogin: claims.Login},
			Source: input.Source,
			Template: devsandboxv1alpha1.SandboxTemplateReference{
				Name: template.Name, Version: template.Spec.Version, ImageDigest: template.Spec.Image.Digest,
			},
			Profile: profile, Lifecycle: devsandboxv1alpha1.SandboxLifecycle{IdleTimeoutSeconds: input.IdleTimeoutSeconds},
			DesiredState: devsandboxv1alpha1.DesiredStateRunning,
		},
	}
	lifecycle.DefaultDevSandbox(value)
	if err := lifecycle.ValidateDevSandbox(value); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_sandbox", "sandbox configuration is invalid", err.Error())
		return
	}
	if err := h.Store.CreateSandbox(request.Context(), value); err != nil {
		if apierrors.IsAlreadyExists(err) {
			writeAPIError(writer, http.StatusConflict, "sandbox_exists", "sandbox already exists", nil)
			return
		}
		storeError(writer, err)
		return
	}
	if h.Metrics != nil {
		h.Metrics.LifecycleDuration.WithLabelValues("create").Observe(time.Since(started).Seconds())
	}
	h.auditSandbox("sandbox.create", "success", value, "")
	h.auditSandbox("template.selected", "success", value, "")
	h.auditSandbox("profile.selected", "success", value, "")
	if value.Spec.Source.RepositoryID != "" || value.Spec.Source.RepositoryURL != "" {
		h.auditSandbox("repository.selected", "success", value, "")
		h.auditSandbox("commit.selected", "success", value, "")
	}
	writeJSON(writer, http.StatusCreated, sandboxView(*value))
}

func (h Handler) sandboxRoute(writer http.ResponseWriter, request *http.Request, path []string) {
	name := path[0]
	value, err := h.Store.GetSandbox(request.Context(), name)
	if err != nil {
		storeError(writer, err)
		return
	}
	if auth.AuthorizeOwner(requestClaims(request), value.Spec.Owner.GitHubUserID) != nil {
		writeAPIError(writer, http.StatusNotFound, "sandbox_not_found", "sandbox not found", nil)
		return
	}
	if len(path) == 1 {
		switch request.Method {
		case http.MethodGet:
			writeJSON(writer, http.StatusOK, sandboxView(*value))
		case http.MethodDelete:
			h.shutdownAgent(value)
			if err := h.Store.DeleteSandbox(request.Context(), value); err != nil && !apierrors.IsNotFound(err) {
				storeError(writer, err)
				return
			}
			h.auditSandbox("sandbox.delete", "success", value, "")
			writer.WriteHeader(http.StatusNoContent)
		default:
			methodError(writer, http.MethodGet+", "+http.MethodDelete)
		}
		return
	}
	switch {
	case len(path) == 2 && path[1] == "stop":
		h.transition(writer, request, value, false)
	case len(path) == 2 && path[1] == "resume":
		h.transition(writer, request, value, true)
	case len(path) == 2 && path[1] == "events":
		h.events(writer, request, value)
	case len(path) == 2 && path[1] == "vscode-url":
		h.vscodeURL(writer, request, value)
	case len(path) == 2 && path[1] == "exec":
		h.execRequest(writer, request, value)
	case len(path) == 2 && path[1] == "jobs":
		h.agentHTTP(writer, request, value, http.MethodGet, "/v1/jobs")
	case len(path) == 3 && path[1] == "jobs":
		h.stopManagedJob(writer, request, value, path[2])
	case len(path) == 2 && path[1] == "shell":
		h.websocket(writer, request, value, "/v1/shell", h.agentPort())
	case len(path) == 3 && path[1] == "exec" && path[2] == "stream":
		h.websocket(writer, request, value, "/v1/exec/stream", h.agentPort())
	case len(path) == 3 && path[1] == "ports":
		port, err := strconv.Atoi(path[2])
		if err != nil || port < 1 || port > 65535 {
			writeAPIError(writer, http.StatusBadRequest, "invalid_port", "port must be between 1 and 65535", nil)
			return
		}
		request.URL.RawQuery = "port=" + strconv.Itoa(port)
		h.websocket(writer, request, value, "/v1/tunnel", h.agentPort())
	default:
		writeAPIError(writer, http.StatusNotFound, "not_found", "endpoint not found", nil)
	}
}

func (h Handler) transition(writer http.ResponseWriter, request *http.Request, value *devsandboxv1alpha1.DevSandbox, resume bool) {
	started := time.Now()
	if request.Method != http.MethodPost {
		methodError(writer, http.MethodPost)
		return
	}
	phase := value.Status.Phase
	if resume {
		if phase != "Stopped" {
			writeAPIError(writer, http.StatusConflict, "invalid_state", "only a stopped sandbox can be resumed", map[string]string{"phase": phase})
			return
		}
		value.Spec.DesiredState = devsandboxv1alpha1.DesiredStateRunning
	} else {
		if value.Spec.DesiredState == devsandboxv1alpha1.DesiredStateStopped && (phase == "Stopped" || phase == "Stopping") {
			writeJSON(writer, http.StatusAccepted, sandboxView(*value))
			return
		}
		if phase == "Failed" || phase == "Deleting" || phase == "Stopped" {
			writeAPIError(writer, http.StatusConflict, "invalid_state", "sandbox cannot be stopped from its current state", map[string]string{"phase": phase})
			return
		}
		value.Spec.DesiredState = devsandboxv1alpha1.DesiredStateStopped
	}
	if err := h.Store.UpdateSandbox(request.Context(), value); err != nil {
		if apierrors.IsConflict(err) {
			writeAPIError(writer, http.StatusConflict, "resource_conflict", "sandbox changed; retry the request", nil)
			return
		}
		storeError(writer, err)
		return
	}
	if !resume {
		h.shutdownAgent(value)
	}
	operation := "stop"
	if resume {
		operation = "resume"
		if h.Metrics != nil {
			h.Metrics.LifecycleDuration.WithLabelValues(operation).Observe(time.Since(started).Seconds())
		}
	}
	h.auditSandbox("sandbox."+operation, "success", value, "")
	writeJSON(writer, http.StatusAccepted, sandboxView(*value))
}

func (h Handler) events(writer http.ResponseWriter, request *http.Request, value *devsandboxv1alpha1.DevSandbox) {
	if request.Method != http.MethodGet {
		methodError(writer, http.MethodGet)
		return
	}
	values, err := h.Store.ListEvents(request.Context(), value.UID)
	if err != nil {
		storeError(writer, err)
		return
	}
	items := make([]SandboxEvent, 0, len(values))
	for _, value := range values {
		items = append(items, eventView(value))
	}
	writeJSON(writer, http.StatusOK, map[string]interface{}{"items": items})
}

func (h Handler) vscodeURL(writer http.ResponseWriter, request *http.Request, value *devsandboxv1alpha1.DevSandbox) {
	if request.Method != http.MethodPost {
		methodError(writer, http.MethodPost)
		return
	}
	if err := requireRunning(value); err != nil {
		invalidState(writer, value.Status.Phase)
		return
	}
	template, err := h.Store.GetTemplate(request.Context(), value.Spec.Template.Name)
	if err != nil {
		storeError(writer, err)
		return
	}
	if !template.Spec.Capabilities.VSCode {
		writeAPIError(writer, http.StatusConflict, "vscode_unavailable", "sandbox template does not provide VS Code", nil)
		return
	}
	if h.Credentials == nil || h.WebBaseURL == "" {
		writeAPIError(writer, http.StatusServiceUnavailable, "vscode_unavailable", "VS Code URL service is unavailable", nil)
		return
	}
	claim, err := h.Claims.Issue(request.Context(), gateway.RouteClaim{
		OwnerID: value.Spec.Owner.GitHubUserID, SandboxName: value.Name, SandboxUID: string(value.UID),
		RouterID: value.Status.UpstreamSandboxName, Namespace: h.namespace(), Port: h.vscodePort(),
	})
	if err != nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "vscode_unavailable", "VS Code URL service is unavailable", nil)
		return
	}
	ttl := h.CredentialTTL
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	credential, expires, err := h.Credentials.Issue(request.Context(), claim, ttl)
	if err != nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "vscode_unavailable", "VS Code URL service is unavailable", nil)
		return
	}
	endpoint, err := url.Parse(h.WebBaseURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
		writeAPIError(writer, http.StatusServiceUnavailable, "vscode_unavailable", "VS Code URL service is misconfigured", nil)
		return
	}
	endpoint.Path = "/bootstrap"
	endpoint.RawQuery = ""
	endpoint.Fragment = "credential=" + credential
	h.auditSandbox("session.start", "success", value, "vscode")
	writeJSON(writer, http.StatusOK, map[string]interface{}{"url": endpoint.String(), "expiresAt": expires})
}

func (h Handler) agentHTTP(writer http.ResponseWriter, request *http.Request, value *devsandboxv1alpha1.DevSandbox, method, upstreamPath string) {
	if request.Method != method {
		methodError(writer, method)
		return
	}
	if requireRunning(value) != nil {
		invalidState(writer, value.Status.Phase)
		return
	}
	if h.Router == nil {
		writeAPIError(writer, http.StatusBadGateway, "sandbox_unavailable", "sandbox connection is unavailable", nil)
		return
	}
	request.URL.Path = upstreamPath
	request.URL.RawPath = ""
	request.Header.Del("Cookie")
	stopActivity := h.startActivity(request.Context(), value, "foreground")
	defer stopActivity()
	if upstreamPath == "/v1/exec" {
		h.session(value, "exec", 1)
		defer h.session(value, "exec", -1)
	}
	h.Router.ServeRoute(writer, request, h.routeTarget(value, h.agentPort(), ""))
}

func (h Handler) websocket(writer http.ResponseWriter, request *http.Request, value *devsandboxv1alpha1.DevSandbox, upstreamPath string, port int) {
	if request.Method != http.MethodGet {
		methodError(writer, http.MethodGet)
		return
	}
	if requireRunning(value) != nil {
		invalidState(writer, value.Status.Phase)
		return
	}
	if h.WebSockets == nil {
		writeAPIError(writer, http.StatusBadGateway, "sandbox_unavailable", "sandbox connection is unavailable", nil)
		return
	}
	request.Header.Del("Cookie")
	session := strings.TrimPrefix(upstreamPath, "/v1/")
	h.session(value, session, 1)
	defer h.session(value, session, -1)
	target := h.routeTarget(value, port, upstreamPath)
	target.RawQuery = request.URL.RawQuery
	gateway.WebSocketProxy{
		Router: h.WebSockets,
		OnActivity: func() {
			h.renewActivity(request.Context(), value, "connection")
		},
	}.ServeHTTP(writer, request, target)
}

func (h Handler) auditSandbox(event, outcome string, value *devsandboxv1alpha1.DevSandbox, session string) {
	if h.Audit == nil || value == nil {
		return
	}
	repositoryID := value.Spec.Source.RepositoryID
	if repositoryID == "" {
		repositoryID = value.Spec.Source.RepositoryURL
	}
	h.Audit.Record(observability.AuditEvent{
		Event: event, Outcome: outcome, UserID: value.Spec.Owner.GitHubUserID,
		Namespace: h.namespace(), SandboxUID: string(value.UID), SandboxName: value.Name,
		Template: value.Spec.Template.Name, Profile: string(value.Spec.Profile.Name),
		Repository: repositoryID, CommitSHA: value.Spec.Source.CommitSHA, Session: session,
	})
}

func (h Handler) session(value *devsandboxv1alpha1.DevSandbox, session string, delta float64) {
	event := "session.start"
	if delta < 0 {
		event = "session.end"
	}
	h.auditSandbox(event, "success", value, session)
	if h.Metrics != nil {
		h.Metrics.ActiveConnections.WithLabelValues(session).Add(delta)
	}
}

func (h Handler) routeTarget(value *devsandboxv1alpha1.DevSandbox, port int, path string) gateway.RouteTarget {
	return gateway.RouteTarget{
		ID: value.Status.UpstreamSandboxName, UID: string(value.UID),
		Namespace: h.namespace(), Port: port, Path: path,
	}
}

func requireRunning(value *devsandboxv1alpha1.DevSandbox) error {
	if value.Status.Phase != "Running" || value.Status.UpstreamSandboxName == "" {
		return errors.New("sandbox is not running")
	}
	return nil
}

func invalidState(writer http.ResponseWriter, phase string) {
	writeAPIError(writer, http.StatusConflict, "invalid_state", "sandbox must be running", map[string]string{"phase": phase})
}

func (h Handler) namespace() string {
	if h.WorkloadNamespace != "" {
		return h.WorkloadNamespace
	}
	return "devsandbox-workloads"
}

func (h Handler) agentPort() int {
	if h.AgentPort > 0 {
		return h.AgentPort
	}
	return 8081
}

func (h Handler) vscodePort() int {
	if h.VSCodePort > 0 {
		return h.VSCodePort
	}
	return 13337
}

func splitPath(value string) []string {
	value = strings.Trim(value, "/")
	if value == "" {
		return nil
	}
	return strings.Split(value, "/")
}

func decodeJSON(reader io.Reader, target interface{}) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing content")
	}
	return nil
}

func generatedSandboxName(login string) string {
	login = strings.ToLower(login)
	login = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(login, "-")
	login = strings.Trim(login, "-")
	if len(login) > 40 {
		login = login[:40]
	}
	if login == "" {
		login = "sandbox"
	}
	raw := make([]byte, 5)
	if _, err := rand.Read(raw); err != nil {
		return login + "-new"
	}
	return login + "-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
}

func methodError(writer http.ResponseWriter, allow string) {
	writer.Header().Set("Allow", allow)
	writeAPIError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", nil)
}

func storeError(writer http.ResponseWriter, err error) {
	if apierrors.IsNotFound(err) {
		writeAPIError(writer, http.StatusNotFound, "not_found", "resource not found", nil)
		return
	}
	writeAPIError(writer, http.StatusServiceUnavailable, "kubernetes_unavailable", "management data is temporarily unavailable", nil)
}
