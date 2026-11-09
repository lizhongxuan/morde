package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
)

const (
	ConversationMessageChunkBytes = 16 << 10
	MaxConversationMessageBytes   = 8 << 20
)

type InitializeConversationMessageUploadCommand struct {
	AcceptConversationMessageCommand
	TotalBytes  int
	TotalChunks int
	SHA256      string
}

type ConversationMessageUpload struct {
	ClientMessageID   string
	State             string
	TotalBytes        int
	TotalChunks       int
	ReceivedSequences []int
	Message           *conversation.Message
}

type ConversationMessageUploadInit struct {
	Receipt     ConversationMessageReceipt
	TotalBytes  int
	TotalChunks int
	ContentHash []byte
	RequestHash []byte
}

type ConversationMessageUploadStore interface {
	InitializeConversationMessageUpload(context.Context, ConversationMessageUploadInit) (ConversationMessageUpload, error)
	PutConversationMessageChunk(context.Context, ConversationMessageReceipt, int, []byte, []byte) error
	GetConversationMessageUpload(context.Context, ConversationMessageReceipt) (ConversationMessageUpload, error)
	CommitConversationMessageUpload(context.Context, ConversationMessageReceipt) (conversation.Message, error)
}

type ConversationMessageUploadCleanupStore interface {
	CleanupExpiredConversationMessageUploads(context.Context, time.Time, int32) (int32, error)
}

func (s *TaskService) CleanupExpiredConversationMessageUploads(ctx context.Context, now time.Time, limit int32) (int32, error) {
	if now.IsZero() || limit < 1 || limit > 1000 {
		return 0, domain.InvalidArgument("limit", "conversation message upload cleanup request is invalid")
	}
	store, ok := s.store.(ConversationMessageUploadCleanupStore)
	if !ok {
		return 0, domain.NewError(domain.CodeUnsupported, "conversation message upload cleanup is unavailable")
	}
	return store.CleanupExpiredConversationMessageUploads(ctx, now.UTC(), limit)
}

func decodeConversationSHA256(value string) ([]byte, error) {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return nil, domain.InvalidArgument("sha256", "sha256 must be 64 lowercase hexadecimal characters")
	}
	digest, err := hex.DecodeString(value)
	if err != nil || len(digest) != sha256.Size {
		return nil, domain.InvalidArgument("sha256", "sha256 must be 64 lowercase hexadecimal characters")
	}
	return digest, nil
}

func (s *TaskService) InitializeConversationMessageUpload(ctx context.Context, principal identity.Principal, conversationID string, command InitializeConversationMessageUploadCommand) (ConversationMessageUpload, error) {
	if command.TotalBytes < 1 || command.TotalBytes > MaxConversationMessageBytes ||
		command.TotalChunks != (command.TotalBytes+ConversationMessageChunkBytes-1)/ConversationMessageChunkBytes {
		return ConversationMessageUpload{}, domain.InvalidArgument("total_bytes", "message size or chunk count is invalid")
	}
	digest, err := decodeConversationSHA256(command.SHA256)
	if err != nil {
		return ConversationMessageUpload{}, err
	}
	receipt, err := s.prepareConversationMessage(ctx, principal, conversationID, command.AcceptConversationMessageCommand)
	if err != nil {
		return ConversationMessageUpload{}, err
	}
	requestHash, err := hashIdempotencyRequest(struct {
		ConversationID string                   `json:"conversation_id"`
		Input          ConversationRoutingInput `json:"input"`
		TotalBytes     int                      `json:"total_bytes"`
		TotalChunks    int                      `json:"total_chunks"`
		SHA256         string                   `json:"sha256"`
	}{conversationID, receipt.Input, command.TotalBytes, command.TotalChunks, command.SHA256})
	if err != nil {
		return ConversationMessageUpload{}, err
	}
	store, ok := s.store.(ConversationMessageUploadStore)
	if !ok {
		return ConversationMessageUpload{}, domain.NewError(domain.CodeUnsupported, "conversation message uploads are unavailable")
	}
	return store.InitializeConversationMessageUpload(ctx, ConversationMessageUploadInit{
		Receipt: receipt, TotalBytes: command.TotalBytes, TotalChunks: command.TotalChunks,
		ContentHash: digest, RequestHash: requestHash,
	})
}

func (s *TaskService) conversationUploadScope(ctx context.Context, principal identity.Principal, projectID, conversationID, clientID, instanceID string) (ConversationMessageReceipt, ConversationMessageUploadStore, error) {
	receipt, err := s.prepareConversationMessage(ctx, principal, conversationID, AcceptConversationMessageCommand{
		ProjectID: projectID, ServiceInstanceID: instanceID, ClientMessageID: clientID, IdempotencyKey: clientID,
	})
	if err != nil {
		return ConversationMessageReceipt{}, nil, err
	}
	store, ok := s.store.(ConversationMessageUploadStore)
	if !ok {
		return ConversationMessageReceipt{}, nil, domain.NewError(domain.CodeUnsupported, "conversation message uploads are unavailable")
	}
	return receipt, store, nil
}

func (s *TaskService) PutConversationMessageChunk(ctx context.Context, principal identity.Principal, projectID, conversationID, clientID, instanceID string, sequence int, content []byte, digestHex string) error {
	if sequence < 0 || sequence >= MaxConversationMessageBytes/ConversationMessageChunkBytes || len(content) < 1 || len(content) > ConversationMessageChunkBytes {
		return domain.InvalidArgument("chunk", "message chunk is invalid")
	}
	digest, err := decodeConversationSHA256(digestHex)
	if err != nil {
		return err
	}
	actual := sha256.Sum256(content)
	if !bytes.Equal(digest, actual[:]) {
		return domain.InvalidArgument("sha256", "chunk digest does not match content")
	}
	receipt, store, err := s.conversationUploadScope(ctx, principal, projectID, conversationID, clientID, instanceID)
	if err != nil {
		return err
	}
	return store.PutConversationMessageChunk(ctx, receipt, sequence, content, digest)
}

func (s *TaskService) GetConversationMessageUpload(ctx context.Context, principal identity.Principal, projectID, conversationID, clientID, instanceID string) (ConversationMessageUpload, error) {
	receipt, store, err := s.conversationUploadScope(ctx, principal, projectID, conversationID, clientID, instanceID)
	if err != nil {
		return ConversationMessageUpload{}, err
	}
	return store.GetConversationMessageUpload(ctx, receipt)
}

func (s *TaskService) CommitConversationMessageUpload(ctx context.Context, principal identity.Principal, projectID, conversationID, clientID, instanceID, idempotencyKey string) (conversation.Message, error) {
	if clientID != idempotencyKey {
		return conversation.Message{}, domain.InvalidArgument("client_message_id", "client message id and idempotency key must match")
	}
	receipt, store, err := s.conversationUploadScope(ctx, principal, projectID, conversationID, clientID, instanceID)
	if err != nil {
		return conversation.Message{}, err
	}
	return store.CommitConversationMessageUpload(ctx, receipt)
}
