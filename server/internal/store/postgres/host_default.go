package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"
)

// EnsureDefaultHost uses the same owner lock as normal host creation. The
// stopped host, its one workspace binding, and its preference are committed
// together, so concurrent logins cannot allocate an unbound default computer.
func (s *Store) GetDefaultHostForOwner(ctx context.Context, ownerID string) (host.Host, bool, error) {
	if !domain.ValidUserID(ownerID) {
		return host.Host{}, false, domain.InvalidArgument("user_id", "user id is invalid")
	}
	rows, err := s.pool.Query(ctx, `SELECT h.id, (h.id=COALESCE(p.default_host_id,'')) AS is_default FROM hosts AS h
LEFT JOIN user_preferences AS p ON p.user_id=h.owner_id
WHERE h.owner_id=$1 AND h.deleted_at IS NULL AND h.state <> 'deleted'
ORDER BY is_default DESC, h.created_at, h.id LIMIT 2`, ownerID)
	if err != nil {
		return host.Host{}, false, fmt.Errorf("find default Host for owner: %w", err)
	}
	var hostID string
	var firstIsDefault bool
	var count int
	for rows.Next() {
		var id string
		var isDefault bool
		if err := rows.Scan(&id, &isDefault); err != nil {
			rows.Close()
			return host.Host{}, false, fmt.Errorf("read default Host for owner: %w", err)
		}
		if count == 0 {
			hostID, firstIsDefault = id, isDefault
		}
		count++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return host.Host{}, false, fmt.Errorf("read default Host for owner: %w", err)
	}
	rows.Close()
	if count == 0 {
		return host.Host{}, false, nil
	}
	if count > 1 && !firstIsDefault {
		return host.Host{}, false, domain.NewError(domain.CodeConflict, "default Host is not selected")
	}
	row, err := s.q.GetHost(ctx, hostID)
	if err != nil {
		return host.Host{}, false, fmt.Errorf("load default Host for owner: %w", err)
	}
	value, err := hostDomainWithBindings(ctx, s.q, row)
	if err != nil {
		return host.Host{}, false, err
	}
	return value, true, nil
}

func (s *Store) EnsureDefaultHost(ctx context.Context, params app.CreateHostParams) (host.Host, bool, error) {
	if params.Host.State != host.StateStopped || len(params.WorkspaceIDs) != 1 || params.Idempotency.Key != "" {
		return host.Host{}, false, domain.InvalidArgument("host", "default host must be stopped with exactly one workspace")
	}
	if err := validateHostInsert(params, host.StateStopped); err != nil {
		return host.Host{}, false, err
	}
	var result host.Host
	created := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, params.Host.OwnerID); err != nil {
			return fmt.Errorf("lock host owner allocation: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, createCapacityLockKey); err != nil {
			return fmt.Errorf("lock host capacity allocation: %w", err)
		}
		queries := sqlcgen.New(tx)
		var quotaHosts, quotaCPU, quotaMem, defaultCPU, defaultMem int
		var status string
		if err := tx.QueryRow(ctx, `SELECT quota_hosts, quota_cpu_milli, quota_mem_mib,
       default_host_cpu_milli, default_host_mem_mib, status
FROM users WHERE id=$1 FOR UPDATE`, params.Host.OwnerID).Scan(
			&quotaHosts, &quotaCPU, &quotaMem, &defaultCPU, &defaultMem, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "user not found")
			}
			return fmt.Errorf("read default host owner: %w", err)
		}
		if status != "approved" {
			return domain.NewError(domain.CodeForbidden, "account is not approved")
		}
		workspaceID := params.WorkspaceIDs[0]

		rows, err := tx.Query(ctx, `SELECT h.id, (h.id=COALESCE(p.default_host_id,'')) AS is_default FROM hosts AS h
LEFT JOIN user_preferences AS p ON p.user_id=h.owner_id
WHERE h.owner_id=$1 AND h.deleted_at IS NULL AND h.state <> 'deleted'
ORDER BY is_default DESC, h.created_at, h.id LIMIT 2`, params.Host.OwnerID)
		if err != nil {
			return fmt.Errorf("find existing default host: %w", err)
		}
		var existingID string
		var firstIsDefault bool
		var existingCount int
		for rows.Next() {
			var id string
			var isDefault bool
			if err := rows.Scan(&id, &isDefault); err != nil {
				rows.Close()
				return fmt.Errorf("read existing default host: %w", err)
			}
			if existingCount == 0 {
				existingID, firstIsDefault = id, isDefault
			}
			existingCount++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read existing default host: %w", err)
		}
		rows.Close()
		if existingCount > 1 && !firstIsDefault {
			return nil
		}

		if existingID != "" {
			if err := lockHost(ctx, tx, existingID); err != nil {
				return err
			}
			workspaceRevisions, err := lockWorkspacesForBinding(ctx, tx, params.WorkspaceIDs, params.Host.OwnerID)
			if err != nil {
				return err
			}
			row, err := queries.GetHost(ctx, existingID)
			if err != nil {
				return fmt.Errorf("read locked default host: %w", err)
			}
			current, err := hostDomainWithBindings(ctx, queries, row)
			if err != nil {
				return err
			}
			activeCount := 0
			for _, binding := range current.Workspaces {
				if binding.State == workspace.BindingDetached {
					continue
				}
				activeCount++
				if binding.OwnerID != current.OwnerID || binding.Workspace.OwnerID != current.OwnerID ||
					binding.Workspace.State != workspace.StateReady || binding.Workspace.DeletedAt != nil ||
					(binding.State != workspace.BindingPreparing && binding.State != workspace.BindingActive) ||
					binding.SyncState != workspace.BindingSyncHealthy {
					return domain.NewError(domain.CodeWorkspaceBusy, "default host workspace binding is not available")
				}
			}
			if activeCount > 1 {
				return domain.NewError(domain.CodeWorkspaceConflict, "default host has multiple active workspace bindings")
			}
			if activeCount == 0 {
				if current.State != host.StateStopped {
					return domain.NewError(domain.CodeWorkspaceConflict, "default host is missing its workspace binding")
				}
				if err := ensureDefaultHostWorkspaceBinding(ctx, tx, existingID, current.OwnerID, workspaceID, workspaceRevisions[workspaceID]); err != nil {
					return err
				}
				row, err = queries.GetHost(ctx, existingID)
				if err != nil {
					return fmt.Errorf("read default host after workspace binding: %w", err)
				}
				current, err = hostDomainWithBindings(ctx, queries, row)
				if err != nil {
					return err
				}
			}
			result = current
		} else {
			if quotaHosts == 0 {
				return nil
			}
			workspaceRevisions, err := lockWorkspacesForBinding(ctx, tx, params.WorkspaceIDs, params.Host.OwnerID)
			if err != nil {
				return err
			}
			if err := lockHostVolumeCleanupID(ctx, tx, params.Host.ID); err != nil {
				return err
			}
			pendingCleanup, err := queries.HasPendingOrphanHostVolumeCleanup(ctx, params.Host.ID)
			if err != nil {
				return fmt.Errorf("check pending Host volume cleanup before default create: %w", err)
			}
			if pendingCleanup {
				return domain.NewError(domain.CodeConflict, "host id is pending orphan volume cleanup")
			}
			params.Host.CPUMilli, params.Host.CPULimitMilli = defaultCPU, defaultCPU
			params.Host.MemMiB, params.Host.MemLimitMiB = defaultMem, defaultMem
			if err := validateDefaultHostAllocation(ctx, tx, params.Host, quotaCPU, quotaMem, params.Capacity); err != nil {
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
				return fmt.Errorf("insert stopped default host: %w", err)
			}
			if err := insertDefaultHostWorkspaceBinding(ctx, tx, params.Host.ID, params.Host.OwnerID, workspaceID, workspaceRevisions[workspaceID]); err != nil {
				return err
			}
			existingID = params.Host.ID
			created = true
			row, err := queries.GetHost(ctx, existingID)
			if err != nil {
				return fmt.Errorf("read created default host: %w", err)
			}
			result, err = hostDomainWithBindings(ctx, queries, row)
			if err != nil {
				return err
			}
		}

		if _, err := tx.Exec(ctx, `INSERT INTO user_preferences (user_id, default_host_id, last_host_issue)
VALUES ($1, $2, '')
ON CONFLICT (user_id) DO UPDATE SET default_host_id=EXCLUDED.default_host_id,
  last_host_issue='', updated_at=clock_timestamp()`, params.Host.OwnerID, existingID); err != nil {
			return fmt.Errorf("set default host preference: %w", err)
		}
		return nil
	})
	if err != nil {
		return host.Host{}, false, err
	}
	return result, created, nil
}

func validateDefaultHostAllocation(ctx context.Context, tx pgx.Tx, value host.Host, quotaCPU, quotaMem int, capacity app.Capacity) error {
	if value.CPUMilli <= 0 || value.MemMiB <= 0 || quotaCPU < 0 || quotaMem < 0 {
		return domain.NewError(domain.CodeInvalidState, "default host resources are invalid")
	}
	var ownerCPU, ownerMemory int64
	if err := tx.QueryRow(ctx, allocatedResourceUsageSQL, value.OwnerID).Scan(&ownerCPU, &ownerMemory); err != nil {
		return fmt.Errorf("read default host owner allocation: %w", err)
	}
	if err := validateAllocation("cpu_milli", ownerCPU, int64(value.CPUMilli), int64(quotaCPU), domain.CodeQuotaExceeded); err != nil {
		return err
	}
	if err := validateAllocation("mem_mib", ownerMemory, int64(value.MemMiB), int64(quotaMem), domain.CodeQuotaExceeded); err != nil {
		return err
	}
	var clusterCPU, clusterMemory int64
	if err := tx.QueryRow(ctx, allocatedResourceUsageSQL, nil).Scan(&clusterCPU, &clusterMemory); err != nil {
		return fmt.Errorf("read default host cluster allocation: %w", err)
	}
	if err := validateAllocation("cpu_milli", clusterCPU, int64(value.CPUMilli), capacity.CPUMilli, domain.CodeCapacityExceeded); err != nil {
		return err
	}
	return validateAllocation("mem_mib", clusterMemory, int64(value.MemMiB), capacity.MemMiB, domain.CodeCapacityExceeded)
}

func insertDefaultHostWorkspaceBinding(ctx context.Context, tx pgx.Tx, hostID, ownerID, workspaceID string, revision int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO workspace_bindings (
  id, owner_id, host_id, workspace_id, directory_name, state,
  binding_version, desired_revision, applied_revision
)
VALUES ($1, $2, $3, $4, $4, 'preparing', 1, $5, 0)`,
		bindingID(hostID, workspaceID), ownerID, hostID, workspaceID, revision)
	if err != nil {
		return workspaceBindingInsertError(err)
	}
	return nil
}

func ensureDefaultHostWorkspaceBinding(ctx context.Context, tx pgx.Tx, hostID, ownerID, workspaceID string, revision int64) error {
	var id, state string
	var version int64
	err := tx.QueryRow(ctx, `SELECT id, state, binding_version FROM workspace_bindings
WHERE host_id=$1 AND workspace_id=$2 FOR UPDATE`, hostID, workspaceID).Scan(&id, &state, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return insertDefaultHostWorkspaceBinding(ctx, tx, hostID, ownerID, workspaceID, revision)
	}
	if err != nil {
		return fmt.Errorf("read previous default host workspace binding: %w", err)
	}
	if state != string(workspace.BindingDetached) {
		return domain.NewError(domain.CodeWorkspaceConflict, "default host workspace binding is not detached")
	}
	result, err := tx.Exec(ctx, `UPDATE workspace_bindings
SET state='preparing', binding_version=binding_version+1, desired_revision=$4, updated_at=now()
WHERE id=$1 AND host_id=$2 AND state='detached' AND binding_version=$3`, id, hostID, version, revision)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.NewError(domain.CodeWorkspaceBusy, "workspace is already bound to a host")
		}
		return fmt.Errorf("restore default host workspace binding: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeWorkspaceConflict, "default host workspace binding changed")
	}
	return nil
}
