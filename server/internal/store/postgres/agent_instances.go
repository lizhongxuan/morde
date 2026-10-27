package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type agentTaskLock struct {
	HostID, InstanceID, SessionID, EngineVersion, ProfileVersion string
	Generation                                                   int64
	CheckpointThrough                                            int64
	CheckpointDigest                                             []byte
}

type agentInstanceRow struct {
	Fence              app.AgentInstanceFence
	State              string
	ExpiresAt          time.Time
	NonceHash          []byte
	Receipt            []byte
	ReceiptHash        []byte
	InitialLeaseMillis int64
}

func (s *Store) StartAgentInstance(ctx context.Context, request app.AgentInstanceStart) (app.AgentInstanceLease, bool, error) {
	if err := validateAgentStart(request); err != nil {
		return app.AgentInstanceLease{}, false, err
	}
	var result app.AgentInstanceLease
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, err := lockAuthorizedAgentTask(ctx, tx, request.Fence)
		if err != nil {
			return err
		}
		if request.Fence.AgentInstanceID == current.HostID {
			return domain.InvalidArgument("agent_instance_id", "agent instance cannot be the Host")
		}
		nonceHash := sha256.Sum256(request.SupervisorNonce)
		generation := int64(1)
		if current.InstanceID == "" && current.Generation == 0 {
			if current.SessionID != "" || current.EngineVersion != "" || current.ProfileVersion != "" ||
				current.CheckpointThrough != -1 || len(current.CheckpointDigest) != 0 {
				return agentStopUnconfirmed()
			}
			var hasRows bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_agent_instances WHERE task_id=$1)`, request.Fence.TaskID).Scan(&hasRows); err != nil {
				return fmt.Errorf("check prior agent instances: %w", err)
			}
			if hasRows {
				return agentStopUnconfirmed()
			}
		} else {
			previous, err := lockAgentInstance(ctx, tx, request.Fence.TaskID, request.Fence.OwnerID, current.Generation)
			if err != nil {
				return err
			}
			if previous.Fence.AgentInstanceID != current.InstanceID || previous.Fence.AgentSessionID != current.SessionID ||
				previous.Fence.EngineVersion != current.EngineVersion || previous.Fence.ProfileVersion != current.ProfileVersion {
				return agentStopUnconfirmed()
			}
			if previous.State == "active" {
				candidate := request.Fence
				candidate.AgentGeneration = current.Generation
				if previous.Fence == candidate && bytes.Equal(previous.NonceHash, nonceHash[:]) {
					if previous.InitialLeaseMillis != request.LeaseDuration.Milliseconds() {
						return domain.NewError(domain.CodeIdempotencyConflict, "agent start was reused with a different lease duration")
					}
					var deadline time.Time
					err := tx.QueryRow(ctx, `UPDATE task_agent_instances i
SET lease_expires_at=LEAST(i.lease_expires_at,t.lease_expires_at,t.auth_expires_at),updated_at=clock_timestamp()
FROM tasks t WHERE i.task_id=t.id AND i.task_id=$1 AND i.agent_generation=$2 AND i.lease_expires_at>clock_timestamp()
 AND t.lease_expires_at>clock_timestamp() AND t.auth_expires_at>clock_timestamp() RETURNING i.lease_expires_at`,
						request.Fence.TaskID, current.Generation).Scan(&deadline)
					if err == nil {
						result = app.AgentInstanceLease{Fence: previous.Fence, ExpiresAt: deadline}
						replayed = true
						return nil
					}
					if !errors.Is(err, pgx.ErrNoRows) {
						return fmt.Errorf("check existing agent lease: %w", err)
					}
				}
				return agentStopUnconfirmed()
			}
			if previous.State != "stopped" || len(previous.Receipt) == 0 || len(previous.ReceiptHash) != sha256.Size {
				return agentStopUnconfirmed()
			}
			receiptHash := sha256.Sum256(previous.Receipt)
			if !bytes.Equal(previous.ReceiptHash, receiptHash[:]) {
				return agentStopUnconfirmed()
			}
			if request.Fence.AgentInstanceID == previous.Fence.AgentInstanceID || request.Fence.AgentSessionID != previous.Fence.AgentSessionID ||
				request.Fence.EngineVersion != previous.Fence.EngineVersion || request.Fence.ProfileVersion != previous.Fence.ProfileVersion {
				return domain.NewError(domain.CodeConflict, "agent recovery must retain the existing session and runtime versions")
			}
			if current.CheckpointThrough >= 0 {
				var exists bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_agent_checkpoints
WHERE task_id=$1 AND owner_id=$2 AND agent_session_id=$3 AND through_seq=$4 AND digest=$5)`,
					request.Fence.TaskID, request.Fence.OwnerID, current.SessionID, current.CheckpointThrough, current.CheckpointDigest).Scan(&exists); err != nil {
					return fmt.Errorf("check agent recovery checkpoint: %w", err)
				}
				if !exists {
					return domain.NewError(domain.CodeInvalidState, "agent recovery checkpoint is unavailable")
				}
			} else if current.CheckpointThrough != -1 || len(current.CheckpointDigest) != 0 {
				return domain.NewError(domain.CodeInvalidState, "agent recovery checkpoint is invalid")
			}
			if current.Generation == math.MaxInt64 {
				return domain.NewError(domain.CodeInvalidState, "agent generation is exhausted")
			}
			generation = current.Generation + 1
		}
		expiresAt, err := agentLeaseDeadline(ctx, tx, request.Fence.TaskID, request.LeaseDuration)
		if err != nil {
			return err
		}
		fence := request.Fence
		fence.AgentGeneration = generation
		inserted, err := tx.Exec(ctx, `INSERT INTO task_agent_instances
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,engine_version,profile_version,supervisor_id,
 stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state)
SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'active' WHERE $14::timestamptz>clock_timestamp()`,
			fence.TaskID, fence.OwnerID, fence.AgentInstanceID, fence.AgentGeneration, fence.AgentSessionID,
			fence.EngineVersion, fence.ProfileVersion, fence.SupervisorID, nonceHash[:], fence.LeaseOwner,
			fence.LeaseVersion, fence.AuthorizationVersion, request.LeaseDuration.Milliseconds(), expiresAt)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return domain.NewError(domain.CodeConflict, "agent instance or supervisor nonce was already used")
			}
			return fmt.Errorf("create agent instance: %w", err)
		}
		if inserted.RowsAffected() != 1 {
			return domain.NewError(domain.CodeUnauthenticated, "agent task lease or authorization has expired")
		}
		_, err = tx.Exec(ctx, `UPDATE tasks SET agent_instance_id=$2,agent_generation=$3,agent_session_id=$4,
agent_engine_version=$5,agent_profile_version=$6,updated_at=clock_timestamp() WHERE id=$1`, fence.TaskID,
			fence.AgentInstanceID, fence.AgentGeneration, fence.AgentSessionID, fence.EngineVersion, fence.ProfileVersion)
		if err != nil {
			return fmt.Errorf("bind agent instance to task: %w", err)
		}
		result = app.AgentInstanceLease{Fence: fence, ExpiresAt: expiresAt}
		return nil
	})
	return result, replayed, err
}

func (s *Store) RenewAgentInstance(ctx context.Context, fence app.AgentInstanceFence, duration time.Duration) (app.AgentInstanceLease, error) {
	if err := validateExistingAgentFence(fence); err != nil || duration < time.Millisecond || duration > app.MaxAgentLease {
		return app.AgentInstanceLease{}, domain.InvalidArgument("agent_instance", "agent fence or lease duration is invalid")
	}
	var result app.AgentInstanceLease
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, err := lockAuthorizedAgentTask(ctx, tx, fence)
		if err != nil {
			return err
		}
		if current.InstanceID != fence.AgentInstanceID || current.Generation != fence.AgentGeneration || current.SessionID != fence.AgentSessionID ||
			current.EngineVersion != fence.EngineVersion || current.ProfileVersion != fence.ProfileVersion {
			return agentStopUnconfirmed()
		}
		instance, err := lockAgentInstance(ctx, tx, fence.TaskID, fence.OwnerID, fence.AgentGeneration)
		if err != nil {
			return err
		}
		if instance.Fence != fence || instance.State != "active" {
			return agentStopUnconfirmed()
		}
		var stillValid bool
		if err := tx.QueryRow(ctx, `SELECT lease_expires_at>clock_timestamp() FROM task_agent_instances WHERE task_id=$1 AND agent_generation=$2`,
			fence.TaskID, fence.AgentGeneration).Scan(&stillValid); err != nil {
			return fmt.Errorf("check agent lease before renewal: %w", err)
		}
		if !stillValid {
			return agentStopUnconfirmed()
		}
		expiresAt, err := agentLeaseDeadline(ctx, tx, fence.TaskID, duration)
		if err != nil {
			return err
		}
		resultRow, err := tx.Exec(ctx, `UPDATE task_agent_instances i SET lease_expires_at=$3,updated_at=clock_timestamp()
FROM tasks t WHERE i.task_id=t.id AND i.task_id=$1 AND i.agent_generation=$2
  AND i.lease_expires_at>clock_timestamp() AND t.lease_expires_at>clock_timestamp()
  AND t.auth_expires_at>clock_timestamp() AND $3::timestamptz>clock_timestamp()`, fence.TaskID, fence.AgentGeneration, expiresAt)
		if err != nil {
			return fmt.Errorf("renew agent lease: %w", err)
		}
		if resultRow.RowsAffected() != 1 {
			return agentStopUnconfirmed()
		}
		result = app.AgentInstanceLease{Fence: fence, ExpiresAt: expiresAt}
		return nil
	})
	return result, err
}

func (s *Store) ConfirmAgentStopped(ctx context.Context, proof app.VerifiedAgentStop) (bool, error) {
	receipt, encoded := proof.Value()
	if err := validateExistingAgentFence(receipt.Fence); err != nil || len(receipt.SupervisorNonce) != 32 ||
		len(receipt.Signature) != sha256.Size || !receipt.TreeStopped || len(encoded) == 0 || len(encoded) > 4096 {
		return false, domain.InvalidArgument("receipt", "verified supervisor stop receipt is required")
	}
	nonceHash := sha256.Sum256(receipt.SupervisorNonce)
	receiptHash := sha256.Sum256(encoded)
	var replayed bool
	// Stop evidence remains recordable after authorization is revoked.
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockOwnerExecutionControl(ctx, tx, receipt.Fence.OwnerID, false); err != nil {
			return err
		}
		var ownerID string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR SHARE`, receipt.Fence.OwnerID).Scan(&ownerID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "agent stop owner is unavailable")
		} else if err != nil {
			return fmt.Errorf("lock agent stop owner: %w", err)
		}
		var currentID, currentSession string
		var currentGeneration int64
		err := tx.QueryRow(ctx, `SELECT agent_instance_id,agent_generation,agent_session_id FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`,
			receipt.Fence.TaskID, receipt.Fence.OwnerID).Scan(&currentID, &currentGeneration, &currentSession)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "agent stop task is unavailable")
		}
		if err != nil {
			return fmt.Errorf("lock agent stop task: %w", err)
		}
		instance, err := lockAgentInstance(ctx, tx, receipt.Fence.TaskID, receipt.Fence.OwnerID, receipt.Fence.AgentGeneration)
		if err != nil {
			return err
		}
		if instance.Fence != receipt.Fence || !bytes.Equal(instance.NonceHash, nonceHash[:]) {
			return domain.NewError(domain.CodeUnauthenticated, "supervisor stop receipt does not match the agent instance")
		}
		if instance.State == "stopped" {
			if !bytes.Equal(instance.ReceiptHash, receiptHash[:]) {
				return domain.NewError(domain.CodeIdempotencyConflict, "agent stop receipt was already recorded differently")
			}
			replayed = true
			return nil
		}
		if instance.State != "active" || currentID != receipt.Fence.AgentInstanceID || currentGeneration != receipt.Fence.AgentGeneration ||
			currentSession != receipt.Fence.AgentSessionID {
			return agentStopUnconfirmed()
		}
		_, err = tx.Exec(ctx, `UPDATE task_agent_instances SET state='stopped', stop_receipt=$3, stop_receipt_digest=$4,
stopped_at=clock_timestamp(),updated_at=clock_timestamp() WHERE task_id=$1 AND agent_generation=$2`,
			receipt.Fence.TaskID, receipt.Fence.AgentGeneration, encoded, receiptHash[:])
		if err != nil {
			return fmt.Errorf("persist verified agent stop receipt: %w", err)
		}
		return nil
	})
	return replayed, err
}

func lockAuthorizedAgentTask(ctx context.Context, tx pgx.Tx, fence app.AgentInstanceFence) (agentTaskLock, error) {
	gate, err := requireOwnerExecutionEnabled(ctx, tx, fence.OwnerID, nil)
	if err != nil {
		return agentTaskLock{}, err
	}
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR SHARE`, fence.OwnerID).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		return agentTaskLock{}, domain.NewError(domain.CodeUnauthenticated, "agent owner is unavailable")
	} else if err != nil {
		return agentTaskLock{}, fmt.Errorf("lock agent owner: %w", err)
	}
	if status != string(identity.StatusApproved) {
		return agentTaskLock{}, domain.NewError(domain.CodeUnauthenticated, "agent owner is unavailable")
	}
	var current agentTaskLock
	var authKind, tokenID, installationID string
	err = tx.QueryRow(ctx, `SELECT COALESCE(host_id,''),agent_instance_id,agent_generation,agent_session_id,agent_engine_version,
agent_profile_version,checkpoint_through_seq,checkpoint_digest,auth_kind,auth_token_id,COALESCE(auth_client_installation_id,'') FROM tasks
WHERE id=$1 AND owner_id=$2 AND status='running' AND execution_location='server' AND execution_epoch=$3
 AND lease_owner=$4 AND lease_version=$5 AND lease_expires_at>clock_timestamp()
 AND auth_version=$6 AND auth_expires_at>clock_timestamp() FOR UPDATE`, fence.TaskID, fence.OwnerID, gate.epoch,
		fence.LeaseOwner, fence.LeaseVersion, fence.AuthorizationVersion).Scan(&current.HostID, &current.InstanceID, &current.Generation,
		&current.SessionID, &current.EngineVersion, &current.ProfileVersion, &current.CheckpointThrough, &current.CheckpointDigest,
		&authKind, &tokenID, &installationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentTaskLock{}, domain.NewError(domain.CodeUnauthenticated, "agent task authorization is unavailable")
	}
	if err != nil {
		return agentTaskLock{}, fmt.Errorf("lock agent task: %w", err)
	}
	if err := checkAgentCheckpointAuthorization(ctx, tx, fence.OwnerID, authKind, tokenID, installationID); err != nil {
		return agentTaskLock{}, err
	}
	return current, nil
}

func agentLeaseDeadline(ctx context.Context, tx pgx.Tx, taskID string, duration time.Duration) (time.Time, error) {
	var deadline time.Time
	err := tx.QueryRow(ctx, `SELECT LEAST(clock_timestamp()+($2::bigint * interval '1 millisecond'),lease_expires_at,auth_expires_at)
FROM tasks WHERE id=$1 AND lease_expires_at>clock_timestamp() AND auth_expires_at>clock_timestamp()`, taskID, duration.Milliseconds()).Scan(&deadline)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, domain.NewError(domain.CodeUnauthenticated, "agent task lease or authorization has expired")
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("calculate agent lease deadline: %w", err)
	}
	return deadline, nil
}

func lockAgentInstance(ctx context.Context, tx pgx.Tx, taskID, ownerID string, generation int64) (agentInstanceRow, error) {
	var row agentInstanceRow
	row.Fence.TaskID, row.Fence.OwnerID, row.Fence.AgentGeneration = taskID, ownerID, generation
	err := tx.QueryRow(ctx, `SELECT agent_instance_id,agent_session_id,engine_version,profile_version,supervisor_id,
stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state,stop_receipt,stop_receipt_digest
FROM task_agent_instances WHERE task_id=$1 AND owner_id=$2 AND agent_generation=$3 FOR UPDATE`, taskID, ownerID, generation).Scan(
		&row.Fence.AgentInstanceID, &row.Fence.AgentSessionID, &row.Fence.EngineVersion, &row.Fence.ProfileVersion,
		&row.Fence.SupervisorID, &row.NonceHash, &row.Fence.LeaseOwner, &row.Fence.LeaseVersion,
		&row.Fence.AuthorizationVersion, &row.InitialLeaseMillis, &row.ExpiresAt, &row.State, &row.Receipt, &row.ReceiptHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentInstanceRow{}, agentStopUnconfirmed()
	}
	if err != nil {
		return agentInstanceRow{}, fmt.Errorf("lock agent instance: %w", err)
	}
	return row, nil
}

func lockActiveAgentLease(ctx context.Context, tx pgx.Tx, fence app.AgentInstanceFence) error {
	var instanceID string
	err := tx.QueryRow(ctx, `SELECT agent_instance_id FROM task_agent_instances
WHERE task_id=$1 AND owner_id=$2 AND agent_instance_id=$3 AND agent_generation=$4 AND agent_session_id=$5
  AND engine_version=$6 AND profile_version=$7 AND lease_owner=$8 AND lease_version=$9 AND auth_version=$10
  AND state='active' AND lease_expires_at>clock_timestamp() FOR UPDATE`, fence.TaskID, fence.OwnerID,
		fence.AgentInstanceID, fence.AgentGeneration, fence.AgentSessionID, fence.EngineVersion,
		fence.ProfileVersion, fence.LeaseOwner, fence.LeaseVersion, fence.AuthorizationVersion).Scan(&instanceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeUnauthenticated, "agent runtime lease is unavailable")
	}
	if err != nil {
		return fmt.Errorf("lock active agent lease: %w", err)
	}
	return nil
}

func validateAgentStart(request app.AgentInstanceStart) error {
	return request.Validate()
}

func validateExistingAgentFence(fence app.AgentInstanceFence) error {
	return fence.Validate()
}

func agentStopUnconfirmed() error {
	err := domain.NewError(domain.CodeInvalidState, "previous agent stop is unconfirmed")
	err.Details["reason"] = "agent_stop_unconfirmed"
	return err
}

var _ app.AgentInstanceStore = (*Store)(nil)
