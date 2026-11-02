//go:build linux

package workspacecurrent

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func openWorkspaceCurrentFile(root, relativePath string) (*os.File, error) {
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat2(rootFD, filepath.ToSlash(relativePath), &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	closeErr := unix.Close(rootFD)
	if err != nil {
		return nil, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return nil, errors.Join(closeErr, unix.Close(fd))
	}
	return os.NewFile(uintptr(fd), relativePath), nil
}
