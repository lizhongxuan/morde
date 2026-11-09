package worker

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"go.uber.org/zap"
)

type GoalAgentTriggerProcessor interface {
	ProcessPending(context.Context, int32) (int, error)
}

type GoalAgentTriggerWorkerOptions struct {
	Service      GoalAgentTriggerProcessor
	PollInterval time.Duration
	BatchSize    int32
	Logger       *zap.Logger
}

type GoalAgentTriggerWorker struct {
	service      GoalAgentTriggerProcessor
	pollInterval time.Duration
	batchSize    int32
	logger       *zap.Logger
	runMu        sync.Mutex
	started      bool
}

func NewGoalAgentTriggerWorker(options GoalAgentTriggerWorkerOptions) (*GoalAgentTriggerWorker, error) {
	if options.Service == nil || options.PollInterval <= 0 || options.BatchSize < 1 || options.BatchSize > 100 {
		return nil, errors.New("goal agent trigger worker requires a service, positive interval, and batch size from 1 to 100")
	}
	logger := options.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &GoalAgentTriggerWorker{service: options.Service, pollInterval: options.PollInterval,
		batchSize: options.BatchSize, logger: logger}, nil
}

func (w *GoalAgentTriggerWorker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("goal agent trigger worker context is required")
	}
	w.runMu.Lock()
	if w.started {
		w.runMu.Unlock()
		return errors.New("goal agent trigger worker can only be run once")
	}
	w.started = true
	w.runMu.Unlock()
	for ctx.Err() == nil {
		if err := w.processSafely(ctx); err != nil && ctx.Err() == nil {
			w.logger.Warn("Goal agent trigger processing failed; will retry", zap.String("error_type", fmt.Sprintf("%T", err)))
		}
		if !waitGoalAgentTriggerWorker(ctx, w.pollInterval) {
			break
		}
	}
	return ctx.Err()
}

func (w *GoalAgentTriggerWorker) processSafely(ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			w.logger.Error("Goal agent trigger processing panicked", zap.Any("panic", recovered), zap.ByteString("stack", debug.Stack()))
			err = errors.New("goal agent trigger processing panicked")
		}
	}()
	_, err = w.service.ProcessPending(ctx, w.batchSize)
	return err
}

func waitGoalAgentTriggerWorker(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
