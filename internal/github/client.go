package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
	"github.com/rmoreirao/AKSAgentSandbox/internal/repository"
)

const (
	defaultAPIURL    = "https://api.github.com"
	defaultDeviceURL = "https://github.com/login/device/code"
	defaultTokenURL  = "https://github.com/login/oauth/access_token"
)

// Client implements GitHub App device authorization, identity and refresh
// calls using standard net/http.
type Client struct {
	ClientID     string
	ClientSecret string
	Scopes       []string
	APIURL       string
	DeviceURL    string
	TokenURL     string
	HTTPClient   *http.Client
}

func (c *Client) StartDeviceFlow(ctx context.Context) (auth.DeviceAuthorization, error) {
	if c.ClientID == "" {
		return auth.DeviceAuthorization{}, errors.New("github app client ID is required")
	}
	scopes := c.Scopes
	if len(scopes) == 0 {
		scopes = []string{"read:user", "read:org", "repo"}
	}
	form := url.Values{"client_id": {c.ClientID}, "scope": {strings.Join(scopes, " ")}}
	var body struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int64  `json:"expires_in"`
		Interval        int64  `json:"interval"`
	}
	if err := c.postForm(ctx, c.endpoint(c.DeviceURL, defaultDeviceURL), form, &body); err != nil {
		return auth.DeviceAuthorization{}, fmt.Errorf("start GitHub device flow: %w", err)
	}
	return auth.DeviceAuthorization{
		DeviceCode: body.DeviceCode, UserCode: body.UserCode, VerificationURI: body.VerificationURI,
		ExpiresIn: time.Duration(body.ExpiresIn) * time.Second,
		Interval:  time.Duration(body.Interval) * time.Second,
	}, nil
}

func (c *Client) PollDeviceFlow(ctx context.Context, deviceCode string) (auth.DeviceToken, error) {
	if deviceCode == "" || c.ClientID == "" {
		return auth.DeviceToken{}, errors.New("github device flow request is incomplete")
	}
	form := url.Values{
		"client_id":   {c.ClientID},
		"device_code": {deviceCode},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
	}
	var body tokenResponse
	if err := c.postForm(ctx, c.endpoint(c.TokenURL, defaultTokenURL), form, &body); err != nil {
		return auth.DeviceToken{}, fmt.Errorf("poll GitHub device flow: %w", err)
	}
	if err := mapOAuthError(body.Error, body.ErrorDescription); err != nil {
		return auth.DeviceToken{}, err
	}
	if body.AccessToken == "" {
		return auth.DeviceToken{}, errors.New("github returned no access token")
	}
	return auth.DeviceToken{AccessToken: body.AccessToken, RefreshToken: body.RefreshToken}, nil
}

func (c *Client) Refresh(ctx context.Context, refreshCredential string) (auth.RefreshResult, error) {
	if refreshCredential == "" || c.ClientID == "" {
		return auth.RefreshResult{}, errors.New("github refresh request is incomplete")
	}
	form := url.Values{
		"client_id":     {c.ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshCredential},
	}
	if c.ClientSecret != "" {
		form.Set("client_secret", c.ClientSecret)
	}
	var body tokenResponse
	if err := c.postForm(ctx, c.endpoint(c.TokenURL, defaultTokenURL), form, &body); err != nil {
		return auth.RefreshResult{}, fmt.Errorf("refresh GitHub credential: %w", err)
	}
	if err := mapOAuthError(body.Error, body.ErrorDescription); err != nil {
		return auth.RefreshResult{}, err
	}
	now := time.Now()
	return auth.RefreshResult{
		AccessToken: body.AccessToken, AccessExpiresAt: now.Add(time.Duration(body.ExpiresIn) * time.Second),
		RefreshToken: body.RefreshToken, RefreshExpiresAt: now.Add(time.Duration(body.RefreshTokenExpiresIn) * time.Second),
	}, nil
}

func (c *Client) CurrentUser(ctx context.Context, accessToken string) (auth.GitHubUser, error) {
	var body struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := c.get(ctx, c.apiEndpoint("/user"), accessToken, &body); err != nil {
		return auth.GitHubUser{}, fmt.Errorf("get GitHub user: %w", err)
	}
	if body.ID <= 0 || body.Login == "" {
		return auth.GitHubUser{}, errors.New("github returned an invalid user")
	}
	return auth.GitHubUser{ID: strconv.FormatInt(body.ID, 10), Login: body.Login}, nil
}

func (c *Client) IsActiveOrgMember(ctx context.Context, accessToken, org string) (bool, error) {
	if org == "" {
		return false, errors.New("primary GitHub organization is required")
	}
	var body struct {
		State string `json:"state"`
	}
	endpoint := c.apiEndpoint("/user/memberships/orgs/" + url.PathEscape(org))
	status, err := c.getStatus(ctx, endpoint, accessToken, &body)
	if status == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("validate primary organization membership: %w", err)
	}
	return strings.EqualFold(body.State, "active"), nil
}

func (c *Client) ResolveRepository(
	ctx context.Context, accessToken, repositoryID, requestedRef, primaryOrg string,
) (repository.RemoteRepository, error) {
	parts := strings.Split(repositoryID, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || primaryOrg == "" {
		return repository.RemoteRepository{}, errors.New("repository resolution request is invalid")
	}
	resource := "/repos/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
	var metadata struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		Private       bool   `json:"private"`
		DefaultBranch string `json:"default_branch"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	}
	status, err := c.getStatus(ctx, c.apiEndpoint(resource), accessToken, &metadata)
	if status == http.StatusNotFound {
		return repository.RemoteRepository{}, repository.ErrRepositoryNotFound
	}
	if err != nil {
		return repository.RemoteRepository{}, fmt.Errorf("resolve GitHub repository: %w", err)
	}
	if metadata.ID <= 0 || metadata.FullName == "" || metadata.Owner.Login == "" || metadata.DefaultBranch == "" {
		return repository.RemoteRepository{}, errors.New("github returned invalid repository metadata")
	}
	if metadata.Private && !strings.EqualFold(metadata.Owner.Login, primaryOrg) {
		return repository.RemoteRepository{}, repository.ErrExternalPrivate
	}
	refName := strings.TrimSpace(requestedRef)
	if refName == "" {
		refName = metadata.DefaultBranch
	}
	refName = strings.TrimPrefix(refName, "refs/heads/")
	var commit struct {
		SHA string `json:"sha"`
	}
	commitEndpoint := resource + "/git/commits/" + url.PathEscape(refName)
	commitTarget := interface{}(&commit)
	var reference struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if !commitPattern.MatchString(refName) {
		commitEndpoint = resource + "/git/ref/heads/" + url.PathEscape(refName)
		commitTarget = &reference
	}
	status, err = c.getStatus(ctx, c.apiEndpoint(commitEndpoint), accessToken, commitTarget)
	if status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
		return repository.RemoteRepository{}, repository.ErrRepositoryNotFound
	}
	if err != nil {
		return repository.RemoteRepository{}, fmt.Errorf("resolve GitHub repository ref: %w", err)
	}
	if reference.Object.SHA != "" {
		commit.SHA = reference.Object.SHA
	}
	if !commitPattern.MatchString(commit.SHA) {
		return repository.RemoteRepository{}, errors.New("github returned an invalid commit")
	}
	var profile struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
		Email string `json:"email"`
	}
	if err := c.get(ctx, c.apiEndpoint("/user"), accessToken, &profile); err != nil {
		return repository.RemoteRepository{}, fmt.Errorf("resolve GitHub author profile: %w", err)
	}
	if profile.Login == "" || profile.ID <= 0 {
		return repository.RemoteRepository{}, errors.New("github returned an invalid user profile")
	}
	authorName := strings.TrimSpace(profile.Name)
	if authorName == "" {
		authorName = profile.Login
	}
	authorEmail := strings.TrimSpace(profile.Email)
	if authorEmail == "" {
		authorEmail = strconv.FormatInt(profile.ID, 10) + "+" + profile.Login + "@users.noreply.github.com"
	}
	id, cloneURL, err := repository.CanonicalGitHubURL(metadata.FullName)
	if err != nil {
		return repository.RemoteRepository{}, errors.New("github returned an invalid repository name")
	}
	return repository.RemoteRepository{
		ID: id, URL: cloneURL, Owner: strings.ToLower(metadata.Owner.Login),
		DefaultBranch: metadata.DefaultBranch, RefName: refName, CommitSHA: strings.ToLower(commit.SHA),
		AuthorName: authorName, AuthorEmail: authorEmail, PrimaryOrg: strings.ToLower(primaryOrg),
		Private: metadata.Private, ReadOnly: !strings.EqualFold(metadata.Owner.Login, primaryOrg),
	}, nil
}

type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshToken          string `json:"refresh_token"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
	Error                 string `json:"error"`
	ErrorDescription      string `json:"error_description"`
}

var commitPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func mapOAuthError(code, description string) error {
	switch code {
	case "":
		return nil
	case "authorization_pending":
		return auth.ErrAuthorizationPending
	case "slow_down":
		return auth.ErrSlowDown
	case "expired_token":
		return auth.ErrDeviceFlowExpired
	case "invalid_grant":
		return auth.ErrInvalidGrant
	default:
		if description == "" {
			description = code
		}
		return fmt.Errorf("github oauth error %s", description)
	}
}

func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values, target interface{}) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", "devsandbox")
	response, err := c.client().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("github returned status %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(target); err != nil {
		return errors.New("github returned an invalid response")
	}
	return nil
}

func (c *Client) get(ctx context.Context, endpoint, accessToken string, target interface{}) error {
	_, err := c.getStatus(ctx, endpoint, accessToken, target)
	return err
}

func (c *Client) getStatus(ctx context.Context, endpoint, accessToken string, target interface{}) (int, error) {
	if accessToken == "" {
		return 0, errors.New("github access token is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "devsandbox")
	response, err := c.client().Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return response.StatusCode, fmt.Errorf("github returned status %d", response.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(target); err != nil {
		return response.StatusCode, errors.New("github returned an invalid response")
	}
	return response.StatusCode, nil
}

func (c *Client) endpoint(configured, fallback string) string {
	if configured != "" {
		return configured
	}
	return fallback
}

func (c *Client) apiEndpoint(resourcePath string) string {
	base := strings.TrimSuffix(c.APIURL, "/")
	if base == "" {
		base = defaultAPIURL
	}
	return base + resourcePath
}

func (c *Client) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}
