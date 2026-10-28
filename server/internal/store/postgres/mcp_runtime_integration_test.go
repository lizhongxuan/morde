package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/task"
)

func TestPostgresMCPRuntimeOperationDurabilityAndTakeoverSnapshot(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	ensureMCPTestAPIKey(t, ctx, store, owner.ID, "t1234567")
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	const runtimeInstanceID = "mcp-runtime-test-instance"
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_generation=1, runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, runtimeInstanceID); err != nil {
		t.Fatal(err)
	}
	fence := app.AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: runtimeInstanceID, ControlVersion: 0, AIAllowed: true}
	key := app.MCPRuntimeOperationKey{OwnerID: owner.ID, APIKeyID: "t1234567", SessionID: "sdk-session-1", HostID: hostValue.ID, IdempotencyKey: "tool-call-1"}
	request := newMCPRuntimeBeginRequest(t, key, fence, 1)
	request.Capability, request.Action = "file.read", "read"
	request.Arguments = json.RawMessage(`{"workspace_id":"w1234567","path":"notes.txt"}`)
	request.WorkspaceID, request.BindingVersion = "w1234567", 3

	prepared, replayed, err := store.BeginMCPRuntimeOperation(ctx, request)
	if err != nil || replayed || prepared.Status != task.OperationPrepared || prepared.LeaseVersion != 1 ||
		prepared.WorkspaceID != request.WorkspaceID || prepared.BindingVersion != request.BindingVersion {
		t.Fatalf("begin MCP runtime operation = %#v, replayed=%t, err=%v", prepared, replayed, err)
	}
	replayRequest := request
	replayRequest.NewRuntimeTaskID = mustNewTaskID(t)
	replayRequest.NewOperationID = mustNewOperationID(t)
	replayedOperation, replayed, err := store.BeginMCPRuntimeOperation(ctx, replayRequest)
	if err != nil || !replayed || replayedOperation.OperationID != prepared.OperationID || replayedOperation.RuntimeTaskID != prepared.RuntimeTaskID {
		t.Fatalf("idempotent MCP runtime begin = %#v, replayed=%t, err=%v", replayedOperation, replayed, err)
	}
	changedBinding := replayRequest
	changedBinding.BindingVersion++
	if _, _, err := store.BeginMCPRuntimeOperation(ctx, changedBinding); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("MCP replay with changed Workspace binding version error = %v", err)
	}

	differentHash := request
	differentRequestHash := sha256.Sum256([]byte("different request"))
	differentHash.RequestHash = append([]byte(nil), differentRequestHash[:]...)
	if _, _, err := store.BeginMCPRuntimeOperation(ctx, differentHash); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed MCP request hash error = %v", err)
	}
	secondKey := key
	secondKey.IdempotencyKey = "tool-call-2"
	if _, _, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, secondKey, fence, 1)); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("second active MCP operation error = %v", err)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			marked, err := store.MarkMCPRuntimeOperationDispatched(ctx, key, prepared.RuntimeTaskID, fence, prepared.LeaseVersion)
			if err == nil && marked.Status != task.OperationDispatched {
				err = domain.NewError(domain.CodeInternal, "concurrent MCP dispatch did not return dispatched state")
			}
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("concurrent idempotent MCP dispatch replay: %v", err)
		}
	}
	dispatched, err := store.MarkMCPRuntimeOperationDispatched(ctx, key, prepared.RuntimeTaskID, fence, prepared.LeaseVersion)
	if err != nil || dispatched.Status != task.OperationDispatched {
		t.Fatalf("dispatched MCP runtime operation = %#v, err=%v", dispatched, err)
	}

	activeIDs, err := store.ListActiveMCPRuntimeTaskIDs(ctx, owner.ID, hostValue.ID, fence)
	if err != nil || len(activeIDs) != 1 || activeIDs[0] != prepared.RuntimeTaskID {
		t.Fatalf("active MCP runtime task ids = %#v, err=%v", activeIDs, err)
	}
	wrongInstanceFence := fence
	wrongInstanceFence.RuntimeInstanceID = "different-instance"
	activeIDs, err = store.ListActiveMCPRuntimeTaskIDs(ctx, owner.ID, hostValue.ID, wrongInstanceFence)
	if err != nil || len(activeIDs) != 0 {
		t.Fatalf("active MCP runtime task ids for stale instance = %#v, err=%v", activeIDs, err)
	}

	renewed, err := store.RenewMCPRuntimeOperationLease(ctx, key, prepared.RuntimeTaskID, fence, 1, prepared.LeaseVersion, 2*time.Minute)
	if err != nil || !renewed.LeaseExpiresAt.After(dispatched.LeaseExpiresAt) {
		t.Fatalf("renew MCP runtime operation lease = %#v, err=%v", renewed, err)
	}
	if _, err := store.RenewMCPRuntimeOperationLease(ctx, key, prepared.RuntimeTaskID, fence, 2, prepared.LeaseVersion, time.Minute); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("renew with another execution epoch error = %v", err)
	}

	unknownResult := json.RawMessage(`{"outcome":"needs reconciliation"}`)
	unknown, err := store.SettleMCPRuntimeOperation(ctx, key, prepared.RuntimeTaskID, task.OperationUnknown, unknownResult)
	if err != nil || unknown.Status != task.OperationUnknown {
		t.Fatalf("settle MCP operation unknown = %#v, err=%v", unknown, err)
	}
	activeIDs, err = store.ListActiveMCPRuntimeTaskIDs(ctx, owner.ID, hostValue.ID, fence)
	if err != nil || len(activeIDs) != 1 || activeIDs[0] != prepared.RuntimeTaskID {
		t.Fatalf("unknown MCP operation did not remain active: ids=%#v err=%v", activeIDs, err)
	}
	ownerActive, err := store.ListOwnerActiveMCPRuntimeOperations(ctx, owner.ID)
	if err != nil || len(ownerActive) != 1 || ownerActive[0].OperationID != prepared.OperationID ||
		ownerActive[0].WorkspaceID != request.WorkspaceID || ownerActive[0].BindingVersion != request.BindingVersion {
		t.Fatalf("owner active MCP runtime operations = %#v, err=%v", ownerActive, err)
	}
	if _, _, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, secondKey, fence, 1)); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("new operation while prior result is unknown error = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE mcp_runtime_operations SET cancel_requested_at=clock_timestamp() WHERE operation_id=$1`, prepared.OperationID); err != nil {
		t.Fatal(err)
	}
	pendingCancellation, err := store.ListMCPRuntimeOperationsPendingCancellation(ctx, 100)
	if err != nil || len(pendingCancellation) != 1 || pendingCancellation[0].OperationID != prepared.OperationID {
		t.Fatalf("pending MCP cancellation operations = %#v, err=%v", pendingCancellation, err)
	}

	cancelledResult := mcpRuntimeCancelledReceipt(t, unknown)
	cancelled, err := store.SettleMCPRuntimeOperation(ctx, key, prepared.RuntimeTaskID, task.OperationCancelled, cancelledResult)
	if err != nil || cancelled.Status != task.OperationCancelled {
		t.Fatalf("settle MCP operation cancelled = %#v, err=%v", cancelled, err)
	}
	activeIDs, err = store.ListActiveMCPRuntimeTaskIDs(ctx, owner.ID, hostValue.ID, fence)
	if err != nil || len(activeIDs) != 0 {
		t.Fatalf("cancelled MCP operation remains active: ids=%#v err=%v", activeIDs, err)
	}
	ownerActive, err = store.ListOwnerActiveMCPRuntimeOperations(ctx, owner.ID)
	if err != nil || len(ownerActive) != 0 {
		t.Fatalf("cancelled operation remains in owner active list: %#v, err=%v", ownerActive, err)
	}
	stored, err := store.GetMCPRuntimeOperation(ctx, key)
	if err != nil || stored.Status != task.OperationCancelled || !validMCPRuntimeCancellationReceipt(stored.Result, stored) ||
		stored.WorkspaceID != request.WorkspaceID || stored.BindingVersion != request.BindingVersion {
		t.Fatalf("get settled MCP operation = %#v, err=%v", stored, err)
	}

	newOperation, replayed, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, secondKey, fence, 1))
	if err != nil || replayed || newOperation.RuntimeTaskID != prepared.RuntimeTaskID || newOperation.LeaseVersion != 2 {
		t.Fatalf("begin after cancelled MCP operation = %#v, replayed=%t, err=%v", newOperation, replayed, err)
	}
	if _, err := store.SettleMCPRuntimeOperation(ctx, secondKey, newOperation.RuntimeTaskID, task.OperationFailed, json.RawMessage(`{"error":"expected test terminal"}`)); err != nil {
		t.Fatalf("settle prepared MCP operation failed: %v", err)
	}
}

func TestPostgresMCPRuntimeBeginCancelsExpiredPreparedOperation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	ensureMCPTestAPIKey(t, ctx, store, owner.ID, "t1234567")
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	const runtimeInstanceID = "mcp-expired-prepared-test-instance"
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_generation=1, runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, runtimeInstanceID); err != nil {
		t.Fatal(err)
	}
	fence := app.AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: runtimeInstanceID, ControlVersion: 0, AIAllowed: true}
	firstKey := app.MCPRuntimeOperationKey{OwnerID: owner.ID, APIKeyID: "t1234567", SessionID: "sdk-session-expired-prepared", HostID: hostValue.ID, IdempotencyKey: "first-call"}
	first, _, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, firstKey, fence, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE mcp_runtime_operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE operation_id=$1`, first.OperationID); err != nil {
		t.Fatal(err)
	}
	pending, err := store.ListMCPRuntimeOperationsPendingCancellation(ctx, 32)
	if err != nil || len(pending) != 1 || pending[0].OperationID != first.OperationID || pending[0].Status != task.OperationPrepared {
		t.Fatalf("expired prepared MCP operation recovery queue = %#v, err=%v", pending, err)
	}

	secondKey := firstKey
	secondKey.IdempotencyKey = "second-call"
	second, replayed, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, secondKey, fence, 1))
	if err != nil || replayed || second.Status != task.OperationPrepared || second.LeaseVersion != first.LeaseVersion+1 {
		t.Fatalf("begin after expired prepared MCP operation = %#v, replayed=%t, err=%v", second, replayed, err)
	}
	settled, err := store.GetMCPRuntimeOperation(ctx, firstKey)
	if err != nil || settled.Status != task.OperationCancelled || string(settled.Result) != "null" {
		t.Fatalf("expired prepared MCP operation = %#v, err=%v", settled, err)
	}
}

func TestPostgresMCPRuntimeCancellationUsesExpiredLeaseAndNotFoundEvidence(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	ensureMCPTestAPIKey(t, ctx, store, owner.ID, "t1234567")
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	const runtimeInstanceID = "mcp-not-found-test-instance"
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_generation=2, runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, runtimeInstanceID); err != nil {
		t.Fatal(err)
	}
	fence := app.AgentHostFence{RuntimeGeneration: 2, RuntimeInstanceID: runtimeInstanceID, ControlVersion: 0, AIAllowed: true}
	key := app.MCPRuntimeOperationKey{OwnerID: owner.ID, APIKeyID: "t1234567", SessionID: "sdk-session-not-found", HostID: hostValue.ID, IdempotencyKey: "not-found-call"}
	prepared, _, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, key, fence, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkMCPRuntimeOperationDispatched(ctx, key, prepared.RuntimeTaskID, fence, prepared.LeaseVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE mcp_runtime_operations SET cancel_requested_at=clock_timestamp() WHERE operation_id=$1`, prepared.OperationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkMCPRuntimeOperationDispatched(ctx, key, prepared.RuntimeTaskID, fence, prepared.LeaseVersion); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("dispatch replay after cancellation request error = %v", err)
	}
	if _, err := store.RenewMCPRuntimeOperationLease(ctx, key, prepared.RuntimeTaskID, fence, 1, prepared.LeaseVersion, time.Minute); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("renew after cancellation request error = %v", err)
	}
	if _, err := store.SettleMCPRuntimeOperationNotFound(ctx, key, prepared.RuntimeTaskID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("settle operation-not-found before lease expiry error = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE mcp_runtime_operations SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE operation_id=$1`, prepared.OperationID); err != nil {
		t.Fatal(err)
	}
	settled, err := store.SettleMCPRuntimeOperationNotFound(ctx, key, prepared.RuntimeTaskID)
	if err != nil || settled.Status != task.OperationCancelled {
		t.Fatalf("settle expired operation-not-found = %#v, err=%v", settled, err)
	}
	replay, err := store.SettleMCPRuntimeOperationNotFound(ctx, key, prepared.RuntimeTaskID)
	if err != nil || replay.Status != task.OperationCancelled {
		t.Fatalf("replay expired operation-not-found settlement = %#v, err=%v", replay, err)
	}
}

func TestPostgresMCPRuntimeOperationRequiresOwnerAndHostFences(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	ensureMCPTestAPIKey(t, ctx, store, owner.ID, "t1234567")
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	const runtimeInstanceID = "mcp-fence-test-instance"
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_generation=3, runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, runtimeInstanceID); err != nil {
		t.Fatal(err)
	}
	fence := app.AgentHostFence{RuntimeGeneration: 3, RuntimeInstanceID: runtimeInstanceID, ControlVersion: 0, AIAllowed: true}
	key := app.MCPRuntimeOperationKey{OwnerID: owner.ID, APIKeyID: "t1234567", SessionID: "sdk-session-fence", HostID: hostValue.ID, IdempotencyKey: "fenced-call"}
	request := newMCPRuntimeBeginRequest(t, key, fence, 1)

	wrongEpoch := request
	wrongEpoch.ExpectedExecutionEpoch = 99
	if _, _, err := store.BeginMCPRuntimeOperation(ctx, wrongEpoch); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("begin with stale owner execution epoch error = %v", err)
	}
	wrogFence := request
	wrogFence.Fence.RuntimeInstanceID = "old-instance"
	if _, _, err := store.BeginMCPRuntimeOperation(ctx, wrogFence); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("begin with stale runtime instance fence error = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='disabled', epoch=2 WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.BeginMCPRuntimeOperation(ctx, request); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("begin after owner execution gate disabled error = %v", err)
	}
}

func TestPostgresMCPRuntimeContextIsIsolatedByAPIKey(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	ensureMCPTestAPIKey(t, ctx, store, owner.ID, "t1234567")
	ensureMCPTestAPIKey(t, ctx, store, owner.ID, "t7654321")
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	const runtimeInstanceID = "mcp-key-isolation-runtime"
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_generation=4, runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, runtimeInstanceID); err != nil {
		t.Fatal(err)
	}
	fence := app.AgentHostFence{RuntimeGeneration: 4, RuntimeInstanceID: runtimeInstanceID, ControlVersion: 0, AIAllowed: true}
	keyA := app.MCPRuntimeOperationKey{OwnerID: owner.ID, APIKeyID: "t1234567", SessionID: "shared-protocol-session", HostID: hostValue.ID, IdempotencyKey: "same-tool-call"}
	keyB := keyA
	keyB.APIKeyID = "t7654321"
	operationA, replayed, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, keyA, fence, 1))
	if err != nil || replayed {
		t.Fatalf("begin first API Key session = %#v replayed=%t err=%v", operationA, replayed, err)
	}
	operationB, replayed, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, keyB, fence, 1))
	if err != nil || replayed || operationA.RuntimeTaskID == operationB.RuntimeTaskID || operationA.OperationID == operationB.OperationID {
		t.Fatalf("second API Key did not receive an isolated context: A=%#v B=%#v replayed=%t err=%v", operationA, operationB, replayed, err)
	}
	loadedA, err := store.GetMCPRuntimeOperation(ctx, keyA)
	if err != nil || loadedA.OperationID != operationA.OperationID {
		t.Fatalf("lookup first API Key operation = %#v err=%v", loadedA, err)
	}
	loadedB, err := store.GetMCPRuntimeOperation(ctx, keyB)
	if err != nil || loadedB.OperationID != operationB.OperationID {
		t.Fatalf("lookup second API Key operation = %#v err=%v", loadedB, err)
	}
	activeTaskIDs, err := store.ListActiveMCPRuntimeTaskIDs(ctx, owner.ID, hostValue.ID, fence)
	if err != nil || len(activeTaskIDs) != 2 {
		t.Fatalf("API Key session active runtime task IDs = %#v err=%v", activeTaskIDs, err)
	}
}

func TestPostgresMCPRuntimePersistsActorSeparatelyFromHostOwner(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	actor := createTestUser(t, store, 8, 20_000, 40_000)
	ensureMCPTestAPIKey(t, ctx, store, actor.ID, "t1234567")
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	if _, err := store.UpsertHostMember(ctx, host.Member{
		HostID: hostValue.ID, OwnerID: owner.ID, UserID: actor.ID, Permissions: []host.Permission{host.PermissionMCPControl},
	}); err != nil {
		t.Fatal(err)
	}
	const runtimeInstanceID = "mcp-shared-actor-runtime"
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_generation=1, runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, runtimeInstanceID); err != nil {
		t.Fatal(err)
	}
	fence := app.AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: runtimeInstanceID, ControlVersion: 0, AIAllowed: true}
	key := app.MCPRuntimeOperationKey{
		OwnerID: owner.ID, ActorUserID: actor.ID, APIKeyID: "t1234567", SessionID: "shared-actor-session",
		HostID: hostValue.ID, IdempotencyKey: "shared-actor-call",
	}
	operation, replayed, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, key, fence, 1))
	if err != nil || replayed {
		t.Fatalf("begin shared actor MCP operation = %#v, replayed=%t, err=%v", operation, replayed, err)
	}
	if operation.Key.OwnerID != owner.ID || operation.Key.ActorUserID != actor.ID {
		t.Fatalf("stored operation owner/actor = %q/%q, want %q/%q", operation.Key.OwnerID, operation.Key.ActorUserID, owner.ID, actor.ID)
	}
	ownerActive, err := store.ListOwnerActiveMCPRuntimeOperations(ctx, owner.ID)
	if err != nil || len(ownerActive) != 1 || ownerActive[0].Key.ActorUserID != actor.ID {
		t.Fatalf("Owner stop-all lost member operation: %#v, err=%v", ownerActive, err)
	}

	wrongActorKey := key
	wrongActorKey.ActorUserID = owner.ID
	if _, err := store.GetMCPRuntimeOperation(ctx, wrongActorKey); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("operation lookup with another actor = %v", err)
	}
	if _, err := store.GetMCPRuntimeOperation(ctx, key); err != nil {
		t.Fatalf("operation lookup for persisted actor: %v", err)
	}
}

func newMCPRuntimeBeginRequest(t *testing.T, key app.MCPRuntimeOperationKey, fence app.AgentHostFence, epoch int64) app.BeginMCPRuntimeOperation {
	t.Helper()
	requestHash := sha256.Sum256([]byte(key.IdempotencyKey + ":request"))
	return app.BeginMCPRuntimeOperation{
		Key: key, NewRuntimeTaskID: mustNewTaskID(t), NewOperationID: mustNewOperationID(t),
		RequestHash: append([]byte(nil), requestHash[:]...),
		Capability:  "browser", Action: "navigate", Arguments: json.RawMessage(`{"url":"https://example.test"}`),
		Fence: fence, ExpectedExecutionEpoch: epoch, LeaseDuration: time.Minute,
	}
}

func ensureMCPTestAPIKey(t *testing.T, ctx context.Context, store *Store, ownerID, tokenID string) {
	t.Helper()
	if _, err := store.pool.Exec(ctx, `
INSERT INTO tokens (id, owner_id, kind, name, secret_hash, expires_at)
VALUES ($1,$2,'api_key','test',decode(repeat('00',32),'hex'),clock_timestamp()+interval '1 day')`, tokenID, ownerID); err != nil {
		t.Fatalf("insert MCP test API Key %s: %v", tokenID, err)
	}
}

func mcpRuntimeCancelledReceipt(t *testing.T, operation app.MCPRuntimeOperation) json.RawMessage {
	t.Helper()
	receipt, err := json.Marshal(map[string]any{
		"runtime_status": "cancelled", "operation_id": operation.OperationID,
		"runtime_generation": operation.Fence.RuntimeGeneration, "runtime_instance_id": operation.Fence.RuntimeInstanceID,
		"control_version": operation.Fence.ControlVersion, "lease_version": operation.LeaseVersion,
		"host_id": operation.Key.HostID, "runtime_task_id": operation.RuntimeTaskID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func mustNewTaskID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewTaskID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustNewOperationID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
