package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

type proofBlocks struct {
	*syncBlockMemory
	beforeOpen func()
}

type proofManifestStore struct {
	*syncServiceStore
	page workspace.ManifestPage
}

type proofLockStore struct {
	*syncServiceStore
	afterLock  func()
	releaseErr error
}

func (s *proofLockStore) TryWorkspaceBlockStoreReadLock(ctx context.Context, workspaceID string) (LeaderLock, bool, error) {
	lock, acquired, err := s.syncServiceStore.TryWorkspaceBlockStoreReadLock(ctx, workspaceID)
	if acquired && s.afterLock != nil {
		s.afterLock()
	}
	if acquired && s.releaseErr != nil {
		lock = proofReleaseLock{LeaderLock: lock, err: s.releaseErr}
	}
	return lock, acquired, err
}

type proofReleaseLock struct {
	LeaderLock
	err error
}

func (l proofReleaseLock) Release(ctx context.Context) error {
	return errors.Join(l.LeaderLock.Release(ctx), l.err)
}

func (s *proofManifestStore) GetWorkspaceManifestPage(context.Context, string, int64, string, string, int32) (workspace.ManifestPage, error) {
	return s.page, nil
}

func (b *proofBlocks) Open(workspaceID, digest string) (io.ReadCloser, error) {
	if b.beforeOpen != nil {
		b.beforeOpen()
	}
	return b.syncBlockMemory.Open(workspaceID, digest)
}

func proofFixture(t *testing.T) (*WorkspaceSyncService, *syncServiceStore, *proofBlocks, TaskArtifactProof) {
	t.Helper()
	store := newSyncServiceStore()
	store.workspace.Revision = 4
	blocks := &proofBlocks{syncBlockMemory: newSyncBlockMemory()}
	first, second := []byte("hello "), []byte("world")
	firstHash, secondHash, fileHash := sha256.Sum256(first), sha256.Sum256(second), sha256.Sum256(append(append([]byte(nil), first...), second...))
	firstDigest, secondDigest, fileDigest := hex.EncodeToString(firstHash[:]), hex.EncodeToString(secondHash[:]), hex.EncodeToString(fileHash[:])
	blocks.data[firstDigest] = first
	blocks.data[secondDigest] = second
	file := workspace.File{
		Path: "output/result.txt", SizeBytes: int64(len(first) + len(second)), SHA256: fileDigest, Mode: 0644,
		Chunks: []workspace.Chunk{{SHA256: firstDigest, SizeBytes: int64(len(first))}, {SHA256: secondDigest, SizeBytes: int64(len(second))}},
	}
	store.snapshots[4] = []workspace.File{file}
	service, err := NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	return service, store, blocks, TaskArtifactProof{
		OwnerID: store.workspace.OwnerID, WorkspaceID: store.workspace.ID, Revision: 4,
		Path: file.Path, SizeBytes: file.SizeBytes, SHA256: file.SHA256,
	}
}

func TestVerifyCommittedArtifactReadsPinnedRevision(t *testing.T) {
	service, store, blocks, claim := proofFixture(t)
	opens := 0
	blocks.beforeOpen = func() {
		opens++
		if store.blockReadLocksHeld != 1 {
			t.Error("artifact block was read without the shared storage lock")
		}
	}
	proof, err := service.VerifyCommittedArtifact(context.Background(), claim)
	if err != nil || proof != claim || opens != 2 {
		t.Fatalf("verified proof = %#v, opens = %d, err = %v", proof, opens, err)
	}
	if store.blockReadLocksHeld != 0 || store.blockReadLockReleases != 1 {
		t.Fatal("artifact verification did not release its storage lock")
	}
}

func TestVerifyCommittedArtifactRevisionZeroIsNotLatest(t *testing.T) {
	service, store, _, claim := proofFixture(t)
	old := syncServiceFile(claim.Path, nil)
	store.snapshots[0] = []workspace.File{old}
	claim.Revision, claim.SizeBytes, claim.SHA256 = 0, 0, old.SHA256
	proof, err := service.VerifyCommittedArtifact(context.Background(), claim)
	if err != nil || proof.Revision != 0 || proof.SHA256 != old.SHA256 {
		t.Fatalf("revision-zero proof = %#v, err = %v", proof, err)
	}
	claim.SHA256 = store.snapshots[4][0].SHA256
	if _, err := service.VerifyCommittedArtifact(context.Background(), claim); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("latest hash was accepted for revision zero: %v", err)
	}
}

func TestVerifyCommittedArtifactRejectsInvalidClaims(t *testing.T) {
	cases := []struct {
		name string
		edit func(*TaskArtifactProof)
		code domain.ErrorCode
	}{
		{name: "other owner", edit: func(p *TaskArtifactProof) { p.OwnerID = "u7654321" }, code: domain.CodeNotFound},
		{name: "absolute path", edit: func(p *TaskArtifactProof) { p.Path = "/etc/passwd" }, code: domain.CodeInvalidArgument},
		{name: "parent path", edit: func(p *TaskArtifactProof) { p.Path = "../result.txt" }, code: domain.CodeInvalidArgument},
		{name: "missing path", edit: func(p *TaskArtifactProof) { p.Path = "output/missing.txt" }, code: domain.CodeNotFound},
		{name: "future revision", edit: func(p *TaskArtifactProof) { p.Revision = 5 }, code: domain.CodeWorkspaceConflict},
		{name: "missing committed revision", edit: func(p *TaskArtifactProof) { p.Revision = 3 }, code: domain.CodeNotFound},
		{name: "wrong size", edit: func(p *TaskArtifactProof) { p.SizeBytes++ }, code: domain.CodeWorkspaceConflict},
		{name: "wrong digest", edit: func(p *TaskArtifactProof) { p.SHA256 = strings.Repeat("a", 64) }, code: domain.CodeWorkspaceConflict},
		{name: "invalid digest", edit: func(p *TaskArtifactProof) { p.SHA256 = strings.Repeat("A", 64) }, code: domain.CodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			service, _, blocks, claim := proofFixture(t)
			tc.edit(&claim)
			opens := 0
			blocks.beforeOpen = func() { opens++ }
			proof, err := service.VerifyCommittedArtifact(context.Background(), claim)
			if domain.CodeOf(err) != tc.code || proof != (TaskArtifactProof{}) || opens != 0 {
				t.Fatalf("proof = %#v, opened blocks = %d, err = %v", proof, opens, err)
			}
		})
	}
}

func TestVerifyCommittedArtifactRejectsMissingOrCorruptBlocks(t *testing.T) {
	for _, name := range []string{"missing", "corrupt", "wrong file digest"} {
		t.Run(name, func(t *testing.T) {
			service, store, blocks, claim := proofFixture(t)
			file := store.snapshots[4][0]
			switch name {
			case "missing":
				delete(blocks.data, file.Chunks[1].SHA256)
			case "corrupt":
				blocks.data[file.Chunks[1].SHA256] = []byte("wrong")
			case "wrong file digest":
				claim.SHA256 = strings.Repeat("a", 64)
				file.SHA256 = claim.SHA256
				store.snapshots[4] = []workspace.File{file}
			}
			proof, err := service.VerifyCommittedArtifact(context.Background(), claim)
			if err == nil || proof != (TaskArtifactProof{}) {
				t.Fatalf("corrupt file produced proof = %#v, err = %v", proof, err)
			}
			if name == "missing" && domain.CodeOf(err) != domain.CodeNotFound {
				t.Fatalf("missing block error = %v", err)
			}
			if name != "missing" && domain.CodeOf(err) != domain.CodeWorkspaceConflict {
				t.Fatalf("corrupt file error = %v", err)
			}
			if store.blockReadLocksHeld != 0 {
				t.Fatal("failed proof leaked its storage lock")
			}
		})
	}
}

func TestVerifyCommittedArtifactRejectsConflictingManifest(t *testing.T) {
	_, store, blocks, claim := proofFixture(t)
	file := store.snapshots[4][0]
	for _, page := range []workspace.ManifestPage{
		{WorkspaceID: "w7654321", Revision: 4, Files: []workspace.File{file}},
		{WorkspaceID: claim.WorkspaceID, Revision: 3, Files: []workspace.File{file}},
		{WorkspaceID: claim.WorkspaceID, Revision: 4, Files: []workspace.File{file, file}},
	} {
		service, err := NewWorkspaceSyncService(&proofManifestStore{syncServiceStore: store, page: page}, blocks, workspace.MaxSyncManifestBytes, 1<<40)
		if err != nil {
			t.Fatal(err)
		}
		proof, err := service.VerifyCommittedArtifact(context.Background(), claim)
		if domain.CodeOf(err) != domain.CodeWorkspaceConflict || proof != (TaskArtifactProof{}) {
			t.Fatalf("conflicting manifest produced proof = %#v, err = %v", proof, err)
		}
	}
}

func TestVerifyCommittedArtifactExcludesConcurrentBlockGC(t *testing.T) {
	service, store, blocks, claim := proofFixture(t)
	entered, proceed, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	blocks.beforeOpen = func() {
		if entered != nil {
			close(entered)
			<-proceed
			entered = nil
		}
	}
	go func() {
		_, err := service.VerifyCommittedArtifact(context.Background(), claim)
		done <- err
	}()
	<-entered
	lock, acquired, err := store.TryWorkspaceBlockStoreLock(context.Background(), claim.WorkspaceID)
	if err != nil || acquired || lock != nil {
		t.Fatalf("block GC acquired exclusive lock during read: acquired=%v err=%v", acquired, err)
	}
	close(proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lock, acquired, err = store.TryWorkspaceBlockStoreLock(context.Background(), claim.WorkspaceID)
	if err != nil || !acquired || lock == nil {
		t.Fatalf("block GC remained blocked after read: acquired=%v err=%v", acquired, err)
	}
	if err := lock.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyCommittedArtifactRechecksWorkspaceAfterLock(t *testing.T) {
	_, store, blocks, claim := proofFixture(t)
	wrapped := &proofLockStore{syncServiceStore: store, afterLock: func() { store.workspace.State = workspace.StateDeleting }}
	service, err := NewWorkspaceSyncService(wrapped, blocks, workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := service.VerifyCommittedArtifact(context.Background(), claim)
	if domain.CodeOf(err) != domain.CodeNotFound || proof != (TaskArtifactProof{}) || store.blockReadLocksHeld != 0 {
		t.Fatalf("workspace deleted before lock produced proof = %#v, err = %v", proof, err)
	}
}

func TestVerifyCommittedArtifactLockReleaseFailureReturnsNoProof(t *testing.T) {
	_, store, blocks, claim := proofFixture(t)
	releaseErr := errors.New("lock release failed")
	service, err := NewWorkspaceSyncService(&proofLockStore{syncServiceStore: store, releaseErr: releaseErr}, blocks, workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := service.VerifyCommittedArtifact(context.Background(), claim)
	if !errors.Is(err, releaseErr) || proof != (TaskArtifactProof{}) || store.blockReadLocksHeld != 0 {
		t.Fatalf("failed release returned proof = %#v, err = %v", proof, err)
	}
}

func TestVerifyCommittedArtifactCancellationReturnsNoProof(t *testing.T) {
	service, store, blocks, claim := proofFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	blocks.beforeOpen = cancel
	proof, err := service.VerifyCommittedArtifact(ctx, claim)
	if !errors.Is(err, context.Canceled) || proof != (TaskArtifactProof{}) || store.blockReadLocksHeld != 0 {
		t.Fatalf("cancelled proof = %#v, err = %v", proof, err)
	}
}

func TestVerifyCommittedArtifactRejectsDeletedWorkspace(t *testing.T) {
	service, store, _, claim := proofFixture(t)
	now := store.workspace.CreatedAt
	store.workspace.DeletedAt = &now
	if _, err := service.VerifyCommittedArtifact(context.Background(), claim); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("deleted workspace produced proof: %v", err)
	}
	if _, err := service.VerifyCommittedArtifact(nil, claim); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("nil context error = %v", err)
	}
}
