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
