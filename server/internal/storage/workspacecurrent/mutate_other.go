//go:build !linux

package workspacecurrent

import "errors"

func createWorkspaceCurrentDirectory(string, string, uint32) error {
	return errors.New("safe Workspace current mutations require Linux openat2")
}

func removeWorkspaceCurrentEmptyDirectory(string, string) error {
	return errors.New("safe Workspace current mutations require Linux openat2")
}

func removeWorkspaceCurrentRegularFile(string, string, string) error {
	return errors.New("safe Workspace current mutations require Linux openat2")
}

func moveWorkspaceCurrentRegularFile(string, string, string, string, uint32) error {
	return errors.New("safe Workspace current mutations require Linux openat2")
}

func writeWorkspaceCurrentSymbolicLink(string, string, string, string) error {
	return errors.New("safe Workspace current mutations require Linux openat2")
}

func removeWorkspaceCurrentSymbolicLink(string, string, string) error {
	return errors.New("safe Workspace current mutations require Linux openat2")
}
