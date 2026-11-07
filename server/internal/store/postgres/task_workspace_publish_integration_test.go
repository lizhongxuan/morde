package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
)

const taskWorkspacePublishRuntimeID = "ri_task_publish"

func newTaskWorkspacePublishTask(t *testing.T, ctx context.Context, store *Store, name string) (host.Host, workspace.Workspace, workspace.Binding, task.Task, app.TaskWorkspacePublishCommand) {
	t.Helper()
	owner := createPreRetirementTestUser(t, store, 3, 10_000, 20_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, name)
	hostValue := createRunningControlHostWithoutWorkspace(t, ctx, store, owner.ID, "ai", 0)
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id=$2 WHERE id=$1`, hostValue.ID, taskWorkspacePublishRuntimeID); err != nil {
		t.Fatal(err)
	}
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
	manifestSHA, err := workspace.ManifestSHA256([]workspace.File{})
	if err != nil {
		t.Fatal(err)
	}
	contextDigest, err := hex.DecodeString(manifestSHA)
	if err != nil {
		t.Fatal(err)
	}
	revision := workspaceValue.Revision
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "task-workspace-publish:"+name, "write a report")
	submission.Task.WorkspaceID = workspaceValue.ID
	submission.Task.InputRevision = &revision
	submission.Task.ContextDigest = contextDigest
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit Task Workspace publish task: %v", err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "task-workspace-publish-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim Task Workspace publish task: %#v found=%t err=%v", claimed, found, err)
	}
	requestHash := sha256.Sum256([]byte("Task Workspace publish:" + name))
	command := app.TaskWorkspacePublishCommand{
		OwnerID: owner.ID, TaskID: claimed.ID, WorkspaceID: workspaceValue.ID, HostID: hostValue.ID,
		LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		BindingVersion: binding.BindingVersion, InputRevision: revision, BaseRevision: revision,
		HostFence: app.AgentHostFence{RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: taskWorkspacePublishRuntimeID,
			ControlVersion: hostValue.ControlVersion, AIAllowed: true},
		ViewID: "v" + strings.Repeat("a", 64), InputManifestSHA256: manifestSHA,
		OperationKey: "task-workspace-publish:s1234567:" + workspaceValue.ID, RequestHash: requestHash[:],
	}
	return hostValue, workspaceValue, binding, claimed, command
}

func taskWorkspacePublishTestService(t *testing.T, store *Store) (*app.WorkspaceSyncService, *workspacefs.Store) {
	t.Helper()
	blocks, err := workspacefs.Open(filepath.Join(t.TempDir(), "blocks"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocks.Close() })
	service, err := app.NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, syncTestOwnerQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}
	return service, blocks
}

func taskWorkspacePublishSnapshot(t *testing.T, files []workspace.File, emptyDirectories []string) ([]workspace.File, []workspace.Directory, string) {
	t.Helper()
	for index := range files {
		files[index].MetadataKnown = true
	}
	directoryPaths := map[string]string{".": "."}
	addDirectory := func(path string, includeSelf bool) {
		parts := strings.Split(path, "/")
		limit := len(parts)
		if !includeSelf {
			limit--
		}
		prefix := ""
		for index := 0; index < limit; index++ {
			if index > 0 {
				prefix += "/"
			}
			prefix += parts[index]
			directoryPaths[strings.ToLower(prefix)] = prefix
		}
	}
	for _, file := range files {
		addDirectory(file.Path, false)
	}
	for _, path := range emptyDirectories {
		addDirectory(path, true)
	}
	paths := make([]string, 0, len(directoryPaths))
	for _, path := range directoryPaths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	directories := make([]workspace.Directory, 0, len(paths))
	for _, path := range paths {
		mode, modTime := uint32(0o755), int64(1)
		directories = append(directories, workspace.Directory{Path: path, Mode: &mode, ModTimeNS: &modTime, MetadataKnown: true})
	}
	digest, err := workspace.ManifestTreeWithMetadataSHA256(files, emptyDirectories, directories)
	if err != nil {
		t.Fatal(err)
	}
	return files, directories, digest
}

func TestTaskWorkspacePublishCommitsCheckpointAndBlocksNewOperations(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	hostValue, workspaceValue, binding, claimed, command := newTaskWorkspacePublishTask(t, ctx, store, "task-publish-success")
	service, _ := taskWorkspacePublishTestService(t, store)
	file := syncTestFile("reports/result.txt", []byte("published result"))
	file.Xattrs = []workspace.ExtendedAttribute{{Name: "user.task", Value: []byte("kept")}}
	file.ACL = &workspace.NativeACL{Platform: "linux", Format: "linux-posix-acl-v1", Data: []byte{7, 8, 9}}
	emptyDirectories := []string{"reports/archive"}
	files, directories, _ := taskWorkspacePublishSnapshot(t, []workspace.File{file}, emptyDirectories)
	directories[0].Xattrs = []workspace.ExtendedAttribute{{Name: "user.root", Value: []byte("task root")}}
	manifestSHA, err := workspace.ManifestTreeWithMetadataSHA256(files, emptyDirectories, directories)
	if err != nil {
		t.Fatal(err)
	}
	operation := task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: claimed.ID, Key: "before-publish",
		RequestHash: hashBytes("operation before publish"), HostID: hostValue.ID, WorkspaceID: workspaceValue.ID,
		BindingVersion: binding.BindingVersion, InputRevision: claimed.InputRevision,
		WorkspaceCheckpointRevision: claimed.WorkspaceCheckpointRevision,
		RuntimeGeneration:           command.HostFence.RuntimeGeneration, RuntimeInstanceID: command.HostFence.RuntimeInstanceID,
		ControlVersion: command.HostFence.ControlVersion, LeaseVersion: claimed.Lease.Version,
		RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationPrepared,
	}
	if _, _, err := store.CreateOperation(ctx, operation, claimed.Lease.Owner); err != nil {
		t.Fatalf("CreateOperation before pending publish: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_legacy_volumes (workspace_id, volume_name, import_state) VALUES ($1,$2,'pending')`, workspaceValue.ID, "legacy-"+workspaceValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveTaskWorkspacePublish(ctx, command); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("reserve Task Workspace publish during pending legacy import = %v; want workspace busy", err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM workspace_legacy_volumes WHERE workspace_id=$1`, workspaceValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReserveTaskWorkspacePublish(ctx, command); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("reserve Task Workspace publish during operation = %v; want workspace busy", err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: claimed.ID, OperationKey: operation.Key, LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, RuntimeGeneration: command.HostFence.RuntimeGeneration,
		RuntimeInstanceID: command.HostFence.RuntimeInstanceID, ControlVersion: command.HostFence.ControlVersion,
		RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationDispatched, ExternalID: "before-publish",
	}); err != nil {
		t.Fatalf("dispatch operation before publish: %v", err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: claimed.ID, OperationKey: operation.Key, LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, Status: task.OperationSucceeded, Result: `{"ok":true}`,
	}); err != nil {
		t.Fatalf("settle operation before publish: %v", err)
	}
	reserved, err := service.ReserveTaskWorkspacePublish(ctx, command)
	if err != nil || reserved.State != app.TaskWorkspacePublishPending {
		t.Fatalf("reserve Task Workspace publish = %#v, err=%v", reserved, err)
	}
	blocked := operation
	blocked.ID, blocked.Key, blocked.RequestHash = mustID(t, domain.NewOperationID), "blocked-during-publish", hashBytes("blocked operation")
	if _, _, err := store.CreateOperation(ctx, blocked, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("CreateOperation during pending publish = %v; want workspace busy", err)
	}
	reads := 0
	receipt, err := service.CommitTaskWorkspacePublish(ctx, command, files, emptyDirectories, directories, manifestSHA, func(_ context.Context, digest string) ([]byte, error) {
		reads++
		if digest != file.SHA256 {
			t.Fatalf("requested output block %q; want %q", digest, file.SHA256)
		}
		return []byte("published result"), nil
	})
	if err != nil || receipt.State != app.TaskWorkspacePublishSucceeded || receipt.PublishedRevision != 1 || reads != 1 {
		t.Fatalf("commit Task Workspace publish = %#v, reads=%d, err=%v", receipt, reads, err)
	}
	page, err := store.GetWorkspaceManifestPage(ctx, workspaceValue.ID, receipt.PublishedRevision, "", "", 10)
	if err != nil || len(page.Files) != 1 || page.Files[0].Path != file.Path || page.Files[0].SHA256 != file.SHA256 ||
		len(page.EmptyDirectories) != 1 || page.EmptyDirectories[0] != emptyDirectories[0] ||
		!reflect.DeepEqual(page.Files[0].Xattrs, file.Xattrs) || page.Files[0].ACL == nil || file.ACL == nil ||
		page.Files[0].ACL.Platform != file.ACL.Platform || page.Files[0].ACL.Format != file.ACL.Format || !bytes.Equal(page.Files[0].ACL.Data, file.ACL.Data) ||
		len(page.Directories) != len(directories) || !workspace.SameDirectoryMetadata(page.Directories[0], directories[0]) {
		t.Fatalf("published Workspace manifest = %#v, err=%v", page, err)
	}
	changes, err := store.GetWorkspaceChangePage(ctx, workspaceValue.ID, 0, "", 10)
	if err != nil {
		t.Fatalf("read Task publish change feed: %v", err)
	}
	var rootChange *workspace.Directory
	for _, item := range changes.Items {
		if item.Path == "." && item.Action == workspace.ChangeDirectoryUpsert {
			rootChange = item.Directory
		}
	}
	if rootChange == nil || !workspace.SameDirectoryMetadata(*rootChange, directories[0]) {
		t.Fatalf("Task publish root directory change = %#v; want %#v", rootChange, directories[0])
	}
	details, err := store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil || details.Task.WorkspaceCheckpointRevision == nil || *details.Task.WorkspaceCheckpointRevision != receipt.PublishedRevision {
		t.Fatalf("Task after publish = %#v, err=%v", details.Task, err)
	}
	bindingAfterPublish, err := store.GetWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID)
	if err != nil || bindingAfterPublish.DesiredRevision != receipt.PublishedRevision || bindingAfterPublish.AppliedRevision != 0 {
		t.Fatalf("Host binding after Task publish = %#v, err=%v; want desired checkpoint with unapplied Host replica", bindingAfterPublish, err)
	}
	afterPublish := operation
	afterPublish.ID = mustID(t, domain.NewOperationID)
	afterPublish.Key = "after-publish"
	afterPublish.RequestHash = hashBytes("operation after Task publish")
	afterPublish.WorkspaceCheckpointRevision = &receipt.PublishedRevision
	afterPublish.Status = task.OperationPrepared
	if _, _, err := store.CreateOperation(ctx, afterPublish, claimed.Lease.Owner); err != nil {
		t.Fatalf("CreateOperation after Task published its private-view checkpoint: %v", err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: claimed.ID, OperationKey: afterPublish.Key, LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, RuntimeGeneration: command.HostFence.RuntimeGeneration,
		RuntimeInstanceID: command.HostFence.RuntimeInstanceID, ControlVersion: command.HostFence.ControlVersion,
		RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationDispatched, ExternalID: "after-publish",
	}); err != nil {
		t.Fatalf("dispatch operation after Task publish: %v", err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: claimed.ID, OperationKey: afterPublish.Key, LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, Status: task.OperationSucceeded, Result: `{"ok":true}`,
	}); err != nil {
		t.Fatalf("settle operation after Task publish: %v", err)
	}
	restartedService, _ := taskWorkspacePublishTestService(t, store)
	replayed, err := restartedService.CommitTaskWorkspacePublish(ctx, command, files, emptyDirectories, directories, manifestSHA, func(context.Context, string) ([]byte, error) {
		t.Fatal("published receipt replay read Runtime output blocks")
		return nil, nil
	})
	if err != nil || replayed != receipt || reads != 1 {
		t.Fatalf("idempotent publish replay = %#v, reads=%d, err=%v", replayed, reads, err)
	}
}

func TestTaskWorkspacePublishedArtifactPersistsAcrossPostgresConnectionRestartForOwner(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	_, workspaceValue, _, claimed, command := newTaskWorkspacePublishTask(t, ctx, store, "persisted-owner-artifact")
	otherOwner := createTestUser(t, store, 2, 4000, 8000)

	blocksRoot := filepath.Join(t.TempDir(), "blocks")
	blocks, err := workspacefs.Open(blocksRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := blocks.Close(); err != nil {
			t.Errorf("close Workspace block store: %v", err)
		}
	})
	service, err := app.NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, syncTestOwnerQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}

	wantContent := []byte("persisted Workspace artifact")
	file := syncTestFile("reports/generated.txt", wantContent)
	files, directories, _ := taskWorkspacePublishSnapshot(t, []workspace.File{file}, nil)
	manifestSHA, err := workspace.ManifestTreeWithMetadataSHA256(files, nil, directories)
	if err != nil {
		t.Fatal(err)
	}
	stepID := mustID(t, domain.NewStepID)
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusRunning, CurrentStep: stepID,
		Steps: []task.Step{{ID: stepID, TaskID: claimed.ID, Sequence: 1, Kind: "tool", Status: task.StepSucceeded, Title: "Save artifact"}},
		Event: task.Event{Type: "task.step", Payload: []byte(`{"step_id":"` + stepID + `"}`)},
	}); err != nil {
		t.Fatalf("persist artifact source step: %v", err)
	}

	proof := app.TaskArtifactProof{OwnerID: claimed.OwnerID, WorkspaceID: workspaceValue.ID, Revision: 1,
		Path: file.Path, SizeBytes: file.SizeBytes, SHA256: file.SHA256}
	if _, err := service.VerifyCommittedArtifact(ctx, proof); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("verify unpublished artifact = %v; want workspace conflict", err)
	}
	if details, err := store.GetTask(ctx, claimed.ID, claimed.OwnerID); err != nil || len(details.Artifacts) != 0 {
		t.Fatalf("unpublished artifact was reported on Task: artifacts=%#v err=%v", details.Artifacts, err)
	}

	reserved, err := service.ReserveTaskWorkspacePublish(ctx, command)
	if err != nil || reserved.State != app.TaskWorkspacePublishPending {
		t.Fatalf("reserve artifact publication = %#v, err=%v", reserved, err)
	}
	receipt, err := service.CommitTaskWorkspacePublish(ctx, command, files, nil, directories, manifestSHA,
		func(_ context.Context, digest string) ([]byte, error) {
			if digest != file.SHA256 {
				t.Fatalf("requested output block %q; want %q", digest, file.SHA256)
			}
			return append([]byte(nil), wantContent...), nil
		})
	if err != nil || receipt.State != app.TaskWorkspacePublishSucceeded || receipt.PublishedRevision != 1 {
		t.Fatalf("commit artifact publication = %#v, err=%v", receipt, err)
	}
	artifact := task.Artifact{ID: mustID(t, domain.NewArtifactID), TaskID: claimed.ID, StepID: stepID,
		WorkspaceID: workspaceValue.ID, Revision: receipt.PublishedRevision, SizeBytes: file.SizeBytes,
		SHA256: file.SHA256, Label: "Generated report", Path: file.Path, MediaType: "text/plain"}
	if _, err := store.RecordValidatedArtifact(ctx, claimed.OwnerID, artifact, claimed.Lease.Owner, claimed.Lease.Version); err != nil {
		t.Fatalf("record committed Workspace artifact: %v", err)
	}
	if _, err := service.VerifyCommittedArtifact(ctx, app.TaskArtifactProof{
		OwnerID: claimed.OwnerID, WorkspaceID: workspaceValue.ID, Revision: receipt.PublishedRevision + 1,
		Path: file.Path, SizeBytes: file.SizeBytes, SHA256: file.SHA256,
	}); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("verify artifact at an uncommitted revision = %v; want workspace conflict", err)
	}

	// Close the only application pool and create a fresh one against the same
	// temporary PostgreSQL database. The Workspace block store remains durable
	// independently of PostgreSQL, matching the production storage boundary.
	reopenConfig := pool.Config().Copy()
	pool.Close()
	reopenedPool, err := pgxpool.NewWithConfig(ctx, reopenConfig)
	if err != nil {
		t.Fatalf("reopen temporary PostgreSQL database: %v", err)
	}
	t.Cleanup(func() { reopenedPool.Close() })
	reopenedStore := New(reopenedPool)
	reopenedService, err := app.NewWorkspaceSyncService(reopenedStore, blocks, workspace.MaxSyncManifestBytes, syncTestOwnerQuotaBytes)
	if err != nil {
		t.Fatal(err)
	}

	ownerDetails, err := reopenedStore.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil || len(ownerDetails.Artifacts) != 1 || ownerDetails.Artifacts[0].ID != artifact.ID ||
		ownerDetails.Artifacts[0].Path != file.Path || ownerDetails.Artifacts[0].Revision != receipt.PublishedRevision {
		t.Fatalf("owner Task artifacts after PostgreSQL reconnect = %#v, err=%v", ownerDetails.Artifacts, err)
	}
	if _, err := reopenedStore.GetTask(ctx, claimed.ID, otherOwner.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other owner's Task artifact lookup = %v; want not found", err)
	}
	principal := identity.Principal{UserID: claimed.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	page, err := reopenedService.GetManifest(ctx, principal, workspaceValue.ID, receipt.PublishedRevision, "", "", 10)
	if err != nil || len(page.Files) != 1 || page.Files[0].Path != file.Path || page.Files[0].SHA256 != file.SHA256 {
		t.Fatalf("owner manifest after PostgreSQL reconnect = %#v, err=%v", page, err)
	}
	otherPrincipal := identity.Principal{UserID: otherOwner.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t7654321"}
	if _, err := reopenedService.GetManifest(ctx, otherPrincipal, workspaceValue.ID, receipt.PublishedRevision, "", "", 10); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other owner's Workspace manifest lookup = %v; want not found", err)
	}
	if _, err := reopenedService.VerifyCommittedArtifact(ctx, proof); err != nil {
		t.Fatalf("verify published artifact after PostgreSQL reconnect: %v", err)
	}
	block, err := blocks.Open(workspaceValue.ID, file.Chunks[0].SHA256)
	if err != nil {
		t.Fatalf("open persisted artifact block: %v", err)
	}
	gotContent, readErr := io.ReadAll(block)
	closeErr := block.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(gotContent, wantContent) {
		t.Fatalf("persisted artifact content = %q, read err=%v close err=%v", gotContent, readErr, closeErr)
	}
}

func TestTaskWorkspacePublishBlocksMCPAdmissionAndDispatch(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	hostValue, workspaceValue, binding, claimed, command := newTaskWorkspacePublishTask(t, ctx, store, "task-publish-mcp-barrier")
	connectorID := mustID(t, domain.NewMCPConnectorID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO mcp_connectors (id, owner_id, name, endpoint_url, enabled, credential_version, revision)
VALUES ($1,$2,$3,$4,true,0,1)`, connectorID, claimed.OwnerID, "publish barrier connector", "https://connector.example.test/mcp"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_mcp_connector_grants
(task_id, owner_id, connector_id, connector_revision, credential_version, grant_version, connector_owner_id)
VALUES ($1,$2,$3,1,0,1,$2)`, claimed.ID, claimed.OwnerID, connectorID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveTaskWorkspacePublish(ctx, command); err != nil {
		t.Fatalf("reserve Task Workspace publish: %v", err)
	}
	operation := app.MCPConnectorOperation{
		ID: mustID(t, domain.NewOperationID), TaskID: claimed.ID, OwnerID: claimed.OwnerID, ConnectorID: connectorID,
		Key: "mcp-during-publish", ToolName: "search", ConnectorRevision: 1, GrantVersion: 1,
		LeaseVersion: claimed.Lease.Version, HostID: hostValue.ID, WorkspaceID: workspaceValue.ID,
		BindingVersion: binding.BindingVersion, InputRevision: claimed.InputRevision,
		WorkspaceCheckpointRevision: claimed.WorkspaceCheckpointRevision, RequestHash: hashBytes("mcp-during-publish"),
	}
	if _, _, err := store.PrepareMCPConnectorOperation(ctx, operation, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("MCP prepare during Task Workspace publish = %v; want Workspace busy", err)
	}
	dispatch := operation
	dispatch.ID, dispatch.Key = mustID(t, domain.NewOperationID), "mcp-dispatch-during-publish"
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_mcp_connector_operations
(id,task_id,owner_id,connector_id,connector_revision,credential_version,grant_version,lease_version,
 host_id,workspace_id,binding_version,input_revision,workspace_checkpoint_revision,
 operation_key,tool_name,request_hash,status)
VALUES ($1,$2,$3,$4,1,0,1,$5,$6,$7,$8,$9,$10,$11,$12,$13,'prepared')`,
		dispatch.ID, dispatch.TaskID, dispatch.OwnerID, dispatch.ConnectorID, dispatch.LeaseVersion,
		dispatch.HostID, dispatch.WorkspaceID, dispatch.BindingVersion, *dispatch.InputRevision,
		*dispatch.WorkspaceCheckpointRevision, dispatch.Key, dispatch.ToolName, dispatch.RequestHash); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkMCPConnectorOperationDispatched(ctx, dispatch, claimed.Lease.Owner); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("MCP dispatch during Task Workspace publish = %v; want Workspace busy", err)
	}
}

func TestTaskWorkspacePublishBlocksSyncUntilPublishSettles(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	_, workspaceValue, _, _, command := newTaskWorkspacePublishTask(t, ctx, store, "task-publish-sync-barrier")
	service, _ := taskWorkspacePublishTestService(t, store)
	if _, err := service.ReserveTaskWorkspacePublish(ctx, command); err != nil {
		t.Fatal(err)
	}
	external := syncTestFile("reports/result.txt", []byte("same content"))
	session := newTestSyncSession(t, workspaceValue, command.OwnerID, []workspace.File{external}, []workspace.File{external}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks(session.UploadFiles),
		syncTestIdempotency(command.OwnerID, "workspace.sync.create", "task-publish-sync-barrier:create", session.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, workspaceValue.ID, session.ID,
		syncTestIdempotency(command.OwnerID, "workspace.sync.commit", "task-publish-sync-barrier:commit", session.ID)); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("sync commit during pending Task publish = %v; want Workspace busy", err)
	}
	file := syncTestFile("reports/task.txt", []byte("task output"))
	files, directories, manifestSHA := taskWorkspacePublishSnapshot(t, []workspace.File{file}, nil)
	file = files[0]
	published, err := service.CommitTaskWorkspacePublish(ctx, command, files, nil, directories, manifestSHA, func(_ context.Context, digest string) ([]byte, error) {
		if digest != file.SHA256 {
			t.Fatalf("Task publish requested block %q; want %q", digest, file.SHA256)
		}
		return []byte("task output"), nil
	})
	if err != nil || published.State != app.TaskWorkspacePublishSucceeded || published.PublishedRevision != 1 {
		t.Fatalf("Task publish after blocked sync = %#v, err=%v", published, err)
	}
	receipt, err := store.FindTaskWorkspacePublishReceipt(ctx, command.OwnerID, command.TaskID, command.WorkspaceID, command.OperationKey)
	if err != nil || receipt.State != app.TaskWorkspacePublishSucceeded {
		t.Fatalf("Task publish receipt = %#v, err=%v", receipt, err)
	}
	page, err := store.GetWorkspaceManifestPage(ctx, workspaceValue.ID, 1, "", "", 10)
	if err != nil || len(page.Files) != 1 || page.Files[0].Path != file.Path || page.Files[0].SHA256 != file.SHA256 {
		t.Fatalf("Workspace revision after Task publish = %#v, err=%v", page, err)
	}
}

func TestTaskWorkspacePublishSerializesSameContentAcrossTasks(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	hostValue, workspaceValue, binding, first, firstCommand := newTaskWorkspacePublishTask(t, ctx, store, "task-publish-same-content-race")
	manifestSHA, err := workspace.ManifestSHA256([]workspace.File{})
	if err != nil {
		t.Fatal(err)
	}
	contextDigest, err := hex.DecodeString(manifestSHA)
	if err != nil {
		t.Fatal(err)
	}
	inputRevision := workspaceValue.Revision
	secondSubmission := newTestTaskSubmission(t, first.OwnerID, hostValue.ID, "task-publish-same-content-second", "write the same report")
	secondSubmission.Task.WorkspaceID = workspaceValue.ID
	secondSubmission.Task.InputRevision = &inputRevision
	secondSubmission.Task.ContextDigest = contextDigest
	secondCreated, err := store.SubmitTask(ctx, secondSubmission)
	if err != nil {
		t.Fatalf("submit second Task at the same Workspace revision: %v", err)
	}
	second, found, err := store.ClaimNextTask(ctx, "task-workspace-publish-second-worker", time.Minute)
	if err != nil || !found || second.ID != secondCreated.Task.ID {
		t.Fatalf("claim second Task: task=%#v found=%t err=%v", second, found, err)
	}
	secondRequestHash := sha256.Sum256([]byte("second Task publish same content"))
	secondCommand := firstCommand
	secondCommand.TaskID, secondCommand.LeaseOwner, secondCommand.LeaseVersion = second.ID, second.Lease.Owner, second.Lease.Version
	secondCommand.ViewID = "v" + strings.Repeat("b", 64)
	secondCommand.OperationKey = "task-workspace-publish:second:" + workspaceValue.ID
	secondCommand.RequestHash = secondRequestHash[:]

	service, _ := taskWorkspacePublishTestService(t, store)
	for _, command := range []app.TaskWorkspacePublishCommand{firstCommand, secondCommand} {
		if _, err := service.ReserveTaskWorkspacePublish(ctx, command); err != nil {
			t.Fatalf("reserve competing publish for task %s: %v", command.TaskID, err)
		}
	}
	file := syncTestFile("reports/shared.txt", []byte("identical output from separate Tasks"))
	files, directories, outputManifestSHA := taskWorkspacePublishSnapshot(t, []workspace.File{file}, nil)
	file = files[0]
	type publishResult struct {
		taskID  string
		command app.TaskWorkspacePublishCommand
		receipt app.TaskWorkspacePublishReceipt
		err     error
	}
	start := make(chan struct{})
	results := make(chan publishResult, 2)
	for _, command := range []app.TaskWorkspacePublishCommand{firstCommand, secondCommand} {
		command := command
		go func() {
			<-start
			receipt, publishErr := service.CommitTaskWorkspacePublish(ctx, command, files, nil, directories, outputManifestSHA,
				func(_ context.Context, digest string) ([]byte, error) {
					if digest != file.SHA256 {
						return nil, errors.New("unexpected output block digest")
					}
					return []byte("identical output from separate Tasks"), nil
				})
			results <- publishResult{taskID: command.TaskID, command: command, receipt: receipt, err: publishErr}
		}()
	}
	close(start)

	var winnerID string
	var succeeded, conflicted int
	var retryCommand *app.TaskWorkspacePublishCommand
	for range 2 {
		result := <-results
		switch {
		case result.err == nil && result.receipt.State == app.TaskWorkspacePublishSucceeded && result.receipt.PublishedRevision == inputRevision+1:
			winnerID = result.taskID
			succeeded++
		case domain.CodeOf(result.err) == domain.CodeWorkspaceConflict:
			persisted, err := store.FindTaskWorkspacePublishReceipt(ctx, result.command.OwnerID, result.command.TaskID, result.command.WorkspaceID, result.command.OperationKey)
			if err != nil || persisted.State != app.TaskWorkspacePublishConflict {
				t.Fatalf("losing Task conflict receipt = %#v, err=%v", persisted, err)
			}
			conflicted++
		case domain.CodeOf(result.err) == domain.CodeWorkspaceBusy:
			command := result.command
			retryCommand = &command
		default:
			t.Fatalf("competing Task publish: task=%s receipt=%#v err=%v", result.taskID, result.receipt, result.err)
		}
	}
	if retryCommand != nil {
		_, err := service.CommitTaskWorkspacePublish(ctx, *retryCommand, files, nil, directories, outputManifestSHA,
			func(_ context.Context, digest string) ([]byte, error) {
				if digest != file.SHA256 {
					return nil, errors.New("unexpected retry output block digest")
				}
				return []byte("identical output from separate Tasks"), nil
			})
		persisted, findErr := store.FindTaskWorkspacePublishReceipt(ctx, retryCommand.OwnerID, retryCommand.TaskID, retryCommand.WorkspaceID, retryCommand.OperationKey)
		if domain.CodeOf(err) != domain.CodeWorkspaceConflict || findErr != nil || persisted.State != app.TaskWorkspacePublishConflict {
			t.Fatalf("losing Task publish retry conflict = %#v, publish err=%v, receipt err=%v", persisted, err, findErr)
		}
		conflicted++
	}
	if succeeded != 1 || conflicted != 1 || winnerID == "" {
		t.Fatalf("same-content publish results: succeeded=%d conflicted=%d winner=%q", succeeded, conflicted, winnerID)
	}
	workspaceAfter, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || workspaceAfter.Revision != inputRevision+1 {
		t.Fatalf("Workspace after same-content race = revision %d, err=%v", workspaceAfter.Revision, err)
	}
	page, err := store.GetWorkspaceManifestPage(ctx, workspaceValue.ID, workspaceAfter.Revision, "", "", 10)
	if err != nil || len(page.Files) != 1 || page.Files[0].Path != file.Path || page.Files[0].SHA256 != file.SHA256 {
		t.Fatalf("Workspace after same-content race = %#v, err=%v", page, err)
	}

	loser := first
	loserCommand := firstCommand
	if winnerID == first.ID {
		loser, loserCommand = second, secondCommand
	}
	losingOperation := task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: loser.ID, Key: "after-competing-publish",
		RequestHash: hashBytes("loser operation after same-content publish"), HostID: hostValue.ID,
		WorkspaceID: workspaceValue.ID, BindingVersion: binding.BindingVersion, InputRevision: loser.InputRevision,
		WorkspaceCheckpointRevision: loser.WorkspaceCheckpointRevision,
		RuntimeGeneration:           loserCommand.HostFence.RuntimeGeneration, RuntimeInstanceID: loserCommand.HostFence.RuntimeInstanceID,
		ControlVersion: loserCommand.HostFence.ControlVersion, LeaseVersion: loser.Lease.Version,
		RuntimeLeaseVersion: loser.Lease.Version, Status: task.OperationPrepared,
	}
	if _, _, err := store.CreateOperation(ctx, losingOperation, loser.Lease.Owner); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("losing Task operation after identical external content = %v; want Workspace conflict", err)
	}
	winner := first
	if winnerID == second.ID {
		winner = second
	}
	winnerDetails, err := store.GetTask(ctx, winner.ID, winner.OwnerID)
	if err != nil || winnerDetails.Task.WorkspaceCheckpointRevision == nil || *winnerDetails.Task.WorkspaceCheckpointRevision != inputRevision+1 {
		t.Fatalf("winning Task checkpoint after same-content race = %#v, err=%v", winnerDetails.Task.WorkspaceCheckpointRevision, err)
	}
	winningOperation := losingOperation
	winningOperation.ID, winningOperation.TaskID = mustID(t, domain.NewOperationID), winner.ID
	winningOperation.Key, winningOperation.RequestHash = "after-winning-publish", hashBytes("winner continues after publish")
	winningOperation.LeaseVersion, winningOperation.RuntimeLeaseVersion = winner.Lease.Version, winner.Lease.Version
	winningOperation.WorkspaceCheckpointRevision = winnerDetails.Task.WorkspaceCheckpointRevision
	if _, _, err := store.CreateOperation(ctx, winningOperation, winner.Lease.Owner); err != nil {
		t.Fatalf("winning Task operation after its publish = %v", err)
	}
}

func TestTaskWorkspacePublishRejectsTaskWithoutLease(t *testing.T) {
	t.Run("reservation", func(t *testing.T) {
		store, _ := newTemporaryStore(t)
		ctx := context.Background()
		_, _, _, claimed, command := newTaskWorkspacePublishTask(t, ctx, store, "task-publish-no-lease-reserve")
		if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='paused', lease_owner='', lease_expires_at=NULL,
waiting_reason='workspace_input_changed' WHERE id=$1`, claimed.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ReserveTaskWorkspacePublish(ctx, command); domain.CodeOf(err) != domain.CodeInvalidState {
			t.Fatalf("publish reservation without a Task lease = %v; want lease conflict", err)
		}
	})

	t.Run("commit", func(t *testing.T) {
		store, _ := newTemporaryStore(t)
		ctx := context.Background()
		_, _, _, claimed, command := newTaskWorkspacePublishTask(t, ctx, store, "task-publish-no-lease-commit")
		if _, err := store.ReserveTaskWorkspacePublish(ctx, command); err != nil {
			t.Fatalf("reserve before pausing Task: %v", err)
		}
		if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='paused', lease_owner='', lease_expires_at=NULL,
waiting_reason='workspace_input_changed' WHERE id=$1`, claimed.ID); err != nil {
			t.Fatal(err)
		}
		files, directories, manifestSHA := taskWorkspacePublishSnapshot(t, []workspace.File{}, nil)
		if _, err := store.CommitTaskWorkspacePublish(ctx, command, files, nil, directories, manifestSHA); domain.CodeOf(err) != domain.CodeInvalidState {
			t.Fatalf("publish commit without a Task lease = %v; want lease conflict", err)
		}
	})
}

func TestTaskWorkspacePublishQuotaRejectionReleasesOperationBarrier(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	hostValue, workspaceValue, binding, claimed, command := newTaskWorkspacePublishTask(t, ctx, store, "task-publish-quota")
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET quota_bytes=1 WHERE id=$1`, workspaceValue.ID); err != nil {
		t.Fatal(err)
	}
	service, _ := taskWorkspacePublishTestService(t, store)
	file := syncTestFile("reports/result.txt", []byte("over quota"))
	files, directories, manifestSHA := taskWorkspacePublishSnapshot(t, []workspace.File{file}, nil)
	if _, err := service.ReserveTaskWorkspacePublish(ctx, command); err != nil {
		t.Fatal(err)
	}
	_, err := service.CommitTaskWorkspacePublish(ctx, command, files, nil, directories, manifestSHA, func(context.Context, string) ([]byte, error) {
		return []byte("over quota"), nil
	})
	if domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("over-quota Task publish = %v; want quota exceeded", err)
	}
	receipt, err := service.FindTaskWorkspacePublishReceipt(ctx, command.OwnerID, command.TaskID, command.WorkspaceID, command.OperationKey)
	if err != nil || receipt.State != app.TaskWorkspacePublishQuotaRejected {
		t.Fatalf("quota rejection receipt = %#v, err=%v", receipt, err)
	}
	operation := task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: claimed.ID, Key: "after-quota-rejection",
		RequestHash: hashBytes("after quota rejection"), HostID: hostValue.ID, WorkspaceID: workspaceValue.ID,
		BindingVersion: binding.BindingVersion, InputRevision: claimed.InputRevision,
		WorkspaceCheckpointRevision: claimed.WorkspaceCheckpointRevision,
		RuntimeGeneration:           command.HostFence.RuntimeGeneration, RuntimeInstanceID: command.HostFence.RuntimeInstanceID,
		ControlVersion: command.HostFence.ControlVersion, LeaseVersion: claimed.Lease.Version,
		RuntimeLeaseVersion: claimed.Lease.Version, Status: task.OperationPrepared,
	}
	if _, _, err := store.CreateOperation(ctx, operation, claimed.Lease.Owner); err != nil {
		t.Fatalf("CreateOperation after quota rejection = %v; want operation barrier released", err)
	}
	current, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || current.Revision != 0 || current.UsedBytes != 0 {
		t.Fatalf("Workspace after quota rejection = %#v, err=%v", current, err)
	}
}
