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
)

const SnapshotCursorVersion = 2

const (
	SnapshotScopeObjectType = "personal"
	SnapshotScopeObjectID   = "all"
)

const maxSnapshotCursorBytes = 2048

// SnapshotCursor fixes the account, personal-object scope, and keyset position.
// It is signed by the application before it is returned to a client.
type SnapshotCursor struct {
	Version           int    `json:"v"`
	OwnerID           string `json:"owner_id"`
	ServiceInstanceID string `json:"service_instance_id"`
	ObjectType        string `json:"object_type"`
	ObjectID          string `json:"object_id"`
	Watermark         int64  `json:"watermark"`
	AfterObjectType   string `json:"after_object_type,omitempty"`
	AfterObjectID     string `json:"after_object_id,omitempty"`
}

// SnapshotBatch is the bounded store result before the application signs its
// next-page cursor.
type SnapshotBatch struct {
	ServiceInstanceID string
	Watermark         int64
	Items             []Object
	AfterObjectType   string
	AfterObjectID     string
	HasMore           bool
}

type Change struct {
	ChangeSequence int64  `json:"change_sequence"`
	ObjectType     string `json:"object_type"`
	ObjectID       string `json:"object_id"`
	Revision       int64  `json:"revision"`
	Operation      string `json:"operation"`
	MutationID     string `json:"mutation_id,omitempty"`
}

type ChangePage struct {
	Items             []Change `json:"items"`
	ThroughSequence   int64    `json:"through_sequence"`
	NextAfterSequence int64    `json:"next_after_sequence"`
	HasMore           bool     `json:"has_more"`
}

func EncodeSnapshotCursor(cursor SnapshotCursor, dataKey []byte) (string, error) {
	if err := validateSnapshotCursor(cursor); err != nil {
		return "", err
	}
	key, err := deriveSnapshotCursorKey(dataKey)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode personal snapshot cursor: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func DecodeSnapshotCursor(value string, dataKey []byte) (SnapshotCursor, error) {
	if value == "" || len(value) > maxSnapshotCursorBytes {
		return SnapshotCursor{}, errors.New("invalid personal snapshot cursor")
	}
	parts := bytes.SplitN([]byte(value), []byte{'.'}, 2)
	if len(parts) != 2 {
		return SnapshotCursor{}, errors.New("invalid personal snapshot cursor")
	}
	payload, err := base64.RawURLEncoding.DecodeString(string(parts[0]))
	if err != nil {
		return SnapshotCursor{}, errors.New("invalid personal snapshot cursor")
	}
	signature, err := base64.RawURLEncoding.DecodeString(string(parts[1]))
	if err != nil || len(signature) != sha256.Size {
		return SnapshotCursor{}, errors.New("invalid personal snapshot cursor")
	}
	key, err := deriveSnapshotCursorKey(dataKey)
	if err != nil {
		return SnapshotCursor{}, err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return SnapshotCursor{}, errors.New("invalid personal snapshot cursor")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var cursor SnapshotCursor
	if err := decoder.Decode(&cursor); err != nil {
		return SnapshotCursor{}, errors.New("invalid personal snapshot cursor")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return SnapshotCursor{}, errors.New("invalid personal snapshot cursor")
	}
	if err := validateSnapshotCursor(cursor); err != nil {
		return SnapshotCursor{}, errors.New("invalid personal snapshot cursor")
	}
	return cursor, nil
}

func deriveSnapshotCursorKey(dataKey []byte) ([]byte, error) {
	if len(dataKey) != 32 {
		return nil, errors.New("personal snapshot cursor key must be 32 bytes")
	}
	mac := hmac.New(sha256.New, dataKey)
	_, _ = mac.Write([]byte("morde/personal-sync/snapshot-cursor/v2"))
	return mac.Sum(nil), nil
}

func validateSnapshotCursor(cursor SnapshotCursor) error {
	if cursor.Version != SnapshotCursorVersion || cursor.OwnerID == "" || len(cursor.OwnerID) > 128 ||
		cursor.ServiceInstanceID == "" || len(cursor.ServiceInstanceID) > 64 ||
		cursor.ObjectType != SnapshotScopeObjectType || cursor.ObjectID != SnapshotScopeObjectID || cursor.Watermark < 0 {
		return errors.New("invalid personal snapshot cursor")
	}
	if (cursor.AfterObjectType == "") != (cursor.AfterObjectID == "") {
		return errors.New("invalid personal snapshot cursor")
	}
	if cursor.AfterObjectType != "" && !IsPersonalSyncObjectType(cursor.AfterObjectType) {
		return errors.New("invalid personal snapshot cursor")
	}
	if cursor.AfterObjectType != "" && ValidateObjectIdentifier(cursor.AfterObjectType, cursor.AfterObjectID) != nil {
		return errors.New("invalid personal snapshot cursor")
	}
	return nil
}
