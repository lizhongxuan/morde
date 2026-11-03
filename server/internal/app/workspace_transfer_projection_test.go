package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func TestPrepareWorkspaceOwnershipTransferProjectionVerifiesBothOwners(t *testing.T) {
	store := newSyncServiceStore()
	file := transferProjectionTestFile()
	store.workspace.Revision, store.workspace.UsedBytes = 2, file.SizeBytes
	store.snapshots[2] = []workspace.File{file}
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	revisions := map[string]int64{"u1234567": 2}
	var materializedOwner string
	if err := service.SetWorkspaceCurrentRevisionReader(func(ownerID, workspaceID string) (int64, bool, error) {
		if workspaceID != store.workspace.ID {
			return 0, false, errors.New("unexpected Workspace")
		}
		revision, exists := revisions[ownerID]
		return revision, exists, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.SetWorkspaceCurrentManifestVerifier(func(_ context.Context, ownerID, workspaceID string, files []workspace.File, directories []string, _ []workspace.Directory) (bool, error) {
		return workspaceID == store.workspace.ID && (ownerID == "u1234567" || ownerID == "u7654321") &&
			reflect.DeepEqual(files, []workspace.File{file}) && len(directories) == 0, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.SetWorkspaceCurrentMaterializer(func(_ context.Context, ownerID, workspaceID string, revision int64, files []workspace.File, directories []string, _ []workspace.Directory) error {
		if workspaceID != store.workspace.ID || revision != 2 || !reflect.DeepEqual(files, []workspace.File{file}) || len(directories) != 0 {
			return errors.New("recipient materializer received the wrong committed projection")
		}
		materializedOwner = ownerID
		revisions[ownerID] = revision
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.PrepareWorkspaceOwnershipTransferProjection(context.Background(), validWorkspaceTransfer()); err != nil {
		t.Fatalf("prepare recipient projection: %v", err)
	}
	if materializedOwner != "u7654321" || revisions[materializedOwner] != 2 {
		t.Fatalf("recipient projection = owner %q revision %d", materializedOwner, revisions[materializedOwner])
	}
}

func TestPrepareWorkspaceOwnershipTransferProjectionPreservesDirectoryMetadata(t *testing.T) {
	store := newSyncServiceStore()
	file := transferProjectionTestFile()
	file.Path = "src/README.md"
	mode, modified := uint32(0o750), int64(1_700_000_000_000_000_000)
	directory := workspace.Directory{Path: "src", Mode: &mode, ModTimeNS: &modified}
	store.workspace.Revision, store.workspace.UsedBytes = 2, file.SizeBytes
	store.snapshots[2] = []workspace.File{file}
	store.directorySnapshots[2] = []workspace.Directory{directory}
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	revisions := map[string]int64{"u1234567": 2}
	if err := service.SetWorkspaceCurrentRevisionReader(func(ownerID, _ string) (int64, bool, error) {
		revision, exists := revisions[ownerID]
		return revision, exists, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.SetWorkspaceCurrentManifestVerifier(func(_ context.Context, ownerID, workspaceID string, files []workspace.File, _ []string, directories []workspace.Directory) (bool, error) {
		return workspaceID == store.workspace.ID && (ownerID == "u1234567" || ownerID == "u7654321") &&
			reflect.DeepEqual(files, []workspace.File{file}) && reflect.DeepEqual(directories, []workspace.Directory{directory}), nil
	}); err != nil {
		t.Fatal(err)
	}
	var materialized []workspace.Directory
	if err := service.SetWorkspaceCurrentMaterializer(func(_ context.Context, ownerID, workspaceID string, revision int64,
		files []workspace.File, _ []string, directories []workspace.Directory) error {
		if ownerID != "u7654321" || workspaceID != store.workspace.ID || revision != 2 || !reflect.DeepEqual(files, []workspace.File{file}) {
			return errors.New("recipient materializer received the wrong committed projection")
		}
		materialized = append([]workspace.Directory(nil), directories...)
		revisions[ownerID] = revision
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.PrepareWorkspaceOwnershipTransferProjection(context.Background(), validWorkspaceTransfer()); err != nil {
		t.Fatalf("prepare recipient projection: %v", err)
	}
	if !reflect.DeepEqual(materialized, []workspace.Directory{directory}) {
		t.Fatalf("recipient directory metadata = %#v, want %#v", materialized, []workspace.Directory{directory})
	}
}

func TestPrepareWorkspaceOwnershipTransferProjectionRejectsUnverifiedSource(t *testing.T) {
	store := newSyncServiceStore()
	file := transferProjectionTestFile()
	store.workspace.Revision, store.workspace.UsedBytes = 2, file.SizeBytes
	store.snapshots[2] = []workspace.File{file}
	service, err := NewWorkspaceSyncService(store, newSyncBlockMemory(), workspace.MaxSyncManifestBytes, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	materialized := false
	if err := service.SetWorkspaceCurrentRevisionReader(func(string, string) (int64, bool, error) { return 1, true, nil }); err != nil {
		t.Fatal(err)
	}
	if err := service.SetWorkspaceCurrentManifestVerifier(func(context.Context, string, string, []workspace.File, []string, []workspace.Directory) (bool, error) {
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.SetWorkspaceCurrentMaterializer(func(context.Context, string, string, int64, []workspace.File, []string, []workspace.Directory) error {
		materialized = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.PrepareWorkspaceOwnershipTransferProjection(context.Background(), validWorkspaceTransfer()); domain.CodeOf(err) != domain.CodeWorkspaceConflict || materialized {
		t.Fatalf("unverified source projection error = %v, materialized=%t", err, materialized)
	}
}

func transferProjectionTestFile() workspace.File {
	content := []byte("hello")
	digest := sha256.Sum256(content)
	return workspace.File{
		Path: "README.md", SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), Mode: 0644,
		Chunks: []workspace.Chunk{{SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(content))}},
	}
}
