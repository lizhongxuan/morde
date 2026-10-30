package workspace

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"fmt"
	"strings"
	"time"
)

func SyncthingDeviceIDFromCertificateDER(der []byte) (string, error) {
	certificate, err := x509.ParseCertificate(der)
	if err != nil || !bytes.Equal(certificate.Raw, der) ||
		certificate.CheckSignature(certificate.SignatureAlgorithm, certificate.RawTBSCertificate, certificate.Signature) != nil {
		return "", fmt.Errorf("invalid self-signed Syncthing certificate")
	}
	return syncthingDeviceID(sha256.Sum256(certificate.Raw)), nil
}

func syncthingDeviceID(certificateHash [32]byte) string {
	// Syncthing adds per-group Luhn32 check digits to the base32 certificate hash.
	encoded := strings.TrimRight(base32.StdEncoding.EncodeToString(certificateHash[:]), "=")
	var checked strings.Builder
	for offset := 0; offset < len(encoded); offset += 13 {
		part := encoded[offset : offset+13]
		checked.WriteString(part)
		checked.WriteByte(luhn32CheckDigit(part))
	}
	value := checked.String()
	var grouped strings.Builder
	for offset := 0; offset < len(value); offset += 7 {
		if offset > 0 {
			grouped.WriteByte('-')
		}
		grouped.WriteString(value[offset : offset+7])
	}
	return grouped.String()
}

func luhn32CheckDigit(value string) byte {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	factor, sum := 1, 0
	for i := 0; i < len(value); i++ {
		codepoint := strings.IndexByte(alphabet, value[i])
		addend := factor * codepoint
		sum += addend/32 + addend%32
		if factor == 1 {
			factor = 2
		} else {
			factor = 1
		}
	}
	return alphabet[(32-sum%32)%32]
}

type SyncDeviceChallenge struct {
	ID                   string
	OwnerID              string
	WorkspaceID          string
	ClientInstallationID string
	DeviceID             string
	CertificateDER       []byte
	CertificateSHA256    []byte
	Nonce                []byte
	ExpiresAt            time.Time
}

type SyncDeviceBindingState string

const (
	SyncDeviceBindingPendingNode SyncDeviceBindingState = "pending_node"
	SyncDeviceBindingActive      SyncDeviceBindingState = "active"
	SyncDeviceBindingRevoking    SyncDeviceBindingState = "revoking"
	SyncDeviceBindingRevoked     SyncDeviceBindingState = "revoked"
)

type SyncNodeConnection struct {
	DeviceID     string
	RelayAddress string
}

type SyncNodeDesiredState struct {
	OwnerID  string
	Revision int64
	Folders  []SyncNodeFolder
}

type SyncNodeFolder struct {
	WorkspaceID string
	FolderID    string
	DeviceIDs   []string
}

type SyncDeviceBinding struct {
	ID                   string
	OwnerID              string
	WorkspaceID          string
	ClientInstallationID string
	DeviceID             string
	CertificateSHA256    []byte
	FolderID             string
	State                SyncDeviceBindingState
	CreatedAt            time.Time
	RevokedAt            *time.Time
}
