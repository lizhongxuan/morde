//go:build darwin || linux

package workspacefs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestCopyTreePreservesHiddenFilesDirectoriesAndSafeSymlinks(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, ".empty"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	const contents = "private workspace file"
	filePath := filepath.Join(source, ".hidden-file")
	if err := os.WriteFile(filePath, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filePath, 0640); err != nil {
		t.Fatal(err)
	}
	atime := time.Date(2024, time.March, 6, 7, 8, 9, 0, time.UTC)
	mtime := time.Date(2024, time.March, 7, 8, 9, 10, 0, time.UTC)
	if err := os.Chtimes(filePath, atime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../.hidden-file", filepath.Join(source, "nested", "safe-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(source, "nested"), atime, mtime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source, atime, mtime); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(source, "nested", "safe-link")
	if err := unix.UtimesNanoAt(unix.AT_FDCWD, linkPath, []unix.Timespec{
		unix.NsecToTimespec(atime.UnixNano()),
		unix.NsecToTimespec(mtime.UnixNano()),
	}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}

	if err := CopyTree(source, destination); err != nil {
		t.Fatalf("CopyTree() error = %v", err)
	}
	info, err := os.Stat(filepath.Join(destination, ".hidden-file"))
	if err != nil {
		t.Fatal(err)
	}
	gotAccessTime, ok := treeCopyAccessTime(info)
	if info.Mode().Perm() != 0640 || !info.ModTime().Equal(mtime) || (ok && !gotAccessTime.Equal(atime)) {
		t.Fatalf("copied mode/times = %v / %v / %v, want 0640 / %v / %v", info.Mode().Perm(), gotAccessTime, info.ModTime(), atime, mtime)
	}
	got, err := os.ReadFile(filepath.Join(destination, ".hidden-file"))
	if err != nil || string(got) != contents {
		t.Fatalf("copied hidden file = %q, %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(destination, ".empty")); err != nil || !info.IsDir() {
		t.Fatalf("copied empty directory = %#v, %v", info, err)
	}
	for _, name := range []string{".", "nested"} {
		copiedPath := filepath.Join(destination, name)
		info, err := os.Stat(copiedPath)
		if err != nil {
			t.Fatal(err)
		}
		gotAccessTime, ok := treeCopyAccessTime(info)
		if info.Mode().Perm() != 0750 || !info.ModTime().Equal(mtime) || (ok && !gotAccessTime.Equal(atime)) {
			t.Fatalf("copied directory %q mode/times = %v / %v / %v, want 0750 / %v / %v", name, info.Mode().Perm(), gotAccessTime, info.ModTime(), atime, mtime)
		}
	}
	if target, err := os.Readlink(filepath.Join(destination, "nested", "safe-link")); err != nil || target != "../.hidden-file" {
		t.Fatalf("copied symlink target = %q, %v", target, err)
	}
	linkInfo, err := os.Lstat(filepath.Join(destination, "nested", "safe-link"))
	if err != nil {
		t.Fatal(err)
	}
	linkAccessTime, ok := treeCopyAccessTime(linkInfo)
	if !linkInfo.ModTime().Equal(mtime) || (ok && !linkAccessTime.Equal(atime)) {
		t.Fatalf("copied symlink times = %v / %v, want %v / %v", linkAccessTime, linkInfo.ModTime(), atime, mtime)
	}
}

func TestCopyTreeRejectsAbsoluteAndEscapingSymlinksAndRollsBack(t *testing.T) {
	for _, test := range []struct {
		name   string
		target string
	}{
		{name: "absolute", target: filepath.Join(string(filepath.Separator), "outside")},
		{name: "escaping", target: "../../outside"},
		{name: "drive-relative", target: "C:outside"},
		{name: "colon", target: "name:stream"},
		{name: "backslash", target: `..\\outside`},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := t.TempDir()
			destination := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "a-first"), []byte("copy before failure"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(test.target, filepath.Join(source, "z-link")); err != nil {
				t.Fatal(err)
			}
			if err := CopyTree(source, destination); err == nil || !strings.Contains(err.Error(), "absolute or escaping symlink") {
				t.Fatalf("CopyTree() error = %v, want unsafe symlink rejection", err)
			}
			entries, err := os.ReadDir(destination)
			if err != nil || len(entries) != 0 {
				t.Fatalf("destination after rollback = %v, %v", entries, err)
			}
		})
	}
	for _, target := range []string{"", strings.Repeat("a", 4097), "bad\x00target", string([]byte{0xff})} {
		if err := validateTreeCopySymlink("link", target); err == nil {
			t.Fatalf("validateTreeCopySymlink(%q) unexpectedly accepted an invalid target", target)
		}
	}
}

func TestCopyTreePreservesExtendedAttributes(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	file := filepath.Join(source, "file")
	if err := os.WriteFile(file, []byte("metadata must not be lost"), 0600); err != nil {
		t.Fatal(err)
	}
	attribute := "user.workspace-copy-test"
	if err := unix.Setxattr(file, attribute, []byte("preserve or reject"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skipf("filesystem does not support extended attributes: %v", err)
		}
		t.Fatal(err)
	}
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	wantXattrs, wantACL, err := ReadTreeCopyMetadata(sourceRoot, source, "file", false)
	_ = sourceRoot.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateTreeCopy(context.Background(), source); err != nil {
		t.Fatalf("ValidateTreeCopy() error = %v", err)
	}
	if err := CopyTree(source, destination); err != nil {
		t.Fatalf("CopyTree() error = %v", err)
	}
	destinationRoot, err := os.OpenRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	gotXattrs, gotACL, err := ReadTreeCopyMetadata(destinationRoot, destination, "file", false)
	_ = destinationRoot.Close()
	if err != nil || !SameTreeCopyMetadata(wantXattrs, wantACL, gotXattrs, gotACL) {
		t.Fatalf("copied metadata = %#v / %#v, %v; want %#v / %#v", gotXattrs, gotACL, err, wantXattrs, wantACL)
	}
}

func TestCopyTreePreservesRootExtendedAttributes(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	attribute := "user.workspace-copy-test"
	if err := unix.Setxattr(source, attribute, []byte("root metadata must not be lost"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skipf("filesystem does not support extended attributes: %v", err)
		}
		t.Fatal(err)
	}
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	wantXattrs, wantACL, err := ReadTreeCopyMetadata(sourceRoot, source, ".", false)
	_ = sourceRoot.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(source, destination); err != nil {
		t.Fatalf("CopyTree() error = %v", err)
	}
	destinationRoot, err := os.OpenRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	gotXattrs, gotACL, err := ReadTreeCopyMetadata(destinationRoot, destination, ".", false)
	_ = destinationRoot.Close()
	if err != nil || !SameTreeCopyMetadata(wantXattrs, wantACL, gotXattrs, gotACL) {
		t.Fatalf("copied root metadata = %#v / %#v, %v; want %#v / %#v", gotXattrs, gotACL, err, wantXattrs, wantACL)
	}
}

func TestCopyTreeDoesNotFollowSymlinksForMetadata(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	target := filepath.Join(source, "target")
	if err := os.WriteFile(target, []byte("symlink target"), 0600); err != nil {
		t.Fatal(err)
	}
	attribute := "user.workspace-copy-nofollow-test"
	if err := unix.Setxattr(target, attribute, []byte("target-only"), 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skipf("filesystem does not support extended attributes: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	targetXattrs, _, err := ReadTreeCopyMetadata(sourceRoot, source, "target", false)
	if err != nil {
		_ = sourceRoot.Close()
		t.Fatal(err)
	}
	linkXattrs, linkACL, err := ReadTreeCopyMetadata(sourceRoot, source, "link", true)
	_ = sourceRoot.Close()
	if err != nil {
		t.Fatal(err)
	}
	foundTargetAttribute := false
	for _, value := range targetXattrs {
		foundTargetAttribute = foundTargetAttribute || value.Name == attribute
	}
	if !foundTargetAttribute {
		t.Fatalf("target attributes = %#v, want %q", targetXattrs, attribute)
	}
	for _, value := range linkXattrs {
		if value.Name == attribute {
			t.Fatalf("symlink metadata followed its target: %#v", linkXattrs)
		}
	}
	if err := CopyTree(source, destination); err != nil {
		t.Fatalf("CopyTree() error = %v", err)
	}
	destinationRoot, err := os.OpenRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	gotXattrs, gotACL, err := ReadTreeCopyMetadata(destinationRoot, destination, "link", true)
	_ = destinationRoot.Close()
	if err != nil || !SameTreeCopyMetadata(linkXattrs, linkACL, gotXattrs, gotACL) {
		t.Fatalf("copied symlink metadata = %#v / %#v, %v; want %#v / %#v", gotXattrs, gotACL, err, linkXattrs, linkACL)
	}
}

func TestCopyTreePreservesNativeACL(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native ACL test is macOS-only")
	}
	source := t.TempDir()
	destination := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a-first"), []byte("copy before failure"), 0600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(source, "z-acl")
	if err := os.WriteFile(file, []byte("ACL must not be lost"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("/bin/chmod", "+a", "everyone allow read", file).Run(); err != nil {
		t.Skipf("filesystem does not support setting a native ACL: %v", err)
	}
	sourceRoot, err := os.OpenRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	wantXattrs, wantACL, err := ReadTreeCopyMetadata(sourceRoot, source, "z-acl", false)
	_ = sourceRoot.Close()
	if err != nil {
		t.Fatal(err)
	}
	if wantACL == nil {
		t.Skip("filesystem did not expose the configured native ACL")
	}
	if err := ValidateTreeCopy(context.Background(), source); err != nil {
		t.Fatalf("ValidateTreeCopy() error = %v", err)
	}
	if err := CopyTree(source, destination); err != nil {
		t.Fatalf("CopyTree() error = %v", err)
	}
	destinationRoot, err := os.OpenRoot(destination)
	if err != nil {
		t.Fatal(err)
	}
	gotXattrs, gotACL, err := ReadTreeCopyMetadata(destinationRoot, destination, "z-acl", false)
	_ = destinationRoot.Close()
	if err != nil || !SameTreeCopyMetadata(wantXattrs, wantACL, gotXattrs, gotACL) {
		t.Fatalf("copied ACL metadata = %#v / %#v, %v; want %#v / %#v", gotXattrs, gotACL, err, wantXattrs, wantACL)
	}
}

func TestCopyTreePreservesHardLinksAndRejectsSpecialFiles(t *testing.T) {
	t.Run("hard links inside workspace", func(t *testing.T) {
		source := t.TempDir()
		destination := t.TempDir()
		first := filepath.Join(source, "a-file")
		second := filepath.Join(source, "z-link")
		if err := os.WriteFile(first, []byte("linked"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(first, second); err != nil {
			t.Fatal(err)
		}
		if err := ValidateTreeCopy(context.Background(), source); err != nil {
			t.Fatalf("ValidateTreeCopy() error = %v", err)
		}
		if err := CopyTree(source, destination); err != nil {
			t.Fatalf("CopyTree() error = %v", err)
		}
		copiedFirst, err := os.Stat(filepath.Join(destination, "a-file"))
		if err != nil {
			t.Fatal(err)
		}
		copiedSecond, err := os.Stat(filepath.Join(destination, "z-link"))
		if err != nil || !os.SameFile(copiedFirst, copiedSecond) {
			t.Fatalf("copied hard-link identity = %v / %v, %v; want same file", copiedFirst, copiedSecond, err)
		}
		sourceInfo, err := os.Stat(first)
		if err != nil || os.SameFile(sourceInfo, copiedFirst) {
			t.Fatalf("source and destination share inode: %v / %v, %v", sourceInfo, copiedFirst, err)
		}
		if contents, err := os.ReadFile(filepath.Join(destination, "z-link")); err != nil || string(contents) != "linked" {
			t.Fatalf("copied hard-link contents = %q, %v", contents, err)
		}
	})

	t.Run("external hard link becomes independent copy", func(t *testing.T) {
		parent := t.TempDir()
		source := filepath.Join(parent, "source")
		destination := filepath.Join(parent, "destination")
		if err := os.Mkdir(source, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(destination, 0700); err != nil {
			t.Fatal(err)
		}
		first := filepath.Join(source, "file")
		outside := filepath.Join(parent, "outside-link")
		if err := os.WriteFile(first, []byte("detached"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(first, outside); err != nil {
			t.Fatal(err)
		}
		if err := ValidateTreeCopy(context.Background(), source); err != nil {
			t.Fatalf("ValidateTreeCopy() error = %v", err)
		}
		if err := CopyTree(source, destination); err != nil {
			t.Fatalf("CopyTree() error = %v", err)
		}
		copied, err := os.Stat(filepath.Join(destination, "file"))
		outsideInfo, outsideErr := os.Stat(outside)
		if err != nil || outsideErr != nil || os.SameFile(copied, outsideInfo) {
			t.Fatalf("copied file remains linked outside workspace: %v / %v, %v, %v", copied, outsideInfo, err, outsideErr)
		}
	})

	t.Run("fifo", func(t *testing.T) {
		source := t.TempDir()
		destination := t.TempDir()
		if err := os.WriteFile(filepath.Join(source, "a-file"), []byte("copy before failure"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(source, "z-fifo"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := CopyTree(source, destination); err == nil || !strings.Contains(err.Error(), "does not support special file") {
			t.Fatalf("CopyTree() error = %v, want special-file rejection", err)
		}
		if entries, err := os.ReadDir(destination); err != nil || len(entries) != 0 {
			t.Fatalf("destination after rollback = %v, %v", entries, err)
		}
	})
}

func TestCopyTreeDropsPrivilegedPermissionBits(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	file := filepath.Join(source, "program")
	if err := os.WriteFile(file, []byte("local tool"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 04751); err != nil {
		t.Skipf("filesystem does not allow setting setuid mode: %v", err)
	}
	if info, err := os.Stat(file); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSetuid == 0 {
		t.Skip("filesystem did not retain the setuid bit")
	}
	if err := CopyTree(source, destination); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(destination, "program"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0751 || info.Mode()&os.ModeSetuid != 0 {
		t.Fatalf("copied mode = %v, want ordinary 0751 without setuid", info.Mode())
	}
}

func TestCopyTreeRequiresEmptyNonOverlappingDestination(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	marker := filepath.Join(destination, "keep")
	if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(source, destination); !errors.Is(err, ErrTreeCopyDestinationNotEmpty) {
		t.Fatalf("CopyTree(non-empty) error = %v", err)
	}
	if contents, err := os.ReadFile(marker); err != nil || string(contents) != "preserve" {
		t.Fatalf("non-empty destination marker = %q, %v", contents, err)
	}
	child := filepath.Join(source, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	if err := CopyTree(source, child); !errors.Is(err, ErrTreeCopyOverlappingRoots) {
		t.Fatalf("CopyTree(overlap) error = %v", err)
	}
}
