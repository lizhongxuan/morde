package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"
)

func TestTaskHubWorkspaceSyncRequiresOneExactUploadFile(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "hub-file-put-upload-scope")
	first := syncTestFile("reports/result.txt", []byte("result"))
	second := syncTestFile("reports/nested/extra.txt", []byte("extra"))

	insertSession := func(files []workspace.File, scope []string) string {
		t.Helper()
		session := newTestSyncSession(t, workspaceValue, owner.ID, files, files, nil)
		session.ReplicaID = "device:t1234567"
		session.Direction, session.Policy, session.Scope = workspace.SyncUpload, workspace.SyncCompareMerge, scope
		params, err := workspaceSyncSessionParams(session)
		if err != nil {
			t.Fatalf("build Workspace sync session: %v", err)
		}
		if _, err := sqlcgen.New(store.pool).CreateWorkspaceSyncSession(ctx, params); err != nil {
			t.Fatalf("insert Workspace sync session: %v", err)
		}
		return session.ID
	}

	singleSessionID := insertSession([]workspace.File{first}, []string{first.Path})
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	matches, err := taskHubWorkspaceSyncHasExactUpload(ctx, tx, workspaceValue.ID, singleSessionID, first.Path, first.SizeBytes, first.SHA256)
	if err != nil || !matches {
		t.Fatalf("exact Task file upload manifest = %t, err=%v; want match", matches, err)
	}

	multipleSessionID := insertSession([]workspace.File{first, second}, []string{"reports"})
	matches, err = taskHubWorkspaceSyncHasExactUpload(ctx, tx, workspaceValue.ID, multipleSessionID, first.Path, first.SizeBytes, first.SHA256)
	if err != nil || matches {
		t.Fatalf("recursive multi-file Task upload manifest = %t, err=%v; want no match", matches, err)
	}
}

func TestTaskHubWorkspaceSyncAdvancesTaskCheckpointWithCAS(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "hub-file-put-checkpoint")
	file := syncTestFile("result.txt", []byte("result"))
	session := newTestSyncSession(t, workspaceValue, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks(session.UploadFiles),
		syncTestIdempotency(owner.ID, "workspace.sync.create", "hub-file-put-checkpoint:create", session.ID)); err != nil {
		t.Fatalf("create Workspace sync session: %v", err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, workspaceValue.ID, session.ID,
		syncTestIdempotency(owner.ID, "workspace.sync.commit", "hub-file-put-checkpoint:commit", session.ID)); err != nil {
		t.Fatalf("commit Workspace sync session: %v", err)
	}

	submission := newTestTaskSubmission(t, owner.ID, "", "hub-file-put-checkpoint", "write a Workspace file")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Task: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET workspace_id=$2, input_revision=0, workspace_checkpoint_revision=0
WHERE id=$1 AND owner_id=$3`, created.Task.ID, workspaceValue.ID, owner.ID); err != nil {
		t.Fatalf("attach Task to source Workspace checkpoint: %v", err)
	}

	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := advanceTaskHubWorkspaceCheckpointTx(ctx, tx, owner.ID, created.Task.ID, workspaceValue.ID, 0, 1); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("advance Task Workspace checkpoint: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit Task Workspace checkpoint: %v", err)
	}
	var checkpoint int64
	if err := store.pool.QueryRow(ctx, `SELECT workspace_checkpoint_revision FROM tasks WHERE id=$1 AND owner_id=$2`,
		created.Task.ID, owner.ID).Scan(&checkpoint); err != nil || checkpoint != 1 {
		t.Fatalf("Task checkpoint = %d, err=%v; want 1", checkpoint, err)
	}

	tx, err = store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := advanceTaskHubWorkspaceCheckpointTx(ctx, tx, owner.ID, created.Task.ID, workspaceValue.ID, 0, 1); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("advance from stale source checkpoint = %v; want Workspace conflict", err)
	}
}

func TestPostgresHubTaskOperationIsIdempotentAndTerminal(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	targetID := mustID(t, domain.NewClientInstallationID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'Task target','desktop',clock_timestamp()+interval '1 day')`, targetID, owner.ID); err != nil {
		t.Fatal(err)
	}
	submission := newTestTaskSubmission(t, owner.ID, "", "hub-task-operation", "inspect target")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "hub-task-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim task: claimed=%t task=%+v err=%v", found, claimed, err)
	}
	operation := app.TaskHubOperation{
		OwnerID: owner.ID, TaskID: claimed.ID, Key: "agent:hub-test-operation",
		ToolName: "morde_hub_files_" + targetID + "_v1", TargetInstallationID: targetID, Action: hub.ActionFiles,
		TaskAuthorizationVersion: claimed.Authorization.Version, TargetGrantVersion: 1,
		LeaseVersion: claimed.Lease.Version, RequestHash: hashBytes("bounded files request"), Status: task.OperationPrepared,
	}

	var wait sync.WaitGroup
	preparedResults := make(chan app.TaskHubOperation, 2)
	replayedResults := make(chan bool, 2)
	errs := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			prepared, replayed, err := store.PrepareTaskHubOperation(ctx, operation, claimed.Lease.Owner)
			preparedResults <- prepared
			replayedResults <- replayed
			errs <- err
		}()
	}
	wait.Wait()
	close(preparedResults)
	close(replayedResults)
	close(errs)
	var prepared app.TaskHubOperation
	newRows, replayedRows := 0, 0
	for value := range preparedResults {
		prepared = value
	}
	for replayed := range replayedResults {
		if replayed {
			replayedRows++
		} else {
			newRows++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent prepare: %v", err)
		}
	}
	if newRows != 1 || replayedRows != 1 || prepared.Status != task.OperationPrepared {
		t.Fatalf("concurrent prepares created %d rows and replayed %d; last operation=%+v", newRows, replayedRows, prepared)
	}
	if _, _, err := store.PrepareTaskHubOperation(ctx, operation, claimed.Lease.Owner); err != nil {
		t.Fatalf("idempotent prepare: %v", err)
	}
	changed := operation
	changed.RequestHash = hashBytes("changed request")
	if _, _, err := store.PrepareTaskHubOperation(ctx, changed, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("same operation key with changed request hash = %v", err)
	}

	if err := store.MarkTaskHubOperationDispatched(ctx, prepared, claimed.Lease.Owner); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	if err := store.MarkTaskHubOperationDispatched(ctx, prepared, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("duplicate dispatch = %v", err)
	}
	resultHash := hashBytes("metadata only; body is not persisted")
	if err := store.CompleteTaskHubOperation(ctx, prepared, claimed.Lease.Owner, task.OperationSucceeded, resultHash); err != nil {
		t.Fatalf("complete operation: %v", err)
	}
	if err := store.CompleteTaskHubOperation(ctx, prepared, claimed.Lease.Owner, task.OperationSucceeded, resultHash); err != nil {
		t.Fatalf("idempotent completion: %v", err)
	}
	if err := store.CompleteTaskHubOperation(ctx, prepared, claimed.Lease.Owner, task.OperationSucceeded, hashBytes("different result")); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("changed completion hash = %v", err)
	}
	stored, err := store.GetTaskHubOperation(ctx, owner.ID, claimed.ID, operation.Key)
	if err != nil || stored.Status != task.OperationSucceeded || string(stored.ResultHash) != string(resultHash) {
		t.Fatalf("stored successful operation = %+v err=%v", stored, err)
	}

	preparedFailure := operation
	preparedFailure.Key = "agent:hub-prepared-failure"
	preparedFailure.RequestHash = hashBytes("request rejected before dispatch")
	preparedFailure, _, err = store.PrepareTaskHubOperation(ctx, preparedFailure, claimed.Lease.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteTaskHubOperation(ctx, preparedFailure, claimed.Lease.Owner, task.OperationSucceeded, resultHash); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("prepared operation reported success without dispatch = %v", err)
	}
	failureHash := hashBytes("request was rejected before dispatch")
	if err := store.CompleteTaskHubOperation(ctx, preparedFailure, claimed.Lease.Owner, task.OperationFailed, failureHash); err != nil {
		t.Fatalf("record failure before dispatch: %v", err)
	}
	if err := store.CompleteTaskHubOperation(ctx, preparedFailure, claimed.Lease.Owner, task.OperationFailed, failureHash); err != nil {
		t.Fatalf("idempotent pre-dispatch failure: %v", err)
	}
	if err := store.CompleteTaskHubOperation(ctx, preparedFailure, claimed.Lease.Owner, task.OperationFailed, resultHash); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("changed pre-dispatch failure hash = %v", err)
	}
	storedFailure, err := store.GetTaskHubOperation(ctx, owner.ID, claimed.ID, preparedFailure.Key)
	if err != nil || storedFailure.Status != task.OperationFailed || string(storedFailure.ResultHash) != string(failureHash) {
		t.Fatalf("stored pre-dispatch failure = %+v err=%v", storedFailure, err)
	}

	unknown := operation
	unknown.Key = "agent:hub-unknown-operation"
	unknown.RequestHash = hashBytes("unknown request")
	unknownPrepared, _, err := store.PrepareTaskHubOperation(ctx, unknown, claimed.Lease.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTaskHubOperationDispatched(ctx, unknownPrepared, claimed.Lease.Owner); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTaskHubOperationUnknown(ctx, unknownPrepared, claimed.Lease.Owner); err != nil {
		t.Fatalf("mark unknown: %v", err)
	}
	if err := store.MarkTaskHubOperationUnknown(ctx, unknownPrepared, claimed.Lease.Owner); err != nil {
		t.Fatalf("repeat unknown settlement: %v", err)
	}
	unknownReplay, replayed, err := store.PrepareTaskHubOperation(ctx, unknown, claimed.Lease.Owner)
	if err != nil || !replayed || unknownReplay.Status != task.OperationUnknown {
		t.Fatalf("unknown operation replay = %+v replayed=%t err=%v", unknownReplay, replayed, err)
	}
}

func TestPostgresTaskHubFilePutLocalReceiptIsDurableAndIdempotent(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "task-hub-local-receipt")
	targetID := mustID(t, domain.NewClientInstallationID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'Task target','desktop',clock_timestamp()+interval '1 day')`, targetID, owner.ID); err != nil {
		t.Fatalf("insert target installation: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `ALTER TABLE task_hub_operations DROP CONSTRAINT IF EXISTS task_hub_operations_action_check`); err != nil {
		t.Fatalf("allow file.put only in the isolated receipt test database: %v", err)
	}
	submission := newTestTaskSubmission(t, owner.ID, "", "task-hub-local-receipt", "write one Workspace file")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Task: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET workspace_id=$2, input_revision=$3, workspace_checkpoint_revision=$3
WHERE id=$1 AND owner_id=$4`, created.Task.ID, workspaceValue.ID, workspaceValue.Revision, owner.ID); err != nil {
		t.Fatalf("attach Task to its Workspace checkpoint: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "hub-file-put-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Task: claimed=%t task=%+v err=%v", found, claimed, err)
	}
	operation := app.TaskHubOperation{
		OwnerID: owner.ID, TaskID: claimed.ID, Key: "agent:hub-file-put-receipt",
		ToolName: "morde_hub_file_put_" + targetID + "_v1", TargetInstallationID: targetID, Action: hub.ActionFilePut,
		TaskAuthorizationVersion: claimed.Authorization.Version, TargetGrantVersion: 1,
		LeaseVersion: claimed.Lease.Version, RequestHash: hashBytes("Task file.put request"),
		Status: task.OperationPrepared, WorkspaceID: workspaceValue.ID, WorkspacePath: "reports/result.txt",
		WorkspaceSizeBytes: 6, WorkspaceSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		SourceCheckpointRevision: workspaceValue.Revision, SyncStage: app.TaskHubWriteStagePending,
	}
	prepared, replayed, err := store.PrepareTaskHubOperation(ctx, operation, claimed.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare isolated file.put operation: operation=%+v replayed=%t err=%v", prepared, replayed, err)
	}
	if err := store.MarkTaskHubOperationDispatched(ctx, prepared, claimed.Lease.Owner); err != nil {
		t.Fatalf("mark file.put dispatched: %v", err)
	}
	receiptHash := hashBytes("canonical local receipt")
	if err := store.RecordTaskHubOperationLocalReceipt(ctx, prepared, claimed.Lease.Owner, receiptHash); err != nil {
		t.Fatalf("record local receipt: %v", err)
	}
	stored, err := store.GetTaskHubOperation(ctx, owner.ID, claimed.ID, operation.Key)
	if err != nil || stored.Status != task.OperationDispatched || stored.SyncStage != app.TaskHubWriteStageLocal || len(stored.LocalReceiptHash) != 32 {
		t.Fatalf("stored local receipt = %+v err=%v", stored, err)
	}
	if err := store.RecordTaskHubOperationLocalReceipt(ctx, prepared, claimed.Lease.Owner, receiptHash); err != nil {
		t.Fatalf("idempotent local receipt: %v", err)
	}
	if err := store.RecordTaskHubOperationLocalReceipt(ctx, prepared, claimed.Lease.Owner, hashBytes("changed local receipt")); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("changed local receipt = %v; want conflict", err)
	}
}

func TestPostgresHubTaskOperationRechecksTargetBeforeDispatch(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	targetID := mustID(t, domain.NewClientInstallationID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO client_installations (id,owner_id,display_name,platform,expires_at)
VALUES ($1,$2,'Task target','desktop',clock_timestamp()+interval '1 day')`, targetID, owner.ID); err != nil {
		t.Fatal(err)
	}
	submission := newTestTaskSubmission(t, owner.ID, "", "hub-target-revoke", "inspect target")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "hub-target-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim task: claimed=%t task=%+v err=%v", found, claimed, err)
	}
	operation := app.TaskHubOperation{
		OwnerID: owner.ID, TaskID: claimed.ID, Key: "agent:hub-target-revoke", ToolName: "morde_hub_screenshot_" + targetID + "_v1",
		TargetInstallationID: targetID, Action: hub.ActionScreenshot, TaskAuthorizationVersion: claimed.Authorization.Version,
		TargetGrantVersion: 1, LeaseVersion: claimed.Lease.Version, RequestHash: hashBytes("screenshot request"), Status: task.OperationPrepared,
	}
	prepared, _, err := store.PrepareTaskHubOperation(ctx, operation, claimed.Lease.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE client_installations SET revoked_at=clock_timestamp() WHERE id=$1`, targetID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkTaskHubOperationDispatched(ctx, prepared, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("dispatch after target revocation = %v", err)
	}
	if err := store.CompleteTaskHubOperation(ctx, prepared, claimed.Lease.Owner, task.OperationFailed, hashBytes("target revoked")); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("pre-dispatch failure after target revocation = %v", err)
	}
	stored, err := store.GetTaskHubOperation(ctx, owner.ID, claimed.ID, operation.Key)
	if err != nil || stored.Status != task.OperationPrepared {
		t.Fatalf("revoked target crossed dispatch fence: operation=%+v err=%v", stored, err)
	}
}
