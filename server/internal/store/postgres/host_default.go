package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/store/postgres/sqlcgen"
)

// EnsureDefaultHost uses the same owner lock as normal host creation. The
// stopped host and its preference are committed together, so concurrent logins
// cannot allocate more than one default computer.
func (s *Store) EnsureDefaultHost(ctx context.Context, params app.CreateHostParams) (host.Host, bool, error) {
	if params.Host.State != host.StateStopped || len(params.WorkspaceIDs) != 0 || params.Idempotency.Key != "" {
		return host.Host{}, false, domain.InvalidArgument("host", "default host must be stopped and unbound")
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
		queries := sqlcgen.New(tx)
		var quotaHosts, defaultCPU, defaultMem int
		var status string
		if err := tx.QueryRow(ctx, `SELECT quota_hosts, default_host_cpu_milli, default_host_mem_mib, status
FROM users WHERE id=$1 FOR UPDATE`, params.Host.OwnerID).Scan(&quotaHosts, &defaultCPU, &defaultMem, &status); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "user not found")
			}
			return fmt.Errorf("read default host owner: %w", err)
		}
		if status != "approved" {
			return domain.NewError(domain.CodeForbidden, "account is not approved")
		}
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
		if existingID == "" {
			if quotaHosts == 0 {
				return nil
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
			existingID = params.Host.ID
			created = true
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_preferences (user_id, default_host_id, last_host_issue)
VALUES ($1, $2, '')
ON CONFLICT (user_id) DO UPDATE SET default_host_id=EXCLUDED.default_host_id,
  last_host_issue='', updated_at=clock_timestamp()`, params.Host.OwnerID, existingID); err != nil {
			return fmt.Errorf("set default host preference: %w", err)
		}
		row, err := queries.GetHost(ctx, existingID)
		if err != nil {
			return fmt.Errorf("read default host: %w", err)
		}
		result, err = hostDomainWithBindings(ctx, queries, row)
		return err
	})
	if err != nil {
		return host.Host{}, false, err
	}
	return result, created, nil
}
