package worker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
)

type ScheduleProcessor interface {
	ProcessDue(context.Context) (bool, error)
}

type ScheduleWorker struct {
	processor    ScheduleProcessor
	pollInterval time.Duration
	logger       *zap.Logger
	runMu        sync.Mutex
	started      bool
}

func NewScheduleWorker(processor ScheduleProcessor, pollInterval time.Duration, logger *zap.Logger) (*ScheduleWorker, error) {
	if processor == nil || pollInterval <= 0 {
		return nil, errors.New("schedule processor and positive poll interval are required")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &ScheduleWorker{processor: processor, pollInterval: pollInterval, logger: logger}, nil
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
		processed, err := w.processor.ProcessDue(ctx)
		if err != nil && ctx.Err() == nil {
			w.logger.Error("schedule processing failed", zap.String("error_type", fmt.Sprintf("%T", err)))
		}
		if ctx.Err() != nil {
			break
		}
		if processed {
			continue
		}
		if !waitTaskWorker(ctx, w.pollInterval) {
			break
		}
	}
	return ctx.Err()
}
