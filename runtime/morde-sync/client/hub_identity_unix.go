//go:build darwin || linux

package client

import (
	"fmt"
	"os"
	"syscall"
)

func hubDirectoryFileIdentity(root *os.Root) (string, error) {
	info, err := root.Stat(".")
	if err != nil || !info.IsDir() {
		return "", errHubInspect
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return "", errHubInspect
	}
	return fmt.Sprintf("unix:%d:%d", stat.Dev, stat.Ino), nil
}
