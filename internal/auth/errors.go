package auth

import "errors"

var (
	ErrInvalidState         = errors.New("invalid or expired device-flow state")
	ErrAuthorizationPending = errors.New("device authorization is pending")
	ErrSlowDown             = errors.New("device authorization polling too quickly")
	ErrDeviceFlowExpired    = errors.New("device authorization expired")
	ErrMembershipRequired   = errors.New("active primary-organization membership is required")
	ErrInvalidGrant         = errors.New("github refresh credential is no longer valid")
	ErrReauthentication     = errors.New("github reauthentication is required")
	ErrInvalidSession       = errors.New("invalid platform session")
	ErrExpiredSession       = errors.New("expired platform session")
	ErrCredentialNotFound   = errors.New("github refresh credential not found")
	ErrOwnerMismatch        = errors.New("authenticated user does not own the sandbox")
)
