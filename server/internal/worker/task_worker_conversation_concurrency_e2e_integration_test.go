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
	var executionState string
	if err := pool.QueryRow(ctx, `SELECT state FROM owner_execution_controls WHERE owner_id=$1`, owner.ID).Scan(&executionState); err != nil {
		t.Fatalf("read new owner's default execution state: %v", err)
	}
	if executionState != "enabled" {
		t.Fatalf("new owner's default execution state = %q, want enabled", executionState)
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
	objectives := []string{
		"parallel conversation task alpha", "parallel conversation task beta",
		"parallel conversation task gamma", "parallel conversation task delta",
	}
	submissions := []app.TaskSubmissionResult{first}
	for i, objective := range objectives[1:] {
		submission, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
			ConversationID: first.Conversation.ID, ModelID: "synthetic-local-model", Objective: objective,
			Budget: budget, IdempotencyKey: fmt.Sprintf("parallel-conversation-%d", i+2),
		})
		if err != nil {
			t.Fatalf("submit conversation Task %q: %v", objective, err)
		}
		submissions = append(submissions, submission)
	}
	for _, submission := range submissions[1:] {
		if submission.Task.ID == first.Task.ID || submission.Task.ConversationID != first.Task.ConversationID || submission.Task.ConversationID != first.Conversation.ID {
			t.Fatalf("conversation submissions are not separate Tasks in one conversation: first=%+v other=%+v", first.Task, submission.Task)
		}
	}

	started := make(chan task.Task, len(submissions))
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
		PollInterval: 5 * time.Millisecond, MaxConcurrentTasks: len(submissions), MaxConcurrentTasksPerOwner: len(submissions), Logger: zap.NewNop(),
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

	seen := make(map[string]task.Task, len(submissions))
	for range len(submissions) {
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
	taskObjectives := make(map[string]string, len(submissions))
	for i, submission := range submissions {
		taskObjectives[submission.Task.ID] = objectives[i]
	}
	for taskID, objective := range taskObjectives {
		claimed, ok := seen[taskID]
		if !ok || claimed.OwnerID != owner.ID || claimed.ConversationID != first.Conversation.ID || claimed.Objective != objective {
			t.Fatalf("claimed Task %q = %+v found=%t; want its own owner, conversation, and objective", taskID, claimed, ok)
		}
		if claimed.ConversationContext == nil || claimed.ConversationContext.ConversationID != first.Conversation.ID {
			t.Fatalf("Task %q did not receive its fixed conversation context: %+v", taskID, claimed.ConversationContext)
		}
		for _, message := range claimed.ConversationContext.Messages {
			for _, siblingObjective := range objectives {
				if message.Content == siblingObjective {
					t.Fatalf("Task %q received an unfinished sibling instruction in its context: %+v", taskID, message)
				}
			}
		}
		details, err := store.GetTask(ctx, taskID, owner.ID)
		if err != nil || details.Task.Status != task.StatusRunning {
			t.Fatalf("Task %q while sibling is running = status %q, err=%v", taskID, details.Task.Status, err)
		}
	}

	close(release)
	for taskID := range taskObjectives {
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
