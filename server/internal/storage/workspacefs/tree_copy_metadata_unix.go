//go:build darwin || linux

package workspacefs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"

	"morde/server/internal/domain/workspace"
)

// ReadTreeCopyMetadata reads only attributes of the named entry. Symlink
// attributes are always queried without following the link.
func ReadTreeCopyMetadata(root *os.Root, rootPath, name string, symlink bool) ([]workspace.ExtendedAttribute, *workspace.NativeACL, error) {
	if root == nil || !filepath.IsAbs(rootPath) || name != "." && workspace.ValidateRelativePath(name) != nil {
		return nil, nil, errors.New("invalid workspace metadata path")
	}
	var file *os.File
	if !symlink {
		var err error
		file, err = root.Open(name)
		if err != nil {
			return nil, nil, fmt.Errorf("open workspace metadata entry %q: %w", name, err)
		}
		defer func() { _ = file.Close() }()
	}
	fullPath := filepath.Join(rootPath, filepath.FromSlash(name))
	xattrs, err := readTreeCopyXattrs(file, fullPath, symlink)
	if err != nil {
		return nil, nil, fmt.Errorf("read workspace extended attributes %q: %w", name, err)
	}
	acl, err := readTreeCopyNativeACL(file, fullPath, name, symlink)
	if err != nil {
		return nil, nil, err
	}
	if err := workspace.ValidateNativeMetadata(xattrs, acl); err != nil {
		return nil, nil, fmt.Errorf("workspace metadata %q: %w", name, err)
	}
	return xattrs, acl, nil
}

// ApplyTreeCopyMetadata sets and reads back entry metadata before reporting
// success. The caller creates entries inside an unpublished staging tree.
func ApplyTreeCopyMetadata(root *os.Root, rootPath, name string, symlink bool,
	xattrs []workspace.ExtendedAttribute, acl *workspace.NativeACL) error {
	if root == nil || !filepath.IsAbs(rootPath) || name != "." && workspace.ValidateRelativePath(name) != nil {
		return errors.New("invalid workspace metadata path")
	}
	if err := workspace.ValidateNativeMetadata(xattrs, acl); err != nil {
		return err
	}
	var file *os.File
	if !symlink {
		var err error
		file, err = root.Open(name)
		if err != nil {
			return fmt.Errorf("open workspace metadata destination %q: %w", name, err)
		}
		defer func() { _ = file.Close() }()
		return ApplyOpenedTreeCopyMetadata(file, filepath.Join(rootPath, filepath.FromSlash(name)), name, xattrs, acl)
	}
	fullPath := filepath.Join(rootPath, filepath.FromSlash(name))
	if err := applyTreeCopyXattrs(file, fullPath, symlink, xattrs); err != nil {
		return fmt.Errorf("apply workspace extended attributes %q: %w", name, err)
	}
	if err := applyTreeCopyNativeACL(file, fullPath, name, symlink, acl); err != nil {
		return err
	}
	actualXattrs, actualACL, err := ReadTreeCopyMetadata(root, rootPath, name, symlink)
	if err != nil {
		return err
	}
	if !SameTreeCopyMetadata(xattrs, acl, actualXattrs, actualACL) {
		return fmt.Errorf("workspace metadata read-back mismatch for %q", name)
	}
	return nil
}

// ApplyOpenedTreeCopyMetadata applies attributes through a stable open handle,
// including entries whose final permission bits would prevent reopening.
func ApplyOpenedTreeCopyMetadata(file *os.File, path, name string,
	xattrs []workspace.ExtendedAttribute, acl *workspace.NativeACL) error {
	if file == nil || !filepath.IsAbs(path) || name != "." && workspace.ValidateRelativePath(name) != nil {
		return errors.New("invalid open workspace metadata destination")
	}
	if err := workspace.ValidateNativeMetadata(xattrs, acl); err != nil {
		return err
	}
	if err := applyTreeCopyXattrs(file, path, false, xattrs); err != nil {
		return fmt.Errorf("apply workspace extended attributes %q: %w", name, err)
	}
	if err := applyTreeCopyNativeACL(file, path, name, false, acl); err != nil {
		return err
	}
	return VerifyOpenedTreeCopyMetadata(file, path, name, xattrs, acl)
}

// VerifyOpenedTreeCopyMetadata checks metadata through the same stable handle
// after callers apply the final mode bits.
func VerifyOpenedTreeCopyMetadata(file *os.File, path, name string,
	xattrs []workspace.ExtendedAttribute, acl *workspace.NativeACL) error {
	if file == nil || !filepath.IsAbs(path) || name != "." && workspace.ValidateRelativePath(name) != nil {
		return errors.New("invalid open workspace metadata destination")
	}
	actualXattrs, err := readTreeCopyXattrs(file, path, false)
	if err != nil {
		return err
	}
	actualACL, err := readTreeCopyNativeACL(file, path, name, false)
	if err != nil {
		return err
	}
	if !SameTreeCopyMetadata(xattrs, acl, actualXattrs, actualACL) {
		return fmt.Errorf("workspace metadata read-back mismatch for %q", name)
	}
	return nil
}

func SameTreeCopyMetadata(first []workspace.ExtendedAttribute, firstACL *workspace.NativeACL,
	second []workspace.ExtendedAttribute, secondACL *workspace.NativeACL) bool {
	if len(first) != len(second) || (firstACL == nil) != (secondACL == nil) {
		return false
	}
	for index := range first {
		if first[index].Name != second[index].Name || !bytes.Equal(first[index].Value, second[index].Value) {
			return false
		}
	}
	return firstACL == nil || firstACL.Platform == secondACL.Platform && firstACL.Format == secondACL.Format && bytes.Equal(firstACL.Data, secondACL.Data)
}

func readTreeCopyXattrs(file *os.File, path string, symlink bool) ([]workspace.ExtendedAttribute, error) {
	names, err := listTreeCopyXattrNames(file, path, symlink)
	if err != nil {
		return nil, err
	}
	attributes := make([]workspace.ExtendedAttribute, 0, len(names))
	for _, name := range names {
		if treeCopyIgnorableExtendedAttribute(name) || treeCopyNativeACLXattr(name) {
			continue
		}
		value, err := getTreeCopyXattr(file, path, name, symlink)
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", name, err)
		}
		attributes = append(attributes, workspace.ExtendedAttribute{Name: name, Value: value})
	}
	sort.Slice(attributes, func(i, j int) bool { return attributes[i].Name < attributes[j].Name })
	return attributes, nil
}

func applyTreeCopyXattrs(file *os.File, path string, symlink bool, wanted []workspace.ExtendedAttribute) error {
	wantedByName := make(map[string][]byte, len(wanted))
	for _, attribute := range wanted {
		if treeCopyNativeACLXattr(attribute.Name) {
			return fmt.Errorf("ACL xattr %q must use the native ACL field", attribute.Name)
		}
		wantedByName[attribute.Name] = attribute.Value
	}
	existing, err := listTreeCopyXattrNames(file, path, symlink)
	if err != nil {
		return err
	}
	for _, name := range existing {
		if treeCopyIgnorableExtendedAttribute(name) || treeCopyNativeACLXattr(name) {
			continue
		}
		if _, ok := wantedByName[name]; !ok {
			if err := removeTreeCopyXattr(file, path, name, symlink); err != nil {
				return fmt.Errorf("remove inherited attribute %q: %w", name, err)
			}
		}
	}
	for _, attribute := range wanted {
		if err := setTreeCopyXattr(file, path, attribute.Name, attribute.Value, symlink); err != nil {
			return fmt.Errorf("set %q: %w", attribute.Name, err)
		}
	}
	return nil
}

func setTreeCopySymlinkTimes(root, name string, sourceInfo os.FileInfo) error {
	atime, ok := treeCopyAccessTime(sourceInfo)
	if !ok {
		atime = sourceInfo.ModTime()
	}
	path := filepath.Join(root, filepath.FromSlash(name))
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{
		unix.NsecToTimespec(atime.UnixNano()),
		unix.NsecToTimespec(sourceInfo.ModTime().UnixNano()),
	}, unix.AT_SYMLINK_NOFOLLOW)
}
