package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacefs"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

const syncTestOwnerQuotaBytes = 1 << 40

func TestWorkspaceSyncPersistsPlanAndCommitsImmutableRevision(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync")
	file := syncTestFile("src/main.go", []byte("hello"))
	session := newTestSyncSession(t, value, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
	session.PlannedBytes = 0
	transfers := []workspace.TransferBlock{{Chunk: file.Chunks[0], Role: workspace.BlockUpload, Received: true}}
	createRecord := syncTestIdempotency(owner.ID, "workspace.sync.create", "create:initial", session.ID)
	created, replayed, err := createTestWorkspaceSyncSession(ctx, store, session, transfers, createRecord)
	if err != nil || replayed {
		t.Fatalf("create sync session = %#v, replayed=%t, err=%v", created, replayed, err)
	}
	if len(created.TransferBlocks) != 1 || !created.TransferBlocks[0].Received || created.TransferBlocks[0].Role != workspace.BlockUpload {
		t.Fatalf("transfer block state = %#v", created.TransferBlocks)
	}
	if _, replayed, err := createTestWorkspaceSyncSession(ctx, store, session, transfers, createRecord); err != nil || !replayed {
		t.Fatalf("replay sync session, replayed=%t err=%v", replayed, err)
	}

	commitRecord := syncTestIdempotency(owner.ID, "workspace.sync.commit", "commit:initial", session.ID)
	committed, replayed, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID, commitRecord)
	if err != nil || replayed || committed.State != workspace.SyncCompleted || committed.TargetRevision != 1 {
		t.Fatalf("commit sync session = %#v replayed=%t err=%v", committed, replayed, err)
	}
	if _, replayed, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID, commitRecord); err != nil || !replayed {
		t.Fatalf("replay sync commit, replayed=%t err=%v", replayed, err)
	}
	updatedWorkspace, err := store.GetWorkspace(ctx, value.ID)
	if err != nil || updatedWorkspace.Revision != 1 || updatedWorkspace.UsedBytes != file.SizeBytes {
		t.Fatalf("workspace after commit = %#v err=%v", updatedWorkspace, err)
	}
	oldPage, err := store.GetWorkspaceManifestPage(ctx, value.ID, 0, "", "", 100)
	if err != nil || len(oldPage.Files) != 0 {
		t.Fatalf("revision zero manifest = %#v err=%v", oldPage, err)
	}
	newPage, err := store.GetWorkspaceManifestPage(ctx, value.ID, 1, "", "", 100)
	if err != nil || len(newPage.Files) != 1 || newPage.Files[0].Path != file.Path || newPage.Files[0].SHA256 != file.SHA256 {
		t.Fatalf("revision one manifest = %#v err=%v", newPage, err)
	}
	changes, err := store.GetWorkspaceChangePage(ctx, value.ID, 0, "", 100)
	if err != nil || len(changes.Items) != 1 || changes.Items[0].Action != workspace.ChangeUpsert || changes.Items[0].File == nil {
		t.Fatalf("workspace change page = %#v err=%v", changes, err)
	}
}

func TestWorkspaceSyncEnforcesOwnerQuotaAcrossConcurrentWorkspaces(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	first := createTestWorkspace(t, ctx, store, owner.ID, "owner-quota-first")
	second := createTestWorkspace(t, ctx, store, owner.ID, "owner-quota-second")
	firstFile := syncTestFile("first.txt", []byte("sixsix"))
	secondFile := syncTestFile("second.txt", []byte("sixsix"))
	firstSession := newTestSyncSession(t, first, owner.ID, []workspace.File{firstFile}, []workspace.File{firstFile}, nil)
	secondSession := newTestSyncSession(t, second, owner.ID, []workspace.File{secondFile}, []workspace.File{secondFile}, nil)
	firstSession.PlannedBytes, secondSession.PlannedBytes = firstFile.SizeBytes, secondFile.SizeBytes
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for _, item := range []struct {
		session workspace.SyncSession
		file    workspace.File
	}{{firstSession, firstFile}, {secondSession, secondFile}} {
		wait.Add(1)
		go func(session workspace.SyncSession, file workspace.File) {
			defer wait.Done()
			<-start
			_, _, err := store.CreateWorkspaceSyncSession(ctx, session,
				[]workspace.TransferBlock{{Chunk: file.Chunks[0], Role: workspace.BlockUpload}},
				[]app.WorkspaceBlockReservation{{WorkspaceID: session.WorkspaceID, Digest: file.Chunks[0].SHA256, ReservedBytes: file.SizeBytes}}, 10,
				syncTestIdempotency(owner.ID, "workspace.sync.create", "owner-quota:"+session.ID, session.ID))
			errs <- err
		}(item.session, item.file)
	}
	close(start)
	wait.Wait()
	close(errs)

	succeeded, quotaRejected := 0, 0
	for err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		switch domain.CodeOf(err) {
		case domain.CodeQuotaExceeded:
			quotaRejected++
		default:
			t.Errorf("concurrent owner-quota reservation error = %v", err)
		}
	}
	if succeeded != 1 || quotaRejected != 1 {
		t.Fatalf("concurrent owner-quota reservations: succeeded=%d quota_rejected=%d, want 1 each", succeeded, quotaRejected)
	}
	var allocated, reserved int64
	if err := store.pool.QueryRow(ctx, `SELECT allocated_bytes, reserved_bytes FROM workspace_owner_block_usage WHERE owner_id=$1`, owner.ID).Scan(&allocated, &reserved); err != nil {
		t.Fatal(err)
	}
	if allocated != 0 || reserved != 6 {
		t.Fatalf("owner usage after concurrent reservations = allocated %d reserved %d, want 0 and 6", allocated, reserved)
	}
}

func TestWorkspaceSyncQuotaOverrunIsPersistedAndRejectedOnRetry(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "owner-quota-overrun")
	file := syncTestFile("overrun.txt", []byte("sixsix"))
	session := newTestSyncSession(t, value, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
	session.PlannedBytes = file.SizeBytes
	if _, _, err := createTestWorkspaceSyncSessionWithQuota(ctx, store, session,
		[]workspace.TransferBlock{{Chunk: file.Chunks[0], Role: workspace.BlockUpload}}, 10,
		syncTestIdempotency(owner.ID, "workspace.sync.create", "overrun:create", session.ID)); err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		err := store.AckWorkspaceSyncBlock(ctx, value.ID, session.ID, file.SHA256, 11, 10)
		if domain.CodeOf(err) != domain.CodeQuotaExceeded {
			t.Fatalf("upload acknowledgement attempt %d error = %v, want quota exceeded", attempt, err)
		}
	}

	var allocated, reserved int64
	if err := store.pool.QueryRow(ctx, `SELECT allocated_bytes, reserved_bytes FROM workspace_owner_block_usage WHERE owner_id=$1`, owner.ID).Scan(&allocated, &reserved); err != nil {
		t.Fatal(err)
	}
	if allocated != 11 || reserved != 0 {
		t.Fatalf("owner usage after repeated overrun = allocated %d reserved %d, want 11 and 0", allocated, reserved)
	}
	var blockState string
	var blockAllocated int64
	if err := store.pool.QueryRow(ctx, `SELECT state, allocated_bytes FROM workspace_block_usage WHERE workspace_id=$1 AND sha256=$2`, value.ID, file.SHA256).Scan(&blockState, &blockAllocated); err != nil {
		t.Fatal(err)
	}
	if blockState != "stored" || blockAllocated != 11 {
		t.Fatalf("overrun block ledger = state %q allocated %d, want stored and 11", blockState, blockAllocated)
	}
	progress, err := store.GetWorkspaceSyncSession(ctx, value.ID, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if progress.TransferBlocks[0].Received || progress.ConfirmedBytes != 0 {
		t.Fatalf("overrun block was acknowledged: %#v", progress)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID, syncTestIdempotency(owner.ID, "workspace.sync.commit", "overrun:commit", session.ID)); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("commit after overrun error = %v, want workspace busy", err)
	}
	retrySession := newTestSyncSession(t, value, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
	transfers := []workspace.TransferBlock{{Chunk: file.Chunks[0], Role: workspace.BlockUpload, Received: true}}
	if _, _, err := store.CreateWorkspaceSyncSession(ctx, retrySession, transfers, nil, 10,
		syncTestIdempotency(owner.ID, "workspace.sync.create", "overrun:retry-session", retrySession.ID)); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("new session reusing overrun block error = %v, want quota exceeded", err)
	}
}

func TestWorkspaceBlockReconcileRestoresAcceptedReservationAboveQuota(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "owner-quota-reconcile")

	storedFile := syncTestFile("stored.txt", []byte("stored-data"))
	if err := seedTestStoredWorkspaceBlockUsage(ctx, store, value.ID, storedFile.Chunks[0]); err != nil {
		t.Fatal(err)
	}
	pendingFile := syncTestFile("pending.txt", []byte("sixsix"))
	session := newTestSyncSession(t, value, owner.ID, []workspace.File{pendingFile}, []workspace.File{pendingFile}, nil)
	session.PlannedBytes = pendingFile.SizeBytes
	transfer := workspace.TransferBlock{Chunk: pendingFile.Chunks[0], Role: workspace.BlockUpload}
	if _, _, err := createTestWorkspaceSyncSessionWithQuota(ctx, store, session, []workspace.TransferBlock{transfer}, 100,
		syncTestIdempotency(owner.ID, "workspace.sync.create", "reconcile:active-upload", session.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM workspace_block_usage WHERE workspace_id=$1 AND sha256=$2`, value.ID, pendingFile.SHA256); err != nil {
		t.Fatal(err)
	}

	blockStore, err := workspacefs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := blockStore.Close(); err != nil {
			t.Errorf("close workspace block store: %v", err)
		}
	})
	reservationBytes, err := blockStore.ReservationBytes(pendingFile.SizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := store.ListWorkspaceBlocksNeedingReservation(ctx)
	if err != nil || len(targets) != 1 || targets[0].WorkspaceID != value.ID || targets[0].Digest != pendingFile.SHA256 {
		t.Fatalf("active upload blocks needing reservation = %#v, err=%v", targets, err)
	}
	err = store.ReconcileWorkspaceBlockUsage(ctx, []app.WorkspaceBlockAllocation{{
		WorkspaceID: value.ID, Digest: storedFile.SHA256, AllocatedBytes: 11,
	}}, []app.WorkspaceBlockReservation{{
		WorkspaceID: value.ID, Digest: pendingFile.SHA256, ReservedBytes: reservationBytes,
	}}, 10)
	if err != nil {
		t.Fatalf("reconcile accepted reservation after quota reduction: %v", err)
	}

	var allocated, reserved int64
	if err := store.pool.QueryRow(ctx, `SELECT allocated_bytes, reserved_bytes FROM workspace_owner_block_usage WHERE owner_id=$1`, owner.ID).Scan(&allocated, &reserved); err != nil {
		t.Fatal(err)
	}
	if allocated != 11 || reserved != reservationBytes {
		t.Fatalf("reconciled owner usage = allocated %d reserved %d, want 11 and %d", allocated, reserved, reservationBytes)
	}
	var state string
	var blockAllocated, blockReserved int64
	if err := store.pool.QueryRow(ctx, `SELECT state, allocated_bytes, reserved_bytes FROM workspace_block_usage WHERE workspace_id=$1 AND sha256=$2`, value.ID, pendingFile.SHA256).Scan(&state, &blockAllocated, &blockReserved); err != nil {
		t.Fatal(err)
	}
	if state != "reserved" || blockAllocated != 0 || blockReserved != reservationBytes {
		t.Fatalf("recovered block ledger = state %q allocated %d reserved %d", state, blockAllocated, blockReserved)
	}

	second := createTestWorkspace(t, ctx, store, owner.ID, "owner-quota-after-reconcile")
	newFile := syncTestFile("new.txt", []byte("newdata"))
	newSession := newTestSyncSession(t, second, owner.ID, []workspace.File{newFile}, []workspace.File{newFile}, nil)
	newSession.PlannedBytes = newFile.SizeBytes
	newTransfer := workspace.TransferBlock{Chunk: newFile.Chunks[0], Role: workspace.BlockUpload}
	if _, _, err := createTestWorkspaceSyncSessionWithQuota(ctx, store, newSession, []workspace.TransferBlock{newTransfer}, 10,
		syncTestIdempotency(owner.ID, "workspace.sync.create", "reconcile:new-upload", newSession.ID)); domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("new reservation after reconciliation error = %v, want quota exceeded", err)
	}
}

func TestWorkspaceSyncCaseOnlyRenameAdvancesRevisionWithoutChangingQuota(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "case-rename")
	original := syncTestFile("foo.txt", []byte("same content"))
	initial := newTestSyncSession(t, value, owner.ID, []workspace.File{original}, []workspace.File{original}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, initial, syncTestBlocks(initial.UploadFiles), syncTestIdempotency(owner.ID, "workspace.sync.create", "case:initial", initial.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, initial.ID, syncTestIdempotency(owner.ID, "workspace.sync.commit", "case:initial", initial.ID)); err != nil {
		t.Fatal(err)
	}
	value, err := store.GetWorkspace(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	renamed := original
	renamed.Path = "Foo.txt"
	update := newTestSyncSession(t, value, owner.ID, []workspace.File{renamed}, []workspace.File{renamed}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, update, syncTestBlocks(update.UploadFiles), syncTestIdempotency(owner.ID, "workspace.sync.create", "case:rename", update.ID)); err != nil {
		t.Fatal(err)
	}
	committed, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, update.ID, syncTestIdempotency(owner.ID, "workspace.sync.commit", "case:rename", update.ID))
	if err != nil || committed.State != workspace.SyncCompleted || committed.TargetRevision != value.Revision+1 {
		t.Fatalf("case-only rename commit = %#v err=%v", committed, err)
	}
	updated, err := store.GetWorkspace(ctx, value.ID)
	if err != nil || updated.Revision != committed.TargetRevision || updated.UsedBytes != original.SizeBytes {
		t.Fatalf("workspace after case-only rename = %#v err=%v", updated, err)
	}
	manifest, err := store.GetWorkspaceManifestPage(ctx, value.ID, updated.Revision, "", "", 10)
	if err != nil || len(manifest.Files) != 1 || manifest.Files[0].Path != renamed.Path {
		t.Fatalf("renamed workspace manifest = %#v err=%v", manifest, err)
	}
	changes, err := store.GetWorkspaceChangePage(ctx, value.ID, 0, "", 10)
	if err != nil {
		t.Fatalf("case-only rename change feed = %#v err=%v", changes, err)
	}
	byPath := make(map[string]workspace.Change, len(changes.Items))
	for _, change := range changes.Items {
		if change.Revision == committed.TargetRevision {
			byPath[change.Path] = change
		}
	}
	if byPath[original.Path].Action != workspace.ChangeDelete || byPath[original.Path].File != nil ||
		byPath[renamed.Path].Action != workspace.ChangeUpsert || byPath[renamed.Path].File == nil {
		t.Fatalf("case-only rename change feed omitted old delete or new upsert: %#v", changes.Items)
	}
}

func TestWorkspaceSyncConcurrentCaseOnlyRenameConflictsWithoutOverwriting(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "case-rename-conflict")
	original := syncTestFile("foo.txt", []byte("same content"))
	seed := newTestSyncSession(t, value, owner.ID, []workspace.File{original}, []workspace.File{original}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, seed, syncTestBlocks(seed.UploadFiles), syncTestIdempotency(owner.ID, "workspace.sync.create", "case-conflict:seed", seed.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, seed.ID, syncTestIdempotency(owner.ID, "workspace.sync.commit", "case-conflict:seed", seed.ID)); err != nil {
		t.Fatal(err)
	}
	value, err := store.GetWorkspace(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	localRename := original
	localRename.Path = "FOO.txt"
	planned := newTestSyncSession(t, value, owner.ID, []workspace.File{localRename}, []workspace.File{localRename}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, planned, syncTestBlocks(planned.UploadFiles), syncTestIdempotency(owner.ID, "workspace.sync.create", "case-conflict:local", planned.ID)); err != nil {
		t.Fatal(err)
	}
	remoteRename := original
	remoteRename.Path = "Foo.txt"
	remote := newTestSyncSession(t, value, owner.ID, []workspace.File{remoteRename}, []workspace.File{remoteRename}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, remote, syncTestBlocks(remote.UploadFiles), syncTestIdempotency(owner.ID, "workspace.sync.create", "case-conflict:remote", remote.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, remote.ID, syncTestIdempotency(owner.ID, "workspace.sync.commit", "case-conflict:remote", remote.ID)); err != nil {
		t.Fatal(err)
	}
	result, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, planned.ID, syncTestIdempotency(owner.ID, "workspace.sync.commit", "case-conflict:local", planned.ID))
	if err != nil || result.State != workspace.SyncConflict || len(result.Conflicts) != 1 ||
		result.Conflicts[0].Reason != "case_collision" || result.Conflicts[0].Path != remoteRename.Path {
		t.Fatalf("concurrent case-only rename = %#v err=%v", result, err)
	}
	manifest, err := store.GetWorkspaceManifestPage(ctx, value.ID, result.TargetRevision, "", "", 10)
	if err != nil || len(manifest.Files) != 1 || manifest.Files[0].Path != remoteRename.Path {
		t.Fatalf("workspace after conflicting case rename = %#v err=%v", manifest, err)
	}
}

func TestWorkspaceSyncRejectsHostReplicaCommitAfterBindingDetached(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "host-sync-detach")
	created := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{value.ID})
	active, err := store.TransitionWorkspaceBinding(ctx, created.ID, value.ID, 1,
		workspace.BindingPreparing, workspace.BindingActive, 0, 0)
	if err != nil {
		t.Fatalf("activate Host workspace binding: %v", err)
	}

	file := syncTestFile("pending.txt", []byte("authorized before detach"))
	session := newTestSyncSession(t, value, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
	session.ReplicaID = fmt.Sprintf("host:%s:g%d:%s:b%d", created.ID, created.RuntimeGeneration, value.ID, active.BindingVersion)
	createRecord := syncTestIdempotency(owner.ID, "workspace.sync.create", "host-detach:create", session.ID)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks(session.UploadFiles),
		createRecord); err != nil {
		t.Fatalf("create Host replica session while binding is active: %v", err)
	}
	if _, replayed, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks(session.UploadFiles), createRecord); err != nil || !replayed {
		t.Fatalf("replay Host replica sync session, replayed=%t err=%v", replayed, err)
	}

	unbinding, err := store.TransitionWorkspaceBinding(ctx, created.ID, value.ID, active.BindingVersion,
		workspace.BindingActive, workspace.BindingUnbinding, 0, 0)
	if err != nil {
		t.Fatalf("begin workspace unbinding: %v", err)
	}
	if _, err := store.TransitionWorkspaceBinding(ctx, created.ID, value.ID, unbinding.BindingVersion,
		workspace.BindingUnbinding, workspace.BindingDetached, 0, 0); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("direct detach with unfinished Host session error = %v, want workspace busy", err)
	}
	if _, err := store.CompleteWorkspaceBindingUnbind(ctx, created.ID, value.ID, unbinding.BindingVersion, 0); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("complete unbind with unfinished Host session error = %v, want workspace busy", err)
	}
	stillUnbinding, err := store.GetWorkspaceBinding(ctx, created.ID, value.ID)
	if err != nil || stillUnbinding.State != workspace.BindingUnbinding || stillUnbinding.BindingVersion != unbinding.BindingVersion || stillUnbinding.AppliedRevision != 0 {
		t.Fatalf("binding after rejected unbind = %#v, err=%v", stillUnbinding, err)
	}
	if _, _, err := store.CancelWorkspaceSyncSession(ctx, value.ID, session.ID,
		syncTestIdempotency(owner.ID, "workspace.sync.cancel", "host-detach:cancel", session.ID)); err != nil {
		t.Fatalf("cancel unfinished Host sync session: %v", err)
	}
	detached, err := store.CompleteWorkspaceBindingUnbind(ctx, created.ID, value.ID, unbinding.BindingVersion, 0)
	if err != nil || detached.State != workspace.BindingDetached || detached.AppliedRevision != 0 || detached.BindingVersion != unbinding.BindingVersion+1 {
		t.Fatalf("complete workspace unbind after session cancellation = %#v, err=%v", detached, err)
	}
	staleSession := newTestSyncSession(t, value, owner.ID, nil, nil, nil)
	staleSession.ReplicaID = fmt.Sprintf("host:%s:g%d:%s:b%d", created.ID, created.RuntimeGeneration, value.ID, unbinding.BindingVersion)
	staleSession.Direction = workspace.SyncBidirectional
	staleSession.Policy = workspace.SyncLive
	if _, _, err := store.CreateWorkspaceSyncSession(ctx, staleSession, nil, nil, syncTestOwnerQuotaBytes,
		syncTestIdempotency(owner.ID, "workspace.sync.create", "host-detach:stale-create", staleSession.ID)); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale Host sync session creation after detach error = %v, want invalid state", err)
	}

	_, _, err = commitWorkspaceSyncSession(ctx, store, value.ID, session.ID,
		syncTestIdempotency(owner.ID, "workspace.sync.commit", "host-detach:commit", session.ID))
	if domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("old Host replica session commit after detach error = %v, want invalid state", err)
	}
	current, err := store.GetWorkspace(ctx, value.ID)
	if err != nil || current.Revision != 0 || current.UsedBytes != 0 {
		t.Fatalf("workspace changed after stale Host commit = %#v, err=%v", current, err)
	}
	unchanged, err := store.GetWorkspaceSyncSession(ctx, value.ID, session.ID)
	if err != nil || unchanged.State != workspace.SyncCancelled {
		t.Fatalf("Host session state after cancellation = %#v, err=%v", unchanged, err)
	}
}

func TestWorkspaceBindingDetachSerializesHostSyncSessionCreate(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "host-sync-detach-race")
	created := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{value.ID})
	active, err := store.TransitionWorkspaceBinding(ctx, created.ID, value.ID, 1,
		workspace.BindingPreparing, workspace.BindingActive, 0, 0)
	if err != nil {
		t.Fatalf("activate Host workspace binding: %v", err)
	}
	unbinding, err := store.TransitionWorkspaceBinding(ctx, created.ID, value.ID, active.BindingVersion,
		workspace.BindingActive, workspace.BindingUnbinding, 0, 0)
	if err != nil {
		t.Fatalf("begin workspace unbinding: %v", err)
	}

	session := newTestSyncSession(t, value, owner.ID, nil, nil, nil)
	session.ReplicaID = fmt.Sprintf("host:%s:g%d:%s:b%d", created.ID, created.RuntimeGeneration, value.ID, unbinding.BindingVersion)
	session.Direction = workspace.SyncBidirectional
	session.Policy = workspace.SyncLive
	createRecord := syncTestIdempotency(owner.ID, "workspace.sync.create", "host-detach-race:create", session.ID)
	start := make(chan struct{})
	createDone := make(chan error, 1)
	detachDone := make(chan error, 1)
	go func() {
		<-start
		_, _, err := store.CreateWorkspaceSyncSession(ctx, session, nil, nil, syncTestOwnerQuotaBytes, createRecord)
		createDone <- err
	}()
	go func() {
		<-start
		_, err := store.CompleteWorkspaceBindingUnbind(ctx, created.ID, value.ID, unbinding.BindingVersion, 0)
		detachDone <- err
	}()
	close(start)
	createErr := <-createDone
	detachErr := <-detachDone
	switch {
	case createErr == nil:
		if domain.CodeOf(detachErr) != domain.CodeWorkspaceBusy {
			t.Fatalf("detach racing with successful Host session creation error = %v, want workspace busy", detachErr)
		}
		if _, _, err := store.CancelWorkspaceSyncSession(ctx, value.ID, session.ID,
			syncTestIdempotency(owner.ID, "workspace.sync.cancel", "host-detach-race:cancel", session.ID)); err != nil {
			t.Fatalf("cancel Host sync session after serialized create: %v", err)
		}
		if _, err := store.CompleteWorkspaceBindingUnbind(ctx, created.ID, value.ID, unbinding.BindingVersion, 0); err != nil {
			t.Fatalf("complete unbind after serialized session cancellation: %v", err)
		}
	case domain.CodeOf(createErr) == domain.CodeInvalidState:
		if detachErr != nil {
			t.Fatalf("detach succeeded after serialized session rejection: %v", detachErr)
		}
	default:
		t.Fatalf("Host sync session create racing with detach error = %v, detach error = %v", createErr, detachErr)
	}
	if createErr == nil && detachErr == nil {
		t.Fatal("Host sync session creation and workspace detach both succeeded")
	}
}

func TestListDeviceWorkspaceSyncProgressRespectsActiveInstallationAndHidesDetails(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "device-progress")
	makeDevice := func(name string) identity.Token {
		t.Helper()
		installationID := createTestClientInstallation(t, pool, owner.ID, nil)
		id, err := domain.NewTokenID()
		if err != nil {
			t.Fatal(err)
		}
		_, secretHash, err := identity.NewCredential(identity.KindDevice, id)
		if err != nil {
			t.Fatal(err)
		}
		token, err := store.CreateToken(ctx, identity.Token{
			ID: id, OwnerID: owner.ID, ClientInstallationID: installationID, Kind: identity.KindDevice, Name: name,
			Scopes:     []identity.Scope{identity.ScopeWorkspaceSync},
			SecretHash: secretHash, ExpiresAt: time.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	device := makeDevice("active")
	revoked := makeDevice("revoked")
	file := syncTestFile("private/path.txt", []byte("private"))
	session := newTestSyncSession(t, value, owner.ID, []workspace.File{file}, []workspace.File{}, nil)
	session.ReplicaID = "device:" + device.ID
	session.Direction = workspace.SyncDownload
	session.PlannedBytes = file.SizeBytes
	session.DownloadFiles = []workspace.File{file}
	session.DownloadActions = workspace.DownloadActionsForPlan(session.DownloadFiles, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, []workspace.TransferBlock{{Chunk: file.Chunks[0], Role: workspace.BlockDownload}},
		syncTestIdempotency(owner.ID, "workspace.sync.create", "progress:authorized", session.ID)); err != nil {
		t.Fatal(err)
	}
	ignored := newTestSyncSession(t, value, owner.ID, []workspace.File{}, []workspace.File{}, nil)
	ignored.ReplicaID = "device:" + revoked.ID
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, ignored, nil,
		syncTestIdempotency(owner.ID, "workspace.sync.create", "progress:revoked", ignored.ID)); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeToken(ctx, revoked.ID, owner.ID); err != nil {
		t.Fatal(err)
	}

	items, err := store.ListDeviceWorkspaceSyncProgress(ctx, owner.ID, value.ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("ListDeviceWorkspaceSyncProgress() = %#v, %v", items, err)
	}
	got := items[0]
	if got.DeviceID != device.ID || got.SessionID != session.ID || got.State != workspace.SyncPlanned ||
		got.PlannedBytes != file.SizeBytes || got.ConfirmedBytes != 0 ||
		got.DownloadActionsTotal != 1 || got.DownloadActionsDone != 0 {
		t.Fatalf("device progress = %#v", got)
	}
	if err := store.RevokeToken(ctx, device.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	items, err = store.ListDeviceWorkspaceSyncProgress(ctx, owner.ID, value.ID)
	if err != nil || len(items) != 0 {
		t.Fatalf("revoked device progress = %#v, %v", items, err)
	}
}

func TestListWorkspaceBlockReferencesIncludesHistoricalRevisions(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "block-references")

	for index, body := range [][]byte{[]byte("version one"), []byte("version two")} {
		file := syncTestFile("main.txt", body)
		session := newTestSyncSession(t, value, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
		if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks(session.UploadFiles), syncTestIdempotency(owner.ID, "workspace.sync.create", fmt.Sprintf("create:history:%d", index), session.ID)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID, syncTestIdempotency(owner.ID, "workspace.sync.commit", fmt.Sprintf("commit:history:%d", index), session.ID)); err != nil {
			t.Fatal(err)
		}
		updated, err := store.GetWorkspace(ctx, value.ID)
		if err != nil {
			t.Fatal(err)
		}
		value = updated
	}

	references, err := store.ListWorkspaceBlockReferences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 2 {
		t.Fatalf("block references = %#v, want both immutable revisions", references)
	}
	got := map[string]int64{}
	for _, reference := range references {
		if reference.WorkspaceID != value.ID {
			t.Errorf("block reference workspace = %q, want %q", reference.WorkspaceID, value.ID)
		}
		got[reference.SHA256] = reference.SizeBytes
	}
	for _, body := range [][]byte{[]byte("version one"), []byte("version two")} {
		file := syncTestFile("main.txt", body)
		if got[file.SHA256] != file.SizeBytes {
			t.Errorf("historical block %s size = %d, want %d", file.SHA256, got[file.SHA256], file.SizeBytes)
		}
	}
}

func TestListWorkspaceBlockReferencesSkipsDeletingAndDeletedWorkspaces(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	ready := createTestWorkspace(t, ctx, store, owner.ID, "block-references-ready")
	deleting := createTestWorkspace(t, ctx, store, owner.ID, "block-references-deleting")
	deleted := createTestWorkspace(t, ctx, store, owner.ID, "block-references-deleted")

	for index, value := range []workspace.Workspace{ready, deleting, deleted} {
		file := syncTestFile("main.txt", []byte(fmt.Sprintf("workspace %d", index)))
		session := newTestSyncSession(t, value, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
		if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks(session.UploadFiles),
			syncTestIdempotency(owner.ID, "workspace.sync.create", fmt.Sprintf("block-references-create:%d", index), session.ID)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID,
			syncTestIdempotency(owner.ID, "workspace.sync.commit", fmt.Sprintf("block-references-commit:%d", index), session.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET state='deleting' WHERE id=$1`, deleting.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET state='deleted', deleted_at=now() WHERE id=$1`, deleted.ID); err != nil {
		t.Fatal(err)
	}

	references, err := store.ListWorkspaceBlockReferences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 || references[0].WorkspaceID != ready.ID {
		t.Fatalf("verified workspace references = %#v, want only ready workspace %s", references, ready.ID)
	}
}

func TestWorkspaceBlockGCReferencesIncludeHistoryAndActiveUploadPins(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "block-gc-pins")

	for index, body := range [][]byte{[]byte("history one"), []byte("history two")} {
		file := syncTestFile("main.txt", body)
		session := newTestSyncSession(t, value, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
		if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, syncTestBlocks(session.UploadFiles),
			syncTestIdempotency(owner.ID, "workspace.sync.create", fmt.Sprintf("gc-history-create:%d", index), session.ID)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID,
			syncTestIdempotency(owner.ID, "workspace.sync.commit", fmt.Sprintf("gc-history-commit:%d", index), session.ID)); err != nil {
			t.Fatal(err)
		}
		updated, err := store.GetWorkspace(ctx, value.ID)
		if err != nil {
			t.Fatal(err)
		}
		value = updated
	}

	pinned := make([]workspace.File, 0, 2)
	for _, item := range []struct {
		path     string
		body     []byte
		received bool
	}{{"received.bin", []byte("received upload pin"), true}, {"pending.bin", []byte("unreceived upload pin"), false}} {
		file := syncTestFile(item.path, item.body)
		session := newTestSyncSession(t, value, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
		block := workspace.TransferBlock{Chunk: file.Chunks[0], Role: workspace.BlockUpload, Received: item.received}
		if !item.received {
			session.PlannedBytes = file.SizeBytes
		}
		if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, []workspace.TransferBlock{block},
			syncTestIdempotency(owner.ID, "workspace.sync.create", "gc-pin:"+item.path, session.ID)); err != nil {
			t.Fatal(err)
		}
		pinned = append(pinned, file)
	}
	orphan := syncTestFile("orphan.bin", []byte("not present in revision or upload plan"))

	references := workspaceGCReferenceMap(t, store, ctx, value.ID)
	want := [][]byte{[]byte("history one"), []byte("history two"), []byte("received upload pin"), []byte("unreceived upload pin")}
	if len(references) != len(want) {
		t.Fatalf("protected workspace block references = %#v, want %d unique chunks", references, len(want))
	}
	for _, body := range want {
		file := syncTestFile("unused", body)
		if size, ok := references[file.SHA256]; !ok || size != file.SizeBytes {
			t.Errorf("protected block %s has size %d and present=%t, want %d", file.SHA256, size, ok, file.SizeBytes)
		}
	}
	for _, file := range pinned {
		if references[file.SHA256] != file.SizeBytes {
			t.Errorf("active upload pin %s was not retained", file.SHA256)
		}
	}
	if _, ok := references[orphan.SHA256]; ok {
		t.Errorf("orphan block %s unexpectedly appears in the protection set", orphan.SHA256)
	}
}

func TestExpireIdleWorkspaceSyncSessionsRemovesOnlyExpiredActivePins(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "block-gc-expiry")

	createPinnedSession := func(path, body string, key string) (workspace.SyncSession, workspace.File) {
		t.Helper()
		file := syncTestFile(path, []byte(body))
		session := newTestSyncSession(t, value, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
		session.PlannedBytes = file.SizeBytes
		if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, []workspace.TransferBlock{{
			Chunk: file.Chunks[0], Role: workspace.BlockUpload, Received: false,
		}}, syncTestIdempotency(owner.ID, "workspace.sync.create", key, session.ID)); err != nil {
			t.Fatal(err)
		}
		return session, file
	}
	stale, staleFile := createPinnedSession("stale.bin", "stale pin", "gc-expiry-stale")
	fresh, freshFile := createPinnedSession("fresh.bin", "fresh pin", "gc-expiry-fresh")
	terminal, terminalFile := createPinnedSession("terminal.bin", "terminal pin", "gc-expiry-terminal")
	if _, _, err := store.CancelWorkspaceSyncSession(ctx, value.ID, terminal.ID,
		syncTestIdempotency(owner.ID, "workspace.sync.cancel", "gc-expiry-cancel", terminal.ID)); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().UTC().Add(-time.Hour)
	oldAt := cutoff.Add(-time.Minute)
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_sync_sessions SET updated_at=$3 WHERE workspace_id=$1 AND id=$2`, value.ID, stale.ID, oldAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspace_sync_sessions SET updated_at=$3 WHERE workspace_id=$1 AND id=$2`, value.ID, terminal.ID, oldAt); err != nil {
		t.Fatal(err)
	}

	expired, err := store.ExpireIdleWorkspaceSyncSessions(ctx, value.ID, cutoff)
	if err != nil || expired != 1 {
		t.Fatalf("ExpireIdleWorkspaceSyncSessions() = (%d, %v), want (1, nil)", expired, err)
	}
	for _, item := range []struct {
		sessionID string
		want      workspace.SyncState
	}{{stale.ID, workspace.SyncExpired}, {fresh.ID, workspace.SyncPlanned}, {terminal.ID, workspace.SyncCancelled}} {
		session, err := store.GetWorkspaceSyncSession(ctx, value.ID, item.sessionID)
		if err != nil || session.State != item.want {
			t.Errorf("session %s state = %s, %v; want %s", item.sessionID, session.State, err, item.want)
		}
	}
	references := workspaceGCReferenceMap(t, store, ctx, value.ID)
	if _, ok := references[staleFile.SHA256]; ok {
		t.Errorf("expired upload pin %s remains protected", staleFile.SHA256)
	}
	if references[freshFile.SHA256] != freshFile.SizeBytes {
		t.Errorf("fresh upload pin %s is not protected", freshFile.SHA256)
	}
	if _, ok := references[terminalFile.SHA256]; ok {
		t.Errorf("cancelled upload pin %s remains protected", terminalFile.SHA256)
	}
}

func TestListWorkspaceIDsForBlockGCOnlyReturnsReadyNonDeletedWorkspaces(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	ready := createTestWorkspace(t, ctx, store, owner.ID, "block-gc-ready")
	deleting := createTestWorkspace(t, ctx, store, owner.ID, "block-gc-deleting")
	deleted := createTestWorkspace(t, ctx, store, owner.ID, "block-gc-deleted")
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET state='deleting' WHERE id=$1`, deleting.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE workspaces SET deleted_at=now() WHERE id=$1`, deleted.ID); err != nil {
		t.Fatal(err)
	}

	workspaceIDs, err := store.ListWorkspaceIDsForBlockGC(ctx)
	if err != nil || len(workspaceIDs) != 1 || workspaceIDs[0] != ready.ID {
		t.Fatalf("ListWorkspaceIDsForBlockGC() = %#v, %v; want only %s", workspaceIDs, err, ready.ID)
	}
}

func TestWorkspaceBlockStoreAdvisoryLockModesShareOneKey(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	workspaceID, err := domain.NewWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	firstRead, acquired, err := store.TryWorkspaceBlockStoreReadLock(ctx, workspaceID)
	if err != nil || !acquired || firstRead == nil {
		t.Fatalf("first shared workspace block lock = (%v, %t, %v)", firstRead, acquired, err)
	}
	defer func() { _ = firstRead.Release(ctx) }()
	secondRead, acquired, err := store.TryWorkspaceBlockStoreReadLock(ctx, workspaceID)
	if err != nil || !acquired || secondRead == nil {
		t.Fatalf("second shared workspace block lock = (%v, %t, %v)", secondRead, acquired, err)
	}
	defer func() { _ = secondRead.Release(ctx) }()
	if exclusive, acquired, err := store.TryWorkspaceBlockStoreLock(ctx, workspaceID); err != nil || acquired || exclusive != nil {
		if exclusive != nil {
			_ = exclusive.Release(ctx)
		}
		t.Fatalf("exclusive lock with shared holders = (%v, %t, %v), want not acquired", exclusive, acquired, err)
	}
	if err := firstRead.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := secondRead.Release(ctx); err != nil {
		t.Fatal(err)
	}
	exclusive, acquired, err := store.TryWorkspaceBlockStoreLock(ctx, workspaceID)
	if err != nil || !acquired || exclusive == nil {
		t.Fatalf("exclusive workspace block lock = (%v, %t, %v)", exclusive, acquired, err)
	}
	defer func() { _ = exclusive.Release(ctx) }()
	if shared, acquired, err := store.TryWorkspaceBlockStoreReadLock(ctx, workspaceID); err != nil || acquired || shared != nil {
		if shared != nil {
			_ = shared.Release(ctx)
		}
		t.Fatalf("shared lock with exclusive holder = (%v, %t, %v), want not acquired", shared, acquired, err)
	}
}

func TestWorkspaceBlockGCSerializesWithCommit(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "block-gc-serialize")
	file := syncTestFile("commit.bin", []byte("commit waits for gc"))
	commitSession := newTestSyncSession(t, value, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, commitSession, syncTestBlocks(commitSession.UploadFiles),
		syncTestIdempotency(owner.ID, "workspace.sync.create", "gc-serialize-commit-create", commitSession.ID)); err != nil {
		t.Fatal(err)
	}

	releaseGC := make(chan struct{})
	release := func() {
		select {
		case <-releaseGC:
		default:
			close(releaseGC)
		}
	}
	defer release()
	gcEntered := make(chan []workspace.BlockReference, 1)
	gcDone := make(chan error, 1)
	go func() {
		gcDone <- store.WithWorkspaceBlockGC(ctx, value.ID, func(references []workspace.BlockReference) ([]app.WorkspaceBlockRemoval, error) {
			gcEntered <- references
			<-releaseGC
			return nil, nil
		})
	}()
	select {
	case references := <-gcEntered:
		if len(references) != 1 || references[0].SHA256 != file.SHA256 {
			t.Fatalf("GC protection set while commit is pending = %#v", references)
		}
	case err := <-gcDone:
		t.Fatalf("workspace GC returned before entering callback: %v", err)
	case <-ctx.Done():
		t.Fatal("workspace GC did not enter callback")
	}

	commitDone := make(chan error, 1)
	go func() {
		_, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, commitSession.ID,
			syncTestIdempotency(owner.ID, "workspace.sync.commit", "gc-serialize-commit", commitSession.ID))
		commitDone <- err
	}()
	select {
	case err := <-commitDone:
		release()
		t.Fatalf("workspace commit completed while GC held the row lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case err := <-gcDone:
		if err != nil {
			t.Fatalf("workspace GC: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("workspace GC did not finish after callback was released")
	}
	select {
	case err := <-commitDone:
		if err != nil {
			t.Fatalf("workspace commit after GC: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("workspace commit did not finish after GC released the row lock")
	}

	value, err := store.GetWorkspace(ctx, value.ID)
	if err != nil || value.Revision != 1 {
		t.Fatalf("workspace after serialized commit = %#v, %v", value, err)
	}
}

func TestWorkspaceBlockGCSerializesWithSessionCreation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "block-gc-create-serialize")
	file := syncTestFile("pending.bin", []byte("session creation waits for gc"))
	session := newTestSyncSession(t, value, owner.ID, []workspace.File{file}, []workspace.File{file}, nil)
	session.PlannedBytes = file.SizeBytes
	blocks := []workspace.TransferBlock{{Chunk: file.Chunks[0], Role: workspace.BlockUpload, Received: false}}
	createRecord := syncTestIdempotency(owner.ID, "workspace.sync.create", "gc-create-serialize", session.ID)

	releaseGC := make(chan struct{})
	release := func() {
		select {
		case <-releaseGC:
		default:
			close(releaseGC)
		}
	}
	defer release()
	gcEntered := make(chan struct{}, 1)
	gcDone := make(chan error, 1)
	go func() {
		gcDone <- store.WithWorkspaceBlockGC(ctx, value.ID, func([]workspace.BlockReference) ([]app.WorkspaceBlockRemoval, error) {
			gcEntered <- struct{}{}
			<-releaseGC
			return nil, nil
		})
	}()
	select {
	case <-gcEntered:
	case err := <-gcDone:
		t.Fatalf("workspace GC returned before entering callback: %v", err)
	case <-ctx.Done():
		t.Fatal("workspace GC did not enter callback")
	}

	createDone := make(chan error, 1)
	go func() {
		_, _, err := createTestWorkspaceSyncSession(ctx, store, session, blocks, createRecord)
		createDone <- err
	}()
	select {
	case err := <-createDone:
		release()
		t.Fatalf("sync session creation completed while GC held the row lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case err := <-gcDone:
		if err != nil {
			t.Fatalf("workspace GC: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("workspace GC did not finish after callback was released")
	}
	select {
	case err := <-createDone:
		if err != nil {
			t.Fatalf("sync session creation after GC: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("sync session creation did not finish after GC released the row lock")
	}
	if references := workspaceGCReferenceMap(t, store, ctx, value.ID); references[file.SHA256] != file.SizeBytes {
		t.Fatalf("new active upload session pin is missing after GC: %#v", references)
	}
}

func workspaceGCReferenceMap(t *testing.T, store *Store, ctx context.Context, workspaceID string) map[string]int64 {
	t.Helper()
	references := make(map[string]int64)
	err := store.WithWorkspaceBlockGC(ctx, workspaceID, func(items []workspace.BlockReference) ([]app.WorkspaceBlockRemoval, error) {
		for _, item := range items {
			if item.WorkspaceID != workspaceID {
				t.Errorf("GC reference workspace = %q, want %q", item.WorkspaceID, workspaceID)
			}
			references[item.SHA256] = item.SizeBytes
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return references
}

func TestWorkspaceSyncCommitConflictsChangedPathAndAppliesUnrelatedPath(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-conflict")
	plannedSame := syncTestFile("same.txt", []byte("client"))
	plannedOther := syncTestFile("other.txt", []byte("safe"))
	planned := newTestSyncSession(t, value, owner.ID, []workspace.File{plannedSame, plannedOther}, []workspace.File{plannedSame, plannedOther}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, planned, syncTestBlocks(planned.UploadFiles), syncTestIdempotency(owner.ID, "workspace.sync.create", "create:planned", planned.ID)); err != nil {
		t.Fatal(err)
	}

	remoteSame := syncTestFile("same.txt", []byte("remote"))
	remote := newTestSyncSession(t, value, owner.ID, []workspace.File{remoteSame}, []workspace.File{remoteSame}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, remote, syncTestBlocks(remote.UploadFiles), syncTestIdempotency(owner.ID, "workspace.sync.create", "create:remote", remote.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, remote.ID, syncTestIdempotency(owner.ID, "workspace.sync.commit", "commit:remote", remote.ID)); err != nil {
		t.Fatal(err)
	}

	result, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, planned.ID, syncTestIdempotency(owner.ID, "workspace.sync.commit", "commit:planned", planned.ID))
	if err != nil {
		t.Fatal(err)
	}
	if result.State != workspace.SyncConflict || len(result.Conflicts) != 1 || result.Conflicts[0].Path != plannedSame.Path || result.TargetRevision != 2 {
		t.Fatalf("partial sync result = %#v", result)
	}
	page, err := store.GetWorkspaceManifestPage(ctx, value.ID, 2, "", "", 100)
	if err != nil || len(page.Files) != 2 {
		t.Fatalf("revision two manifest = %#v err=%v", page, err)
	}
	got := map[string]string{}
	for _, file := range page.Files {
		got[file.Path] = file.SHA256
	}
	if got["same.txt"] != remoteSame.SHA256 || got["other.txt"] != plannedOther.SHA256 {
		t.Fatalf("partial sync manifest = %#v", got)
	}
	if _, err := store.q.GetWorkspaceSyncCursor(ctx, sqlcgen.GetWorkspaceSyncCursorParams{WorkspaceID: value.ID, ReplicaID: planned.ReplicaID}); err == nil {
		t.Fatal("conflicted session advanced the durable cursor")
	}
}

func TestWorkspaceSyncBlockAcknowledgementsAreIdempotentAndGateCommit(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-ack")
	remoteFile := syncTestFile("remote.txt", []byte("remote"))
	seed := newTestSyncSession(t, value, owner.ID, []workspace.File{remoteFile}, []workspace.File{remoteFile}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, seed, syncTestBlocks(seed.UploadFiles), syncTestIdempotency(owner.ID, "workspace.sync.create", "create:seed", seed.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, seed.ID, syncTestIdempotency(owner.ID, "workspace.sync.commit", "commit:seed", seed.ID)); err != nil {
		t.Fatal(err)
	}
	value, err := store.GetWorkspace(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	uploadFile := syncTestFile("local.txt", []byte("local"))
	sessionID, err := workspace.NewSyncSessionID()
	if err != nil {
		t.Fatal(err)
	}
	ackCursor := workspace.EncodeChangeCursor(workspace.ChangeCursor{Revision: value.Revision, Path: "remote.txt"})
	session := workspace.SyncSession{
		ID: sessionID, WorkspaceID: value.ID, OwnerID: owner.ID, ReplicaID: "session:" + owner.ID,
		Direction: workspace.SyncBidirectional, Policy: workspace.SyncLive, State: workspace.SyncPlanned, Stage: workspace.SyncStageCompare,
		BaseRevision: value.Revision, TargetRevision: value.Revision, PinnedRevision: value.Revision,
		PlannedBytes: uploadFile.SizeBytes + remoteFile.SizeBytes,
		Files:        []workspace.File{uploadFile}, UploadFiles: []workspace.File{uploadFile}, DownloadFiles: []workspace.File{remoteFile},
		Scope: []string{}, DeletedPaths: []string{}, UploadDeletedPaths: []string{}, Conflicts: []workspace.Conflict{}, AckCursor: &ackCursor,
	}
	session.DownloadActions = workspace.DownloadActionsForPlan(session.DownloadFiles, nil)
	transfers := []workspace.TransferBlock{
		{Chunk: uploadFile.Chunks[0], Role: workspace.BlockUpload},
		{Chunk: remoteFile.Chunks[0], Role: workspace.BlockDownload},
	}
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, transfers, syncTestIdempotency(owner.ID, "workspace.sync.create", "create:mixed", session.ID)); err != nil {
		t.Fatal(err)
	}
	commitRecord := syncTestIdempotency(owner.ID, "workspace.sync.commit", "commit:mixed", session.ID)
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID, commitRecord); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("commit before block acknowledgements error = %v", err)
	}
	if err := store.AckWorkspaceSyncBlock(ctx, value.ID, session.ID, uploadFile.SHA256, uploadFile.SizeBytes, syncTestOwnerQuotaBytes); err != nil {
		t.Fatal(err)
	}
	if err := store.AckWorkspaceSyncBlock(ctx, value.ID, session.ID, uploadFile.SHA256, uploadFile.SizeBytes, syncTestOwnerQuotaBytes); err != nil {
		t.Fatalf("repeat upload acknowledgement: %v", err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID, commitRecord); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("commit before download acknowledgement error = %v", err)
	}
	if err := store.AckWorkspaceSyncDownloadBlock(ctx, value.ID, session.ID, remoteFile.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := store.AckWorkspaceSyncDownloadBlock(ctx, value.ID, session.ID, remoteFile.SHA256); err != nil {
		t.Fatalf("repeat download acknowledgement: %v", err)
	}
	progress, err := store.GetWorkspaceSyncSession(ctx, value.ID, session.ID)
	if err != nil || progress.ConfirmedBytes != session.PlannedBytes || len(progress.TransferBlocks) != 2 {
		t.Fatalf("acknowledged sync progress = %#v err=%v", progress, err)
	}
	for _, block := range progress.TransferBlocks {
		if !block.Received {
			t.Fatalf("block remained unacknowledged: %#v", block)
		}
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID, commitRecord); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("commit before file apply acknowledgement error = %v", err)
	}
	if err := store.AckWorkspaceSyncDownloadAction(ctx, value.ID, session.ID, remoteFile.Path, workspace.ChangeUpsert, remoteFile.SHA256, remoteFile.SizeBytes); err != nil {
		t.Fatal(err)
	}
	if err := store.AckWorkspaceSyncDownloadAction(ctx, value.ID, session.ID, remoteFile.Path, workspace.ChangeUpsert, remoteFile.SHA256, remoteFile.SizeBytes); err != nil {
		t.Fatalf("repeat download action acknowledgement: %v", err)
	}
	actionProgress, err := store.GetWorkspaceSyncSession(ctx, value.ID, session.ID)
	if err != nil || actionProgress.ConfirmedBytes != session.PlannedBytes || len(actionProgress.DownloadActions) != 1 || !actionProgress.DownloadActions[0].Applied {
		t.Fatalf("acknowledged download action progress = %#v err=%v", actionProgress, err)
	}
	committed, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID, commitRecord)
	if err != nil || committed.State != workspace.SyncCompleted {
		t.Fatalf("commit acknowledged sync session = %#v err=%v", committed, err)
	}
	cursor, err := store.q.GetWorkspaceSyncCursor(ctx, sqlcgen.GetWorkspaceSyncCursorParams{WorkspaceID: value.ID, ReplicaID: session.ReplicaID})
	if err != nil || cursor.Revision != value.Revision || cursor.FilePath != "remote.txt" {
		t.Fatalf("durable sync cursor = %#v err=%v", cursor, err)
	}
}

func TestWorkspaceSyncDownloadActionsRequireEveryPathIncludingEmptyFile(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-download-actions")
	shared := []byte("shared")
	fileA := syncTestFile("a.txt", shared)
	fileB := syncTestFile("b.txt", shared)
	emptyFile := syncTestFile("empty.txt", nil)
	seed := newTestSyncSession(t, value, owner.ID, []workspace.File{fileA, fileB, emptyFile}, []workspace.File{fileA, fileB, emptyFile}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, seed, syncTestBlocks(seed.UploadFiles), syncTestIdempotency(owner.ID, "workspace.sync.create", "create:action-seed", seed.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, seed.ID, syncTestIdempotency(owner.ID, "workspace.sync.commit", "commit:action-seed", seed.ID)); err != nil {
		t.Fatal(err)
	}
	value, err := store.GetWorkspace(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := workspace.NewSyncSessionID()
	if err != nil {
		t.Fatal(err)
	}
	ackCursor := workspace.EncodeChangeCursor(workspace.ChangeCursor{Revision: value.Revision})
	session := workspace.SyncSession{
		ID: sessionID, WorkspaceID: value.ID, OwnerID: owner.ID, ReplicaID: "session:" + owner.ID,
		Direction: workspace.SyncDownload, Policy: workspace.SyncFillMissing, State: workspace.SyncPlanned, Stage: workspace.SyncStageCompare,
		BaseRevision: 0, TargetRevision: value.Revision, PinnedRevision: value.Revision,
		PlannedBytes: int64(len(shared)), Files: []workspace.File{}, UploadFiles: []workspace.File{},
		DownloadFiles: []workspace.File{fileA, fileB, emptyFile}, Scope: []string{}, DeletedPaths: []string{},
		UploadDeletedPaths: []string{}, DownloadDeletedPaths: []string{}, Conflicts: []workspace.Conflict{}, AckCursor: &ackCursor,
	}
	session.DownloadActions = workspace.DownloadActionsForPlan(session.DownloadFiles, session.DownloadDeletedPaths)
	blocks := []workspace.TransferBlock{{Chunk: fileA.Chunks[0], Role: workspace.BlockDownload}}
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, blocks, syncTestIdempotency(owner.ID, "workspace.sync.create", "create:actions", session.ID)); err != nil {
		t.Fatal(err)
	}
	created, err := store.GetWorkspaceSyncSession(ctx, value.ID, session.ID)
	if err != nil || len(created.DownloadActions) != 3 || created.DownloadActions[2].Path != "empty.txt" || created.DownloadActions[2].Applied {
		t.Fatalf("persisted download actions = %#v err=%v", created.DownloadActions, err)
	}
	commitRecord := syncTestIdempotency(owner.ID, "workspace.sync.commit", "commit:actions", session.ID)
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID, commitRecord); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("commit before block acknowledgement error = %v", err)
	}
	if err := store.AckWorkspaceSyncDownloadBlock(ctx, value.ID, session.ID, fileA.Chunks[0].SHA256); err != nil {
		t.Fatal(err)
	}
	verified, err := store.GetWorkspaceSyncSession(ctx, value.ID, session.ID)
	if err != nil || verified.State != workspace.SyncVerifying || verified.Stage != workspace.SyncStageVerify {
		t.Fatalf("download after transfer stage = %#v err=%v", verified, err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID, commitRecord); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("commit before path acknowledgements error = %v", err)
	}
	for _, file := range []workspace.File{fileA, fileB} {
		if err := store.AckWorkspaceSyncDownloadAction(ctx, value.ID, session.ID, file.Path, workspace.ChangeUpsert, file.SHA256, file.SizeBytes); err != nil {
			t.Fatalf("ack %s: %v", file.Path, err)
		}
		applying, err := store.GetWorkspaceSyncSession(ctx, value.ID, session.ID)
		if err != nil || applying.State != workspace.SyncApplying || applying.Stage != workspace.SyncStageApply {
			t.Fatalf("download after applying %s = %#v err=%v", file.Path, applying, err)
		}
		if err := store.AckWorkspaceSyncDownloadAction(ctx, value.ID, session.ID, file.Path, workspace.ChangeUpsert, file.SHA256, file.SizeBytes); err != nil {
			t.Fatalf("repeat ack %s: %v", file.Path, err)
		}
		if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID, commitRecord); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
			t.Fatalf("commit before every path action was applied error = %v", err)
		}
	}
	if err := store.AckWorkspaceSyncDownloadAction(ctx, value.ID, session.ID, emptyFile.Path, workspace.ChangeUpsert, emptyFile.SHA256, 0); err != nil {
		t.Fatalf("ack zero-byte file: %v", err)
	}
	progress, err := store.GetWorkspaceSyncSession(ctx, value.ID, session.ID)
	if err != nil || progress.ConfirmedBytes != session.PlannedBytes || len(progress.DownloadActions) != 3 {
		t.Fatalf("all download action progress = %#v err=%v", progress, err)
	}
	for _, action := range progress.DownloadActions {
		if !action.Applied {
			t.Fatalf("download action remains unapplied: %#v", action)
		}
	}
	committed, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, session.ID, commitRecord)
	if err != nil || committed.State != workspace.SyncCompleted {
		t.Fatalf("commit after every action was applied = %#v err=%v", committed, err)
	}
	cursor, err := store.q.GetWorkspaceSyncCursor(ctx, sqlcgen.GetWorkspaceSyncCursorParams{WorkspaceID: value.ID, ReplicaID: session.ReplicaID})
	if err != nil || cursor.Revision != value.Revision {
		t.Fatalf("cursor after all actions applied = %#v err=%v", cursor, err)
	}
}

func TestWorkspaceSyncDownloadDeleteActionMustBeAcknowledged(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "sync-download-delete")
	remoteFile := syncTestFile("removed.txt", []byte("remote"))
	seed := newTestSyncSession(t, value, owner.ID, []workspace.File{remoteFile}, []workspace.File{remoteFile}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, seed, syncTestBlocks(seed.UploadFiles), syncTestIdempotency(owner.ID, "workspace.sync.create", "create:delete-seed", seed.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, seed.ID, syncTestIdempotency(owner.ID, "workspace.sync.commit", "commit:delete-seed", seed.ID)); err != nil {
		t.Fatal(err)
	}
	value, err := store.GetWorkspace(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	deleteRemote := newTestSyncSession(t, value, owner.ID, []workspace.File{}, []workspace.File{}, []string{remoteFile.Path})
	deleteRemote.Direction = workspace.SyncUpload
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, deleteRemote, []workspace.TransferBlock{}, syncTestIdempotency(owner.ID, "workspace.sync.create", "create:delete-remote", deleteRemote.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, deleteRemote.ID, syncTestIdempotency(owner.ID, "workspace.sync.commit", "commit:delete-remote", deleteRemote.ID)); err != nil {
		t.Fatal(err)
	}
	value, err = store.GetWorkspace(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	ackCursor := workspace.EncodeChangeCursor(workspace.ChangeCursor{Revision: value.Revision, Path: remoteFile.Path})
	downloadID, err := workspace.NewSyncSessionID()
	if err != nil {
		t.Fatal(err)
	}
	deletion := workspace.SyncSession{
		ID: downloadID, WorkspaceID: value.ID, OwnerID: owner.ID, ReplicaID: "session:" + owner.ID,
		Direction: workspace.SyncDownload, Policy: workspace.SyncCompareMerge, State: workspace.SyncPlanned, Stage: workspace.SyncStageCompare,
		BaseRevision: 1, TargetRevision: value.Revision, PinnedRevision: value.Revision,
		Files: []workspace.File{remoteFile}, UploadFiles: []workspace.File{}, DownloadFiles: []workspace.File{},
		Scope: []string{}, DeletedPaths: []string{}, UploadDeletedPaths: []string{}, DownloadDeletedPaths: []string{remoteFile.Path},
		Conflicts: []workspace.Conflict{}, AckCursor: &ackCursor,
	}
	deletion.DownloadActions = workspace.DownloadActionsForPlan(deletion.DownloadFiles, deletion.DownloadDeletedPaths)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, deletion, []workspace.TransferBlock{}, syncTestIdempotency(owner.ID, "workspace.sync.create", "create:download-delete", deletion.ID)); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.GetWorkspaceSyncSession(ctx, value.ID, deletion.ID)
	if err != nil || len(persisted.DownloadDeletedPaths) != 1 || len(persisted.DownloadActions) != 1 || persisted.DownloadActions[0].Action != workspace.ChangeDelete {
		t.Fatalf("persisted delete plan = %#v err=%v", persisted, err)
	}
	commitRecord := syncTestIdempotency(owner.ID, "workspace.sync.commit", "commit:download-delete", deletion.ID)
	if _, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, deletion.ID, commitRecord); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("commit before delete action acknowledgement error = %v", err)
	}
	if err := store.AckWorkspaceSyncDownloadAction(ctx, value.ID, deletion.ID, remoteFile.Path, workspace.ChangeDelete, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := store.AckWorkspaceSyncDownloadAction(ctx, value.ID, deletion.ID, remoteFile.Path, workspace.ChangeDelete, "", 0); err != nil {
		t.Fatalf("repeat delete action acknowledgement: %v", err)
	}
	committed, _, err := commitWorkspaceSyncSession(ctx, store, value.ID, deletion.ID, commitRecord)
	if err != nil || committed.State != workspace.SyncCompleted || committed.ConfirmedBytes != 0 {
		t.Fatalf("commit after delete action acknowledgement = %#v err=%v", committed, err)
	}
	cursor, err := store.q.GetWorkspaceSyncCursor(ctx, sqlcgen.GetWorkspaceSyncCursorParams{WorkspaceID: value.ID, ReplicaID: deletion.ReplicaID})
	if err != nil || cursor.Revision != value.Revision || cursor.FilePath != remoteFile.Path {
		t.Fatalf("cursor after delete acknowledgement = %#v err=%v", cursor, err)
	}
}

func syncTestFile(path string, body []byte) workspace.File {
	digest := sha256.Sum256(body)
	sha := hex.EncodeToString(digest[:])
	chunks := []workspace.Chunk{}
	if len(body) > 0 {
		chunks = append(chunks, workspace.Chunk{SHA256: sha, SizeBytes: int64(len(body))})
	}
	return workspace.File{Path: path, SizeBytes: int64(len(body)), SHA256: sha, Mode: 0644, Chunks: chunks}
}

func newTestSyncSession(t *testing.T, value workspace.Workspace, ownerID string, files, upload []workspace.File, deletes []string) workspace.SyncSession {
	t.Helper()
	id, err := workspace.NewSyncSessionID()
	if err != nil {
		t.Fatal(err)
	}
	return workspace.SyncSession{
		ID: id, WorkspaceID: value.ID, OwnerID: ownerID, ReplicaID: "session:" + ownerID,
		Direction: workspace.SyncUpload, Policy: workspace.SyncLive, State: workspace.SyncPlanned, Stage: workspace.SyncStageCompare,
		BaseRevision: value.Revision, TargetRevision: value.Revision, PinnedRevision: value.Revision,
		Files: files, UploadFiles: upload, DownloadFiles: []workspace.File{}, Scope: []string{}, DeletedPaths: []string{},
		UploadDeletedPaths: deletes, Conflicts: []workspace.Conflict{},
	}
}

func syncTestBlocks(files []workspace.File) []workspace.TransferBlock {
	byDigest := make(map[string]workspace.Chunk)
	for _, file := range files {
		for _, chunk := range file.Chunks {
			byDigest[chunk.SHA256] = chunk
		}
	}
	blocks := make([]workspace.TransferBlock, 0, len(byDigest))
	for _, chunk := range byDigest {
		blocks = append(blocks, workspace.TransferBlock{Chunk: chunk, Role: workspace.BlockUpload, Received: true})
	}
	return blocks
}

func createTestWorkspaceSyncSession(ctx context.Context, store *Store, session workspace.SyncSession, transfers []workspace.TransferBlock, idempotency app.IdempotencyRecord) (workspace.SyncSession, bool, error) {
	return createTestWorkspaceSyncSessionWithQuota(ctx, store, session, transfers, syncTestOwnerQuotaBytes, idempotency)
}

func createTestWorkspaceSyncSessionWithQuota(ctx context.Context, store *Store, session workspace.SyncSession, transfers []workspace.TransferBlock, maxOwnerBytes int64, idempotency app.IdempotencyRecord) (workspace.SyncSession, bool, error) {
	reservations := make([]app.WorkspaceBlockReservation, 0)
	seeded := make(map[string]struct{})
	for _, transfer := range transfers {
		if transfer.Role != workspace.BlockUpload {
			continue
		}
		if transfer.Received {
			if _, ok := seeded[transfer.SHA256]; !ok {
				if err := seedTestStoredWorkspaceBlockUsage(ctx, store, session.WorkspaceID, transfer.Chunk); err != nil {
					return workspace.SyncSession{}, false, err
				}
				seeded[transfer.SHA256] = struct{}{}
			}
			continue
		}
		reservations = append(reservations, app.WorkspaceBlockReservation{
			WorkspaceID: session.WorkspaceID, Digest: transfer.SHA256, ReservedBytes: transfer.SizeBytes,
		})
	}
	return store.CreateWorkspaceSyncSession(ctx, session, transfers, reservations, maxOwnerBytes, idempotency)
}

func seedTestStoredWorkspaceBlockUsage(ctx context.Context, store *Store, workspaceID string, chunk workspace.Chunk) error {
	return store.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID string
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&ownerID); err != nil {
			return err
		}
		reservation := app.WorkspaceBlockReservation{WorkspaceID: workspaceID, Digest: chunk.SHA256, ReservedBytes: chunk.SizeBytes}
		if err := reserveWorkspaceBlockUsage(ctx, tx, workspaceID, ownerID, []app.WorkspaceBlockReservation{reservation}, syncTestOwnerQuotaBytes); err != nil {
			return err
		}
		overrun, err := finalizeWorkspaceBlockUsageTx(ctx, tx, workspaceID, chunk.SHA256, chunk.SizeBytes, syncTestOwnerQuotaBytes)
		if err != nil {
			return err
		}
		if overrun {
			return domain.NewError(domain.CodeQuotaExceeded, "test workspace block allocation exceeded quota")
		}
		return nil
	})
}

func syncTestIdempotency(ownerID, operation, key, resourceID string) app.IdempotencyRecord {
	requestHash := sha256.Sum256([]byte(operation + ":" + resourceID))
	return app.IdempotencyRecord{
		OwnerID: ownerID, Operation: operation, Key: key, RequestHash: requestHash[:], ResourceID: resourceID,
	}
}

func commitWorkspaceSyncSession(ctx context.Context, store *Store, workspaceID, sessionID string, idempotency app.IdempotencyRecord) (workspace.SyncSession, bool, error) {
	return store.CommitWorkspaceSyncSession(ctx, workspaceID, sessionID, idempotency)
}
