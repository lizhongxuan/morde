//go:build linux

package runtimeapplications

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
)

func runApplicationCommand(ctx context.Context, spec applicationCommand) applicationCommandOutcome {
	command := exec.CommandContext(ctx, spec.path, spec.args...)
	command.Env = []string{"HOME=/root", "PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	var stdout *applicationCapture
	if spec.captureStdout {
		stdout = &applicationCapture{limit: spec.captureLimit}
		command.Stdout = stdout
	} else {
		command.Stdout = io.Discard
	}
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return applicationCommandOutcome{stdout: captureBytes(stdout), exitCode: exitError.ExitCode()}
		}
		return applicationCommandOutcome{err: err}
	}
	if stdout != nil && stdout.truncated {
		return applicationCommandOutcome{stdout: stdout.data, err: errInvalidPackage}
	}
	return applicationCommandOutcome{stdout: captureBytes(stdout)}
}

func captureBytes(output *applicationCapture) []byte {
	if output == nil {
		return nil
	}
	return output.data
}

type applicationCapture struct {
	limit     int
	data      []byte
	truncated bool
}

func (w *applicationCapture) Write(value []byte) (int, error) {
	if len(w.data) < w.limit {
		count := w.limit - len(w.data)
		if count > len(value) {
			count = len(value)
		}
		w.data = append(w.data, value[:count]...)
		if count < len(value) {
			w.truncated = true
		}
	} else if len(value) > 0 {
		w.truncated = true
	}
	return len(value), nil
}
