package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresConversationTaskSubmissionsSerializeAndIndependentConversationsOverlap(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	otherPool, err := pgxpool.NewWithConfig(ctx, pool.Config().Copy())
	if err != nil {
		t.Fatalf("open second Portal database pool: %v", err)
	}
	t.Cleanup(otherPool.Close)
	otherStore := New(otherPool)
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)

	conversationA := createMainConversationForTest(t, store, owner.ID)
	conversationB := createMainConversationForTest(t, store, owner.ID)
	const advisoryLockKey int64 = 824726195
	if _, err := pool.Exec(ctx, `CREATE FUNCTION wait_for_conversation_task_insert_test_release() RETURNS trigger AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(824726195);
  RETURN NEW;
END;
$$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create task insert barrier: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER wait_for_conversation_task_insert_test_release
BEFORE INSERT ON tasks FOR EACH ROW EXECUTE FUNCTION wait_for_conversation_task_insert_test_release()`); err != nil {
		t.Fatalf("install task insert barrier: %v", err)
	}
	advisoryConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire PostgreSQL advisory lock connection: %v", err)
	}
	defer func() {
		_, _ = advisoryConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
		advisoryConn.Release()
	}()
	if _, err := advisoryConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		t.Fatalf("hold task insert advisory lock: %v", err)
	}

	type submissionResult struct {
		value app.TaskSubmissionResult
		err   error
	}
	submissions := []struct {
		store      *Store
		submission app.TaskSubmission
	}{}
	for _, candidate := range []struct {
		store          *Store
		conversationID string
		key            string
	}{{store, conversationA.ID, "independent-task-a"}, {otherStore, conversationB.ID, "independent-task-b"}} {
		submission := newTestTaskSubmission(t, owner.ID, "", candidate.key, candidate.key)
		submission.Task.ConversationID, submission.Message.ConversationID = candidate.conversationID, candidate.conversationID
		submissions = append(submissions, struct {
			store      *Store
			submission app.TaskSubmission
		}{candidate.store, submission})
	}
	start := make(chan struct{})
	results := make(chan submissionResult, len(submissions))
	var workers sync.WaitGroup
	for _, candidate := range submissions {
		candidate := candidate
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			value, err := candidate.store.SubmitTask(ctx, candidate.submission)
			results <- submissionResult{value: value, err: err}
		}()
	}
	waitCtx, cancelWait := context.WithTimeout(ctx, 5*time.Second)
	defer cancelWait()
	close(start)
	waitForPostgresLockWait(t, waitCtx, pool, "%INSERT INTO tasks%", 2)
	var unlocked bool
	if err := advisoryConn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, advisoryLockKey).Scan(&unlocked); err != nil || !unlocked {
		t.Fatalf("release task insert advisory lock: unlocked=%t err=%v", unlocked, err)
	}
	workers.Wait()
	close(results)
	for result := range results {
		if result.err != nil || result.value.Task.ID == "" {
			t.Fatalf("independent conversation submission = %#v, err=%v", result.value.Task, result.err)
		}
	}

	conversationC := createMainConversationForTest(t, store, owner.ID)
	sameConversationSubmissions := []struct {
		store      *Store
		submission app.TaskSubmission
	}{}
	for _, candidate := range []struct {
		store *Store
		key   string
	}{{store, "same-task-a"}, {otherStore, "same-task-b"}} {
		submission := newTestTaskSubmission(t, owner.ID, "", candidate.key, candidate.key)
		submission.Task.ConversationID, submission.Message.ConversationID = conversationC.ID, conversationC.ID
		sameConversationSubmissions = append(sameConversationSubmissions, struct {
			store      *Store
			submission app.TaskSubmission
		}{candidate.store, submission})
	}
	start = make(chan struct{})
	results = make(chan submissionResult, len(sameConversationSubmissions))
	workers = sync.WaitGroup{}
	for _, candidate := range sameConversationSubmissions {
		candidate := candidate
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			value, err := candidate.store.SubmitTask(ctx, candidate.submission)
			results <- submissionResult{value: value, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	successes := 0
	for result := range results {
		if result.err == nil {
			successes++
		} else {
			t.Fatalf("same conversation submission error = %v", result.err)
		}
	}
	if successes != 2 {
		t.Fatalf("same conversation submissions: success=%d, want 2", successes)
	}
	var active int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE conversation_id=$1 AND owner_id=$2
AND status IN ('queued','running','waiting_user','pausing','paused','cancelling')`, conversationC.ID, owner.ID).Scan(&active); err != nil || active != 2 {
		t.Fatalf("active tasks in same conversation=%d err=%v, want two", active, err)
	}
}

func TestPostgresPausedTaskResumeRacesWithSameConversationSubmission(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	otherPool, err := pgxpool.NewWithConfig(ctx, pool.Config().Copy())
	if err != nil {
		t.Fatalf("open second Portal database pool: %v", err)
	}
	t.Cleanup(otherPool.Close)
	otherStore := New(otherPool)
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	conversationValue := createMainConversationForTest(t, store, owner.ID)
	initial := newTestTaskSubmission(t, owner.ID, "", "paused-conversation-task", "initial task")
	initial.Task.ConversationID = conversationValue.ID
	initial.Message.ConversationID = conversationValue.ID
	created, err := store.SubmitTask(ctx, initial)
	if err != nil {
		t.Fatalf("submit initial task: %v", err)
	}
	pauseID := mustID(t, domain.NewTaskActionID)
	pause, err := store.RequestTaskAction(ctx,
		taskActionIdempotency(owner.ID, task.ActionPause, "pause-for-resume-race", created.Task.ID, task.StatusQueued, pauseID),
		created.Task.ID, task.ActionPause, task.StatusQueued, pauseID)
	if err != nil || pause.Task.Status != task.StatusPausing {
		t.Fatalf("request pause: task=%q err=%v", pause.Task.Status, err)
	}
	stopClaim, found, err := store.ClaimNextTaskStop(ctx, "resume-race-stop-worker", time.Minute)
	if err != nil || !found || stopClaim.Request.ID != pauseID {
		t.Fatalf("claim pause: found=%t request=%q err=%v", found, stopClaim.Request.ID, err)
	}
	paused, err := store.CompleteTaskStop(ctx, created.Task.ID, pauseID, "resume-race-stop-worker", stopClaim.Request.LeaseVersion)
	if err != nil || paused.Status != task.StatusPaused {
		t.Fatalf("complete pause: status=%q err=%v", paused.Status, err)
	}

	resumeID := mustID(t, domain.NewTaskActionID)
	resumeRecord := taskActionIdempotency(owner.ID, task.ActionResume, "resume-same-conversation-race", created.Task.ID, task.StatusPaused, resumeID)
	newSubmission := newTestTaskSubmission(t, owner.ID, "", "submit-during-resume", "competing task")
	newSubmission.Task.ConversationID = conversationValue.ID
	newSubmission.Message.ConversationID = conversationValue.ID
	type outcome struct {
		resumed bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		<-start
		_, err := store.RequestTaskAction(ctx, resumeRecord, created.Task.ID, task.ActionResume, task.StatusPaused, resumeID)
		results <- outcome{resumed: err == nil, err: err}
	}()
	go func() {
		defer workers.Done()
		<-start
		_, err := otherStore.SubmitTask(ctx, newSubmission)
		results <- outcome{err: err}
	}()
	close(start)
	workers.Wait()
	close(results)
	resumeCount, submitCount := 0, 0
	for result := range results {
		if result.resumed {
			resumeCount++
		} else if result.err == nil {
			submitCount++
		} else {
			t.Fatalf("resume/submit race returned unexpected error: %v", result.err)
		}
	}
	if resumeCount != 1 || submitCount != 1 {
		t.Fatalf("resume/submit race: resumed=%d submitted=%d, want 1/1", resumeCount, submitCount)
	}
	var status string
	var active int
	if err := pool.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1`, created.Task.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE conversation_id=$1 AND owner_id=$2
AND status IN ('queued','running','waiting_user','pausing','paused','cancelling')`, conversationValue.ID, owner.ID).Scan(&active); err != nil || active != 2 {
		t.Fatalf("active tasks after resume/submit race=%d err=%v, want two", active, err)
	}
	if status != string(task.StatusQueued) {
		t.Fatalf("resumed Task status=%q, want queued", status)
	}
}
