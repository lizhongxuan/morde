package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain/identity"
)

const (
	modelBudgetShadowTestBaseURL = "https://shadow-provider.example/v1"
	modelBudgetShadowTestModel   = "shadow-test-model"
	modelBudgetShadowTestPrompt  = "private-shadow-prompt-canary"
	modelBudgetShadowTestAPIKey  = "private-shadow-api-key-canary"
)

type modelBudgetShadowTestStore struct {
	*modelGatewayStoreTest
	preview    ModelTaskBudgetShadowPreview
	previewErr error
	previewN   int
}

func (s *modelBudgetShadowTestStore) PreviewTaskModelBudget(_ context.Context,
	_ ModelTaskBudgetShadowPreviewRequest) (ModelTaskBudgetShadowPreview, error) {
	s.previewN++
	return s.preview, s.previewErr
}

type modelBudgetShadowTestResolver struct {
	endpoint ModelGatewayEndpoint
	err      error
}

func (r *modelBudgetShadowTestResolver) ResolveGatewayEndpoint(context.Context, string, string, bool, bool) (ModelGatewayEndpoint, error) {
	return r.endpoint, r.err
}

func (r *modelBudgetShadowTestResolver) ResolveProbeEndpoint(context.Context, identity.Principal, string) (ModelGatewayEndpoint, error) {
	return r.endpoint, nil
}

func (*modelBudgetShadowTestResolver) SaveProbeCapabilities(_ context.Context, _ identity.Principal, _ string,
	_ time.Time, value ModelCapabilities) (ModelCapabilities, error) {
	return value, nil
}

type modelBudgetShadowTestRun struct {
	service      *ModelGatewayService
	store        *modelBudgetShadowTestStore
	client       *modelGatewayClientTest
	call         ModelCall
	observations []ModelOutputBudgetShadowObservation
}

func newModelBudgetShadowTestRun(t *testing.T, rootSessionID string, preview ModelTaskBudgetShadowPreview,
	previewErr error, sampleRate float64, observe func(ModelOutputBudgetShadowObservation)) *modelBudgetShadowTestRun {
	t.Helper()

	modelMaxOutput, contextWindow := 16_000, 64_000
	store := &modelBudgetShadowTestStore{
		modelGatewayStoreTest: &modelGatewayStoreTest{},
		preview:               preview,
		previewErr:            previewErr,
	}
	client := &modelGatewayClientTest{response: ModelResponse{
		Text: "provider response", StopReason: "stop", ProviderReportedModel: modelBudgetShadowTestModel,
		Usage: ModelUsage{Known: true, InputTokens: 17, OutputTokens: 23},
	}}
	resolver := &modelBudgetShadowTestResolver{endpoint: ModelGatewayEndpoint{
		Endpoint: ModelEndpoint{BaseURL: modelBudgetShadowTestBaseURL, APIKey: modelBudgetShadowTestAPIKey,
			ModelID: modelBudgetShadowTestModel},
		Version: time.Unix(1, 0), Personal: true,
		ContextWindowTokens: &contextWindow, MaxOutputTokens: &modelMaxOutput,
	}}
	service, err := NewModelGatewayService(store, resolver, client)
	if err != nil {
		t.Fatal(err)
	}
	setTestModelResponseSealer(t, service)

	definition := syntheticModelOutputBudgetStrategy()
	definition.Models[0].BaseURL = modelBudgetShadowTestBaseURL
	definition.Models[0].Model = modelBudgetShadowTestModel
	if err := service.SetModelOutputBudgetShadow(definition, sampleRate); err != nil {
		t.Fatal(err)
	}
	fence := &ModelTaskFence{
		TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", LeaseOwner: "worker-1", LeaseVersion: 1,
		AgentInstanceID: "agent-1", AgentGeneration: 1, AgentSessionID: "session-child", AgentRootSessionID: rootSessionID,
		AuthorizationVersion: 1, ThroughSequence: 0, EngineVersion: "dsh-1", ProfileVersion: "v1",
	}
	callID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelCall{
		Source: ModelCallTask, Task: fence, ModelID: "p1234567", CallID: callID,
		RawRequest: []byte(fmt.Sprintf(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":%q}]}],"tools":[],"max_tokens":321}`,
			modelBudgetShadowTestPrompt)),
	}
	run := &modelBudgetShadowTestRun{service: service, store: store, client: client, call: call}
	service.SetModelOutputBudgetShadowObserver(func(value ModelOutputBudgetShadowObservation) {
		run.observations = append(run.observations, value)
		if observe != nil {
			observe(value)
		}
	})
	return run
}

func modelBudgetShadowTestPreview(taskLimit int64) ModelTaskBudgetShadowPreview {
	return ModelTaskBudgetShadowPreview{
		CapturedAt: time.Unix(1, 0).UTC(), TaskApplicable: true, TaskOutputLimit: &taskLimit,
	}
}

func (run *modelBudgetShadowTestRun) complete(t *testing.T) ModelResponse {
	t.Helper()
	response, err := run.service.Complete(context.Background(), run.call)
	if err != nil {
		t.Fatalf("complete Task model call: %v", err)
	}
	return response
}

func TestTaskModelBudgetShadowDoesNotChangeProviderCapOrLedgerDispatch(t *testing.T) {
	shadow := newModelBudgetShadowTestRun(t, "session-root", modelBudgetShadowTestPreview(900), nil, 1, nil)
	var shadowObservations []ModelOutputBudgetShadowObservation
	shadow.service.SetModelOutputBudgetShadowObserver(func(value ModelOutputBudgetShadowObservation) {
		shadowObservations = append(shadowObservations, value)
	})
	shadowResponse := shadow.complete(t)

	legacy := newModelBudgetShadowTestRun(t, "session-root", modelBudgetShadowTestPreview(900), nil, 0, nil)
	legacyResponse := legacy.complete(t)

	if shadow.client.request.MaxOutputTokens != legacy.client.request.MaxOutputTokens || shadow.client.request.MaxOutputTokens != 321 {
		t.Fatalf("shadow changed the provider output cap: shadow=%d legacy=%d",
			shadow.client.request.MaxOutputTokens, legacy.client.request.MaxOutputTokens)
	}
	if shadow.store.reservation.ReservedTokens != legacy.store.reservation.ReservedTokens ||
		shadow.store.reserveN != legacy.store.reserveN || shadow.store.dispatchN != legacy.store.dispatchN ||
		shadow.store.reserveN != 1 || shadow.store.dispatchN != 1 || shadow.client.calls != legacy.client.calls || shadow.client.calls != 1 {
		t.Fatalf("shadow changed live reservation or dispatch behavior: shadow reservation=%+v reserve=%d dispatch=%d provider=%d; legacy reservation=%+v reserve=%d dispatch=%d provider=%d",
			shadow.store.reservation, shadow.store.reserveN, shadow.store.dispatchN, shadow.client.calls,
			legacy.store.reservation, legacy.store.reserveN, legacy.store.dispatchN, legacy.client.calls)
	}
	if shadowResponse.Text != legacyResponse.Text || shadowResponse.Usage != legacyResponse.Usage {
		t.Fatalf("shadow changed the model result: shadow=%+v legacy=%+v", shadowResponse, legacyResponse)
	}
	if shadow.store.previewN != 1 || len(shadowObservations) != 2 {
		t.Fatalf("shadow plan and result were not both observed: preview_calls=%d observations=%+v", shadow.store.previewN, shadowObservations)
	}
	plan, result := shadowObservations[0], shadowObservations[1]
	if plan.Phase != "plan" || plan.Outcome != "calculated" || plan.Stage != "subtask" ||
		plan.StrategyVersion != "test-policy-v1" || plan.GoalTokens != 1_024 || plan.MinimumTokens != 512 ||
		plan.SafeTokens != 900 || plan.SendTokens != 900 || plan.CurrentOutputCap != 321 ||
		!reflect.DeepEqual(plan.LimitingDimensions, []string{"task_budget"}) {
		t.Fatalf("unexpected shadow plan observation: %+v", plan)
	}
	if result.Phase != "result" || result.UsageKnown != true || result.ActualOutputTokens != 23 ||
		result.StopReason != "stop" || result.FailureClass != "none" || result.RouteFingerprint != plan.RouteFingerprint {
		t.Fatalf("unexpected shadow result observation: %+v", result)
	}
	if !strings.HasPrefix(plan.RouteFingerprint, "sha256:") || strings.Contains(plan.RouteFingerprint, modelBudgetShadowTestBaseURL) {
		t.Fatalf("observation did not use a route fingerprint: %q", plan.RouteFingerprint)
	}
	assertModelBudgetShadowObservationContainsNoPromptOrCredentials(t, shadowObservations)
}

func TestTaskModelBudgetShadowUsesAdaptiveProtocolCapacityAboveLegacyLimit(t *testing.T) {
	taskLimit := int64(60_000)
	run := newModelBudgetShadowTestRun(t, "session-root", modelBudgetShadowTestPreview(taskLimit), nil, 1, nil)
	resolver := run.service.models.(*modelBudgetShadowTestResolver)
	modelMaxOutput, contextWindow := 64_000, 128_000
	resolver.endpoint.MaxOutputTokens = &modelMaxOutput
	resolver.endpoint.ContextWindowTokens = &contextWindow
	definition := syntheticModelOutputBudgetStrategy()
	definition.Models[0].BaseURL = modelBudgetShadowTestBaseURL
	definition.Models[0].Model = modelBudgetShadowTestModel
	for index := range definition.Models[0].Stages {
		if definition.Models[0].Stages[index].Stage == string(modelOutputBudgetStageSubtask) {
			definition.Models[0].Stages[index].GoalTokens = 40_000
			definition.Models[0].Stages[index].MinimumTokens = 512
		}
	}
	if err := run.service.SetModelOutputBudgetShadow(definition, 1); err != nil {
		t.Fatal(err)
	}

	run.complete(t)
	if len(run.observations) != 2 {
		t.Fatalf("shadow plan and result observations = %d, want 2: %+v", len(run.observations), run.observations)
	}
	plan := run.observations[0]
	if plan.SafeTokens != 60_000 || plan.SendTokens != 40_000 || plan.MinimumTokens != 512 ||
		!reflect.DeepEqual(plan.LimitingDimensions, []string{"stage_goal"}) {
		t.Fatalf("shadow did not model an adaptive cap above the legacy protocol limit: %+v", plan)
	}
	if run.client.request.MaxOutputTokens != 321 {
		t.Fatalf("shadow changed the live legacy output cap: %d", run.client.request.MaxOutputTokens)
	}
}

func TestTaskModelBudgetShadowDoesNotGuessRootTextOnlyStage(t *testing.T) {
	run := newModelBudgetShadowTestRun(t, "session-child", modelBudgetShadowTestPreview(900), nil, 1, nil)
	response := run.complete(t)

	if response.Text == "" || run.client.calls != 1 || run.store.reserveN != 1 || run.store.dispatchN != 1 || run.store.previewN != 0 {
		t.Fatalf("unknown-stage shadow blocked or previewed a normal Task call: response=%+v provider=%d reserve=%d dispatch=%d preview=%d",
			response, run.client.calls, run.store.reserveN, run.store.dispatchN, run.store.previewN)
	}
	if len(run.observations) != 2 {
		t.Fatalf("unknown-stage plan and result observations = %d, want 2: %+v", len(run.observations), run.observations)
	}
	for _, observation := range run.observations {
		if observation.Stage != modelBudgetShadowStageUnknown || observation.BlockReason != modelBudgetShadowStageUnknown ||
			observation.Outcome != "blocked" {
			t.Fatalf("root text-only stage was guessed instead of marked unknown: %+v", observation)
		}
	}
}

func TestTaskModelBudgetShadowObserverPanicDoesNotAffectComplete(t *testing.T) {
	run := newModelBudgetShadowTestRun(t, "session-root", modelBudgetShadowTestPreview(900), nil, 1,
		func(ModelOutputBudgetShadowObservation) { panic("observer failure") })
	response := run.complete(t)

	if response.Text == "" || run.client.calls != 1 || run.store.reserveN != 1 || run.store.dispatchN != 1 || run.store.settleN != 1 {
		t.Fatalf("observer panic affected a normal provider call: response=%+v provider=%d reserve=%d dispatch=%d settle=%d",
			response, run.client.calls, run.store.reserveN, run.store.dispatchN, run.store.settleN)
	}
}

func TestTaskModelBudgetShadowPreviewErrorRecordsBoundedReasonAndDoesNotBlock(t *testing.T) {
	previewErr := errors.New("private database endpoint and credentials must not be observed")
	run := newModelBudgetShadowTestRun(t, "session-root", ModelTaskBudgetShadowPreview{}, previewErr, 1, nil)
	response := run.complete(t)

	if response.Text == "" || run.client.calls != 1 || run.store.reserveN != 1 || run.store.dispatchN != 1 || run.store.settleN != 1 {
		t.Fatalf("preview failure blocked a normal provider call: response=%+v provider=%d reserve=%d dispatch=%d settle=%d",
			response, run.client.calls, run.store.reserveN, run.store.dispatchN, run.store.settleN)
	}
	if run.store.previewN != 1 || len(run.observations) != 2 {
		t.Fatalf("preview failure did not produce bounded plan/result observations: preview=%d observations=%+v", run.store.previewN, run.observations)
	}
	for _, observation := range run.observations {
		if observation.BlockReason != "budget_snapshot_unavailable" || observation.Outcome != "blocked" {
			t.Fatalf("preview failure reason was not normalized: %+v", observation)
		}
	}
	assertModelBudgetShadowObservationContainsNoPromptOrCredentials(t, run.observations)
}

func TestTaskModelBudgetShadowSampleRateZeroDoesNotObserveOrPreview(t *testing.T) {
	run := newModelBudgetShadowTestRun(t, "session-root", modelBudgetShadowTestPreview(900), nil, 0, nil)
	response := run.complete(t)

	if response.Text == "" || run.client.calls != 1 || run.store.reserveN != 1 || run.store.dispatchN != 1 {
		t.Fatalf("sample rate zero changed normal provider behavior: response=%+v provider=%d reserve=%d dispatch=%d",
			response, run.client.calls, run.store.reserveN, run.store.dispatchN)
	}
	if run.store.previewN != 0 || len(run.observations) != 0 {
		t.Fatalf("sample rate zero performed shadow work: preview=%d observations=%+v", run.store.previewN, run.observations)
	}
}

func assertModelBudgetShadowObservationContainsNoPromptOrCredentials(t *testing.T, observations []ModelOutputBudgetShadowObservation) {
	t.Helper()
	for _, observation := range observations {
		// Serialize only the allowlisted payload fields; Context is intentionally
		// excluded because it is the caller's trace context, not event data.
		payload, err := json.Marshal(struct {
			Phase              string
			RouteFingerprint   string
			StrategyVersion    string
			Stage              string
			Outcome            string
			BlockReason        string
			LimitingDimensions []string
			GoalTokens         int64
			MinimumTokens      int64
			SafeTokens         int64
			SendTokens         int64
			CurrentOutputCap   int64
			UsageKnown         bool
			ActualOutputTokens int64
			StopReason         string
			FailureClass       string
		}{
			Phase: observation.Phase, RouteFingerprint: observation.RouteFingerprint,
			StrategyVersion: observation.StrategyVersion, Stage: observation.Stage, Outcome: observation.Outcome,
			BlockReason: observation.BlockReason, LimitingDimensions: observation.LimitingDimensions,
			GoalTokens: observation.GoalTokens, MinimumTokens: observation.MinimumTokens,
			SafeTokens: observation.SafeTokens, SendTokens: observation.SendTokens,
			CurrentOutputCap: observation.CurrentOutputCap, UsageKnown: observation.UsageKnown,
			ActualOutputTokens: observation.ActualOutputTokens, StopReason: observation.StopReason,
			FailureClass: observation.FailureClass,
		})
		if err != nil {
			t.Fatalf("marshal allowlisted shadow observation: %v", err)
		}
		if strings.Contains(string(payload), modelBudgetShadowTestPrompt) || strings.Contains(string(payload), modelBudgetShadowTestAPIKey) ||
			strings.Contains(string(payload), modelBudgetShadowTestBaseURL) {
			t.Fatalf("shadow observation contains prompt, credentials, or raw route: %s", payload)
		}
	}
}
