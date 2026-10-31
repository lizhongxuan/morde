package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func TestDescribeWorkspaceReceiveFileBuildsManifest(t *testing.T) {
	for _, content := range [][]byte{[]byte("received Workspace file"), {}} {
		path := filepath.Join(t.TempDir(), "received")
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}

		manifest, err := describeWorkspaceReceiveFile(file, WorkspaceSyncReceiveReservation{
			Path: "received", TargetBytes: int64(len(content)),
		})
		closeErr := file.Close()
		if err != nil {
			t.Fatalf("describeWorkspaceReceiveFile() error = %v", err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}

		digest := sha256.Sum256(content)
		if manifest.Path != "received" || manifest.SizeBytes != int64(len(content)) || manifest.SHA256 != hex.EncodeToString(digest[:]) {
			t.Fatalf("manifest = %+v, want path/size/content digest for %q", manifest, content)
		}
		if len(content) == 0 && len(manifest.Chunks) != 0 {
			t.Fatalf("empty file produced chunks: %+v", manifest.Chunks)
		}
		if len(content) > 0 && (len(manifest.Chunks) != 1 || manifest.Chunks[0].SizeBytes != int64(len(content)) || manifest.Chunks[0].SHA256 != manifest.SHA256) {
			t.Fatalf("manifest chunks = %+v, want one matching content chunk", manifest.Chunks)
		}
	}
}

func TestDescribeWorkspaceReceiveFileRejectsSizeDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "received")
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("close received Workspace file: %v", err)
		}
	})

	if _, err := describeWorkspaceReceiveFile(file, WorkspaceSyncReceiveReservation{Path: "received", TargetBytes: 4}); err == nil {
		t.Fatal("describeWorkspaceReceiveFile() accepted a file whose size changed")
	}
}

func TestWorkspaceSyncFileDeleteRequiresMissingPath(t *testing.T) {
	_, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := workspaceSyncFileDeleteError(err); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("delete settlement for an existing directory = %v; want workspace conflict", err)
	}
	if err := workspaceSyncFileDeleteError(os.ErrNotExist); err != nil {
		t.Fatalf("delete settlement for a missing path = %v; want nil", err)
	}
}

type quotaReceiveTestStore struct {
	*syncServiceStore
	reservation     WorkspaceSyncReceiveReservation
	activeReceives  []WorkspaceSyncReceiveReservation
	beginAbortCalls int
	completeCalls   int
	beginAbortErr   error
	lockReleaseErr  error
	lockAttempts    int
}

func (s *quotaReceiveTestStore) ReserveWorkspaceSyncReceive(context.Context, WorkspaceSyncReceiveRequest, string, []byte) (WorkspaceSyncReceiveReservation, error) {
	return WorkspaceSyncReceiveReservation{}, errors.New("unused")
}

func (s *quotaReceiveTestStore) ReserveWorkspaceSyncReceiveBlockUsage(context.Context, string, string, string, []WorkspaceBlockReservation, int64) error {
	return errors.New("unused")
}

func (s *quotaReceiveTestStore) GetWorkspaceSyncReceiveReservation(context.Context, string, string) (WorkspaceSyncReceiveReservation, error) {
	return s.reservation, nil
}

func (s *quotaReceiveTestStore) CommitWorkspaceSyncReceive(context.Context, string, string, string, *workspace.File, *string) (WorkspaceSyncReceiveReceipt, error) {
	return WorkspaceSyncReceiveReceipt{}, errors.New("unused")
}

func (s *quotaReceiveTestStore) ListActiveWorkspaceSyncReceiveReservations(context.Context) ([]WorkspaceSyncReceiveReservation, error) {
	return s.activeReceives, nil
}

func (s *quotaReceiveTestStore) ListActiveWorkspaceSyncReceiveOwners(context.Context) ([]string, error) {
	return nil, errors.New("unused")
}

func (s *quotaReceiveTestStore) BeginAbortWorkspaceSyncReceive(context.Context, string, string, string) (WorkspaceSyncReceiveReservation, error) {
	s.beginAbortCalls++
	if s.beginAbortErr != nil {
		return WorkspaceSyncReceiveReservation{}, s.beginAbortErr
	}
	s.reservation.State = "aborting"
	return s.reservation, nil
}

func (s *quotaReceiveTestStore) CompleteAbortWorkspaceSyncReceive(context.Context, string, string, string) error {
	s.completeCalls++
	s.reservation.State = "aborted"
	return nil
}

func (s *quotaReceiveTestStore) TryWorkspaceBlockStoreLock(ctx context.Context, _ string) (LeaderLock, bool, error) {
	s.lockAttempts++
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if s.blockLockBusy {
		return nil, false, nil
	}
	return quotaReceiveTestLock{err: s.lockReleaseErr}, true, nil
}

type quotaReceiveTestLock struct{ err error }

func (l quotaReceiveTestLock) Release(context.Context) error { return l.err }

type quotaReceiveTestBlocks struct{}

func (quotaReceiveTestBlocks) Open(string, string) (io.ReadCloser, error) {
	return nil, errors.New("unused")
}

func (quotaReceiveTestBlocks) PutBlock(string, string, io.Reader) (int64, error) {
	return 0, errors.New("unused")
}

func (quotaReceiveTestBlocks) ReservationBytes(int64) (int64, error) { return 0, errors.New("unused") }

func (quotaReceiveTestBlocks) BlockAllocatedBytes(string, string) (int64, error) {
	return 0, errors.New("unused")
}

func newQuotaReceiveTestService(t *testing.T, store *quotaReceiveTestStore) *WorkspaceSyncService {
	t.Helper()
	service, err := NewWorkspaceSyncService(store, quotaReceiveTestBlocks{}, 1024, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func newQuotaReceiveTestStore(t *testing.T) *quotaReceiveTestStore {
	t.Helper()
	reservationID, err := domain.NewPendingRequestID()
	if err != nil {
		t.Fatal(err)
	}
	return &quotaReceiveTestStore{
		syncServiceStore: newSyncServiceStore(),
		reservation: WorkspaceSyncReceiveReservation{
			ID: reservationID, OwnerID: "u1234567", WorkspaceID: "w1234567", State: "reserved",
		},
	}
}

type recoveryQuotaReceiveStore struct {
	*quotaReceiveTestStore
	materializationMarks int
}

func (s *recoveryQuotaReceiveStore) ListWorkspaceCurrentMaterializations(context.Context, string, int32) ([]WorkspaceCurrentMaterialization, error) {
	return nil, nil
}

func (s *recoveryQuotaReceiveStore) MarkWorkspaceCurrentMaterialized(context.Context, string, int64) error {
	s.materializationMarks++
	return nil
}

func (*recoveryQuotaReceiveStore) BeginWorkspaceCurrentMaterialization(context.Context, string, int64, string) error {
	return nil
}

func (s *recoveryQuotaReceiveStore) MarkWorkspaceCurrentMaterializedForReceive(context.Context, string, int64, string) error {
	s.materializationMarks++
	return nil
}

func TestReconcileRecoversActiveReceiveWithoutPendingMaterialization(t *testing.T) {
	store := &recoveryQuotaReceiveStore{quotaReceiveTestStore: newQuotaReceiveTestStore(t)}
	store.activeReceives = []WorkspaceSyncReceiveReservation{store.reservation}
	service, err := NewWorkspaceSyncService(store, quotaReceiveTestBlocks{}, 1024, 1024)
	if err != nil {
		t.Fatal(err)
	}
	materialized := false
	if err := service.SetWorkspaceCurrentMaterializer(func(context.Context, string, string, int64, []workspace.File, []string) error {
		materialized = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.SetWorkspaceCurrentRevisionReader(func(string, string) (int64, bool, error) {
		return 0, true, nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := service.ReconcilePendingWorkspaceCurrentMaterializations(context.Background()); err != nil {
		t.Fatalf("reconcile active receive without a pending row: %v", err)
	}
	if store.beginAbortCalls != 1 || store.completeCalls != 1 || store.reservation.State != "aborted" {
		t.Fatalf("receive recovery = begin %d, complete %d, state %q; want 1, 1, aborted", store.beginAbortCalls, store.completeCalls, store.reservation.State)
	}
	if !materialized || store.materializationMarks != 1 {
		t.Fatalf("receive recovery materialized=%t marks=%d; want true and 1", materialized, store.materializationMarks)
	}
}

type startupMaterializationTestStore struct {
	*quotaReceiveTestStore
	rows                 []WorkspaceCurrentMaterialization
	materializationMarks int
}

func (s *startupMaterializationTestStore) ListWorkspaceCurrentMaterializations(_ context.Context, after string, _ int32) ([]WorkspaceCurrentMaterialization, error) {
	if after != "" || len(s.rows) == 0 {
		return nil, nil
	}
	return s.rows, nil
}

func (s *startupMaterializationTestStore) MarkWorkspaceCurrentMaterialized(context.Context, string, int64) error {
	s.materializationMarks++
	return nil
}

func (*startupMaterializationTestStore) BeginWorkspaceCurrentMaterialization(context.Context, string, int64, string) error {
	return nil
}

func (*startupMaterializationTestStore) MarkWorkspaceCurrentMaterializedForReceive(context.Context, string, int64, string) error {
	return nil
}

func TestStartupReconcileVerifiesProjectionBeforeSkippingMaterialization(t *testing.T) {
	content := []byte("durable")
	digest := sha256.Sum256(content)
	file := workspace.File{Path: "result.txt", SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), Mode: 0640,
		Chunks: []workspace.Chunk{{SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(content))}}}
	for _, matches := range []bool{true, false} {
		t.Run(map[bool]string{true: "matches", false: "drifted"}[matches], func(t *testing.T) {
			base := newQuotaReceiveTestStore(t)
			base.workspace.Revision = 1
			base.workspace.UsedBytes = int64(len(content))
			base.snapshots[1] = []workspace.File{file}
			store := &startupMaterializationTestStore{
				quotaReceiveTestStore: base,
				rows:                  []WorkspaceCurrentMaterialization{{WorkspaceID: base.workspace.ID, OwnerID: base.workspace.OwnerID, DesiredRevision: 1, AppliedRevision: -1}},
			}
			service, err := NewWorkspaceSyncService(store, quotaReceiveTestBlocks{}, 1024, 1024)
			if err != nil {
				t.Fatal(err)
			}
			materializations := 0
			if err := service.SetWorkspaceCurrentMaterializer(func(_ context.Context, _ string, _ string, revision int64, files []workspace.File, _ []string) error {
				materializations++
				if revision != 1 || len(files) != 1 || files[0].Path != file.Path {
					t.Fatalf("materialization revision/files = %d/%+v", revision, files)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := service.SetWorkspaceCurrentRevisionReader(func(string, string) (int64, bool, error) { return 1, true, nil }); err != nil {
				t.Fatal(err)
			}
			verifications := 0
			if err := service.SetWorkspaceCurrentManifestVerifier(func(_ context.Context, ownerID, workspaceID string, files []workspace.File, _ []string) (bool, error) {
				verifications++
				if ownerID != base.workspace.OwnerID || workspaceID != base.workspace.ID || len(files) != 1 || files[0].SHA256 != file.SHA256 {
					t.Fatalf("verified Workspace manifest = %s/%s/%+v", ownerID, workspaceID, files)
				}
				return matches, nil
			}); err != nil {
				t.Fatal(err)
			}

			if err := service.ReconcilePendingWorkspaceCurrentMaterializations(context.Background()); err != nil {
				t.Fatalf("startup reconciliation: %v", err)
			}
			wantMaterializations := 0
			if !matches {
				wantMaterializations = 1
			}
			if verifications != 1 || materializations != wantMaterializations || store.materializationMarks != 1 {
				t.Fatalf("verifications=%d materializations=%d marks=%d; want 1/%d/1", verifications, materializations, store.materializationMarks, wantMaterializations)
			}
		})
	}
}

func TestAbortWorkspaceSyncReceiveDoesNotAdvanceWhenBlockStoreLockIsBusy(t *testing.T) {
	store := newQuotaReceiveTestStore(t)
	store.blockLockBusy = true
	service := newQuotaReceiveTestService(t, store)

	err := service.AbortWorkspaceSyncReceive(context.Background(), store.reservation.OwnerID, store.reservation.ID)
	if domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("abort without block-store lock = %v; want workspace busy", err)
	}
	if store.beginAbortCalls != 0 || store.reservation.State != "reserved" {
		t.Fatalf("abort advanced without block-store lock: calls=%d state=%q", store.beginAbortCalls, store.reservation.State)
	}
}

func TestAbortWorkspaceSyncReceiveReturnsBlockStoreLockReleaseError(t *testing.T) {
	store := newQuotaReceiveTestStore(t)
	beginErr := errors.New("begin abort failed")
	releaseErr := errors.New("release lock failed")
	store.beginAbortErr = beginErr
	store.lockReleaseErr = releaseErr
	service := newQuotaReceiveTestService(t, store)

	err := service.AbortWorkspaceSyncReceive(context.Background(), store.reservation.OwnerID, store.reservation.ID)
	if !errors.Is(err, beginErr) || !errors.Is(err, releaseErr) {
		t.Fatalf("abort error = %v; want both begin and lock-release errors", err)
	}
}
