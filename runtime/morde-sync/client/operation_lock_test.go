package client

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestWorkspaceOperationLockSubprocessHelper(t *testing.T) {
	if os.Getenv("MORDE_SYNC_LOCK_PROCESS_HELPER") != "1" {
		return
	}
	release, err := acquireOperationLock(context.Background(), os.Getenv("MORDE_SYNC_LOCK_DIRECTORY"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := os.WriteFile(os.Getenv("MORDE_SYNC_LOCK_READY"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(os.Getenv("MORDE_SYNC_LOCK_RELEASE")); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the parent process to release the lock")
}

func TestWorkspaceOperationLockExcludesAnotherProcess(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "workspace")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	readyPath := filepath.Join(root, "ready")
	releasePath := filepath.Join(root, "release")
	command := exec.Command(os.Args[0], "-test.run=^TestWorkspaceOperationLockSubprocessHelper$")
	command.Env = append(os.Environ(),
		"MORDE_SYNC_LOCK_PROCESS_HELPER=1",
		"MORDE_SYNC_LOCK_DIRECTORY="+directory,
		"MORDE_SYNC_LOCK_READY="+readyPath,
		"MORDE_SYNC_LOCK_RELEASE="+releasePath,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyPath); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(readyPath); err != nil {
		t.Fatalf("child process did not acquire the lock: %v", err)
	}
	if held, err := IsOperationLockHeld(context.Background(), directory); err != nil || !held {
		t.Fatalf("lock probe while child holds lock = %t, error = %v", held, err)
	}
	if _, err := acquireOperationLock(context.Background(), directory); err == nil || err.Error() != "another Morde sync operation is active for this directory" {
		t.Fatalf("parent acquired a lock held by another process: %v", err)
	}
	if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- command.Wait() }()
	select {
	case err := <-result:
		waited = true
		if err != nil {
			t.Fatalf("child process: %v", err)
		}
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		<-result
		waited = true
		t.Fatal("child process did not release the lock")
	}
	release, err := acquireOperationLock(context.Background(), directory)
	if err != nil {
		t.Fatalf("parent could not acquire the released lock: %v", err)
	}
	release()
	if held, err := IsOperationLockHeld(context.Background(), directory); err != nil || held {
		t.Fatalf("lock probe after child exit = %t, error = %v", held, err)
	}
}

func TestWorkspaceOperationLockSerializesAndReleases(t *testing.T) {
	directory := t.TempDir()
	release, err := acquireOperationLock(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireOperationLock(context.Background(), directory); err == nil || err.Error() != "another Morde sync operation is active for this directory" {
		t.Fatalf("second operation lock error = %v", err)
	}

	release()
	next, err := acquireOperationLock(context.Background(), directory)
	if err != nil {
		t.Fatalf("acquire lock after release: %v", err)
	}
	next()
}

func TestWorkspaceOperationLockSerializesMacOSCaseAliases(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS filesystem behavior only")
	}
	root := t.TempDir()
	project := filepath.Join(root, "Project")
	alias := filepath.Join(root, "PROJECT")
	if err := os.Mkdir(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(alias); err != nil {
		t.Skip("the current macOS volume is case-sensitive")
	}
	project, err := canonicalDirectory(project)
	if err != nil {
		t.Fatal(err)
	}
	alias, err = canonicalDirectory(alias)
	if err != nil {
		t.Fatal(err)
	}
	if project == alias {
		t.Skip("realpath already normalizes case aliases on this macOS volume")
	}

	release, err := acquireOperationLock(context.Background(), project)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := acquireOperationLock(context.Background(), alias); err == nil || err.Error() != "another Morde sync operation is active for this directory" {
		t.Fatalf("case-alias operation lock error = %v", err)
	}
}

func TestWorkspaceOperationLockKeepsCaseSensitiveDirectoriesIndependent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS filesystem behavior only")
	}
	root := t.TempDir()
	upper := filepath.Join(root, "Project")
	lower := filepath.Join(root, "project")
	if err := os.Mkdir(upper, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lower, 0o700); err != nil {
		t.Skip("the current macOS volume is case-insensitive")
	}
	upper, err := canonicalDirectory(upper)
	if err != nil {
		t.Fatal(err)
	}
	lower, err = canonicalDirectory(lower)
	if err != nil {
		t.Fatal(err)
	}
	first, err := acquireOperationLock(context.Background(), upper)
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	second, err := acquireOperationLock(context.Background(), lower)
	if err != nil {
		t.Fatalf("case-distinct directory should have its own lock: %v", err)
	}
	second()
}
