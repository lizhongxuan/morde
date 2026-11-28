package mordesync

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"
)

func TestTaskWorkspaceSnapshotSHA256IsCanonicalAndBindsWorkspaceSemantics(t *testing.T) {
	base := taskWorkspaceSnapshotFixture()
	first, err := TaskWorkspaceSnapshotSHA256(base)
	if err != nil {
		t.Fatal(err)
	}
	reordered := taskWorkspaceSnapshotFixture()
	slices.Reverse(reordered.Files)
	slices.Reverse(reordered.Directories)
	slices.Reverse(reordered.EmptyDirectories)
	reorderedDigest, err := TaskWorkspaceSnapshotSHA256(reordered)
	if err != nil || reorderedDigest != first {
		t.Fatalf("reordered equivalent manifest digest = %q, %v; want %q", reorderedDigest, err, first)
	}

	mutations := []struct {
		name string
		edit func(*Manifest)
	}{
		{name: "content", edit: func(manifest *Manifest) {
			manifest.Files[0].SHA256 = snapshotTestHash('a')
			manifest.Files[1].SHA256 = snapshotTestHash('a')
		}},
		{name: "mode", edit: func(manifest *Manifest) { manifest.Files[0].Mode = 0o600; manifest.Files[1].Mode = 0o600 }},
		{name: "mtime", edit: func(manifest *Manifest) {
			manifest.Files[0].ModTimeNS++
			manifest.Files[1].ModTimeNS++
		}},
		{name: "hardlink relation", edit: func(manifest *Manifest) { manifest.Files[1].HardlinkTo = "" }},
		{name: "symlink target", edit: func(manifest *Manifest) {
			target := "other.txt"
			manifest.TotalBytes += int64(len(target)) - manifest.Files[2].Size
			manifest.Files[2].SymlinkTarget = target
			manifest.Files[2].Size = int64(len(target))
			manifest.Files[2].SHA256 = snapshotTestSHA256([]byte(target))
		}},
		{name: "xattr", edit: func(manifest *Manifest) {
			manifest.Files[0].Xattrs[0].Value = []byte("changed")
			manifest.Files[1].Xattrs[0].Value = []byte("changed")
		}},
		{name: "acl", edit: func(manifest *Manifest) {
			manifest.Files[0].ACL.Data = []byte{2}
			manifest.Files[1].ACL.Data = []byte{2}
		}},
		{name: "directory metadata", edit: func(manifest *Manifest) { manifest.Directories[1].Mode = 0o700 }},
		{name: "empty directory", edit: func(manifest *Manifest) {
			manifest.EmptyDirectories[0] = "other-empty"
			manifest.Directories[2].Path = "other-empty"
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			manifest := taskWorkspaceSnapshotFixture()
			mutation.edit(&manifest)
			got, err := TaskWorkspaceSnapshotSHA256(manifest)
			if err != nil {
				t.Fatalf("mutated manifest was rejected: %v", err)
			}
			if got == first {
				t.Fatal("Workspace snapshot mutation did not change the common digest")
			}
		})
	}
}

func TestTaskWorkspaceSnapshotSHA256RequiresRootMetadata(t *testing.T) {
	manifest := Manifest{Files: []File{}, Directories: []Directory{}}
	if _, err := TaskWorkspaceSnapshotSHA256(manifest); err == nil {
		t.Fatal("snapshot digest accepted a manifest without root directory metadata")
	}
}

func taskWorkspaceSnapshotFixture() Manifest {
	contentHash := snapshotTestHash('c')
	block := Block{Offset: 0, Size: 1, SHA256: contentHash}
	file := File{
		Path: "a.txt", Kind: FileKindRegular, Size: 1, Mode: 0o640, ModTimeNS: 100, ModTimeKnown: true,
		MetadataKnown: true, Xattrs: []ExtendedAttribute{{Name: "user.snapshot", Value: []byte("value")}},
		ACL:    &NativeACL{Platform: "linux", Format: "linux-posix-acl-v1", Data: []byte{1}},
		SHA256: contentHash, Blocks: []Block{block},
	}
	linked := file
	linked.Path, linked.HardlinkTo = "b.txt", "a.txt"
	symlinkTarget := "a.txt"
	symlinkHash := snapshotTestSHA256([]byte(symlinkTarget))
	symlink := File{
		Path: "nested/link", Kind: FileKindSymlink, SymlinkTarget: symlinkTarget,
		Size: int64(len(symlinkTarget)), Mode: 0, ModTimeNS: 101, ModTimeKnown: true,
		MetadataKnown: true, SHA256: symlinkHash,
	}
	return Manifest{
		FileCount: 3, TotalBytes: 2 + int64(len(symlinkTarget)), Files: []File{file, linked, symlink},
		EmptyDirectories: []string{"empty"},
		Directories: []Directory{
			{Path: ".", Mode: 0o700, ModeKnown: true, ModTimeNS: 102, ModTimeKnown: true, MetadataKnown: true},
			{Path: "nested", Mode: 0o750, ModeKnown: true, ModTimeNS: 103, ModTimeKnown: true, MetadataKnown: true},
			{Path: "empty", Mode: 0o700, ModeKnown: true, ModTimeNS: 104, ModTimeKnown: true, MetadataKnown: true},
		},
	}
}

func snapshotTestHash(value byte) string {
	return snapshotTestSHA256([]byte{value})
}

func snapshotTestSHA256(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
