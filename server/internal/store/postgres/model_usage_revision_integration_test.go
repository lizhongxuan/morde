package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type revisionModelResolverTest struct{ version time.Time }

func (r revisionModelResolverTest) ResolveGatewayEndpoint(_ context.Context, _ string, _ string, needsTools, _ bool) (app.ModelGatewayEndpoint, error) {
	if needsTools {
		return app.ModelGatewayEndpoint{}, domain.NewError(domain.CodeUnsupported, "synthetic text model has no tools")
	}
	return app.ModelGatewayEndpoint{Endpoint: app.ModelEndpoint{BaseURL: "https://model.example/v1", APIKey: "synthetic-key", ModelID: "test"},
		Version: r.version, Personal: true}, nil
}

func (r revisionModelResolverTest) ResolveProbeEndpoint(ctx context.Context, principal identity.Principal, modelID string) (app.ModelGatewayEndpoint, error) {
	return r.ResolveGatewayEndpoint(ctx, principal.UserID, modelID, false, false)
}

func (revisionModelResolverTest) SaveProbeCapabilities(_ context.Context, _ identity.Principal, _ string, _ time.Time, value app.ModelCapabilities) (app.ModelCapabilities, error) {
	return value, nil
}

type revisionProviderTest struct {
	started chan struct{}
	release chan struct{}
	aborted chan struct{}
	calls   atomic.Int32
}

func setTaskModelResponseSealer(t *testing.T, gateway *app.ModelGatewayService) {
	t.Helper()
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := gateway.SetModelResponseSealer(sealer); err != nil {
		t.Fatal(err)
	}
}

func (p *revisionProviderTest) Complete(ctx context.Context, _ app.ModelRequest) (app.ModelResponse, error) {
	p.calls.Add(1)
	close(p.started)
	select {
	case <-ctx.Done():
		if p.aborted != nil {
			close(p.aborted)
		}
		return app.ModelResponse{}, ctx.Err()
	case <-p.release:
		return app.ModelResponse{Text: "old result", StopReason: "stop",
			Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}, nil
	}
}

func (*revisionProviderTest) Check(context.Context, app.ModelEndpoint) (app.ModelCapabilities, error) {
	return app.ModelCapabilities{}, errors.New("unexpected model check")
}

func revisionGatewayFixture(t *testing.T) (routeActionsTestFixture, *app.ModelGatewayService, *revisionProviderTest, app.ModelTaskFence, app.ModelCall) {
	t.Helper()
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	initial := f.receive(t, "revision-model-original", "start work", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, initial, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, initial.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, f.ownerID); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "revision-model-worker", time.Minute)
	if err != nil || !found || claimed.ID != taskID {
		t.Fatalf("claim revision model task: found=%t err=%v", found, err)
	}
	digest := sha256.Sum256([]byte("revision model checkpoint"))
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET budget_model_tokens=50000,agent_instance_id='agent-1',agent_generation=1,
agent_session_id='session-1',agent_engine_version='engine',agent_profile_version='profile',checkpoint_through_seq=0,checkpoint_digest=$2
WHERE id=$1`, taskID, digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest)
VALUES ($1,$2,'session-1',0,$3)`, taskID, f.ownerID, digest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_agent_instances
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,engine_version,profile_version,supervisor_id,
 stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state)
VALUES ($1,$2,'agent-1',1,'session-1','engine','profile','revision-supervisor',$3,$4,$5,1,30000,
 clock_timestamp()+interval '30 seconds','active')`, taskID, f.ownerID, make([]byte, 32), claimed.Lease.Owner, claimed.Lease.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_agent_checkpoints(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,
 from_seq,through_seq,digest,ciphertext,engine_version,profile_version,auth_version,lease_version)
 VALUES ($1,$2,'agent-1',1,'session-1','session-1',0,0,$3,decode('01','hex'),'engine','profile',1,$4)`,
		taskID, f.ownerID, digest[:], claimed.Lease.Version); err != nil {
		t.Fatal(err)
	}
	var version time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id='p1234567'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	provider := &revisionProviderTest{started: make(chan struct{}), release: make(chan struct{})}
	gateway, err := app.NewModelGatewayService(f.store, revisionModelResolverTest{version: version}, provider)
	if err != nil {
		t.Fatal(err)
	}
	setTaskModelResponseSealer(t, gateway)
	fence := app.ModelTaskFence{TaskID: taskID, OwnerID: f.ownerID, InitiatorID: f.ownerID,
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version, AgentInstanceID: "agent-1", AgentGeneration: 1,
		AgentSessionID: "session-1", AuthorizationVersion: 1, ThroughSequence: 0, EngineVersion: "engine", ProfileVersion: "profile"}
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	call := app.ModelCall{Source: app.ModelCallTask, Task: &fence, ModelID: "p1234567", CallID: callID,
		RawRequest: []byte(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[],"max_tokens":16}`)}
	return f, gateway, provider, fence, call
}

func acceptRevisionForGateway(t *testing.T, f routeActionsTestFixture, fence app.ModelTaskFence) {
	t.Helper()
	claim := f.receive(t, "revision-model-update", "change the request", app.ConversationRoutingInput{ReferenceTaskID: fence.TaskID})
	snapshot, err := f.store.LoadConversationRouteSnapshot(context.Background(), claim)
	if err != nil || len(snapshot.Context.Candidates) != 1 {
		t.Fatalf("revision task snapshot: %+v err=%v", snapshot, err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(context.Background(), claim, snapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteUpdate, TaskID: fence.TaskID, ExpectedRevision: snapshot.Context.Candidates[0].Revision}); err != nil {
		t.Fatal(err)
	}
	if proven, err := f.store.CheckRevisionSettlement(context.Background(), fence); err != nil || !proven {
		t.Fatalf("revision witness not accepted: proven=%t err=%v", proven, err)
	}
}

func TestTaskModelRevisionCancellationSettlesRealLedgerWithoutOldOutput(t *testing.T) {
	f, gateway, provider, fence, call := revisionGatewayFixture(t)
	lifecycle, stopService := context.WithCancel(context.Background())
	defer stopService()
	explicit, cancelExplicit := context.WithCancel(context.Background())
	defer cancelExplicit()
	taskCtx, cancelTask := context.WithCancelCause(context.Background())
	defer cancelTask(context.Canceled)
	ctx := app.WithModelExplicitCancellation(app.WithModelTaskLifecycle(taskCtx, lifecycle), explicit)
	type outcome struct {
		response app.ModelResponse
		err      error
	}
	done := make(chan outcome, 1)
	go func() { response, err := gateway.Complete(ctx, call); done <- outcome{response, err} }()
	select {
	case <-provider.started:
	case result := <-done:
		t.Fatalf("provider did not start: %v", result.err)
	case <-time.After(10 * time.Second):
		t.Fatal("provider did not start")
	}
	acceptRevisionForGateway(t, f, fence)
	cancelTask(app.ErrTaskRevisionInterrupted)
	select {
	case result := <-done:
		t.Fatalf("proven revision cancelled provider before accounting: %v", result.err)
	case <-time.After(250 * time.Millisecond):
	}
	close(provider.release)
	result := <-done
	if !errors.Is(result.err, context.Canceled) || result.response.Text != "" || provider.calls.Load() != 1 {
		t.Fatalf("old Task received provider result: %#v err=%v calls=%d", result.response, result.err, provider.calls.Load())
	}
	var state string
	var input, output int64
	if err := f.store.pool.QueryRow(context.Background(), `SELECT state,input_tokens,output_tokens FROM model_call_ledger
WHERE owner_id=$1 AND source='task' AND call_id=$2`, f.ownerID, call.CallID).Scan(&state, &input, &output); err != nil ||
		state != "settled" || input != 8 || output != 3 {
		t.Fatalf("revision model usage = %s %d %d err=%v", state, input, output, err)
	}
}

func TestTaskModelRevisionAfterProviderDispatchFencesDelivery(t *testing.T) {
	f, gateway, provider, fence, call := revisionGatewayFixture(t)
	done := make(chan struct {
		response app.ModelResponse
		err      error
	}, 1)
	go func() {
		response, err := gateway.Complete(context.Background(), call)
		done <- struct {
			response app.ModelResponse
			err      error
		}{response, err}
	}()
	select {
	case <-provider.started:
	case result := <-done:
		t.Fatalf("provider did not start: %v", result.err)
	case <-time.After(10 * time.Second):
		t.Fatal("provider did not start")
	}
	acceptRevisionForGateway(t, f, fence)
	close(provider.release)
	result := <-done
	if domain.CodeOf(result.err) != domain.CodeUnauthenticated || result.response.Text != "" {
		t.Fatalf("old result reached a Task after its revision changed: %#v err=%v", result.response, result.err)
	}
	var state string
	if err := f.store.pool.QueryRow(context.Background(), `SELECT state FROM model_call_ledger WHERE owner_id=$1 AND source='task' AND call_id=$2`,
		f.ownerID, call.CallID).Scan(&state); err != nil || state != "settled" {
		t.Fatalf("real usage was not settled before delivery fence: %s err=%v", state, err)
	}
}

// Exercise the actual DSH Node process and IPC, not just a synthetic canceled
// context: its own shutdown must not be mistaken for an explicit model stop.
func TestTaskModelRevisionRealDSHSettlesDispatchedUsage(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	initial := f.receive(t, "revision-gateway-node-original", "Summarize the report", app.ConversationRoutingInput{})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, initial, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	var taskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, initial.Message.ID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, f.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET budget_model_tokens=50000 WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "revision-gateway-node-worker", time.Minute)
	if err != nil || !found || claimed.ID != taskID {
		t.Fatalf("claim initial task: found=%t err=%v", found, err)
	}
	var version time.Time
	if err := f.store.pool.QueryRow(ctx, `SELECT updated_at FROM model_connections WHERE id='p1234567'`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	provider := &revisionProviderTest{started: make(chan struct{}), release: make(chan struct{}), aborted: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(provider.release) })
	gateway, err := app.NewModelGatewayService(f.store, revisionModelResolverTest{version: version}, provider)
	if err != nil {
		t.Fatal(err)
	}
	setTaskModelResponseSealer(t, gateway)
	executor := revisionTestExecutor(t, f.store, gateway)
	service, stopService := context.WithCancel(context.Background())
	defer stopService()
	firstDone := make(chan error, 1)
	go func() { firstDone <- executor.ExecuteTask(app.WithModelTaskLifecycle(service, service), claimed) }()
	select {
	case <-provider.started:
	case err := <-firstDone:
		t.Fatalf("real DSH exited before provider dispatch: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("real DSH did not dispatch a model call")
	}

	fence := app.ModelTaskFence{TaskID: taskID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version}
	if err := f.store.pool.QueryRow(ctx, `SELECT agent_instance_id,agent_generation,agent_session_id,auth_version,agent_engine_version,agent_profile_version
FROM tasks WHERE id=$1`, taskID).Scan(&fence.AgentInstanceID, &fence.AgentGeneration, &fence.AgentSessionID,
		&fence.AuthorizationVersion, &fence.EngineVersion, &fence.ProfileVersion); err != nil {
		t.Fatal(err)
	}
	fence.AgentRootSessionID = fence.AgentSessionID
	acceptRevisionForGateway(t, f, fence)
	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("interrupted DSH unexpectedly completed")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("real DSH did not stop after the revision")
	}
	providerCanceled := false
	select {
	case <-provider.aborted:
		providerCanceled = true
	case <-time.After(250 * time.Millisecond):
		release.Do(func() { close(provider.release) })
	}
	deadline := time.Now().Add(10 * time.Second)
	var state, result string
	var input, output int64
	for {
		err := f.store.pool.QueryRow(ctx, `SELECT state,COALESCE(input_tokens,0),COALESCE(output_tokens,0)
FROM model_call_ledger WHERE task_id=$1 AND source='task'`, taskID).Scan(&state, &input, &output)
		if err != nil {
			t.Fatal(err)
		}
		if state != "dispatched" || time.Now().After(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT result FROM tasks WHERE id=$1`, taskID).Scan(&result); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.aborted:
		providerCanceled = true
	default:
	}
	if providerCanceled || state != "settled" || input != 8 || output != 3 || provider.calls.Load() != 1 || strings.Contains(result, "old result") {
		t.Fatalf("real DSH usage/result: provider_canceled=%t state=%s input=%d output=%d calls=%d old_result_visible=%t",
			providerCanceled, state, input, output, provider.calls.Load(), strings.Contains(result, "old result"))
	}
}
