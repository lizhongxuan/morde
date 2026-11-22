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
	value.RouteAttempt = 0
	value.HistoryIncomplete = true
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
		HistoryIncomplete bool `json:"history_incomplete"`
		History           []struct {
			Content   string `json:"content"`
			Truncated bool   `json:"content_truncated"`
		} `json:"history"`
	}
	if err := json.Unmarshal([]byte(request.Input[0].Text), &input); err != nil {
		t.Fatal(err)
	}
	if !input.HistoryIncomplete || len(input.History) != 1 || !input.History[0].Truncated || strings.Contains(input.History[0].Content, "167600") {
		t.Fatalf("long history follow-up must request authorized source search rather than guess: input=%+v instructions=%q",
			input, request.Instructions)
	}
	assertH09HistorySearchPolicy(t, request.Instructions)
	if !useLegacyConversationRouteModelRequest(value) {
		t.Fatal("first route attempt with no running prior-request task did not select the legacy fallback")
	}
	legacyRequest, err := legacyConversationRouteModelRequest(value, request)
	if err != nil {
		t.Fatal(err)
	}
	var legacyInput struct {
		HistoryIncomplete bool `json:"history_incomplete"`
		History           []struct {
			Content   string `json:"content"`
			Truncated bool   `json:"content_truncated"`
		} `json:"history"`
	}
	if err := json.Unmarshal([]byte(legacyRequest.Input[0].Text), &legacyInput); err != nil {
		t.Fatal(err)
	}
	if !legacyInput.HistoryIncomplete || len(legacyInput.History) != 1 || !legacyInput.History[0].Truncated ||
		strings.Contains(legacyInput.History[0].Content, "167600") {
		t.Fatalf("legacy fallback did not preserve the incomplete-history marker and authorized search policy: input=%+v instructions=%q",
			legacyInput, legacyRequest.Instructions)
	}
	assertH09HistorySearchPolicy(t, legacyRequest.Instructions)
	review := value
	review.ResultReview = &ConversationTaskResultReview{Sources: []ConversationTaskResultSource{{TaskID: "q1234567",
		TaskRevision: 1, EventSequence: 1, AuthorizationVersion: 1, Objective: "review", Content: "synthetic result"}}}
	reviewRequest, err := ConversationRouteModelRequest(review)
	if err != nil || !strings.Contains(reviewRequest.Instructions, conversationRouteIncompleteHistoryInstructions) ||
		strings.Contains(reviewRequest.Instructions, conversationRouteHistorySearchTaskInstructions) {
		t.Fatalf("result review did not keep its no-Task incomplete-history behavior: request=%+v err=%v", reviewRequest, err)
	}
}

func assertH09HistorySearchPolicy(t *testing.T, instructions string) {
	t.Helper()
	if !strings.Contains(instructions, conversationRouteHistorySearchTaskInstructions) {
		t.Fatalf("history search policy missing: %q", instructions)
	}
	const noInferenceRule = "never infer omitted details or present excerpts as complete"
	if strings.Contains(strings.ToLower(conversationRouteHistorySearchTaskInstructions), noInferenceRule) {
		if !strings.Contains(strings.ToLower(instructions), noInferenceRule) {
			t.Fatalf("history search policy lost its no-inference rule: %q", instructions)
		}
		return
	}
	const legacyNoInferenceRule = "Do not infer omitted details or present excerpts as complete"
	if !strings.Contains(instructions, legacyNoInferenceRule) ||
		!strings.Contains(instructions, conversationRouteIncompleteHistoryInstructions) {
		t.Fatalf("history search policy or its separate no-inference safeguard missing: %q", instructions)
	}
}
