package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
)

type modelBudgetLunaPreparationStore struct {
	*modelGatewayStoreTest
	prepareCalls int
	preparation  ModelBudgetReservationPreparation
}

func (s *modelBudgetLunaPreparationStore) PrepareTaskModelBudget(_ context.Context,
	value ModelBudgetReservationPreparation) (ModelBudgetReservationResult, error) {
	s.prepareCalls++
	s.preparation = value
	send := min(value.GoalTokens, value.StaticSafeTokens)
	return ModelBudgetReservationResult{PlanID: value.PlanID, MaxOutputTokens: send}, nil
}

func (s *modelBudgetLunaPreparationStore) ValidateTaskModelBudgetPreparation(ctx context.Context,
	value ModelCallReservation) error {
	return s.modelGatewayStoreTest.ValidateTaskModelBudgetPreparation(ctx, value)
}

func TestGPT6LunaAdaptiveBudgetSignsAndDispatchesReasoningMinimum(t *testing.T) {
	store, service, fence, callID, budgetRequest := modelBudgetLunaFixture(t, 40_000, 128_000, 32_000)
	plan, err := service.PrepareTaskModelBudget(context.Background(), budgetRequest)
	if err != nil {
		t.Fatalf("prepare GPT-6 Luna adaptive budget: %v", err)
	}
	if store.prepareCalls != 1 || store.preparation.MinimumTokens != maxGPT6LunaReasoningTokens ||
		store.preparation.GoalTokens != 32_000 || store.preparation.StaticSafeTokens < maxGPT6LunaReasoningTokens ||
		int64(plan.MaxTokens) < maxGPT6LunaReasoningTokens {
		t.Fatalf("adaptive Luna plan did not sign the reasoning minimum: plan=%+v preparation=%+v calls=%d",
			plan, store.preparation, store.prepareCalls)
	}

	client := &modelGatewayClientTest{response: ModelResponse{Text: "done", StopReason: "stop", ProviderReportedModel: "gpt-6-luna",
		Usage: ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}}
	service.client = client
	setTestModelResponseSealer(t, service)
	finalRequest := fmt.Sprintf(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[],"max_tokens":%d}`, plan.MaxTokens)
	call := ModelCall{
		Source: ModelCallTask, Task: &fence, ModelID: budgetRequest.ModelID, CallID: callID, LedgerCallID: callID,
		BudgetPlanID: plan.PlanID, BudgetCallID: budgetRequest.BudgetCallID, BudgetRequestDigest: budgetRequest.RequestDigest,
		RawRequest: []byte(finalRequest),
	}
	if _, err := service.Complete(context.Background(), call); err != nil {
		t.Fatalf("dispatch GPT-6 Luna call using the prepared plan: %v", err)
	}
	expectedInputReservation := modelRequestInputTokenReservation(client.request)
	expectedReservation := expectedInputReservation + int64(plan.MaxTokens)
	if client.calls != 1 || client.request.MaxOutputTokens != plan.MaxTokens || client.request.ReasoningEffort != "max" ||
		client.request.DisableThinking || store.reservation.ReservedTokens != expectedReservation ||
		store.reservation.ReservedTokens < maxGPT6LunaReasoningTokens {
		t.Fatalf("prepared cap, provider cap, and reservation diverged: plan=%+v provider=%+v reservation=%+v calls=%d",
			plan, client.request, store.reservation, client.calls)
	}
}

func TestGPT6LunaAdaptiveBudgetFailsClosedBelowReasoningMinimum(t *testing.T) {
	tests := []struct {
		name     string
		modelMax int
		goal     int64
	}{
		{name: "goal below reasoning minimum", modelMax: 40_000, goal: maxGPT6LunaReasoningTokens - 1},
		{name: "verified model cap below reasoning minimum", modelMax: maxGPT6LunaReasoningTokens - 1, goal: 32_000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, service, _, _, request := modelBudgetLunaFixture(t, test.modelMax, 128_000, test.goal)
			_, err := service.PrepareTaskModelBudget(context.Background(), request)
			if domain.CodeOf(err) != domain.CodeQuotaExceeded || store.prepareCalls != 0 || store.reserveN != 0 ||
				store.dispatchN != 0 {
				t.Fatalf("insufficient adaptive Luna budget was not rejected before ledger write: err=%v prepare=%d reserve=%d dispatch=%d",
					err, store.prepareCalls, store.reserveN, store.dispatchN)
			}
		})
	}
}

func TestAdaptiveBudgetMarksOnlyContextWindowMinimumDenialForCompaction(t *testing.T) {
	t.Run("context window exhausted by projected input", func(t *testing.T) {
		store, service, _, _, request := modelBudgetLunaFixture(t, 40_000, 8_192, 32_000)
		request.ProviderRequest = json.RawMessage(fmt.Sprintf(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":%q}]}],"tools":[]}`, strings.Repeat("x", 10_000)))
		_, err := service.PrepareTaskModelBudget(context.Background(), request)
		var modelErr *domain.Error
		if !errors.As(err, &modelErr) || modelErr.Code != domain.CodeQuotaExceeded ||
			modelErr.Details["reason"] != "context_window_below_stage_minimum" || store.prepareCalls != 0 {
			t.Fatalf("context-bound request did not expose a pre-ledger compaction signal: err=%v details=%#v prepare_calls=%d",
				err, modelErr, store.prepareCalls)
		}
	})

	t.Run("verified model output cap exhausted", func(t *testing.T) {
		store, service, _, _, request := modelBudgetLunaFixture(t, 24_999, 128_000, 32_000)
		_, err := service.PrepareTaskModelBudget(context.Background(), request)
		var modelErr *domain.Error
		if !errors.As(err, &modelErr) || modelErr.Code != domain.CodeQuotaExceeded ||
			modelErr.Details["reason"] == "context_window_below_stage_minimum" || store.prepareCalls != 0 {
			t.Fatalf("model output ceiling was misclassified as context pressure: err=%v details=%#v prepare_calls=%d",
				err, modelErr, store.prepareCalls)
		}
	})
}

func modelBudgetLunaFixture(t *testing.T, modelMax, contextWindow int, goal int64) (
	*modelBudgetLunaPreparationStore, *ModelGatewayService, ModelTaskFence, string, TaskModelBudgetPrepareRequest,
) {
	t.Helper()
	store := &modelBudgetLunaPreparationStore{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	client := &modelGatewayClientTest{response: ModelResponse{Text: "unused", StopReason: "stop", ProviderReportedModel: "gpt-6-luna",
		Usage: ModelUsage{Known: true, InputTokens: 1, OutputTokens: 1}}}
	maxOutput, contextCapacity := modelMax, contextWindow
	resolver := &modelBudgetShadowTestResolver{endpoint: ModelGatewayEndpoint{
		Endpoint: ModelEndpoint{BaseURL: "https://api.openai.com/v1", APIKey: "test-key", ModelID: "gpt-6-luna"},
		Version:  time.Unix(1, 0), Personal: true, MaxOutputTokens: &maxOutput, MaxOutputSource: ModelMaxOutputOverride,
		ContextWindowTokens: &contextCapacity, ContextWindowSource: ModelContextWindowOverride,
	}}
	service, err := NewModelGatewayService(store, resolver, client)
	if err != nil {
		t.Fatal(err)
	}
	service.SetGPT6LunaMaxReasoning(true)
	definition := syntheticModelOutputBudgetStrategy()
	definition.Models[0].BaseURL = "https://api.openai.com/v1"
	definition.Models[0].Model = "gpt-6-luna"
	for index := range definition.Models[0].Stages {
		if definition.Models[0].Stages[index].Stage == string(modelOutputBudgetStageShortAnswer) {
			definition.Models[0].Stages[index].GoalTokens = goal
		}
	}
	if err := service.SetModelOutputBudgetStrategy(definition); err != nil {
		t.Fatal(err)
	}
	fence := ModelTaskFence{
		TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", LeaseOwner: "worker-1", LeaseVersion: 1,
		AgentInstanceID: "agent-1", AgentGeneration: 1, AgentSessionID: "session-1", AgentRootSessionID: "session-1",
		AuthorizationVersion: 1, EngineVersion: "dsh-1", ProfileVersion: "v1",
	}
	callID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	request := TaskModelBudgetPrepareRequest{
		Fence: fence, DSHCallID: callID, ModelID: "p1234567", ThroughSequence: 0, Turn: 1, Step: 0,
		BudgetCallID: "budget-call-123", RequestDigest: strings.Repeat("a", 64),
		ProviderRequest: json.RawMessage(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[]}`),
	}
	return store, service, fence, callID, request
}
