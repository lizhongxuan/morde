package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestTaskQueuePriorityFencingAndFairClaimOrder(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)

	create := func(key, objective string) task.Task {
		t.Helper()
		created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", key, objective))
		if err != nil {
			t.Fatalf("submit %s task: %v", key, err)
		}
		return created.Task
	}
	agedBackground := create("priority-aged", "aged background")
	pinned := create("priority-pinned", "pinned")
	firstNormal := create("priority-normal-first", "first normal")
	secondNormal := create("priority-normal-second", "second normal")
	freshBackground := create("priority-background-fresh", "fresh background")

	agedBackground = setTestTaskPriority(t, store, owner.ID, agedBackground, task.QueuePriorityBackground, "priority-set-aged")
	pinned = setTestTaskPriority(t, store, owner.ID, pinned, task.QueuePriorityPinned, "priority-set-pinned")
	freshBackground = setTestTaskPriority(t, store, owner.ID, freshBackground, task.QueuePriorityBackground, "priority-set-background")

	// Backdate one low-priority Task to prove aging outranks fresh pinned work.
	createdAt := time.Now().UTC().Add(-20 * time.Minute)
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET created_at=$2 WHERE id=$1`, agedBackground.ID, createdAt); err != nil {
		t.Fatalf("backdate background task: %v", err)
	}
	for id, offset := range map[string]time.Duration{
		pinned.ID:          -4 * time.Minute,
		firstNormal.ID:     -3 * time.Minute,
		secondNormal.ID:    -2 * time.Minute,
		freshBackground.ID: -time.Minute,
	} {
		if _, err := store.pool.Exec(ctx, `UPDATE tasks SET created_at=$2 WHERE id=$1`, id, time.Now().UTC().Add(offset)); err != nil {
			t.Fatalf("set deterministic queue time for %s: %v", id, err)
		}
	}

	replayRecord := testTaskPriorityIdempotency(t, owner.ID, pinned.ID, task.QueuePriorityPinned, 1, "priority-set-pinned")
	updated, err := store.SetTaskPriority(ctx, replayRecord,
		pinned.ID, task.QueuePriorityPinned, 1, replayRecord.ResourceID)
	if err != nil || updated.QueuePriority != task.QueuePriorityPinned || updated.EventSequence != 2 {
		t.Fatalf("idempotent priority replay = %#v, err=%v", updated, err)
	}
	page, err := store.GetTaskEventPage(ctx, owner.ID, pinned.ID, 1, 10)
	if err != nil || page.CurrentSequence != 2 || len(page.Items) != 1 || page.Items[0].Type != "task.priority" {
		t.Fatalf("priority event page = %#v, err=%v", page, err)
	}
	staleRecord := testTaskPriorityIdempotency(t, owner.ID, pinned.ID, task.QueuePriorityBackground, 1, "priority-stale")
	if _, err := store.SetTaskPriority(ctx, staleRecord,
		pinned.ID, task.QueuePriorityBackground, 1, staleRecord.ResourceID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale event sequence priority update error = %v, want conflict", err)
	}

	want := []string{agedBackground.ID, pinned.ID, firstNormal.ID, secondNormal.ID, freshBackground.ID}
	for index, taskID := range want {
		claimed, found, err := store.ClaimNextTask(ctx, fmt.Sprintf("queue-priority-worker-%d", index), time.Minute)
		if err != nil || !found || claimed.ID != taskID {
			t.Fatalf("claim %d = %s found=%t err=%v, want %s", index, claimed.ID, found, err, taskID)
		}
		if claimed.QueueEnteredAt.IsZero() || claimed.QueueEnteredAt.After(time.Now().UTC().Add(time.Second)) {
			t.Fatalf("claim %d queue entry timestamp = %s, want a durable pre-claim time", index, claimed.QueueEnteredAt)
		}
		if index == 1 {
			runningRecord := testTaskPriorityIdempotency(t, owner.ID, pinned.ID, task.QueuePriorityNormal, updated.EventSequence, "priority-running")
			if _, err := store.SetTaskPriority(ctx, runningRecord,
				pinned.ID, task.QueuePriorityNormal, updated.EventSequence, runningRecord.ResourceID); domain.CodeOf(err) != domain.CodeConflict {
				t.Fatalf("running task priority update error = %v, want conflict", err)
			}
		}
	}
}

func TestConcurrentTaskClaimsPreservePriorityAndEventuallyClaimBackground(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	create := func(key string) task.Task {
		t.Helper()
		created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", key, key))
		if err != nil {
			t.Fatalf("submit %s task: %v", key, err)
		}
		return created.Task
	}
	agedBackground := setTestTaskPriority(t, store, owner.ID, create("concurrent-aged-background"), task.QueuePriorityBackground, "concurrent-aged-background-priority")
	firstPinned := setTestTaskPriority(t, store, owner.ID, create("concurrent-pinned-first"), task.QueuePriorityPinned, "concurrent-pinned-first-priority")
	secondPinned := setTestTaskPriority(t, store, owner.ID, create("concurrent-pinned-second"), task.QueuePriorityPinned, "concurrent-pinned-second-priority")
	normal := create("concurrent-normal")
	freshBackground := setTestTaskPriority(t, store, owner.ID, create("concurrent-background-fresh"), task.QueuePriorityBackground, "concurrent-background-fresh-priority")

	queuedAt := time.Now().UTC()
	for id, offset := range map[string]time.Duration{
		agedBackground.ID:  -20 * time.Minute,
		firstPinned.ID:     -4 * time.Minute,
		secondPinned.ID:    -3 * time.Minute,
		normal.ID:          -2 * time.Minute,
		freshBackground.ID: -time.Minute,
	} {
		if _, err := store.pool.Exec(ctx, `UPDATE tasks SET created_at=$2 WHERE id=$1`, id, queuedAt.Add(offset)); err != nil {
			t.Fatalf("set deterministic queue time for %s: %v", id, err)
		}
	}

	type claimResult struct {
		claimed task.Task
		found   bool
		err     error
	}
	const workers = 2
	ready := make(chan struct{}, workers)
	start := make(chan struct{})
	results := make(chan claimResult, workers)
	for worker := 0; worker < workers; worker++ {
		go func(worker int) {
			ready <- struct{}{}
			<-start
			claimed, found, err := store.ClaimNextTask(ctx, fmt.Sprintf("concurrent-priority-worker-%d", worker), time.Minute)
			results <- claimResult{claimed: claimed, found: found, err: err}
		}(worker)
	}
	for worker := 0; worker < workers; worker++ {
		<-ready
	}
	close(start)

	claimedIDs := make(map[string]bool, workers)
	for worker := 0; worker < workers; worker++ {
		result := <-results
		if result.err != nil || !result.found {
			t.Fatalf("concurrent worker claim = task %s found=%t err=%v", result.claimed.ID, result.found, result.err)
		}
		if claimedIDs[result.claimed.ID] {
			t.Fatalf("concurrent workers claimed task %s more than once", result.claimed.ID)
		}
		claimedIDs[result.claimed.ID] = true
	}
	if !claimedIDs[agedBackground.ID] || !claimedIDs[firstPinned.ID] || len(claimedIDs) != workers {
		t.Fatalf("concurrent claims = %v, want aged background %s and first pinned %s", claimedIDs, agedBackground.ID, firstPinned.ID)
	}

	for _, want := range []string{secondPinned.ID, normal.ID, freshBackground.ID} {
		claimed, found, err := store.ClaimNextTask(ctx, "concurrent-priority-follow-up", time.Minute)
		if err != nil || !found || claimed.ID != want {
			t.Fatalf("follow-up claim = task %s found=%t err=%v, want %s", claimed.ID, found, err, want)
		}
	}
}

func TestClaimedTaskQueueEnteredAtUsesLatestQueuedStatusEvent(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	create := func(key string) task.Task {
		t.Helper()
		created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", key, "queue timestamp"))
		if err != nil {
			t.Fatalf("submit %s task: %v", key, err)
		}
		return created.Task
	}
	latestQueuedStatusAt := func(taskID string) time.Time {
		t.Helper()
		var createdAt time.Time
		if err := store.pool.QueryRow(ctx, `SELECT created_at FROM task_events
WHERE task_id=$1 AND event_type='task.status' AND payload->>'status'='queued'
ORDER BY sequence DESC LIMIT 1`, taskID).Scan(&createdAt); err != nil {
			t.Fatalf("read latest queued status event for %s: %v", taskID, err)
		}
		return createdAt
	}

	priorityTask := create("queue-time-priority")
	initialQueuedAt := latestQueuedStatusAt(priorityTask.ID)
	priorityTask = setTestTaskPriority(t, store, owner.ID, priorityTask, task.QueuePriorityPinned, "queue-time-priority-set")
	if afterPriorityAt := latestQueuedStatusAt(priorityTask.ID); !afterPriorityAt.Equal(initialQueuedAt) {
		t.Fatalf("priority update changed latest queued status timestamp: before=%s after=%s", initialQueuedAt, afterPriorityAt)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "queue-time-priority-worker", time.Minute)
	if err != nil || !found || claimed.ID != priorityTask.ID || !claimed.QueueEnteredAt.Equal(initialQueuedAt) {
		t.Fatalf("priority claim queue entry = %s found=%t err=%v, want original queued event %s", claimed.QueueEnteredAt, found, err, initialQueuedAt)
	}

	reenteredTask := create("queue-time-reentry")
	pauseID := mustTaskActionID(t)
	pauseRecord := taskActionIdempotency(owner.ID, task.ActionPause, "queue-time-reentry-pause", reenteredTask.ID, task.StatusQueued, pauseID)
	if result, err := store.RequestTaskAction(ctx, pauseRecord, reenteredTask.ID, task.ActionPause, task.StatusQueued, nil, pauseID); err != nil || result.Task.Status != task.StatusPausing {
		t.Fatalf("request queued task pause = %#v, err=%v", result, err)
	}
	stopClaim, found, err := store.ClaimNextTaskStop(ctx, "queue-time-reentry-stop-worker", time.Minute)
	if err != nil || !found || stopClaim.Request.ID != pauseID {
		t.Fatalf("claim queued task pause = %#v found=%t err=%v", stopClaim, found, err)
	}
	if paused, err := store.CompleteTaskStop(ctx, reenteredTask.ID, pauseID, "queue-time-reentry-stop-worker", stopClaim.Request.LeaseVersion); err != nil || paused.Status != task.StatusPaused {
		t.Fatalf("complete queued task pause = %#v, err=%v", paused, err)
	}
	resumeID := mustTaskActionID(t)
	resumeRecord := taskActionIdempotency(owner.ID, task.ActionResume, "queue-time-reentry-resume", reenteredTask.ID, task.StatusPaused, resumeID)
	if result, err := store.RequestTaskAction(ctx, resumeRecord, reenteredTask.ID, task.ActionResume, task.StatusPaused, nil, resumeID); err != nil || result.Task.Status != task.StatusQueued {
		t.Fatalf("resume task = %#v, err=%v", result, err)
	}
	reenteredAt := latestQueuedStatusAt(reenteredTask.ID)
	claimed, found, err = store.ClaimNextTask(ctx, "queue-time-reentry-worker", time.Minute)
	if err != nil || !found || claimed.ID != reenteredTask.ID || !claimed.QueueEnteredAt.Equal(reenteredAt) {
		t.Fatalf("re-entry claim queue entry = %s found=%t err=%v, want latest queued event %s", claimed.QueueEnteredAt, found, err, reenteredAt)
	}
}

func setTestTaskPriority(t *testing.T, store *Store, ownerID string, current task.Task,
	priority task.QueuePriority, key string) task.Task {
	t.Helper()
	record := testTaskPriorityIdempotency(t, ownerID, current.ID, priority, current.EventSequence, key)
	updated, err := store.SetTaskPriority(context.Background(), record,
		current.ID, priority, current.EventSequence, record.ResourceID)
	if err != nil {
		t.Fatalf("set priority %s for task %s: %v", priority, current.ID, err)
	}
	return updated
}

func testTaskPriorityIdempotency(t *testing.T, ownerID, taskID string, priority task.QueuePriority,
	expectedSequence int64, key string) app.IdempotencyRecord {
	t.Helper()
	requestID := mustTaskActionID(t)
	return app.IdempotencyRecord{
		OwnerID: ownerID, Operation: "task.priority.set", Key: key,
		RequestHash: hashBytes(fmt.Sprintf("%s:%s:%d", taskID, priority, expectedSequence)),
		ResourceID:  requestID, RelatedID: taskID,
	}
}

func mustTaskActionID(t *testing.T) string {
	t.Helper()
	id, err := domain.NewTaskActionID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
