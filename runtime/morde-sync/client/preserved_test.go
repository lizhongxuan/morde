package client

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mordesync "morde-sync"
)

func TestLiveReplacementKeepsLocalRecoveryCopyOutsideSyncTree(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "workspace")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, directory, "main.go", "baseline")
	oldHandle, err := os.OpenFile(filepath.Join(directory, "main.go"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer oldHandle.Close()

	expected := makeFile("main.go", []byte("baseline"))
	remote := makeFile("main.go", []byte("remote update"))
	if _, err := replaceIfHash(context.Background(), directory, testWorkspace, expected, remote, strings.NewReader("remote update")); err != nil {
		t.Fatalf("replace live file: %v", err)
	}
	assertLocalFile(t, directory, "main.go", "remote update")

	if _, err := oldHandle.WriteAt([]byte("late edit"), 0); err != nil {
		t.Fatalf("write through pre-sync file handle: %v", err)
	}
	items, err := ListPreserved(context.Background(), directory, testWorkspace)
	if err != nil || len(items) != 1 || items[0].Path != "main.go" {
		t.Fatalf("preserved files = %#v, %v", items, err)
	}
	archivePath := filepath.Join(parent, preservedDirectory, testWorkspace, items[0].ID, "content")
	archived, err := os.ReadFile(archivePath)
	if err != nil || string(archived) != "late edit" {
		t.Fatalf("late write was not retained in recovery copy: %q, %v", archived, err)
	}
	manifest, err := mordesync.Scan(context.Background(), parent, nil)
	if err != nil || manifest.FileCount != 1 || manifest.Files[0].Path != filepath.Join("workspace", "main.go") {
		t.Fatalf("parent Workspace manifest included recovery data: %#v, %v", manifest, err)
	}

	if err := os.Remove(filepath.Join(directory, "main.go")); err != nil {
		t.Fatal(err)
	}
	if err := RestorePreserved(context.Background(), directory, testWorkspace, items[0].ID); err != nil {
		t.Fatalf("restore local recovery copy: %v", err)
	}
	assertLocalFile(t, directory, "main.go", "late edit")
	if err := RestorePreserved(context.Background(), directory, testWorkspace, items[0].ID); !errors.Is(err, ErrPreservedDestinationExists) {
		t.Fatalf("restore over existing file = %v, want conflict", err)
	}
	if itemsAfterRestore, err := ListPreserved(context.Background(), directory, testWorkspace); err != nil || len(itemsAfterRestore) != 1 {
		t.Fatalf("restore removed the archive: %#v, %v", itemsAfterRestore, err)
	}
}

func TestLiveDeleteKeepsRestorableLocalRecoveryCopy(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "remove.txt", "baseline")
	expected := makeFile("remove.txt", []byte("baseline"))
	_, removed, err := removeIfHash(context.Background(), directory, testWorkspace, expected)
	if err != nil || !removed {
		t.Fatalf("remove live file = removed %t, err %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "remove.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted path still exists: %v", err)
	}
	items, err := ListPreserved(context.Background(), directory, testWorkspace)
	if err != nil || len(items) != 1 || items[0].Path != "remove.txt" {
		t.Fatalf("preserved files = %#v, %v", items, err)
	}
	if err := RestorePreserved(context.Background(), directory, testWorkspace, items[0].ID); err != nil {
		t.Fatalf("restore deleted file: %v", err)
	}
	assertLocalFile(t, directory, "remove.txt", "baseline")
}

func TestLiveSyncReplacesAndRestoresSymlinkIdentity(t *testing.T) {
	linkFile := func(t *testing.T, directory, name, target string) mordesync.File {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(directory, filepath.FromSlash(name))); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		info, err := os.Lstat(filepath.Join(directory, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		return mordesync.File{Path: name, Kind: mordesync.FileKindSymlink, SymlinkTarget: target,
			Size: int64(len([]byte(target))), Mode: 0, SHA256: sumSHA256([]byte(target)),
			ModTimeNS: info.ModTime().UnixNano(), ModTimeKnown: true}
	}

	t.Run("link target update and recovery", func(t *testing.T) {
		directory := t.TempDir()
		writeTestFile(t, directory, "target.txt", "target stays untouched")
		expected := linkFile(t, directory, "link", "target.txt")
		remote := mordesync.File{Path: "link", Kind: mordesync.FileKindSymlink, SymlinkTarget: "dangling.txt",
			Size: int64(len("dangling.txt")), Mode: 0, SHA256: sumSHA256([]byte("dangling.txt"))}
		if _, err := replaceIfHash(context.Background(), directory, testWorkspace, expected, remote, nil); err != nil {
			t.Fatalf("replace link target: %v", err)
		}
		if target, err := os.Readlink(filepath.Join(directory, "link")); err != nil || target != remote.SymlinkTarget {
			t.Fatalf("updated symlink = %q, %v", target, err)
		}
		assertLocalFile(t, directory, "target.txt", "target stays untouched")
		items, err := ListPreserved(context.Background(), directory, testWorkspace)
		if err != nil || len(items) != 1 || items[0].Path != "link" || items[0].Size != expected.Size {
			t.Fatalf("preserved symlink = %#v, %v", items, err)
		}
		if err := os.Remove(filepath.Join(directory, "link")); err != nil {
			t.Fatal(err)
		}
		if err := RestorePreserved(context.Background(), directory, testWorkspace, items[0].ID); err != nil {
			t.Fatalf("restore original symlink: %v", err)
		}
		if target, err := os.Readlink(filepath.Join(directory, "link")); err != nil || target != expected.SymlinkTarget {
			t.Fatalf("restored symlink = %q, %v", target, err)
		}
		assertLocalFile(t, directory, "target.txt", "target stays untouched")
	})

	t.Run("symlink to regular file", func(t *testing.T) {
		directory := t.TempDir()
		expected := linkFile(t, directory, "link", "target.txt")
		remote := makeFile("link", []byte("regular result"))
		if _, err := replaceIfHash(context.Background(), directory, testWorkspace, expected, remote, strings.NewReader("regular result")); err != nil {
			t.Fatalf("replace symlink with file: %v", err)
		}
		info, err := os.Lstat(filepath.Join(directory, "link"))
		got, readErr := os.ReadFile(filepath.Join(directory, "link"))
		if err != nil || readErr != nil || !info.Mode().IsRegular() || string(got) != "regular result" {
			t.Fatalf("replacement = %#v %q, %v/%v", info, got, err, readErr)
		}
		items, err := ListPreserved(context.Background(), directory, testWorkspace)
		if err != nil || len(items) != 1 {
			t.Fatalf("preserved symlink = %#v, %v", items, err)
		}
		if err := os.Remove(filepath.Join(directory, "link")); err != nil {
			t.Fatal(err)
		}
		if err := RestorePreserved(context.Background(), directory, testWorkspace, items[0].ID); err != nil {
			t.Fatalf("restore replaced symlink: %v", err)
		}
		if target, err := os.Readlink(filepath.Join(directory, "link")); err != nil || target != expected.SymlinkTarget {
			t.Fatalf("restored symlink = %q, %v", target, err)
		}
	})

	t.Run("regular file to symlink", func(t *testing.T) {
		directory := t.TempDir()
		writeTestFile(t, directory, "link", "local regular file")
		expected := makeFile("link", []byte("local regular file"))
		remote := mordesync.File{Path: "link", Kind: mordesync.FileKindSymlink, SymlinkTarget: "remote.txt",
			Size: int64(len("remote.txt")), Mode: 0, SHA256: sumSHA256([]byte("remote.txt"))}
		if _, err := replaceIfHash(context.Background(), directory, testWorkspace, expected, remote, nil); err != nil {
			t.Fatalf("replace file with symlink: %v", err)
		}
		if target, err := os.Readlink(filepath.Join(directory, "link")); err != nil || target != remote.SymlinkTarget {
			t.Fatalf("replacement symlink = %q, %v", target, err)
		}
		items, err := ListPreserved(context.Background(), directory, testWorkspace)
		if err != nil || len(items) != 1 {
			t.Fatalf("preserved regular file = %#v, %v", items, err)
		}
		if err := os.Remove(filepath.Join(directory, "link")); err != nil {
			t.Fatal(err)
		}
		if err := RestorePreserved(context.Background(), directory, testWorkspace, items[0].ID); err != nil {
			t.Fatalf("restore replaced regular file: %v", err)
		}
		assertLocalFile(t, directory, "link", "local regular file")
	})
}

func TestLiveDeletePreservesAndRestoresSymlinkWithoutFollowingIt(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "target.txt", "untouched")
	if err := os.Symlink("target.txt", filepath.Join(directory, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	info, err := os.Lstat(filepath.Join(directory, "link"))
	if err != nil {
		t.Fatal(err)
	}
	expected := mordesync.File{Path: "link", Kind: mordesync.FileKindSymlink, SymlinkTarget: "target.txt",
		Size: int64(len("target.txt")), Mode: 0, SHA256: sumSHA256([]byte("target.txt")),
		ModTimeNS: info.ModTime().UnixNano(), ModTimeKnown: true}
	_, removed, err := removeIfHash(context.Background(), directory, testWorkspace, expected)
	if err != nil || !removed {
		t.Fatalf("remove symlink = removed %t, err %v", removed, err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "link")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deleted symlink remains: %v", err)
	}
	items, err := ListPreserved(context.Background(), directory, testWorkspace)
	if err != nil || len(items) != 1 || items[0].Size != expected.Size {
		t.Fatalf("preserved symlink = %#v, %v", items, err)
	}
	if err := RestorePreserved(context.Background(), directory, testWorkspace, items[0].ID); err != nil {
		t.Fatalf("restore deleted symlink: %v", err)
	}
	if target, err := os.Readlink(filepath.Join(directory, "link")); err != nil || target != expected.SymlinkTarget {
		t.Fatalf("restored symlink = %q, %v", target, err)
	}
	assertLocalFile(t, directory, "target.txt", "untouched")
}

func TestDeletePreservedRemovesOnlySelectedRecoveryCopy(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "first.txt", "first baseline")
	writeTestFile(t, directory, "second.txt", "second baseline")
	first := makeFile("first.txt", []byte("first baseline"))
	second := makeFile("second.txt", []byte("second baseline"))
	if _, err := replaceIfHash(context.Background(), directory, testWorkspace, first,
		makeFile("first.txt", []byte("first update")), strings.NewReader("first update")); err != nil {
		t.Fatalf("preserve first file: %v", err)
	}
	if _, err := replaceIfHash(context.Background(), directory, testWorkspace, second,
		makeFile("second.txt", []byte("second update")), strings.NewReader("second update")); err != nil {
		t.Fatalf("preserve second file: %v", err)
	}
	items, err := ListPreserved(context.Background(), directory, testWorkspace)
	if err != nil || len(items) != 2 {
		t.Fatalf("recovery copies = %#v, %v", items, err)
	}
	var selected, remaining PreservedFile
	for _, item := range items {
		switch item.Path {
		case "first.txt":
			selected = item
		case "second.txt":
			remaining = item
		}
	}
	if selected.ID == "" || remaining.ID == "" {
		t.Fatalf("unexpected recovery copies: %#v", items)
	}
	if err := DeletePreserved(context.Background(), directory, testWorkspace, selected.ID); err != nil {
		t.Fatalf("delete selected recovery copy: %v", err)
	}
	items, err = ListPreserved(context.Background(), directory, testWorkspace)
	if err != nil || len(items) != 1 || items[0].ID != remaining.ID {
		t.Fatalf("remaining recovery copies = %#v, %v", items, err)
	}
	assertLocalFile(t, directory, "first.txt", "first update")
	assertLocalFile(t, directory, "second.txt", "second update")
	if err := DeletePreserved(context.Background(), directory, testWorkspace, selected.ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delete missing recovery copy = %v, want not-exist", err)
	}
	if err := DeletePreserved(context.Background(), directory, testWorkspace, "../first.txt"); err == nil {
		t.Fatal("invalid recovery ID was accepted")
	}
}

func TestDeletePreservedRejectsContentSymlinkWithoutFollowingIt(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "workspace")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	entry := filepath.Join(parent, preservedDirectory, testWorkspace, id)
	if err := os.MkdirAll(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	metadata := preservedFileMetadata{ID: id, WorkspaceID: testWorkspace, Path: "notes.txt", CreatedAt: time.Now().UTC()}
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(entry, "metadata.json"), metadataBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(target, []byte("outside data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(entry, "content")); err != nil {
		t.Skipf("file symlink unavailable: %v", err)
	}
	if err := DeletePreserved(context.Background(), directory, testWorkspace, id); err == nil {
		t.Fatal("deleted recovery copy with symlink content")
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "outside data" {
		t.Fatalf("symlink target changed: %q, %v", content, err)
	}
	if _, err := os.Lstat(filepath.Join(entry, "content")); err != nil {
		t.Fatalf("recovery symlink was removed after rejection: %v", err)
	}
}

func TestDeletePreservedRemovesSafeSymlinkWithoutFollowingTarget(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "target.txt", "untouched")
	if err := os.Symlink("target.txt", filepath.Join(directory, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	info, err := os.Lstat(filepath.Join(directory, "link"))
	if err != nil {
		t.Fatal(err)
	}
	expected := mordesync.File{Path: "link", Kind: mordesync.FileKindSymlink, SymlinkTarget: "target.txt",
		Size: int64(len("target.txt")), Mode: 0, SHA256: sumSHA256([]byte("target.txt")),
		ModTimeNS: info.ModTime().UnixNano(), ModTimeKnown: true}
	remote := mordesync.File{Path: "link", Kind: mordesync.FileKindSymlink, SymlinkTarget: "other.txt",
		Size: int64(len("other.txt")), Mode: 0, SHA256: sumSHA256([]byte("other.txt"))}
	if _, err := replaceIfHash(context.Background(), directory, testWorkspace, expected, remote, nil); err != nil {
		t.Fatalf("replace symlink: %v", err)
	}
	items, err := ListPreserved(context.Background(), directory, testWorkspace)
	if err != nil || len(items) != 1 {
		t.Fatalf("preserved symlink = %#v, %v", items, err)
	}
	if err := DeletePreserved(context.Background(), directory, testWorkspace, items[0].ID); err != nil {
		t.Fatalf("delete safe preserved symlink: %v", err)
	}
	if items, err := ListPreserved(context.Background(), directory, testWorkspace); err != nil || len(items) != 0 {
		t.Fatalf("preserved entries after deletion = %#v, %v", items, err)
	}
	assertLocalFile(t, directory, "target.txt", "untouched")
	if target, err := os.Readlink(filepath.Join(directory, "link")); err != nil || target != remote.SymlinkTarget {
		t.Fatalf("current symlink = %q, %v", target, err)
	}
}

func TestLiveReplacementDoesNotOverwriteConcurrentLocalFileAfterPreservingBaseline(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "main.go", "baseline")
	expected := makeFile("main.go", []byte("baseline"))
	remote := makeFile("main.go", []byte("remote update"))
	_, err := replaceIfHashAtBoundary(context.Background(), directory, testWorkspace, expected, remote,
		strings.NewReader("remote update"), func() {
			if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte("concurrent edit"), 0o600); err != nil {
				t.Errorf("create concurrent local file: %v", err)
			}
		})
	if !errors.Is(err, mordesync.ErrDestinationChange) {
		t.Fatalf("live replacement error = %v, want destination change", err)
	}
	assertLocalFile(t, directory, "main.go", "concurrent edit")
	items, listErr := ListPreserved(context.Background(), directory, testWorkspace)
	if listErr != nil || len(items) != 1 {
		t.Fatalf("baseline recovery copy = %#v, %v", items, listErr)
	}
}

func TestLiveDeleteDoesNotRemoveConcurrentLocalFileAfterPreservingBaseline(t *testing.T) {
	directory := t.TempDir()
	writeTestFile(t, directory, "remove.txt", "baseline")
	expected := makeFile("remove.txt", []byte("baseline"))
	_, removed, err := removeIfHashAtBoundary(context.Background(), directory, testWorkspace, expected, func() {
		if err := os.WriteFile(filepath.Join(directory, "remove.txt"), []byte("concurrent edit"), 0o600); err != nil {
			t.Errorf("create concurrent local file: %v", err)
		}
	})
	if removed || !errors.Is(err, mordesync.ErrDestinationChange) {
		t.Fatalf("live delete removed=%t err=%v, want destination change", removed, err)
	}
	assertLocalFile(t, directory, "remove.txt", "concurrent edit")
	items, listErr := ListPreserved(context.Background(), directory, testWorkspace)
	if listErr != nil || len(items) != 1 {
		t.Fatalf("baseline recovery copy = %#v, %v", items, listErr)
	}
}

func TestPreservationFailureBlocksLiveReplacement(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "workspace")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, directory, "main.go", "baseline")
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(parent, preservedDirectory)); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	_, err := replaceIfHash(context.Background(), directory, testWorkspace, makeFile("main.go", []byte("baseline")),
		makeFile("main.go", []byte("remote update")), strings.NewReader("remote update"))
	if err == nil {
		t.Fatal("replacement succeeded with an unsafe preservation directory")
	}
	assertLocalFile(t, directory, "main.go", "baseline")
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("preservation followed a symlink: entries=%v err=%v", entries, err)
	}
}

func TestRecoveryDirectoryCannotBeSelectedAsWorkspace(t *testing.T) {
	recoveryRoot := filepath.Join(t.TempDir(), preservedDirectory, testWorkspace)
	if err := os.MkdirAll(recoveryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := openClientWorkspaceRoot(recoveryRoot); err == nil {
		t.Fatal("recovery copy directory was accepted as a sync root")
	}
}
