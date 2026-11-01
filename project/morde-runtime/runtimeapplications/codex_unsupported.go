//go:build !linux

package runtimeapplications

import (
	"context"
	"errors"
)

func runCodexProcess(context.Context, commandSpec) commandOutcome {
	return commandOutcome{err: errors.New("Codex status probe requires Linux")}
}
