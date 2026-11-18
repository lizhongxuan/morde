//go:build !linux

package workspacecurrent

import (
	"errors"
	"os"

	"morde/server/internal/domain/workspace"
)

func workspaceCurrentDirectoryEntry(*os.File, string) (workspace.DirectoryEntry, error) {
	return workspace.DirectoryEntry{}, errors.New("safe Workspace current directory listing requires Linux openat2")
}
