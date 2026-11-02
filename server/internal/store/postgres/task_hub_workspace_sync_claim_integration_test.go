package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"
)

func TestPostgresClaimTaskHubWorkspaceSyncWaitsForLatestFilePut(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "task-hub-sync-claim")
	targetID := mustID(t, domain.NewClientInstallationID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'Task sync target','desktop',clock_timestamp()+interval '1 day')`, targetID, owner.ID); err != nil {
		t.Fatalf("insert target installation: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `ALTER TABLE task_hub_operations DROP CONSTRAINT IF EXISTS task_hub_operations_action_check`); err != nil {
		t.Fatalf("allow file.put only in isolated claim predicate test database: %v", err)
	}

	submission := newTestTaskSubmission(t, owner.ID, "", "task-hub-sync-claim", "write one Workspace file")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Task: %v", err)
	}
	const fileSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	newLinkedSession := func(operationKey string) workspace.SyncSession {
		t.Helper()
		session := newTestSyncSession(t, workspaceValue, owner.ID, []workspace.File{}, []workspace.File{}, []string{})
		session.ReplicaID = "device:" + targetID
		session.Scope = []string{"result.txt"}
		session.Policy = workspace.SyncCompareMerge
		session.TaskID, session.TaskHubOperationKey = created.Task.ID, operationKey
		params, err := workspaceSyncSessionParams(session)
		if err != nil {
			t.Fatalf("build Workspace sync session: %v", err)
		}
		if _, err := sqlcgen.New(store.pool).CreateWorkspaceSyncSession(ctx, params); err != nil {
			t.Fatalf("insert Workspace sync session: %v", err)
		}
		return session
	}
	setWaiting := func(operationKey string) {
		t.Helper()
		var sequence int64
		if err := store.pool.QueryRow(ctx, `UPDATE tasks SET status='waiting_user', waiting_reason='task_hub_workspace_sync',
lease_owner='', lease_expires_at=NULL, finished_at=NULL, event_seq=event_seq+1
WHERE id=$1 AND owner_id=$2 RETURNING event_seq`, created.Task.ID, owner.ID).Scan(&sequence); err != nil {
			t.Fatalf("put Task into Workspace sync wait: %v", err)
		}
		payload, err := json.Marshal(map[string]any{"detail": map[string]string{"operation_key": operationKey}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO task_events(task_id,sequence,event_type,payload)
VALUES ($1,$2,'task.waiting_user',$3)`, created.Task.ID, sequence, payload); err != nil {
			t.Fatalf("record Task Hub sync wait event: %v", err)
		}
	}
	insertFilePut := func(key, status, stage string, resultHash []byte, sessionID any, syncRevision any) {
		t.Helper()
		var localReceiptHash any = hashBytes("local receipt:" + key)
		if stage == "pending" {
			localReceiptHash = nil
		}
		_, err := store.pool.Exec(ctx, `INSERT INTO task_hub_operations (
task_id, owner_id, operation_key, tool_name, target_installation_id, action,
task_authorization_version, target_grant_version, lease_version, request_hash, result_hash, status,
workspace_id, workspace_path, workspace_size_bytes, workspace_sha256, source_checkpoint_revision,
local_receipt_hash, sync_stage, sync_session_id, sync_revision
) VALUES ($1,$2,$3,$4,$5,'file.put',1,1,1,$6,$7,$8,$9,'result.txt',4,$10,$11,$12,$13,$14,$15)`,
			created.Task.ID, owner.ID, key, "morde_hub_file_put_"+targetID+"_v1", targetID,
			hashBytes("Task file.put request:"+key), resultHash, status, workspaceValue.ID, fileSHA256,
			workspaceValue.Revision, localReceiptHash, stage, sessionID, syncRevision)
		if err != nil {
			t.Fatalf("insert file.put operation %q: %v", key, err)
		}
	}

	firstKey := "agent:sync-pending-first"
	insertFilePut(firstKey, "dispatched", "local_applied", nil, nil, nil)
	session := newLinkedSession(firstKey)
	if _, err := store.pool.Exec(ctx, `UPDATE task_hub_operations SET sync_stage='sync_pending', sync_session_id=$4
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3`, owner.ID, created.Task.ID, firstKey, session.ID); err != nil {
		t.Fatalf("link first file.put operation: %v", err)
	}
	setWaiting(firstKey)
	if claimed, found, err := store.ClaimNextTask(ctx, "hub-sync-claim-worker", time.Minute); err != nil || found {
		t.Fatalf("claim before Workspace sync commit = %#v, found=%t, err=%v; want no claim", claimed, found, err)
	}

	if _, err := store.pool.Exec(ctx, `UPDATE workspace_sync_sessions SET state='completed', target_revision=$3
WHERE workspace_id=$1 AND id=$2`, workspaceValue.ID, session.ID, workspaceValue.Revision); err != nil {
		t.Fatalf("complete linked Workspace sync session: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_hub_operations SET status='succeeded', result_hash=$4,
sync_stage='cloud_synced', sync_session_id=$5, sync_revision=$6
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3`, owner.ID, created.Task.ID,
		firstKey, hashBytes("cloud-synced metadata result"), session.ID, workspaceValue.Revision); err != nil {
		t.Fatalf("settle first file.put operation: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "hub-sync-claim-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID || claimed.Status != "running" {
		t.Fatalf("claim after Workspace sync commit = %#v, found=%t, err=%v; want the waiting Task", claimed, found, err)
	}

	setWaiting("agent:missing-current-operation")
	if claimed, found, err := store.ClaimNextTask(ctx, "hub-sync-claim-worker", time.Minute); err != nil || found {
		t.Fatalf("old cloud-synced operation woke a Task with no matching waiting operation = %#v, found=%t, err=%v; want no claim",
			claimed, found, err)
	}

	failedKey := "agent:sync-failed-current"
	insertFilePut(failedKey, "failed", "pending", hashBytes("failed result"), nil, nil)
	setWaiting(failedKey)
	if claimed, found, err := store.ClaimNextTask(ctx, "hub-sync-claim-worker", time.Minute); err != nil || found {
		t.Fatalf("old cloud-synced operation woke a Task whose current operation failed = %#v, found=%t, err=%v; want no claim",
			claimed, found, err)
	}

	pendingKey := "agent:sync-pending-second"
	insertFilePut(pendingKey, "dispatched", "local_applied", nil, nil, nil)
	pendingSession := newLinkedSession(pendingKey)
	if _, err := store.pool.Exec(ctx, `UPDATE task_hub_operations SET sync_stage='sync_pending', sync_session_id=$4
WHERE owner_id=$1 AND task_id=$2 AND operation_key=$3`, owner.ID, created.Task.ID, pendingKey, pendingSession.ID); err != nil {
		t.Fatalf("link second file.put operation: %v", err)
	}
	setWaiting(pendingKey)
	if claimed, found, err := store.ClaimNextTask(ctx, "hub-sync-claim-worker", time.Minute); err != nil || found {
		t.Fatalf("old cloud-synced operation woke a Task with a newer pending write = %#v, found=%t, err=%v; want no claim",
			claimed, found, err)
	}
}
