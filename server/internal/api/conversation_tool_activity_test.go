package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestConversationMessageToolActivityDoesNotExposeInternalTask(t *testing.T) {
	now := time.Now().UTC()
	service := &conversationServiceStub{messagePage: app.MessagePage{Items: []conversation.Message{
		{ID: "m1234567", ConversationID: "c1234567", Sequence: 1, Role: conversation.RoleUser,
			Content: "Find a source", CreatedAt: now,
			ToolActivity: &conversation.ToolActivity{Tool: "web_search", Status: task.StepRunning}},
		{ID: "m7654321", ConversationID: "c1234567", Sequence: 2, Role: conversation.RoleUser,
			Content: "Another message", CreatedAt: now},
	}}}
	response := httptest.NewRecorder()
	taskAPIRouter(nil, service).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/c1234567/messages", nil))
	var body struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK || len(body.Items) != 2 {
		t.Fatalf("message page response: status=%d err=%v", response.Code, err)
	}
	var activity map[string]string
	if err := json.Unmarshal(body.Items[0]["tool_activity"], &activity); err != nil || len(activity) != 2 ||
		activity["tool"] != "web_search" || activity["status"] != "running" {
		t.Fatalf("tool activity = %+v err=%v", activity, err)
	}
	if _, exists := body.Items[0]["task_id"]; exists {
		t.Fatal("internal Task ID leaked in the user message")
	}
	if _, exists := body.Items[1]["tool_activity"]; exists {
		t.Fatal("ordinary message has an internal tool activity")
	}
	projectMessage := service.messagePage.Items[0]
	projectMessage.OwnerID, projectMessage.ActorID = "u7654321", "u1234567"
	projectView, err := toProjectConversationMessageResponse(projectMessage)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(projectView)
	if err != nil {
		t.Fatal(err)
	}
	var projectBody map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &projectBody); err != nil || len(projectBody["tool_activity"]) == 0 ||
		len(projectBody["task_id"]) != 0 || len(projectBody["related_task_id"]) != 0 {
		t.Fatalf("project message omitted activity or exposed an internal Task ID: err=%v", err)
	}

	invalid := conversation.Message{ID: "m1234567", ConversationID: "c1234567", Sequence: 1,
		Role: conversation.RoleAssistant, CreatedAt: now, ToolActivity: &conversation.ToolActivity{Tool: "web_search", Status: task.StepRunning}}
	if _, err := toMessageResponse(invalid); err == nil {
		t.Fatal("assistant message accepted a user-only tool activity")
	}
	invalid.Role, invalid.ToolActivity.Tool = conversation.RoleUser, "other_tool"
	if _, err := toMessageResponse(invalid); err == nil {
		t.Fatal("unsupported internal tool was exposed")
	}
}
