package app_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/model"
)

type liveModelCallStore struct {
	reservations []app.ModelCallReservation
	settlements  []app.ModelCallSettlement
}

func (store *liveModelCallStore) ReserveModelCall(_ context.Context, value app.ModelCallReservation) error {
	store.reservations = append(store.reservations, value)
	return nil
}

func (store *liveModelCallStore) BeginModelCallDispatch(_ context.Context, _ app.ModelCallReservation) error {
	return nil
}

func (store *liveModelCallStore) AbandonModelCallReservation(_ context.Context, _ app.ModelCallReservation) error {
	return nil
}

func (store *liveModelCallStore) SettleModelCall(_ context.Context, value app.ModelCallSettlement) error {
	store.settlements = append(store.settlements, value)
	return nil
}

type liveModelResolver struct{ endpoint app.ModelEndpoint }

func (resolver *liveModelResolver) ResolveGatewayEndpoint(_ context.Context, _, _ string, _, _ bool) (app.ModelGatewayEndpoint, error) {
	return app.ModelGatewayEndpoint{Endpoint: resolver.endpoint, Version: time.Unix(1, 0), Personal: true}, nil
}

func (resolver *liveModelResolver) ResolveProbeEndpoint(ctx context.Context, principal identity.Principal, modelID string) (app.ModelGatewayEndpoint, error) {
	return resolver.ResolveGatewayEndpoint(ctx, principal.UserID, modelID, true, true)
}

func (*liveModelResolver) SaveProbeCapabilities(_ context.Context, _ identity.Principal, _ string, _ time.Time, capabilities app.ModelCapabilities) (app.ModelCapabilities, error) {
	return capabilities, nil
}

func TestOpenAIGPT6LunaThroughMordeModelGateway(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("MORDE_TEST_OPENAI_KEY"))
	if key == "" {
		t.Skip("MORDE_TEST_OPENAI_KEY is unset; real model-gateway test was not run")
	}
	client, err := model.NewResponsesClient([]string{"api.openai.com"}, os.Getenv("MORDE_TEST_MODEL_PROXY"))
	if err != nil {
		t.Fatal(err)
	}
	store := &liveModelCallStore{}
	resolver := &liveModelResolver{endpoint: app.ModelEndpoint{
		BaseURL: "https://api.openai.com/v1", APIKey: key, ModelID: "gpt-6-luna",
	}}
	gateway, err := app.NewModelGatewayService(store, resolver, client)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindClient,
		TokenID: "t1234567", ClientInstallationID: "i1234567",
	}
	searchTool := app.ModelTool{
		Name: "morde_web_search", Description: "Find a synthetic result.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
		Strict:     true,
	}
	call := app.ModelCall{
		Source: app.ModelCallClient, Principal: principal, ModelID: "p1234567", CallID: "live-model-call-0001",
		Request: app.ModelRequest{
			Instructions: "Use the search tool for the synthetic query ALPHA-24. Do not invent a result.",
			Input:        []app.ModelInput{{Type: "message", Role: "user", Text: "Search ALPHA-24 and tell me what you found."}},
			Tools:        []app.ModelTool{searchTool}, RequiredTool: searchTool.Name,
			MaxOutputTokens: 64, DisableThinking: true,
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	first, err := gateway.Complete(ctx, call)
	if err != nil {
		t.Fatalf("first gateway call failed: %v", err)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != searchTool.Name {
		t.Fatalf("Morde gateway did not receive one search tool call: response=%#v", first)
	}
	toolCall := first.ToolCalls[0]
	call.CallID = "live-model-call-0002"
	call.Request.RequiredTool = ""
	call.Request.Input = append(call.Request.Input,
		app.ModelInput{Type: "function_call", CallID: toolCall.CallID, Name: toolCall.Name, Arguments: toolCall.Arguments},
		app.ModelInput{Type: "function_call_output", CallID: toolCall.CallID, Output: "Synthetic lookup result: BETA-42."},
	)
	call.Request.MaxOutputTokens = 128
	second, err := gateway.Complete(ctx, call)
	if err != nil {
		t.Fatalf("tool-result gateway call failed: %v", err)
	}
	if !strings.Contains(second.Text, "BETA-42") {
		t.Fatalf("Morde gateway answer did not use the synthetic tool result: response=%#v", second)
	}
	if len(store.reservations) != 2 || len(store.settlements) != 2 {
		t.Fatalf("model usage ledger calls: reservations=%d settlements=%d", len(store.reservations), len(store.settlements))
	}
	for index, settlement := range store.settlements {
		if settlement.Unknown || !settlement.Usage.Known || settlement.Usage.InputTokens < 1 || settlement.Usage.OutputTokens < 1 {
			t.Fatalf("model call %d usage settlement was invalid: %#v", index+1, settlement)
		}
	}
}
