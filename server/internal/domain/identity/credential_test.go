package identity

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestNewAndParseCredential(t *testing.T) {
	for _, tt := range []struct {
		kind   Kind
		prefix string
	}{
		{KindSession, "chs_"},
		{KindAPIKey, "chk_"},
		{KindDevice, "chd_"},
		{KindTicket, "chx_"},
	} {
		t.Run(string(tt.kind), func(t *testing.T) {
			raw, hash, err := NewCredential(tt.kind, "t1234567")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(raw, tt.prefix) {
				t.Fatalf("credential = %q, want prefix %q", raw, tt.prefix)
			}
			kind, tokenID, secret, err := ParseCredential(raw)
			if err != nil || kind != tt.kind || tokenID != "t1234567" {
				t.Fatalf("ParseCredential() = (%q, %q, %q, %v)", kind, tokenID, secret, err)
			}
			if len(secret) != 43 || !VerifySecret(secret, hash) {
				t.Fatalf("generated secret did not verify against its hash")
			}
		})
	}
}

func TestParseCredentialAllowsUnderscoreInSecret(t *testing.T) {
	secret := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, credentialSecretSize))
	if !strings.Contains(secret, "_") {
		t.Fatalf("test secret %q does not contain underscore", secret)
	}
	gotKind, gotID, gotSecret, err := ParseCredential("chs_t1234567_" + secret)
	if err != nil || gotKind != KindSession || gotID != "t1234567" || gotSecret != secret {
		t.Fatalf("ParseCredential() = (%q, %q, %q, %v)", gotKind, gotID, gotSecret, err)
	}
}

func TestNewCredentialRejectsInvalidKindAndTokenID(t *testing.T) {
	for _, tt := range []struct {
		kind    Kind
		tokenID string
	}{
		{KindSystem, "t1234567"},
		{KindSession, "bad-id"},
	} {
		if _, _, err := NewCredential(tt.kind, tt.tokenID); !errors.Is(err, ErrInvalidCredential) {
			t.Fatalf("NewCredential(%q, %q) error = %v, want ErrInvalidCredential", tt.kind, tt.tokenID, err)
		}
	}
}

func TestParseCredentialRejectsMalformedValues(t *testing.T) {
	validSecret := base64.RawURLEncoding.EncodeToString(make([]byte, credentialSecretSize))
	for _, raw := range []string{
		"",
		"chx_t1234567_" + validSecret + "=",
		"chz_t1234567_" + validSecret,
		"chs_x1234567_" + validSecret,
		"chs_t1234567_" + strings.Repeat("A", 42),
		"chs_t1234567_" + strings.Repeat("*", 43),
		"chs_t1234567_" + validSecret + "_extra",
	} {
		if _, _, _, err := ParseCredential(raw); !errors.Is(err, ErrInvalidCredential) {
			t.Errorf("ParseCredential(%q) error = %v, want ErrInvalidCredential", raw, err)
		}
	}
}

func TestVerifySecret(t *testing.T) {
	secret := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, credentialSecretSize))
	hash := HashSecret(secret)
	if !VerifySecret(secret, hash) {
		t.Fatal("VerifySecret() = false for matching secret")
	}
	if VerifySecret(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x6a}, credentialSecretSize)), hash) {
		t.Fatal("VerifySecret() = true for different secret")
	}
	if VerifySecret("invalid", hash) || VerifySecret(secret, hash[:len(hash)-1]) {
		t.Fatal("VerifySecret() = true for malformed secret or hash")
	}
}
