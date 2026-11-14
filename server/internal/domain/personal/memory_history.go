package personal

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"morde/server/internal/domain"
)

const MemoryHistoryCursorVersion = 1

const maxMemoryHistoryCursorBytes = 2048

const (
	MemoryHistorySourceAvailable   = "available"
	MemoryHistorySourceUnrecorded  = "unrecorded"
	MemoryHistorySourceUnavailable = "unavailable"
)

// MemoryHistoryCursor fixes the owner, memory object, and descending revision
// position for a signed history page request.
type MemoryHistoryCursor struct {
	Version           int    `json:"v"`
	OwnerID           string `json:"owner_id"`
	ServiceInstanceID string `json:"service_instance_id"`
	ObjectType        string `json:"object_type"`
	ObjectID          string `json:"object_id"`
	AfterRevision     int64  `json:"after_revision"`
}

type MemoryHistorySource struct {
	Kind           string `json:"kind"`
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id,omitempty"`
}

// MemoryRevision uses a nil body when the body is unavailable or provenance
// policy requires withholding it. Older legacy bodies are never reconstructed.
type MemoryRevision struct {
	Revision     int64                 `json:"revision"`
	ChangedAt    time.Time             `json:"changed_at"`
	Body         json.RawMessage       `json:"body"`
	Restorable   bool                  `json:"restorable"`
	SourceStatus string                `json:"source_status"`
	Sources      []MemoryHistorySource `json:"sources"`
}

type MemoryHistoryBatch struct {
	ServiceInstanceID string
	Items             []MemoryRevision
	HasMore           bool
}

func EncodeMemoryHistoryCursor(cursor MemoryHistoryCursor, dataKey []byte) (string, error) {
	if err := validateMemoryHistoryCursor(cursor); err != nil {
		return "", err
	}
	key, err := deriveMemoryHistoryCursorKey(dataKey)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode personal memory history cursor: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func DecodeMemoryHistoryCursor(value string, dataKey []byte) (MemoryHistoryCursor, error) {
	if value == "" || len(value) > maxMemoryHistoryCursorBytes {
		return MemoryHistoryCursor{}, errors.New("invalid personal memory history cursor")
	}
	parts := bytes.SplitN([]byte(value), []byte{'.'}, 2)
	if len(parts) != 2 {
		return MemoryHistoryCursor{}, errors.New("invalid personal memory history cursor")
	}
	payload, err := base64.RawURLEncoding.DecodeString(string(parts[0]))
	if err != nil {
		return MemoryHistoryCursor{}, errors.New("invalid personal memory history cursor")
	}
	signature, err := base64.RawURLEncoding.DecodeString(string(parts[1]))
	if err != nil || len(signature) != sha256.Size {
		return MemoryHistoryCursor{}, errors.New("invalid personal memory history cursor")
	}
	key, err := deriveMemoryHistoryCursorKey(dataKey)
	if err != nil {
		return MemoryHistoryCursor{}, err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return MemoryHistoryCursor{}, errors.New("invalid personal memory history cursor")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var cursor MemoryHistoryCursor
	if err := decoder.Decode(&cursor); err != nil {
		return MemoryHistoryCursor{}, errors.New("invalid personal memory history cursor")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return MemoryHistoryCursor{}, errors.New("invalid personal memory history cursor")
	}
	if err := validateMemoryHistoryCursor(cursor); err != nil {
		return MemoryHistoryCursor{}, errors.New("invalid personal memory history cursor")
	}
	return cursor, nil
}

func deriveMemoryHistoryCursorKey(dataKey []byte) ([]byte, error) {
	if len(dataKey) != 32 {
		return nil, errors.New("personal memory history cursor key must be 32 bytes")
	}
	mac := hmac.New(sha256.New, dataKey)
	_, _ = mac.Write([]byte("morde/personal-memory-history/cursor/v1"))
	return mac.Sum(nil), nil
}

func validateMemoryHistoryCursor(cursor MemoryHistoryCursor) error {
	if cursor.Version != MemoryHistoryCursorVersion || !domain.ValidUserID(cursor.OwnerID) ||
		cursor.ServiceInstanceID == "" || len(cursor.ServiceInstanceID) > 64 ||
		cursor.ObjectType != ObjectTypeMemory || ValidateObjectIdentifier(cursor.ObjectType, cursor.ObjectID) != nil ||
		cursor.AfterRevision < 1 {
		return errors.New("invalid personal memory history cursor")
	}
	return nil
}
