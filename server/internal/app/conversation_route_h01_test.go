package app

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"

	"morde/server/internal/domain/conversation"
)

func TestH01TruncatedRouteCandidateUsesFullContentDigestForExactDuplicate(t *testing.T) {
	first := strings.Repeat("same prefix ", 1_500) + "first tail"
	otherTail := strings.Repeat("same prefix ", 1_500) + "different tail"
	digest := sha256.Sum256([]byte(first))
	scope, err := ConversationRouteExecutionScopeDigest(ConversationRoutingInput{})
	if err != nil {
		t.Fatal(err)
	}
	candidate := ConversationRouteRequestCandidate{
		MessageID: "m1234567", Sequence: 1, Content: "same prefix … first tail", ContentSHA256: digest[:],
		ContentTruncated: true, RoutingStatus: conversation.RoutingProcessing,
		SameExecutionScope: true, SameAuthorization: true, ExecutionScopeDigest: scope,
	}
	if !ValidConversationRouteExactDuplicateTarget(candidate.MessageID, first, []ConversationRouteRequestCandidate{candidate}) {
		t.Fatal("the complete matching content digest was not accepted")
	}
	if ValidConversationRouteExactDuplicateTarget(candidate.MessageID, otherTail, []ConversationRouteRequestCandidate{candidate}) {
		t.Fatal("equal preview with a different full-content tail was treated as a duplicate")
	}
	attachmentScope, err := ConversationRouteExecutionScopeDigest(ConversationRoutingInput{ClientAttachmentIDs: []string{"attach01"}})
	if err != nil || bytes.Equal(scope, attachmentScope) {
		t.Fatalf("attachment scope was not included in the execution digest: err=%v", err)
	}
	candidate.SameExecutionScope = bytes.Equal(scope, attachmentScope)
	if ValidConversationRouteExactDuplicateTarget(candidate.MessageID, first, []ConversationRouteRequestCandidate{candidate}) {
		t.Fatal("a full-content match outside the execution scope was treated as a duplicate")
	}
	candidate.SameExecutionScope, candidate.SameAuthorization = true, false
	if ValidConversationRouteExactDuplicateTarget(candidate.MessageID, first, []ConversationRouteRequestCandidate{candidate}) {
		t.Fatal("a full-content match outside the authorization snapshot was treated as a duplicate")
	}

	candidate.SameAuthorization = true
	request, err := ConversationRouteModelRequest(ConversationRouteContext{
		ActorID: "u1234567", Content: "new short message", ExecutionScopeDigest: scope,
		RequestCandidates: []ConversationRouteRequestCandidate{candidate},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(request.Input[0].Text, `"content_truncated":true`) || strings.Contains(request.Input[0].Text, `"ContentSHA256"`) {
		t.Fatalf("model request did not expose an explicit bounded preview: %s", request.Input[0].Text)
	}
}
