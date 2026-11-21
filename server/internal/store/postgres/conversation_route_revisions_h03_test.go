package postgres

import (
	"context"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestConversationRevisionCancelsApprovalOnPausedTask(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	initial := f.receive(t, "h03-revision-initial", "Create a report", app.ConversationRoutingInput{})
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
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET status='paused' WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	pendingID := mustID(t, domain.NewPendingRequestID)
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_pending_requests
(id,task_id,version,kind,title,object,content,impact,call_id,tool_name,request_hash,runtime_generation,runtime_instance_id,control_version,status)
VALUES ($1,$2,1,'approval','Approve the old action','Workspace','Write the report','External change',$3,'morde_workspace_file_write',$4,0,'test-runtime',0,'pending')`,
		pendingID, taskID, "h03-revision-call", hashBytes("h03-revision-approval")); err != nil {
		t.Fatal(err)
	}
	update := f.receive(t, "h03-revision-update", "Change the report format", app.ConversationRoutingInput{ReferenceTaskID: taskID})
	snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, update)
	if err != nil || len(snapshot.Context.Candidates) != 1 || snapshot.Context.Candidates[0].TaskID != taskID {
		t.Fatalf("paused revision target = %+v, err=%v", snapshot.Context.Candidates, err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, update, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteUpdate,
		TaskID: taskID, ExpectedRevision: snapshot.Context.Candidates[0].Revision}); err != nil {
		t.Fatal(err)
	}
	var status, decision string
	if err := f.store.pool.QueryRow(ctx, `SELECT status,decision FROM task_pending_requests WHERE id=$1 AND task_id=$2`, pendingID, taskID).
		Scan(&status, &decision); err != nil || status != string(task.PendingRequestCancelled) || decision != "" {
		t.Fatalf("old approval after paused Task revision = %q/%q, err=%v", status, decision, err)
	}
}
