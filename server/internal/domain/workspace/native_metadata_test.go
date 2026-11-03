package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

func TestValidateNativeMetadataBoundsAndOrdering(t *testing.T) {
	valid := []ExtendedAttribute{{Name: "user.a", Value: []byte("one")}, {Name: "user.b", Value: []byte("two")}}
	if err := ValidateNativeMetadata(valid, &NativeACL{Platform: "linux", Format: "linux-posix-acl-v1", Data: []byte{1}}); err != nil {
		t.Fatalf("ValidateNativeMetadata() rejected valid metadata: %v", err)
	}
	for name, xattrs := range map[string][]ExtendedAttribute{
		"unsorted":           {{Name: "user.b"}, {Name: "user.a"}},
		"duplicate":          {{Name: "user.a"}, {Name: "user.a"}},
		"empty name":         {{Name: ""}},
		"NUL name":           {{Name: "user.bad\x00name"}},
		"invalid UTF-8 name": {{Name: string([]byte{0xff})}},
		"oversized name":     {{Name: strings.Repeat("n", MaxWorkspaceXattrNameBytes+1)}},
		"empty user name":    {{Name: "user."}},
		"security namespace": {{Name: "security.capability"}},
		"trusted namespace":  {{Name: "trusted.morde"}},
		"system namespace":   {{Name: "system.other"}},
		"oversized value":    {{Name: "user.a", Value: make([]byte, MaxWorkspaceXattrValueBytes+1)}},
		"too many": func() []ExtendedAttribute {
			attributes := make([]ExtendedAttribute, MaxWorkspaceXattrs+1)
			for index := range attributes {
				attributes[index].Name = fmt.Sprintf("user.%03d", index)
			}
			return attributes
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateNativeMetadata(xattrs, nil); err == nil {
				t.Fatal("accepted invalid extended attributes")
			}
		})
	}
	for _, acl := range []*NativeACL{
		{Platform: "linux", Format: "darwin-filesec-v1", Data: []byte{1}},
		{Platform: "darwin", Format: "darwin-filesec-v1"},
		{Platform: "windows", Format: "windows-acl-v1", Data: []byte{1}},
		{Platform: "darwin", Format: "darwin-filesec-v1", Data: make([]byte, MaxWorkspaceACLBytes+1)},
	} {
		if err := ValidateNativeMetadata(nil, acl); err == nil {
			t.Errorf("accepted invalid native ACL %#v", acl)
		}
	}
}

func TestNativeMetadataChangesManifestIdentity(t *testing.T) {
	emptyDigest := sha256.Sum256(nil)
	file := File{Path: "notes.txt", SizeBytes: 0, SHA256: hex.EncodeToString(emptyDigest[:]), Mode: 0600, Chunks: []Chunk{}}
	legacy, err := ManifestSHA256([]File{file})
	if err != nil {
		t.Fatal(err)
	}
	knownEmpty := file
	knownEmpty.MetadataKnown = true
	knownEmptyDigest, err := ManifestSHA256([]File{knownEmpty})
	if err != nil || knownEmptyDigest == legacy {
		t.Fatalf("known-empty metadata did not change manifest identity: legacy=%q known=%q err=%v", legacy, knownEmptyDigest, err)
	}
	file.MetadataKnown = true
	file.Xattrs = []ExtendedAttribute{{Name: "user.label", Value: []byte("private")}}
	withXattr, err := ManifestSHA256([]File{file})
	if err != nil || withXattr == legacy {
		t.Fatalf("file xattr did not change manifest identity: legacy=%q metadata=%q err=%v", legacy, withXattr, err)
	}
	file.Xattrs[0].Value = []byte("changed")
	changedXattr, err := ManifestSHA256([]File{file})
	if err != nil || changedXattr == withXattr {
		t.Fatalf("xattr value did not change manifest identity: first=%q second=%q err=%v", withXattr, changedXattr, err)
	}
	directory := Directory{Path: ".", Mode: uint32Pointer(0750), ModTimeNS: int64Pointer(1),
		MetadataKnown: true, ACL: &NativeACL{Platform: "darwin", Format: "darwin-filesec-v1", Data: []byte{1, 2}}}
	first, err := ManifestTreeWithMetadataSHA256([]File{file}, nil, []Directory{directory})
	if err != nil {
		t.Fatal(err)
	}
	directory.ACL.Data = []byte{1, 3}
	second, err := ManifestTreeWithMetadataSHA256([]File{file}, nil, []Directory{directory})
	if err != nil || first == second {
		t.Fatalf("directory ACL did not change manifest identity: first=%q second=%q err=%v", first, second, err)
	}
}

func TestWorkspaceRootDirectoryMetadataValidation(t *testing.T) {
	root := Directory{Path: ".", Mode: uint32Pointer(0750), ModTimeNS: int64Pointer(42),
		MetadataKnown: true, Xattrs: []ExtendedAttribute{{Name: "user.root", Value: []byte("kept")}}}
	if err := ValidateDirectory(root, true); err != nil {
		t.Fatalf("ValidateDirectory() rejected root: %v", err)
	}
	if err := ValidateCreateSyncSession(CreateSyncSession{Direction: SyncUpload, Policy: SyncLive, Directories: []Directory{root}}, 1<<20); err != nil {
		t.Fatalf("ValidateCreateSyncSession() rejected Workspace root metadata: %v", err)
	}
	if err := ValidateCreateSyncSession(CreateSyncSession{Direction: SyncUpload, Policy: SyncLive, Scope: []string{"src"}, Directories: []Directory{root}}, 1<<20); err == nil {
		t.Fatal("scoped sync accepted Workspace root metadata")
	}
}

func TestManifestNativeMetadataRequiresRootDirectory(t *testing.T) {
	emptyDigest := sha256.Sum256(nil)
	file := File{Path: "note.txt", SizeBytes: 0, SHA256: hex.EncodeToString(emptyDigest[:]), Chunks: []Chunk{}, MetadataKnown: true}
	if _, err := ManifestTreeWithMetadataSHA256([]File{file}, nil, nil); err == nil {
		t.Fatal("manifest with native file metadata accepted a missing root directory record")
	}
}

func TestSameFileIncludesExtendedMetadata(t *testing.T) {
	first := File{Path: "a", SizeBytes: 0, SHA256: strings.Repeat("0", 64), Mode: 0600, Chunks: []Chunk{},
		MetadataKnown: true, Xattrs: []ExtendedAttribute{{Name: "user.x", Value: []byte("one")}}}
	second := first
	second.Xattrs = []ExtendedAttribute{{Name: "user.x", Value: []byte("two")}}
	if sameFile(first, second) {
		t.Fatal("sameFile() ignored xattr value changes")
	}
}

func TestNativeMetadataMustBeExplicitlyKnown(t *testing.T) {
	file := File{Path: "a", SizeBytes: 0, SHA256: strings.Repeat("0", 64), Mode: 0600, Chunks: []Chunk{},
		Xattrs: []ExtendedAttribute{{Name: "user.x", Value: []byte("one")}}}
	if err := ValidateFile(file); err == nil {
		t.Fatal("accepted file attributes without the metadata-known flag")
	}
	directory := Directory{Path: ".", Mode: uint32Pointer(0700), ModTimeNS: int64Pointer(1),
		ACL: &NativeACL{Platform: "linux", Format: "linux-posix-acl-v1", Data: []byte{1}}}
	if err := ValidateDirectory(directory, true); err == nil {
		t.Fatal("accepted directory ACL without the metadata-known flag")
	}
}

func uint32Pointer(value uint32) *uint32 { return &value }
func int64Pointer(value int64) *int64    { return &value }
