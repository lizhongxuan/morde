package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"morde/server/internal/domain/host"
)

type applicationStub struct {
	mu sync.Mutex

	creating       []string
	advance        func(context.Context, string) (map[string]int, error)
	advanceCalls   int
	reconcile      func(context.Context) (map[string]int, error)
	reconcileCalls int
	counts         map[host.State]int
	lock           LeaderLock
	lockAcquired   bool
	lockErr        error
	flush          func(context.Context) error
}

func (a *applicationStub) ListCreatingHosts(context.Context) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.creating...), nil
}

func (a *applicationStub) AdvanceCreate(ctx context.Context, hostID string) (map[string]int, error) {
	a.mu.Lock()
	a.advanceCalls++
	advance := a.advance
	a.mu.Unlock()
	if advance != nil {
		return advance(ctx, hostID)
	}
	return nil, nil
}

func (a *applicationStub) Reconcile(ctx context.Context) (map[string]int, error) {
	a.mu.Lock()
	a.reconcileCalls++
	reconcile := a.reconcile
	a.mu.Unlock()
	if reconcile != nil {
		return reconcile(ctx)
	}
	return nil, nil
}

func (a *applicationStub) FlushActivity(ctx context.Context) error {
	if a.flush != nil {
		return a.flush(ctx)
	}
	return nil
}

func (a *applicationStub) HostCounts(context.Context) (map[host.State]int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.counts, nil
}

func (a *applicationStub) TryReconcilerLock(context.Context) (LeaderLock, bool, error) {
	return a.lock, a.lockAcquired, a.lockErr
}

type leaderLockStub struct {
	released chan struct{}
}

func (l *leaderLockStub) Release(context.Context) error {
	close(l.released)
	return nil
}

func testWorker(t *testing.T, app Application, queue *CreateQueue) (*Worker, *prometheus.Registry) {
	t.Helper()
	registry := prometheus.NewRegistry()
	worker, err := New(Options{
		Application: app, CreateQueue: queue,
		Interval: time.Hour, ActivityFlushInterval: time.Hour,
		Registerer: registry,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return worker, registry
}

func startWorker(ctx context.Context, worker *Worker) <-chan error {
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	return done
}

func waitFor[T any](t *testing.T, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for worker")
		var zero T
		return zero
	}
}

func TestWorkerRecoversCreatingHostsAndDeduplicatesNotifications(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := NewCreateQueue()
	if err := queue.Notify(ctx, "host-1"); err != nil {
		t.Fatalf("Notify() error = %v", err)
	}
	app := &applicationStub{creating: []string{"host-1", "host-1"}}
	entered := make(chan string, 2)
	resume := make(chan struct{})
	app.advance = func(ctx context.Context, hostID string) (map[string]int, error) {
		entered <- hostID
		select {
		case <-resume:
			app.mu.Lock()
			app.creating = nil
			app.mu.Unlock()
			return map[string]int{"create_timeout": 1}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	worker, registry := testWorker(t, app, queue)
	done := startWorker(ctx, worker)
	if got := waitFor(t, entered); got != "host-1" {
		t.Fatalf("recovered host id = %q", got)
	}
	if err := queue.Notify(ctx, "host-1"); err != nil {
		t.Fatalf("duplicate Notify() error = %v", err)
	}
	close(resume)
	waitForCounter(t, registry, "cloudhost_reconcile_actions_total", "action", "create_timeout", 1)
	cancel()
	if err := waitFor(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	app.mu.Lock()
	calls := app.advanceCalls
	app.mu.Unlock()
	if calls != 1 {
		t.Fatalf("AdvanceCreate() calls = %d, want 1", calls)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	for _, family := range families {
		if family.GetName() == "cloudhost_reconcile_actions_total" {
			for _, metric := range family.Metric {
				if len(metric.Label) == 1 && metric.Label[0].GetValue() == "create_timeout" && metric.GetCounter().GetValue() == 1 {
					return
				}
			}
		}
	}
	t.Fatal("create_timeout action metric was not recorded")
}

func TestWorkerRecoversPanicsAndReleasesHostDeduplication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := NewCreateQueue()
	app := &applicationStub{}
	first := make(chan struct{})
	second := make(chan struct{})
	app.advance = func(context.Context, string) (map[string]int, error) {
		app.mu.Lock()
		call := app.advanceCalls
		app.mu.Unlock()
		if call == 1 {
			close(first)
			panic("expected test panic")
		}
		close(second)
		return nil, nil
	}
	worker, _ := testWorker(t, app, queue)
	done := startWorker(ctx, worker)
	if err := queue.Notify(ctx, "host-2"); err != nil {
		t.Fatalf("first Notify() error = %v", err)
	}
	waitFor(t, first)
	waitPendingRemoved(t, queue, "host-2")
	if err := queue.Notify(ctx, "host-2"); err != nil {
		t.Fatalf("second Notify() error = %v", err)
	}
	waitFor(t, second)
	cancel()
	if err := waitFor(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	app.mu.Lock()
	calls := app.advanceCalls
	app.mu.Unlock()
	if calls != 2 {
		t.Fatalf("AdvanceCreate() calls = %d, want 2", calls)
	}
}

func TestWorkerReleasesLeaderLockOnShutdownAndRecordsActions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lock := &leaderLockStub{released: make(chan struct{})}
	reconciled := make(chan struct{}, 1)
	app := &applicationStub{
		lock: lock, lockAcquired: true,
		counts: map[host.State]int{host.StateRunning: 3},
		reconcile: func(context.Context) (map[string]int, error) {
			reconciled <- struct{}{}
			return map[string]int{"lost": 1, "unexpected": 7}, nil
		},
	}
	worker, registry := testWorker(t, app, NewCreateQueue())
	done := startWorker(ctx, worker)
	waitFor(t, reconciled)
	cancel()
	if err := waitFor(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v", err)
	}
	waitFor(t, lock.released)
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	values := make(map[string]float64)
	for _, family := range families {
		for _, metric := range family.Metric {
			label := ""
			for _, pair := range metric.Label {
				if pair.GetName() == "action" || pair.GetName() == "state" || pair.GetName() == "result" {
					label = pair.GetValue()
				}
			}
			values[family.GetName()+":"+label] = metric.GetGauge().GetValue() + metric.GetCounter().GetValue()
		}
	}
	if values["cloudhost_reconcile_actions_total:lost"] != 1 || values["cloudhost_reconcile_actions_total:unexpected"] != 0 ||
		values["cloudhost_reconcile_runs_total:ok"] != 1 || values["cloudhost_hosts:running"] != 3 {
		t.Fatalf("worker metrics = %#v", values)
	}
}

func waitPendingRemoved(t *testing.T, queue *CreateQueue, hostID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		queue.mu.Lock()
		_, exists := queue.pending[hostID]
		queue.mu.Unlock()
		if !exists {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("create queue did not release host deduplication")
}

func waitForCounter(t *testing.T, registry *prometheus.Registry, familyName, labelName, labelValue string, want float64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		families, err := registry.Gather()
		if err != nil {
			t.Fatalf("Gather() error = %v", err)
		}
		for _, family := range families {
			if family.GetName() != familyName {
				continue
			}
			for _, metric := range family.Metric {
				for _, label := range metric.Label {
					if label.GetName() == labelName && label.GetValue() == labelValue && metric.GetCounter().GetValue() == want {
						return
					}
				}
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("%s{ %s=%q } counter did not reach %v", familyName, labelName, labelValue, want)
		}
		time.Sleep(time.Millisecond)
	}
}
