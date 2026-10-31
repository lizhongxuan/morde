package postgres

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func TestPostgresMCPConnectorOperationReplayCASAndVersionFences(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	fixture := newMCPConnectorOperationFixture(t, ctx, store, owner.ID, "mcp-operation-replay")
	operation := fixture.operation("replay-key", "search")

	prepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, operation, fixture.leaseOwner)
	if err != nil || replayed || prepared.Status != task.OperationPrepared || prepared.CreatedAt.IsZero() || prepared.ID != operation.ID {
		t.Fatalf("prepare = %#v replayed=%t err=%v", prepared, replayed, err)
	}
	retry := operation
	retry.ID = mustID(t, domain.NewOperationID)
	preparedAgain, replayed, err := store.PrepareMCPConnectorOperation(ctx, retry, fixture.leaseOwner)
	if err != nil || !replayed || preparedAgain.ID != operation.ID || preparedAgain.Status != task.OperationPrepared {
		t.Fatalf("idempotent prepare = %#v replayed=%t err=%v", preparedAgain, replayed, err)
	}
	changedHash := retry
	changedHash.RequestHash = hashBytes("different MCP arguments")
	if _, _, err := store.PrepareMCPConnectorOperation(ctx, changedHash, fixture.leaseOwner); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("same key with a different request hash = %v", err)
	}

	// The request remains the same across Task lease recovery. A still-prepared
	// operation can be adopted because no remote side effect has been sent.
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, fixture.running.ID); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "recovered-task-worker", time.Minute)
	if err != nil || !found || claimed.ID != fixture.running.ID || claimed.Lease.Version == prepared.LeaseVersion {
		t.Fatalf("reclaim task lease = %#v found=%t err=%v", claimed, found, err)
	}
	fixture.running = claimed
	fixture.leaseOwner = claimed.Lease.Owner
	adoption := operation
	adoption.LeaseVersion = claimed.Lease.Version
	adopted, replayed, err := store.PrepareMCPConnectorOperation(ctx, adoption, claimed.Lease.Owner)
	if err != nil || !replayed || adopted.ID != operation.ID || adopted.LeaseVersion != claimed.Lease.Version {
		t.Fatalf("adopt prepared operation = %#v replayed=%t err=%v", adopted, replayed, err)
	}

	for name, mutate := range map[string]func(*app.MCPConnectorOperation){
		"connector revision": func(value *app.MCPConnectorOperation) { value.ConnectorRevision++ },
		"credential version": func(value *app.MCPConnectorOperation) { value.CredentialVersion++ },
		"grant version":      func(value *app.MCPConnectorOperation) { value.GrantVersion++ },
		"lease version":      func(value *app.MCPConnectorOperation) { value.LeaseVersion++ },
	} {
		t.Run(name, func(t *testing.T) {
			value := fixture.operation("isolation-"+name, "search")
			mutate(&value)
			if _, _, err := store.PrepareMCPConnectorOperation(ctx, value, claimed.Lease.Owner); err == nil {
				t.Fatal("prepare succeeded with a stale or unrelated version")
			}
		})
	}

	other := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	otherOwner := operation
	otherOwner.ID = mustID(t, domain.NewOperationID)
	otherOwner.Key = "other-owner"
	otherOwner.OwnerID = other.ID
	if _, _, err := store.PrepareMCPConnectorOperation(ctx, otherOwner, fixture.leaseOwner); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner prepare = %v", err)
	}
	if _, err := store.GetMCPConnectorOperation(ctx, other.ID, fixture.running.ID, fixture.connectorID, operation.Key); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner operation read = %v", err)
	}

	// Dispatch must revalidate current connector and grant state. A revoked or
	// disabled connector cannot cross the external side-effect boundary.
	preparedDispatch, _, err := store.PrepareMCPConnectorOperation(ctx, fixture.operation("revoke-key", "search"), claimed.Lease.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE mcp_connectors SET enabled=false WHERE id=$1`, fixture.connectorID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkMCPConnectorOperationDispatched(ctx, preparedDispatch, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("dispatch with disabled connector = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE mcp_connectors SET enabled=true WHERE id=$1`, fixture.connectorID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_mcp_connector_grants SET revoked_at=clock_timestamp() WHERE task_id=$1 AND connector_id=$2`, fixture.running.ID, fixture.connectorID); err != nil {
		t.Fatal(err)
	}
	revokedReplay, replayed, err := store.PrepareMCPConnectorOperation(ctx, preparedDispatch, claimed.Lease.Owner)
	if err != nil || !replayed || revokedReplay.Status != task.OperationPrepared {
		t.Fatalf("prepare replay after grant revocation = %#v replayed=%t err=%v", revokedReplay, replayed, err)
	}
	if _, err := store.MarkMCPConnectorOperationDispatched(ctx, preparedDispatch, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("dispatch with revoked grant = %v", err)
	}
}

func TestPostgresMCPConnectorOperationDispatchedCompletionAndUnknownAreTerminal(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	fixture := newMCPConnectorOperationFixture(t, ctx, store, owner.ID, "mcp-operation-terminal")

	completedInput := fixture.operation("completed-key", "search")
	prepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, completedInput, fixture.leaseOwner)
	if err != nil || replayed {
		t.Fatalf("prepare completion operation = %#v replay=%t err=%v", prepared, replayed, err)
	}
	dispatched, err := store.MarkMCPConnectorOperationDispatched(ctx, prepared, fixture.leaseOwner)
	if err != nil || dispatched.Status != task.OperationDispatched {
		t.Fatalf("dispatch operation = %#v err=%v", dispatched, err)
	}
	if _, err := store.MarkMCPConnectorOperationDispatched(ctx, prepared, fixture.leaseOwner); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate dispatch = %v", err)
	}
	completed, err := store.CompleteMCPConnectorOperation(ctx, dispatched, fixture.leaseOwner, task.OperationSucceeded, `{"count":1}`)
	if err != nil || completed.Status != task.OperationSucceeded || completed.Result != `{"count":1}` {
		t.Fatalf("complete operation = %#v err=%v", completed, err)
	}
	completedAgain, err := store.CompleteMCPConnectorOperation(ctx, dispatched, fixture.leaseOwner, task.OperationSucceeded, `{"count":1}`)
	if err != nil || completedAgain.Status != task.OperationSucceeded {
		t.Fatalf("idempotent completion = %#v err=%v", completedAgain, err)
	}
	if _, err := store.CompleteMCPConnectorOperation(ctx, dispatched, fixture.leaseOwner, task.OperationSucceeded, `{"count":2}`); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("different duplicate completion result = %v", err)
	}

	unknownInput := fixture.operation("unknown-key", "update")
	unknownPrepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, unknownInput, fixture.leaseOwner)
	if err != nil || replayed {
		t.Fatalf("prepare unknown operation = %#v replay=%t err=%v", unknownPrepared, replayed, err)
	}
	unknownDispatched, err := store.MarkMCPConnectorOperationDispatched(ctx, unknownPrepared, fixture.leaseOwner)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkMCPConnectorOperationUnknown(ctx, unknownDispatched, fixture.leaseOwner, "transport outcome uncertain"); err != nil {
		t.Fatal(err)
	}
	unknown, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, unknownInput.Key)
	if err != nil || unknown.Status != task.OperationUnknown || unknown.Result != "transport outcome uncertain" {
		t.Fatalf("get unknown operation = %#v err=%v", unknown, err)
	}
	replayedUnknown, replayed, err := store.PrepareMCPConnectorOperation(ctx, unknownInput, fixture.leaseOwner)
	if err != nil || !replayed || replayedUnknown.Status != task.OperationUnknown {
		t.Fatalf("unknown operation replay = %#v replayed=%t err=%v", replayedUnknown, replayed, err)
	}
	if _, err := store.MarkMCPConnectorOperationDispatched(ctx, unknownDispatched, fixture.leaseOwner); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("unknown operation was dispatchable again: %v", err)
	}
	if err := store.MarkMCPConnectorOperationUnknown(ctx, unknownDispatched, fixture.leaseOwner, "different summary"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("unknown result was overwritten: %v", err)
	}
}

func TestPostgresMCPConnectorOperationPersistsWorkspaceFence(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "mcp-operation-workspace-fence")
	fixture := newMCPConnectorOperationFixtureWithWorkspace(t, ctx, store, owner.ID, "mcp-operation-workspace-fence", &workspaceValue)
	input := fixture.operation("workspace-fence", "search")
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_legacy_volumes (workspace_id, volume_name, import_state) VALUES ($1,$2,'pending')`, workspaceValue.ID, "legacy-"+workspaceValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PrepareMCPConnectorOperation(ctx, fixture.operation("blocked-by-import", "search"), fixture.leaseOwner); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("MCP operation preparation during pending legacy import = %v; want workspace busy", err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM workspace_legacy_volumes WHERE workspace_id=$1`, workspaceValue.ID); err != nil {
		t.Fatal(err)
	}
	prepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, input, fixture.leaseOwner)
	if err != nil || replayed || prepared.Status != task.OperationPrepared || prepared.HostID != fixture.hostID ||
		prepared.WorkspaceID != workspaceValue.ID || prepared.BindingVersion != fixture.bindingVersion || prepared.InputRevision == nil ||
		*prepared.InputRevision != workspaceValue.Revision || prepared.WorkspaceCheckpointRevision == nil ||
		*prepared.WorkspaceCheckpointRevision != workspaceValue.Revision {
		t.Fatalf("prepare Workspace-fenced operation = %#v replay=%t err=%v", prepared, replayed, err)
	}
	loaded, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, input.Key)
	if err != nil || !sameMCPConnectorOperationRequest(loaded, input) {
		t.Fatalf("loaded Workspace-fenced operation = %#v err=%v", loaded, err)
	}

	changedBinding := input
	changedBinding.BindingVersion++
	if _, _, err := store.PrepareMCPConnectorOperation(ctx, changedBinding, fixture.leaseOwner); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("same operation key with a new binding version = %v", err)
	}
	changedCheckpoint := input
	newRevision := workspaceValue.Revision + 1
	changedCheckpoint.InputRevision, changedCheckpoint.WorkspaceCheckpointRevision = &newRevision, &newRevision
	if _, _, err := store.PrepareMCPConnectorOperation(ctx, changedCheckpoint, fixture.leaseOwner); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("same operation key with a new checkpoint = %v", err)
	}
	staleTaskSnapshot := fixture.operation("stale-task-snapshot", "search")
	staleTaskSnapshot.InputRevision, staleTaskSnapshot.WorkspaceCheckpointRevision = &newRevision, &newRevision
	if _, _, err := store.PrepareMCPConnectorOperation(ctx, staleTaskSnapshot, fixture.leaseOwner); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("new operation with a different Task checkpoint = %v", err)
	}

	wrongBinding := fixture.operation("stale-binding", "search")
	wrongBinding.BindingVersion++
	if _, _, err := store.PrepareMCPConnectorOperation(ctx, wrongBinding, fixture.leaseOwner); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("new operation with stale binding version = %v", err)
	}
	if _, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, wrongBinding.Key); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("stale binding operation was persisted: %v", err)
	}
}

func TestPostgresMCPConnectorDispatchWaitsForWorkspaceCommitFence(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "mcp-operation-commit-race")
	fixture := newMCPConnectorOperationFixtureWithWorkspace(t, ctx, store, owner.ID, "mcp-operation-commit-race", &workspaceValue)
	prepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, fixture.operation("commit-race", "search"), fixture.leaseOwner)
	if err != nil || replayed {
		t.Fatalf("prepare MCP operation before Workspace commit: %#v replay=%t err=%v", prepared, replayed, err)
	}

	file := syncTestFile("external-update.txt", []byte("new Workspace revision"))
	session := newTestSyncSession(t, workspaceValue, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
	session.PlannedBytes = 0
	session.ReplicaID = fmt.Sprintf("host:%s:g%d:%s:b%d", fixture.hostID, fixture.hostGeneration, workspaceValue.ID, fixture.bindingVersion)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks([]workspace.File{file}),
		syncTestIdempotency(owner.ID, "workspace.sync.create", "mcp-commit-race-create", session.ID)); err != nil {
		t.Fatalf("create Host-scoped Workspace sync session: %v", err)
	}

	const advisoryLockKey int64 = 824726199
	if _, err := pool.Exec(ctx, `CREATE FUNCTION wait_for_mcp_workspace_revision_race_release() RETURNS trigger AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(824726199);
  RETURN NEW;
END;
$$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create Workspace revision test barrier: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER wait_for_mcp_workspace_revision_race_release
BEFORE INSERT ON workspace_revisions FOR EACH ROW EXECUTE FUNCTION wait_for_mcp_workspace_revision_race_release()`); err != nil {
		t.Fatalf("install Workspace revision test barrier: %v", err)
	}
	advisoryConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = advisoryConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
		advisoryConn.Release()
	}()
	if _, err := advisoryConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		t.Fatal(err)
	}

	type commitResult struct {
		session workspace.SyncSession
		err     error
	}
	committed := make(chan commitResult, 1)
	go func() {
		value, _, commitErr := commitWorkspaceSyncSession(ctx, store, workspaceValue.ID, session.ID,
			syncTestIdempotency(owner.ID, "workspace.sync.commit", "mcp-commit-race-commit", session.ID))
		committed <- commitResult{session: value, err: commitErr}
	}()
	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	waitForPostgresLockWait(t, waitCtx, pool, "%INSERT INTO workspace_revisions%", 1)

	dispatched := make(chan error, 1)
	go func() {
		_, dispatchErr := store.MarkMCPConnectorOperationDispatched(ctx, prepared, fixture.leaseOwner)
		dispatched <- dispatchErr
	}()
	waitForPostgresLockWait(t, waitCtx, pool, "%FROM hosts WHERE id=$1 FOR SHARE%", 1)
	var unlocked bool
	if err := advisoryConn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, advisoryLockKey).Scan(&unlocked); err != nil || !unlocked {
		t.Fatalf("release Workspace revision test barrier: unlocked=%t err=%v", unlocked, err)
	}
	select {
	case result := <-committed:
		if result.err != nil || result.session.State != workspace.SyncCompleted || result.session.TargetRevision != workspaceValue.Revision+1 {
			t.Fatalf("Workspace commit that won dispatch race = %#v err=%v", result.session, result.err)
		}
	case <-waitCtx.Done():
		t.Fatalf("Workspace sync commit did not finish: %v", waitCtx.Err())
	}
	select {
	case err := <-dispatched:
		if domain.CodeOf(err) != domain.CodeWorkspaceConflict {
			t.Fatalf("dispatch after Workspace commit = %v, want Workspace conflict", err)
		}
	case <-waitCtx.Done():
		t.Fatalf("MCP dispatch did not finish after Workspace commit: %v", waitCtx.Err())
	}
	stored, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, prepared.Key)
	if err != nil || stored.Status != task.OperationPrepared {
		t.Fatalf("operation after stale dispatch rejection = %#v err=%v", stored, err)
	}
}

func TestPostgresMCPConnectorOperationSettlesAfterConnectorDeletion(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	fixture := newMCPConnectorOperationFixture(t, ctx, store, owner.ID, "mcp-operation-delete")

	var dispatched []app.MCPConnectorOperation
	for _, item := range []struct{ key, tool string }{{"delete-succeeded", "search"}, {"delete-unknown", "update"}} {
		prepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, fixture.operation(item.key, item.tool), fixture.leaseOwner)
		if err != nil || replayed {
			t.Fatalf("prepare %s operation = %#v replay=%t err=%v", item.key, prepared, replayed, err)
		}
		value, err := store.MarkMCPConnectorOperationDispatched(ctx, prepared, fixture.leaseOwner)
		if err != nil {
			t.Fatalf("dispatch %s operation: %v", item.key, err)
		}
		dispatched = append(dispatched, value)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM mcp_connectors WHERE id=$1 AND owner_id=$2`, fixture.connectorID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteMCPConnectorOperation(ctx, dispatched[0], fixture.leaseOwner, task.OperationSucceeded, "completed after connector deletion"); err != nil {
		t.Fatalf("persist known result after connector deletion: %v", err)
	}
	if err := store.MarkMCPConnectorOperationUnknown(ctx, dispatched[1], fixture.leaseOwner, "unknown after connector deletion"); err != nil {
		t.Fatalf("persist unknown result after connector deletion: %v", err)
	}
	for key, status := range map[string]task.OperationStatus{
		"delete-succeeded": task.OperationSucceeded,
		"delete-unknown":   task.OperationUnknown,
	} {
		value, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, key)
		if err != nil || value.Status != status {
			t.Fatalf("read %s operation after connector deletion = %#v err=%v", key, value, err)
		}
	}
}

func TestPostgresTaskStopWaitsForDispatchedMCPConnectorOperation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	fixture := newMCPConnectorOperationFixture(t, ctx, store, owner.ID, "mcp-operation-stop")
	preparedInput := fixture.operation("stop-prepared", "search")
	prepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, preparedInput, fixture.leaseOwner)
	if err != nil || replayed {
		t.Fatalf("prepare operation to cancel = %#v replay=%t err=%v", prepared, replayed, err)
	}
	dispatchedInput := fixture.operation("stop-dispatched", "update")
	dispatchedPrepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, dispatchedInput, fixture.leaseOwner)
	if err != nil || replayed {
		t.Fatalf("prepare operation to settle = %#v replay=%t err=%v", dispatchedPrepared, replayed, err)
	}
	dispatched, err := store.MarkMCPConnectorOperationDispatched(ctx, dispatchedPrepared, fixture.leaseOwner)
	if err != nil {
		t.Fatal(err)
	}

	stopID := mustID(t, domain.NewTaskActionID)
	stopRecord := taskActionIdempotency(owner.ID, task.ActionCancel, "mcp-stop-action", fixture.running.ID, task.StatusRunning, stopID)
	requested, err := store.RequestTaskAction(ctx, stopRecord, fixture.running.ID, task.ActionCancel, task.StatusRunning, stopID)
	if err != nil || requested.Task.Status != task.StatusCancelling || len(requested.UnsettledOperationIDs) != 2 {
		t.Fatalf("stop request = %#v err=%v", requested, err)
	}
	claim, found, err := store.ClaimNextTaskStop(ctx, "stop-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim stop = %#v found=%t err=%v", claim, found, err)
	}
	preparedAfterStop, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, prepared.Key)
	if err != nil || preparedAfterStop.Status != task.OperationCancelled {
		t.Fatalf("prepared operation after stop claim = %#v err=%v", preparedAfterStop, err)
	}
	if _, err := store.CompleteTaskStop(ctx, fixture.running.ID, stopID, "stop-worker", claim.Request.LeaseVersion); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stop finalized before dispatched connector call settled: %v", err)
	}
	if err := store.MarkMCPConnectorOperationUnknown(ctx, dispatched, fixture.leaseOwner, "transport outcome uncertain"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE id=$1`, stopID); err != nil {
		t.Fatal(err)
	}
	stopped, err := store.CompleteTaskStop(ctx, fixture.running.ID, stopID, "stop-worker", claim.Request.LeaseVersion)
	if err != nil || stopped.Status != task.StatusCancelled {
		t.Fatalf("stop after local connector call settled unknown = %#v err=%v", stopped, err)
	}
	unknown, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, dispatched.Key)
	if err != nil || unknown.Status != task.OperationUnknown {
		t.Fatalf("unknown operation audit record after stop = %#v err=%v", unknown, err)
	}
}

func TestPostgresConnectorGrantRevocationFencesOperations(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	fixture := newMCPConnectorOperationFixture(t, ctx, store, owner.ID, "mcp-operation-grant-revoke")

	prepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, fixture.operation("grant-prepared", "search"), fixture.leaseOwner)
	if err != nil || replayed {
		t.Fatalf("prepare before grant revoke = %#v replayed=%t err=%v", prepared, replayed, err)
	}
	dispatchedPrepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, fixture.operation("grant-dispatched", "update"), fixture.leaseOwner)
	if err != nil || replayed {
		t.Fatalf("prepare dispatched before grant revoke = %#v replayed=%t err=%v", dispatchedPrepared, replayed, err)
	}
	dispatched, err := store.MarkMCPConnectorOperationDispatched(ctx, dispatchedPrepared, fixture.leaseOwner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RevokeTaskMCPConnectorGrant(ctx, owner.ID, fixture.running.ID, fixture.connectorID, 1); err != nil {
		t.Fatalf("revoke task connector grant: %v", err)
	}
	for key, status := range map[string]task.OperationStatus{
		"grant-prepared":   task.OperationCancelled,
		"grant-dispatched": task.OperationDispatched,
	} {
		value, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, key)
		if err != nil || value.Status != status || (key == "grant-dispatched") != value.CancelRequested {
			t.Fatalf("operation after grant revoke %q = %#v err=%v", key, value, err)
		}
	}
	if err := store.MarkMCPConnectorOperationUnknown(ctx, dispatched, fixture.leaseOwner, "call ended after grant revocation"); err != nil {
		t.Fatalf("settle revoked in-flight MCP operation: %v", err)
	}
	if _, err := store.CompleteMCPConnectorOperation(ctx, dispatched, fixture.leaseOwner, task.OperationSucceeded, "late result"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("late result after grant revocation was accepted: %v", err)
	}
}

func TestPostgresConnectorRevisionChangeFencesOperations(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	fixture := newMCPConnectorOperationFixture(t, ctx, store, owner.ID, "mcp-operation-connector-update")

	prepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, fixture.operation("revision-prepared", "search"), fixture.leaseOwner)
	if err != nil || replayed {
		t.Fatalf("prepare before connector revision change = %#v replayed=%t err=%v", prepared, replayed, err)
	}
	dispatchedPrepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, fixture.operation("revision-dispatched", "update"), fixture.leaseOwner)
	if err != nil || replayed {
		t.Fatalf("prepare dispatched before connector revision change = %#v replayed=%t err=%v", dispatchedPrepared, replayed, err)
	}
	dispatched, err := store.MarkMCPConnectorOperationDispatched(ctx, dispatchedPrepared, fixture.leaseOwner)
	if err != nil {
		t.Fatal(err)
	}
	connector, err := store.GetMCPConnector(ctx, owner.ID, fixture.connectorID)
	if err != nil {
		t.Fatal(err)
	}
	connector.Name = "updated connector"
	if _, err := store.UpdateMCPConnector(ctx, owner.ID, connector, connector.Revision, audit.Entry{
		ActorID: owner.ID, ActorKind: audit.ActorSession, Action: audit.ActionMCPConnectorUpdate, Target: connector.ID,
	}); err != nil {
		t.Fatalf("update MCP connector revision: %v", err)
	}
	for key, status := range map[string]task.OperationStatus{
		"revision-prepared":   task.OperationCancelled,
		"revision-dispatched": task.OperationDispatched,
	} {
		value, err := store.GetMCPConnectorOperation(ctx, owner.ID, fixture.running.ID, fixture.connectorID, key)
		if err != nil || value.Status != status || (key == "revision-dispatched") != value.CancelRequested {
			t.Fatalf("operation after connector revision change %q = %#v err=%v", key, value, err)
		}
	}
	if err := store.MarkMCPConnectorOperationUnknown(ctx, dispatched, fixture.leaseOwner, "call ended after connector revision change"); err != nil {
		t.Fatalf("settle connector-revoked in-flight MCP operation: %v", err)
	}
	if _, err := store.CompleteMCPConnectorOperation(ctx, dispatched, fixture.leaseOwner, task.OperationSucceeded, "late result"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("late result after connector revision change was accepted: %v", err)
	}
}

type mcpConnectorOperationFixture struct {
	t                           *testing.T
	ownerID                     string
	connectorID                 string
	hostID                      string
	hostGeneration              int64
	workspaceID                 string
	bindingVersion              int64
	inputRevision               *int64
	workspaceCheckpointRevision *int64
	running                     task.Task
	leaseOwner                  string
}

func newMCPConnectorOperationFixture(t *testing.T, ctx context.Context, store *Store, ownerID, idempotencyKey string) mcpConnectorOperationFixture {
	return newMCPConnectorOperationFixtureWithWorkspace(t, ctx, store, ownerID, idempotencyKey, nil)
}

func newMCPConnectorOperationFixtureWithWorkspace(t *testing.T, ctx context.Context, store *Store, ownerID, idempotencyKey string,
	workspaceValue *workspace.Workspace) mcpConnectorOperationFixture {
	t.Helper()
	hostValue := createRunningControlHost(t, ctx, store, ownerID, "ai", 0)
	var bindingVersion int64
	if workspaceValue != nil {
		binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
			OwnerID: ownerID, HostID: hostValue.ID, Workspace: *workspaceValue, Directory: workspaceValue.ID,
			State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
			SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
		})
		if err != nil {
			t.Fatal(err)
		}
		binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
			workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, workspaceValue.Revision)
		if err != nil {
			t.Fatal(err)
		}
		bindingVersion = binding.BindingVersion
	}
	submission := newTestTaskSubmission(t, ownerID, hostValue.ID, idempotencyKey, "run a task with a connector")
	if workspaceValue != nil {
		revision := workspaceValue.Revision
		manifestSHA, err := workspace.ManifestSHA256([]workspace.File{})
		if err != nil {
			t.Fatal(err)
		}
		contextDigest, err := hex.DecodeString(manifestSHA)
		if err != nil {
			t.Fatal(err)
		}
		submission.Task.WorkspaceID, submission.Task.InputRevision, submission.Task.ContextDigest = workspaceValue.ID, &revision, contextDigest
	}
	submitted, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	running, found, err := store.ClaimNextTask(ctx, "mcp-task-worker", time.Minute)
	if err != nil || !found || running.ID != submitted.Task.ID {
		t.Fatalf("claim MCP operation task = %#v found=%t err=%v", running, found, err)
	}
	connectorID := mustID(t, domain.NewMCPConnectorID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO mcp_connectors (id, owner_id, name, endpoint_url, enabled, credential_version, revision)
VALUES ($1,$2,$3,$4,true,0,1)`, connectorID, ownerID, "test connector", "https://connector.example.test/mcp"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_mcp_connector_grants
(task_id, owner_id, connector_id, connector_revision, credential_version, grant_version)
VALUES ($1,$2,$3,1,0,1)`, running.ID, ownerID, connectorID); err != nil {
		t.Fatal(err)
	}
	fixture := mcpConnectorOperationFixture{
		t: t, ownerID: ownerID, connectorID: connectorID, hostID: hostValue.ID,
		hostGeneration: hostValue.RuntimeGeneration, running: running, leaseOwner: running.Lease.Owner,
	}
	if workspaceValue != nil {
		revision := workspaceValue.Revision
		fixture.workspaceID, fixture.bindingVersion = workspaceValue.ID, bindingVersion
		fixture.inputRevision, fixture.workspaceCheckpointRevision = &revision, &revision
	}
	return fixture
}

func (f mcpConnectorOperationFixture) operation(key, toolName string) app.MCPConnectorOperation {
	return app.MCPConnectorOperation{
		ID: mustID(f.t, domain.NewOperationID), TaskID: f.running.ID, OwnerID: f.ownerID, ConnectorID: f.connectorID,
		Key: key, ToolName: toolName, ConnectorRevision: 1, CredentialVersion: 0, GrantVersion: 1,
		LeaseVersion: f.running.Lease.Version, HostID: f.hostID, WorkspaceID: f.workspaceID, BindingVersion: f.bindingVersion,
		InputRevision: f.inputRevision, WorkspaceCheckpointRevision: f.workspaceCheckpointRevision,
		RequestHash: hashBytes("connector-request:" + key),
	}
}
