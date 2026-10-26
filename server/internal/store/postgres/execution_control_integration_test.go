package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

func TestPostgresExecutionControlStopsTasksAndReenablesWithoutRevival(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	scheduled, _ := createPostgresSchedule(t, ctx, store, owner.ID, hostValue.ID,
		schedule.Timing{Kind: schedule.TimingDaily, StartsOn: time.Now().UTC().Format("2006-01-02"), LocalTime: "23:59", TimeZone: "UTC"},
		schedule.MisfireSkip, []schedule.NotificationEvent{schedule.NotifyCompleted})

	initial, err := store.GetExecutionControl(ctx, owner.ID)
	if err != nil || initial.State != execution.StateEnabled || initial.Epoch != 1 {
		t.Fatalf("initial owner execution control = %#v, err=%v", initial, err)
	}

	queued := newTestTaskSubmission(t, owner.ID, hostValue.ID, "execution-stop-queued", "queued task")
	queuedTask, err := store.SubmitTask(ctx, queued)
	if err != nil {
		t.Fatal(err)
	}
	running := newTestTaskSubmission(t, owner.ID, hostValue.ID, "execution-stop-running", "running task")
	runningTask, err := store.SubmitTask(ctx, running)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='running', lease_owner='execution-test-worker', lease_version=3,
lease_expires_at=clock_timestamp()-interval '1 second', runtime_lease_safe_after=clock_timestamp()-interval '1 second'
WHERE id=$1`, runningTask.Task.ID); err != nil {
		t.Fatal(err)
	}

	stopRecord := executionControlIdempotency(owner.ID, "execution.stop_all", "execution-stop-all")
	stopping, err := store.StopAllExecution(ctx, stopRecord, initial.Epoch)
	if err != nil {
		t.Fatalf("stop all execution: %v", err)
	}
	if stopping.State != execution.StateStopping || stopping.Epoch != 2 || stopping.PendingTaskCount != 2 {
		t.Fatalf("stop-all control = %#v, want stopping epoch 2 and two tasks", stopping)
	}
	var scheduleStatus, nextKey string
	var scheduleVersion int64
	var nextCheck pgtype.Timestamptz
	if err := store.pool.QueryRow(ctx, `SELECT status,version,next_occurrence_key,next_check_at FROM schedules WHERE id=$1`, scheduled.ID).
		Scan(&scheduleStatus, &scheduleVersion, &nextKey, &nextCheck); err != nil {
		t.Fatal(err)
	}
	if scheduleStatus != string(schedule.StatusDisabled) || scheduleVersion != scheduled.Version+1 || nextKey != "" || nextCheck.Valid {
		t.Fatalf("schedule after stop-all = %s version %d cursor %q check %#v", scheduleStatus, scheduleVersion, nextKey, nextCheck)
	}

	for _, id := range []string{queuedTask.Task.ID, runningTask.Task.ID} {
		var status string
		var leaseOwner string
		if err := store.pool.QueryRow(ctx, `SELECT status, lease_owner FROM tasks WHERE id=$1`, id).Scan(&status, &leaseOwner); err != nil {
			t.Fatal(err)
		}
		if status != string(task.StatusCancelling) || leaseOwner != "" {
			t.Fatalf("task %s after stop-all = status %s owner %q", id, status, leaseOwner)
		}
	}
	if _, found, err := store.ClaimNextTask(ctx, "execution-test-claim", time.Minute); err != nil || found {
		t.Fatalf("claim during owner stop = found %t, err=%v", found, err)
	}
	if _, err := store.RenewTaskLease(ctx, runningTask.Task.ID, "execution-test-worker", 3, time.Minute); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("renew lease after global fence error = %v", err)
	}
	blocked := newTestTaskSubmission(t, owner.ID, hostValue.ID, "execution-submit-after-stop", "must not queue")
	if _, err := store.SubmitTask(ctx, blocked); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("submit while stop is draining error = %v", err)
	}

	for i := 0; i < 2; i++ {
		claim, found, err := store.ClaimNextTaskStop(ctx, "execution-stop-worker", time.Minute)
		if err != nil || !found {
			t.Fatalf("claim task stop = found %t, err=%v", found, err)
		}
		if len(claim.Operations) != 0 {
			t.Fatalf("unexpected operations on task %s: %#v", claim.Task.ID, claim.Operations)
		}
		if _, err := store.CompleteTaskStop(ctx, claim.Task.ID, claim.Request.ID, claim.Request.LeaseOwner, claim.Request.LeaseVersion); err != nil {
			t.Fatalf("complete task stop %s: %v", claim.Task.ID, err)
		}
	}
	if err := store.ReconcileStoppingExecution(ctx); err != nil {
		t.Fatalf("reconcile owner execution gate: %v", err)
	}
	disabled, err := store.GetExecutionControl(ctx, owner.ID)
	if err != nil || disabled.State != execution.StateDisabled || disabled.Epoch != 3 || disabled.PendingTaskCount != 0 {
		t.Fatalf("disabled control = %#v, err=%v", disabled, err)
	}

	enabled, err := store.EnableExecution(ctx, executionControlIdempotency(owner.ID, "execution.enable", "execution-enable"), disabled.Epoch)
	if err != nil || enabled.State != execution.StateEnabled || enabled.Epoch != 4 {
		t.Fatalf("enabled control = %#v, err=%v", enabled, err)
	}
	stillDisabled, err := store.GetSchedule(ctx, owner.ID, scheduled.ID)
	if err != nil || stillDisabled.Status != schedule.StatusDisabled || stillDisabled.NextCheckAt != nil {
		t.Fatalf("schedule revived after owner gate enable = %#v, err=%v", stillDisabled, err)
	}
	for _, id := range []string{queuedTask.Task.ID, runningTask.Task.ID} {
		var status string
		if err := store.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, id).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != string(task.StatusCancelled) {
			t.Fatalf("task %s revived after enable: status %s", id, status)
		}
	}

	replayed, err := store.StopAllExecution(ctx, stopRecord, initial.Epoch)
	if err != nil || replayed.State != execution.StateStopping || replayed.Epoch != 2 || replayed.PendingTaskCount != 2 {
		t.Fatalf("stop-all replay = %#v, err=%v", replayed, err)
	}
	if _, err := store.StopAllExecution(ctx, executionControlIdempotency(owner.ID, "execution.stop_all", "stale-execution-stop"), initial.Epoch); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale expected epoch error = %v", err)
	}
}

func TestPostgresExecutionStopBarrierSerializesAgainstTaskSubmission(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)

	var group sync.WaitGroup
	group.Add(2)
	start := make(chan struct{})
	var submitErr, stopErr error
	var submitted app.TaskSubmissionResult
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "execution-race-submit", "race submission")
	go func() {
		defer group.Done()
		<-start
		submitted, submitErr = store.SubmitTask(ctx, submission)
	}()
	go func() {
		defer group.Done()
		<-start
		_, stopErr = store.StopAllExecution(ctx, executionControlIdempotency(owner.ID, "execution.stop_all", "execution-race-stop"), 1)
	}()
	close(start)
	group.Wait()
	if stopErr != nil {
		t.Fatalf("stop-all race: %v", stopErr)
	}
	if submitErr != nil && domain.CodeOf(submitErr) != domain.CodeConflict {
		t.Fatalf("submission race returned unexpected error: %v", submitErr)
	}
	if submitErr == nil {
		var status string
		if err := store.pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, submitted.Task.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != string(task.StatusCancelling) && status != string(task.StatusCancelled) {
			t.Fatalf("task submitted before stop barrier escaped cancellation: status %s", status)
		}
	}
	var queuedCount int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE owner_id=$1 AND status IN ('queued','running')`, owner.ID).Scan(&queuedCount); err != nil {
		t.Fatal(err)
	}
	if queuedCount != 0 {
		t.Fatalf("queued or running tasks after stop barrier = %d", queuedCount)
	}
}

func TestPostgresExecutionGateFencesOperationDispatch(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id='ri_execution_gate_test' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "execution-dispatch-submit", "dispatch fence"))
	if err != nil {
		t.Fatal(err)
	}
	running, found, err := store.ClaimNextTask(ctx, "execution-dispatch-worker", time.Minute)
	if err != nil || !found || running.ID != submitted.Task.ID {
		t.Fatalf("claim task = %#v, found=%t, err=%v", running, found, err)
	}
	operation, replayed, err := store.CreateOperation(ctx, task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: running.ID, Key: "browser.click", RequestHash: hashBytes("exact-click"),
		HostID: hostValue.ID, RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_execution_gate_test",
		ControlVersion: 0, LeaseVersion: running.Lease.Version, RuntimeLeaseVersion: running.Lease.Version, Status: task.OperationPrepared,
	}, "execution-dispatch-worker")
	if err != nil || replayed || operation.Status != task.OperationPrepared {
		t.Fatalf("create prepared operation = %#v replayed=%t err=%v", operation, replayed, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second',
runtime_lease_safe_after=clock_timestamp()-interval '1 second' WHERE id=$1`, running.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StopAllExecution(ctx, executionControlIdempotency(owner.ID, "execution.stop_all", "execution-dispatch-stop"), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: running.ID, OperationKey: operation.Key, LeaseOwner: "execution-dispatch-worker", LeaseVersion: running.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_execution_gate_test", ControlVersion: 0,
		RuntimeLeaseVersion: running.Lease.Version, Status: task.OperationDispatched,
	}); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("dispatch after stop barrier error = %v", err)
	}

	claim, found, err := store.ClaimNextTaskStop(ctx, "execution-stop-worker", time.Minute)
	if err != nil || !found || len(claim.Operations) != 1 || claim.Operations[0].ID != operation.ID {
		t.Fatalf("claim stop with prepared operation = %#v, found=%t, err=%v", claim, found, err)
	}
	if err := store.SettleTaskStopOperation(ctx, claim.Task.ID, claim.Request.ID, claim.Request.LeaseOwner, claim.Request.LeaseVersion,
		operation.ID, task.OperationCancelled, "prepared operation was never dispatched", false); err != nil {
		t.Fatalf("settle prepared operation on stop: %v", err)
	}
	if _, err := store.CompleteTaskStop(ctx, claim.Task.ID, claim.Request.ID, claim.Request.LeaseOwner, claim.Request.LeaseVersion); err != nil {
		t.Fatalf("complete operation stop: %v", err)
	}
	if err := store.ReconcileStoppingExecution(ctx); err != nil {
		t.Fatal(err)
	}
	control, err := store.GetExecutionControl(ctx, owner.ID)
	if err != nil || control.State != execution.StateDisabled {
		t.Fatalf("execution control after operation drain = %#v, err=%v", control, err)
	}
}

func TestPostgresOwnerStopWaitsForMCPRuntimeCancellation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	ensureMCPTestAPIKey(t, ctx, store, owner.ID, "t1234567")
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	const runtimeInstanceID = "execution-mcp-stop-test-instance"
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_generation=1, runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, runtimeInstanceID); err != nil {
		t.Fatal(err)
	}
	fence := app.AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: runtimeInstanceID, ControlVersion: 0, AIAllowed: true}
	key := app.MCPRuntimeOperationKey{OwnerID: owner.ID, APIKeyID: "t1234567", SessionID: "execution-mcp-stop-session", HostID: hostValue.ID, IdempotencyKey: "execution-mcp-stop-call"}
	operation, _, err := store.BeginMCPRuntimeOperation(ctx, newMCPRuntimeBeginRequest(t, key, fence, 1))
	if err != nil {
		t.Fatal(err)
	}
	operation, err = store.MarkMCPRuntimeOperationDispatched(ctx, key, operation.RuntimeTaskID, fence, operation.LeaseVersion)
	if err != nil || operation.Status != task.OperationDispatched {
		t.Fatalf("dispatch MCP operation = %#v, err=%v", operation, err)
	}

	stopping, err := store.StopAllExecution(ctx, executionControlIdempotency(owner.ID, "execution.stop_all", "execution-mcp-stop-all"), 1)
	if err != nil || stopping.State != execution.StateStopping || stopping.Epoch != 2 || stopping.PendingTaskCount != 1 {
		t.Fatalf("owner stop with active MCP operation = %#v, err=%v", stopping, err)
	}
	pending, err := store.ListMCPRuntimeOperationsPendingCancellation(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].OperationID != operation.OperationID {
		t.Fatalf("durable MCP cancellation request = %#v, err=%v", pending, err)
	}
	if err := store.ReconcileStoppingExecution(ctx); err != nil {
		t.Fatal(err)
	}
	stillStopping, err := store.GetExecutionControl(ctx, owner.ID)
	if err != nil || stillStopping.State != execution.StateStopping {
		t.Fatalf("Owner gate closed before MCP cancellation proof: %#v, err=%v", stillStopping, err)
	}
	if _, err := store.RenewMCPRuntimeOperationLease(ctx, key, operation.RuntimeTaskID, fence, 1, operation.LeaseVersion, time.Minute); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("MCP operation renew after owner stop error = %v", err)
	}

	if _, err := store.SettleMCPRuntimeOperation(ctx, key, operation.RuntimeTaskID, task.OperationCancelled, mcpRuntimeCancelledReceipt(t, operation)); err != nil {
		t.Fatalf("settle confirmed MCP cancellation: %v", err)
	}
	if err := store.ReconcileStoppingExecution(ctx); err != nil {
		t.Fatal(err)
	}
	disabled, err := store.GetExecutionControl(ctx, owner.ID)
	if err != nil || disabled.State != execution.StateDisabled || disabled.Epoch != 3 || disabled.PendingTaskCount != 0 {
		t.Fatalf("Owner gate after confirmed MCP cancellation = %#v, err=%v", disabled, err)
	}
}

func executionControlIdempotency(ownerID, operation, key string) app.IdempotencyRecord {
	return app.IdempotencyRecord{
		OwnerID: ownerID, Operation: operation, Key: key, RequestHash: hashBytes(operation + ":" + key),
		ResourceID: ownerID, RelatedID: "pending",
	}
}

func TestPostgresMissingExecutionGateFailsClosed(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	if _, err := store.pool.Exec(ctx, `DELETE FROM owner_execution_controls WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetExecutionControl(ctx, owner.ID); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("missing owner gate read error = %v", err)
	}
	if _, err := store.StopAllExecution(ctx, executionControlIdempotency(owner.ID, "execution.stop_all", "missing-gate-stop"), 1); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("missing owner gate stop error = %v", err)
	}
}
