//go:build darwin

package mordesync

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	darwinFileSecMagic = 0x012cc16d
	darwinNoACL        = ^uint32(0)
	darwinFileSecBase  = 44
	darwinACEBytes     = 28
)

func captureDarwinACLFD(file *os.File, name string) (*NativeACL, error) {
	data, err := getDarwinFileSecFD(file, name)
	if err != nil || len(data) == 0 {
		return nil, err
	}
	count := binary.LittleEndian.Uint32(data[36:40])
	if count == darwinNoACL {
		return nil, nil
	}
	return &NativeACL{Platform: "darwin", Format: "darwin-filesec-v1", Data: data}, nil
}

func captureDarwinACLSymlink(parent *os.File, name, relative string) (*NativeACL, error) {
	data, err := getDarwinFileSecSymlink(parent, name, relative)
	if err != nil || len(data) == 0 {
		return nil, err
	}
	count := binary.LittleEndian.Uint32(data[36:40])
	if count == darwinNoACL {
		return nil, nil
	}
	return &NativeACL{Platform: "darwin", Format: "darwin-filesec-v1", Data: data}, nil
}

func getDarwinFileSecFD(file *os.File, name string) ([]byte, error) {
	attributes := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	buffer := make([]byte, maxWorkspaceACLBytes+4096)
	//nolint:staticcheck // x/sys/unix has no fgetattrlist wrapper.
	_, _, errno := unix.Syscall6(unix.SYS_FGETATTRLIST,
		file.Fd(), uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)), 0, 0)
	runtime.KeepAlive(attributes)
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return nil, fmt.Errorf("%w: inspect ACL on %q: %w", ErrUnsupportedMetadata, name, errno)
	}
	return parseDarwinFileSec(buffer, name)
}

func getDarwinFileSecSymlink(parent *os.File, name, relative string) ([]byte, error) {
	path, err := darwinFDPath(parent)
	if err != nil {
		return nil, fmt.Errorf("%w: inspect ACL on symlink %q: %w", ErrUnsupportedMetadata, relative, err)
	}
	path += "/" + name
	pathPtr, err := unix.BytePtrFromString(path)
	if err != nil {
		return nil, err
	}
	attributes := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	buffer := make([]byte, maxWorkspaceACLBytes+4096)
	//nolint:staticcheck // x/sys/unix has no getattrlist no-follow wrapper.
	_, _, errno := unix.Syscall6(unix.SYS_GETATTRLIST,
		uintptr(unsafe.Pointer(pathPtr)), uintptr(unsafe.Pointer(&attributes)),
		uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), uintptr(unix.FSOPT_NOFOLLOW), 0)
	runtime.KeepAlive(pathPtr)
	runtime.KeepAlive(attributes)
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return nil, fmt.Errorf("%w: inspect ACL on symlink %q: %w", ErrUnsupportedMetadata, relative, errno)
	}
	latestParentPath, err := darwinFDPath(parent)
	if err != nil || latestParentPath+"/"+name != path {
		return nil, errors.Join(fmt.Errorf("%w: parent changed while inspecting symlink %q", ErrRescanRequired, relative), err)
	}
	return parseDarwinFileSec(buffer, relative)
}

func parseDarwinFileSec(buffer []byte, name string) ([]byte, error) {
	if len(buffer) < 12 {
		return nil, fmt.Errorf("%w: ACL metadata for %q is incomplete", ErrUnsupportedMetadata, name)
	}
	totalLength := int(binary.LittleEndian.Uint32(buffer[:4]))
	dataOffset := int(int32(binary.LittleEndian.Uint32(buffer[4:8])))
	dataLength := int(binary.LittleEndian.Uint32(buffer[8:12]))
	dataStart := 4 + dataOffset
	if totalLength < 12 || totalLength > len(buffer) || dataOffset < 8 || dataStart < 12 || dataStart > totalLength || dataLength > totalLength-dataStart {
		return nil, fmt.Errorf("%w: ACL metadata for %q is incomplete", ErrUnsupportedMetadata, name)
	}
	if dataLength == 0 {
		return nil, nil
	}
	data := append([]byte(nil), buffer[dataStart:dataStart+dataLength]...)
	if err := validateDarwinFileSec(data); err != nil {
		return nil, fmt.Errorf("%w: ACL metadata for %q is invalid: %v", ErrUnsupportedMetadata, name, err)
	}
	return data, nil
}

func validateDarwinFileSec(data []byte) error {
	if len(data) < darwinFileSecBase || binary.LittleEndian.Uint32(data[:4]) != darwinFileSecMagic {
		return errors.New("unrecognized file security structure")
	}
	count := binary.LittleEndian.Uint32(data[36:40])
	if count == darwinNoACL {
		if len(data) != darwinFileSecBase {
			return errors.New("no-ACL payload has trailing data")
		}
		return nil
	}
	if count > 128 || uint64(len(data)) != uint64(darwinFileSecBase)+uint64(count)*darwinACEBytes {
		return errors.New("invalid ACL entry count")
	}
	return nil
}

func applyDarwinACLFD(file *os.File, name string, acl *NativeACL) error {
	var data []byte
	if acl == nil {
		current, err := getDarwinFileSecFD(file, name)
		if err != nil {
			return err
		}
		if len(current) == 0 || binary.LittleEndian.Uint32(current[36:40]) == darwinNoACL {
			return nil
		}
		data = append([]byte(nil), current[:darwinFileSecBase]...)
		binary.LittleEndian.PutUint32(data[36:40], darwinNoACL)
	} else {
		if acl.Platform != "darwin" || acl.Format != "darwin-filesec-v1" || validateDarwinFileSec(acl.Data) != nil {
			return fmt.Errorf("%w: Darwin ACL payload is invalid", ErrInvalidManifest)
		}
		data = acl.Data
	}
	return setDarwinFileSecFD(file, name, data)
}

func setDarwinFileSecFD(file *os.File, name string, data []byte) error {
	attributes := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	buffer := make([]byte, 8+len(data))
	binary.LittleEndian.PutUint32(buffer[:4], 8)
	binary.LittleEndian.PutUint32(buffer[4:8], uint32(len(data)))
	copy(buffer[8:], data)
	//nolint:staticcheck // x/sys/unix has no fsetattrlist wrapper.
	_, _, errno := unix.Syscall6(unix.SYS_FSETATTRLIST,
		file.Fd(), uintptr(unsafe.Pointer(&attributes)), uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)), 0, 0)
	runtime.KeepAlive(attributes)
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return fmt.Errorf("restore Darwin ACL on %q: %w", name, errno)
	}
	return nil
}

func applyDarwinACLSymlink(parent *os.File, name, relative string, acl *NativeACL) error {
	var data []byte
	if acl == nil {
		current, err := getDarwinFileSecSymlink(parent, name, relative)
		if err != nil {
			return err
		}
		if len(current) == 0 || binary.LittleEndian.Uint32(current[36:40]) == darwinNoACL {
			return nil
		}
		data = append([]byte(nil), current[:darwinFileSecBase]...)
		binary.LittleEndian.PutUint32(data[36:40], darwinNoACL)
	} else {
		if acl.Platform != "darwin" || acl.Format != "darwin-filesec-v1" || validateDarwinFileSec(acl.Data) != nil {
			return fmt.Errorf("%w: Darwin ACL payload is invalid", ErrInvalidManifest)
		}
		data = acl.Data
	}
	path, err := darwinFDPath(parent)
	if err != nil {
		return err
	}
	path += "/" + name
	pathPtr, err := unix.BytePtrFromString(path)
	if err != nil {
		return err
	}
	attributes := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	buffer := make([]byte, 8+len(data))
	binary.LittleEndian.PutUint32(buffer[:4], 8)
	binary.LittleEndian.PutUint32(buffer[4:8], uint32(len(data)))
	copy(buffer[8:], data)
	//nolint:staticcheck // x/sys/unix has no setattrlist no-follow wrapper.
	_, _, errno := unix.Syscall6(unix.SYS_SETATTRLISTAT,
		parent.Fd(), uintptr(unsafe.Pointer(pathPtr)), uintptr(unsafe.Pointer(&attributes)),
		uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), uintptr(unix.FSOPT_NOFOLLOW))
	runtime.KeepAlive(pathPtr)
	runtime.KeepAlive(attributes)
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return fmt.Errorf("restore Darwin ACL on symlink %q: %w", relative, errno)
	}
	return nil
}

func darwinFDPath(file *os.File) (string, error) {
	if file == nil {
		return "", errors.New("metadata handle is unavailable")
	}
	buffer := make([]byte, unix.PathMax)
	//nolint:staticcheck // F_GETPATH needs a pointer argument not exposed by the typed wrapper.
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, file.Fd(), uintptr(unix.F_GETPATH), uintptr(unsafe.Pointer(&buffer[0])))
	runtime.KeepAlive(buffer)
	if errno != 0 {
		return "", errno
	}
	end := 0
	for end < len(buffer) && buffer[end] != 0 {
		end++
	}
	if end == 0 || end == len(buffer) {
		return "", errors.New("file descriptor path is unavailable")
	}
	return string(buffer[:end]), nil
}
