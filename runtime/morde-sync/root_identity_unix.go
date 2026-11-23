//go:build darwin || linux

package mordesync

import (
	"fmt"
	"os"
	"syscall"
)

func matchesSelectedRootIdentity(root *os.Root, device, inode string) bool {
	info, err := root.Stat(".")
	if err != nil || !info.IsDir() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Ino != 0 && fmt.Sprint(stat.Dev) == device && fmt.Sprint(stat.Ino) == inode
}
