package api

import (
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

func TestConversationMessageAssociationResponseFieldsAreOptionalAndScoped(t *testing.T) {
	message := conversation.Message{ID: "m1234567", ConversationID: "c1234567", Sequence: 2,
		Role: conversation.RoleUser, Content: "same request", CreatedAt: time.Now().UTC()}
	view, err := toMessageResponse(message)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["duplicate_of_message_id"]; ok {
		t.Fatalf("unassociated message exposed duplicate target: %s", encoded)
	}
	if _, ok := body["related_task_id"]; ok {
		t.Fatalf("message without a task exposed related task: %s", encoded)
	}

	message.DuplicateOfMessageID = "m7654321"
	message.RelatedTaskID = "q1234567"
	view, err = toMessageResponse(message)
	if err != nil || view.DuplicateOfMessageID != message.DuplicateOfMessageID || view.RelatedTaskID != message.RelatedTaskID {
		t.Fatalf("association fields were not projected: %+v err=%v", view, err)
	}
	message.DuplicateOfMessageID = message.ID
	if _, err := toMessageResponse(message); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("self-referencing duplicate was accepted: %v", err)
	}
	message.DuplicateOfMessageID = "m7654321"
	message.TaskID = "q7654321"
	if _, err := toMessageResponse(message); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("duplicate message with its own Task was accepted: %v", err)
	}
	message.TaskID = ""
	message.DuplicateOfMessageID = ""
	if _, err := toMessageResponse(message); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("unassociated message with a related Task was accepted: %v", err)
	}
	message.RelatedTaskID = "bad"
	if _, err := toMessageResponse(message); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("invalid related task id was accepted: %v", err)
	}
}
