//go:build linux

package workspacefs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyTreeCopyNativeACLWithoutStoredACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("workspace"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	if err := applyTreeCopyNativeACL(file, path, "file", false, nil); err != nil {
		t.Fatalf("clear absent or unsupported POSIX ACL: %v", err)
	}
}
