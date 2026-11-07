package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

type taskMediaCleanupProcessorStub struct {
	calls chan taskMediaCleanupCall
}

type taskMediaCleanupCall struct {
	now       time.Time
	batchSize int32
}

func (s taskMediaCleanupProcessorStub) RunOnce(_ context.Context, now time.Time, batchSize int32) error {
	s.calls <- taskMediaCleanupCall{now: now, batchSize: batchSize}
	return nil
}

func TestTaskMediaCleanupWorkerRunsUntilContextStops(t *testing.T) {
	service := taskMediaCleanupProcessorStub{calls: make(chan taskMediaCleanupCall, 1)}
	worker, err := NewTaskMediaCleanupWorker(TaskMediaCleanupWorkerOptions{Service: service, PollInterval: time.Hour, BatchSize: 10})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	select {
	case call := <-service.calls:
		if call.now.IsZero() || call.batchSize != 10 {
			t.Fatalf("cleanup worker call = %#v", call)
		}
	case <-time.After(time.Second):
		cancel()
		t.Fatal("cleanup worker did not run immediately")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cleanup worker exit error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup worker did not stop")
	}
	if err := worker.Run(context.Background()); err == nil {
		t.Fatal("cleanup worker accepted a second run")
	}
}
