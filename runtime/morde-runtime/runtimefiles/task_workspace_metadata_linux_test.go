//go:build linux

package runtimefiles

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	mordesync "morde-sync"
)

func TestTaskWorkspacePreservesAndExportsLinuxACL(t *testing.T) {
	root := t.TempDir()
	workspaceID, taskID := "wabc1234", "qabc1234"
	viewID := "v" + strings.Repeat("f", 64)
	sourcePath := filepath.Join(root, workspaceID)
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	nestedPath := filepath.Join(sourcePath, "nested")
	if err := os.Mkdir(nestedPath, 0o750); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(nestedPath, "acl.txt")
	const content = "linux acl metadata"
	if err := os.WriteFile(filePath, []byte(content), 0o740); err != nil {
		t.Fatal(err)
	}
	aclData := taskWorkspaceLinuxTestACL()
	for _, path := range []string{sourcePath, nestedPath, filePath} {
		if err := unix.Setxattr(path, "system.posix_acl_access", aclData, 0); err != nil {
			t.Skipf("filesystem does not support POSIX ACL xattrs: %v", err)
		}
	}
	_, inputDigest, err := scanTaskWorkspace(context.Background(), sourcePath)
	if err != nil {
		t.Fatalf("scan source workspace: %v", err)
	}
	taskRootPath := filepath.Join(t.TempDir(), "task-views")
	workspace, err := OpenTaskWorkspace(root, taskRootPath, uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspace.Close() })
	if err := workspace.PrepareTaskWorkspaceView(context.Background(), "habc1234", taskID, workspaceID, viewID, 1, 0, 0, inputDigest); err != nil {
		t.Fatalf("prepare Task Workspace view: %v", err)
	}
	viewPath := filepath.Join(taskRootPath, taskID, viewID, taskWorkspaceContentDirectory)
	manifest, copiedDigest, err := scanTaskWorkspace(context.Background(), viewPath)
	if err != nil || copiedDigest != inputDigest || len(manifest.Files) != 1 || manifest.Files[0].ACL == nil || len(manifest.Directories) != 2 ||
		manifest.Files[0].ACL.Platform != "linux" || manifest.Files[0].ACL.Format != "linux-posix-acl-v1" {
		t.Fatalf("copied ACL manifest = %#v, digest %q, err %v; want source digest %q", manifest.Files, copiedDigest, err, inputDigest)
	}
	var aclAttributes []mordesync.ExtendedAttribute
	if err := json.Unmarshal(manifest.Files[0].ACL.Data, &aclAttributes); err != nil || len(aclAttributes) != 1 ||
		aclAttributes[0].Name != "system.posix_acl_access" || !bytes.Equal(aclAttributes[0].Value, aclData) {
		t.Fatalf("copied native ACL payload = %s, %v", manifest.Files[0].ACL.Data, err)
	}
	for _, directory := range manifest.Directories {
		if directory.ACL == nil || directory.ACL.Platform != "linux" || directory.ACL.Format != "linux-posix-acl-v1" {
			t.Fatalf("copied directory ACL for %q = %#v", directory.Path, directory.ACL)
		}
	}
	page, err := workspace.TaskWorkspaceManifestPage(context.Background(), taskID, viewID, "", "", 10)
	if err != nil || len(page.Files) != 1 || page.Files[0].ACL == nil || page.Files[0].ACL.Platform != "linux" ||
		page.Files[0].ACL.Format != "linux-posix-acl-v1" || !bytes.Equal(page.Files[0].ACL.Data, manifest.Files[0].ACL.Data) || len(page.Directories) != 2 {
		t.Fatalf("exported Task Workspace ACL = %#v, %v", page.Files, err)
	}
	for _, directory := range page.Directories {
		if directory.ACL == nil || directory.ACL.Platform != "linux" || directory.ACL.Format != "linux-posix-acl-v1" {
			t.Fatalf("exported directory ACL for %q = %#v", directory.Path, directory.ACL)
		}
	}
}

func taskWorkspaceLinuxTestACL() []byte {
	const (
		aclVersion = 2
		userObj    = 0x01
		user       = 0x02
		groupObj   = 0x04
		mask       = 0x10
		other      = 0x20
	)
	entries := []struct {
		tag  uint16
		perm uint16
		id   uint32
	}{
		{tag: userObj, perm: 7, id: ^uint32(0)},
		{tag: user, perm: 4, id: 12345},
		{tag: groupObj, perm: 4, id: ^uint32(0)},
		{tag: mask, perm: 4, id: ^uint32(0)},
		{tag: other, perm: 0, id: ^uint32(0)},
	}
	data := make([]byte, 4+len(entries)*8)
	binary.LittleEndian.PutUint32(data, aclVersion)
	for index, entry := range entries {
		offset := 4 + index*8
		binary.LittleEndian.PutUint16(data[offset:], entry.tag)
		binary.LittleEndian.PutUint16(data[offset+2:], entry.perm)
		binary.LittleEndian.PutUint32(data[offset+4:], entry.id)
	}
	return data
}
