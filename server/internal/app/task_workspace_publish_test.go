package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func taskWorkspacePublishFixture(t *testing.T, store WorkspaceSyncStore, blocks WorkspaceBlockStore) (*WorkspaceSyncService, TaskWorkspacePublishCommand, workspace.File, []byte) {
	t.Helper()
	service, err := NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("published output")
	digest := sha256.Sum256(data)
	hexDigest := hex.EncodeToString(digest[:])
	file := workspace.File{
		Path: "reports/result.txt", SizeBytes: int64(len(data)), SHA256: hexDigest, Mode: 0o644,
		Chunks: []workspace.Chunk{{SHA256: hexDigest, SizeBytes: int64(len(data))}},
	}
	command := TaskWorkspacePublishCommand{
		OwnerID: "u1234567", TaskID: "q1234567", WorkspaceID: "w1234567", HostID: "h1234567",
		LeaseOwner: "worker-1", LeaseVersion: 1, BindingVersion: 1, InputRevision: 0, BaseRevision: 0,
		HostFence: AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: "runtime-1", AIAllowed: true},
		ViewID:    "v" + strings.Repeat("a", 64), InputManifestSHA256: strings.Repeat("b", 64),
		OperationKey: "task-workspace-publish:s1234567:w1234567", RequestHash: make([]byte, 32),
	}
	return service, command, file, data
}

func TestTaskWorkspacePublishPersistsOutputAndReplaysReceipt(t *testing.T) {
	store := newSyncServiceStore()
	blocks := newSyncBlockMemory()
	service, command, file, data := taskWorkspacePublishFixture(t, store, blocks)
	if _, err := service.ReserveTaskWorkspacePublish(context.Background(), command); err != nil {
		t.Fatalf("ReserveTaskWorkspacePublish() error = %v", err)
	}
	manifestSHA, err := workspace.ManifestSHA256([]workspace.File{file})
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	receipt, err := service.CommitTaskWorkspacePublish(context.Background(), command, []workspace.File{file}, nil, manifestSHA, func(_ context.Context, digest string) ([]byte, error) {
		reads++
		if digest != file.SHA256 {
			t.Fatalf("block digest = %q; want %q", digest, file.SHA256)
		}
		return append([]byte(nil), data...), nil
	})
	if err != nil || receipt.State != TaskWorkspacePublishSucceeded || receipt.PublishedRevision != 1 || reads != 1 {
		t.Fatalf("CommitTaskWorkspacePublish() = %#v, reads=%d, err=%v", receipt, reads, err)
	}
	if store.workspace.Revision != 1 || len(store.snapshots[1]) != 1 || store.snapshots[1][0].SHA256 != file.SHA256 {
		t.Fatalf("committed Workspace = %#v, snapshot = %#v", store.workspace, store.snapshots[1])
	}
	proof, err := service.VerifyCommittedArtifact(context.Background(), TaskArtifactProof{
		OwnerID: command.OwnerID, WorkspaceID: command.WorkspaceID, Revision: receipt.PublishedRevision,
		Path: file.Path, SizeBytes: file.SizeBytes, SHA256: file.SHA256,
	})
	if err != nil || proof.Revision != receipt.PublishedRevision {
		t.Fatalf("VerifyCommittedArtifact() = %#v, %v", proof, err)
	}
	replayed, err := service.CommitTaskWorkspacePublish(context.Background(), command, []workspace.File{file}, nil, manifestSHA, func(context.Context, string) ([]byte, error) {
		t.Fatal("idempotent publish replay fetched Runtime block bytes")
		return nil, nil
	})
	if err != nil || replayed != receipt || reads != 1 {
		t.Fatalf("idempotent CommitTaskWorkspacePublish() = %#v, reads=%d, err=%v", replayed, reads, err)
	}
}

func TestTaskWorkspacePublishPersistsCASConflict(t *testing.T) {
	store := newSyncServiceStore()
	blocks := newSyncBlockMemory()
	service, command, file, data := taskWorkspacePublishFixture(t, store, blocks)
	if _, err := service.ReserveTaskWorkspacePublish(context.Background(), command); err != nil {
		t.Fatalf("ReserveTaskWorkspacePublish() error = %v", err)
	}
	store.workspace.Revision = 1
	manifestSHA, _ := workspace.ManifestSHA256([]workspace.File{file})
	receipt, err := service.CommitTaskWorkspacePublish(context.Background(), command, []workspace.File{file}, nil, manifestSHA, func(context.Context, string) ([]byte, error) {
		return append([]byte(nil), data...), nil
	})
	if domain.CodeOf(err) != domain.CodeWorkspaceConflict || receipt.State != TaskWorkspacePublishConflict || len(store.snapshots[2]) != 0 {
		t.Fatalf("CAS conflict = %#v, err=%v, revisions=%#v", receipt, err, store.snapshots)
	}
	stored, err := service.FindTaskWorkspacePublishReceipt(context.Background(), command.OwnerID, command.TaskID, command.WorkspaceID, command.OperationKey)
	if err != nil || stored.State != TaskWorkspacePublishConflict {
		t.Fatalf("FindTaskWorkspacePublishReceipt() = %#v, %v", stored, err)
	}
}

type taskWorkspaceQuotaStore struct {
	*syncServiceStore
}

func (s *taskWorkspaceQuotaStore) ReserveTaskWorkspaceBlockUsage(context.Context, string, []WorkspaceBlockReservation, int64) error {
	return domain.NewError(domain.CodeQuotaExceeded, "physical Workspace quota exceeded")
}

func TestTaskWorkspacePublishQuotaRejectsPendingReceipt(t *testing.T) {
	base := newSyncServiceStore()
	store := &taskWorkspaceQuotaStore{syncServiceStore: base}
	service, command, file, data := taskWorkspacePublishFixture(t, store, newSyncBlockMemory())
	if _, err := service.ReserveTaskWorkspacePublish(context.Background(), command); err != nil {
		t.Fatalf("ReserveTaskWorkspacePublish() error = %v", err)
	}
	manifestSHA, _ := workspace.ManifestSHA256([]workspace.File{file})
	_, err := service.CommitTaskWorkspacePublish(context.Background(), command, []workspace.File{file}, nil, manifestSHA, func(context.Context, string) ([]byte, error) {
		return append([]byte(nil), data...), nil
	})
	if domain.CodeOf(err) != domain.CodeQuotaExceeded {
		t.Fatalf("CommitTaskWorkspacePublish() error = %v; want physical quota rejection", err)
	}
	receipt, err := service.FindTaskWorkspacePublishReceipt(context.Background(), command.OwnerID, command.TaskID, command.WorkspaceID, command.OperationKey)
	if err != nil || receipt.State != TaskWorkspacePublishQuotaRejected {
		t.Fatalf("quota receipt = %#v, %v; want quota_rejected", receipt, err)
	}
}
