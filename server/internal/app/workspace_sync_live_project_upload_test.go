package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"morde/server/internal/domain/workspace"
)

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
