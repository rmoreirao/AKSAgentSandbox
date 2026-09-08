package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/rmoreirao/AKSAgentSandbox/internal/auth"
)

var (
	ErrInvalidRouteClaim = errors.New("invalid route claim")
	ErrExpiredRouteClaim = errors.New("expired route claim")
)

type RouteClaim struct {
	OwnerID     string `json:"sub"`
	SandboxName string `json:"sandbox"`
	SandboxUID  string `json:"sandboxUid"`
	RouterID    string `json:"routerId"`
	Namespace   string `json:"namespace"`
	Port        int    `json:"port"`
	IssuedAt    int64  `json:"iat"`
	ExpiresAt   int64  `json:"exp"`
}

type ClaimManager struct {
	Signer   auth.Signer
	Lifetime time.Duration
	Now      func() time.Time
}

func (m ClaimManager) Issue(ctx context.Context, claim RouteClaim) (string, error) {
	if m.Signer == nil || claim.OwnerID == "" || claim.SandboxName == "" ||
		claim.SandboxUID == "" || claim.RouterID == "" || claim.Namespace == "" ||
		claim.Port < 1 || claim.Port > 65535 {
		return "", ErrInvalidRouteClaim
	}
	now := m.now()
	lifetime := m.Lifetime
	if lifetime <= 0 {
		lifetime = 5 * time.Minute
	}
	claim.IssuedAt = now.Unix()
	claim.ExpiresAt = now.Add(lifetime).Unix()
	payload, err := json.Marshal(claim)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	message := "route1." + encoded
	signature, err := m.Signer.Sign(ctx, []byte(message))
	if err != nil {
		return "", err
	}
	return message + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (m ClaimManager) Verify(ctx context.Context, token string) (RouteClaim, error) {
	var claim RouteClaim
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "route1" || m.Signer == nil {
		return claim, ErrInvalidRouteClaim
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || m.Signer.Verify(ctx, []byte(parts[0]+"."+parts[1]), signature) != nil {
		return claim, ErrInvalidRouteClaim
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &claim) != nil ||
		claim.OwnerID == "" || claim.SandboxName == "" || claim.SandboxUID == "" ||
		claim.RouterID == "" || claim.Namespace == "" || claim.Port < 1 || claim.Port > 65535 ||
		claim.IssuedAt <= 0 || claim.ExpiresAt <= claim.IssuedAt ||
		claim.IssuedAt > m.now().Unix()+60 {
		return RouteClaim{}, ErrInvalidRouteClaim
	}
	if m.now().Unix() >= claim.ExpiresAt {
		return RouteClaim{}, ErrExpiredRouteClaim
	}
	return claim, nil
}

func (m ClaimManager) VerifySandbox(ctx context.Context, token, sandboxName, sandboxUID string) (RouteClaim, error) {
	claim, err := m.Verify(ctx, token)
	if err != nil {
		return RouteClaim{}, err
	}
	if claim.SandboxName != sandboxName || claim.SandboxUID != sandboxUID {
		return RouteClaim{}, ErrInvalidRouteClaim
	}
	return claim, nil
}

func (m ClaimManager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}
