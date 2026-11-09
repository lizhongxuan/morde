//go:build !linux

package workspacereplica

import "errors"

func chownWorkspaceTree(string, uint32, uint32) error {
	return errors.New("Host workspace ownership updates require Linux")
}

func chownAppliedPaths(string, []string, uint32, uint32) error {
	return errors.New("Host workspace ownership updates require Linux")
}
