//go:build linux

package workspacecurrent

import (
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"morde/server/internal/domain/workspace"
)

func workspaceCurrentDirectoryEntry(directory *os.File, name string) (workspace.DirectoryEntry, error) {
	if directory == nil || name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
		return workspace.DirectoryEntry{}, ErrUnsafePath
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return workspace.DirectoryEntry{}, err
	}
	kind, size := "other", int64(0)
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		kind, size = "file", stat.Size
	case unix.S_IFDIR:
		kind = "directory"
	case unix.S_IFLNK:
		kind = "symlink"
	}
	return workspace.DirectoryEntry{Name: name, Kind: kind, SizeBytes: size,
		ModifiedAt: time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec).UTC()}, nil
}
