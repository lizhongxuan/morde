package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"morde/server/internal/domain/workspace"
)

type recordingWorkspaceCurrentDirectory struct {
	cloudDSHLiveWorkspaceDirectory
	moves int
}

func (r *recordingWorkspaceCurrentDirectory) MoveRegularFile(ctx context.Context, ownerID, workspaceID, sourcePath, targetPath, expectedSHA256 string, expectedMode uint32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.moves++
	source := filepath.Join(r.root, filepath.FromSlash(sourcePath))
	target := filepath.Join(r.root, filepath.FromSlash(targetPath))
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != expectedSHA256 {
		return errors.New("source file changed")
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if uint32(info.Mode().Perm()) != expectedMode {
		return errors.New("source file mode changed")
	}
	if _, err := os.Lstat(target); err == nil {
		return errors.New("target file already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(source, target)
}

func TestApplyLiveProjectUploadWritesSharedDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "project", "demo"), 0o750); err != nil {
		t.Fatal(err)
	}
	blocks := newSyncBlockMemory()
	content := []byte("package main\n")
	digest := sha256.Sum256(content)
	digestText := hex.EncodeToString(digest[:])
	chunk := workspace.Chunk{SHA256: digestText, SizeBytes: int64(len(content))}
	if _, err := blocks.PutBlock("w1234567", digestText, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	service := &WorkspaceSyncService{blocks: blocks, currentDirectory: cloudDSHLiveWorkspaceDirectory{root: root}, maxOwnerBytes: 1 << 20}
	session := workspace.SyncSession{
		OwnerID: "u1234567", WorkspaceID: "w1234567", FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Direction: workspace.SyncUpload, Scope: []string{"project/demo"},
		UploadFiles: []workspace.File{{Path: "project/demo/main.go", SizeBytes: int64(len(content)), SHA256: digestText,
			Mode: 0o644, Chunks: []workspace.Chunk{chunk}}},
	}
	if conflicts, err := service.applyLiveProjectUpload(context.Background(), WorkspaceSyncDirectCommitPlan{Session: session}); err != nil || len(conflicts) != 0 {
		t.Fatalf("applyLiveProjectUpload() = conflicts %#v, err %v", conflicts, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "project", "demo", "main.go")); err != nil || string(got) != string(content) {
		t.Fatalf("shared file = %q, err %v; want %q", got, err, content)
	}
}

func TestApplyLiveProjectUploadMovesUnchangedRenamedFileInSharedDirectory(t *testing.T) {
	ownerID, workspaceID := "u1234567", "w1234567"
	root := t.TempDir()
	projectDir := filepath.Join(root, "project", "demo")
	if err := os.MkdirAll(projectDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "old.txt"), []byte("unchanged content"), 0o644); err != nil {
		t.Fatal(err)
	}
	content := []byte("unchanged content")
	digest := sha256.Sum256(content)
	digestText := hex.EncodeToString(digest[:])
	blocks := newSyncBlockMemory()
	if _, err := blocks.PutBlock(workspaceID, digestText, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	directory := &recordingWorkspaceCurrentDirectory{cloudDSHLiveWorkspaceDirectory: cloudDSHLiveWorkspaceDirectory{root: root}}
	service := &WorkspaceSyncService{blocks: blocks, currentDirectory: directory, maxOwnerBytes: 1 << 20}
	file := workspace.File{Kind: workspace.FileKindRegular, Path: "project/demo/old.txt", SizeBytes: int64(len(content)),
		SHA256: digestText, Mode: 0o644, MetadataKnown: true, Chunks: []workspace.Chunk{{SHA256: digestText, SizeBytes: int64(len(content))}}}
	desired := file
	desired.Path = "project/demo/new.txt"
	session := workspace.SyncSession{OwnerID: ownerID, WorkspaceID: workspaceID, FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Direction: workspace.SyncUpload, Scope: []string{"project/demo"}, UploadFiles: []workspace.File{desired},
		UploadDeletedPaths: []string{file.Path}}
	plan := WorkspaceSyncDirectCommitPlan{Session: session, PinnedFiles: []workspace.File{file}}
	if conflicts, err := service.applyLiveProjectUpload(context.Background(), plan); err != nil || len(conflicts) != 0 {
		t.Fatalf("rename project upload = conflicts %#v, err %v", conflicts, err)
	}
	if directory.moves != 1 {
		t.Fatalf("file moves = %d; want exactly one atomic move", directory.moves)
	}
	if _, err := os.Stat(filepath.Join(projectDir, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old path after move = %v; want not-exist", err)
	}
	got, err := os.ReadFile(filepath.Join(projectDir, "new.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("moved file content = %q; want %q", got, content)
	}
}

func TestApplyLiveProjectUploadPreservesConcurrentChanges(t *testing.T) {
	root := t.TempDir()
	projectPath := filepath.Join(root, "project", "demo")
	if err := os.MkdirAll(projectPath, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projectPath, "main.go")
	remote := []byte("cloud changed")
	if err := os.WriteFile(path, remote, 0o644); err != nil {
		t.Fatal(err)
	}
	local := []byte("pc changed")
	localDigest := sha256.Sum256(local)
	localDigestText := hex.EncodeToString(localDigest[:])
	blocks := newSyncBlockMemory()
	if _, err := blocks.PutBlock("w1234567", localDigestText, bytes.NewReader(local)); err != nil {
		t.Fatal(err)
	}
	base := []byte("original")
	baseDigest := sha256.Sum256(base)
	service := &WorkspaceSyncService{blocks: blocks, currentDirectory: cloudDSHLiveWorkspaceDirectory{root: root}, maxOwnerBytes: 1 << 20}
	session := workspace.SyncSession{
		OwnerID: "u1234567", WorkspaceID: "w1234567", FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Direction: workspace.SyncUpload, Scope: []string{"project/demo"},
		UploadFiles: []workspace.File{{Path: "project/demo/main.go", SizeBytes: int64(len(local)), SHA256: localDigestText,
			Mode: 0o644, Chunks: []workspace.Chunk{{SHA256: localDigestText, SizeBytes: int64(len(local))}}}},
	}
	plan := WorkspaceSyncDirectCommitPlan{Session: session, PinnedFiles: []workspace.File{{Path: "project/demo/main.go", SizeBytes: int64(len(base)),
		SHA256: hex.EncodeToString(baseDigest[:]), Mode: 0o644}}}
	conflicts, err := service.applyLiveProjectUpload(context.Background(), plan)
	if err != nil || len(conflicts) != 1 || conflicts[0].Path != "project/demo/main.go" {
		t.Fatalf("concurrent upload result = conflicts %#v, err %v", conflicts, err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(remote) {
		t.Fatalf("concurrent file = %q, err %v; cloud change must be preserved", got, err)
	}
}

func TestApplyLiveProjectUploadDeletesNestedTreeBottomUp(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "project", "demo", "sub", "nested")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	content := []byte("remove me")
	filePath := filepath.Join(nested, "file.txt")
	if err := os.WriteFile(filePath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	service := &WorkspaceSyncService{currentDirectory: cloudDSHLiveWorkspaceDirectory{root: root}, maxOwnerBytes: 1 << 20}
	session := workspace.SyncSession{
		OwnerID: "u1234567", WorkspaceID: "w1234567", FolderID: "morde-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Direction: workspace.SyncUpload, Scope: []string{"project/demo"},
		UploadDeletedPaths:       []string{"project/demo/sub/nested/file.txt"},
		UploadDeletedDirectories: []string{"project/demo/sub", "project/demo/sub/nested"},
	}
	plan := WorkspaceSyncDirectCommitPlan{Session: session,
		PinnedFiles:             []workspace.File{{Path: "project/demo/sub/nested/file.txt", SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), Mode: 0o644}},
		PinnedDirectoryMetadata: []workspace.Directory{{Path: "project/demo/sub"}, {Path: "project/demo/sub/nested"}},
	}
	if conflicts, err := service.applyLiveProjectUpload(context.Background(), plan); err != nil || len(conflicts) != 0 {
		t.Fatalf("nested deletion = conflicts %#v, err %v", conflicts, err)
	}
	if _, err := os.Stat(filepath.Join(root, "project", "demo", "sub")); !os.IsNotExist(err) {
		t.Fatalf("deleted tree remains: stat err %v", err)
	}
}
