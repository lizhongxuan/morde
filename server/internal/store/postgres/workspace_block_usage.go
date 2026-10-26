package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"

	"github.com/jackc/pgx/v5"
)

func lockWorkspaceOwnerUsage(ctx context.Context, tx pgx.Tx, ownerID string) (int64, int64, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO workspace_owner_block_usage(owner_id) VALUES($1) ON CONFLICT(owner_id) DO NOTHING`, ownerID); err != nil {
		return 0, 0, fmt.Errorf("initialize workspace owner block usage: %w", err)
	}
	var allocatedBytes, reservedBytes int64
	if err := tx.QueryRow(ctx, `SELECT allocated_bytes, reserved_bytes FROM workspace_owner_block_usage WHERE owner_id=$1 FOR UPDATE`, ownerID).Scan(&allocatedBytes, &reservedBytes); err != nil {
		return 0, 0, fmt.Errorf("lock workspace owner block usage: %w", err)
	}
	return allocatedBytes, reservedBytes, nil
}

func reserveWorkspaceBlockUsage(ctx context.Context, tx pgx.Tx, workspaceID, ownerID string, reservations []app.WorkspaceBlockReservation, maxOwnerBytes int64) error {
	return reserveWorkspaceBlockUsageWithQuota(ctx, tx, workspaceID, ownerID, reservations, maxOwnerBytes, true)
}

func reserveWorkspaceBlockUsageWithQuota(ctx context.Context, tx pgx.Tx, workspaceID, ownerID string, reservations []app.WorkspaceBlockReservation, maxOwnerBytes int64, enforceQuota bool) error {
	if !domain.ValidWorkspaceID(workspaceID) || !domain.ValidUserID(ownerID) || maxOwnerBytes <= 0 {
		return domain.InvalidArgument("workspace_block_reservation", "workspace block reservation fields are invalid")
	}
	byDigest := make(map[string]int64, len(reservations))
	for _, reservation := range reservations {
		if reservation.WorkspaceID != workspaceID || !validPostgresSHA256(reservation.Digest) || reservation.ReservedBytes <= 0 {
			return domain.InvalidArgument("workspace_block_reservation", "workspace block reservation is invalid")
		}
		if prior, exists := byDigest[reservation.Digest]; exists && prior != reservation.ReservedBytes {
			return domain.InvalidArgument("workspace_block_reservation", "duplicate workspace block reservations disagree")
		}
		byDigest[reservation.Digest] = reservation.ReservedBytes
	}
	digests := make([]string, 0, len(byDigest))
	for digest := range byDigest {
		digests = append(digests, digest)
	}
	sort.Strings(digests)
	requestedBytes := make([]int64, len(digests))
	for i, digest := range digests {
		requestedBytes[i] = byDigest[digest]
	}
	allocatedBytes, reservedBytes, err := lockWorkspaceOwnerUsage(ctx, tx, ownerID)
	if err != nil {
		return err
	}
	if enforceQuota && (allocatedBytes > maxOwnerBytes || reservedBytes > maxOwnerBytes-allocatedBytes) {
		return domain.NewError(domain.CodeQuotaExceeded, "workspace owner physical storage quota is already exceeded")
	}
	insertedRows, err := tx.Query(ctx, `
INSERT INTO workspace_block_usage(workspace_id, sha256, state, reserved_bytes)
SELECT $1, request.sha256, 'reserved', request.reserved_bytes
FROM unnest($2::text[], $3::bigint[]) AS request(sha256, reserved_bytes)
	ON CONFLICT(workspace_id, sha256) DO NOTHING
	RETURNING sha256`, workspaceID, digests, requestedBytes)
	if err != nil {
		return fmt.Errorf("create workspace block reservations: %w", err)
	}
	newRows := make(map[string]struct{}, len(digests))
	for insertedRows.Next() {
		var digest string
		if err := insertedRows.Scan(&digest); err != nil {
			insertedRows.Close()
			return fmt.Errorf("read newly created workspace block reservation: %w", err)
		}
		newRows[digest] = struct{}{}
	}
	if err := insertedRows.Err(); err != nil {
		insertedRows.Close()
		return fmt.Errorf("iterate newly created workspace block reservations: %w", err)
	}
	insertedRows.Close()
	rows, err := tx.Query(ctx, `
SELECT request.sha256, usage.state, usage.allocated_bytes, usage.reserved_bytes, request.reserved_bytes
FROM unnest($2::text[], $3::bigint[]) AS request(sha256, reserved_bytes)
JOIN workspace_block_usage usage ON usage.workspace_id=$1 AND usage.sha256=request.sha256
ORDER BY request.sha256
FOR UPDATE OF usage`, workspaceID, digests, requestedBytes)
	if err != nil {
		return fmt.Errorf("lock workspace block reservations: %w", err)
	}
	var reservationDelta int64
	var increaseDigests []string
	var increaseBytes []int64
	rowCount := 0
	for rows.Next() {
		var digest, state string
		var existingAllocated, existingReserved, requested int64
		if err := rows.Scan(&digest, &state, &existingAllocated, &existingReserved, &requested); err != nil {
			rows.Close()
			return fmt.Errorf("read locked workspace block reservation: %w", err)
		}
		rowCount++
		if _, isNew := newRows[digest]; isNew {
			if state != "reserved" || existingAllocated != 0 || existingReserved != requested {
				rows.Close()
				return domain.NewError(domain.CodeInternal, "new workspace block reservation is invalid")
			}
			if reservationDelta > math.MaxInt64-requested {
				rows.Close()
				return domain.NewError(domain.CodeQuotaExceeded, "workspace owner storage quota would be exceeded")
			}
			reservationDelta += requested
			continue
		}
		switch state {
		case "stored":
			if existingReserved != 0 || existingAllocated < 0 {
				rows.Close()
				return domain.NewError(domain.CodeInternal, "stored workspace block usage is invalid")
			}
		case "reserved":
			if existingAllocated != 0 || existingReserved <= 0 {
				rows.Close()
				return domain.NewError(domain.CodeInternal, "reserved workspace block usage is invalid")
			}
			if requested > existingReserved {
				delta := requested - existingReserved
				if reservationDelta > math.MaxInt64-delta {
					rows.Close()
					return domain.NewError(domain.CodeQuotaExceeded, "workspace owner storage quota would be exceeded")
				}
				reservationDelta += delta
				increaseDigests = append(increaseDigests, digest)
				increaseBytes = append(increaseBytes, requested)
			}
		default:
			rows.Close()
			return domain.NewError(domain.CodeInternal, "workspace block usage state is invalid")
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate locked workspace block reservations: %w", err)
	}
	rows.Close()
	if rowCount != len(digests) {
		return domain.NewError(domain.CodeInternal, "workspace block reservation batch is incomplete")
	}
	if reservationDelta == 0 {
		return nil
	}
	if reservedBytes > math.MaxInt64-reservationDelta {
		return domain.NewError(domain.CodeQuotaExceeded, "workspace owner storage quota would be exceeded")
	}
	nextReserved := reservedBytes + reservationDelta
	if enforceQuota && (allocatedBytes > maxOwnerBytes || nextReserved > maxOwnerBytes-allocatedBytes) {
		return domain.NewError(domain.CodeQuotaExceeded, "workspace owner physical storage quota would be exceeded")
	}
	if len(increaseDigests) > 0 {
		if _, err := tx.Exec(ctx, `
UPDATE workspace_block_usage usage
SET reserved_bytes=request.reserved_bytes, updated_at=now()
FROM unnest($2::text[], $3::bigint[]) AS request(sha256, reserved_bytes)
WHERE usage.workspace_id=$1 AND usage.sha256=request.sha256
  AND usage.state='reserved' AND usage.reserved_bytes < request.reserved_bytes`, workspaceID, increaseDigests, increaseBytes); err != nil {
			return fmt.Errorf("increase workspace block reservations: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `
UPDATE workspace_owner_block_usage SET reserved_bytes=$2, updated_at=now() WHERE owner_id=$1`, ownerID, nextReserved); err != nil {
		return fmt.Errorf("persist workspace owner block reservation: %w", err)
	}
	return nil
}

func (s *Store) ReserveWorkspaceBlockUsage(ctx context.Context, workspaceID string, reservations []app.WorkspaceBlockReservation, maxOwnerBytes int64) error {
	if !domain.ValidWorkspaceID(workspaceID) || maxOwnerBytes <= 0 {
		return domain.InvalidArgument("workspace_block_reservation", "workspace block reservation fields are invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID, state, importState string
		if err := tx.QueryRow(ctx, `
SELECT w.owner_id, w.state, COALESCE(v.import_state, '')
FROM workspaces w LEFT JOIN workspace_legacy_volumes v ON v.workspace_id=w.id
WHERE w.id=$1 FOR UPDATE OF w`, workspaceID).Scan(&ownerID, &state, &importState); err != nil {
			return storeError("lock workspace before reserving imported block usage", "workspace", err)
		}
		if state != "ready" || importState != "pending" {
			return domain.NewError(domain.CodeWorkspaceBusy, "workspace is not pending legacy import")
		}
		return reserveWorkspaceBlockUsage(ctx, tx, workspaceID, ownerID, reservations, maxOwnerBytes)
	})
}

// FinalizeWorkspaceBlockUsage converts a durable pre-write reservation into
// the actual allocated data bytes reported by the filesystem.
func (s *Store) FinalizeWorkspaceBlockUsage(ctx context.Context, workspaceID, digest string, allocatedBytes, maxOwnerBytes int64) error {
	if !domain.ValidWorkspaceID(workspaceID) || !validPostgresSHA256(digest) || allocatedBytes < 0 || maxOwnerBytes <= 0 {
		return domain.InvalidArgument("workspace_block_usage", "workspace block usage fields are invalid")
	}
	var overrun bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		overrun, err = finalizeWorkspaceBlockUsageTx(ctx, tx, workspaceID, digest, allocatedBytes, maxOwnerBytes)
		return err
	})
	if err != nil {
		return err
	}
	if overrun {
		return domain.NewError(domain.CodeQuotaExceeded, "workspace block allocation exceeded its reserved physical quota")
	}
	return nil
}

func finalizeWorkspaceBlockUsageTx(ctx context.Context, tx pgx.Tx, workspaceID, digest string, allocatedBytes, maxOwnerBytes int64) (bool, error) {
	var ownerID string
	if err := tx.QueryRow(ctx, `SELECT owner_id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&ownerID); err != nil {
		return false, storeError("lock workspace before settling block usage", "workspace", err)
	}
	ownerAllocated, ownerReserved, err := lockWorkspaceOwnerUsage(ctx, tx, ownerID)
	if err != nil {
		return false, err
	}
	var state string
	var existingAllocated, existingReserved int64
	if err := tx.QueryRow(ctx, `
SELECT state, allocated_bytes, reserved_bytes FROM workspace_block_usage
WHERE workspace_id=$1 AND sha256=$2 FOR UPDATE`, workspaceID, digest).Scan(&state, &existingAllocated, &existingReserved); err != nil {
		return false, storeError("lock workspace block reservation before settlement", "workspace block usage", err)
	}
	if state == "stored" {
		if existingAllocated != allocatedBytes || existingReserved != 0 {
			return false, domain.NewError(domain.CodeInternal, "stored workspace block allocation changed")
		}
		return ownerAllocated > maxOwnerBytes || ownerReserved > maxOwnerBytes-ownerAllocated, nil
	}
	if state != "reserved" || existingAllocated != 0 || existingReserved <= 0 || ownerReserved < existingReserved {
		return false, domain.NewError(domain.CodeInternal, "workspace block reservation is missing or invalid")
	}
	if ownerAllocated > math.MaxInt64-allocatedBytes {
		return false, domain.NewError(domain.CodeInternal, "workspace owner allocated bytes overflow")
	}
	nextAllocated := ownerAllocated + allocatedBytes
	nextReserved := ownerReserved - existingReserved
	overrun := nextAllocated > maxOwnerBytes || nextReserved > maxOwnerBytes-nextAllocated || allocatedBytes > existingReserved
	if _, err := tx.Exec(ctx, `
UPDATE workspace_block_usage SET state='stored', allocated_bytes=$3, reserved_bytes=0, updated_at=now()
WHERE workspace_id=$1 AND sha256=$2`, workspaceID, digest, allocatedBytes); err != nil {
		return false, fmt.Errorf("settle workspace block usage: %w", err)
	}
	if _, err := tx.Exec(ctx, `
UPDATE workspace_owner_block_usage SET allocated_bytes=$2, reserved_bytes=$3, updated_at=now()
WHERE owner_id=$1`, ownerID, nextAllocated, nextReserved); err != nil {
		return false, fmt.Errorf("settle workspace owner block usage: %w", err)
	}
	return overrun, nil
}

func (s *Store) ListWorkspaceBlocksNeedingReservation(ctx context.Context) ([]app.WorkspaceBlockReservationTarget, error) {
	rows, err := s.pool.Query(ctx, `
SELECT b.workspace_id, b.sha256, b.size_bytes
FROM workspace_sync_sessions s
JOIN workspace_sync_session_blocks b ON b.workspace_id=s.workspace_id AND b.session_id=s.id
LEFT JOIN workspace_block_usage u ON u.workspace_id=b.workspace_id AND u.sha256=b.sha256
WHERE s.state IN ('planned','transferring','verifying','applying')
  AND b.role='upload' AND b.received=false AND u.sha256 IS NULL
ORDER BY b.workspace_id, b.sha256`)
	if err != nil {
		return nil, fmt.Errorf("list workspace blocks needing startup reservation: %w", err)
	}
	defer rows.Close()
	items := make([]app.WorkspaceBlockReservationTarget, 0)
	for rows.Next() {
		var item app.WorkspaceBlockReservationTarget
		if err := rows.Scan(&item.WorkspaceID, &item.Digest, &item.SizeBytes); err != nil {
			return nil, fmt.Errorf("scan workspace block needing startup reservation: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workspace blocks needing startup reservation: %w", err)
	}
	return items, nil
}

func (s *Store) ReconcileWorkspaceBlockUsage(ctx context.Context, allocations []app.WorkspaceBlockAllocation, recovered []app.WorkspaceBlockReservation, maxOwnerBytes int64) error {
	if maxOwnerBytes <= 0 {
		return domain.InvalidArgument("workspace_owner_quota", "workspace owner physical quota must be positive")
	}
	seen := make(map[string]struct{}, len(allocations))
	for _, allocation := range allocations {
		key := allocation.WorkspaceID + ":" + allocation.Digest
		if !domain.ValidWorkspaceID(allocation.WorkspaceID) || !validPostgresSHA256(allocation.Digest) || allocation.AllocatedBytes < 0 {
			return domain.NewError(domain.CodeInternal, "workspace block allocation scan is invalid")
		}
		if _, ok := seen[key]; ok {
			return domain.NewError(domain.CodeInternal, "workspace block allocation scan contains duplicates")
		}
		seen[key] = struct{}{}
	}
	sort.Slice(allocations, func(i, j int) bool {
		if allocations[i].WorkspaceID == allocations[j].WorkspaceID {
			return allocations[i].Digest < allocations[j].Digest
		}
		return allocations[i].WorkspaceID < allocations[j].WorkspaceID
	})
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE workspace_block_quota_scan(workspace_id text NOT NULL, sha256 text NOT NULL, allocated_bytes bigint NOT NULL, PRIMARY KEY(workspace_id, sha256)) ON COMMIT DROP`); err != nil {
			return fmt.Errorf("create workspace block reconciliation table: %w", err)
		}
		if len(allocations) > 0 {
			_, err := tx.CopyFrom(ctx, pgx.Identifier{"workspace_block_quota_scan"}, []string{"workspace_id", "sha256", "allocated_bytes"}, pgx.CopyFromSlice(len(allocations), func(i int) ([]any, error) {
				item := allocations[i]
				return []any{item.WorkspaceID, item.Digest, item.AllocatedBytes}, nil
			}))
			if err != nil {
				return fmt.Errorf("load workspace block reconciliation scan: %w", err)
			}
		}
		var invalid, missingReferences bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM workspace_block_quota_scan a
  LEFT JOIN workspaces w ON w.id=a.workspace_id
  WHERE w.id IS NULL OR w.state='deleted' OR w.deleted_at IS NOT NULL
)`).Scan(&invalid); err != nil {
			return fmt.Errorf("validate scanned workspace block ownership: %w", err)
		}
		if invalid {
			return domain.NewError(domain.CodeInternal, "workspace block storage contains data for an unknown or deleted workspace")
		}
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM workspace_revision_files f
  JOIN workspaces w ON w.id=f.workspace_id AND w.state='ready' AND w.deleted_at IS NULL
  CROSS JOIN LATERAL jsonb_array_elements(f.chunks) c
  LEFT JOIN workspace_block_quota_scan a
    ON a.workspace_id=f.workspace_id AND a.sha256=(c->>'sha256')
  WHERE a.sha256 IS NULL
) OR EXISTS (
  SELECT 1 FROM workspace_sync_sessions s
  JOIN workspace_sync_session_blocks b ON b.workspace_id=s.workspace_id AND b.session_id=s.id
  LEFT JOIN workspace_block_quota_scan a ON a.workspace_id=b.workspace_id AND a.sha256=b.sha256
  WHERE s.state IN ('planned','transferring','verifying','applying')
    AND b.role='upload' AND b.received=true AND a.sha256 IS NULL
)`).Scan(&missingReferences); err != nil {
			return fmt.Errorf("check workspace block references during reconciliation: %w", err)
		}
		if missingReferences {
			return domain.NewError(domain.CodeInternal, "referenced workspace content is missing from block storage")
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO workspace_owner_block_usage(owner_id)
SELECT DISTINCT w.owner_id FROM workspace_block_quota_scan a JOIN workspaces w ON w.id=a.workspace_id
ON CONFLICT(owner_id) DO NOTHING`); err != nil {
			return fmt.Errorf("initialize owners during workspace block reconciliation: %w", err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO workspace_block_usage(workspace_id, sha256, state, allocated_bytes, reserved_bytes)
SELECT workspace_id, sha256, 'stored', allocated_bytes, 0 FROM workspace_block_quota_scan
ON CONFLICT(workspace_id, sha256) DO UPDATE
SET state='stored', allocated_bytes=EXCLUDED.allocated_bytes, reserved_bytes=0, updated_at=now()`); err != nil {
			return fmt.Errorf("reconcile stored workspace block usage: %w", err)
		}
		if _, err := tx.Exec(ctx, `
DELETE FROM workspace_block_usage u
USING workspaces w
WHERE w.id=u.workspace_id AND w.state='deleted'
  AND NOT EXISTS (SELECT 1 FROM workspace_block_quota_scan a WHERE a.workspace_id=u.workspace_id AND a.sha256=u.sha256)`); err != nil {
			return fmt.Errorf("remove deleted workspace block usage: %w", err)
		}
		if _, err := tx.Exec(ctx, `
DELETE FROM workspace_block_usage u
WHERE NOT EXISTS (SELECT 1 FROM workspace_block_quota_scan a WHERE a.workspace_id=u.workspace_id AND a.sha256=u.sha256)
  AND u.state='stored'`); err != nil {
			return fmt.Errorf("remove absent workspace block usage: %w", err)
		}
		if _, err := tx.Exec(ctx, `
DELETE FROM workspace_block_usage u
WHERE u.state='reserved'
  AND NOT EXISTS (SELECT 1 FROM workspace_block_quota_scan a WHERE a.workspace_id=u.workspace_id AND a.sha256=u.sha256)
  AND NOT EXISTS (
    SELECT 1 FROM workspace_sync_sessions s
    JOIN workspace_sync_session_blocks b ON b.workspace_id=s.workspace_id AND b.session_id=s.id
    WHERE s.workspace_id=u.workspace_id AND b.sha256=u.sha256 AND b.role='upload' AND b.received=false
      AND s.state IN ('planned','transferring','verifying','applying')
  )
  AND NOT EXISTS (SELECT 1 FROM workspace_legacy_volumes v WHERE v.workspace_id=u.workspace_id AND v.import_state='pending')`); err != nil {
			return fmt.Errorf("release orphaned workspace block reservations: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE workspace_owner_block_usage o SET
  allocated_bytes=COALESCE((SELECT sum(u.allocated_bytes) FROM workspace_block_usage u JOIN workspaces w ON w.id=u.workspace_id WHERE w.owner_id=o.owner_id),0),
  reserved_bytes=COALESCE((SELECT sum(u.reserved_bytes) FROM workspace_block_usage u JOIN workspaces w ON w.id=u.workspace_id WHERE w.owner_id=o.owner_id),0),
  updated_at=now()`); err != nil {
			return fmt.Errorf("rebuild workspace owner block usage counters: %w", err)
		}
		if len(recovered) > 0 {
			byWorkspace := make(map[string][]app.WorkspaceBlockReservation)
			for _, reservation := range recovered {
				if !domain.ValidWorkspaceID(reservation.WorkspaceID) || !validPostgresSHA256(reservation.Digest) || reservation.ReservedBytes <= 0 {
					return domain.NewError(domain.CodeInternal, "recovered workspace block reservation is invalid")
				}
				byWorkspace[reservation.WorkspaceID] = append(byWorkspace[reservation.WorkspaceID], reservation)
			}
			workspaceIDs := make([]string, 0, len(byWorkspace))
			for workspaceID := range byWorkspace {
				workspaceIDs = append(workspaceIDs, workspaceID)
			}
			sort.Strings(workspaceIDs)
			for _, workspaceID := range workspaceIDs {
				var ownerID, state string
				if err := tx.QueryRow(ctx, `SELECT owner_id, state FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&ownerID, &state); err != nil || state != "ready" {
					return domain.NewError(domain.CodeInternal, "recovered workspace block reservation has no ready workspace")
				}
				if err := reserveWorkspaceBlockUsageWithQuota(ctx, tx, workspaceID, ownerID, byWorkspace[workspaceID], maxOwnerBytes, false); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func applyWorkspaceBlockRemovals(ctx context.Context, tx pgx.Tx, workspaceID string, removed []app.WorkspaceBlockRemoval) error {
	if len(removed) == 0 {
		return nil
	}
	var ownerID string
	if err := tx.QueryRow(ctx, `SELECT owner_id FROM workspaces WHERE id=$1`, workspaceID).Scan(&ownerID); err != nil {
		return storeError("read workspace owner after block collection", "workspace", err)
	}
	ownerAllocated, ownerReserved, err := lockWorkspaceOwnerUsage(ctx, tx, ownerID)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(removed))
	var allocatedDebit, reservedDebit int64
	for _, item := range removed {
		if !validPostgresSHA256(item.Digest) || item.AllocatedBytes < 0 {
			return domain.NewError(domain.CodeInternal, "removed workspace block allocation is invalid")
		}
		if _, ok := seen[item.Digest]; ok {
			return domain.NewError(domain.CodeInternal, "workspace block collection returned duplicate removals")
		}
		seen[item.Digest] = struct{}{}
		var state string
		var storedBytes, reservedBytes int64
		if err := tx.QueryRow(ctx, `
SELECT state, allocated_bytes, reserved_bytes FROM workspace_block_usage
WHERE workspace_id=$1 AND sha256=$2 FOR UPDATE`, workspaceID, item.Digest).Scan(&state, &storedBytes, &reservedBytes); err != nil {
			return storeError("lock removed workspace block usage", "workspace block usage", err)
		}
		switch state {
		case "stored":
			if storedBytes != item.AllocatedBytes || allocatedDebit > math.MaxInt64-storedBytes {
				return domain.NewError(domain.CodeInternal, "removed workspace block allocation does not match ledger")
			}
			allocatedDebit += storedBytes
		case "reserved":
			if item.AllocatedBytes > reservedBytes || reservedDebit > math.MaxInt64-reservedBytes {
				return domain.NewError(domain.CodeInternal, "removed workspace block exceeded its reservation")
			}
			reservedDebit += reservedBytes
		default:
			return domain.NewError(domain.CodeInternal, "removed workspace block usage state is invalid")
		}
		if _, err := tx.Exec(ctx, `DELETE FROM workspace_block_usage WHERE workspace_id=$1 AND sha256=$2`, workspaceID, item.Digest); err != nil {
			return fmt.Errorf("remove collected workspace block ledger row: %w", err)
		}
	}
	if ownerAllocated < allocatedDebit || ownerReserved < reservedDebit {
		return domain.NewError(domain.CodeInternal, "workspace owner block usage counters are inconsistent")
	}
	if _, err := tx.Exec(ctx, `
UPDATE workspace_owner_block_usage SET allocated_bytes=$2, reserved_bytes=$3, updated_at=now()
WHERE owner_id=$1`, ownerID, ownerAllocated-allocatedDebit, ownerReserved-reservedDebit); err != nil {
		return fmt.Errorf("debit collected workspace owner block usage: %w", err)
	}
	return nil
}

func ensureWorkspaceMutationsBlockUsageStored(ctx context.Context, tx pgx.Tx, workspaceID string, mutations []syncMutation) error {
	files := make([]workspace.File, 0, len(mutations))
	for _, mutation := range mutations {
		if mutation.file != nil {
			files = append(files, *mutation.file)
		}
	}
	filesJSON, err := json.Marshal(files)
	if err != nil {
		return fmt.Errorf("encode workspace mutations for block usage check: %w", err)
	}
	return ensureWorkspaceFilesBlockUsageStored(ctx, tx, workspaceID, filesJSON)
}

func ensureWorkspaceFilesBlockUsageStored(ctx context.Context, tx pgx.Tx, workspaceID string, filesJSON []byte) error {
	var missing bool
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1
  FROM jsonb_array_elements($2::jsonb) f
  CROSS JOIN LATERAL jsonb_array_elements(f->'chunks') c
  LEFT JOIN workspace_block_usage u
    ON u.workspace_id=$1 AND u.sha256=(c->>'sha256') AND u.state='stored'
  WHERE u.sha256 IS NULL
)`, workspaceID, filesJSON).Scan(&missing); err != nil {
		return fmt.Errorf("check workspace files against physical block ledger: %w", err)
	}
	if missing {
		return domain.NewError(domain.CodeInternal, "workspace manifest references unaccounted physical blocks")
	}
	return nil
}

var _ app.WorkspaceBlockUsageStore = (*Store)(nil)
