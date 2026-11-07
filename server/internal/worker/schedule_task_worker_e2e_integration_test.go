package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/notification"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

// TestScheduleWorkerOccurrenceExecutesAndNotifiesOnce covers the durable path
// from a due schedule through the ordinary Task queue to its result notice.
// The executor is deliberately local and synthetic; no model or DSH is started.
func TestScheduleWorkerOccurrenceExecutesAndNotifiesOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatalf("enable owner task execution: %v", err)
	}

	session := createF01Session(t, ctx, store, owner.ID)
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatalf("create audit service: %v", err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatalf("create credential service: %v", err)
	}
	principal, err := credentials.VerifySession(ctx, session)
	if err != nil || principal.Kind != identity.KindSession || principal.UserID != owner.ID {
		t.Fatalf("verify schedule owner session: principal=%#v err=%v", principal, err)
	}
	schedules, err := app.NewScheduleService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatalf("create schedule service: %v", err)
	}
	runAt := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Second)
	scheduled, err := schedules.Create(ctx, principal, app.CreateScheduleCommand{
		ModelID: "synthetic-local-model", Objective: "synthetic scheduled objective",
		Timing:        schedule.Timing{Kind: schedule.TimingOnce, RunAt: runAt.Format(time.RFC3339), TimeZone: "UTC"},
		Budget:        task.Budget{ActiveSeconds: 60, ToolCalls: 2, ModelTokens: 100},
		MisfirePolicy: schedule.MisfireRunOnce, NotificationEvents: []schedule.NotificationEvent{schedule.NotifyCompleted},
	}, "schedule-worker-e2e-once")
	if err != nil {
		t.Fatalf("create due one-time schedule: %v", err)
	}

	scheduleWorker, err := NewScheduleWorker(ScheduleWorkerOptions{
		Service: schedules, PollInterval: 10 * time.Millisecond, BatchSize: 10,
	})
	if err != nil {
		t.Fatalf("create schedule worker: %v", err)
	}
	scheduleCtx, stopScheduleWorker := context.WithCancel(ctx)
	scheduleDone := make(chan error, 1)
	go func() { scheduleDone <- scheduleWorker.Run(scheduleCtx) }()
	scheduleWorkerJoined := false
	t.Cleanup(func() {
		stopScheduleWorker()
		if !scheduleWorkerJoined {
			select {
			case err := <-scheduleDone:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("ScheduleWorker stopped with error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("ScheduleWorker did not stop")
			}
		}
	})
	var runs app.ScheduleRunPage
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		runs, err = schedules.Runs(ctx, principal, scheduled.ID, app.PageRequest{Limit: 10})
		if err == nil && len(runs.Items) != 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || len(runs.Items) != 1 || runs.Items[0].TaskID == nil || runs.Items[0].Status != schedule.RunQueued {
		t.Fatalf("schedule runs after ScheduleWorker processing = %#v, err=%v; want one queued Task", runs, err)
	}
	stopScheduleWorker()
	select {
	case err := <-scheduleDone:
		scheduleWorkerJoined = true
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ScheduleWorker.Run() = %v, want context canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ScheduleWorker did not stop after cancellation")
	}
	if processed, err := schedules.ProcessDue(ctx, time.Now().UTC(), 10); err != nil || processed != 0 {
		t.Fatalf("ProcessDue() after ScheduleWorker = processed %d, err=%v; want no duplicate occurrence", processed, err)
	}
	taskID := *runs.Items[0].TaskID

	executor := syntheticScheduleResultExecutor{store: store, result: "synthetic local result"}
	taskWorker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: &executor, WorkerID: "schedule-e2e-worker",
		LeaseDuration: 5 * time.Second, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create task worker: %v", err)
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- taskWorker.Run(workerCtx) }()
	defer func() {
		stopWorker()
		select {
		case err := <-workerDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("stop TaskWorker: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("TaskWorker did not stop")
		}
	}()

	taskDeadline := time.Now().Add(15 * time.Second)
	var details app.TaskDetails
	for time.Now().Before(taskDeadline) {
		details, err = store.GetTask(ctx, taskID, owner.ID)
		if err == nil && details.Task.Status == task.StatusCompleted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || details.Task.Status != task.StatusCompleted || details.Task.Result != executor.result {
		t.Fatalf("scheduled Task result = status %q, result %q, err=%v; want completed synthetic result", details.Task.Status, details.Task.Result, err)
	}
	if executor.executions() != 1 {
		t.Fatalf("synthetic executor ran %d times; want once", executor.executions())
	}

	runs, err = schedules.Runs(ctx, principal, scheduled.ID, app.PageRequest{Limit: 10})
	if err != nil || len(runs.Items) != 1 || runs.Items[0].TaskID == nil || *runs.Items[0].TaskID != taskID || runs.Items[0].Status != schedule.RunCompleted {
		t.Fatalf("schedule runs after Task completion = %#v, err=%v; want one completed run", runs, err)
	}

	notifications, err := app.NewNotificationService(store)
	if err != nil {
		t.Fatalf("create notification service: %v", err)
	}
	if count, err := notifications.ProcessPending(ctx, 10); err != nil || count == 0 {
		t.Fatalf("ProcessPending() = consumed %d, err=%v; want persisted result notification", count, err)
	}
	page, err := notifications.List(ctx, principal, false, app.PageRequest{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].TaskID != taskID || page.Items[0].Kind != notification.KindCompleted {
		t.Fatalf("owner notification page = %#v, err=%v; want completed notice for scheduled Task", page, err)
	}
}

type syntheticScheduleResultExecutor struct {
	store interface {
		CommitTaskProgress(context.Context, app.TaskProgress) (task.Event, error)
	}
	result string
	count  atomic.Int32
}

func (e *syntheticScheduleResultExecutor) ExecuteTask(ctx context.Context, claimed task.Task) error {
	e.count.Add(1)
	_, err := e.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusCompleted, Result: e.result,
		Event: task.Event{Type: "task.result", Payload: []byte(`{"result":"synthetic local result"}`)},
	})
	return err
}

func (e *syntheticScheduleResultExecutor) executions() int { return int(e.count.Load()) }
