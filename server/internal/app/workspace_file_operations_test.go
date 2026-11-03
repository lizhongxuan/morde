package app

import (
	"context"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type workspaceFileOperationTestStore struct {
	*syncServiceStore
	command WorkspaceFileOperationCommand
	record  IdempotencyRecord
	calls   int
	replay  bool
}

func (s *workspaceFileOperationTestStore) ApplyWorkspaceFileOperation(_ context.Context, ownerID, workspaceID string,
	command WorkspaceFileOperationCommand, record IdempotencyRecord) (WorkspaceFileOperationResult, error) {
	s.calls++
	if !s.replay && command.ExpectedCurrentRevision != s.workspace.Revision {
		return WorkspaceFileOperationResult{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed")
	}
	s.command, s.record = command, record
	revision := command.ExpectedCurrentRevision + 1
	if s.replay {
		revision = s.workspace.Revision
	}
	s.workspace.Revision = revision
	s.workspace.UsedBytes = 0
	s.snapshots[revision] = []workspace.File{}
	return WorkspaceFileOperationResult{
		WorkspaceID: workspaceID, Action: command.Action, SourcePath: command.SourcePath, TargetPath: command.TargetPath,
		ExpectedCurrentRevision: command.ExpectedCurrentRevision, Revision: revision, Replayed: s.replay,
	}, nil
}

func (*workspaceFileOperationTestStore) ListWorkspaceCurrentMaterializations(context.Context, string, int32) ([]WorkspaceCurrentMaterialization, error) {
	return []WorkspaceCurrentMaterialization{}, nil
}

func (*workspaceFileOperationTestStore) MarkWorkspaceCurrentMaterialized(context.Context, string, int64) error {
	return nil
}

func TestWorkspaceFileOperationRequiresOwnerSessionAndValidPaths(t *testing.T) {
	base := newSyncServiceStore()
	store := &workspaceFileOperationTestStore{syncServiceStore: base}
	service := newWorkspaceFileOperationTestService(t, store)
	command := WorkspaceFileOperationCommand{Action: WorkspaceFileDelete, SourcePath: "notes.txt", ExpectedCurrentRevision: 0}
	for _, principal := range []identity.Principal{
		{UserID: base.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindAPIKey},
		{UserID: "u7654321", Role: identity.RoleUser, Kind: identity.KindSession},
	} {
		if _, err := service.ApplyFileOperation(context.Background(), principal, base.workspace.ID, command, "file-op-01"); domain.CodeOf(err) != domain.CodeForbidden && domain.CodeOf(err) != domain.CodeNotFound {
			t.Errorf("unauthorized file operation error = %v; want forbidden or not found", err)
		}
	}
	if store.calls != 0 {
		t.Fatalf("unauthorized requests reached Store %d times", store.calls)
	}
	principal := identity.Principal{UserID: base.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	_, readOnlyErr := service.ApplyFileOperation(context.Background(), principal, base.workspace.ID, command, "file-op-read-only")
	if domain.CodeOf(readOnlyErr) != domain.CodeForbidden || store.calls != 0 || !strings.Contains(readOnlyErr.Error(), "write_enabled") {
		t.Fatalf("default read-only operation = %v, calls=%d; want forbidden with write_enabled guidance", readOnlyErr, store.calls)
	}
	for _, invalid := range []WorkspaceFileOperationCommand{
		{Action: WorkspaceFileDelete, SourcePath: "../notes.txt", WriteEnabled: true},
		{Action: WorkspaceFileDelete, SourcePath: "notes//today.txt", WriteEnabled: true},
		{Action: WorkspaceFileDelete, SourcePath: "notes/./today.txt", WriteEnabled: true},
		{Action: WorkspaceFileDelete, SourcePath: "/notes.txt", WriteEnabled: true},
		{Action: WorkspaceFileDelete, SourcePath: `notes\\today.txt`, WriteEnabled: true},
		{Action: WorkspaceFileMove, SourcePath: "notes.txt", TargetPath: "notes.txt/child", WriteEnabled: true},
		{Action: WorkspaceFileRename, SourcePath: "notes.txt", TargetPath: "notes.txt", WriteEnabled: true},
		{Action: WorkspaceFileDelete, SourcePath: "notes.txt", TargetPath: "other.txt", WriteEnabled: true},
	} {
		invalid.ExpectedCurrentRevision = 0
		if _, err := service.ApplyFileOperation(context.Background(), principal, base.workspace.ID, invalid, "file-op-02"); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Errorf("invalid operation %+v error = %v; want invalid argument", invalid, err)
		}
	}
	if store.calls != 0 {
		t.Fatalf("invalid requests reached Store %d times", store.calls)
	}
	if !validWorkspaceFileOperation(WorkspaceFileOperationCommand{
		Action: WorkspaceFileRename, SourcePath: "notes.txt", TargetPath: "Notes.txt",
	}) {
		t.Fatal("case-only rename was rejected")
	}
}

func TestWorkspaceFileOperationRejectsSensitiveCredentialPaths(t *testing.T) {
	base := newSyncServiceStore()
	store := &workspaceFileOperationTestStore{syncServiceStore: base}
	service := newWorkspaceFileOperationTestService(t, store)
	principal := identity.Principal{UserID: base.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	for _, test := range []struct {
		name      string
		operation WorkspaceFileOperationCommand
	}{
		{name: "ssh", operation: WorkspaceFileOperationCommand{Action: WorkspaceFileDelete, SourcePath: ".ssh/id_ed25519", WriteEnabled: true}},
		{name: "gnupg", operation: WorkspaceFileOperationCommand{Action: WorkspaceFileDelete, SourcePath: "keys/.gnupg/private-keys-v1.d/key", WriteEnabled: true}},
		{name: "aws", operation: WorkspaceFileOperationCommand{Action: WorkspaceFileDelete, SourcePath: "workspace/.aws/credentials", WriteEnabled: true}},
		{name: "azure", operation: WorkspaceFileOperationCommand{Action: WorkspaceFileDelete, SourcePath: ".azure/token-cache", WriteEnabled: true}},
		{name: "kube", operation: WorkspaceFileOperationCommand{Action: WorkspaceFileDelete, SourcePath: ".kube/config", WriteEnabled: true}},
		{name: "docker", operation: WorkspaceFileOperationCommand{Action: WorkspaceFileDelete, SourcePath: ".docker/config.json", WriteEnabled: true}},
		{name: "gcloud", operation: WorkspaceFileOperationCommand{Action: WorkspaceFileDelete, SourcePath: ".config/gcloud/application_default_credentials.json", WriteEnabled: true}},
		{name: "gh", operation: WorkspaceFileOperationCommand{Action: WorkspaceFileDelete, SourcePath: "user/.config/gh/hosts.yml", WriteEnabled: true}},
		{name: "netrc", operation: WorkspaceFileOperationCommand{Action: WorkspaceFileDelete, SourcePath: "notes/.netrc", WriteEnabled: true}},
		{name: "git credentials", operation: WorkspaceFileOperationCommand{Action: WorkspaceFileDelete, SourcePath: ".git-credentials", WriteEnabled: true}},
		{name: "env basename", operation: WorkspaceFileOperationCommand{Action: WorkspaceFileDelete, SourcePath: "src/.env.production", WriteEnabled: true}},
		{name: "env target", operation: WorkspaceFileOperationCommand{Action: WorkspaceFileMove, SourcePath: "notes.txt", TargetPath: "backup/.env.local", WriteEnabled: true}},
	} {
		_, err := service.ApplyFileOperation(context.Background(), principal, base.workspace.ID, test.operation, "sensitive-path-key")
		if domain.CodeOf(err) != domain.CodeForbidden {
			t.Errorf("sensitive path %q error = %v, want forbidden", test.name, err)
		}
		for _, rule := range []string{".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".config/gcloud", ".config/gh", ".netrc", ".git-credentials", ".env*"} {
			if err == nil || !strings.Contains(err.Error(), rule) {
				t.Errorf("sensitive path error %v does not explain policy rule %q", err, rule)
				break
			}
		}
	}
	for _, path := range []string{"src/.ssh-not-secret/key", "docs/template.env", "workspace/.aws-backup/notes.txt"} {
		if WorkspaceFileOperationPathIsSensitive(path) {
			t.Errorf("unlisted path %q unexpectedly matches the credential path policy", path)
		}
	}
	if store.calls != 0 {
		t.Fatalf("sensitive-path requests reached Store %d times", store.calls)
	}
}

func TestWorkspaceFileOperationUsesExpectedRevisionIdempotencyAndMaterialization(t *testing.T) {
	base := newSyncServiceStore()
	file := syncServiceFile("notes.txt", []byte("note"))
	base.workspace.Revision, base.workspace.UsedBytes = 1, file.SizeBytes
	base.snapshots[1] = []workspace.File{file}
	store := &workspaceFileOperationTestStore{syncServiceStore: base}
	service := newWorkspaceFileOperationTestService(t, store)
	principal := identity.Principal{UserID: base.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	command := WorkspaceFileOperationCommand{Action: WorkspaceFileDelete, SourcePath: "notes.txt", ExpectedCurrentRevision: 0, WriteEnabled: true}
	if _, err := service.ApplyFileOperation(context.Background(), principal, base.workspace.ID, command, "delete-note-01"); domain.CodeOf(err) != domain.CodeWorkspaceConflict || store.calls != 1 {
		t.Fatalf("stale revision operation = %v, calls=%d; want Store conflict", err, store.calls)
	}
	command.ExpectedCurrentRevision = 1
	result, err := service.ApplyFileOperation(context.Background(), principal, base.workspace.ID, command, "delete-note-01")
	if err != nil || result.Revision != 2 || result.Replayed || store.calls != 2 || store.record.Operation != "workspace.file_operation" ||
		store.record.ResourceID != base.workspace.ID || len(store.record.RequestHash) != 32 || store.command != command {
		t.Fatalf("delete operation = %#v err=%v Store=%#v", result, err, store)
	}
	store.replay = true
	replayed, err := service.ApplyFileOperation(context.Background(), principal, base.workspace.ID, command, "delete-note-01")
	if err != nil || !replayed.Replayed || replayed.Revision != 2 || store.calls != 3 || replayed.Action != WorkspaceFileDelete {
		t.Fatalf("replayed delete operation = %#v err=%v calls=%d", replayed, err, store.calls)
	}
}

func TestWorkspaceFileOperationMoveValidatesSymlinkAtNewPath(t *testing.T) {
	file := syncServiceFile("a/b/current", []byte("../../target"))
	file.Kind, file.SymlinkTarget, file.Mode, file.Chunks = workspace.FileKindSymlink, "../../target", 0, []workspace.Chunk{}
	if err := ValidateWorkspaceFileOperationMove(file, "a/current"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("move that makes symlink escape = %v; want invalid argument", err)
	}
	if err := ValidateWorkspaceFileOperationMove(file, "x/y/current"); err != nil {
		t.Fatalf("safe symlink move = %v", err)
	}
	regular := syncServiceFile("a/file.txt", []byte("content"))
	if err := ValidateWorkspaceFileOperationMove(regular, "renamed.txt"); err != nil {
		t.Fatalf("regular-file move = %v", err)
	}
}

func newWorkspaceFileOperationTestService(t *testing.T, store *workspaceFileOperationTestStore) *WorkspaceSyncService {
	t.Helper()
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetWorkspaceCurrentMaterializer(func(context.Context, string, string, int64, []workspace.File, []string, []workspace.Directory) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return service
}
