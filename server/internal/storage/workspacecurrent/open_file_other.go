//go:build !linux

package workspacecurrent

import (
	"errors"
	"os"
)

func openWorkspaceCurrentFile(string, string) (*os.File, error) {
	return nil, errors.New("safe Workspace current file access requires Linux openat2")
}
