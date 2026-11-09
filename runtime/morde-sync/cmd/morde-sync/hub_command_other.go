//go:build !linux

package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
)

func commandSignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

func runHubCommandRunner(_, _, _, _ string) error {
	return errors.New("command sandbox unavailable")
}

func runHubCommand(context.Context, string, string, string, int) (hubCommandResult, error) {
	return hubCommandResult{}, errors.New("command sandbox unavailable")
}

func probeHubCommand(context.Context) bool { return false }
