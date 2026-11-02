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
	cancel            context.CancelFunc
	cancelAfterCommit bool
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

func TestRecordCaptureWorkerDoesNotFailTransientCommitErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	queue := &recordCaptureQueueFake{
		items:     []record.QueueItem{{ID: "j1234567", State: record.QueueLeased, LeaseOwner: "record-worker", LeaseVersion: 4}},
		commitErr: domain.NewError(domain.CodeInternal, "temporary database failure"), cancelAfterCommit: true,
	}
	access := &recordDeleteQueueFake{}
	worker := newRecordWorkerForTest(t, queue, access, cancel)
	if err := worker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	if queue.claimed < 1 || queue.committed != 1 || queue.failed != 0 {
		t.Fatalf("transient commit calls: claim=%d commit=%d fail=%d", queue.claimed, queue.committed, queue.failed)
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
