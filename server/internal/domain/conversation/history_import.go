package conversation

import (
	"crypto/sha256"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/personal"
)

const (
	HistoryImportSourceKind          = "history_import"
	MaxHistoryImportMessages         = 200
	MaxHistoryImportMessageBytes     = 32 << 10
	MaxHistoryImportTotalBytes       = 1 << 20
	MaxHistoryImportAttachments      = 16
	MaxHistoryImportTotalAttachments = 512
	MaxHistoryImportIdentifierSize   = 128
)

// HistoryImport contains only user-confirmed text messages. Owner identity is
// deliberately omitted; callers derive it from the authenticated Principal.
type HistoryImport struct {
	SourceID             string           `json:"source_id"`
	DeviceAccountID      string           `json:"device_account_id"`
	SourceConversationID string           `json:"source_conversation_id"`
	TargetConversationID string           `json:"target_conversation_id,omitempty"`
	Title                string           `json:"title,omitempty"`
	Messages             []HistoryMessage `json:"messages"`
}

type HistoryMessage struct {
	SourceMessageID string   `json:"source_message_id"`
	Role            Role     `json:"role"`
	Content         string   `json:"content"`
	AttachmentIDs   []string `json:"attachment_ids,omitempty"`
}

type ImportedHistoryMessage struct {
	SourceMessageID string
	Message         Message
	AttachmentIDs   []string
	Replay          bool
}

type HistoryImportResult struct {
	Conversation Conversation
	Messages     []ImportedHistoryMessage
	Replay       bool
}

// ValidateHistoryImport validates bounded import data before it reaches an
// application service or persistence layer.
func ValidateHistoryImport(value HistoryImport) error {
	for _, field := range []struct {
		name string
		id   string
	}{
		{name: "source_id", id: value.SourceID},
		{name: "device_account_id", id: value.DeviceAccountID},
		{name: "source_conversation_id", id: value.SourceConversationID},
	} {
		if !validHistorySourceID(field.id) {
			return domain.InvalidArgument(field.name, "history import source identity is invalid")
		}
	}
	if value.TargetConversationID != "" && !domain.ValidConversationID(value.TargetConversationID) {
		return domain.InvalidArgument("target_conversation_id", "target conversation is invalid")
	}
	if !utf8.ValidString(value.Title) || strings.ContainsRune(value.Title, '\x00') || utf8.RuneCountInString(value.Title) > 120 {
		return domain.InvalidArgument("title", "history import title is invalid")
	}
	if len(value.Messages) == 0 || len(value.Messages) > MaxHistoryImportMessages {
		return domain.InvalidArgument("messages", "history import message count is outside the allowed range")
	}

	seenMessages := make(map[string]struct{}, len(value.Messages))
	var totalBytes int
	totalAttachments := 0
	for _, message := range value.Messages {
		if !validHistorySourceID(message.SourceMessageID) {
			return domain.InvalidArgument("messages", "history import message identity is invalid")
		}
		if _, exists := seenMessages[message.SourceMessageID]; exists {
			return domain.InvalidArgument("messages", "history import message identities must be unique")
		}
		seenMessages[message.SourceMessageID] = struct{}{}
		if message.Role != RoleUser && message.Role != RoleAssistant {
			return domain.InvalidArgument("messages", "history import supports only user and assistant text messages")
		}
		if message.Content == "" || !utf8.ValidString(message.Content) || strings.ContainsRune(message.Content, '\x00') {
			return domain.InvalidArgument("messages", "history import message text is invalid")
		}
		if len(message.Content) > MaxHistoryImportMessageBytes {
			return domain.NewError(domain.CodeRequestTooLarge, "history import message exceeds the size limit")
		}
		totalBytes += len(message.Content)
		if totalBytes > MaxHistoryImportTotalBytes {
			return domain.NewError(domain.CodeRequestTooLarge, "history import exceeds the total text size limit")
		}
		if len(message.AttachmentIDs) > MaxHistoryImportAttachments {
			return domain.InvalidArgument("messages", "history import attachment count exceeds the limit")
		}
		totalAttachments += len(message.AttachmentIDs)
		if totalAttachments > MaxHistoryImportTotalAttachments {
			return domain.InvalidArgument("messages", "history import attachment count exceeds the batch limit")
		}
		seenAttachments := make(map[string]struct{}, len(message.AttachmentIDs))
		for _, attachmentID := range message.AttachmentIDs {
			if !validPersonalAttachmentID(attachmentID) {
				return domain.InvalidArgument("messages", "history import attachment identity is invalid")
			}
			if _, exists := seenAttachments[attachmentID]; exists {
				return domain.InvalidArgument("messages", "history import attachment identities must be unique per message")
			}
			seenAttachments[attachmentID] = struct{}{}
			totalBytes += len(attachmentID)
			if totalBytes > MaxHistoryImportTotalBytes {
				return domain.NewError(domain.CodeRequestTooLarge, "history import exceeds the total request data limit")
			}
		}
	}
	return nil
}

// HashHistoryMessage returns the stable digest used to detect a source-key
// replay whose role, text, or attachment set changed.
func HashHistoryMessage(message HistoryMessage) ([32]byte, error) {
	encoded, err := json.Marshal(struct {
		Role          Role     `json:"role"`
		Content       string   `json:"content"`
		AttachmentIDs []string `json:"attachment_ids"`
	}{Role: message.Role, Content: message.Content, AttachmentIDs: message.AttachmentIDs})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func validHistorySourceID(value string) bool {
	if value == "" || len(value) > MaxHistoryImportIdentifierSize || !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validPersonalAttachmentID(value string) bool {
	return personal.ValidPersonalAttachmentID(value)
}
