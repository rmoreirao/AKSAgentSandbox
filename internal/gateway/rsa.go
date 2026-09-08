package gateway

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
)

type RSAPublicKeyVerifier struct {
	Key *rsa.PublicKey
}

func ParseRSAPublicKeyPEM(value []byte) (RSAPublicKeyVerifier, error) {
	block, _ := pem.Decode(value)
	if block == nil {
		return RSAPublicKeyVerifier{}, errors.New("route public key is not PEM")
	}
	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PublicKey); ok && rsaKey.N.BitLen() >= 2048 {
			return RSAPublicKeyVerifier{Key: rsaKey}, nil
		}
	}
	if key, err := x509.ParsePKCS1PublicKey(block.Bytes); err == nil && key.N.BitLen() >= 2048 {
		return RSAPublicKeyVerifier{Key: key}, nil
	}
	return RSAPublicKeyVerifier{}, errors.New("route public key must be a 2048-bit or larger RSA public key")
}

func (v RSAPublicKeyVerifier) Sign(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("public key cannot sign")
}

func (v RSAPublicKeyVerifier) Verify(_ context.Context, message, signature []byte) error {
	if v.Key == nil {
		return ErrInvalidRouteClaim
	}
	digest := sha256.Sum256(message)
	if err := rsa.VerifyPKCS1v15(v.Key, crypto.SHA256, digest[:], signature); err != nil {
		return ErrInvalidRouteClaim
	}
	return nil
}
