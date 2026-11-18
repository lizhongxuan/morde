package workspacecurrent

import "os"

type workspaceCurrentDirectoryModeRestore struct {
	file *os.File
	mode uint32
}

func (r *workspaceCurrentDirectoryModeRestore) restore() error {
	if r == nil || r.file == nil {
		return nil
	}
	return restoreWorkspaceCurrentDirectoryMode(r.file, r.mode)
}

func (r *workspaceCurrentDirectoryModeRestore) close() error {
	if r == nil || r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}
