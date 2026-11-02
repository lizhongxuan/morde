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

type ScheduleProcessor interface {
	ProcessDue(context.Context, time.Time, int32) (int, error)
}

type ScheduleWorkerOptions struct {
	Service      ScheduleProcessor
	PollInterval time.Duration
	BatchSize    int32
	Logger       *zap.Logger
}

type ScheduleWorker struct {
	service      ScheduleProcessor
	pollInterval time.Duration
	batchSize    int32
	logger       *zap.Logger
	runMu        sync.Mutex
	started      bool
}

func NewScheduleWorker(options ScheduleWorkerOptions) (*ScheduleWorker, error) {
	if options.Service == nil || options.PollInterval <= 0 || options.BatchSize < 1 || options.BatchSize > 100 {
		return nil, errors.New("schedule worker requires a service, positive interval, and batch size from 1 to 100")
	}
	logger := options.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ScheduleWorker{service: options.Service, pollInterval: options.PollInterval,
		batchSize: options.BatchSize, logger: logger}, nil
}

func (w *ScheduleWorker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("schedule worker context is required")
	}
	w.runMu.Lock()
	if w.started {
		w.runMu.Unlock()
		return errors.New("schedule worker can only be run once")
	}
	w.started = true
	w.runMu.Unlock()

	for ctx.Err() == nil {
		if err := w.processSafely(ctx); err != nil && ctx.Err() == nil {
			w.logger.Warn("schedule processing failed; will retry", zap.String("error_type", fmt.Sprintf("%T", err)))
		}
		if !waitScheduleWorker(ctx, w.pollInterval) {
			break
		}
	}
	return ctx.Err()
}

func (w *ScheduleWorker) processSafely(ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			w.logger.Error("schedule processing panicked", zap.Any("panic", recovered), zap.ByteString("stack", debug.Stack()))
			err = errors.New("schedule processing panicked")
		}
	}()
	_, err = w.service.ProcessDue(ctx, time.Now().UTC(), w.batchSize)
	return err
}

func waitScheduleWorker(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
