package record

import (
	"mime"
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
	Purpose    Purpose
	Kind       ItemKind
	Payload    []byte
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
	if len(value.Payload) == 0 || int64(len(value.Payload)) > MaxCapturePayloadBytes {
		return domain.NewError(domain.CodeRequestTooLarge, "record payload is empty or exceeds the allowed size")
	}
	if len(value.Attachments) > MaxCaptureAttachments {
		return domain.NewError(domain.CodeRequestTooLarge, "record attachment count exceeds the allowed limit")
	}
	total := int64(len(value.Payload))
	for _, attachment := range value.Attachments {
		if !validFileName(attachment.FileName) || !validMediaType(attachment.MediaType) {
			return domain.InvalidArgument("record_attachment", "record attachment metadata is invalid")
		}
		size := int64(len(attachment.Content))
		if size < 1 || size > MaxCaptureAttachmentBytes || total > MaxCaptureTotalBytes-size {
			return domain.NewError(domain.CodeRequestTooLarge, "record attachment size exceeds the allowed limit")
		}
		total += size
	}
	if total > MaxCaptureTotalBytes {
		return domain.NewError(domain.CodeRequestTooLarge, "record capture size exceeds the allowed limit")
	}
	return nil
}

func validFileName(value string) bool {
	return value != "" && len(value) <= 255 && utf8.ValidString(value) && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "/\\\x00") && strings.IndexFunc(value, isControl) < 0
}

func validMediaType(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	parsed, _, err := mime.ParseMediaType(value)
	return err == nil && parsed != ""
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
	ConsentVersion int64
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

func ValidRecordID(id string) bool { return validRecordID(id, 'v') }
func ValidQueueItemID(id string) bool { return validRecordID(id, 'j') }
func ValidAttachmentID(id string) bool { return validRecordID(id, 'g') }
func ValidTombstoneID(id string) bool { return validRecordID(id, 'l') }

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
