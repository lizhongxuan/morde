//go:build linux

package workspacecurrent

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func openWorkspaceCurrentParent(root, relativePath string, createParents bool) (*os.File, string, error) {
	parts := strings.Split(filepath.ToSlash(relativePath), "/")
	if len(parts) < 3 {
		return nil, "", ErrUnsafePath
	}
	workspaceDir, err := openWorkspaceCurrentFile(root, filepath.Join(parts[0], parts[1]))
	if err != nil {
		return nil, "", err
	}
	parentFD, err := unix.Dup(int(workspaceDir.Fd()))
	closeErr := workspaceDir.Close()
	if err != nil || closeErr != nil {
		if err == nil {
			err = closeErr
		}
		return nil, "", err
	}
	for _, component := range parts[2 : len(parts)-1] {
		if component == "" || component == "." || component == ".." {
			_ = unix.Close(parentFD)
			return nil, "", ErrUnsafePath
		}
		nextFD, openErr := unix.Openat(parentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if errors.Is(openErr, unix.ENOENT) && createParents {
			if mkdirErr := unix.Mkdirat(parentFD, component, 0750); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				_ = unix.Close(parentFD)
				return nil, "", mkdirErr
			}
			if syncErr := unix.Fsync(parentFD); syncErr != nil {
				_ = unix.Close(parentFD)
				return nil, "", syncErr
			}
			nextFD, openErr = unix.Openat(parentFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		}
		if openErr != nil {
			_ = unix.Close(parentFD)
			if errors.Is(openErr, unix.ELOOP) || errors.Is(openErr, unix.ENOTDIR) {
				return nil, "", ErrUnsafePath
			}
			return nil, "", openErr
		}
		if err := unix.Close(parentFD); err != nil {
			_ = unix.Close(nextFD)
			return nil, "", err
		}
		parentFD = nextFD
	}
	return os.NewFile(uintptr(parentFD), parts[len(parts)-2]), parts[len(parts)-1], nil
}

func createWorkspaceCurrentDirectory(root, relativePath string, mode uint32) error {
	if mode > 0777 {
		return ErrUnsafePath
	}
	parent, name, err := openWorkspaceCurrentParent(root, relativePath, true)
	if err != nil {
		return err
	}
	defer parent.Close()
	err = unix.Mkdirat(int(parent.Fd()), name, mode&0777)
	if errors.Is(err, unix.EEXIST) {
		var stat unix.Stat_t
		if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return ErrWriteConflict
		}
		return nil
	}
	if err != nil {
		return err
	}
	return unix.Fsync(int(parent.Fd()))
}

func removeWorkspaceCurrentEmptyDirectory(root, relativePath string) error {
	parent, name, err := openWorkspaceCurrentParent(root, relativePath, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	var stat unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return ErrUnsafePath
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		if errors.Is(err, unix.ENOTEMPTY) || errors.Is(err, unix.EEXIST) {
			return ErrWriteConflict
		}
		return err
	}
	return unix.Fsync(int(parent.Fd()))
}

func removeWorkspaceCurrentRegularFile(root, relativePath, expectedSHA256 string) error {
	parent, name, err := openWorkspaceCurrentParent(root, relativePath, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	var before unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFREG {
		return ErrUnsafePath
	}
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	hasher := sha256.New()
	_, readErr := io.Copy(hasher, file)
	var opened unix.Stat_t
	statErr := unix.Fstat(fd, &opened)
	closeErr := file.Close()
	if readErr != nil || statErr != nil || closeErr != nil {
		return errors.Join(readErr, statErr, closeErr)
	}
	if hex.EncodeToString(hasher.Sum(nil)) != expectedSHA256 || before.Dev != opened.Dev || before.Ino != opened.Ino {
		return ErrWriteConflict
	}
	var current unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if current.Mode&unix.S_IFMT != unix.S_IFREG || current.Dev != opened.Dev || current.Ino != opened.Ino {
		return ErrWriteConflict
	}
	if err := unix.Unlinkat(int(parent.Fd()), name, 0); err != nil {
		return err
	}
	return unix.Fsync(int(parent.Fd()))
}

func writeWorkspaceCurrentSymbolicLink(root, relativePath, target, expectedTarget string) error {
	parent, name, err := openWorkspaceCurrentParent(root, relativePath, true)
	if err != nil {
		return err
	}
	defer parent.Close()
	parentFD := int(parent.Fd())
	if current, linkErr := readWorkspaceCurrentLinkAt(parentFD, name); linkErr == nil {
		if current == target {
			return nil
		}
		if expectedTarget == "" || current != expectedTarget {
			return ErrWriteConflict
		}
	} else if !errors.Is(linkErr, unix.ENOENT) {
		return linkErr
	} else if expectedTarget != "" {
		return ErrWriteConflict
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := ".morde-link-" + hex.EncodeToString(nonce[:])
	if err := unix.Symlinkat(target, parentFD, temporary); err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = unix.Unlinkat(parentFD, temporary, 0)
		}
	}()
	if expectedTarget == "" {
		if err := unix.Renameat2(parentFD, temporary, parentFD, name, unix.RENAME_NOREPLACE); err != nil {
			return err
		}
		removeTemporary = false
		return unix.Fsync(parentFD)
	}
	if err := unix.Renameat2(parentFD, temporary, parentFD, name, unix.RENAME_EXCHANGE); err != nil {
		return err
	}
	previous, err := readWorkspaceCurrentLinkAt(parentFD, temporary)
	if err != nil || previous != expectedTarget {
		current, currentErr := readWorkspaceCurrentLinkAt(parentFD, name)
		if currentErr == nil && current == target {
			if restoreErr := unix.Renameat2(parentFD, temporary, parentFD, name, unix.RENAME_EXCHANGE); restoreErr == nil {
				return ErrWriteConflict
			}
		}
		removeTemporary = false
		return errors.Join(ErrWriteConflict, err)
	}
	if err := unix.Unlinkat(parentFD, temporary, 0); err != nil {
		removeTemporary = false
		return err
	}
	removeTemporary = false
	return unix.Fsync(parentFD)
}

func removeWorkspaceCurrentSymbolicLink(root, relativePath, expectedTarget string) error {
	parent, name, err := openWorkspaceCurrentParent(root, relativePath, false)
	if err != nil {
		return err
	}
	defer parent.Close()
	parentFD := int(parent.Fd())
	target, err := readWorkspaceCurrentLinkAt(parentFD, name)
	if err != nil {
		return err
	}
	if target != expectedTarget {
		return ErrWriteConflict
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(parentFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFLNK {
		return ErrUnsafePath
	}
	if err := unix.Unlinkat(parentFD, name, 0); err != nil {
		return err
	}
	return unix.Fsync(parentFD)
}

func readWorkspaceCurrentLinkAt(parentFD int, name string) (string, error) {
	buffer := make([]byte, 4096)
	count, err := unix.Readlinkat(parentFD, name, buffer)
	if err != nil {
		return "", err
	}
	if count == len(buffer) {
		return "", ErrUnsafePath
	}
	return string(buffer[:count]), nil
}
