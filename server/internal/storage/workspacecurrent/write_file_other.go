//go:build !linux

package workspacecurrent

import (
	"context"
	"errors"
	"io"
)

func writeWorkspaceCurrentFileFrom(context.Context, string, string, io.Reader, int64, string, uint32, string) error {
	return errors.New("safe Workspace current file writes require Linux openat2")
}
