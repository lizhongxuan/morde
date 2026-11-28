package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func TestTaskModelOutputBudgetStageUsesSummaryForCompaction(t *testing.T) {
	rootFence := ModelTaskFence{AgentSessionID: "root-session", AgentRootSessionID: "root-session"}
	if got := taskModelOutputBudgetStage(rootFence, false, ""); got != modelOutputBudgetStageShortAnswer {
		t.Fatalf("ordinary no-tool Task stage = %q, want short_answer", got)
	}
	if got := taskModelOutputBudgetStage(rootFence, true, ""); got != modelOutputBudgetStageToolStep {
		t.Fatalf("ordinary tool Task stage = %q, want tool_step", got)
	}
	childFence := ModelTaskFence{AgentSessionID: "child-session", AgentRootSessionID: "root-session"}
	if got := taskModelOutputBudgetStage(childFence, true, ""); got != modelOutputBudgetStageSubtask {
		t.Fatalf("ordinary child Agent stage = %q, want subtask", got)
	}
	if got := taskModelOutputBudgetStage(childFence, true, "compaction"); got != modelOutputBudgetStageSummary {
		t.Fatalf("compaction stage = %q, want summary even for a tool-using child Agent", got)
	}
}

func TestTaskModelBudgetAdmissionRequiresVerifiedCapacityAndExactStrategyRoute(t *testing.T) {
	maxOutput, contextWindow := 16_000, 64_000
	endpoint := ModelGatewayEndpoint{
		Endpoint: ModelEndpoint{BaseURL: modelBudgetShadowTestBaseURL, ModelID: modelBudgetShadowTestModel},
		Version:  time.Unix(1, 0), MaxOutputTokens: &maxOutput, MaxOutputSource: ModelMaxOutputModelCatalog,
		ContextWindowTokens: &contextWindow, ContextWindowSource: ModelContextWindowModelCatalog,
	}
	resolver := &modelBudgetShadowTestResolver{endpoint: endpoint}
	service, err := NewModelGatewayService(&modelGatewayStoreTest{}, resolver, &modelGatewayClientTest{})
	if err != nil {
		t.Fatal(err)
	}
	value := task.Task{InitiatorID: "u1234567", ModelID: "p1234567"}
	if eligible, err := service.SupportsTaskModelBudgetAdmission(context.Background(), value); err != nil || eligible {
		t.Fatalf("route without strategy eligibility=%t err=%v, want false", eligible, err)
	}

	definition := syntheticModelOutputBudgetStrategy()
	definition.Models[0].BaseURL = modelBudgetShadowTestBaseURL
	definition.Models[0].Model = modelBudgetShadowTestModel
	if err := service.SetModelOutputBudgetStrategy(definition); err != nil {
		t.Fatal(err)
	}
	if eligible, err := service.SupportsTaskModelBudgetAdmission(context.Background(), value); err != nil || !eligible {
		t.Fatalf("reviewed exact route eligibility=%t err=%v, want true", eligible, err)
	}

	unknownCapacity := *resolver
	unknownCapacity.endpoint.MaxOutputSource = ModelMaxOutputUnknown
	service.models = &unknownCapacity
	if eligible, err := service.SupportsTaskModelBudgetAdmission(context.Background(), value); err != nil || eligible {
		t.Fatalf("unknown-capacity route eligibility=%t err=%v, want false", eligible, err)
	}

	unlistedRoute := *resolver
	unlistedRoute.endpoint.Endpoint.ModelID = "unlisted-model"
	service.models = &unlistedRoute
	if eligible, err := service.SupportsTaskModelBudgetAdmission(context.Background(), value); err != nil || eligible {
		t.Fatalf("unlisted route eligibility=%t err=%v, want false", eligible, err)
	}
}

func TestAdaptiveBudgetRejectsStaleCatalogCapacityFromConnectionResolver(t *testing.T) {
	for _, staleField := range []string{"output", "context"} {
		t.Run(staleField, func(t *testing.T) {
			store := &modelConnectionStoreStub{values: make(map[string]ModelConnectionRecord)}
			modelConnections := modelConnectionTestService(t, store, &modelClientStub{})
			principal := identity.Principal{UserID: "u7654321", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
			created, err := modelConnections.Create(context.Background(), principal, ModelConnectionCommand{
				Name: "stale capacity", BaseURL: "https://api.openai.com/v1", ModelName: "gpt-6-luna", APIKey: "secret-value",
			})
			if err != nil {
				t.Fatal(err)
			}
			record := store.values[created.ID]
			checkedAt := time.Now().UTC()
			record.Capabilities = ModelCapabilities{Text: true}
			record.CheckedAt = &checkedAt
			staleVersion := "outdated-catalog-version"
			switch staleField {
			case "output":
				stale := 64_000
				record.MaxOutputTokens = &stale
				record.MaxOutputCatalogVersion = &staleVersion
			case "context":
				stale := 900_000
				record.ContextWindowTokens = &stale
				record.ContextWindowCatalogVersion = &staleVersion
			}
			store.values[created.ID] = record

			endpoint, err := modelConnections.ResolveGatewayEndpoint(context.Background(), principal.UserID, created.ID, false, false)
			if err != nil {
				t.Fatal(err)
			}
			if staleField == "output" && (endpoint.MaxOutputTokens != nil || endpoint.MaxOutputSource != ModelMaxOutputUnknown || endpoint.MaxOutputCatalogVersion != nil) {
				t.Fatalf("gateway resolver exposed stale output catalog capacity: %+v", endpoint)
			}
			if staleField == "context" && (endpoint.ContextWindowTokens != nil || endpoint.ContextWindowSource != ModelContextWindowUnknown || endpoint.ContextWindowCatalogVersion != nil) {
				t.Fatalf("gateway resolver exposed stale context catalog capacity: %+v", endpoint)
			}
			view, err := modelConnections.Get(context.Background(), principal, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if staleField == "context" && (view.ContextWindowTokens != nil || view.ContextWindowSource != ModelContextWindowUnknown || view.ContextWindowCatalogVersion != nil) {
				t.Fatalf("model connection view exposed stale context catalog capacity: %+v", view)
			}

			gateway, err := NewModelGatewayService(&modelGatewayStoreTest{}, modelConnections, &modelGatewayClientTest{})
			if err != nil {
				t.Fatal(err)
			}
			definition := syntheticModelOutputBudgetStrategy()
			definition.Models[0].BaseURL = "https://api.openai.com/v1"
			definition.Models[0].Model = "gpt-6-luna"
			if err := gateway.SetModelOutputBudgetStrategy(definition); err != nil {
				t.Fatal(err)
			}
			taskValue := task.Task{ID: "q1234567", InitiatorID: principal.UserID, ModelID: created.ID}
			if eligible, err := gateway.SupportsTaskModelBudgetAdmission(context.Background(), taskValue); err != nil || eligible {
				t.Fatalf("stale %s capacity eligibility=%t err=%v, want ineligible", staleField, eligible, err)
			}

			fence := ModelTaskFence{TaskID: taskValue.ID, OwnerID: principal.UserID, InitiatorID: principal.UserID,
				LeaseOwner: "worker-1", LeaseVersion: 1, AgentInstanceID: "agent-1", AgentGeneration: 1,
				AgentSessionID: "session-1", AgentRootSessionID: "session-1", AuthorizationVersion: 1,
				ThroughSequence: 0, EngineVersion: "dsh-1", ProfileVersion: "v1"}
			dshCallID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
			if err != nil {
				t.Fatal(err)
			}
			request := json.RawMessage(`{"provider":"morde","model":"` + created.ID + `","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[]}`)
			_, err = gateway.PrepareTaskModelBudget(context.Background(), TaskModelBudgetPrepareRequest{
				Fence: fence, DSHCallID: dshCallID, ModelID: created.ID, ThroughSequence: 0, Turn: 1, Step: 0,
				BudgetCallID: "budget_call_1", RequestDigest: strings.Repeat("a", 64), ProviderRequest: request,
			})
			if domain.CodeOf(err) != domain.CodeUnsupported {
				t.Fatalf("prepare with stale %s capacity err=%v, want unsupported before admission", staleField, err)
			}
			if staleField == "context" {
				refreshed, err := modelConnections.Update(context.Background(), principal, created.ID, ModelConnectionCommand{
					Name: created.Name, BaseURL: created.BaseURL, ModelName: created.ModelName,
				})
				if err != nil || refreshed.ContextWindowTokens == nil || *refreshed.ContextWindowTokens != 1_050_000 ||
					refreshed.ContextWindowCatalogVersion == nil || *refreshed.ContextWindowCatalogVersion != modelContextCapacityCatalogVersion {
					t.Fatalf("updating connection did not refresh stale context capacity: %+v err=%v", refreshed, err)
				}
			}
		})
	}
}

func TestTaskModelBudgetAdmissionPropagatesEndpointResolutionFailure(t *testing.T) {
	service, err := NewModelGatewayService(&modelGatewayStoreTest{}, &modelBudgetShadowTestResolver{err: errors.New("resolver unavailable")}, &modelGatewayClientTest{})
	if err != nil {
		t.Fatal(err)
	}
	if eligible, err := service.SupportsTaskModelBudgetAdmission(context.Background(), task.Task{InitiatorID: "u1234567", ModelID: "p1234567"}); err == nil || eligible {
		t.Fatalf("unresolved route eligibility=%t err=%v, want propagated failure", eligible, err)
	}
}

func TestAdaptiveModelBudgetIncludesGatewayAddedTaskInstructions(t *testing.T) {
	request := ModelRequest{Instructions: "runtime system prompt", Input: []ModelInput{{Type: "message", Role: "user", Text: "hello"}}}
	prepared := request
	prepared.Instructions = mordeSoulInstructions(prepared.Instructions)
	want := modelRequestInputTokenReservation(prepared)
	got := taskModelBudgetInputTokenReservation(request)
	if got != want || got <= modelRequestInputTokenReservation(request) {
		t.Fatalf("adaptive input hold = %d, expected gateway request hold %d and greater than raw DSH projection %d",
			got, want, modelRequestInputTokenReservation(request))
	}

	withSoul := request
	withSoul.Instructions = strings.TrimSpace(mordeSoulFile) + "\n\n" + request.Instructions
	preparedWithSoul := withSoul
	preparedWithSoul.Instructions = mordeSoulInstructions(withSoul.Instructions)
	gotWithSoul := taskModelBudgetInputTokenReservation(withSoul)
	if wantWithSoul := modelRequestInputTokenReservation(preparedWithSoul); gotWithSoul != wantWithSoul {
		t.Fatalf("DSH projection containing SOUL input reservation = %d, want final gateway request reservation %d",
			gotWithSoul, wantWithSoul)
	}
	if strings.Count(preparedWithSoul.Instructions, strings.TrimSpace(mordeSoulFile)) != 1 ||
		strings.Count(preparedWithSoul.Instructions, agentInstructions) != 1 {
		t.Fatalf("final gateway projection duplicated SOUL or policy: SOUL=%d policy=%d",
			strings.Count(preparedWithSoul.Instructions, strings.TrimSpace(mordeSoulFile)),
			strings.Count(preparedWithSoul.Instructions, agentInstructions))
	}
}

func TestModelBudgetFinalRequestDigestBindsExactOutputCap(t *testing.T) {
	providerDigest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	first, err := ModelBudgetFinalRequestDigest(providerDigest, 2048)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ModelBudgetFinalRequestDigest(providerDigest, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("changing the signed output cap did not change the request digest")
	}
	if _, err := ModelBudgetFinalRequestDigest("not-a-digest", 2048); err == nil {
		t.Fatal("invalid provider projection digest was accepted")
	}
}

func TestModelBudgetCapabilityVersionBindsCurrentEndpointSnapshot(t *testing.T) {
	maxOutput, contextWindow := 128_000, 256_000
	maxCatalog := "catalog-v1"
	contextCatalog := "context-catalog-v1"
	updated := time.Date(2026, time.November, 27, 12, 0, 0, 0, time.UTC)
	endpoint := ModelGatewayEndpoint{
		Endpoint: ModelEndpoint{BaseURL: "https://api.example/v1", ModelID: "model-a"}, Version: updated,
		MaxOutputTokens: &maxOutput, MaxOutputSource: ModelMaxOutputModelCatalog,
		MaxOutputCatalogVersion: &maxCatalog, ContextWindowTokens: &contextWindow,
		ContextWindowSource:         ModelContextWindowModelCatalog,
		ContextWindowCatalogVersion: &contextCatalog,
	}
	first := modelBudgetCapabilityVersion(endpoint)
	if first == "" || len(first) != len("cap_v1_")+64 {
		t.Fatalf("capability version = %q, want a versioned SHA-256 snapshot", first)
	}
	if got := modelBudgetCapabilityVersion(endpoint); got != first {
		t.Fatalf("unchanged endpoint snapshot changed version: %q != %q", got, first)
	}

	changedOutput := endpoint
	changedOutputTokens := maxOutput + 1
	changedOutput.MaxOutputTokens = &changedOutputTokens
	if got := modelBudgetCapabilityVersion(changedOutput); got == first {
		t.Fatal("output capacity change did not invalidate the snapshot")
	}
	changedContext := endpoint
	changedContextTokens := contextWindow + 1
	changedContext.ContextWindowTokens = &changedContextTokens
	if got := modelBudgetCapabilityVersion(changedContext); got == first {
		t.Fatal("context window change did not invalidate the snapshot")
	}
	changedCatalog := endpoint
	maxCatalogV2 := "catalog-v2"
	changedCatalog.MaxOutputCatalogVersion = &maxCatalogV2
	if got := modelBudgetCapabilityVersion(changedCatalog); got == first {
		t.Fatal("catalog version change did not invalidate the snapshot")
	}
	changedContextCatalog := endpoint
	contextCatalogV2 := "context-catalog-v2"
	changedContextCatalog.ContextWindowCatalogVersion = &contextCatalogV2
	if got := modelBudgetCapabilityVersion(changedContextCatalog); got == first {
		t.Fatal("context catalog version change did not invalidate the snapshot")
	}
	changedUpdatedAt := endpoint
	changedUpdatedAt.Version = updated.Add(time.Nanosecond)
	if got := modelBudgetCapabilityVersion(changedUpdatedAt); got == first {
		t.Fatal("connection update did not invalidate the snapshot")
	}
}

type modelBudgetCapacityWaitStoreTest struct {
	*modelGatewayStoreTest
	request TaskModelBudgetCapacityWaitRequest
	err     error
	calls   int
}

func (s *modelBudgetCapacityWaitStoreTest) WaitForTaskModelBudgetCapacity(_ context.Context, request TaskModelBudgetCapacityWaitRequest) error {
	s.calls++
	s.request = request
	return s.err
}

func (*modelBudgetCapacityWaitStoreTest) ReconcileTaskModelBudgetCapacityWaits(context.Context, int32) (TaskModelBudgetCapacityReconcileResult, error) {
	return TaskModelBudgetCapacityReconcileResult{}, nil
}

func TestWaitForTaskModelBudgetCapacityOnlyQueuesTransientConcurrencyLimit(t *testing.T) {
	concurrencyErr := domain.NewError(domain.CodeRateLimited, "model concurrency limit reached")
	concurrencyErr.Details["reason"] = "model_concurrency_limit"
	concurrencyErr.Details["stage"] = "reservation"
	if !isTaskModelBudgetConcurrencyLimit(concurrencyErr) {
		t.Fatalf("concurrency error was not recognized: code=%q details=%#v", concurrencyErr.Code, concurrencyErr.Details)
	}
	store := &modelBudgetCapacityWaitStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}}
	service := &ModelGatewayService{store: store}
	fence := ModelTaskFence{TaskID: "t1234567", OwnerID: "u1234567", InitiatorID: "u1234567", LeaseOwner: "worker",
		LeaseVersion: 9, AgentInstanceID: "i1234567", AgentGeneration: 2, AgentSessionID: "s1234567",
		AgentRootSessionID: "s1234567", AuthorizationVersion: 3, ThroughSequence: 6}
	rate := &project.RateSnapshot{Provider: "provider", ModelID: "p1234567", Model: "provider-model", Currency: "USD", Version: 1,
		InputMicrosPerMillion: 1_000_000, OutputMicrosPerMillion: 1_000_000}

	got := service.waitForTaskModelBudgetCapacity(context.Background(), concurrencyErr, fence, "p1234567", "dsh-call", "ledger-call",
		"budget-call", strings.Repeat("a", 64), 130, 500, "j1234567", rate)
	if !errors.Is(got, ErrTaskModelBudgetCapacityWaiting) || store.calls != 1 {
		t.Fatalf("wait result = %v; calls = %d, want durable capacity wait", got, store.calls)
	}
	want := TaskModelBudgetCapacityWaitRequest{Fence: fence, ModelID: "p1234567", ProjectID: "j1234567", DSHCallID: "dsh-call",
		LedgerCallID: "ledger-call", BudgetCallID: "budget-call", RequestDigest: strings.Repeat("a", 64),
		InputTokenReservation: 130, MinimumOutputTokens: 500, MinimumProjectMicros: 630, ProjectBudgetCurrency: "USD"}
	if store.request != want {
		t.Fatalf("persisted wait request = %+v, want %+v", store.request, want)
	}

	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "quota exhausted", err: domain.NewError(domain.CodeQuotaExceeded, "budget exhausted")},
		{name: "provider rate limit", err: func() error {
			err := domain.NewError(domain.CodeRateLimited, "provider rate limit")
			err.Details["reason"], err.Details["stage"] = "provider_status", "provider_status"
			return err
		}()},
		{name: "wrong stage", err: func() error {
			err := domain.NewError(domain.CodeRateLimited, "model concurrency limit reached")
			err.Details["reason"], err.Details["stage"] = "model_concurrency_limit", "provider_status"
			return err
		}()},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := store.calls
			if got := service.waitForTaskModelBudgetCapacity(context.Background(), test.err, fence, "p1234567", "dsh-call", "ledger-call",
				"budget-call", strings.Repeat("a", 64), 130, 500, "j1234567", rate); got != test.err {
				t.Fatalf("non-capacity error = %v, want original error %v", got, test.err)
			}
			if store.calls != before {
				t.Fatalf("non-capacity error enqueued wait: calls %d -> %d", before, store.calls)
			}
		})
	}
}

func TestWaitForTaskModelBudgetCapacityFailsClosedWhenWaitCannotBePersisted(t *testing.T) {
	prepareErr := domain.NewError(domain.CodeRateLimited, "model concurrency limit reached")
	prepareErr.Details["reason"], prepareErr.Details["stage"] = "model_concurrency_limit", "reservation"
	persistErr := errors.New("database unavailable")
	service := &ModelGatewayService{store: &modelBudgetCapacityWaitStoreTest{modelGatewayStoreTest: &modelGatewayStoreTest{}, err: persistErr}}
	got := service.waitForTaskModelBudgetCapacity(context.Background(), prepareErr, ModelTaskFence{}, "p1234567", "dsh-call", "ledger-call",
		"budget-call", strings.Repeat("b", 64), 1, 2, "", nil)
	if errors.Is(got, ErrTaskModelBudgetCapacityWaiting) || !errors.Is(got, prepareErr) || !errors.Is(got, persistErr) {
		t.Fatalf("unpersisted capacity wait result = %v; want both prepare and storage errors and no waiting marker", got)
	}
}
