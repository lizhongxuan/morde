//go:build linux

package workspacecurrent

import (
	"errors"

	"golang.org/x/sys/unix"
)

func readWorkspaceCurrentSymbolicLink(root, parentPath, name string) (string, error) {
	parent, err := openWorkspaceCurrentFile(root, parentPath)
	if err != nil {
		return "", err
	}
	defer parent.Close()
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFLNK {
		return "", ErrUnsafePath
	}
	buffer := make([]byte, 4096)
	length, err := unix.Readlinkat(int(parent.Fd()), name, buffer)
	if err != nil {
		return "", err
	}
	if length == len(buffer) {
		return "", errors.New("Workspace symbolic link target exceeds the limit")
	}
	return string(buffer[:length]), nil
}
