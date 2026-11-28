package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/worker"
)

// This client is deliberately in-process and synthetic. It exercises the real
// PostgreSQL ledger and gateway decisions without contacting a model provider.
type adaptiveBudgetCrashBoundaryClient struct {
	calls    atomic.Int32
	response app.ModelResponse
	err      error
}

func (client *adaptiveBudgetCrashBoundaryClient) Complete(_ context.Context, _ app.ModelRequest) (app.ModelResponse, error) {
	client.calls.Add(1)
	return client.response, client.err
}

func (*adaptiveBudgetCrashBoundaryClient) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{}, errors.New("unexpected model capability check")
}

func newAdaptiveBudgetCrashBoundaryGateway(t *testing.T, client *adaptiveBudgetCrashBoundaryClient) (
	routeActionsTestFixture, *app.ModelGatewayService, *adaptiveBudgetCrashBoundaryClient, app.ModelCall, worker.CloudDSHModelBudgetPlan,
) {
	t.Helper()

	fixture, _, _, fence, call := revisionGatewayFixture(t)
	ctx := context.Background()
	fence.AgentRootSessionID = fence.AgentSessionID
	if _, err := fixture.store.pool.Exec(ctx, `UPDATE model_connections SET base_url='https://models.invalid/v1',
context_window_tokens=32768,context_window_override_tokens=32768,context_window_source='legacy_manual',
context_window_resolved_at=NULL,context_window_catalog_version=NULL,max_output_tokens=8192,
max_output_override_tokens=8192,max_output_source='override',max_output_resolved_at=NULL,
max_output_catalog_version=NULL,max_output_source_url=NULL,updated_at=clock_timestamp() WHERE id=$1`, call.ModelID); err != nil {
		t.Fatalf("configure exact test route capacity: %v", err)
	}
	var connectionVersion time.Time
	if err := fixture.store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id=$1`, call.ModelID).Scan(&connectionVersion); err != nil {
		t.Fatal(err)
	}
	contextWindow, maxOutput := 32768, 8192
	endpoint := app.ModelGatewayEndpoint{
		Endpoint: app.ModelEndpoint{BaseURL: "https://models.invalid/v1", APIKey: "synthetic-only", ModelID: "test"},
		Version:  connectionVersion, Personal: true, ContextWindowTokens: &contextWindow,
		ContextWindowSource: app.ModelContextWindowLegacyManual, MaxOutputTokens: &maxOutput,
		MaxOutputSource: app.ModelMaxOutputOverride,
	}
	gateway, err := app.NewModelGatewayService(fixture.store, adaptiveBudgetGatewayResolver{endpoint: endpoint}, client)
	if err != nil {
		t.Fatal(err)
	}
	setTaskModelResponseSealer(t, gateway)
	stages := []app.ModelOutputBudgetStageDefinition{
		{Stage: "root_text_only", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "tool_step", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "subtask", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "summary", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "long_form", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "text_continuation", GoalTokens: 3072, MinimumTokens: 64},
	}
	if err := gateway.SetModelOutputBudgetStrategy(app.ModelOutputBudgetStrategyDefinition{
		Version: "pg-crash-boundary-v1",
		Models:  []app.ModelOutputBudgetModelDefinition{{BaseURL: endpoint.Endpoint.BaseURL, Model: endpoint.Endpoint.ModelID, Stages: stages}},
	}); err != nil {
		t.Fatal(err)
	}
	planner, err := worker.NewCloudDSHModelBudgetPlanner(gateway)
	if err != nil {
		t.Fatal(err)
	}
	providerProjection := []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"adaptive crash boundary"}]}],"tools":[]}`)
	requestDigest := adaptiveBudgetProjectionDigest(t, providerProjection)
	dshCallID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.PrepareModelBudget(ctx, worker.CloudDSHModelBudgetPlanRequest{
		Fence: app.AgentInstanceFence{
			TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID,
			AgentInstanceID: fence.AgentInstanceID, AgentGeneration: fence.AgentGeneration, AgentSessionID: fence.AgentSessionID,
			LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion, AuthorizationVersion: fence.AuthorizationVersion,
			EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion,
		},
		TaskRevision: fence.TaskRevision, SessionID: fence.AgentSessionID, ModelID: call.ModelID,
		ThroughSeq: fence.ThroughSequence, Turn: 1, Step: 0, CallID: "budget-call-crash-boundary",
		DSHCallID: dshCallID, RequestDigest: requestDigest, Request: providerProjection,
		Config: json.RawMessage(`{"provider":"morde","model":"p1234567"}`),
	})
	if err != nil {
		t.Fatalf("prepare through DSH budget planner and PostgreSQL: %v", err)
	}
	if plan.PlanID == "" || plan.MaxTokens != 3072 {
		t.Fatalf("prepared output plan = %#v, want persisted plan with 3072 tokens", plan)
	}
	var finalRequest map[string]any
	if err := json.Unmarshal(providerProjection, &finalRequest); err != nil {
		t.Fatal(err)
	}
	finalRequest["max_tokens"] = plan.MaxTokens
	finalProjection, err := json.Marshal(finalRequest)
	if err != nil {
		t.Fatal(err)
	}
	call.Source = app.ModelCallTask
	call.Task = &fence
	call.CallID = dshCallID
	call.LedgerCallID = dshCallID
	call.RawRequest = finalProjection
	call.BudgetPlanID, call.BudgetCallID, call.BudgetRequestDigest = plan.PlanID, "budget-call-crash-boundary", requestDigest
	return fixture, gateway, client, call, plan
}

func TestPostgresAdaptiveBudgetPreparedCommitHasNoDispatchBeforeGatewayCall(t *testing.T) {
	client := &adaptiveBudgetCrashBoundaryClient{}
	fixture, _, client, call, plan := newAdaptiveBudgetCrashBoundaryGateway(t, client)
	ctx := context.Background()
	var state string
	var consumed bool
	if err := fixture.store.pool.QueryRow(ctx, `SELECT ledger.state,preparation.consumed_at IS NOT NULL
FROM model_call_ledger ledger JOIN model_budget_preparations preparation
 ON preparation.owner_id=ledger.owner_id AND preparation.source=ledger.source AND preparation.call_id=ledger.call_id
WHERE ledger.owner_id=$1 AND ledger.source='task' AND ledger.call_id=$2`, fixture.ownerID, call.CallID).
		Scan(&state, &consumed); err != nil {
		t.Fatal(err)
	}
	if state != "prepared" || consumed || client.calls.Load() != 0 || plan.PlanID == "" {
		t.Fatalf("committed pre-dispatch state = %q consumed=%t provider_calls=%d plan=%q; want prepared, unconsumed, and no provider call",
			state, consumed, client.calls.Load(), plan.PlanID)
	}
	var providerModel, endpoint, capabilityVersion string
	var connectionVersion time.Time
	var sendTokens, inputReserved int64
	if err := fixture.store.pool.QueryRow(ctx, `SELECT provider_model,endpoint,connection_version,capability_version,send_tokens,input_reserved_tokens
FROM model_budget_preparations WHERE owner_id=$1 AND source='task' AND call_id=$2`, fixture.ownerID, call.CallID).
		Scan(&providerModel, &endpoint, &connectionVersion, &capabilityVersion, &sendTokens, &inputReserved); err != nil {
		t.Fatal(err)
	}
	requestDigest, err := app.ModelBudgetFinalRequestDigest(call.BudgetRequestDigest, sendTokens)
	if err != nil {
		t.Fatal(err)
	}
	reservation := app.ModelCallReservation{
		Source: app.ModelCallTask, Task: call.Task, OwnerID: fixture.ownerID, PersonalModel: true,
		ModelID: call.ModelID, ProviderEndpoint: endpoint, ProviderModel: providerModel, ConnectionVersion: connectionVersion,
		CallID: call.LedgerCallID, DSHCallID: call.CallID, BudgetPlanID: call.BudgetPlanID, BudgetCallID: call.BudgetCallID,
		BudgetRequestDigest: call.BudgetRequestDigest, BudgetCapabilityVersion: capabilityVersion,
		RequestDigest: requestDigest, ReservedTokens: inputReserved + sendTokens,
	}
	if _, err := fixture.store.pool.Exec(ctx, `CREATE FUNCTION test_adaptive_budget_reject_dispatch() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
 IF OLD.state='prepared' AND NEW.state='dispatched' THEN RAISE EXCEPTION 'injected dispatch transaction failure'; END IF;
 RETURN NEW;
END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.pool.Exec(ctx, `CREATE TRIGGER test_adaptive_budget_reject_dispatch
BEFORE UPDATE OF state ON model_call_ledger FOR EACH ROW
EXECUTE FUNCTION test_adaptive_budget_reject_dispatch()`); err != nil {
		_, _ = fixture.store.pool.Exec(ctx, `DROP FUNCTION IF EXISTS test_adaptive_budget_reject_dispatch()`)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fixture.store.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_adaptive_budget_reject_dispatch ON model_call_ledger`)
		_, _ = fixture.store.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_adaptive_budget_reject_dispatch()`)
	})
	if err := fixture.store.BeginModelCallDispatch(ctx, reservation); err == nil {
		t.Fatal("injected PostgreSQL dispatch update unexpectedly committed")
	}
	if _, err := fixture.store.pool.Exec(ctx, `DROP TRIGGER test_adaptive_budget_reject_dispatch ON model_call_ledger`); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.pool.Exec(ctx, `DROP FUNCTION test_adaptive_budget_reject_dispatch()`); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.pool.QueryRow(ctx, `SELECT ledger.state,preparation.consumed_at IS NOT NULL
FROM model_call_ledger ledger JOIN model_budget_preparations preparation
 ON preparation.owner_id=ledger.owner_id AND preparation.source=ledger.source AND preparation.call_id=ledger.call_id
WHERE ledger.owner_id=$1 AND ledger.source='task' AND ledger.call_id=$2`, fixture.ownerID, call.CallID).
		Scan(&state, &consumed); err != nil || state != "prepared" || consumed || client.calls.Load() != 0 {
		t.Fatalf("dispatch failure left partial state = %q consumed=%t provider_calls=%d err=%v; want persisted prepared hold and no call",
			state, consumed, client.calls.Load(), err)
	}
}

func TestPostgresAdaptiveBudgetSettledReplayDoesNotDispatchTwice(t *testing.T) {
	client := &adaptiveBudgetCrashBoundaryClient{response: app.ModelResponse{
		Text: "Synthetic settled response.", StopReason: "stop",
		Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 6},
	}}
	fixture, gateway, client, call, _ := newAdaptiveBudgetCrashBoundaryGateway(t, client)
	ctx := context.Background()
	response, err := gateway.Complete(ctx, call)
	if err != nil {
		t.Fatalf("dispatch and settle through gateway/PostgreSQL: %v", err)
	}
	if response.Text != "Synthetic settled response." || client.calls.Load() != 1 {
		t.Fatalf("settled response/calls = %q/%d, want one synthetic provider dispatch", response.Text, client.calls.Load())
	}
	var state string
	var consumed bool
	if err := fixture.store.pool.QueryRow(ctx, `SELECT ledger.state,preparation.consumed_at IS NOT NULL
FROM model_call_ledger ledger JOIN model_budget_preparations preparation
 ON preparation.owner_id=ledger.owner_id AND preparation.source=ledger.source AND preparation.call_id=ledger.call_id
WHERE ledger.owner_id=$1 AND ledger.source='task' AND ledger.call_id=$2`, fixture.ownerID, call.CallID).
		Scan(&state, &consumed); err != nil || state != "settled" || !consumed {
		t.Fatalf("settled ledger/preparation = %q consumed=%t, %v", state, consumed, err)
	}
	replayed, err := gateway.Complete(ctx, call)
	if err != nil || replayed.Text != response.Text || client.calls.Load() != 1 {
		t.Fatalf("settled replay = %q err=%v provider_calls=%d; want stored response without redispatch", replayed.Text, err, client.calls.Load())
	}
}

func TestPostgresAdaptiveBudgetAmbiguousTransportFailureIsUnknownAndCannotReplay(t *testing.T) {
	client := &adaptiveBudgetCrashBoundaryClient{err: &app.ModelClientError{Kind: app.ModelErrorTransport, Stage: "transport"}}
	fixture, gateway, client, call, _ := newAdaptiveBudgetCrashBoundaryGateway(t, client)
	ctx := context.Background()
	if _, err := gateway.Complete(ctx, call); err == nil {
		t.Fatal("ambiguous synthetic transport failure was reported as success")
	}
	var state string
	var consumed bool
	if err := fixture.store.pool.QueryRow(ctx, `SELECT ledger.state,preparation.consumed_at IS NOT NULL
FROM model_call_ledger ledger JOIN model_budget_preparations preparation
 ON preparation.owner_id=ledger.owner_id AND preparation.source=ledger.source AND preparation.call_id=ledger.call_id
WHERE ledger.owner_id=$1 AND ledger.source='task' AND ledger.call_id=$2`, fixture.ownerID, call.CallID).
		Scan(&state, &consumed); err != nil || state != "unknown" || !consumed || client.calls.Load() != 1 {
		t.Fatalf("ambiguous dispatch state = %q consumed=%t provider_calls=%d err=%v; want unknown after one dispatch",
			state, consumed, client.calls.Load(), err)
	}
	if _, err := gateway.Complete(ctx, call); err == nil || client.calls.Load() != 1 {
		t.Fatalf("unknown dispatch replay err=%v provider_calls=%d; want rejected without a second dispatch", err, client.calls.Load())
	}
	if err := fixture.store.pool.QueryRow(ctx, `SELECT state FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		fixture.ownerID, call.CallID).Scan(&state); err != nil || state != "unknown" {
		t.Fatalf("unknown ledger after rejected replay = %q, %v", state, err)
	}
}
