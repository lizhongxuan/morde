package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacecurrent"
)

func TestPostgresWorkspaceOwnershipTransferPreservesTaskHistoryAndRevokesFormerOwner(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	oldOwner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	newOwner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, oldOwner.ID, host.ControlAI, 0)
	workspaceValue := hostValue.Workspaces[0].Workspace
	prepareTransferWorkspace(t, ctx, store, workspaceValue.ID, oldOwner.ID, 5)

	binding, err := store.GetWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, workspaceValue.Revision)
	if err != nil {
		t.Fatalf("activate Workspace binding for historical Task: %v", err)
	}
	submission := newTestTaskSubmission(t, oldOwner.ID, hostValue.ID, "transfer-history-task", "remember the existing file")
	submission.Task.WorkspaceID = workspaceValue.ID
	inputRevision := workspaceValue.Revision
	submission.Task.InputRevision = &inputRevision
	submission.Task.ContextDigest = hashBytes("Workspace input snapshot")
	createdTask, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("create historical Workspace Task: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='completed', finished_at=clock_timestamp() WHERE id=$1`, createdTask.Task.ID); err != nil {
		t.Fatalf("complete historical Workspace Task: %v", err)
	}
	binding, err = store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingActive, workspace.BindingUnbinding, workspaceValue.Revision, workspaceValue.Revision)
	if err != nil {
		t.Fatalf("begin Workspace detach: %v", err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
		workspace.BindingUnbinding, workspace.BindingDetached, workspaceValue.Revision, workspaceValue.Revision); err != nil {
		t.Fatalf("detach Workspace: %v", err)
	}

	oldPrincipal := workspaceTransferPrincipal(oldOwner.ID)
	newPrincipal := workspaceTransferPrincipal(newOwner.ID)
	manifest, err := testWorkspaceSyncService(t, store).GetManifest(ctx, oldPrincipal, workspaceValue.ID, workspaceValue.Revision, "", "", 100)
	if err != nil || len(manifest.Files) != 1 || manifest.Files[0].Path != "README.md" || manifest.Files[0].SizeBytes != 5 {
		t.Fatalf("former owner's initial manifest = %#v, %v", manifest, err)
	}
	syncSessionID := mustID(t, workspace.NewSyncSessionID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_sync_sessions
(id,workspace_id,owner_id,replica_id,direction,policy,state,stage,base_revision,target_revision,pinned_revision,
 planned_bytes,confirmed_bytes,files,upload_files,download_files)
VALUES ($1,$2,$3,$4,'download','fill_missing','completed','complete',$5,$5,$5,0,0,'[]'::jsonb,'[]'::jsonb,'[]'::jsonb)`,
		syncSessionID, workspaceValue.ID, oldOwner.ID, "session:"+oldOwner.ID, workspaceValue.Revision); err != nil {
		t.Fatalf("create historical Workspace sync session: %v", err)
	}
	if _, err := testWorkspaceSyncService(t, store).GetSyncSession(ctx, oldPrincipal, workspaceValue.ID, syncSessionID); err != nil {
		t.Fatalf("former owner could not read their current-owner-scoped historical session before transfer: %v", err)
	}
	seedWorkspaceOwnerPhysicalUsage(t, ctx, store, oldOwner.ID, workspaceValue.ID, 9)

	service := newTestWorkspaceOwnershipTransferService(t, store, 100)
	created, replayed, err := service.RequestWorkspaceTransfer(ctx, oldPrincipal, workspaceValue.ID, newOwner.ID, workspaceValue.Revision, "transfer-request-01")
	if err != nil || replayed || created.State != workspace.OwnershipTransferPending {
		t.Fatalf("request ownership transfer = %#v replayed=%t err=%v", created, replayed, err)
	}
	replayedTransfer, replayed, err := service.RequestWorkspaceTransfer(ctx, oldPrincipal, workspaceValue.ID, newOwner.ID, workspaceValue.Revision, "transfer-request-01")
	if err != nil || !replayed || replayedTransfer.ID != created.ID {
		t.Fatalf("replay ownership transfer request = %#v replayed=%t err=%v", replayedTransfer, replayed, err)
	}

	accepted, err := service.AcceptWorkspaceTransfer(ctx, newPrincipal, workspaceValue.ID, created.ID, "transfer-accept-01")
	if err != nil || accepted.State != workspace.OwnershipTransferAccepted || accepted.OperationID == "" {
		t.Fatalf("accept Workspace transfer = %#v, %v", accepted, err)
	}
	repeated, err := service.AcceptWorkspaceTransfer(ctx, newPrincipal, workspaceValue.ID, created.ID, "transfer-accept-01")
	if err != nil || repeated.State != workspace.OwnershipTransferAccepted || repeated.OperationID != accepted.OperationID {
		t.Fatalf("replay Workspace acceptance = %#v, %v", repeated, err)
	}

	current, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || current.OwnerID != newOwner.ID || current.ID != workspaceValue.ID || current.Revision != workspaceValue.Revision || current.UsedBytes != 5 {
		t.Fatalf("Workspace identity or tree changed during transfer: %#v, %v", current, err)
	}
	var oldAllocated, newAllocated int64
	if err := store.pool.QueryRow(ctx, `SELECT allocated_bytes FROM workspace_owner_block_usage WHERE owner_id=$1`, oldOwner.ID).Scan(&oldAllocated); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT allocated_bytes FROM workspace_owner_block_usage WHERE owner_id=$1`, newOwner.ID).Scan(&newAllocated); err != nil {
		t.Fatal(err)
	}
	if oldAllocated != 0 || newAllocated != 9 {
		t.Fatalf("owner physical quota after transfer = old:%d new:%d, want old:0 new:9", oldAllocated, newAllocated)
	}
	var endedOwnerRows, currentOwnerRows int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_ownership_history
WHERE workspace_id=$1 AND owner_id=$2 AND ended_at IS NOT NULL`, workspaceValue.ID, oldOwner.ID).Scan(&endedOwnerRows); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_ownership_history
WHERE workspace_id=$1 AND owner_id=$2 AND ended_at IS NULL AND transfer_id=$3`, workspaceValue.ID, newOwner.ID, created.ID).Scan(&currentOwnerRows); err != nil {
		t.Fatal(err)
	}
	if endedOwnerRows != 1 || currentOwnerRows != 1 {
		t.Fatalf("Workspace ownership history = ended old rows %d, current new rows %d", endedOwnerRows, currentOwnerRows)
	}
	var volumeOwner, provisionedOwner string
	if err := store.pool.QueryRow(ctx, `SELECT owner_id, provisioned_owner_id FROM workspace_volumes WHERE workspace_id=$1`, workspaceValue.ID).
		Scan(&volumeOwner, &provisionedOwner); err != nil || volumeOwner != newOwner.ID || provisionedOwner != oldOwner.ID {
		t.Fatalf("Workspace volume ownership after transfer = %q/%q, %v", volumeOwner, provisionedOwner, err)
	}
	var historicalTaskOwner, historicalTaskInitiator string
	if err := store.pool.QueryRow(ctx, `SELECT owner_id,initiator_id FROM tasks WHERE id=$1`, createdTask.Task.ID).
		Scan(&historicalTaskOwner, &historicalTaskInitiator); err != nil || historicalTaskOwner != oldOwner.ID || historicalTaskInitiator != oldOwner.ID {
		t.Fatalf("Task ownership history changed during Workspace transfer: owner=%q initiator=%q err=%v", historicalTaskOwner, historicalTaskInitiator, err)
	}
	if _, err := store.GetTask(ctx, createdTask.Task.ID, oldOwner.ID); err != nil {
		t.Fatalf("former Task owner lost access through Task authorization: %v", err)
	}
	if _, err := store.GetTask(ctx, createdTask.Task.ID, newOwner.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("new Workspace owner read old Task history through Task authorization: %v", err)
	}

	syncService := testWorkspaceSyncService(t, store)
	if _, err := syncService.GetManifest(ctx, oldPrincipal, workspaceValue.ID, workspaceValue.Revision, "", "", 100); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("former owner read transferred Workspace file metadata: %v", err)
	}
	if _, err := syncService.GetSyncSession(ctx, oldPrincipal, workspaceValue.ID, syncSessionID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("former owner read historical Workspace sync session after transfer: %v", err)
	}
	newManifest, err := syncService.GetManifest(ctx, newPrincipal, workspaceValue.ID, workspaceValue.Revision, "", "", 100)
	if err != nil || len(newManifest.Files) != 1 || newManifest.Files[0].Path != "README.md" {
		t.Fatalf("new owner could not read preserved Workspace file metadata: %#v, %v", newManifest, err)
	}
	if _, err := syncService.GetSyncSession(ctx, newPrincipal, workspaceValue.ID, syncSessionID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("new owner read an old owner's session-scoped history: %v", err)
	}
}

func TestPostgresWorkspaceOwnershipTransferConfirmsRecipientProjectionBeforeAcceptance(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	oldOwner := createTestUser(t, store, 8, 20_000, 40_000)
	newOwner := createTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTransferReadyWorkspace(t, ctx, store, oldOwner.ID, "transfer-projection")
	currentRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	current, err := workspacecurrent.Open(filepath.Join(currentRoot, "current"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = current.Close() }()
	content := []byte("hello")
	digest := sha256.Sum256(content)
	digestText := hex.EncodeToString(digest[:])
	file := workspace.File{
		Path: "README.md", SizeBytes: int64(len(content)), SHA256: digestText, Mode: 0644, MetadataKnown: true,
		Chunks: []workspace.Chunk{{SHA256: digestText, SizeBytes: int64(len(content))}},
	}
	prepareTransferWorkspace(t, ctx, store, workspaceValue.ID, oldOwner.ID, int64(len(content)))
	seedWorkspaceOwnerPhysicalUsage(t, ctx, store, oldOwner.ID, workspaceValue.ID, int64(len(content)))
	blocks := workspaceTransferProjectionBlockStore{digestText: content}
	rootMetadata := []workspace.Directory{{Path: ".", MetadataKnown: true}}
	if err := current.Materialize(ctx, oldOwner.ID, workspaceValue.ID, workspaceValue.Revision, []workspace.File{file}, nil, rootMetadata, blocks); err != nil {
		t.Fatalf("materialize former-owner projection: %v", err)
	}
	syncService, err := app.NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncService.SetWorkspaceCurrentMaterializer(func(ctx context.Context, ownerID, workspaceID string, revision int64, files []workspace.File, directories []string, metadata []workspace.Directory) error {
		return current.Materialize(ctx, ownerID, workspaceID, revision, files, directories, metadata, blocks)
	}); err != nil {
		t.Fatal(err)
	}
	if err := syncService.SetWorkspaceCurrentRevisionReader(current.CurrentRevision); err != nil {
		t.Fatal(err)
	}
	if err := syncService.SetWorkspaceCurrentManifestVerifier(current.MatchesManifest); err != nil {
		t.Fatal(err)
	}
	service, err := app.NewWorkspaceTransferService(store, 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetWorkspaceOwnershipTransferProjection(app.WorkspaceOwnershipTransferProjection{
		PrepareRecipient: syncService.PrepareWorkspaceOwnershipTransferProjection,
		DiscardRecipient: func(ctx context.Context, value workspace.OwnershipTransfer) error {
			return current.Delete(ctx, value.ToOwnerID, value.WorkspaceID)
		},
		RetireFormer: func(ctx context.Context, value workspace.OwnershipTransfer) error {
			return current.Delete(ctx, value.FromOwnerID, value.WorkspaceID)
		},
	}); err != nil {
		t.Fatal(err)
	}
	created, _, err := service.RequestWorkspaceTransfer(ctx, workspaceTransferPrincipal(oldOwner.ID), workspaceValue.ID, newOwner.ID, workspaceValue.Revision, "projection-create-01")
	if err != nil {
		t.Fatalf("request Workspace transfer: %v", err)
	}
	accepted, err := service.AcceptWorkspaceTransfer(ctx, workspaceTransferPrincipal(newOwner.ID), workspaceValue.ID, created.ID, "projection-accept-01")
	if err != nil || accepted.State != workspace.OwnershipTransferAccepted {
		t.Fatalf("accept Workspace transfer with confirmed projection: %#v, %v", accepted, err)
	}
	if _, exists, err := current.CurrentRevision(oldOwner.ID, workspaceValue.ID); err != nil || exists {
		t.Fatalf("former-owner projection remains after acceptance: exists=%t err=%v", exists, err)
	}
	revision, exists, err := current.CurrentRevision(newOwner.ID, workspaceValue.ID)
	if err != nil || !exists || revision != workspaceValue.Revision {
		t.Fatalf("recipient projection after acceptance: revision=%d exists=%t err=%v", revision, exists, err)
	}
	matched, err := current.MatchesManifest(ctx, newOwner.ID, workspaceValue.ID, []workspace.File{file}, nil, rootMetadata)
	if err != nil || !matched {
		t.Fatalf("recipient projection does not match the committed manifest: matched=%t err=%v", matched, err)
	}
	ownerDigest := sha256.Sum256([]byte(newOwner.ID))
	projectedPath := filepath.Join(currentRoot, "current", hex.EncodeToString(ownerDigest[:]), workspaceValue.ID, "README.md")
	projectedBytes, err := os.ReadFile(projectedPath)
	info, statErr := os.Stat(projectedPath)
	if err != nil || statErr != nil {
		t.Fatalf("open recipient README projection: stat=%v, read=%v", statErr, err)
	}
	if !bytes.Equal(projectedBytes, content) || info.Mode().Perm() != 0644 {
		t.Fatalf("recipient README projection = bytes %q mode %o, want %q mode 0644", projectedBytes, info.Mode().Perm(), content)
	}
}

func TestPostgresWorkspaceOwnershipTransferSerializesAcceptAndCancel(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	oldOwner := createTestUser(t, store, 8, 20_000, 40_000)
	newOwner := createTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTransferReadyWorkspace(t, ctx, store, oldOwner.ID, "transfer-race")
	service := newTestWorkspaceOwnershipTransferService(t, store, 100)
	created, _, err := service.RequestWorkspaceTransfer(ctx, workspaceTransferPrincipal(oldOwner.ID), workspaceValue.ID, newOwner.ID, 0, "race-create-01")
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	type outcome struct {
		accepted bool
		err      error
	}
	acceptDone := make(chan outcome, 1)
	cancelDone := make(chan outcome, 1)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		<-start
		_, err := service.AcceptWorkspaceTransfer(ctx, workspaceTransferPrincipal(newOwner.ID), workspaceValue.ID, created.ID, "race-accept-01")
		acceptDone <- outcome{accepted: err == nil, err: err}
	}()
	go func() {
		defer wait.Done()
		<-start
		_, err := service.CancelWorkspaceTransfer(ctx, workspaceTransferPrincipal(oldOwner.ID), workspaceValue.ID, created.ID, "race-cancel-01")
		cancelDone <- outcome{accepted: err == nil, err: err}
	}()
	close(start)
	wait.Wait()
	accepted := <-acceptDone
	cancelled := <-cancelDone
	if accepted.accepted == cancelled.accepted {
		t.Fatalf("accept/cancel race had invalid winners: accept=%v cancel=%v", accepted.err, cancelled.err)
	}
	finalTransfer, err := store.GetWorkspaceOwnershipTransfer(ctx, oldOwner.ID, workspaceValue.ID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.accepted && (finalTransfer.State != workspace.OwnershipTransferAccepted || current.OwnerID != newOwner.ID) {
		t.Fatalf("acceptance race final state = %#v, owner=%q", finalTransfer, current.OwnerID)
	}
	if cancelled.accepted && (finalTransfer.State != workspace.OwnershipTransferCancelled || current.OwnerID != oldOwner.ID) {
		t.Fatalf("cancellation race final state = %#v, owner=%q", finalTransfer, current.OwnerID)
	}
}

func TestPostgresWorkspaceOwnershipTransferQuotaFailureResetsToPending(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	oldOwner := createTestUser(t, store, 8, 20_000, 40_000)
	newOwner := createTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTransferReadyWorkspace(t, ctx, store, oldOwner.ID, "transfer-quota")
	otherWorkspace := createTestWorkspace(t, ctx, store, newOwner.ID, "target-storage")
	seedWorkspaceOwnerPhysicalUsage(t, ctx, store, oldOwner.ID, workspaceValue.ID, 2)
	seedWorkspaceOwnerPhysicalUsage(t, ctx, store, newOwner.ID, otherWorkspace.ID, 9)
	service := newTestWorkspaceOwnershipTransferService(t, store, 10)
	created, _, err := service.RequestWorkspaceTransfer(ctx, workspaceTransferPrincipal(oldOwner.ID), workspaceValue.ID, newOwner.ID, 0, "quota-create-01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcceptWorkspaceTransfer(ctx, workspaceTransferPrincipal(newOwner.ID), workspaceValue.ID, created.ID, "quota-accept-01"); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("acceptance over quota error = %v, want quota exceeded", err)
	}
	var currentOwner string
	if err := store.pool.QueryRow(ctx, `SELECT owner_id FROM workspaces WHERE id=$1`, workspaceValue.ID).Scan(&currentOwner); err != nil || currentOwner != oldOwner.ID {
		t.Fatalf("Workspace owner after rejected quota transfer = %q, %v", currentOwner, err)
	}
	transfer, err := store.GetWorkspaceOwnershipTransfer(ctx, oldOwner.ID, workspaceValue.ID, created.ID)
	if err != nil || transfer.State != workspace.OwnershipTransferPending || transfer.OperationID != "" {
		t.Fatalf("transfer after quota rejection = %#v, %v; want pending without operation ID", transfer, err)
	}
	if _, err := service.CancelWorkspaceTransfer(ctx, workspaceTransferPrincipal(oldOwner.ID), workspaceValue.ID, created.ID, "quota-cancel-01"); err != nil {
		t.Fatalf("cancel transfer after quota rejection: %v", err)
	}
}

func TestPostgresWorkspaceOwnershipTransferQuotaFailureCanRetryWithSameKey(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	oldOwner := createTestUser(t, store, 8, 20_000, 40_000)
	newOwner := createTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTransferReadyWorkspace(t, ctx, store, oldOwner.ID, "transfer-quota-retry")
	otherWorkspace := createTestWorkspace(t, ctx, store, newOwner.ID, "target-storage-retry")
	seedWorkspaceOwnerPhysicalUsage(t, ctx, store, oldOwner.ID, workspaceValue.ID, 2)
	seedWorkspaceOwnerPhysicalUsage(t, ctx, store, newOwner.ID, otherWorkspace.ID, 9)
	service := newTestWorkspaceOwnershipTransferService(t, store, 10)
	created, _, err := service.RequestWorkspaceTransfer(ctx, workspaceTransferPrincipal(oldOwner.ID), workspaceValue.ID, newOwner.ID, 0, "quota-retry-create-01")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcceptWorkspaceTransfer(ctx, workspaceTransferPrincipal(newOwner.ID), workspaceValue.ID, created.ID, "quota-retry-accept-01"); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("first acceptance over quota error = %v, want quota exceeded", err)
	}
	digest := sha256.Sum256([]byte("physical-usage:" + otherWorkspace.ID))
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_block_usage SET allocated_bytes=8
WHERE workspace_id=$1 AND sha256=$2`, otherWorkspace.ID, hex.EncodeToString(digest[:])); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_owner_block_usage SET allocated_bytes=8 WHERE owner_id=$1`, newOwner.ID); err != nil {
		t.Fatal(err)
	}
	accepted, err := service.AcceptWorkspaceTransfer(ctx, workspaceTransferPrincipal(newOwner.ID), workspaceValue.ID, created.ID, "quota-retry-accept-01")
	if err != nil || accepted.State != workspace.OwnershipTransferAccepted {
		t.Fatalf("retry acceptance with the same key = %#v, %v", accepted, err)
	}
	var ownerID string
	if err := store.pool.QueryRow(ctx, `SELECT owner_id FROM workspaces WHERE id=$1`, workspaceValue.ID).Scan(&ownerID); err != nil || ownerID != newOwner.ID {
		t.Fatalf("Workspace owner after same-key retry = %q, %v", ownerID, err)
	}
}

func TestPostgresWorkspaceOwnershipTransferUnknownResultReconcilesAndRetries(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	oldOwner := createTestUser(t, store, 8, 20_000, 40_000)
	newOwner := createTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTransferReadyWorkspace(t, ctx, store, oldOwner.ID, "transfer-unknown")
	service := newTestWorkspaceOwnershipTransferService(t, store, 100)
	created, _, err := service.RequestWorkspaceTransfer(ctx, workspaceTransferPrincipal(oldOwner.ID), workspaceValue.ID, newOwner.ID, 0, "unknown-create-01")
	if err != nil {
		t.Fatal(err)
	}
	operationID := mustID(t, domain.NewOperationID)
	startRecord := app.IdempotencyRecord{
		OwnerID: newOwner.ID, Operation: "workspace.transfer.accept", Key: "unknown-accept-01",
		RequestHash: hashBytes("unknown accept"), ResourceID: created.ID,
	}
	accepting, _, err := store.StartWorkspaceOwnershipTransferAcceptance(ctx, newOwner.ID, workspaceValue.ID, created.ID, operationID, startRecord)
	if err != nil || accepting.State != workspace.OwnershipTransferAccepting || accepting.OperationID != operationID {
		t.Fatalf("start acceptance = %#v, %v", accepting, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_ownership_transfers SET state='unknown', version=version+1, updated_at=clock_timestamp()
WHERE id=$1 AND state='accepting' AND operation_id=$2`, created.ID, operationID); err != nil {
		t.Fatal(err)
	}
	accepted, err := service.AcceptWorkspaceTransfer(ctx, workspaceTransferPrincipal(newOwner.ID), workspaceValue.ID, created.ID, "unknown-accept-02")
	if err != nil || accepted.State != workspace.OwnershipTransferAccepted {
		t.Fatalf("safe retry after unknown acceptance = %#v, %v", accepted, err)
	}
	current, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil || current.OwnerID != newOwner.ID {
		t.Fatalf("Workspace owner after unknown acceptance retry = %q, %v", current.OwnerID, err)
	}
	transfer, err := store.GetWorkspaceOwnershipTransfer(ctx, newOwner.ID, workspaceValue.ID, created.ID)
	if err != nil || transfer.State != workspace.OwnershipTransferAccepted {
		t.Fatalf("transfer after unknown acceptance retry = %#v, %v", transfer, err)
	}
}

func TestPostgresWorkspaceOwnershipTransferUnknownMismatchedOwnerRemainsFrozen(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	oldOwner := createTestUser(t, store, 8, 20_000, 40_000)
	newOwner := createTestUser(t, store, 8, 20_000, 40_000)
	unexpectedOwner := createTestUser(t, store, 8, 20_000, 40_000)
	workspaceValue := createTransferReadyWorkspace(t, ctx, store, oldOwner.ID, "transfer-unknown-mismatch")
	service := newTestWorkspaceOwnershipTransferService(t, store, 100)
	created, _, err := service.RequestWorkspaceTransfer(ctx, workspaceTransferPrincipal(oldOwner.ID), workspaceValue.ID, newOwner.ID, 0, "unknown-mismatch-create-01")
	if err != nil {
		t.Fatal(err)
	}
	operationID := mustID(t, domain.NewOperationID)
	started, _, err := store.StartWorkspaceOwnershipTransferAcceptance(ctx, newOwner.ID, workspaceValue.ID, created.ID, operationID,
		transferRecord(newOwner.ID, "workspace.transfer.accept", "unknown-mismatch-accept-01", created.ID))
	if err != nil || started.State != workspace.OwnershipTransferAccepting {
		t.Fatalf("start acceptance = %#v, %v", started, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_ownership_transfers SET state='unknown', version=version+1, updated_at=clock_timestamp()
WHERE id=$1 AND state='accepting' AND operation_id=$2`, created.ID, operationID); err != nil {
		t.Fatal(err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE workspaces SET owner_id=$2 WHERE id=$1`, workspaceValue.ID, unexpectedOwner.ID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE workspace_volumes SET owner_id=$2 WHERE workspace_id=$1`, workspaceValue.ID, unexpectedOwner.ID); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AcceptWorkspaceTransfer(ctx, workspaceTransferPrincipal(newOwner.ID), workspaceValue.ID, created.ID, "unknown-mismatch-accept-02"); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("acceptance with mismatched owner tuple = %v, want frozen busy result", err)
	}
	if _, err := service.CancelWorkspaceTransfer(ctx, workspaceTransferPrincipal(oldOwner.ID), workspaceValue.ID, created.ID, "unknown-mismatch-cancel-01"); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("cancellation with mismatched owner tuple = %v, want frozen busy result", err)
	}
	transfer, err := store.GetWorkspaceOwnershipTransfer(ctx, newOwner.ID, workspaceValue.ID, created.ID)
	if err != nil || transfer.State != workspace.OwnershipTransferUnknown || transfer.OperationID != operationID {
		t.Fatalf("mismatched transfer after reconciliation attempt = %#v, %v", transfer, err)
	}
}

func TestPostgresWorkspaceOwnershipTransferRefusesBusyAndUnsupportedReferences(t *testing.T) {
	t.Run("active binding", func(t *testing.T) {
		store, _ := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 8, 20_000, 40_000)
		other := createTestUser(t, store, 8, 20_000, 40_000)
		hostValue := createRunningControlHost(t, ctx, store, owner.ID, host.ControlAI, 0)
		workspaceValue := hostValue.Workspaces[0].Workspace
		prepareTransferWorkspace(t, ctx, store, workspaceValue.ID, owner.ID, 5)
		binding, err := store.GetWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.TransitionWorkspaceBinding(ctx, hostValue.ID, workspaceValue.ID, binding.BindingVersion,
			workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, workspaceValue.Revision); err != nil {
			t.Fatalf("activate binding for transfer refusal: %v", err)
		}
		id := mustID(t, workspace.NewOwnershipTransferID)
		_, _, err = store.CreateWorkspaceOwnershipTransfer(ctx, owner.ID, workspaceValue.ID, other.ID, 0, id, transferRecord(owner.ID, "workspace.transfer.create", "busy-binding-01", id))
		if domain.CodeOf(err) != domain.CodeWorkspaceBusy {
			t.Fatalf("request with active binding error = %v, want busy", err)
		}
	})

	t.Run("orphan sync folder is safely removed during transfer", func(t *testing.T) {
		store, _ := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 8, 20_000, 40_000)
		other := createTestUser(t, store, 8, 20_000, 40_000)
		workspaceValue := createTransferReadyWorkspace(t, ctx, store, owner.ID, "transfer-device")
		folderID := "morde-" + strings.Repeat("a", 32)
		if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_sync_folders(workspace_id,owner_id,folder_id,state)
VALUES ($1,$2,$3,'revoked')`, workspaceValue.ID, owner.ID, folderID); err != nil {
			t.Fatal(err)
		}
		service := newTestWorkspaceOwnershipTransferService(t, store, 100)
		created, replayed, err := service.RequestWorkspaceTransfer(ctx, workspaceTransferPrincipal(owner.ID), workspaceValue.ID,
			other.ID, workspaceValue.Revision, "orphan-device-folder-create")
		if err != nil || replayed {
			t.Fatalf("request transfer with orphaned device folder = %#v replayed=%t err=%v", created, replayed, err)
		}
		accepted, err := service.AcceptWorkspaceTransfer(ctx, workspaceTransferPrincipal(other.ID), workspaceValue.ID,
			created.ID, "orphan-device-folder-accept")
		if err != nil || accepted.State != workspace.OwnershipTransferAccepted {
			t.Fatalf("accept transfer with orphaned device folder = %#v err=%v", accepted, err)
		}
		current, err := store.GetWorkspace(ctx, workspaceValue.ID)
		if err != nil || current.OwnerID != other.ID {
			t.Fatalf("Workspace owner after orphan-folder transfer = %q, %v", current.OwnerID, err)
		}
		var folders int
		if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_folders WHERE workspace_id=$1`, workspaceValue.ID).Scan(&folders); err != nil || folders != 0 {
			t.Fatalf("orphaned device folder after transfer = %d rows, want 0: %v", folders, err)
		}
		var desiredRevision, appliedRevision int64
		if err := store.pool.QueryRow(ctx, `SELECT desired_revision,applied_revision FROM workspace_sync_node_revisions WHERE owner_id=$1`, owner.ID).
			Scan(&desiredRevision, &appliedRevision); err != nil || desiredRevision != 1 || appliedRevision != 0 {
			t.Fatalf("former-owner node revision after orphan-folder transfer = %d/%d, want 1/0: %v", desiredRevision, appliedRevision, err)
		}
		var desired workspace.SyncNodeDesiredState
		if err := store.ReconcileWorkspaceSyncNode(ctx, owner.ID, func(_ context.Context, value workspace.SyncNodeDesiredState) error {
			desired = value
			return nil
		}); err != nil {
			t.Fatalf("reconcile former owner after orphan-folder transfer: %v", err)
		}
		if len(desired.Folders) != 0 || desired.Revision != desiredRevision {
			t.Fatalf("former-owner desired state after transfer = %#v, want empty folders at revision %d", desired, desiredRevision)
		}
	})

	t.Run("active sync session", func(t *testing.T) {
		store, _ := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 8, 20_000, 40_000)
		other := createTestUser(t, store, 8, 20_000, 40_000)
		workspaceValue := createTransferReadyWorkspace(t, ctx, store, owner.ID, "transfer-sync")
		sessionID := mustID(t, workspace.NewSyncSessionID)
		if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_sync_sessions
(id,workspace_id,owner_id,replica_id,direction,policy,state,stage,base_revision,target_revision,pinned_revision,
 planned_bytes,confirmed_bytes,files,upload_files,download_files)
VALUES ($1,$2,$3,$4,'download','fill_missing','transferring','transfer',0,0,0,0,0,'[]'::jsonb,'[]'::jsonb,'[]'::jsonb)`,
			sessionID, workspaceValue.ID, owner.ID, "session:"+owner.ID); err != nil {
			t.Fatal(err)
		}
		id := mustID(t, workspace.NewOwnershipTransferID)
		_, _, err := store.CreateWorkspaceOwnershipTransfer(ctx, owner.ID, workspaceValue.ID, other.ID, 0, id,
			transferRecord(owner.ID, "workspace.transfer.create", "busy-session-01", id))
		if domain.CodeOf(err) != domain.CodeWorkspaceBusy {
			t.Fatalf("request with active sync session error = %v, want busy", err)
		}
	})

	t.Run("legacy volume history", func(t *testing.T) {
		store, _ := newTemporaryStore(t)
		ctx := context.Background()
		owner := createTestUser(t, store, 8, 20_000, 40_000)
		other := createTestUser(t, store, 8, 20_000, 40_000)
		workspaceValue := createTransferReadyWorkspace(t, ctx, store, owner.ID, "transfer-legacy")
		if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_legacy_volumes(workspace_id,volume_name,import_state)
VALUES ($1,$2,'verified')`, workspaceValue.ID, "legacy-"+workspaceValue.ID); err != nil {
			t.Fatal(err)
		}
		id := mustID(t, workspace.NewOwnershipTransferID)
		_, _, err := store.CreateWorkspaceOwnershipTransfer(ctx, owner.ID, workspaceValue.ID, other.ID, 0, id,
			transferRecord(owner.ID, "workspace.transfer.create", "unsupported-legacy-01", id))
		if domain.CodeOf(err) != domain.CodeUnsupported {
			t.Fatalf("request with legacy volume history error = %v, want unsupported", err)
		}
	})
}

func newTestWorkspaceOwnershipTransferService(t *testing.T, store *Store, maxOwnerBytes int64) *app.WorkspaceTransferService {
	t.Helper()
	service, err := app.NewWorkspaceTransferService(store, maxOwnerBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetWorkspaceOwnershipTransferProjection(app.WorkspaceOwnershipTransferProjection{
		PrepareRecipient: func(context.Context, workspace.OwnershipTransfer) error { return nil },
		DiscardRecipient: func(context.Context, workspace.OwnershipTransfer) error { return nil },
		RetireFormer:     func(context.Context, workspace.OwnershipTransfer) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	return service
}

type workspaceTransferTestBlocks struct{}

func (workspaceTransferTestBlocks) Open(string, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (workspaceTransferTestBlocks) PutBlock(string, string, io.Reader) (int64, error) {
	return 0, errors.New("test blocks are read-only")
}

func (workspaceTransferTestBlocks) ReservationBytes(size int64) (int64, error)        { return size, nil }
func (workspaceTransferTestBlocks) BlockAllocatedBytes(string, string) (int64, error) { return 0, nil }

type workspaceTransferProjectionBlockStore map[string][]byte

func (b workspaceTransferProjectionBlockStore) Open(_ string, digest string) (io.ReadCloser, error) {
	content, ok := b[digest]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (workspaceTransferProjectionBlockStore) PutBlock(string, string, io.Reader) (int64, error) {
	return 0, errors.New("projection test blocks are read-only")
}

func (workspaceTransferProjectionBlockStore) ReservationBytes(size int64) (int64, error) {
	return size, nil
}

func (workspaceTransferProjectionBlockStore) BlockAllocatedBytes(string, string) (int64, error) {
	return 0, nil
}

func testWorkspaceSyncService(t *testing.T, store *Store) *app.WorkspaceSyncService {
	t.Helper()
	service, err := app.NewWorkspaceSyncService(store, workspaceTransferTestBlocks{}, workspace.MaxSyncManifestBytes, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func workspaceTransferPrincipal(ownerID string) identity.Principal {
	return identity.Principal{UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t" + ownerID[1:]}
}

func createTransferReadyWorkspace(t *testing.T, ctx context.Context, store *Store, ownerID, name string) workspace.Workspace {
	t.Helper()
	value := createTestWorkspace(t, ctx, store, ownerID, name)
	prepareTransferWorkspace(t, ctx, store, value.ID, ownerID, 0)
	return value
}

func prepareTransferWorkspace(t *testing.T, ctx context.Context, store *Store, workspaceID, ownerID string, fileBytes int64) {
	t.Helper()
	if err := store.MarkWorkspaceVolumeReady(ctx, ownerID, workspaceID, app.WorkspaceNamedVolumeName(workspaceID)); err != nil {
		t.Fatalf("mark transfer test Workspace volume ready: %v", err)
	}
	if fileBytes > 0 {
		file := []byte("hello")
		digest := sha256.Sum256(file)
		chunks, err := json.Marshal([]workspace.Chunk{{SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(file))}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revision_files
(workspace_id,valid_from_revision,file_path,folded_path,size_bytes,sha256,mode,chunks,metadata_known)
VALUES ($1,0,'README.md','readme.md',$2,$3,420,$4::jsonb,true)`, workspaceID, fileBytes, hex.EncodeToString(digest[:]), chunks); err != nil {
			t.Fatalf("seed transfer test Workspace manifest: %v", err)
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_revision_directories
(workspace_id,valid_from_revision,directory_path,folded_path,metadata_known)
VALUES ($1,0,'.','.',true)`, workspaceID); err != nil {
			t.Fatalf("seed transfer test Workspace root metadata: %v", err)
		}
		if _, err := store.pool.Exec(ctx, `UPDATE workspace_revisions SET file_count=1,total_bytes=$2 WHERE workspace_id=$1 AND revision=0`, workspaceID, fileBytes); err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET used_bytes=$2 WHERE id=$1`, workspaceID, fileBytes); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_current_materializations
SET desired_revision=0, applied_revision=0, updated_at=clock_timestamp() WHERE workspace_id=$1`, workspaceID); err != nil {
		t.Fatalf("mark transfer test Workspace tree materialized: %v", err)
	}
}

func seedWorkspaceOwnerPhysicalUsage(t *testing.T, ctx context.Context, store *Store, ownerID, workspaceID string, allocatedBytes int64) {
	t.Helper()
	digest := sha256.Sum256([]byte("physical-usage:" + workspaceID))
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_block_usage(workspace_id,sha256,state,allocated_bytes)
VALUES ($1,$2,'stored',$3)`, workspaceID, hex.EncodeToString(digest[:]), allocatedBytes); err != nil {
		t.Fatalf("seed test Workspace physical usage: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO workspace_owner_block_usage(owner_id,allocated_bytes,reserved_bytes)
VALUES ($1,$2,0) ON CONFLICT(owner_id) DO UPDATE SET allocated_bytes=workspace_owner_block_usage.allocated_bytes+EXCLUDED.allocated_bytes`,
		ownerID, allocatedBytes); err != nil {
		t.Fatalf("seed test owner physical usage: %v", err)
	}
}

func transferRecord(ownerID, operation, key, resourceID string) app.IdempotencyRecord {
	return app.IdempotencyRecord{OwnerID: ownerID, Operation: operation, Key: key,
		RequestHash: hashBytes(operation + ":" + resourceID + ":" + key), ResourceID: resourceID}
}
