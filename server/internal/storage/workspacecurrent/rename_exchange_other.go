//go:build !linux

package workspacecurrent

import "errors"

func renameExchange(_, _ string) error {
	return errors.New("atomic Workspace directory exchange requires Linux renameat2")
}
