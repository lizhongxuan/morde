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

	"morde/server/internal/domain"
	"morde/server/internal/domain/record"
)

type RecordCaptureQueue interface {
	Claim(context.Context, string, time.Time, time.Duration, int32) ([]record.QueueItem, error)
	Commit(context.Context, string, string, int64) (record.RecordMetadata, error)
	Fail(context.Context, string, string, int64, string) error
}

type RecordDeleteQueue interface {
	ProcessDeletes(context.Context, int32) (int32, error)
}

type RecordCaptureWorkerOptions struct {
	Capture      RecordCaptureQueue
	Access       RecordDeleteQueue
	WorkerID     string
	PollInterval time.Duration
	BatchSize    int32
	Logger       *zap.Logger
}

type RecordCaptureWorker struct {
	capture      RecordCaptureQueue
	access       RecordDeleteQueue
	workerID     string
	pollInterval time.Duration
	batchSize    int32
	logger       *zap.Logger
	runMu        sync.Mutex
	started      bool
}

func NewRecordCaptureWorker(options RecordCaptureWorkerOptions) (*RecordCaptureWorker, error) {
	if options.Capture == nil || options.Access == nil {
		return nil, errors.New("record worker capture and access services are required")
	}
	if options.WorkerID == "" || strings.TrimSpace(options.WorkerID) != options.WorkerID ||
		len(options.WorkerID) > 128 || !utf8.ValidString(options.WorkerID) {
		return nil, errors.New("record worker id is invalid")
	}
	for _, char := range options.WorkerID {
		if char < 0x21 || char > 0x7e {
			return nil, errors.New("record worker id is invalid")
		}
	}
	if options.PollInterval <= 0 {
		return nil, errors.New("record worker poll interval must be positive")
	}
	if options.BatchSize < 1 || options.BatchSize > 100 {
		return nil, errors.New("record worker batch size must be between 1 and 100")
	}
	logger := options.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	return &RecordCaptureWorker{
		capture: options.Capture, access: options.Access, workerID: options.WorkerID,
		pollInterval: options.PollInterval, batchSize: options.BatchSize, logger: logger,
	}, nil
}

func (w *RecordCaptureWorker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("record worker context is required")
	}
	w.runMu.Lock()
	if w.started {
		w.runMu.Unlock()
		return errors.New("record worker can only be run once")
	}
	w.started = true
	w.runMu.Unlock()

	for ctx.Err() == nil {
		if err := w.processSafely(ctx); err != nil && ctx.Err() == nil {
			w.logger.Warn("record queue processing failed; will retry", zap.String("error_type", fmt.Sprintf("%T", err)))
		}
		if !waitRecordWorker(ctx, w.pollInterval) {
			break
		}
	}
	return ctx.Err()
}

func (w *RecordCaptureWorker) processSafely(ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			w.logger.Error("record queue processing panicked", zap.Any("panic", recovered), zap.ByteString("stack", debug.Stack()))
			err = errors.New("record queue processing panicked")
		}
	}()

	if _, err := w.access.ProcessDeletes(ctx, w.batchSize); err != nil {
		w.logger.Warn("record deletion processing failed; will retry", zap.String("error_type", fmt.Sprintf("%T", err)))
	}
	items, err := w.capture.Claim(ctx, w.workerID, time.Now().UTC(), 2*time.Minute, w.batchSize)
	if err != nil {
		return err
	}
	for _, item := range items {
		if ctx.Err() != nil {
			break
		}
		if !record.ValidQueueItemID(item.ID) || item.State != record.QueueLeased ||
			item.LeaseOwner != w.workerID || item.LeaseVersion < 1 {
			err = errors.Join(err, errors.New("record worker received an invalid lease"))
			continue
		}
		if _, commitErr := w.capture.Commit(ctx, item.ID, w.workerID, item.LeaseVersion); commitErr != nil {
			if recordCaptureFailureIsPermanent(commitErr) && ctx.Err() == nil {
				if failErr := w.capture.Fail(ctx, item.ID, w.workerID, item.LeaseVersion, "commit_failed"); failErr != nil {
					commitErr = errors.Join(commitErr, failErr)
				}
			}
			err = errors.Join(err, commitErr)
		}
	}
	return err
}

func recordCaptureFailureIsPermanent(err error) bool {
	switch domain.CodeOf(err) {
	case domain.CodeInternal, domain.CodeUpstreamError, domain.CodeUpstreamTimeout,
		domain.CodeRateLimited, domain.CodeConflict, domain.CodeNotFound, domain.CodeInvalidState:
		return false
	default:
		return true
	}
}

func waitRecordWorker(ctx context.Context, interval time.Duration) bool {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
