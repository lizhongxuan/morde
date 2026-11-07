package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

type routeGatewayStoreTest struct {
	*modelGatewayStoreTest
	reservation ModelCallReservation
	project     *ProjectModelReservation
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
