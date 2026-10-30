package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type syncServiceStore struct {
	workspace                                workspace.Workspace
	binding                                  workspace.Binding
	snapshots                                map[int64][]workspace.File
	sessions                                 map[string]workspace.SyncSession
	commitReceipts                           map[string]WorkspaceSyncCommitReceipt
	taskWorkspacePublishReceipts             map[string]TaskWorkspacePublishReceipt
	taskWorkspacePublishCommands             map[string]TaskWorkspacePublishCommand
	blocks                                   map[string][]workspace.TransferBlock
	changes                                  []workspace.Change
	progress                                 []workspace.DeviceSyncProgress
	progressOwner, progressWorkspace         string
	blockLockBusy, blockReadLockBusy         bool
	blockLockAcquires, blockReadLockAcquires int
	blockLockReleases, blockReadLockReleases int
	blockLockHeld                            bool
	blockReadLocksHeld                       int
	onCreate                                 func()
	syncDeviceRevoked                        bool
	usageFinalizeCalls                       int
}

type syncServiceBlockLock struct {
	store  *syncServiceStore
	shared bool
}

func (lock syncServiceBlockLock) Release(context.Context) error {
	if lock.shared {
		lock.store.blockReadLockReleases++
		lock.store.blockReadLocksHeld--
	} else {
		lock.store.blockLockReleases++
		lock.store.blockLockHeld = false
	}
	return nil
}

func newSyncServiceStore() *syncServiceStore {
	return &syncServiceStore{
		workspace: workspace.Workspace{ID: "w1234567", OwnerID: "u1234567", Name: "test", State: workspace.StateReady, QuotaBytes: 1 << 30},
		snapshots: map[int64][]workspace.File{0: {}}, sessions: map[string]workspace.SyncSession{}, commitReceipts: map[string]WorkspaceSyncCommitReceipt{},
		blocks: map[string][]workspace.TransferBlock{}, changes: []workspace.Change{},
		taskWorkspacePublishReceipts: map[string]TaskWorkspacePublishReceipt{}, taskWorkspacePublishCommands: map[string]TaskWorkspacePublishCommand{},
	}
}

func taskWorkspacePublishReceiptKey(taskID, operationKey string) string {
	return taskID + "/" + operationKey
}

func (s *syncServiceStore) ReserveTaskWorkspacePublish(_ context.Context, value TaskWorkspacePublishCommand) (TaskWorkspacePublishReceipt, error) {
	key := taskWorkspacePublishReceiptKey(value.TaskID, value.OperationKey)
	if receipt, exists := s.taskWorkspacePublishReceipts[key]; exists {
		command := s.taskWorkspacePublishCommands[key]
		if command.WorkspaceID != value.WorkspaceID || command.BaseRevision != value.BaseRevision || !bytes.Equal(command.RequestHash, value.RequestHash) {
			return TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeIdempotencyConflict, "Task Workspace publish key was reused")
		}
		return receipt, nil
	}
	receipt := TaskWorkspacePublishReceipt{State: TaskWorkspacePublishPending, WorkspaceID: value.WorkspaceID, BaseRevision: value.BaseRevision}
	s.taskWorkspacePublishReceipts[key], s.taskWorkspacePublishCommands[key] = receipt, value
	return receipt, nil
}

func (s *syncServiceStore) GetTaskWorkspacePublishReceipt(_ context.Context, value TaskWorkspacePublishCommand) (TaskWorkspacePublishReceipt, error) {
	key := taskWorkspacePublishReceiptKey(value.TaskID, value.OperationKey)
	receipt, exists := s.taskWorkspacePublishReceipts[key]
	if !exists {
		return TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeNotFound, "Task Workspace publish receipt not found")
	}
	command := s.taskWorkspacePublishCommands[key]
	if command.WorkspaceID != value.WorkspaceID || command.BaseRevision != value.BaseRevision || !bytes.Equal(command.RequestHash, value.RequestHash) {
		return TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeIdempotencyConflict, "Task Workspace publish key was reused")
	}
	return receipt, nil
}

func (s *syncServiceStore) FindTaskWorkspacePublishReceipt(_ context.Context, ownerID, taskID, workspaceID, operationKey string) (TaskWorkspacePublishReceipt, error) {
	key := taskWorkspacePublishReceiptKey(taskID, operationKey)
	receipt, exists := s.taskWorkspacePublishReceipts[key]
	command := s.taskWorkspacePublishCommands[key]
	if !exists || command.OwnerID != ownerID || command.WorkspaceID != workspaceID {
		return TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeNotFound, "Task Workspace publish receipt not found")
	}
	return receipt, nil
}

func (s *syncServiceStore) CommitTaskWorkspacePublish(_ context.Context, value TaskWorkspacePublishCommand, files []workspace.File, manifestSHA string) (TaskWorkspacePublishReceipt, error) {
	key := taskWorkspacePublishReceiptKey(value.TaskID, value.OperationKey)
	receipt, exists := s.taskWorkspacePublishReceipts[key]
	if !exists || receipt.State != TaskWorkspacePublishPending {
		return TaskWorkspacePublishReceipt{}, domain.NewError(domain.CodeNotFound, "pending Task Workspace publish receipt not found")
	}
	if current := s.workspace.Revision; current != value.BaseRevision {
		receipt.State = TaskWorkspacePublishConflict
		s.taskWorkspacePublishReceipts[key] = receipt
		return receipt, nil
	}
	revision := value.BaseRevision
	if !reflect.DeepEqual(s.snapshots[value.BaseRevision], files) {
		revision++
	}
	receipt.State, receipt.ManifestSHA256, receipt.PublishedRevision = TaskWorkspacePublishSucceeded, manifestSHA, revision
	s.taskWorkspacePublishReceipts[key] = receipt
	s.workspace.Revision, s.snapshots[revision] = revision, append([]workspace.File(nil), files...)
	return receipt, nil
}

func (s *syncServiceStore) RejectTaskWorkspacePublishQuota(_ context.Context, value TaskWorkspacePublishCommand) error {
	key := taskWorkspacePublishReceiptKey(value.TaskID, value.OperationKey)
	receipt, exists := s.taskWorkspacePublishReceipts[key]
	if !exists {
		return domain.NewError(domain.CodeNotFound, "Task Workspace publish receipt not found")
	}
	if receipt.State == TaskWorkspacePublishPending {
		receipt.State = TaskWorkspacePublishQuotaRejected
		s.taskWorkspacePublishReceipts[key] = receipt
	}
	return nil
}

func (s *syncServiceStore) ReserveTaskWorkspaceBlockUsage(context.Context, string, []WorkspaceBlockReservation, int64) error {
	return nil
}

func syncCommitReceiptKey(record IdempotencyRecord) string {
	return record.OwnerID + "/" + record.Operation + "/" + record.Key
}

func (s *syncServiceStore) GetWorkspace(_ context.Context, id string) (workspace.Workspace, error) {
	if id != s.workspace.ID {
		return workspace.Workspace{}, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return s.workspace, nil
}

func (s *syncServiceStore) GetWorkspaceBinding(_ context.Context, hostID, workspaceID string) (workspace.Binding, error) {
	if s.binding.HostID != hostID || s.binding.Workspace.ID != workspaceID {
		return workspace.Binding{}, domain.NewError(domain.CodeNotFound, "workspace binding not found")
	}
	return s.binding, nil
}

func (s *syncServiceStore) CheckWorkspaceSyncDeviceAccess(_ context.Context, _ identity.Principal, _ string) error {
	if s.syncDeviceRevoked {
		return domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return nil
}

func (s *syncServiceStore) WithWorkspaceSyncDeviceSessionAccess(ctx context.Context, principal identity.Principal, workspaceID, _ string, write func() error) error {
	if err := s.CheckWorkspaceSyncDeviceAccess(ctx, principal, workspaceID); err != nil {
		return err
	}
	return write()
}

func (s *syncServiceStore) TryWorkspaceBlockStoreLock(ctx context.Context, _ string) (LeaderLock, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	s.blockLockAcquires++
	if s.blockLockBusy || s.blockLockHeld || s.blockReadLocksHeld != 0 {
		return nil, false, nil
	}
	s.blockLockHeld = true
	return syncServiceBlockLock{store: s}, true, nil
}

func (s *syncServiceStore) TryWorkspaceBlockStoreReadLock(ctx context.Context, _ string) (LeaderLock, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	s.blockReadLockAcquires++
	if s.blockReadLockBusy || s.blockLockHeld {
		return nil, false, nil
	}
	s.blockReadLocksHeld++
	return syncServiceBlockLock{store: s, shared: true}, true, nil
}

func (s *syncServiceStore) GetWorkspaceManifestPage(_ context.Context, id string, revision int64, prefix, after string, limit int32) (workspace.ManifestPage, error) {
	files, ok := s.snapshots[revision]
	if !ok || id != s.workspace.ID {
		return workspace.ManifestPage{}, domain.NewError(domain.CodeNotFound, "workspace revision not found")
	}
	result := make([]workspace.File, 0)
	for _, file := range files {
		if after != "" && file.Path <= after {
			continue
		}
		if prefix != "" && file.Path != prefix && !strings.HasPrefix(file.Path, prefix+"/") {
			continue
		}
		result = append(result, file)
	}
	if int32(len(result)) > limit {
		result = result[:limit]
		last := result[len(result)-1].Path
		cursor := workspace.EncodeManifestCursor(workspace.ManifestCursor{Revision: revision, Prefix: prefix, After: last})
		return workspace.ManifestPage{WorkspaceID: id, Revision: revision, Files: result, NextCursor: &cursor}, nil
	}
	return workspace.ManifestPage{WorkspaceID: id, Revision: revision, Files: result}, nil
}

func (s *syncServiceStore) GetWorkspaceChangePage(_ context.Context, _ string, revision int64, after string, limit int32) (workspace.ChangePage, error) {
	items := make([]workspace.Change, 0)
	for _, change := range s.changes {
		if change.Revision > revision || change.Revision == revision && change.Path > after {
			items = append(items, change)
		}
	}
	if int32(len(items)) > limit {
		items = items[:limit]
	}
	if len(items) == 0 {
		return workspace.ChangePage{Items: []workspace.Change{}, NextCursor: workspace.EncodeChangeCursor(workspace.ChangeCursor{Revision: revision, Path: after})}, nil
	}
	last := items[len(items)-1]
	return workspace.ChangePage{Items: items, NextCursor: workspace.EncodeChangeCursor(workspace.ChangeCursor{Revision: last.Revision, Path: last.Path})}, nil
}

func (s *syncServiceStore) CreateWorkspaceSyncSession(_ context.Context, session workspace.SyncSession, transfers []workspace.TransferBlock, _ []WorkspaceBlockReservation, _ int64, _ IdempotencyRecord) (workspace.SyncSession, bool, error) {
	var manifestBytes int64
	for _, file := range session.Files {
		manifestBytes += file.SizeBytes
	}
	if manifestBytes > s.workspace.QuotaBytes {
		return workspace.SyncSession{}, false, domain.NewError(domain.CodeQuotaExceeded, "workspace storage quota would be exceeded")
	}
	if s.onCreate != nil {
		s.onCreate()
	}
	if existing, ok := s.sessions[session.ID]; ok {
		return existing, true, nil
	}
	session.TransferBlocks = append([]workspace.TransferBlock(nil), transfers...)
	s.sessions[session.ID] = session
	s.blocks[session.ID] = append([]workspace.TransferBlock(nil), transfers...)
	return session, false, nil
}

func (s *syncServiceStore) GetWorkspaceSyncSession(_ context.Context, workspaceID, sessionID string) (workspace.SyncSession, error) {
	session, ok := s.sessions[sessionID]
	if !ok || session.WorkspaceID != workspaceID {
		return workspace.SyncSession{}, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	return session, nil
}

func (s *syncServiceStore) ListDeviceWorkspaceSyncProgress(_ context.Context, ownerID, workspaceID string) ([]workspace.DeviceSyncProgress, error) {
	s.progressOwner, s.progressWorkspace = ownerID, workspaceID
	return append([]workspace.DeviceSyncProgress(nil), s.progress...), nil
}

func (s *syncServiceStore) ListWorkspaceSyncTransferBlocks(_ context.Context, _, sessionID string) ([]workspace.TransferBlock, error) {
	return append([]workspace.TransferBlock(nil), s.blocks[sessionID]...), nil
}

func (s *syncServiceStore) ReserveWorkspaceSyncCommit(_ context.Context, workspaceID, sessionID string, record IdempotencyRecord) (WorkspaceSyncCommitReceipt, error) {
	key := syncCommitReceiptKey(record)
	if receipt, ok := s.commitReceipts[key]; ok {
		if record.ResourceID != sessionID || record.Operation != "workspace.sync.commit" {
			return WorkspaceSyncCommitReceipt{}, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for a different request")
		}
		return receipt, nil
	}
	if workspaceID != s.workspace.ID || record.ResourceID != sessionID || record.Operation != "workspace.sync.commit" {
		return WorkspaceSyncCommitReceipt{}, domain.NewError(domain.CodeNotFound, "workspace sync commit receipt not found")
	}
	receipt := WorkspaceSyncCommitReceipt{State: WorkspaceSyncCommitPending}
	s.commitReceipts[key] = receipt
	return receipt, nil
}

func (s *syncServiceStore) InspectWorkspaceSyncCommit(_ context.Context, workspaceID, sessionID string, record IdempotencyRecord) (WorkspaceSyncCommitReceipt, error) {
	if workspaceID != s.workspace.ID || record.ResourceID != sessionID || record.Operation != "workspace.sync.commit" {
		return WorkspaceSyncCommitReceipt{}, domain.NewError(domain.CodeNotFound, "workspace sync commit receipt not found")
	}
	receipt, ok := s.commitReceipts[syncCommitReceiptKey(record)]
	if !ok {
		return WorkspaceSyncCommitReceipt{}, domain.NewError(domain.CodeNotFound, "workspace sync commit receipt not found")
	}
	return receipt, nil
}

func (s *syncServiceStore) FinalizeWorkspaceBlockUsage(context.Context, string, string, int64, int64) error {
	s.usageFinalizeCalls++
	return nil
}

func (s *syncServiceStore) AckWorkspaceSyncBlock(_ context.Context, _, sessionID, digest string, _, _ int64) error {
	session := s.sessions[sessionID]
	for i := range s.blocks[sessionID] {
		block := &s.blocks[sessionID][i]
		if block.SHA256 == digest && block.Role == workspace.BlockUpload {
			if !block.Received {
				block.Received = true
				session.ConfirmedBytes += block.SizeBytes
			}
			session.State, session.Stage = workspace.SyncTransferring, workspace.SyncStageTransfer
			s.sessions[sessionID] = session
			return nil
		}
	}
	return domain.NewError(domain.CodeNotFound, "workspace sync block not found")
}

func (s *syncServiceStore) AckWorkspaceSyncDownloadBlock(_ context.Context, _, sessionID, digest string) error {
	session := s.sessions[sessionID]
	for i := range s.blocks[sessionID] {
		block := &s.blocks[sessionID][i]
		if block.SHA256 == digest && block.Role == workspace.BlockDownload {
			if !block.Received {
				block.Received = true
				session.ConfirmedBytes += block.SizeBytes
			}
			for _, pending := range s.blocks[sessionID] {
				if !pending.Received {
					session.State, session.Stage = workspace.SyncTransferring, workspace.SyncStageTransfer
					s.sessions[sessionID] = session
					return nil
				}
			}
			actionsPending := false
			for _, action := range session.DownloadActions {
				if !action.Applied {
					actionsPending = true
					break
				}
			}
			if actionsPending || len(session.DownloadActions) == 0 {
				session.State, session.Stage = workspace.SyncVerifying, workspace.SyncStageVerify
			} else {
				session.State, session.Stage = workspace.SyncApplying, workspace.SyncStageApply
			}
			s.sessions[sessionID] = session
			return nil
		}
	}
	return domain.NewError(domain.CodeNotFound, "workspace sync block not found")
}

func (s *syncServiceStore) AckWorkspaceSyncDownloadAction(_ context.Context, _, sessionID, path string, action workspace.ChangeAction, digest string, sizeBytes int64) error {
	session, ok := s.sessions[sessionID]
	if !ok {
		return domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	for i := range session.DownloadActions {
		planned := &session.DownloadActions[i]
		matchesDigest := planned.SHA256 == nil && digest == "" || planned.SHA256 != nil && *planned.SHA256 == digest
		if planned.Path != path {
			continue
		}
		if planned.Action != action || !matchesDigest || planned.SizeBytes != sizeBytes {
			return domain.NewError(domain.CodeWorkspaceConflict, "workspace download action does not match the session plan")
		}
		if planned.Applied {
			return nil
		}
		planned.Applied = true
		session.DownloadActions = append([]workspace.DownloadAction(nil), session.DownloadActions...)
		for _, block := range s.blocks[sessionID] {
			if !block.Received {
				session.State, session.Stage = workspace.SyncTransferring, workspace.SyncStageTransfer
				s.sessions[sessionID] = session
				return nil
			}
		}
		for _, pending := range session.DownloadActions {
			if !pending.Applied {
				session.State, session.Stage = workspace.SyncApplying, workspace.SyncStageApply
				s.sessions[sessionID] = session
				return nil
			}
		}
		session.State, session.Stage = workspace.SyncApplying, workspace.SyncStageApply
		s.sessions[sessionID] = session
		return nil
	}
	return domain.NewError(domain.CodeNotFound, "workspace download action not found")
}

func (s *syncServiceStore) CommitWorkspaceSyncSession(_ context.Context, workspaceID, sessionID string, record IdempotencyRecord) (workspace.SyncSession, bool, error) {
	session, ok := s.sessions[sessionID]
	if !ok || session.WorkspaceID != workspaceID {
		return workspace.SyncSession{}, false, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	for _, block := range s.blocks[sessionID] {
		if !block.Received {
			return workspace.SyncSession{}, false, domain.NewError(domain.CodeWorkspaceBusy, "workspace sync block is not yet acknowledged")
		}
	}
	for _, action := range session.DownloadActions {
		if !action.Applied {
			return workspace.SyncSession{}, false, domain.NewError(domain.CodeWorkspaceBusy, "workspace sync download action is not yet applied")
		}
	}
	session.TransferBlocks = append([]workspace.TransferBlock(nil), s.blocks[sessionID]...)
	session.State, session.Stage = workspace.SyncCompleted, workspace.SyncStageComplete
	session.TargetRevision = 1
	s.sessions[sessionID] = session
	s.workspace.Revision = 1
	s.snapshots[1] = append([]workspace.File(nil), session.UploadFiles...)
	s.commitReceipts[syncCommitReceiptKey(record)] = WorkspaceSyncCommitReceipt{State: WorkspaceSyncCommitSucceeded, Result: session}
	return session, false, nil
}

func (s *syncServiceStore) CancelWorkspaceSyncSession(_ context.Context, workspaceID, sessionID string, _ IdempotencyRecord) (workspace.SyncSession, bool, error) {
	session, ok := s.sessions[sessionID]
	if !ok || session.WorkspaceID != workspaceID {
		return workspace.SyncSession{}, false, domain.NewError(domain.CodeNotFound, "workspace sync session not found")
	}
	session.State, session.Stage = workspace.SyncCancelled, workspace.SyncStageComplete
	s.sessions[sessionID] = session
	return session, false, nil
}

func (s *syncServiceStore) FailWorkspaceSyncSession(_ context.Context, _, sessionID string) error {
	session := s.sessions[sessionID]
	session.State, session.Stage = workspace.SyncFailed, workspace.SyncStageComplete
	s.sessions[sessionID] = session
	return nil
}

type syncBlockMemory struct {
	data  map[string][]byte
	onPut func()
}

func newSyncBlockMemory() *syncBlockMemory { return &syncBlockMemory{data: map[string][]byte{}} }

func syncServiceFile(path string, data []byte) workspace.File {
	digest := sha256.Sum256(data)
	chunks := []workspace.Chunk{}
	if len(data) > 0 {
		chunks = append(chunks, workspace.Chunk{SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(data))})
	}
	return workspace.File{
		Path: path, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), Mode: 0644, Chunks: chunks,
	}
}

func (s *syncBlockMemory) PutBlock(_ string, digest string, src io.Reader) (int64, error) {
	if s.onPut != nil {
		s.onPut()
	}
	data, err := io.ReadAll(io.LimitReader(src, workspace.MaxSyncBlockBytes+1))
	if err != nil {
		return 0, err
	}
	if int64(len(data)) > workspace.MaxSyncBlockBytes {
		return 0, errors.New("block exceeds 4 MiB")
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != digest {
		return 0, errors.New("digest mismatch")
	}
	s.data[digest] = append([]byte(nil), data...)
	return int64(len(data)), nil
}

func (s *syncBlockMemory) Open(_, digest string) (io.ReadCloser, error) {
	data, ok := s.data[digest]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *syncBlockMemory) ReservationBytes(size int64) (int64, error) { return size, nil }

func (s *syncBlockMemory) BlockAllocatedBytes(_, digest string) (int64, error) {
	data, ok := s.data[digest]
	if !ok {
		return 0, os.ErrNotExist
	}
	return int64(len(data)), nil
}

func TestWorkspaceSyncServiceUploadIsResumableAndWorkspaceScoped(t *testing.T) {
	store := newSyncServiceStore()
	blocks := newSyncBlockMemory()
	store.onCreate = func() {
		if !store.blockLockHeld {
			t.Error("sync session was persisted without the exclusive block-store lock")
		}
	}
	blocks.onPut = func() {
		if store.blockReadLocksHeld != 1 {
			t.Error("block upload did not hold the shared block-store lock")
		}
		if _, acquired, err := store.TryWorkspaceBlockStoreLock(context.Background(), store.workspace.ID); err != nil || acquired {
			t.Errorf("exclusive block-store lock during upload = acquired %v, error %v; want busy", acquired, err)
		}
	}
	service, err := NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	body := []byte("abc")
	digest := sha256.Sum256(body)
	file := workspace.File{Path: "src/main.go", SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(digest[:]), Mode: 0644, Chunks: []workspace.Chunk{{SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(body))}}}
	session, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncUpload, Policy: workspace.SyncLive, BaseRevision: 0, Files: []workspace.File{file},
	}, "sync-create-001")
	if err != nil {
		t.Fatal(err)
	}
	if session.State != workspace.SyncPlanned || len(session.TransferBlocks) != 1 || session.TransferBlocks[0].Role != workspace.BlockUpload || session.PlannedBytes != int64(len(body)) {
		t.Fatalf("created session = %#v", session)
	}
	if _, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, "sync-commit-001"); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("commit before transfer error = %v", err)
	}
	if _, err := service.PutSyncBlock(context.Background(), principal, store.workspace.ID, session.ID, file.SHA256, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	if store.usageFinalizeCalls != 0 {
		t.Fatalf("successful block upload used %d separate settlement transactions, want settlement inside ACK", store.usageFinalizeCalls)
	}
	if _, err := service.PutSyncBlock(context.Background(), principal, store.workspace.ID, session.ID, strings.Repeat("0", 64), bytes.NewReader(body)); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unplanned block upload error = %v", err)
	}
	committed, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, "sync-commit-001")
	if err != nil {
		t.Fatal(err)
	}
	if committed.State != workspace.SyncCompleted || committed.TargetRevision != 1 || committed.ConfirmedBytes != int64(len(body)) {
		t.Fatalf("committed session = %#v", committed)
	}
	if store.blockLockAcquires != 2 || store.blockLockReleases != 1 || store.blockReadLockAcquires != 2 || store.blockReadLockReleases != 2 {
		t.Fatalf("block-store lock counts = exclusive %d/%d shared %d/%d", store.blockLockAcquires, store.blockLockReleases, store.blockReadLockAcquires, store.blockReadLockReleases)
	}
}

func TestWorkspaceSyncServiceCreateReportsQuotaExceeded(t *testing.T) {
	store := newSyncServiceStore()
	store.workspace.QuotaBytes = 2
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	_, err = service.CreateSyncSession(context.Background(), principal, store.workspace.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncUpload, Policy: workspace.SyncLive, Files: []workspace.File{syncServiceFile("too-large.txt", []byte("abc"))},
	}, "quota-create")
	if domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("over-quota create error = %v, want quota_exceeded", err)
	}
	if len(store.sessions) != 0 {
		t.Fatalf("over-quota create persisted %d sessions", len(store.sessions))
	}
}

func TestWorkspaceSyncServiceBlockStoreLockContention(t *testing.T) {
	store := newSyncServiceStore()
	blocks := newSyncBlockMemory()
	service, err := NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	body := []byte("blocked")
	file := syncServiceFile("blocked.txt", body)
	store.blockLockBusy = true
	if _, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncUpload, Policy: workspace.SyncLive, BaseRevision: 0, Files: []workspace.File{file},
	}, "sync-create-locked"); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("create with busy block store error = %v, want workspace busy", err)
	}
	if len(store.sessions) != 0 {
		t.Fatalf("create with busy block store persisted sessions: %d", len(store.sessions))
	}
	store.blockLockBusy = false
	session, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncUpload, Policy: workspace.SyncLive, BaseRevision: 0, Files: []workspace.File{file},
	}, "sync-create-unlocked")
	if err != nil {
		t.Fatal(err)
	}
	store.blockReadLockBusy = true
	if _, err := service.PutSyncBlock(context.Background(), principal, store.workspace.ID, session.ID, file.Chunks[0].SHA256, bytes.NewReader(body)); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("upload with busy block store error = %v, want workspace busy", err)
	}
	if _, ok := blocks.data[file.Chunks[0].SHA256]; ok {
		t.Fatal("upload wrote a block while its shared block-store lock was unavailable")
	}
	if store.blockLockAcquires != 2 || store.blockLockReleases != 1 || store.blockReadLockAcquires != 1 || store.blockReadLockReleases != 0 {
		t.Fatalf("block-store lock counts = exclusive %d/%d shared %d/%d", store.blockLockAcquires, store.blockLockReleases, store.blockReadLockAcquires, store.blockReadLockReleases)
	}
}

func TestWorkspaceSyncCommitReturnsRetryableBusyWhileReceiptIsPending(t *testing.T) {
	store, blocks, service, principal, session, _ := uploadedSyncFixture(t)
	const idempotencyKey = "commit-pending-verify"
	record, err := idempotencyRecord(session.OwnerID, "workspace.sync.commit", idempotencyKey, struct {
		WorkspaceID string `json:"workspace_id"`
		SessionID   string `json:"session_id"`
	}{store.workspace.ID, session.ID}, session.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := store.ReserveWorkspaceSyncCommit(context.Background(), store.workspace.ID, session.ID, record)
	if err != nil || receipt.State != WorkspaceSyncCommitPending {
		t.Fatalf("reserved commit receipt = %#v, err=%v; want pending", receipt, err)
	}

	blocks.openError = errors.New("temporary block read failure")
	if _, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, idempotencyKey); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("commit while block verification is temporarily unavailable = %v, want retryable workspace busy", err)
	}
	receipt, err = store.InspectWorkspaceSyncCommit(context.Background(), store.workspace.ID, session.ID, record)
	if err != nil || receipt.State != WorkspaceSyncCommitPending {
		t.Fatalf("receipt after retryable verification failure = %#v, err=%v; want pending", receipt, err)
	}
	if store.workspace.Revision != 0 || store.sessions[session.ID].State == workspace.SyncCompleted {
		t.Fatalf("temporary block verification failure committed state: revision=%d session=%s", store.workspace.Revision, store.sessions[session.ID].State)
	}

	blocks.openError = nil
	committed, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, idempotencyKey)
	if err != nil || committed.State != workspace.SyncCompleted || committed.TargetRevision != 1 {
		t.Fatalf("same-key retry after block storage recovers = %#v, err=%v", committed, err)
	}
	receipt, err = store.InspectWorkspaceSyncCommit(context.Background(), store.workspace.ID, session.ID, record)
	if err != nil || receipt.State != WorkspaceSyncCommitSucceeded {
		t.Fatalf("receipt after successful retry = %#v, err=%v; want succeeded", receipt, err)
	}
}

func TestWorkspaceSyncServiceAuthorizesDeviceWorkspaceDynamically(t *testing.T) {
	store := newSyncServiceStore()
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	device := identity.Principal{UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: "t1234567", Scopes: []identity.Scope{identity.ScopeWorkspaceSync}}
	if _, err := service.GetManifest(context.Background(), device, store.workspace.ID, 0, "", "", 100); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unbound device error = %v", err)
	}
	device.ClientInstallationID = "i1234567"
	if _, err := service.GetManifest(context.Background(), device, store.workspace.ID, 0, "", "", 100); err != nil {
		t.Fatalf("owned device manifest error = %v", err)
	}
	store.syncDeviceRevoked = true
	if _, err := service.GetManifest(context.Background(), device, store.workspace.ID, 0, "", "", 100); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked device manifest error = %v", err)
	}
	store.syncDeviceRevoked = false
	device.Scopes = nil
	if _, err := service.GetManifest(context.Background(), device, store.workspace.ID, 0, "", "", 100); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("device without sync scope error = %v", err)
	}
}

type revokeOnOpenWorkspaceBlockStore struct {
	WorkspaceBlockStore
	revoke func()
}

func (s revokeOnOpenWorkspaceBlockStore) Open(workspaceID, digest string) (io.ReadCloser, error) {
	reader, err := s.WorkspaceBlockStore.Open(workspaceID, digest)
	if err == nil {
		s.revoke()
	}
	return reader, err
}

func TestWorkspaceSyncDeviceBlockStreamRechecksAccessBeforeSending(t *testing.T) {
	store := newSyncServiceStore()
	blocks := newSyncBlockMemory()
	content := []byte("workspace file content")
	file := syncServiceFile("notes.txt", content)
	if _, err := blocks.PutBlock(store.workspace.ID, file.SHA256, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	store.workspace.Revision = 1
	store.snapshots[1] = []workspace.File{file}
	device := identity.Principal{
		UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindDevice,
		TokenID: "t1234567", ClientInstallationID: "i1234567", Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	service, err := NewWorkspaceSyncService(store, revokeOnOpenWorkspaceBlockStore{
		WorkspaceBlockStore: blocks,
		revoke:              func() { store.syncDeviceRevoked = true },
	}, workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSyncSession(context.Background(), device, store.workspace.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncDownload, Policy: workspace.SyncFillMissing, BaseRevision: 0, Files: []workspace.File{},
	}, "device-download-stream-revoke")
	if err != nil {
		t.Fatal(err)
	}
	streamCalled := false
	err = service.StreamSyncBlock(context.Background(), device, store.workspace.ID, session.ID, file.SHA256, func(int64, []byte) error {
		streamCalled = true
		return nil
	})
	if domain.CodeOf(err) != domain.CodeNotFound || streamCalled {
		t.Fatalf("device block stream after read-time revocation = %v, callback called %t; want denied before sending", err, streamCalled)
	}
}

func TestWorkspaceSyncDeviceBlockStreamStopsAtRevocationBoundary(t *testing.T) {
	store := newSyncServiceStore()
	blocks := newSyncBlockMemory()
	content := bytes.Repeat([]byte("x"), 2*workspaceSyncStreamChunkBytes+17)
	file := syncServiceFile("notes.txt", content)
	if _, err := blocks.PutBlock(store.workspace.ID, file.SHA256, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	store.workspace.Revision = 1
	store.snapshots[1] = []workspace.File{file}
	device := identity.Principal{
		UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindDevice,
		TokenID: "t1234567", ClientInstallationID: "i1234567", Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	service, err := NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSyncSession(context.Background(), device, store.workspace.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncDownload, Policy: workspace.SyncFillMissing, BaseRevision: 0, Files: []workspace.File{},
	}, "device-download-stream-boundary")
	if err != nil {
		t.Fatal(err)
	}
	var transferred []byte
	writeCalls := 0
	err = service.StreamSyncBlock(context.Background(), device, store.workspace.ID, session.ID, file.SHA256, func(_ int64, chunk []byte) error {
		writeCalls++
		transferred = append(transferred, chunk...)
		store.syncDeviceRevoked = true
		return nil
	})
	if domain.CodeOf(err) != domain.CodeNotFound || writeCalls != 1 || len(transferred) != workspaceSyncStreamChunkBytes {
		t.Fatalf("revoked device stream = err %v, calls %d, bytes %d; want one authorized chunk then stop", err, writeCalls, len(transferred))
	}
}

type revokeAfterFirstRead struct {
	*bytes.Reader
	revoked bool
	revoke  func()
}

func (r *revokeAfterFirstRead) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 && !r.revoked {
		r.revoked = true
		r.revoke()
	}
	return n, err
}

func TestWorkspaceSyncDeviceUploadStopsAfterRevocation(t *testing.T) {
	store := newSyncServiceStore()
	blocks := newSyncBlockMemory()
	service, err := NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("workspace upload content")
	file := syncServiceFile("upload.txt", content)
	device := identity.Principal{
		UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindDevice,
		TokenID: "t1234567", ClientInstallationID: "i1234567", Scopes: []identity.Scope{identity.ScopeWorkspaceSync},
	}
	session, err := service.CreateSyncSession(context.Background(), device, store.workspace.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncUpload, Policy: workspace.SyncLive, BaseRevision: 0, Files: []workspace.File{file},
	}, "device-upload-revoke")
	if err != nil {
		t.Fatal(err)
	}
	source := &revokeAfterFirstRead{Reader: bytes.NewReader(content), revoke: func() { store.syncDeviceRevoked = true }}
	if _, err := service.PutSyncBlock(context.Background(), device, store.workspace.ID, session.ID, file.SHA256, source); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("upload after mid-transfer device revocation = %v; want not found", err)
	}
	if _, exists := blocks.data[file.SHA256]; exists {
		t.Fatal("partially authorized upload published a block")
	}
}

func TestWorkspaceSyncServiceFencesPreparingHostReplicaToInitialDownload(t *testing.T) {
	store := newSyncServiceStore()
	store.workspace.Revision = 2
	store.snapshots[2] = []workspace.File{}
	store.binding = workspace.Binding{
		HostID: "h1234567", OwnerID: store.workspace.OwnerID, Workspace: store.workspace,
		Directory: store.workspace.ID, State: workspace.BindingPreparing, BindingVersion: 4,
		DesiredRevision: store.workspace.Revision,
	}
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindHostReplica,
		HostScope: store.binding.HostID, HostGeneration: 3, ReplicaWorkspaceID: store.workspace.ID, BindingVersion: store.binding.BindingVersion,
	}
	created, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncDownload, Policy: workspace.SyncFillMissing, BaseRevision: 0, Files: []workspace.File{},
	}, "initial-download")
	if err != nil || created.Direction != workspace.SyncDownload || created.TargetRevision != 2 || created.ReplicaID != "host:h1234567:g3:w1234567:b4" {
		t.Fatalf("CreateSyncSession() initial Host download = %#v, %v", created, err)
	}
	for _, command := range []workspace.CreateSyncSession{
		{Direction: workspace.SyncUpload, Policy: workspace.SyncFillMissing, BaseRevision: 0, Files: []workspace.File{}},
		{Direction: workspace.SyncBidirectional, Policy: workspace.SyncFillMissing, BaseRevision: 0, Files: []workspace.File{}},
	} {
		if _, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, command, "unsafe-initial-sync"); domain.CodeOf(err) != domain.CodeForbidden {
			t.Fatalf("CreateSyncSession() unsafe preparing operation error = %v", err)
		}
	}
	store.binding.State = workspace.BindingUnbinding
	checkpoint, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncBidirectional, Policy: workspace.SyncFillMissing, BaseRevision: store.workspace.Revision, Files: []workspace.File{},
	}, "initial-unbind-checkpoint")
	if err != nil || checkpoint.Direction != workspace.SyncBidirectional || checkpoint.Policy != workspace.SyncFillMissing {
		t.Fatalf("CreateSyncSession() initial additive unbind checkpoint = %#v, %v", checkpoint, err)
	}
	if _, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncBidirectional, Policy: workspace.SyncFillMissing, BaseRevision: store.workspace.Revision,
		Files: []workspace.File{}, DeletedPaths: []string{"stale.txt"},
	}, "unsafe-initial-unbind-delete"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("CreateSyncSession() destructive initial unbind checkpoint error = %v", err)
	}
}

func TestWorkspaceSyncProgressIsPortalOwnerOnlyAndSanitized(t *testing.T) {
	store := newSyncServiceStore()
	store.progress = []workspace.DeviceSyncProgress{{
		WorkspaceID: store.workspace.ID, DeviceID: "t7654321", SessionID: "p1234567",
		Direction: workspace.SyncDownload, State: workspace.SyncApplying, Stage: workspace.SyncStageApply,
		PlannedBytes: 10, ConfirmedBytes: 10, DownloadActionsTotal: 2, DownloadActionsDone: 1,
	}}
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	portal := identity.Principal{UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	items, err := service.ListDeviceSyncProgress(context.Background(), portal, store.workspace.ID)
	if err != nil || len(items) != 1 || items[0].DownloadActionsDone != 1 {
		t.Fatalf("ListDeviceSyncProgress() = %#v, %v", items, err)
	}
	if store.progressOwner != store.workspace.OwnerID || store.progressWorkspace != store.workspace.ID {
		t.Fatalf("progress query scope = %q / %q", store.progressOwner, store.progressWorkspace)
	}
	for _, principal := range []identity.Principal{
		{UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567"},
		{UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindDevice, TokenID: "t1234567"},
		{UserID: "u7654321", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"},
	} {
		before := store.progressOwner
		if _, err := service.ListDeviceSyncProgress(context.Background(), principal, store.workspace.ID); err == nil {
			t.Fatalf("ListDeviceSyncProgress() allowed principal kind=%q user=%q", principal.Kind, principal.UserID)
		}
		if principal.Kind == identity.KindSession && store.progressOwner != before {
			t.Fatal("foreign workspace owner reached the progress query")
		}
	}
}

func TestWorkspaceSyncServiceDownloadOnlyReturnsSessionPlannedBlocks(t *testing.T) {
	store := newSyncServiceStore()
	blocks := newSyncBlockMemory()
	service, err := NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("server data")
	digest := sha256.Sum256(body)
	chunk := workspace.Chunk{SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(body))}
	file := workspace.File{Path: "readme.md", SizeBytes: int64(len(body)), SHA256: chunk.SHA256, Mode: 0644, Chunks: []workspace.Chunk{chunk}}
	if _, err := blocks.PutBlock(store.workspace.ID, chunk.SHA256, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	store.workspace.Revision = 1
	store.snapshots[1] = []workspace.File{file}
	principal := identity.Principal{UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	session, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncDownload, Policy: workspace.SyncFillMissing, BaseRevision: 0, Files: []workspace.File{},
	}, "sync-download-001")
	if err != nil {
		t.Fatal(err)
	}
	var actual []byte
	err = service.StreamSyncBlock(context.Background(), principal, store.workspace.ID, session.ID, chunk.SHA256, func(total int64, part []byte) error {
		if total != int64(len(body)) {
			t.Fatalf("download total size = %d; want %d", total, len(body))
		}
		actual = append(actual, part...)
		return nil
	})
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("download data=%q, err=%v", actual, err)
	}
	if session.PlannedBytes != int64(len(body)) || len(session.TransferBlocks) != 1 || session.TransferBlocks[0].Role != workspace.BlockDownload || session.TransferBlocks[0].Received {
		t.Fatalf("download session plan = %#v", session)
	}
	if _, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, "sync-download-commit-001"); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("download commit before durable acknowledgement error = %v", err)
	}
	if err := service.AckDownloadedBlock(context.Background(), principal, store.workspace.ID, session.ID, chunk.SHA256); err != nil {
		t.Fatalf("ack downloaded block after durable write: %v", err)
	}
	verified, err := service.GetSyncSession(context.Background(), principal, store.workspace.ID, session.ID)
	if err != nil || verified.State != workspace.SyncVerifying || verified.Stage != workspace.SyncStageVerify {
		t.Fatalf("download after transfer stage = %#v err=%v", verified, err)
	}
	if err := service.AckDownloadedBlock(context.Background(), principal, store.workspace.ID, session.ID, chunk.SHA256); err != nil {
		t.Fatalf("repeat download block acknowledgement: %v", err)
	}
	if _, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, "sync-download-commit-001"); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("download commit before file apply acknowledgement error = %v", err)
	}
	if err := service.AckDownloadAction(context.Background(), principal, store.workspace.ID, session.ID, file.Path, workspace.ChangeUpsert, file.SHA256, file.SizeBytes); err != nil {
		t.Fatalf("ack applied download file: %v", err)
	}
	if err := service.AckDownloadAction(context.Background(), principal, store.workspace.ID, session.ID, file.Path, workspace.ChangeUpsert, file.SHA256, file.SizeBytes); err != nil {
		t.Fatalf("repeat applied download file acknowledgement: %v", err)
	}
	committed, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, "sync-download-commit-001")
	if err != nil || committed.State != workspace.SyncCompleted || committed.ConfirmedBytes != int64(len(body)) {
		t.Fatalf("download committed session = %#v, err=%v", committed, err)
	}
	if err := service.StreamSyncBlock(context.Background(), principal, store.workspace.ID, session.ID, strings.Repeat("0", 64), func(int64, []byte) error {
		t.Fatal("unplanned block stream invoked callback")
		return nil
	}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("unplanned block read error = %v", err)
	}
}

func TestWorkspaceSyncServiceRequiresEachDownloadActionIncludingEmptyFiles(t *testing.T) {
	store := newSyncServiceStore()
	blocks := newSyncBlockMemory()
	service, err := NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	shared := []byte("shared")
	sharedFileA := syncServiceFile("a.txt", shared)
	sharedFileB := syncServiceFile("b.txt", shared)
	emptyFile := syncServiceFile("empty.txt", nil)
	if _, err := blocks.PutBlock(store.workspace.ID, sharedFileA.SHA256, bytes.NewReader(shared)); err != nil {
		t.Fatal(err)
	}
	store.workspace.Revision = 1
	store.snapshots[1] = []workspace.File{sharedFileA, sharedFileB, emptyFile}
	session, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncDownload, Policy: workspace.SyncFillMissing, BaseRevision: 0, Files: []workspace.File{},
	}, "sync-empty-actions-001")
	if err != nil {
		t.Fatal(err)
	}
	if len(session.TransferBlocks) != 1 || len(session.DownloadActions) != 3 || session.PlannedBytes != int64(len(shared)) {
		t.Fatalf("deduplicated download plan = %#v", session)
	}
	if err := service.AckDownloadedBlock(context.Background(), principal, store.workspace.ID, session.ID, sharedFileA.SHA256); err != nil {
		t.Fatal(err)
	}
	beforeActions, err := service.GetSyncSession(context.Background(), principal, store.workspace.ID, session.ID)
	if err != nil || beforeActions.ConfirmedBytes != int64(len(shared)) {
		t.Fatalf("block transfer progress = %#v err=%v", beforeActions, err)
	}
	if _, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, "sync-empty-actions-commit"); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("commit before path acknowledgements error = %v", err)
	}
	for _, file := range []workspace.File{sharedFileA, sharedFileB} {
		if err := service.AckDownloadAction(context.Background(), principal, store.workspace.ID, session.ID, file.Path, workspace.ChangeUpsert, file.SHA256, file.SizeBytes); err != nil {
			t.Fatalf("ack %s: %v", file.Path, err)
		}
		applying, err := service.GetSyncSession(context.Background(), principal, store.workspace.ID, session.ID)
		if err != nil || applying.State != workspace.SyncApplying || applying.Stage != workspace.SyncStageApply {
			t.Fatalf("download after applying %s = %#v err=%v", file.Path, applying, err)
		}
		if err := service.AckDownloadAction(context.Background(), principal, store.workspace.ID, session.ID, file.Path, workspace.ChangeUpsert, file.SHA256, file.SizeBytes); err != nil {
			t.Fatalf("repeat ack %s: %v", file.Path, err)
		}
		if _, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, "sync-empty-actions-commit"); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
			t.Fatalf("commit before every path acknowledgement error = %v", err)
		}
	}
	if err := service.AckDownloadAction(context.Background(), principal, store.workspace.ID, session.ID, emptyFile.Path, workspace.ChangeUpsert, emptyFile.SHA256, 0); err != nil {
		t.Fatalf("ack zero-byte file: %v", err)
	}
	current, err := service.GetSyncSession(context.Background(), principal, store.workspace.ID, session.ID)
	if err != nil || current.ConfirmedBytes != beforeActions.ConfirmedBytes || len(current.DownloadActions) != 3 {
		t.Fatalf("download action progress = %#v err=%v", current, err)
	}
	for _, action := range current.DownloadActions {
		if !action.Applied {
			t.Fatalf("download action remains unacknowledged: %#v", action)
		}
	}
	committed, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, "sync-empty-actions-commit")
	if err != nil || committed.State != workspace.SyncCompleted {
		t.Fatalf("commit after every download action acknowledgement = %#v err=%v", committed, err)
	}
}

func TestWorkspaceSyncServiceRequiresDownloadDeleteAcknowledgement(t *testing.T) {
	store := newSyncServiceStore()
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	oldFile := syncServiceFile("removed.txt", []byte("old"))
	store.workspace.Revision = 1
	store.snapshots[0] = []workspace.File{oldFile}
	store.snapshots[1] = []workspace.File{}
	session, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncDownload, Policy: workspace.SyncCompareMerge, BaseRevision: 0, Files: []workspace.File{oldFile},
	}, "sync-delete-action-001")
	if err != nil {
		t.Fatal(err)
	}
	if len(session.TransferBlocks) != 0 || len(session.DownloadActions) != 1 || session.DownloadActions[0].Action != workspace.ChangeDelete {
		t.Fatalf("download delete plan = %#v", session)
	}
	if _, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, "sync-delete-action-commit"); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("commit before delete acknowledgement error = %v", err)
	}
	if err := service.AckDownloadAction(context.Background(), principal, store.workspace.ID, session.ID, oldFile.Path, workspace.ChangeDelete, "", 0); err != nil {
		t.Fatalf("ack applied delete: %v", err)
	}
	if err := service.AckDownloadAction(context.Background(), principal, store.workspace.ID, session.ID, oldFile.Path, workspace.ChangeDelete, "", 0); err != nil {
		t.Fatalf("repeat applied delete acknowledgement: %v", err)
	}
	current, err := service.GetSyncSession(context.Background(), principal, store.workspace.ID, session.ID)
	if err != nil || current.ConfirmedBytes != 0 || !current.DownloadActions[0].Applied {
		t.Fatalf("delete acknowledgement progress = %#v err=%v", current, err)
	}
	if _, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, "sync-delete-action-commit"); err != nil {
		t.Fatalf("commit after delete acknowledgement: %v", err)
	}
}

func TestWorkspaceSyncServiceBoundsChangesWait(t *testing.T) {
	store := newSyncServiceStore()
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	cursor := workspace.EncodeChangeCursor(workspace.ChangeCursor{Revision: 0})
	start := time.Now()
	_, err = service.GetChanges(context.Background(), principal, store.workspace.ID, cursor, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("zero-wait changes query took %s", time.Since(start))
	}
}
