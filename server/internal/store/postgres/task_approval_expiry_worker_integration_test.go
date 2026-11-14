package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/worker"
)

type offlineApprovalExecutor struct {
	claimed chan task.Task
}

func (e *offlineApprovalExecutor) ExecuteTask(ctx context.Context, value task.Task) error {
	select {
	case e.claimed <- value:
	case <-ctx.Done():
		return ctx.Err()
	}
	<-ctx.Done()
	return ctx.Err()
}

func createApprovalExpiryTask(t *testing.T, store *Store, ownerID, hostID, label string, expiresAt *time.Time) (string, string) {
	t.Helper()
	ctx := context.Background()
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, ownerID, hostID, label, "synthetic approval task"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, label+"-prepare", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim approval fixture: task=%s found=%t err=%v", claimed.ID, found, err)
	}
	stepID, pendingID := mustID(t, domain.NewStepID), mustID(t, domain.NewPendingRequestID)
	pending := &task.PendingRequest{ID: pendingID, TaskID: claimed.ID, Version: 1, Kind: "approval", Title: "Approve action",
		Object: "synthetic-object", Content: "Approve this exact action?", Impact: "Synthetic metadata only.",
		CallID: label + "-call", ToolName: "synthetic_tool", RequestHash: hashBytes(label + "-request"),
		RuntimeGeneration: 1, RuntimeInstanceID: "synthetic-runtime", ControlVersion: 1,
		Status: task.PendingRequestPending, ExpiresAt: expiresAt}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "approval_required",
		Usage: task.Usage{ActiveSeconds: 1, ToolCalls: 1},
		Steps: []task.Step{{ID: stepID, TaskID: claimed.ID, Sequence: 1, Kind: "tool", Status: task.StepWaitingUser,
			Title: pending.Title, Summary: pending.Content}},
		Event: task.Event{TaskID: claimed.ID, Type: "task.waiting_user", Payload: []byte(`{"reason":"approval_required"}`)}, PendingRequest: pending,
	}); err != nil {
		t.Fatal(err)
	}
	return claimed.ID, pendingID
}

func makeApprovalExpired(t *testing.T, store *Store, pendingID string) {
	t.Helper()
	if _, err := store.pool.Exec(context.Background(), `UPDATE task_pending_requests SET
created_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second'
WHERE id=$1`, pendingID); err != nil {
		t.Fatalf("set legal past approval times: %v", err)
	}
}

func TestPostgresTaskWorkerExpiresOfflineApproval(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	host := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	future := time.Now().UTC().Add(time.Hour)
	dueTask, dueID := createApprovalExpiryTask(t, store, owner.ID, host.ID, "offline-due", &future)
	futureTask, futureID := createApprovalExpiryTask(t, store, owner.ID, host.ID, "offline-future", &future)
	unknownTask, unknownID := createApprovalExpiryTask(t, store, owner.ID, host.ID, "offline-unknown", nil)
	makeApprovalExpired(t, store, dueID)
	executor := &offlineApprovalExecutor{claimed: make(chan task.Task, 1)}
	w, err := worker.NewTaskWorker(worker.TaskWorkerOptions{Store: store, Executor: executor, WorkerID: "offline-approval-worker",
		LeaseDuration: time.Minute, PollInterval: 10 * time.Millisecond, MaxConcurrentTasks: 1, MaxConcurrentTasksPerOwner: 1})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()
	defer func() { cancel(); <-done }()
	select {
	case claimed := <-executor.claimed:
		if claimed.ID != dueTask {
			t.Fatalf("worker resumed the wrong task: got=%s want=%s", claimed.ID, dueTask)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not expire and resume the approval while the client was offline")
	}
	for _, item := range []struct {
		taskID, pendingID, status, decision string
	}{
		{dueTask, dueID, "expired", "reject"},
		{futureTask, futureID, "pending", ""},
		{unknownTask, unknownID, "pending", ""},
	} {
		var status, decision string
		if err := store.pool.QueryRow(ctx, `SELECT status,decision FROM task_pending_requests WHERE task_id=$1 AND id=$2`,
			item.taskID, item.pendingID).Scan(&status, &decision); err != nil || status != item.status || decision != item.decision {
			t.Fatalf("approval state for task %s: status=%s decision=%s err=%v", item.taskID, status, decision, err)
		}
	}
}

func TestPostgresTaskWorkerExpiresApprovalWhileExecutionSlotBusy(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	host := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	future := time.Now().UTC().Add(time.Hour)
	waitingTask, pendingID := createApprovalExpiryTask(t, store, owner.ID, host.ID, "busy-expiry", &future)
	busy, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, host.ID, "busy-execution", "synthetic long task"))
	if err != nil {
		t.Fatal(err)
	}
	executor := &offlineApprovalExecutor{claimed: make(chan task.Task, 1)}
	w, err := worker.NewTaskWorker(worker.TaskWorkerOptions{Store: store, Executor: executor, WorkerID: "busy-approval-worker",
		LeaseDuration: time.Minute, PollInterval: 10 * time.Millisecond, MaxConcurrentTasks: 1, MaxConcurrentTasksPerOwner: 1})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(runCtx) }()
	defer func() { cancel(); <-done }()
	select {
	case claimed := <-executor.claimed:
		if claimed.ID != busy.Task.ID {
			t.Fatalf("worker claimed unexpected task %s", claimed.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("busy task did not start")
	}
	makeApprovalExpired(t, store, pendingID)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var status, taskStatus string
		if err := store.pool.QueryRow(ctx, `SELECT p.status,t.status FROM task_pending_requests p JOIN tasks t ON t.id=p.task_id
WHERE p.id=$1 AND t.id=$2`, pendingID, waitingTask).Scan(&status, &taskStatus); err != nil {
			t.Fatal(err)
		}
		if status == "expired" && taskStatus == "queued" {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("approval expiry was blocked by a running task: approval=%s task=%s", status, taskStatus)
		case <-ticker.C:
		}
	}
}

func TestPostgresApprovalExpiryRacesResponseAndOtherWorkers(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	host := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	future := time.Now().UTC().Add(time.Hour)
	targetTask, targetID := createApprovalExpiryTask(t, store, owner.ID, host.ID, "race-due", &future)
	otherTask, otherID := createApprovalExpiryTask(t, store, owner.ID, host.ID, "race-other", &future)
	makeApprovalExpired(t, store, targetID)
	if changed, err := store.ExpirePendingRequest(ctx, owner.ID, targetTask, otherID, 1); err != nil || changed {
		t.Fatalf("approval from a different task was accepted: changed=%t err=%v", changed, err)
	}
	if changed, err := store.ExpirePendingRequest(ctx, owner.ID, targetTask, targetID, 2); err != nil || changed {
		t.Fatalf("stale approval version was accepted: changed=%t err=%v", changed, err)
	}
	var initialLease int64
	if err := store.pool.QueryRow(ctx, `SELECT lease_version FROM tasks WHERE id=$1`, targetTask).Scan(&initialLease); err != nil {
		t.Fatal(err)
	}
	record := app.IdempotencyRecord{OwnerID: owner.ID, Operation: "task.pending.respond", Key: "race-expired-approval",
		RequestHash: hashBytes("race-expired:" + targetID), ResourceID: targetTask, RelatedID: targetID}
	type result struct {
		response bool
		changed  bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, 3)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			changed, err := store.ExpireNextPendingRequest(ctx)
			results <- result{changed: changed, err: err}
		}()
	}
	go func() {
		<-start
		err := store.RespondToPendingRequest(ctx, record, targetTask, targetID, 1, task.PendingDecisionApprove)
		results <- result{response: true, err: err}
	}()
	close(start)
	for i := 0; i < 3; i++ {
		result := <-results
		if result.response && domain.CodeOf(result.err) != domain.CodeConflict || !result.response && result.err != nil {
			t.Fatalf("concurrent expiry/response result = %+v", result)
		}
	}
	var status, decision, taskStatus string
	var lease, events int64
	if err := store.pool.QueryRow(ctx, `SELECT p.status,p.decision,t.status,t.lease_version,
(SELECT count(*) FROM task_events e WHERE e.task_id=t.id AND e.event_type='task.response'
 AND e.payload->>'reason'='approval_expired' AND e.payload->>'pending_id'=p.id
 AND e.payload->>'pending_version'='1' AND e.payload->>'decision'='reject')
FROM task_pending_requests p JOIN tasks t ON t.id=p.task_id WHERE p.id=$1 AND t.id=$2`, targetID, targetTask).
		Scan(&status, &decision, &taskStatus, &lease, &events); err != nil || status != "expired" || decision != "reject" ||
		taskStatus != "queued" || lease != initialLease+1 || events != 1 {
		t.Fatalf("expiry did not settle exactly once: request=%s decision=%s task=%s lease=%d events=%d err=%v",
			status, decision, taskStatus, lease, events, err)
	}
	if changed, err := store.ExpireNextPendingRequest(ctx); err != nil || changed {
		t.Fatalf("expiry replay changed another request: changed=%t err=%v", changed, err)
	}
	if err := store.RespondToPendingRequest(ctx, record, targetTask, targetID, 1, task.PendingDecisionApprove); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("expired approval replay was accepted: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT p.status,p.decision,t.status FROM task_pending_requests p JOIN tasks t ON t.id=p.task_id
WHERE p.id=$1 AND t.id=$2`, otherID, otherTask).Scan(&status, &decision, &taskStatus); err != nil ||
		status != "pending" || decision != "" || taskStatus != "waiting_user" {
		t.Fatalf("unrelated approval was altered: request=%s decision=%s task=%s err=%v", status, decision, taskStatus, err)
	}
}

func TestPostgresApprovalResponseBeforeDeadlineSurvivesExpiryScan(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	host := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	future := time.Now().UTC().Add(time.Hour)
	taskID, pendingID := createApprovalExpiryTask(t, store, owner.ID, host.ID, "before-deadline", &future)
	record := app.IdempotencyRecord{OwnerID: owner.ID, Operation: "task.pending.respond", Key: "before-deadline-approval",
		RequestHash: hashBytes("approve-future:" + pendingID), ResourceID: taskID, RelatedID: pendingID}
	start := make(chan struct{})
	response := make(chan error, 1)
	expiry := make(chan error, 1)
	go func() {
		<-start
		response <- store.RespondToPendingRequest(ctx, record, taskID, pendingID, 1, task.PendingDecisionApprove)
	}()
	go func() {
		<-start
		changed, err := store.ExpireNextPendingRequest(ctx)
		if changed && err == nil {
			err = domain.NewError(domain.CodeConflict, "future approval was expired")
		}
		expiry <- err
	}()
	close(start)
	if err := <-response; err != nil {
		t.Fatalf("approval before deadline failed: %v", err)
	}
	if err := <-expiry; err != nil {
		t.Fatal(err)
	}
	if err := store.RespondToPendingRequest(ctx, record, taskID, pendingID, 1, task.PendingDecisionApprove); err != nil {
		t.Fatalf("idempotent approval replay failed: %v", err)
	}
	var status, decision string
	var responseCount int64
	if err := store.pool.QueryRow(ctx, `SELECT p.status,p.decision,
(SELECT count(*) FROM task_events e WHERE e.task_id=p.task_id AND e.event_type='task.response'
 AND e.payload->>'pending_id'=p.id) FROM task_pending_requests p WHERE p.id=$1 AND p.task_id=$2`, pendingID, taskID).
		Scan(&status, &decision, &responseCount); err != nil || status != "approved" || decision != "approve" || responseCount != 1 {
		t.Fatalf("approval changed during scan or replay: status=%s decision=%s events=%d err=%v", status, decision, responseCount, err)
	}
}
