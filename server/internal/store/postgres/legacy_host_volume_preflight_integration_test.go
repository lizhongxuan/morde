package postgres

import (
	"context"
	"os"
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
