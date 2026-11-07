package api

import (
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

func TestConversationResultResponseIncludesOnlyValidSources(t *testing.T) {
	message := conversation.Message{ID: "m1234567", ConversationID: "c1234567", Sequence: 2,
		Role: conversation.RoleAssistant, Content: "Answer A\n\nAnswer B", CreatedAt: time.Now().UTC(),
		Sources: []conversation.MessageSource{{UserMessageID: "m7654321", TaskID: "q1234567", TaskRevision: 2, EventSequence: 5, Content: "Answer A"},
			{UserMessageID: "m2345678", TaskID: "q2345678", TaskRevision: 0, EventSequence: 6, Content: "Answer B"}}}
	view, err := toMessageResponse(message)
	if err != nil || len(view.Sources) != 2 || view.Sources[0].TaskRevision != 2 || view.Sources[1].Content != "Answer B" {
		t.Fatalf("combined message sources=%+v err=%v", view.Sources, err)
	}
	message.Sources[0].EventSequence = 0
	if _, err := toMessageResponse(message); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("invalid result source was accepted: %v", err)
	}
}

func TestProjectMessageAttachmentCannotUsePersonalScope(t *testing.T) {
	message := conversation.Message{ID: "m1234567", ConversationID: "c1234567", OwnerID: "u1234567", ActorID: "u1234567",
		Sequence: 1, Role: conversation.RoleUser, Content: "document", CreatedAt: time.Now().UTC(),
		AttachmentIDs: []string{"pca_0123456789abcdef0123456789abcdef"}}
	view, err := toProjectConversationMessageResponse(message)
	if err != nil || len(view.AttachmentIDs) != 1 || view.AttachmentIDs[0] != message.AttachmentIDs[0] {
		t.Fatalf("project attachment response = %+v err=%v", view, err)
	}
	if _, err := toMessageResponse(message); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("project attachment was exposed through personal message contract: %v", err)
	}
	message.AttachmentIDs[0] = "pa_0123456789abcdef0123456789abcdef"
	if _, err := toProjectConversationMessageResponse(message); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("personal attachment was exposed through project message contract: %v", err)
	}
}
