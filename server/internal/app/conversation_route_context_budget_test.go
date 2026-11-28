package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

func TestConversationRouteModelRequestFitsMinimumWindowForShortMessages(t *testing.T) {
	for _, message := range []string{"你好", "???"} {
		t.Run(message, func(t *testing.T) {
			request, err := ConversationRouteModelRequest(ConversationRouteContext{ActorID: "u1234567", Content: message})
			if err != nil {
				t.Fatal(err)
			}
			if !ConversationRouteInputFitsContextWindow(request, 4096) {
				t.Fatalf("short conversation route request does not fit the minimum window (output=%d)", request.MaxOutputTokens)
			}
		})
	}
}

func TestConversationRouteModelRequestProjectsLargeCurrentMessages(t *testing.T) {
	for _, size := range []int{8<<10 + 1, 16<<10 + 1, 200<<10 + 1} {
		t.Run(fmt.Sprintf("%d_bytes", size), func(t *testing.T) {
			prefix, middle, suffix := "START-ANCHOR\n", "MIDDLE-ANCHOR\n", "END-ANCHOR"
			padding := size - len(prefix) - len(middle) - len(suffix)
			content := prefix + strings.Repeat("x", padding/2) + middle + strings.Repeat("y", padding-padding/2) + suffix
			digest := sha256.Sum256([]byte(content))
			request, err := ConversationRouteModelRequest(ConversationRouteContext{ActorID: "u1234567", CurrentMessageID: "m1234567", Content: content})
			if err != nil {
				t.Fatal(err)
			}
			var input struct {
				Message          string `json:"message"`
				MessageID        string `json:"message_id"`
				MessageBytes     int64  `json:"message_bytes"`
				MessageSHA256    string `json:"message_sha256"`
				MessageTruncated bool   `json:"message_truncated"`
			}
			if err := json.Unmarshal([]byte(request.Input[0].Text), &input); err != nil {
				t.Fatal(err)
			}
			if !input.MessageTruncated || input.MessageID != "m1234567" || input.MessageBytes != int64(len(content)) ||
				input.MessageSHA256 != hex.EncodeToString(digest[:]) || len(request.Input[0].Text) > 12<<10 ||
				!strings.Contains(input.Message, "START-ANCHOR") || !strings.Contains(input.Message, "END-ANCHOR") ||
				strings.Contains(input.Message, "MIDDLE-ANCHOR") || !strings.Contains(request.Instructions, "morde_search_current_conversation_message") {
				t.Fatalf("large route input was not represented by a bounded, verifiable source reference: bytes=%d input=%+v requestBytes=%d",
					size, input, len(request.Input[0].Text))
			}
		})
	}
}

func TestConversationRouteContextMatchesRuneSafeCurrentMessagePreviews(t *testing.T) {
	claim, _ := routeGatewayFixture()
	claim.Message.Content = strings.Repeat("a汉", 1024)
	for _, previewBytes := range []int{ConversationRouteCurrentMessagePreviewBytes, 1024} {
		value := ConversationRouteContext{ActorID: claim.Message.ActorID, CurrentMessageID: claim.Message.ID, Content: claim.Message.Content}
		ProjectConversationRouteMessage(&value, previewBytes)
		if len(value.Content) >= previewBytes || !conversationRouteContextMatchesClaim(value, claim) {
			t.Fatalf("rune-safe %d-byte preview did not match its original source: actual=%d", previewBytes, len(value.Content))
		}
	}
}

func TestConversationRouteTaskObjectiveReferencesLargeSource(t *testing.T) {
	content := "START-ANCHOR\n" + strings.Repeat("x", 200<<10) + "\nEND-ANCHOR"
	digest := sha256.Sum256([]byte(content))
	objective := ConversationRouteTaskMessageObjective("m1234567", content)
	if len(objective) > 8<<10 || !strings.Contains(objective, "message_id=m1234567") ||
		!strings.Contains(objective, "sha256="+hex.EncodeToString(digest[:])) ||
		!strings.Contains(objective, "morde_read_current_conversation_message") ||
		!strings.Contains(objective, "morde_search_current_conversation_message") ||
		!strings.Contains(objective, "START-ANCHOR") || !strings.Contains(objective, "END-ANCHOR") ||
		strings.Contains(objective, strings.Repeat("x", 4096)) {
		t.Fatalf("large Task objective did not retain a bounded source pointer: objectiveBytes=%d", len(objective))
	}
}

func TestConversationRouteBudgetCountsMordeSoulOnceForPreflightAndDSH(t *testing.T) {
	soulTokens := conversationRouteTextTokenEstimate(strings.TrimSpace(mordeSoulFile))
	if conversationRouteRuntimePromptReserveTokens < int(soulTokens)+4 {
		t.Fatalf("runtime reserve=%d does not cover Morde SOUL estimate=%d plus system-role framing",
			conversationRouteRuntimePromptReserveTokens, soulTokens)
	}
	preflight := ModelRequest{Instructions: "route", Input: []ModelInput{{Role: "user", Text: strings.Repeat("x", 20_000)}}, MaxOutputTokens: 512}
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

func TestResolveConversationSummaryModelUsesMinimumWindowWhenCapacityIsUnknown(t *testing.T) {
	claim, _ := routeGatewayFixture()
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, &modelGatewayClientTest{})
	if err != nil {
		t.Fatal(err)
	}
	modelID, window, err := service.ResolveConversationSummaryModel(context.Background(), claim)
	if err != nil || modelID != "p1234567" || window != 4096 {
		t.Fatalf("unknown-capacity summary model = %q, %d, %v; want selected model at safe 4K minimum", modelID, window, err)
	}
}

func TestConversationRouteGatewayRejectsOverBudgetBeforeReservation(t *testing.T) {
	claim, value := routeGatewayFixture()
	claim.Message.Content, value.Content = strings.Repeat("汉", 3200), strings.Repeat("汉", 3200)
	value.CurrentMessageID = claim.Message.ID
	store := &routeGatewayStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{}
	service, err := NewModelGatewayService(store, &modelGatewayResolverTest{}, client)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.ClassifyConversationRoute(context.Background(), claim, value)
	if domain.CodeOf(err) != domain.CodeResultTooLarge ||
		store.reservation.ModelID != "" || store.begins != 0 || client.calls != 0 {
		t.Fatalf("over-budget route was reserved or dispatched: code=%s reservation=%+v begins=%d calls=%d",
			domain.CodeOf(err), store.reservation, store.begins, client.calls)
	}
	var routeErr *domain.Error
	if !errors.As(err, &routeErr) || routeErr.Details["stage"] != "gateway_preflight" ||
		routeErr.Details["context_window_tokens"] != 4096 || routeErr.Details["context_window_known"] != false {
		t.Fatalf("preflight failure omitted safe window diagnostics: %+v", routeErr)
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
	_, err = service.ClassifyConversationRoute(context.Background(), claim, value)
	if domain.CodeOf(err) != domain.CodeResultTooLarge ||
		!store.unknown || store.decision != nil || client.calls != 1 {
		t.Fatalf("provider context overflow did not produce a bounded, non-replayed failure: code=%s unknown=%t decision=%+v calls=%d",
			domain.CodeOf(err), store.unknown, store.decision, client.calls)
	}
	var routeErr *domain.Error
	if !errors.As(err, &routeErr) || routeErr.Details["stage"] != "provider_status" ||
		routeErr.Details["provider_code"] != "context_length_exceeded" {
		t.Fatalf("provider context overflow lost its safe failure category: %+v", routeErr)
	}
}
