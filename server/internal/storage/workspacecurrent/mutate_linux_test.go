//go:build linux

package workspacecurrent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"morde/server/internal/domain/workspace"
)

func TestWorkspaceCurrentPathMutationsAreScopedAndConditional(t *testing.T) {
	store := openSymlinkTestStore(t)
	ctx := context.Background()
	if err := store.Materialize(ctx, "u1234567", "w1234567", 0, nil, nil,
		[]workspace.Directory{{Path: ".", MetadataKnown: true}}, noWorkspaceBlocks{}); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateDirectory(ctx, "u1234567", "w1234567", "project/site/empty", 0750); err != nil {
		t.Fatalf("create nested empty directory: %v", err)
	}
	if err := store.CreateDirectory(ctx, "u1234567", "w1234567", "project/site/empty", 0750); err != nil {
		t.Fatalf("repeat directory creation: %v", err)
	}
	if err := store.WriteRegularFile(ctx, "u1234567", "w1234567", "project/site/report.txt", []byte("report"), 0644, ""); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("report"))
	expectedSHA := hex.EncodeToString(digest[:])
	staleDigest := sha256.Sum256([]byte("stale"))
	if err := store.RemoveRegularFile(ctx, "u1234567", "w1234567", "project/site/report.txt", hex.EncodeToString(staleDigest[:])); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("remove with stale digest = %v, want ErrWriteConflict", err)
	}
	if _, err := os.Stat(filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567", "project/site/report.txt")); err != nil {
		t.Fatalf("stale delete removed target: %v", err)
	}
	if err := store.RemoveEmptyDirectory(ctx, "u1234567", "w1234567", "project/site"); err == nil {
		t.Fatal("removed non-empty directory")
	}
	if err := store.RemoveRegularFile(ctx, "u1234567", "w1234567", "project/site/report.txt", expectedSHA); err != nil {
		t.Fatalf("remove file with matching digest: %v", err)
	}
	if err := store.RemoveEmptyDirectory(ctx, "u1234567", "w1234567", "project/site/empty"); err != nil {
		t.Fatalf("remove empty directory: %v", err)
	}

	if err := store.WriteSymbolicLink(ctx, "u1234567", "w1234567", "project/site/link", "report.txt", ""); err != nil {
		t.Fatalf("create symbolic link: %v", err)
	}
	if err := store.WriteSymbolicLink(ctx, "u1234567", "w1234567", "project/site/link", "new.txt", "report.txt"); err != nil {
		t.Fatalf("replace symbolic link with expected target: %v", err)
	}
	if target, err := store.ReadSymbolicLink("u1234567", "w1234567", "project/site/link"); err != nil || target != "new.txt" {
		t.Fatalf("symbolic link target = %q, %v; want new.txt", target, err)
	}
	if err := store.RemoveSymbolicLink(ctx, "u1234567", "w1234567", "project/site/link", "report.txt"); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("remove symbolic link with stale target = %v, want ErrWriteConflict", err)
	}
	if err := store.RemoveSymbolicLink(ctx, "u1234567", "w1234567", "project/site/link", "new.txt"); err != nil {
		t.Fatalf("remove symbolic link with matching target: %v", err)
	}
	if err := store.WriteSymbolicLink(ctx, "u1234567", "w1234567", "project/site/escape", "../../../outside", ""); err == nil {
		t.Fatal("created symbolic link escaping Workspace root")
	}
}

func TestWorkspaceCurrentMoveRegularFileIsNoReplaceAndIdempotent(t *testing.T) {
	store := newMutationTestWorkspace(t)
	ctx := context.Background()
	content := []byte("move me")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	if err := store.WriteRegularFile(ctx, "u1234567", "w1234567", "source.txt", content, 0640, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateDirectory(ctx, "u1234567", "w1234567", "nested", 0750); err != nil {
		t.Fatal(err)
	}
	if err := store.MoveRegularFile(ctx, "u1234567", "w1234567", "source.txt", "nested/target.txt", sha, 0640); err != nil {
		t.Fatalf("move regular file: %v", err)
	}
	if err := store.MoveRegularFile(ctx, "u1234567", "w1234567", "source.txt", "nested/target.txt", sha, 0640); err != nil {
		t.Fatalf("retry completed move: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567", "source.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source after move = %v; want absent", err)
	}
	got, err := os.ReadFile(filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567", "nested", "target.txt"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("target after move = %q, %v; want %q", got, err, content)
	}
	targetPath := filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567", "nested", "target.txt")
	if err := os.Chmod(targetPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.MoveRegularFile(ctx, "u1234567", "w1234567", "source.txt", "nested/target.txt", sha, 0640); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("retry after target mode changed = %v; want ErrWriteConflict", err)
	}
	if err := os.Chmod(targetPath, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.MoveRegularFile(ctx, "u1234567", "w1234567", "source.txt", "nested/target.txt", sha, 0640); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("retry after target changed = %v; want ErrWriteConflict", err)
	}

	if err := store.WriteRegularFile(ctx, "u1234567", "w1234567", "other.txt", content, 0600, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteRegularFile(ctx, "u1234567", "w1234567", "existing.txt", content, 0600, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.MoveRegularFile(ctx, "u1234567", "w1234567", "other.txt", "existing.txt", sha, 0600); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("move over existing target = %v; want ErrWriteConflict", err)
	}
}

func TestWorkspaceCurrentMoveRegularFileRejectsStaleDigestAndEscapes(t *testing.T) {
	store := newMutationTestWorkspace(t)
	ctx := context.Background()
	content := []byte("current")
	if err := store.WriteRegularFile(ctx, "u1234567", "w1234567", "source.txt", content, 0600, ""); err != nil {
		t.Fatal(err)
	}
	currentDigest := sha256.Sum256(content)
	currentSHA := hex.EncodeToString(currentDigest[:])
	if err := store.MoveRegularFile(ctx, "u1234567", "w1234567", "source.txt", "target.txt", currentSHA, 0644); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("move with stale source mode = %v; want ErrWriteConflict", err)
	}
	stale := sha256.Sum256([]byte("stale"))
	if err := store.MoveRegularFile(ctx, "u1234567", "w1234567", "source.txt", "target.txt", hex.EncodeToString(stale[:]), 0600); !errors.Is(err, ErrWriteConflict) {
		t.Fatalf("move with stale digest = %v; want ErrWriteConflict", err)
	}
	for _, test := range []struct{ source, target string }{
		{source: "../source.txt", target: "target.txt"},
		{source: "source.txt", target: "../../target.txt"},
	} {
		if err := store.MoveRegularFile(ctx, "u1234567", "w1234567", test.source, test.target, currentSHA, 0600); err == nil {
			t.Errorf("move %q to %q escaped Workspace", test.source, test.target)
		}
	}
	got, err := os.ReadFile(filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567", "source.txt"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("source after rejected moves = %q, %v; want unchanged", got, err)
	}
}

func TestWorkspaceCurrentMoveRegularFileRejectsSymlinks(t *testing.T) {
	store := newMutationTestWorkspace(t)
	ctx := context.Background()
	content := []byte("keep")
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	if err := store.WriteRegularFile(ctx, "u1234567", "w1234567", "source.txt", content, 0600, ""); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "sentinel.txt")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	workspaceDir := filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567")
	if err := os.Symlink(outsideDir, filepath.Join(workspaceDir, "link-parent")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "source.txt"), content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.MoveRegularFile(ctx, "u1234567", "w1234567", "link-parent/source.txt", "other.txt", sha, 0600); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("move through symlink source parent = %v; want ErrUnsafePath", err)
	}
	if err := store.MoveRegularFile(ctx, "u1234567", "w1234567", "source.txt", "link-parent/new.txt", sha, 0600); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("move through symlink parent = %v; want ErrUnsafePath", err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(workspaceDir, "link-target")); err != nil {
		t.Fatal(err)
	}
	if err := store.MoveRegularFile(ctx, "u1234567", "w1234567", "source.txt", "link-target", sha, 0600); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("move over symlink target = %v; want ErrUnsafePath", err)
	}
	if got, err := os.ReadFile(outsideFile); err != nil || string(got) != "outside" {
		t.Fatalf("outside target after symlink attempts = %q, %v; want unchanged", got, err)
	}
}

func newMutationTestWorkspace(t *testing.T) *Store {
	t.Helper()
	store := openSymlinkTestStore(t)
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 0, nil, nil, workspaceRootMetadata(), noWorkspaceBlocks{}); err != nil {
		t.Fatal(err)
	}
	return store
}
