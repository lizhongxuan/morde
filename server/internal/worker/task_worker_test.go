package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain/task"
)

type taskLeaseStoreStub struct {
	claim      func(context.Context, string, time.Duration) (task.Task, bool, error)
	renew      func(context.Context, string, string, int64, time.Duration) (task.Task, error)
	mu         sync.Mutex
	claims     int
	renews     int
	claimCalls chan struct{}
	renewCalls chan struct{}
}

func (s *taskLeaseStoreStub) ClaimNextTask(ctx context.Context, workerID string, lease time.Duration) (task.Task, bool, error) {
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
	return task.Task{}, false, nil
}

func (s *taskLeaseStoreStub) RenewTaskLease(ctx context.Context, taskID, workerID string, version int64, lease time.Duration) (task.Task, error) {
	s.mu.Lock()
	s.renews++
	s.mu.Unlock()
	if s.renewCalls != nil {
		select {
		case s.renewCalls <- struct{}{}:
		default:
		}
	}
	if s.renew != nil {
		return s.renew(ctx, taskID, workerID, version, lease)
	}
	return task.Task{}, errors.New("unexpected renewal")
}

func (s *taskLeaseStoreStub) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims, s.renews
}

type taskExecutorFunc func(context.Context, task.Task) error

func (f taskExecutorFunc) ExecuteTask(ctx context.Context, value task.Task) error {
	return f(ctx, value)
}

func newTestTaskWorker(t *testing.T, store TaskLeaseStore, executor TaskExecutor, lease, poll time.Duration) *TaskWorker {
	t.Helper()
	worker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: executor, WorkerID: "worker-test",
		LeaseDuration: lease, PollInterval: poll,
	})
	if err != nil {
		t.Fatalf("NewTaskWorker() error = %v", err)
	}
	return worker
}

func validTaskClaim(workerID string) task.Task {
	expiresAt := time.Now().Add(time.Minute)
	return task.Task{
		ID: "task-1", Status: task.StatusRunning,
		Lease: task.Lease{Owner: workerID, Version: 1, ExpiresAt: &expiresAt},
	}
}

func startTaskWorker(ctx context.Context, worker *TaskWorker) <-chan error {
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	return done
}

func waitTaskWorkerSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for task worker signal")
	}
}

func waitTaskWorkerResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for task worker result")
		return nil
	}
}

func TestTaskWorkerPollWithoutTasksStopsOnCancellation(t *testing.T) {
	store := &taskLeaseStoreStub{claimCalls: make(chan struct{}, 8)}
	worker := newTestTaskWorker(t, store, taskExecutorFunc(func(context.Context, task.Task) error {
		t.Fatal("executor called without a task claim")
		return nil
	}), time.Second, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskWorker(ctx, worker)
	waitTaskWorkerSignal(t, store.claimCalls)
	waitTaskWorkerSignal(t, store.claimCalls)
	cancel()
	if err := waitTaskWorkerResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	claims, renewals := store.counts()
	if claims < 2 || renewals != 0 {
		t.Fatalf("claim/renew counts = %d/%d, want repeated claims and no renewals", claims, renewals)
	}
}

func TestTaskWorkerRetriesClaimError(t *testing.T) {
	store := &taskLeaseStoreStub{claimCalls: make(chan struct{}, 8)}
	store.claim = func(_ context.Context, _ string, _ time.Duration) (task.Task, bool, error) {
		claims, _ := store.counts()
		if claims == 1 {
			return task.Task{}, false, errors.New("temporary store failure")
		}
		return task.Task{}, false, nil
	}
	worker := newTestTaskWorker(t, store, taskExecutorFunc(func(context.Context, task.Task) error {
		t.Fatal("executor called after claim failure")
		return nil
	}), time.Second, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskWorker(ctx, worker)
	waitTaskWorkerSignal(t, store.claimCalls)
	waitTaskWorkerSignal(t, store.claimCalls)
	cancel()
	if err := waitTaskWorkerResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	claims, renewals := store.counts()
	if claims < 2 || renewals != 0 {
		t.Fatalf("claim/renew counts = %d/%d, want retry and no renewals", claims, renewals)
	}
}

func TestTaskWorkerExecutesClaimedTask(t *testing.T) {
	store := &taskLeaseStoreStub{}
	store.claim = func(_ context.Context, workerID string, _ time.Duration) (task.Task, bool, error) {
		claims, _ := store.counts()
		if claims == 1 {
			return validTaskClaim(workerID), true, nil
		}
		return task.Task{}, false, nil
	}
	got := make(chan task.Task, 1)
	worker := newTestTaskWorker(t, store, taskExecutorFunc(func(_ context.Context, value task.Task) error {
		got <- value
		return nil
	}), time.Second, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskWorker(ctx, worker)
	select {
	case value := <-got:
		if value.ID != "task-1" || value.Lease.Owner != "worker-test" || value.Lease.Version != 1 {
			t.Fatalf("executed task = %#v", value)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("executor was not called")
	}
	cancel()
	if err := waitTaskWorkerResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestTaskWorkerCancelsAndWaitsWhenRenewalFails(t *testing.T) {
	store := &taskLeaseStoreStub{claimCalls: make(chan struct{}, 8)}
	store.claim = func(_ context.Context, workerID string, _ time.Duration) (task.Task, bool, error) {
		claims, _ := store.counts()
		if claims == 1 {
			return validTaskClaim(workerID), true, nil
		}
		return task.Task{}, false, nil
	}
	store.renew = func(context.Context, string, string, int64, time.Duration) (task.Task, error) {
		return task.Task{}, errors.New("lease lost")
	}
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	worker := newTestTaskWorker(t, store, taskExecutorFunc(func(ctx context.Context, _ task.Task) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return ctx.Err()
	}), 60*time.Millisecond, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskWorker(ctx, worker)
	waitTaskWorkerSignal(t, started)
	waitTaskWorkerSignal(t, canceled)
	select {
	case err := <-done:
		t.Fatalf("Run() returned before executor exited: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	// The worker continues polling after losing the lease; stop it after the executor exits.
	deadline := time.After(3 * time.Second)
	for {
		claims, renewals := store.counts()
		if claims >= 2 && renewals >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("worker did not resume polling after lease loss; claims/renewals = %d/%d", claims, renewals)
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := waitTaskWorkerResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestTaskWorkerRenewsLeaseWhileExecuting(t *testing.T) {
	store := &taskLeaseStoreStub{renewCalls: make(chan struct{}, 2)}
	store.claim = func(_ context.Context, workerID string, _ time.Duration) (task.Task, bool, error) {
		claims, _ := store.counts()
		if claims == 1 {
			return validTaskClaim(workerID), true, nil
		}
		return task.Task{}, false, nil
	}
	store.renew = func(_ context.Context, taskID, workerID string, version int64, _ time.Duration) (task.Task, error) {
		value := validTaskClaim(workerID)
		value.ID = taskID
		value.Lease.Version = version
		return value, nil
	}
	executorCanceled := make(chan struct{})
	worker := newTestTaskWorker(t, store, taskExecutorFunc(func(ctx context.Context, _ task.Task) error {
		<-ctx.Done()
		close(executorCanceled)
		return ctx.Err()
	}), 60*time.Millisecond, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskWorker(ctx, worker)
	waitTaskWorkerSignal(t, store.renewCalls)
	select {
	case <-executorCanceled:
		t.Fatal("successful lease renewal canceled the executor")
	default:
	}
	cancel()
	waitTaskWorkerSignal(t, executorCanceled)
	if err := waitTaskWorkerResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestTaskWorkerShutdownCancelsAndWaitsWithoutChangingTaskState(t *testing.T) {
	store := &taskLeaseStoreStub{}
	store.claim = func(_ context.Context, workerID string, _ time.Duration) (task.Task, bool, error) {
		claims, _ := store.counts()
		if claims == 1 {
			return validTaskClaim(workerID), true, nil
		}
		return task.Task{}, false, nil
	}
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	worker := newTestTaskWorker(t, store, taskExecutorFunc(func(ctx context.Context, _ task.Task) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return ctx.Err()
	}), time.Hour, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskWorker(ctx, worker)
	waitTaskWorkerSignal(t, started)
	cancel()
	waitTaskWorkerSignal(t, canceled)
	select {
	case err := <-done:
		t.Fatalf("Run() returned before executor exited: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := waitTaskWorkerResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	_, renewals := store.counts()
	if renewals != 0 {
		t.Fatalf("shutdown changed lease state with %d renewals", renewals)
	}
}

func TestTaskWorkerRejectsClaimWithoutLease(t *testing.T) {
	store := &taskLeaseStoreStub{claimCalls: make(chan struct{}, 8)}
	store.claim = func(_ context.Context, _ string, _ time.Duration) (task.Task, bool, error) {
		claims, _ := store.counts()
		if claims == 1 {
			return task.Task{ID: "task-without-lease", Status: task.StatusRunning}, true, nil
		}
		return task.Task{}, false, nil
	}
	executed := false
	worker := newTestTaskWorker(t, store, taskExecutorFunc(func(context.Context, task.Task) error {
		executed = true
		return nil
	}), time.Second, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := startTaskWorker(ctx, worker)
	waitTaskWorkerSignal(t, store.claimCalls)
	waitTaskWorkerSignal(t, store.claimCalls)
	cancel()
	if err := waitTaskWorkerResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	claims, renewals := store.counts()
	if executed || claims < 2 || renewals != 0 {
		t.Fatalf("executed/claim/renew counts = %v/%d/%d, want false/retries/0", executed, claims, renewals)
	}
}

func TestNewTaskWorkerRejectsInvalidLeaseConfiguration(t *testing.T) {
	store := &taskLeaseStoreStub{}
	executor := taskExecutorFunc(func(context.Context, task.Task) error { return nil })
	for _, options := range []TaskWorkerOptions{
		{Store: store, Executor: executor, WorkerID: "", LeaseDuration: time.Second, PollInterval: time.Second},
		{Store: store, Executor: executor, WorkerID: " worker", LeaseDuration: time.Second, PollInterval: time.Second},
		{Store: store, Executor: executor, WorkerID: "worker", LeaseDuration: time.Microsecond, PollInterval: time.Second},
		{Store: store, Executor: executor, WorkerID: "worker", LeaseDuration: time.Second, PollInterval: 0},
	} {
		if _, err := NewTaskWorker(options); err == nil {
			t.Fatalf("NewTaskWorker(%#v) succeeded, want validation error", options)
		}
	}
}

func TestNewTaskWorkerUsesNonZeroRenewIntervalForShortLease(t *testing.T) {
	worker := newTestTaskWorker(t, &taskLeaseStoreStub{}, taskExecutorFunc(func(context.Context, task.Task) error {
		return nil
	}), time.Millisecond, time.Second)
	if worker.renewInterval < time.Millisecond {
		t.Fatalf("renew interval = %s, want at least 1ms", worker.renewInterval)
	}
}
