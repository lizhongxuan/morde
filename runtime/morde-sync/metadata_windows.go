//go:build windows

package mordesync

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

func captureMetadataFD(file *os.File, name string) ([]ExtendedAttribute, *NativeACL, error) {
	if file == nil {
		return nil, nil, fmt.Errorf("%w: metadata handle for %q is unavailable", ErrUnsupportedMetadata, name)
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: inspect ACL on %q: %w", ErrUnsupportedMetadata, name, err)
	}
	if sddl := sd.String(); !sddlHasOnlyInheritedDACL(sddl) {
		return nil, nil, fmt.Errorf("%w: %q has an explicit or unrecognized Windows ACL", ErrUnsupportedMetadata, name)
	}
	if err := checkWindowsStreams(file, name); err != nil {
		return nil, nil, err
	}
	return nil, nil, nil
}

func captureSymlinkMetadataAt(_ *os.File, _ string, relative string) ([]ExtendedAttribute, *NativeACL, error) {
	// Windows does not expose a handle-relative, no-follow ACL/stream query in
	// the APIs available to this package. Do not report an unchecked symlink as
	// safely transferable.
	return nil, nil, fmt.Errorf("%w: cannot inspect Windows symlink ACL/streams for %q", ErrUnsupportedMetadata, relative)
}

func applyMetadataFD(file *os.File, name string, xattrs []ExtendedAttribute, acl *NativeACL, known bool) error {
	if len(xattrs) != 0 || acl != nil {
		return fmt.Errorf("%w: Windows ACL and extended attributes are not supported", ErrUnsupportedMetadata)
	}
	_, _, err := captureMetadataFD(file, name)
	if !known && err == nil {
		return nil
	}
	return err
}

func applySymlinkMetadataAt(_ *os.File, _ string, relative string, xattrs []ExtendedAttribute, acl *NativeACL, _ bool) error {
	if len(xattrs) != 0 || acl != nil {
		return fmt.Errorf("%w: Windows symlink ACL and extended attributes are not supported", ErrUnsupportedMetadata)
	}
	return fmt.Errorf("%w: cannot inspect Windows symlink ACL/streams for %q", ErrUnsupportedMetadata, relative)
}

func sddlHasOnlyInheritedDACL(sddl string) bool {
	start := strings.Index(sddl, "D:")
	if start < 0 {
		return false
	}
	value := sddl[start+2:]
	if end := strings.Index(value, "S:"); end >= 0 {
		value = value[:end]
	}
	if strings.Contains(value, "NO_ACCESS_CONTROL") {
		return false
	}
	seen := false
	for len(value) > 0 {
		open := strings.IndexByte(value, '(')
		if open < 0 {
			return seen && strings.Trim(value, "P AIGU") == ""
		}
		prefix := value[:open]
		for _, flag := range []string{"AI", "AR", "P"} {
			prefix = strings.ReplaceAll(prefix, flag, "")
		}
		if strings.TrimSpace(prefix) != "" {
			return false
		}
		close := strings.IndexByte(value[open+1:], ')')
		if close < 0 {
			return false
		}
		ace := strings.Split(value[open+1:open+1+close], ";")
		if len(ace) < 6 || !strings.Contains(ace[1], "ID") {
			return false
		}
		seen = true
		value = value[open+2+close:]
	}
	return seen
}

func checkWindowsStreams(file *os.File, name string) error {
	const maxBuffer = 1 << 20
	buffer := make([]byte, maxBuffer)
	err := windows.GetFileInformationByHandleEx(windows.Handle(file.Fd()), windows.FileStreamInfo, &buffer[0], uint32(len(buffer)))
	if err != nil {
		return fmt.Errorf("%w: inspect alternate data streams on %q: %w", ErrUnsupportedMetadata, name, err)
	}
	for offset := 0; ; {
		if offset < 0 || offset+24 > len(buffer) {
			return fmt.Errorf("%w: stream metadata for %q is incomplete", ErrUnsupportedMetadata, name)
		}
		next := int(binary.LittleEndian.Uint32(buffer[offset : offset+4]))
		nameBytes := int(binary.LittleEndian.Uint32(buffer[offset+4 : offset+8]))
		if nameBytes < 0 || nameBytes%2 != 0 || nameBytes > len(buffer)-offset-24 {
			return fmt.Errorf("%w: stream metadata for %q is malformed", ErrUnsupportedMetadata, name)
		}
		streamName := string(utf16.Decode(readUTF16(buffer[offset+24 : offset+24+nameBytes])))
		if streamName != "::$DATA" && streamName != "::$INDEX_ALLOCATION" {
			return fmt.Errorf("%w: %q has alternate data stream %q", ErrUnsupportedMetadata, name, streamName)
		}
		if next == 0 {
			return nil
		}
		if next < 24+nameBytes || next%8 != 0 || next > len(buffer)-offset {
			return fmt.Errorf("%w: stream metadata for %q is malformed", ErrUnsupportedMetadata, name)
		}
		offset += next
	}
}

func readUTF16(value []byte) []uint16 {
	words := make([]uint16, len(value)/2)
	for i := range words {
		words[i] = binary.LittleEndian.Uint16(value[i*2 : i*2+2])
	}
	return words
}
