package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type retrySyncBlocks struct {
	*syncBlockMemory
	openError error
}

func (b *retrySyncBlocks) Open(workspaceID, digest string) (io.ReadCloser, error) {
	if b.openError != nil {
		return nil, b.openError
	}
	return b.syncBlockMemory.Open(workspaceID, digest)
}

func TestWorkspaceSyncCommitRetriesTemporaryBlockReadFailure(t *testing.T) {
	store, blocks, service, principal, session, file := uploadedSyncFixture(t)
	temporary := errors.New("temporary storage error")
	blocks.openError = temporary
	if _, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, "commit-retry"); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("first commit error = %v, want retryable workspace busy", err)
	}
	if got := store.sessions[session.ID].State; got == workspace.SyncFailed || got == workspace.SyncCompleted {
		t.Fatalf("first commit state = %s, want an active session", got)
	}
	if store.workspace.Revision != 0 {
		t.Fatalf("failed commit changed revision to %d", store.workspace.Revision)
	}
	blocks.openError = nil
	committed, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, "commit-retry")
	if err != nil || committed.State != workspace.SyncCompleted || committed.TargetRevision != 1 {
		t.Fatalf("retried commit = %#v, %v", committed, err)
	}
	if files := store.snapshots[1]; len(files) != 1 || files[0].SHA256 != file.SHA256 {
		t.Fatalf("committed manifest = %#v", files)
	}
}

func TestWorkspaceSyncCommitRejectsCorruptUploadedBlock(t *testing.T) {
	store, blocks, service, principal, session, file := uploadedSyncFixture(t)
	blocks.data[file.Chunks[0].SHA256] = []byte("bad")
	if _, err := service.CommitSyncSession(context.Background(), principal, store.workspace.ID, session.ID, "commit-corrupt"); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("corrupt commit error = %v, want workspace conflict", err)
	}
	if store.sessions[session.ID].State != workspace.SyncFailed || store.workspace.Revision != 0 {
		t.Fatalf("corrupt commit changed session/revision: %s, %d", store.sessions[session.ID].State, store.workspace.Revision)
	}
}

func uploadedSyncFixture(t *testing.T) (*syncServiceStore, *retrySyncBlocks, *WorkspaceSyncService, identity.Principal, workspace.SyncSession, workspace.File) {
	t.Helper()
	store := newSyncServiceStore()
	blocks := &retrySyncBlocks{syncBlockMemory: newSyncBlockMemory()}
	service, err := NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession}
	body := []byte("abc")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	file := workspace.File{Path: "src/main.go", SizeBytes: int64(len(body)), SHA256: digest, Mode: 0644, Chunks: []workspace.Chunk{{SHA256: digest, SizeBytes: int64(len(body))}}}
	session, err := service.CreateSyncSession(context.Background(), principal, store.workspace.ID, workspace.CreateSyncSession{
		Direction: workspace.SyncUpload, Policy: workspace.SyncLive, BaseRevision: 0, Files: []workspace.File{file},
	}, "create-retry")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutSyncBlock(context.Background(), principal, store.workspace.ID, session.ID, digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	return store, blocks, service, principal, session, file
}
