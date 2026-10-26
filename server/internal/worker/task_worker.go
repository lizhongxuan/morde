package worker

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"morde/server/internal/domain/task"
)

type TaskLeaseStore interface {
	ClaimNextTask(context.Context, string, time.Duration) (task.Task, bool, error)
	RenewTaskLease(context.Context, string, string, int64, time.Duration) (task.Task, error)
}

type TaskExecutor interface {
	ExecuteTask(context.Context, task.Task) error
}

var errTaskExecutorPanicked = errors.New("task executor panicked")

type TaskWorkerOptions struct {
	Store         TaskLeaseStore
	Executor      TaskExecutor
	WorkerID      string
	LeaseDuration time.Duration
	PollInterval  time.Duration
	Logger        *zap.Logger
}

type TaskWorker struct {
	store         TaskLeaseStore
	executor      TaskExecutor
	workerID      string
	leaseDuration time.Duration
	pollInterval  time.Duration
	renewInterval time.Duration
	logger        *zap.Logger
	runMu         sync.Mutex
	started       bool
}

func NewTaskWorker(options TaskWorkerOptions) (*TaskWorker, error) {
	if options.Store == nil || options.Executor == nil {
		return nil, errors.New("task worker dependencies are required")
	}
	if options.WorkerID == "" || strings.TrimSpace(options.WorkerID) != options.WorkerID ||
		len(options.WorkerID) > 255 || !utf8.ValidString(options.WorkerID) {
		return nil, errors.New("task worker id is invalid")
	}
	if options.LeaseDuration < time.Millisecond {
		return nil, errors.New("task worker lease duration must be at least 1ms")
	}
	if options.PollInterval <= 0 {
		return nil, errors.New("task worker poll interval must be positive")
	}
	logger := options.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	renewInterval := options.LeaseDuration / 3
	if renewInterval < time.Millisecond {
		renewInterval = time.Millisecond
	}
	return &TaskWorker{
		store: options.Store, executor: options.Executor, workerID: options.WorkerID,
		leaseDuration: options.LeaseDuration, pollInterval: options.PollInterval,
		renewInterval: renewInterval, logger: logger,
	}, nil
}

// Run must receive the service lifecycle context, not an HTTP request context.
func (w *TaskWorker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("task worker context is required")
	}
	w.runMu.Lock()
	if w.started {
		w.runMu.Unlock()
		return errors.New("task worker can only be run once")
	}
	w.started = true
	w.runMu.Unlock()

	for ctx.Err() == nil {
		claimed, ok, err := w.store.ClaimNextTask(ctx, w.workerID, w.leaseDuration)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			w.logger.Error("claim task failed", zap.String("error_type", fmt.Sprintf("%T", err)))
			if !waitTaskWorker(ctx, w.pollInterval) {
				break
			}
			continue
		}
		if !ok {
			if !waitTaskWorker(ctx, w.pollInterval) {
				break
			}
			continue
		}
		if !w.validClaim(claimed) {
			w.logger.Error("task claim returned an invalid lease", zap.String("task_id", claimed.ID))
			if !waitTaskWorker(ctx, w.pollInterval) {
				break
			}
			continue
		}
		if err := w.runClaimed(ctx, claimed); err != nil && ctx.Err() == nil {
			if !errors.Is(err, errTaskExecutorPanicked) {
				w.logger.Error("task execution stopped", zap.String("task_id", claimed.ID), zap.String("error_type", fmt.Sprintf("%T", err)))
			}
			if !waitTaskWorker(ctx, w.pollInterval) {
				break
			}
		}
	}
	return ctx.Err()
}

func (w *TaskWorker) validClaim(value task.Task) bool {
	return value.ID != "" && value.Status == task.StatusRunning &&
		value.Lease.Owner == w.workerID && value.Lease.Version > 0 && value.Lease.ExpiresAt != nil
}

func (w *TaskWorker) runClaimed(ctx context.Context, claimed task.Task) error {
	executeCtx, cancelExecute := context.WithCancel(ctx)
	defer cancelExecute()
	executeDone := make(chan error, 1)
	go func() {
		executeDone <- w.executeSafely(executeCtx, claimed)
	}()

	ticker := time.NewTicker(w.renewInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			cancelExecute()
			w.waitForExecution(executeDone, claimed.ID)
			return ctx.Err()
		}
		select {
		case err := <-executeDone:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		case <-ctx.Done():
			cancelExecute()
			w.waitForExecution(executeDone, claimed.ID)
			return ctx.Err()
		case <-ticker.C:
			renewCtx, cancelRenew := context.WithTimeout(ctx, w.renewInterval)
			renewed, err := w.store.RenewTaskLease(
				renewCtx, claimed.ID, w.workerID, claimed.Lease.Version, w.leaseDuration,
			)
			cancelRenew()
			if err == nil && !w.validRenewal(renewed, claimed) {
				err = errors.New("renewed task lease is invalid")
			}
			if err != nil {
				cancelExecute()
				w.waitForExecution(executeDone, claimed.ID)
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
		}
	}
}

func (w *TaskWorker) waitForExecution(done <-chan error, taskID string) {
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		w.logger.Error("task executor stopped after lease cancellation", zap.String("task_id", taskID), zap.String("error_type", fmt.Sprintf("%T", err)))
	}
}

func (w *TaskWorker) validRenewal(value, claimed task.Task) bool {
	return value.ID == claimed.ID && value.Status == task.StatusRunning &&
		value.Lease.Owner == w.workerID && value.Lease.Version == claimed.Lease.Version &&
		value.Lease.ExpiresAt != nil
}

func (w *TaskWorker) executeSafely(ctx context.Context, value task.Task) (result error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			w.logger.Error("task executor panicked", zap.String("task_id", value.ID), zap.ByteString("stack", debug.Stack()))
			result = errTaskExecutorPanicked
		}
	}()
	result = w.executor.ExecuteTask(ctx, value)
	return result
}

func waitTaskWorker(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}
