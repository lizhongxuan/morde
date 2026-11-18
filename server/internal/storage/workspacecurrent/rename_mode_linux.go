//go:build linux

package workspacecurrent

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func prepareWorkspaceCurrentDirectoryForRename(rootPath, workspacePath, name string) (restore *workspaceCurrentDirectoryModeRestore, retErr error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return nil, ErrUnsafePath
	}
	parent, err := openWorkspaceCurrentFile(rootPath, workspacePath)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, parent.Close()) }()
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), name)
	defer func() {
		if restore == nil {
			retErr = errors.Join(retErr, directory.Close())
		}
	}()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	mode := stat.Mode & 0o7777
	if mode&0o700 == 0o700 {
		return nil, nil
	}
	if err := chmodWorkspaceCurrentDirectory(fd, mode|0o700); err != nil {
		return nil, err
	}
	restore = &workspaceCurrentDirectoryModeRestore{file: directory, mode: mode}
	return restore, nil
}

func restoreWorkspaceCurrentDirectoryMode(directory *os.File, mode uint32) error {
	return chmodWorkspaceCurrentDirectory(int(directory.Fd()), mode)
}

func chmodWorkspaceCurrentDirectory(fd int, mode uint32) error {
	err := unix.Fchmodat(fd, "", mode, unix.AT_EMPTY_PATH)
	if err == nil || (!errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL)) {
		return err
	}
	// Some container filesystems reject AT_EMPTY_PATH; the proc fd link still names this held directory.
	if fallbackErr := unix.Fchmodat(unix.AT_FDCWD, fmt.Sprintf("/proc/self/fd/%d", fd), mode, 0); fallbackErr != nil {
		return errors.Join(err, fallbackErr)
	}
	return nil
}
