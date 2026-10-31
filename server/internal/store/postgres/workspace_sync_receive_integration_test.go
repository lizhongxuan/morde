package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func TestWorkspaceSyncReceiveReservationSettlesIdempotently(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-receive-settlement")
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, value.ID, value.Revision); err != nil {
		t.Fatal(err)
	}

	request := receiveTestRequest(owner.ID, value.ID, "src/config.json", 5, "first-write")
	reservation := reserveTestWorkspaceReceive(t, ctx, store, request)
	if reservation.State != "reserved" || reservation.BaseRevision != 0 || reservation.BaseFileSize != nil || reservation.BaseFileSHA256 != nil {
		t.Fatalf("receive reservation = %#v", reservation)
	}
	owners, err := store.ListActiveWorkspaceSyncReceiveOwners(ctx)
	if err != nil || len(owners) != 1 || owners[0] != owner.ID {
		t.Fatalf("active receive owners = %#v, err=%v", owners, err)
	}
	if err := store.BeginWorkspaceCurrentMaterialization(ctx, value.ID, 0, ""); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("materialization during active receive = %v; want workspace busy", err)
	}

	file := syncTestFile(request.Path, []byte("hello"))
	blockReservation := app.WorkspaceBlockReservation{
		WorkspaceID: value.ID, Digest: file.Chunks[0].SHA256, ReservedBytes: file.Chunks[0].SizeBytes, SizeBytes: file.Chunks[0].SizeBytes,
	}
	if err := store.ReserveWorkspaceSyncReceiveBlockUsage(ctx, owner.ID, value.ID, reservation.ID,
		[]app.WorkspaceBlockReservation{blockReservation}, syncTestOwnerQuotaBytes); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeWorkspaceBlockUsage(ctx, value.ID, file.Chunks[0].SHA256, file.Chunks[0].SizeBytes, syncTestOwnerQuotaBytes); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.CommitWorkspaceSyncReceive(ctx, owner.ID, value.ID, reservation.ID, &file, nil)
	if err != nil || receipt.State != "settled" || receipt.Revision != 1 || receipt.SHA256 != file.SHA256 {
		t.Fatalf("settle receive = %#v, err=%v", receipt, err)
	}
	stored, err := store.GetWorkspaceSyncReceiveReservation(ctx, owner.ID, reservation.ID)
	if err != nil || stored.State != "settled" || stored.ResultRevision == nil || *stored.ResultRevision != 1 ||
		stored.ResultSHA256 == nil || *stored.ResultSHA256 != file.SHA256 {
		t.Fatalf("stored receive result = %#v, err=%v", stored, err)
	}
	var associationCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM workspace_sync_receive_blocks WHERE reservation_id=$1`, reservation.ID).Scan(&associationCount); err != nil || associationCount != 0 {
		t.Fatalf("receive block associations after settlement = %d, err=%v; want zero", associationCount, err)
	}
	owners, err = store.ListActiveWorkspaceSyncReceiveOwners(ctx)
	if err != nil || len(owners) != 0 {
		t.Fatalf("active owners after settlement = %#v, err=%v", owners, err)
	}
	replayed, err := store.CommitWorkspaceSyncReceive(ctx, owner.ID, value.ID, reservation.ID, &file, nil)
	if err != nil || replayed != receipt {
		t.Fatalf("replay receive settlement = %#v, err=%v; want %#v", replayed, err, receipt)
	}
	retriedReservation := reserveTestWorkspaceReceive(t, ctx, store, request)
	if retriedReservation.ID != reservation.ID || retriedReservation.State != "settled" {
		t.Fatalf("replay receive reservation = %#v; want original settled reservation", retriedReservation)
	}
	changedRequest := request
	changedRequest.TargetBytes++
	if _, err := reserveTestWorkspaceReceiveErr(t, ctx, store, changedRequest); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("reuse receive operation ID with different request = %v; want idempotency conflict", err)
	}
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, value.ID, 1); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceSyncReceiveSerializesCompetingReservations(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-receive-race")
	authorizeTestWorkspaceSyncReceive(t, ctx, store, owner.ID, value.ID)
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, value.ID, 0); err != nil {
		t.Fatal(err)
	}
	requests := []app.WorkspaceSyncReceiveRequest{
		receiveTestRequest(owner.ID, value.ID, "a.txt", 1, "race-a"),
		receiveTestRequest(owner.ID, value.ID, "b.txt", 1, "race-b"),
	}
	start := make(chan struct{})
	type result struct {
		reservation app.WorkspaceSyncReceiveReservation
		err         error
	}
	results := make(chan result, len(requests))
	var wg sync.WaitGroup
	for _, request := range requests {
		request := request
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			id, err := domain.NewPendingRequestID()
			if err != nil {
				results <- result{err: err}
				return
			}
			hash := receiveTestRequestHash(request)
			reservation, err := store.ReserveWorkspaceSyncReceive(ctx, request, id, hash[:])
			results <- result{reservation: reservation, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	succeeded, busy := 0, 0
	for item := range results {
		if item.err == nil && item.reservation.State == "reserved" {
			succeeded++
		} else if domain.CodeOf(item.err) == domain.CodeWorkspaceBusy {
			busy++
		} else {
			t.Fatalf("competing receive reservation = %#v, err=%v", item.reservation, item.err)
		}
	}
	if succeeded != 1 || busy != 1 {
		t.Fatalf("competing receive outcomes: succeeded=%d busy=%d; want one each", succeeded, busy)
	}
}

func TestWorkspaceSyncReceiveRequiresBoundFolderID(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-receive-folder-binding")
	binding := registerTestWorkspaceSyncDevice(t, store, syncDeviceTestPrincipal(t, store, owner.ID), value.ID,
		"BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB", "receive-folder-binding")
	applyWorkspaceSyncNodeState(t, store, owner.ID)
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, value.ID, 0); err != nil {
		t.Fatal(err)
	}

	request := receiveTestRequest(owner.ID, value.ID, "bound.txt", 1, "folder-binding")
	request.FolderID = "morde-" + strings.Repeat("c", 32)
	if _, err := reserveTestWorkspaceReceiveErr(t, ctx, store, request); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("receive using another folder ID = %v; want forbidden", err)
	}
	request.FolderID = binding.FolderID
	if reservation := reserveTestWorkspaceReceive(t, ctx, store, request); reservation.State != "reserved" {
		t.Fatalf("receive using the bound folder ID = %#v", reservation)
	}
}

func TestWorkspaceSyncReceiveReservesExactProjectedQuota(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	within := createTestWorkspace(t, ctx, store, owner.ID, "sync-receive-quota-exact")
	over := createTestWorkspace(t, ctx, store, owner.ID, "sync-receive-quota-over")
	for _, value := range []workspace.Workspace{within, over} {
		if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET quota_bytes=10 WHERE id=$1`, value.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkWorkspaceCurrentMaterialized(ctx, value.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	reserveTestWorkspaceReceive(t, ctx, store, receiveTestRequest(owner.ID, within.ID, "exact.txt", 10, "quota-exact"))
	if _, err := reserveTestWorkspaceReceiveErr(t, ctx, store, receiveTestRequest(owner.ID, over.ID, "over.txt", 11, "quota-over")); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("reserve above Workspace quota = %v; want quota exceeded", err)
	}
}

func TestWorkspaceSyncReceiveCommitsDirectoryRevision(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-receive-directory")
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, value.ID, 0); err != nil {
		t.Fatal(err)
	}
	request := receiveTestRequest(owner.ID, value.ID, "reports/archive", 0, "directory-upsert")
	request.Operation = app.WorkspaceSyncReceiveDirectoryUpsert
	reservation := reserveTestWorkspaceReceive(t, ctx, store, request)
	directory := request.Path
	receipt, err := store.CommitWorkspaceSyncReceive(ctx, owner.ID, value.ID, reservation.ID, nil, &directory)
	if err != nil || receipt.State != "settled" || receipt.Revision != 1 {
		t.Fatalf("settle directory receive = %#v, err=%v", receipt, err)
	}
	manifest, err := store.GetWorkspaceManifestPage(ctx, value.ID, 1, "", "", 10)
	if err != nil || len(manifest.EmptyDirectories) != 1 || manifest.EmptyDirectories[0] != directory {
		t.Fatalf("manifest after directory receive = %#v, err=%v", manifest, err)
	}
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, value.ID, 1); err != nil {
		t.Fatal(err)
	}
	repeatedRequest := receiveTestRequest(owner.ID, value.ID, directory, 0, "directory-upsert-repeat")
	repeatedRequest.Operation = app.WorkspaceSyncReceiveDirectoryUpsert
	repeatedReservation := reserveTestWorkspaceReceive(t, ctx, store, repeatedRequest)
	repeated, err := store.CommitWorkspaceSyncReceive(ctx, owner.ID, value.ID, repeatedReservation.ID, nil, &directory)
	if err != nil || repeated.State != "settled" || repeated.Revision != 1 {
		t.Fatalf("settle unchanged directory receive = %#v, err=%v; want revision 1", repeated, err)
	}
	deleteRequest := receiveTestRequest(owner.ID, value.ID, directory, 0, "directory-delete")
	deleteRequest.Operation = app.WorkspaceSyncReceiveDirectoryDelete
	deleteReservation := reserveTestWorkspaceReceive(t, ctx, store, deleteRequest)
	if deleteReservation.BaseDirectoryPath == nil || *deleteReservation.BaseDirectoryPath != directory {
		t.Fatalf("directory delete base = %#v, want %q", deleteReservation.BaseDirectoryPath, directory)
	}
	deleted, err := store.CommitWorkspaceSyncReceive(ctx, owner.ID, value.ID, deleteReservation.ID, nil, &directory)
	if err != nil || deleted.State != "settled" || deleted.Revision != 2 {
		t.Fatalf("settle directory deletion = %#v, err=%v", deleted, err)
	}
	manifest, err = store.GetWorkspaceManifestPage(ctx, value.ID, 2, "", "", 10)
	if err != nil || len(manifest.EmptyDirectories) != 0 {
		t.Fatalf("manifest after directory deletion = %#v, err=%v", manifest, err)
	}
}

func TestWorkspaceSyncReceiveAllowsOnlyQuotaReducingOverage(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-receive-quota-shrink")
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET quota_bytes=10 WHERE id=$1`, value.ID); err != nil {
		t.Fatal(err)
	}
	initialFile := syncTestFile("data.txt", []byte("0123456789"))
	if err := seedTestStoredWorkspaceBlockUsage(ctx, store, value.ID, initialFile.Chunks[0]); err != nil {
		t.Fatal(err)
	}
	session := newTestSyncSession(t, value, owner.ID, []workspace.File{initialFile}, []workspace.File{initialFile}, nil)
	session.PlannedBytes = 0
	created, replayed, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks(session.UploadFiles),
		syncTestIdempotency(owner.ID, "workspace.sync.create", "receive-quota-initial", session.ID))
	if err != nil || replayed {
		t.Fatalf("create initial Workspace manifest = %#v, replayed=%t, err=%v", created, replayed, err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID,
		syncTestIdempotency(owner.ID, "workspace.sync.commit", "receive-quota-initial", session.ID)); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, value.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET quota_bytes=5 WHERE id=$1`, value.ID); err != nil {
		t.Fatal(err)
	}
	request := receiveTestRequest(owner.ID, value.ID, initialFile.Path, 8, "quota-shrink")
	reservation := reserveTestWorkspaceReceive(t, ctx, store, request)
	shrunkFile := syncTestFile(initialFile.Path, []byte("abcdefgh"))
	if err := seedTestStoredWorkspaceBlockUsage(ctx, store, value.ID, shrunkFile.Chunks[0]); err != nil {
		t.Fatal(err)
	}
	receipt, err := store.CommitWorkspaceSyncReceive(ctx, owner.ID, value.ID, reservation.ID, &shrunkFile, nil)
	if err != nil || receipt.Revision != 2 {
		t.Fatalf("commit quota-reducing overage = %#v, err=%v", receipt, err)
	}
	updated, err := store.GetWorkspace(ctx, value.ID)
	if err != nil || updated.UsedBytes != 8 || updated.QuotaBytes != 5 || updated.Revision != 2 {
		t.Fatalf("Workspace after allowed overage shrink = %#v, err=%v", updated, err)
	}
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, value.ID, 2); err != nil {
		t.Fatal(err)
	}
	deleteRequest := receiveTestDeleteRequest(owner.ID, value.ID, initialFile.Path, "quota-delete")
	deleteReservation := reserveTestWorkspaceReceive(t, ctx, store, deleteRequest)
	deleted, err := store.CommitWorkspaceSyncReceive(ctx, owner.ID, value.ID, deleteReservation.ID, nil, nil)
	if err != nil || deleted.Revision != 3 || deleted.SHA256 != "" {
		t.Fatalf("delete over-quota Workspace file = %#v, err=%v", deleted, err)
	}
	updated, err = store.GetWorkspace(ctx, value.ID)
	if err != nil || updated.UsedBytes != 0 || updated.Revision != 3 {
		t.Fatalf("Workspace after direct delete = %#v, err=%v", updated, err)
	}
}

func TestWorkspaceSyncReceiveBlocksSyncCommitAndTaskPublish(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-receive-sync-guard")
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, value.ID, 0); err != nil {
		t.Fatal(err)
	}
	session := newTestSyncSession(t, value, owner.ID, []workspace.File{}, []workspace.File{}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, []workspace.TransferBlock{},
		syncTestIdempotency(owner.ID, "workspace.sync.create", "receive-sync-guard", session.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := reserveTestWorkspaceReceiveErr(t, ctx, store, receiveTestRequest(owner.ID, value.ID, "active.txt", 1, "active-session-guard")); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("reserve direct receive during ordinary sync session = %v; want workspace busy", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_sync_sessions SET state='completed', stage='complete' WHERE workspace_id=$1 AND id=$2`, value.ID, session.ID); err != nil {
		t.Fatal(err)
	}
	reservation := reserveTestWorkspaceReceive(t, ctx, store, receiveTestRequest(owner.ID, value.ID, "a.txt", 1, "sync-guard"))
	blockDigest := strings.Repeat("a", 64)
	blockReservation := app.WorkspaceBlockReservation{WorkspaceID: value.ID, Digest: blockDigest, ReservedBytes: 4, SizeBytes: 3}
	if err := store.ReserveWorkspaceSyncReceiveBlockUsage(ctx, owner.ID, value.ID, reservation.ID,
		[]app.WorkspaceBlockReservation{blockReservation}, syncTestOwnerQuotaBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveWorkspaceSyncCommit(ctx, value.ID, session.ID,
		syncTestIdempotency(owner.ID, "workspace.sync.commit", "receive-sync-guard", session.ID)); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("reserve Workspace sync commit during direct receive = %v; want workspace busy", err)
	}
	if _, err := store.BeginAbortWorkspaceSyncReceive(ctx, owner.ID, value.ID, reservation.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginWorkspaceCurrentMaterialization(ctx, value.ID, 0, reservation.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkWorkspaceCurrentMaterializedForReceive(ctx, value.ID, 0, reservation.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAbortWorkspaceSyncReceive(ctx, owner.ID, value.ID, reservation.ID); err != nil {
		t.Fatal(err)
	}
	var blockExists bool
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_block_usage WHERE workspace_id=$1 AND sha256=$2)`, value.ID, blockDigest).Scan(&blockExists); err != nil || blockExists {
		t.Fatalf("aborted receive reserved block still exists=%t, err=%v", blockExists, err)
	}
	var ownerReserved int64
	if err := store.pool.QueryRow(ctx, `SELECT reserved_bytes FROM workspace_owner_block_usage WHERE owner_id=$1`, owner.ID).Scan(&ownerReserved); err != nil || ownerReserved != 0 {
		t.Fatalf("owner reserved block bytes after abort = %d, err=%v; want zero", ownerReserved, err)
	}
}

func TestWorkspaceSyncReceiveBlocksTaskPublishReservation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	_, value, _, _, command := newTaskWorkspacePublishTask(t, ctx, store, "sync-receive-task-guard")
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, value.ID, 0); err != nil {
		t.Fatal(err)
	}
	reserveTestWorkspaceReceive(t, ctx, store, receiveTestRequest(command.OwnerID, value.ID, "a.txt", 1, "task-guard"))
	if _, err := store.ReserveTaskWorkspacePublish(ctx, command); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("reserve Task publish during direct receive = %v; want workspace busy", err)
	}
}

func reserveTestWorkspaceReceive(t *testing.T, ctx context.Context, store *Store, request app.WorkspaceSyncReceiveRequest) app.WorkspaceSyncReceiveReservation {
	t.Helper()
	reservation, err := reserveTestWorkspaceReceiveErr(t, ctx, store, request)
	if err != nil {
		t.Fatalf("reserve Workspace receive: %v", err)
	}
	return reservation
}

func reserveTestWorkspaceReceiveErr(t *testing.T, ctx context.Context, store *Store, request app.WorkspaceSyncReceiveRequest) (app.WorkspaceSyncReceiveReservation, error) {
	authorizeTestWorkspaceSyncReceive(t, ctx, store, request.OwnerID, request.WorkspaceID)
	id, err := domain.NewPendingRequestID()
	if err != nil {
		return app.WorkspaceSyncReceiveReservation{}, err
	}
	hash := receiveTestRequestHash(request)
	return store.ReserveWorkspaceSyncReceive(ctx, request, id, hash[:])
}

func authorizeTestWorkspaceSyncReceive(t *testing.T, ctx context.Context, store *Store, ownerID, workspaceID string) {
	t.Helper()
	var active bool
	if err := store.pool.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_sync_device_bindings b
JOIN client_installations i ON i.id=b.client_installation_id AND i.owner_id=b.owner_id
JOIN workspace_sync_folders f ON f.workspace_id=b.workspace_id AND f.owner_id=b.owner_id
WHERE b.owner_id=$1 AND b.workspace_id=$2 AND b.state='active' AND f.state='active'
  AND i.revoked_at IS NULL AND i.expires_at>clock_timestamp()
)`, ownerID, workspaceID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active {
		return
	}
	principal := syncDeviceTestPrincipal(t, store, ownerID)
	registerTestWorkspaceSyncDevice(t, store, principal, workspaceID,
		"AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA", "receive-test-"+workspaceID)
	applyWorkspaceSyncNodeState(t, store, ownerID)
}

func receiveTestRequest(ownerID, workspaceID, path string, targetBytes int64, key string) app.WorkspaceSyncReceiveRequest {
	operation := sha256.Sum256([]byte("workspace-sync-receive:" + key))
	folderHash := sha256.Sum256([]byte(workspaceID + "\x00receive-test-" + workspaceID))
	return app.WorkspaceSyncReceiveRequest{
		OwnerID: ownerID, WorkspaceID: workspaceID, FolderID: "morde-" + hex.EncodeToString(folderHash[:16]), OperationID: hex.EncodeToString(operation[:]), Path: path,
		Operation: app.WorkspaceSyncReceiveUpsert, TargetBytes: targetBytes,
	}
}

func receiveTestDeleteRequest(ownerID, workspaceID, path string, key string) app.WorkspaceSyncReceiveRequest {
	request := receiveTestRequest(ownerID, workspaceID, path, 0, key)
	request.Operation = app.WorkspaceSyncReceiveDelete
	return request
}

func receiveTestRequestHash(request app.WorkspaceSyncReceiveRequest) [sha256.Size]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s:%s:%s:%s:%d", request.OwnerID, request.WorkspaceID, request.FolderID,
		request.OperationID, request.Path, request.Operation, request.TargetBytes)))
}
