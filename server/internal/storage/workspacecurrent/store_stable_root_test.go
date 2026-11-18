//go:build darwin || linux

package workspacecurrent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"morde/server/internal/domain/workspace"
)

type stableRootTestBlocks map[string][]byte

func (blocks stableRootTestBlocks) Open(_ string, digest string) (io.ReadCloser, error) {
	value, ok := blocks[digest]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(value)), nil
}

func TestMaterializePreservesWorkspaceRootInode(t *testing.T) {
	tempRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(tempRoot, "current"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	first := []byte("first revision")
	firstDigest := sha256.Sum256(first)
	firstSHA := hex.EncodeToString(firstDigest[:])
	firstFile := workspace.File{
		Path: "readme.txt", SizeBytes: int64(len(first)), SHA256: firstSHA, Mode: 0o640, MetadataKnown: true,
		Chunks: []workspace.Chunk{{SHA256: firstSHA, SizeBytes: int64(len(first))}},
	}
	blocks := stableRootTestBlocks{firstSHA: first}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1,
		[]workspace.File{firstFile}, nil, workspaceRootMetadata(), blocks); err != nil {
		t.Fatalf("materialize first revision: %v", err)
	}
	target := filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567")
	rootBeforeUpdate, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	second := []byte("second revision")
	secondDigest := sha256.Sum256(second)
	secondSHA := hex.EncodeToString(secondDigest[:])
	secondFile := workspace.File{
		Path: "readme.txt", SizeBytes: int64(len(second)), SHA256: secondSHA, Mode: 0o600, MetadataKnown: true,
		Chunks: []workspace.Chunk{{SHA256: secondSHA, SizeBytes: int64(len(second))}},
	}
	blocks[secondSHA] = second
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 2,
		[]workspace.File{secondFile}, nil, workspaceRootMetadata(), blocks); err != nil {
		t.Fatalf("materialize second revision: %v", err)
	}
	rootAfterUpdate, err := os.Stat(target)
	if err != nil || !os.SameFile(rootBeforeUpdate, rootAfterUpdate) {
		t.Fatalf("Workspace root inode changed after update: before=%v after=%v err=%v", rootBeforeUpdate, rootAfterUpdate, err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "readme.txt")); err != nil || !bytes.Equal(got, second) {
		t.Fatalf("updated Workspace file = %q, %v", got, err)
	}
}

func TestWorkspaceContentUpdateRollbackRestoresExistingRoot(t *testing.T) {
	rootPath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ownerHash, workspaceID := ownerHashForTest("u1234567"), "w1234567"
	target := filepath.Join(rootPath, ownerHash, workspaceID)
	stage := filepath.Join(rootPath, ".morde-staging", "stage")
	backup := filepath.Join(rootPath, ".morde-staging", "backup")
	for _, path := range []string{target, stage, backup} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(target, "old.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "new.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	rootBefore, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}

	update, err := publishWorkspaceContents(rootPath, ownerHash, workspaceID, stage, backup)
	if err != nil {
		t.Fatalf("publish Workspace contents: %v", err)
	}
	if err := update.rollbackContents(); err != nil {
		t.Fatalf("rollback Workspace contents: %v", err)
	}
	if err := update.close(); err != nil {
		t.Fatalf("close Workspace content update: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "old.txt")); err != nil || string(got) != "old" {
		t.Fatalf("restored old Workspace file = %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(target, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new Workspace file survived rollback: %v", err)
	}
	rootAfter, err := os.Stat(target)
	if err != nil || !os.SameFile(rootBefore, rootAfter) {
		t.Fatalf("Workspace root inode changed during rollback: before=%v after=%v err=%v", rootBefore, rootAfter, err)
	}
}

func TestInterruptedWorkspaceUpdateBlocksReadsAndPreservesRecoveryData(t *testing.T) {
	store := openSymlinkTestStore(t)
	defer func() { _ = store.Close() }()

	content := []byte("committed revision")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	file := workspace.File{
		Path: "old.txt", SizeBytes: int64(len(content)), SHA256: sha, Mode: 0o640, MetadataKnown: true,
		Chunks: []workspace.Chunk{{SHA256: sha, SizeBytes: int64(len(content))}},
	}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 1,
		[]workspace.File{file}, nil, workspaceRootMetadata(), stableRootTestBlocks{sha: content}); err != nil {
		t.Fatalf("materialize committed revision: %v", err)
	}

	ownerHash := ownerHashForTest("u1234567")
	target := filepath.Join(store.rootPath, ownerHash, "w1234567")
	stagingRoot := filepath.Join(store.rootPath, ".morde-staging")
	backup := filepath.Join(stagingRoot, "backup-"+ownerHash+"-w1234567-interrupted")
	stage := filepath.Join(stagingRoot, ownerHash+"-w1234567-interrupted")
	if err := os.Mkdir(backup, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "staged.txt"), []byte("staged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(target, file.Path), filepath.Join(backup, file.Path)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "partial.txt"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.WorkspacePath("u1234567", "w1234567"); !errors.Is(err, ErrWorkspaceUpdateRecoveryRequired) {
		t.Fatalf("WorkspacePath() error = %v", err)
	}
	if _, _, err := store.OpenRegularFile("u1234567", "w1234567", "partial.txt"); !errors.Is(err, ErrWorkspaceUpdateRecoveryRequired) {
		t.Fatalf("OpenRegularFile() error = %v", err)
	}
	if _, err := store.ReadDirectory("u1234567", "w1234567", ""); !errors.Is(err, ErrWorkspaceUpdateRecoveryRequired) {
		t.Fatalf("ReadDirectory() error = %v", err)
	}
	if _, err := store.StatPath("u1234567", "w1234567", "partial.txt"); !errors.Is(err, ErrWorkspaceUpdateRecoveryRequired) {
		t.Fatalf("StatPath() error = %v", err)
	}
	if _, err := store.IsEmptyDirectory("u1234567", "w1234567", "partial.txt"); !errors.Is(err, ErrWorkspaceUpdateRecoveryRequired) {
		t.Fatalf("IsEmptyDirectory() error = %v", err)
	}
	if _, _, err := store.CurrentRevision("u1234567", "w1234567"); !errors.Is(err, ErrWorkspaceUpdateRecoveryRequired) {
		t.Fatalf("CurrentRevision() error = %v", err)
	}
	if _, err := store.MatchesManifest(context.Background(), "u1234567", "w1234567", []workspace.File{file}, nil,
		workspaceRootMetadata()); !errors.Is(err, ErrWorkspaceUpdateRecoveryRequired) {
		t.Fatalf("MatchesManifest() error = %v", err)
	}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 2,
		[]workspace.File{file}, nil, workspaceRootMetadata(), stableRootTestBlocks{sha: content}); !errors.Is(err, ErrWorkspaceUpdateRecoveryRequired) {
		t.Fatalf("Materialize() error = %v", err)
	}
	if err := store.Delete(context.Background(), "u1234567", "w1234567"); !errors.Is(err, ErrWorkspaceUpdateRecoveryRequired) {
		t.Fatalf("Delete() error = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(backup, file.Path)); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("preserved recovery data = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(stage, "staged.txt")); err != nil || string(got) != "staged" {
		t.Fatalf("preserved staged data = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "partial.txt")); err != nil || string(got) != "partial" {
		t.Fatalf("preserved partial live entry = %q, %v", got, err)
	}
}
