package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

const (
	authModeDeviceFlow      = "device-flow"
	authModeGitHubCLIStatic = "github-cli-static"
)

var githubLoginPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

func githubCLIBootstrapCredential(ctx context.Context) (string, error) {
	login := strings.TrimSpace(os.Getenv("DEVSANDBOX_GITHUB_LOGIN"))
	if !githubLoginPattern.MatchString(login) {
		return "", cliError(ExitInvalid, "invalid_environment",
			"DEVSANDBOX_GITHUB_LOGIN must identify the deployed GitHub account", nil)
	}
	output, err := exec.CommandContext(ctx, "gh", "auth", "token", "--user", login).Output()
	if err != nil {
		return "", cliError(ExitAuthentication, "github_cli_authentication_required",
			"GitHub CLI has no stored credential for "+login+"; run 'gh auth login'", nil)
	}
	credential := strings.TrimSpace(string(output))
	if credential == "" || strings.ContainsAny(credential, " \t\r\n") {
		return "", cliError(ExitAuthentication, "github_cli_authentication_required",
			"GitHub CLI returned no usable credential; run 'gh auth login'", nil)
	}
	return credential, nil
}

func isAuthenticationError(err error) bool {
	var detail *Error
	return errors.As(err, &detail) && detail.ExitCode == ExitAuthentication
}
