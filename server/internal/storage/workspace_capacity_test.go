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
