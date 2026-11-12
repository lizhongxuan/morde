package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
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
		ActorID: "u1234567", Role: conversation.RoleUser, Sequence: 1, Content: "开始一项新任务"},
		ServiceInstanceID: "00000000-0000-4000-8000-000000000001", LeaseOwner: "worker-1", LeaseVersion: 1}
	return claim, ConversationRouteContext{ActorID: claim.Message.ActorID, ModelID: "p1234567", Content: claim.Message.Content}
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
				len(client.request.Tools) != 0 || !strings.Contains(client.request.Instructions, "jokes, and simple questions") {
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
