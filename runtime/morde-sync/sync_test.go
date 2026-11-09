package mordesync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestScanCountsAndHashesFixedBlocks(t *testing.T) {
	root := t.TempDir()
	content := bytes.Repeat([]byte("morde"), BlockSize/5+1)
	if err := os.WriteFile(filepath.Join(root, "large.bin"), content, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var progress []ScanProgress
	manifest, err := Scan(context.Background(), root, func(value ScanProgress) { progress = append(progress, value) })
	if err != nil {
		t.Fatal(err)
	}
	if manifest.FileCount != 2 || manifest.TotalBytes != int64(len(content)) {
		t.Fatalf("manifest totals = files:%d bytes:%d", manifest.FileCount, manifest.TotalBytes)
	}
	var sawInventory, sawHash bool
	for _, value := range progress {
		if value.Phase == PhaseInventory && value.FilesFound == 2 && value.BytesFound == int64(len(content)) {
			sawInventory = true
		}
		if value.Phase == PhaseHash && value.FilesHashed == 2 && value.BytesHashed == int64(len(content)) {
			sawHash = true
		}
	}
	if !sawInventory || !sawHash {
		t.Fatalf("scan progress did not report folder totals and hashing completion: %+v", progress)
	}
	if len(manifest.Files) != 2 || manifest.Files[0].Path != "empty" || manifest.Files[1].Path != "large.bin" {
		t.Fatalf("manifest files are not stable and sorted: %#v", manifest.Files)
	}
	large := manifest.Files[1]
	if large.SHA256 != digest(content) || len(large.Blocks) != 2 || large.Blocks[0].Size != BlockSize || large.Blocks[1].Size != int64(len(content))-BlockSize {
		t.Fatalf("large file manifest = %#v", large)
	}
	if large.Blocks[0].SHA256 != digest(content[:BlockSize]) || large.Blocks[1].SHA256 != digest(content[BlockSize:]) {
		t.Fatal("block hashes do not match source bytes")
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("ValidateManifest() error = %v", err)
	}
}

func TestScanRecordsHardlinksWithStableCanonicalAnchor(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "z", "original.txt")
	alias := filepath.Join(root, "a", "deep", "alias.txt")
	if err := os.MkdirAll(filepath.Dir(original), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(alias), 0o700); err != nil {
		t.Fatal(err)
	}
	content := []byte("shared inode")
	if err := os.WriteFile(original, content, 0o640); err != nil {
		t.Fatal(err)
	}
	modified := time.Date(2024, 6, 7, 8, 9, 10, 123456789, time.UTC)
	if err := os.Chtimes(original, modified, modified); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(original, alias); err != nil {
		t.Skipf("hard links are unavailable: %v", err)
	}
	manifest, err := Scan(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 2 || manifest.Files[0].Path != "a/deep/alias.txt" || manifest.Files[0].HardlinkTo != "" ||
		manifest.Files[1].Path != "z/original.txt" || manifest.Files[1].HardlinkTo != manifest.Files[0].Path {
		t.Fatalf("scanned hard-link group = %#v", manifest.Files)
	}
	if first, second := manifest.Files[0], manifest.Files[1]; first.Size != second.Size || first.SHA256 != second.SHA256 ||
		first.Mode != second.Mode || first.ModTimeNS != second.ModTimeNS || first.ModTimeKnown != second.ModTimeKnown || !slices.Equal(first.Blocks, second.Blocks) {
		t.Fatalf("hard-linked manifest metadata differs: %#v / %#v", first, second)
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("ValidateManifest() error = %v", err)
	}
}

func TestScanAndApplyDetachesHardlinkPeerOutsideWorkspace(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	insidePath := filepath.Join(root, "inside.txt")
	content := []byte("shared inode")
	if err := os.WriteFile(insidePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(insidePath, filepath.Join(outside, "peer.txt")); err != nil {
		t.Skipf("hard links are unavailable: %v", err)
	}

	manifest, err := Scan(context.Background(), root, nil)
	if err != nil || len(manifest.Files) != 1 || manifest.Files[0].HardlinkTo != "" {
		t.Fatalf("Scan() manifest=%#v error=%v; external peers must not enter the manifest", manifest, err)
	}
	target := t.TempDir()
	result, err := ApplyMissing(context.Background(), target, manifest.Files[0], bytes.NewReader(content))
	if err != nil || result.Status != ApplyWritten {
		t.Fatalf("ApplyMissing() = %#v, %v", result, err)
	}
	sourceInfo, err := os.Stat(insidePath)
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Stat(filepath.Join(target, "inside.txt"))
	if err != nil || os.SameFile(sourceInfo, targetInfo) {
		t.Fatalf("copied source and destination share inode: %v / %v, %v", sourceInfo, targetInfo, err)
	}
	if err := os.WriteFile(filepath.Join(outside, "peer.txt"), []byte("changed outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "inside.txt")); err != nil || !bytes.Equal(got, content) {
		t.Fatalf("destination followed external peer mutation: %q, %v", got, err)
	}
}

func TestInDirectoryScopeWithoutScopeIncludesAll(t *testing.T) {
	if !InDirectoryScope("src", nil) || !InDirectoryScope("src", []string{}) {
		t.Fatal("an empty scope should include every directory")
	}
}

func TestScanAndApplyMissingPreserveModificationTime(t *testing.T) {
	source := t.TempDir()
	modified := time.Date(2024, time.June, 7, 8, 9, 10, 123456789, time.UTC)
	if err := os.WriteFile(filepath.Join(source, "notes.txt"), []byte("timestamped"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(source, "notes.txt"), modified, modified); err != nil {
		t.Fatal(err)
	}
	manifest, err := Scan(context.Background(), source, nil)
	if err != nil || len(manifest.Files) != 1 || !manifest.Files[0].ModTimeKnown || manifest.Files[0].ModTimeNS != modified.UnixNano() {
		t.Fatalf("scanned modification time = %#v, %v", manifest.Files, err)
	}
	target := t.TempDir()
	if result, err := ApplyMissing(context.Background(), target, manifest.Files[0], bytes.NewReader([]byte("timestamped"))); err != nil || result.Status != ApplyWritten {
		t.Fatalf("ApplyMissing() = %#v, %v", result, err)
	}
	info, err := os.Stat(filepath.Join(target, "notes.txt"))
	if err != nil || info.ModTime().UnixNano() != modified.UnixNano() {
		t.Fatalf("applied modification time = %v, %v; want %v", info, err, modified)
	}
}

func TestValidateManifestHardlinkGroups(t *testing.T) {
	content := []byte("shared bytes")
	anchor := makeFileForTest("a/anchor", content)
	peer := makeFileForTest("z/peer", content)
	anchor.Mode, peer.Mode = 0o640, 0o640
	anchor.ModTimeNS, peer.ModTimeNS = 1234, 1234
	anchor.ModTimeKnown, peer.ModTimeKnown = true, true
	peer.HardlinkTo = anchor.Path
	manifest := Manifest{FileCount: 2, TotalBytes: anchor.Size + peer.Size, Files: []File{anchor, peer}}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("valid hardlink group rejected: %v", err)
	}
	subset := Manifest{FileCount: 1, TotalBytes: peer.Size, Files: []File{peer}}
	if err := ValidateManifestSubset(subset); err != nil {
		t.Fatalf("filtered hardlink member rejected without its surrounding anchor: %v", err)
	}

	symlinkAnchor := File{Path: anchor.Path, Kind: FileKindSymlink, SymlinkTarget: "target", Size: int64(len("target")),
		SHA256: digest([]byte("target"))}
	invalid := []Manifest{
		{FileCount: 1, TotalBytes: peer.Size, Files: []File{peer}},
		{FileCount: 2, TotalBytes: symlinkAnchor.Size + peer.Size, Files: []File{symlinkAnchor, peer}},
	}
	badMetadata := peer
	badMetadata.Mode++
	invalid = append(invalid, Manifest{FileCount: 2, TotalBytes: anchor.Size + peer.Size, Files: []File{anchor, badMetadata}})
	badContent := peer
	badContent.SHA256 = digest([]byte("different"))
	invalid = append(invalid, Manifest{FileCount: 2, TotalBytes: anchor.Size + peer.Size, Files: []File{anchor, badContent}})
	selfLink := peer
	selfLink.HardlinkTo = selfLink.Path
	invalid = append(invalid, Manifest{FileCount: 2, TotalBytes: anchor.Size + peer.Size, Files: []File{anchor, selfLink}})
	for index, candidate := range invalid {
		if err := ValidateManifest(candidate); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("ValidateManifest(invalid group %d) = %v, want ErrInvalidManifest", index, err)
		}
	}
}

func TestApplyMissingPreservesHardlinkIdentityAndFailsClosed(t *testing.T) {
	content := []byte("hardlinked download")
	anchor := makeFileForTest("a/anchor", content)
	peer := makeFileForTest("z/peer", content)
	peer.HardlinkTo = anchor.Path
	target := t.TempDir()
	if result, err := ApplyMissing(context.Background(), target, anchor, bytes.NewReader(content)); err != nil || result.Status != ApplyWritten {
		t.Fatalf("apply anchor = %+v, %v", result, err)
	}
	if result, err := ApplyMissing(context.Background(), target, peer, nil); err != nil || result.Status != ApplyWritten {
		t.Fatalf("apply hardlink peer = %+v, %v", result, err)
	}
	anchorInfo, err := os.Stat(filepath.Join(target, anchor.Path))
	if err != nil {
		t.Fatal(err)
	}
	peerInfo, err := os.Stat(filepath.Join(target, peer.Path))
	if err != nil || !os.SameFile(anchorInfo, peerInfo) {
		t.Fatalf("applied group does not share an inode: %v / %v, %v", anchorInfo, peerInfo, err)
	}

	independent := t.TempDir()
	for _, file := range []File{anchor, peer} {
		fullPath := filepath.Join(independent, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, content, os.FileMode(file.Mode)); err != nil {
			t.Fatal(err)
		}
	}
	result, err := ApplyMissing(context.Background(), independent, peer, nil)
	if err != nil || result.Status != ApplyConflict {
		t.Fatalf("independent existing file was reported as present: %+v, %v", result, err)
	}
	if _, err := ApplyMissing(context.Background(), t.TempDir(), peer, nil); !errors.Is(err, ErrHardlinkAnchor) {
		t.Fatalf("missing hardlink anchor error = %v, want ErrHardlinkAnchor", err)
	}
}

func TestApplyMissingConcurrentHardlinkIsAtomic(t *testing.T) {
	content := []byte("same inode once")
	anchor := makeFileForTest("a/anchor", content)
	peer := makeFileForTest("z/peer", content)
	peer.HardlinkTo = anchor.Path
	target := t.TempDir()
	if result, err := ApplyMissing(context.Background(), target, anchor, bytes.NewReader(content)); err != nil || result.Status != ApplyWritten {
		t.Fatalf("apply anchor = %+v, %v", result, err)
	}
	start := make(chan struct{})
	results := make(chan ApplyResult, 2)
	errorsFound := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			result, err := ApplyMissing(context.Background(), target, peer, nil)
			results <- result
			errorsFound <- err
		}()
	}
	close(start)
	written, present := 0, 0
	for range 2 {
		result, err := <-results, <-errorsFound
		if err != nil {
			t.Fatalf("concurrent hardlink apply error: %v", err)
		}
		switch result.Status {
		case ApplyWritten:
			written++
		case ApplyPresent:
			present++
		default:
			t.Fatalf("concurrent hardlink apply status = %q", result.Status)
		}
	}
	if written != 1 || present != 1 {
		t.Fatalf("concurrent apply results = written:%d present:%d", written, present)
	}
	anchorInfo, err := os.Stat(filepath.Join(target, anchor.Path))
	if err != nil {
		t.Fatal(err)
	}
	peerInfo, err := os.Stat(filepath.Join(target, peer.Path))
	if err != nil || !os.SameFile(anchorInfo, peerInfo) {
		t.Fatalf("concurrent apply lost hardlink identity: %v / %v, %v", anchorInfo, peerInfo, err)
	}
}

func makeFileForTest(name string, content []byte) File {
	file := File{Path: name, Size: int64(len(content)), Mode: 0o644, SHA256: digest(content)}
	if len(content) > 0 {
		file.Blocks = []Block{{Offset: 0, Size: int64(len(content)), SHA256: file.SHA256}}
	}
	return file
}

func TestScanExcludesLocalRecoveryDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "workspace.txt"), []byte("synced"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovery := filepath.Join(root, reservedRecoveryDirectory, "wabc1234", "0123456789abcdef0123456789abcdef")
	if err := os.MkdirAll(recovery, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recovery, "content"), []byte("local-only recovery"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := Scan(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.FileCount != 1 || manifest.TotalBytes != int64(len("synced")) || len(manifest.Files) != 1 || manifest.Files[0].Path != "workspace.txt" {
		t.Fatalf("recovery directory entered sync manifest: %#v", manifest)
	}
}

func TestScanPersistsOnlyEmptyLeafDirectoriesAndCapsCombinedItems(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"nested/empty", "nested/with-file", "reserved-only", reservedRecoveryDirectory} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(name)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "with-file", "content"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "reserved-only", reservedTempPrefix+"staging"), []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, reservedRecoveryDirectory, "ignored"), []byte("preserved"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := Scan(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.FileCount != 1 || manifest.TotalBytes != int64(len("file")) || len(manifest.Files) != 1 {
		t.Fatalf("directory metadata changed file totals: %#v", manifest)
	}
	want := []string{"nested/empty", "reserved-only"}
	if !slices.Equal(manifest.EmptyDirectories, want) {
		t.Fatalf("empty directories = %#v; want %#v", manifest.EmptyDirectories, want)
	}
	gotDirectoryPaths := make([]string, len(manifest.Directories))
	for i, directory := range manifest.Directories {
		if !directory.ModeKnown || !directory.ModTimeKnown {
			t.Fatalf("scanned directory metadata is unknown: %#v", directory)
		}
		gotDirectoryPaths[i] = directory.Path
	}
	wantDirectoryPaths := []string{".", "nested", "nested/empty", "nested/with-file", "reserved-only"}
	if !slices.Equal(gotDirectoryPaths, wantDirectoryPaths) {
		t.Fatalf("scanned directories = %#v; want %#v", gotDirectoryPaths, wantDirectoryPaths)
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("ValidateManifest() error = %v", err)
	}

	// Seed the inventory with the maximum number of already discovered files.
	// This keeps the test small while exercising the same combined-item guard
	// used by Scan as soon as it discovers another empty directory.
	tooManyFiles := make([]scannedFile, MaxManifestEntries)
	if err := os.Mkdir(filepath.Join(root, "one-more"), 0o700); err != nil {
		t.Fatal(err)
	}
	cappedRoot, err := openDirectoryRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cappedRoot.Close()
	if err := inventory(context.Background(), cappedRoot, "", &tooManyFiles, new([]string), new([]scannedDirectory), &ScanProgress{}, func() {}); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("inventory over combined item cap error = %v", err)
	}
	for _, paths := range [][]string{{"tree/leaf", "tree"}, {"tree", "tree/leaf"}} {
		var validator ManifestPathValidator
		if err := validator.AddEmptyDirectory(paths[0]); err != nil {
			t.Fatalf("AddEmptyDirectory(%q) error = %v", paths[0], err)
		}
		if err := validator.AddEmptyDirectory(paths[1]); !errors.Is(err, ErrInvalidManifest) {
			t.Fatalf("AddEmptyDirectory(%q) after %q error = %v", paths[1], paths[0], err)
		}
	}
	target := t.TempDir()
	created, err := EnsureEmptyDirectory(context.Background(), target, "nested/leaf")
	if err != nil || created.Status != ApplyWritten {
		t.Fatalf("EnsureEmptyDirectory() = %+v, %v", created, err)
	}
	manifest, err = Scan(context.Background(), target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(manifest.EmptyDirectories, []string{"nested/leaf"}) {
		t.Fatalf("scanned directories = %#v", manifest.EmptyDirectories)
	}
	created, err = EnsureEmptyDirectory(context.Background(), target, "nested/leaf")
	if err != nil || created.Status != ApplyPresent {
		t.Fatalf("EnsureEmptyDirectory(existing) = %+v, %v", created, err)
	}
	if err := os.WriteFile(filepath.Join(target, "nested", "leaf", "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveEmptyDirectory(context.Background(), target, "nested/leaf")
	if err != nil || removed.Status != ApplyConflict {
		t.Fatalf("RemoveEmptyDirectory(non-empty) = %+v, %v", removed, err)
	}
	content, err := os.ReadFile(filepath.Join(target, "nested", "leaf", "keep.txt"))
	if err != nil || string(content) != "keep" {
		t.Fatalf("non-empty directory contents changed: %q, %v", content, err)
	}
	if err := os.Remove(filepath.Join(target, "nested", "leaf", "keep.txt")); err != nil {
		t.Fatal(err)
	}
	removed, err = RemoveEmptyDirectory(context.Background(), target, "nested/leaf")
	if err != nil || removed.Status != ApplyWritten {
		t.Fatalf("RemoveEmptyDirectory(empty) = %+v, %v", removed, err)
	}
	removed, err = RemoveEmptyDirectory(context.Background(), target, "nested/leaf")
	if err != nil || removed.Status != ApplyPresent {
		t.Fatalf("RemoveEmptyDirectory(missing) = %+v, %v", removed, err)
	}

	makeFile := func(name string) File {
		content := []byte(name)
		return File{Path: name, Size: int64(len(content)), SHA256: digest(content), Mode: 0o600,
			Blocks: []Block{{SHA256: digest(content), Offset: 0, Size: int64(len(content))}}}
	}
	cases := []Manifest{
		{EmptyDirectories: []string{"empty", "empty"}},
		{EmptyDirectories: []string{"Empty", "empty"}},
		{EmptyDirectories: []string{reservedRecoveryDirectory + "/user-data"}},
		{FileCount: 1, TotalBytes: int64(len("empty/child")), Files: []File{makeFile("empty/child")}, EmptyDirectories: []string{"empty"}},
		{FileCount: 1, TotalBytes: int64(len("empty")), Files: []File{makeFile("empty")}, EmptyDirectories: []string{"empty/child"}},
	}
	for index, manifest := range cases {
		if err := ValidateManifest(manifest); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("ValidateManifest(conflict %d) = %v", index, err)
		}
	}
}

func TestApplyDirectoryMetadataRestoresKnownAttributes(t *testing.T) {
	root := t.TempDir()
	directoryPath := filepath.Join(root, "src")
	if err := os.Mkdir(directoryPath, 0o750); err != nil {
		t.Fatal(err)
	}
	originalTime := time.Unix(1_700_000_000, 123_000_000)
	if err := os.Chtimes(directoryPath, originalTime, originalTime); err != nil {
		t.Fatal(err)
	}
	manifest, err := Scan(context.Background(), root, nil)
	if err != nil || len(manifest.Directories) != 2 {
		t.Fatalf("scan initial directory = %#v, %v", manifest.Directories, err)
	}
	var expected Directory
	for _, directory := range manifest.Directories {
		if directory.Path == "src" {
			expected = directory
		}
	}
	mode, modified := uint32(0o700), int64(1_700_000_010_987_654_321)
	desired := Directory{Path: "src", Mode: mode, ModeKnown: true, ModTimeNS: modified, ModTimeKnown: true}
	result, err := ApplyDirectoryMetadata(context.Background(), root, &expected, desired, true)
	if err != nil || result.Status != ApplyWritten {
		t.Fatalf("ApplyDirectoryMetadata() = %+v, %v", result, err)
	}
	info, err := os.Stat(directoryPath)
	if err != nil || info.Mode().Perm() != os.FileMode(mode) || info.ModTime().UnixNano() != modified {
		t.Fatalf("directory metadata = mode %v mtime %v, err=%v", info.Mode().Perm(), info.ModTime().UnixNano(), err)
	}
	actual, err := Scan(context.Background(), root, nil)
	if err != nil || len(actual.Directories) != 2 {
		t.Fatalf("rescan after directory metadata apply = %#v, %v", actual.Directories, err)
	}
	var actualDirectory *Directory
	for index := range actual.Directories {
		if actual.Directories[index].Path == "src" {
			actualDirectory = &actual.Directories[index]
		}
	}
	if actualDirectory == nil || !actualDirectory.ModeKnown || actualDirectory.Mode != mode ||
		!actualDirectory.ModTimeKnown || actualDirectory.ModTimeNS != modified {
		t.Fatalf("rescan after directory metadata apply = %#v, %v", actual.Directories, err)
	}
}

func TestApplyDirectoryMetadataAllowsUnknownAttributes(t *testing.T) {
	root := t.TempDir()
	desired := Directory{Path: "reports"}
	result, err := ApplyDirectoryMetadata(context.Background(), root, nil, desired, false)
	if err != nil || result.Status != ApplyWritten {
		t.Fatalf("ApplyDirectoryMetadata() = %+v, %v", result, err)
	}
	info, err := os.Stat(filepath.Join(root, desired.Path))
	if err != nil || !info.IsDir() {
		t.Fatalf("directory without metadata = %#v, %v", info, err)
	}
}

func TestScanAndApplySafeSymlinksWithoutFollowingTargets(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "target.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"link-file": "target.txt", "link-dir": "folder", "dangling": "not-created", "nested/back": "../target.txt"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(source, filepath.FromSlash(name))), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(source, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := Scan(context.Background(), source, nil)
	if err != nil {
		t.Fatalf("Scan() = %v", err)
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("ValidateManifest() = %v", err)
	}
	files := make(map[string]File, len(manifest.Files))
	wantBytes := int64(len("inside"))
	for _, file := range manifest.Files {
		files[file.Path] = file
		if file.Kind == FileKindSymlink {
			wantBytes += int64(len([]byte(file.SymlinkTarget)))
			if file.Size != int64(len([]byte(file.SymlinkTarget))) || file.SHA256 != digest([]byte(file.SymlinkTarget)) ||
				file.Mode != 0 || len(file.Blocks) != 0 || !file.ModTimeKnown {
				t.Fatalf("symlink manifest = %#v", file)
			}
		}
	}
	if manifest.FileCount != 5 || manifest.TotalBytes != wantBytes || files["dangling"].SymlinkTarget != "not-created" ||
		files["link-file"].SymlinkTarget != "target.txt" || files["link-dir"].SymlinkTarget != "folder" || files["nested/back"].SymlinkTarget != "../target.txt" {
		t.Fatalf("symlink manifest = %#v, totals want files=5 bytes=%d", manifest, wantBytes)
	}

	target := t.TempDir()
	for _, name := range []string{"dangling", "link-file", "link-dir", "nested/back"} {
		file := files[name]
		result, err := ApplyMissing(context.Background(), target, file, nil)
		if err != nil || result.Status != ApplyWritten {
			t.Fatalf("ApplyMissing(%q) = %#v, %v", name, result, err)
		}
		if got, err := os.Readlink(filepath.Join(target, filepath.FromSlash(name))); err != nil || got != file.SymlinkTarget {
			t.Fatalf("applied link %q = %q, %v; want %q", name, got, err, file.SymlinkTarget)
		}
		result, err = ApplyMissing(context.Background(), target, file, nil)
		if err != nil || result.Status != ApplyPresent {
			t.Fatalf("ApplyMissing(existing %q) = %#v, %v", name, result, err)
		}
	}
	actual, err := Scan(context.Background(), target, nil)
	if err != nil {
		t.Fatalf("Scan(applied links) = %v", err)
	}
	for _, name := range []string{"dangling", "link-file", "link-dir", "nested/back"} {
		if actualFile := manifestFile(t, actual, name); actualFile.Kind != FileKindSymlink || actualFile.SymlinkTarget != files[name].SymlinkTarget ||
			actualFile.ModTimeNS != files[name].ModTimeNS {
			t.Fatalf("applied link %q changed: got %#v want %#v", name, actualFile, files[name])
		}
	}
}

func TestScanRejectsSymlinksThatEscapeWorkspaceAndChangedSources(t *testing.T) {
	t.Run("escaping symlink is not followed", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("private sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		if _, err := Scan(context.Background(), root, nil); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("Scan(symlink) error = %v", err)
		}
		if got, err := os.ReadFile(outside); err != nil || string(got) != "private sentinel" {
			t.Fatalf("outside sentinel changed during rejected scan: %q, %v", got, err)
		}
	})

	t.Run("changed after inventory", func(t *testing.T) {
		root := t.TempDir()
		name := filepath.Join(root, "large.bin")
		if err := os.WriteFile(name, bytes.Repeat([]byte("x"), BlockSize+1), 0o600); err != nil {
			t.Fatal(err)
		}
		changed := false
		_, err := Scan(context.Background(), root, func(progress ScanProgress) {
			if !changed && progress.Phase == PhaseHash && progress.BytesHashed >= BlockSize {
				changed = true
				if writeErr := os.WriteFile(name, []byte("shortened"), 0o600); writeErr != nil {
					t.Errorf("change source: %v", writeErr)
				}
			}
		})
		if !changed || !errors.Is(err, ErrRescanRequired) {
			t.Fatalf("Scan() changed=%v error=%v", changed, err)
		}
	})

	t.Run("changed after a file was hashed", func(t *testing.T) {
		root := t.TempDir()
		name := filepath.Join(root, "a-first.txt")
		if err := os.WriteFile(name, []byte("before"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "z-large.bin"), bytes.Repeat([]byte("z"), BlockSize+1), 0o600); err != nil {
			t.Fatal(err)
		}
		changed := false
		_, err := Scan(context.Background(), root, func(progress ScanProgress) {
			if !changed && progress.Phase == PhaseHash && progress.FilesHashed == 1 {
				changed = true
				if writeErr := os.WriteFile(name, []byte("after!"), 0o600); writeErr != nil {
					t.Errorf("change already-hashed source: %v", writeErr)
				}
			}
		})
		if !changed || !errors.Is(err, ErrRescanRequired) {
			t.Fatalf("Scan() changed=%v error=%v", changed, err)
		}
	})
}

func TestValidateSymlinkTargetRejectsUnsafeTargets(t *testing.T) {
	for _, target := range []string{"", "../../outside", "/etc/passwd", `C:outside`, "name:stream", `dir\\file`, string([]byte{0xff})} {
		if err := ValidateSymlinkTarget("nested/link", target); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("ValidateSymlinkTarget(%q) = %v, want ErrUnsafePath", target, err)
		}
	}
	if err := ValidateSymlinkTarget("link", strings.Repeat("a", 4097)); !errors.Is(err, ErrUnsafePath) {
		t.Errorf("ValidateSymlinkTarget(oversized) = %v, want ErrUnsafePath", err)
	}
	for _, target := range []string{"inside", "../inside", "../../"} {
		if err := ValidateSymlinkTarget("nested/deep/link", target); err != nil {
			t.Errorf("ValidateSymlinkTarget(%q) = %v, want safe target", target, err)
		}
	}
}

func TestValidateManifestEnforcesSymlinkContract(t *testing.T) {
	target := "../content.txt"
	file := File{Path: "nested/link", Kind: FileKindSymlink, SymlinkTarget: target,
		Size: int64(len(target)), Mode: 0, SHA256: digest([]byte(target))}
	if err := ValidateManifest(Manifest{FileCount: 1, TotalBytes: file.Size, Files: []File{file}}); err != nil {
		t.Fatalf("valid symlink manifest: %v", err)
	}
	invalid := []File{file, file, file, file, file}
	invalid[0].Mode = 0o777
	invalid[1].Size++
	invalid[2].SHA256 = digest([]byte("different"))
	invalid[3].Blocks = []Block{{Offset: 0, Size: file.Size, SHA256: file.SHA256}}
	invalid[4].SymlinkTarget = "../../outside"
	for index, candidate := range invalid {
		if err := ValidateManifest(Manifest{FileCount: 1, TotalBytes: candidate.Size, Files: []File{candidate}}); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("ValidateManifest(symlink case %d) = %v, want ErrInvalidManifest", index, err)
		}
	}
	child := File{Path: "nested/link/child", Size: 0, Mode: 0o600, SHA256: digest(nil)}
	if err := ValidateManifest(Manifest{FileCount: 2, TotalBytes: file.Size, Files: []File{file, child}}); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("manifest accepted a child beneath a symlink: %v", err)
	}
}

func TestPlanAndApplyMissingNeverOverwrite(t *testing.T) {
	source := t.TempDir()
	files := map[string][]byte{
		"same.txt":    []byte("same"),
		"different":   []byte("from source"),
		"nested/new":  []byte("new content"),
		"zero-byte":   nil,
		"multi-block": bytes.Repeat([]byte("b"), BlockSize+9),
	}
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(source, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest, err := Scan(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.MkdirAll(filepath.Join(target, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "same.txt"), files["same.txt"], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "different"), []byte("keep local"), 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := PlanMissing(context.Background(), target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[string]ApplyStatus, len(plan.Entries))
	for _, entry := range plan.Entries {
		statuses[entry.Path] = entry.Status
	}
	if statuses["same.txt"] != ApplyPresent || statuses["different"] != ApplyConflict || statuses["nested/new"] != ApplyMissingStatus {
		t.Fatalf("plan statuses = %#v", statuses)
	}

	var newFile File
	var zeroFile File
	var multiFile File
	for _, file := range manifest.Files {
		switch file.Path {
		case "nested/new":
			newFile = file
		case "zero-byte":
			zeroFile = file
		case "multi-block":
			multiFile = file
		}
	}
	for _, file := range []File{newFile, zeroFile, multiFile} {
		result, err := ApplyMissing(context.Background(), target, file, bytes.NewReader(files[file.Path]))
		if err != nil || result.Status != ApplyWritten {
			t.Fatalf("ApplyMissing(%q) = %#v, %v", file.Path, result, err)
		}
		got, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(file.Path)))
		if err != nil || !bytes.Equal(got, files[file.Path]) {
			t.Fatalf("installed %q differs: err=%v", file.Path, err)
		}
	}
	result, err := ApplyMissing(context.Background(), target, manifestFile(t, manifest, "different"), bytes.NewReader(files["different"]))
	if err != nil || result.Status != ApplyConflict {
		t.Fatalf("ApplyMissing(existing difference) = %#v, %v", result, err)
	}
	got, err := os.ReadFile(filepath.Join(target, "different"))
	if err != nil || string(got) != "keep local" {
		t.Fatalf("existing file changed: %q, %v", got, err)
	}
	result, err = ApplyMissing(context.Background(), target, manifestFile(t, manifest, "same.txt"), strings.NewReader("unused"))
	if err != nil || result.Status != ApplyPresent {
		t.Fatalf("ApplyMissing(existing identical) = %#v, %v", result, err)
	}
}

func TestApplyMissingRejectsBadContentAndUnsafePaths(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "file.txt"), []byte("expected"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := Scan(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	file := manifest.Files[0]
	if _, err := ApplyMissing(context.Background(), target, file, strings.NewReader("tampered")); !errors.Is(err, ErrContentMismatch) {
		t.Fatalf("ApplyMissing(tampered) error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, file.Path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bad content left a destination: %v", err)
	}
	unsafe := file
	unsafe.Path = "../outside"
	if _, err := ApplyMissing(context.Background(), target, unsafe, strings.NewReader("expected")); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ApplyMissing(unsafe path) error = %v", err)
	}
	unsafe.Path = "."
	if err := ValidateManifest(Manifest{FileCount: 1, TotalBytes: unsafe.Size, Files: []File{unsafe}}); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ValidateManifest(root path) error = %v", err)
	}
}

func TestValidateManifestRejectsCaseFoldedPathCollisions(t *testing.T) {
	makeFile := func(name string) File {
		content := []byte(name)
		return File{Path: name, Size: int64(len(content)), SHA256: digest(content), Mode: 0o600,
			Blocks: []Block{{SHA256: digest(content), Offset: 0, Size: int64(len(content))}}}
	}
	files := []File{makeFile("Foo.txt"), makeFile("foo.txt")}
	manifest := Manifest{FileCount: int64(len(files)), TotalBytes: files[0].Size + files[1].Size, Files: files}
	if err := ValidateManifest(manifest); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ValidateManifest(case-folded collision) = %v", err)
	}
}

func TestValidateManifestRejectsCaseFoldedDirectoryCollisions(t *testing.T) {
	makeFile := func(name string) File {
		content := []byte(name)
		return File{Path: name, Size: int64(len(content)), SHA256: digest(content), Mode: 0o600,
			Blocks: []Block{{SHA256: digest(content), Offset: 0, Size: int64(len(content))}}}
	}
	files := []File{makeFile("Workspace/a.txt"), makeFile("workspace/b.txt")}
	manifest := Manifest{FileCount: int64(len(files)), TotalBytes: files[0].Size + files[1].Size, Files: files}
	if err := ValidateManifest(manifest); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("ValidateManifest(case-folded directory collision) = %v", err)
	}
}

func TestReadBlocksRevalidatesSource(t *testing.T) {
	root := t.TempDir()
	content := bytes.Repeat([]byte("block"), BlockSize/5+1)
	if err := os.WriteFile(filepath.Join(root, "code.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := Scan(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	file := manifest.Files[0]
	refs := make([]BlockRef, len(file.Blocks))
	for i := range refs {
		refs[i] = BlockRef{File: file, Index: i}
	}
	var seen atomic.Int32
	err = ReadBlocks(context.Background(), root, refs, func(_ File, _ int, block Block, data []byte) error {
		if sumHex(data) != block.SHA256 {
			return errors.New("block bytes differ")
		}
		seen.Add(1)
		return nil
	})
	if err != nil || int(seen.Load()) != len(refs) {
		t.Fatalf("ReadBlocks() seen=%d error=%v", seen.Load(), err)
	}
	content[0] ^= 1
	if err := os.WriteFile(filepath.Join(root, "code.bin"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReadBlocks(context.Background(), root, refs[:1], func(File, int, Block, []byte) error { return nil }); !errors.Is(err, ErrRescanRequired) {
		t.Fatalf("ReadBlocks(changed source) error = %v", err)
	}
}

func TestApplyMissingNoReplaceRace(t *testing.T) {
	source := t.TempDir()
	content := []byte("remote data")
	if err := os.WriteFile(filepath.Join(source, "file.txt"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := Scan(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	reader := &racingReader{target: filepath.Join(target, "file.txt"), content: content}
	result, err := ApplyMissing(context.Background(), target, manifest.Files[0], reader)
	if err != nil || result.Status != ApplyConflict {
		t.Fatalf("ApplyMissing(race) = %#v, %v", result, err)
	}
	got, err := os.ReadFile(reader.target)
	if err != nil || string(got) != "local winner" {
		t.Fatalf("race destination = %q, %v", got, err)
	}
}

type racingReader struct {
	target  string
	content []byte
	done    bool
}

func (r *racingReader) Read(buffer []byte) (int, error) {
	if !r.done {
		r.done = true
		if err := os.WriteFile(r.target, []byte("local winner"), 0o600); err != nil {
			return 0, err
		}
	}
	if len(r.content) == 0 {
		return 0, io.EOF
	}
	n := copy(buffer, r.content)
	r.content = r.content[n:]
	return n, nil
}

func manifestFile(t *testing.T, manifest Manifest, path string) File {
	t.Helper()
	for _, file := range manifest.Files {
		if file.Path == path {
			return file
		}
	}
	t.Fatalf("manifest missing file %q", path)
	return File{}
}

func digest(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func TestBufferAndConcurrencyContract(t *testing.T) {
	if BlockSize != 4<<20 || Parallelism != 4 {
		t.Fatalf("sync bounds = block:%d parallel:%d", BlockSize, Parallelism)
	}
}
