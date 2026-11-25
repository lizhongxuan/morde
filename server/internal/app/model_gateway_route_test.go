package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

type routeGatewayStoreTest struct {
	*modelGatewayStoreTest
	reservation ModelCallReservation
	project     *ProjectModelReservation
	prepareErr  error
	decision    *ConversationRouteDecision
	usage       ModelUsage
	unknown     bool
	begins      int
}

func (s *routeGatewayStoreTest) ResolveConversationRouteModel(_ context.Context, _ ConversationRouteClaim) (string, error) {
	return "p1234567", nil
}
func (s *routeGatewayStoreTest) PrepareConversationRouteModelCall(_ context.Context, call ModelCallReservation, project *ProjectModelReservation) (*ConversationRouteDecision, error) {
	s.reservation, s.project = call, project
	if s.prepareErr != nil {
		return nil, s.prepareErr
	}
	return s.decision, nil
}
func (s *routeGatewayStoreTest) BeginConversationRouteModelCall(_ context.Context, _ ModelCallReservation) error {
	s.begins++
	return nil
}
func (s *routeGatewayStoreTest) SettleConversationRouteModelCall(_ context.Context, _ ModelCallReservation, decision *ConversationRouteDecision, usage ModelUsage, unknown bool) error {
	s.decision, s.usage, s.unknown = decision, usage, unknown
	return nil
}

func routeGatewayFixture() (ConversationRouteClaim, ConversationRouteContext) {
	claim := ConversationRouteClaim{Message: conversation.Message{ID: "m1234567", ConversationID: "c1234567", OwnerID: "u1234567",
		ActorID: "u1234567", Role: conversation.RoleUser, Sequence: 1, Content: "开始一项新任务"}, RouteAttempt: 1,
		ServiceInstanceID: "00000000-0000-4000-8000-000000000001", LeaseOwner: "worker-1", LeaseVersion: 1}
	return claim, ConversationRouteContext{ActorID: claim.Message.ActorID, RouteAttempt: 1, ModelID: "p1234567", Content: claim.Message.Content}
}

func TestLegacyRouteModelRequestAddsDirectToolContractForStructurallyEligibleContexts(t *testing.T) {
	query := "请搜索公开网站关于日本央行利率政策的最新信息"
	eligible := ConversationRouteContext{ActorID: "u1234567", Content: query}
	request, err := ConversationRouteModelRequest(eligible)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := legacyConversationRouteModelRequest(eligible, request)
	if err != nil || !strings.Contains(legacy.Instructions, conversationRouteDirectToolInstructions) {
		t.Fatalf("eligible legacy search request lost its direct-tool contract: err=%v instructions=%q", err, legacy.Instructions)
	}

	digest := sha256.Sum256([]byte(query))
	for _, test := range []struct {
		name             string
		context          ConversationRouteContext
		toolInstructions bool
	}{
		{"ordinary chat", ConversationRouteContext{ActorID: "u1234567", Content: "你好"}, true},
		{"task reference", ConversationRouteContext{ActorID: "u1234567", Content: query, ReferenceTaskID: "q1234567"}, false},
		{"attachments", ConversationRouteContext{ActorID: "u1234567", Content: query, CurrentMessageHasAttachments: true}, false},
		{"unresolved predecessor", ConversationRouteContext{ActorID: "u1234567", Content: query, PreviousUnresolved: true}, false},
		{"task control wording", ConversationRouteContext{ActorID: "u1234567", Content: query + "，顺便停止当前任务"}, true},
		{"non-keyword search phrasing", ConversationRouteContext{ActorID: "u1234567", Content: "有没有日本央行利率政策的最新公开信息？"}, true},
		{"truncated message", ConversationRouteContext{ActorID: "u1234567", Content: query, CurrentMessageID: "m1234567",
			ContentBytes: int64(len(query)), ContentSHA256: digest[:], ContentTruncated: true}, false},
		{"incomplete candidate set", ConversationRouteContext{ActorID: "u1234567", Content: query, CandidateSetIncomplete: true}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := ConversationRouteModelRequest(test.context)
			if err != nil {
				t.Fatalf("build route request: %v", err)
			}
			legacy, err := legacyConversationRouteModelRequest(test.context, request)
			if err != nil {
				t.Fatalf("build legacy route request: %v", err)
			}
			got := strings.Contains(legacy.Instructions, conversationRouteDirectToolInstructions)
			if got != test.toolInstructions {
				t.Fatalf("legacy direct-tool contract eligibility = %t, want %t: %s", got, test.toolInstructions, legacy.Instructions)
			}
		})
	}
}

func TestLegacyRouteModelRequestPreservesIncompleteHistorySummary(t *testing.T) {
	const summary = "较早要求：保留全部付款条件。"
	summaryDigest := sha256.Sum256([]byte(summary))
	sourcesDigest := sha256.Sum256([]byte("authorized source proof"))
	value := ConversationRouteContext{ActorID: "u1234567", Content: "历史里的第 400 条记录数值和校验值是什么？",
		HistoryIncomplete: true, SummaryVersion: 3, SummaryText: summary, SummarySHA256: summaryDigest[:], SummarySourcesSHA256: sourcesDigest[:]}
	request, err := ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := legacyConversationRouteModelRequest(value, request)
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		Summary struct {
			Version       int64  `json:"version"`
			SHA256        string `json:"sha256"`
			SourcesSHA256 string `json:"sources_sha256"`
			Text          string `json:"text"`
		} `json:"summary"`
		HistoryIncomplete bool `json:"history_incomplete"`
	}
	if err := json.Unmarshal([]byte(legacy.Input[0].Text), &input); err != nil {
		t.Fatal(err)
	}
	if input.Summary.Version != value.SummaryVersion || input.Summary.SHA256 != hex.EncodeToString(summaryDigest[:]) ||
		input.Summary.SourcesSHA256 != hex.EncodeToString(sourcesDigest[:]) || input.Summary.Text != summary || !input.HistoryIncomplete ||
		!strings.Contains(legacy.Instructions, conversationRouteHistorySearchTaskInstructions) ||
		strings.Contains(legacy.Instructions, conversationRouteDirectToolInstructions) {
		t.Fatalf("legacy conversion changed the incomplete-history summary path: input=%+v instructions=%q", input, legacy.Instructions)
	}
}

func TestConversationRouteProviderErrorCarriesOnlyConfirmedRateLimitDelay(t *testing.T) {
	providerErr := &ModelClientError{Kind: ModelErrorRateLimited, Stage: "provider_status", RetryAfter: 3 * time.Second}
	var routeErr *domain.Error
	if err := conversationRouteProviderError(providerErr); !errors.As(err, &routeErr) || routeErr.Code != domain.CodeRateLimited || routeErr.RetryAfter != 3*time.Second {
		t.Fatalf("provider rate limit mapping = %#v, err=%v", routeErr, err)
	}
	providerErr = &ModelClientError{Kind: ModelErrorRateLimited, Stage: "provider_status", RetryAfterExceedsDurableHorizon: true}
	if err := conversationRouteProviderError(providerErr); !errors.As(err, &routeErr) || routeErr.Code != domain.CodeRateLimited ||
		routeErr.RetryAfter != 0 || routeErr.Details["retry_after_exceeds_durable_horizon"] != "true" {
		t.Fatalf("over-horizon provider rate limit mapping = %#v, err=%v", routeErr, err)
	}
	providerErr.Stage = "transport"
	if err := conversationRouteProviderError(providerErr); !errors.As(err, &routeErr) || routeErr.RetryAfter != 0 {
		t.Fatalf("non-provider failure preserved retry delay: %#v, err=%v", routeErr, err)
	}
}

func TestConversationRouteInitialAttemptIncludesJoinCandidates(t *testing.T) {
	claim, value := routeGatewayFixture()
	scopeDigest := make([]byte, sha256.Size)
	scopeDigest[0] = 1
	value.ExecutionScopeDigest = scopeDigest
	value.History = []ConversationRouteHistory{{MessageID: "m7654321", Sequence: 1, Role: conversation.RoleAssistant,
		Content: "1. 继续任务 A；2. 开始新任务"}}
	value.RequestCandidates = []ConversationRouteRequestCandidate{{MessageID: "m8765432", Sequence: 2, Content: "继续任务 A",
		RoutingStatus: conversation.RoutingProcessing, SameExecutionScope: true, SameAuthorization: true, ExecutionScopeDigest: scopeDigest}}
	initial, err := ConversationRouteModelRequest(value)
	if err != nil || !strings.Contains(initial.Instructions, `"target_message_id"`) ||
		!strings.Contains(initial.Instructions, "same_execution_scope") || !strings.Contains(initial.Instructions, "joinable field is true") ||
		!strings.Contains(initial.Instructions, `bare number such as "1"`) ||
		!strings.Contains(initial.Input[0].Text, `"prior_requests":[`) || !strings.Contains(initial.Input[0].Text, "继续任务 A") {
		t.Fatalf("initial route did not include authorized join context and selection rules: request=%+v err=%v", initial, err)
	}
	var requestInput struct {
		PriorRequests []conversationRouteModelRequestCandidate `json:"prior_requests"`
		History       []ConversationRouteHistory               `json:"history"`
	}
	if err := json.Unmarshal([]byte(initial.Input[0].Text), &requestInput); err != nil || len(requestInput.PriorRequests) != 1 ||
		!requestInput.PriorRequests[0].Joinable || len(requestInput.History) != 1 || requestInput.History[0].Content != value.History[0].Content {
		t.Fatalf("initial route omitted joinability or authorized option history: input=%+v err=%v", requestInput, err)
	}
	joined := ParseConversationRouteDecisionForContext(`{"action":"join","task_id":"","target_message_id":"m8765432","explicit_new":false,"question":"","reply":"","dependency_task_ids":[]}`, value)
	if joined.Action != ConversationRouteJoin || joined.TargetMessageID != "m8765432" {
		t.Fatalf("initial route rejected a same-scope prior request: %+v", joined)
	}
	actualDigest, actualID, err := conversationRouteCallDigest(claim, value, initial)
	if err != nil {
		t.Fatal(err)
	}
	value.RouteAttempt = 2
	current, err := ConversationRouteModelRequest(value)
	if err != nil || !strings.Contains(current.Input[0].Text, `"prior_requests":[`) {
		t.Fatalf("retries did not keep the association-aware request: request=%+v err=%v", current, err)
	}
	currentDigest, currentID, err := conversationRouteCallDigest(claim, value, current)
	if err != nil || bytes.Equal(actualDigest, currentDigest) || actualID == currentID {
		t.Fatalf("retry attempt reused the first route model identity: digest=%x id=%q err=%v", currentDigest, currentID, err)
	}
}

func TestConversationRouteGatewayJoinsFollowupToSameDeliverable(t *testing.T) {
	claim, value := routeGatewayFixture()
	const targetMessageID = "m7654321"
	const followup = "同一个方案增加 AI 难度和存档设计"
	claim.Message.Sequence = 2
	claim.Message.Content, value.Content = followup, followup
	digest := make([]byte, sha256.Size)
	digest[0] = 1
	value.ExecutionScopeDigest = digest
	value.RequestCandidates = []ConversationRouteRequestCandidate{{MessageID: targetMessageID, Sequence: 1,
		Content: "为五子棋应用写一份设计方案并保存为 Markdown", RoutingStatus: conversation.RoutingRouted,
		TaskID: "q1234567", TaskStatus: task.StatusRunning, SameExecutionScope: true, SameAuthorization: true, ExecutionScopeDigest: digest}}
	output := `{"action":"join","task_id":"","target_message_id":"` + targetMessageID + `","explicit_new":false,"question":"","reply":"","dependency_task_ids":[]}`
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{response: ModelResponse{Text: output, StopReason: "stop",
		Usage: ModelUsage{Known: true, InputTokens: 12, OutputTokens: 4}}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{personal: true}, client)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := service.ClassifyConversationRoute(context.Background(), claim, value)
	if err != nil || decision.Action != ConversationRouteJoin || decision.TargetMessageID != targetMessageID || client.calls != 1 ||
		!strings.Contains(client.request.Instructions, "a follow-up that adds requirements to the same deliverable, use join") ||
		!strings.Contains(client.request.Input[0].Text, `"joinable":true`) || !strings.Contains(client.request.Input[0].Text, followup) {
		t.Fatalf("same-deliverable follow-up did not join its authorized Task: decision=%+v err=%v request=%+v",
			decision, err, client.request)
	}
}

func TestConversationRouteLegacyAttemptKeepsVersionedRequest(t *testing.T) {
	claim, value := routeGatewayFixture()
	claim.RouteAttempt, value.RouteAttempt = 0, 0
	scopeDigest := make([]byte, sha256.Size)
	scopeDigest[0] = 1
	value.ExecutionScopeDigest = scopeDigest
	value.RequestCandidates = []ConversationRouteRequestCandidate{{MessageID: "m8765432", Sequence: 2,
		Content: "同一项工作", RoutingStatus: conversation.RoutingProcessing, SameExecutionScope: true,
		ExecutionScopeDigest: scopeDigest}}
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{response: ModelResponse{Text: `{"action":"new","task_id":"","question":"","reply":"","dependency_task_ids":[]}`,
		StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{personal: true}, client)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := service.ClassifyConversationRoute(context.Background(), claim, value)
	if err != nil || decision.Action != ConversationRouteNew || client.calls != 1 ||
		!strings.Contains(client.request.Instructions, legacyConversationRouteInstructions) ||
		!strings.Contains(client.request.Instructions, conversationRouteAcknowledgementInstructions) ||
		!strings.Contains(client.request.Instructions, "An explicit request to create, write, export, save, or deliver a file or document") {
		t.Fatalf("legacy route attempt did not preserve its prompt: decision=%+v instructions=%q err=%v",
			decision, client.request.Instructions, err)
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal([]byte(client.request.Input[0].Text), &input); err != nil {
		t.Fatal(err)
	}
	if _, exists := input["prior_requests"]; exists {
		t.Fatalf("legacy route attempt received association-aware fields: %s", client.request.Input[0].Text)
	}
	if _, exists := input["active_tasks"]; !exists {
		t.Fatalf("legacy route attempt lost the prior active_tasks input: %s", client.request.Input[0].Text)
	}
}

func TestConversationRouteGatewayAccountsForTextOnlyDecisionAndRecoversIt(t *testing.T) {
	claim, value := routeGatewayFixture()
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{response: ModelResponse{Text: `{"action":"new","task_id":"","question":""}`,
		StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{personal: true}, client)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := service.ClassifyConversationRoute(context.Background(), claim, value)
	if err != nil || decision.Action != ConversationRouteNew || client.calls != 1 || store.begins != 1 || store.unknown ||
		store.reservation.Source != ModelCallRoute || store.reservation.Task != nil || store.reservation.Route == nil ||
		store.reservation.OwnerID != claim.Message.ActorID || len(client.request.Tools) != 0 || !store.usage.Known {
		t.Fatalf("route call or settlement failed: decision=%+v err=%v", decision, err)
	}
	callID := store.reservation.CallID
	decision, err = service.ClassifyConversationRoute(context.Background(), claim, value)
	if err != nil || decision.Action != ConversationRouteNew || client.calls != 1 || store.begins != 1 || store.reservation.CallID != callID {
		t.Fatalf("settled result was not recovered: decision=%+v err=%v calls=%d", decision, err, client.calls)
	}
	value.Content = "different"
	if _, err := service.ClassifyConversationRoute(context.Background(), claim, value); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("mismatched message was accepted: %v", err)
	}
}

func TestConversationRouteGatewayRejectsTruncatedAndToolResponses(t *testing.T) {
	for _, test := range []struct {
		name     string
		response ModelResponse
	}{
		{name: "truncated JSON", response: ModelResponse{Text: `{"action":"reply","task_id":"","question":"","reply":"OK"}`,
			StopReason: "length", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}},
		{name: "unexpected tool call", response: ModelResponse{Text: `{"action":"reply","task_id":"","question":"","reply":"OK"}`,
			ToolCalls:  []ModelToolCall{{CallID: "call-1", Name: "unexpected", Arguments: `{}`}},
			StopReason: "tool_calls", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim, value := routeGatewayFixture()
			store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
			client := &modelGatewayClientTest{response: test.response}
			service, err := NewModelGatewayService(store, &modelGatewayResolverTest{personal: true}, client)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := service.ClassifyConversationRoute(context.Background(), claim, value)
			if err != nil || !decision.InvalidOutput || store.decision == nil || !store.decision.InvalidOutput ||
				store.unknown || !store.usage.Known || client.calls != 1 {
				t.Fatalf("incomplete or tool response was accepted: decision=%+v settled=%+v unknown=%t usage=%+v calls=%d err=%v",
					decision, store.decision, store.unknown, store.usage, client.calls, err)
			}
		})
	}
}

func TestConversationRouteGatewayUsesConfiguredGPT6LunaMaxReasoning(t *testing.T) {
	claim, value := routeGatewayFixture()
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{response: ModelResponse{Text: `{"action":"reply","task_id":"","question":"","reply":"OK"}`,
		StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}}
	window := 65_536
	resolver := &modelGatewayResolverTest{personal: true, contextWindow: &window, endpoint: ModelEndpoint{
		BaseURL: "https://api.openai.com/v1", APIKey: "test-key", ModelID: "gpt-6-luna",
	}}
	service, err := NewModelGatewayService(store, resolver, client)
	if err != nil {
		t.Fatal(err)
	}
	service.SetGPT6LunaMaxReasoning(true)
	decision, err := service.ClassifyConversationRoute(context.Background(), claim, value)
	if err != nil || decision.Action != ConversationRouteReply || decision.Reply != "OK" || client.calls != 1 ||
		client.request.ReasoningEffort != "max" || client.request.DisableThinking ||
		client.request.MaxOutputTokens < maxGPT6LunaReasoningTokens ||
		store.reservation.ReservedTokens < maxGPT6LunaReasoningTokens {
		t.Fatalf("GPT-6 Luna max reasoning was not applied and reserved: decision=%+v request=%+v reservation=%+v error=%v",
			decision, client.request, store.reservation, err)
	}
}

func TestConversationRouteGatewayKeepsJokesAndTimeAsTasklessReplies(t *testing.T) {
	for _, test := range []struct {
		name    string
		message string
		reply   string
	}{
		{name: "joke", message: "讲个笑话", reply: "为什么程序员喜欢黑暗？因为光会产生 bug。"},
		{name: "time", message: "现在几点了？", reply: "现在是 09:00。"},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim, value := routeGatewayFixture()
			claim.Message.Content, value.Content = test.message, test.message
			value.TimeZone = "Asia/Shanghai"
			value.ServerReceivedAt = time.Date(2026, 11, 12, 1, 0, 0, 0, time.UTC)
			store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
			client := &modelGatewayClientTest{response: ModelResponse{
				Text:       `{"action":"reply","task_id":"","question":"","reply":"` + test.reply + `"}`,
				StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3},
			}}
			service, err := NewModelGatewayService(store, &modelGatewayResolverTest{personal: true}, client)
			if err != nil {
				t.Fatal(err)
			}
			decision, err := service.ClassifyConversationRoute(context.Background(), claim, value)
			if err != nil || decision.Action != ConversationRouteReply || decision.Reply != test.reply || client.calls != 1 ||
				store.decision == nil || store.decision.Action != ConversationRouteReply || store.reservation.Task != nil ||
				len(client.request.Tools) != 0 || !strings.Contains(client.request.Instructions, "jokes, acknowledgements") ||
				!strings.Contains(client.request.Instructions, "Simple questions") {
				t.Fatalf("ordinary conversation did not remain a taskless reply: decision=%+v err=%v calls=%d", decision, err, client.calls)
			}
			var input struct {
				Message          string `json:"message"`
				ReferenceTime    string `json:"reference_time"`
				TimeZone         string `json:"time_zone"`
				TimeZoneProvided bool   `json:"time_zone_provided"`
			}
			if err := json.Unmarshal([]byte(client.request.Input[0].Text), &input); err != nil {
				t.Fatal(err)
			}
			if input.Message != test.message || input.ReferenceTime != "2026-11-12T09:00:00+08:00" ||
				input.TimeZone != "Asia/Shanghai" || !input.TimeZoneProvided {
				t.Fatalf("route model received incorrect source time: %+v", input)
			}
		})
	}
}

func TestConversationResultReviewRetryUsesFreshModelCallIdentity(t *testing.T) {
	claim, value := routeGatewayFixture()
	value.ResultReview = &ConversationTaskResultReview{Sources: []ConversationTaskResultSource{{
		TaskID: "q1234567", TaskRevision: 1, EventSequence: 2, AuthorizationVersion: 1,
		Objective: "return a short text result", Content: "B40-PASS",
	}}}
	request, err := ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, firstCallID, err := conversationRouteCallDigest(claim, value, request)
	if err != nil {
		t.Fatal(err)
	}
	value.ResultReview.RetryAttempt = 1
	retryRequest, err := ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	retryDigest, retryCallID, err := conversationRouteCallDigest(claim, value, retryRequest)
	if err != nil || bytes.Equal(firstDigest, retryDigest) || firstCallID == retryCallID ||
		request.Instructions != retryRequest.Instructions || request.Input[0].Text != retryRequest.Input[0].Text {
		t.Fatalf("explicit result review retry reused its model call identity: first=%q retry=%q err=%v", firstCallID, retryCallID, err)
	}
}

func TestConversationRouteGatewayHoldsUnknownProviderOutcome(t *testing.T) {
	claim, value := routeGatewayFixture()
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{err: errors.New("provider unavailable")}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{personal: true}, client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ClassifyConversationRoute(context.Background(), claim, value); err == nil || !store.unknown || store.decision != nil {
		t.Fatalf("unknown provider result was not held: %v", err)
	}
}

func TestConversationRouteGatewayLeavesConfirmedRateLimitForAtomicRecovery(t *testing.T) {
	claim, value := routeGatewayFixture()
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{err: &ModelClientError{Kind: ModelErrorRateLimited, Stage: "provider_status"}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{personal: true}, client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ClassifyConversationRoute(context.Background(), claim, value); domain.CodeOf(err) != domain.CodeRateLimited ||
		store.begins != 1 || store.unknown || store.decision != nil || store.usage.Known {
		t.Fatalf("confirmed 429 was recorded as unknown usage instead of deferred to route recovery: begins=%d unknown=%t settlement=%+v usage=%+v err=%v",
			store.begins, store.unknown, store.decision, store.usage, err)
	}
}

func TestConversationRouteGatewayBudgetDenialDoesNotReachProvider(t *testing.T) {
	claim, value := routeGatewayFixture()
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{},
		prepareErr: domain.NewError(domain.CodeQuotaExceeded, "model token budget exceeded")}
	client := &modelGatewayClientTest{response: ModelResponse{Text: `{"action":"new","task_id":"","question":""}`,
		StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{personal: true}, client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ClassifyConversationRoute(context.Background(), claim, value); domain.CodeOf(err) != domain.CodeQuotaExceeded ||
		client.calls != 0 || store.begins != 0 || store.decision != nil || store.unknown {
		t.Fatalf("route budget denial dispatched or settled a model call: calls=%d begins=%d decision=%+v unknown=%t err=%v",
			client.calls, store.begins, store.decision, store.unknown, err)
	}
}

func TestConversationRouteGatewayRejectsOversizeBeforeReservation(t *testing.T) {
	claim, value := routeGatewayFixture()
	claim.Message.Content = strings.Repeat("界", maxAgentInputBytes)
	value.Content = claim.Message.Content
	value.CurrentMessageID = claim.Message.ID
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ClassifyConversationRoute(context.Background(), claim, value); domain.CodeOf(err) != domain.CodeResultTooLarge ||
		store.reservation.Source != "" || store.begins != 0 || client.calls != 0 {
		t.Fatalf("oversize route was not rejected before reservation and dispatch: %v", err)
	}
}
