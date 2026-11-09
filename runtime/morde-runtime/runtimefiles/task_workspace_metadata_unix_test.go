//go:build darwin || linux

package runtimefiles

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"morde-runtime/contract"
	mordesync "morde-sync"
)

func taskWorkspaceTestXattrName() string {
	return "user.morde-runtime-test"
}

func TestTaskWorkspacePreservesAndExportsNativeMetadata(t *testing.T) {
	root := t.TempDir()
	workspaceID, taskID := "wabc1234", "qabc1234"
	viewID := "v" + strings.Repeat("d", 64)
	sourcePath := filepath.Join(root, workspaceID)
	nestedPath := filepath.Join(sourcePath, "nested")
	filePath := filepath.Join(nestedPath, "note.txt")
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(nestedPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("native metadata"), 0o640); err != nil {
		t.Fatal(err)
	}
	attributeName := taskWorkspaceTestXattrName()
	for path, value := range map[string]string{
		sourcePath: "workspace-root",
		nestedPath: "nested-directory",
		filePath:   "regular-file",
	} {
		if err := unix.Setxattr(path, attributeName, []byte(value), 0); err != nil {
			t.Skipf("filesystem does not support extended attributes: %v", err)
		}
	}
	rootModified := time.Unix(1_700_001_000, 123_456_789)
	nestedModified := time.Unix(1_700_002_000, 987_654_321)
	if err := os.Chtimes(sourcePath, rootModified, rootModified); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(nestedPath, nestedModified, nestedModified); err != nil {
		t.Fatal(err)
	}

	sourceManifest, inputDigest, err := scanTaskWorkspace(context.Background(), sourcePath)
	if err != nil {
		t.Fatalf("scan source workspace: %v", err)
	}
	if _, err := taskManifestSHA256(sourceManifest); err != nil {
		t.Fatalf("digest source workspace: %v", err)
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
	viewManifest, viewDigest, err := scanTaskWorkspace(context.Background(), viewPath)
	if err != nil || viewDigest != inputDigest {
		t.Fatalf("copied workspace digest = %q, %v; want %q", viewDigest, err, inputDigest)
	}
	assertXattr := func(path, want string) {
		t.Helper()
		value := make([]byte, len(want)+1)
		count, err := unix.Getxattr(path, attributeName, value)
		if err != nil || string(value[:max(count, 0)]) != want {
			t.Fatalf("xattr %q on %q = %q, %v; want %q", attributeName, path, value[:max(count, 0)], err, want)
		}
	}
	assertXattr(viewPath, "workspace-root")
	assertXattr(filepath.Join(viewPath, "nested"), "nested-directory")
	assertXattr(filepath.Join(viewPath, "nested", "note.txt"), "regular-file")
	if len(viewManifest.Directories) != 2 || viewManifest.Directories[0].Path != "." || viewManifest.Directories[1].Path != "nested" {
		t.Fatalf("copied directory metadata = %#v", viewManifest.Directories)
	}
	if viewManifest.Directories[0].Mode != 0o700 || viewManifest.Directories[0].ModTimeNS != rootModified.UnixNano() ||
		viewManifest.Directories[1].Mode != 0o750 || viewManifest.Directories[1].ModTimeNS != nestedModified.UnixNano() {
		t.Fatalf("copied directory mode/time = %#v", viewManifest.Directories)
	}

	page, err := workspace.TaskWorkspaceManifestPage(context.Background(), taskID, viewID, "", "", 10)
	if err != nil || len(page.Files) != 1 || len(page.Files[0].Xattrs) != 1 || page.Files[0].Xattrs[0].Name != attributeName ||
		!bytes.Equal(page.Files[0].Xattrs[0].Value, []byte("regular-file")) || !page.Files[0].MetadataKnown {
		t.Fatalf("manifest page file metadata = %#v, %v", page.Files, err)
	}
	pageDirectories := make(map[string]TaskWorkspaceManifestDirectory, len(page.Directories))
	for _, directory := range page.Directories {
		pageDirectories[directory.Path] = directory
	}
	for path, want := range map[string]string{".": "workspace-root", "nested": "nested-directory"} {
		directory, ok := pageDirectories[path]
		if !ok || !directory.MetadataKnown || len(directory.Xattrs) != 1 || directory.Xattrs[0].Name != attributeName ||
			!bytes.Equal(directory.Xattrs[0].Value, []byte(want)) {
			t.Fatalf("manifest page directory %q metadata = %#v, present %v", path, directory, ok)
		}
	}

	response, err := workspace.ExportTaskWorkspaceManifestPage(context.Background(), contract.TaskWorkspaceManifestRequest{
		ProtocolVersion: contract.ProtocolVersion, TaskID: taskID, WorkspaceID: workspaceID, BindingVersion: 1,
		InputRevision: 0, WorkspaceCheckpointRevision: 0, InputManifestSHA256: inputDigest, ViewID: viewID, Limit: 10,
	})
	if err != nil || len(response.Files) != 1 || len(response.Files[0].Xattrs) != 1 || len(response.Directories) != 2 {
		t.Fatalf("contract manifest response = %#v, %v", response, err)
	}
	rootExported := false
	for _, directory := range response.Directories {
		if directory.Path == "." && directory.MetadataKnown && len(directory.Xattrs) == 1 &&
			directory.Xattrs[0].Name == attributeName && bytes.Equal(directory.Xattrs[0].Value, []byte("workspace-root")) {
			rootExported = true
		}
	}
	if !rootExported {
		t.Fatalf("contract response omitted native root directory metadata: %#v", response.Directories)
	}

	privateRoot, _, _, err := workspace.openTaskWorkspaceView(taskID, viewID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := readTaskWorkspaceManifestSnapshot(privateRoot, viewID)
	_ = privateRoot.Close()
	if err != nil || len(snapshot.Files) != 1 || len(snapshot.Files[0].Xattrs) != 1 || len(snapshot.Directories) != 2 {
		t.Fatalf("private metadata snapshot = %#v, %v", snapshot, err)
	}
	snapshot.Files[0].Xattrs[0].Value = []byte("mutated")
	if err := validateTaskWorkspaceManifestSnapshot(&snapshot); !errors.Is(err, ErrTaskWorkspaceView) {
		t.Fatalf("mutated native metadata snapshot validation = %v; want ErrTaskWorkspaceView", err)
	}
}

func TestTaskWorkspaceRejectsSourceMetadataMutationBeforePublish(t *testing.T) {
	root := t.TempDir()
	workspaceID, taskID := "wabc1234", "qabc1234"
	viewID := "v" + strings.Repeat("e", 64)
	sourcePath := filepath.Join(root, workspaceID)
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(sourcePath, "note.txt")
	if err := os.WriteFile(filePath, []byte("unchanged bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filePath, taskWorkspaceTestXattrName(), []byte("before"), 0); err != nil {
		t.Skipf("filesystem does not support extended attributes: %v", err)
	}
	_, originalDigest, err := scanTaskWorkspace(context.Background(), sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filePath, taskWorkspaceTestXattrName(), []byte("after"), 0); err != nil {
		t.Fatal(err)
	}
	taskRootPath := filepath.Join(t.TempDir(), "task-views")
	workspace, err := OpenTaskWorkspace(root, taskRootPath, uint32(os.Getuid()), uint32(os.Getgid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspace.Close() })
	if err := workspace.PrepareTaskWorkspaceView(context.Background(), "habc1234", taskID, workspaceID, viewID, 1, 0, 0, originalDigest); !errors.Is(err, ErrTaskWorkspaceView) {
		t.Fatalf("prepare with mutated source xattr = %v; want ErrTaskWorkspaceView", err)
	}
	if _, err := os.Lstat(filepath.Join(taskRootPath, taskID, viewID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata-mutated Task Workspace view was published: %v", err)
	}
}

func TestCopyTaskManifestRejectsIncompatibleACL(t *testing.T) {
	sourcePath := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourcePath, "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := mordesync.Scan(context.Background(), sourcePath, nil)
	if err != nil || len(manifest.Files) != 1 {
		t.Fatalf("scan source: manifest=%#v err=%v", manifest, err)
	}
	foreignACL := &mordesync.NativeACL{Platform: "linux", Format: "linux-posix-acl-v1", Data: []byte("[]")}
	if runtime.GOOS == "linux" {
		foreignACL = &mordesync.NativeACL{Platform: "darwin", Format: "darwin-filesec-v1", Data: []byte("opaque foreign ACL")}
	}
	manifest.Files[0].MetadataKnown = true
	manifest.Files[0].ACL = foreignACL
	targetPath := t.TempDir()
	sourceRoot, err := os.OpenRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRoot.Close()
	targetRoot, err := os.OpenRoot(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	defer targetRoot.Close()
	if err := copyTaskManifest(context.Background(), sourceRoot, targetRoot, targetPath, manifest, uint32(os.Getuid()), uint32(os.Getgid())); !errors.Is(err, ErrTaskWorkspaceView) {
		t.Fatalf("copy with foreign ACL = %v; want ErrTaskWorkspaceView", err)
	}
	if _, err := os.Lstat(filepath.Join(targetPath, "file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incompatible ACL left a partial target file: %v", err)
	}
}
