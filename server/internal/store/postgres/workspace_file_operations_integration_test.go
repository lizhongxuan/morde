package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"reflect"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestPostgresWorkspaceFileOperationRenamesAndReplaysIdempotently(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "rename file")
	file := syncTestFile("README.md", []byte("readme"))
	seedWorkspaceRestoreRevision(t, ctx, store, value, file)
	operation := app.WorkspaceFileOperationCommand{
		Action: app.WorkspaceFileRename, SourcePath: "README.md", TargetPath: "docs/README.md", ExpectedCurrentRevision: 1, WriteEnabled: true,
	}
	record := workspaceFileOperationRecord(owner.ID, value.ID, "rename-file")
	result, err := store.ApplyWorkspaceFileOperation(ctx, owner.ID, value.ID, operation, record)
	if err != nil || result.Revision != 2 || result.Replayed {
		t.Fatalf("Workspace file rename = %#v, err=%v", result, err)
	}
	page, err := store.GetWorkspaceManifestPage(ctx, value.ID, 2, "", "", 10)
	if err != nil || len(page.Files) != 1 || page.Files[0].Path != "docs/README.md" || page.Files[0].SHA256 != file.SHA256 {
		t.Fatalf("renamed Workspace manifest = %#v, err=%v", page, err)
	}
	changes, err := store.GetWorkspaceChangePage(ctx, value.ID, 1, file.Path, 10)
	changeActions := map[string]workspace.ChangeAction{}
	for _, change := range changes.Items {
		changeActions[change.Path] = change.Action
	}
	if err != nil || len(changes.Items) != 2 || changeActions[file.Path] != workspace.ChangeDelete ||
		changeActions["docs/README.md"] != workspace.ChangeUpsert {
		t.Fatalf("rename change feed = %#v, err=%v", changes, err)
	}
	replayed, err := store.ApplyWorkspaceFileOperation(ctx, owner.ID, value.ID, operation, record)
	if err != nil || !replayed.Replayed || replayed.Revision != 2 {
		t.Fatalf("replayed Workspace file rename = %#v, err=%v", replayed, err)
	}
	other := record
	differentHash := sha256.Sum256([]byte("different request"))
	other.RequestHash = differentHash[:]
	if _, err := store.ApplyWorkspaceFileOperation(ctx, owner.ID, value.ID, operation, other); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("reused file operation key error = %v, want idempotency conflict", err)
	}
}

func TestPostgresWorkspaceFileOperationRenamesSymlinkMetadataWithoutFollowingTarget(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "rename symlink")
	link := syncTestFile("a/link", []byte("../not-created"))
	link.Kind, link.SymlinkTarget, link.Mode, link.Chunks = workspace.FileKindSymlink, "../not-created", 0, []workspace.Chunk{}
	seedWorkspaceRestoreRevision(t, ctx, store, value, link)
	operation := app.WorkspaceFileOperationCommand{
		Action: app.WorkspaceFileMove, SourcePath: "a/link", TargetPath: "b/link", ExpectedCurrentRevision: 1, WriteEnabled: true,
	}
	result, err := store.ApplyWorkspaceFileOperation(ctx, owner.ID, value.ID, operation,
		workspaceFileOperationRecord(owner.ID, value.ID, "move-symlink"))
	if err != nil || result.Revision != 2 {
		t.Fatalf("Workspace symlink move = %#v, err=%v", result, err)
	}
	page, err := store.GetWorkspaceManifestPage(ctx, value.ID, 2, "", "", 10)
	if err != nil || len(page.Files) != 1 || page.Files[0].Path != "b/link" || page.Files[0].Kind != workspace.FileKindSymlink ||
		page.Files[0].SymlinkTarget != "../not-created" || len(page.Files[0].Chunks) != 0 {
		t.Fatalf("moved Workspace symlink metadata = %#v, err=%v", page.Files, err)
	}
}

func TestPostgresWorkspaceFileOperationRejectsMoveThatWouldMakeSymlinkEscape(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 5, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "unsafe symlink move")
	link := syncTestFile("a/b/link", []byte("../../outside"))
	link.Kind, link.SymlinkTarget, link.Mode, link.Chunks = workspace.FileKindSymlink, "../../outside", 0, []workspace.Chunk{}
	seedWorkspaceRestoreRevision(t, ctx, store, value, link)
	operation := app.WorkspaceFileOperationCommand{
		Action: app.WorkspaceFileMove, SourcePath: "a/b/link", TargetPath: "a/link", ExpectedCurrentRevision: 1, WriteEnabled: true,
	}
	_, err := store.ApplyWorkspaceFileOperation(ctx, owner.ID, value.ID, operation,
		workspaceFileOperationRecord(owner.ID, value.ID, "move-symlink-escape"))
	if domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("unsafe Workspace symlink move = %v; want invalid argument", err)
	}
	page, readErr := store.GetWorkspaceManifestPage(ctx, value.ID, 1, "", "", 10)
	if readErr != nil || len(page.Files) != 1 || page.Files[0].Path != "a/b/link" || page.Files[0].SymlinkTarget != "../../outside" {
		t.Fatalf("Workspace changed after unsafe symlink move: %#v, err=%v", page.Files, readErr)
	}
}

func TestPostgresWorkspaceFileOperationMovesDirectorySubtree(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "move directory")
	file := syncTestFile("src/main.go", []byte("package main"))
	seedWorkspaceRestoreRevision(t, ctx, store, value, file)
	operation := app.WorkspaceFileOperationCommand{
		Action: app.WorkspaceFileMove, SourcePath: "src", TargetPath: "archive/src", ExpectedCurrentRevision: 1, WriteEnabled: true,
	}
	result, err := store.ApplyWorkspaceFileOperation(ctx, owner.ID, value.ID, operation, workspaceFileOperationRecord(owner.ID, value.ID, "move-directory"))
	if err != nil || result.Revision != 2 {
		t.Fatalf("Workspace directory move = %#v, err=%v", result, err)
	}
	page, err := store.GetWorkspaceManifestPage(ctx, value.ID, 2, "", "", 10)
	if err != nil || len(page.Files) != 1 || page.Files[0].Path != "archive/src/main.go" || page.Files[0].SHA256 != file.SHA256 {
		t.Fatalf("moved directory manifest = %#v, err=%v", page, err)
	}
}

func TestPostgresWorkspaceFileOperationDeletesFileAsNewRevision(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "delete file")
	file := syncTestFile("notes/today.txt", []byte("private note"))
	seedWorkspaceRestoreRevision(t, ctx, store, value, file)
	operation := app.WorkspaceFileOperationCommand{
		Action: app.WorkspaceFileDelete, SourcePath: file.Path, ExpectedCurrentRevision: 1, WriteEnabled: true,
	}
	result, err := store.ApplyWorkspaceFileOperation(ctx, owner.ID, value.ID, operation, workspaceFileOperationRecord(owner.ID, value.ID, "delete-file"))
	if err != nil || result.Revision != 2 {
		t.Fatalf("Workspace file delete = %#v, err=%v", result, err)
	}
	page, err := store.GetWorkspaceManifestPage(ctx, value.ID, 2, "", "", 10)
	current, currentErr := store.GetWorkspace(ctx, value.ID)
	if err != nil || currentErr != nil || len(page.Files) != 0 || current.Revision != 2 || current.UsedBytes != 0 {
		t.Fatalf("deleted Workspace manifest/current = %#v / %#v, err=%v/%v", page, current, err, currentErr)
	}
}

func TestPostgresWorkspaceFileOperationDeletesSymlinkEntryWithoutFollowingTarget(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 6, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "delete symlink")
	link := syncTestFile("a/link", []byte("../not-created"))
	link.Kind, link.SymlinkTarget, link.Mode, link.Chunks = workspace.FileKindSymlink, "../not-created", 0, []workspace.Chunk{}
	seedWorkspaceRestoreRevision(t, ctx, store, value, link)
	operation := app.WorkspaceFileOperationCommand{
		Action: app.WorkspaceFileDelete, SourcePath: link.Path, ExpectedCurrentRevision: 1, WriteEnabled: true,
	}
	result, err := store.ApplyWorkspaceFileOperation(ctx, owner.ID, value.ID, operation,
		workspaceFileOperationRecord(owner.ID, value.ID, "delete-symlink"))
	if err != nil || result.Revision != 2 {
		t.Fatalf("Workspace symlink delete = %#v, err=%v", result, err)
	}
	page, err := store.GetWorkspaceManifestPage(ctx, value.ID, 2, "", "", 10)
	if err != nil || len(page.Files) != 0 {
		t.Fatalf("Workspace manifest after deleting symlink = %#v, err=%v", page.Files, err)
	}
}

func TestPostgresWorkspaceFileOperationRejectsStaleAndInFlightWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stale      bool
		activeSync bool
		activeHost bool
	}{
		{name: "stale revision", stale: true},
		{name: "in-flight sync", activeSync: true},
		{name: "active Host binding", activeHost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _ := newTemporaryStore(t)
			ctx := context.Background()
			owner := createTestUser(t, store, 3, 10_000, 20_000)
			value := createTestWorkspace(t, ctx, store, owner.ID, "file operation fence")
			file := syncTestFile("notes.txt", []byte("note"))
			seedWorkspaceRestoreRevision(t, ctx, store, value, file)
			value.Revision = 1
			if tc.activeSync {
				session := newTestSyncSession(t, value, owner.ID, nil, nil, nil)
				if _, _, err := createTestWorkspaceSyncSession(ctx, store, session, nil,
					syncTestIdempotency(owner.ID, "workspace.sync.create", "file-op-fence:"+value.ID, session.ID)); err != nil {
					t.Fatalf("create active Workspace sync session: %v", err)
				}
			}
			if tc.activeHost {
				createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{value.ID})
			}
			expectedRevision := int64(1)
			wantCode := domain.CodeWorkspaceBusy
			if tc.stale {
				expectedRevision, wantCode = 0, domain.CodeWorkspaceConflict
			}
			operation := app.WorkspaceFileOperationCommand{
				Action: app.WorkspaceFileDelete, SourcePath: file.Path, ExpectedCurrentRevision: expectedRevision, WriteEnabled: true,
			}
			_, err := store.ApplyWorkspaceFileOperation(ctx, owner.ID, value.ID, operation,
				workspaceFileOperationRecord(owner.ID, value.ID, "file-op-fence:"+tc.name))
			if domain.CodeOf(err) != wantCode {
				t.Fatalf("file operation error = %v, want %s", err, wantCode)
			}
			current, getErr := store.GetWorkspace(ctx, value.ID)
			if getErr != nil || current.Revision != 1 || current.UsedBytes != file.SizeBytes {
				t.Fatalf("Workspace changed after rejected file operation = %#v, err=%v", current, getErr)
			}
		})
	}
}

func TestPostgresWorkspaceFileOperationFailsClosedForCredentialInDirectorySubtree(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 10_000, 20_000)
	value := createTestWorkspace(t, ctx, store, owner.ID, "credential subtree")
	file := syncTestFile("project/.ssh/id_ed25519", []byte("key material"))
	seedWorkspaceRestoreRevision(t, ctx, store, value, file)
	operation := app.WorkspaceFileOperationCommand{
		Action: app.WorkspaceFileMove, SourcePath: "project", TargetPath: "archive/project", ExpectedCurrentRevision: 1, WriteEnabled: true,
	}
	_, err := store.ApplyWorkspaceFileOperation(ctx, owner.ID, value.ID, operation,
		workspaceFileOperationRecord(owner.ID, value.ID, "credential-subtree"))
	if domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("directory move containing a credential path = %v, want forbidden", err)
	}
	current, getErr := store.GetWorkspace(ctx, value.ID)
	page, pageErr := store.GetWorkspaceManifestPage(ctx, value.ID, 1, "", "", 10)
	if getErr != nil || pageErr != nil || current.Revision != 1 || len(page.Files) != 1 || page.Files[0].Path != file.Path {
		t.Fatalf("Workspace changed after credential subtree rejection: current=%#v manifest=%#v errors=%v/%v", current, page, getErr, pageErr)
	}
}

func TestPostgresWorkspaceFileOperationDecodesSymlinkIdentityAndRejectsSpecialMode(t *testing.T) {
	file := syncTestFile("project/link", []byte("../README.md"))
	file.Kind = workspace.FileKindSymlink
	file.SymlinkTarget = "../README.md"
	file.Mode = 0
	file.Chunks = []workspace.Chunk{}
	encodedChunks, err := json.Marshal(file.Chunks)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := workspaceFileFromRow(file.Path, file.SizeBytes, file.SHA256, 0, workspace.FileKindSymlink,
		pgtype.Text{String: file.SymlinkTarget, Valid: true}, pgtype.Int8{}, pgtype.Text{}, encodedChunks, []byte(`[]`), nil, false)
	if err != nil || decoded.Kind != workspace.FileKindSymlink || decoded.SymlinkTarget != file.SymlinkTarget || !reflect.DeepEqual(decoded.Chunks, file.Chunks) {
		t.Fatalf("decode Workspace symlink = %#v, err=%v", decoded, err)
	}
	if _, err := workspaceFileFromRow(file.Path, file.SizeBytes, file.SHA256, int32(0o777), workspace.FileKindSymlink,
		pgtype.Text{String: file.SymlinkTarget, Valid: true}, pgtype.Int8{}, pgtype.Text{}, encodedChunks, []byte(`[]`), nil, false); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("decode symlink with permission bits = %v, want fail-closed internal error", err)
	}
}

func workspaceFileOperationRecord(ownerID, workspaceID, key string) app.IdempotencyRecord {
	digest := sha256.Sum256([]byte("workspace file operation:" + workspaceID + ":" + key))
	return app.IdempotencyRecord{
		OwnerID: ownerID, Operation: "workspace.file_operation", Key: key, RequestHash: digest[:], ResourceID: workspaceID,
	}
}
