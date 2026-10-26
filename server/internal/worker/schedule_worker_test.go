package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type scheduleProcessorFake struct {
	calls     atomic.Int32
	steps     chan scheduleProcessorResult
	callTimes chan time.Time
}

type scheduleProcessorResult struct {
	processed bool
	err       error
}

func (f *scheduleProcessorFake) ProcessDue(ctx context.Context) (bool, error) {
	f.calls.Add(1)
	if f.callTimes != nil {
		f.callTimes <- time.Now()
	}
	select {
	case result := <-f.steps:
		return result.processed, result.err
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func TestScheduleWorkerDrainsProcessedOccurrencesThenPolls(t *testing.T) {
	processor := &scheduleProcessorFake{steps: make(chan scheduleProcessorResult, 3)}
	processor.steps <- scheduleProcessorResult{processed: true}
	processor.steps <- scheduleProcessorResult{processed: true}
	processor.steps <- scheduleProcessorResult{}
	worker, err := NewScheduleWorker(processor, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	waitForSchedulePolls(t, &processor.calls, 3)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestScheduleWorkerBacksOffAfterErrorsAndStopsPromptly(t *testing.T) {
	processor := &scheduleProcessorFake{steps: make(chan scheduleProcessorResult, 2), callTimes: make(chan time.Time, 3)}
	processor.steps <- scheduleProcessorResult{err: errors.New("temporary database error")}
	processor.steps <- scheduleProcessorResult{}
	worker, err := NewScheduleWorker(processor, 80*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	firstCall := <-processor.callTimes
	select {
	case <-processor.callTimes:
		cancel()
		t.Fatal("worker retried before its poll interval")
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case secondCall := <-processor.callTimes:
		if elapsed := secondCall.Sub(firstCall); elapsed < 80*time.Millisecond {
			cancel()
			t.Fatalf("retry delay = %s, want at least one poll interval", elapsed)
		}
	case <-time.After(time.Second):
		cancel()
		t.Fatal("worker did not retry after its poll interval")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestScheduleWorkerRejectsConcurrentSecondRun(t *testing.T) {
	processor := &scheduleProcessorFake{steps: make(chan scheduleProcessorResult)}
	worker, err := NewScheduleWorker(processor, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	waitForSchedulePolls(t, &processor.calls, 1)
	if err := worker.Run(context.Background()); err == nil {
		cancel()
		t.Fatal("second Run() unexpectedly succeeded")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Run() error = %v, want context.Canceled", err)
	}
}

func waitForSchedulePolls(t *testing.T, calls *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for calls.Load() < want && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := calls.Load(); got < want {
		t.Fatalf("processor calls = %d, want at least %d", got, want)
	}
}
