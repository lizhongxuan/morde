//go:build linux

package workspacefs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

const (
	ext4SuperMagic        = 0xef53
	enospcFreeSpaceBytes  = 2 << 20
	enospcTestVolumeLimit = 256 << 20
)

// TestPutBlockENOSPCRetryAfterSpaceRecovery is opt-in and must run on a
// dedicated ext4 mount supplied through WORKSPACEFS_ENOSPC_DIR.
func TestPutBlockENOSPCRetryAfterSpaceRecovery(t *testing.T) {
	volumeRoot := os.Getenv("WORKSPACEFS_ENOSPC_DIR")
	if volumeRoot == "" {
		t.Skip("set WORKSPACEFS_ENOSPC_DIR to a dedicated ext4 mount")
	}
	volumeRoot, err := filepath.Abs(volumeRoot)
	if err != nil || volumeRoot == string(filepath.Separator) {
		t.Fatalf("WORKSPACEFS_ENOSPC_DIR must name a dedicated ext4 mount, got %q", volumeRoot)
	}
	volumeInfo, err := os.Lstat(volumeRoot)
	if err != nil || !volumeInfo.IsDir() || volumeInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("WORKSPACEFS_ENOSPC_DIR must be a real mount directory: %q", volumeRoot)
	}
	mountInfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatalf("read mount table: %v", err)
	}
	volumeDevice, volumeMounted := mountInfoDevice(string(mountInfo), volumeRoot)
	rootDevice, rootMounted := mountInfoDevice(string(mountInfo), string(filepath.Separator))
	if !volumeMounted || !rootMounted || volumeDevice == rootDevice {
		t.Fatalf("WORKSPACEFS_ENOSPC_DIR must be a separate mount from the system root: %q", volumeRoot)
	}
	var initial syscall.Statfs_t
	if err := syscall.Statfs(volumeRoot, &initial); err != nil {
		t.Fatalf("stat ENOSPC test volume: %v", err)
	}
	if uint64(initial.Type) != ext4SuperMagic {
		t.Fatalf("ENOSPC test volume type = %#x, want ext4 %#x", uint64(initial.Type), ext4SuperMagic)
	}
	if initial.Bsize <= 0 || uint64(initial.Blocks) > uint64(enospcTestVolumeLimit)/uint64(initial.Bsize) {
		t.Fatalf("ENOSPC test volume must be at most %d bytes", enospcTestVolumeLimit)
	}

	scratch, err := os.MkdirTemp(volumeRoot, "workspacefs-enospc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(scratch); err != nil {
			t.Errorf("remove ENOSPC test directory: %v", err)
		}
	})
	store, err := Open(filepath.Join(scratch, "blocks"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close ENOSPC test block store: %v", err)
		}
	})
	workspaceID, err := domain.NewWorkspaceID()
	if err != nil {
		t.Fatal(err)
	}
	ownerID, err := domain.NewUserID()
	if err != nil {
		t.Fatal(err)
	}
	tokenID, err := domain.NewTokenID()
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := workspace.NewSyncSessionID()
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{'x'}, int(MaxBlockBytes))
	digest := digestOf(data)
	file := workspace.File{
		Path: "payload.bin", SizeBytes: MaxBlockBytes, SHA256: digest, Mode: 0644,
		Chunks: []workspace.Chunk{{SHA256: digest, SizeBytes: MaxBlockBytes}},
	}
	transfer := workspace.TransferBlock{Chunk: file.Chunks[0], Role: workspace.BlockUpload}
	value := workspace.Workspace{ID: workspaceID, OwnerID: ownerID, State: workspace.StateReady, QuotaBytes: 1 << 30}
	principal := identity.Principal{
		UserID: ownerID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: tokenID,
		WorkspaceIDs: []string{workspaceID}, Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	syncStore := &enospcSyncTestStore{
		workspace: value,
		session: workspace.SyncSession{
			ID: sessionID, WorkspaceID: workspaceID, OwnerID: ownerID, ReplicaID: "device:" + tokenID,
			Direction: workspace.SyncUpload, Policy: workspace.SyncLive, State: workspace.SyncPlanned,
			Stage: workspace.SyncStageCompare, UploadFiles: []workspace.File{file},
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		},
		blocks: []workspace.TransferBlock{transfer},
	}
	syncService, err := app.NewWorkspaceSyncService(syncStore, store, workspace.MaxSyncManifestBytes, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot, err := store.openWorkspace(workspaceID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := workspaceRoot.Close(); err != nil {
		t.Fatal(err)
	}

	fillerPath := filepath.Join(scratch, "filler")
	filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	var beforeFill syscall.Statfs_t
	if err := syscall.Statfs(volumeRoot, &beforeFill); err != nil {
		_ = filler.Close()
		t.Fatal(err)
	}
	freeBytes := int64(beforeFill.Bfree) * int64(beforeFill.Bsize)
	if freeBytes <= enospcFreeSpaceBytes+MaxBlockBytes {
		_ = filler.Close()
		t.Fatalf("ext4 test volume has only %d free bytes before fill", freeBytes)
	}
	targetBytes := freeBytes - enospcFreeSpaceBytes
	zeroes := make([]byte, 1<<20)
	var filled int64
	for filled < targetBytes {
		want := int64(len(zeroes))
		if remaining := targetBytes - filled; remaining < want {
			want = remaining
		}
		n, writeErr := filler.Write(zeroes[:want])
		filled += int64(n)
		if writeErr != nil {
			if !errors.Is(writeErr, syscall.ENOSPC) {
				_ = filler.Close()
				t.Fatalf("fill ext4 test volume: %v", writeErr)
			}
			break
		}
		if int64(n) != want {
			_ = filler.Close()
			t.Fatalf("short write filling ext4 test volume: wrote %d of %d", n, want)
		}
	}
	if err := filler.Sync(); err != nil && !errors.Is(err, syscall.ENOSPC) {
		_ = filler.Close()
		t.Fatalf("sync ext4 filler: %v", err)
	}
	if err := filler.Close(); err != nil && !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("close ext4 filler: %v", err)
	}
	var full syscall.Statfs_t
	if err := syscall.Statfs(volumeRoot, &full); err != nil {
		t.Fatal(err)
	}
	if available := int64(full.Bfree) * int64(full.Bsize); available >= MaxBlockBytes {
		t.Fatalf("ext4 test volume still has %d bytes; expected less than one %d-byte block", available, MaxBlockBytes)
	}

	if _, err := syncService.PutSyncBlock(context.Background(), principal, workspaceID, sessionID, digest, bytes.NewReader(data)); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("PutSyncBlock on full ext4 volume error = %v, want ENOSPC", err)
	}
	if syncStore.workspace.Revision != 0 || syncStore.blocks[0].Received {
		t.Fatalf("failed upload changed revision or acknowledgement: revision=%d block=%#v", syncStore.workspace.Revision, syncStore.blocks[0])
	}
	if _, err := syncService.CommitSyncSession(context.Background(), principal, workspaceID, sessionID, "enospc-commit-before"); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("commit with failed upload error = %v, want workspace busy", err)
	}
	if syncStore.workspace.Revision != 0 {
		t.Fatalf("failed upload advanced workspace revision to %d", syncStore.workspace.Revision)
	}
	entries, err := os.ReadDir(filepath.Join(scratch, "blocks", workspaceID))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("workspace block directory after failed write contains %v", entryNames(entries))
	}

	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	if err := syncPathDir(scratch); err != nil {
		t.Fatalf("sync ext4 directory after freeing space: %v", err)
	}
	if written, err := syncService.PutSyncBlock(context.Background(), principal, workspaceID, sessionID, digest, bytes.NewReader(data)); err != nil || written != MaxBlockBytes {
		t.Fatalf("PutSyncBlock after ext4 space recovery = (%d, %v)", written, err)
	}
	if _, err := syncService.CommitSyncSession(context.Background(), principal, workspaceID, sessionID, "enospc-commit-after"); err != nil {
		t.Fatalf("commit resumed upload after ext4 space recovery: %v", err)
	}
	if syncStore.workspace.Revision != 1 || syncStore.session.State != workspace.SyncCompleted || !syncStore.blocks[0].Received {
		t.Fatalf("recovered session state = revision %d session=%q block_received=%t", syncStore.workspace.Revision, syncStore.session.State, syncStore.blocks[0].Received)
	}
	if err := store.VerifyBlock(workspaceID, digest, MaxBlockBytes); err != nil {
		t.Fatalf("verify retried block: %v", err)
	}
}

func mountInfoDevice(mountInfo, mountPoint string) (string, bool) {
	encoded := strings.NewReplacer("\\", "\\134", "\t", "\\011", "\n", "\\012", " ", "\\040").Replace(filepath.Clean(mountPoint))
	for _, line := range strings.Split(mountInfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 5 && fields[4] == encoded {
			return fields[2], true
		}
	}
	return "", false
}

func TestMountInfoDeviceMatchesExactEscapedMountPoint(t *testing.T) {
	mountInfo := "22 1 0:41 / / rw - overlay overlay rw\n23 22 7:0 / /tmp/ext4\\040test rw - ext4 /dev/loop0 rw\n"
	if device, ok := mountInfoDevice(mountInfo, "/tmp/ext4 test"); !ok || device != "7:0" {
		t.Fatalf("mountInfoDevice() = (%q, %t), want (7:0, true)", device, ok)
	}
	if _, ok := mountInfoDevice(mountInfo, "/tmp/ext4"); ok {
		t.Fatal("mountInfoDevice matched a mount path prefix")
	}
}

type enospcSyncTestStore struct {
	app.WorkspaceSyncStore
	workspace workspace.Workspace
	session   workspace.SyncSession
	blocks    []workspace.TransferBlock
}

type enospcSyncTestLock struct{}

func (enospcSyncTestLock) Release(context.Context) error { return nil }

func (s *enospcSyncTestStore) GetWorkspace(_ context.Context, workspaceID string) (workspace.Workspace, error) {
	if workspaceID != s.workspace.ID {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return s.workspace, nil
}

func (s *enospcSyncTestStore) TryWorkspaceBlockStoreReadLock(context.Context, string) (app.LeaderLock, bool, error) {
	return enospcSyncTestLock{}, true, nil
}

func (s *enospcSyncTestStore) GetWorkspaceSyncSession(_ context.Context, workspaceID, sessionID string) (workspace.SyncSession, error) {
	if workspaceID != s.workspace.ID || sessionID != s.session.ID {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	return s.session, nil
}

func (s *enospcSyncTestStore) ListWorkspaceSyncTransferBlocks(context.Context, string, string) ([]workspace.TransferBlock, error) {
	return append([]workspace.TransferBlock(nil), s.blocks...), nil
}

func (s *enospcSyncTestStore) ReserveWorkspaceSyncCommit(_ context.Context, workspaceID, sessionID string, record app.IdempotencyRecord) (app.WorkspaceSyncCommitReceipt, error) {
	if workspaceID != s.workspace.ID || sessionID != s.session.ID || record.Operation != "workspace.sync.commit" || record.ResourceID != sessionID {
		return app.WorkspaceSyncCommitReceipt{}, domain.NewError(domain.CodeNotFound, "workspace sync commit receipt not found")
	}
	return app.WorkspaceSyncCommitReceipt{State: app.WorkspaceSyncCommitPending}, nil
}

func (s *enospcSyncTestStore) InspectWorkspaceSyncCommit(_ context.Context, workspaceID, sessionID string, record app.IdempotencyRecord) (app.WorkspaceSyncCommitReceipt, error) {
	if workspaceID != s.workspace.ID || sessionID != s.session.ID || record.Operation != "workspace.sync.commit" || record.ResourceID != sessionID {
		return app.WorkspaceSyncCommitReceipt{}, domain.NewError(domain.CodeNotFound, "workspace sync commit receipt not found")
	}
	return app.WorkspaceSyncCommitReceipt{State: app.WorkspaceSyncCommitPending}, nil
}

func (s *enospcSyncTestStore) AckWorkspaceSyncBlock(_ context.Context, _, _, digest string, _, _ int64) error {
	for i := range s.blocks {
		if s.blocks[i].SHA256 == digest && s.blocks[i].Role == workspace.BlockUpload {
			s.blocks[i].Received = true
			s.session.ConfirmedBytes += s.blocks[i].SizeBytes
			s.session.TransferBlocks = append([]workspace.TransferBlock(nil), s.blocks...)
			return nil
		}
	}
	return domain.NewError(domain.CodeNotFound, "workspace sync block not found")
}

func (s *enospcSyncTestStore) CommitWorkspaceSyncSession(_ context.Context, workspaceID, sessionID string, _ app.IdempotencyRecord) (workspace.SyncSession, bool, error) {
	if workspaceID != s.workspace.ID || sessionID != s.session.ID {
		return workspace.SyncSession{}, false, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	for _, block := range s.blocks {
		if block.Role == workspace.BlockUpload && !block.Received {
			return workspace.SyncSession{}, false, domain.NewError(domain.CodeWorkspaceBusy, "workspace sync block is not yet acknowledged")
		}
	}
	s.workspace.Revision++
	s.session.State = workspace.SyncCompleted
	s.session.Stage = workspace.SyncStageComplete
	s.session.TargetRevision = s.workspace.Revision
	s.session.TransferBlocks = append([]workspace.TransferBlock(nil), s.blocks...)
	return s.session, false, nil
}
