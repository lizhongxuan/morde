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

type NotificationProcessor interface {
	ProcessPending(context.Context, int32) (int, error)
}

type NotificationWorkerOptions struct {
	Service      NotificationProcessor
	PollInterval time.Duration
	BatchSize    int32
	Logger       *zap.Logger
}

type NotificationWorker struct {
	service      NotificationProcessor
	pollInterval time.Duration
	batchSize    int32
	logger       *zap.Logger
	runMu        sync.Mutex
	started      bool
}

func NewNotificationWorker(options NotificationWorkerOptions) (*NotificationWorker, error) {
	if options.Service == nil || options.PollInterval <= 0 || options.BatchSize < 1 || options.BatchSize > 100 {
		return nil, errors.New("notification worker requires a service, positive interval, and batch size from 1 to 100")
	}
	logger := options.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &NotificationWorker{service: options.Service, pollInterval: options.PollInterval, batchSize: options.BatchSize, logger: logger}, nil
}

func (w *NotificationWorker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("notification worker context is required")
	}
	w.runMu.Lock()
	if w.started {
		w.runMu.Unlock()
		return errors.New("notification worker can only be run once")
	}
	w.started = true
	w.runMu.Unlock()

	for ctx.Err() == nil {
		if err := w.processSafely(ctx); err != nil && ctx.Err() == nil {
			w.logger.Warn("notification processing failed; will retry", zap.String("error_type", fmt.Sprintf("%T", err)))
		}
		if !waitNotificationWorker(ctx, w.pollInterval) {
			break
		}
	}
	return ctx.Err()
}

func (w *NotificationWorker) processSafely(ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			w.logger.Error("notification processing panicked", zap.Any("panic", recovered), zap.ByteString("stack", debug.Stack()))
			err = errors.New("notification processing panicked")
		}
	}()
	_, err = w.service.ProcessPending(ctx, w.batchSize)
	return err
}

func waitNotificationWorker(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
