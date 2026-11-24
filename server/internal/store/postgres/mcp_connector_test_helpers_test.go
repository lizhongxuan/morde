package postgres

import (
	"context"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

// bindApprovedMCPConnectorOperationForTest gives low-level dispatch tests the
// same exact, current approval binding required by production MCP operations.
func bindApprovedMCPConnectorOperationForTest(t *testing.T, ctx context.Context, store *Store,
	value app.MCPConnectorOperation) app.MCPConnectorOperation {
	t.Helper()
	var taskRevision int64
	if err := store.pool.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1 AND owner_id=$2`,
		value.TaskID, value.OwnerID).Scan(&taskRevision); err != nil {
		t.Fatalf("read MCP operation Task revision: %v", err)
	}
	var approvalVersion int64
	if err := store.pool.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM task_pending_requests WHERE task_id=$1`,
		value.TaskID).Scan(&approvalVersion); err != nil {
		t.Fatalf("read next MCP approval version: %v", err)
	}
	approvalID := mustID(t, domain.NewPendingRequestID)
	callID := "mcp-test-call-" + approvalID[1:]
	approvalHash := hashBytes("approved test MCP request:" + value.Key)
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_pending_requests
(id,task_id,version,kind,title,object,content,impact,call_id,tool_name,request_hash,task_revision,
 runtime_generation,runtime_instance_id,control_version,status,decision,responded_by_user_id,responded_at,expires_at)
VALUES ($1,$2,$3,'approval','Approve MCP action','Test connector','Synthetic MCP action','External connector action',
 $4,$5,$6,$7,$8,$9,$10,'approved','approve',$11,clock_timestamp(),clock_timestamp()+interval '1 hour')`,
		approvalID, value.TaskID, approvalVersion, callID, value.ToolName, approvalHash, taskRevision,
		value.ConnectorRevision, "mcp-connector:"+value.ConnectorID, value.GrantVersion, value.OwnerID); err != nil {
		t.Fatalf("insert exact approved MCP test request: %v", err)
	}
	value.TaskRevision = &taskRevision
	value.CallID = callID
	value.ApprovalID = approvalID
	value.ApprovalVersion = approvalVersion
	value.ApprovalRequestHash = approvalHash
	return value
}
