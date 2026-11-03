package app

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type workspaceCopyServiceStore struct {
	*syncServiceStore
	reservation    workspace.CopyReservation
	record         IdempotencyRecord
	lockHeld       bool
	stagedFiles    []workspace.File
	stagedDirs     []string
	stagedMetadata []workspace.Directory
	staged         int
	completed      int
}

func (s *workspaceCopyServiceStore) GetWorkspaceCopyReservationByIdempotency(_ context.Context, record IdempotencyRecord,
	name, serviceInstanceID string, quota int64) (workspace.CopyReservation, bool, error) {
	if s.reservation.ID == "" || record.Key != s.record.Key {
		return workspace.CopyReservation{}, false, nil
	}
	if record.OwnerID != s.record.OwnerID || record.ResourceID != s.record.ResourceID ||
		!bytes.Equal(record.RequestHash, s.record.RequestHash) || name != "copy" || serviceInstanceID != "instance-1" || quota != 1<<30 {
		return workspace.CopyReservation{}, false, domain.NewError(domain.CodeIdempotencyConflict, "copy key mismatch")
	}
	return s.reservation, true, nil
}

func (s *workspaceCopyServiceStore) CreateWorkspaceCopyReservation(_ context.Context, ownerID, sourceID, targetID, name, instanceID, reservationID string,
	revision, reservedBytes, maxOwnerBytes, quota int64, record IdempotencyRecord) (workspace.CopyReservation, bool, error) {
	if s.reservation.ID != "" {
		return s.reservation, true, nil
	}
	s.record = record
	s.reservation = workspace.CopyReservation{ID: reservationID, OwnerID: ownerID, SourceWorkspaceID: sourceID,
		TargetWorkspaceID: targetID, ServiceInstanceID: instanceID, SourceRevision: revision, ReservedBytes: reservedBytes,
		State: workspace.CopyReserved}
	return s.reservation, false, nil
}

func (s *workspaceCopyServiceStore) GetWorkspaceManifestPageForCopy(ctx context.Context, ownerID, reservationID string,
	revision int64, prefix, after string, limit int32) (workspace.ManifestPage, error) {
	if ownerID != s.reservation.OwnerID || reservationID != s.reservation.ID || revision != s.reservation.SourceRevision || !s.reservation.State.Active() {
		return workspace.ManifestPage{}, domain.NewError(domain.CodeNotFound, "copy manifest unavailable")
	}
	return s.GetWorkspaceManifestPage(ctx, s.reservation.SourceWorkspaceID, revision, prefix, after, limit)
}

func (s *workspaceCopyServiceStore) TryWorkspaceCopyLock(context.Context, string) (LeaderLock, bool, error) {
	if s.lockHeld {
		return nil, false, nil
	}
	s.lockHeld = true
	return workspaceCopyTestLock{store: s}, true, nil
}

type workspaceCopyTestLock struct{ store *workspaceCopyServiceStore }

func (lock workspaceCopyTestLock) Release(context.Context) error {
	lock.store.lockHeld = false
	return nil
}

func (s *workspaceCopyServiceStore) SetWorkspaceCopyReservationState(_ context.Context, id string, from, to workspace.CopyState) error {
	if id != s.reservation.ID || s.reservation.State != from {
		return domain.NewError(domain.CodeWorkspaceConflict, "copy state changed")
	}
	s.reservation.State = to
	return nil
}

func (s *workspaceCopyServiceStore) FailWorkspaceCopyReservationBeforeSideEffects(_ context.Context, ownerID, id string) error {
	if ownerID != s.reservation.OwnerID || id != s.reservation.ID || s.reservation.State != workspace.CopyReserved {
		return domain.NewError(domain.CodeWorkspaceConflict, "copy cannot be safely released")
	}
	s.reservation.State = workspace.CopyFailed
	return nil
}

func (s *workspaceCopyServiceStore) CheckWorkspaceVolumeCopyReservation(_ context.Context, request WorkspaceVolumeCopyRequest) error {
	if request.ReservationID != s.reservation.ID || !s.reservation.State.Active() {
		return domain.NewError(domain.CodeWorkspaceBusy, "copy reservation inactive")
	}
	return nil
}

func (s *workspaceCopyServiceStore) StageWorkspaceCopy(_ context.Context, id, ownerID, name string, files []workspace.File,
	directories []string, metadata []workspace.Directory, allocations []WorkspaceBlockAllocation, quota int64) error {
	if id != s.reservation.ID || ownerID != s.reservation.OwnerID || name != "copy" || quota != 1<<30 || len(allocations) != 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "invalid staged copy")
	}
	s.stagedFiles = append([]workspace.File(nil), files...)
	s.stagedDirs = append([]string(nil), directories...)
	s.stagedMetadata = append([]workspace.Directory(nil), metadata...)
	s.staged++
	return nil
}

func (s *workspaceCopyServiceStore) CompleteWorkspaceCopy(_ context.Context, id, ownerID string, maxOwnerBytes int64) error {
	if id != s.reservation.ID || ownerID != s.reservation.OwnerID || maxOwnerBytes != 1<<30 || s.staged == 0 {
		return domain.NewError(domain.CodeWorkspaceConflict, "invalid completed copy")
	}
	s.reservation.State = workspace.CopyCompleted
	s.completed++
	return nil
}

type workspaceCopyServiceVolumes struct{ checkedBytes, checks, ensures int64 }

func (v *workspaceCopyServiceVolumes) EnsureWorkspaceVolume(context.Context, string, string, string, string) error {
	v.ensures++
	return nil
}

func (*workspaceCopyServiceVolumes) VerifyWorkspaceVolume(context.Context, string, string, string, string) error {
	return nil
}

func (v *workspaceCopyServiceVolumes) CheckCapacityForWrite(_ context.Context, bytes int64) error {
	v.checkedBytes, v.checks = bytes, v.checks+1
	return nil
}

type workspaceCopyServiceCopier struct {
	files        []workspace.File
	directories  []workspace.Directory
	preflightErr error
	preflights   int
	err          error
	calls        int
}

func (c *workspaceCopyServiceCopier) ValidateWorkspaceVolumeCopySource(ctx context.Context, request WorkspaceVolumeCopyRequest,
	reservation WorkspaceVolumeCopyReservationChecker) error {
	c.preflights++
	if err := reservation.CheckWorkspaceVolumeCopyReservation(ctx, request); err != nil {
		return err
	}
	return c.preflightErr
}

func (c *workspaceCopyServiceCopier) CopyWorkspaceVolume(ctx context.Context, request WorkspaceVolumeCopyRequest,
	reservation WorkspaceVolumeCopyReservationChecker) ([]workspace.File, []workspace.Directory, error) {
	c.calls++
	if err := reservation.CheckWorkspaceVolumeCopyReservation(ctx, request); err != nil {
		return nil, nil, err
	}
	files := c.files
	if files == nil {
		files = request.Files
	}
	return files, c.directories, c.err
}

func newWorkspaceCopyServiceForTest(t *testing.T, copier WorkspaceVolumeCopier) (*WorkspaceCopyService, *workspaceCopyServiceStore,
	*workspaceCopyServiceVolumes, workspace.File) {
	t.Helper()
	syncStore := newSyncServiceStore()
	file := syncServiceFile(".hidden/notes.txt", []byte("copy me"))
	modified := int64(1_700_000_000_123_000_000)
	file.ModTimeNS = &modified
	file.MetadataKnown = true
	syncStore.workspace.Revision, syncStore.workspace.UsedBytes = 1, file.SizeBytes
	syncStore.snapshots[1] = []workspace.File{file}
	rootMode, rootTime := uint32(0o755), int64(1_700_000_000_000_000_000)
	rootDirectory := workspace.Directory{Path: ".", Mode: &rootMode, ModTimeNS: &rootTime, MetadataKnown: true}
	if copyCopier, ok := copier.(*workspaceCopyServiceCopier); ok {
		for index := range copyCopier.directories {
			copyCopier.directories[index].MetadataKnown = true
		}
		foundRoot := false
		for _, directory := range copyCopier.directories {
			foundRoot = foundRoot || directory.Path == "."
		}
		if !foundRoot {
			copyCopier.directories = append([]workspace.Directory{rootDirectory}, copyCopier.directories...)
		}
	}
	metadata := []workspace.Directory{rootDirectory}
	if copyCopier, ok := copier.(*workspaceCopyServiceCopier); ok {
		for _, directory := range copyCopier.directories {
			if directory.Path != "." {
				metadata = append(metadata, directory)
			}
		}
	}
	syncStore.directorySnapshots[1] = metadata
	blocks := newSyncBlockMemory()
	blocks.data[file.Chunks[0].SHA256] = []byte("copy me")
	store := &workspaceCopyServiceStore{syncServiceStore: syncStore}
	syncService, err := NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncService.SetWorkspaceCurrentManifestVerifier(func(_ context.Context, ownerID, workspaceID string, files []workspace.File, dirs []string, _ []workspace.Directory) (bool, error) {
		matches := len(files) == 1 && files[0].Path == file.Path && files[0].SizeBytes == file.SizeBytes &&
			files[0].SHA256 == file.SHA256 && files[0].Mode == file.Mode && reflect.DeepEqual(files[0].Chunks, file.Chunks)
		return ownerID == syncStore.workspace.OwnerID && workspaceID == syncStore.workspace.ID && matches && len(dirs) == 0, nil
	}); err != nil {
		t.Fatal(err)
	}
	volumes := &workspaceCopyServiceVolumes{}
	service, err := NewWorkspaceCopyService(store, syncService, blocks, volumes, copier, "instance-1", 1<<30, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetCurrentMaterializer(func(_ context.Context, ownerID, workspaceID string, revision int64, files []workspace.File, dirs []string, metadata []workspace.Directory) error {
		if ownerID != syncStore.workspace.OwnerID || workspaceID != store.reservation.TargetWorkspaceID || revision != 1 ||
			!reflect.DeepEqual(files, workspaceCopyTestExpectedFiles(file, copier)) || len(dirs) != 0 || len(metadata) != 2 ||
			metadata[0].Path != "." || metadata[1].Path != ".hidden" {
			return errors.New("materializer received the wrong copied checkpoint")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return service, store, volumes, file
}

func workspaceCopyTestExpectedFiles(file workspace.File, copier WorkspaceVolumeCopier) []workspace.File {
	if value, ok := copier.(*workspaceCopyServiceCopier); ok && value.files != nil {
		return value.files
	}
	return []workspace.File{file}
}

func TestWorkspaceCopyServiceCopiesConfirmedCheckpointAndReplaysAfterSourceChanges(t *testing.T) {
	mode, modified := uint32(0o750), int64(1_700_000_000_123_000_000)
	copier := &workspaceCopyServiceCopier{directories: []workspace.Directory{{Path: ".hidden", Mode: &mode, ModTimeNS: &modified}}}
	service, store, volumes, file := newWorkspaceCopyServiceForTest(t, copier)
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	result, err := service.Copy(context.Background(), principal, "w1234567", WorkspaceCopyCommand{Name: "copy", ExpectedCurrentRevision: 1}, "copy-key-01")
	if err != nil {
		t.Fatalf("Copy() = %v", err)
	}
	if result.State != workspace.CopyCompleted || result.Replayed || result.SourceRevision != 1 || result.Workspace.State != workspace.StateReady ||
		store.workspace.Revision != 1 || store.completed != 1 || copier.calls != 1 || volumes.checks != 1 || volumes.checkedBytes != file.SizeBytes ||
		!reflect.DeepEqual(store.stagedFiles, []workspace.File{file}) || !reflect.DeepEqual(store.stagedMetadata, copier.directories) {
		t.Fatalf("copy result/state mismatch: result=%#v store=%#v copier=%#v volumes=%#v", result, store, copier, volumes)
	}
	store.workspace.Revision = 2
	replay, err := service.Copy(context.Background(), principal, "w1234567", WorkspaceCopyCommand{Name: "copy", ExpectedCurrentRevision: 1}, "copy-key-01")
	if err != nil || !replay.Replayed || replay.Workspace.ID != result.Workspace.ID || copier.calls != 1 {
		t.Fatalf("same-key replay after source changes = %#v, %v; copier calls=%d", replay, err, copier.calls)
	}
}

func TestWorkspaceCopyPreflightFailureReleasesReservationBeforeSideEffects(t *testing.T) {
	wantErr := errors.New("source contains unsupported hard links")
	copier := &workspaceCopyServiceCopier{preflightErr: wantErr}
	service, store, volumes, _ := newWorkspaceCopyServiceForTest(t, copier)
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	_, err := service.Copy(context.Background(), principal, "w1234567", WorkspaceCopyCommand{Name: "copy", ExpectedCurrentRevision: 1}, "copy-key-02")
	if !errors.Is(err, wantErr) || store.reservation.State != workspace.CopyFailed || copier.preflights != 1 ||
		copier.calls != 0 || volumes.ensures != 0 {
		t.Fatalf("Copy() = %v, state=%s, preflights=%d, copies=%d, volume ensures=%d; want failed reservation before side effects",
			err, store.reservation.State, copier.preflights, copier.calls, volumes.ensures)
	}
}

func TestWorkspaceCopyPromotesVerifiedFileTimestampToNewRevision(t *testing.T) {
	mode, directoryTime := uint32(0o750), int64(1_700_000_000_123_000_000)
	copier := &workspaceCopyServiceCopier{directories: []workspace.Directory{{Path: ".hidden", Mode: &mode, ModTimeNS: &directoryTime}}}
	service, store, _, file := newWorkspaceCopyServiceForTest(t, copier)
	file.ModTimeNS = nil
	store.snapshots[1][0].ModTimeNS = nil
	copiedAt := int64(1_700_000_100_123_000_000)
	copier.files = []workspace.File{file}
	copier.files[0].ModTimeNS = &copiedAt
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	result, err := service.Copy(context.Background(), principal, "w1234567", WorkspaceCopyCommand{Name: "copy", ExpectedCurrentRevision: 1}, "copy-key-file-time")
	if err != nil || result.State != workspace.CopyCompleted || len(store.stagedFiles) != 1 || store.stagedFiles[0].ModTimeNS == nil ||
		*store.stagedFiles[0].ModTimeNS != copiedAt {
		t.Fatalf("copy with unknown source timestamp did not persist verified copied timestamp: result=%#v staged=%#v err=%v", result, store.stagedFiles, err)
	}
}

func TestWorkspaceCopyServiceRetriesUnknownCopyThroughReservationManifest(t *testing.T) {
	mode, modified := uint32(0o750), int64(1_700_000_000_123_000_000)
	copier := &workspaceCopyServiceCopier{directories: []workspace.Directory{{Path: ".hidden", Mode: &mode, ModTimeNS: &modified}}, err: errors.New("docker copy response lost")}
	service, store, _, _ := newWorkspaceCopyServiceForTest(t, copier)
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	command := WorkspaceCopyCommand{Name: "copy", ExpectedCurrentRevision: 1}
	if _, err := service.Copy(context.Background(), principal, "w1234567", command, "copy-key-02"); err == nil {
		t.Fatal("first copy succeeded despite unknown Docker result")
	}
	if store.reservation.State != workspace.CopyUnknown {
		t.Fatalf("copy reservation state = %q, want unknown", store.reservation.State)
	}
	copier.err = nil
	result, err := service.Copy(context.Background(), principal, "w1234567", command, "copy-key-02")
	if err != nil || result.State != workspace.CopyCompleted || !result.Replayed || copier.calls != 2 || store.completed != 1 {
		t.Fatalf("unknown copy retry = %#v, %v; copier calls=%d store=%#v", result, err, copier.calls, store)
	}
}
