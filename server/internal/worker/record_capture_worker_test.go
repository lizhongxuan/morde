package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/record"
)

type recordCaptureQueueFake struct {
	items             []record.QueueItem
	commitErr         error
	claimed           int
	committed         int
	failed            int
	failureCode       string
	failedState       record.QueueState
	cancel            context.CancelFunc
	cancelAfterCommit bool
}

type recordCaptureRecoveryQueueFake struct {
	*recordCaptureQueueFake
	recovered bool
}

func (f *recordCaptureRecoveryQueueFake) RecoverCloudDSHCaptures(_ context.Context, limit int32) error {
	if limit != 3 {
		return errors.New("invalid recovery limit")
	}
	f.recovered = true
	return nil
}

func (f *recordCaptureRecoveryQueueFake) Claim(ctx context.Context, worker string, now time.Time, lease time.Duration, limit int32) ([]record.QueueItem, error) {
	if !f.recovered {
		return nil, errors.New("capture recovery did not run before claiming")
	}
	return f.recordCaptureQueueFake.Claim(ctx, worker, now, lease, limit)
}

func (f *recordCaptureQueueFake) Claim(_ context.Context, worker string, now time.Time, lease time.Duration, limit int32) ([]record.QueueItem, error) {
	f.claimed++
	if worker != "record-worker" || now.IsZero() || lease <= 0 || limit != 3 {
		return nil, errors.New("invalid claim arguments")
	}
	return f.items, nil
}

func (f *recordCaptureQueueFake) Commit(_ context.Context, queueID, worker string, leaseVersion int64) (record.RecordMetadata, error) {
	f.committed++
	if queueID != "j1234567" || worker != "record-worker" || leaseVersion != 4 {
		return record.RecordMetadata{}, errors.New("invalid commit arguments")
	}
	if f.cancelAfterCommit && f.cancel != nil {
		f.cancel()
	}
	return record.RecordMetadata{}, f.commitErr
}

func (f *recordCaptureQueueFake) Fail(_ context.Context, queueID, worker string, leaseVersion int64, failureCode string) error {
	f.failed++
	f.failureCode = failureCode
	f.failedState = record.QueueFailed
	if queueID != "j1234567" || worker != "record-worker" || leaseVersion != 4 {
		return errors.New("invalid fail arguments")
	}
	if f.cancel != nil {
		f.cancel()
	}
	return nil
}

type recordDeleteQueueFake struct {
	calls int
}

func (f *recordDeleteQueueFake) ProcessDeletes(_ context.Context, limit int32) (int32, error) {
	f.calls++
	if limit != 3 {
		return 0, errors.New("invalid delete batch size")
	}
	return 0, nil
}

func newRecordWorkerForTest(t *testing.T, capture RecordCaptureQueue, access RecordDeleteQueue, cancel context.CancelFunc) *RecordCaptureWorker {
	t.Helper()
	worker, err := NewRecordCaptureWorker(RecordCaptureWorkerOptions{
		Capture: capture, Access: access, WorkerID: "record-worker", PollInterval: time.Hour, BatchSize: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if queue, ok := capture.(*recordCaptureQueueFake); ok {
		queue.cancel = cancel
	}
	return worker
}

func TestRecordCaptureWorkerCommitsAndProcessesDeletions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	queue := &recordCaptureQueueFake{items: []record.QueueItem{{
		ID: "j1234567", State: record.QueueLeased, LeaseOwner: "record-worker", LeaseVersion: 4,
	}}, cancelAfterCommit: true}
	access := &recordDeleteQueueFake{}
	worker := newRecordWorkerForTest(t, queue, access, cancel)
	if err := worker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	if queue.claimed != 1 || queue.committed != 1 || queue.failed != 0 || access.calls != 1 {
		t.Fatalf("worker calls: claim=%d commit=%d fail=%d deletes=%d", queue.claimed, queue.committed, queue.failed, access.calls)
	}
}

func TestRecordCaptureWorkerRecoversCompletedCloudDSHTasksBeforeClaiming(t *testing.T) {
	queue := &recordCaptureRecoveryQueueFake{recordCaptureQueueFake: &recordCaptureQueueFake{}}
	access := &recordDeleteQueueFake{}
	worker := newRecordWorkerForTest(t, queue, access, nil)
	if err := worker.processSafely(context.Background()); err != nil {
		t.Fatalf("processSafely() error = %v", err)
	}
	if !queue.recovered || queue.claimed != 1 {
		t.Fatalf("recovery/claim calls: recovered=%t claimed=%d", queue.recovered, queue.claimed)
	}
}

func TestRecordCaptureWorkerDoesNotFailTransientCommitErrors(t *testing.T) {
	queue := &recordCaptureQueueFake{
		items: []record.QueueItem{{
			ID: "j1234567", State: record.QueueLeased, LeaseOwner: "record-worker", LeaseVersion: 4,
			Attempts: maxRecordCaptureAttempts - 1,
		}},
		commitErr: domain.NewError(domain.CodeInternal, "temporary database failure"),
	}
	access := &recordDeleteQueueFake{}
	worker := newRecordWorkerForTest(t, queue, access, nil)
	if err := worker.processSafely(context.Background()); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("processSafely() error = %v, want transient internal error", err)
	}
	if queue.claimed != 1 || queue.committed != 1 || queue.failed != 0 {
		t.Fatalf("transient commit calls: claim=%d commit=%d fail=%d", queue.claimed, queue.committed, queue.failed)
	}
}

func TestRecordCaptureWorkerFailsTransientCommitErrorsAtAttemptLimit(t *testing.T) {
	queue := &recordCaptureQueueFake{
		items: []record.QueueItem{{
			ID: "j1234567", State: record.QueueLeased, LeaseOwner: "record-worker", LeaseVersion: 4,
			Attempts: maxRecordCaptureAttempts,
		}},
		commitErr: domain.NewError(domain.CodeInternal, "temporary database failure"),
	}
	access := &recordDeleteQueueFake{}
	worker := newRecordWorkerForTest(t, queue, access, nil)
	if err := worker.processSafely(context.Background()); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("processSafely() error = %v, want transient internal error", err)
	}
	if queue.committed != 1 || queue.failed != 1 || queue.failureCode != "commit_failed" || queue.failedState != record.QueueFailed {
		t.Fatalf("transient commit at attempt limit: commit=%d fail=%d code=%q state=%q",
			queue.committed, queue.failed, queue.failureCode, queue.failedState)
	}
}

func TestRecordCaptureWorkerFailsPermanentCommitErrorsWithFixedCode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := &recordCaptureQueueFake{
		items:     []record.QueueItem{{ID: "j1234567", State: record.QueueLeased, LeaseOwner: "record-worker", LeaseVersion: 4}},
		commitErr: domain.NewError(domain.CodeForbidden, "installation is no longer valid"),
	}
	access := &recordDeleteQueueFake{}
	worker := newRecordWorkerForTest(t, queue, access, cancel)
	if err := worker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	if queue.failed != 1 || queue.failureCode != "commit_failed" {
		t.Fatalf("Fail calls = %d code = %q, want one fixed safe failure", queue.failed, queue.failureCode)
	}
}

func TestNewRecordCaptureWorkerValidatesOptions(t *testing.T) {
	queue := &recordCaptureQueueFake{}
	access := &recordDeleteQueueFake{}
	for _, options := range []RecordCaptureWorkerOptions{
		{}, {Capture: queue, Access: access, WorkerID: "record-worker", BatchSize: 1},
		{Capture: queue, Access: access, WorkerID: "record-worker", PollInterval: time.Second},
		{Capture: queue, Access: access, WorkerID: "", PollInterval: time.Second, BatchSize: 1},
		{Capture: queue, Access: access, WorkerID: "bad worker", PollInterval: time.Second, BatchSize: 1},
		{Capture: queue, Access: access, WorkerID: "record-worker", PollInterval: time.Second, BatchSize: 101},
	} {
		if _, err := NewRecordCaptureWorker(options); err == nil {
			t.Errorf("NewRecordCaptureWorker(%#v) error = nil", options)
		}
	}
}
