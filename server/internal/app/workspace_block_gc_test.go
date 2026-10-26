package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain/workspace"
)

type blockGCTestStore struct {
	workspaceIDs []string
	references   []workspace.BlockReference
	lockBusy     bool
	lockCalls    int
	expireCalls  int
	cutoff       time.Time
}

type blockGCTestLock struct{}

func (blockGCTestLock) Release(context.Context) error { return nil }

func (s *blockGCTestStore) ListWorkspaceIDsForBlockGC(context.Context) ([]string, error) {
	return append([]string(nil), s.workspaceIDs...), nil
}

func (s *blockGCTestStore) TryWorkspaceBlockStoreLock(context.Context, string) (LeaderLock, bool, error) {
	s.lockCalls++
	if s.lockBusy {
		return nil, false, nil
	}
	return blockGCTestLock{}, true, nil
}

func (s *blockGCTestStore) ExpireIdleWorkspaceSyncSessions(_ context.Context, _ string, cutoff time.Time) (int64, error) {
	s.expireCalls++
	s.cutoff = cutoff
	return 2, nil
}

func (s *blockGCTestStore) WithWorkspaceBlockGC(_ context.Context, _ string, collect func([]workspace.BlockReference) ([]WorkspaceBlockRemoval, error)) error {
	_, err := collect(append([]workspace.BlockReference(nil), s.references...))
	return err
}

type blockGCTestFiles struct {
	blocks      map[string]map[string]int64
	listCalls   int
	deleteCalls int
	tempCalls   int
	tempErr     error
}

func (f *blockGCTestFiles) ListBlockNames(workspaceID string) ([]string, error) {
	f.listCalls++
	names := make([]string, 0, len(f.blocks[workspaceID]))
	for name := range f.blocks[workspaceID] {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func (f *blockGCTestFiles) DeleteBlock(workspaceID, digest string) (int64, error) {
	f.deleteCalls++
	size, ok := f.blocks[workspaceID][digest]
	if !ok {
		return 0, nil
	}
	delete(f.blocks[workspaceID], digest)
	return size, nil
}

func (f *blockGCTestFiles) RemoveStaleTempBlocks(string) error {
	f.tempCalls++
	return f.tempErr
}

func TestWorkspaceBlockGarbageCollectorKeepsReferencesAndRemovesOrphans(t *testing.T) {
	keptRevision := strings.Repeat("a", 64)
	activeUpload := strings.Repeat("b", 64)
	orphan := strings.Repeat("c", 64)
	store := &blockGCTestStore{
		workspaceIDs: []string{"w1234567"},
		references: []workspace.BlockReference{
			{WorkspaceID: "w1234567", SHA256: keptRevision, SizeBytes: 8},
			{WorkspaceID: "w1234567", SHA256: activeUpload, SizeBytes: 16},
		},
	}
	files := &blockGCTestFiles{blocks: map[string]map[string]int64{
		"w1234567": {keptRevision: 8, activeUpload: 16, orphan: 32},
	}}
	gc, err := NewWorkspaceBlockGarbageCollector(store, files)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := gc.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.WorkspacesScanned != 1 || stats.SessionsExpired != 2 || stats.BlocksDeleted != 1 || stats.BytesReclaimed != 32 {
		t.Fatalf("Collect() stats = %#v", stats)
	}
	if len(files.blocks["w1234567"]) != 2 || files.blocks["w1234567"][keptRevision] != 8 || files.blocks["w1234567"][activeUpload] != 16 {
		t.Fatalf("GC removed a protected revision/upload block: %v", files.blocks["w1234567"])
	}
	if store.expireCalls != 1 || !store.cutoff.Before(time.Now().UTC().Add(-workspaceSyncIdleTTL+time.Second)) {
		t.Fatalf("expired-session cutoff = %v, calls = %d", store.cutoff, store.expireCalls)
	}
	if files.tempCalls != 1 {
		t.Fatalf("stale temp cleanup calls = %d, want 1", files.tempCalls)
	}
}

func TestWorkspaceBlockGarbageCollectorSkipsWorkspaceWhenLockIsBusy(t *testing.T) {
	digest := strings.Repeat("d", 64)
	store := &blockGCTestStore{workspaceIDs: []string{"w1234567"}, lockBusy: true}
	files := &blockGCTestFiles{blocks: map[string]map[string]int64{"w1234567": {digest: 16}}}
	gc, err := NewWorkspaceBlockGarbageCollector(store, files)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := gc.Collect(context.Background())
	if err != nil || stats != (WorkspaceBlockGCStats{}) {
		t.Fatalf("Collect() = %#v, %v; want zero stats", stats, err)
	}
	if store.expireCalls != 0 || files.listCalls != 0 || files.tempCalls != 0 || files.deleteCalls != 0 {
		t.Fatalf("GC touched workspace while lock was busy: expiry=%d list=%d temp=%d delete=%d", store.expireCalls, files.listCalls, files.tempCalls, files.deleteCalls)
	}
}

func TestWorkspaceBlockGarbageCollectorFailsClosedOnInvalidProtectionSet(t *testing.T) {
	store := &blockGCTestStore{
		workspaceIDs: []string{"w1234567"},
		references:   []workspace.BlockReference{{WorkspaceID: "w8765432", SHA256: strings.Repeat("e", 64), SizeBytes: 8}},
	}
	files := &blockGCTestFiles{blocks: map[string]map[string]int64{"w1234567": {strings.Repeat("f", 64): 16}}}
	gc, err := NewWorkspaceBlockGarbageCollector(store, files)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gc.Collect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "protection reference is invalid") {
		t.Fatalf("Collect() error = %v, want invalid protection reference", err)
	}
	if files.deleteCalls != 0 || len(files.blocks["w1234567"]) != 1 {
		t.Fatalf("GC deleted blocks with an invalid protection set: calls=%d blocks=%v", files.deleteCalls, files.blocks["w1234567"])
	}
}

func TestWorkspaceBlockGarbageCollectorLimitsDeletesPerWorkspaceRun(t *testing.T) {
	blocks := make(map[string]int64, workspaceBlockGCDeletesPerRun+1)
	for i := 0; i <= workspaceBlockGCDeletesPerRun; i++ {
		blocks[fmt.Sprintf("%064x", i+1)] = 1
	}
	store := &blockGCTestStore{workspaceIDs: []string{"w1234567"}}
	files := &blockGCTestFiles{blocks: map[string]map[string]int64{"w1234567": blocks}}
	gc, err := NewWorkspaceBlockGarbageCollector(store, files)
	if err != nil {
		t.Fatal(err)
	}
	first, err := gc.Collect(context.Background())
	if err != nil || first.BlocksDeleted != workspaceBlockGCDeletesPerRun || len(blocks) != 1 {
		t.Fatalf("first Collect() = %#v, %v; remaining=%d", first, err, len(blocks))
	}
	second, err := gc.Collect(context.Background())
	if err != nil || second.BlocksDeleted != 1 || len(blocks) != 0 {
		t.Fatalf("second Collect() = %#v, %v; remaining=%d", second, err, len(blocks))
	}
}

func TestWorkspaceBlockGarbageCollectorReportsTempCleanupFailure(t *testing.T) {
	store := &blockGCTestStore{workspaceIDs: []string{"w1234567"}}
	files := &blockGCTestFiles{blocks: map[string]map[string]int64{"w1234567": {}}, tempErr: errors.New("temp cleanup failed")}
	gc, err := NewWorkspaceBlockGarbageCollector(store, files)
	if err != nil {
		t.Fatal(err)
	}
	_, err = gc.Collect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "temp cleanup failed") {
		t.Fatalf("Collect() error = %v, want temp cleanup failure", err)
	}
}
