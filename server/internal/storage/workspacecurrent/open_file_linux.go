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
	return openWorkspaceCurrentPath(root, relativePath, unix.O_RDONLY)
}

func openWorkspaceCurrentDirectory(root, relativePath string) (*os.File, error) {
	return openWorkspaceCurrentPath(root, relativePath, unix.O_PATH|unix.O_DIRECTORY)
}

func openWorkspaceCurrentPath(root, relativePath string, flags int) (*os.File, error) {
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	openFlags := flags | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if flags&unix.O_PATH == 0 {
		openFlags |= unix.O_NONBLOCK
	}
	fd, err := unix.Openat2(rootFD, filepath.ToSlash(relativePath), &unix.OpenHow{
		Flags:   uint64(openFlags),
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS,
	})
	if errors.Is(err, unix.ENOSYS) {
		fd, err = openWorkspaceCurrentPathWithoutOpenat2(rootFD, relativePath, flags)
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

func openWorkspaceCurrentPathWithoutOpenat2(rootFD int, relativePath string, finalFlags int) (int, error) {
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
		flags := unix.O_CLOEXEC | unix.O_NOFOLLOW
		if index < len(components)-1 {
			flags |= unix.O_PATH | unix.O_DIRECTORY
		} else {
			flags |= finalFlags
		}
		if flags&unix.O_PATH == 0 {
			flags |= unix.O_NONBLOCK
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
