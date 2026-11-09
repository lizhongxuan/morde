//go:build !linux

package runtimeapplications

import (
	"context"
	"errors"
)

func runApplicationCommand(context.Context, applicationCommand) applicationCommandOutcome {
	return applicationCommandOutcome{err: errors.New("application installation requires Linux")}
}
