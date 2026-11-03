package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type workspaceRestoreTestStore struct {
	*syncServiceStore
	restoreCalls int
	record       IdempotencyRecord
	records      []IdempotencyRecord
	paths        []string
	replay       bool
}

type workspaceDiffDirectoryTestStore struct {
	*syncServiceStore
	directories map[int64][]string
}

func (s *workspaceDiffDirectoryTestStore) GetWorkspaceManifestPage(ctx context.Context, workspaceID string, revision int64, prefix, after string, limit int32) (workspace.ManifestPage, error) {
	page, err := s.syncServiceStore.GetWorkspaceManifestPage(ctx, workspaceID, revision, prefix, after, limit)
	if err != nil {
		return workspace.ManifestPage{}, err
	}
	page.EmptyDirectories = []string{}
	for _, directory := range s.directories[revision] {
		if directory <= after || (prefix != "" && directory != prefix && !strings.HasPrefix(directory, prefix+"/")) {
			continue
		}
		page.EmptyDirectories = append(page.EmptyDirectories, directory)
	}
	return page, nil
}

func (s *workspaceRestoreTestStore) RestoreWorkspaceRevision(_ context.Context, ownerID, workspaceID string, targetRevision, expectedRevision int64, paths []string, record IdempotencyRecord) (WorkspaceRestoreResult, error) {
	s.restoreCalls++
	s.record = record
	s.records = append(s.records, record)
	s.paths = append([]string(nil), paths...)
	if s.replay {
		return WorkspaceRestoreResult{WorkspaceID: workspaceID, TargetRevision: targetRevision, ExpectedCurrentRevision: expectedRevision, Revision: 3, Replayed: true}, nil
	}
	s.replay = true
	s.workspace.Revision = 3
	s.snapshots[3] = append([]workspace.File(nil), s.snapshots[2]...)
	return WorkspaceRestoreResult{WorkspaceID: workspaceID, TargetRevision: targetRevision, ExpectedCurrentRevision: expectedRevision, Revision: 3}, nil
}

func (*workspaceRestoreTestStore) ListWorkspaceCurrentMaterializations(context.Context, string, int32) ([]WorkspaceCurrentMaterialization, error) {
	return []WorkspaceCurrentMaterialization{}, nil
}

func (*workspaceRestoreTestStore) MarkWorkspaceCurrentMaterialized(context.Context, string, int64) error {
	return nil
}

func TestWorkspaceDiffReturnsChangesAndOnlySmallUTF8Content(t *testing.T) {
	store := newSyncServiceStore()
	store.workspace.Revision = 2
	blocks := newSyncBlockMemory()
	addFile := func(path string, data []byte) workspace.File {
		file := syncServiceFile(path, data)
		if len(data) > 0 {
			blocks.data[file.Chunks[0].SHA256] = append([]byte(nil), data...)
		}
		return file
	}
	large := bytes.Repeat([]byte("x"), maxWorkspaceDiffTextFileBytes+1)
	largeNew := append([]byte(nil), large...)
	largeNew[len(largeNew)-1] = 'y'
	store.snapshots[1] = []workspace.File{
		addFile("change.txt", []byte("old text\n")),
		addFile("deleted.txt", []byte("gone\n")),
		addFile("renamed.txt", []byte("same\n")),
		addFile("large.txt", largeNew),
		addFile("binary.bin", []byte{0, 1, 2}),
	}
	store.snapshots[2] = []workspace.File{
		addFile("change.txt", []byte("new text\n")),
		addFile("added.txt", []byte("new file\n")),
		addFile("Renamed.txt", []byte("same\n")),
		addFile("large.txt", large),
		addFile("binary.bin", []byte{0, 4, 2}),
	}
	service, err := NewWorkspaceSyncService(store, blocks, workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	result, err := service.GetDiff(context.Background(), principal, store.workspace.ID, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Changes) != 6 {
		t.Fatalf("diff changes = %#v", result.Changes)
	}
	byPath := make(map[string]WorkspaceDiffChange, len(result.Changes))
	for _, change := range result.Changes {
		byPath[strings.ToLower(change.Path)] = change
	}
	if change := byPath["change.txt"]; change.Action != "changed" || change.DisplayStatus != "text" || change.BeforeText == nil || *change.BeforeText != "old text\n" || change.AfterText == nil || *change.AfterText != "new text\n" {
		t.Fatalf("modified text diff = %#v", change)
	}
	if change := byPath["deleted.txt"]; change.Action != "removed" || change.DisplayStatus != "text" || change.BeforeText == nil || change.AfterText != nil {
		t.Fatalf("deleted text diff = %#v", change)
	}
	if change := byPath["added.txt"]; change.Action != "added" || change.DisplayStatus != "text" || change.BeforeText != nil || change.AfterText == nil {
		t.Fatalf("added text diff = %#v", change)
	}
	if change := byPath["renamed.txt"]; change.Action != "renamed" || change.DisplayStatus != "metadata_only" {
		t.Fatalf("case-only rename diff = %#v", change)
	}
	if change := byPath["large.txt"]; change.DisplayStatus != "too_large" || change.BeforeText != nil || change.AfterText != nil {
		t.Fatalf("large file diff = %#v", change)
	}
	if change := byPath["binary.bin"]; change.DisplayStatus != "binary" || change.BeforeText != nil || change.AfterText != nil {
		t.Fatalf("binary diff = %#v", change)
	}
}

func TestWorkspaceDiffReportsSymlinkTargetChangesWithoutReadingReferents(t *testing.T) {
	linkFile := func(target string) workspace.File {
		digest := sha256.Sum256([]byte(target))
		return workspace.File{Kind: "symlink", Path: "docs/current", SymlinkTarget: target, SizeBytes: int64(len(target)),
			SHA256: hex.EncodeToString(digest[:]), Mode: 0, Chunks: []workspace.Chunk{}}
	}
	store := newSyncServiceStore()
	store.workspace.Revision = 2
	store.snapshots[1] = []workspace.File{linkFile("guide-v1.md")}
	store.snapshots[2] = []workspace.File{linkFile("guide-v2.md")}
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	result, err := service.GetDiff(context.Background(), principal, store.workspace.ID, 1, 2)
	if err != nil || len(result.Changes) != 1 {
		t.Fatalf("symlink diff = %#v, err=%v", result, err)
	}
	change := result.Changes[0]
	if change.Action != "changed" || change.DisplayStatus != "not_applicable" || change.Before == nil || change.After == nil ||
		change.Before.Kind != "symlink" || change.After.Kind != "symlink" || change.Before.SymlinkTarget != "guide-v1.md" ||
		change.After.SymlinkTarget != "guide-v2.md" || change.BeforeText != nil || change.AfterText != nil {
		t.Fatalf("symlink target change exposed referent content or lost link metadata: %#v", change)
	}
}

func TestWorkspaceDiffRejectsNonOwnerCredentialsAndUnavailableRevisions(t *testing.T) {
	store := newSyncServiceStore()
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.GetDiff(context.Background(), identity.Principal{UserID: store.workspace.OwnerID, Kind: identity.KindAPIKey}, store.workspace.ID, 0, 0); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("API key diff error = %v, want forbidden", err)
	}
	principal := identity.Principal{UserID: store.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	if _, err := service.GetDiff(context.Background(), principal, store.workspace.ID, 1, 0); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("reverse diff error = %v, want conflict", err)
	}
	if _, err := service.GetDiff(context.Background(), principal, store.workspace.ID, 0, 1); domain.CodeOf(err) != domain.CodeWorkspaceConflict {
		t.Fatalf("future diff error = %v, want conflict", err)
	}
}

func TestWorkspaceDiffIncludesEmptyDirectoriesAndRejectsOversizedResults(t *testing.T) {
	base := newSyncServiceStore()
	base.workspace.Revision = 1
	base.snapshots[1] = []workspace.File{}
	store := &workspaceDiffDirectoryTestStore{syncServiceStore: base, directories: map[int64][]string{0: {"before/empty"}, 1: {"after/empty"}}}
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: base.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	result, err := service.GetDiff(context.Background(), principal, base.workspace.ID, 0, 1)
	if err != nil || len(result.Changes) != 2 {
		t.Fatalf("empty-directory diff = %#v, err=%v", result, err)
	}
	if result.Changes[0].Path != "after/empty" || result.Changes[0].Action != "added" || result.Changes[1].Path != "before/empty" || result.Changes[1].Action != "removed" {
		t.Fatalf("empty-directory diff order = %#v", result.Changes)
	}
	for _, change := range result.Changes {
		if change.DisplayStatus != "not_applicable" {
			t.Fatalf("empty-directory display status = %q", change.DisplayStatus)
		}
	}

	base.workspace.Revision = 1
	base.snapshots[1] = make([]workspace.File, maxWorkspaceDiffChanges+1)
	for i := range base.snapshots[1] {
		base.snapshots[1][i] = syncServiceFile(fmt.Sprintf("file-%03d.txt", i), nil)
	}
	if _, err := service.GetDiff(context.Background(), principal, base.workspace.ID, 0, 1); domain.CodeOf(err) != domain.CodeResultTooLarge {
		t.Fatalf("oversized diff error = %v, want result too large", err)
	}
}

func TestWorkspaceDiffReportsDirectoryMetadataChanges(t *testing.T) {
	beforeMode, afterMode := uint32(0o750), uint32(0o700)
	beforeTime, afterTime := int64(100), int64(200)
	changes, err := diffWorkspaceSnapshots(nil, nil, []workspace.Directory{{Path: "folder", Mode: &beforeMode, ModTimeNS: &beforeTime}},
		nil, nil, []workspace.Directory{{Path: "folder", Mode: &afterMode, ModTimeNS: &afterTime}})
	if err != nil || len(changes) != 1 {
		t.Fatalf("directory metadata diff = %#v, %v", changes, err)
	}
	change := changes[0]
	if change.Path != "folder" || change.Action != "changed" || change.Before == nil || change.After == nil ||
		change.Before.Mode == nil || *change.Before.Mode != beforeMode || change.After.Mode == nil || *change.After.Mode != afterMode ||
		change.Before.ModTimeNS == nil || *change.Before.ModTimeNS != beforeTime || change.After.ModTimeNS == nil || *change.After.ModTimeNS != afterTime {
		t.Fatalf("directory metadata diff lost before/after values: %#v", change)
	}
}

func TestWorkspaceRestoreRequiresSelectedPathsAndReplaysIdempotently(t *testing.T) {
	base := newSyncServiceStore()
	base.workspace.Revision = 2
	base.snapshots[1] = []workspace.File{}
	base.snapshots[2] = []workspace.File{}
	store := &workspaceRestoreTestStore{syncServiceStore: base}
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	materialized := 0
	if err := service.SetWorkspaceCurrentMaterializer(func(_ context.Context, ownerID, workspaceID string, revision int64, _ []workspace.File, _ []string, _ []workspace.Directory) error {
		if ownerID != base.workspace.OwnerID || workspaceID != base.workspace.ID || revision != 3 {
			t.Errorf("materialized %s/%s at revision %d", ownerID, workspaceID, revision)
		}
		materialized++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: base.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	command := WorkspaceRestoreCommand{TargetRevision: 1, ExpectedCurrentRevision: 2, Paths: []string{"z/file.txt", "a"}}
	first, err := service.Restore(context.Background(), principal, base.workspace.ID, command, "restore-key-01")
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 3 || first.Replayed || store.restoreCalls != 1 || strings.Join(store.paths, ",") != "a,z/file.txt" || materialized != 1 {
		t.Fatalf("first restore = %#v, paths=%v, calls=%d, materialized=%d", first, store.paths, store.restoreCalls, materialized)
	}
	command.Paths = []string{"a", "z/file.txt"}
	second, err := service.Restore(context.Background(), principal, base.workspace.ID, command, "restore-key-01")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.Revision != first.Revision || len(store.records) != 2 || !bytes.Equal(store.records[0].RequestHash, store.records[1].RequestHash) || materialized != 2 {
		t.Fatalf("replayed restore = %#v, materialized=%d", second, materialized)
	}
	if _, err := service.Restore(context.Background(), principal, base.workspace.ID, WorkspaceRestoreCommand{TargetRevision: 1, ExpectedCurrentRevision: 2, Paths: []string{"a", "a/b"}}, "restore-key-02"); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("overlapping restore paths error = %v, want invalid argument", err)
	}
	if _, err := service.Restore(context.Background(), identity.Principal{UserID: principal.UserID, Kind: identity.KindAPIKey}, base.workspace.ID, command, "restore-key-03"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("API key restore error = %v, want forbidden", err)
	}
}

func TestWorkspaceRestoreMaterializationFailureReturnsRetryableConflict(t *testing.T) {
	base := newSyncServiceStore()
	base.workspace.Revision = 2
	base.snapshots[1], base.snapshots[2] = []workspace.File{}, []workspace.File{}
	store := &workspaceRestoreTestStore{syncServiceStore: base}
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	materializeErr := errors.New("local current view is unavailable")
	if err := service.SetWorkspaceCurrentMaterializer(func(context.Context, string, string, int64, []workspace.File, []string, []workspace.Directory) error {
		return materializeErr
	}); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: base.workspace.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	_, err = service.Restore(context.Background(), principal, base.workspace.ID,
		WorkspaceRestoreCommand{TargetRevision: 1, ExpectedCurrentRevision: 2, Paths: []string{"file.txt"}}, "restore-key-04")
	if domain.CodeOf(err) != domain.CodeWorkspaceBusy || store.restoreCalls != 1 {
		t.Fatalf("restore error = %v, calls=%d, want retryable busy after durable commit", err, store.restoreCalls)
	}
}
