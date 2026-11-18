//go:build linux

package workspacecurrent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

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
	if errors.Is(err, unix.ENOSYS) {
		fd, err = openWorkspaceCurrentFileWithoutOpenat2(rootFD, relativePath)
	}
	closeErr := unix.Close(rootFD)
	if err != nil {
		return nil, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return nil, errors.Join(closeErr, unix.Close(fd))
	}
	return os.NewFile(uintptr(fd), relativePath), nil
}

func openWorkspaceCurrentFileWithoutOpenat2(rootFD int, relativePath string) (int, error) {
	if relativePath == "" || filepath.IsAbs(relativePath) {
		return -1, ErrUnsafePath
	}
	components := strings.Split(filepath.ToSlash(relativePath), "/")
	parentFD, parentOwned := rootFD, false
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			if parentOwned {
				_ = unix.Close(parentFD)
			}
			return -1, ErrUnsafePath
		}
		flags := unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if index < len(components)-1 {
			flags |= unix.O_PATH | unix.O_DIRECTORY
		} else {
			flags |= unix.O_RDONLY
		}
		fd, err := unix.Openat(parentFD, component, flags, 0)
		if parentOwned {
			if closeErr := unix.Close(parentFD); closeErr != nil {
				if err == nil {
					_ = unix.Close(fd)
				}
				return -1, errors.Join(err, closeErr)
			}
		}
		if err != nil {
			if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
				return -1, ErrUnsafePath
			}
			return -1, err
		}
		parentFD, parentOwned = fd, true
	}
	return parentFD, nil
}
