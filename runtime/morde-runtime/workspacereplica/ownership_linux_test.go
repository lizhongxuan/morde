//go:build linux

package workspacereplica

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestOwnershipUpdatesDoNotFollowWorkspaceSymlinks(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root for ownership boundary verification")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "file.txt"), []byte("workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(outside, 2000, 2000); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "link")); err != nil {
		t.Fatal(err)
	}
	if err := chownWorkspaceTree(workspace, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	outsideInfo, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if outsideInfo.Sys().(*syscall.Stat_t).Uid != 2000 {
		t.Fatalf("ownership update followed workspace symlink: outside uid = %d", outsideInfo.Sys().(*syscall.Stat_t).Uid)
	}
	linkInfo, err := os.Lstat(filepath.Join(workspace, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Sys().(*syscall.Stat_t).Uid != 1000 {
		t.Fatalf("workspace symlink uid = %d, want 1000", linkInfo.Sys().(*syscall.Stat_t).Uid)
	}
	if err := chownAppliedPaths(workspace, []string{"."}, 1200, 1201); err != nil {
		t.Fatalf("chownAppliedPaths rejected workspace root: %v", err)
	}
	rootInfo, err := os.Stat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if stat := rootInfo.Sys().(*syscall.Stat_t); stat.Uid != 1200 || stat.Gid != 1201 {
		t.Fatalf("workspace root owner = %d:%d, want 1200:1201", stat.Uid, stat.Gid)
	}
	if err := chownAppliedPaths(workspace, []string{"../outside"}, 1000, 1000); err == nil {
		t.Fatal("chownAppliedPaths accepted traversal")
	}
}
