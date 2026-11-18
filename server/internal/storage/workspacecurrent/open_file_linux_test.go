//go:build linux

package workspacecurrent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"morde/server/internal/domain/workspace"
)

func TestWorkspacePathRequiresAnExistingSafeDirectory(t *testing.T) {
	store := openSymlinkTestStore(t)
	if _, err := store.WorkspacePath("u1234567", "w1234567"); err == nil {
		t.Fatal("WorkspacePath() accepted a missing Workspace directory")
	}
	if err := store.Materialize(context.Background(), "u1234567", "w1234567", 0, nil, nil,
		[]workspace.Directory{{Path: ".", MetadataKnown: true}}, noWorkspaceBlocks{}); err != nil {
		t.Fatal(err)
	}
	path, err := store.WorkspacePath("u1234567", "w1234567")
	want := filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567")
	if err != nil || path != want {
		t.Fatalf("WorkspacePath() = %q, %v; want %q", path, err, want)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		t.Fatalf("WorkspacePath() result is not an existing directory: %v, %v", info, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WorkspacePath("u1234567", "w1234567"); err == nil {
		t.Fatal("WorkspacePath() followed a symlink")
	}
}
