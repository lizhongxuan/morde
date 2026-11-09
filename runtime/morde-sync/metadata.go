package mordesync

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"unicode/utf8"
)

func displayMetadataPath(relative string) string {
	if relative == "" {
		return "."
	}
	return relative
}

const (
	maxWorkspaceXattrs          = 64
	maxWorkspaceXattrNameBytes  = 255
	maxWorkspaceXattrValueBytes = 16 << 10
	maxWorkspaceACLBytes        = 32 << 10
	maxWorkspaceMetadataBytes   = 64 << 10
)

func captureRegularFileMetadata(root *os.Root, name string, expected os.FileInfo, relative string) ([]ExtendedAttribute, *NativeACL, error) {
	opened, err := root.Open(name)
	if err != nil {
		return nil, nil, fmt.Errorf("open %q metadata: %w", relative, ErrRescanRequired)
	}
	defer opened.Close()
	info, err := opened.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(expected, info) {
		return nil, nil, errors.Join(fmt.Errorf("file %q changed while inspecting metadata: %w", relative, ErrRescanRequired), err)
	}
	xattrs, acl, err := captureMetadataFD(opened, relative)
	if err != nil {
		return nil, nil, err
	}
	latest, err := root.Lstat(name)
	if err != nil || !latest.Mode().IsRegular() || !os.SameFile(info, latest) {
		return nil, nil, errors.Join(fmt.Errorf("file %q changed while inspecting metadata: %w", relative, ErrRescanRequired), err)
	}
	return xattrs, acl, nil
}

func captureSymlinkMetadata(root *os.Root, name, relative string) ([]ExtendedAttribute, *NativeACL, error) {
	parent, err := root.Open(".")
	if err != nil {
		return nil, nil, fmt.Errorf("open parent for %q metadata: %w", relative, ErrRescanRequired)
	}
	defer parent.Close()
	return captureSymlinkMetadataAt(parent, name, relative)
}

func captureSymlinkMetadataPath(root *os.Root, relative, displayName string) ([]ExtendedAttribute, *NativeACL, error) {
	parent, name, closeParent, err := openParent(root, relative, false)
	if err != nil {
		return nil, nil, err
	}
	defer closeParent()
	opened, err := parent.Open(".")
	if err != nil {
		return nil, nil, fmt.Errorf("open parent for symlink %q metadata: %w", displayName, ErrRescanRequired)
	}
	defer opened.Close()
	return captureSymlinkMetadataAt(opened, name, displayName)
}

func checkUnsupportedMetadataFD(file *os.File, name string) error {
	_, _, err := captureMetadataFD(file, name)
	return err
}

func checkSymlinkMetadata(root *os.Root, name, relative string) error {
	_, _, err := captureSymlinkMetadata(root, name, relative)
	return err
}

func applySymlinkMetadataPath(root *os.Root, file File) error {
	parent, name, closeParent, err := openParent(root, file.Path, false)
	if err != nil {
		return err
	}
	defer closeParent()
	opened, err := parent.Open(".")
	if err != nil {
		return err
	}
	defer opened.Close()
	return applySymlinkMetadataAt(opened, name, file.Path, file.Xattrs, file.ACL, file.MetadataKnown)
}

func checkUnsupportedSymlinkMetadata(parent *os.File, name, relative string) error {
	_, _, err := captureSymlinkMetadataAt(parent, name, relative)
	return err
}

func checkUnsupportedMetadataParents(root *os.Root, relative string) error {
	current := root
	var owned *os.Root
	closeOwned := func() {
		if owned != nil {
			_ = owned.Close()
			owned = nil
		}
	}
	defer closeOwned()

	checkDirectory := func(directory *os.Root, label string) error {
		opened, err := directory.Open(".")
		if err != nil {
			return fmt.Errorf("open directory %q metadata: %w", label, ErrDestinationChange)
		}
		defer opened.Close()
		_, _, err = captureMetadataFD(opened, label)
		return err
	}
	if err := checkDirectory(current, "."); err != nil {
		return err
	}
	parts := strings.Split(relative, "/")
	for _, part := range parts[:len(parts)-1] {
		info, err := current.Lstat(part)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil
		}
		child, err := current.OpenRoot(part)
		if err != nil {
			return errors.Join(ErrDestinationChange, err)
		}
		openedInfo, statErr := child.Stat(".")
		if statErr != nil || !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
			_ = child.Close()
			return errors.Join(ErrDestinationChange, statErr)
		}
		if err := checkDirectory(child, strings.Join(parts[:len(parts)-1], "/")); err != nil {
			_ = child.Close()
			return err
		}
		closeOwned()
		owned = child
		current = child
	}
	return nil
}

func validateMetadata(xattrs []ExtendedAttribute, acl *NativeACL) error {
	if len(xattrs) > maxWorkspaceXattrs {
		return ErrInvalidManifest
	}
	total := 0
	previous := ""
	for _, xattr := range xattrs {
		if len(xattr.Name) == 0 || len(xattr.Name) > maxWorkspaceXattrNameBytes || !utf8.ValidString(xattr.Name) ||
			strings.IndexByte(xattr.Name, 0) >= 0 || xattr.Name == "com.apple.provenance" ||
			!strings.HasPrefix(xattr.Name, "user.") || len(xattr.Name) == len("user.") ||
			previous != "" && xattr.Name <= previous || len(xattr.Value) > maxWorkspaceXattrValueBytes {
			return ErrInvalidManifest
		}
		previous = xattr.Name
		total += len(xattr.Name) + len(xattr.Value)
	}
	if acl != nil {
		valid := acl.Platform == "linux" && acl.Format == "linux-posix-acl-v1" ||
			acl.Platform == "darwin" && acl.Format == "darwin-filesec-v1"
		if !valid || len(acl.Data) == 0 || len(acl.Data) > maxWorkspaceACLBytes {
			return ErrInvalidManifest
		}
		total += len(acl.Platform) + len(acl.Format) + len(acl.Data)
	}
	if total > maxWorkspaceMetadataBytes {
		return ErrInvalidManifest
	}
	return nil
}

func sameMetadata(aXattrs []ExtendedAttribute, aACL *NativeACL, bXattrs []ExtendedAttribute, bACL *NativeACL) bool {
	if len(aXattrs) != len(bXattrs) || (aACL == nil) != (bACL == nil) {
		return false
	}
	for i := range aXattrs {
		if aXattrs[i].Name != bXattrs[i].Name || !bytes.Equal(aXattrs[i].Value, bXattrs[i].Value) {
			return false
		}
	}
	return aACL == nil || aACL.Platform == bACL.Platform && aACL.Format == bACL.Format && bytes.Equal(aACL.Data, bACL.Data)
}

func metadataMatchesFD(file *os.File, name string, xattrs []ExtendedAttribute, acl *NativeACL, known bool) (bool, error) {
	actualXattrs, actualACL, err := captureMetadataFD(file, name)
	if err != nil {
		return false, err
	}
	if !known {
		return len(actualXattrs) == 0 && actualACL == nil, nil
	}
	return sameMetadata(xattrs, acl, actualXattrs, actualACL), nil
}

func metadataMatchesSymlink(parent *os.File, name, relative string, xattrs []ExtendedAttribute, acl *NativeACL, known bool) (bool, error) {
	actualXattrs, actualACL, err := captureSymlinkMetadataAt(parent, name, relative)
	if err != nil {
		return false, err
	}
	if !known {
		return len(actualXattrs) == 0 && actualACL == nil, nil
	}
	return sameMetadata(xattrs, acl, actualXattrs, actualACL), nil
}

func metadataMatchesSymlinkRoot(parent *os.Root, name, relative string, xattrs []ExtendedAttribute, acl *NativeACL, known bool) (bool, error) {
	opened, err := parent.Open(".")
	if err != nil {
		return false, fmt.Errorf("open parent for symlink %q metadata: %w", relative, ErrDestinationChange)
	}
	defer opened.Close()
	return metadataMatchesSymlink(opened, name, relative, xattrs, acl, known)
}
