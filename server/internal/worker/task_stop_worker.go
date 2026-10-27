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

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

// TaskStopStore persists stop-request claims and their operation outcomes.
// The worker deliberately depends on this narrow interface so stop recovery
// can run independently of task execution and scheduling.
type TaskStopStore interface {
	ClaimNextTaskStop(context.Context, string, time.Duration) (app.TaskStopClaim, bool, error)
	RenewTaskStopClaim(context.Context, string, string, string, int64, time.Duration) error
	SettleTaskStopOperation(context.Context, string, string, string, int64, string, task.OperationStatus, string, bool) error
	CompleteTaskStop(context.Context, string, string, string, int64) (task.Task, error)
	ReleaseTaskStopClaim(context.Context, string, string, string, int64, time.Duration) error
}

type ExecutionStopReconciler interface {
	ReconcileStoppingExecution(context.Context) error
}

type MCPRuntimeStopReconciler interface {
	ReconcileMCPRuntimeCancellations(context.Context) error
}

// TaskStopOperationExecutor reconciles one already-dispatched runtime
// operation. Nonterminal statuses leave the durable operation unsettled.
type TaskStopOperationExecutor interface {
	StopTaskOperation(context.Context, task.Task, task.Operation) (status task.OperationStatus, result string, notFound bool, err error)
}

type TaskStopWorkerOptions struct {
	Store             TaskStopStore
	Executor          TaskStopOperationExecutor
	WorkerID          string
	LeaseDuration     time.Duration
	PollInterval      time.Duration
	OperationTimeout  time.Duration
	RetryAfter        time.Duration
	MCPRuntimeStopper MCPRuntimeStopReconciler
	Logger            *zap.Logger
}

type TaskStopWorker struct {
	store             TaskStopStore
	executor          TaskStopOperationExecutor
	workerID          string
	leaseDuration     time.Duration
	pollInterval      time.Duration
	operationTimeout  time.Duration
	retryAfter        time.Duration
	mcpRuntimeStopper MCPRuntimeStopReconciler
	renewInterval     time.Duration
	logger            *zap.Logger
	runMu             sync.Mutex
	started           bool
}

func NewTaskStopWorker(options TaskStopWorkerOptions) (*TaskStopWorker, error) {
	if options.Store == nil || options.Executor == nil {
		return nil, errors.New("task stop worker store and executor are required")
	}
	if options.WorkerID == "" || strings.TrimSpace(options.WorkerID) != options.WorkerID ||
		len(options.WorkerID) > 255 || !utf8.ValidString(options.WorkerID) {
		return nil, errors.New("task stop worker id is invalid")
	}
	if options.LeaseDuration < time.Millisecond {
		return nil, errors.New("task stop worker lease duration must be at least 1ms")
	}
	if options.PollInterval <= 0 {
		return nil, errors.New("task stop worker poll interval must be positive")
	}
	if options.OperationTimeout <= 0 {
		return nil, errors.New("task stop worker operation timeout must be positive")
	}
	if options.RetryAfter < 0 {
		return nil, errors.New("task stop worker retry delay cannot be negative")
	}
	if options.RetryAfter == 0 {
		options.RetryAfter = options.PollInterval
	}
	logger := options.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	renewInterval := options.LeaseDuration / 3
	if renewInterval < time.Millisecond {
		renewInterval = time.Millisecond
	}
	return &TaskStopWorker{
		store: options.Store, executor: options.Executor, workerID: options.WorkerID,
		leaseDuration: options.LeaseDuration, pollInterval: options.PollInterval,
		operationTimeout: options.OperationTimeout, retryAfter: options.RetryAfter,
		mcpRuntimeStopper: options.MCPRuntimeStopper,
		renewInterval:     renewInterval, logger: logger,
	}, nil
}

// Run continuously claims and settles one task stop request at a time. It must
// receive the service lifecycle context, not an HTTP request context.
func (w *TaskStopWorker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("task stop worker context is required")
	}
	w.runMu.Lock()
	if w.started {
		w.runMu.Unlock()
		return errors.New("task stop worker can only be run once")
	}
	w.started = true
	w.runMu.Unlock()

	for ctx.Err() == nil {
		if err := w.reconcileExecutionStops(ctx); err != nil {
			w.logger.Error("reconcile owner execution stop failed", zap.String("error_type", fmt.Sprintf("%T", err)))
			if !waitStopWorker(ctx, w.pollInterval) {
				break
			}
			continue
		}
		claim, ok, err := w.claim(ctx)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			w.logger.Error("claim task stop failed", zap.String("error_type", fmt.Sprintf("%T", err)))
			if !waitStopWorker(ctx, w.pollInterval) {
				break
			}
			continue
		}
		if !ok {
			if !waitStopWorker(ctx, w.pollInterval) {
				break
			}
			continue
		}
		if err := w.validateClaim(claim); err != nil {
			w.logger.Error("task stop claim is invalid", zap.String("error_type", fmt.Sprintf("%T", err)))
			w.release(claim, ctx)
			if !waitStopWorker(ctx, w.pollInterval) {
				break
			}
			continue
		}
		if err := w.processClaim(ctx, claim); err != nil {
			if ctx.Err() == nil {
				w.logger.Error("task stop recovery deferred", zap.String("task_id", claim.Task.ID), zap.String("request_id", claim.Request.ID), zap.String("error_type", fmt.Sprintf("%T", err)))
			}
			w.release(claim, ctx)
			if !waitStopWorker(ctx, w.pollInterval) {
				break
			}
		}
	}
	return ctx.Err()
}

func (w *TaskStopWorker) reconcileExecutionStops(ctx context.Context) error {
	if w.mcpRuntimeStopper != nil {
		callCtx, cancel := context.WithTimeout(ctx, w.operationTimeout)
		err := w.mcpRuntimeStopper.ReconcileMCPRuntimeCancellations(callCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("reconcile MCP runtime cancellations: %w", err)
		}
	}
	reconciler, ok := w.store.(ExecutionStopReconciler)
	if !ok {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, w.operationTimeout)
	defer cancel()
	return reconciler.ReconcileStoppingExecution(callCtx)
}

func (w *TaskStopWorker) claim(ctx context.Context) (app.TaskStopClaim, bool, error) {
	callCtx, cancel := context.WithTimeout(ctx, w.operationTimeout)
	defer cancel()
	return w.store.ClaimNextTaskStop(callCtx, w.workerID, w.leaseDuration)
}

func (w *TaskStopWorker) validateClaim(claim app.TaskStopClaim) error {
	request := claim.Request
	if claim.Task.ID == "" || request.ID == "" || request.TaskID != claim.Task.ID ||
		request.Status != task.StopRequestPending || request.LeaseOwner != w.workerID ||
		request.LeaseVersion < 1 || request.LeaseExpiresAt == nil || request.SafeAfter.IsZero() {
		return errors.New("task stop claim identity, state, or lease is invalid")
	}
	switch request.Action {
	case task.ActionPause:
		if claim.Task.Status != task.StatusPausing {
			return errors.New("pause stop request task is not pausing")
		}
	case task.ActionCancel:
		if claim.Task.Status != task.StatusCancelling {
			return errors.New("cancel stop request task is not cancelling")
		}
	default:
		return errors.New("task stop request action is invalid")
	}
	seen := make(map[string]struct{}, len(claim.Operations))
	for _, operation := range claim.Operations {
		if operation.ID == "" || operation.TaskID != claim.Task.ID {
			return errors.New("task stop operation identity is invalid")
		}
		if _, exists := seen[operation.ID]; exists {
			return errors.New("task stop claim contains a duplicate operation")
		}
		seen[operation.ID] = struct{}{}
		switch operation.Status {
		case task.OperationPrepared, task.OperationDispatched, task.OperationUnknown:
		default:
			return errors.New("task stop claim contains a non-recoverable operation status")
		}
	}
	return nil
}

func (w *TaskStopWorker) processClaim(ctx context.Context, claim app.TaskStopClaim) error {
	for _, operation := range claim.Operations {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if operation.Status == task.OperationPrepared {
			if err := w.renew(ctx, claim); err != nil {
				return fmt.Errorf("renew before prepared operation settlement: %w", err)
			}
			if err := w.settle(ctx, claim, operation.ID, task.OperationCancelled, "operation was prepared but never dispatched", false); err != nil {
				return fmt.Errorf("settle prepared operation: %w", err)
			}
			continue
		}

		status, result, notFound, err := w.stopOperation(ctx, claim, operation)
		if err != nil {
			return fmt.Errorf("reconcile operation %s: %w", operation.ID, err)
		}
		requireSafeAfter := false
		if notFound {
			status = task.OperationCancelled
			requireSafeAfter = true
			if result == "" {
				result = "operation not found; safe-after confirmation is required"
			}
		} else if !terminalOperationStatus(status) {
			return fmt.Errorf("runtime operation %s remains nonterminal (%s)", operation.ID, status)
		}

		if err := w.renew(ctx, claim); err != nil {
			return fmt.Errorf("renew before operation settlement: %w", err)
		}
		if err := w.settle(ctx, claim, operation.ID, status, result, requireSafeAfter); err != nil {
			return fmt.Errorf("settle operation %s: %w", operation.ID, err)
		}
	}

	if err := w.renew(ctx, claim); err != nil {
		return fmt.Errorf("renew before completing task stop: %w", err)
	}
	completeCtx, cancel := context.WithTimeout(ctx, w.operationTimeout)
	defer cancel()
	if _, err := w.store.CompleteTaskStop(completeCtx, claim.Task.ID, claim.Request.ID,
		w.workerID, claim.Request.LeaseVersion); err != nil {
		return fmt.Errorf("complete task stop: %w", err)
	}
	return nil
}

func (w *TaskStopWorker) stopOperation(ctx context.Context, claim app.TaskStopClaim, operation task.Operation) (task.OperationStatus, string, bool, error) {
	if err := w.renew(ctx, claim); err != nil {
		return "", "", false, fmt.Errorf("renew before external stop operation: %w", err)
	}
	executeCtx, cancel := context.WithTimeout(ctx, w.operationTimeout)
	defer cancel()
	done := make(chan taskStopExecutionResult, 1)
	go func() {
		status, result, notFound, err := w.stopOperationSafely(executeCtx, claim.Task, operation)
		done <- taskStopExecutionResult{status: status, result: result, notFound: notFound, err: err}
	}()

	ticker := time.NewTicker(w.renewInterval)
	defer ticker.Stop()
	for {
		select {
		case result := <-done:
			if ctx.Err() != nil {
				return "", "", false, ctx.Err()
			}
			if err := executeCtx.Err(); errors.Is(err, context.DeadlineExceeded) {
				return "", "", false, err
			}
			return result.status, result.result, result.notFound, result.err
		case <-ctx.Done():
			cancel()
			<-done
			return "", "", false, ctx.Err()
		case <-executeCtx.Done():
			cancel()
			<-done
			return "", "", false, executeCtx.Err()
		case <-ticker.C:
			if err := w.renew(ctx, claim); err != nil {
				cancel()
				<-done
				return "", "", false, fmt.Errorf("renew task stop claim while reconciling operation: %w", err)
			}
		}
	}
}

type taskStopExecutionResult struct {
	status   task.OperationStatus
	result   string
	notFound bool
	err      error
}

func (w *TaskStopWorker) stopOperationSafely(ctx context.Context, taskValue task.Task, operation task.Operation) (status task.OperationStatus, result string, notFound bool, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			w.logger.Error("task stop executor panicked", zap.String("task_id", taskValue.ID), zap.String("operation_id", operation.ID), zap.ByteString("stack", debug.Stack()))
			status, result, notFound, err = "", "", false, errors.New("task stop executor panicked")
		}
	}()
	return w.executor.StopTaskOperation(ctx, taskValue, operation)
}

func (w *TaskStopWorker) renew(ctx context.Context, claim app.TaskStopClaim) error {
	callCtx, cancel := context.WithTimeout(ctx, w.operationTimeout)
	defer cancel()
	return w.store.RenewTaskStopClaim(callCtx, claim.Task.ID, claim.Request.ID,
		w.workerID, claim.Request.LeaseVersion, w.leaseDuration)
}

func (w *TaskStopWorker) settle(ctx context.Context, claim app.TaskStopClaim, operationID string, status task.OperationStatus, result string, requireSafeAfter bool) error {
	callCtx, cancel := context.WithTimeout(ctx, w.operationTimeout)
	defer cancel()
	return w.store.SettleTaskStopOperation(callCtx, claim.Task.ID, claim.Request.ID,
		w.workerID, claim.Request.LeaseVersion, operationID, status, result, requireSafeAfter)
}

func (w *TaskStopWorker) release(claim app.TaskStopClaim, ctx context.Context) {
	releaseParent := ctx
	if ctx.Err() != nil {
		releaseParent = context.WithoutCancel(ctx)
	}
	releaseCtx, cancel := context.WithTimeout(releaseParent, w.operationTimeout)
	defer cancel()
	if err := w.store.ReleaseTaskStopClaim(releaseCtx, claim.Task.ID, claim.Request.ID,
		w.workerID, claim.Request.LeaseVersion, w.retryAfter); err != nil {
		w.logger.Error("release task stop claim failed", zap.String("task_id", claim.Task.ID), zap.String("request_id", claim.Request.ID), zap.String("error_type", fmt.Sprintf("%T", err)))
	}
}

func terminalOperationStatus(status task.OperationStatus) bool {
	switch status {
	case task.OperationSucceeded, task.OperationFailed, task.OperationCancelled:
		return true
	default:
		return false
	}
}

func waitStopWorker(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}
