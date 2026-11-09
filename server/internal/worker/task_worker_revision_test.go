package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type revisionWitnessTaskStore struct {
	*taskLeaseStoreStub
	proven bool
	err    error
}

type revisionWitnessFunc func(context.Context, task.Task) (bool, error)

func (f revisionWitnessFunc) RevisionStopProven(ctx context.Context, value task.Task) (bool, error) {
	return f(ctx, value)
}

func (s *revisionWitnessTaskStore) RevisionStopProven(context.Context, task.Task) (bool, error) {
	return s.proven, s.err
}

func TestPendingDeliveryFailureUsesOnlyProvenRevisionCause(t *testing.T) {
	for _, test := range []struct {
		name                                   string
		proven, witnessError, other, lifecycle bool
		cancelTask, stopService                bool
		wantRevision                           bool
	}{
		{name: "proven revision", proven: true, lifecycle: true, wantRevision: true},
		{name: "unproven authorization", lifecycle: true},
		{name: "witness unavailable", proven: true, witnessError: true, lifecycle: true},
		{name: "other delivery error", proven: true, other: true, lifecycle: true},
		{name: "missing lifecycle", proven: true},
		{name: "stopped lifecycle", proven: true, lifecycle: true, stopService: true},
		{name: "task already stopped", proven: true, lifecycle: true, cancelTask: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, _, _, _, _, value := cloudDSHFixture(t)
			executor.options.RevisionStops = revisionWitnessFunc(func(context.Context, task.Task) (bool, error) {
				if test.witnessError {
					return false, errors.New("witness unavailable")
				}
				return test.proven, nil
			})
			service, stopService := context.WithCancel(context.Background())
			defer stopService()
			ctx, stopTask := context.WithCancel(context.Background())
			defer stopTask()
			if test.lifecycle {
				ctx = app.WithModelTaskLifecycle(ctx, service)
			}
			if test.stopService {
				stopService()
			}
			if test.cancelTask {
				stopTask()
			}
			cause := error(domain.NewError(domain.CodeUnauthenticated, "authorization unavailable"))
			if test.other {
				cause = errors.New("input pipe unavailable")
			}
			got := executor.pendingDeliveryFailureCause(ctx, value, cause)
			if errors.Is(got, app.ErrTaskRevisionInterrupted) != test.wantRevision || !test.wantRevision && !errors.Is(got, cause) {
				t.Fatalf("delivery failure cause = %v", got)
			}
		})
	}
}

func TestCloudDSHInstanceRenewalPreservesOnlyProvenRevisionCause(t *testing.T) {
	for _, test := range []struct {
		name                  string
		withLifecycle, proven bool
	}{
		{"proven revision", true, true},
		{"ordinary renewal failure", true, false},
		{"missing service lifecycle", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor, _, _, _, _, value := cloudDSHFixture(t)
			executor.options.RevisionStops = revisionWitnessFunc(func(context.Context, task.Task) (bool, error) { return test.proven, nil })
			service, stopService := context.WithCancel(context.Background())
			defer stopService()
			base := context.Background()
			if test.withLifecycle {
				base = app.WithModelTaskLifecycle(base, service)
			}
			ctx, cancel := context.WithCancelCause(base)
			defer cancel(context.Canceled)
			done := make(chan struct{})
			go func() {
				executor.renewCloudDSHLease(ctx, cancel, value, app.AgentInstanceFence{}, time.Now().Add(60*time.Millisecond), nil)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("renewal check did not stop")
			}
			if got := errors.Is(context.Cause(ctx), app.ErrTaskRevisionInterrupted); got != (test.withLifecycle && test.proven) {
				t.Fatalf("renewal cancellation cause = %v", context.Cause(ctx))
			}
		})
	}
}

func TestTaskWorkerUsesRevisionCauseOnlyWithVerifiedWitness(t *testing.T) {
	for _, test := range []struct {
		name         string
		proven       bool
		err          error
		wantRevision bool
	}{
		{"verified revision", true, nil, true},
		{"ordinary lost lease", false, nil, false},
		{"proof unavailable", true, errors.New("witness unavailable"), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &revisionWitnessTaskStore{taskLeaseStoreStub: &taskLeaseStoreStub{
				renew: func(context.Context, string, string, int64, time.Duration) (task.Task, error) {
					return task.Task{}, errors.New("old lease fenced")
				}}, proven: test.proven, err: test.err}
			observed := make(chan error, 1)
			var lifecycle context.Context
			executor := taskExecutorFunc(func(ctx context.Context, _ task.Task) error {
				lifecycle = app.ModelTaskLifecycle(ctx)
				<-ctx.Done()
				observed <- context.Cause(ctx)
				return ctx.Err()
			})
			worker := newTestTaskWorker(t, store, executor, 30*time.Millisecond, time.Millisecond)
			service, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := worker.runClaimed(service, validTaskClaim("worker-test")); err == nil {
				t.Fatal("lost lease did not stop the worker")
			}
			cause := <-observed
			if errors.Is(cause, app.ErrTaskRevisionInterrupted) != test.wantRevision {
				t.Fatalf("cancellation cause = %v", cause)
			}
			if lifecycle == nil || lifecycle.Err() != nil {
				t.Fatal("task cancellation incorrectly cancelled the independent lifecycle")
			}
			cancel()
			if lifecycle.Err() == nil {
				t.Fatal("worker shutdown did not cancel the model lifecycle")
			}
		})
	}
}
