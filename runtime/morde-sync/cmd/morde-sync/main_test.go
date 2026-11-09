package main

import (
	"path/filepath"
	"testing"
)

func TestProbeOperationLockExitCodes(t *testing.T) {
	directory := t.TempDir()
	if status := probeOperationLock(directory); status != 0 {
		t.Fatalf("probe free directory status = %d, want 0", status)
	}
	missing := filepath.Join(directory, "missing")
	if status := probeOperationLock(missing); status != 2 {
		t.Fatalf("probe invalid directory status = %d, want 2", status)
	}
}
