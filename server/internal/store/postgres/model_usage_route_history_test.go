package postgres

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/conversation"
)

func TestConversationRouteHistoryProjectionAllowsOnlyIncompleteAuthorizedHistory(t *testing.T) {
	requestDigest := sha256.Sum256([]byte("original request"))
	replyDigest := sha256.Sum256([]byte("original reply"))
	expected := []app.ConversationRouteHistory{
		{MessageID: "m1234567", Sequence: 1, Role: conversation.RoleUser, Content: "original request",
			ContentBytes: int64(len("original request")), ContentSHA256: hex.EncodeToString(requestDigest[:]), SourceSHA256: requestDigest[:]},
		{MessageID: "m2345678", Sequence: 2, Role: conversation.RoleAssistant, Content: "original reply",
			ContentBytes: int64(len("original reply")), ContentSHA256: hex.EncodeToString(replyDigest[:]), SourceSHA256: replyDigest[:]},
	}
	trimmed := []app.ConversationRouteHistory{
		expected[0],
		expected[1],
	}
	trimmed[1].Content = ""
	trimmed[1].Truncated = true
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
	trimmed[1] = expected[1]
	trimmed[1].Content = ""
	trimmed[1].Truncated = true
	trimmed[1].SourceSHA256 = append([]byte(nil), trimmed[1].SourceSHA256...)
	trimmed[1].SourceSHA256[0] ^= 0xff
	if conversationRouteHistoryProjectionMatches(expected, trimmed, true) {
		t.Fatal("incomplete history accepted a changed original source digest")
	}
}

func TestConversationRouteHistoryProjectionAllowsOnlyCanonicalBoundedPreview(t *testing.T) {
	originalContent := strings.Repeat("source history content ", 180)
	digest := sha256.Sum256([]byte(originalContent))
	expected := []app.ConversationRouteHistory{{MessageID: "m1234567", Sequence: 1, Role: conversation.RoleUser,
		Content: originalContent, ContentBytes: int64(len(originalContent)), ContentSHA256: hex.EncodeToString(digest[:]), SourceSHA256: digest[:]}}
	preview, truncated := app.ConversationRouteTextPreview(originalContent, app.ConversationRouteHistoryPreviewBytes)
	if !truncated {
		t.Fatal("synthetic source did not produce a history preview")
	}
	actual := []app.ConversationRouteHistory{expected[0]}
	actual[0].Content = preview
	actual[0].Truncated = true
	if int64(len(actual[0].Content)) > actual[0].ContentBytes || !conversationRouteHistoryProjectionMatches(expected, actual, true) {
		t.Fatal("canonical bounded history preview was rejected")
	}
	actual[0].Content += " unrelated text"
	if conversationRouteHistoryProjectionMatches(expected, actual, true) {
		t.Fatal("noncanonical history excerpt was accepted")
	}
	actual[0].Content = strings.Repeat("x", int(expected[0].ContentBytes)+1)
	if conversationRouteHistoryProjectionMatches(expected, actual, true) {
		t.Fatal("history content longer than its original source was accepted")
	}
}
