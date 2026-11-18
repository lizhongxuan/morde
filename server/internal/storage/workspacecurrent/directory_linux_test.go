//go:build linux

package workspacecurrent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadDirectoryListsSortedEntriesAndDoesNotFollowSymlinks(t *testing.T) {
	store := openSymlinkTestStore(t)
	workspaceRoot := filepath.Join(store.rootPath, ownerHashForTest("u1234567"), "w1234567")
	if err := os.MkdirAll(filepath.Join(workspaceRoot, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "z.txt"), []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "sub", "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspaceRoot, "link")); err != nil {
		t.Fatal(err)
	}

	entries, err := store.ReadDirectory("u1234567", "w1234567", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Name != "link" || entries[0].Kind != "symlink" ||
		entries[1].Name != "sub" || entries[1].Kind != "directory" ||
		entries[2].Name != "z.txt" || entries[2].Kind != "file" || entries[2].SizeBytes != 1 {
		t.Fatalf("ReadDirectory() = %#v", entries)
	}
	if _, err := store.ReadDirectory("u1234567", "w1234567", "link"); err == nil {
		t.Fatal("ReadDirectory() followed a symlink")
	}
	if _, err := store.ReadDirectory("u1234567", "w1234567", "../secret.txt"); err == nil {
		t.Fatal("ReadDirectory() accepted a path outside the Workspace")
	}
}
