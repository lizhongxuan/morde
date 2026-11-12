//go:build linux

package workspacereplica

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func chownWorkspaceTree(root string, uid, gid uint32) error {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("open Host workspace for ownership update")
	}
	return chownDirectory(fd, uid, gid)
}

func chownAppliedPaths(root string, paths []string, uid, gid uint32) error {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errors.New("open Host workspace for ownership update")
	}
	defer unix.Close(fd)
	for _, value := range paths {
		if value == "." {
			if err := unix.Fchown(fd, int(uid), int(gid)); err != nil {
				return errors.New("set Host workspace directory owner")
			}
			continue
		}
		if value == "" || filepath.IsAbs(value) || filepath.Clean(value) != value || value == ".." || strings.HasPrefix(value, ".."+string(filepath.Separator)) {
			return errors.New("sync returned an invalid workspace path")
		}
		if err := chownAppliedPath(fd, strings.Split(value, string(filepath.Separator)), uid, gid); err != nil {
			return err
		}
	}
	return nil
}

func chownDirectory(fd int, uid, gid uint32) error {
	if err := unix.Fchown(fd, int(uid), int(gid)); err != nil {
		_ = unix.Close(fd)
		return errors.New("set Host workspace directory owner")
	}
	directory := os.NewFile(uintptr(fd), "workspace")
	if directory == nil {
		_ = unix.Close(fd)
		return errors.New("open Host workspace directory")
	}
	entries, err := directory.ReadDir(-1)
	if err != nil {
		_ = directory.Close()
		return errors.New("read Host workspace directory")
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "." || name == ".." {
			continue
		}
		var info unix.Stat_t
		if err := unix.Fstatat(fd, name, &info, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			_ = directory.Close()
			return errors.New("inspect Host workspace entry")
		}
		mode := info.Mode & unix.S_IFMT
		switch mode {
		case unix.S_IFDIR:
			child, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if err != nil {
				_ = directory.Close()
				return errors.New("open Host workspace subdirectory")
			}
			if err := chownDirectory(child, uid, gid); err != nil {
				_ = directory.Close()
				return err
			}
		case unix.S_IFREG:
			child, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
			if err != nil {
				_ = directory.Close()
				return errors.New("open Host workspace file")
			}
			if err := unix.Fchown(child, int(uid), int(gid)); err != nil {
				_ = unix.Close(child)
				_ = directory.Close()
				return errors.New("set Host workspace file owner")
			}
			_ = unix.Close(child)
		case unix.S_IFLNK:
			if err := unix.Fchownat(fd, name, int(uid), int(gid), unix.AT_SYMLINK_NOFOLLOW); err != nil {
				_ = directory.Close()
				return errors.New("set Host workspace link owner")
			}
		}
	}
	if err := directory.Close(); err != nil {
		return errors.New("close Host workspace directory")
	}
	return nil
}

func chownAppliedPath(rootFD int, parts []string, uid, gid uint32) error {
	fd, err := unix.Dup(rootFD)
	if err != nil {
		return errors.New("duplicate Host workspace directory")
	}
	for _, part := range parts[:len(parts)-1] {
		child, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = unix.Close(fd)
		if openErr != nil {
			if errors.Is(openErr, unix.ENOENT) {
				return nil
			}
			return errors.New("open Host workspace path component")
		}
		fd = child
		if err := unix.Fchown(fd, int(uid), int(gid)); err != nil {
			_ = unix.Close(fd)
			return errors.New("set Host workspace directory owner")
		}
	}
	defer unix.Close(fd)
	name := parts[len(parts)-1]
	var info unix.Stat_t
	if err := unix.Fstatat(fd, name, &info, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return errors.New("inspect synchronized Host workspace entry")
	}
	switch info.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		child, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return errors.New("open synchronized Host workspace file")
		}
		defer unix.Close(child)
		if err := unix.Fchown(child, int(uid), int(gid)); err != nil {
			return errors.New("set synchronized Host workspace file owner")
		}
	case unix.S_IFLNK:
		if err := unix.Fchownat(fd, name, int(uid), int(gid), unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return errors.New("set synchronized Host workspace link owner")
		}
	case unix.S_IFDIR:
		child, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return errors.New("open synchronized Host workspace directory")
		}
		if err := unix.Fchown(child, int(uid), int(gid)); err != nil {
			_ = unix.Close(child)
			return errors.New("set synchronized Host workspace directory owner")
		}
		return unix.Close(child)
	}
	return nil
}
