package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	if _, err := SnapshotHubFile(selection.Directory, selection.Identity, "outside-link"); err == nil {
		t.Fatal("SnapshotHubFile followed an outside symlink")
	}
	if _, err := ReadHubFileChunk(selection.Directory, selection.Identity, "outside-link", "", 0, 32); err == nil {
		t.Fatal("ReadHubFileChunk followed an outside symlink")
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
	if err := os.WriteFile(filepath.Join(directory, "selected.pdf"), []byte("selected file"), 0o600); err != nil {
		t.Fatal(err)
	}
	selection, err := SelectHubRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotHubFile(selection.Directory, selection.Identity, "selected.pdf")
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
	if _, err := SnapshotHubFile(directory, selection.Identity, "replacement-secret"); err == nil {
		t.Fatal("root replacement was accepted by SnapshotHubFile")
	}
	if _, err := ReadHubFileChunk(directory, selection.Identity, "selected.pdf", snapshot.Version, 0, 8); err == nil {
		t.Fatal("root replacement was accepted by ReadHubFileChunk")
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
	if _, err := ReadHubFileChunk(selection.Directory, selection.Identity, "x", strings.Repeat("a", 64), -1, 1); err == nil {
		t.Fatal("ReadHubFileChunk accepted a negative offset")
	}
	if _, err := ReadHubFileChunk(selection.Directory, selection.Identity, "x", strings.Repeat("a", 64), 0, HubInspectMaxChunkBytes+1); err == nil {
		t.Fatal("ReadHubFileChunk accepted an oversized byte limit")
	}
}

func TestHubInspectionSnapshotsAndReadsLargeBinaryFiles(t *testing.T) {
	directory := t.TempDir()
	content := make([]byte, HubInspectMaxBytes+37)
	for index := range content {
		content[index] = byte(index % 251)
	}
	for _, name := range []string{"report.pdf", "book.docx"} {
		if err := os.WriteFile(filepath.Join(directory, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	selection, err := SelectHubRoot(directory)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"report.pdf", "book.docx"} {
		snapshot, err := SnapshotHubFile(selection.Directory, selection.Identity, name)
		if err != nil {
			t.Fatalf("SnapshotHubFile(%s) error = %v", name, err)
		}
		wantDigest := sha256.Sum256(content)
		wantSHA := hex.EncodeToString(wantDigest[:])
		if snapshot.Path != name || snapshot.SizeBytes != int64(len(content)) || snapshot.SHA256 != wantSHA || len(snapshot.Version) != sha256.Size*2 {
			t.Fatalf("snapshot for %s = %+v", name, snapshot)
		}
		repeated, err := SnapshotHubFile(selection.Directory, selection.Identity, name)
		if err != nil || repeated.Version != snapshot.Version {
			t.Fatalf("snapshot version was not stable: first=%+v repeated=%+v err=%v", snapshot, repeated, err)
		}

		var reconstructed []byte
		for offset := int64(0); offset < snapshot.SizeBytes; {
			chunk, err := ReadHubFileChunk(selection.Directory, selection.Identity, name, snapshot.Version, offset, 7<<10)
			if err != nil {
				t.Fatalf("ReadHubFileChunk(%s, %d) error = %v", name, offset, err)
			}
			if chunk.Path != name || chunk.Offset != offset || chunk.Version != snapshot.Version || len(chunk.Data) == 0 || len(chunk.Data) > 7<<10 {
				t.Fatalf("chunk for %s at %d = %+v", name, offset, chunk)
			}
			chunkDigest := sha256.Sum256(chunk.Data)
			if chunk.SHA256 != hex.EncodeToString(chunkDigest[:]) {
				t.Fatalf("chunk digest mismatch for %s at %d", name, offset)
			}
			reconstructed = append(reconstructed, chunk.Data...)
			offset += int64(len(chunk.Data))
		}
		gotDigest := sha256.Sum256(reconstructed)
		if !bytes.Equal(reconstructed, content) || hex.EncodeToString(gotDigest[:]) != snapshot.SHA256 {
			t.Fatalf("reconstructed bytes for %s differ from the snapshot", name)
		}
	}
}

func TestHubInspectionRejectsChangedFileAfterSnapshot(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "report.pdf")
	original := bytes.Repeat([]byte("A"), HubInspectMaxBytes+19)
	changed := bytes.Repeat([]byte("B"), len(original))
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := SelectHubRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotHubFile(selection.Directory, selection.Identity, "report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHubFileChunk(selection.Directory, selection.Identity, "report.pdf", snapshot.Version, 0, 64); !errors.Is(err, ErrHubFileChanged) {
		t.Fatalf("ReadHubFileChunk error = %v, want ErrHubFileChanged", err)
	}
}

func TestHubInspectionRejectsParentSymlinkReplacementAfterSnapshot(t *testing.T) {
	rootDirectory := t.TempDir()
	outsideDirectory := t.TempDir()
	selectedParent := filepath.Join(rootDirectory, "nested")
	if err := os.Mkdir(selectedParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(selectedParent, "report.pdf"), []byte("selected content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDirectory, "report.pdf"), []byte("outside content"), 0o600); err != nil {
		t.Fatal(err)
	}
	selection, err := SelectHubRoot(rootDirectory)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := SnapshotHubFile(selection.Directory, selection.Identity, "nested/report.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(selectedParent, filepath.Join(rootDirectory, "old-nested")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDirectory, selectedParent); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if _, err := ReadHubFileChunk(selection.Directory, selection.Identity, "nested/report.pdf", snapshot.Version, 0, 32); err == nil {
		t.Fatal("ReadHubFileChunk followed a replaced parent symlink")
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
