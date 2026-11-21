package postgres

import (
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/conversation"
)

func TestConversationRouteHistoryProjectionAllowsOnlyIncompleteAuthorizedHistory(t *testing.T) {
	expected := []app.ConversationRouteHistory{
		{MessageID: "m1234567", Sequence: 1, Role: conversation.RoleUser, Content: "original request"},
		{MessageID: "m2345678", Sequence: 2, Role: conversation.RoleAssistant, Content: "original reply"},
	}
	trimmed := []app.ConversationRouteHistory{
		expected[0],
		{MessageID: expected[1].MessageID, Sequence: expected[1].Sequence, Role: expected[1].Role, Truncated: true},
	}
	if !conversationRouteHistoryProjectionMatches(expected, trimmed, true) {
		t.Fatal("explicitly incomplete history could not omit or clear authorized content")
	}
	if conversationRouteHistoryProjectionMatches(expected, trimmed, false) {
		t.Fatal("complete history accepted omitted content")
	}
	trimmed[1].Content = "rewritten reply"
	if conversationRouteHistoryProjectionMatches(expected, trimmed, true) {
		t.Fatal("incomplete history accepted rewritten content")
	}
	trimmed[1].Content = ""
	trimmed[1].MessageID = "m3456789"
	if conversationRouteHistoryProjectionMatches(expected, trimmed, true) {
		t.Fatal("incomplete history accepted a message outside the authorized snapshot")
	}
}
