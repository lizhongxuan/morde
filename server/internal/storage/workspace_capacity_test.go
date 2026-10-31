package storage

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"

	"morde/server/internal/domain"
)

func TestWorkspaceCapacity(t *testing.T) {
	root := t.TempDir()
	capacity, err := NewWorkspaceCapacity(root, 1)
	if err != nil {
		t.Fatalf("NewWorkspaceCapacity() error = %v", err)
	}
	if err := capacity.CheckCapacity(context.Background()); err != nil {
		t.Fatalf("CheckCapacity() error = %v", err)
	}

	capacity.minFreeBytes = math.MaxInt64
	if err := capacity.CheckCapacity(context.Background()); domain.CodeOf(err) != domain.CodeCapacityExceeded {
		t.Fatalf("CheckCapacity(low space) error = %v, want capacity_exceeded", err)
	}
}

func TestNewWorkspaceCapacityValidatesInputs(t *testing.T) {
	for _, test := range []struct {
		root    string
		minFree int64
	}{
		{root: "relative", minFree: 1},
		{root: filepath.Join(string(filepath.Separator)), minFree: 1},
		{root: t.TempDir(), minFree: 0},
	} {
		if _, err := NewWorkspaceCapacity(test.root, test.minFree); err == nil {
			t.Fatalf("NewWorkspaceCapacity(%q, %d) succeeded", test.root, test.minFree)
		}
	}
}

func TestWorkspaceCapacityHonorsCancellation(t *testing.T) {
	capacity, err := NewWorkspaceCapacity(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := capacity.CheckCapacity(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckCapacity(cancelled) error = %v, want context.Canceled", err)
	}
}

func TestWorkspaceSyncStorageRequiresDedicatedFilesystems(t *testing.T) {
	root := t.TempDir()
	err := CheckWorkspaceSyncStorageIsolation(
		filepath.Join(root, "blocks"), filepath.Join(root, "current"), filepath.Join(root, "docker"), filepath.Join(root, "site"),
		filepath.Join(root, "nodes"), filepath.Join(root, "relay"), []string{filepath.Join(root, "postgres")},
	)
	if err == nil {
		t.Fatal("Workspace sync storage on the shared filesystem was accepted")
	}
}

func TestWorkspaceCurrentStorageRemainsIsolatedWhenSyncIsDisabled(t *testing.T) {
	root := t.TempDir()
	err := CheckWorkspaceCurrentStorageIsolation(
		filepath.Join(root, "blocks"), filepath.Join(root, "current"), filepath.Join(root, "docker"), filepath.Join(root, "site"),
		[]string{filepath.Join(root, "postgres")},
	)
	if err == nil {
		t.Fatal("Workspace current files on the shared filesystem were accepted")
	}
}

func TestWorkspaceSyncRootsCannotShareFilesystem(t *testing.T) {
	devices := map[uint64]string{1: "Workspace blocks"}
	if err := addWorkspaceSyncFilesystemDevice(devices, 2, "Workspace current files"); err != nil {
		t.Fatalf("record distinct current filesystem: %v", err)
	}
	if err := addWorkspaceSyncFilesystemDevice(devices, 2, "Workspace node state"); err == nil {
		t.Fatal("Workspace current and node roots sharing one filesystem were accepted")
	}
}

func TestMountInfoHasPathUnescapesMountPoint(t *testing.T) {
	mountInfo := []byte("36 25 8:1 / /srv/morde\\040sync rw,relatime - ext4 /dev/sda1 rw\n")
	if !mountInfoHasPath(mountInfo, "/srv/morde sync") {
		t.Fatal("mountinfo escaped mount point was not recognized")
	}
	if mountInfoHasPath(mountInfo, "/srv/morde") {
		t.Fatal("mountinfo accepted a partial mount point")
	}
}
