package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func TestPostgresWorkspaceRestoreCreatesNewRevisionAndReplaysIdempotently(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "restore")
	file := syncTestFile("README.md", []byte("latest"))
	seedWorkspaceRestoreRevision(t, ctx, store, value, file)

	requestHash := sha256.Sum256([]byte("restore-v1:" + value.ID + ":README.md"))
	record := app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "workspace.restore", Key: "restore:" + value.ID,
		RequestHash: requestHash[:], ResourceID: value.ID,
	}
	result, err := store.RestoreWorkspaceRevision(ctx, owner.ID, value.ID, 0, 1, []string{"README.md"}, record)
	if err != nil || result.Replayed || result.Revision != 2 {
		t.Fatalf("Workspace restore = %#v, err=%v", result, err)
	}
	current, err := store.GetWorkspace(ctx, value.ID)
	if err != nil || current.Revision != 2 || current.UsedBytes != 0 {
		t.Fatalf("Workspace after restore = %#v, err=%v", current, err)
	}
	page, err := store.GetWorkspaceManifestPage(ctx, value.ID, 2, "", "", 10)
	if err != nil || len(page.Files) != 0 {
		t.Fatalf("restored revision manifest = %#v, err=%v", page, err)
	}
	changes, err := store.GetWorkspaceChangePage(ctx, value.ID, 1, file.Path, 10)
	if err != nil || len(changes.Items) != 1 || changes.Items[0].Revision != 2 || changes.Items[0].Path != file.Path || changes.Items[0].Action != workspace.ChangeDelete {
		t.Fatalf("restore change feed = %#v, err=%v", changes, err)
	}
	var desired, applied int64
	if err := store.pool.QueryRow(ctx, `SELECT desired_revision,applied_revision FROM workspace_current_materializations WHERE workspace_id=$1`, value.ID).
		Scan(&desired, &applied); err != nil || desired != 2 || applied != 1 {
		t.Fatalf("current materialization after restore = %d/%d, err=%v", desired, applied, err)
	}
	replayed, err := store.RestoreWorkspaceRevision(ctx, owner.ID, value.ID, 0, 1, []string{"README.md"}, record)
	if err != nil || !replayed.Replayed || replayed.Revision != result.Revision {
		t.Fatalf("replayed Workspace restore = %#v, err=%v", replayed, err)
	}
	otherHash := sha256.Sum256([]byte("restore-v1:" + value.ID + ":other.txt"))
	otherRequest := record
	otherRequest.RequestHash = otherHash[:]
	if _, err := store.RestoreWorkspaceRevision(ctx, owner.ID, value.ID, 0, 1, []string{"other.txt"}, otherRequest); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("reused restore key error = %v, want idempotency conflict", err)
	}
}

func TestPostgresWorkspaceRestoreRestoresUnknownDirectoryMetadata(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "restore directory metadata")
	file := syncTestFile("reports/README.md", []byte("workspace history"))
	seedWorkspaceRestoreRevision(t, ctx, store, value, file)
	chunks, err := json.Marshal(file.Chunks)
	if err != nil {
		t.Fatal(err)
	}
	fileJSON, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	mode, modified := int32(0o750), int64(1_700_000_000_123_000_000)
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revisions(workspace_id,revision,file_count,total_bytes,directory_count)
VALUES ($1,2,1,$2,0)`, value.ID, file.SizeBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_revision_files SET valid_to_revision=2
WHERE workspace_id=$1 AND file_path=$2 AND valid_to_revision IS NULL`, value.ID, file.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revision_files
(workspace_id,valid_from_revision,file_path,folded_path,size_bytes,sha256,mode,chunks)
VALUES ($1,2,$2,lower($2),$3,$4,$5,$6)`, value.ID, file.Path, file.SizeBytes, file.SHA256, int32(file.Mode), chunks); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_changes(workspace_id,revision,file_path,action,file)
	VALUES ($1,2,$2,'upsert',$3)`, value.ID, file.Path, fileJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_changes(workspace_id,revision,file_path,action,file)
VALUES ($1,2,'reports','directory_upsert',NULL)`, value.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revision_directories
(workspace_id,valid_from_revision,directory_path,folded_path,mode,mod_time_ns) VALUES ($1,2,'reports','reports',$2,$3)`, value.ID, mode, modified); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET revision=2 WHERE id=$1`, value.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_current_materializations SET desired_revision=2,applied_revision=2 WHERE workspace_id=$1`, value.ID); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("restore old directory metadata:" + value.ID))
	result, err := store.RestoreWorkspaceRevision(ctx, owner.ID, value.ID, 1, 2, []string{"reports"}, app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "workspace.restore", Key: "restore-directory-metadata:" + value.ID,
		RequestHash: hash[:], ResourceID: value.ID,
	})
	if err != nil || result.Revision != 3 {
		t.Fatalf("Workspace restore to unknown directory metadata = %#v, err=%v", result, err)
	}
	page, err := store.GetWorkspaceManifestPage(ctx, value.ID, 3, "", "", 10)
	if err != nil || len(page.Directories) != 2 || !workspace.SameDirectoryMetadata(page.Directories[0], workspace.Directory{Path: "."}) ||
		!workspace.SameDirectoryMetadata(page.Directories[1], workspace.Directory{Path: "reports"}) {
		t.Fatalf("restored directory metadata = %#v, err=%v", page.Directories, err)
	}
}

func TestPostgresWorkspaceRestoreFailsClosedForActiveHostAndUnconfirmedCurrentRevision(t *testing.T) {
	for _, tc := range []struct {
		name        string
		activeHost  bool
		activeSync  bool
		unconfirmed bool
	}{
		{name: "active binding", activeHost: true},
		{name: "in-flight sync", activeSync: true},
		{name: "unconfirmed materialization", unconfirmed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := newTemporaryStore(t)
			ctx := context.Background()
			owner := createTestUser(t, store, 3, 10_000, 20_000)
			value := createTestWorkspace(t, ctx, store, owner.ID, "restore fence")
			file := syncTestFile("README.md", []byte("latest"))
			seedWorkspaceRestoreRevision(t, ctx, store, value, file)
			if tc.activeSync {
				value.Revision = 1
				session := newTestSyncSession(t, value, owner.ID, nil, nil, nil)
				if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, nil,
					syncTestIdempotency(owner.ID, "workspace.sync.create", "restore-fence:"+value.ID, session.ID)); err != nil {
					t.Fatalf("create in-flight sync session: %v", err)
				}
			}
			if tc.activeHost {
				createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{value.ID})
			}
			if tc.unconfirmed {
				if _, err := store.pool.Exec(ctx, `UPDATE workspace_current_materializations SET applied_revision=-1 WHERE workspace_id=$1`, value.ID); err != nil {
					t.Fatal(err)
				}
			}
			hash := sha256.Sum256([]byte("blocked restore:" + value.ID))
			_, err := store.RestoreWorkspaceRevision(ctx, owner.ID, value.ID, 0, 1, []string{"README.md"}, app.IdempotencyRecord{
				OwnerID: owner.ID, Operation: "workspace.restore", Key: "blocked:" + value.ID, RequestHash: hash[:], ResourceID: value.ID,
			})
			if domain.CodeOf(err) != domain.CodeWorkspaceBusy {
				t.Fatalf("restore error = %v, want Workspace busy", err)
			}
			current, getErr := store.GetWorkspace(ctx, value.ID)
			if getErr != nil || current.Revision != 1 || current.UsedBytes != file.SizeBytes {
				t.Fatalf("Workspace changed after blocked restore = %#v, err=%v", current, getErr)
			}
		})
	}
}

func TestPostgresWorkspaceRestoreRejectsStaleExpectedRevision(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "stale restore")
	file := syncTestFile("README.md", []byte("latest"))
	seedWorkspaceRestoreRevision(t, ctx, store, value, file)
	hash := sha256.Sum256([]byte("stale restore:" + value.ID))
	_, err := store.RestoreWorkspaceRevision(ctx, owner.ID, value.ID, 0, 0, []string{"README.md"}, app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "workspace.restore", Key: "stale:" + value.ID, RequestHash: hash[:], ResourceID: value.ID,
	})
	if domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("restore with stale current revision = %v, want Workspace conflict", err)
	}
	current, getErr := store.GetWorkspace(ctx, value.ID)
	if getErr != nil || current.Revision != 1 || current.UsedBytes != file.SizeBytes {
		t.Fatalf("Workspace changed after stale restore = %#v, err=%v", current, getErr)
	}
}

func TestPostgresWorkspaceRestoreSerializesConcurrentExpectedRevision(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "concurrent restore")
	file := syncTestFile("README.md", []byte("latest"))
	seedWorkspaceRestoreRevision(t, ctx, store, value, file)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, key := range []string{"first", "second"} {
		go func(key string) {
			<-start
			hash := sha256.Sum256([]byte(key + ":" + value.ID))
			_, err := store.RestoreWorkspaceRevision(ctx, owner.ID, value.ID, 0, 1, []string{"README.md"}, app.IdempotencyRecord{
				OwnerID: owner.ID, Operation: "workspace.restore", Key: key + ":" + value.ID,
				RequestHash: hash[:], ResourceID: value.ID,
			})
			results <- err
		}(key)
	}
	close(start)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) || (first != nil && domain.CodeOf(first) != domain.CodeWorkspaceConflict) ||
		(second != nil && domain.CodeOf(second) != domain.CodeWorkspaceConflict) {
		t.Fatalf("concurrent restore results = %v, %v; want one success and one conflict", first, second)
	}
	current, err := store.GetWorkspace(ctx, value.ID)
	if err != nil || current.Revision != 2 || current.UsedBytes != 0 {
		t.Fatalf("Workspace after concurrent restore = %#v, err=%v", current, err)
	}
}

func TestPostgresWorkspaceRestoreSerializesTaskFilePut(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "restore task file put")
	previousFile := syncTestFile("README.md", []byte("previous workspace content"))
	seedWorkspaceRestoreRevision(t, ctx, store, workspaceValue, previousFile)
	workspaceValue.Revision = 1
	workspaceValue.UsedBytes = previousFile.SizeBytes

	client := syncDeviceTestPrincipal(t, store, owner.ID)
	device := clientTestToken(t, owner.ID, client.ClientInstallationID, identity.KindDevice, nil)
	if _, err := store.CreateToken(ctx, device); err != nil {
		t.Fatalf("create Workspace sync device token: %v", err)
	}
	registerTestWorkspaceSyncDevice(t, store, client, workspaceValue.ID,
		"AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA", "restore Task file.put device")
	applyWorkspaceSyncNodeState(t, store, owner.ID)
	if _, err := pool.Exec(ctx, `ALTER TABLE task_hub_operations DROP CONSTRAINT IF EXISTS task_hub_operations_action_check`); err != nil {
		t.Fatalf("allow file.put in isolated test database: %v", err)
	}

	submission := newTestTaskSubmission(t, owner.ID, "", "restore-task-file-put", "write a workspace file")
	submission.Task.ExecutionLocation = "server"
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Task: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET workspace_id=$2, input_revision=$3, workspace_checkpoint_revision=$3
WHERE id=$1 AND owner_id=$4`, created.Task.ID, workspaceValue.ID, workspaceValue.Revision, owner.ID); err != nil {
		t.Fatalf("attach Task to Workspace checkpoint: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "restore-task-file-put-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Task: task=%#v found=%t err=%v", claimed, found, err)
	}
	file := syncTestFile(previousFile.Path, []byte("Task output wins over stale restore"))
	operation := app.TaskHubOperation{
		OwnerID: owner.ID, TaskID: claimed.ID, Key: "agent:restore-task-file-put",
		ToolName:             "morde_hub_file_put_" + client.ClientInstallationID + "_v1",
		TargetInstallationID: client.ClientInstallationID, Action: hub.ActionFilePut,
		TaskAuthorizationVersion: claimed.Authorization.Version, TargetGrantVersion: 1,
		LeaseVersion: claimed.Lease.Version, RequestHash: hashBytes("restore Task file.put request"),
		Status: task.OperationPrepared, WorkspaceID: workspaceValue.ID, WorkspacePath: file.Path,
		WorkspaceSizeBytes: file.SizeBytes, WorkspaceSHA256: file.SHA256,
		SourceCheckpointRevision: workspaceValue.Revision, SyncStage: app.TaskHubWriteStagePending,
	}
	prepared, replayed, err := store.PrepareTaskHubOperation(ctx, operation, claimed.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare Task file.put: operation=%#v replayed=%t err=%v", prepared, replayed, err)
	}
	if err := store.MarkTaskHubOperationDispatched(ctx, prepared, claimed.Lease.Owner); err != nil {
		t.Fatalf("dispatch Task file.put: %v", err)
	}
	if err := store.RecordTaskHubOperationLocalReceipt(ctx, prepared, claimed.Lease.Owner, hashBytes("local Task file.put receipt")); err != nil {
		t.Fatalf("record Task file.put local receipt: %v", err)
	}

	session := newTestSyncSession(t, workspaceValue, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
	session.ReplicaID = "device:" + device.ID
	session.Policy = workspace.SyncCompareMerge
	session.Scope = []string{file.Path}
	session.TaskID, session.TaskHubOperationKey = claimed.ID, operation.Key
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks(session.UploadFiles),
		syncTestIdempotency(owner.ID, "workspace.sync.create", "restore-task-file-put:create", session.ID)); err != nil {
		t.Fatalf("create Task file.put Workspace sync session: %v", err)
	}
	commitKey := "restore-task-file-put:commit"
	commitRecord := syncTestIdempotency(owner.ID, "workspace.sync.commit", commitKey, session.ID)
	if _, err := store.ReserveWorkspaceSyncCommit(ctx, workspaceValue.ID, session.ID, commitRecord); err != nil {
		t.Fatalf("reserve Task file.put Workspace sync commit: %v", err)
	}

	restoreHash := sha256.Sum256([]byte("restore Task file.put:" + workspaceValue.ID))
	restoreRecord := app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "workspace.restore", Key: "restore-task-file-put:" + workspaceValue.ID,
		RequestHash: restoreHash[:], ResourceID: workspaceValue.ID,
	}
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if err := blocker.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceValue.ID).Scan(new(string)); err != nil {
		t.Fatalf("hold Workspace row lock: %v", err)
	}
	type restoreResult struct {
		result app.WorkspaceRestoreResult
		err    error
	}
	restoreDone := make(chan restoreResult, 1)
	go func() {
		result, err := store.RestoreWorkspaceRevision(ctx, owner.ID, workspaceValue.ID, 0, 1, []string{previousFile.Path}, restoreRecord)
		restoreDone <- restoreResult{result: result, err: err}
	}()
	waitForPostgresLockWait(t, ctx, pool, "%SELECT id FROM workspaces WHERE id=$1 FOR UPDATE%", 1)
	type syncResult struct {
		session workspace.SyncSession
		err     error
	}
	syncDone := make(chan syncResult, 1)
	go func() {
		committed, _, err := store.CommitWorkspaceSyncSession(ctx, workspaceValue.ID, session.ID, commitRecord)
		syncDone <- syncResult{session: committed, err: err}
	}()
	waitForPostgresLockWait(t, ctx, pool, "%FROM workspaces WHERE id=$1 FOR UPDATE%", 2)
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("release Workspace row lock: %v", err)
	}

	restored := <-restoreDone
	if domain.CodeOf(restored.err) != domain.CodeWorkspaceBusy || restored.result.Revision != 0 {
		t.Fatalf("restore racing Task file.put = %#v, err=%v; want busy without a revision", restored.result, restored.err)
	}
	committed := <-syncDone
	if committed.err != nil || committed.session.State != workspace.SyncCompleted || committed.session.TargetRevision != 2 {
		t.Fatalf("Task file.put Workspace sync = %#v, err=%v; want completed revision 2", committed.session, committed.err)
	}
	current, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || current.Revision != 2 {
		t.Fatalf("Workspace after Task write and blocked restore = %#v, err=%v", current, err)
	}
	page, err := store.GetWorkspaceManifestPage(ctx, workspaceValue.ID, current.Revision, "", "", 10)
	if err != nil || len(page.Files) != 1 || page.Files[0].Path != file.Path || page.Files[0].SHA256 != file.SHA256 {
		t.Fatalf("Workspace manifest after Task write = %#v, err=%v", page, err)
	}
	operationAfter, err := store.GetTaskHubOperation(ctx, owner.ID, claimed.ID, operation.Key)
	if err != nil || operationAfter.Status != task.OperationSucceeded || operationAfter.SyncStage != app.TaskHubWriteStageCloudSynced || operationAfter.SyncRevision != 2 {
		t.Fatalf("Task file.put receipt after restore race = %#v, err=%v", operationAfter, err)
	}
	taskAfter, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || taskAfter.Task.WorkspaceCheckpointRevision == nil || *taskAfter.Task.WorkspaceCheckpointRevision != 2 {
		t.Fatalf("Task checkpoint after restore race = %#v, err=%v", taskAfter.Task.WorkspaceCheckpointRevision, err)
	}
	if _, err := store.RestoreWorkspaceRevision(ctx, owner.ID, workspaceValue.ID, 0, 1, []string{previousFile.Path}, restoreRecord); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("retry restore against stale revision = %v; want Workspace conflict", err)
	}
}

func seedWorkspaceRestoreRevision(t *testing.T, ctx context.Context, store *Store, value workspace.Workspace, file workspace.File) {
	t.Helper()
	if err := workspace.ValidateFile(file); err != nil {
		t.Fatalf("restore fixture file is invalid: %v", err)
	}
	chunks := file.Chunks
	if chunks == nil {
		chunks = []workspace.Chunk{}
	}
	chunksJSON, err := json.Marshal(chunks)
	if err != nil {
		t.Fatal(err)
	}
	kind := workspace.CanonicalFileKind(file.Kind)
	var symlinkTarget any
	if kind == workspace.FileKindSymlink {
		symlinkTarget = file.SymlinkTarget
	}
	fileJSON, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revisions(workspace_id,revision,file_count,total_bytes,directory_count)
VALUES ($1,1,1,$2,0)`, value.ID, file.SizeBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revision_files
(workspace_id,valid_from_revision,file_path,folded_path,size_bytes,sha256,mode,kind,symlink_target,chunks)
VALUES ($1,1,$2,lower($2),$3,$4,$5,$6,$7,$8)`, value.ID, file.Path, file.SizeBytes, file.SHA256, int32(file.Mode), kind, symlinkTarget, chunksJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_changes(workspace_id,revision,file_path,action,file)
VALUES ($1,1,$2,'upsert',$3)`, value.ID, file.Path, fileJSON); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range file.Chunks {
		if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_block_usage(workspace_id,sha256,state,allocated_bytes)
VALUES ($1,$2,'stored',$3) ON CONFLICT(workspace_id,sha256) DO NOTHING`, value.ID, chunk.SHA256, chunk.SizeBytes); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET revision=1,used_bytes=$2 WHERE id=$1`, value.ID, file.SizeBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_current_materializations SET desired_revision=1,applied_revision=1 WHERE workspace_id=$1`, value.ID); err != nil {
		t.Fatal(err)
	}
}
