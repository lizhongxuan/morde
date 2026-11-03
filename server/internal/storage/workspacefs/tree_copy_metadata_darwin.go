//go:build darwin

package workspacefs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"morde/server/internal/domain/workspace"
)

const darwinACLXattr = "com.apple.system.Security"

type darwinTreeCopyAttrList struct {
	bitmapCount   uint16
	reserved      uint16
	commonAttr    uint32
	volumeAttr    uint32
	directoryAttr uint32
	fileAttr      uint32
	forkAttr      uint32
}

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
	if errors.Is(err, unix.ENOATTR) {
		return nil
	}
	return err
}

func treeCopyNativeACLXattr(name string) bool { return name == darwinACLXattr }

func readTreeCopyNativeACL(file *os.File, path, name string, symlink bool) (*workspace.NativeACL, error) {
	buffer := make([]byte, workspace.MaxWorkspaceACLBytes+64)
	attributes := darwinTreeCopyAttrList{bitmapCount: 5, commonAttr: unix.ATTR_CMN_EXTENDED_SECURITY}
	var result uintptr
	var errno syscall.Errno
	if symlink {
		pathPtr, err := unix.BytePtrFromString(path)
		if err != nil {
			return nil, fmt.Errorf("inspect workspace ACL %q: %w", name, err)
		}
		//nolint:staticcheck // The no-follow getattrlist path has no x/sys wrapper.
		_, _, errno = unix.Syscall6(unix.SYS_GETATTRLIST,
			uintptr(unsafe.Pointer(pathPtr)), uintptr(unsafe.Pointer(&attributes)),
			uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), uintptr(unix.FSOPT_NOFOLLOW), 0)
		runtime.KeepAlive(pathPtr)
	} else {
		//nolint:staticcheck // The descriptor-based getattrlist path has no x/sys wrapper.
		result, _, errno = unix.Syscall6(unix.SYS_FGETATTRLIST,
			file.Fd(), uintptr(unsafe.Pointer(&attributes)),
			uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), 0, 0)
	}
	runtime.KeepAlive(attributes)
	runtime.KeepAlive(buffer)
	_ = result
	if errno != 0 {
		return nil, fmt.Errorf("inspect workspace ACL %q: %w", name, errno)
	}
	data, err := darwinTreeCopyACLFromGetattrlist(buffer, name)
	if err != nil || len(data) == 0 {
		return nil, err
	}
	return &workspace.NativeACL{Platform: "darwin", Format: "darwin-filesec-v1", Data: data}, nil
}

func darwinTreeCopyACLFromGetattrlist(buffer []byte, name string) ([]byte, error) {
	length := int(binary.LittleEndian.Uint32(buffer[:4]))
	if length < 12 || length > len(buffer) {
		return nil, fmt.Errorf("workspace ACL metadata for %q is incomplete", name)
	}
	dataOffset := int(int32(binary.LittleEndian.Uint32(buffer[4:8])))
	dataLength := int(binary.LittleEndian.Uint32(buffer[8:12]))
	dataStart := 4 + dataOffset
	if dataOffset < 8 || dataStart < 12 || dataStart > length || dataLength > length-dataStart || dataLength > workspace.MaxWorkspaceACLBytes {
		return nil, fmt.Errorf("workspace ACL metadata for %q is incomplete", name)
	}
	if dataLength == 0 {
		return nil, nil
	}
	fileSecurity := buffer[dataStart : dataStart+dataLength]
	if len(fileSecurity) < 40 || binary.LittleEndian.Uint32(fileSecurity[:4]) != 0x012cc16d {
		return nil, fmt.Errorf("workspace ACL metadata for %q is unrecognized", name)
	}
	entryCount := binary.LittleEndian.Uint32(fileSecurity[36:40])
	if entryCount == ^uint32(0) {
		return nil, nil
	}
	if entryCount == 0 || entryCount > 128 {
		return nil, fmt.Errorf("workspace ACL metadata for %q is invalid", name)
	}
	return append([]byte(nil), fileSecurity...), nil
}

func applyTreeCopyNativeACL(file *os.File, path, name string, symlink bool, acl *workspace.NativeACL) error {
	if acl == nil {
		current, err := readTreeCopyNativeACL(file, path, name, symlink)
		if err != nil || current == nil {
			return err
		}
		data := append([]byte(nil), current.Data...)
		binary.LittleEndian.PutUint32(data[36:40], ^uint32(0)) // KAUTH_FILESEC_NOACL
		if err := setDarwinTreeCopyACL(file, path, name, symlink, data[:44]); err != nil {
			return err
		}
		return nil
	}
	if acl.Platform != "darwin" || acl.Format != "darwin-filesec-v1" {
		return fmt.Errorf("workspace ACL for %q is not compatible with macOS", name)
	}
	if len(acl.Data) < 44 || len(acl.Data) > workspace.MaxWorkspaceACLBytes ||
		uint32(acl.Data[0])|uint32(acl.Data[1])<<8|uint32(acl.Data[2])<<16|uint32(acl.Data[3])<<24 != 0x012cc16d {
		return fmt.Errorf("workspace ACL for %q is malformed", name)
	}
	entryCount := uint32(acl.Data[36]) | uint32(acl.Data[37])<<8 | uint32(acl.Data[38])<<16 | uint32(acl.Data[39])<<24
	if entryCount == 0 || entryCount == ^uint32(0) || entryCount > 128 {
		return fmt.Errorf("workspace ACL for %q is malformed", name)
	}
	return setDarwinTreeCopyACL(file, path, name, symlink, acl.Data)
}

func setDarwinTreeCopyACL(file *os.File, path, name string, symlink bool, data []byte) error {
	attrBuffer := make([]byte, 8+len(data))
	binary.LittleEndian.PutUint32(attrBuffer[:4], 8)
	binary.LittleEndian.PutUint32(attrBuffer[4:8], uint32(len(data)))
	copy(attrBuffer[8:], data)
	attributes := darwinTreeCopyAttrList{bitmapCount: 5, commonAttr: unix.ATTR_CMN_EXTENDED_SECURITY}
	var errno syscall.Errno
	if symlink {
		pathPtr, err := unix.BytePtrFromString(path)
		if err != nil {
			return fmt.Errorf("apply workspace ACL %q: %w", name, err)
		}
		//nolint:staticcheck // The no-follow setattrlist path has no x/sys wrapper.
		_, _, errno = unix.Syscall6(unix.SYS_SETATTRLIST,
			uintptr(unsafe.Pointer(pathPtr)), uintptr(unsafe.Pointer(&attributes)),
			uintptr(unsafe.Pointer(&attrBuffer[0])), uintptr(len(attrBuffer)), uintptr(unix.FSOPT_NOFOLLOW), 0)
		runtime.KeepAlive(pathPtr)
	} else {
		//nolint:staticcheck // The descriptor-based setattrlist path has no x/sys wrapper.
		_, _, errno = unix.Syscall6(unix.SYS_FSETATTRLIST,
			file.Fd(), uintptr(unsafe.Pointer(&attributes)),
			uintptr(unsafe.Pointer(&attrBuffer[0])), uintptr(len(attrBuffer)), 0, 0)
	}
	runtime.KeepAlive(attributes)
	runtime.KeepAlive(attrBuffer)
	if errno != 0 {
		return fmt.Errorf("apply workspace ACL %q: %w", name, errno)
	}
	return nil
}

// shortcut: macOS regenerates com.apple.provenance on copied files; ignore it because it is system generated.
func treeCopyIgnorableExtendedAttribute(name string) bool { return name == "com.apple.provenance" }
