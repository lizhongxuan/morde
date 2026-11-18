//go:build !linux

package workspacecurrent

import "errors"

func readWorkspaceCurrentSymbolicLink(string, string, string) (string, error) {
	return "", errors.New("safe Workspace current symbolic link access requires Linux openat2")
}
