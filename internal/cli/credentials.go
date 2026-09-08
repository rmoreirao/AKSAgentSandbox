package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/zalando/go-keyring"
)

const credentialService = "devsandbox"

type CredentialStore interface {
	Load(context.Context, string) (string, error)
	Store(context.Context, string, string) error
	Delete(context.Context, string) error
}

type OSKeyring struct{}

func (OSKeyring) Load(_ context.Context, account string) (string, error) {
	value, err := keyring.Get(credentialService, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", cliError(ExitAuthentication, "authentication_required", "run 'devsandbox login' to authenticate", nil)
	}
	if err != nil {
		return "", credentialError(err)
	}
	return value, nil
}

func (OSKeyring) Store(_ context.Context, account, value string) error {
	if value == "" {
		return errors.New("refusing to store an empty platform session")
	}
	if err := keyring.Set(credentialService, account, value); err != nil {
		return credentialError(err)
	}
	return nil
}

func (OSKeyring) Delete(_ context.Context, account string) error {
	err := keyring.Delete(credentialService, account)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return credentialError(err)
	}
	return nil
}

func credentialAccount(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err == nil && parsed.Host != "" {
		return strings.ToLower(parsed.Scheme + "://" + parsed.Host)
	}
	return endpoint
}

func credentialError(err error) error {
	return cliError(ExitInternal, "credential_store_unavailable",
		fmt.Sprintf("operating-system credential store is unavailable: %v", err), nil)
}
