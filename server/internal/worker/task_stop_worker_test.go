package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

type taskStopStoreStub struct {
	mu sync.Mutex

	claim       func(context.Context, string, time.Duration) (app.TaskStopClaim, bool, error)
	renew       func(context.Context, string, string, string, int64, time.Duration) error
	settle      func(context.Context, string, string, string, int64, string, task.OperationStatus, string, bool) error
	complete    func(context.Context, string, string, string, int64) (task.Task, error)
	release     func(context.Context, string, string, string, int64, time.Duration) error
	reconcile   func(context.Context) error
	claims      int
	renews      int
	settlements []task.OperationStatus
	safeAfter   []bool
	completes   int
	releases    int
	claimCalls  chan struct{}
	releaseCall chan struct{}
}

func (s *taskStopStoreStub) ReconcileStoppingExecution(ctx context.Context) error {
	if s.reconcile != nil {
		return s.reconcile(ctx)
	}
	return nil
}

func (s *taskStopStoreStub) ClaimNextTaskStop(ctx context.Context, workerID string, lease time.Duration) (app.TaskStopClaim, bool, error) {
	s.mu.Lock()
	s.claims++
	s.mu.Unlock()
	if s.claimCalls != nil {
		select {
		case s.claimCalls <- struct{}{}:
		default:
		}
	}
	if s.claim != nil {
		return s.claim(ctx, workerID, lease)
	}
	return app.TaskStopClaim{}, false, nil
}

func (s *taskStopStoreStub) RenewTaskStopClaim(ctx context.Context, taskID, requestID, owner string, version int64, lease time.Duration) error {
	s.mu.Lock()
	s.renews++
	s.mu.Unlock()
	if s.renew != nil {
		return s.renew(ctx, taskID, requestID, owner, version, lease)
	}
	return nil
}

func (s *taskStopStoreStub) SettleTaskStopOperation(ctx context.Context, taskID, requestID, owner string, version int64, operationID string, status task.OperationStatus, result string, requireSafeAfter bool) error {
	s.mu.Lock()
	s.settlements = append(s.settlements, status)
	s.safeAfter = append(s.safeAfter, requireSafeAfter)
	s.mu.Unlock()
	if s.settle != nil {
		return s.settle(ctx, taskID, requestID, owner, version, operationID, status, result, requireSafeAfter)
	}
	return nil
}

func (s *taskStopStoreStub) CompleteTaskStop(ctx context.Context, taskID, requestID, owner string, version int64) (task.Task, error) {
	s.mu.Lock()
	s.completes++
	s.mu.Unlock()
	if s.complete != nil {
		return s.complete(ctx, taskID, requestID, owner, version)
	}
	return task.Task{ID: taskID}, nil
}

func (s *taskStopStoreStub) ReleaseTaskStopClaim(ctx context.Context, taskID, requestID, owner string, version int64, retryAfter time.Duration) error {
	s.mu.Lock()
	s.releases++
	s.mu.Unlock()
	if s.releaseCall != nil {
		select {
		case s.releaseCall <- struct{}{}:
		default:
		}
	}
	if s.release != nil {
		return s.release(ctx, taskID, requestID, owner, version, retryAfter)
	}
	return nil
}

func (s *taskStopStoreStub) counts() (claims, renews, settlements, completes, releases int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims, s.renews, len(s.settlements), s.completes, s.releases
}

func (s *taskStopStoreStub) settledStatuses() []task.OperationStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]task.OperationStatus(nil), s.settlements...)
}

func (s *taskStopStoreStub) safeAfterRequirements() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.safeAfter...)
}

type taskStopExecutorFunc func(context.Context, task.Task, task.Operation) (task.OperationStatus, string, bool, error)

func (f taskStopExecutorFunc) StopTaskOperation(ctx context.Context, taskValue task.Task, operation task.Operation) (task.OperationStatus, string, bool, error) {
	return f(ctx, taskValue, operation)
}

type mcpRuntimeStopReconcilerFunc func(context.Context) error

func (f mcpRuntimeStopReconcilerFunc) ReconcileMCPRuntimeCancellations(ctx context.Context) error {
	return f(ctx)
}

func newTestTaskStopWorker(t *testing.T, store TaskStopStore, executor TaskStopOperationExecutor, lease, poll, timeout time.Duration) *TaskStopWorker {
	t.Helper()
	worker, err := NewTaskStopWorker(TaskStopWorkerOptions{
		Store: store, Executor: executor, WorkerID: "worker-stop-test",
		LeaseDuration: lease, PollInterval: poll, OperationTimeout: timeout,
	})
	if err != nil {
		t.Fatalf("NewTaskStopWorker() error = %v", err)
	}
	return worker
}

func validTaskStopClaim(workerID string, safeAfter time.Time, operations ...task.Operation) app.TaskStopClaim {
	expiresAt := time.Now().Add(time.Minute)
	return app.TaskStopClaim{
		Task: task.Task{ID: "task-stop-1", Status: task.StatusCancelling},
		Request: task.StopRequest{
			ID: "stop-request-1", TaskID: "task-stop-1", Action: task.ActionCancel,
			Status: task.StopRequestPending, SafeAfter: safeAfter,
			LeaseOwner: workerID, LeaseVersion: 2, LeaseExpiresAt: &expiresAt,
		},
		Operations: operations,
	}
}

func waitTaskStopSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for task stop worker signal")
	}
}

func waitTaskStopCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for task stop worker condition")
}

func startTaskStopWorker(ctx context.Context, worker *TaskStopWorker) <-chan error {
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	return done
}

func TestTaskStopWorkerSettlesPreparedOperationAndCompletes(t *testing.T) {
	store := &taskStopStoreStub{claimCalls: make(chan struct{}, 4)}
	store.claim = func(_ context.Context, workerID string, _ time.Duration) (app.TaskStopClaim, bool, error) {
		claims, _, _, _, _ := store.counts()
		if claims == 1 {
			return validTaskStopClaim(workerID, time.Now().Add(-time.Second), task.Operation{
				ID: "prepared-operation", TaskID: "task-stop-1", Status: task.OperationPrepared,
			}), true, nil
		}
		return app.TaskStopClaim{}, false, nil
	}
	executorCalls := 0
	worker := newTestTaskStopWorker(t, store, taskStopExecutorFunc(func(context.Context, task.Task, task.Operation) (task.OperationStatus, string, bool, error) {
		executorCalls++
		return "", "", false, errors.New("prepared operation must not reach the runtime")
	}), time.Second, 5*time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskStopWorker(ctx, worker)
	waitTaskStopCondition(t, func() bool {
		_, _, settlements, completes, _ := store.counts()
		return settlements == 1 && completes == 1
	})
	if statuses := store.settledStatuses(); len(statuses) != 1 || statuses[0] != task.OperationCancelled {
		t.Fatalf("settled statuses = %v, want [cancelled]", statuses)
	}
	if requirements := store.safeAfterRequirements(); len(requirements) != 1 || requirements[0] {
		t.Fatalf("safe-after requirements = %v, want [false] for prepared operation", requirements)
	}
	if executorCalls != 0 {
		t.Fatalf("executor calls = %d, want 0 for a prepared operation", executorCalls)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestTaskStopWorkerLeavesRemoteCancellingOperationUnsettled(t *testing.T) {
	store := &taskStopStoreStub{releaseCall: make(chan struct{}, 2)}
	store.claim = func(_ context.Context, workerID string, _ time.Duration) (app.TaskStopClaim, bool, error) {
		claims, _, _, _, _ := store.counts()
		if claims == 1 {
			return validTaskStopClaim(workerID, time.Now().Add(-time.Second), task.Operation{
				ID: "remote-operation", TaskID: "task-stop-1", Status: task.OperationDispatched,
			}), true, nil
		}
		return app.TaskStopClaim{}, false, nil
	}
	worker := newTestTaskStopWorker(t, store, taskStopExecutorFunc(func(context.Context, task.Task, task.Operation) (task.OperationStatus, string, bool, error) {
		return task.OperationStatus("cancelling"), "still stopping", false, nil
	}), time.Second, 5*time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskStopWorker(ctx, worker)
	waitTaskStopSignal(t, store.releaseCall)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	_, _, settlements, completes, releases := store.counts()
	if settlements != 0 || completes != 0 || releases != 1 {
		t.Fatalf("settlements/completes/releases = %d/%d/%d, want 0/0/1", settlements, completes, releases)
	}
}

func TestTaskStopWorkerNotFoundWaitsForSafeAfter(t *testing.T) {
	store := &taskStopStoreStub{releaseCall: make(chan struct{}, 2)}
	store.settle = func(_ context.Context, _, _, _ string, _ int64, _ string, _ task.OperationStatus, _ string, requireSafeAfter bool) error {
		if requireSafeAfter {
			return errors.New("database safe-after boundary has not passed")
		}
		return nil
	}
	store.claim = func(_ context.Context, workerID string, _ time.Duration) (app.TaskStopClaim, bool, error) {
		claims, _, _, _, _ := store.counts()
		if claims == 1 {
			claim := validTaskStopClaim(workerID, time.Now().Add(time.Minute), task.Operation{
				ID: "missing-operation", TaskID: "task-stop-1", Status: task.OperationUnknown,
			})
			// The worker clock may lag the database. Renewal and completion must
			// rely on the store's database-clock lease and safe-after checks.
			expiredOnWorkerClock := time.Now().Add(-time.Minute)
			claim.Request.LeaseExpiresAt = &expiredOnWorkerClock
			return claim, true, nil
		}
		return app.TaskStopClaim{}, false, nil
	}
	worker := newTestTaskStopWorker(t, store, taskStopExecutorFunc(func(context.Context, task.Task, task.Operation) (task.OperationStatus, string, bool, error) {
		return "", "operation not found", true, nil
	}), time.Second, 5*time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskStopWorker(ctx, worker)
	waitTaskStopSignal(t, store.releaseCall)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	_, _, settlements, completes, releases := store.counts()
	if settlements != 1 || completes != 0 || releases != 1 {
		t.Fatalf("settlement attempts/completes/releases = %d/%d/%d, want 1/0/1 before database safe-after", settlements, completes, releases)
	}
	if requirements := store.safeAfterRequirements(); len(requirements) != 1 || !requirements[0] {
		t.Fatalf("safe-after requirements = %v, want [true] for not-found proof", requirements)
	}
}

func TestTaskStopWorkerRuntimeTerminalProofDoesNotRequireSafeAfter(t *testing.T) {
	store := &taskStopStoreStub{}
	store.claim = func(_ context.Context, workerID string, _ time.Duration) (app.TaskStopClaim, bool, error) {
		claims, _, _, _, _ := store.counts()
		if claims == 1 {
			// The database clock can be ahead of the worker clock; the store's
			// completion method represents its authoritative safe-after check.
			return validTaskStopClaim(workerID, time.Now().Add(time.Minute), task.Operation{
				ID: "runtime-operation", TaskID: "task-stop-1", Status: task.OperationDispatched,
			}), true, nil
		}
		return app.TaskStopClaim{}, false, nil
	}
	worker := newTestTaskStopWorker(t, store, taskStopExecutorFunc(func(context.Context, task.Task, task.Operation) (task.OperationStatus, string, bool, error) {
		return task.OperationSucceeded, "runtime confirmed completion", false, nil
	}), time.Second, 5*time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskStopWorker(ctx, worker)
	waitTaskStopCondition(t, func() bool {
		_, _, settlements, completes, _ := store.counts()
		return settlements == 1 && completes == 1
	})
	if statuses := store.settledStatuses(); len(statuses) != 1 || statuses[0] != task.OperationSucceeded {
		t.Fatalf("settled statuses = %v, want [succeeded]", statuses)
	}
	if requirements := store.safeAfterRequirements(); len(requirements) != 1 || requirements[0] {
		t.Fatalf("safe-after requirements = %v, want [false] for terminal runtime proof", requirements)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestTaskStopWorkerSettlesNotFoundAfterSafeAfter(t *testing.T) {
	store := &taskStopStoreStub{}
	store.claim = func(_ context.Context, workerID string, _ time.Duration) (app.TaskStopClaim, bool, error) {
		claims, _, _, _, _ := store.counts()
		if claims == 1 {
			return validTaskStopClaim(workerID, time.Now().Add(-time.Second), task.Operation{
				ID: "missing-operation", TaskID: "task-stop-1", Status: task.OperationUnknown,
			}), true, nil
		}
		return app.TaskStopClaim{}, false, nil
	}
	worker := newTestTaskStopWorker(t, store, taskStopExecutorFunc(func(context.Context, task.Task, task.Operation) (task.OperationStatus, string, bool, error) {
		return "", "operation not found", true, nil
	}), time.Second, 5*time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskStopWorker(ctx, worker)
	waitTaskStopCondition(t, func() bool {
		_, _, settlements, completes, _ := store.counts()
		return settlements == 1 && completes == 1
	})
	if statuses := store.settledStatuses(); len(statuses) != 1 || statuses[0] != task.OperationCancelled {
		t.Fatalf("settled statuses = %v, want [cancelled]", statuses)
	}
	if requirements := store.safeAfterRequirements(); len(requirements) != 1 || !requirements[0] {
		t.Fatalf("safe-after requirements = %v, want [true] for not-found proof", requirements)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestTaskStopWorkerDoesNotSettleAfterClaimRenewalIsLost(t *testing.T) {
	store := &taskStopStoreStub{}
	store.claim = func(_ context.Context, workerID string, _ time.Duration) (app.TaskStopClaim, bool, error) {
		claims, _, _, _, _ := store.counts()
		if claims == 1 {
			return validTaskStopClaim(workerID, time.Now().Add(-time.Second), task.Operation{
				ID: "remote-operation", TaskID: "task-stop-1", Status: task.OperationDispatched,
			}), true, nil
		}
		return app.TaskStopClaim{}, false, nil
	}
	store.renew = func(_ context.Context, _, _, _ string, _ int64, _ time.Duration) error {
		_, renews, _, _, _ := store.counts()
		if renews >= 2 {
			return errors.New("claim was lost")
		}
		return nil
	}
	executorStarted := make(chan struct{})
	executorCanceled := make(chan struct{})
	worker := newTestTaskStopWorker(t, store, taskStopExecutorFunc(func(ctx context.Context, _ task.Task, _ task.Operation) (task.OperationStatus, string, bool, error) {
		close(executorStarted)
		<-ctx.Done()
		close(executorCanceled)
		return task.OperationSucceeded, "late result", false, nil
	}), 45*time.Millisecond, 5*time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskStopWorker(ctx, worker)
	waitTaskStopSignal(t, executorStarted)
	waitTaskStopSignal(t, executorCanceled)
	waitTaskStopCondition(t, func() bool {
		_, _, _, _, releases := store.counts()
		return releases == 1
	})
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	_, _, settlements, completes, _ := store.counts()
	if settlements != 0 || completes != 0 {
		t.Fatalf("settlements/completes = %d/%d, want 0/0 after lease loss", settlements, completes)
	}
}

func TestTaskStopWorkerRunStopsOnCancellation(t *testing.T) {
	store := &taskStopStoreStub{claimCalls: make(chan struct{}, 4)}
	worker := newTestTaskStopWorker(t, store, taskStopExecutorFunc(func(context.Context, task.Task, task.Operation) (task.OperationStatus, string, bool, error) {
		t.Fatal("executor called without a task stop claim")
		return "", "", false, nil
	}), time.Second, 5*time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskStopWorker(ctx, worker)
	waitTaskStopSignal(t, store.claimCalls)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestTaskStopWorkerCancellationStopsInFlightExecutor(t *testing.T) {
	store := &taskStopStoreStub{
		claimCalls:  make(chan struct{}, 4),
		releaseCall: make(chan struct{}, 2),
	}
	store.claim = func(_ context.Context, workerID string, _ time.Duration) (app.TaskStopClaim, bool, error) {
		claims, _, _, _, _ := store.counts()
		if claims == 1 {
			return validTaskStopClaim(workerID, time.Now().Add(time.Minute), task.Operation{
				ID: "remote-operation", TaskID: "task-stop-1", Status: task.OperationDispatched,
			}), true, nil
		}
		return app.TaskStopClaim{}, false, nil
	}
	executorStarted := make(chan struct{})
	executorCanceled := make(chan struct{})
	worker := newTestTaskStopWorker(t, store, taskStopExecutorFunc(func(ctx context.Context, _ task.Task, _ task.Operation) (task.OperationStatus, string, bool, error) {
		close(executorStarted)
		<-ctx.Done()
		close(executorCanceled)
		return task.OperationCancelled, "", false, nil
	}), time.Second, 5*time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskStopWorker(ctx, worker)
	waitTaskStopSignal(t, executorStarted)
	cancel()
	waitTaskStopSignal(t, executorCanceled)
	waitTaskStopSignal(t, store.releaseCall)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	_, _, settlements, completes, releases := store.counts()
	if settlements != 0 || completes != 0 || releases != 1 {
		t.Fatalf("settlements/completes/releases = %d/%d/%d, want 0/0/1 on shutdown", settlements, completes, releases)
	}
}

func TestTaskStopWorkerReconcilesMCPBeforeOwnerGate(t *testing.T) {
	var order []string
	store := &taskStopStoreStub{
		reconcile: func(context.Context) error {
			order = append(order, "owner-gate")
			return nil
		},
	}
	worker, err := NewTaskStopWorker(TaskStopWorkerOptions{
		Store: store,
		Executor: taskStopExecutorFunc(func(context.Context, task.Task, task.Operation) (task.OperationStatus, string, bool, error) {
			t.Fatal("task executor should not run during execution-stop reconciliation")
			return "", "", false, nil
		}),
		WorkerID: "worker-stop-order-test", LeaseDuration: time.Second,
		PollInterval: time.Second, OperationTimeout: time.Second,
		MCPRuntimeStopper: mcpRuntimeStopReconcilerFunc(func(context.Context) error {
			order = append(order, "mcp-runtime")
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.reconcileExecutionStops(context.Background()); err != nil {
		t.Fatalf("reconcile execution stops: %v", err)
	}
	if len(order) != 2 || order[0] != "mcp-runtime" || order[1] != "owner-gate" {
		t.Fatalf("stop reconciliation order = %v, want [mcp-runtime owner-gate]", order)
	}
}
