package client

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

func TestHubInspectionListsAndReadsWithinSelectedRoot(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "notes"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("hello from the selected root")
	if err := os.WriteFile(filepath.Join(directory, "notes", "readme.txt"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	selection, err := SelectHubRoot(directory)
	if err != nil {
		t.Fatalf("SelectHubRoot() error = %v", err)
	}
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Directory != canonical || len(selection.Identity) != 64 {
		t.Fatalf("selection = %+v", selection)
	}
	secondSelection, err := SelectHubRoot(directory)
	if err != nil || secondSelection.Identity != selection.Identity {
		t.Fatalf("identity was not stable across calls: first=%+v second=%+v err=%v", selection, secondSelection, err)
	}
	entries, err := ListHubFiles(selection.Directory, selection.Identity, "notes")
	if err != nil {
		t.Fatalf("ListHubFiles() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Path != "notes/readme.txt" || entries[0].Kind != "file" || entries[0].SizeBytes != int64(len(content)) {
		t.Fatalf("entries = %+v", entries)
	}
	got, err := ReadHubFile(selection.Directory, selection.Identity, entries[0].Path, HubInspectMaxBytes)
	if err != nil || got.Path != entries[0].Path || got.Text != string(content) || got.Truncated {
		t.Fatalf("ReadHubFile() = %+v, err=%v", got, err)
	}
}

func TestHubInspectionSkipsOutsideSymlinkAndRejectsNestedSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "outside-link")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "nested-link")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	selection, err := SelectHubRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ListHubFiles(selection.Directory, selection.Identity, "")
	if err != nil {
		t.Fatalf("ListHubFiles() error = %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("symlink entries were returned: %+v", entries)
	}
	if _, err := ReadHubFile(selection.Directory, selection.Identity, "outside-link", 32); err == nil {
		t.Fatal("ReadHubFile followed an outside symlink")
	}
	if _, err := ListHubFiles(selection.Directory, selection.Identity, "nested-link"); err == nil {
		t.Fatal("ListHubFiles followed a nested symlink")
	}
	if _, err := ReadHubFile(selection.Directory, selection.Identity, "nested-link/secret.txt", 32); err == nil {
		t.Fatal("ReadHubFile followed a nested symlink")
	}
}

func TestHubInspectionRejectsRootReplacement(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "selected")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	selection, err := SelectHubRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, filepath.Join(parent, "old-selected")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "replacement-secret"), []byte("new root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListHubFiles(directory, selection.Identity, ""); err == nil {
		t.Fatal("root replacement was accepted with the old identity")
	}
	if _, err := ReadHubFile(directory, selection.Identity, "replacement-secret", 32); err == nil {
		t.Fatal("root replacement exposed the replacement directory")
	}
}

func TestHubInspectionRejectsTraversalAndInvalidBounds(t *testing.T) {
	directory := t.TempDir()
	selection, err := SelectHubRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../outside", "a/../outside", "/etc/passwd", `a\\b`, "~/secret", "a//b", "a/./b"} {
		if _, err := ListHubFiles(selection.Directory, selection.Identity, path); err == nil {
			t.Errorf("ListHubFiles accepted unsafe path %q", path)
		}
		if _, err := ReadHubFile(selection.Directory, selection.Identity, path, 32); err == nil {
			t.Errorf("ReadHubFile accepted unsafe path %q", path)
		}
	}
	if _, err := ReadHubFile(selection.Directory, selection.Identity, "x", 0); err == nil {
		t.Fatal("ReadHubFile accepted a zero byte limit")
	}
	if _, err := ReadHubFile(selection.Directory, selection.Identity, "x", HubInspectMaxBytes+1); err == nil {
		t.Fatal("ReadHubFile accepted an oversized byte limit")
	}
}

func TestHubInspectionTruncatesAtValidUTF8Boundary(t *testing.T) {
	directory := t.TempDir()
	content := []byte("ab界tail")
	if err := os.WriteFile(filepath.Join(directory, "text.txt"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	selection, err := SelectHubRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadHubFile(selection.Directory, selection.Identity, "text.txt", 4)
	if err != nil {
		t.Fatalf("ReadHubFile() error = %v", err)
	}
	if got.Text != "ab" || !got.Truncated || !utf8.ValidString(got.Text) {
		t.Fatalf("truncated text = %+v", got)
	}
}

func TestHubInspectionRejectsInvalidUTF8(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "binary.txt"), []byte{'a', 0xff, 'b'}, 0o600); err != nil {
		t.Fatal(err)
	}
	selection, err := SelectHubRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHubFile(selection.Directory, selection.Identity, "binary.txt", 32); err == nil {
		t.Fatal("ReadHubFile accepted invalid UTF-8")
	}
}

func TestHubInspectionCapsDirectoryEntries(t *testing.T) {
	directory := t.TempDir()
	for index := 0; index < HubInspectMaxEntries+20; index++ {
		name := filepath.Join(directory, fmt.Sprintf("entry-%03d.txt", index))
		if err := os.WriteFile(name, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	selection, err := SelectHubRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ListHubFiles(selection.Directory, selection.Identity, "")
	if err != nil {
		t.Fatalf("ListHubFiles() error = %v", err)
	}
	if len(entries) != HubInspectMaxEntries {
		t.Fatalf("entry count = %d, want %d", len(entries), HubInspectMaxEntries)
	}
}
