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

type TaskMediaCleanupProcessor interface {
	RunOnce(context.Context, time.Time, int32) error
}

type TaskMediaCleanupWorkerOptions struct {
	Service      TaskMediaCleanupProcessor
	PollInterval time.Duration
	BatchSize    int32
	Logger       *zap.Logger
}

type TaskMediaCleanupWorker struct {
	service      TaskMediaCleanupProcessor
	pollInterval time.Duration
	batchSize    int32
	logger       *zap.Logger
	runMu        sync.Mutex
	started      bool
}

func NewTaskMediaCleanupWorker(options TaskMediaCleanupWorkerOptions) (*TaskMediaCleanupWorker, error) {
	if options.Service == nil || options.PollInterval <= 0 || options.BatchSize < 1 || options.BatchSize > 1000 {
		return nil, errors.New("task media cleanup worker requires a service, positive interval, and batch size from 1 to 1000")
	}
	logger := options.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &TaskMediaCleanupWorker{service: options.Service, pollInterval: options.PollInterval,
		batchSize: options.BatchSize, logger: logger}, nil
}

func (w *TaskMediaCleanupWorker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("task media cleanup worker context is required")
	}
	w.runMu.Lock()
	if w.started {
		w.runMu.Unlock()
		return errors.New("task media cleanup worker can only be run once")
	}
	w.started = true
	w.runMu.Unlock()
	for ctx.Err() == nil {
		if err := w.processSafely(ctx); err != nil && ctx.Err() == nil {
			w.logger.Warn("task media cleanup failed; will retry", zap.String("error_type", fmt.Sprintf("%T", err)))
		}
		if !waitScheduleWorker(ctx, w.pollInterval) {
			break
		}
	}
	return ctx.Err()
}

func (w *TaskMediaCleanupWorker) processSafely(ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			w.logger.Error("task media cleanup panicked", zap.Any("panic", recovered), zap.ByteString("stack", debug.Stack()))
			err = errors.New("task media cleanup panicked")
		}
	}()
	return w.service.RunOnce(ctx, time.Now().UTC(), w.batchSize)
}
