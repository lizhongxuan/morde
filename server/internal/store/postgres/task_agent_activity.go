package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func (s *Store) RecordTaskAgentActivity(ctx context.Context, observation app.TaskAgentActivityObservation) (bool, error) {
	fence := observation.Fence
	if ctx == nil || fence.Validate() != nil || observation.ObservationSequence < 1 || observation.ObservationSequence > 1<<53-1 ||
		(observation.Status != "idle" && observation.Status != "running") {
		return false, domain.InvalidArgument("agent_activity", "agent activity observation is invalid")
	}
	accepted := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		hostTarget, err := lockAgentHostTarget(ctx, tx, fence.TaskID, fence.OwnerID)
		if err != nil {
			if domain.CodeOf(err) == domain.CodeUnauthenticated {
				return nil
			}
			return err
		}
		current, err := lockAuthorizedAgentTask(ctx, tx, fence)
		if err != nil {
			if domain.CodeOf(err) == domain.CodeUnauthenticated {
				return nil
			}
			return err
		}
		if current.HostID != hostTarget.TaskHostID || current.InitiatorID != hostTarget.TaskActorID ||
			current.InstanceID != fence.AgentInstanceID || current.Generation != fence.AgentGeneration || current.SessionID != fence.AgentSessionID ||
			current.EngineVersion != fence.EngineVersion || current.ProfileVersion != fence.ProfileVersion {
			return nil
		}
		instance, err := lockAgentInstance(ctx, tx, fence.TaskID, fence.OwnerID, fence.AgentGeneration)
		if err != nil {
			return err
		}
		if instance.Fence != fence || instance.State != "active" {
			return nil
		}
		var leaseActive bool
		if err := tx.QueryRow(ctx, `SELECT lease_expires_at>clock_timestamp() FROM task_agent_instances
WHERE task_id=$1 AND owner_id=$2 AND agent_generation=$3`, fence.TaskID, fence.OwnerID, fence.AgentGeneration).Scan(&leaseActive); err != nil {
			return fmt.Errorf("check agent activity lease: %w", err)
		}
		if !leaseActive {
			return nil
		}
		var isRoot bool
		var priorStatus string
		var priorGeneration, priorSequence int64
		err = tx.QueryRow(ctx, `SELECT parent_session_id IS NULL,COALESCE(activity_status,''),COALESCE(activity_generation,0),COALESCE(activity_sequence,0)
FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 FOR UPDATE`,
			fence.TaskID, fence.OwnerID, fence.AgentSessionID).Scan(&isRoot, &priorStatus, &priorGeneration, &priorSequence)
		if errors.Is(err, pgx.ErrNoRows) || !isRoot {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock root agent activity: %w", err)
		}
		if priorGeneration > fence.AgentGeneration || (priorGeneration == fence.AgentGeneration && observation.ObservationSequence <= priorSequence) {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE task_agent_sessions SET activity_status=$4,activity_generation=$5,
activity_sequence=$6,activity_observed_at=clock_timestamp() WHERE task_id=$1 AND owner_id=$2 AND session_id=$3`,
			fence.TaskID, fence.OwnerID, fence.AgentSessionID, observation.Status, fence.AgentGeneration, observation.ObservationSequence)
		if err != nil {
			return fmt.Errorf("persist root agent activity: %w", err)
		}
		accepted = true
		if priorGeneration == fence.AgentGeneration && priorStatus == observation.Status {
			return nil
		}
		sequence, err := incrementTaskEventSequence(ctx, tx, fence.TaskID)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{"agent_id": fence.AgentSessionID, "agent_status": observation.Status,
			"observation_sequence": observation.ObservationSequence, "phase": "agent_status"})
		if err != nil {
			return fmt.Errorf("encode agent activity event: %w", err)
		}
		return insertTaskEvent(ctx, tx, task.Event{TaskID: fence.TaskID, Sequence: sequence, Type: "task.step", Payload: payload})
	})
	return accepted, err
}
