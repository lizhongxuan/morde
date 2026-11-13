//go:build linux

package workspacefs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"golang.org/x/sys/unix"

	"morde/server/internal/domain/workspace"
)

const (
	linuxACLAccess  = "system.posix_acl_access"
	linuxACLDefault = "system.posix_acl_default"
)

func listTreeCopyXattrNames(file *os.File, path string, symlink bool) ([]string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		size, err := treeCopyListXattrSize(file, path, symlink)
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return []string{}, nil
		}
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return []string{}, nil
		}
		attributes := make([]byte, size)
		count, err := treeCopyListXattrs(file, path, symlink, attributes)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if count < 0 || count > len(attributes) {
			return nil, errors.New("extended attribute list is incomplete")
		}
		names := make([]string, 0, 8)
		for remaining := attributes[:count]; len(remaining) > 0; {
			end := bytes.IndexByte(remaining, 0)
			if end <= 0 {
				return nil, errors.New("extended attribute list is malformed")
			}
			names = append(names, string(remaining[:end]))
			remaining = remaining[end+1:]
		}
		sort.Strings(names)
		return names, nil
	}
	return nil, errors.New("extended attributes changed while inspecting")
}

func treeCopyListXattrSize(file *os.File, path string, symlink bool) (int, error) {
	if symlink {
		return unix.Llistxattr(path, nil)
	}
	return unix.Flistxattr(int(file.Fd()), nil)
}

func treeCopyListXattrs(file *os.File, path string, symlink bool, buffer []byte) (int, error) {
	if symlink {
		return unix.Llistxattr(path, buffer)
	}
	return unix.Flistxattr(int(file.Fd()), buffer)
}

func getTreeCopyXattr(file *os.File, path, name string, symlink bool) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		size, err := treeCopyGetXattrSize(file, path, name, symlink)
		if err != nil {
			return nil, err
		}
		if size < 0 || size > workspace.MaxWorkspaceXattrValueBytes {
			return nil, errors.New("extended attribute value exceeds limit")
		}
		value := make([]byte, size)
		count, err := treeCopyGetXattr(file, path, name, symlink, value)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if count < 0 || count > len(value) {
			return nil, errors.New("extended attribute value is incomplete")
		}
		return value[:count], nil
	}
	return nil, errors.New("extended attribute changed while inspecting")
}

func treeCopyGetXattrSize(file *os.File, path, name string, symlink bool) (int, error) {
	if symlink {
		return unix.Lgetxattr(path, name, nil)
	}
	return unix.Fgetxattr(int(file.Fd()), name, nil)
}

func treeCopyGetXattr(file *os.File, path, name string, symlink bool, value []byte) (int, error) {
	if symlink {
		return unix.Lgetxattr(path, name, value)
	}
	return unix.Fgetxattr(int(file.Fd()), name, value)
}

func setTreeCopyXattr(file *os.File, path, name string, value []byte, symlink bool) error {
	if symlink {
		return unix.Lsetxattr(path, name, value, 0)
	}
	return unix.Fsetxattr(int(file.Fd()), name, value, 0)
}

func removeTreeCopyXattr(file *os.File, path, name string, symlink bool) error {
	var err error
	if symlink {
		err = unix.Lremovexattr(path, name)
	} else {
		err = unix.Fremovexattr(int(file.Fd()), name)
	}
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
		return nil
	}
	return err
}

func treeCopyNativeACLXattr(name string) bool {
	return name == linuxACLAccess || name == linuxACLDefault
}

func readTreeCopyNativeACL(file *os.File, _ string, _ string, symlink bool) (*workspace.NativeACL, error) {
	if symlink {
		return nil, nil
	}
	attributes := make([]workspace.ExtendedAttribute, 0, 2)
	for _, name := range []string{linuxACLAccess, linuxACLDefault} {
		value, err := getTreeCopyXattr(file, "", name, false)
		if errors.Is(err, unix.ENODATA) {
			continue
		}
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("inspect POSIX ACL %q: %w", name, err)
		}
		attributes = append(attributes, workspace.ExtendedAttribute{Name: name, Value: value})
	}
	if len(attributes) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(attributes)
	if err != nil {
		return nil, err
	}
	return &workspace.NativeACL{Platform: "linux", Format: "linux-posix-acl-v1", Data: data}, nil
}

func applyTreeCopyNativeACL(file *os.File, _ string, name string, symlink bool, acl *workspace.NativeACL) error {
	if symlink {
		if acl != nil {
			return fmt.Errorf("POSIX ACL cannot be applied to symlink %q", name)
		}
		return nil
	}
	if acl == nil {
		for _, attribute := range []string{linuxACLAccess, linuxACLDefault} {
			if err := removeTreeCopyXattr(file, "", attribute, false); err != nil {
				return fmt.Errorf("clear POSIX ACL %q: %w", attribute, err)
			}
		}
		return nil
	}
	if acl.Platform != "linux" || acl.Format != "linux-posix-acl-v1" {
		return fmt.Errorf("workspace ACL for %q is not compatible with Linux", name)
	}
	var attributes []workspace.ExtendedAttribute
	if err := json.Unmarshal(acl.Data, &attributes); err != nil {
		return fmt.Errorf("decode Linux POSIX ACL for %q: %w", name, err)
	}
	canonical, err := json.Marshal(attributes)
	if err != nil || !bytes.Equal(canonical, acl.Data) || len(attributes) == 0 || len(attributes) > 2 {
		return fmt.Errorf("Linux POSIX ACL for %q is malformed", name)
	}
	previous := ""
	for _, attribute := range attributes {
		if attribute.Name != linuxACLAccess && attribute.Name != linuxACLDefault || attribute.Name <= previous || len(attribute.Value) == 0 {
			return fmt.Errorf("Linux POSIX ACL for %q is malformed", name)
		}
		previous = attribute.Name
	}
	for _, attribute := range []string{linuxACLAccess, linuxACLDefault} {
		if err := removeTreeCopyXattr(file, "", attribute, false); err != nil {
			return fmt.Errorf("clear POSIX ACL %q: %w", attribute, err)
		}
	}
	for _, attribute := range attributes {
		if err := setTreeCopyXattr(file, "", attribute.Name, attribute.Value, false); err != nil {
			return fmt.Errorf("set POSIX ACL %q: %w", attribute.Name, err)
		}
	}
	return nil
}

func treeCopyIgnorableExtendedAttribute(name string) bool { return false }
