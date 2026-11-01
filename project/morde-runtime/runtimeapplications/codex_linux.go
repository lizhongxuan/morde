//go:build linux

package runtimeapplications

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func runCodexProcess(ctx context.Context, spec commandSpec) commandOutcome {
	command := exec.CommandContext(ctx, spec.path, spec.args...)
	command.Dir = spec.dir
	command.Env = spec.env
	command.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: spec.uid, Gid: spec.gid, NoSetGroups: true},
		Setpgid:    true,
	}
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
	command.WaitDelay = 500 * time.Millisecond
	stdout := &limitedOutput{limit: maxVersionBytes}
	stderr := &limitedOutput{limit: maxVersionBytes}
	if spec.captureStdout {
		command.Stdout = stdout
	} else {
		command.Stdout = io.Discard
	}
	if spec.captureStderr {
		command.Stderr = stderr
	} else {
		command.Stderr = io.Discard
	}
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			return commandOutcome{
				stdout: stdout.data, stderr: stderr.data, exitCode: exitError.ExitCode(),
				truncated: stdout.truncated, stderrTruncated: stderr.truncated,
			}
		}
		return commandOutcome{err: err}
	}
	return commandOutcome{
		stdout: stdout.data, stderr: stderr.data, exitCode: 0,
		truncated: stdout.truncated, stderrTruncated: stderr.truncated,
	}
}

type limitedOutput struct {
	limit     int
	data      []byte
	truncated bool
}

func (w *limitedOutput) Write(value []byte) (int, error) {
	written := 0
	if len(w.data) < w.limit {
		remaining := w.limit - len(w.data)
		if len(value) < remaining {
			remaining = len(value)
		}
		w.data = append(w.data, value[:remaining]...)
		written = remaining
	}
	if len(value) > written {
		w.truncated = true
	}
	return len(value), nil
}

var _ io.Writer = (*limitedOutput)(nil)
