package conversation

import (
	"strings"
	"testing"

	"morde/server/internal/domain"
)

func validHistoryImport() HistoryImport {
	return HistoryImport{
		SourceID: "openai-export", DeviceAccountID: "account-01", SourceConversationID: "chat-01",
		Title: "Imported chat",
		Messages: []HistoryMessage{
			{SourceMessageID: "message-01", Role: RoleUser, Content: "question", AttachmentIDs: []string{"pa_0123456789abcdef0123456789abcdef"}},
			{SourceMessageID: "message-02", Role: RoleAssistant, Content: "answer"},
		},
	}
}

func TestValidateHistoryImportAcceptsBoundedUserConfirmedText(t *testing.T) {
	if err := ValidateHistoryImport(validHistoryImport()); err != nil {
		t.Fatalf("ValidateHistoryImport() = %v", err)
	}
}

func TestValidateHistoryImportRejectsInvalidIdentityRoleAndDuplicate(t *testing.T) {
	tests := map[string]func(*HistoryImport){
		"empty source":          func(value *HistoryImport) { value.SourceID = " " },
		"invalid target":        func(value *HistoryImport) { value.TargetConversationID = "not-a-conversation" },
		"unsupported role":      func(value *HistoryImport) { value.Messages[0].Role = Role("tool") },
		"duplicate message id":  func(value *HistoryImport) { value.Messages[1].SourceMessageID = value.Messages[0].SourceMessageID },
		"invalid attachment id": func(value *HistoryImport) { value.Messages[0].AttachmentIDs[0] = "a1234567" },
		"empty text":            func(value *HistoryImport) { value.Messages[0].Content = "" },
		"invalid utf8":          func(value *HistoryImport) { value.Messages[0].Content = string([]byte{0xff}) },
		"nul text":              func(value *HistoryImport) { value.Messages[0].Content = "a\x00b" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := validHistoryImport()
			mutate(&value)
			if err := ValidateHistoryImport(value); err == nil {
				t.Fatal("ValidateHistoryImport() unexpectedly succeeded")
			}
		})
	}
}

func TestValidateHistoryImportEnforcesCountAndByteBounds(t *testing.T) {
	value := validHistoryImport()
	value.Messages[0].Content = strings.Repeat("x", MaxHistoryImportMessageBytes+1)
	if err := ValidateHistoryImport(value); domain.CodeOf(err) != domain.CodeRequestTooLarge {
		t.Fatalf("oversized message error = %v, want request_too_large", err)
	}

	value = validHistoryImport()
	value.Messages[0].Content = strings.Repeat("x", MaxHistoryImportTotalBytes/2)
	value.Messages[1].Content = strings.Repeat("y", MaxHistoryImportTotalBytes/2)
	if err := ValidateHistoryImport(value); domain.CodeOf(err) != domain.CodeRequestTooLarge {
		t.Fatalf("oversized total error = %v, want request_too_large", err)
	}

	value = validHistoryImport()
	value.Messages = make([]HistoryMessage, MaxHistoryImportMessages+1)
	if err := ValidateHistoryImport(value); err == nil {
		t.Fatal("message count above the limit was accepted")
	}
}

func TestHashHistoryMessageCoversRoleTextAndAttachmentOrder(t *testing.T) {
	message := HistoryMessage{SourceMessageID: "source-1", Role: RoleUser, Content: "text", AttachmentIDs: []string{
		"pa_0123456789abcdef0123456789abcdef", "pa_fedcba9876543210fedcba9876543210",
	}}
	base, err := HashHistoryMessage(message)
	if err != nil {
		t.Fatal(err)
	}
	variants := []HistoryMessage{
		{Role: RoleAssistant, Content: message.Content, AttachmentIDs: message.AttachmentIDs},
		{Role: message.Role, Content: "changed", AttachmentIDs: message.AttachmentIDs},
		{Role: message.Role, Content: message.Content, AttachmentIDs: []string{
			"pa_fedcba9876543210fedcba9876543210", "pa_0123456789abcdef0123456789abcdef",
		}},
	}
	for _, variant := range variants {
		got, err := HashHistoryMessage(variant)
		if err != nil || got == base {
			t.Fatalf("changed message hash = %x, %v; want different from %x", got, err, base)
		}
	}
}
