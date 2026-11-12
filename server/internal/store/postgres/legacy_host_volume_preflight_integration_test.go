package postgres

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func TestLegacyHostVolumeInventoryPreflightIsReadOnly(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 10_000, 20_000)
	legacyWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "legacy-volume-preflight")
	hostValue := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, []string{legacyWorkspace.ID})
	if _, err := pool.Exec(ctx, `UPDATE workspace_volumes SET state='legacy', legacy_host_id=$2,
volume_name=NULL, ready_at=NULL WHERE workspace_id=$1`, legacyWorkspace.ID, hostValue.ID); err != nil {
		t.Fatalf("mark test Workspace volume legacy: %v", err)
	}
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_volumes`).Scan(&before); err != nil {
		t.Fatalf("count Workspace volumes before preflight: %v", err)
	}
	query, err := os.ReadFile("../../../migrations/preflight/legacy_host_volume_inventory.sql")
	if err != nil {
		t.Fatalf("read legacy Host volume inventory: %v", err)
	}
	rows, err := pool.Query(ctx, string(query))
	if err != nil {
		t.Fatalf("run legacy Host volume inventory: %v", err)
	}
	defer rows.Close()
	wantHostVolumes := map[string]bool{
		domain.WorkspaceRootVolumeName(hostValue.ID): false,
		domain.ProfileVolumeName(hostValue.ID):       false,
		domain.RuntimeVolumeName(hostValue.ID):       false,
		domain.HostAppsVolumeName(hostValue.ID):      false,
	}
	wantComposeVolumes := map[string]bool{
		"workspace-root": false,
		"profile-data":   false,
		"runtime-data":   false,
		"host-apps":      false,
	}
	for rows.Next() {
		var kind, volumeName string
		var ownerID, candidateHostID, hostState *string
		var boundWorkspaceIDs, legacyWorkspaceIDs []string
		var bindingCount int64
		if err := rows.Scan(&kind, &volumeName, &ownerID, &candidateHostID, &hostState,
			&boundWorkspaceIDs, &legacyWorkspaceIDs, &bindingCount); err != nil {
			t.Fatalf("scan legacy Host volume candidate: %v", err)
		}
		if _, ok := wantHostVolumes[volumeName]; ok {
			if ownerID == nil || *ownerID != owner.ID || candidateHostID == nil || *candidateHostID != hostValue.ID ||
				len(boundWorkspaceIDs) != 1 || boundWorkspaceIDs[0] != legacyWorkspace.ID || bindingCount != 1 {
				t.Fatalf("Host volume candidate %q lacks its database owner/binding context: owner=%v host=%v bindings=%v count=%d",
					volumeName, ownerID, candidateHostID, boundWorkspaceIDs, bindingCount)
			}
			wantHostVolumes[volumeName] = true
			continue
		}
		if _, ok := wantComposeVolumes[volumeName]; ok {
			if ownerID != nil || candidateHostID != nil || len(boundWorkspaceIDs) != 0 || len(legacyWorkspaceIDs) != 0 {
				t.Fatalf("unscoped Compose volume candidate %q was given inferred ownership or Workspace mapping: owner=%v host=%v bound=%v legacy=%v",
					volumeName, ownerID, candidateHostID, boundWorkspaceIDs, legacyWorkspaceIDs)
			}
			wantComposeVolumes[volumeName] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read legacy Host volume candidates: %v", err)
	}
	for name, seen := range wantHostVolumes {
		if !seen {
			t.Errorf("missing Host-scoped volume candidate %q", name)
		}
	}
	for name, seen := range wantComposeVolumes {
		if !seen {
			t.Errorf("missing unscoped Compose volume candidate %q", name)
		}
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspace_volumes`).Scan(&after); err != nil || after != before {
		t.Fatalf("read-only inventory changed Workspace volumes: before=%d after=%d err=%v", before, after, err)
	}
}

func TestLegacyHostWorkspacePreflightClassifiesBindingsAndUnmappedVolumesReadOnly(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 10, 10_000, 20_000)
	otherOwner := createTestUser(t, store, 2, 10_000, 20_000)
	capacity := app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}

	zeroBindingWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "zero-binding")
	oneBindingWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "one-binding")
	multiBindingWorkspaceA := createTestWorkspace(t, ctx, store, owner.ID, "multi-binding-a")
	multiBindingWorkspaceB := createTestWorkspace(t, ctx, store, owner.ID, "multi-binding-b")
	mismatchedOwnerWorkspace := createTestWorkspace(t, ctx, store, otherOwner.ID, "mismatched-owner")
	unassignedWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "unassigned-legacy-volume")
	missingHostWorkspace := createTestWorkspace(t, ctx, store, owner.ID, "missing-host-legacy-volume")

	zeroBindingHost := createTestHost(t, ctx, store, owner.ID, capacity, []string{zeroBindingWorkspace.ID})
	oneBindingHost := createTestHost(t, ctx, store, owner.ID, capacity, []string{oneBindingWorkspace.ID})
	multiBindingHost := createTestHost(t, ctx, store, owner.ID, capacity, []string{multiBindingWorkspaceA.ID})
	if _, err := pool.Exec(ctx, `UPDATE workspace_bindings SET state='detached' WHERE host_id=$1`, zeroBindingHost.ID); err != nil {
		t.Fatalf("detach zero-binding test Host Workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_bindings SET state='active' WHERE host_id IN ($1,$2)`, oneBindingHost.ID, multiBindingHost.ID); err != nil {
		t.Fatalf("activate test Host Workspace bindings: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_bindings (
  id, owner_id, host_id, workspace_id, directory_name, state,
  binding_version, desired_revision, applied_revision
) VALUES ($1,$2,$3,$4,$4,'active',1,$5,0)`,
		bindingID(multiBindingHost.ID, multiBindingWorkspaceB.ID), owner.ID, multiBindingHost.ID,
		multiBindingWorkspaceB.ID, multiBindingWorkspaceB.Revision); err != nil {
		t.Fatalf("insert second synthetic active Host binding: %v", err)
	}

	markLegacyVolume := func(workspaceID string, legacyHostID any) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE workspace_volumes SET state='legacy', legacy_host_id=$2,
volume_name=NULL, ready_at=NULL WHERE workspace_id=$1`, workspaceID, legacyHostID); err != nil {
			t.Fatalf("mark synthetic Workspace volume %s legacy: %v", workspaceID, err)
		}
	}
	markLegacyVolume(zeroBindingWorkspace.ID, nil)
	markLegacyVolume(oneBindingWorkspace.ID, oneBindingHost.ID)
	markLegacyVolume(multiBindingWorkspaceA.ID, multiBindingHost.ID)
	markLegacyVolume(multiBindingWorkspaceB.ID, multiBindingHost.ID)
	markLegacyVolume(mismatchedOwnerWorkspace.ID, oneBindingHost.ID)
	markLegacyVolume(unassignedWorkspace.ID, nil)
	const nonexistentHostID = "h0000000"
	var nonexistentHostCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM hosts WHERE id=$1`, nonexistentHostID).Scan(&nonexistentHostCount); err != nil || nonexistentHostCount != 0 {
		t.Fatalf("synthetic missing Host id already exists: count=%d err=%v", nonexistentHostCount, err)
	}
	markLegacyVolume(missingHostWorkspace.ID, nonexistentHostID)

	type inventoryMetadataSnapshot struct {
		workspaceCount, bindingCount, volumeCount int64
		bindingStates, volumeStates               string
	}
	snapshot := func() (inventoryMetadataSnapshot, error) {
		var value inventoryMetadataSnapshot
		if err := pool.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM workspaces),
  (SELECT count(*) FROM workspace_bindings),
  (SELECT count(*) FROM workspace_volumes)`).Scan(&value.workspaceCount, &value.bindingCount, &value.volumeCount); err != nil {
			return inventoryMetadataSnapshot{}, err
		}
		if err := pool.QueryRow(ctx, `SELECT COALESCE(string_agg(id || ':' || state, ',' ORDER BY id), '') FROM workspace_bindings`).Scan(&value.bindingStates); err != nil {
			return inventoryMetadataSnapshot{}, err
		}
		if err := pool.QueryRow(ctx, `SELECT COALESCE(string_agg(workspace_id || ':' || owner_id || ':' || state || ':' ||
  COALESCE(volume_name, 'NULL') || ':' || COALESCE(legacy_host_id, 'NULL') || ':' || (ready_at IS NOT NULL)::text,
  ',' ORDER BY workspace_id), '') FROM workspace_volumes`).Scan(&value.volumeStates); err != nil {
			return inventoryMetadataSnapshot{}, err
		}
		return value, nil
	}
	before, err := snapshot()
	if err != nil {
		t.Fatalf("snapshot legacy Host metadata before preflight: %v", err)
	}

	inventorySQL, err := os.ReadFile("../../../migrations/preflight/legacy_host_volume_inventory.sql")
	if err != nil {
		t.Fatalf("read legacy Host volume inventory: %v", err)
	}
	rows, err := pool.Query(ctx, string(inventorySQL))
	if err != nil {
		t.Fatalf("run legacy Host volume inventory: %v", err)
	}
	type hostInventory struct {
		ownerID, hostID string
		bound, legacy   []string
		bindingCount    int64
	}
	hosts := make(map[string]hostInventory)
	for rows.Next() {
		var kind, volumeName string
		var ownerID, hostID, hostState *string
		var boundWorkspaceIDs, legacyWorkspaceIDs []string
		var bindingCount int64
		if err := rows.Scan(&kind, &volumeName, &ownerID, &hostID, &hostState,
			&boundWorkspaceIDs, &legacyWorkspaceIDs, &bindingCount); err != nil {
			rows.Close()
			t.Fatalf("scan Host volume inventory row: %v", err)
		}
		if kind != "workspace_root" || hostID == nil {
			continue
		}
		if ownerID == nil || hostState == nil || volumeName != domain.WorkspaceRootVolumeName(*hostID) {
			rows.Close()
			t.Fatalf("Host workspace-root inventory row has incomplete identity: kind=%q volume=%q owner=%v host=%v state=%v", kind, volumeName, ownerID, hostID, hostState)
		}
		hosts[*hostID] = hostInventory{
			ownerID: *ownerID, hostID: *hostID, bound: boundWorkspaceIDs,
			legacy: legacyWorkspaceIDs, bindingCount: bindingCount,
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("read Host volume inventory: %v", err)
	}
	rows.Close()
	assertInventory := func(hostID string, wantCount int64, wantBound, wantLegacy []string) {
		t.Helper()
		got, ok := hosts[hostID]
		if !ok {
			t.Fatalf("Host %s is missing from volume inventory", hostID)
		}
		if got.ownerID != owner.ID || got.hostID != hostID || got.bindingCount != wantCount {
			t.Fatalf("Host %s inventory identity/count = %#v; want owner=%s count=%d", hostID, got, owner.ID, wantCount)
		}
		sort.Strings(got.bound)
		sort.Strings(got.legacy)
		sort.Strings(wantBound)
		sort.Strings(wantLegacy)
		if strings.Join(got.bound, ",") != strings.Join(wantBound, ",") || strings.Join(got.legacy, ",") != strings.Join(wantLegacy, ",") {
			t.Fatalf("Host %s inventory paths = bound %v, legacy %v; want bound %v, legacy %v", hostID, got.bound, got.legacy, wantBound, wantLegacy)
		}
	}
	assertInventory(zeroBindingHost.ID, 0, nil, nil)
	assertInventory(oneBindingHost.ID, 1, []string{oneBindingWorkspace.ID}, []string{oneBindingWorkspace.ID, mismatchedOwnerWorkspace.ID})
	assertInventory(multiBindingHost.ID, 2,
		[]string{multiBindingWorkspaceA.ID, multiBindingWorkspaceB.ID},
		[]string{multiBindingWorkspaceA.ID, multiBindingWorkspaceB.ID})

	conflictSQL, err := os.ReadFile("../../../migrations/preflight/workspace_host_workspace_conflicts.sql")
	if err != nil {
		t.Fatalf("read Workspace/Host conflict inventory: %v", err)
	}
	const unassignedMarker = "-- Legacy Workspace volumes without a matching Host owner must be assigned manually."
	conflictSections := strings.SplitN(string(conflictSQL), unassignedMarker, 2)
	if len(conflictSections) != 2 {
		t.Fatal("Workspace/Host conflict inventory is missing its unassigned legacy volume query")
	}
	unassignedRows, err := pool.Query(ctx, strings.TrimSpace(conflictSections[1]))
	if err != nil {
		t.Fatalf("run unassigned legacy volume inventory: %v", err)
	}
	type unassignedVolume struct {
		ownerID string
		hostID  *string
	}
	unassigned := make(map[string]unassignedVolume)
	for unassignedRows.Next() {
		var workspaceID, ownerID string
		var legacyHostID *string
		if err := unassignedRows.Scan(&workspaceID, &ownerID, &legacyHostID); err != nil {
			unassignedRows.Close()
			t.Fatalf("scan unassigned legacy volume row: %v", err)
		}
		unassigned[workspaceID] = unassignedVolume{ownerID: ownerID, hostID: legacyHostID}
	}
	if err := unassignedRows.Err(); err != nil {
		unassignedRows.Close()
		t.Fatalf("read unassigned legacy volume rows: %v", err)
	}
	unassignedRows.Close()
	if len(unassigned) != 4 {
		t.Fatalf("unassigned legacy volume rows = %#v; want zero-binding and unassigned NULL Host, owner mismatch, and missing Host", unassigned)
	}
	for workspaceID, want := range map[string]unassignedVolume{
		zeroBindingWorkspace.ID:     {ownerID: owner.ID},
		mismatchedOwnerWorkspace.ID: {ownerID: otherOwner.ID, hostID: legacyPreflightStringPointer(oneBindingHost.ID)},
		unassignedWorkspace.ID:      {ownerID: owner.ID},
		missingHostWorkspace.ID:     {ownerID: owner.ID, hostID: legacyPreflightStringPointer(nonexistentHostID)},
	} {
		got, ok := unassigned[workspaceID]
		if !ok || got.ownerID != want.ownerID || (got.hostID == nil) != (want.hostID == nil) ||
			(got.hostID != nil && *got.hostID != *want.hostID) {
			t.Fatalf("unassigned legacy Workspace %s = %#v, want %#v", workspaceID, got, want)
		}
	}
	after, err := snapshot()
	if err != nil {
		t.Fatalf("snapshot legacy Host metadata after preflight: %v", err)
	}
	if after != before {
		t.Fatalf("read-only preflight changed DB metadata: before=%#v after=%#v", before, after)
	}
}

func legacyPreflightStringPointer(value string) *string {
	return &value
}
