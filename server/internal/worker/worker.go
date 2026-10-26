package worker

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/domain/host"
)

// shortcut: creation concurrency is fixed at four; make it configurable only if deployments need different throughput.
const createWorkers = 4

type LeaderLock interface {
	Release(context.Context) error
}

// Application is the app-layer facade used by background jobs. Implementations
// keep persistence and sandbox operations behind app use cases.
type Application interface {
	ListCreatingHosts(context.Context) ([]string, error)
	AdvanceCreate(context.Context, string) (map[string]int, error)
	Reconcile(context.Context) (map[string]int, error)
	FlushActivity(context.Context) error
	HostCounts(context.Context) (map[host.State]int, error)
	TryReconcilerLock(context.Context) (LeaderLock, bool, error)
}

type Options struct {
	Application           Application
	CreateQueue           *CreateQueue
	Interval              time.Duration
	ActivityFlushInterval time.Duration
	Logger                *zap.Logger
	Registerer            prometheus.Registerer
}

type Worker struct {
	app                   Application
	queue                 *CreateQueue
	interval              time.Duration
	activityFlushInterval time.Duration
	logger                *zap.Logger
	metrics               *metrics
	runMu                 sync.Mutex
	started               bool
}

func New(options Options) (*Worker, error) {
	if options.Application == nil || options.CreateQueue == nil || options.Interval <= 0 ||
		options.ActivityFlushInterval <= 0 || options.Registerer == nil {
		return nil, fmt.Errorf("worker dependencies and positive intervals are required")
	}
	logger := options.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	workerMetrics, err := newMetrics(options.Registerer)
	if err != nil {
		return nil, err
	}
	return &Worker{
		app: options.Application, queue: options.CreateQueue,
		interval: options.Interval, activityFlushInterval: options.ActivityFlushInterval,
		logger: logger, metrics: workerMetrics,
	}, nil
}

// Run starts recovery scans, create workers, activity flushing, and the
// single-leader reconciler. It returns when ctx is canceled.
func (w *Worker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("worker context is required")
	}
	w.runMu.Lock()
	if w.started {
		w.runMu.Unlock()
		return errors.New("worker can only be run once")
	}
	w.started = true
	w.runMu.Unlock()

	var wg sync.WaitGroup
	for range createWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.createLoop(ctx)
		}()
	}
	for _, run := range []func(context.Context){w.scanLoop, w.reconcileLoop, w.activityLoop} {
		wg.Add(1)
		go func(run func(context.Context)) {
			defer wg.Done()
			run(ctx)
		}(run)
	}
	<-ctx.Done()
	wg.Wait()
	return ctx.Err()
}

func (w *Worker) scanLoop(ctx context.Context) {
	if err := w.scanCreating(ctx); err != nil && ctx.Err() == nil {
		w.logger.Error("scan creating hosts failed", zap.Error(err))
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.scanCreating(ctx); err != nil && ctx.Err() == nil {
				w.logger.Error("scan creating hosts failed", zap.Error(err))
			}
		}
	}
}

func (w *Worker) scanCreating(ctx context.Context) error {
	return w.runTask("creating_scan", "", func() error {
		ids, err := w.app.ListCreatingHosts(ctx)
		if err != nil {
			return err
		}
		for _, hostID := range ids {
			if hostID == "" {
				w.logger.Warn("skip empty host id from creating scan")
				continue
			}
			if err := w.queue.Notify(ctx, hostID); err != nil {
				return fmt.Errorf("queue creating host %s: %w", hostID, err)
			}
		}
		return nil
	})
}

func (w *Worker) createLoop(ctx context.Context) {
	for {
		hostID, ok := w.queue.next(ctx)
		if !ok {
			return
		}
		var actions map[string]int
		err := w.runTask("advance_create", hostID, func() error {
			defer w.queue.finish(hostID)
			var err error
			actions, err = w.app.AdvanceCreate(ctx, hostID)
			return err
		})
		w.recordActions(actions)
		if err != nil && ctx.Err() == nil {
			w.logger.Error("advance host creation failed", zap.String("host_id", hostID), zap.Error(err))
		}
	}
}

func (w *Worker) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		var lock LeaderLock
		var acquired bool
		err := w.runTask("try_reconciler_lock", "", func() error {
			var lockErr error
			lock, acquired, lockErr = w.app.TryReconcilerLock(ctx)
			return lockErr
		})
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			w.metrics.reconcileRuns.WithLabelValues("error").Inc()
			w.logger.Error("acquire reconciler lock failed", zap.Error(err))
		case !acquired:
			w.metrics.reconcileRuns.WithLabelValues("not_leader").Inc()
		default:
			if lock == nil {
				w.metrics.reconcileRuns.WithLabelValues("error").Inc()
				w.logger.Error("acquire reconciler lock returned no lock")
				break
			}
			w.runAsLeader(ctx, lock, ticker.C)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) runAsLeader(ctx context.Context, lock LeaderLock, ticks <-chan time.Time) {
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.runTask("release_reconciler_lock", "", func() error { return lock.Release(releaseCtx) }); err != nil {
			w.logger.Error("release reconciler lock failed", zap.Error(err))
		}
	}()
	w.reconcileOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			w.reconcileOnce(ctx)
		}
	}
}

func (w *Worker) reconcileOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	var actions map[string]int
	err := w.runTask("reconcile", "", func() error {
		var err error
		actions, err = w.app.Reconcile(ctx)
		return err
	})
	w.recordActions(actions)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.metrics.reconcileRuns.WithLabelValues("error").Inc()
		w.logger.Error("reconcile failed", zap.Error(err))
		return
	}

	var counts map[host.State]int
	if err := w.runTask("host_counts", "", func() error {
		var err error
		counts, err = w.app.HostCounts(ctx)
		return err
	}); err != nil {
		if ctx.Err() != nil {
			return
		}
		w.metrics.reconcileRuns.WithLabelValues("error").Inc()
		w.logger.Error("read host counts failed", zap.Error(err))
		return
	}
	w.recordHostCounts(counts)
	w.metrics.reconcileRuns.WithLabelValues("ok").Inc()
}

func (w *Worker) recordActions(actions map[string]int) {
	for action, count := range actions {
		if !validReconcileAction(action) || count < 0 {
			w.logger.Warn("skip invalid reconcile action metric", zap.String("action", action), zap.Int("count", count))
			continue
		}
		if count > 0 {
			w.metrics.reconcileActions.WithLabelValues(action).Add(float64(count))
		}
	}
}

func (w *Worker) recordHostCounts(counts map[host.State]int) {
	for _, state := range []host.State{host.StateCreating, host.StateRunning, host.StateStopping, host.StateStopped, host.StateFailed, host.StateDeleting, host.StateDeleted} {
		count := counts[state]
		if count < 0 {
			count = 0
		}
		w.metrics.hosts.WithLabelValues(string(state)).Set(float64(count))
	}
	for state, count := range counts {
		if !validHostState(state) || count < 0 {
			w.logger.Warn("skip invalid host state metric", zap.String("state", string(state)), zap.Int("count", count))
		}
	}
}

func (w *Worker) activityLoop(ctx context.Context) {
	ticker := time.NewTicker(w.activityFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := w.runTask("flush_activity", "", func() error { return w.app.FlushActivity(ctx) })
			if err != nil && ctx.Err() == nil {
				w.logger.Error("flush host activity failed", zap.Error(err))
			}
		}
	}
}

func (w *Worker) runTask(name, hostID string, task func() error) (err error) {
	defer func() {
		if value := recover(); value != nil {
			w.logger.Error("worker task panicked", zap.String("task", name), zap.String("host_id", hostID), zap.Any("panic", value), zap.ByteString("stack", debug.Stack()))
			err = fmt.Errorf("worker task %s panicked", name)
		}
	}()
	return task()
}
