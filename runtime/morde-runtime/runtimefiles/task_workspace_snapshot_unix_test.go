//go:build darwin || linux

package runtimefiles

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	mordesync "morde-sync"
)

func TestTaskWorkspaceExportSnapshotMatchesOpenedRootAndTracksChanges(t *testing.T) {
	workspace, taskID, viewID, _, viewPath := openTaskExportWorkspace(t, map[string]string{"seed.txt": "seed"})
	nested := filepath.Join(viewPath, "nested")
	if err := os.Mkdir(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(viewPath, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	dataPath := filepath.Join(nested, "data.txt")
	if err := os.WriteFile(dataPath, []byte("before"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(dataPath, filepath.Join(nested, "alias.txt")); err != nil {
		t.Skipf("hardlinks are unavailable: %v", err)
	}
	if err := os.Symlink("data.txt", filepath.Join(nested, "link.txt")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if err := unix.Setxattr(dataPath, "user.morde-snapshot-test", []byte("before"), 0); err != nil {
		t.Skipf("user xattrs are unavailable: %v", err)
	}
	modified := time.Unix(1_700_000_000, 987_654_321)
	if err := os.Chtimes(dataPath, modified, modified); err != nil {
		t.Fatal(err)
	}

	firstPage, firstManifest := taskWorkspaceSnapshotPageAndOpenedScan(t, workspace, taskID, viewID, viewPath)
	if firstPage.WorkspaceSnapshotSHA256 == "" || firstPage.WorkspaceSnapshotSHA256 != firstManifest {
		t.Fatalf("export digest = %q; opened-root digest = %q", firstPage.WorkspaceSnapshotSHA256, firstManifest)
	}
	firstSnapshot, err := mordesync.Scan(context.Background(), viewPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sawHardlink, sawSymlink, sawMetadata bool
	for _, file := range firstSnapshot.Files {
		sawHardlink = sawHardlink || file.HardlinkTo != ""
		sawSymlink = sawSymlink || file.Kind == mordesync.FileKindSymlink
		if file.Path == "nested/data.txt" {
			sawMetadata = file.MetadataKnown && len(file.Xattrs) == 1 && string(file.Xattrs[0].Value) == "before" && file.ModTimeKnown
		}
	}
	if !sawHardlink || !sawSymlink || !sawMetadata || len(firstSnapshot.EmptyDirectories) != 1 || firstSnapshot.EmptyDirectories[0] != "empty" {
		t.Fatalf("fixture coverage hardlink=%v symlink=%v metadata=%v empty=%#v", sawHardlink, sawSymlink, sawMetadata, firstSnapshot.EmptyDirectories)
	}

	if err := os.WriteFile(dataPath, []byte("after execution"), 0o640); err != nil {
		t.Fatal(err)
	}
	secondPage, secondManifest := taskWorkspaceSnapshotPageAndOpenedScan(t, workspace, taskID, viewID, viewPath)
	if secondPage.WorkspaceSnapshotSHA256 != secondManifest || secondPage.WorkspaceSnapshotSHA256 == firstPage.WorkspaceSnapshotSHA256 {
		t.Fatalf("content-change export/opened-root digests = %q/%q; first=%q", secondPage.WorkspaceSnapshotSHA256, secondManifest, firstPage.WorkspaceSnapshotSHA256)
	}

	if err := unix.Setxattr(dataPath, "user.morde-snapshot-test", []byte("after"), 0); err != nil {
		t.Fatal(err)
	}
	metadataPage, metadataManifest := taskWorkspaceSnapshotPageAndOpenedScan(t, workspace, taskID, viewID, viewPath)
	if metadataPage.WorkspaceSnapshotSHA256 != metadataManifest || metadataPage.WorkspaceSnapshotSHA256 == secondPage.WorkspaceSnapshotSHA256 {
		t.Fatalf("metadata-change export/opened-root digests = %q/%q; prior=%q", metadataPage.WorkspaceSnapshotSHA256, metadataManifest, secondPage.WorkspaceSnapshotSHA256)
	}
}

func taskWorkspaceSnapshotPageAndOpenedScan(t *testing.T, workspace *Workspace, taskID, viewID, viewPath string) (TaskWorkspaceManifestPage, string) {
	t.Helper()
	page, err := workspace.TaskWorkspaceManifestPage(context.Background(), taskID, viewID, "", "", TaskWorkspaceManifestPageFilesMax)
	if err != nil {
		t.Fatalf("export Task Workspace snapshot: %v", err)
	}
	root, err := os.OpenRoot(viewPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest, scanErr := mordesync.ScanOpenedRoot(context.Background(), root, nil)
	closeErr := root.Close()
	if scanErr != nil || closeErr != nil {
		t.Fatalf("scan opened Task Workspace root: %v, close %v", scanErr, closeErr)
	}
	digest, err := mordesync.TaskWorkspaceSnapshotSHA256(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 64 {
		t.Fatalf("opened-root snapshot digest has length %d", len(digest))
	}
	return page, digest
}
