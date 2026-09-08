package auth

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"
)

// StaticTokenProvider supports non-interactive PoC validation when GitHub App
// registration cannot be completed. It remains bound to one immutable user ID.
type StaticTokenProvider struct {
	UserID    string
	TokenFile string
	Now       func() time.Time
}

func (p StaticTokenProvider) AccessToken(_ context.Context, userID string) (string, time.Time, error) {
	if p.UserID == "" || p.TokenFile == "" || userID != p.UserID {
		return "", time.Time{}, ErrReauthentication
	}
	value, err := os.ReadFile(p.TokenFile)
	if err != nil {
		return "", time.Time{}, errors.New("read static GitHub token")
	}
	token := strings.TrimSpace(string(value))
	if token == "" {
		return "", time.Time{}, errors.New("static GitHub token is empty")
	}
	now := time.Now()
	if p.Now != nil {
		now = p.Now()
	}
	return token, now.Add(24 * time.Hour), nil
}
