package postgres

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"math"
	"sort"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

const createCapacityLockKey int64 = 0x43484f5354434150 // "CHOSTCAP"

// GetHostCreateReplay returns the existing resource for an exact create retry.
func (s *Store) GetHostCreateReplay(ctx context.Context, record app.IdempotencyRecord) (host.Host, bool, error) {
	if err := validateHostCreateIdempotency(record); err != nil {
		return host.Host{}, false, err
	}
	var replay host.Host
	found := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, exists, err := findIdempotency(ctx, tx, record)
		if err != nil || !exists {
			return err
		}
		replay, err = hostCreateReplayInTx(ctx, tx, record.OwnerID, stored.ResourceID)
		if err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil {
		return host.Host{}, false, err
	}
	return replay, found, nil
}

// CreateHost validates current allocations, inserts the host, and creates its
// initial workspace bindings in one transaction. Owner and global advisory
// transaction locks serialize quota and capacity checks respectively.
func (s *Store) CreateHost(ctx context.Context, params app.CreateHostParams) (host.Host, bool, error) {
	if err := validateCreateHost(params); err != nil {
		return host.Host{}, false, err
	}

	var created host.Host
	replayed := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, params.Host.OwnerID); err != nil {
			return fmt.Errorf("lock host owner allocation: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, createCapacityLockKey); err != nil {
			return fmt.Errorf("lock host capacity allocation: %w", err)
		}
		if params.Idempotency.Key != "" {
			stored, replay, err := reserveIdempotency(ctx, tx, params.Idempotency, params.Host.ID, "")
			if err != nil {
				return err
			}
			if replay {
				created, err = hostCreateReplayInTx(ctx, tx, params.Host.OwnerID, stored.ResourceID)
				if err != nil {
					return err
				}
				replayed = true
				return nil
			}
		}
		if err := lockHostVolumeCleanupID(ctx, tx, params.Host.ID); err != nil {
			return err
		}
		pendingCleanup, err := sqlcgen.New(tx).HasPendingOrphanHostVolumeCleanup(ctx, params.Host.ID)
		if err != nil {
			return fmt.Errorf("check pending Host volume cleanup before create: %w", err)
		}
		if pendingCleanup {
			return domain.NewError(domain.CodeConflict, "host id is pending orphan volume cleanup")
		}

		queries := sqlcgen.New(tx)
		user, err := queries.GetUserByID(ctx, params.Host.OwnerID)
		if err != nil {
			return storeError("get host owner", "user", err)
		}

		var userHosts, userCPU, userMemory int64
		if err := tx.QueryRow(ctx, allocatedUsageSQL, params.Host.OwnerID).Scan(&userHosts, &userCPU, &userMemory); err != nil {
			return fmt.Errorf("read owner host allocation: %w", err)
		}
		if err := validateAllocation("host_count", userHosts, 1, int64(user.QuotaHosts), domain.CodeQuotaExceeded); err != nil {
			return err
		}
		if err := validateAllocation("cpu_milli", userCPU, int64(params.Host.CPUMilli), int64(user.QuotaCpuMilli), domain.CodeQuotaExceeded); err != nil {
			return err
		}
		if err := validateAllocation("mem_mib", userMemory, int64(params.Host.MemMiB), int64(user.QuotaMemMib), domain.CodeQuotaExceeded); err != nil {
			return err
		}

		var clusterHosts, clusterCPU, clusterMemory int64
		if err := tx.QueryRow(ctx, allocatedUsageSQL, nil).Scan(&clusterHosts, &clusterCPU, &clusterMemory); err != nil {
			return fmt.Errorf("read cluster host allocation: %w", err)
		}
		_ = clusterHosts
		if err := validateAllocation("cpu_milli", clusterCPU, int64(params.Host.CPUMilli), params.Capacity.CPUMilli, domain.CodeCapacityExceeded); err != nil {
			return err
		}
		if err := validateAllocation("mem_mib", clusterMemory, int64(params.Host.MemMiB), params.Capacity.MemMiB, domain.CodeCapacityExceeded); err != nil {
			return err
		}

		workspaceRevisions, err := lockWorkspacesForBinding(ctx, tx, params.WorkspaceIDs, params.Host.OwnerID)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, insertHostSQL,
			params.Host.ID, params.Host.OwnerID, params.Host.Name, params.Host.TemplateID,
			params.Host.RuntimeProfile, string(params.Host.State), params.Host.StateReason,
			int32(params.Host.CPUMilli), int32(params.Host.MemMiB),
			int32(params.Host.CPULimitMilli), int32(params.Host.MemLimitMiB), params.Host.AIOKeyEnc,
			nullableTimestamp(params.Host.ExpiresAt), nullableTimestamp(params.Host.LastActiveAt),
			params.Host.RuntimeCredentialEnc, nullableText(params.Host.RuntimeInstanceID),
			params.Host.RuntimeGeneration,
		); err != nil {
			return fmt.Errorf("insert host: %w", err)
		}
		for _, workspaceID := range params.WorkspaceIDs {
			if _, err := tx.Exec(ctx, `
INSERT INTO workspace_bindings (
  id, owner_id, host_id, workspace_id, directory_name, state,
  binding_version, desired_revision, applied_revision
)
VALUES ($1, $2, $3, $4, $4, 'preparing', 1, $5, 0)`,
				bindingID(params.Host.ID, workspaceID), params.Host.OwnerID, params.Host.ID,
				workspaceID, workspaceRevisions[workspaceID]); err != nil {
				return workspaceBindingInsertError(err)
			}
		}
		if params.GrantTokenID != "" {
			tag, err := tx.Exec(ctx, `UPDATE tokens
SET host_ids = array_append(host_ids, $3)
WHERE id=$1 AND owner_id=$2 AND kind='api_key' AND revoked_at IS NULL
  AND expires_at > clock_timestamp() AND 'host:create'=ANY(scopes)
  AND cardinality(host_ids) < 500
  AND EXISTS (SELECT 1 FROM users WHERE id=$2 AND status='approved')`,
				params.GrantTokenID, params.Host.OwnerID, params.Host.ID)
			if err != nil {
				return fmt.Errorf("grant newly created host to API key: %w", err)
			}
			if tag.RowsAffected() != 1 {
				return domain.NewError(domain.CodeForbidden, "API key cannot be granted access to the new host")
			}
		}

		row, err := queries.GetHost(ctx, params.Host.ID)
		if err != nil {
			return fmt.Errorf("read created host: %w", err)
		}
		created = hostDomain(row)
		bindingRows, err := queries.ListWorkspaceBindingsByHost(ctx, params.Host.ID)
		if err != nil {
			return fmt.Errorf("read created host workspace bindings: %w", err)
		}
		created.Workspaces = mapRows(bindingRows, workspaceBindingDomainFromList)
		return nil
	})
	if err != nil {
		return host.Host{}, false, err
	}
	return created, replayed, nil
}

func validateHostCreateIdempotency(record app.IdempotencyRecord) error {
	if record.Operation != "host.create" {
		return domain.InvalidArgument("idempotency_key", "host create idempotency operation is invalid")
	}
	return validateIdempotency(record)
}

func hostCreateReplayInTx(ctx context.Context, tx pgx.Tx, ownerID, hostID string) (host.Host, error) {
	row, err := sqlcgen.New(tx).GetHost(ctx, hostID)
	if err != nil {
		return host.Host{}, storeError("replay host creation", "host", err)
	}
	if row.OwnerID != ownerID {
		return host.Host{}, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is bound to another host")
	}
	value, err := hostDomainWithBindings(ctx, sqlcgen.New(tx), row)
	if err != nil {
		return host.Host{}, err
	}
	return value, nil
}

const allocatedUsageSQL = `
SELECT count(*)::bigint,
       COALESCE(sum(h.cpu_milli), 0)::bigint,
       COALESCE(sum(h.mem_mib), 0)::bigint
FROM hosts h
WHERE h.deleted_at IS NULL
  AND ($1::text IS NULL OR h.owner_id = $1)
  AND (
	    h.state IN ('creating', 'running', 'stopping', 'deleting')
    OR (h.state = 'failed' AND (
      h.sandbox_id IS NOT NULL
      OR EXISTS (
        SELECT 1 FROM workspace_bindings wb
        WHERE wb.host_id = h.id AND wb.state <> 'detached'
      )
    ))
  )`

const insertHostSQL = `
INSERT INTO hosts (
  id, owner_id, name, template_id, runtime_profile, state, state_reason,
  cpu_milli, mem_mib, cpu_limit_milli, mem_limit_mib, aio_key_enc,
  expires_at, last_active_at, runtime_credential_enc, runtime_instance_id, runtime_generation, updated_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, now())`

func validateCreateHost(params app.CreateHostParams) error {
	h := params.Host
	if !domain.ValidHostID(h.ID) || !domain.ValidUserID(h.OwnerID) || h.State != host.StateCreating ||
		h.TemplateID == "" || h.RuntimeProfile == "" || len(h.AIOKeyEnc) == 0 || len(h.RuntimeCredentialEnc) == 0 || h.RuntimeGeneration <= 0 {
		return domain.InvalidArgument("host", "host creation fields are invalid")
	}
	if params.Idempotency.Key != "" {
		if err := validateHostCreateIdempotency(params.Idempotency); err != nil {
			return err
		}
		if params.Idempotency.OwnerID != h.OwnerID || params.Idempotency.ResourceID != h.ID {
			return domain.InvalidArgument("idempotency_key", "host create idempotency resource is invalid")
		}
	} else if params.Idempotency.OwnerID != "" || params.Idempotency.Operation != "" || len(params.Idempotency.RequestHash) != 0 ||
		params.Idempotency.ResourceID != "" || params.Idempotency.RelatedID != "" {
		return domain.InvalidArgument("idempotency_key", "host create idempotency fields are incomplete")
	}
	if h.CPUMilli <= 0 || h.MemMiB <= 0 || h.CPULimitMilli < h.CPUMilli || h.MemLimitMiB < h.MemMiB ||
		h.CPUMilli > math.MaxInt32 || h.MemMiB > math.MaxInt32 ||
		h.CPULimitMilli > math.MaxInt32 || h.MemLimitMiB > math.MaxInt32 {
		return domain.InvalidArgument("host.resources", "host resource values are invalid")
	}
	if params.Capacity.CPUMilli <= 0 || params.Capacity.MemMiB <= 0 {
		return domain.InvalidArgument("capacity", "cluster capacity limits are invalid")
	}
	seen := make(map[string]struct{}, len(params.WorkspaceIDs))
	for _, workspaceID := range params.WorkspaceIDs {
		if !domain.ValidWorkspaceID(workspaceID) {
			return domain.InvalidArgument("workspace_ids", "workspace id is invalid")
		}
		if _, exists := seen[workspaceID]; exists {
			return domain.InvalidArgument("workspace_ids", "workspace ids contain duplicates")
		}
		seen[workspaceID] = struct{}{}
	}
	return nil
}

func lockWorkspacesForBinding(ctx context.Context, tx pgx.Tx, workspaceIDs []string, ownerID string) (map[string]int64, error) {
	ids := append([]string(nil), workspaceIDs...)
	sort.Strings(ids)
	revisions := make(map[string]int64, len(ids))
	if len(ids) == 0 {
		return revisions, nil
	}
	rows, err := tx.Query(ctx, `
SELECT id, owner_id, state, revision
FROM workspaces WHERE id = ANY($1::text[])
ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, fmt.Errorf("lock workspaces for host binding: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, owner, state string
		var revision int64
		if err := rows.Scan(&id, &owner, &state, &revision); err != nil {
			return nil, fmt.Errorf("read workspace for host binding: %w", err)
		}
		if owner != ownerID {
			return nil, domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if state != string(workspace.StateReady) {
			conflict := domain.NewError(domain.CodeWorkspaceBusy, "workspace is not available for binding")
			conflict.Details["state"] = state
			return nil, conflict
		}
		revisions[id] = revision
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read workspaces for host binding: %w", err)
	}
	if len(revisions) != len(ids) {
		return nil, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	return revisions, nil
}

func bindingID(hostID, workspaceID string) string {
	hash := md5.Sum([]byte(hostID + ":" + workspaceID))
	return "b" + hex.EncodeToString(hash[:])
}

func workspaceBindingInsertError(err error) error {
	if isUniqueViolation(err) {
		return domain.NewError(domain.CodeWorkspaceBusy, "workspace is already bound to a host")
	}
	return fmt.Errorf("create workspace binding: %w", err)
}

func validateAllocation(dimension string, used, requested, limit int64, code domain.ErrorCode) error {
	if requested > limit || used > limit-requested {
		message := "host quota exceeded"
		if code == domain.CodeCapacityExceeded {
			message = "cluster capacity exceeded"
		}
		err := domain.NewError(code, message)
		err.Details["dimension"] = dimension
		err.Details["limit"] = limit
		return err
	}
	return nil
}
