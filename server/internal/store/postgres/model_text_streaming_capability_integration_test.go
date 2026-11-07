package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type modelStreamingResolverTest struct{ store *Store }

func (r modelStreamingResolverTest) ResolveGatewayEndpoint(ctx context.Context, ownerID, modelID string, _, _ bool) (app.ModelGatewayEndpoint, error) {
	var model, owner string
	var version time.Time
	var checked bool
	err := r.store.pool.QueryRow(ctx, `SELECT model_name,COALESCE(owner_id,''),updated_at,checked_at IS NOT NULL AND capability_text
FROM model_connections WHERE id=$1`, modelID).Scan(&model, &owner, &version, &checked)
	if err != nil || !checked || owner != "" && owner != ownerID {
		return app.ModelGatewayEndpoint{}, domain.NewError(domain.CodeNotFound, "synthetic model is unavailable")
	}
	return app.ModelGatewayEndpoint{Endpoint: app.ModelEndpoint{BaseURL: "https://model.example/v1", APIKey: "synthetic-key", ModelID: model},
		Version: version, Personal: owner != ""}, nil
}

func (r modelStreamingResolverTest) ResolveProbeEndpoint(ctx context.Context, principal identity.Principal, modelID string) (app.ModelGatewayEndpoint, error) {
	return r.ResolveGatewayEndpoint(ctx, principal.UserID, modelID, false, false)
}

func (modelStreamingResolverTest) SaveProbeCapabilities(_ context.Context, _ identity.Principal, _ string, _ time.Time, value app.ModelCapabilities) (app.ModelCapabilities, error) {
	return value, nil
}

type modelStreamingProviderTest struct {
	calls         int
	fail          bool
	usedStreaming bool
	changeVersion func()
}

func (p *modelStreamingProviderTest) Complete(_ context.Context, request app.ModelRequest) (app.ModelResponse, error) {
	p.calls++
	p.usedStreaming = request.OnTextDelta != nil
	if p.changeVersion != nil {
		p.changeVersion()
	}
	if p.fail {
		return app.ModelResponse{}, errors.New("synthetic provider failure")
	}
	answer := "hello"
	if request.Instructions == "Reply exactly OK using text streaming." {
		answer = "OK"
	}
	if request.OnTextDelta != nil {
		if err := request.OnTextDelta(answer); err != nil {
			return app.ModelResponse{}, err
		}
	}
	return app.ModelResponse{Text: answer, StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 2}}, nil
}

func (*modelStreamingProviderTest) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{}, errors.New("unexpected probe")
}

func modelStreamingTask(f routeActionsTestFixture, fence app.ModelTaskFence) task.Task {
	return task.Task{ID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID, ConversationID: f.conversation.ID,
		ModelID: "p1234567", Lease: task.Lease{Owner: fence.LeaseOwner, Version: fence.LeaseVersion},
		Authorization: f.authorization}
}

func TestModelTextStreamingProbeAndDispatchUseCurrentProof(t *testing.T) {
	f, _, _, fence, call := revisionGatewayFixture(t)
	ctx := context.Background()
	provider := &modelStreamingProviderTest{}
	gateway, err := app.NewModelGatewayService(f.store, modelStreamingResolverTest{store: f.store}, provider)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: f.authorization.TokenID}
	claimed := modelStreamingTask(f, fence)
	if enabled, err := gateway.TaskTextStreamingCapability(ctx, claimed); err != nil || enabled {
		t.Fatalf("unknown capability enabled: enabled=%t err=%v", enabled, err)
	}
	capability, err := gateway.CheckModelCapability(ctx, principal, "p1234567", "synthetic-stream-probe-001", "text_stream")
	if err != nil || !capability.Text || !capability.TextStream || !provider.usedStreaming || provider.calls != 1 {
		t.Fatalf("explicit streaming probe: capability=%+v err=%v calls=%d", capability, err, provider.calls)
	}
	if _, err := gateway.CheckModelCapability(ctx, principal, "p1234567", "synthetic-stream-probe-001", "text_stream"); err != nil || provider.calls != 1 {
		t.Fatalf("verified probe replay repeated billing: err=%v calls=%d", err, provider.calls)
	}
	record, err := f.store.GetModelConnection(ctx, "p1234567")
	if err != nil || !record.Capabilities.TextStream {
		t.Fatalf("verified capability not visible on connection: %t err=%v", record.Capabilities.TextStream, err)
	}
	if enabled, err := gateway.TaskTextStreamingCapability(ctx, claimed); err != nil || !enabled {
		t.Fatalf("verified capability unavailable to task: enabled=%t err=%v", enabled, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=clock_timestamp() WHERE id=$1`, principal.TokenID); err != nil {
		t.Fatal(err)
	}
	if enabled, err := gateway.TaskTextStreamingCapability(ctx, claimed); err == nil || enabled {
		t.Fatalf("revoked task credential retained streaming authorization: enabled=%t err=%v", enabled, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tokens SET revoked_at=NULL WHERE id=$1`, principal.TokenID); err != nil {
		t.Fatal(err)
	}
	var delta string
	call.Request.OnTextDelta = func(value string) error { delta += value; return nil }
	response, err := gateway.Complete(ctx, call)
	if err != nil || response.Text != "hello" || delta != "hello" || !provider.usedStreaming {
		t.Fatalf("verified task did not stream: response=%+v delta=%q err=%v", response, delta, err)
	}
	var ledgerState, taskID, ownerID, initiatorID, modelID string
	var through, revision int64
	err = f.store.pool.QueryRow(ctx, `SELECT l.state,d.task_id,d.task_owner_id,d.initiator_id,d.model_id,d.through_sequence,d.task_revision
FROM model_text_streaming_dispatches d JOIN model_call_ledger l ON l.owner_id=d.owner_id AND l.source=d.source AND l.call_id=d.call_id
WHERE d.owner_id=$1 AND d.source='task' AND d.call_id=$2`, f.ownerID, call.CallID).
		Scan(&ledgerState, &taskID, &ownerID, &initiatorID, &modelID, &through, &revision)
	if err != nil || ledgerState != "settled" || taskID != fence.TaskID || ownerID != fence.OwnerID || initiatorID != fence.InitiatorID ||
		modelID != call.ModelID || through != fence.ThroughSequence || revision != 0 {
		t.Fatalf("stream dispatch evidence incorrect: state=%s task=%s err=%v", ledgerState, taskID, err)
	}
	provider.fail = true
	if _, err := gateway.CheckModelCapability(ctx, principal, "p1234567", "synthetic-stream-probe-002", "text_stream"); err == nil || provider.calls != 3 {
		t.Fatalf("failed check unexpectedly succeeded or retried: err=%v calls=%d", err, provider.calls)
	}
	record, err = f.store.GetModelConnection(ctx, "p1234567")
	if err != nil || record.Capabilities.TextStream || !record.Capabilities.Text || record.CheckedAt == nil {
		t.Fatalf("failed probe damaged text or retained streaming proof: text=%t stream=%t err=%v", record.Capabilities.Text, record.Capabilities.TextStream, err)
	}
}

func TestModelTextStreamingFallsBackWhenConnectionChanges(t *testing.T) {
	f, _, _, fence, call := revisionGatewayFixture(t)
	ctx := context.Background()
	provider := &modelStreamingProviderTest{}
	gateway, err := app.NewModelGatewayService(f.store, modelStreamingResolverTest{store: f.store}, provider)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: f.authorization.TokenID}
	if _, err := gateway.CheckModelCapability(ctx, principal, "p1234567", "synthetic-stream-probe-003", "text_stream"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE model_connections SET updated_at=clock_timestamp() WHERE id='p1234567'`); err != nil {
		t.Fatal(err)
	}
	if enabled, err := gateway.TaskTextStreamingCapability(ctx, modelStreamingTask(f, fence)); err != nil || enabled {
		t.Fatalf("stale proof enabled streaming: enabled=%t err=%v", enabled, err)
	}
	var deltas int
	call.Request.OnTextDelta = func(string) error { deltas++; return nil }
	response, err := gateway.Complete(ctx, call)
	if err != nil || response.Text != "hello" || provider.usedStreaming || deltas != 0 || provider.calls != 2 {
		t.Fatalf("stale proof did not fall back before dispatch: response=%+v streaming=%t deltas=%d calls=%d err=%v",
			response, provider.usedStreaming, deltas, provider.calls, err)
	}
	var count int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM model_text_streaming_dispatches WHERE owner_id=$1 AND call_id=$2`,
		f.ownerID, call.CallID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("non-streaming call created streaming proof: count=%d err=%v", count, err)
	}
	provider.changeVersion = func() {
		if _, err := f.store.pool.Exec(ctx, `UPDATE model_connections SET updated_at=clock_timestamp() WHERE id='p1234567'`); err != nil {
			t.Error(err)
		}
	}
	if _, err := gateway.CheckModelCapability(ctx, principal, "p1234567", "synthetic-stream-probe-004", "text_stream"); err == nil {
		t.Fatal("connection change during streaming probe was accepted")
	}
	record, err := f.store.GetModelConnection(ctx, "p1234567")
	if err != nil || record.Capabilities.TextStream || !record.Capabilities.Text {
		t.Fatalf("stale streaming proof remained visible: text=%t stream=%t err=%v", record.Capabilities.Text, record.Capabilities.TextStream, err)
	}
}
