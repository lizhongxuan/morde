package personal

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"strings"

	"morde/server/internal/domain"
)

const (
	MaxAttachmentChunkBytes          int64 = 4 << 20
	DefaultMaxAttachmentBytes        int64 = 300 << 20
	// MaxAttachmentBytes keeps chunk indexes within int32.
	MaxAttachmentBytes               int64 = (1<<31 - 1) * MaxAttachmentChunkBytes
	PersonalAttachmentUploadIDPrefix       = "pu_"
	PersonalAttachmentIDPrefix             = "pa_"
)

type AttachmentUpload struct {
	UploadID     string
	OwnerID      string
	ObjectType   string
	ObjectID     string
	BaseRevision int64
	SizeBytes    int64
	SHA256       string
	MediaType    string
	RequestHash  []byte
}

type AttachmentReservation struct {
	OwnerID       string
	UploadID      string
	ReservationID string
	SizeBytes     int64
}

type AttachmentChunk struct {
	UploadID  string
	Sequence  int32
	SizeBytes int64
	SHA256    string
}

type AttachmentUploadReceipt struct {
	UploadID      string
	ReservationID string
	State         AttachmentUploadState
	ChunkCount    int32
	Replay        bool
}

type AttachmentUploadState string

const (
	AttachmentUploadPending   AttachmentUploadState = "pending"
	AttachmentUploadActive    AttachmentUploadState = "active"
	AttachmentUploadCommitted AttachmentUploadState = "committed"
	AttachmentUploadCancelled AttachmentUploadState = "cancelled"
	AttachmentUploadExpired   AttachmentUploadState = "expired"
)

type AttachmentChunkReceipt struct {
	UploadID string
	Sequence int32
	Replay   bool
}

type AttachmentCommitReceipt struct {
	AttachmentID          string
	ObjectType            string
	ObjectID              string
	ResultRevision        int64
	AccountChangeSequence int64
	Replay                bool
}

type AttachmentMetadata struct {
	AttachmentID string
	ObjectType   string
	ObjectID     string
	MediaType    string
	SizeBytes    int64
	SHA256       string
}

func ValidateAttachmentUpload(value AttachmentUpload) error {
	if !ValidPersonalAttachmentUploadID(value.UploadID) || !domain.ValidUserID(value.OwnerID) || value.ObjectType != ObjectTypeLibraryItem {
		return domain.InvalidArgument("attachment_upload", "personal attachment upload identity is invalid")
	}
	if err := ValidateObjectIdentifier(value.ObjectType, value.ObjectID); err != nil {
		return err
	}
	if value.BaseRevision < 0 {
		return domain.InvalidArgument("base_revision", "personal attachment base revision is invalid")
	}
	if value.SizeBytes < 0 || value.SizeBytes > MaxAttachmentBytes {
		return domain.NewError(domain.CodeRequestTooLarge, "personal attachment size is outside the allowed range")
	}
	if !validAttachmentSHA256(value.SHA256) || !validAttachmentMediaType(value.MediaType) {
		return domain.InvalidArgument("attachment_upload", "personal attachment digest or media type is invalid")
	}
	if value.SizeBytes == 0 && value.SHA256 != emptySHA256 {
		return domain.InvalidArgument("sha256", "empty personal attachment digest is invalid")
	}
	if len(value.RequestHash) != 0 {
		if len(value.RequestHash) != sha256.Size {
			return domain.InvalidArgument("request_hash", "personal attachment request hash is invalid")
		}
		expectedHash, err := HashAttachmentUploadContent(value)
		if err != nil || !bytes.Equal(value.RequestHash, expectedHash) {
			return domain.InvalidArgument("request_hash", "personal attachment request hash is invalid")
		}
	}
	return nil
}

func ValidateAttachmentIdempotencyKey(value string) error {
	if !validStableID(value, 8) {
		return domain.InvalidArgument("idempotency_key", "personal attachment idempotency key is invalid")
	}
	return nil
}

func ValidateAttachmentReservation(value AttachmentReservation) error {
	if !domain.ValidUserID(value.OwnerID) || !ValidPersonalAttachmentUploadID(value.UploadID) ||
		!validStableID(value.ReservationID, 8) || value.SizeBytes < 0 || value.SizeBytes > MaxAttachmentBytes {
		return domain.InvalidArgument("attachment_reservation", "personal attachment reservation is invalid")
	}
	return nil
}

func ValidAttachmentUploadState(value AttachmentUploadState) bool {
	switch value {
	case AttachmentUploadPending, AttachmentUploadActive, AttachmentUploadCommitted,
		AttachmentUploadCancelled, AttachmentUploadExpired:
		return true
	default:
		return false
	}
}

func HashAttachmentUploadContent(value AttachmentUpload) ([]byte, error) {
	encoded, err := json.Marshal(struct {
		ObjectType   string `json:"object_type"`
		ObjectID     string `json:"object_id"`
		BaseRevision int64  `json:"base_revision"`
		SizeBytes    int64  `json:"size_bytes"`
		SHA256       string `json:"sha256"`
		MediaType    string `json:"media_type"`
	}{
		ObjectType: value.ObjectType, ObjectID: value.ObjectID, BaseRevision: value.BaseRevision,
		SizeBytes: value.SizeBytes, SHA256: value.SHA256, MediaType: value.MediaType,
	})
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(encoded)
	return hash[:], nil
}

func AttachmentChunkCount(sizeBytes int64) (int32, error) {
	if sizeBytes < 0 || sizeBytes > MaxAttachmentBytes {
		return 0, domain.InvalidArgument("size_bytes", "personal attachment size is invalid")
	}
	if sizeBytes == 0 {
		return 0, nil
	}
	return int32((sizeBytes-1)/MaxAttachmentChunkBytes + 1), nil
}

func ExpectedAttachmentChunkSize(sizeBytes int64, sequence int32) (int64, error) {
	count, err := AttachmentChunkCount(sizeBytes)
	if err != nil {
		return 0, err
	}
	if sequence < 0 || sequence >= count {
		return 0, domain.InvalidArgument("sequence", "personal attachment chunk sequence is invalid")
	}
	remaining := sizeBytes - int64(sequence)*MaxAttachmentChunkBytes
	if remaining > MaxAttachmentChunkBytes {
		remaining = MaxAttachmentChunkBytes
	}
	return remaining, nil
}

func ValidateAttachmentChunk(value AttachmentChunk) error {
	if !ValidPersonalAttachmentUploadID(value.UploadID) || value.Sequence < 0 ||
		value.SizeBytes < 1 || value.SizeBytes > MaxAttachmentChunkBytes || !validAttachmentSHA256(value.SHA256) {
		return domain.InvalidArgument("attachment_chunk", "personal attachment chunk is invalid")
	}
	return nil
}

func ValidateAttachmentChunkForUpload(upload AttachmentUpload, chunk AttachmentChunk) error {
	if err := ValidateAttachmentUpload(upload); err != nil {
		return err
	}
	if err := ValidateAttachmentChunk(chunk); err != nil {
		return err
	}
	if chunk.UploadID != upload.UploadID {
		return domain.NewError(domain.CodeNotFound, "personal attachment upload not found")
	}
	expectedSize, err := ExpectedAttachmentChunkSize(upload.SizeBytes, chunk.Sequence)
	if err != nil || chunk.SizeBytes != expectedSize {
		return domain.InvalidArgument("attachment_chunk", "personal attachment chunk does not match the upload plan")
	}
	return nil
}

func ValidateAttachmentMetadata(value AttachmentMetadata) error {
	if !ValidPersonalAttachmentID(value.AttachmentID) || value.ObjectType != ObjectTypeLibraryItem ||
		ValidateObjectIdentifier(value.ObjectType, value.ObjectID) != nil || value.SizeBytes < 0 ||
		value.SizeBytes > MaxAttachmentBytes || !validAttachmentSHA256(value.SHA256) ||
		!validAttachmentMediaType(value.MediaType) || (value.SizeBytes == 0 && value.SHA256 != emptySHA256) {
		return errors.New("personal attachment metadata is invalid")
	}
	return nil
}

func NewPersonalAttachmentUploadID() (string, error) {
	return newPersonalAttachmentID(PersonalAttachmentUploadIDPrefix)
}

func NewPersonalAttachmentID() (string, error) {
	return newPersonalAttachmentID(PersonalAttachmentIDPrefix)
}

func ValidPersonalAttachmentUploadID(value string) bool {
	return validPersonalAttachmentID(value, PersonalAttachmentUploadIDPrefix)
}

func ValidPersonalAttachmentID(value string) bool {
	return validPersonalAttachmentID(value, PersonalAttachmentIDPrefix)
}

func newPersonalAttachmentID(prefix string) (string, error) {
	var random [16]byte
	if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
		return "", fmt.Errorf("generate personal attachment id: %w", err)
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func validPersonalAttachmentID(value, prefix string) bool {
	if len(value) != len(prefix)+16*2 || !strings.HasPrefix(value, prefix) || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value[len(prefix):])
	return err == nil
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func validAttachmentSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validAttachmentMediaType(value string) bool {
	if len(value) == 0 || len(value) > 128 || strings.Count(value, "/") != 1 {
		return false
	}
	typeName, subtype, ok := strings.Cut(value, "/")
	if !ok || typeName == "." || typeName == ".." || subtype == "." || subtype == ".." ||
		!validAttachmentMediaToken(typeName) || !validAttachmentMediaToken(subtype) {
		return false
	}
	parsed, params, err := mime.ParseMediaType(value)
	return err == nil && parsed == value && len(params) == 0
}

func validAttachmentMediaToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)) {
			continue
		}
		return false
	}
	return true
}
