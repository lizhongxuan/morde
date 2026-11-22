package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func (s *Store) RecordTaskAgentActivity(ctx context.Context, observation app.TaskAgentActivityObservation) (bool, error) {
	if err := validateTaskAgentActivityObservation(observation); err != nil {
		return false, err
	}
	fence := observation.Fence
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
		var taskRevision int64
		if err := tx.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1 AND owner_id=$2`, fence.TaskID, fence.OwnerID).Scan(&taskRevision); err != nil {
			return fmt.Errorf("read current Task revision for agent activity: %w", err)
		}
		if observation.TaskRevision != taskRevision {
			return nil
		}
		var parentSessionID pgtype.Text
		var sessionRevision pgtype.Int8
		var priorStatus, priorPhase, priorState, priorActivityID string
		var priorGeneration, priorSequence int64
		err = tx.QueryRow(ctx, `SELECT parent_session_id,task_revision,COALESCE(activity_status,''),
COALESCE(activity_phase,''),COALESCE(activity_state,''),COALESCE(activity_id,''),
COALESCE(activity_generation,0),COALESCE(activity_sequence,0)
FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 FOR UPDATE`,
			fence.TaskID, fence.OwnerID, observation.AgentSessionID).Scan(&parentSessionID, &sessionRevision,
			&priorStatus, &priorPhase, &priorState, &priorActivityID, &priorGeneration, &priorSequence)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock Task agent activity session: %w", err)
		}
		if parentSessionID.Valid {
			if !sessionRevision.Valid || sessionRevision.Int64 != observation.TaskRevision {
				return nil
			}
		} else if observation.AgentSessionID != current.SessionID {
			return nil
		}
		if priorGeneration > fence.AgentGeneration || (priorGeneration == fence.AgentGeneration && observation.ObservationSequence <= priorSequence) {
			return nil
		}

		status, phase, state, activityID := priorStatus, priorPhase, priorState, priorActivityID
		progressed := false
		switch observation.Kind {
		case app.TaskAgentActivityKindStatus:
			status = observation.Status
			if status == "idle" {
				phase, state, activityID = "", "", ""
			}
			progressed = priorGeneration != fence.AgentGeneration || priorStatus != status ||
				(observation.Status == "idle" && (priorPhase != "" || priorState != "" || priorActivityID != ""))
		case app.TaskAgentActivityKindActivity:
			status = "running"
			phase, state, activityID = observation.Phase, observation.State, observation.ActivityID
			progressed = priorGeneration != fence.AgentGeneration || priorPhase != phase || priorState != state || priorActivityID != activityID
		case app.TaskAgentActivityKindHeartbeat:
			if observation.Phase != priorPhase || observation.ActivityID != priorActivityID {
				return domain.InvalidArgument("agent_activity.heartbeat", "agent heartbeat does not match the current activity")
			}
			if status == "" {
				status = "idle"
				if observation.Phase != "" {
					status = "running"
				}
			}
		}
		_, err = tx.Exec(ctx, `UPDATE task_agent_sessions SET activity_status=$4,activity_generation=$5,
activity_sequence=$6,activity_observed_at=clock_timestamp(),activity_revision=$7,
activity_phase=NULLIF($8,''),activity_state=NULLIF($9,''),activity_id=NULLIF($10,''),
activity_heartbeat_at=CASE WHEN $12::text='heartbeat' THEN clock_timestamp()
  WHEN activity_generation IS DISTINCT FROM $5 THEN NULL ELSE activity_heartbeat_at END,
activity_progressed_at=CASE WHEN $11::boolean OR activity_revision IS DISTINCT FROM $7 THEN clock_timestamp() ELSE activity_progressed_at END
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3`, fence.TaskID, fence.OwnerID,
			observation.AgentSessionID, status, fence.AgentGeneration, observation.ObservationSequence,
			observation.TaskRevision, phase, state, activityID, progressed, observation.Kind)
		if err != nil {
			return fmt.Errorf("persist Task agent activity: %w", err)
		}
		accepted = true
		if observation.Kind == app.TaskAgentActivityKindHeartbeat || !progressed {
			return nil
		}
		if err := clearTaskAgentStallNoticesForProgress(ctx, tx, fence.TaskID, fence.OwnerID); err != nil {
			return err
		}
		sequence, err := incrementTaskEventSequence(ctx, tx, fence.TaskID)
		if err != nil {
			return err
		}
		payload := map[string]any{"agent_id": observation.AgentSessionID,
			"observation_sequence": observation.ObservationSequence, "task_revision": observation.TaskRevision}
		if observation.Kind == app.TaskAgentActivityKindStatus {
			payload["agent_status"], payload["phase"] = status, "agent_status"
		} else {
			payload["kind"], payload["phase"], payload["status"] = "agent_activity", phase, state
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode Task agent activity event: %w", err)
		}
		return insertTaskEvent(ctx, tx, task.Event{TaskID: fence.TaskID, Sequence: sequence, Type: "task.step", Payload: encoded})
	})
	return accepted, err
}

func validateTaskAgentActivityObservation(observation app.TaskAgentActivityObservation) error {
	if observation.Fence.Validate() != nil || observation.AgentSessionID == "" || len(observation.AgentSessionID) > 256 ||
		!utf8.ValidString(observation.AgentSessionID) || strings.ContainsRune(observation.AgentSessionID, '\x00') ||
		observation.TaskRevision < 0 || observation.ObservationSequence < 1 || observation.ObservationSequence > 1<<53-1 {
		return domain.InvalidArgument("agent_activity", "agent activity observation is invalid")
	}
	switch observation.Kind {
	case app.TaskAgentActivityKindStatus:
		if (observation.Status != "idle" && observation.Status != "running") || observation.Phase != "" || observation.State != "" || observation.ActivityID != "" {
			return domain.InvalidArgument("agent_activity", "agent status observation is invalid")
		}
	case app.TaskAgentActivityKindActivity:
		if observation.Status != "" || !validTaskAgentActivityPhase(observation.Phase) || !validTaskAgentActivityState(observation.State) ||
			!validTaskAgentActivityID(observation.ActivityID) {
			return domain.InvalidArgument("agent_activity", "agent phase observation is invalid")
		}
	case app.TaskAgentActivityKindHeartbeat:
		if observation.Status != "" || observation.State != "" ||
			(observation.Phase != "" && !validTaskAgentActivityPhase(observation.Phase)) ||
			(observation.Phase == "") != (observation.ActivityID == "") ||
			(observation.ActivityID != "" && !validTaskAgentActivityID(observation.ActivityID)) {
			return domain.InvalidArgument("agent_activity", "agent heartbeat is invalid")
		}
	default:
		return domain.InvalidArgument("agent_activity.kind", "agent activity kind is invalid")
	}
	return nil
}

func validTaskAgentActivityPhase(value string) bool {
	switch value {
	case "model", "tool", "subagent", "checkpoint":
		return true
	default:
		return false
	}
}

func validTaskAgentActivityState(value string) bool {
	switch value {
	case "started", "waiting", "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func validTaskAgentActivityID(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}
