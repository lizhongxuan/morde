package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

func (s *Store) GetHostResources(ctx context.Context, hostID string) (app.HostResourceSnapshot, error) {
	if !domain.ValidHostID(hostID) {
		return app.HostResourceSnapshot{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	var state string
	var cpuMilli, memMiB int
	var version int64
	var updateState string
	err := s.pool.QueryRow(ctx, `
SELECT state, cpu_milli, mem_mib, resource_version, resource_update_state
FROM hosts WHERE id=$1 AND deleted_at IS NULL`, hostID).Scan(&state, &cpuMilli, &memMiB, &version, &updateState)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.HostResourceSnapshot{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return app.HostResourceSnapshot{}, fmt.Errorf("get host resources: %w", err)
	}
	return app.HostResourceSnapshot{
		HostID: hostID, State: host.State(state),
		Resources: app.HostResources{CPUMilli: cpuMilli, MemMiB: memMiB}, Version: version, UpdateState: updateState,
	}, nil
}

// UpdateStoppedHostResources changes the next-start request and limit together.
// Running hosts use BeginOnlineHostResources followed by runtime verification.
func (s *Store) UpdateStoppedHostResources(ctx context.Context, params app.UpdateStoppedHostResourcesParams) (app.HostResourceSnapshot, error) {
	actorAllowed := params.ActorKind == audit.ActorSession && domain.ValidUserID(params.ActorID) ||
		params.ActorKind == audit.ActorSystem && params.ActorID == "system"
	if !domain.ValidHostID(params.HostID) || params.ExpectedVersion < 1 ||
		params.Resources.CPUMilli < 1 || params.Resources.MemMiB < 1 ||
		params.Resources.CPUMilli > math.MaxInt32 || params.Resources.MemMiB > math.MaxInt32 ||
		params.Capacity.CPUMilli <= 0 || params.Capacity.MemMiB <= 0 || !actorAllowed {
		return app.HostResourceSnapshot{}, domain.InvalidArgument("host.resources", "host resource update parameters are invalid")
	}
	var updated app.HostResourceSnapshot
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID string
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM hosts WHERE id=$1`, params.HostID).Scan(&ownerID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "host not found")
			}
			return fmt.Errorf("read host owner for resource update: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, ownerID); err != nil {
			return fmt.Errorf("lock host owner allocation for resource update: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, createCapacityLockKey); err != nil {
			return fmt.Errorf("lock host capacity allocation for resource update: %w", err)
		}
		var state string
		var oldCPU, oldMemory int
		var version int64
		var hasSandbox, hasInstance, deleted bool
		var updateState string
		if err := tx.QueryRow(ctx, `
SELECT state, cpu_milli, mem_mib, resource_version, resource_update_state,
       sandbox_id IS NOT NULL, NULLIF(runtime_instance_id, '') IS NOT NULL, deleted_at IS NOT NULL
FROM hosts WHERE id=$1 FOR UPDATE`, params.HostID).Scan(
			&state, &oldCPU, &oldMemory, &version, &updateState, &hasSandbox, &hasInstance, &deleted,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "host not found")
			}
			return fmt.Errorf("lock host for resource update: %w", err)
		}
		if deleted {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		if updateState != "idle" && (state != string(host.StateStopped) || hasSandbox || hasInstance) {
			return domain.NewError(domain.CodeConflict, "host resource update requires reconciliation")
		}
		if state == string(host.StateRunning) {
			return domain.NewError(domain.CodeUnsupported, "running host requires online resource update")
		}
		if state != string(host.StateStopped) || hasSandbox || hasInstance {
			return domain.NewError(domain.CodeInvalidState, "host must be fully stopped to change resources")
		}
		if version != params.ExpectedVersion || version == math.MaxInt64 {
			return domain.NewError(domain.CodeConflict, "host resources changed; refresh and retry")
		}

		owner, err := sqlcgen.New(tx).GetUserByID(ctx, ownerID)
		if err != nil {
			return storeError("get host owner for resource update", "user", err)
		}
		var ownerCPU, ownerMemory int64
		if err := tx.QueryRow(ctx, allocatedResourceUsageSQL, ownerID).Scan(&ownerCPU, &ownerMemory); err != nil {
			return fmt.Errorf("read owner host allocation for resource update: %w", err)
		}
		if err := validateAllocation("cpu_milli", ownerCPU, int64(params.Resources.CPUMilli), int64(owner.QuotaCpuMilli), domain.CodeQuotaExceeded); err != nil {
			return err
		}
		if err := validateAllocation("mem_mib", ownerMemory, int64(params.Resources.MemMiB), int64(owner.QuotaMemMib), domain.CodeQuotaExceeded); err != nil {
			return err
		}
		var clusterCPU, clusterMemory int64
		if err := tx.QueryRow(ctx, allocatedResourceUsageSQL, nil).Scan(&clusterCPU, &clusterMemory); err != nil {
			return fmt.Errorf("read cluster host allocation for resource update: %w", err)
		}
		if err := validateAllocation("cpu_milli", clusterCPU, int64(params.Resources.CPUMilli), params.Capacity.CPUMilli, domain.CodeCapacityExceeded); err != nil {
			return err
		}
		if err := validateAllocation("mem_mib", clusterMemory, int64(params.Resources.MemMiB), params.Capacity.MemMiB, domain.CodeCapacityExceeded); err != nil {
			return err
		}

		var nextVersion int64
		err = tx.QueryRow(ctx, `
UPDATE hosts SET cpu_milli=$3, mem_mib=$4, cpu_limit_milli=$3, mem_limit_mib=$4,
                 resource_update_state='idle', resource_target_cpu_milli=NULL, resource_target_mem_mib=NULL,
                 resource_version=resource_version+1, updated_at=now()
WHERE id=$1 AND resource_version=$2 AND state='stopped' AND deleted_at IS NULL
  AND sandbox_id IS NULL AND NULLIF(runtime_instance_id, '') IS NULL
RETURNING resource_version`, params.HostID, params.ExpectedVersion,
			params.Resources.CPUMilli, params.Resources.MemMiB).Scan(&nextVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "host changed before resource update")
		}
		if err != nil {
			return fmt.Errorf("update stopped host resources: %w", err)
		}
		entry, err := auditParams(audit.Entry{
			ActorID: params.ActorID, ActorKind: params.ActorKind,
			Action: audit.Action("host.resources.update"), Target: params.HostID,
			Detail: map[string]any{
				"old":              map[string]int{"cpu_milli": oldCPU, "mem_mib": oldMemory},
				"new":              map[string]int{"cpu_milli": params.Resources.CPUMilli, "mem_mib": params.Resources.MemMiB},
				"resource_version": nextVersion,
			},
		})
		if err != nil {
			return err
		}
		if _, err := sqlcgen.New(tx).CreateAuditLog(ctx, entry); err != nil {
			return fmt.Errorf("write host resource update audit: %w", err)
		}
		updated = app.HostResourceSnapshot{
			HostID: params.HostID, State: host.StateStopped, Resources: params.Resources, Version: nextVersion, UpdateState: "idle",
		}
		return nil
	})
	if err != nil {
		return app.HostResourceSnapshot{}, err
	}
	return updated, nil
}

var _ app.HostResourceStore = (*Store)(nil)

func (s *Store) BeginOnlineHostResources(ctx context.Context, params app.UpdateStoppedHostResourcesParams) (app.OnlineHostResourceUpdate, error) {
	actorAllowed := params.ActorKind == audit.ActorSession && domain.ValidUserID(params.ActorID) ||
		params.ActorKind == audit.ActorSystem && params.ActorID == "system"
	if !domain.ValidHostID(params.HostID) || params.ExpectedVersion < 1 || params.Resources.CPUMilli < 1 ||
		params.Resources.MemMiB < 1 || params.Resources.CPUMilli > math.MaxInt32 || params.Resources.MemMiB > math.MaxInt32 ||
		params.Capacity.CPUMilli < 1 || params.Capacity.MemMiB < 1 || !actorAllowed {
		return app.OnlineHostResourceUpdate{}, domain.InvalidArgument("host.resources", "online resource update parameters are invalid")
	}
	var result app.OnlineHostResourceUpdate
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID string
		if err := tx.QueryRow(ctx, `SELECT owner_id FROM hosts WHERE id=$1`, params.HostID).Scan(&ownerID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "host not found")
			}
			return fmt.Errorf("read host owner for online resource update: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, ownerID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, createCapacityLockKey); err != nil {
			return err
		}
		var state, updateState, profile, sandboxID, instanceID string
		var oldCPU, oldMemory, oldLimitCPU, oldLimitMemory int
		var version, generation int64
		var deleted bool
		if err := tx.QueryRow(ctx, `
SELECT state, resource_update_state, runtime_profile, COALESCE(sandbox_id,''), COALESCE(runtime_instance_id,''),
       cpu_milli, mem_mib, cpu_limit_milli, mem_limit_mib, resource_version, runtime_generation, deleted_at IS NOT NULL
FROM hosts WHERE id=$1 FOR UPDATE`, params.HostID).Scan(
			&state, &updateState, &profile, &sandboxID, &instanceID, &oldCPU, &oldMemory,
			&oldLimitCPU, &oldLimitMemory, &version, &generation, &deleted,
		); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "host not found")
			}
			return fmt.Errorf("lock online host resources: %w", err)
		}
		if deleted {
			return domain.NewError(domain.CodeNotFound, "host not found")
		}
		if version != params.ExpectedVersion || version == math.MaxInt64 || updateState != "idle" {
			return domain.NewError(domain.CodeConflict, "host resources changed; refresh and retry")
		}
		if state != string(host.StateRunning) || sandboxID == "" || instanceID == "" {
			return domain.NewError(domain.CodeInvalidState, "host must be running to change resources online")
		}
		owner, err := sqlcgen.New(tx).GetUserByID(ctx, ownerID)
		if err != nil {
			return storeError("get host owner for online resource update", "user", err)
		}
		var ownerCPU, ownerMemory, clusterCPU, clusterMemory int64
		if err := tx.QueryRow(ctx, allocatedResourceUsageSQL, ownerID).Scan(&ownerCPU, &ownerMemory); err != nil {
			return fmt.Errorf("read owner resource allocation: %w", err)
		}
		if err := tx.QueryRow(ctx, allocatedResourceUsageSQL, nil).Scan(&clusterCPU, &clusterMemory); err != nil {
			return fmt.Errorf("read cluster resource allocation: %w", err)
		}
		// Until runtime confirmation, both the previous and requested limits may
		// consume capacity. Reserve the larger value for each dimension.
		cpuIncrease := int64(max(oldCPU, oldLimitCPU, params.Resources.CPUMilli) - oldCPU)
		memoryIncrease := int64(max(oldMemory, oldLimitMemory, params.Resources.MemMiB) - oldMemory)
		if err := validateAllocation("cpu_milli", ownerCPU, cpuIncrease, int64(owner.QuotaCpuMilli), domain.CodeQuotaExceeded); err != nil {
			return err
		}
		if err := validateAllocation("mem_mib", ownerMemory, memoryIncrease, int64(owner.QuotaMemMib), domain.CodeQuotaExceeded); err != nil {
			return err
		}
		if err := validateAllocation("cpu_milli", clusterCPU, cpuIncrease, params.Capacity.CPUMilli, domain.CodeCapacityExceeded); err != nil {
			return err
		}
		if err := validateAllocation("mem_mib", clusterMemory, memoryIncrease, params.Capacity.MemMiB, domain.CodeCapacityExceeded); err != nil {
			return err
		}
		var nextVersion int64
		if err := tx.QueryRow(ctx, `
UPDATE hosts SET resource_update_state='applying', resource_target_cpu_milli=$3,
                 resource_target_mem_mib=$4, resource_version=resource_version+1, updated_at=now()
WHERE id=$1 AND resource_version=$2 AND state='running' AND resource_update_state='idle'
RETURNING resource_version`, params.HostID, version, params.Resources.CPUMilli, params.Resources.MemMiB).Scan(&nextVersion); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeConflict, "host changed before online resource update")
			}
			return fmt.Errorf("reserve online host resources: %w", err)
		}
		result = app.OnlineHostResourceUpdate{
			HostID: params.HostID, RuntimeProfile: profile, SandboxID: sandboxID,
			RuntimeInstanceID: instanceID, RuntimeGeneration: generation,
			Old:        app.HostResources{CPUMilli: oldCPU, MemMiB: oldMemory},
			OldRuntime: app.HostResources{CPUMilli: oldLimitCPU, MemMiB: oldLimitMemory}, Target: params.Resources,
			ReservedVersion: nextVersion, ActorID: params.ActorID, ActorKind: params.ActorKind,
		}
		return nil
	})
	if err != nil {
		return app.OnlineHostResourceUpdate{}, err
	}
	return result, nil
}

func (s *Store) CompleteOnlineHostResources(ctx context.Context, update app.OnlineHostResourceUpdate) (app.HostResourceSnapshot, error) {
	var result app.HostResourceSnapshot
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var nextVersion int64
		err := tx.QueryRow(ctx, `
UPDATE hosts SET cpu_milli=$3, mem_mib=$4, cpu_limit_milli=$3, mem_limit_mib=$4,
                 resource_update_state='idle', resource_target_cpu_milli=NULL, resource_target_mem_mib=NULL,
                 resource_version=resource_version+1, updated_at=now()
WHERE id=$1 AND resource_version=$2 AND state='running' AND deleted_at IS NULL
  AND resource_update_state='applying' AND runtime_profile=$5 AND sandbox_id=$6
  AND runtime_instance_id=$7 AND runtime_generation=$8
  AND cpu_milli=$9 AND mem_mib=$10
  AND resource_target_cpu_milli=$3 AND resource_target_mem_mib=$4
RETURNING resource_version`, update.HostID, update.ReservedVersion, update.Target.CPUMilli, update.Target.MemMiB,
			update.RuntimeProfile, update.SandboxID, update.RuntimeInstanceID, update.RuntimeGeneration,
			update.Old.CPUMilli, update.Old.MemMiB).Scan(&nextVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "host changed during online resource update")
		}
		if err != nil {
			return fmt.Errorf("complete online host resources: %w", err)
		}
		entry, err := auditParams(audit.Entry{
			ActorID: update.ActorID, ActorKind: update.ActorKind,
			Action: audit.Action("host.resources.update"), Target: update.HostID,
			Detail: map[string]any{
				"old":              map[string]int{"cpu_milli": update.Old.CPUMilli, "mem_mib": update.Old.MemMiB},
				"new":              map[string]int{"cpu_milli": update.Target.CPUMilli, "mem_mib": update.Target.MemMiB},
				"resource_version": nextVersion, "online": true,
			},
		})
		if err != nil {
			return err
		}
		if _, err := sqlcgen.New(tx).CreateAuditLog(ctx, entry); err != nil {
			return fmt.Errorf("write online host resource audit: %w", err)
		}
		result = app.HostResourceSnapshot{HostID: update.HostID, State: host.StateRunning, Resources: update.Target, Version: nextVersion, UpdateState: "idle"}
		return nil
	})
	if err != nil {
		return app.HostResourceSnapshot{}, err
	}
	return result, nil
}

func (s *Store) AbortOnlineHostResources(ctx context.Context, update app.OnlineHostResourceUpdate, unknown bool) error {
	state := "idle"
	if unknown {
		state = "unknown"
	}
	command, err := s.pool.Exec(ctx, `
UPDATE hosts SET resource_update_state=$3,
                 resource_target_cpu_milli=CASE WHEN $4 THEN resource_target_cpu_milli ELSE NULL END,
                 resource_target_mem_mib=CASE WHEN $4 THEN resource_target_mem_mib ELSE NULL END,
                 resource_version=resource_version+1, updated_at=now()
WHERE id=$1 AND resource_version=$2 AND resource_update_state='applying'
  AND runtime_profile=$5 AND sandbox_id=$6 AND runtime_instance_id=$7 AND runtime_generation=$8`,
		update.HostID, update.ReservedVersion, state, unknown,
		update.RuntimeProfile, update.SandboxID, update.RuntimeInstanceID, update.RuntimeGeneration)
	if err != nil {
		return fmt.Errorf("record online host resource failure: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "host changed during online resource failure")
	}
	return nil
}
