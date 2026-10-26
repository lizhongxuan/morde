package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type notificationProcessorFake struct {
	calls atomic.Int32
	call  func(context.Context, int32, int32) (int, error)
}

func (f *notificationProcessorFake) ProcessPending(ctx context.Context, batch int32) (int, error) {
	call := f.calls.Add(1)
	return f.call(ctx, batch, call)
}

func newNotificationWorkerForTest(t *testing.T, service NotificationProcessor, poll time.Duration, batch int32) *NotificationWorker {
	t.Helper()
	worker, err := NewNotificationWorker(NotificationWorkerOptions{Service: service, PollInterval: poll, BatchSize: batch})
	if err != nil {
		t.Fatalf("NewNotificationWorker() error = %v", err)
	}
	return worker
}

func TestNewNotificationWorkerValidatesOptions(t *testing.T) {
	for _, options := range []NotificationWorkerOptions{
		{}, {Service: &notificationProcessorFake{}, BatchSize: 1},
		{Service: &notificationProcessorFake{}, PollInterval: time.Second},
		{Service: &notificationProcessorFake{}, PollInterval: time.Second, BatchSize: 101},
	} {
		if _, err := NewNotificationWorker(options); err == nil {
			t.Errorf("NewNotificationWorker(%#v) error = nil", options)
		}
	}
}

func TestNotificationWorkerRetriesErrorsAndExitsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	service := &notificationProcessorFake{call: func(_ context.Context, batch, call int32) (int, error) {
		if batch != 5 {
			t.Errorf("batch size = %d, want 5", batch)
		}
		if call == 1 {
			return 0, errors.New("temporary database error")
		}
		cancel()
		return 0, nil
	}}
	worker := newNotificationWorkerForTest(t, service, time.Millisecond, 5)
	if err := worker.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	if got := service.calls.Load(); got != 2 {
		t.Fatalf("ProcessPending calls = %d, want retry then success", got)
	}
	if err := worker.Run(context.Background()); err == nil {
		t.Fatal("second Run() was accepted")
	}
}

func TestNotificationWorkerStopsQuietlyWhenNoEventsChange(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	service := &notificationProcessorFake{call: func(_ context.Context, _ int32, call int32) (int, error) {
		if call == 1 {
			cancel()
		}
		return 0, nil
	}}
	worker := newNotificationWorkerForTest(t, service, time.Hour, 10)
	if err := worker.Run(ctx); !errors.Is(err, context.Canceled) || service.calls.Load() != 1 {
		t.Fatalf("Run() error=%v calls=%d", err, service.calls.Load())
	}
}
