//go:build !linux

package workspacecurrent

import "os"

func prepareWorkspaceCurrentDirectoryForRename(string, string, string) (*workspaceCurrentDirectoryModeRestore, error) {
	return nil, nil
}

func restoreWorkspaceCurrentDirectoryMode(*os.File, uint32) error {
	return nil
}
