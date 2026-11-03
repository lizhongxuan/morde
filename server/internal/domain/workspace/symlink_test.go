package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func symlinkFile(path, target string) File {
	digest := sha256.Sum256([]byte(target))
	return File{Kind: FileKindSymlink, SymlinkTarget: target, Path: path, SizeBytes: int64(len([]byte(target))),
		SHA256: hex.EncodeToString(digest[:]), Mode: 0, Chunks: []Chunk{}}
}

func TestWorkspaceSymlinkFileValidation(t *testing.T) {
	if CanonicalFileKind("") != FileKindRegular || CanonicalFileKind(FileKindSymlink) != FileKindSymlink {
		t.Fatal("CanonicalFileKind() did not normalize legacy regular files")
	}
	if err := ValidateFile(symlinkFile("src/current", "../README.md")); err != nil {
		t.Fatalf("ValidateFile() rejected safe relative symlink: %v", err)
	}
	for _, target := range []string{"", "/outside", "../../outside", "../tmp/../../workspace", "C:relative", "C:/outside", "name:stream", `dir\outside`, "bad\x00target", string([]byte{0xff})} {
		if err := ValidateFile(symlinkFile("src/current", target)); err == nil {
			t.Errorf("ValidateFile() accepted unsafe symlink target %q", target)
		}
	}
	if err := ValidateFile(symlinkFile("link", strings.Repeat("a", MaxSyncPathBytes+1))); err == nil {
		t.Fatal("ValidateFile() accepted an oversized symlink target")
	}
	badHash := symlinkFile("src/current", "README.md")
	badHash.SHA256 = strings.Repeat("0", 64)
	if err := ValidateFile(badHash); err == nil {
		t.Fatal("ValidateFile() accepted a target/hash mismatch")
	}
	badMode := symlinkFile("src/current", "README.md")
	badMode.Mode = 0777
	if err := ValidateFile(badMode); err == nil {
		t.Fatal("ValidateFile() accepted symlink permission bits")
	}
	badChunks := symlinkFile("src/current", "README.md")
	badChunks.Chunks = []Chunk{{SHA256: strings.Repeat("0", 64), SizeBytes: 1}}
	if err := ValidateFile(badChunks); err == nil {
		t.Fatal("ValidateFile() accepted symlink content chunks")
	}
	regularWithTarget := symlinkFile("src/current", "README.md")
	regularWithTarget.Kind = ""
	if err := ValidateFile(regularWithTarget); err == nil {
		t.Fatal("ValidateFile() accepted a symlink target on a regular file")
	}
}

func TestWorkspaceSymlinkManifestDigestIncludesKindAndTarget(t *testing.T) {
	link := symlinkFile("current", "README.md")
	first, err := ManifestSHA256([]File{link})
	if err != nil {
		t.Fatal(err)
	}
	link.SymlinkTarget = "LICENSE"
	digest := sha256.Sum256([]byte(link.SymlinkTarget))
	link.SizeBytes, link.SHA256 = int64(len(link.SymlinkTarget)), hex.EncodeToString(digest[:])
	second, err := ManifestSHA256([]File{link})
	if err != nil || first == second {
		t.Fatalf("manifest digest did not change with link target: %q, %q, %v", first, second, err)
	}

	regular := File{Path: link.Path, SizeBytes: link.SizeBytes, SHA256: link.SHA256, Mode: 0, Chunks: []Chunk{{SHA256: link.SHA256, SizeBytes: link.SizeBytes}}}
	regularDigest, err := ManifestSHA256([]File{regular})
	if err != nil || regularDigest == second {
		t.Fatalf("manifest digest did not distinguish regular file from symlink: %q, %q, %v", regularDigest, second, err)
	}
	streamed, err := ManifestSHA256Stream(func(yield func(File) error) error { return yield(link) })
	if err != nil || streamed != second {
		t.Fatalf("streamed symlink digest = %q, %v; want %q", streamed, err, second)
	}
}

func TestWorkspaceManifestRejectsEntriesBelowSymlinks(t *testing.T) {
	link := symlinkFile("cache", "README.md")
	emptyDigest := sha256.Sum256(nil)
	child := File{Path: "cache/item", SizeBytes: 0, SHA256: hex.EncodeToString(emptyDigest[:]), Mode: 0644, Chunks: []Chunk{}}
	if err := ValidateNoSymlinkDescendants([]File{link, child}, nil); err == nil {
		t.Fatal("accepted a file beneath a symlink")
	}
	if err := ValidateNoSymlinkDescendants([]File{link}, []string{"CACHE/empty"}); err == nil {
		t.Fatal("accepted a directory beneath a symlink")
	}
	if _, err := ManifestSHA256([]File{link, child}); err == nil {
		t.Fatal("manifest digest accepted a file beneath a symlink")
	}
	if _, err := ManifestTreeSHA256([]File{link}, []string{"cache/empty"}); err == nil {
		t.Fatal("tree digest accepted a directory beneath a symlink")
	}
	session := CreateSyncSession{Direction: SyncUpload, Policy: SyncCompareMerge, Files: []File{link, child}}
	if err := ValidateCreateSyncSession(session, 1024); err == nil {
		t.Fatal("sync session accepted a file beneath a symlink")
	}
}

func TestSameFileIncludesSymlinkIdentity(t *testing.T) {
	first := symlinkFile("link", "one")
	second := symlinkFile("link", "two")
	if sameFile(first, second) {
		t.Fatal("sameFile() treated different symlink targets as equal")
	}
	legacyRegular := File{Path: "notes", SizeBytes: 0, SHA256: strings.Repeat("0", 64), Mode: 0644, Chunks: []Chunk{}}
	explicitRegular := legacyRegular
	explicitRegular.Kind = FileKindRegular
	if !sameFile(legacyRegular, explicitRegular) {
		t.Fatal("sameFile() treated legacy and explicit regular-file kinds as different")
	}
}

func TestSameFileIncludesHardlinkIdentity(t *testing.T) {
	first := File{Path: "b.txt", HardlinkTo: "a.txt", SizeBytes: 0, SHA256: strings.Repeat("0", 64), Mode: 0644, Chunks: []Chunk{}}
	second := first
	second.HardlinkTo = ""
	if sameFile(first, second) {
		t.Fatal("sameFile() treated a hardlink alias and independent file as equal")
	}
}
