//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"morde-sync/client"
)

const hubCommandOutputLimit = 16 << 10

var hubCommandExecutable = os.Executable

func commandSignalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func runHubCommandRunner(workspace, expectedIdentity, privateTemp, command string) error {
	selection, err := client.SelectHubRoot(workspace)
	if err != nil || selection.Directory != workspace || selection.Identity != expectedIdentity {
		return errors.New("invalid command workspace")
	}
	if !privateCommandDirectory(privateTemp) {
		return errors.New("invalid command temporary directory")
	}
	return client.ExecHubCommandLandlocked(workspace, expectedIdentity, privateTemp,
		[]string{"/bin/sh", "-c", command},
		[]string{"PATH=/usr/bin:/bin", "HOME=" + privateTemp, "TMPDIR=" + privateTemp, "LANG=C.UTF-8", "LC_ALL=C"})
}

func privateCommandDirectory(directory string) bool {
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return false
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func runHubCommand(parent context.Context, workspace, expectedIdentity, command string, timeoutMS int) (hubCommandResult, error) {
	if parent == nil || len(expectedIdentity) != 64 || timeoutMS < 1000 || timeoutMS > 300000 || command == "" || !utf8.ValidString(command) || strings.ContainsRune(command, '\x00') || len(command) > 8<<10 {
		return hubCommandResult{}, errors.New("invalid command request")
	}
	selection, err := client.SelectHubRoot(workspace)
	if err != nil || selection.Directory != workspace || selection.Identity != expectedIdentity {
		return hubCommandResult{}, errors.New("invalid command workspace")
	}
	privateTemp, err := os.MkdirTemp("", "morde-hub-command-")
	if err != nil {
		return hubCommandResult{}, errors.New("command temporary directory unavailable")
	}
	removePrivateTemp := true
	defer func() {
		if removePrivateTemp {
			_ = os.RemoveAll(privateTemp)
		}
	}()
	if err := os.Chmod(privateTemp, 0o700); err != nil || !privateCommandDirectory(privateTemp) {
		return hubCommandResult{}, errors.New("command temporary directory unavailable")
	}

	serviceManagerEnv, err := commandServiceManagerEnv()
	if err != nil {
		return hubCommandResult{}, err
	}
	systemdRun, err := systemCommand("systemd-run")
	if err != nil {
		return hubCommandResult{}, err
	}
	systemctl, err := systemCommand("systemctl")
	if err != nil {
		return hubCommandResult{}, err
	}
	executable, err := hubCommandExecutable()
	if err != nil || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return hubCommandResult{}, errors.New("command helper unavailable")
	}
	info, err := os.Lstat(executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		return hubCommandResult{}, errors.New("command helper unavailable")
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil || !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return hubCommandResult{}, errors.New("command helper unavailable")
	}
	executable = resolved
	unitID, err := newHubCommandUnitID()
	if err != nil {
		return hubCommandResult{}, errors.New("command service unavailable")
	}
	unit := "morde-hub-command-" + unitID + ".service"
	maxRuntime := (timeoutMS+999)/1000 + 15

	commandCtx, cancel := context.WithTimeout(parent, time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()
	var stopOnce sync.Once
	var stopErr error
	stop := func() error {
		stopOnce.Do(func() {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer stopCancel()
			stopErr = stopHubCommandUnit(stopCtx, systemctl, unit, serviceManagerEnv)
		})
		return stopErr
	}

	stdout := &hubCommandOutput{cancel: cancel}
	stderr := &hubCommandOutput{cancel: cancel}
	args := []string{
		"--user", "--quiet", "--wait", "--pipe", "--collect", "--service-type=exec", "--unit=" + unit,
		"--property=KillMode=control-group", "--property=RuntimeMaxSec=" + fmt.Sprintf("%ds", maxRuntime),
		"--property=TimeoutStopSec=5s", "--property=TasksMax=128", "--property=MemoryMax=1G", "--property=LimitCORE=0",
		"--working-directory=" + workspace,
		"--setenv=PATH=/usr/bin:/bin", "--setenv=HOME=" + privateTemp, "--setenv=TMPDIR=" + privateTemp,
		"--setenv=LANG=C.UTF-8", "--setenv=LC_ALL=C",
		"--", executable, "--hub-command-runner", workspace, expectedIdentity, privateTemp, command,
	}
	process := exec.CommandContext(commandCtx, systemdRun, args...)
	process.Env = serviceManagerEnv
	process.Stdout = stdout
	process.Stderr = stderr
	process.Cancel = stop
	process.WaitDelay = 12 * time.Second
	if err := process.Run(); err != nil && !errors.Is(commandCtx.Err(), context.DeadlineExceeded) && parent.Err() == nil && process.ProcessState == nil {
		return hubCommandResult{}, errors.New("command sandbox could not start")
	}
	stopped, err := waitHubCommandStopped(context.Background(), systemctl, unit, serviceManagerEnv)
	if err != nil || !stopped {
		removePrivateTemp = false
		return hubCommandResult{TimedOut: true, Stopped: false}, nil
	}
	if stdout.overflowed() || stderr.overflowed() {
		return hubCommandResult{}, errors.New("command output exceeds the limit")
	}
	if parent.Err() != nil {
		return hubCommandResult{}, parent.Err()
	}

	timedOut := errors.Is(commandCtx.Err(), context.DeadlineExceeded)
	exitCode := 0
	if process.ProcessState != nil {
		exitCode = process.ProcessState.ExitCode()
	}
	if timedOut {
		exitCode = 124
	} else if exitCode < 0 || exitCode > 255 {
		return hubCommandResult{}, errors.New("command exit status unavailable")
	}
	out, errOut := stdout.String(), stderr.String()
	if !utf8.ValidString(out) || !utf8.ValidString(errOut) {
		return hubCommandResult{}, errors.New("command output is not UTF-8 text")
	}
	return hubCommandResult{ExitCode: exitCode, Stdout: out, Stderr: errOut, TimedOut: timedOut, Stopped: true}, nil
}

func probeHubCommand(ctx context.Context) bool {
	if ctx == nil || ctx.Err() != nil {
		return false
	}
	workspace, err := os.MkdirTemp("", "morde-hub-command-probe-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(workspace)
	if err := os.Chmod(workspace, 0o700); err != nil {
		return false
	}
	selection, err := client.SelectHubRoot(workspace)
	if err != nil {
		return false
	}
	result, err := runHubCommand(ctx, workspace, selection.Identity, ":", 5000)
	return err == nil && result.ExitCode == 0 && result.Stopped && !result.TimedOut
}

func commandServiceManagerEnv() ([]string, error) {
	if os.Geteuid() == 0 {
		return nil, errors.New("user service manager unavailable")
	}
	runtimeDir := filepath.Join("/run/user", fmt.Sprint(os.Getuid()))
	info, err := os.Lstat(runtimeDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("user service manager unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return nil, errors.New("user service manager unavailable")
	}
	bus := filepath.Join(runtimeDir, "bus")
	busInfo, err := os.Lstat(bus)
	if err != nil || busInfo.Mode()&os.ModeSocket == 0 || busInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("user service manager unavailable")
	}
	return []string{
		"PATH=/usr/bin:/bin", "LANG=C", "XDG_RUNTIME_DIR=" + runtimeDir,
		"DBUS_SESSION_BUS_ADDRESS=unix:path=" + bus,
	}, nil
}

func systemCommand(name string) (string, error) {
	for _, directory := range []string{"/usr/bin", "/bin"} {
		path := filepath.Join(directory, name)
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !filepath.IsAbs(resolved) {
			continue
		}
		info, err := os.Stat(resolved)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return resolved, nil
		}
	}
	return "", errors.New("user service manager unavailable")
}

func newHubCommandUnitID() (string, error) {
	var bytesID [16]byte
	if _, err := rand.Read(bytesID[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", bytesID[:]), nil
}

func stopHubCommandUnit(ctx context.Context, systemctl, unit string, env []string) error {
	stop := exec.CommandContext(ctx, systemctl, "--user", "stop", unit)
	stop.Env = env
	if err := stop.Run(); err != nil {
		stopped, checkErr := hubCommandUnitStopped(ctx, systemctl, unit, env)
		if checkErr != nil || !stopped {
			return errors.New("command stop could not be confirmed")
		}
	}
	stopped, err := waitHubCommandStopped(ctx, systemctl, unit, env)
	if err != nil || !stopped {
		return errors.New("command stop could not be confirmed")
	}
	return nil
}

func waitHubCommandStopped(parent context.Context, systemctl, unit string, env []string) (bool, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	for {
		stopped, err := hubCommandUnitStopped(ctx, systemctl, unit, env)
		if err != nil {
			return false, err
		}
		if stopped {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func hubCommandUnitStopped(ctx context.Context, systemctl, unit string, env []string) (bool, error) {
	load := exec.CommandContext(ctx, systemctl, "--user", "show", "--property=LoadState", "--value", unit)
	load.Env = env
	loadOutput, err := load.Output()
	if err != nil {
		return false, errors.New("user service manager state unavailable")
	}
	if strings.TrimSpace(string(loadOutput)) == "not-found" {
		return true, nil
	}
	state := exec.CommandContext(ctx, systemctl, "--user", "show", "--property=ActiveState", "--value", unit)
	state.Env = env
	stateOutput, err := state.Output()
	if err != nil {
		return false, errors.New("user service manager state unavailable")
	}
	switch strings.TrimSpace(string(stateOutput)) {
	case "inactive", "failed":
		return true, nil
	default:
		return false, nil
	}
}

type hubCommandOutput struct {
	mu       sync.Mutex
	data     bytes.Buffer
	cancel   context.CancelFunc
	overflow bool
}

func (w *hubCommandOutput) Write(value []byte) (int, error) {
	w.mu.Lock()
	remaining := hubCommandOutputLimit - w.data.Len()
	if remaining < len(value) {
		if remaining > 0 {
			_, _ = w.data.Write(value[:remaining])
		}
		w.overflow = true
		w.mu.Unlock()
		w.cancel()
		return len(value), nil
	}
	_, _ = w.data.Write(value)
	w.mu.Unlock()
	return len(value), nil
}

func (w *hubCommandOutput) overflowed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.overflow
}

func (w *hubCommandOutput) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.String()
}
