package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain"
)

func TestConversationRouteInputFitsMinimumWindowAndAccountsForCJK(t *testing.T) {
	request := ModelRequest{Instructions: "route", Input: []ModelInput{{Text: "short message"}}, MaxOutputTokens: 512}
	if !ConversationRouteInputFitsContextWindow(request, 4096) {
		t.Fatal("short route request did not fit the minimum supported window")
	}
	request.Input[0].Text = strings.Repeat("汉", 3200)
	if ConversationRouteInputFitsContextWindow(request, 4096) ||
		!ConversationRouteInputFitsContextWindow(request, 8192) {
		t.Fatal("CJK input estimate did not account for a larger selected model window")
	}
	if ConversationRouteInputFitsContextWindow(request, 2048) {
		t.Fatal("unsupported model capacity was accepted")
	}
}

func TestConversationRouteBudgetCountsMordeSoulOnceForPreflightAndDSH(t *testing.T) {
	preflight := ModelRequest{Instructions: "route", Input: []ModelInput{{Role: "user", Text: strings.Repeat("x", 22_000)}}, MaxOutputTokens: 512}
	dsh := preflight
	dsh.Instructions = strings.TrimSpace(mordeSoulFile) + "\n\n" + preflight.Instructions
	preflightFits := ConversationRouteInputFitsContextWindow(preflight, 8192)
	dshFits := ConversationRouteInputFitsContextWindow(dsh, 8192)
	if !preflightFits || dshFits != preflightFits {
		t.Fatalf("preflight and DSH budgets disagree about their shared SOUL prompt: preflight=%t dsh=%t", preflightFits, dshFits)
	}
}

func TestResolveConversationRouteContextWindowFallsBackToMinimumWhenUnknown(t *testing.T) {
	claim, _ := routeGatewayFixture()
	service, err := NewModelGatewayService(&modelGatewayStoreTest{}, &modelGatewayResolverTest{}, &modelGatewayClientTest{})
	if err != nil {
		t.Fatal(err)
	}
	window, known, err := service.ResolveConversationRouteContextWindow(context.Background(), claim, "p1234567")
	if err != nil || window != 4096 || known {
		t.Fatalf("unknown model capacity = %d known=%t err=%v", window, known, err)
	}

	configured := 32_768
	service, err = NewModelGatewayService(&modelGatewayStoreTest{}, &modelGatewayResolverTest{contextWindow: &configured}, &modelGatewayClientTest{})
	if err != nil {
		t.Fatal(err)
	}
	window, known, err = service.ResolveConversationRouteContextWindow(context.Background(), claim, "p1234567")
	if err != nil || window != configured || !known {
		t.Fatalf("configured model capacity = %d known=%t err=%v", window, known, err)
	}
}

func TestConversationRouteGatewayRejectsOverBudgetBeforeReservation(t *testing.T) {
	claim, value := routeGatewayFixture()
	claim.Message.Content, value.Content = strings.Repeat("汉", 3200), strings.Repeat("汉", 3200)
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ClassifyConversationRoute(context.Background(), claim, value); domain.CodeOf(err) != domain.CodeResultTooLarge ||
		store.reservation.ModelID != "" || store.begins != 0 || client.calls != 0 {
		t.Fatalf("over-budget route was reserved or dispatched: code=%s reservation=%+v begins=%d calls=%d",
			domain.CodeOf(err), store.reservation, store.begins, client.calls)
	}
}

func TestCoordinatorGatewayRejectsOverBudgetBeforeReservation(t *testing.T) {
	claim, value := routeGatewayFixture()
	route, err := ConversationRouteModelRequest(value)
	if err != nil {
		t.Fatal(err)
	}
	frame := dshModelRequest{Provider: "morde", Model: value.ModelID, MaxTokens: &route.MaxOutputTokens,
		Messages: []dshModelMessage{
			{Role: "system", Content: []dshModelBlock{{Type: "text", Text: strings.TrimSpace(mordeSoulFile) + "\n\n" + route.Instructions}}},
			{Role: "user", Content: []dshModelBlock{{Type: "text", Text: strings.Repeat("汉", 3200)}}},
		}, Tools: []dshModelTool{}}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CompleteConversationCoordinatorModel(context.Background(), claim, value, "coord_session_12345678", 0, raw)
	if domain.CodeOf(err) != domain.CodeResultTooLarge || store.reservation.ModelID != "" || store.begins != 0 || client.calls != 0 {
		t.Fatalf("over-budget coordinator request was reserved or dispatched: code=%s reservation=%+v begins=%d calls=%d",
			domain.CodeOf(err), store.reservation, store.begins, client.calls)
	}
}

func TestConversationRouteProviderContextLimitHasBoundedFailure(t *testing.T) {
	claim, value := routeGatewayFixture()
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{err: &ModelClientError{Kind: ModelErrorInvalidRequest,
		Stage: "provider_status", ProviderCode: "context_length_exceeded"}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ClassifyConversationRoute(context.Background(), claim, value); domain.CodeOf(err) != domain.CodeResultTooLarge ||
		!store.unknown || store.decision != nil || client.calls != 1 {
		t.Fatalf("provider context overflow did not produce a bounded, non-replayed failure: code=%s unknown=%t decision=%+v calls=%d",
			domain.CodeOf(err), store.unknown, store.decision, client.calls)
	}
}
