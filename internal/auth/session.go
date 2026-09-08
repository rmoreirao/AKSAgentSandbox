package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Signer is implemented by an in-memory HMAC key or a Key Vault-backed
// asymmetric signing key. Verify must use the corresponding verification key.
type Signer interface {
	Sign(ctx context.Context, message []byte) ([]byte, error)
	Verify(ctx context.Context, message, signature []byte) error
}

type HMACSigner struct {
	Key []byte
}

func (s HMACSigner) Sign(_ context.Context, message []byte) ([]byte, error) {
	if len(s.Key) < 32 {
		return nil, errors.New("session signing key must contain at least 32 bytes")
	}
	mac := hmac.New(sha256.New, s.Key)
	_, _ = mac.Write(message)
	return mac.Sum(nil), nil
}

func (s HMACSigner) Verify(ctx context.Context, message, signature []byte) error {
	expected, err := s.Sign(ctx, message)
	if err != nil {
		return err
	}
	if !hmac.Equal(expected, signature) {
		return ErrInvalidSession
	}
	return nil
}

// KeyVaultSigningClient is the narrow interface required from a Key Vault key
// adapter. Keeping it here lets production use an asymmetric key without
// coupling authentication to an Azure SDK.
type KeyVaultSigningClient interface {
	Sign(ctx context.Context, keyID string, digest []byte) ([]byte, error)
	Verify(ctx context.Context, keyID string, digest, signature []byte) error
}

type KeyVaultSigner struct {
	Client KeyVaultSigningClient
	KeyID  string
}

func (s KeyVaultSigner) Sign(ctx context.Context, message []byte) ([]byte, error) {
	if s.Client == nil || s.KeyID == "" {
		return nil, errors.New("key vault signer is not configured")
	}
	digest := sha256.Sum256(message)
	return s.Client.Sign(ctx, s.KeyID, digest[:])
}

func (s KeyVaultSigner) Verify(ctx context.Context, message, signature []byte) error {
	if s.Client == nil || s.KeyID == "" {
		return errors.New("key vault signer is not configured")
	}
	digest := sha256.Sum256(message)
	if err := s.Client.Verify(ctx, s.KeyID, digest[:], signature); err != nil {
		return fmt.Errorf("%w: signature verification failed", ErrInvalidSession)
	}
	return nil
}

type Identity struct {
	UserID string `json:"userId"`
	Login  string `json:"login"`
	Org    string `json:"org"`
}

type SessionClaims struct {
	Subject   string `json:"sub"`
	Login     string `json:"login"`
	Org       string `json:"org"`
	Audience  string `json:"aud"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

type SessionManager struct {
	Signer   Signer
	Audience string
	Lifetime time.Duration
	Now      func() time.Time
}

func (m SessionManager) Issue(ctx context.Context, identity Identity) (string, time.Time, error) {
	if m.Signer == nil || identity.UserID == "" || identity.Org == "" || m.Audience == "" {
		return "", time.Time{}, errors.New("session manager or immutable identity is incomplete")
	}
	lifetime := m.Lifetime
	if lifetime <= 0 {
		lifetime = 15 * time.Minute
	}
	now := m.now().UTC()
	expires := now.Add(lifetime)
	claims := SessionClaims{
		Subject: identity.UserID, Login: identity.Login, Org: identity.Org,
		Audience: m.Audience, IssuedAt: now.Unix(), ExpiresAt: expires.Unix(),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", time.Time{}, err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	message := "ds1." + encoded
	signature, err := m.Signer.Sign(ctx, []byte(message))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign platform session: %w", err)
	}
	return message + "." + base64.RawURLEncoding.EncodeToString(signature), expires, nil
}

func (m SessionManager) Verify(ctx context.Context, token string) (SessionClaims, error) {
	var claims SessionClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != "ds1" || m.Signer == nil {
		return claims, ErrInvalidSession
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims, ErrInvalidSession
	}
	message := parts[0] + "." + parts[1]
	if err := m.Signer.Verify(ctx, []byte(message), signature); err != nil {
		return claims, ErrInvalidSession
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &claims) != nil {
		return SessionClaims{}, ErrInvalidSession
	}
	now := m.now().Unix()
	if claims.Subject == "" || claims.Org == "" || claims.Audience != m.Audience ||
		claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt || claims.IssuedAt > now+60 {
		return SessionClaims{}, ErrInvalidSession
	}
	if now >= claims.ExpiresAt {
		return SessionClaims{}, ErrExpiredSession
	}
	return claims, nil
}

func (m SessionManager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}
