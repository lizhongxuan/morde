//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"morde-sync/client"
)

func TestMain(m *testing.M) {
	if runner := os.Getenv("MORDE_SYNC_BIN"); runner != "" {
		hubCommandExecutable = func() (string, error) { return runner, nil }
	}
	os.Exit(m.Run())
}

func TestHubCommandRunsInWorkspaceAndStopsDetachedDescendants(t *testing.T) {
	if !probeHubCommand(context.Background()) {
		t.Skip("Linux command sandbox or user systemd manager unavailable")
	}
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside-secret")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	selection, err := client.SelectHubRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	outsideWrite := filepath.Join(parent, "outside-write")
	command := fmt.Sprintf("printf 'inside\\n' > allowed.txt; cat allowed.txt; if cat %s >/dev/null 2>&1; then exit 41; fi; if printf bad > %s 2>/dev/null; then exit 42; fi; if cat /proc/self/status >/dev/null 2>&1; then exit 43; fi; setsid sleep 60 >/dev/null 2>&1 & echo $! > child.pid",
		shellQuote(outside), shellQuote(outsideWrite))
	started := time.Now()
	result, err := runHubCommand(context.Background(), workspace, selection.Identity, command, 10_000)
	if err != nil {
		t.Fatalf("runHubCommand() error = %v", err)
	}
	if result.ExitCode != 0 || result.TimedOut || !result.Stopped || result.Stdout != "inside\n" {
		t.Fatalf("command result = %+v", result)
	}
	if time.Since(started) > 8*time.Second {
		t.Fatalf("command did not stop its detached descendant promptly: %s", time.Since(started))
	}
	if content, err := os.ReadFile(filepath.Join(workspace, "allowed.txt")); err != nil || string(content) != "inside\n" {
		t.Fatalf("workspace write = %q, %v", content, err)
	}
	if _, err := os.Stat(outsideWrite); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("command wrote outside the workspace: stat err=%v", err)
	}
	pidBytes, err := os.ReadFile(filepath.Join(workspace, "child.pid"))
	if err != nil {
		t.Fatalf("read descendant PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid <= 0 {
		t.Fatalf("descendant PID = %q, err=%v", pidBytes, err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
	}
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("setsid descendant %d survived command completion", pid)
	}
}

func TestHubCommandTimeoutStopsDetachedDescendants(t *testing.T) {
	if !probeHubCommand(context.Background()) {
		t.Skip("Linux command sandbox or user systemd manager unavailable")
	}
	workspace := t.TempDir()
	selection, err := client.SelectHubRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	command := `setsid sleep 60 >/dev/null 2>&1 & echo $! > child.pid; sleep 60`
	started := time.Now()
	result, err := runHubCommand(context.Background(), workspace, selection.Identity, command, 1000)
	if err != nil {
		t.Fatalf("runHubCommand() error = %v", err)
	}
	if !result.TimedOut || !result.Stopped || result.ExitCode != 124 {
		t.Fatalf("timeout result = %+v", result)
	}
	if time.Since(started) > 8*time.Second {
		t.Fatalf("timeout did not stop the command promptly: %s", time.Since(started))
	}
	assertHubCommandChildExited(t, filepath.Join(workspace, "child.pid"))
}

func TestHubCommandCancellationStopsDetachedDescendants(t *testing.T) {
	if !probeHubCommand(context.Background()) {
		t.Skip("Linux command sandbox or user systemd manager unavailable")
	}
	workspace := t.TempDir()
	selection, err := client.SelectHubRoot(workspace)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := runHubCommand(ctx, workspace, selection.Identity, `setsid sleep 60 >/dev/null 2>&1 & echo $! > child.pid; sleep 60`, 60_000)
		result <- err
	}()
	pidFile := filepath.Join(workspace, "child.pid")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidFile); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(pidFile); err != nil {
		cancel()
		t.Fatalf("command did not start: %v", err)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled command error = %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled command did not finish")
	}
	assertHubCommandChildExited(t, pidFile)
}

func assertHubCommandChildExited(t *testing.T, pidFile string) {
	t.Helper()
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read descendant PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil || pid <= 0 {
		t.Fatalf("descendant PID = %q, err=%v", pidBytes, err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
	}
	if err := syscall.Kill(pid, 0); err == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("setsid descendant %d survived command stop", pid)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
