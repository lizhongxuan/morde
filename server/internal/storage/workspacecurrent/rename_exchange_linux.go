//go:build linux

package workspacecurrent

import "golang.org/x/sys/unix"

func renameExchange(first, second string) error {
	return unix.Renameat2(unix.AT_FDCWD, first, unix.AT_FDCWD, second, unix.RENAME_EXCHANGE)
}
