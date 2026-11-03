package postgres

import (
	"context"
	"io"
	"path/filepath"
	"runtime"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/storage/workspacecurrent"
)

func TestPostgresWorkspaceRestoreMaterializationFailureKeepsCurrentTreeAndRetries(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("atomic Workspace current directory replacement requires Linux")
	}
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	workspaceValue := createTestWorkspace(t, ctx, store, owner.ID, "restore-materialization-recovery")

	oldBody := []byte("revision one")
	oldFile := syncTestFile("docs/state.txt", oldBody)
	oldSession := newTestSyncSession(t, workspaceValue, owner.ID, []workspace.File{oldFile}, []workspace.File{oldFile}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, oldSession, syncTestBlocks([]workspace.File{oldFile}),
		syncTestIdempotency(owner.ID, "workspace.sync.create", "restore-recovery:create-1", oldSession.ID)); err != nil {
		t.Fatalf("create first Workspace revision: %v", err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, workspaceValue.ID, oldSession.ID,
		syncTestIdempotency(owner.ID, "workspace.sync.commit", "restore-recovery:commit-1", oldSession.ID)); err != nil {
		t.Fatalf("commit first Workspace revision: %v", err)
	}

	newBody := []byte("revision two")
	newFile := syncTestFile("docs/state.txt", newBody)
	workspaceValue, err := store.GetWorkspace(ctx, workspaceValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	newSession := newTestSyncSession(t, workspaceValue, owner.ID, []workspace.File{newFile}, []workspace.File{newFile}, nil)
	if _, _, err := createTestWorkspaceSyncSession(ctx, store, newSession, syncTestBlocks([]workspace.File{newFile}),
		syncTestIdempotency(owner.ID, "workspace.sync.create", "restore-recovery:create-2", newSession.ID)); err != nil {
		t.Fatalf("create second Workspace revision: %v", err)
	}
	if _, _, err := commitWorkspaceSyncSession(ctx, store, workspaceValue.ID, newSession.ID,
		syncTestIdempotency(owner.ID, "workspace.sync.commit", "restore-recovery:commit-2", newSession.ID)); err != nil {
		t.Fatalf("commit second Workspace revision: %v", err)
	}

	blocks := workspaceTransferProjectionBlockStore{
		oldFile.Chunks[0].SHA256: oldBody,
		newFile.Chunks[0].SHA256: newBody,
	}
	tempDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	current, err := workspacecurrent.Open(filepath.Join(tempDir, "current"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := current.Close(); err != nil {
			t.Errorf("close current Workspace store: %v", err)
		}
	}()
	if err := current.Materialize(ctx, owner.ID, workspaceValue.ID, 2, []workspace.File{newFile}, nil, nil, blocks); err != nil {
		t.Fatalf("publish current Workspace revision before restore: %v", err)
	}
	if err := store.MarkWorkspaceCurrentMaterialized(ctx, workspaceValue.ID, 2); err != nil {
		t.Fatalf("record current Workspace revision before restore: %v", err)
	}

	service, err := app.NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetWorkspaceCurrentRevisionReader(current.CurrentRevision); err != nil {
		t.Fatal(err)
	}
	if err := service.SetWorkspaceCurrentManifestVerifier(current.MatchesManifest); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	var materializeErr error
	if err := service.SetWorkspaceCurrentMaterializer(func(ctx context.Context, ownerID, workspaceID string, revision int64,
		files []workspace.File, emptyDirectories []string, directories []workspace.Directory) error {
		attempts++
		if attempts == 1 {
			materializeErr = current.Materialize(ctx, ownerID, workspaceID, revision, files, emptyDirectories, directories, workspaceTransferProjectionBlockStore{})
			return materializeErr
		}
		materializeErr = current.Materialize(ctx, ownerID, workspaceID, revision, files, emptyDirectories, directories, blocks)
		return materializeErr
	}); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t" + owner.ID[1:]}
	command := app.WorkspaceRestoreCommand{TargetRevision: 1, ExpectedCurrentRevision: 2, Paths: []string{"docs/state.txt"}}
	const key = "restore-materialization-recovery-01"
	if _, err := service.Restore(ctx, principal, workspaceValue.ID, command, key); domain.CodeOf(err) != domain.CodeWorkspaceBusy {
		t.Fatalf("first Restore() error = %v, want materialization pending conflict", err)
	}
	if revision, ok, err := current.CurrentRevision(owner.ID, workspaceValue.ID); err != nil || !ok || revision != 2 {
		t.Fatalf("current revision after failed restore materialization = %d, ok=%t, err=%v; want intact revision 2", revision, ok, err)
	}
	if matched, err := current.MatchesManifest(ctx, owner.ID, workspaceValue.ID, []workspace.File{newFile}, nil, nil); err != nil || !matched {
		t.Fatalf("current Workspace after failed restore materialization matched=%t err=%v; want previous tree intact", matched, err)
	}

	result, err := service.Restore(ctx, principal, workspaceValue.ID, command, key)
	if err != nil {
		var desired, applied int64
		_ = pool.QueryRow(ctx, `SELECT desired_revision,applied_revision FROM workspace_current_materializations WHERE workspace_id=$1`, workspaceValue.ID).Scan(&desired, &applied)
		currentRevision, exists, revisionErr := current.CurrentRevision(owner.ID, workspaceValue.ID)
		t.Fatalf("retry Restore() after materialization failure: %v; attempts=%d materializer_error=%v current=(%d,%t,%v) database=(%d,%d)", err, attempts, materializeErr, currentRevision, exists, revisionErr, desired, applied)
	}
	if result.Revision != 3 || attempts != 2 {
		t.Fatalf("Restore() result=%#v materializer attempts=%d; want committed revision 3 and retry", result, attempts)
	}
	if revision, ok, err := current.CurrentRevision(owner.ID, workspaceValue.ID); err != nil || !ok || revision != 3 {
		t.Fatalf("current revision after retry = %d, ok=%t, err=%v; want 3", revision, ok, err)
	}
	if matched, err := current.MatchesManifest(ctx, owner.ID, workspaceValue.ID, []workspace.File{oldFile}, nil, nil); err != nil || !matched {
		t.Fatalf("restored current Workspace matched=%t err=%v; want revision-one contents", matched, err)
	}
	file, _, err := current.OpenRegularFile(owner.ID, workspaceValue.ID, oldFile.Path)
	if err != nil {
		t.Fatalf("open restored file: %v", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil || string(data) != string(oldBody) {
		t.Fatalf("restored file = %q, err=%v; want %q", data, err, oldBody)
	}
}
