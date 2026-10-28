package postgres

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
	"morde/server/internal/worker"
)

type retiredTaskExecutor struct{}

func (retiredTaskExecutor) StopTaskOperation(context.Context, task.Task, task.Operation) (task.OperationStatus, string, bool, error) {
	return task.OperationCancelled, "", false, nil
}

func TestRetireTaskExecutionStopsExistingTasksAndSchedulesIdempotently(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 20_000, 40_000)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	computer := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	plan, _ := createPostgresSchedule(t, ctx, store, owner.ID, computer.ID,
		schedule.Timing{Kind: schedule.TimingDaily, StartsOn: time.Now().UTC().Format("2006-01-02"), LocalTime: "23:59", TimeZone: "UTC"},
		schedule.MisfireSkip, nil)
	queued, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, computer.ID, "retire-queued", "queued task"))
	if err != nil {
		t.Fatal(err)
	}
	running, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, computer.ID, "retire-running", "running task"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='running', lease_owner='retire-worker', lease_version=3,
lease_expires_at=clock_timestamp()+interval '1 minute', runtime_lease_safe_after=clock_timestamp()+interval '1 minute'
WHERE id=$1`, running.Task.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RetireTaskExecution(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := store.GetExecutionControl(ctx, owner.ID)
	if err != nil || first.State != execution.StateStopping || first.PendingTaskCount != 2 {
		t.Fatalf("retired owner gate = %#v, err=%v", first, err)
	}
	planAfter, err := store.GetSchedule(ctx, owner.ID, plan.ID)
	if err != nil || planAfter.Status != schedule.StatusDisabled || planAfter.NextCheckAt != nil {
		t.Fatalf("retired schedule = %#v, err=%v", planAfter, err)
	}
	for _, id := range []string{queued.Task.ID, running.Task.ID} {
		var status string
		var pending int
		if err := pool.QueryRow(ctx, `SELECT status, (SELECT count(*) FROM task_stop_requests
WHERE task_id=$1 AND action='cancel' AND status='pending') FROM tasks WHERE id=$1`, id).Scan(&status, &pending); err != nil {
			t.Fatal(err)
		}
		if status != string(task.StatusCancelling) || pending != 1 {
			t.Fatalf("task %s after retirement: status=%s pending=%d", id, status, pending)
		}
	}
	if err := store.RetireTaskExecution(ctx); err != nil {
		t.Fatalf("idempotent retirement: %v", err)
	}
	second, err := store.GetExecutionControl(ctx, owner.ID)
	if err != nil || second.State != execution.StateStopping || second.Epoch != first.Epoch || second.PendingTaskCount != first.PendingTaskCount {
		t.Fatalf("gate after repeated retirement = %#v, err=%v", second, err)
	}
	if _, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, computer.ID, "retire-blocked", "blocked task")); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("submit after retirement = %v, want conflict", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE task_id IN ($1,$2)`, queued.Task.ID, running.Task.ID); err != nil {
		t.Fatal(err)
	}
	stopWorker, err := worker.NewTaskStopWorker(worker.TaskStopWorkerOptions{
		Store: store, Executor: retiredTaskExecutor{}, WorkerID: "retirement-integration",
		LeaseDuration: time.Second, PollInterval: 5 * time.Millisecond,
		OperationTimeout: time.Second, RetryAfter: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	stopCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- stopWorker.Run(stopCtx) }()
	deadline := time.Now().Add(5 * time.Second)
	drained := false
	for time.Now().Before(deadline) {
		var activeTasks, pendingStops int
		err := pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM tasks WHERE owner_id=$1 AND status IN ('queued','running','pausing','cancelling')),
(SELECT count(*) FROM task_stop_requests sr JOIN tasks t ON t.id=sr.task_id WHERE t.owner_id=$1 AND sr.status='pending')`, owner.ID).Scan(&activeTasks, &pendingStops)
		if err != nil {
			cancel()
			t.Fatalf("read retirement progress: %v (worker: %v)", err, <-done)
		}
		gate, err := store.GetExecutionControl(ctx, owner.ID)
		if err != nil {
			cancel()
			t.Fatalf("read execution control: %v (worker: %v)", err, <-done)
		}
		if activeTasks == 0 && pendingStops == 0 && gate.State == execution.StateDisabled {
			drained = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stop worker = %v, want context cancellation", err)
	}
	if !drained {
		t.Fatal("retired tasks, stop requests, and owner gate did not drain")
	}
}

func TestRetireTaskExecutionWaitsForAccountRuntimeGuardianProof(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	runtimeService, err := app.NewAccountAgentRuntimeService(store)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 19
	runtime, _, err := runtimeService.Start(ctx, app.AccountAgentRuntimeStart{OwnerID: owner.ID,
		RuntimeID: "retire-runtime-proof", LeaseOwner: "retire-runtime-worker", SupervisorNonce: nonce,
		MaxConcurrentTasks: 2, LeaseDuration: 30 * time.Second})
	if err != nil {
		t.Fatalf("start account Runtime: %v", err)
	}
	if _, err := runtimeService.Ready(ctx, runtime.Fence); err != nil {
		t.Fatalf("ready account Runtime: %v", err)
	}
	if err := store.RetireTaskExecution(ctx); err != nil {
		t.Fatalf("retire task execution: %v", err)
	}
	control, err := store.GetExecutionControl(ctx, owner.ID)
	if err != nil || control.State != execution.StateStopping || control.Runtime.State != execution.RuntimeStateStopping {
		t.Fatalf("retirement finalized before Runtime proof: control=%#v err=%v", control, err)
	}
	if err := store.ReconcileStoppingExecution(ctx); err != nil {
		t.Fatal(err)
	}
	control, err = store.GetExecutionControl(ctx, owner.ID)
	if err != nil || control.State != execution.StateStopping || control.Runtime.State != execution.RuntimeStateStopping {
		t.Fatalf("reconcile disabled owner before Runtime proof: control=%#v err=%v", control, err)
	}
}

func TestRetireTaskExecutionFailsClosedOnExhaustedEpoch(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 20_000, 40_000)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled', epoch=$2 WHERE owner_id=$1`, owner.ID, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	if err := store.RetireTaskExecution(ctx); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("retirement with exhausted epoch = %v", err)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM owner_execution_controls WHERE owner_id=$1`, owner.ID).Scan(&state); err != nil || state != "enabled" {
		t.Fatalf("gate changed despite failed retirement: state=%q err=%v", state, err)
	}
}

func TestRetireTaskExecutionRejectsUndrainableStopState(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 20_000, 40_000)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	computer := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
	result, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, computer.ID, "retire-orphan", "orphan stop request"))
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := domain.NewTaskActionID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='cancelled', finished_at=clock_timestamp() WHERE id=$1`, result.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_stop_requests (id, task_id, action, status, safe_after)
VALUES ($1,$2,'cancel','pending',clock_timestamp())`, requestID, result.Task.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RetireTaskExecution(ctx); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("retirement with undrainable stop request = %v", err)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM owner_execution_controls WHERE owner_id=$1`, owner.ID).Scan(&state); err != nil || state != "enabled" {
		t.Fatalf("gate changed despite failed retirement: state=%q err=%v", state, err)
	}
}
