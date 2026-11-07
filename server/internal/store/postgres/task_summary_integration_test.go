package postgres

import (
	"context"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskSummaryStepCountsExcludeBudget(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "task-summary-step-counts", "Check task summary counts"))
	if err != nil {
		t.Fatalf("submit Task: %v", err)
	}
	for _, step := range []struct {
		sequence int64
		kind     string
		status   string
	}{
		{1, "plan", string(task.StepSucceeded)},
		{2, "budget", string(task.StepSucceeded)},
		{3, "model", string(task.StepRunning)},
		{4, "tool", string(task.StepFailed)},
	} {
		stepID := mustID(t, domain.NewStepID)
		if _, err := pool.Exec(ctx, `INSERT INTO task_steps (id, task_id, sequence, kind, status) VALUES ($1, $2, $3, $4, $5)`,
			stepID, created.Task.ID, step.sequence, step.kind, step.status); err != nil {
			t.Fatalf("insert Task step %q: %v", step.kind, err)
		}
	}

	checkCounts := func(items []task.Task) {
		t.Helper()
		if len(items) != 1 || items[0].ID != created.Task.ID || items[0].StepCount != 3 || items[0].CompletedStepCount != 1 {
			t.Fatalf("Task summary step counts = %#v; want total 3, completed 1", items)
		}
	}

	page, err := store.ListTasks(ctx, owner.ID, app.TaskListRequest{Page: app.PageRequest{Limit: 20}})
	if err != nil {
		t.Fatalf("list Tasks: %v", err)
	}
	checkCounts(page.Items)

	active, err := store.ListConversationActiveTasks(ctx, owner.ID, "", created.Conversation.ID, nil, app.PageRequest{Limit: 20})
	if err != nil {
		t.Fatalf("list active conversation Tasks: %v", err)
	}
	checkCounts(active.Items)
}
