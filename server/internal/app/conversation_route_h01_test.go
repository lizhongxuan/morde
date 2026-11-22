package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
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

func TestH09LongHistoryFollowupIsMarkedForAuthorizedSearch(t *testing.T) {
	_, value := routeGatewayFixture()
	value.Content = "历史里的第 400 条记录数值和校验值是什么？"
	value.History = []ConversationRouteHistory{{MessageID: "m7654321", Sequence: 1, Role: conversation.RoleUser,
		Content: strings.Repeat("前文记录 ", 1_600) + "记录0400：数值=842；校验=167600" + strings.Repeat(" 后文记录", 1_600)}}
	request, err := ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 1 {
		t.Fatalf("built %d route inputs, want one", len(request.Input))
	}
	var input struct {
		History []struct {
			Content   string `json:"content"`
			Truncated bool   `json:"content_truncated"`
		} `json:"history"`
	}
	if err := json.Unmarshal([]byte(request.Input[0].Text), &input); err != nil {
		t.Fatal(err)
	}
	if len(input.History) != 1 || !input.History[0].Truncated || strings.Contains(input.History[0].Content, "167600") ||
		!strings.Contains(request.Instructions, conversationRouteIncompleteHistoryInstructions) ||
		!strings.Contains(request.Instructions, conversationRouteHistorySearchTaskInstructions) {
		t.Fatalf("long history follow-up must request authorized source search rather than guess: input=%+v instructions=%q",
			input, request.Instructions)
	}
	review := value
	review.ResultReview = &ConversationTaskResultReview{Sources: []ConversationTaskResultSource{{TaskID: "q1234567",
		TaskRevision: 1, EventSequence: 1, AuthorizationVersion: 1, Objective: "review", Content: "synthetic result"}}}
	reviewRequest, err := ConversationRouteModelRequest(review)
	if err != nil || !strings.Contains(reviewRequest.Instructions, conversationRouteIncompleteHistoryInstructions) ||
		strings.Contains(reviewRequest.Instructions, conversationRouteHistorySearchTaskInstructions) {
		t.Fatalf("result review did not keep its no-Task incomplete-history behavior: request=%+v err=%v", reviewRequest, err)
	}
}
