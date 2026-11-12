package workspace

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base32"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func ValidSyncFolderID(value string) bool {
	return len(value) == 38 && strings.HasPrefix(value, "morde-") && strings.Trim(value[6:], "0123456789abcdef") == ""
}

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
	FolderID             string
	FolderKind           SyncFolderKind
	RelativePath         string
	ClientInstallationID string
	DeviceID             string
	CertificateDER       []byte
	CertificateSHA256    []byte
	Nonce                []byte
	ExpiresAt            time.Time
}

type SyncFolderKind string

const (
	SyncFolderKindWorkspaceRoot SyncFolderKind = "workspace_root"
	SyncFolderKindProject       SyncFolderKind = "project"
)

type SyncFolder struct {
	OwnerID            string
	WorkspaceID        string
	FolderID           string
	Kind               SyncFolderKind
	ProjectName        string
	RelativePath       string
	State              string
	LastUploadRevision *int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (folder SyncFolder) SyncAvailable() bool {
	return folder.Kind == SyncFolderKindWorkspaceRoot
}

// ValidateSyncProjectFolderName accepts one portable directory name. It keeps
// the submitted Unicode spelling; path normalization and collision checks are
// performed again before project content is materialized.
func ValidateSyncProjectFolderName(value string) error {
	if value == "" || !utf8.ValidString(value) || strings.TrimSpace(value) != value || utf8.RuneCountInString(value) > 120 ||
		value == "." || value == ".." || strings.HasSuffix(value, ".") || strings.HasSuffix(value, " ") ||
		strings.ContainsAny(value, "/\\<>:\"|?*") {
		return fmt.Errorf("project folder name is invalid")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("project folder name is invalid")
		}
	}
	baseName := strings.ToUpper(strings.SplitN(value, ".", 2)[0])
	switch baseName {
	case "CON", "PRN", "AUX", "NUL":
		return fmt.Errorf("project folder name is reserved")
	}
	if len(baseName) == 4 && (strings.HasPrefix(baseName, "COM") || strings.HasPrefix(baseName, "LPT")) &&
		baseName[3] >= '1' && baseName[3] <= '9' {
		return fmt.Errorf("project folder name is reserved")
	}
	return nil
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

type SyncNodeObservation struct {
	Present            bool                        `json:"present"`
	Running            bool                        `json:"running"`
	ConnectedDeviceIDs []string                    `json:"connected_device_ids"`
	Folders            []SyncNodeFolderObservation `json:"folders"`
}

type SyncNodeFolderObservation struct {
	FolderID  string `json:"folder_id"`
	State     string `json:"state"`
	NeedBytes int64  `json:"need_bytes"`
	NeedFiles int64  `json:"need_files"`
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
	FolderKind           SyncFolderKind
	RelativePath         string
	State                SyncDeviceBindingState
	CreatedAt            time.Time
	RevokedAt            *time.Time
}
