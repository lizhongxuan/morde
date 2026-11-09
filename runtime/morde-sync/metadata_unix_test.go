//go:build darwin || linux

package mordesync

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func testXattrName() string {
	return "user.morde-sync-test"
}

func TestScanCapturesExtendedAttributes(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "with-xattr")
	if err := os.WriteFile(file, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(file, testXattrName(), []byte("value"), 0); err != nil {
		t.Skipf("filesystem does not support setting extended attributes: %v", err)
	}
	manifest, err := Scan(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("Scan() = %v", err)
	}
	var scanned *File
	for index := range manifest.Files {
		if manifest.Files[index].Path == "with-xattr" {
			scanned = &manifest.Files[index]
		}
	}
	if scanned == nil || !scanned.MetadataKnown || len(scanned.Xattrs) != 1 || scanned.Xattrs[0].Name != testXattrName() || !bytes.Equal(scanned.Xattrs[0].Value, []byte("value")) {
		t.Fatalf("scanned file metadata = %#v", scanned)
	}
	if scanned.Xattrs == nil {
		t.Fatal("Scan() must preserve known-empty xattrs as a non-nil slice")
	}
	if err := ValidateManifest(manifest); err != nil {
		t.Fatalf("ValidateManifest() = %v", err)
	}
}

func TestScanCapturesDirectoryExtendedAttributes(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "with-xattr")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(directory, testXattrName(), []byte("directory"), 0); err != nil {
		t.Skipf("filesystem does not support setting directory extended attributes: %v", err)
	}
	manifest, err := Scan(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("Scan() = %v", err)
	}
	for _, scanned := range manifest.Directories {
		if scanned.Path == "with-xattr" {
			if !scanned.MetadataKnown || len(scanned.Xattrs) != 1 || scanned.Xattrs[0].Name != testXattrName() || !bytes.Equal(scanned.Xattrs[0].Value, []byte("directory")) {
				t.Fatalf("scanned directory metadata = %#v", scanned)
			}
			return
		}
	}
	t.Fatal("Scan() omitted directory metadata")
}

func TestScanCapturesSymlinkExtendedAttributes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink("target", link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if err := unix.Lsetxattr(link, testXattrName(), []byte("link"), 0); err != nil {
		t.Skipf("filesystem does not support setting symlink extended attributes: %v", err)
	}
	manifest, err := Scan(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("Scan() = %v", err)
	}
	for _, scanned := range manifest.Files {
		if scanned.Path == "link" {
			if !scanned.MetadataKnown || len(scanned.Xattrs) != 1 || scanned.Xattrs[0].Name != testXattrName() || !bytes.Equal(scanned.Xattrs[0].Value, []byte("link")) {
				t.Fatalf("scanned symlink metadata = %#v", scanned)
			}
			return
		}
	}
	t.Fatal("Scan() omitted symlink metadata")
}

func TestApplyMissingRefusesUnknownMetadataOnExistingFile(t *testing.T) {
	root := t.TempDir()
	content := []byte("content")
	filePath := filepath.Join(root, "with-xattr")
	if err := os.WriteFile(filePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(filePath, testXattrName(), []byte("value"), 0); err != nil {
		t.Skipf("filesystem does not support setting extended attributes: %v", err)
	}
	manifestFile := File{Path: "with-xattr", Size: int64(len(content)), Mode: 0o600, SHA256: digest(content),
		Blocks: []Block{{Offset: 0, Size: int64(len(content)), SHA256: digest(content)}}}
	result, err := ApplyMissing(context.Background(), root, manifestFile, bytes.NewReader(content))
	if err != nil || result.Status != ApplyConflict {
		t.Fatalf("ApplyMissing() = %+v, %v, want metadata conflict", result, err)
	}
}

func TestApplyMissingRestoresExtendedAttributes(t *testing.T) {
	root := t.TempDir()
	content := []byte("content")
	attribute := ExtendedAttribute{Name: testXattrName(), Value: []byte("restored")}
	manifestFile := File{Path: "restored", Size: int64(len(content)), Mode: 0o600, SHA256: digest(content), MetadataKnown: true,
		Xattrs: []ExtendedAttribute{attribute}, Blocks: []Block{{Offset: 0, Size: int64(len(content)), SHA256: digest(content)}}}
	result, err := ApplyMissing(context.Background(), root, manifestFile, bytes.NewReader(content))
	if err != nil || result.Status != ApplyWritten {
		t.Fatalf("ApplyMissing() = %+v, %v", result, err)
	}
	actual := make([]byte, len(attribute.Value))
	count, err := unix.Getxattr(filepath.Join(root, manifestFile.Path), attribute.Name, actual)
	if err != nil || count != len(attribute.Value) || !bytes.Equal(actual[:count], attribute.Value) {
		t.Fatalf("restored xattr = %q, %v", actual[:max(count, 0)], err)
	}
}

func TestApplyDirectoryMetadataRestoresExtendedAttributes(t *testing.T) {
	root := t.TempDir()
	directoryPath := filepath.Join(root, "restored")
	if err := os.Mkdir(directoryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	initial, err := Scan(context.Background(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	var expected Directory
	for _, directory := range initial.Directories {
		if directory.Path == "restored" {
			expected = directory
		}
	}
	attribute := ExtendedAttribute{Name: testXattrName(), Value: []byte("directory")}
	desired := Directory{Path: "restored", MetadataKnown: true, Xattrs: []ExtendedAttribute{attribute}}
	result, err := ApplyDirectoryMetadata(context.Background(), root, &expected, desired, true)
	if err != nil || result.Status != ApplyWritten {
		t.Fatalf("ApplyDirectoryMetadata() = %+v, %v", result, err)
	}
	actual := make([]byte, len(attribute.Value))
	count, err := unix.Getxattr(directoryPath, attribute.Name, actual)
	if err != nil || count != len(attribute.Value) || !bytes.Equal(actual[:count], attribute.Value) {
		t.Fatalf("restored directory xattr = %q, %v", actual[:max(count, 0)], err)
	}
}

func TestScanAndApplyLinuxPOSIXACL(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux POSIX ACL payloads are Linux-specific")
	}
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source")
	content := []byte("content")
	if err := os.WriteFile(sourcePath, content, 0o640); err != nil {
		t.Fatal(err)
	}
	aclData := linuxTestACL()
	if err := unix.Setxattr(sourcePath, "system.posix_acl_access", aclData, 0); err != nil {
		t.Skipf("filesystem does not support POSIX ACL xattrs: %v", err)
	}
	manifest, err := Scan(context.Background(), root, nil)
	if err != nil {
		t.Fatalf("Scan() = %v", err)
	}
	var source *File
	for index := range manifest.Files {
		if manifest.Files[index].Path == "source" {
			source = &manifest.Files[index]
		}
	}
	if source == nil || source.ACL == nil || source.ACL.Platform != "linux" || source.ACL.Format != "linux-posix-acl-v1" {
		t.Fatalf("scanned ACL = %#v", source)
	}
	var aclAttributes []ExtendedAttribute
	if err := json.Unmarshal(source.ACL.Data, &aclAttributes); err != nil || len(aclAttributes) != 1 || aclAttributes[0].Name != "system.posix_acl_access" || !bytes.Equal(aclAttributes[0].Value, aclData) {
		t.Fatalf("Linux ACL payload = %s, %v", source.ACL.Data, err)
	}
	source.Path = "copy"
	result, err := ApplyMissing(context.Background(), root, *source, bytes.NewReader(content))
	if err != nil || result.Status != ApplyWritten {
		t.Fatalf("ApplyMissing() = %+v, %v", result, err)
	}
	actual := make([]byte, len(aclData))
	count, err := unix.Getxattr(filepath.Join(root, source.Path), "system.posix_acl_access", actual)
	if err != nil || count != len(aclData) || !bytes.Equal(actual[:count], aclData) {
		t.Fatalf("restored POSIX ACL = %x, %v", actual[:max(count, 0)], err)
	}
}

func linuxTestACL() []byte {
	const (
		aclVersion = 2
		userObj    = 0x01
		user       = 0x02
		groupObj   = 0x04
		mask       = 0x10
		other      = 0x20
	)
	entries := []struct {
		tag  uint16
		perm uint16
		id   uint32
	}{
		{tag: userObj, perm: 6, id: ^uint32(0)},
		{tag: user, perm: 4, id: 12345},
		{tag: groupObj, perm: 4, id: ^uint32(0)},
		{tag: mask, perm: 4, id: ^uint32(0)},
		{tag: other, perm: 0, id: ^uint32(0)},
	}
	data := make([]byte, 4+len(entries)*8)
	binary.LittleEndian.PutUint32(data, aclVersion)
	for index, entry := range entries {
		offset := 4 + index*8
		binary.LittleEndian.PutUint16(data[offset:], entry.tag)
		binary.LittleEndian.PutUint16(data[offset+2:], entry.perm)
		binary.LittleEndian.PutUint32(data[offset+4:], entry.id)
	}
	return data
}

func TestMetadataLimitsAndUnknownMetadataValidation(t *testing.T) {
	content := []byte("x")
	file := File{Path: "x", Size: 1, Mode: 0o600, SHA256: digest(content), Blocks: []Block{{Offset: 0, Size: 1, SHA256: digest(content)}}}
	file.Xattrs = []ExtendedAttribute{{Name: "user.example", Value: []byte("x")}}
	if !errors.Is(ValidateFile(file), ErrInvalidManifest) {
		t.Fatal("unknown non-empty xattrs must be rejected")
	}
	file.MetadataKnown = true
	file.Xattrs = []ExtendedAttribute{{Name: "user.example", Value: make([]byte, maxWorkspaceXattrValueBytes+1)}}
	if !errors.Is(ValidateFile(file), ErrInvalidManifest) {
		t.Fatal("oversized xattr value must be rejected")
	}
	if err := validateMetadata([]ExtendedAttribute{{Name: "a", Value: []byte("1")}, {Name: "a", Value: []byte("2")}}, nil); !errors.Is(err, ErrInvalidManifest) {
		t.Fatalf("duplicate xattrs error = %v", err)
	}
	for _, name := range []string{"security.capability", "trusted.morde", "system.example", "system.posix_acl_access", "com.apple.quarantine", "com.morde.example", "user."} {
		if err := validateMetadata([]ExtendedAttribute{{Name: name, Value: []byte("x")}}, nil); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("validateMetadata(%q) = %v, want ErrInvalidManifest", name, err)
		}
	}
	if err := validateMetadata([]ExtendedAttribute{{Name: "user.example", Value: []byte("x")}}, nil); err != nil {
		t.Errorf("validateMetadata(user.example) = %v", err)
	}
}
