package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	auditdomain "morde/server/internal/domain/audit"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresTaskSubmissionAppendAndReplay(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)

	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "create-task", "first work")
	if submission.Task.ConversationID != "" || submission.Conversation != nil {
		t.Fatal("test requires a task with no conversation so the store creates one atomically")
	}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit task without conversation: %v", err)
	}
	if created.Task.ConversationID == "" || created.Conversation.ID != created.Task.ConversationID || created.Conversation.HostID != hostValue.ID ||
		created.Message.TaskID != created.Task.ID || created.Message.ConversationID != created.Conversation.ID {
		t.Fatalf("created associations = %#v", created)
	}

	replayed, err := store.SubmitTask(ctx, submission)
	if err != nil || !replayed.Replayed || replayed.Task.ID != created.Task.ID || replayed.Message.ID != created.Message.ID || replayed.Conversation.ID != created.Conversation.ID {
		t.Fatalf("idempotent replay = %#v, err=%v", replayed, err)
	}

	different := submission
	different.Idempotency.RequestHash = hashBytes("different request")
	if _, err := store.SubmitTask(ctx, different); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("different request under same key error = %v", err)
	}

	appendRequest := app.TaskSubmission{
		Task:        task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, HostID: hostValue.ID, ModelID: "model-test"},
		Message:     conversation.Message{ID: mustID(t, domain.NewMessageID), ConversationID: created.Conversation.ID, OwnerID: owner.ID, Role: conversation.RoleUser, Content: "continue"},
		Idempotency: testIdempotency(owner.ID, "append-message", "append-task-message"),
	}
	appended, err := store.AppendMessage(ctx, appendRequest)
	if err != nil || appended.Task.ID != created.Task.ID || appended.Message.TaskID != created.Task.ID || appended.Task.Status != task.StatusQueued {
		t.Fatalf("append to active task = %#v, err=%v", appended, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, created.Task.ID, 1, 10)
	if err != nil || len(events) != 1 || events[0].Sequence != 2 || events[0].Type != "task.message" {
		t.Fatalf("append event = %#v, err=%v", events, err)
	}
	eventPage, err := store.GetTaskEventPage(ctx, owner.ID, created.Task.ID, 1, 10)
	if err != nil || eventPage.HostID != hostValue.ID || eventPage.Status != task.StatusQueued || eventPage.CurrentSequence != 2 ||
		len(eventPage.Items) != 1 || eventPage.Items[0].Sequence != 2 {
		t.Fatalf("event page = %#v, err=%v", eventPage, err)
	}

	mismatch := appendRequest
	mismatch.Task.ModelID = "different-model"
	mismatch.Message.ID = mustID(t, domain.NewMessageID)
	mismatch.Message.Content = "must not be persisted"
	mismatch.Idempotency = testIdempotency(owner.ID, "append-message", "append-mismatch")
	if _, err := store.AppendMessage(ctx, mismatch); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("mismatched model error = %v", err)
	}

	otherHost := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)
	mismatchHost := appendRequest
	mismatchHost.Task.HostID = otherHost.ID
	mismatchHost.Message.ID = mustID(t, domain.NewMessageID)
	mismatchHost.Message.Content = "must not cross hosts"
	mismatchHost.Idempotency = testIdempotency(owner.ID, "append-message", "append-host-mismatch")
	if _, err := store.AppendMessage(ctx, mismatchHost); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("mismatched Host error = %v", err)
	}
	messages, err := store.ListMessages(ctx, owner.ID, created.Conversation.ID, app.PageRequest{Limit: 20})
	if err != nil || len(messages.Items) != 2 {
		t.Fatalf("messages after rejected append = %d, err=%v", len(messages.Items), err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='pausing' WHERE id=$1`, created.Task.ID); err != nil {
		t.Fatal(err)
	}
	pausingMessage := appendRequest
	pausingMessage.Message.ID = mustID(t, domain.NewMessageID)
	pausingMessage.Idempotency = testIdempotency(owner.ID, "append-message", "append-during-pause")
	if _, err := store.AppendMessage(ctx, pausingMessage); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("append during pause error = %v", err)
	}
	messages, err = store.ListMessages(ctx, owner.ID, created.Conversation.ID, app.PageRequest{Limit: 20})
	if err != nil || len(messages.Items) != 2 {
		t.Fatalf("messages after rejected append during pause = %d, err=%v", len(messages.Items), err)
	}

	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='completed', finished_at=clock_timestamp() WHERE id=$1`, created.Task.ID); err != nil {
		t.Fatal(err)
	}
	newTask := newTestTaskSubmission(t, owner.ID, hostValue.ID, "append-after-complete", "new task after completion")
	newTask.Task.ConversationID = created.Conversation.ID
	newTask.Message.ConversationID = created.Conversation.ID
	newTask.Conversation = nil
	newTask.Idempotency.RequestHash = hashBytes("completed conversation new request")
	newTask.Idempotency.Key = "append-after-complete"
	newTask.Idempotency.Operation = "append-message"
	result, err := store.AppendMessage(ctx, newTask)
	if err != nil || result.Task.ID != newTask.Task.ID || result.Task.ID == created.Task.ID {
		t.Fatalf("append after completion should create a new task: %#v, err=%v", result, err)
	}
}

func TestPostgresTaskPersistsFileDeliveryIntent(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	for _, test := range []struct {
		key       string
		objective string
		want      task.FileDeliveryIntent
	}{
		{key: "persist-intent-none", objective: "Explain what caused the error.", want: task.FileDeliveryNone},
		{key: "persist-intent-file", objective: "Generate and save a PDF report.", want: task.FileDeliveryRequired},
		{key: "persist-intent-unclear", objective: "Write a report about the project.", want: task.FileDeliveryUnclear},
	} {
		submission := newTestTaskSubmission(t, owner.ID, "", test.key, test.objective)
		created, err := store.SubmitTask(ctx, submission)
		if err != nil {
			t.Fatalf("submit %s task: %v", test.want, err)
		}
		loaded, err := store.GetTask(ctx, created.Task.ID, owner.ID)
		if err != nil {
			t.Fatalf("load %s task: %v", test.want, err)
		}
		if created.Task.FileDeliveryIntent != test.want || loaded.Task.FileDeliveryIntent != test.want {
			t.Fatalf("file delivery intent create/read = %q/%q, want %q", created.Task.FileDeliveryIntent, loaded.Task.FileDeliveryIntent, test.want)
		}
	}
}

func TestPostgresTaskAppendOnlyRaisesFileDeliveryIntent(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	submission := newTestTaskSubmission(t, owner.ID, "", "append-intent-start", "Explain what caused the error.")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}

	appendMessage := func(key, content string, intent task.FileDeliveryIntent) app.TaskSubmissionResult {
		t.Helper()
		result, appendErr := store.AppendMessage(ctx, app.TaskSubmission{
			Task: task.Task{
				OwnerID: owner.ID, ConversationID: created.Conversation.ID, ModelID: created.Task.ModelID,
				FileDeliveryIntent: intent,
			},
			Message: conversation.Message{
				ID: mustID(t, domain.NewMessageID), ConversationID: created.Conversation.ID,
				OwnerID: owner.ID, Role: conversation.RoleUser, Content: content,
			},
			Idempotency: testIdempotency(owner.ID, "append-intent", key),
		})
		if appendErr != nil {
			t.Fatalf("append %q: %v", key, appendErr)
		}
		return result
	}

	required := appendMessage("append-intent-required", "Generate and save a PDF report.", task.FileDeliveryRequired)
	if required.Task.FileDeliveryIntent != task.FileDeliveryRequired {
		t.Fatalf("appended required intent = %q", required.Task.FileDeliveryIntent)
	}
	stillRequired := appendMessage("append-intent-neutral", "Continue the explanation.", task.FileDeliveryNone)
	if stillRequired.Task.FileDeliveryIntent != task.FileDeliveryRequired {
		t.Fatalf("later neutral message downgraded intent to %q", stillRequired.Task.FileDeliveryIntent)
	}
}

func TestPostgresTaskSubmissionFencesWorkspaceRevisionAndHostBinding(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "task-input")
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	revision := workspaceValue.Revision
	newWorkspaceTask := func(key string, inputRevision int64) app.TaskSubmission {
		submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, key, "work from pinned files")
		submission.Task.WorkspaceID = workspaceValue.ID
		submission.Task.InputRevision = &inputRevision
		submission.Task.ContextDigest = hashBytes("workspace input manifest")
		return submission
	}
	if _, err := store.SubmitTask(ctx, newWorkspaceTask("task-before-sync", revision)); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("task using a preparing Workspace binding error = %v", err)
	}

	binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := store.SubmitTask(ctx, newWorkspaceTask("task-current-input", revision))
	if err != nil || accepted.Task.WorkspaceID != workspaceValue.ID || accepted.Task.InputRevision == nil || *accepted.Task.InputRevision != revision {
		t.Fatalf("task with current active Workspace binding = %#v, err=%v", accepted, err)
	}
	appendValue := app.TaskSubmission{
		Task: task.Task{OwnerID: owner.ID, ConversationID: accepted.Conversation.ID, HostID: hostValue.ID, ModelID: accepted.Task.ModelID},
		Message: conversation.Message{
			ID: mustID(t, domain.NewMessageID), ConversationID: accepted.Conversation.ID,
			OwnerID: owner.ID, Role: conversation.RoleUser, Content: "continue this task",
		},
		Idempotency: testIdempotency(owner.ID, "conversation.append_message", "task-workspace-context-append"),
	}
	appended, err := store.AppendMessage(ctx, appendValue)
	if err != nil || appended.Task.WorkspaceID != workspaceValue.ID || appended.Task.InputRevision == nil || *appended.Task.InputRevision != revision {
		t.Fatalf("append did not preserve active task Workspace input: %#v, err=%v", appended, err)
	}
	mismatchedAppend := appendValue
	mismatchedAppend.Task.WorkspaceID = workspaceValue.ID
	wrongRevision := revision + 1
	mismatchedAppend.Task.InputRevision = &wrongRevision
	mismatchedAppend.Task.ContextDigest = hashBytes("workspace input manifest")
	mismatchedAppend.Message.ID = mustID(t, domain.NewMessageID)
	mismatchedAppend.Message.Content = "must not change the pinned input"
	mismatchedAppend.Idempotency = testIdempotency(owner.ID, "conversation.append_message", "task-workspace-context-mismatch")
	if _, err := store.AppendMessage(ctx, mismatchedAppend); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("append with a different Workspace snapshot error = %v", err)
	}
	if _, err := store.SubmitTask(ctx, newWorkspaceTask("task-stale-input", revision+1)); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("task with uncommitted Workspace revision error = %v", err)
	}
	if _, err := store.SetWorkspaceBindingSyncStatus(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingSyncConflict, []string{"input.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitTask(ctx, newWorkspaceTask("task-conflicted-binding", revision)); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("task with conflicted Host replica error = %v", err)
	}
}

func TestPostgresWorkspaceSyncCommitSerializesWithTaskSubmission(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)

	prepare := func(t *testing.T, name, key string) (app.TaskSubmission, workspace.Workspace, workspace.SyncSession) {
		t.Helper()
		workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, name)
		hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
		binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
			OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
			State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
			SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
		})
		if err != nil {
			t.Fatalf("create Workspace binding: %v", err)
		}
		if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
			workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0); err != nil {
			t.Fatalf("activate Workspace binding: %v", err)
		}
		revision := workspaceValue.Revision
		submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, key, "work from the pinned Workspace")
		submission.Task.WorkspaceID = workspaceValue.ID
		submission.Task.InputRevision = &revision
		submission.Task.ContextDigest = hashBytes("workspace input manifest")

		file := syncTestFile(name+".txt", []byte("committed after the task snapshot"))
		session := newTestSyncSession(t, workspaceValue, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
		if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks([]workspace.File{file}),
			syncTestIdempotency(owner.ID, "workspace.sync.create", "create:"+key, session.ID)); err != nil {
			t.Fatalf("create Workspace sync session: %v", err)
		}
		return submission, workspaceValue, session
	}

	t.Run("sync commit wins and stale task is rejected", func(t *testing.T) {
		submission, workspaceValue, session := prepare(t, "sync-first", "task-sync-first")
		committed, _, err := commitWorkspaceSyncSession(ctx, store, workspaceValue.ID, session.ID,
			syncTestIdempotency(owner.ID, "workspace.sync.commit", "commit:task-sync-first", session.ID))
		if err != nil || committed.State != workspace.SyncCompleted || committed.TargetRevision != 1 {
			t.Fatalf("commit Workspace sync first = %#v, err=%v", committed, err)
		}
		if _, err := store.SubmitTask(ctx, submission); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
			t.Fatalf("submit Task with stale input after sync commit error = %v, want workspace conflict", err)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE workspace_id=$1`, workspaceValue.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("stale Task rows after rejected submission = %d, err=%v", count, err)
		}
	})

	t.Run("task submission wins then sync commit advances revision", func(t *testing.T) {
		submission, workspaceValue, session := prepare(t, "task-first", "task-task-first")
		const advisoryLockKey int64 = 824726194
		if _, err := pool.Exec(ctx, `CREATE FUNCTION wait_for_task_submission_test_release() RETURNS trigger AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(824726194);
  RETURN NEW;
END;
$$ LANGUAGE plpgsql`); err != nil {
			t.Fatalf("create Task insert test barrier: %v", err)
		}
		if _, err := pool.Exec(ctx, `CREATE TRIGGER wait_for_task_submission_test_release
BEFORE INSERT ON tasks FOR EACH ROW EXECUTE FUNCTION wait_for_task_submission_test_release()`); err != nil {
			t.Fatalf("install Task insert test barrier: %v", err)
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
			t.Fatalf("hold Task insert advisory lock: %v", err)
		}

		type submissionResult struct {
			value app.TaskSubmissionResult
			err   error
		}
		type commitResult struct {
			value workspace.SyncSession
			err   error
		}
		submitted := make(chan submissionResult, 1)
		committed := make(chan commitResult, 1)
		go func() {
			value, err := store.SubmitTask(ctx, submission)
			submitted <- submissionResult{value: value, err: err}
		}()

		waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
		defer waitCancel()
		// The INSERT trigger runs only after SubmitTask holds the Workspace row lock.
		waitForPostgresLockWait(t, waitCtx, pool, "%INSERT INTO tasks%", 1)
		go func() {
			value, _, err := commitWorkspaceSyncSession(ctx, store, workspaceValue.ID, session.ID,
				syncTestIdempotency(owner.ID, "workspace.sync.commit", "commit:task-task-first", session.ID))
			committed <- commitResult{value: value, err: err}
		}()
		waitForPostgresLockWait(t, waitCtx, pool, "%FROM workspaces%FOR UPDATE%", 1)
		var unlocked bool
		if err := advisoryConn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, advisoryLockKey).Scan(&unlocked); err != nil || !unlocked {
			t.Fatalf("release Task insert advisory lock: unlocked=%t err=%v", unlocked, err)
		}

		var accepted app.TaskSubmissionResult
		select {
		case result := <-submitted:
			if result.err != nil {
				t.Fatalf("Task submission queued before sync commit: %v", result.err)
			}
			accepted = result.value
		case <-waitCtx.Done():
			t.Fatalf("Task submission did not finish before sync commit: %v", waitCtx.Err())
		}
		if accepted.Task.WorkspaceID != workspaceValue.ID || accepted.Task.InputRevision == nil || *accepted.Task.InputRevision != workspaceValue.Revision {
			t.Fatalf("Task input after first lock grant = %#v", accepted.Task)
		}
		select {
		case result := <-committed:
			if result.err != nil || result.value.State != workspace.SyncCompleted || result.value.TargetRevision != workspaceValue.Revision+1 {
				t.Fatalf("Workspace sync commit after Task submission = %#v, err=%v", result.value, result.err)
			}
		case <-waitCtx.Done():
			t.Fatalf("Workspace sync commit did not finish after Task submission: %v", waitCtx.Err())
		}
		stored, err := store.GetTask(ctx, accepted.Task.ID, owner.ID)
		if err != nil || stored.Task.InputRevision == nil || *stored.Task.InputRevision != workspaceValue.Revision {
			t.Fatalf("stored Task input after later Workspace commit = %#v, err=%v", stored.Task, err)
		}
		currentWorkspace, err := store.GetWorkspace(ctx, workspaceValue.ID)
		if err != nil || currentWorkspace.Revision != workspaceValue.Revision+1 {
			t.Fatalf("Workspace after serialized commit = %#v, err=%v", currentWorkspace, err)
		}
	})
}

func TestPostgresWorkspaceSyncCommitSerializesWithTaskOperationAdmission(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "operation-admission")
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id='ri_workspace_admission' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	revision := workspaceValue.Revision
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "workspace-operation-admission", "read the pinned Workspace")
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &revision
	submission.Task.ContextDigest = hashBytes("workspace operation admission manifest")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Workspace task: %v", err)
	}
	if created.Task.WorkspaceCheckpointRevision == nil || *created.Task.WorkspaceCheckpointRevision != revision ||
		created.Task.InputRevision == nil || *created.Task.InputRevision != revision {
		t.Fatalf("submitted Task checkpoint = %#v, input = %#v; want both %d", created.Task.WorkspaceCheckpointRevision, created.Task.InputRevision, revision)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "workspace-admission-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Workspace task: %#v, found=%t, err=%v", claimed, found, err)
	}
	unscopedBrowserOperation, replayed, err := store.CreateOperation(ctx, task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: claimed.ID, Key: "browser.click:missing-workspace-fence",
		RequestHash: hashBytes("browser click without workspace fence"), HostID: hostValue.ID,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_workspace_admission",
		ControlVersion: hostValue.ControlVersion, LeaseVersion: claimed.Lease.Version,
		RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationPrepared,
	}, claimed.Lease.Owner)
	if err != nil || replayed || unscopedBrowserOperation.Status != task.OperationPrepared {
		t.Fatalf("prepare unscoped browser operation: %#v replay=%t err=%v", unscopedBrowserOperation, replayed, err)
	}
	file := syncTestFile("external-update.txt", []byte("external Workspace update"))
	writerSubmission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "workspace-file-writer", "write a file")
	writerSubmission.Task.WorkspaceID = workspaceValue.ID
	writerSubmission.Task.InputRevision = &revision
	writerSubmission.Task.ContextDigest = hashBytes("writer Workspace manifest")
	writerCreated, err := store.SubmitTask(ctx, writerSubmission)
	if err != nil {
		t.Fatalf("submit Workspace writer task: %v", err)
	}
	writer, found, err := store.ClaimNextTask(ctx, "workspace-writer-worker", time.Minute)
	if err != nil || !found || writer.ID != writerCreated.Task.ID {
		t.Fatalf("claim Workspace writer task: %#v, found=%t, err=%v", writer, found, err)
	}
	operation, replayed, err := store.CreateOperation(ctx, task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: claimed.ID, Key: "file.read:input",
		RequestHash: hashBytes("read pinned input"), HostID: hostValue.ID, WorkspaceID: workspaceValue.ID,
		BindingVersion: binding.BindingVersion, InputRevision: &revision,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_workspace_admission",
		ControlVersion: hostValue.ControlVersion, LeaseVersion: claimed.Lease.Version,
		RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationPrepared,
	}, claimed.Lease.Owner)
	if err != nil || replayed || operation.Status != task.OperationPrepared || operation.InputRevision == nil || *operation.InputRevision != revision ||
		operation.WorkspaceCheckpointRevision == nil || *operation.WorkspaceCheckpointRevision != revision {
		t.Fatalf("create revision-bound task operation: %#v, replayed=%t, err=%v", operation, replayed, err)
	}
	loadedOperation, err := store.GetOperation(ctx, owner.ID, claimed.ID, operation.Key)
	if err != nil || loadedOperation.InputRevision == nil || *loadedOperation.InputRevision != revision ||
		loadedOperation.WorkspaceCheckpointRevision == nil || *loadedOperation.WorkspaceCheckpointRevision != revision {
		t.Fatalf("load revision-bound task operation: %#v, err=%v", loadedOperation, err)
	}
	writeKey := "file-write-output"
	writerOperation, replayed, err := store.CreateOperation(ctx, task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: writer.ID, Key: writeKey,
		RequestHash: hashBytes("write pinned output"), HostID: hostValue.ID, WorkspaceID: workspaceValue.ID,
		BindingVersion: binding.BindingVersion, InputRevision: &revision,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_workspace_admission",
		ControlVersion: hostValue.ControlVersion, LeaseVersion: writer.Lease.Version,
		RuntimeLeaseVersion: writer.Lease.Version, Status: task.OperationPrepared,
	}, writer.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("create writer operation: %#v replay=%t err=%v", writerOperation, replayed, err)
	}

	session := newTestSyncSession(t, workspaceValue, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks([]workspace.File{file}),
		syncTestIdempotency(owner.ID, "workspace.sync.create", "admission-session", session.ID)); err != nil {
		t.Fatalf("create external sync session: %v", err)
	}
	commit := func() (workspace.SyncSession, error) {
		value, _, commitErr := commitWorkspaceSyncSession(ctx, store, workspaceValue.ID, session.ID,
			syncTestIdempotency(owner.ID, "workspace.sync.commit", "admission-commit", session.ID))
		return value, commitErr
	}
	if _, err := commit(); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("Workspace commit during admitted task operation error = %v, want workspace busy", err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: claimed.ID, OperationKey: operation.Key, LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, Status: task.OperationFailed, Result: `{"error":"test_settled"}`,
	}); err != nil {
		t.Fatalf("settle admitted operation: %v", err)
	}
	writeOutput, err := json.Marshal(map[string]any{"path": file.Path, "bytes_written": file.SizeBytes, "sha256": file.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: writer.ID, OperationKey: writeKey, LeaseOwner: writer.Lease.Owner,
		LeaseVersion: writer.Lease.Version, Status: task.OperationDispatched, ExternalID: "file-write-dispatch",
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_workspace_admission",
		ControlVersion: hostValue.ControlVersion, RuntimeLeaseVersion: writer.Lease.Version,
	}); err != nil {
		t.Fatalf("dispatch writer operation: %v", err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: writer.ID, OperationKey: writeKey, LeaseOwner: writer.Lease.Owner,
		LeaseVersion: writer.Lease.Version, Status: task.OperationSucceeded, Result: string(writeOutput),
	}); err != nil {
		t.Fatalf("settle writer operation: %v", err)
	}
	committed, err := commit()
	if err != nil || committed.State != workspace.SyncCompleted || committed.TargetRevision != revision+1 {
		t.Fatalf("Workspace commit after operation settlement = %#v, err=%v", committed, err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: claimed.ID, OperationKey: unscopedBrowserOperation.Key, LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, Status: task.OperationDispatched, ExternalID: "browser-click",
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_workspace_admission",
		ControlVersion: hostValue.ControlVersion, RuntimeLeaseVersion: claimed.Lease.Version,
	}); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("dispatch of an unscoped operation after Workspace changed = %v, want workspace conflict", err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: claimed.ID, OperationKey: unscopedBrowserOperation.Key, LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, Status: task.OperationFailed, Result: `{"error":"workspace_fence_required"}`,
	}); err != nil {
		t.Fatalf("settle blocked unscoped operation: %v", err)
	}
	staleOperation := task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: claimed.ID, Key: "file.read:stale-input",
		RequestHash: hashBytes("read stale input"), HostID: hostValue.ID, WorkspaceID: workspaceValue.ID,
		BindingVersion: binding.BindingVersion, InputRevision: &revision,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_workspace_admission",
		ControlVersion: hostValue.ControlVersion, LeaseVersion: claimed.Lease.Version,
		RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationPrepared,
	}
	if _, _, err := store.CreateOperation(ctx, staleOperation, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("operation admitted after input revision changed error = %v, want workspace conflict", err)
	}
	completionStepID := mustID(t, domain.NewStepID)
	completionStep := task.Step{ID: completionStepID, TaskID: claimed.ID, Sequence: 1, Kind: "result", Status: task.StepSucceeded,
		Title: "Complete task", Summary: "The task finished."}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusCompleted, CurrentStep: completionStep.ID, Result: "done", Steps: []task.Step{completionStep},
		Event: task.Event{TaskID: claimed.ID, Type: "task.result", Payload: []byte(`{"result":"done"}`)},
	}); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("completion against stale Workspace revision error = %v, want workspace conflict", err)
	}
	if err := store.ReconcileStaleWorkspaceTasks(ctx); err != nil {
		t.Fatalf("reconcile stale Workspace tasks: %v", err)
	}
	readerAfter, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || readerAfter.Task.Status != task.StatusPausing || readerAfter.Task.WaitingReason != "workspace_input_changed" {
		t.Fatalf("stale Workspace reader task = %#v, err=%v", readerAfter.Task, err)
	}
	writerAfter, err := store.GetTask(ctx, writer.ID, owner.ID)
	if err != nil || writerAfter.Task.Status != task.StatusPausing || writerAfter.Task.WaitingReason != "workspace_input_changed" {
		t.Fatalf("matching file content without a publish receipt was treated as a task-owned write: %#v, err=%v", writerAfter.Task, err)
	}
	pausedTasks := make(map[string]bool, 2)
	for {
		stopClaim, found, err := store.ClaimNextTaskStop(ctx, "workspace-revision-stop", time.Minute)
		if err != nil {
			t.Fatalf("claim stale Workspace task stop: %v", err)
		}
		if !found {
			break
		}
		if _, err := store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE id=$1`, stopClaim.Request.ID); err != nil {
			t.Fatal(err)
		}
		paused, err := store.CompleteTaskStop(ctx, stopClaim.Task.ID, stopClaim.Request.ID, "workspace-revision-stop", stopClaim.Request.LeaseVersion)
		if err != nil || paused.Status != task.StatusPaused || paused.WaitingReason != "workspace_input_changed" {
			t.Fatalf("finalize stale Workspace task stop: %#v, err=%v", paused, err)
		}
		pausedTasks[paused.ID] = true
	}
	if !pausedTasks[claimed.ID] || !pausedTasks[writer.ID] {
		t.Fatalf("stale Workspace task stops completed for %v; want both reader %s and writer %s", pausedTasks, claimed.ID, writer.ID)
	}
	resumeID := mustID(t, domain.NewTaskActionID)
	resumeRecord := taskActionIdempotency(owner.ID, task.ActionResume, "stale-workspace-resume", claimed.ID, task.StatusPaused, resumeID)
	if _, err := store.RequestTaskAction(ctx, resumeRecord, claimed.ID, task.ActionResume, task.StatusPaused, resumeID); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("resume against a stale Workspace input error = %v, want workspace conflict", err)
	}
	notifications, err := store.ListUnprocessedNotificationEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	var staleNotice bool
	for _, event := range notifications {
		var payload struct {
			Reason string `json:"reason"`
		}
		if event.TaskID == claimed.ID && event.Type == "task.waiting_user" && json.Unmarshal(event.Payload, &payload) == nil &&
			payload.Reason == "workspace_input_changed" {
			staleNotice = true
		}
	}
	if !staleNotice {
		t.Fatalf("stale Workspace task notification event missing: %#v", notifications)
	}
	notificationService, err := app.NewNotificationService(store)
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := notificationService.ProcessPending(ctx, 10); err != nil || processed == 0 {
		t.Fatalf("process stale Workspace task notification: processed=%d err=%v", processed, err)
	}
	page, err := store.ListNotifications(ctx, owner.ID, false, app.PageRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var attentionNotice bool
	for _, notification := range page.Items {
		if notification.TaskID == claimed.ID && string(notification.Kind) == "attention_required" && !notification.Actionable {
			attentionNotice = true
		}
	}
	if !attentionNotice {
		t.Fatalf("stale Workspace task attention notification missing: %#v", page.Items)
	}
}

func TestPostgresConversationChangesTargetAndRejectsOperationAfterHostUnready(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20000, 40000)
	firstHost := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	secondHost := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	first, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, firstHost.ID, "switch-first", "first"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='completed', finished_at=clock_timestamp() WHERE id=$1`, first.Task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET state='stopped' WHERE id=$1`, secondHost.ID); err != nil {
		t.Fatal(err)
	}
	secondInput := newTestTaskSubmission(t, owner.ID, secondHost.ID, "switch-second", "second")
	secondInput.Task.ConversationID = first.Conversation.ID
	secondInput.Task.ModelID = "another-model"
	secondInput.Message.ConversationID = first.Conversation.ID
	secondInput.Idempotency.Operation = "append-message"
	second, err := store.AppendMessage(ctx, secondInput)
	if err != nil || second.Task.ID == first.Task.ID || second.Task.HostID != secondHost.ID || second.Task.ModelID != "another-model" {
		t.Fatalf("new turn with changed target: result=%+v err=%v", second, err)
	}
	old, err := store.GetTask(ctx, first.Task.ID, owner.ID)
	if err != nil || old.Task.HostID != firstHost.ID || old.Task.ModelID != "model-test" {
		t.Fatalf("old task target changed: task=%+v err=%v", old.Task, err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "target-switch-worker", time.Minute)
	if err != nil || !found || claimed.ID != second.Task.ID || claimed.HostID != secondHost.ID {
		t.Fatalf("claim with stopped Host: task=%+v found=%t err=%v", claimed, found, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET state='failed', runtime_instance_id='runtime-not-ready' WHERE id=$1`, secondHost.ID); err != nil {
		t.Fatal(err)
	}
	unready := task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: claimed.ID, Key: "host-not-ready",
		RequestHash: hashBytes("host-not-ready"), HostID: secondHost.ID, RuntimeGeneration: secondHost.RuntimeGeneration,
		RuntimeInstanceID: "runtime-not-ready", ControlVersion: secondHost.ControlVersion,
		LeaseVersion: claimed.Lease.Version, RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationPrepared}
	if _, _, err := store.CreateOperation(ctx, unready, "target-switch-worker"); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("unready Host accepted an operation: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='completed', lease_owner='', lease_expires_at=NULL,
finished_at=clock_timestamp() WHERE id=$1`, second.Task.ID); err != nil {
		t.Fatal(err)
	}
	thirdInput := newTestTaskSubmission(t, owner.ID, "", "switch-third", "model only")
	thirdInput.Task.ConversationID = first.Conversation.ID
	thirdInput.Message.ConversationID = first.Conversation.ID
	third, err := store.SubmitTask(ctx, thirdInput)
	if err != nil || third.Task.HostID != "" {
		t.Fatalf("submit model-only task: result=%+v err=%v", third, err)
	}
	claimed, found, err = store.ClaimNextTask(ctx, "model-only-worker", time.Minute)
	if err != nil || !found || claimed.ID != third.Task.ID || claimed.HostID != "" {
		t.Fatalf("claim model-only task: task=%+v found=%t err=%v", claimed, found, err)
	}
	operation := task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: claimed.ID, Key: "no-target",
		RequestHash: hashBytes("no-target"), HostID: firstHost.ID, RuntimeInstanceID: "runtime-test",
		LeaseVersion: claimed.Lease.Version, RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationPrepared}
	if _, _, err := store.CreateOperation(ctx, operation, "model-only-worker"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("model-only task obtained a Host operation: %v", err)
	}
}

func TestPostgresNewTaskHasKnownZeroModelUsage(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 2, 10000, 20000)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "initial-usage", "model-only"))
	if err != nil || created.Task.Usage.ModelTokens == nil || *created.Task.Usage.ModelTokens != 0 {
		t.Fatalf("new task model usage = %+v, err=%v", created.Task.Usage, err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "usage-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim task: found=%t err=%v", found, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET usage_model_tokens=18 WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	staleUsage := int64(0)
	progress := app.TaskProgress{TaskID: claimed.ID, LeaseOwner: "usage-worker",
		LeaseVersion: claimed.Lease.Version, Status: task.StatusRunning, Usage: task.Usage{ModelTokens: &staleUsage},
		Event: task.Event{Type: "task.step", Payload: []byte(`{}`)}}
	if _, err := store.CommitTaskProgress(ctx, progress); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || loaded.Task.Usage.ModelTokens == nil || *loaded.Task.Usage.ModelTokens != 18 {
		t.Fatalf("stale progress overwrote settled model usage: usage=%+v err=%v", loaded.Task.Usage, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET usage_model_tokens=NULL WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitTaskProgress(ctx, progress); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || loaded.Task.Usage.ModelTokens != nil {
		t.Fatalf("unknown model usage was not retained: usage=%+v err=%v", loaded.Task.Usage, err)
	}
}

func TestPostgresClientTaskAuthorizationSnapshotSurvivesReloadAndRevocation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	installationID := createTestClientInstallation(t, store.pool, owner.ID, nil)
	clientToken := clientTestToken(t, owner.ID, installationID, identity.KindClient, nil)
	if _, err := store.CreateToken(ctx, clientToken); err != nil {
		t.Fatalf("create client token: %v", err)
	}

	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "client-task-snapshot", "run from a client")
	submission.Task.Authorization = task.Authorization{
		Kind: string(identity.KindClient), TokenID: clientToken.ID, ClientInstallationID: installationID,
	}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit client task: %v", err)
	}
	if created.Task.Authorization.ClientInstallationID != installationID {
		t.Fatalf("submitted task installation = %q, want %q", created.Task.Authorization.ClientInstallationID, installationID)
	}

	loaded, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || loaded.Task.Authorization.ClientInstallationID != installationID {
		t.Fatalf("reloaded task authorization = %#v, err=%v", loaded.Task.Authorization, err)
	}

	appendRequest := app.TaskSubmission{
		Task: task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, HostID: hostValue.ID, ModelID: submission.Task.ModelID},
		Message: conversation.Message{
			ID: mustID(t, domain.NewMessageID), ConversationID: created.Conversation.ID, OwnerID: owner.ID,
			Role: conversation.RoleUser, Content: "continue from the same client",
		},
		Idempotency: testIdempotency(owner.ID, "append-message", "client-task-append"),
	}
	appended, err := store.AppendMessage(ctx, appendRequest)
	if err != nil || appended.Task.ID != created.Task.ID || appended.Task.Authorization.ClientInstallationID != installationID {
		t.Fatalf("append preserved client authorization = %#v, err=%v", appended.Task.Authorization, err)
	}

	claimed, found, err := store.ClaimNextTask(ctx, "client-task-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID || claimed.Authorization.ClientInstallationID != installationID {
		t.Fatalf("claimed client task = %#v, found=%t, err=%v", claimed, found, err)
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 2, LoginWindow: time.Minute,
	}, credentialDefaultHostNoop{})
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.AuthorizeTaskAction(ctx, owner.ID, identity.Kind(claimed.Authorization.Kind),
		claimed.Authorization.TokenID, hostValue.ID, nil, identity.ScopeTaskSubmit); err != nil {
		t.Fatalf("authorize task before installation revocation: %v", err)
	}
	if err := store.RevokeClientInstallation(ctx, installationID, owner.ID, "127.0.0.1", auditdomain.ActorSession); err != nil {
		t.Fatalf("revoke installation: %v", err)
	}
	if err := credentials.AuthorizeTaskAction(ctx, owner.ID, identity.Kind(claimed.Authorization.Kind),
		claimed.Authorization.TokenID, hostValue.ID, nil, identity.ScopeTaskSubmit); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("authorize task after installation revocation = %v, want unauthenticated", err)
	}
}

func TestPostgresTaskWebSessionRetainsOwnerInstallation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 2, 10000, 20000)
	other := createTestUser(t, store, 2, 10000, 20000)
	installationID := createTestClientInstallation(t, store.pool, owner.ID, nil)
	otherInstallationID := createTestClientInstallation(t, store.pool, other.ID, nil)
	input := newTestTaskSubmission(t, owner.ID, "", "session-installation", "model-only task")
	input.Task.Authorization.ClientInstallationID = installationID
	created, err := store.SubmitTask(ctx, input)
	if err != nil || created.Task.Authorization.ClientInstallationID != installationID {
		t.Fatalf("session task lost installation: task=%+v err=%v", created.Task, err)
	}
	loaded, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || loaded.Task.Authorization.ClientInstallationID != installationID {
		t.Fatalf("reloaded task lost installation: task=%+v err=%v", loaded.Task, err)
	}
	otherInput := newTestTaskSubmission(t, owner.ID, "", "cross-account-installation", "must fail")
	otherInput.Task.Authorization.ClientInstallationID = otherInstallationID
	if _, err := store.SubmitTask(ctx, otherInput); err == nil {
		t.Fatal("task accepted an installation belonging to another account")
	}
}

func TestPostgresTaskApplicationTargetRequiresCurrentLaunchAndSurvivesClaim(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	if err := store.SetRuntimeInstanceID(ctx, hostValue.ID, hostValue.RuntimeGeneration, hostValue.ControlVersion, "", "runtime-task-app"); err != nil {
		t.Fatal(err)
	}
	hostValue.RuntimeInstanceID = "runtime-task-app"
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "app-target", "find a message")
	submission.Task.ApplicationID = app.WechatApplicationID
	created, err := store.SubmitTask(ctx, submission)
	if err != nil || created.Task.ApplicationID != app.WechatApplicationID {
		t.Fatalf("submit application task = %#v, err=%v", created, err)
	}
	loaded, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || loaded.Task.ApplicationID != app.WechatApplicationID {
		t.Fatalf("load application task = %#v, err=%v", loaded.Task, err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "application-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID || claimed.ApplicationID != app.WechatApplicationID {
		t.Fatalf("claim application task = %#v, found=%t, err=%v", claimed, found, err)
	}
	operation := task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: claimed.ID, Key: "app-action",
		RequestHash: hashBytes("app-action"), HostID: hostValue.ID, RuntimeGeneration: hostValue.RuntimeGeneration,
		RuntimeInstanceID: hostValue.RuntimeInstanceID, ControlVersion: hostValue.ControlVersion,
		LeaseVersion: claimed.Lease.Version, RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationPrepared}
	if _, _, err := store.CreateOperation(ctx, operation, "application-worker"); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("operation without a running managed application error = %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO host_application_launches
		(host_id, owner_id, launch_id, application_id, runtime_generation, runtime_instance_id, control_version, status)
		VALUES ($1,$2,'launch_tasktarget01','wechat',$3,$4,$5,'running')`,
		hostValue.ID, owner.ID, hostValue.RuntimeGeneration, hostValue.RuntimeInstanceID, hostValue.ControlVersion); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateOperation(ctx, operation, "application-worker"); err != nil {
		t.Fatalf("operation with current application = %v", err)
	}
	appendRequest := app.TaskSubmission{
		Task: task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, HostID: hostValue.ID,
			ModelID: submission.Task.ModelID},
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), ConversationID: created.Conversation.ID,
			OwnerID: owner.ID, Role: conversation.RoleUser, Content: "continue"},
		Idempotency: testIdempotency(owner.ID, "append-message", "app-target-mismatch"),
	}
	if _, err := store.AppendMessage(ctx, appendRequest); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("append without active task application target error = %v", err)
	}
	appendRequest.Task.ApplicationID = app.WechatApplicationID
	appendRequest.Idempotency = testIdempotency(owner.ID, "append-message", "app-target-match")
	appended, err := store.AppendMessage(ctx, appendRequest)
	if err != nil || appended.Task.ID != created.Task.ID || appended.Task.ApplicationID != app.WechatApplicationID {
		t.Fatalf("append to application task = %#v, err=%v", appended, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE host_application_launches SET status='stopped' WHERE host_id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	dispatch := app.OperationUpdate{TaskID: claimed.ID, OperationKey: operation.Key, LeaseOwner: "application-worker",
		LeaseVersion: claimed.Lease.Version, RuntimeGeneration: hostValue.RuntimeGeneration,
		RuntimeInstanceID: hostValue.RuntimeInstanceID, ControlVersion: hostValue.ControlVersion,
		RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationDispatched}
	if _, err := store.UpdateOperation(ctx, dispatch); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("dispatch after application stop error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE host_application_launches SET status='running', control_version=control_version+1 WHERE host_id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateOperation(ctx, dispatch); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("dispatch with stale application Host fence error = %v", err)
	}
}

func TestPostgresTaskEventPageDetectsAllEventsExpired(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "create-task", "event retention")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM task_events WHERE task_id=$1`, created.Task.ID); err != nil {
		t.Fatalf("remove events to simulate retention: %v", err)
	}
	page, err := store.GetTaskEventPage(ctx, owner.ID, created.Task.ID, 0, 10)
	if domain.CodeOf(err) != domain.CodeEventCursorExpired {
		t.Fatalf("event page with missing history error = %v, want expired cursor", err)
	}
	if page.HostID != hostValue.ID || page.Status != task.StatusQueued || page.CurrentSequence < 1 {
		t.Fatalf("expired event page lost authorized snapshot cursor: %#v", page)
	}
}

func TestPostgresTaskSnapshotDoesNotMixConcurrentEvents(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 2, 10000, 20000)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "snapshot-event", "observe snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	var first int64
	if err := store.withTaskSnapshot(ctx, func(snapshot pgx.Tx) error {
		if err := snapshot.QueryRow(ctx, `SELECT event_seq FROM tasks WHERE id=$1`, created.Task.ID).Scan(&first); err != nil {
			return err
		}
		if err := store.WithTx(ctx, func(writer pgx.Tx) error {
			var next int64
			if err := writer.QueryRow(ctx, `UPDATE tasks SET event_seq=event_seq+1 WHERE id=$1 RETURNING event_seq`, created.Task.ID).Scan(&next); err != nil {
				return err
			}
			_, err := writer.Exec(ctx, `INSERT INTO task_events (task_id,sequence,event_type,payload) VALUES ($1,$2,'task.status','{}')`, created.Task.ID, next)
			return err
		}); err != nil {
			return err
		}
		var stillVisible int64
		if err := snapshot.QueryRow(ctx, `SELECT event_seq FROM tasks WHERE id=$1`, created.Task.ID).Scan(&stillVisible); err != nil {
			return err
		}
		if stillVisible != first {
			t.Fatalf("one snapshot mixed events: first=%d later=%d", first, stillVisible)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var after int64
	if err := pool.QueryRow(ctx, `SELECT event_seq FROM tasks WHERE id=$1`, created.Task.ID).Scan(&after); err != nil || after != first+1 {
		t.Fatalf("writer event not committed: before=%d after=%d err=%v", first, after, err)
	}
	details, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || details.Task.EventSequence != after {
		t.Fatalf("fresh task snapshot cursor = %d, want %d, err=%v", details.Task.EventSequence, after, err)
	}
}

func TestPostgresAssistantTextDeltasAreOrderedLeasedAndFinalizedAtomically(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "streaming-assistant", "write a summary"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "streaming-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("ClaimNextTask() = %#v, found=%t, err=%v", claimed, found, err)
	}
	messageID := mustID(t, domain.NewMessageID)
	turnID := mustID(t, domain.NewStepID)
	for _, delta := range []string{"Hello ", "world."} {
		if _, err := store.AppendAssistantTextDelta(ctx, app.AssistantTextDelta{
			TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
			MessageID: messageID, TurnID: turnID, Delta: delta,
		}); err != nil {
			t.Fatalf("AppendAssistantTextDelta(%q): %v", delta, err)
		}
	}
	step := task.Step{
		ID: turnID, TaskID: claimed.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded,
		Title: "Ask the model", Summary: "Received a complete response.",
	}
	finalMessage := conversation.Message{
		ID: messageID, ConversationID: created.Conversation.ID, OwnerID: owner.ID,
		Role: conversation.RoleAssistant, Content: "Hello world.", TaskID: claimed.ID,
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusRunning, CurrentStep: turnID, Steps: []task.Step{step},
		Event:            task.Event{TaskID: claimed.ID, Type: "task.step", Payload: []byte(`{"step_id":"` + turnID + `","status":"succeeded"}`)},
		AssistantMessage: &finalMessage,
	}); err != nil {
		t.Fatalf("commit completed provider response: %v", err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, claimed.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 6 {
		t.Fatalf("event history = %#v; want submit, claim, two deltas, step and final message", events)
	}
	for index, event := range events {
		if event.Sequence != int64(index+1) {
			t.Fatalf("event[%d] sequence = %d, want %d", index, event.Sequence, index+1)
		}
	}
	for index, want := range []string{"Hello ", "world."} {
		if events[index+2].Type != "task.message" {
			t.Fatalf("delta event[%d] type = %q", index, events[index+2].Type)
		}
		var payload map[string]any
		if err := json.Unmarshal(events[index+2].Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["message_id"] != messageID || payload["turn_id"] != turnID || payload["delta"] != want || payload["done"] != nil {
			t.Fatalf("delta payload = %#v", payload)
		}
	}
	if events[4].Type != "task.step" || events[5].Type != "task.message" {
		t.Fatalf("terminal event order = %q, %q", events[4].Type, events[5].Type)
	}
	var finalPayload map[string]any
	if err := json.Unmarshal(events[5].Payload, &finalPayload); err != nil {
		t.Fatal(err)
	}
	if finalPayload["message_id"] != messageID || finalPayload["turn_id"] != turnID || finalPayload["content"] != finalMessage.Content || finalPayload["done"] != true {
		t.Fatalf("final assistant event payload = %#v", finalPayload)
	}
	messages, err := store.ListMessages(ctx, owner.ID, created.Conversation.ID, app.PageRequest{Limit: 20})
	var storedAssistant *conversation.Message
	for index := range messages.Items {
		if messages.Items[index].Role == conversation.RoleAssistant {
			storedAssistant = &messages.Items[index]
		}
	}
	if err != nil || len(messages.Items) != 2 || storedAssistant == nil || storedAssistant.ID != messageID || storedAssistant.Content != finalMessage.Content {
		t.Fatalf("stored messages = %#v, err=%v", messages.Items, err)
	}
	if _, err := store.AppendAssistantTextDelta(ctx, app.AssistantTextDelta{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version + 1,
		MessageID: messageID, TurnID: turnID, Delta: "stale",
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale lease delta error = %v, want invalid state", err)
	}
}

func TestPostgresPendingApprovalResponseIsVersionedAndAtomic(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "pending-submit-key", "approve an external action")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "pending-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("ClaimNextTask() = %+v, %t, %v", claimed, found, err)
	}
	stepID := mustID(t, domain.NewStepID)
	pendingID := mustID(t, domain.NewPendingRequestID)
	request := &task.PendingRequest{
		ID: pendingID, TaskID: claimed.ID, Version: 4, Kind: "approval", Title: "Approve export",
		Object: "report.txt", Content: "Export this file?", Impact: "The file will leave this Host.",
		CallID: "call-export", ToolName: "file_export", RequestHash: hashBytes("exact-export-call"),
		RuntimeGeneration: 2, RuntimeInstanceID: "runtime-instance-2", ControlVersion: 3, Status: task.PendingRequestPending,
	}
	usage := task.Usage{ActiveSeconds: 3, ToolCalls: 1}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "approval_required", Usage: usage,
		Steps: []task.Step{{ID: stepID, TaskID: claimed.ID, Sequence: 1, Kind: "tool", Status: task.StepWaitingUser, Title: request.Title, Summary: request.Content}},
		Event: task.Event{TaskID: claimed.ID, Type: "task.waiting_user", Payload: []byte(`{"reason":"approval_required"}`)}, PendingRequest: request,
	}); err != nil {
		t.Fatal(err)
	}
	details, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.PendingRequest == nil || details.PendingRequest.ID != pendingID || details.PendingRequest.Version != 4 || details.PendingRequest.Status != task.PendingRequestPending {
		t.Fatalf("GetTask() pending = %#v, err=%v", details.PendingRequest, err)
	}
	record := app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "task.pending.respond", Key: "pending-response-key",
		RequestHash: hashBytes("approve:" + pendingID + ":4"), ResourceID: claimed.ID, RelatedID: pendingID,
	}
	if err := store.RespondToPendingRequest(ctx, record, claimed.ID, pendingID, 4, task.PendingDecisionApprove); err != nil {
		t.Fatal(err)
	}
	if err := store.RespondToPendingRequest(ctx, record, claimed.ID, pendingID, 4, task.PendingDecisionApprove); err != nil {
		t.Fatalf("idempotent response replay: %v", err)
	}
	details, err = store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusQueued || details.PendingRequest == nil ||
		details.PendingRequest.Status != task.PendingRequestApproved || details.PendingRequest.Decision != task.PendingDecisionApprove {
		t.Fatalf("task after response = status %q pending %#v err=%v", details.Task.Status, details.PendingRequest, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, claimed.ID, 0, 20)
	if err != nil || len(events) != 4 || events[3].Type != "task.response" || events[3].Sequence != 4 {
		t.Fatalf("response event history = %#v err=%v", events, err)
	}
	stale := app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "task.pending.respond", Key: "pending-response-stale",
		RequestHash: hashBytes("stale"), ResourceID: claimed.ID, RelatedID: pendingID,
	}
	if err := store.RespondToPendingRequest(ctx, stale, claimed.ID, pendingID, 3, task.PendingDecisionReject); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale response error = %v, want conflict", err)
	}
}

func TestPostgresConcurrentApprovalByTwoAuthorizedClientsIsAppliedOnce(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "two-client-approval", "approve a shared operation"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "two-client-approval-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("ClaimNextTask() = %#v, found=%t, err=%v", claimed, found, err)
	}
	stepID := mustID(t, domain.NewStepID)
	pendingID := mustID(t, domain.NewPendingRequestID)
	request := &task.PendingRequest{
		ID: pendingID, TaskID: claimed.ID, Version: 1, Kind: "approval", Title: "Approve export",
		Object: "report.txt", Content: "Export this file?", Impact: "The file will leave this Host.",
		CallID: "call-two-client-approval", ToolName: "file_export", RequestHash: hashBytes("two-client-export"),
		RuntimeGeneration: 1, RuntimeInstanceID: "two-client-runtime", ControlVersion: 1, Status: task.PendingRequestPending,
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "approval_required",
		Usage: task.Usage{ActiveSeconds: 1, ToolCalls: 1},
		Steps: []task.Step{{ID: stepID, TaskID: claimed.ID, Sequence: 1, Kind: "tool", Status: task.StepWaitingUser,
			Title: request.Title, Summary: request.Content}},
		Event:          task.Event{TaskID: claimed.ID, Type: "task.waiting_user", Payload: []byte(`{"reason":"approval_required"}`)},
		PendingRequest: request,
	}); err != nil {
		t.Fatal(err)
	}

	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 2, LoginWindow: time.Minute,
	}, credentialDefaultHostNoop{})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := app.NewTaskService(store, credentials, taskChatConfiguredModelCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	principals := make([]identity.Principal, 2)
	for i := range principals {
		installationID := createTestClientInstallation(t, store.pool, owner.ID, nil)
		token := clientTestToken(t, owner.ID, installationID, identity.KindClient, nil)
		if _, err := store.CreateToken(ctx, token); err != nil {
			t.Fatalf("create client %d token: %v", i+1, err)
		}
		principals[i] = identity.Principal{
			UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindClient,
			TokenID: token.ID, ClientInstallationID: installationID,
		}
		if err := credentials.AuthorizeTaskAction(ctx, owner.ID, identity.KindClient, token.ID, hostValue.ID, nil, identity.ScopeTaskControl); err != nil {
			t.Fatalf("authorize client %d for task control: %v", i+1, err)
		}
	}

	start := make(chan struct{})
	type outcome struct {
		client int
		err    error
	}
	results := make(chan outcome, len(principals))
	for i, principal := range principals {
		go func(client int, principal identity.Principal) {
			<-start
			key := fmt.Sprintf("approval-client-%d-key", client+1)
			err := tasks.RespondToPendingRequest(ctx, principal, claimed.ID, pendingID, 1, task.PendingDecisionApprove, key)
			results <- outcome{client: client + 1, err: err}
		}(i, principal)
	}
	close(start)
	successes, conflicts := 0, 0
	for range principals {
		result := <-results
		if result.err == nil {
			successes++
		} else if domain.CodeOf(result.err) == domain.CodeConflict {
			conflicts++
		} else {
			t.Fatalf("client %d approval error = %v, want one success and one conflict", result.client, result.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent approvals: successes=%d conflicts=%d, want one each", successes, conflicts)
	}
	details, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusQueued || details.PendingRequest == nil ||
		details.PendingRequest.Status != task.PendingRequestApproved || details.PendingRequest.Decision != task.PendingDecisionApprove {
		t.Fatalf("task after concurrent approval = status %q pending %#v err=%v", details.Task.Status, details.PendingRequest, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, claimed.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	responseEvents := 0
	for _, event := range events {
		if event.Type == "task.response" {
			responseEvents++
		}
	}
	if responseEvents != 1 {
		t.Fatalf("concurrent approval emitted %d task.response events, want exactly one: %#v", responseEvents, events)
	}
}

func TestPostgresClarificationAnswerIsVersionedIdempotentAndRestorable(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "clarification-submit", "deploy the project"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "clarification-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("ClaimNextTask() = %#v, found=%t, err=%v", claimed, found, err)
	}
	modelStepID := mustID(t, domain.NewStepID)
	stepID := mustID(t, domain.NewStepID)
	requestID := mustID(t, domain.NewPendingRequestID)
	request := &task.ClarificationRequest{
		ID: requestID, TaskID: claimed.ID, Version: 1, ModelStepID: modelStepID, CallID: "call-ask-user",
		Question: "Which environment should I use?", Status: task.ClarificationPending,
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "clarification_required", Usage: task.Usage{ActiveSeconds: 2, ToolCalls: 1},
		Steps: []task.Step{
			{ID: modelStepID, TaskID: claimed.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded, Title: "Ask the model", Summary: "Model asked the user."},
			{ID: stepID, TaskID: claimed.ID, Sequence: 2, Kind: "clarification", Status: task.StepWaitingUser, Title: "The Agent needs one detail", Summary: request.Question},
		},
		Event: task.Event{TaskID: claimed.ID, Type: "task.waiting_user", Payload: []byte(`{"reason":"clarification_required"}`)}, ClarificationRequest: request,
	}); err != nil {
		t.Fatal(err)
	}
	details, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusWaitingUser || details.Task.WaitingReason != "clarification_required" ||
		len(details.ClarificationRequests) != 1 || details.ClarificationRequests[0].ID != requestID || details.ClarificationRequests[0].Status != task.ClarificationPending {
		t.Fatalf("GetTask() clarification = task %#v requests %#v err=%v", details.Task, details.ClarificationRequests, err)
	}
	answer := "Use staging."
	record := app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "task.clarification.answer", Key: "clarification-answer-key",
		RequestHash: hashBytes("answer:" + answer), ResourceID: claimed.ID, RelatedID: requestID,
	}
	if err := store.AnswerClarificationRequest(ctx, record, claimed.ID, requestID, 1, answer, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.AnswerClarificationRequest(ctx, record, claimed.ID, requestID, 1, answer, "", ""); err != nil {
		t.Fatalf("idempotent answer replay: %v", err)
	}
	details, err = store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusQueued || details.Task.WaitingReason != "" || details.Task.Lease.Version != claimed.Lease.Version+1 ||
		len(details.ClarificationRequests) != 1 || details.ClarificationRequests[0].Status != task.ClarificationAnswered || details.ClarificationRequests[0].Answer != answer {
		t.Fatalf("task after clarification answer = task %#v requests %#v err=%v", details.Task, details.ClarificationRequests, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, claimed.ID, 0, 20)
	if err != nil || len(events) == 0 || events[len(events)-1].Type != "task.response" || !json.Valid(events[len(events)-1].Payload) {
		t.Fatalf("clarification response event = %#v err=%v", events, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[len(events)-1].Payload, &payload); err != nil || payload["clarification_id"] != requestID || payload["answer"] != answer {
		t.Fatalf("response event payload = %#v err=%v", payload, err)
	}
	stale := record
	stale.Key = "clarification-answer-stale"
	stale.RequestHash = hashBytes("stale")
	if err := store.AnswerClarificationRequest(ctx, stale, claimed.ID, requestID, 2, "other answer", "", ""); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale clarification version error = %v", err)
	}
}

func TestPostgresInitialFileDeliveryClarificationIsDurableAndAtomic(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "file-intent-task", "Write a report about the project.")
	classificationStepID := mustID(t, domain.NewStepID)
	clarificationStepID := mustID(t, domain.NewStepID)
	requestID := mustID(t, domain.NewPendingRequestID)
	question := "Do you need a file saved in this Workspace, or is a chat answer enough?"
	submission.Task.FileDeliveryIntent = task.FileDeliveryUnclear
	submission.Task.Status = task.StatusWaitingUser
	submission.Task.WaitingReason = "clarification_required"
	submission.Task.CurrentStep = clarificationStepID
	submission.InitialEvent = task.Event{Type: "task.waiting_user", Payload: []byte(`{"status":"waiting_user","reason":"clarification_required"}`)}
	submission.InitialSteps = []task.Step{
		{ID: classificationStepID, TaskID: submission.Task.ID, Sequence: 1, Kind: "verification", Status: task.StepSucceeded,
			Title: "Check file delivery intent", Summary: "The server could not determine whether a file is required."},
		{ID: clarificationStepID, TaskID: submission.Task.ID, Sequence: 2, Kind: "clarification", Status: task.StepWaitingUser,
			Title: "Confirm file delivery", Summary: question},
	}
	submission.InitialClarificationRequest = &task.ClarificationRequest{
		ID: requestID, TaskID: submission.Task.ID, Version: 1, ModelStepID: classificationStepID,
		CallID: app.FileDeliveryIntentClarificationCallID, Question: question, Status: task.ClarificationPending,
	}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	details, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusWaitingUser || details.Task.FileDeliveryIntent != task.FileDeliveryUnclear ||
		len(details.Steps) != 2 || len(details.ClarificationRequests) != 1 || details.ClarificationRequests[0].ID != requestID {
		t.Fatalf("submitted file clarification = %#v, err=%v", details, err)
	}

	append := newTestTaskSubmission(t, owner.ID, hostValue.ID, "append-before-file-answer", "continue")
	append.Task.ConversationID = created.Task.ConversationID
	append.Message.ConversationID = created.Task.ConversationID
	append.Idempotency.Operation = "conversation.append_message"
	if _, err := store.AppendMessage(ctx, append); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("append bypassed pending file clarification: %v", err)
	}

	answer := "不需要文件，只要聊天答复"
	objective := submission.Task.Objective + "\n\nUser clarification about file delivery: " + answer
	requiredRecord := testIdempotency(owner.ID, "task.clarification.answer", "file-intent-required")
	requiredRecord.ResourceID, requiredRecord.RelatedID = created.Task.ID, requestID
	if err := store.AnswerClarificationRequest(ctx, requiredRecord, created.Task.ID, requestID, 1, "需要交付文件", task.FileDeliveryRequired, submission.Task.Objective+"\n\nUser clarification about file delivery: 需要交付文件"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("file delivery without Workspace error = %v", err)
	}
	details, err = store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusWaitingUser || details.ClarificationRequests[0].Status != task.ClarificationPending {
		t.Fatalf("rejected answer changed pending task state: %#v, err=%v", details, err)
	}
	answerRecord := testIdempotency(owner.ID, "task.clarification.answer", "file-intent-chat-only")
	answerRecord.ResourceID, answerRecord.RelatedID = created.Task.ID, requestID
	if err := store.AnswerClarificationRequest(ctx, answerRecord, created.Task.ID, requestID, 1, answer, task.FileDeliveryNone, objective); err != nil {
		t.Fatal(err)
	}
	details, err = store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusQueued || details.Task.FileDeliveryIntent != task.FileDeliveryNone ||
		details.Task.Objective != objective || details.Task.Lease.Version != created.Task.Lease.Version+1 ||
		details.ClarificationRequests[0].Status != task.ClarificationAnswered || details.ClarificationRequests[0].Answer != answer {
		t.Fatalf("resolved file clarification = %#v, err=%v", details, err)
	}
}

func TestPostgresTaskSubmissionRollbackAndHostScopedLists(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "create-task", "initial task"))
	if err != nil {
		t.Fatal(err)
	}

	conversationID := mustID(t, domain.NewConversationID)
	failed := newTestTaskSubmission(t, owner.ID, hostValue.ID, "rollback", "must roll back")
	failed.Task.ConversationID = conversationID
	failed.Conversation = &conversation.Conversation{ID: conversationID, OwnerID: owner.ID, HostID: hostValue.ID, Title: "rollback"}
	failed.Message.ConversationID = conversationID
	failed.Message.ID = created.Message.ID
	_, err = store.SubmitTask(ctx, failed)
	if err == nil {
		t.Fatal("expected duplicate message id to abort submission")
	}
	if _, err := store.GetConversation(ctx, conversationID, owner.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("conversation survived failed transaction: %v", err)
	}
	if _, err := store.GetTask(ctx, failed.Task.ID, owner.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("task survived failed transaction: %v", err)
	}
	var reserved int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE owner_id=$1 AND key=$2`, owner.ID, failed.Idempotency.Key).Scan(&reserved); err != nil || reserved != 0 {
		t.Fatalf("idempotency reservation survived failed transaction: count=%d err=%v", reserved, err)
	}

	all, err := store.ListConversations(ctx, owner.ID, nil, app.PageRequest{Limit: 20})
	if err != nil || len(all.Items) != 1 {
		t.Fatalf("owner conversation list = %#v, err=%v", all, err)
	}
	otherOwner := createTestUser(t, store, 8, 20_000, 40_000)
	if _, err := store.GetConversation(ctx, created.Conversation.ID, otherOwner.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner conversation read error = %v", err)
	}
	if _, err := store.GetTask(ctx, created.Task.ID, otherOwner.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner task read error = %v", err)
	}
	if _, err := store.ListTaskEvents(ctx, otherOwner.ID, created.Task.ID, 0, 10); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner event read error = %v", err)
	}
	none, err := store.ListConversations(ctx, owner.ID, []string{}, app.PageRequest{Limit: 20})
	if err != nil || len(none.Items) != 0 {
		t.Fatalf("empty Host scope conversation list = %#v, err=%v", none, err)
	}
	tasks, err := store.ListTasks(ctx, owner.ID, app.TaskListRequest{HostIDs: []string{hostValue.ID}, Status: task.StatusQueued, Page: app.PageRequest{Limit: 20}})
	if err != nil || len(tasks.Items) != 1 || tasks.Items[0].ID != created.Task.ID {
		t.Fatalf("Host scoped task list = %#v, err=%v", tasks, err)
	}
	noTasks, err := store.ListTasks(ctx, owner.ID, app.TaskListRequest{HostIDs: []string{}, Page: app.PageRequest{Limit: 20}})
	if err != nil || len(noTasks.Items) != 0 {
		t.Fatalf("empty Host scope task list = %#v, err=%v", noTasks, err)
	}
}

func TestPostgresTaskWritesDuringUserTakeoverAndPreserveReplays(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	if err := store.SetRuntimeInstanceID(ctx, hostValue.ID, hostValue.RuntimeGeneration, hostValue.ControlVersion, "", "runtime-during-takeover"); err != nil {
		t.Fatal(err)
	}

	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "fenced-submit", "initial work")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit while Host is AI-controlled: %v", err)
	}
	appendRequest := app.TaskSubmission{
		Task: task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, HostID: hostValue.ID, ModelID: "model-test"},
		Message: conversation.Message{
			ID: mustID(t, domain.NewMessageID), ConversationID: created.Conversation.ID, OwnerID: owner.ID,
			Role: conversation.RoleUser, Content: "continue",
		},
		Idempotency: testIdempotency(owner.ID, "append-message", "fenced-append"),
	}
	appended, err := store.AppendMessage(ctx, appendRequest)
	if err != nil || appended.Task.ID != created.Task.ID {
		t.Fatalf("append while Host is AI-controlled = %#v, err=%v", appended, err)
	}

	transition, err := store.BeginControlTransition(ctx, control.BeginRequest{
		HostID: hostValue.ID, OwnerID: owner.ID, RequestID: "task-write-fence",
		Action: control.ActionTakeOver, ExpectedControlVersion: 0,
	})
	if err != nil || transition.Phase != control.PhaseBlocking {
		t.Fatalf("begin control takeover = %#v, err=%v", transition, err)
	}

	// A retry with the same idempotency key must still return the committed
	// result after the Host fence changes to taking_over.
	replayedSubmit, err := store.SubmitTask(ctx, submission)
	if err != nil || !replayedSubmit.Replayed || replayedSubmit.Task.ID != created.Task.ID {
		t.Fatalf("submit replay after takeover = %#v, err=%v", replayedSubmit, err)
	}
	replayedAppend, err := store.AppendMessage(ctx, appendRequest)
	if err != nil || !replayedAppend.Replayed || replayedAppend.Message.ID != appendRequest.Message.ID {
		t.Fatalf("append replay after takeover = %#v, err=%v", replayedAppend, err)
	}

	newSubmit := newTestTaskSubmission(t, owner.ID, hostValue.ID, "takeover-submit", "server work")
	if _, err := store.SubmitTask(ctx, newSubmit); err != nil {
		t.Fatalf("new task submission during takeover error = %v", err)
	}
	newAppend := appendRequest
	newAppend.Message.ID = mustID(t, domain.NewMessageID)
	newAppend.Message.Content = "continue server work"
	newAppend.Idempotency = testIdempotency(owner.ID, "append-message", "takeover-append")
	if _, err := store.AppendMessage(ctx, newAppend); err != nil {
		t.Fatalf("new message append during takeover error = %v", err)
	}
	claimedTask, claimed, err := store.ClaimNextTask(ctx, "worker-fenced", time.Minute)
	if err != nil || !claimed || claimedTask.ID != created.Task.ID {
		t.Fatalf("claim during takeover = %#v, claimed=%t, err=%v", claimedTask, claimed, err)
	}
	operation := task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: claimedTask.ID, Key: "takeover-operation",
		RequestHash: hashBytes("takeover-operation"), HostID: hostValue.ID, RuntimeGeneration: hostValue.RuntimeGeneration,
		RuntimeInstanceID: "runtime-during-takeover", ControlVersion: transition.BlockedControlVersion,
		LeaseVersion: claimedTask.Lease.Version, RuntimeLeaseVersion: claimedTask.Lease.Version, Status: task.OperationPrepared}
	if _, _, err := store.CreateOperation(ctx, operation, "worker-fenced"); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("Host operation during takeover error = %v", err)
	}
	messages, err := store.ListMessages(ctx, owner.ID, created.Conversation.ID, app.PageRequest{Limit: 20})
	if err != nil || len(messages.Items) != 3 {
		t.Fatalf("messages after takeover append = %d, err=%v", len(messages.Items), err)
	}
}

func TestPostgresTaskProgressWithoutRunningHost(t *testing.T) {
	for _, state := range []string{"creating", "stopped"} {
		t.Run(state, func(t *testing.T) {
			store, pool := newTemporaryStore(t)
			ctx := context.Background()
			owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
			hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)
			if _, err := pool.Exec(ctx, `UPDATE hosts SET state=$2, runtime_instance_id='unready-runtime' WHERE id=$1`, hostValue.ID, state); err != nil {
				t.Fatal(err)
			}
			created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "non-running-host", "server work"))
			if err != nil {
				t.Fatalf("submit task with Host %s: %v", state, err)
			}
			appendRequest := app.TaskSubmission{
				Task: task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, HostID: hostValue.ID, ModelID: "model-test"},
				Message: conversation.Message{ID: mustID(t, domain.NewMessageID), ConversationID: created.Conversation.ID,
					OwnerID: owner.ID, Role: conversation.RoleUser, Content: "continue"},
				Idempotency: testIdempotency(owner.ID, "append-message", "non-running-host-append"),
			}
			if _, err := store.AppendMessage(ctx, appendRequest); err != nil {
				t.Fatalf("append with Host %s: %v", state, err)
			}
			claimed, found, err := store.ClaimNextTask(ctx, "worker-non-running", time.Minute)
			if err != nil || !found || claimed.ID != created.Task.ID {
				t.Fatalf("claim with Host %s = %#v, found=%t, err=%v", state, claimed, found, err)
			}
			stepID := mustID(t, domain.NewStepID)
			event, err := store.CommitTaskProgress(ctx, app.TaskProgress{
				TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
				Status: task.StatusRunning, CurrentStep: stepID,
				Steps: []task.Step{{ID: stepID, TaskID: claimed.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded}},
				Event: task.Event{Type: "task.step", Payload: []byte(`{"status":"succeeded"}`)},
			})
			if err != nil || event.Type != "task.step" {
				t.Fatalf("commit server progress with Host %s = %#v, err=%v", state, event, err)
			}
			details, err := store.GetTask(ctx, claimed.ID, owner.ID)
			if err != nil || details.Task.CurrentStep != stepID || len(details.Steps) != 1 || details.Steps[0].Status != task.StepSucceeded {
				t.Fatalf("load server progress with Host %s = %#v, err=%v", state, details, err)
			}
			operation := task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: claimed.ID, Key: "unready-host-operation",
				RequestHash: hashBytes("unready-host-operation"), HostID: hostValue.ID, RuntimeGeneration: hostValue.RuntimeGeneration,
				RuntimeInstanceID: "unready-runtime", ControlVersion: hostValue.ControlVersion,
				LeaseVersion: claimed.Lease.Version, RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationPrepared}
			if _, _, err := store.CreateOperation(ctx, operation, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeInvalidState {
				t.Fatalf("Host operation while %s error = %v", state, err)
			}
		})
	}
}

func TestPostgresTaskTargetRejectsDeletedAndOtherOwnerHost(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	otherOwner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	otherHost := createTestHost(t, ctx, store, otherOwner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "deleted-host-seed", "queued work"))
	if err != nil {
		t.Fatal(err)
	}
	appendRequest := app.TaskSubmission{
		Task: task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, HostID: hostValue.ID, ModelID: "model-test"},
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), ConversationID: created.Conversation.ID,
			OwnerID: owner.ID, Role: conversation.RoleUser, Content: "continue"},
		Idempotency: testIdempotency(owner.ID, "append-message", "deleted-host-append"),
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET state='deleted', deleted_at=clock_timestamp() WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "deleted-host-submit", "must be rejected")); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("submit against deleted Host error = %v", err)
	}
	if _, err := store.AppendMessage(ctx, appendRequest); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("append against deleted Host error = %v", err)
	}
	if _, found, err := store.ClaimNextTask(ctx, "deleted-host-worker", time.Minute); err != nil || found {
		t.Fatalf("claim against deleted Host: found=%t, err=%v", found, err)
	}
	valid, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "hostless-after-delete", "server work"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "hostless-after-delete-worker", time.Minute)
	if err != nil || !found || claimed.ID != valid.Task.ID {
		t.Fatalf("deleted Host blocked an unrelated task: task=%#v, found=%t, err=%v", claimed, found, err)
	}
	if _, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, otherHost.ID, "other-owner-host-submit", "must be rejected")); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("submit against another owner's Host error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='completed', finished_at=clock_timestamp() WHERE id=$1`, created.Task.ID); err != nil {
		t.Fatal(err)
	}
	newTurn := newTestTaskSubmission(t, owner.ID, otherHost.ID, "other-owner-host-append", "must be rejected")
	newTurn.Task.ConversationID = created.Conversation.ID
	newTurn.Message.ConversationID = created.Conversation.ID
	newTurn.Idempotency.Operation = "append-message"
	if _, err := store.AppendMessage(ctx, newTurn); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("append against another owner's Host error = %v", err)
	}
}

func TestPostgresTaskTargetDeletionSerializesConcurrentSubmissionAndClaim(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	queued, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "fence-race-existing", "queued work"))
	if err != nil {
		t.Fatal(err)
	}

	// Race task creation and claiming against the Host deletion lock.
	fenceTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fenceTx.Rollback(context.Background()) }()
	if err := fenceTx.QueryRow(ctx, `SELECT id FROM hosts WHERE id=$1 FOR UPDATE`, hostValue.ID).Scan(new(string)); err != nil {
		t.Fatalf("lock Host control fence: %v", err)
	}

	type submitResult struct {
		value app.TaskSubmissionResult
		err   error
	}
	type claimResult struct {
		value   task.Task
		claimed bool
		err     error
	}
	started := make(chan struct{}, 2)
	submitted := make(chan submitResult, 1)
	claimed := make(chan claimResult, 1)
	newSubmission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "fence-race-new", "blocked by deletion")
	go func() {
		started <- struct{}{}
		value, err := store.SubmitTask(ctx, newSubmission)
		submitted <- submitResult{value: value, err: err}
	}()
	go func() {
		started <- struct{}{}
		value, didClaim, err := store.ClaimNextTask(ctx, "worker-fence-race", time.Minute)
		claimed <- claimResult{value: value, claimed: didClaim, err: err}
	}()
	<-started
	<-started
	waitForPostgresLockWait(t, ctx, pool, "%FROM hosts WHERE id=$1 AND owner_id=$2 FOR SHARE%", 2)

	if _, err := fenceTx.Exec(ctx, `UPDATE hosts SET state='deleted', deleted_at=clock_timestamp() WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if err := fenceTx.Commit(ctx); err != nil {
		t.Fatalf("commit Host control fence: %v", err)
	}
	select {
	case result := <-submitted:
		if domain.CodeOf(result.err) != domain.CodeNotFound {
			t.Fatalf("racing task submission error = %v", result.err)
		}
	case <-ctx.Done():
		t.Fatalf("task submission remained blocked after fence commit: %v", ctx.Err())
	}
	select {
	case result := <-claimed:
		if result.err != nil || result.claimed {
			t.Fatalf("racing task claim = %#v, claimed=%t, err=%v", result.value, result.claimed, result.err)
		}
	case <-ctx.Done():
		t.Fatalf("task claim remained blocked after fence commit: %v", ctx.Err())
	}
	var taskCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE host_id=$1`, hostValue.ID).Scan(&taskCount); err != nil || taskCount != 1 {
		t.Fatalf("tasks persisted after fenced race = %d, err=%v", taskCount, err)
	}
	stored, err := store.GetTask(ctx, queued.Task.ID, owner.ID)
	if err != nil || stored.Task.Status != task.StatusQueued || stored.Task.Lease.Version != 0 {
		t.Fatalf("queued task after fenced race = %#v, err=%v", stored.Task, err)
	}
}

func waitForPostgresLockWait(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queryPattern string, want int) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM pg_stat_activity
WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1`, queryPattern).Scan(&waiting); err != nil {
			t.Fatalf("inspect PostgreSQL lock waiters: %v", err)
		}
		if waiting >= want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %d PostgreSQL lock waiters; got %d", want, waiting)
		case <-ticker.C:
		}
	}
}

func TestPostgresWorkspaceSyncCommitWinsConcurrentTaskOperationAdmission(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "operation-commit-race")
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id='ri_operation_commit_race' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	revision := workspaceValue.Revision
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "operation-commit-race", "read the pinned Workspace")
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &revision
	submission.Task.ContextDigest = hashBytes("operation commit race input")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "operation-commit-race-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Workspace task: found=%t task=%s err=%v", found, claimed.ID, err)
	}
	file := syncTestFile("external-update.txt", []byte("new confirmed revision"))
	session := newTestSyncSession(t, workspaceValue, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks([]workspace.File{file}),
		syncTestIdempotency(owner.ID, "workspace.sync.create", "create:operation-commit-race", session.ID)); err != nil {
		t.Fatal(err)
	}

	const advisoryLockKey int64 = 824726197
	if _, err := pool.Exec(ctx, `CREATE FUNCTION wait_for_workspace_revision_race_release() RETURNS trigger AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(824726197);
  RETURN NEW;
END;
$$ LANGUAGE plpgsql`); err != nil {
		t.Fatalf("create Workspace revision test barrier: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER wait_for_workspace_revision_race_release
BEFORE INSERT ON workspace_revisions FOR EACH ROW EXECUTE FUNCTION wait_for_workspace_revision_race_release()`); err != nil {
		t.Fatalf("install Workspace revision test barrier: %v", err)
	}
	advisoryConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = advisoryConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
		advisoryConn.Release()
	}()
	if _, err := advisoryConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		t.Fatal(err)
	}

	type commitResult struct {
		value workspace.SyncSession
		err   error
	}
	committed := make(chan commitResult, 1)
	go func() {
		value, _, err := commitWorkspaceSyncSession(ctx, store, workspaceValue.ID, session.ID,
			syncTestIdempotency(owner.ID, "workspace.sync.commit", "commit:operation-commit-race", session.ID))
		committed <- commitResult{value: value, err: err}
	}()

	waitCtx, waitCancel := context.WithTimeout(ctx, 5*time.Second)
	defer waitCancel()
	// The trigger pauses the commit only after it has acquired the Workspace FOR UPDATE lock.
	waitForPostgresLockWait(t, waitCtx, pool, "%INSERT INTO workspace_revisions%", 1)
	operationDone := make(chan error, 1)
	go func() {
		_, _, err := store.CreateOperation(ctx, task.Operation{
			ID: mustID(t, domain.NewOperationID), TaskID: claimed.ID, Key: "file.read:after-sync-wins",
			RequestHash: hashBytes("read input after sync wins"), HostID: hostValue.ID, WorkspaceID: workspaceValue.ID,
			BindingVersion: binding.BindingVersion, InputRevision: &revision,
			RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_operation_commit_race",
			ControlVersion: hostValue.ControlVersion, LeaseVersion: claimed.Lease.Version,
			RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationPrepared,
		}, claimed.Lease.Owner)
		operationDone <- err
	}()
	waitForPostgresLockWait(t, waitCtx, pool, "%FROM workspaces WHERE id=$1 FOR SHARE%", 1)
	var unlocked bool
	if err := advisoryConn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, advisoryLockKey).Scan(&unlocked); err != nil || !unlocked {
		t.Fatalf("release Workspace revision test barrier: unlocked=%t err=%v", unlocked, err)
	}

	select {
	case result := <-committed:
		if result.err != nil || result.value.State != workspace.SyncCompleted || result.value.TargetRevision != revision+1 {
			t.Fatalf("Workspace commit that won admission race = %#v, err=%v", result.value, result.err)
		}
	case <-waitCtx.Done():
		t.Fatalf("Workspace sync commit did not finish: %v", waitCtx.Err())
	}
	select {
	case err := <-operationDone:
		if domain.CodeOf(err) != domain.CodeWorkspaceConflict {
			t.Fatalf("Task operation admitted after Workspace commit error = %v, want workspace conflict", err)
		}
	case <-waitCtx.Done():
		t.Fatalf("Task operation did not finish after Workspace commit: %v", waitCtx.Err())
	}
	var operationCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_operations WHERE task_id=$1`, claimed.ID).Scan(&operationCount); err != nil || operationCount != 0 {
		t.Fatalf("Task operations persisted after stale admission = %d, err=%v", operationCount, err)
	}
}

func TestPostgresTaskLeaseFencesOperationAndRecoversUnknown(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "lease-task")
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id='ri_test' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	revision := workspaceValue.Revision
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "lease-task", "run this")
	submission.Task.WorkspaceID, submission.Task.InputRevision = workspaceValue.ID, &revision
	submission.Task.ContextDigest = hashBytes("lease task workspace input")
	submitted, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	first, claimed, err := store.ClaimNextTask(ctx, "worker-one", time.Minute)
	if err != nil || !claimed || first.ID != submitted.Task.ID || first.Lease.Version != 1 {
		t.Fatalf("first task claim = %#v, claimed=%t, err=%v", first, claimed, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET control_state='user', control_version=1 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	blockedOperation := task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: first.ID, Key: "blocked-during-takeover", RequestHash: hashBytes("blocked"),
		HostID: hostValue.ID, WorkspaceID: workspaceValue.ID, BindingVersion: binding.BindingVersion,
		InputRevision: &revision, WorkspaceCheckpointRevision: &revision,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test",
		ControlVersion: 0, LeaseVersion: first.Lease.Version, RuntimeLeaseVersion: first.Lease.Version, Status: task.OperationPrepared,
	}
	if _, _, err := store.CreateOperation(ctx, blockedOperation, "worker-one"); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("create operation during user control error = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET control_state='ai', control_version=0 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	operationID := mustID(t, domain.NewOperationID)
	operation, replayed, err := store.CreateOperation(ctx, task.Operation{
		ID: operationID, TaskID: first.ID, Key: "browser.click", RequestHash: hashBytes("click"),
		HostID: hostValue.ID, WorkspaceID: workspaceValue.ID, BindingVersion: binding.BindingVersion,
		InputRevision: &revision, WorkspaceCheckpointRevision: &revision,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test",
		ControlVersion: 0, LeaseVersion: first.Lease.Version, RuntimeLeaseVersion: first.Lease.Version, Status: task.OperationPrepared,
	}, "worker-one")
	if err != nil || replayed || operation.Status != task.OperationPrepared || operation.WorkspaceID != workspaceValue.ID || operation.BindingVersion != binding.BindingVersion ||
		operation.InputRevision == nil || *operation.InputRevision != revision || operation.WorkspaceCheckpointRevision == nil || *operation.WorkspaceCheckpointRevision != revision {
		t.Fatalf("create prepared operation = %#v, replayed=%t, err=%v", operation, replayed, err)
	}
	loadedOperation, err := store.GetOperation(ctx, owner.ID, first.ID, operation.Key)
	if err != nil || loadedOperation.ID != operation.ID || loadedOperation.WorkspaceID != operation.WorkspaceID || loadedOperation.BindingVersion != operation.BindingVersion {
		t.Fatalf("get owned operation = %#v, err=%v", loadedOperation, err)
	}
	changedBinding := operation
	changedBinding.RequestHash = hashBytes("changed click")
	changedBinding.ID = mustID(t, domain.NewOperationID)
	if _, _, err := store.CreateOperation(ctx, changedBinding, "worker-one"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("replay with changed request hash error = %v", err)
	}
	otherOwner := createTestUser(t, store, 8, 20_000, 40_000)
	if _, err := store.GetOperation(ctx, otherOwner.ID, first.ID, operation.Key); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner operation lookup error = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET control_state='user', control_version=1 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: first.ID, OperationKey: operation.Key, LeaseOwner: "worker-one", LeaseVersion: first.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: first.Lease.Version, Status: task.OperationDispatched,
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("dispatch operation during user control error = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET control_state='ai', control_version=0 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	operation, err = store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: first.ID, OperationKey: operation.Key, LeaseOwner: "worker-one", LeaseVersion: first.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: first.Lease.Version, Status: task.OperationDispatched,
	})
	if err != nil || operation.Status != task.OperationDispatched {
		t.Fatalf("dispatch operation = %#v, err=%v", operation, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	second, claimed, err := store.ClaimNextTask(ctx, "worker-two", time.Minute)
	if err != nil || !claimed || second.Lease.Version != 2 {
		t.Fatalf("recovered task claim = %#v, claimed=%t, err=%v", second, claimed, err)
	}
	if _, err := store.RenewTaskLease(ctx, first.ID, "worker-one", first.Lease.Version, time.Minute); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale lease renewal error = %v", err)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: first.ID, LeaseOwner: "worker-one", LeaseVersion: first.Lease.Version, Status: task.StatusCompleted,
		Event: task.Event{Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale progress commit error = %v", err)
	}
	unknownOperations, err := store.ListUnknownOperations(ctx, owner.ID, first.ID)
	if err != nil || len(unknownOperations) != 1 || unknownOperations[0].Key != operation.Key || unknownOperations[0].LeaseVersion != second.Lease.Version ||
		unknownOperations[0].WorkspaceID != workspaceValue.ID || unknownOperations[0].BindingVersion != binding.BindingVersion ||
		unknownOperations[0].WorkspaceCheckpointRevision == nil || *unknownOperations[0].WorkspaceCheckpointRevision != revision ||
		unknownOperations[0].RuntimeLeaseVersion != first.Lease.Version || unknownOperations[0].RuntimeInstanceID != "ri_test" {
		t.Fatalf("unknown operations requiring reconciliation = %#v, err=%v", unknownOperations, err)
	}
	unknownOperation, err := store.GetOperation(ctx, owner.ID, first.ID, operation.Key)
	if err != nil || unknownOperation.Status != task.OperationUnknown || unknownOperation.RuntimeLeaseVersion != first.Lease.Version || unknownOperation.LeaseVersion != second.Lease.Version ||
		unknownOperation.WorkspaceID != workspaceValue.ID || unknownOperation.BindingVersion != binding.BindingVersion ||
		unknownOperation.WorkspaceCheckpointRevision == nil || *unknownOperation.WorkspaceCheckpointRevision != revision {
		t.Fatalf("get recovered operation = %#v, err=%v", unknownOperation, err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: first.ID, OperationKey: operation.Key, LeaseOwner: "worker-one", LeaseVersion: first.Lease.Version,
		Status: task.OperationSucceeded, ExternalID: "external",
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale worker update error = %v", err)
	}
	operation, err = store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: second.ID, OperationKey: operation.Key, LeaseOwner: "worker-two", LeaseVersion: second.Lease.Version,
		Status: task.OperationSucceeded, ExternalID: "external", Result: "verified",
	})
	if err != nil || operation.Status != task.OperationSucceeded {
		t.Fatalf("reconciled operation update = %#v, err=%v", operation, err)
	}
	unknownOperations, err = store.ListUnknownOperations(ctx, owner.ID, first.ID)
	if err != nil || len(unknownOperations) != 0 {
		t.Fatalf("resolved unknown operations = %#v, err=%v", unknownOperations, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, first.ID, 0, 20)
	if err != nil || len(events) != 7 {
		t.Fatalf("operation recovery events = %#v, err=%v", events, err)
	}
	for index, event := range events {
		if event.Sequence != int64(index+1) {
			t.Fatalf("task event sequence at index %d = %d", index, event.Sequence)
		}
	}
	var recovery map[string]any
	if err := json.Unmarshal(events[5].Payload, &recovery); err != nil || recovery["requires_reconciliation"] != true || events[5].Type != "task.operation" {
		t.Fatalf("recovery event = %#v, decoded=%#v err=%v", events[5], recovery, err)
	}
	prepared, _, err := store.CreateOperation(ctx, task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: second.ID, Key: "agent:confirmed-not-found",
		RequestHash: hashBytes("confirmed-not-found"), HostID: hostValue.ID, WorkspaceID: workspaceValue.ID,
		BindingVersion: binding.BindingVersion, InputRevision: &revision, WorkspaceCheckpointRevision: &revision,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		LeaseVersion: second.Lease.Version, RuntimeLeaseVersion: second.Lease.Version, Status: task.OperationPrepared,
	}, "worker-two")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: second.ID, OperationKey: prepared.Key, LeaseOwner: "worker-two", LeaseVersion: second.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: second.Lease.Version, Status: task.OperationDispatched,
	})
	if err != nil || prepared.Status != task.OperationDispatched {
		t.Fatalf("dispatch operation for not-found recovery = %#v, err=%v", prepared, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	third, claimed, err := store.ClaimNextTask(ctx, "worker-three", time.Minute)
	if err != nil || !claimed || third.Lease.Version != 3 {
		t.Fatalf("third task claim = %#v, claimed=%t, err=%v", third, claimed, err)
	}
	unknownNotFound, err := store.GetOperation(ctx, owner.ID, third.ID, prepared.Key)
	if err != nil || unknownNotFound.Status != task.OperationUnknown || unknownNotFound.LeaseVersion != third.Lease.Version || unknownNotFound.RuntimeLeaseVersion != second.Lease.Version {
		t.Fatalf("operation before confirmed-not-found rearm = %#v, err=%v", unknownNotFound, err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: third.ID, OperationKey: prepared.Key, LeaseOwner: "worker-two", LeaseVersion: second.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: third.Lease.Version, RearmAfterNotFound: true, Status: task.OperationPrepared,
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale worker rearm error = %v", err)
	}
	rearmed, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: third.ID, OperationKey: prepared.Key, LeaseOwner: "worker-three", LeaseVersion: third.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: third.Lease.Version, RearmAfterNotFound: true, Status: task.OperationPrepared,
	})
	if err != nil || rearmed.ID != prepared.ID || rearmed.Status != task.OperationPrepared || rearmed.RuntimeLeaseVersion != third.Lease.Version {
		t.Fatalf("rearm same operation after confirmed not-found = %#v, err=%v", rearmed, err)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: third.ID, LeaseOwner: "worker-three", LeaseVersion: third.Lease.Version, Status: task.StatusCompleted,
		Usage: task.Usage{ActiveSeconds: 2, ToolCalls: 1}, Event: task.Event{Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
	}); err != nil {
		t.Fatalf("commit task progress: %v", err)
	}
}

func TestPostgresAdoptsPreparedOperationAfterLeaseReclaim(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id='ri_test' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "prepared-recovery", "run once"))
	if err != nil {
		t.Fatal(err)
	}
	first, claimed, err := store.ClaimNextTask(ctx, "worker-one", time.Minute)
	if err != nil || !claimed || first.ID != submitted.Task.ID || first.Lease.Version != 1 {
		t.Fatalf("first claim = %#v, claimed=%t, err=%v", first, claimed, err)
	}
	prepared, replayed, err := store.CreateOperation(ctx, task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: first.ID, Key: "agent:prepared-before-dispatch",
		RequestHash: hashBytes("prepared"), HostID: hostValue.ID, RuntimeGeneration: hostValue.RuntimeGeneration,
		RuntimeInstanceID: "ri_test", ControlVersion: 0, LeaseVersion: first.Lease.Version,
		RuntimeLeaseVersion: first.Lease.Version, Status: task.OperationPrepared,
	}, first.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("create prepared operation = %#v, replayed=%t, err=%v", prepared, replayed, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	second, claimed, err := store.ClaimNextTask(ctx, "worker-two", time.Minute)
	if err != nil || !claimed || second.Lease.Version != 2 {
		t.Fatalf("second claim = %#v, claimed=%t, err=%v", second, claimed, err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: second.ID, OperationKey: prepared.Key, LeaseOwner: first.Lease.Owner, LeaseVersion: first.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: first.Lease.Version, Status: task.OperationDispatched,
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale worker dispatch error = %v", err)
	}
	adopted, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: second.ID, OperationKey: prepared.Key, LeaseOwner: second.Lease.Owner, LeaseVersion: second.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: second.Lease.Version, AdoptPrepared: true, Status: task.OperationPrepared,
	})
	if err != nil || adopted.ID != prepared.ID || adopted.Status != task.OperationPrepared ||
		adopted.LeaseVersion != second.Lease.Version || adopted.RuntimeLeaseVersion != second.Lease.Version {
		t.Fatalf("adopted prepared operation = %#v, err=%v", adopted, err)
	}
	dispatched, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: second.ID, OperationKey: prepared.Key, LeaseOwner: second.Lease.Owner, LeaseVersion: second.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: second.Lease.Version, Status: task.OperationDispatched,
	})
	if err != nil || dispatched.ID != prepared.ID || dispatched.Status != task.OperationDispatched {
		t.Fatalf("dispatch adopted operation = %#v, err=%v", dispatched, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, second.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var sawAdoption bool
	for _, event := range events {
		var payload map[string]any
		if json.Unmarshal(event.Payload, &payload) == nil && payload["adoption_reason"] == "task_lease_reclaimed_before_dispatch" {
			sawAdoption = true
		}
	}
	if !sawAdoption {
		t.Fatal("prepared operation adoption was not persisted in the task event log")
	}
}

func TestPostgresClaimNextTaskIsExclusiveAcrossStores(t *testing.T) {
	store, pool := newTemporaryStore(t)
	otherStore := New(pool)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "claim-race", "race workers"))
	if err != nil {
		t.Fatal(err)
	}

	type claimResult struct {
		task    task.Task
		claimed bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	for _, candidate := range []struct {
		store  *Store
		worker string
	}{{store, "worker-one"}, {otherStore, "worker-two"}} {
		candidate := candidate
		go func() {
			defer workers.Done()
			<-start
			claimedTask, claimed, err := candidate.store.ClaimNextTask(ctx, candidate.worker, time.Minute)
			results <- claimResult{task: claimedTask, claimed: claimed, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	claims := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("claim task: %v", result.err)
		}
		if result.claimed {
			claims++
			if result.task.Lease.Version != 1 || result.task.Lease.Owner == "" {
				t.Fatalf("claimed task lease = %#v", result.task.Lease)
			}
		}
	}
	if claims != 1 {
		t.Fatalf("concurrent workers claimed task %d times, want exactly once", claims)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, submitted.Task.ID, 0, 10)
	if err != nil || len(events) != 2 || events[1].Sequence != 2 || events[1].Type != "task.status" {
		t.Fatalf("claim status event = %#v, err=%v", events, err)
	}
	var claimPayload map[string]any
	if err := json.Unmarshal(events[1].Payload, &claimPayload); err != nil || claimPayload["status"] != string(task.StatusRunning) {
		t.Fatalf("claim status payload = %#v err=%v", claimPayload, err)
	}
}

func TestPostgresTaskBatchItemAndValidatedArtifactPersistence(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "durable-primitives", "generate a report")
	artifactWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "artifact proof")
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: artifactWorkspace, Directory: artifactWorkspace.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: artifactWorkspace.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, artifactWorkspace.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, artifactWorkspace.Revision, 0); err != nil {
		t.Fatal(err)
	}
	revision := int64(0)
	submission.Task.WorkspaceID = artifactWorkspace.ID
	submission.Task.InputRevision = &revision
	submission.Task.ContextDigest = hashBytes("workspace input manifest")
	proofs := seedTaskArtifactManifest(t, store, artifactWorkspace.ID, 0, map[string][]byte{
		"artifacts/report.pdf": []byte("report"), "artifacts/summary.txt": []byte("summary"),
	})
	itemID := mustID(t, domain.NewBatchItemID)
	submission.BatchItems = []task.BatchItem{{ID: itemID, Key: "report", Input: "write report"}}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit task with batch item: %v", err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, "worker-one", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim task: claimed=%t err=%v", ok, err)
	}
	stepID := mustID(t, domain.NewStepID)
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: "worker-one", LeaseVersion: claimed.Lease.Version,
		Status: task.StatusRunning, CurrentStep: "Write report",
		Steps: []task.Step{{ID: stepID, TaskID: created.Task.ID, Sequence: 1, Kind: "tool", Status: task.StepRunning, Title: "Write report"}},
		Event: task.Event{Type: "task.step", Payload: []byte(`{"step_id":"` + stepID + `"}`)},
	}); err != nil {
		t.Fatalf("persist artifact source step: %v", err)
	}
	details, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || len(details.BatchItems) != 1 {
		t.Fatalf("get submitted batch item: %#v err=%v", details.BatchItems, err)
	}
	batchItem := details.BatchItems[0]
	if batchItem.Attempt != 1 {
		t.Fatalf("new batch item attempt = %d, want 1", batchItem.Attempt)
	}
	otherOwner := createTestUser(t, store, 8, 20_000, 40_000)
	if _, err := store.UpdateBatchItem(ctx, otherOwner.ID, batchItem, "worker-one", claimed.Lease.Version); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner batch item update error = %v", err)
	}
	batchItem.Status = task.BatchItemRunning
	updated, err := store.UpdateBatchItem(ctx, owner.ID, batchItem, "worker-one", claimed.Lease.Version)
	if err != nil || updated.Status != task.BatchItemRunning {
		t.Fatalf("mark batch item running: %#v err=%v", updated, err)
	}
	batchItem = updated
	batchItem.Status = task.BatchItemSucceeded
	batchItem.Result = "report.pdf"
	updated, err = store.UpdateBatchItem(ctx, owner.ID, batchItem, "worker-one", claimed.Lease.Version)
	if err != nil || updated.Status != task.BatchItemSucceeded || updated.Result != "report.pdf" {
		t.Fatalf("complete batch item: %#v err=%v", updated, err)
	}
	if _, err := store.UpdateBatchItem(ctx, owner.ID, batchItem, "stale-worker", claimed.Lease.Version); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale batch item update error = %v", err)
	}

	artifact := task.Artifact{
		ID: mustID(t, domain.NewArtifactID), TaskID: created.Task.ID, StepID: stepID,
		BatchItemID: batchItem.ID, BatchAttempt: batchItem.Attempt,
		Label: "Report", Path: "artifacts/report.pdf", MediaType: "application/pdf",
	}
	artifact = addArtifactProof(artifact, proofs[artifact.Path])
	if _, err := store.RecordValidatedArtifact(ctx, otherOwner.ID, artifact, "worker-one", claimed.Lease.Version); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner artifact record error = %v", err)
	}
	stored, err := store.RecordValidatedArtifact(ctx, owner.ID, artifact, "worker-one", claimed.Lease.Version)
	if err != nil || stored.ID != artifact.ID || stored.StepID != stepID || stored.Path != artifact.Path || stored.CreatedAt.IsZero() {
		t.Fatalf("record validated artifact metadata: %#v err=%v", stored, err)
	}
	replayed, err := store.RecordValidatedArtifact(ctx, owner.ID, artifact, "worker-one", claimed.Lease.Version)
	if err != nil || replayed.ID != stored.ID || !replayed.CreatedAt.Equal(stored.CreatedAt) {
		t.Fatalf("idempotent artifact metadata retry: %#v err=%v", replayed, err)
	}
	newArtifactID := artifact
	newArtifactID.ID = mustID(t, domain.NewArtifactID)
	replayed, err = store.RecordValidatedArtifact(ctx, owner.ID, newArtifactID, "worker-one", claimed.Lease.Version)
	if err != nil || replayed.ID != stored.ID {
		t.Fatalf("artifact path replay after crash: %#v err=%v; want existing artifact %q", replayed, err, stored.ID)
	}
	changedMetadata := newArtifactID
	changedMetadata.Label = "Different report"
	if _, err := store.RecordValidatedArtifact(ctx, owner.ID, changedMetadata, "worker-one", claimed.Lease.Version); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("duplicate artifact path with changed metadata error = %v", err)
	}
	ordinaryArtifact := task.Artifact{
		ID: mustID(t, domain.NewArtifactID), TaskID: created.Task.ID, StepID: stepID,
		Label: "Summary", Path: "artifacts/summary.txt", MediaType: "text/plain",
	}
	ordinaryArtifact = addArtifactProof(ordinaryArtifact, proofs[ordinaryArtifact.Path])
	ordinaryStored, err := store.RecordValidatedArtifact(ctx, owner.ID, ordinaryArtifact, "worker-one", claimed.Lease.Version)
	if err != nil {
		t.Fatalf("record ordinary task artifact: %v", err)
	}
	ordinaryReplay := ordinaryArtifact
	ordinaryReplay.ID = mustID(t, domain.NewArtifactID)
	ordinaryReplay, err = store.RecordValidatedArtifact(ctx, owner.ID, ordinaryReplay, "worker-one", claimed.Lease.Version)
	if err != nil || ordinaryReplay.ID != ordinaryStored.ID || ordinaryReplay.BatchItemID != "" || ordinaryReplay.BatchAttempt != 0 {
		t.Fatalf("ordinary artifact path replay: %#v err=%v", ordinaryReplay, err)
	}
	badPath := artifact
	badPath.ID = mustID(t, domain.NewArtifactID)
	badPath.Path = "../outside.txt"
	if _, err := store.RecordValidatedArtifact(ctx, owner.ID, badPath, "worker-one", claimed.Lease.Version); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("traversing artifact path error = %v", err)
	}
	missingStep := artifact
	missingStep.ID = mustID(t, domain.NewArtifactID)
	missingStep.StepID = mustID(t, domain.NewStepID)
	if _, err := store.RecordValidatedArtifact(ctx, owner.ID, missingStep, "worker-one", claimed.Lease.Version); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("artifact with missing source step error = %v", err)
	}

	otherSubmission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "other-artifact-task", "another task")
	otherItemID := mustID(t, domain.NewBatchItemID)
	otherSubmission.BatchItems = []task.BatchItem{{ID: otherItemID, Key: "other", Input: "other item"}}
	_, err = store.SubmitTask(ctx, otherSubmission)
	if err != nil {
		t.Fatalf("submit second task for artifact binding check: %v", err)
	}
	crossTask := artifact
	crossTask.ID = mustID(t, domain.NewArtifactID)
	crossTask.BatchItemID = otherItemID
	if _, err := store.RecordValidatedArtifact(ctx, owner.ID, crossTask, "worker-one", claimed.Lease.Version); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("artifact referencing another task's batch item error = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_artifacts (id, task_id, step_id, label, path, media_type, batch_item_id, batch_attempt)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, mustID(t, domain.NewArtifactID), created.Task.ID, stepID,
		"Cross task", "artifacts/cross-task.pdf", "application/pdf", crossTask.BatchItemID, crossTask.BatchAttempt); err == nil {
		t.Fatal("database accepted an artifact whose batch item belongs to another task")
	}

	details, err = store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || len(details.Artifacts) != 2 || details.Artifacts[0].ID != artifact.ID || len(details.BatchItems) != 1 || details.BatchItems[0].Status != task.BatchItemSucceeded {
		t.Fatalf("durable task details = %#v err=%v", details, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, created.Task.ID, 1, 10)
	if err != nil || len(events) != 6 || events[0].Type != "task.status" || events[1].Type != "task.step" || events[2].Type != "task.batch_item" || events[3].Type != "task.batch_item" || events[4].Type != "task.artifact" || events[5].Type != "task.artifact" {
		t.Fatalf("batch/artifact events = %#v err=%v", events, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[4].Payload, &payload); err != nil || payload["artifact_id"] != artifact.ID || payload["step_id"] != stepID || payload["path"] != artifact.Path || payload["media_type"] != artifact.MediaType || payload["batch_item_id"] != batchItem.ID || payload["batch_attempt"] != float64(1) {
		t.Fatalf("artifact event payload = %#v err=%v", payload, err)
	}
}

func TestPostgresTaskCompletionRejectsArtifactAfterWorkspaceRevisionChanges(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "batch artifact completion")
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0); err != nil {
		t.Fatal(err)
	}
	proofs := seedTaskArtifactManifest(t, store, workspaceValue.ID, 0, map[string][]byte{"output/report.txt": []byte("saved")})
	revision := int64(0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "persisted-artifact-completion", "generate and save a report")
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &revision
	submission.Task.ContextDigest = hashBytes("workspace input manifest")
	submission.Task.FileDeliveryIntent = task.FileDeliveryRequired
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "worker-persisted-artifact", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim task: claimed=%t task=%s err=%v", found, claimed.ID, err)
	}
	stepID := mustID(t, domain.NewStepID)
	step := task.Step{ID: stepID, TaskID: created.Task.ID, Sequence: 1, Kind: "tool", Status: task.StepSucceeded, Title: "Save report"}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusRunning, CurrentStep: stepID, Steps: []task.Step{step},
		Event: task.Event{Type: "task.step", Payload: []byte(`{"step_id":"` + stepID + `"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	artifact := addArtifactProof(task.Artifact{
		ID: mustID(t, domain.NewArtifactID), TaskID: created.Task.ID, StepID: stepID,
		Label: "Report", Path: "output/report.txt", MediaType: "text/plain",
	}, proofs["output/report.txt"])
	if _, err := store.RecordValidatedArtifact(ctx, owner.ID, artifact, claimed.Lease.Owner, claimed.Lease.Version); err != nil {
		t.Fatalf("persist batch artifact before completion: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revisions (workspace_id, revision, file_count, total_bytes)
VALUES ($1, 1, 0, 0)`, workspaceValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_revision_files SET valid_to_revision=1
WHERE workspace_id=$1 AND file_path=$2`, workspaceValue.ID, artifact.Path); err != nil {
		t.Fatal(err)
	}
	seedTaskArtifactManifest(t, store, workspaceValue.ID, 1, map[string][]byte{artifact.Path: []byte("saved")})
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET revision=1 WHERE id=$1`, workspaceValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusCompleted, Result: "report saved", Event: task.Event{Type: "task.result", Payload: []byte(`{"result":"saved"}`)},
	}); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("complete after Workspace revision changed error = %v, want workspace conflict", err)
	}
	result, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || result.Task.Status != task.StatusRunning || len(result.Artifacts) != 1 {
		t.Fatalf("task with stale input = status %q artifacts %#v err=%v", result.Task.Status, result.Artifacts, err)
	}
}

func TestPostgresTaskArtifactsMustMatchWorkspaceCheckpoint(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "artifact checkpoint")
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0); err != nil {
		t.Fatal(err)
	}
	proofs := seedTaskArtifactManifest(t, store, workspaceValue.ID, 0, map[string][]byte{"output/report.txt": []byte("saved")})
	revision := int64(0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "artifact-checkpoint", "save report")
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &revision
	submission.Task.ContextDigest = hashBytes("artifact checkpoint input")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "worker-artifact-checkpoint", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim task: found=%t task=%s err=%v", found, claimed.ID, err)
	}
	stepID := mustID(t, domain.NewStepID)
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusRunning, CurrentStep: stepID,
		Steps: []task.Step{{ID: stepID, TaskID: created.Task.ID, Sequence: 1, Kind: "tool", Status: task.StepSucceeded, Title: "Save report"}},
		Event: task.Event{Type: "task.step", Payload: []byte(`{"step_id":"` + stepID + `"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	artifact := addArtifactProof(task.Artifact{
		ID: mustID(t, domain.NewArtifactID), TaskID: created.Task.ID, StepID: stepID,
		Label: "Report", Path: "output/report.txt", MediaType: "text/plain",
	}, proofs["output/report.txt"])
	if _, err := store.RecordValidatedArtifact(ctx, owner.ID, artifact, claimed.Lease.Owner, claimed.Lease.Version); err != nil {
		t.Fatalf("record artifact at initial checkpoint: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revisions (workspace_id, revision, file_count, total_bytes) VALUES ($1,1,1,5)`, workspaceValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_revision_files SET valid_to_revision=1 WHERE workspace_id=$1 AND file_path=$2`, workspaceValue.ID, artifact.Path); err != nil {
		t.Fatal(err)
	}
	seedTaskArtifactManifest(t, store, workspaceValue.ID, 1, map[string][]byte{artifact.Path: []byte("saved")})
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET revision=1 WHERE id=$1`, workspaceValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET workspace_checkpoint_revision=1 WHERE id=$1`, created.Task.ID); err != nil {
		t.Fatal(err)
	}
	stale := artifact
	stale.ID = mustID(t, domain.NewArtifactID)
	if _, err := store.RecordValidatedArtifact(ctx, owner.ID, stale, claimed.Lease.Owner, claimed.Lease.Version); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("record artifact from an older checkpoint error = %v, want invalid_state", err)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusCompleted, Result: "report saved", Event: task.Event{Type: "task.result", Payload: []byte(`{"result":"saved"}`)},
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("complete with an artifact from an older checkpoint error = %v, want invalid_state", err)
	}
}

func TestPostgresTaskCompletionCommitsOnlyProvedArtifactsAtomically(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "completion artifact")
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0); err != nil {
		t.Fatal(err)
	}
	proofs := seedTaskArtifactManifest(t, store, workspaceValue.ID, 0, map[string][]byte{"output/result.txt": []byte("saved")})
	revision := int64(0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "artifact-completion", "save result")
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &revision
	submission.Task.ContextDigest = hashBytes("workspace input manifest")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "worker-artifact", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim artifact task: claimed=%t task=%s err=%v", found, claimed.ID, err)
	}
	stepID := mustID(t, domain.NewStepID)
	step := task.Step{ID: stepID, TaskID: created.Task.ID, Sequence: 1, Kind: "verify", Status: task.StepSucceeded, Title: "Verify output"}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: "worker-artifact", LeaseVersion: claimed.Lease.Version,
		Status: task.StatusRunning, CurrentStep: stepID, Steps: []task.Step{step},
		Event: task.Event{Type: "task.step", Payload: []byte(`{"step_id":"` + stepID + `"}`)},
	}); err != nil {
		t.Fatalf("persist artifact source step: %v", err)
	}

	artifact := addArtifactProof(task.Artifact{
		ID: mustID(t, domain.NewArtifactID), TaskID: created.Task.ID, StepID: stepID,
		Label: "Result", Path: "output/result.txt", MediaType: "text/plain",
	}, proofs["output/result.txt"])
	completed := app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: "worker-artifact", LeaseVersion: claimed.Lease.Version,
		Status: task.StatusCompleted, CurrentStep: stepID, Result: "saved", Steps: []task.Step{step},
		Event: task.Event{Type: "task.result", Payload: []byte(`{"result":"saved"}`)},
	}
	for _, mutate := range []struct {
		name string
		edit func(*task.Artifact)
	}{
		{name: "digest", edit: func(value *task.Artifact) {
			value.SHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		}},
		{name: "size", edit: func(value *task.Artifact) { value.SizeBytes++ }},
		{name: "path", edit: func(value *task.Artifact) { value.Path = "output/other.txt" }},
	} {
		bad := artifact
		bad.ID = mustID(t, domain.NewArtifactID)
		mutate.edit(&bad)
		attempt := completed
		attempt.Artifacts = []task.Artifact{bad}
		if _, err := store.CommitTaskProgress(ctx, attempt); domain.CodeOf(err) != domain.CodeInvalidState {
			t.Fatalf("tampered artifact %s completion error = %v, want invalid_state", mutate.name, err)
		}
	}
	details, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusRunning || len(details.Artifacts) != 0 {
		t.Fatalf("failed proof partially committed: status=%q artifacts=%#v err=%v", details.Task.Status, details.Artifacts, err)
	}

	if _, err := store.pool.Exec(ctx, `INSERT INTO task_artifacts (id, task_id, step_id, label, path, media_type, batch_item_id, batch_attempt)
VALUES ($1,$2,$3,'Legacy','output/legacy.txt','text/plain',NULL,NULL)`, mustID(t, domain.NewArtifactID), created.Task.ID, stepID); err != nil {
		t.Fatalf("seed legacy artifact without proof: %v", err)
	}
	completed.Artifacts = nil
	if _, err := store.CommitTaskProgress(ctx, completed); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("completion with an unproved artifact row error = %v, want invalid_state", err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM task_artifacts WHERE task_id=$1 AND path='output/legacy.txt'`, created.Task.ID); err != nil {
		t.Fatalf("remove legacy artifact fixture: %v", err)
	}
	completed.Artifacts = []task.Artifact{artifact}
	event, err := store.CommitTaskProgress(ctx, completed)
	if err != nil || event.Type != "task.result" {
		t.Fatalf("complete with committed Workspace proof: event=%#v err=%v", event, err)
	}
	details, err = store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusCompleted || len(details.Artifacts) != 1 ||
		details.Artifacts[0].WorkspaceID != workspaceValue.ID || details.Artifacts[0].Revision != 0 ||
		details.Artifacts[0].SizeBytes != 5 || details.Artifacts[0].SHA256 != proofs["output/result.txt"].SHA256 {
		t.Fatalf("completed task proof state = task:%q artifacts:%#v err=%v", details.Task.Status, details.Artifacts, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, created.Task.ID, 0, 10)
	if err != nil || len(events) != 5 || events[3].Type != "task.artifact" || events[4].Type != "task.result" {
		t.Fatalf("artifact and completion event sequence = %#v err=%v", events, err)
	}

	noFileSubmission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "no-file-completion", "answer question")
	noFile, err := store.SubmitTask(ctx, noFileSubmission)
	if err != nil {
		t.Fatal(err)
	}
	noFileClaim, found, err := store.ClaimNextTask(ctx, "worker-no-file", time.Minute)
	if err != nil || !found || noFileClaim.ID != noFile.Task.ID {
		t.Fatalf("claim no-file task: claimed=%t task=%s err=%v", found, noFileClaim.ID, err)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: noFile.Task.ID, LeaseOwner: "worker-no-file", LeaseVersion: noFileClaim.Lease.Version,
		Status: task.StatusCompleted, Result: "answer", Event: task.Event{Type: "task.result", Payload: []byte(`{"result":"answer"}`)},
	}); err != nil {
		t.Fatalf("complete no-file task: %v", err)
	}
}

func TestPostgresTaskCompletionRejectsFailedBatchAttemptArtifacts(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "batch artifact proof")
	binding, err := store.CreateWorkspaceBinding(ctx, workspace.Binding{
		OwnerID: owner.ID, HostID: hostValue.ID, Workspace: workspaceValue, Directory: workspaceValue.ID,
		State: workspace.BindingPreparing, BindingVersion: 1, DesiredRevision: workspaceValue.Revision,
		SyncState: workspace.BindingSyncHealthy, SyncConflicts: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0); err != nil {
		t.Fatal(err)
	}
	proofs := seedTaskArtifactManifest(t, store, workspaceValue.ID, 0, map[string][]byte{"output/item.txt": []byte("item")})
	revision := int64(0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "failed-batch-proof", "process items")
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &revision
	submission.Task.ContextDigest = hashBytes("workspace input manifest")
	itemID := mustID(t, domain.NewBatchItemID)
	submission.BatchItems = []task.BatchItem{{ID: itemID, Key: "item", Input: "make file"}}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "worker-batch-artifact", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim batch artifact task: claimed=%t task=%s err=%v", found, claimed.ID, err)
	}
	stepID := mustID(t, domain.NewStepID)
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: "worker-batch-artifact", LeaseVersion: claimed.Lease.Version,
		Status: task.StatusRunning, CurrentStep: stepID,
		Steps: []task.Step{{ID: stepID, TaskID: created.Task.ID, Sequence: 1, Kind: "verify", Status: task.StepSucceeded, Title: "Verify item"}},
		Event: task.Event{Type: "task.step", Payload: []byte(`{"step_id":"` + stepID + `"}`)},
	}); err != nil {
		t.Fatal(err)
	}
	details, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || len(details.BatchItems) != 1 {
		t.Fatalf("read batch item: %#v err=%v", details.BatchItems, err)
	}
	item := details.BatchItems[0]
	item.Status = task.BatchItemRunning
	item, err = store.UpdateBatchItem(ctx, owner.ID, item, "worker-batch-artifact", claimed.Lease.Version)
	if err != nil {
		t.Fatal(err)
	}
	item.Status = task.BatchItemFailed
	item.Error = "failed after file write"
	if _, err := store.UpdateBatchItem(ctx, owner.ID, item, "worker-batch-artifact", claimed.Lease.Version); err != nil {
		t.Fatal(err)
	}
	artifact := addArtifactProof(task.Artifact{
		ID: mustID(t, domain.NewArtifactID), TaskID: created.Task.ID, StepID: stepID,
		BatchItemID: item.ID, BatchAttempt: item.Attempt, Label: "Item", Path: "output/item.txt", MediaType: "text/plain",
	}, proofs["output/item.txt"])
	if _, err := store.RecordValidatedArtifact(ctx, owner.ID, artifact, "worker-batch-artifact", claimed.Lease.Version); err != nil {
		t.Fatalf("persist failed attempt artifact evidence: %v", err)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: "worker-batch-artifact", LeaseVersion: claimed.Lease.Version,
		Status: task.StatusCompleted, Result: "completed", Event: task.Event{Type: "task.result", Payload: []byte(`{"result":"completed"}`)},
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("complete with failed batch artifact error = %v, want invalid_state", err)
	}
	details, err = store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusRunning {
		t.Fatalf("failed batch artifact completed Task: status=%q err=%v", details.Task.Status, err)
	}
}

func TestPostgresRetryFailedBatchItemPersistsAttemptAndIsIdempotent(t *testing.T) {
	fixture := newFailedBatchRetryFixture(t)
	before, err := fixture.store.GetTask(fixture.ctx, fixture.taskID, fixture.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := fixture.store.ListTaskEvents(fixture.ctx, fixture.ownerID, fixture.taskID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	key := testIdempotency(fixture.ownerID, "retry-batch-item", "retry-item-once")
	if err := fixture.store.RetryFailedBatchItem(fixture.ctx, key, fixture.taskID, fixture.itemID); err != nil {
		t.Fatalf("retry failed batch item: %v", err)
	}
	after, err := fixture.store.GetTask(fixture.ctx, fixture.taskID, fixture.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	item := batchItemByID(t, after.BatchItems, fixture.itemID)
	if after.Task.Status != task.StatusQueued || after.Task.Lease.Owner != "" || after.Task.Lease.ExpiresAt != nil ||
		after.Task.Lease.Version != before.Task.Lease.Version+1 || after.Task.CurrentStep != "" || after.Task.Result != "" ||
		after.Task.WaitingReason != "" || after.Task.FinishedAt != nil || after.Task.Usage.ActiveSeconds != 17 ||
		after.Task.Usage.ToolCalls != 3 || after.Task.Usage.ModelTokens == nil || *after.Task.Usage.ModelTokens != 29 {
		t.Fatalf("task retry state = %#v; want queued with cleared result/lease and retained usage", after.Task)
	}
	if item.Status != task.BatchItemQueued || item.Attempt != 2 || item.Result != "" || item.Error != "" {
		t.Fatalf("retried batch item = %#v; want clean queued attempt 2", item)
	}
	afterEvents, err := fixture.store.ListTaskEvents(fixture.ctx, fixture.ownerID, fixture.taskID, 0, 100)
	if err != nil || len(afterEvents) != len(beforeEvents)+2 {
		t.Fatalf("retry events = %d (err=%v), before=%d; want exactly two appended events", len(afterEvents), err, len(beforeEvents))
	}
	statusEvent, itemEvent := afterEvents[len(afterEvents)-2], afterEvents[len(afterEvents)-1]
	if statusEvent.Sequence+1 != itemEvent.Sequence || statusEvent.Type != "task.status" || itemEvent.Type != "task.batch_item" {
		t.Fatalf("retry event order = %#v then %#v", statusEvent, itemEvent)
	}
	var retryPayload map[string]any
	if err := json.Unmarshal(itemEvent.Payload, &retryPayload); err != nil || retryPayload["batch_item_id"] != fixture.itemID || retryPayload["status"] != string(task.BatchItemQueued) || retryPayload["attempt"] != float64(2) {
		t.Fatalf("retry item event payload = %#v, err=%v", retryPayload, err)
	}
	if err := fixture.store.RetryFailedBatchItem(fixture.ctx, key, fixture.taskID, fixture.itemID); err != nil {
		t.Fatalf("replay failed batch item retry: %v", err)
	}
	replayed, err := fixture.store.GetTask(fixture.ctx, fixture.taskID, fixture.ownerID)
	if err != nil || batchItemByID(t, replayed.BatchItems, fixture.itemID).Attempt != 2 {
		t.Fatalf("same-key retry replay changed attempt: details=%#v err=%v", replayed, err)
	}
	replayEvents, err := fixture.store.ListTaskEvents(fixture.ctx, fixture.ownerID, fixture.taskID, 0, 100)
	if err != nil || len(replayEvents) != len(afterEvents) {
		t.Fatalf("same-key retry replay appended events: count=%d err=%v want=%d", len(replayEvents), err, len(afterEvents))
	}
	differentBody := key
	differentBody.RequestHash = hashBytes("different retry body")
	if err := fixture.store.RetryFailedBatchItem(fixture.ctx, differentBody, fixture.taskID, fixture.itemID); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("same key with changed request error = %v", err)
	}
}

func TestPostgresRetryFailedBatchItemRejectsUnknownOperation(t *testing.T) {
	fixture := newFailedBatchRetryFixture(t)
	operationID := mustID(t, domain.NewOperationID)
	if _, err := fixture.store.pool.Exec(fixture.ctx, `INSERT INTO task_operations
(id, task_id, operation_key, request_hash, host_id, runtime_generation, runtime_instance_id, control_version, lease_version, runtime_lease_version, status)
VALUES ($1,$2,'unknown-operation',$3,$4,0,'ri_test',0,$5,$5,'unknown')`, operationID, fixture.taskID,
		hashBytes("unknown operation"), fixture.hostID, fixture.leaseVersion); err != nil {
		t.Fatalf("seed unknown operation: %v", err)
	}
	key := testIdempotency(fixture.ownerID, "retry-batch-item", "retry-with-unknown-operation")
	if err := fixture.store.RetryFailedBatchItem(fixture.ctx, key, fixture.taskID, fixture.itemID); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("retry with an unknown operation error = %v", err)
	}
	var reservations int
	if err := fixture.store.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM idempotency_keys WHERE owner_id=$1 AND operation=$2 AND key=$3`, key.OwnerID, key.Operation, key.Key).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatalf("failed retry left an idempotency reservation: count=%d err=%v", reservations, err)
	}
	details, err := fixture.store.GetTask(fixture.ctx, fixture.taskID, fixture.ownerID)
	if err != nil || batchItemByID(t, details.BatchItems, fixture.itemID).Attempt != 1 || details.Task.Status != task.StatusFailed {
		t.Fatalf("unknown operation changed batch state: details=%#v err=%v", details, err)
	}
}

func TestPostgresRetryFailedBatchItemConcurrentDifferentKeys(t *testing.T) {
	fixture := newFailedBatchRetryFixture(t)
	keys := []app.IdempotencyRecord{
		testIdempotency(fixture.ownerID, "retry-batch-item", "retry-concurrent-a"),
		testIdempotency(fixture.ownerID, "retry-batch-item", "retry-concurrent-b"),
	}
	results := make(chan error, len(keys))
	var ready sync.WaitGroup
	ready.Add(len(keys))
	start := make(chan struct{})
	for _, key := range keys {
		key := key
		go func() {
			ready.Done()
			<-start
			results <- fixture.store.RetryFailedBatchItem(fixture.ctx, key, fixture.taskID, fixture.itemID)
		}()
	}
	ready.Wait()
	close(start)
	var successes, conflicts int
	for range keys {
		err := <-results
		if err == nil {
			successes++
		} else if domain.CodeOf(err) == domain.CodeInvalidState {
			conflicts++
		} else {
			t.Fatalf("concurrent retry returned unexpected error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent retries: successes=%d state conflicts=%d; want exactly one of each", successes, conflicts)
	}
	details, err := fixture.store.GetTask(fixture.ctx, fixture.taskID, fixture.ownerID)
	if err != nil || details.Task.Status != task.StatusQueued || batchItemByID(t, details.BatchItems, fixture.itemID).Attempt != 2 {
		t.Fatalf("concurrent retry final state = %#v err=%v", details, err)
	}
}

type failedBatchRetryFixture struct {
	store        *Store
	ctx          context.Context
	ownerID      string
	hostID       string
	taskID       string
	itemID       string
	leaseVersion int64
}

func newFailedBatchRetryFixture(t *testing.T) failedBatchRetryFixture {
	t.Helper()
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "failed-batch", "process the batch")
	failedID := mustID(t, domain.NewBatchItemID)
	succeededID := mustID(t, domain.NewBatchItemID)
	submission.BatchItems = []task.BatchItem{
		{ID: failedID, Key: "failed", Input: "retry this item"},
		{ID: succeededID, Key: "succeeded", Input: "keep this result"},
	}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("create retryable batch task: %v", err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, "retry-worker", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim retryable task: claimed=%t err=%v", ok, err)
	}
	details, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || len(details.BatchItems) != 2 {
		t.Fatalf("load retryable batch items: %#v err=%v", details.BatchItems, err)
	}
	failed := batchItemByID(t, details.BatchItems, failedID)
	failed.Status = task.BatchItemRunning
	failed, err = store.UpdateBatchItem(ctx, owner.ID, failed, "retry-worker", claimed.Lease.Version)
	if err != nil {
		t.Fatalf("mark retryable item running: %v", err)
	}
	failed.Status = task.BatchItemFailed
	failed.Error = "temporary failure"
	if _, err := store.UpdateBatchItem(ctx, owner.ID, failed, "retry-worker", claimed.Lease.Version); err != nil {
		t.Fatalf("mark retryable item failed: %v", err)
	}
	succeeded := batchItemByID(t, details.BatchItems, succeededID)
	succeeded.Status = task.BatchItemSucceeded
	succeeded.Result = "preserved success"
	if _, err := store.UpdateBatchItem(ctx, owner.ID, succeeded, "retry-worker", claimed.Lease.Version); err != nil {
		t.Fatalf("persist completed peer item: %v", err)
	}
	stepID := mustID(t, domain.NewStepID)
	tokens := int64(29)
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET usage_model_tokens=$2 WHERE id=$1`, created.Task.ID, tokens); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: "retry-worker", LeaseVersion: claimed.Lease.Version,
		Status: task.StatusFailed, CurrentStep: stepID, Result: "prior task result", WaitingReason: "prior wait",
		Usage: task.Usage{ActiveSeconds: 17, ToolCalls: 3, ModelTokens: &tokens},
		Steps: []task.Step{{ID: stepID, TaskID: created.Task.ID, Sequence: 1, Kind: "batch_result", Status: task.StepFailed, Title: "Batch failed"}},
		Event: task.Event{Type: "task.result", Payload: []byte(`{"status":"failed"}`)},
	}); err != nil {
		t.Fatalf("mark retryable task failed: %v", err)
	}
	return failedBatchRetryFixture{
		store: store, ctx: ctx, ownerID: owner.ID, hostID: hostValue.ID,
		taskID: created.Task.ID, itemID: failedID, leaseVersion: claimed.Lease.Version,
	}
}

func batchItemByID(t *testing.T, items []task.BatchItem, id string) task.BatchItem {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("batch item %q not found in %#v", id, items)
	return task.BatchItem{}
}

func newTestTaskSubmission(t *testing.T, ownerID, hostID, idemKey, objective string) app.TaskSubmission {
	t.Helper()
	taskID := mustID(t, domain.NewTaskID)
	messageID := mustID(t, domain.NewMessageID)
	authorizationExpiresAt := time.Now().UTC().AddDate(1, 0, 0)
	return app.TaskSubmission{
		Task: task.Task{
			ID: taskID, OwnerID: ownerID, HostID: hostID, ModelID: "model-test", Objective: objective,
			Status: task.StatusQueued, Authorization: task.Authorization{
				Kind: "session", TokenID: testSessionTokenID(ownerID), Version: 1, ExpiresAt: &authorizationExpiresAt,
			},
			Budget: task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000},
		},
		Message:      conversation.Message{ID: messageID, OwnerID: ownerID, Role: conversation.RoleUser, Content: objective},
		InitialEvent: task.Event{Type: "task.status", Payload: []byte(`{"status":"queued"}`)},
		Idempotency:  testIdempotency(ownerID, "submit-task", idemKey),
	}
}

func testIdempotency(ownerID, operation, key string) app.IdempotencyRecord {
	return app.IdempotencyRecord{OwnerID: ownerID, Operation: operation, Key: key, RequestHash: hashBytes(operation + ":" + key)}
}

func seedTaskArtifactManifest(t *testing.T, store *Store, workspaceID string, revision int64, files map[string][]byte) map[string]task.Artifact {
	t.Helper()
	proofs := make(map[string]task.Artifact, len(files))
	var totalBytes int64
	for filePath, content := range files {
		digestBytes := sha256.Sum256(content)
		digest := hex.EncodeToString(digestBytes[:])
		chunks, err := json.Marshal([]workspace.Chunk{{SHA256: digest, SizeBytes: int64(len(content))}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(context.Background(), `INSERT INTO workspace_revision_files
(workspace_id, valid_from_revision, file_path, folded_path, size_bytes, sha256, mode, chunks)
VALUES ($1,$2,$3,lower($3),$4,$5,420,$6::jsonb)`, workspaceID, revision, filePath, len(content), digest, chunks); err != nil {
			t.Fatalf("insert artifact test manifest file: %v", err)
		}
		proofs[filePath] = task.Artifact{WorkspaceID: workspaceID, Revision: revision, SizeBytes: int64(len(content)), SHA256: digest}
		totalBytes += int64(len(content))
	}
	if _, err := store.pool.Exec(context.Background(), `UPDATE workspace_revisions SET file_count=$3,total_bytes=$4 WHERE workspace_id=$1 AND revision=$2`,
		workspaceID, revision, len(files), totalBytes); err != nil {
		t.Fatalf("update artifact test manifest revision: %v", err)
	}
	return proofs
}

func addArtifactProof(artifact, proof task.Artifact) task.Artifact {
	artifact.WorkspaceID = proof.WorkspaceID
	artifact.Revision = proof.Revision
	artifact.SizeBytes = proof.SizeBytes
	artifact.SHA256 = proof.SHA256
	return artifact
}

func hashBytes(value string) []byte {
	hash := sha256.Sum256([]byte(value))
	return hash[:]
}

func mustID(t *testing.T, generate func() (string, error)) string {
	t.Helper()
	value, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
