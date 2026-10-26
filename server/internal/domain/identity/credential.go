package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"morde/server/internal/domain"
)

const credentialSecretSize = 32

func NewCredential(kind Kind, tokenID string) (string, []byte, error) {
	prefix, ok := credentialPrefix(kind)
	if !ok || !domain.ValidTokenID(tokenID) {
		return "", nil, ErrInvalidCredential
	}

	secretBytes := make([]byte, credentialSecretSize)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", nil, fmt.Errorf("generate credential secret: %w", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	return prefix + tokenID + "_" + secret, HashSecret(secret), nil
}

func ParseCredential(raw string) (Kind, string, string, error) {
	kind, prefix, ok := credentialKind(raw)
	if !ok || !strings.HasPrefix(raw, prefix) {
		return "", "", "", ErrInvalidCredential
	}

	parts := raw[len(prefix):]
	if len(parts) != 8+1+43 || parts[8] != '_' {
		return "", "", "", ErrInvalidCredential
	}
	tokenID, secret := parts[:8], parts[9:]
	if !domain.ValidTokenID(tokenID) || !validSecret(secret) {
		return "", "", "", ErrInvalidCredential
	}
	return kind, tokenID, secret, nil
}

func HashSecret(secret string) []byte {
	hash := sha256.Sum256([]byte(secret))
	return hash[:]
}

func VerifySecret(secret string, hash []byte) bool {
	if !validSecret(secret) || len(hash) != sha256.Size {
		return false
	}
	return subtle.ConstantTimeCompare(HashSecret(secret), hash) == 1
}

func credentialPrefix(kind Kind) (string, bool) {
	switch kind {
	case KindSession:
		return "chs_", true
	case KindAPIKey:
		return "chk_", true
	case KindDevice:
		return "chd_", true
	case KindClient:
		return "chc_", true
	case KindTicket:
		return "chx_", true
	default:
		return "", false
	}
}

func credentialKind(raw string) (Kind, string, bool) {
	for _, kind := range []Kind{KindSession, KindAPIKey, KindDevice, KindClient, KindTicket} {
		prefix, _ := credentialPrefix(kind)
		if strings.HasPrefix(raw, prefix) {
			return kind, prefix, true
		}
	}
	return "", "", false
}

func validSecret(secret string) bool {
	if len(secret) != 43 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	return err == nil && len(decoded) == credentialSecretSize && base64.RawURLEncoding.EncodeToString(decoded) == secret
}
