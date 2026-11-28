package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

var taskAgentRequestDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *Store) RecordTaskAgentRequestReconciliation(ctx context.Context, value app.TaskAgentRequestReconciliation) (bool, error) {
	if err := validateTaskAgentRequestReconciliation(value, true); err != nil {
		return false, err
	}
	accepted := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, ok, err := lockTaskAgentRequestReconciliationFence(ctx, tx, value.Fence)
		if err != nil || !ok {
			return err
		}
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1 AND owner_id=$2`,
			value.Fence.TaskID, value.Fence.OwnerID).Scan(&revision); err != nil {
			return fmt.Errorf("read current Task revision for request reconciliation: %w", err)
		}
		if revision != value.TaskRevision {
			return nil
		}
		belongs, err := lockTaskAgentRequestSession(ctx, tx, current, value)
		if err != nil || !belongs {
			return err
		}
		var existingState string
		err = tx.QueryRow(ctx, `SELECT state FROM task_agent_request_reconciliations
WHERE task_id=$1 AND session_id=$2 AND task_revision=$3 AND request_kind=$4 AND activity_id=$5 AND request_digest=$6
FOR UPDATE`, value.Fence.TaskID, value.AgentSessionID, value.TaskRevision, value.RequestKind,
			value.ActivityID, value.RequestDigest).Scan(&existingState)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read prior Task agent request reconciliation: %w", err)
		}
		if err == nil && existingState == "resolved" {
			accepted = true
			return nil
		}
		if err := advanceTaskAgentRequestObservation(ctx, tx, value); err != nil {
			if errors.Is(err, errTaskAgentRequestObservationOutOfOrder) {
				if existingState == "pending" {
					accepted = true
					messages, err := taskAgentPendingMessages(ctx, tx, value.Fence.TaskID, value.Fence.OwnerID, current.ConversationID)
					if err != nil {
						return err
					}
					for _, messageID := range messages {
						if err := createOrPromoteTaskAgentRequestReconciliationNotice(ctx, tx, value.Fence.TaskID,
							value.Fence.OwnerID, messageID, value.TaskRevision, value.Fence.AgentGeneration); err != nil {
							return err
						}
					}
					return nil
				}
				return nil
			}
			return err
		}
		if errors.Is(err, pgx.ErrNoRows) {
			_, err = tx.Exec(ctx, `INSERT INTO task_agent_request_reconciliations
(task_id,owner_id,session_id,task_revision,agent_instance_id,agent_generation,authorization_version,
 phase,request_kind,activity_id,request_digest,observation_sequence,state)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'pending')
ON CONFLICT (task_id,session_id,task_revision,request_kind,activity_id,request_digest) DO NOTHING`,
				value.Fence.TaskID, value.Fence.OwnerID, value.AgentSessionID, value.TaskRevision,
				value.Fence.AgentInstanceID, value.Fence.AgentGeneration, value.Fence.AuthorizationVersion,
				value.Phase, value.RequestKind, value.ActivityID, value.RequestDigest, value.ObservationSequence)
			if err != nil {
				return fmt.Errorf("persist Task agent pending request reconciliation: %w", err)
			}
		}
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM task_agent_request_reconciliations
WHERE task_id=$1 AND session_id=$2 AND task_revision=$3 AND request_kind=$4 AND activity_id=$5 AND request_digest=$6`,
			value.Fence.TaskID, value.AgentSessionID, value.TaskRevision, value.RequestKind,
			value.ActivityID, value.RequestDigest).Scan(&state); err != nil {
			return fmt.Errorf("verify Task agent request reconciliation identity: %w", err)
		}
		accepted = true
		if state != "pending" {
			return nil
		}
		messages, err := taskAgentPendingMessages(ctx, tx, value.Fence.TaskID, value.Fence.OwnerID, current.ConversationID)
		if err != nil {
			return err
		}
		for _, messageID := range messages {
			if err := createOrPromoteTaskAgentRequestReconciliationNotice(ctx, tx, value.Fence.TaskID,
				value.Fence.OwnerID, messageID, value.TaskRevision, value.Fence.AgentGeneration); err != nil {
				return err
			}
		}
		return nil
	})
	return accepted, err
}

func (s *Store) ResolveTaskAgentRequestReconciliation(ctx context.Context, value app.TaskAgentRequestReconciliation) (bool, error) {
	if err := validateTaskAgentRequestReconciliation(value, false); err != nil {
		return false, err
	}
	accepted := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, ok, err := lockTaskAgentRequestReconciliationFence(ctx, tx, value.Fence)
		if err != nil || !ok {
			return err
		}
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1 AND owner_id=$2`,
			value.Fence.TaskID, value.Fence.OwnerID).Scan(&revision); err != nil {
			return fmt.Errorf("read current Task revision for request reconciliation resolution: %w", err)
		}
		if revision != value.TaskRevision {
			return nil
		}
		belongs, err := lockTaskAgentRequestSession(ctx, tx, current, value)
		if err != nil || !belongs {
			return err
		}
		var state string
		err = tx.QueryRow(ctx, `SELECT state FROM task_agent_request_reconciliations
WHERE task_id=$1 AND session_id=$2 AND task_revision=$3 AND request_kind=$4 AND activity_id=$5 AND request_digest=$6
FOR UPDATE`, value.Fence.TaskID, value.AgentSessionID, value.TaskRevision, value.RequestKind,
			value.ActivityID, value.RequestDigest).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock Task agent request reconciliation for resolution: %w", err)
		}
		accepted = true
		if state == "pending" {
			if _, err := tx.Exec(ctx, `UPDATE task_agent_request_reconciliations SET state='resolved',resolved_at=clock_timestamp()
WHERE task_id=$1 AND session_id=$2 AND task_revision=$3 AND request_kind=$4 AND activity_id=$5 AND request_digest=$6 AND state='pending'`,
				value.Fence.TaskID, value.AgentSessionID, value.TaskRevision, value.RequestKind,
				value.ActivityID, value.RequestDigest); err != nil {
				return fmt.Errorf("resolve Task agent pending request: %w", err)
			}
		}
		return clearTaskAgentRequestReconciliationNotices(ctx, tx, value.Fence.TaskID, value.Fence.OwnerID,
			current.ConversationID)
	})
	return accepted, err
}

func validateTaskAgentRequestReconciliation(value app.TaskAgentRequestReconciliation, requireSequence bool) error {
	if value.Fence.Validate() != nil || value.AgentSessionID == "" || len(value.AgentSessionID) > 256 ||
		!utf8.ValidString(value.AgentSessionID) || strings.ContainsRune(value.AgentSessionID, '\x00') ||
		value.TaskRevision < 0 || !taskAgentRequestDigestPattern.MatchString(value.RequestDigest) ||
		value.ActivityID == "" || len(value.ActivityID) > 256 || !utf8.ValidString(value.ActivityID) ||
		strings.ContainsRune(value.ActivityID, '\x00') || !validTaskAgentRequestKind(value.Phase, value.RequestKind) ||
		requireSequence && (value.ObservationSequence < 1 || value.ObservationSequence > 1<<53-1) {
		return domain.InvalidArgument("task_agent_request_reconciliation", "Task agent request reconciliation identity is invalid")
	}
	return nil
}

func validTaskAgentRequestKind(phase, requestKind string) bool {
	switch phase {
	case app.TaskAgentRequestPhaseTool:
		return requestKind == app.TaskAgentRequestKindToolCall
	case app.TaskAgentRequestPhaseSubagent:
		return requestKind == app.TaskAgentRequestKindChildCreate || requestKind == app.TaskAgentRequestKindChildSettle
	case app.TaskAgentRequestPhaseCheckpoint:
		return requestKind == app.TaskAgentRequestKindCheckpointWrite || requestKind == app.TaskAgentRequestKindCheckpointRead
	default:
		return false
	}
}

func lockTaskAgentRequestReconciliationFence(ctx context.Context, tx pgx.Tx, fence app.AgentInstanceFence) (agentTaskLock, bool, error) {
	hostTarget, err := lockAgentHostTarget(ctx, tx, fence.TaskID, fence.OwnerID)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeUnauthenticated {
			return agentTaskLock{}, false, nil
		}
		return agentTaskLock{}, false, err
	}
	current, err := lockAuthorizedAgentTask(ctx, tx, fence)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeUnauthenticated {
			return agentTaskLock{}, false, nil
		}
		return agentTaskLock{}, false, err
	}
	if current.HostID != hostTarget.TaskHostID || current.InitiatorID != hostTarget.TaskActorID ||
		current.InstanceID != fence.AgentInstanceID || current.Generation != fence.AgentGeneration || current.SessionID != fence.AgentSessionID ||
		current.EngineVersion != fence.EngineVersion || current.ProfileVersion != fence.ProfileVersion {
		return agentTaskLock{}, false, nil
	}
	instance, err := lockAgentInstance(ctx, tx, fence.TaskID, fence.OwnerID, fence.AgentGeneration)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeUnauthenticated {
			return agentTaskLock{}, false, nil
		}
		return agentTaskLock{}, false, err
	}
	if instance.Fence != fence || instance.State != "active" {
		return agentTaskLock{}, false, nil
	}
	var leaseActive bool
	if err := tx.QueryRow(ctx, `SELECT lease_expires_at>clock_timestamp() FROM task_agent_instances
WHERE task_id=$1 AND owner_id=$2 AND agent_generation=$3`, fence.TaskID, fence.OwnerID, fence.AgentGeneration).Scan(&leaseActive); err != nil {
		return agentTaskLock{}, false, fmt.Errorf("check agent request reconciliation lease: %w", err)
	}
	if !leaseActive {
		return agentTaskLock{}, false, nil
	}
	return current, true, nil
}

func lockTaskAgentRequestSession(ctx context.Context, tx pgx.Tx, current agentTaskLock,
	value app.TaskAgentRequestReconciliation) (bool, error) {
	var parentSessionID pgtype.Text
	var sessionRevision pgtype.Int8
	if err := tx.QueryRow(ctx, `SELECT parent_session_id,task_revision FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 FOR UPDATE`, value.Fence.TaskID, value.Fence.OwnerID,
		value.AgentSessionID).Scan(&parentSessionID, &sessionRevision); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("lock Task agent request session: %w", err)
	}
	if !parentSessionID.Valid {
		return value.AgentSessionID == current.SessionID, nil
	}
	if !sessionRevision.Valid || sessionRevision.Int64 != value.TaskRevision {
		return false, nil
	}
	belongs, err := agentSessionDescendsFrom(ctx, tx, value.Fence.TaskID, value.Fence.OwnerID,
		value.AgentSessionID, current.SessionID)
	if err != nil {
		return false, fmt.Errorf("verify Task agent request session ancestry: %w", err)
	}
	return belongs, nil
}

var errTaskAgentRequestObservationOutOfOrder = errors.New("task agent request reconciliation observation is out of order")

func advanceTaskAgentRequestObservation(ctx context.Context, tx pgx.Tx, value app.TaskAgentRequestReconciliation) error {
	var priorGeneration, priorSequence int64
	err := tx.QueryRow(ctx, `SELECT COALESCE(activity_generation,0),COALESCE(activity_sequence,0)
FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 FOR UPDATE`,
		value.Fence.TaskID, value.Fence.OwnerID, value.AgentSessionID).Scan(&priorGeneration, &priorSequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Task agent request observation sequence: %w", err)
	}
	if priorGeneration > value.Fence.AgentGeneration ||
		priorGeneration == value.Fence.AgentGeneration && value.ObservationSequence <= priorSequence {
		return errTaskAgentRequestObservationOutOfOrder
	}
	if _, err := tx.Exec(ctx, `UPDATE task_agent_sessions SET activity_status='running',activity_generation=$4,
activity_sequence=$5,activity_observed_at=clock_timestamp(),activity_revision=$6,
activity_phase=$7,activity_state='waiting',activity_id=$8,
activity_heartbeat_at=CASE WHEN activity_generation IS DISTINCT FROM $4 OR activity_revision IS DISTINCT FROM $6 THEN NULL ELSE activity_heartbeat_at END,
activity_progressed_at=CASE WHEN activity_generation IS DISTINCT FROM $4 OR activity_revision IS DISTINCT FROM $6 OR
  activity_phase IS DISTINCT FROM $7 OR activity_state IS DISTINCT FROM 'waiting' OR activity_id IS DISTINCT FROM $8
  THEN clock_timestamp() ELSE activity_progressed_at END
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3`, value.Fence.TaskID, value.Fence.OwnerID,
		value.AgentSessionID, value.Fence.AgentGeneration, value.ObservationSequence, value.TaskRevision,
		value.Phase, value.ActivityID); err != nil {
		return fmt.Errorf("advance Task agent request observation sequence: %w", err)
	}
	return nil
}
