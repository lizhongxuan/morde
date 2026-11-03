//go:build darwin || linux

package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCopyWorkspaceVolumePreservesNativeMetadata(t *testing.T) {
	client, _, sourcePath, targetPath := newWorkspaceCopyDockerForTest(t, nil, "")
	attribute := "user.workspace-copy-test"
	file := filepath.Join(sourcePath, "settings.ini")
	if err := os.WriteFile(file, []byte("settings"), 0600); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string]string{sourcePath: "root", file: "file"} {
		if err := unix.Setxattr(path, attribute, []byte(value), 0); err != nil {
			if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
				t.Skipf("filesystem does not support extended attributes: %v", err)
			}
			t.Fatal(err)
		}
	}
	request := workspaceCopyRequest(t, sourcePath)
	copiedFiles, copiedDirectories, err := client.CopyWorkspaceVolume(context.Background(), request,
		workspaceCopyFenceFunc(func(context.Context, WorkspaceVolumeCopyRequest) error { return nil }))
	if err != nil || len(copiedFiles) != 1 || !copiedFiles[0].MetadataKnown || len(copiedFiles[0].Xattrs) != 1 ||
		copiedFiles[0].Xattrs[0].Name != attribute || string(copiedFiles[0].Xattrs[0].Value) != "file" {
		t.Fatalf("copied file metadata = %#v, err=%v", copiedFiles, err)
	}
	if len(copiedDirectories) != 1 || copiedDirectories[0].Path != "." || !copiedDirectories[0].MetadataKnown ||
		len(copiedDirectories[0].Xattrs) != 1 || copiedDirectories[0].Xattrs[0].Name != attribute ||
		string(copiedDirectories[0].Xattrs[0].Value) != "root" {
		t.Fatalf("copied root directory metadata = %#v", copiedDirectories)
	}
	assertXattr := func(path, want string) {
		t.Helper()
		value := make([]byte, len(want)+1)
		count, err := unix.Getxattr(path, attribute, value)
		if err != nil || string(value[:max(count, 0)]) != want {
			t.Fatalf("xattr on %q = %q, %v; want %q", path, value[:max(count, 0)], err, want)
		}
	}
	assertXattr(targetPath, "root")
	assertXattr(filepath.Join(targetPath, "settings.ini"), "file")
}

func TestWorkspaceCopyPreflightRejectsNativeMetadataChange(t *testing.T) {
	client, _, sourcePath, targetPath := newWorkspaceCopyDockerForTest(t, nil, "")
	attribute := "user.workspace-copy-test"
	file := filepath.Join(sourcePath, "settings.ini")
	if err := os.WriteFile(file, []byte("settings"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(file, attribute, []byte("before"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skipf("filesystem does not support extended attributes: %v", err)
		}
		t.Fatal(err)
	}
	request := workspaceCopyRequest(t, sourcePath)
	if err := unix.Setxattr(file, attribute, []byte("after"), 0); err != nil {
		t.Fatal(err)
	}
	err := client.ValidateWorkspaceVolumeCopySource(context.Background(), request,
		workspaceCopyFenceFunc(func(context.Context, WorkspaceVolumeCopyRequest) error { return nil }))
	if err == nil || !containsText(err.Error(), "source Workspace volume does not match") {
		t.Fatalf("ValidateWorkspaceVolumeCopySource() = %v; want native metadata mismatch", err)
	}
	if entries, readErr := os.ReadDir(targetPath); readErr != nil || len(entries) != 0 {
		t.Fatalf("native metadata preflight changed destination: entries=%v err=%v", entries, readErr)
	}
}
