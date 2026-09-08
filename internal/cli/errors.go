package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const (
	ExitSuccess        = 0
	ExitInvalid        = 2
	ExitAuthentication = 3
	ExitNotFound       = 4
	ExitConflict       = 5
	ExitProvisioning   = 6
	ExitTransport      = 7
	ExitInternal       = 10
)

type Error struct {
	ExitCode int    `json:"-"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Details  any    `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Message }

func cliError(exit int, code, message string, details any) *Error {
	return &Error{ExitCode: exit, Code: code, Message: message, Details: details}
}

func invalid(message string) error {
	return cliError(ExitInvalid, "invalid_arguments", message, nil)
}

func exitCode(err error) int {
	if err == nil {
		return ExitSuccess
	}
	var remote RemoteExitError
	if errors.As(err, &remote) {
		return remote.Code
	}
	var cliErr *Error
	if errors.As(err, &cliErr) {
		return cliErr.ExitCode
	}
	return ExitInternal
}

func writeError(out io.Writer, err error, structured bool) {
	var cliErr *Error
	if !errors.As(err, &cliErr) {
		cliErr = cliError(ExitInternal, "internal_error", err.Error(), nil)
	}
	if structured {
		_ = json.NewEncoder(out).Encode(cliErr)
		return
	}
	_, _ = fmt.Fprintf(out, "Error [%s]: %s\n", cliErr.Code, cliErr.Message)
	if cliErr.Details != nil {
		encoded, _ := json.Marshal(cliErr.Details)
		_, _ = fmt.Fprintf(out, "Details: %s\n", encoded)
	}
}

func errorFromResponse(response *http.Response) error {
	body := apiErrorBody{}
	_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body)
	if body.Code == "" {
		body.Code = "http_error"
	}
	if body.Message == "" {
		body.Message = response.Status
	}
	exit := ExitInternal
	switch response.StatusCode {
	case http.StatusBadRequest:
		exit = ExitInvalid
	case http.StatusUnauthorized, http.StatusForbidden:
		exit = ExitAuthentication
	case http.StatusNotFound:
		exit = ExitNotFound
	case http.StatusConflict, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		exit = ExitConflict
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		exit = ExitProvisioning
	}
	return cliError(exit, body.Code, body.Message, body.Details)
}

// RemoteExitError preserves a remote process exit status for MVP-11 commands.
type RemoteExitError struct{ Code int }

func (e RemoteExitError) Error() string {
	return fmt.Sprintf("remote process exited with status %d", e.Code)
}
