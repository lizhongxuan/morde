package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func TestTaskWorkerExecutesSameConversationTasksConcurrentlyAndSeparately(t *testing.T) {
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
		t.Fatalf("verify owner session: principal=%#v err=%v", principal, err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatalf("create Task service: %v", err)
	}
	budget := task.Budget{ActiveSeconds: 60, ToolCalls: 2, ModelTokens: 100}
	first, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "synthetic-local-model", Objective: "parallel conversation task alpha", Budget: budget,
		IdempotencyKey: "parallel-conversation-alpha",
	})
	if err != nil {
		t.Fatalf("submit first conversation Task: %v", err)
	}
	second, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ConversationID: first.Conversation.ID, ModelID: "synthetic-local-model", Objective: "parallel conversation task beta",
		Budget: budget, IdempotencyKey: "parallel-conversation-beta",
	})
	if err != nil {
		t.Fatalf("submit second conversation Task: %v", err)
	}
	if first.Task.ID == second.Task.ID || first.Task.ConversationID != second.Task.ConversationID || first.Task.ConversationID != first.Conversation.ID {
		t.Fatalf("conversation submissions are not separate Tasks in one conversation: first=%+v second=%+v", first.Task, second.Task)
	}

	started := make(chan task.Task, 2)
	release := make(chan struct{})
	executor := taskExecutorFunc(func(executionCtx context.Context, claimed task.Task) error {
		select {
		case started <- claimed:
		case <-executionCtx.Done():
			return executionCtx.Err()
		}
		select {
		case <-release:
		case <-executionCtx.Done():
			return executionCtx.Err()
		}
		payload, err := json.Marshal(map[string]string{"result": "completed:" + claimed.ID})
		if err != nil {
			return fmt.Errorf("encode synthetic result: %w", err)
		}
		_, err = store.CommitTaskProgress(executionCtx, app.TaskProgress{
			TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
			Status: task.StatusCompleted, Result: "completed:" + claimed.ID,
			Event: task.Event{Type: "task.result", Payload: payload},
		})
		return err
	})
	worker, err := NewTaskWorker(TaskWorkerOptions{
		Store: store, Executor: executor, WorkerID: "same-conversation-worker", LeaseDuration: 15 * time.Second,
		PollInterval: 5 * time.Millisecond, MaxConcurrentTasks: 2, MaxConcurrentTasksPerOwner: 2, Logger: zap.NewNop(),
	})
	if err != nil {
		t.Fatalf("create TaskWorker: %v", err)
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	joined := false
	t.Cleanup(func() {
		stopWorker()
		if !joined {
			select {
			case runErr := <-workerDone:
				if !errors.Is(runErr, context.Canceled) {
					t.Errorf("TaskWorker stopped with error: %v", runErr)
				}
			case <-time.After(5 * time.Second):
				t.Error("TaskWorker did not stop")
			}
		}
	})

	seen := make(map[string]task.Task, 2)
	for range 2 {
		select {
		case claimed := <-started:
			if _, duplicate := seen[claimed.ID]; duplicate {
				t.Fatalf("TaskWorker started Task %q more than once", claimed.ID)
			}
			seen[claimed.ID] = claimed
		case <-ctx.Done():
			t.Fatalf("Tasks from the same conversation did not run concurrently: %v", ctx.Err())
		}
	}
	for taskID, objective := range map[string]string{
		first.Task.ID:  "parallel conversation task alpha",
		second.Task.ID: "parallel conversation task beta",
	} {
		claimed, ok := seen[taskID]
		if !ok || claimed.OwnerID != owner.ID || claimed.ConversationID != first.Conversation.ID || claimed.Objective != objective {
			t.Fatalf("claimed Task %q = %+v found=%t; want its own owner, conversation, and objective", taskID, claimed, ok)
		}
		if claimed.ConversationContext == nil || claimed.ConversationContext.ConversationID != first.Conversation.ID {
			t.Fatalf("Task %q did not receive its fixed conversation context: %+v", taskID, claimed.ConversationContext)
		}
		for _, message := range claimed.ConversationContext.Messages {
			if message.Content == "parallel conversation task alpha" || message.Content == "parallel conversation task beta" {
				t.Fatalf("Task %q received an unfinished sibling instruction in its context: %+v", taskID, message)
			}
		}
		details, err := store.GetTask(ctx, taskID, owner.ID)
		if err != nil || details.Task.Status != task.StatusRunning {
			t.Fatalf("Task %q while sibling is running = status %q, err=%v", taskID, details.Task.Status, err)
		}
	}

	close(release)
	for _, taskID := range []string{first.Task.ID, second.Task.ID} {
		if err := waitF01(t, ctx, func() (bool, error) {
			details, err := store.GetTask(ctx, taskID, owner.ID)
			return err == nil && details.Task.Status == task.StatusCompleted, err
		}, nil, nil); err != nil {
			t.Fatalf("wait for Task %q completion: %v", taskID, err)
		}
		details, err := store.GetTask(ctx, taskID, owner.ID)
		if err != nil || details.Task.Result != "completed:"+taskID {
			t.Fatalf("Task %q result = %q err=%v; sibling result was mixed", taskID, details.Task.Result, err)
		}
	}
}
