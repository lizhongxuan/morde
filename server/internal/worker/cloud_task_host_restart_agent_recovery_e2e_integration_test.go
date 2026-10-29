package worker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/storage/workspacefs"
)

// hostGenerationFenceTestRouter must never be used by this test: stale
// operation reconciliation should reject the old Host fence before resolving
// an endpoint. This test changes only the persisted generation fence and does
// not claim to start, stop, or restart a real Host runtime/container.
type hostGenerationFenceTestRouter struct {
	endpointLookups atomic.Int32
}

func (r *hostGenerationFenceTestRouter) For(string) (app.SandboxClient, error) {
	r.endpointLookups.Add(1)
	return nil, errors.New("unexpected runtime endpoint lookup after Host generation change")
}

func (*hostGenerationFenceTestRouter) Profiles() []string { return []string{"f01-local"} }

func TestCloudTaskHostGenerationFencePreservesAgentChildCheckpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(bytes32(0x41))
	if err != nil {
		t.Fatal(err)
	}
	hostValue, _, _ := createF01Host(t, ctx, store, sealer, owner.ID)
	const oldRuntimeInstanceID = "f01-runtime-generation-one"
	if err := store.SetRuntimeInstanceID(ctx, hostValue.ID, hostValue.RuntimeGeneration, hostValue.ControlVersion, "", oldRuntimeInstanceID); err != nil {
		t.Fatalf("set original persisted Host runtime identity: %v", err)
	}

	auditService, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, auditService, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	sessionCredential := createF01Session(t, ctx, store, owner.ID)
	principal, err := credentials.VerifySession(ctx, sessionCredential)
	if err != nil || principal.UserID != owner.ID || principal.Kind != identity.KindSession {
		t.Fatalf("verify task session: principal=%#v err=%v", principal, err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		HostID:  hostValue.ID,
		ModelID: "p1234567", Objective: "Keep the child job checkpoint while Host generation changes.",
		Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}, IdempotencyKey: "f01-host-generation-agent-recovery",
	})
	if err != nil {
		t.Fatalf("submit cloud task: %v", err)
	}
	firstTask, found, err := store.ClaimNextTask(ctx, "f01-host-generation-worker-one", time.Minute)
	if err != nil || !found || firstTask.ID != submitted.Task.ID {
		t.Fatalf("claim task: found=%t task=%q err=%v", found, firstTask.ID, err)
	}

	stopKey := bytes32(0x52)
	authenticator, err := app.NewAgentStopAuthenticator("f01-host-generation-supervisor", stopKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	startAgent := func(current task.Task, instanceID string, nonceByte byte) app.AgentInstanceLease {
		t.Helper()
		nonce := bytes32(nonceByte)
		lease, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
			TaskID: current.ID, OwnerID: owner.ID, AgentInstanceID: instanceID,
			AgentSessionID: "session_host_generation_recovery", SupervisorID: "f01-host-generation-supervisor",
			LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
			AuthorizationVersion: current.Authorization.Version, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1",
		}, SupervisorNonce: nonce, LeaseDuration: app.MaxAgentLease})
		if err != nil || replayed {
			t.Fatalf("start Agent %s: lease=%#v replayed=%t err=%v", instanceID, lease, replayed, err)
		}
		return lease
	}
	firstAgent := startAgent(firstTask, "agent_host_generation_one", 0x61)
	childSessions, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	child, replayed, err := childSessions.Create(ctx, app.AgentChildSessionCommand{
		Fence: firstAgent.Fence, ParentSessionID: firstAgent.Fence.AgentSessionID,
		SpawnKey: "f01-host-generation-child-spawn", Arguments: []byte(`{"objective":"inspect one file"}`),
		Title: "Inspect one file", Summary: "Persisted child job", AllowedTools: []string{"file_read"}, Background: true,
	})
	if err != nil || replayed || !child.Background || child.SpawnKey != "f01-host-generation-child-spawn" {
		t.Fatalf("create persisted child job: child=%#v replayed=%t err=%v", child, replayed, err)
	}
	childPayload := []byte(fmt.Sprintf(`{"header":{"id":%q},"from_seq":0,"events":[{"seq":0,"text":"checkpoint survives Host generation change"}]}`, child.SessionID))
	childDigest := sha256.Sum256(childPayload)
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{
		Fence: app.AgentCheckpointFence{
			TaskID: firstAgent.Fence.TaskID, OwnerID: firstAgent.Fence.OwnerID,
			AgentInstanceID: firstAgent.Fence.AgentInstanceID, AgentGeneration: firstAgent.Fence.AgentGeneration,
			AgentSessionID: child.SessionID, AgentRootSessionID: firstAgent.Fence.AgentSessionID,
			AuthorizationVersion: firstAgent.Fence.AuthorizationVersion, LeaseOwner: firstAgent.Fence.LeaseOwner,
			LeaseVersion: firstAgent.Fence.LeaseVersion, EngineVersion: firstAgent.Fence.EngineVersion,
			ProfileVersion: firstAgent.Fence.ProfileVersion,
		}, From: 0, Through: 0, Digest: childDigest[:], Payload: childPayload,
	}); err != nil {
		t.Fatalf("save child checkpoint: %v", err)
	}

	// Persist an old-generation Host operation through the real operation
	// ledger state transitions. The result represents a terminal response from
	// the old runtime; no runtime endpoint is launched in this fence-only test.
	callID := "f01-host-generation-old-operation"
	arguments, err := json.Marshal(map[string]string{"url": "https://example.test/old-generation"})
	if err != nil {
		t.Fatal(err)
	}
	operationHashPayload, err := json.Marshal(struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{Name: "browser_open_tab", Arguments: arguments})
	if err != nil {
		t.Fatal(err)
	}
	operationHash := sha256.Sum256(operationHashPayload)
	operationID, err := domain.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	operationKey := f01OperationKey(callID)
	operation, replayed, err := store.CreateOperation(ctx, task.Operation{
		ID: operationID, TaskID: firstTask.ID, Key: operationKey, RequestHash: operationHash[:],
		HostID:            hostValue.ID,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: oldRuntimeInstanceID,
		ControlVersion: hostValue.ControlVersion, LeaseVersion: firstTask.Lease.Version,
		RuntimeLeaseVersion: firstTask.Lease.Version, Status: task.OperationPrepared,
	}, firstTask.Lease.Owner)
	if err != nil || replayed || operation.Status != task.OperationPrepared {
		t.Fatalf("create old-generation operation: operation=%#v replayed=%t err=%v", operation, replayed, err)
	}
	operation, err = store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: firstTask.ID, OperationKey: operationKey, LeaseOwner: firstTask.Lease.Owner, LeaseVersion: firstTask.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: oldRuntimeInstanceID,
		ControlVersion: hostValue.ControlVersion, RuntimeLeaseVersion: firstTask.Lease.Version, Status: task.OperationDispatched,
	})
	if err != nil || operation.Status != task.OperationDispatched {
		t.Fatalf("dispatch old-generation operation in ledger: operation=%#v err=%v", operation, err)
	}
	operation, err = store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: firstTask.ID, OperationKey: operationKey, LeaseOwner: firstTask.Lease.Owner, LeaseVersion: firstTask.Lease.Version,
		Status: task.OperationSucceeded, ExternalID: "f01-host-runtime-op-generation-one",
		Result: `{"target_id":"tab-generation-one"}`,
	})
	if err != nil || operation.Status != task.OperationSucceeded || operation.RuntimeGeneration != 1 {
		t.Fatalf("settle old-generation operation: operation=%#v err=%v", operation, err)
	}
	unknownCallID := callID + "-unknown"
	unknownKey := f01OperationKey(unknownCallID)
	unknownID, err := domain.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	unknownOperation, replayed, err := store.CreateOperation(ctx, task.Operation{
		ID: unknownID, TaskID: firstTask.ID, Key: unknownKey, RequestHash: operationHash[:], HostID: hostValue.ID,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: oldRuntimeInstanceID,
		ControlVersion: hostValue.ControlVersion, LeaseVersion: firstTask.Lease.Version,
		RuntimeLeaseVersion: firstTask.Lease.Version, Status: task.OperationPrepared,
	}, firstTask.Lease.Owner)
	if err != nil || replayed || unknownOperation.Status != task.OperationPrepared {
		t.Fatalf("create uncertain old-generation operation: operation=%#v replayed=%t err=%v", unknownOperation, replayed, err)
	}
	unknownOperation, err = store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: firstTask.ID, OperationKey: unknownKey, LeaseOwner: firstTask.Lease.Owner,
		LeaseVersion: firstTask.Lease.Version, RuntimeGeneration: hostValue.RuntimeGeneration,
		RuntimeInstanceID: oldRuntimeInstanceID, ControlVersion: hostValue.ControlVersion,
		RuntimeLeaseVersion: firstTask.Lease.Version, Status: task.OperationDispatched,
	})
	if err != nil || unknownOperation.Status != task.OperationDispatched {
		t.Fatalf("dispatch uncertain old-generation operation: operation=%#v err=%v", unknownOperation, err)
	}
	unknownOperation, err = store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: firstTask.ID, OperationKey: unknownKey, LeaseOwner: firstTask.Lease.Owner,
		LeaseVersion: firstTask.Lease.Version, Status: task.OperationUnknown,
	})
	if err != nil || unknownOperation.Status != task.OperationUnknown {
		t.Fatalf("mark uncertain old-generation operation unknown: operation=%#v err=%v", unknownOperation, err)
	}

	stopAgent := func(lease app.AgentInstanceLease, nonceByte byte) {
		t.Helper()
		receipt := app.AgentStopReceipt{Fence: lease.Fence, SupervisorNonce: bytes32(nonceByte), TreeStopped: true}
		payload, err := app.AgentStopReceiptSigningPayload(receipt)
		if err != nil {
			t.Fatal(err)
		}
		mac := hmac.New(sha256.New, stopKey)
		_, _ = mac.Write(payload)
		receipt.Signature = mac.Sum(nil)
		if _, err := instances.ConfirmStopped(ctx, receipt); err != nil {
			t.Fatalf("confirm Agent %d stopped: %v", lease.Fence.AgentGeneration, err)
		}
	}
	stopAgent(firstAgent, 0x61)

	// Simulate only the persisted Host generation fence changing. Real Host
	// release/recreate behavior and runtime provisioning are outside this local
	// regression's claim and remain separate acceptance coverage.
	const newRuntimeInstanceID = "f01-runtime-generation-two"
	result, err := pool.Exec(ctx, `UPDATE hosts SET runtime_generation=runtime_generation+1,
runtime_instance_id=$2, updated_at=clock_timestamp() WHERE id=$1 AND runtime_generation=$3`,
		hostValue.ID, newRuntimeInstanceID, hostValue.RuntimeGeneration)
	if err != nil || result.RowsAffected() != 1 {
		t.Fatalf("advance simulated Host generation fence: rows=%d err=%v", result.RowsAffected(), err)
	}
	blocks, err := workspacefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close Workspace blocks: %v", err)
		}
	}()
	syncService, err := app.NewWorkspaceSyncService(store, blocks, 0, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	router := &hostGenerationFenceTestRouter{}
	runtimeControl, err := app.NewRuntimeControl(store, router, sealer)
	if err != nil {
		t.Fatal(err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, runtimeControl, runtimeControl, syncService)
	if err != nil {
		t.Fatal(err)
	}
	currentDetails, err := store.GetTask(ctx, firstTask.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	unknownReconciliation, err := runtimeTools.ReconcileOperation(ctx, app.AgentToolRequest{
		Task:      currentDetails.Task,
		Call:      app.ModelToolCall{CallID: unknownCallID, Name: "browser_open_tab", Arguments: string(arguments)},
		Arguments: arguments, OperationKey: unknownKey, OperationID: unknownOperation.ID,
		RequiredScope: identity.ScopeComputerOperate,
	}, unknownOperation)
	if err != nil || unknownReconciliation.Status != app.AgentToolUnknown {
		t.Fatalf("unknown old-generation operation was accepted after Host generation change: result=%#v err=%v", unknownReconciliation, err)
	}
	if router.endpointLookups.Load() != 0 {
		t.Fatalf("unknown stale Host operation tried to resolve a runtime endpoint %d times", router.endpointLookups.Load())
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1 AND lease_owner=$2 AND lease_version=$3`,
		firstTask.ID, firstTask.Lease.Owner, firstTask.Lease.Version); err != nil {
		t.Fatalf("expire old task lease for Agent recovery: %v", err)
	}
	secondTask, found, err := store.ClaimNextTask(ctx, "f01-host-generation-worker-two", time.Minute)
	if err != nil || !found || secondTask.ID != firstTask.ID || secondTask.Lease.Version != firstTask.Lease.Version+1 {
		t.Fatalf("recover task after Host generation fence change: task=%#v found=%t err=%v", secondTask, found, err)
	}
	secondAgent := startAgent(secondTask, "agent_host_generation_two", 0x62)
	if secondAgent.Fence.AgentGeneration != firstAgent.Fence.AgentGeneration+1 ||
		secondAgent.Fence.AgentSessionID != firstAgent.Fence.AgentSessionID {
		t.Fatalf("Agent recovery fence changed root identity: first=%#v second=%#v", firstAgent.Fence, secondAgent.Fence)
	}

	recoveredChild, err := childSessions.Get(ctx, secondAgent.Fence, secondAgent.Fence.AgentSessionID, child.SessionID)
	if err != nil || recoveredChild.SessionID != child.SessionID || recoveredChild.SpawnKey != child.SpawnKey ||
		!recoveredChild.Background || recoveredChild.Step.Status != task.StepRunning ||
		recoveredChild.CheckpointThrough != 0 || !bytes.Equal(recoveredChild.CheckpointDigest, childDigest[:]) {
		t.Fatalf("child session after Host generation change: child=%#v err=%v", recoveredChild, err)
	}
	checkpointPage, err := checkpoints.ReadPage(ctx, app.AgentCheckpointPageRequest{
		Fence: secondAgent.Fence, SessionID: child.SessionID, From: 0, Through: 0, ThroughDigest: childDigest[:],
	})
	if err != nil || checkpointPage.From != 0 || checkpointPage.Through != 0 || !bytes.Equal(checkpointPage.Payload, childPayload) {
		t.Fatalf("child checkpoint after Host generation change: page=%#v err=%v", checkpointPage, err)
	}

	currentDetails, err = store.GetTask(ctx, secondTask.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentOperation, err := store.GetOperation(ctx, owner.ID, secondTask.ID, operationKey)
	if err != nil || currentOperation.ID != operation.ID || currentOperation.RuntimeGeneration != hostValue.RuntimeGeneration ||
		currentOperation.Status != task.OperationSucceeded {
		t.Fatalf("old Host operation ledger after generation change: operation=%#v err=%v", currentOperation, err)
	}
	reconciliation, err := runtimeTools.ReconcileOperation(ctx, app.AgentToolRequest{
		Task:      currentDetails.Task,
		Call:      app.ModelToolCall{CallID: callID, Name: "browser_open_tab", Arguments: string(arguments)},
		Arguments: arguments, OperationKey: operationKey, OperationID: currentOperation.ID,
		RequiredScope: identity.ScopeComputerOperate,
	}, currentOperation)
	if err != nil || reconciliation.Status != app.AgentToolUnknown {
		t.Fatalf("old-generation Host operation was accepted after persisted fence change: result=%#v err=%v", reconciliation, err)
	}
	if router.endpointLookups.Load() != 0 {
		t.Fatalf("stale Host operation tried to resolve a runtime endpoint %d times", router.endpointLookups.Load())
	}
	unchangedOperation, err := store.GetOperation(ctx, owner.ID, secondTask.ID, operationKey)
	if err != nil || unchangedOperation.ID != operation.ID || unchangedOperation.RuntimeGeneration != hostValue.RuntimeGeneration ||
		unchangedOperation.RuntimeInstanceID != oldRuntimeInstanceID || unchangedOperation.Status != task.OperationSucceeded {
		t.Fatalf("old operation changed during fenced reconciliation: operation=%#v err=%v", unchangedOperation, err)
	}

	unchangedUnknownOperation, err := store.GetOperation(ctx, owner.ID, secondTask.ID, unknownKey)
	if err != nil || unchangedUnknownOperation.ID != unknownOperation.ID ||
		unchangedUnknownOperation.RuntimeGeneration != hostValue.RuntimeGeneration ||
		unchangedUnknownOperation.RuntimeInstanceID != oldRuntimeInstanceID || unchangedUnknownOperation.Status != task.OperationUnknown {
		t.Fatalf("unknown old-generation operation changed during fenced reconciliation: operation=%#v err=%v", unchangedUnknownOperation, err)
	}
}

func bytes32(value byte) []byte {
	result := make([]byte, 32)
	for index := range result {
		result[index] = value
	}
	return result
}
