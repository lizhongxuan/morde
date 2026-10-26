package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

// MarkCreateAttempt commits the one-shot create marker before a caller sends a
// non-idempotent request. The conditional update prevents a retry from sending
// that request a second time.
func (s *Store) MarkCreateAttempt(ctx context.Context, hostID string) error {
	result, err := s.pool.Exec(ctx, `
UPDATE hosts
SET create_attempted_at = now(), updated_at = now()
WHERE id = $1 AND state = 'creating' AND create_attempted_at IS NULL`, hostID)
	if err != nil {
		return fmt.Errorf("mark host create attempt: %w", err)
	}
	if result.RowsAffected() == 1 {
		return nil
	}

	current, err := s.GetHost(ctx, hostID)
	if errors.Is(err, pgx.ErrNoRows) || domain.CodeOf(err) == domain.CodeNotFound {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return fmt.Errorf("check host create attempt: %w", err)
	}
	return hostStateConflict(current)
}

func (s *Store) FailUnattemptedCreate(ctx context.Context, hostID, reason string) (host.Host, bool, error) {
	result, err := s.pool.Exec(ctx, `
UPDATE hosts
SET state = CASE WHEN runtime_generation > 1 THEN 'stopped' ELSE 'failed' END,
    state_reason = CASE WHEN runtime_generation > 1 THEN 'restore_failed' ELSE $2 END,
    updated_at = now()
WHERE id = $1 AND state = 'creating' AND create_attempted_at IS NULL`, hostID, reason)
	if err != nil {
		return host.Host{}, false, fmt.Errorf("fail unattempted host creation: %w", err)
	}
	if result.RowsAffected() == 1 {
		value, err := s.GetHost(ctx, hostID)
		return value, err == nil, err
	}
	current, err := s.GetHost(ctx, hostID)
	if errors.Is(err, pgx.ErrNoRows) || domain.CodeOf(err) == domain.CodeNotFound {
		return host.Host{}, false, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return host.Host{}, false, fmt.Errorf("check host after create timeout: %w", err)
	}
	if current.State == host.StateCreating && current.CreateAttemptedAt != nil {
		return current, false, nil
	}
	return host.Host{}, false, hostStateConflict(current)
}

// TransitionHost performs a compare-and-swap update. A stale expected state
// never overwrites a concurrent transition.
func (s *Store) TransitionHost(ctx context.Context, hostID string, from, to host.State, reason string) (host.Host, error) {
	var updated host.Host
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockHost(ctx, tx, hostID); err != nil {
			return err
		}
		queries := sqlcgen.New(tx)
		currentRow, err := queries.GetHost(ctx, hostID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		if err != nil {
			return fmt.Errorf("read host before transition: %w", err)
		}
		if currentRow.State != string(from) {
			current := hostDomain(currentRow)
			return hostStateConflict(current)
		}
		if to == host.StateDeleted && from != host.StateDeleting {
			return domain.NewError(domain.CodeInvalidState, "Host can only be marked deleted after sandbox cleanup from deleting state")
		}
		if to == host.StateDeleting {
			if currentRow.ControlState == string(host.ControlTakingOver) || currentRow.ControlState == string(host.ControlReturning) {
				return domain.NewError(domain.CodeConflict, "host control transition is in progress; retry deletion after it completes")
			}
			if err := checkPendingApplicationInstall(ctx, tx, hostID); err != nil {
				return err
			}
			if currentRow.RuntimeGeneration > 1 && (currentRow.StateReason == "restore_unknown" ||
				(currentRow.State == string(host.StateCreating) && currentRow.CreateAttemptedAt.Valid)) {
				return domain.NewError(domain.CodeConflict, "restore create outcome is uncertain; contact an operator before deleting this host")
			}
			var hasAttachedWorkspace bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM workspace_bindings WHERE host_id=$1 AND state <> 'detached'
)`, hostID).Scan(&hasAttachedWorkspace); err != nil {
				return fmt.Errorf("check workspace bindings before host deletion: %w", err)
			}
			if hasAttachedWorkspace {
				return domain.NewError(domain.CodeWorkspaceBusy, "unbind and verify all workspaces before deleting this host")
			}
			if err := beginHostSiteDrain(ctx, tx, hostID); err != nil {
				return err
			}
		}
		result, err := tx.Exec(ctx, `
UPDATE hosts
SET state = $3, state_reason = $4,
    deleted_at = CASE WHEN $3 = 'deleted' THEN now() ELSE deleted_at END,
    updated_at = now()
WHERE id = $1 AND state = $2
`, hostID, string(from), string(to), reason)
		if err != nil {
			return fmt.Errorf("transition host state: %w", err)
		}
		if result.RowsAffected() != 1 {
			return hostStateConflict(hostDomain(currentRow))
		}
		if to == host.StateDeleted {
			if err := completeHostSiteDrain(ctx, tx, hostID); err != nil {
				return err
			}
		}
		row, err := queries.GetHost(ctx, hostID)
		if err != nil {
			return fmt.Errorf("read transitioned host: %w", err)
		}
		updated, err = hostDomainWithBindings(ctx, queries, row)
		return err
	})
	if err != nil {
		return host.Host{}, err
	}
	return updated, nil
}

// BeginHostRelease closes admission under the Host row lock, then checks
// active terminal sessions, tasks, and MCP work before changing state to stopping.
func (s *Store) BeginHostRelease(ctx context.Context, hostID string, confirmProcessTermination bool) (host.Host, bool, error) {
	if !domain.ValidHostID(hostID) {
		return host.Host{}, false, domain.InvalidArgument("host.release", "host release request is invalid")
	}
	var result host.Host
	changed := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockHost(ctx, tx, hostID); err != nil {
			return err
		}
		queries := sqlcgen.New(tx)
		row, err := queries.GetHost(ctx, hostID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		if err != nil {
			return fmt.Errorf("read host before release: %w", err)
		}
		result, err = hostDomainWithBindings(ctx, queries, row)
		if err != nil {
			return err
		}
		if result.State == host.StateStopping || result.State == host.StateStopped {
			return nil
		}
		if result.State != host.StateRunning {
			return hostStateConflict(result)
		}
		if result.ControlState != host.ControlAI {
			return nil
		}
		if err := checkPendingApplicationInstall(ctx, tx, hostID); err != nil {
			return err
		}
		var activeTerminalSession bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM host_terminal_sessions
  WHERE host_id=$1 AND runtime_generation=$2 AND state IN ('creating','ready','unknown')
)`, hostID, result.RuntimeGeneration).Scan(&activeTerminalSession); err != nil {
			return fmt.Errorf("check terminal sessions before release: %w", err)
		}
		if activeTerminalSession && !confirmProcessTermination {
			return domain.NewError(domain.CodeConflict, "terminal session may contain running processes; confirm process termination before release")
		}
		var activeApplicationLaunch bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM host_application_launches
  WHERE host_id=$1 AND runtime_generation=$2 AND status IN ('starting','running','stopping','unknown')
)`, hostID, result.RuntimeGeneration).Scan(&activeApplicationLaunch); err != nil {
			return fmt.Errorf("check application launches before release: %w", err)
		}
		if activeApplicationLaunch && !confirmProcessTermination {
			return domain.NewError(domain.CodeConflict, "application may still be running; confirm process termination before release")
		}
		var active bool
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM tasks
  WHERE host_id=$1 AND status IN ('queued','running','waiting_user','pausing','cancelling')
)`, hostID).Scan(&active); err != nil {
			return fmt.Errorf("check active host tasks before release: %w", err)
		}
		if active {
			return nil
		}
		if err := tx.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM mcp_runtime_operations
  WHERE host_id=$1 AND status IN ('prepared','dispatched','unknown')
)`, hostID).Scan(&active); err != nil {
			return fmt.Errorf("check active MCP runtime operations before release: %w", err)
		}
		if active {
			return nil
		}
		if err := beginHostSiteDrain(ctx, tx, hostID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
UPDATE hosts SET state='stopping', state_reason='user_release', updated_at=now()
WHERE id=$1 AND state='running' AND control_state='ai'`, hostID)
		if err != nil {
			return fmt.Errorf("begin host compute release: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return hostStateConflict(result)
		}
		row, err = queries.GetHost(ctx, hostID)
		if err != nil {
			return fmt.Errorf("read host after release transition: %w", err)
		}
		result, err = hostDomainWithBindings(ctx, queries, row)
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return host.Host{}, false, err
	}
	return result, changed, nil
}

// CompleteHostRelease is the only transition that marks the Host stopped.
// The caller must first confirm deletion of every sandbox owned by the Host;
// data volumes and workspace attachment remain untouched.
func (s *Store) CompleteHostRelease(ctx context.Context, hostID string, generation int64) (host.Host, error) {
	if !domain.ValidHostID(hostID) || generation <= 0 {
		return host.Host{}, domain.InvalidArgument("host.release", "host release identity is invalid")
	}
	var updated host.Host
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockHost(ctx, tx, hostID); err != nil {
			return err
		}
		queries := sqlcgen.New(tx)
		current, err := queries.GetHost(ctx, hostID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		if err != nil {
			return fmt.Errorf("read Host before compute release completion: %w", err)
		}
		if current.State != string(host.StateStopping) || current.RuntimeGeneration != generation {
			return domain.NewError(domain.CodeInvalidState, "Host release generation changed")
		}
		if err := checkPendingApplicationInstall(ctx, tx, hostID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE host_application_launches
SET status='stopped', error_code='', updated_at=clock_timestamp()
WHERE host_id=$1 AND runtime_generation=$2 AND status IN ('starting','running','stopping','unknown')`, hostID, generation); err != nil {
			return fmt.Errorf("mark application launches stopped after sandbox deletion: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE host_terminal_sessions
SET state='interrupted', aio_session_id=NULL, updated_at=clock_timestamp()
WHERE host_id=$1 AND runtime_generation=$2 AND state IN ('creating','ready','unknown')`, hostID, generation); err != nil {
			return fmt.Errorf("interrupt terminal sessions after sandbox deletion: %w", err)
		}
		result, err := tx.Exec(ctx, `
UPDATE hosts
SET state='stopped', sandbox_id=NULL, runtime_instance_id=NULL, updated_at=now()
	WHERE id=$1 AND state='stopping' AND runtime_generation=$2`, hostID, generation)
		if err != nil {
			return fmt.Errorf("complete host compute release: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeInvalidState, "Host release generation changed")
		}
		if err := completeHostSiteDrain(ctx, tx, hostID); err != nil {
			return err
		}
		row, err := queries.GetHost(ctx, hostID)
		if err != nil {
			return fmt.Errorf("read Host after compute release completion: %w", err)
		}
		updated, err = hostDomainWithBindings(ctx, queries, row)
		return err
	})
	if err != nil {
		return host.Host{}, err
	}
	return updated, nil
}

// BeginHostRestore atomically reserves capacity, increments the generation,
// rotates the runtime credential, and marks one stopped Host creating. The
// workspace ID and Docker volume names are deliberately never changed.
func (s *Store) BeginHostRestore(ctx context.Context, params app.RestoreHostParams) (host.Host, bool, error) {
	if !domain.ValidHostID(params.HostID) || len(params.RuntimeCredentialEnc) == 0 ||
		params.Capacity.CPUMilli <= 0 || params.Capacity.MemMiB <= 0 || !params.ExpiresAt.After(time.Now()) {
		return host.Host{}, false, domain.InvalidArgument("host.restore", "host restore request is invalid")
	}
	owner, err := s.GetHost(ctx, params.HostID)
	if err != nil {
		return host.Host{}, false, err
	}
	var result host.Host
	changed := false
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, owner.OwnerID); err != nil {
			return fmt.Errorf("lock host owner allocation for restore: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, createCapacityLockKey); err != nil {
			return fmt.Errorf("lock host capacity for restore: %w", err)
		}
		if err := lockHost(ctx, tx, params.HostID); err != nil {
			return err
		}
		queries := sqlcgen.New(tx)
		row, err := queries.GetHost(ctx, params.HostID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		if err != nil {
			return fmt.Errorf("read host before restore: %w", err)
		}
		current, err := hostDomainWithBindings(ctx, queries, row)
		if err != nil {
			return err
		}
		result = current
		if current.State == host.StateCreating && current.RuntimeGeneration > 1 &&
			(current.StateReason == "restore_pending" || current.StateReason == "restore_unknown") {
			return nil
		}
		if current.State == host.StateRunning && current.RuntimeGeneration > 1 {
			return nil
		}
		if current.State != host.StateStopped || current.DeletedAt != nil || current.ControlState != host.ControlAI {
			return hostStateConflict(current)
		}
		if current.RuntimeGeneration <= 0 || current.RuntimeGeneration == math.MaxInt64 {
			return domain.NewError(domain.CodeInvalidState, "host runtime generation cannot be advanced")
		}
		user, err := queries.GetUserByID(ctx, current.OwnerID)
		if err != nil {
			return storeError("get host owner for restore", "user", err)
		}
		var ownerHosts, ownerCPU, ownerMemory int64
		if err := tx.QueryRow(ctx, allocatedUsageSQL, current.OwnerID).Scan(&ownerHosts, &ownerCPU, &ownerMemory); err != nil {
			return fmt.Errorf("read owner host allocation for restore: %w", err)
		}
		if err := validateAllocation("host_count", ownerHosts, 1, int64(user.QuotaHosts), domain.CodeQuotaExceeded); err != nil {
			return err
		}
		if err := validateAllocation("cpu_milli", ownerCPU, int64(current.CPUMilli), int64(user.QuotaCpuMilli), domain.CodeQuotaExceeded); err != nil {
			return err
		}
		if err := validateAllocation("mem_mib", ownerMemory, int64(current.MemMiB), int64(user.QuotaMemMib), domain.CodeQuotaExceeded); err != nil {
			return err
		}
		var totalHosts, totalCPU, totalMemory int64
		if err := tx.QueryRow(ctx, allocatedUsageSQL, nil).Scan(&totalHosts, &totalCPU, &totalMemory); err != nil {
			return fmt.Errorf("read cluster host allocation for restore: %w", err)
		}
		_ = totalHosts
		if err := validateAllocation("cpu_milli", totalCPU, int64(current.CPUMilli), params.Capacity.CPUMilli, domain.CodeCapacityExceeded); err != nil {
			return err
		}
		if err := validateAllocation("mem_mib", totalMemory, int64(current.MemMiB), params.Capacity.MemMiB, domain.CodeCapacityExceeded); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
UPDATE hosts
SET state='creating', state_reason='restore_pending', runtime_generation=runtime_generation+1,
    control_version=0, control_state='ai', runtime_instance_id=NULL, sandbox_id=NULL,
    create_attempted_at=NULL, runtime_credential_enc=$2, expires_at=$3,
    last_active_at=now(), updated_at=now()
WHERE id=$1 AND state='stopped' AND runtime_generation=$4 AND deleted_at IS NULL`,
			params.HostID, params.RuntimeCredentialEnc, params.ExpiresAt, current.RuntimeGeneration)
		if err != nil {
			return fmt.Errorf("begin host restore: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return hostStateConflict(current)
		}
		row, err = queries.GetHost(ctx, params.HostID)
		if err != nil {
			return fmt.Errorf("read host after restore transition: %w", err)
		}
		result, err = hostDomainWithBindings(ctx, queries, row)
		if err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return host.Host{}, false, err
	}
	return result, changed, nil
}

func (s *Store) SetHostStateReason(ctx context.Context, hostID string, expected host.State, generation int64, reason string) error {
	if !domain.ValidHostID(hostID) || generation <= 0 || reason != "restore_unknown" {
		return domain.InvalidArgument("host.state_reason", "host state reason is invalid")
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE hosts SET state_reason=$4, updated_at=now()
WHERE id=$1 AND state=$2 AND runtime_generation=$3 AND create_attempted_at IS NOT NULL`,
		hostID, string(expected), generation, reason)
	if err != nil {
		return fmt.Errorf("set host state reason: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	current, err := s.GetHost(ctx, hostID)
	if errors.Is(err, pgx.ErrNoRows) || domain.CodeOf(err) == domain.CodeNotFound {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return fmt.Errorf("check host state reason: %w", err)
	}
	return hostStateConflict(current)
}

func lockHost(ctx context.Context, tx pgx.Tx, hostID string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM hosts WHERE id=$1 FOR UPDATE`, hostID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return fmt.Errorf("lock host row: %w", err)
	}
	return nil
}

func hostStateConflict(current host.Host) error {
	err := domain.NewError(domain.CodeInvalidState, "host state changed")
	err.Details["state"] = string(current.State)
	return err
}

func workspaceStateConflict(current workspace.Workspace) error {
	err := domain.NewError(domain.CodeInvalidState, "workspace state changed")
	err.Details["state"] = string(current.State)
	return err
}
