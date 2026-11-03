package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/project"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const bytesPerMiB = float64(1024 * 1024)

func (s *Store) RecordHostUsageObservation(ctx context.Context, observation host.UsageObservation, maxGap time.Duration) (host.UsageInterval, error) {
	if err := observation.Validate(); err != nil || maxGap <= 0 || maxGap > time.Minute {
		return host.UsageInterval{}, fmt.Errorf("record Host usage observation: invalid observation or gap")
	}
	var result host.UsageInterval
	err := withTx(ctx, s.pool.Begin, func(tx pgx.Tx) error {
		var generation int64
		var sandboxID, state, ownerID string
		if err := tx.QueryRow(ctx, `SELECT runtime_generation,sandbox_id,state,owner_id FROM hosts WHERE id=$1 FOR UPDATE`, observation.HostID).
			Scan(&generation, &sandboxID, &state, &ownerID); err != nil {
			return storeError("lock Host for usage observation", "host", err)
		}
		identityMatches := generation == observation.RuntimeGeneration && sandboxID == observation.SandboxID
		if !identityMatches {
			observation.State, observation.Reason = host.UsageObservationUnknown, host.UsageGenerationChange
		} else if state != string(host.StateRunning) && observation.State != host.UsageObservationUnknown {
			observation.State, observation.Reason = host.UsageObservationUnknown, host.UsageHostNotRunning
		}
		if observation.State == host.UsageObservationSampled && observation.Metrics != nil {
			delta := observation.ObservedAt.Sub(observation.Metrics.SampledAt)
			if delta > maxGap || delta < -maxGap {
				observation.State, observation.Reason = host.UsageObservationUnknown, host.UsageStaleSample
			}
		}
		attribution := host.UsageAttribution{State: host.UsageAttributionUnknown, OwnerID: ownerID}
		if identityMatches {
			captured, attributionErr := captureHostUsageAttribution(ctx, tx, observation.HostID, ownerID)
			if attributionErr != nil {
				return attributionErr
			}
			attribution = captured
		} else {
			attribution.Reason = host.UsageAttributionGenerationChange
		}
		observation.Attribution = attribution

		var previous storedUsageObservation
		previousErr := tx.QueryRow(ctx, `SELECT id,runtime_generation,sandbox_id,observed_at,state,reason,sampled_at,
       container_id,cpu_usage_total_ns,memory_usage_bytes,owner_id,attribution_state,attribution_reason,
       workspace_id,workspace_binding_id,workspace_binding_version,project_id,project_resource_version,actor_id
FROM host_usage_samples WHERE host_id=$1 ORDER BY id DESC LIMIT 1`, observation.HostID).
			Scan(&previous.ID, &previous.Generation, &previous.SandboxID, &previous.ObservedAt, &previous.State,
				&previous.Reason, &previous.SampledAt, &previous.ContainerID, &previous.CPUUsageTotalNS, &previous.MemoryUsageBytes,
				&previous.OwnerID, &previous.AttributionState, &previous.AttributionReason, &previous.WorkspaceID,
				&previous.WorkspaceBindingID, &previous.WorkspaceBindingVersion, &previous.ProjectID,
				&previous.ProjectResourceVersion, &previous.ActorID)
		if previousErr != nil && !errors.Is(previousErr, pgx.ErrNoRows) {
			return fmt.Errorf("read previous Host usage observation: %w", previousErr)
		}
		var sampledAt, containerID, cpuCounter, memoryUsage, rawPayload any
		if observation.Metrics != nil {
			sampledAt = observation.Metrics.SampledAt.UTC()
			containerID = observation.Metrics.ContainerID
			cpuCounter = observation.Metrics.CPUUsageTotalNS
			memoryUsage = observation.Metrics.MemoryUsageBytes
			rawPayload = observation.Metrics.RawPayload
		}
		attr := observation.Attribution
		var workspaceID, bindingID, bindingVersion, projectID, projectResourceVersion, attributionReason any
		if attr.WorkspaceID != "" {
			workspaceID = attr.WorkspaceID
		}
		if attr.WorkspaceBindingID != "" {
			bindingID = attr.WorkspaceBindingID
			bindingVersion = attr.WorkspaceBindingVersion
		}
		if attr.ProjectID != "" {
			projectID = attr.ProjectID
			projectResourceVersion = attr.ProjectResourceVersion
		}
		if attr.Reason != "" {
			attributionReason = string(attr.Reason)
		}
		var currentID int64
		if err := tx.QueryRow(ctx, `INSERT INTO host_usage_samples
    (host_id,runtime_generation,sandbox_id,owner_id,observed_at,sampled_at,state,reason,source,
     container_id,cpu_usage_total_ns,memory_usage_bytes,raw_payload,
     attribution_state,attribution_reason,workspace_id,workspace_binding_id,workspace_binding_version,
     project_id,project_resource_version,actor_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'docker-engine-cgroup',$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,NULL)
RETURNING id`,
			observation.HostID, observation.RuntimeGeneration, observation.SandboxID, ownerID, observation.ObservedAt.UTC(), sampledAt,
			string(observation.State), nullableUsageReason(observation.Reason), containerID, cpuCounter, memoryUsage, rawPayload,
			string(attr.State), attributionReason, workspaceID, bindingID, bindingVersion, projectID, projectResourceVersion).Scan(&currentID); err != nil {
			return fmt.Errorf("insert Host usage observation: %w", err)
		}

		result = host.UsageInterval{
			HostID: observation.HostID, RuntimeGeneration: observation.RuntimeGeneration, SandboxID: observation.SandboxID,
			EndObservationID: currentID, State: host.UsageIntervalUnknown, CalculationMethod: host.UsageCalculationMethod,
		}
		currentTime := observation.ObservedAt.UTC()
		if observation.Metrics != nil {
			currentTime = observation.Metrics.SampledAt.UTC()
		}
		result.StartedAt, result.EndedAt = currentTime, currentTime
		reason := observation.Reason
		if reason == "" {
			reason = host.UsageNoPreviousSample
		}
		if previousErr == nil {
			result.StartObservationID = &previous.ID
			result.Attribution = reconcileUsageAttribution(storedUsageAttribution(previous), attr)
			startTime := previous.ObservedAt
			if previous.SampledAt.Valid {
				startTime = previous.SampledAt.Time
			}
			result.StartedAt, result.EndedAt = startTime.UTC(), currentTime
			reason = calculateHostUsageInterval(&result, previous, observation, maxGap)
		} else {
			result.Attribution = attr
		}
		result.UnknownReason = reason
		if result.State == host.UsageIntervalMeasured {
			result.UnknownReason = ""
		}
		var startObservationID any
		if result.StartObservationID != nil {
			startObservationID = *result.StartObservationID
		}
		var unknownReason, cpuSeconds, memoryMiBSeconds any
		if result.State == host.UsageIntervalUnknown {
			unknownReason = nullableUsageReason(result.UnknownReason)
		} else {
			cpuSeconds, memoryMiBSeconds = result.CPUSeconds, result.EstimatedMemoryMiBSeconds
		}
		intervalAttribution := result.Attribution
		var intervalWorkspaceID, intervalBindingID, intervalBindingVersion, intervalProjectID, intervalProjectVersion any
		if intervalAttribution.WorkspaceID != "" {
			intervalWorkspaceID = intervalAttribution.WorkspaceID
		}
		if intervalAttribution.WorkspaceBindingID != "" {
			intervalBindingID, intervalBindingVersion = intervalAttribution.WorkspaceBindingID, intervalAttribution.WorkspaceBindingVersion
		}
		if intervalAttribution.ProjectID != "" {
			intervalProjectID, intervalProjectVersion = intervalAttribution.ProjectID, intervalAttribution.ProjectResourceVersion
		}
		if err := tx.QueryRow(ctx, `INSERT INTO host_usage_intervals
    (host_id,runtime_generation,sandbox_id,start_observation_id,end_observation_id,started_at,ended_at,state,
     unknown_reason,calculation_method,cpu_seconds,estimated_memory_mib_seconds,owner_id,attribution_state,
     attribution_reason,workspace_id,workspace_binding_id,workspace_binding_version,project_id,project_resource_version,actor_id)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21) RETURNING id`,
			result.HostID, result.RuntimeGeneration, result.SandboxID, startObservationID, currentID, result.StartedAt,
			result.EndedAt, string(result.State), unknownReason, result.CalculationMethod, cpuSeconds, memoryMiBSeconds,
			intervalAttribution.OwnerID, string(intervalAttribution.State), nullableUsageAttributionReason(intervalAttribution.Reason),
			intervalWorkspaceID, intervalBindingID, intervalBindingVersion, intervalProjectID, intervalProjectVersion, nil).Scan(&result.ID); err != nil {
			return fmt.Errorf("insert Host usage interval: %w", err)
		}
		return nil
	})
	return result, err
}

func captureHostUsageAttribution(ctx context.Context, tx pgx.Tx, hostID, ownerID string) (host.UsageAttribution, error) {
	result := host.UsageAttribution{State: host.UsageAttributionUnknown, OwnerID: ownerID, Reason: host.UsageAttributionNoActiveBinding}
	rows, err := tx.Query(ctx, `SELECT id,workspace_id,binding_version FROM workspace_bindings
WHERE host_id=$1 AND owner_id=$2 AND state='active' ORDER BY id FOR SHARE`, hostID, ownerID)
	if err != nil {
		return result, fmt.Errorf("read active Workspace binding for Host usage: %w", err)
	}
	type binding struct {
		id, workspaceID string
		version         int64
	}
	var bindings []binding
	for rows.Next() {
		var value binding
		if err := rows.Scan(&value.id, &value.workspaceID, &value.version); err != nil {
			rows.Close()
			return result, fmt.Errorf("scan active Workspace binding for Host usage: %w", err)
		}
		bindings = append(bindings, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, fmt.Errorf("iterate active Workspace bindings for Host usage: %w", err)
	}
	rows.Close()
	if len(bindings) == 0 {
		return result, nil
	}
	if len(bindings) != 1 {
		result.Reason = host.UsageAttributionMultipleBindings
		return result, nil
	}
	value := bindings[0]
	result.WorkspaceID, result.WorkspaceBindingID, result.WorkspaceBindingVersion = value.workspaceID, value.id, value.version
	result.Reason = host.UsageAttributionNoProject
	projectRows, err := tx.Query(ctx, `SELECT project_id,version FROM project_resources
WHERE kind='workspace' AND resource_id=$1 AND workspace_id=$1 AND resource_owner_id=$2 AND active
ORDER BY project_id FOR SHARE`, value.workspaceID, ownerID)
	if err != nil {
		return result, fmt.Errorf("read active project resource for Host usage: %w", err)
	}
	type projectResource struct {
		projectID string
		version   int64
	}
	var resources []projectResource
	for projectRows.Next() {
		var resource projectResource
		if err := projectRows.Scan(&resource.projectID, &resource.version); err != nil {
			projectRows.Close()
			return result, fmt.Errorf("scan active project resource for Host usage: %w", err)
		}
		resources = append(resources, resource)
	}
	if err := projectRows.Err(); err != nil {
		projectRows.Close()
		return result, fmt.Errorf("iterate active project resources for Host usage: %w", err)
	}
	projectRows.Close()
	if len(resources) == 0 {
		return result, nil
	}
	if len(resources) != 1 {
		result.Reason = host.UsageAttributionMultipleProjects
		return result, nil
	}
	result.State, result.Reason = host.UsageAttributionProject, ""
	result.ProjectID, result.ProjectResourceVersion = resources[0].projectID, resources[0].version
	// There is no trustworthy actor for a periodic Host-level sample.
	return result, nil
}

func storedUsageAttribution(value storedUsageObservation) host.UsageAttribution {
	result := host.UsageAttribution{OwnerID: value.OwnerID}
	if value.AttributionState.Valid {
		result.State = host.UsageAttributionState(value.AttributionState.String)
	}
	if value.AttributionReason.Valid {
		result.Reason = host.UsageAttributionReason(value.AttributionReason.String)
	}
	if value.WorkspaceID.Valid {
		result.WorkspaceID = value.WorkspaceID.String
	}
	if value.WorkspaceBindingID.Valid {
		result.WorkspaceBindingID = value.WorkspaceBindingID.String
	}
	if value.WorkspaceBindingVersion.Valid {
		result.WorkspaceBindingVersion = value.WorkspaceBindingVersion.Int64
	}
	if value.ProjectID.Valid {
		result.ProjectID = value.ProjectID.String
	}
	if value.ProjectResourceVersion.Valid {
		result.ProjectResourceVersion = value.ProjectResourceVersion.Int64
	}
	if value.ActorID.Valid {
		actorID := value.ActorID.String
		result.ActorID = &actorID
	}
	return result
}

func reconcileUsageAttribution(previous, current host.UsageAttribution) host.UsageAttribution {
	if previous.State == host.UsageAttributionProject && current.State == host.UsageAttributionProject &&
		previous.OwnerID == current.OwnerID && previous.WorkspaceID == current.WorkspaceID &&
		previous.WorkspaceBindingID == current.WorkspaceBindingID && previous.WorkspaceBindingVersion == current.WorkspaceBindingVersion &&
		previous.ProjectID == current.ProjectID && previous.ProjectResourceVersion == current.ProjectResourceVersion {
		return previous
	}
	reason := host.UsageAttributionAssociationChange
	if current.State == host.UsageAttributionUnknown && current.Reason != "" {
		reason = current.Reason
	} else if previous.State == host.UsageAttributionUnknown && previous.Reason != "" {
		reason = previous.Reason
	}
	return host.UsageAttribution{State: host.UsageAttributionUnknown, Reason: reason, OwnerID: current.OwnerID}
}

func nullableUsageAttributionReason(reason host.UsageAttributionReason) any {
	if reason == "" {
		return nil
	}
	return string(reason)
}

func calculateHostUsageInterval(result *host.UsageInterval, previous storedUsageObservation, current host.UsageObservation, maxGap time.Duration) host.UsageUnknownReason {
	switch {
	case previous.Generation != current.RuntimeGeneration || previous.SandboxID != current.SandboxID:
		return host.UsageGenerationChange
	case current.State != host.UsageObservationSampled || current.Metrics == nil:
		if current.Reason != "" {
			return current.Reason
		}
		return host.UsageSamplingFailed
	case previous.State != string(host.UsageObservationSampled) || !previous.SampledAt.Valid ||
		!previous.ContainerID.Valid || !previous.CPUUsageTotalNS.Valid || !previous.MemoryUsageBytes.Valid:
		if previous.Reason.Valid && previous.Reason.String != "" {
			return host.UsageUnknownReason(previous.Reason.String)
		}
		return host.UsageSamplingFailed
	case previous.ContainerID.String != current.Metrics.ContainerID:
		return host.UsageContainerReplaced
	case !current.Metrics.SampledAt.After(previous.SampledAt.Time):
		return host.UsageSamplingGap
	case current.Metrics.SampledAt.Sub(previous.SampledAt.Time) > maxGap:
		return host.UsageSamplingGap
	case current.Metrics.CPUUsageTotalNS < previous.CPUUsageTotalNS.Int64:
		return host.UsageCounterReset
	}

	delta := current.Metrics.SampledAt.Sub(previous.SampledAt.Time)
	deltaSeconds := delta.Seconds()
	result.CPUSeconds = float64(current.Metrics.CPUUsageTotalNS-previous.CPUUsageTotalNS.Int64) / float64(time.Second)
	result.EstimatedMemoryMiBSeconds = ((float64(previous.MemoryUsageBytes.Int64) + float64(current.Metrics.MemoryUsageBytes)) / 2 / bytesPerMiB) * deltaSeconds
	if !host.UsageMeasured(result.CPUSeconds, result.EstimatedMemoryMiBSeconds) {
		result.CPUSeconds, result.EstimatedMemoryMiBSeconds = 0, 0
		return host.UsageSamplingFailed
	}
	result.State = host.UsageIntervalMeasured
	return ""
}

func (s *Store) GetHostUsageHistory(ctx context.Context, hostID string, generation int64) (host.UsageHistory, error) {
	var history host.UsageHistory
	sampleRows, err := s.pool.Query(ctx, `SELECT id,runtime_generation,sandbox_id,owner_id,observed_at,state,reason,sampled_at,
       container_id,cpu_usage_total_ns,memory_usage_bytes,raw_payload,
       attribution_state,attribution_reason,workspace_id,workspace_binding_id,workspace_binding_version,
       project_id,project_resource_version,actor_id
FROM host_usage_samples WHERE host_id=$1 AND runtime_generation=$2 ORDER BY id`, hostID, generation)
	if err != nil {
		return history, fmt.Errorf("list Host usage samples: %w", err)
	}
	defer sampleRows.Close()
	history.Observations = make([]host.UsageObservation, 0)
	for sampleRows.Next() {
		var stored storedUsageObservation
		var raw []byte
		var attribution host.UsageAttribution
		var attributionState, attributionReason pgtype.Text
		var workspaceID, bindingID, projectID, actorID pgtype.Text
		var bindingVersion, projectVersion pgtype.Int8
		if err := sampleRows.Scan(&stored.ID, &stored.Generation, &stored.SandboxID, &stored.OwnerID, &stored.ObservedAt,
			&stored.State, &stored.Reason, &stored.SampledAt, &stored.ContainerID, &stored.CPUUsageTotalNS,
			&stored.MemoryUsageBytes, &raw, &attributionState, &attributionReason, &workspaceID,
			&bindingID, &bindingVersion, &projectID, &projectVersion, &actorID); err != nil {
			return host.UsageHistory{}, fmt.Errorf("scan Host usage sample: %w", err)
		}
		observation := host.UsageObservation{
			ID: stored.ID, HostID: hostID, RuntimeGeneration: stored.Generation, SandboxID: stored.SandboxID,
			ObservedAt: stored.ObservedAt, State: host.UsageObservationState(stored.State), Attribution: attribution,
		}
		if stored.Reason.Valid {
			observation.Reason = host.UsageUnknownReason(stored.Reason.String)
		}
		if attributionState.Valid {
			observation.Attribution.State = host.UsageAttributionState(attributionState.String)
		}
		if attributionReason.Valid {
			observation.Attribution.Reason = host.UsageAttributionReason(attributionReason.String)
		}
		observation.Attribution.OwnerID = stored.OwnerID
		if workspaceID.Valid {
			observation.Attribution.WorkspaceID = workspaceID.String
		}
		if bindingID.Valid {
			observation.Attribution.WorkspaceBindingID = bindingID.String
		}
		if bindingVersion.Valid {
			observation.Attribution.WorkspaceBindingVersion = bindingVersion.Int64
		}
		if projectID.Valid {
			observation.Attribution.ProjectID = projectID.String
		}
		if projectVersion.Valid {
			observation.Attribution.ProjectResourceVersion = projectVersion.Int64
		}
		if actorID.Valid {
			actorValue := actorID.String
			observation.Attribution.ActorID = &actorValue
		}
		if stored.SampledAt.Valid {
			observation.Metrics = &host.ResourceMetrics{
				SampledAt: stored.SampledAt.Time, CPUUsageTotalNS: stored.CPUUsageTotalNS.Int64,
				MemoryUsageBytes: stored.MemoryUsageBytes.Int64, RawPayload: raw,
			}
			if stored.ContainerID.Valid {
				observation.Metrics.ContainerID = stored.ContainerID.String
			}
		}
		history.Observations = append(history.Observations, observation)
		history.Samples++
	}
	if err := sampleRows.Err(); err != nil {
		return host.UsageHistory{}, fmt.Errorf("iterate Host usage samples: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT id,host_id,runtime_generation,sandbox_id,start_observation_id,end_observation_id,
       started_at,ended_at,state,unknown_reason,calculation_method,cpu_seconds,estimated_memory_mib_seconds,
       owner_id,attribution_state,attribution_reason,workspace_id,workspace_binding_id,workspace_binding_version,
       project_id,project_resource_version,actor_id
FROM host_usage_intervals WHERE host_id=$1 AND runtime_generation=$2 ORDER BY ended_at,id`, hostID, generation)
	if err != nil {
		return history, fmt.Errorf("list Host usage intervals: %w", err)
	}
	defer rows.Close()
	history.Intervals = make([]host.UsageInterval, 0)
	for rows.Next() {
		var value host.UsageInterval
		var startID pgtype.Int8
		var reason pgtype.Text
		var cpuSeconds, memoryMiBSeconds pgtype.Float8
		var attributionState, attributionReason, workspaceID, bindingID, projectID, actorID pgtype.Text
		var bindingVersion, projectVersion pgtype.Int8
		if err := rows.Scan(&value.ID, &value.HostID, &value.RuntimeGeneration, &value.SandboxID, &startID, &value.EndObservationID,
			&value.StartedAt, &value.EndedAt, &value.State, &reason, &value.CalculationMethod, &cpuSeconds, &memoryMiBSeconds,
			&value.Attribution.OwnerID, &attributionState, &attributionReason, &workspaceID, &bindingID, &bindingVersion,
			&projectID, &projectVersion, &actorID); err != nil {
			return host.UsageHistory{}, fmt.Errorf("scan Host usage interval: %w", err)
		}
		if startID.Valid {
			value.StartObservationID = &startID.Int64
		}
		if reason.Valid {
			value.UnknownReason = host.UsageUnknownReason(reason.String)
		}
		if cpuSeconds.Valid {
			value.CPUSeconds = cpuSeconds.Float64
		}
		if memoryMiBSeconds.Valid {
			value.EstimatedMemoryMiBSeconds = memoryMiBSeconds.Float64
		}
		if attributionState.Valid {
			value.Attribution.State = host.UsageAttributionState(attributionState.String)
		}
		if attributionReason.Valid {
			value.Attribution.Reason = host.UsageAttributionReason(attributionReason.String)
		}
		if workspaceID.Valid {
			value.Attribution.WorkspaceID = workspaceID.String
		}
		if bindingID.Valid {
			value.Attribution.WorkspaceBindingID = bindingID.String
		}
		if bindingVersion.Valid {
			value.Attribution.WorkspaceBindingVersion = bindingVersion.Int64
		}
		if projectID.Valid {
			value.Attribution.ProjectID = projectID.String
		}
		if projectVersion.Valid {
			value.Attribution.ProjectResourceVersion = projectVersion.Int64
		}
		if actorID.Valid {
			actorValue := actorID.String
			value.Attribution.ActorID = &actorValue
		}
		history.Intervals = append(history.Intervals, value)
	}
	if err := rows.Err(); err != nil {
		return host.UsageHistory{}, fmt.Errorf("iterate Host usage intervals: %w", err)
	}
	return history, nil
}

func (s *Store) GetProjectHostUsageHistory(ctx context.Context, actorID, projectID, hostID string) (host.UsageHistory, error) {
	var history host.UsageHistory
	if !domain.ValidUserID(actorID) || !project.ValidID(projectID) || !domain.ValidHostID(hostID) {
		return history, projectNotFound()
	}
	err := withTx(ctx, s.pool.Begin, func(tx pgx.Tx) error {
		member, err := loadProjectMemberInTx(ctx, tx, projectID, actorID, false)
		if err != nil {
			return err
		}
		if !project.RoleAtLeast(member.Role, project.RoleViewer) {
			return domain.NewError(domain.CodeForbidden, "project role is insufficient")
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM host_usage_samples
WHERE host_id=$1 AND project_id=$2 AND attribution_state='project'`, hostID, projectID).Scan(&history.Samples); err != nil {
			return fmt.Errorf("count project Host usage samples: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT state,unknown_reason,cpu_seconds,estimated_memory_mib_seconds
FROM host_usage_intervals
WHERE host_id=$1 AND project_id=$2 AND attribution_state='project'
ORDER BY ended_at,id`, hostID, projectID)
		if err != nil {
			return fmt.Errorf("list project Host usage intervals: %w", err)
		}
		defer rows.Close()
		history.Intervals = make([]host.UsageInterval, 0)
		for rows.Next() {
			var interval host.UsageInterval
			var reason pgtype.Text
			var cpuSeconds, memoryMiBSeconds pgtype.Float8
			if err := rows.Scan(&interval.State, &reason, &cpuSeconds, &memoryMiBSeconds); err != nil {
				return fmt.Errorf("scan project Host usage interval: %w", err)
			}
			if reason.Valid {
				interval.UnknownReason = host.UsageUnknownReason(reason.String)
			}
			if cpuSeconds.Valid {
				interval.CPUSeconds = cpuSeconds.Float64
			}
			if memoryMiBSeconds.Valid {
				interval.EstimatedMemoryMiBSeconds = memoryMiBSeconds.Float64
			}
			history.Intervals = append(history.Intervals, interval)
		}
		return rows.Err()
	})
	if err != nil {
		return host.UsageHistory{}, err
	}
	return history, nil
}

type storedUsageObservation struct {
	ID                      int64
	Generation              int64
	SandboxID               string
	OwnerID                 string
	ObservedAt              time.Time
	State                   string
	Reason                  pgtype.Text
	SampledAt               pgtype.Timestamptz
	ContainerID             pgtype.Text
	CPUUsageTotalNS         pgtype.Int8
	MemoryUsageBytes        pgtype.Int8
	AttributionState        pgtype.Text
	AttributionReason       pgtype.Text
	WorkspaceID             pgtype.Text
	WorkspaceBindingID      pgtype.Text
	WorkspaceBindingVersion pgtype.Int8
	ProjectID               pgtype.Text
	ProjectResourceVersion  pgtype.Int8
	ActorID                 pgtype.Text
}

func nullableUsageReason(reason host.UsageUnknownReason) any {
	if reason == "" {
		return nil
	}
	return string(reason)
}

var _ app.HostUsageStore = (*Store)(nil)
