package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/task"
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
	t           *testing.T
	ownerID     string
	connectorID string
	running     task.Task
	leaseOwner  string
}

func newMCPConnectorOperationFixture(t *testing.T, ctx context.Context, store *Store, ownerID, idempotencyKey string) mcpConnectorOperationFixture {
	t.Helper()
	hostValue := createRunningControlHost(t, ctx, store, ownerID, "ai", 0)
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, ownerID, hostValue.ID, idempotencyKey, "run a task with a connector"))
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
	return mcpConnectorOperationFixture{t: t, ownerID: ownerID, connectorID: connectorID, running: running, leaseOwner: running.Lease.Owner}
}

func (f mcpConnectorOperationFixture) operation(key, toolName string) app.MCPConnectorOperation {
	return app.MCPConnectorOperation{
		ID: mustID(f.t, domain.NewOperationID), TaskID: f.running.ID, OwnerID: f.ownerID, ConnectorID: f.connectorID,
		Key: key, ToolName: toolName, ConnectorRevision: 1, CredentialVersion: 0, GrantVersion: 1,
		LeaseVersion: f.running.Lease.Version, RequestHash: hashBytes("connector-request:" + key),
	}
}
