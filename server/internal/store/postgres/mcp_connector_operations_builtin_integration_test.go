package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresBuiltinSearchOperationIsolationReplayAndStop(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	other := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	submission := newTestTaskSubmission(t, owner.ID, "", "builtin-search-task", "search public news")
	submission.Task.ExecutionLocation = "server"
	submitted, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "builtin-search-worker", time.Minute)
	if err != nil || !found || claimed.ID != submitted.Task.ID {
		t.Fatalf("claim hostless task = %#v, found=%t, err=%v", claimed, found, err)
	}
	var taskRevision int64
	if err := pool.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1`, claimed.ID).Scan(&taskRevision); err != nil {
		t.Fatalf("read built-in MCP Task revision: %v", err)
	}
	operation := app.MCPConnectorOperation{
		ID: mustID(t, domain.NewOperationID), TaskID: claimed.ID, OwnerID: owner.ID,
		ConnectorID: app.BuiltinSearchMCPConnectorID, Key: "same-key", ToolName: "morde_web_search",
		TaskRevision: &taskRevision, CallID: "builtin-search-call",
		ConnectorRevision: 1, GrantVersion: claimed.Authorization.Version, LeaseVersion: claimed.Lease.Version,
		RequestHash: hashBytes("synthetic search"),
	}
	prepared, replay, err := store.PrepareMCPConnectorOperation(ctx, operation, claimed.Lease.Owner)
	if err != nil || replay || prepared.Status != task.OperationPrepared {
		t.Fatalf("prepare = %#v replay=%t err=%v", prepared, replay, err)
	}
	retry := operation
	retry.ID = mustID(t, domain.NewOperationID)
	again, replay, err := store.PrepareMCPConnectorOperation(ctx, retry, claimed.Lease.Owner)
	if err != nil || !replay || again.ID != prepared.ID {
		t.Fatalf("replay = %#v replay=%t err=%v", again, replay, err)
	}
	retry.RequestHash = hashBytes("different search")
	if _, _, err := store.PrepareMCPConnectorOperation(ctx, retry, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed request error = %v", err)
	}
	if _, err := store.GetMCPConnectorOperation(ctx, other.ID, claimed.ID, app.BuiltinSearchMCPConnectorID, operation.Key); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-user operation read error = %v", err)
	}
	invalid := operation
	invalid.ConnectorID = "builtin:other"
	if _, _, err := store.PrepareMCPConnectorOperation(ctx, invalid, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("unregistered built-in source error = %v", err)
	}
	connectorID := mustID(t, domain.NewMCPConnectorID)
	if _, err := pool.Exec(ctx, `INSERT INTO mcp_connectors (id,owner_id,name,endpoint_url,enabled,credential_version,revision)
VALUES ($1,$2,'test','https://mcp.example.test',true,0,1)`, connectorID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_mcp_connector_grants
(task_id,owner_id,connector_owner_id,connector_id,connector_revision,credential_version,grant_version)
VALUES ($1,$2,$2,$3,1,0,1)`, claimed.ID, owner.ID, connectorID); err != nil {
		t.Fatal(err)
	}
	ordinary := operation
	ordinary.ID, ordinary.ConnectorID = mustID(t, domain.NewOperationID), connectorID
	ordinary.ApprovalID, ordinary.ApprovalVersion = mustID(t, domain.NewPendingRequestID), 1
	ordinary.ApprovalRequestHash = hashBytes("ordinary connector approval")
	if _, err := pool.Exec(ctx, `INSERT INTO task_pending_requests
(id,task_id,version,kind,title,object,content,impact,call_id,tool_name,request_hash,task_revision,
 runtime_generation,runtime_instance_id,control_version,status,decision,responded_by_user_id,responded_at)
VALUES ($1,$2,1,'approval','Approve MCP action','Test connector','Synthetic MCP action','External connector action',$3,$4,$5,$6,
 1,$7,1,'approved','approve',$8,clock_timestamp())`,
		ordinary.ApprovalID, ordinary.TaskID, ordinary.CallID, ordinary.ToolName, ordinary.ApprovalRequestHash,
		*ordinary.TaskRevision, "mcp-connector:"+ordinary.ConnectorID, owner.ID); err != nil {
		t.Fatalf("insert ordinary MCP connector approval: %v", err)
	}
	if _, replay, err := store.PrepareMCPConnectorOperation(ctx, ordinary, claimed.Lease.Owner); err != nil || replay {
		t.Fatalf("ordinary connector shares no built-in source key: replay=%t err=%v", replay, err)
	}
	dispatched, err := store.MarkMCPConnectorOperationDispatched(ctx, prepared, claimed.Lease.Owner)
	if err != nil {
		t.Fatal(err)
	}
	stopID := mustID(t, domain.NewTaskActionID)
	stopRecord := taskActionIdempotency(owner.ID, task.ActionCancel, "builtin-search-stop", claimed.ID, task.StatusRunning, stopID)
	stopping, err := store.RequestTaskAction(ctx, stopRecord, claimed.ID, task.ActionCancel, task.StatusRunning, nil, stopID)
	if err != nil || len(stopping.UnsettledOperationIDs) != 2 {
		t.Fatalf("cancel request = %#v, err=%v", stopping, err)
	}
	stopped, err := store.GetMCPConnectorOperation(ctx, owner.ID, claimed.ID, app.BuiltinSearchMCPConnectorID, operation.Key)
	if err != nil || stopped.Status != task.OperationDispatched || !stopped.CancelRequested {
		t.Fatalf("dispatched search not fenced by cancellation: %#v, %v", stopped, err)
	}
	if _, err := store.CompleteMCPConnectorOperation(ctx, dispatched, claimed.Lease.Owner, task.OperationSucceeded, `{"text":["late"]}`); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("late search result error = %v", err)
	}
	if err := store.MarkMCPConnectorOperationUnknown(ctx, dispatched, claimed.Lease.Owner, "search interrupted"); err != nil {
		t.Fatal(err)
	}
}
