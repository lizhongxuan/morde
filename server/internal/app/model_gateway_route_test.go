package app

import (
	"bytes"
	"context"
	"crypto/sha256"
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

func TestConversationRouteInitialAttemptIncludesJoinCandidates(t *testing.T) {
	claim, value := routeGatewayFixture()
	scopeDigest := make([]byte, sha256.Size)
	scopeDigest[0] = 1
	value.ExecutionScopeDigest = scopeDigest
	value.History = []ConversationRouteHistory{{MessageID: "m7654321", Sequence: 1, Role: conversation.RoleAssistant,
		Content: "1. 继续任务 A；2. 开始新任务"}}
	value.RequestCandidates = []ConversationRouteRequestCandidate{{MessageID: "m8765432", Sequence: 2, Content: "继续任务 A",
		RoutingStatus: conversation.RoutingProcessing, SameExecutionScope: true, ExecutionScopeDigest: scopeDigest}}
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
		TaskID: "q1234567", TaskStatus: task.StatusRunning, SameExecutionScope: true, ExecutionScopeDigest: digest}}
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
		client.request.Instructions != legacyConversationRouteInstructions ||
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

func TestConversationRouteGatewayUsesConfiguredGPT6LunaMaxReasoning(t *testing.T) {
	claim, value := routeGatewayFixture()
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{response: ModelResponse{Text: `{"action":"reply","task_id":"","question":"","reply":"OK"}`,
		StopReason: "stop", Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}}
	resolver := &modelGatewayResolverTest{personal: true, endpoint: ModelEndpoint{
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
