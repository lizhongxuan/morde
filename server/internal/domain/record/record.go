package record

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"mime"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
)

type ItemKind string

const (
	ItemTurn   ItemKind = "turn"
	ItemSample ItemKind = "sample"
)

var secretContentPattern = regexp.MustCompile(`(?i)(\{\{\s*vault:|\bvault(?:\s*[:=]|://)|password\s*[:=]\s*\S+|passwd\s*[:=]\s*\S+|token\s*[:=]\s*\S+|api[_-]?key\s*[:=]\s*\S+|secret\s*[:=]\s*\S+|authorization\s*[:=]\s*\S+|-----BEGIN [A-Z ]*PRIVATE KEY-----|\bBearer[[:space:]]+[A-Za-z0-9._~+/-]{12,}|\bsk-(proj-)?[A-Za-z0-9_-]{16,}|\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}|\bxox[baprs]-[A-Za-z0-9-]{16,}|\bAKIA[0-9A-Z]{16}\b|\b[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b)`)

var base64CandidatePattern = regexp.MustCompile(`[A-Za-z0-9+/_-]{16,}={0,2}`)

const (
	maxBase64CandidateBytes = 4 << 10
	maxBase64Candidates     = 4096
)

func (kind ItemKind) Valid() bool { return kind == ItemTurn || kind == ItemSample }

type AdminCapability string

const (
	CapabilityView       AdminCapability = "view"
	CapabilityAttachment AdminCapability = "attachment"
	CapabilityExport     AdminCapability = "export"
	CapabilityDelete     AdminCapability = "delete"
)

func (capability AdminCapability) Valid() bool {
	return capability == CapabilityView || capability == CapabilityAttachment ||
		capability == CapabilityExport || capability == CapabilityDelete
}

type AttachmentInput struct {
	FileName  string
	MediaType string
	Content   []byte
}

type CaptureInput struct {
	Purpose     Purpose
	Kind        ItemKind
	TaskID      string
	Payload     []byte
	Attachments []AttachmentInput
}

const (
	MaxCapturePayloadBytes    int64 = 32 << 20
	MaxCaptureAttachmentBytes int64 = 64 << 20
	MaxCaptureTotalBytes      int64 = 128 << 20
	MaxCaptureAttachments           = 16
)

func ValidateCapture(value CaptureInput) error {
	if !value.Purpose.Valid() || !value.Kind.Valid() {
		return domain.InvalidArgument("record_capture", "record purpose or item kind is invalid")
	}
	if value.TaskID != "" && !domain.ValidTaskID(value.TaskID) {
		return domain.InvalidArgument("record_capture", "source task id is invalid")
	}
	if (value.Purpose == PurposeFullRecord && value.Kind != ItemTurn) ||
		(value.Purpose == PurposeTrainingContribution && value.Kind != ItemSample) {
		return domain.InvalidArgument("record_capture", "record item kind does not match its purpose")
	}
	if len(value.Payload) == 0 || int64(len(value.Payload)) > MaxCapturePayloadBytes {
		return domain.NewError(domain.CodeRequestTooLarge, "record payload is empty or exceeds the allowed size")
	}
	if !scannableText(value.Payload) {
		return domain.InvalidArgument("record_capture", "record payload cannot be scanned safely")
	}
	if ContainsSensitiveContent(value.Payload) {
		return domain.InvalidArgument("record_capture", "record content contains secret-like data")
	}
	if len(value.Attachments) > MaxCaptureAttachments {
		return domain.NewError(domain.CodeRequestTooLarge, "record attachment count exceeds the allowed limit")
	}
	total := int64(len(value.Payload))
	for _, attachment := range value.Attachments {
		mediaType, scannable := scannableTextMediaType(attachment.MediaType)
		if !validFileName(attachment.FileName) || !scannable {
			return domain.InvalidArgument("record_attachment", "record attachment metadata is invalid")
		}
		size := int64(len(attachment.Content))
		if size < 1 || size > MaxCaptureAttachmentBytes || total > MaxCaptureTotalBytes-size {
			return domain.NewError(domain.CodeRequestTooLarge, "record attachment size exceeds the allowed limit")
		}
		if !scannableText(attachment.Content) {
			return domain.InvalidArgument("record_attachment", "attachment cannot be scanned safely")
		}
		if (mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")) && !json.Valid(attachment.Content) {
			return domain.InvalidArgument("record_attachment", "JSON attachment is invalid")
		}
		if ContainsSensitiveContent(attachment.Content) {
			return domain.InvalidArgument("record_attachment", "attachment contains secret-like data")
		}
		total += size
	}
	if total > MaxCaptureTotalBytes {
		return domain.NewError(domain.CodeRequestTooLarge, "record capture size exceeds the allowed limit")
	}
	return nil
}

// ContainsSensitiveContent blocks common credential fields, formats, and Vault placeholders.
func ContainsSensitiveContent(content []byte) bool {
	if containsRawSensitiveContent(content) {
		return true
	}
	// shortcut: inspect one bounded layer of Base64 text; compressed or non-text decoded data needs a format-specific scanner.
	candidates := base64CandidatePattern.FindAll(content, maxBase64Candidates+1)
	if len(candidates) > maxBase64Candidates {
		return true
	}
	for _, candidate := range candidates {
		if len(candidate) > maxBase64CandidateBytes {
			return true
		}
		decoded, ok := decodeBase64Candidate(candidate)
		if ok && scannableText(decoded) && containsRawSensitiveContent(decoded) {
			return true
		}
	}
	return false
}

func containsRawSensitiveContent(content []byte) bool {
	if secretContentPattern.Match(content) {
		return true
	}
	var value any
	return json.Unmarshal(content, &value) == nil && recordJSONContainsSensitiveValue(value)
}

func decodeBase64Candidate(candidate []byte) ([]byte, bool) {
	encoded := string(candidate)
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(encoded); err == nil {
			return decoded, true
		}
	}
	return nil, false
}

func scannableText(content []byte) bool {
	return utf8.Valid(content) && !bytes.ContainsRune(content, 0)
}

func recordJSONContainsSensitiveValue(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			name := strings.Map(func(r rune) rune {
				if r >= 'A' && r <= 'Z' {
					r += 'a' - 'A'
				}
				if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
					return r
				}
				return -1
			}, key)
			if strings.Contains(name, "secret") || strings.Contains(name, "password") || strings.Contains(name, "passwd") ||
				strings.Contains(name, "token") || strings.Contains(name, "credential") || strings.Contains(name, "authorization") ||
				strings.Contains(name, "apikey") || strings.Contains(name, "privatekey") || strings.Contains(name, "accesskey") || strings.Contains(name, "vault") {
				return true
			}
			if recordJSONContainsSensitiveValue(item) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if recordJSONContainsSensitiveValue(item) {
				return true
			}
		}
	case string:
		return secretContentPattern.MatchString(value)
	}
	return false
}

func validFileName(value string) bool {
	return value != "" && value != "." && value != ".." && len(value) <= 255 && utf8.ValidString(value) && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "/\\\x00") && strings.IndexFunc(value, isControl) < 0
}

func validMediaType(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	parsed, _, err := mime.ParseMediaType(value)
	return err == nil && parsed != ""
}

func scannableTextMediaType(value string) (string, bool) {
	if !validMediaType(value) {
		return "", false
	}
	mediaType, params, _ := mime.ParseMediaType(value)
	charset := strings.ToLower(params["charset"])
	if charset != "" && charset != "utf-8" && charset != "us-ascii" {
		return "", false
	}
	if strings.HasPrefix(mediaType, "text/") || mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
		return mediaType, true
	}
	return "", false
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

type QueueState string

const (
	QueueQueued   QueueState = "queued"
	QueueLeased   QueueState = "leased"
	QueueStored   QueueState = "stored"
	QueueRejected QueueState = "rejected"
	QueueFailed   QueueState = "failed"
)

func (state QueueState) Valid() bool {
	return state == QueueQueued || state == QueueLeased || state == QueueStored ||
		state == QueueRejected || state == QueueFailed
}

type QueueItem struct {
	ID                     string
	OwnerID                string
	Purpose                Purpose
	Kind                   ItemKind
	TaskID                 string
	FailureCode            string
	ConsentVersion         int64
	CapturedByInstallation string
	CapturedAt             time.Time
	ExpiresAt              time.Time
	ReservedBytes          int64
	State                  QueueState
	LeaseOwner             string
	LeaseVersion           int64
	Attempts               int32
}

type RecordMetadata struct {
	ID              string
	OwnerID         string
	Purpose         Purpose
	Kind            ItemKind
	ConsentVersion  int64
	CreatedAt       time.Time
	ExpiresAt       time.Time
	PayloadBytes    int64
	AttachmentBytes int64
	AttachmentCount int32
	State           string
}

type Content struct {
	Metadata RecordMetadata
	Payload  []byte
}

type AttachmentMetadata struct {
	ID        string
	RecordID  string
	FileName  string
	MediaType string
	SizeBytes int64
	SHA256    string
}

type AttachmentContent struct {
	Metadata AttachmentMetadata
	Content  []byte
}

type AdminGrant struct {
	OwnerID    string
	AdminID    string
	Capability AdminCapability
	Granted    bool
	GrantedBy  string
	Version    int64
	UpdatedAt  time.Time
}

type Query struct {
	Purpose  Purpose
	BeforeID string
	Limit    int32
}

type Page struct {
	Items      []RecordMetadata
	NextBefore string
}

type DeleteState string

const (
	DeletePending   DeleteState = "pending"
	DeleteFailed    DeleteState = "failed"
	DeleteCompleted DeleteState = "completed"
)

type DeleteTombstone struct {
	ID          string
	OwnerID     string
	RecordID    string
	RequestedBy string
	State       DeleteState
	Attempts    int32
	FailureCode string
	RequestedAt time.Time
	CompletedAt *time.Time
	UpdatedAt   time.Time
}

func ValidRecordID(id string) bool     { return validRecordID(id, 'v') }
func ValidQueueItemID(id string) bool  { return validRecordID(id, 'j') }
func ValidAttachmentID(id string) bool { return validRecordID(id, 'g') }
func ValidTombstoneID(id string) bool  { return validRecordID(id, 'l') }

func NewRecordID() (string, error)     { return newRecordID('v') }
func NewQueueItemID() (string, error)  { return newRecordID('j') }
func NewAttachmentID() (string, error) { return newRecordID('g') }
func NewTombstoneID() (string, error)  { return newRecordID('l') }

func newRecordID(prefix byte) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	value := [8]byte{prefix}
	max := big.NewInt(int64(len(alphabet)))
	for index := 1; index < len(value); index++ {
		number, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate record ID: %w", err)
		}
		value[index] = alphabet[number.Int64()]
	}
	return string(value[:]), nil
}

func validRecordID(id string, prefix byte) bool {
	if len(id) != 8 || id[0] != prefix {
		return false
	}
	for i := 1; i < len(id); i++ {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789", rune(id[i])) {
			return false
		}
	}
	return true
}
