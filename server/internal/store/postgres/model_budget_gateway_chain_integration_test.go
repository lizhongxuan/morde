package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/worker"
)

type adaptiveBudgetGatewayResolver struct {
	endpoint app.ModelGatewayEndpoint
}

func (resolver adaptiveBudgetGatewayResolver) ResolveGatewayEndpoint(context.Context, string, string, bool, bool) (app.ModelGatewayEndpoint, error) {
	return resolver.endpoint, nil
}

func (resolver adaptiveBudgetGatewayResolver) ResolveProbeEndpoint(context.Context, identity.Principal, string) (app.ModelGatewayEndpoint, error) {
	return resolver.endpoint, nil
}

func (adaptiveBudgetGatewayResolver) SaveProbeCapabilities(_ context.Context, _ identity.Principal, _ string, _ time.Time,
	capabilities app.ModelCapabilities) (app.ModelCapabilities, error) {
	return capabilities, nil
}

type adaptiveBudgetGatewayClient struct {
	calls   int
	request app.ModelRequest
}

func (client *adaptiveBudgetGatewayClient) Complete(_ context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	client.calls++
	client.request = request
	return app.ModelResponse{Text: "Adaptive budget PostgreSQL path verified.", StopReason: "stop",
		Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 6}}, nil
}

func (*adaptiveBudgetGatewayClient) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{}, errors.New("unexpected model capability check")
}

func TestPostgresAdaptiveBudgetPlannerGatewayAndLedgerUseOneOutputCap(t *testing.T) {
	fixture, _, _, fence, call := revisionGatewayFixture(t)
	ctx := context.Background()
	store := fixture.store
	fence.AgentRootSessionID = fence.AgentSessionID

	if _, err := store.pool.Exec(ctx, `UPDATE model_connections SET base_url='https://models.invalid/v1',
context_window_tokens=32768,context_window_override_tokens=32768,context_window_source='legacy_manual',
context_window_resolved_at=NULL,context_window_catalog_version=NULL,max_output_tokens=8192,
max_output_override_tokens=8192,max_output_source='override',max_output_resolved_at=NULL,
max_output_catalog_version=NULL,max_output_source_url=NULL,updated_at=clock_timestamp() WHERE id=$1`, call.ModelID); err != nil {
		t.Fatalf("configure exact test route capacity: %v", err)
	}
	var connectionVersion time.Time
	if err := store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id=$1`, call.ModelID).Scan(&connectionVersion); err != nil {
		t.Fatal(err)
	}
	contextWindow, maxOutput := 32768, 8192
	endpoint := app.ModelGatewayEndpoint{Endpoint: app.ModelEndpoint{BaseURL: "https://models.invalid/v1", APIKey: "synthetic-only", ModelID: "test"},
		Version: connectionVersion, Personal: true, ContextWindowTokens: &contextWindow,
		ContextWindowSource: app.ModelContextWindowLegacyManual, MaxOutputTokens: &maxOutput, MaxOutputSource: app.ModelMaxOutputOverride}
	client := &adaptiveBudgetGatewayClient{}
	gateway, err := app.NewModelGatewayService(store, adaptiveBudgetGatewayResolver{endpoint: endpoint}, client)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
	stages := []app.ModelOutputBudgetStageDefinition{
		{Stage: "short_answer", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "tool_step", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "subtask", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "summary", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "long_form", GoalTokens: 3072, MinimumTokens: 64},
		{Stage: "text_continuation", GoalTokens: 3072, MinimumTokens: 64},
	}
	if err := gateway.SetModelOutputBudgetStrategy(app.ModelOutputBudgetStrategyDefinition{Version: "pg-integration-v1",
		Models: []app.ModelOutputBudgetModelDefinition{{BaseURL: endpoint.Endpoint.BaseURL, Model: endpoint.Endpoint.ModelID, Stages: stages}}}); err != nil {
		t.Fatal(err)
	}

	providerProjection := []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"adaptive budget integration"}]}],"tools":[]}`)
	requestDigest := adaptiveBudgetProjectionDigest(t, providerProjection)
	planner, err := worker.NewCloudDSHModelBudgetPlanner(gateway)
	if err != nil {
		t.Fatal(err)
	}
	dshCallID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.PrepareModelBudget(ctx, worker.CloudDSHModelBudgetPlanRequest{
		Fence: app.AgentInstanceFence{TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID,
			AgentInstanceID: fence.AgentInstanceID, AgentGeneration: fence.AgentGeneration, AgentSessionID: fence.AgentSessionID,
			LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion, AuthorizationVersion: fence.AuthorizationVersion,
			EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion},
		TaskRevision: fence.TaskRevision, SessionID: fence.AgentSessionID, ModelID: call.ModelID,
		ThroughSeq: fence.ThroughSequence, Turn: 1, Step: 0, CallID: "budget-call-gateway-0001", DSHCallID: dshCallID,
		RequestDigest: requestDigest, Request: providerProjection,
	})
	if err != nil {
		t.Fatalf("prepare through the Worker planner adapter and PostgreSQL ledger: %v", err)
	}
	if plan.PlanID == "" || plan.MaxTokens != 3072 {
		t.Fatalf("prepared output plan = %#v, want a persisted plan with 3072 tokens", plan)
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
	call.CallID, err = app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	call.LedgerCallID = dshCallID
	call.RawRequest = finalProjection
	call.BudgetPlanID, call.BudgetCallID, call.BudgetRequestDigest = plan.PlanID, "budget-call-gateway-0001", requestDigest
	response, err := gateway.Complete(ctx, call)
	if err != nil {
		t.Fatalf("send and settle the plan through ModelGateway/PostgreSQL: %v", err)
	}
	if response.Text != "Adaptive budget PostgreSQL path verified." || client.calls != 1 || client.request.MaxOutputTokens != plan.MaxTokens {
		t.Fatalf("gateway response/cap/calls = %q/%d/%d, want one 3072-token dispatch", response.Text, client.request.MaxOutputTokens, client.calls)
	}
	var ledgerState string
	var reservedTokens, outputTokens, inputTokens int64
	if err := store.pool.QueryRow(ctx, `SELECT ledger.state,ledger.reserved_tokens,preparation.send_tokens,preparation.input_reserved_tokens
FROM model_call_ledger ledger JOIN model_budget_preparations preparation
 ON preparation.owner_id=ledger.owner_id AND preparation.source=ledger.source AND preparation.call_id=ledger.call_id
WHERE ledger.owner_id=$1 AND ledger.source='task' AND ledger.call_id=$2`, fence.InitiatorID, call.CallID).
		Scan(&ledgerState, &reservedTokens, &outputTokens, &inputTokens); err != nil {
		t.Fatal(err)
	}
	if ledgerState != "settled" || outputTokens != int64(plan.MaxTokens) || reservedTokens != inputTokens+outputTokens {
		t.Fatalf("settled reservation state/tokens = %s/%d (input=%d output=%d)", ledgerState, reservedTokens, inputTokens, outputTokens)
	}
	replayed, err := gateway.Complete(ctx, call)
	if err != nil || replayed.Text != response.Text || client.calls != 1 {
		t.Fatalf("settled response replay = %q, err=%v, provider calls=%d; want same result without redispatch", replayed.Text, err, client.calls)
	}
}

func adaptiveBudgetProjectionDigest(t *testing.T, raw []byte) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	projection := make(map[string]any, 4)
	for _, key := range []string{"provider", "model", "messages", "tools"} {
		var value any
		if err := json.Unmarshal(fields[key], &value); err != nil {
			t.Fatalf("decode provider projection field %q: %v", key, err)
		}
		projection[key] = value
	}
	canonical, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}
