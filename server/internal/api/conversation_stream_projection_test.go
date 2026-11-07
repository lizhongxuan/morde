package api

import (
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

func TestConversationStreamOutputProjection(t *testing.T) {
	messageID, err := domain.NewMessageID()
	if err != nil {
		t.Fatal(err)
	}
	conversationID, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := domain.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	revision, event := int64(0), int64(3)
	value := conversation.Message{ID: messageID, ConversationID: conversationID, TaskID: taskID,
		Role: conversation.RoleAssistant, Content: "partial", Sequence: 1, CreatedAt: time.Now(),
		OutputStatus: conversation.OutputInterrupted, OutputTaskRevision: &revision}
	view, err := toMessageResponse(value)
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
	if body["output_status"] != "interrupted" || body["output_task_revision"] != float64(0) {
		t.Fatalf("interrupted output fields: %s", encoded)
	}
	if _, ok := body["output_result_event_sequence"]; ok {
		t.Fatalf("interrupted message reported a result: %s", encoded)
	}
	value.OutputStatus = conversation.OutputCompleted
	if _, err := toMessageResponse(value); err == nil {
		t.Fatal("completed stream without result evidence was accepted")
	}
	value.OutputResultEventSequence = &event
	if _, err := toMessageResponse(value); err != nil {
		t.Fatalf("completed stream rejected: %v", err)
	}
	value.Role = conversation.RoleUser
	if _, err := toMessageResponse(value); err == nil {
		t.Fatal("user message accepted assistant stream metadata")
	}
}
