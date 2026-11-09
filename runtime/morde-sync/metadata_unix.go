//go:build darwin || linux

package mordesync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const linuxACLPrefix = "system.posix_acl_"

func captureMetadataFD(file *os.File, name string) ([]ExtendedAttribute, *NativeACL, error) {
	if file == nil {
		return nil, nil, fmt.Errorf("%w: metadata handle for %q is unavailable", ErrUnsupportedMetadata, name)
	}
	for attempt := 0; attempt < 3; attempt++ {
		attributeNames, err := listXattrsFD(file, name)
		if err != nil {
			return nil, nil, err
		}
		xattrs, aclAttributes, err := readXattrValuesFD(file, attributeNames, name)
		if err != nil {
			if errors.Is(err, unix.ERANGE) {
				continue
			}
			return nil, nil, err
		}
		currentNames, err := listXattrsFD(file, name)
		if err != nil {
			return nil, nil, err
		}
		if !equalStrings(attributeNames, currentNames) {
			continue
		}
		if xattrs == nil {
			xattrs = []ExtendedAttribute{}
		}
		var acl *NativeACL
		switch runtime.GOOS {
		case "linux":
			if len(aclAttributes) > 0 {
				data, err := json.Marshal(aclAttributes)
				if err != nil {
					return nil, nil, err
				}
				acl = &NativeACL{Platform: "linux", Format: "linux-posix-acl-v1", Data: data}
			}
		case "darwin":
			acl, err = captureDarwinACLFD(file, name)
			if err != nil {
				return nil, nil, err
			}
		}
		if err := validateMetadata(xattrs, acl); err != nil {
			return nil, nil, fmt.Errorf("%q: %w", name, err)
		}
		return xattrs, acl, nil
	}
	return nil, nil, fmt.Errorf("%w: metadata changed while inspecting %q", ErrRescanRequired, name)
}

func captureSymlinkMetadataAt(parent *os.File, name, relative string) ([]ExtendedAttribute, *NativeACL, error) {
	if parent == nil {
		return nil, nil, fmt.Errorf("%w: parent handle for symlink %q is unavailable", ErrUnsupportedMetadata, relative)
	}
	var linkPath string
	if runtime.GOOS == "darwin" {
		parentPath, err := darwinFDPath(parent)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: inspect metadata on symlink %q: %w", ErrUnsupportedMetadata, relative, err)
		}
		linkPath = filepath.Join(parentPath, name)
	} else {
		linkPath = filepath.Join("/dev/fd", strconv.FormatUint(uint64(parent.Fd()), 10), name)
	}
	for attempt := 0; attempt < 3; attempt++ {
		names, err := listXattrsPath(linkPath, relative, true)
		if err != nil {
			return nil, nil, err
		}
		xattrs, aclAttributes, err := readXattrValuesPath(linkPath, names, relative, true)
		if err != nil {
			if errors.Is(err, unix.ERANGE) {
				continue
			}
			return nil, nil, err
		}
		currentNames, err := listXattrsPath(linkPath, relative, true)
		if err != nil {
			return nil, nil, err
		}
		if !equalStrings(names, currentNames) {
			continue
		}
		if xattrs == nil {
			xattrs = []ExtendedAttribute{}
		}
		var acl *NativeACL
		switch runtime.GOOS {
		case "linux":
			if len(aclAttributes) > 0 {
				data, err := json.Marshal(aclAttributes)
				if err != nil {
					return nil, nil, err
				}
				acl = &NativeACL{Platform: "linux", Format: "linux-posix-acl-v1", Data: data}
			}
		case "darwin":
			acl, err = captureDarwinACLSymlink(parent, name, relative)
			if err != nil {
				return nil, nil, err
			}
		}
		if err := validateMetadata(xattrs, acl); err != nil {
			return nil, nil, fmt.Errorf("%q: %w", relative, err)
		}
		latestParentPath := ""
		if runtime.GOOS == "darwin" {
			latestParentPath, err = darwinFDPath(parent)
			if err != nil || filepath.Join(latestParentPath, name) != linkPath {
				return nil, nil, errors.Join(fmt.Errorf("%w: parent changed while inspecting symlink %q", ErrRescanRequired, relative), err)
			}
		}
		return xattrs, acl, nil
	}
	return nil, nil, fmt.Errorf("%w: metadata changed while inspecting symlink %q", ErrRescanRequired, relative)
}

func listXattrsFD(file *os.File, name string) ([]string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		size, err := unix.Flistxattr(int(file.Fd()), nil)
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: inspect extended attributes on %q: %w", ErrUnsupportedMetadata, name, err)
		}
		if size == 0 {
			return []string{}, nil
		}
		attributes := make([]byte, size)
		count, err := unix.Flistxattr(int(file.Fd()), attributes)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%w: inspect extended attributes on %q: %w", ErrUnsupportedMetadata, name, err)
		}
		return parseXattrNames(attributes, count, name)
	}
	return nil, fmt.Errorf("%w: extended attributes changed while inspecting %q", ErrRescanRequired, name)
}

func listXattrsPath(path, name string, noFollow bool) ([]string, error) {
	list := unix.Listxattr
	if noFollow {
		list = unix.Llistxattr
	}
	for attempt := 0; attempt < 3; attempt++ {
		size, err := list(path, nil)
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%w: inspect extended attributes on %q: %w", ErrUnsupportedMetadata, name, err)
		}
		if size == 0 {
			return []string{}, nil
		}
		attributes := make([]byte, size)
		count, err := list(path, attributes)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%w: inspect extended attributes on %q: %w", ErrUnsupportedMetadata, name, err)
		}
		return parseXattrNames(attributes, count, name)
	}
	return nil, fmt.Errorf("%w: extended attributes changed while inspecting %q", ErrRescanRequired, name)
}

func parseXattrNames(attributes []byte, count int, name string) ([]string, error) {
	if count < 0 || count > len(attributes) {
		return nil, fmt.Errorf("%w: extended attribute list for %q is incomplete", ErrUnsupportedMetadata, name)
	}
	var names []string
	for remaining := attributes[:count]; len(remaining) > 0; {
		end := bytes.IndexByte(remaining, 0)
		if end <= 0 {
			return nil, fmt.Errorf("%w: extended attribute list for %q is malformed", ErrUnsupportedMetadata, name)
		}
		attribute := string(remaining[:end])
		if runtime.GOOS != "darwin" || attribute != "com.apple.provenance" {
			names = append(names, attribute)
		}
		remaining = remaining[end+1:]
	}
	sort.Strings(names)
	if len(names) > maxWorkspaceXattrs+2 {
		return nil, fmt.Errorf("%w: too many attributes on %q", ErrUnsupportedMetadata, name)
	}
	return names, nil
}

func readXattrValuesFD(file *os.File, names []string, relative string) ([]ExtendedAttribute, []ExtendedAttribute, error) {
	var xattrs, aclAttributes []ExtendedAttribute
	for _, name := range names {
		value, err := getXattrFD(file, name)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: read extended attribute %q on %q: %w", ErrUnsupportedMetadata, name, relative, err)
		}
		attribute := ExtendedAttribute{Name: name, Value: value}
		if runtime.GOOS == "linux" && strings.HasPrefix(name, linuxACLPrefix) {
			aclAttributes = append(aclAttributes, attribute)
		} else {
			xattrs = append(xattrs, attribute)
		}
	}
	return xattrs, aclAttributes, nil
}

func readXattrValuesPath(path string, names []string, relative string, noFollow bool) ([]ExtendedAttribute, []ExtendedAttribute, error) {
	get := unix.Getxattr
	if noFollow {
		get = unix.Lgetxattr
	}
	var xattrs, aclAttributes []ExtendedAttribute
	for _, name := range names {
		value, err := getXattrPath(get, path, name)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: read extended attribute %q on %q: %w", ErrUnsupportedMetadata, name, relative, err)
		}
		attribute := ExtendedAttribute{Name: name, Value: value}
		if runtime.GOOS == "linux" && strings.HasPrefix(name, linuxACLPrefix) {
			aclAttributes = append(aclAttributes, attribute)
		} else {
			xattrs = append(xattrs, attribute)
		}
	}
	return xattrs, aclAttributes, nil
}

func getXattrFD(file *os.File, name string) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		size, err := unix.Fgetxattr(int(file.Fd()), name, nil)
		if err != nil {
			return nil, err
		}
		if size > maxWorkspaceXattrValueBytes {
			return nil, ErrInvalidManifest
		}
		value := make([]byte, size)
		count, err := unix.Fgetxattr(int(file.Fd()), name, value)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if count != size {
			continue
		}
		return value, nil
	}
	return nil, unix.ERANGE
}

func getXattrPath(get func(string, string, []byte) (int, error), path, name string) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		size, err := get(path, name, nil)
		if err != nil {
			return nil, err
		}
		if size > maxWorkspaceXattrValueBytes {
			return nil, ErrInvalidManifest
		}
		value := make([]byte, size)
		count, err := get(path, name, value)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if count != size {
			continue
		}
		return value, nil
	}
	return nil, unix.ERANGE
}

func applyMetadataFD(file *os.File, name string, xattrs []ExtendedAttribute, acl *NativeACL, known bool) error {
	if err := validateMetadata(xattrs, acl); err != nil {
		return err
	}
	if !known {
		matches, err := metadataMatchesFD(file, name, xattrs, acl, false)
		if err != nil {
			return err
		}
		if !matches {
			return fmt.Errorf("%w: destination metadata is not known for %q", ErrUnsupportedMetadata, name)
		}
		return nil
	}
	if acl != nil && acl.Platform != runtime.GOOS {
		return fmt.Errorf("%w: %s ACL cannot be restored on %s", ErrUnsupportedMetadata, acl.Platform, runtime.GOOS)
	}
	for _, xattr := range xattrs {
		if err := unix.Fsetxattr(int(file.Fd()), xattr.Name, xattr.Value, 0); err != nil {
			return fmt.Errorf("restore extended attribute %q on %q: %w", xattr.Name, name, err)
		}
	}
	if runtime.GOOS == "linux" {
		if err := applyLinuxACLFD(file, name, acl); err != nil {
			return err
		}
	} else if err := applyDarwinACLFD(file, name, acl); err != nil {
		return err
	}
	actualXattrs, actualACL, err := captureMetadataFD(file, name)
	if err != nil {
		return err
	}
	if !sameMetadata(xattrs, acl, actualXattrs, actualACL) {
		return fmt.Errorf("%w: restored metadata on %q did not match", ErrDestinationChange, name)
	}
	return nil
}

func applySymlinkMetadataAt(parent *os.File, name, relative string, xattrs []ExtendedAttribute, acl *NativeACL, known bool) error {
	if err := validateMetadata(xattrs, acl); err != nil {
		return err
	}
	if !known {
		matches, err := metadataMatchesSymlink(parent, name, relative, xattrs, acl, false)
		if err != nil {
			return err
		}
		if !matches {
			return fmt.Errorf("%w: destination symlink metadata is not known for %q", ErrUnsupportedMetadata, relative)
		}
		return nil
	}
	if acl != nil && acl.Platform != runtime.GOOS {
		return fmt.Errorf("%w: %s ACL cannot be restored on %s", ErrUnsupportedMetadata, acl.Platform, runtime.GOOS)
	}
	var linkPath string
	if runtime.GOOS == "darwin" {
		parentPath, err := darwinFDPath(parent)
		if err != nil {
			return err
		}
		linkPath = filepath.Join(parentPath, name)
	} else {
		linkPath = filepath.Join("/dev/fd", strconv.FormatUint(uint64(parent.Fd()), 10), name)
	}
	for _, xattr := range xattrs {
		if err := unix.Lsetxattr(linkPath, xattr.Name, xattr.Value, 0); err != nil {
			return fmt.Errorf("restore extended attribute %q on symlink %q: %w", xattr.Name, relative, err)
		}
	}
	if runtime.GOOS == "linux" {
		if acl != nil {
			return fmt.Errorf("%w: Linux does not support ACL metadata on symlinks", ErrUnsupportedMetadata)
		}
	} else if err := applyDarwinACLSymlink(parent, name, relative, acl); err != nil {
		return err
	}
	actualXattrs, actualACL, err := captureSymlinkMetadataAt(parent, name, relative)
	if err != nil {
		return err
	}
	if !sameMetadata(xattrs, acl, actualXattrs, actualACL) {
		return fmt.Errorf("%w: restored metadata on symlink %q did not match", ErrDestinationChange, relative)
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func linuxACLAttributes(acl *NativeACL) ([]ExtendedAttribute, error) {
	if acl == nil {
		return nil, nil
	}
	if acl.Platform != "linux" || acl.Format != "linux-posix-acl-v1" {
		return nil, ErrInvalidManifest
	}
	var attributes []ExtendedAttribute
	if err := json.Unmarshal(acl.Data, &attributes); err != nil || len(attributes) == 0 || len(attributes) > 2 {
		return nil, ErrInvalidManifest
	}
	previous := ""
	for _, attribute := range attributes {
		if attribute.Name != "system.posix_acl_access" && attribute.Name != "system.posix_acl_default" || attribute.Name <= previous || len(attribute.Value) == 0 || len(attribute.Value) > maxWorkspaceXattrValueBytes {
			return nil, ErrInvalidManifest
		}
		previous = attribute.Name
	}
	return attributes, nil
}

func applyLinuxACLFD(file *os.File, name string, acl *NativeACL) error {
	attributes, err := linuxACLAttributes(acl)
	if err != nil {
		return fmt.Errorf("restore Linux ACL on %q: %w", name, err)
	}
	wanted := make(map[string][]byte, len(attributes))
	for _, attribute := range attributes {
		wanted[attribute.Name] = attribute.Value
	}
	for _, name := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		value, getErr := getXattrFD(file, name)
		if getErr == nil {
			if expected, ok := wanted[name]; ok {
				if !bytes.Equal(value, expected) {
					if err := unix.Fsetxattr(int(file.Fd()), name, expected, 0); err != nil {
						return fmt.Errorf("restore Linux ACL attribute %q on %q: %w", name, file.Name(), err)
					}
				}
			} else if err := unix.Fremovexattr(int(file.Fd()), name); err != nil && !errors.Is(err, unix.ENODATA) {
				return fmt.Errorf("remove inherited Linux ACL attribute %q on %q: %w", name, file.Name(), err)
			}
			continue
		}
		if !errors.Is(getErr, unix.ENODATA) && !errors.Is(getErr, unix.EOPNOTSUPP) && !errors.Is(getErr, unix.ENOTSUP) {
			return fmt.Errorf("inspect Linux ACL on %q: %w", name, getErr)
		}
		if expected, ok := wanted[name]; ok {
			if err := unix.Fsetxattr(int(file.Fd()), name, expected, 0); err != nil {
				return fmt.Errorf("restore Linux ACL attribute %q on %q: %w", name, file.Name(), err)
			}
		}
	}
	return nil
}
