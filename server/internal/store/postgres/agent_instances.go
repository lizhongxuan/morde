package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type agentTaskLock struct {
	HostID, InstanceID, SessionID, EngineVersion, ProfileVersion string
	Generation                                                   int64
	CheckpointThrough                                            int64
	CheckpointDigest                                             []byte
}

type agentInstanceRow struct {
	Fence                    app.AgentInstanceFence
	RuntimeServiceInstanceID string
	RuntimeID                string
	RuntimeGeneration        int64
	RuntimeSequence          int64
	State                    string
	ExpiresAt                time.Time
	NonceHash                []byte
	Receipt                  []byte
	ReceiptHash              []byte
	GuardianAttestation      []byte
	GuardianAttestationHash  []byte
	InitialLeaseMillis       int64
}

func (s *Store) StartAgentInstance(ctx context.Context, request app.AgentInstanceStart) (app.AgentInstanceLease, bool, error) {
	if err := validateAgentStart(request); err != nil {
		return app.AgentInstanceLease{}, false, err
	}
	var result app.AgentInstanceLease
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var runtimeFence *app.AccountAgentRuntimeFence
		if request.RuntimeFence != nil {
			gate, err := lockOwnerExecutionControl(ctx, tx, request.Fence.OwnerID, false)
			if err != nil {
				return err
			}
			if gate.state != "enabled" || gate.epoch != request.RuntimeFence.ExecutionEpoch {
				return ownerExecutionDisabledError(gate)
			}
			runtime, err := lockAccountAgentRuntime(ctx, tx, *request.RuntimeFence)
			if err != nil {
				return err
			}
			leaseActive, err := accountAgentRuntimeLeaseActive(ctx, tx, *request.RuntimeFence)
			if err != nil {
				return err
			}
			if runtime.State != app.AccountAgentRuntimeRunning || !leaseActive {
				return accountAgentRuntimeFenceError()
			}
			fence := *request.RuntimeFence
			runtimeFence = &fence
		}
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
				if previous.Fence == candidate && sameAccountAgentRuntimeBinding(previous, runtimeFence) && bytes.Equal(previous.NonceHash, nonceHash[:]) {
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
						result = app.AgentInstanceLease{Fence: previous.Fence, RuntimeFence: copyAccountAgentRuntimeFence(runtimeFence),
							RuntimeSequence: previous.RuntimeSequence, ExpiresAt: deadline}
						replayed = true
						return nil
					}
					if !errors.Is(err, pgx.ErrNoRows) {
						return fmt.Errorf("check existing agent lease: %w", err)
					}
				}
				return agentStopUnconfirmed()
			}
			if previous.State != "stopped" || !validAgentStopEvidence(previous) {
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
		runtimeSequence := int64(0)
		var runtimeServiceInstanceID, runtimeIDArg, runtimeGenerationArg, runtimeSequenceArg any
		if runtimeFence != nil {
			runtimeSequence, err = incrementAccountAgentRuntimeSequence(ctx, tx, *runtimeFence)
			if err != nil {
				return err
			}
			runtimeServiceInstanceID, runtimeIDArg = runtimeFence.ServiceInstanceID, runtimeFence.RuntimeID
			runtimeGenerationArg, runtimeSequenceArg = runtimeFence.RuntimeGeneration, runtimeSequence
		}
		inserted, err := tx.Exec(ctx, `INSERT INTO task_agent_instances
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,engine_version,profile_version,supervisor_id,
 stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state,
 service_instance_id,account_runtime_id,account_runtime_generation,runtime_sequence)
SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'active',$15::uuid,$16,$17,$18
WHERE $14::timestamptz>clock_timestamp()`,
			fence.TaskID, fence.OwnerID, fence.AgentInstanceID, fence.AgentGeneration, fence.AgentSessionID,
			fence.EngineVersion, fence.ProfileVersion, fence.SupervisorID, nonceHash[:], fence.LeaseOwner,
			fence.LeaseVersion, fence.AuthorizationVersion, request.LeaseDuration.Milliseconds(), expiresAt,
			runtimeServiceInstanceID, runtimeIDArg, runtimeGenerationArg, runtimeSequenceArg)
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
		if _, err := tx.Exec(ctx, `INSERT INTO task_agent_sessions
(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest)
VALUES ($1,$2,$3,$4,$5) ON CONFLICT (task_id,session_id) DO NOTHING`,
			fence.TaskID, fence.OwnerID, fence.AgentSessionID, current.CheckpointThrough, current.CheckpointDigest); err != nil {
			return fmt.Errorf("register root agent session: %w", err)
		}
		var rootSession bool
		var registeredThrough int64
		var registeredDigest []byte
		if err := tx.QueryRow(ctx, `SELECT parent_session_id IS NULL,checkpoint_through_seq,checkpoint_digest FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 FOR UPDATE`, fence.TaskID, fence.OwnerID, fence.AgentSessionID).
			Scan(&rootSession, &registeredThrough, &registeredDigest); err != nil {
			return fmt.Errorf("verify root agent session registration: %w", err)
		}
		if !rootSession || registeredThrough != current.CheckpointThrough || !bytes.Equal(registeredDigest, current.CheckpointDigest) {
			return domain.NewError(domain.CodeInvalidState, "agent root session checkpoint head changed")
		}
		result = app.AgentInstanceLease{Fence: fence, RuntimeFence: copyAccountAgentRuntimeFence(runtimeFence),
			RuntimeSequence: runtimeSequence, ExpiresAt: expiresAt}
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
		var runtimeServiceInstanceID, runtimeID string
		var runtimeGeneration int64
		var runtimeFence *app.AccountAgentRuntimeFence
		err := tx.QueryRow(ctx, `SELECT COALESCE(service_instance_id::text,''),COALESCE(account_runtime_id,''),
COALESCE(account_runtime_generation,0) FROM task_agent_instances
WHERE task_id=$1 AND owner_id=$2 AND agent_generation=$3`, fence.TaskID, fence.OwnerID, fence.AgentGeneration).
			Scan(&runtimeServiceInstanceID, &runtimeID, &runtimeGeneration)
		if errors.Is(err, pgx.ErrNoRows) {
			return agentStopUnconfirmed()
		}
		if err != nil {
			return fmt.Errorf("read Agent Runtime binding before renewal: %w", err)
		}
		if runtimeServiceInstanceID != "" {
			gate, err := lockOwnerExecutionControl(ctx, tx, fence.OwnerID, false)
			if err != nil {
				return err
			}
			if gate.state != "enabled" {
				return ownerExecutionDisabledError(gate)
			}
			runtime, err := lockCurrentAccountAgentRuntimeForTask(ctx, tx, fence.OwnerID, runtimeServiceInstanceID, runtimeID, runtimeGeneration)
			if err != nil {
				return err
			}
			if runtime.Fence.ExecutionEpoch != gate.epoch {
				return ownerExecutionDisabledError(gate)
			}
			runtimeFence = &runtime.Fence
		}
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
		if !sameAccountAgentRuntimeBinding(instance, runtimeFence) {
			return accountAgentRuntimeFenceError()
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
		result = app.AgentInstanceLease{Fence: fence, RuntimeFence: copyAccountAgentRuntimeFence(runtimeFence),
			RuntimeSequence: instance.RuntimeSequence, ExpiresAt: expiresAt}
		return nil
	})
	return result, err
}

func (s *Store) ActiveAgentGuardianStopIdentity(ctx context.Context, taskID, ownerID, agentInstanceID string, generation int64) (app.AgentGuardianStopIdentity, error) {
	if !domain.ValidTaskID(taskID) || !domain.ValidUserID(ownerID) || len(agentInstanceID) == 0 || len(agentInstanceID) > 128 || generation < 1 {
		return app.AgentGuardianStopIdentity{}, domain.InvalidArgument("agent_instance", "current Agent recovery identity is invalid")
	}
	var result app.AgentGuardianStopIdentity
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var sessionID, engineVersion, profileVersion, supervisorID, leaseOwner, nonceSHA256 string
		var leaseVersion, authorizationVersion int64
		err := tx.QueryRow(ctx, `SELECT t.agent_session_id,t.agent_engine_version,t.agent_profile_version,
i.supervisor_id,i.lease_owner,i.lease_version,i.auth_version,encode(i.stop_nonce_digest,'hex')
FROM tasks t JOIN task_agent_instances i ON i.task_id=t.id AND i.owner_id=t.owner_id
WHERE t.id=$1 AND t.owner_id=$2 AND t.execution_location='server'
  AND t.agent_instance_id=$3 AND t.agent_generation=$4 AND t.agent_session_id=i.agent_session_id
  AND t.agent_engine_version=i.engine_version AND t.agent_profile_version=i.profile_version
  AND i.agent_instance_id=$3 AND i.agent_generation=$4 AND i.state='active'
FOR UPDATE OF t,i`, taskID, ownerID, agentInstanceID, generation).Scan(
			&sessionID, &engineVersion, &profileVersion, &supervisorID, &leaseOwner, &leaseVersion, &authorizationVersion, &nonceSHA256)
		if errors.Is(err, pgx.ErrNoRows) {
			return agentStopUnconfirmed()
		}
		if err != nil {
			return fmt.Errorf("load active Agent Guardian recovery identity: %w", err)
		}
		digest, err := hex.DecodeString(nonceSHA256)
		if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != nonceSHA256 {
			return domain.NewError(domain.CodeInternal, "active Agent nonce digest is invalid")
		}
		result = app.AgentGuardianStopIdentity{
			Fence: app.AgentInstanceFence{TaskID: taskID, OwnerID: ownerID, AgentInstanceID: agentInstanceID,
				AgentGeneration: generation, AgentSessionID: sessionID, SupervisorID: supervisorID,
				LeaseOwner: leaseOwner, LeaseVersion: leaseVersion, AuthorizationVersion: authorizationVersion,
				EngineVersion: engineVersion, ProfileVersion: profileVersion},
			NonceSHA256: nonceSHA256,
		}
		if err := result.Fence.Validate(); err != nil {
			return domain.NewError(domain.CodeInternal, "active Agent fence is invalid")
		}
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
		var currentID, currentSession, currentTaskStatus string
		var currentGeneration int64
		err := tx.QueryRow(ctx, `SELECT agent_instance_id,agent_generation,agent_session_id,status FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`,
			receipt.Fence.TaskID, receipt.Fence.OwnerID).Scan(&currentID, &currentGeneration, &currentSession, &currentTaskStatus)
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
			if len(instance.Receipt) > 0 && !bytes.Equal(instance.ReceiptHash, receiptHash[:]) {
				return domain.NewError(domain.CodeIdempotencyConflict, "agent stop receipt was already recorded differently")
			}
			if len(instance.Receipt) > 0 {
				replayed = true
				if currentTaskStatus == string(task.StatusCancelling) {
					return settleStoppedAgentChildSessions(ctx, tx, receipt.Fence)
				}
				return nil
			}
			if len(instance.GuardianAttestation) == 0 || !validAgentStopEvidence(instance) {
				return agentStopUnconfirmed()
			}
			_, err = tx.Exec(ctx, `UPDATE task_agent_instances SET stop_receipt=$3,stop_receipt_digest=$4,updated_at=clock_timestamp()
WHERE task_id=$1 AND agent_generation=$2`, receipt.Fence.TaskID, receipt.Fence.AgentGeneration, encoded, receiptHash[:])
			if err != nil {
				return fmt.Errorf("persist additional verified agent stop receipt: %w", err)
			}
			if currentTaskStatus == string(task.StatusCancelling) {
				return settleStoppedAgentChildSessions(ctx, tx, receipt.Fence)
			}
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
		if currentTaskStatus == string(task.StatusCancelling) {
			return settleStoppedAgentChildSessions(ctx, tx, receipt.Fence)
		}
		return nil
	})
	return replayed, err
}

func (s *Store) ConfirmAgentGuardianStopped(ctx context.Context, proof app.VerifiedAgentGuardianStop) (bool, error) {
	attestation, encoded := proof.Value()
	if err := validateExistingAgentFence(attestation.Fence); err != nil || !attestation.Destroyed || len(attestation.Signature) != 64 ||
		len(attestation.NonceSHA256) != sha256.Size*2 || len(attestation.ProofSHA256) != sha256.Size*2 || len(encoded) == 0 || len(encoded) > 8192 {
		return false, domain.InvalidArgument("attestation", "verified Guardian stop attestation is required")
	}
	nonceHash, err := hex.DecodeString(attestation.NonceSHA256)
	if err != nil || hex.EncodeToString(nonceHash) != attestation.NonceSHA256 {
		return false, domain.InvalidArgument("attestation", "Guardian nonce digest is invalid")
	}
	attestationHash := sha256.Sum256(encoded)
	var replayed bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockOwnerExecutionControl(ctx, tx, attestation.Fence.OwnerID, false); err != nil {
			return err
		}
		var ownerID string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR SHARE`, attestation.Fence.OwnerID).Scan(&ownerID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "agent stop owner is unavailable")
		} else if err != nil {
			return fmt.Errorf("lock Guardian stop owner: %w", err)
		}
		var currentID, currentSession, currentTaskStatus string
		var currentGeneration int64
		if err := tx.QueryRow(ctx, `SELECT agent_instance_id,agent_generation,agent_session_id,status FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`,
			attestation.Fence.TaskID, attestation.Fence.OwnerID).Scan(&currentID, &currentGeneration, &currentSession, &currentTaskStatus); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "agent stop task is unavailable")
		} else if err != nil {
			return fmt.Errorf("lock Guardian stop task: %w", err)
		}
		instance, err := lockAgentInstance(ctx, tx, attestation.Fence.TaskID, attestation.Fence.OwnerID, attestation.Fence.AgentGeneration)
		if err != nil {
			return err
		}
		if instance.Fence != attestation.Fence || !bytes.Equal(instance.NonceHash, nonceHash) {
			return domain.NewError(domain.CodeUnauthenticated, "Guardian stop attestation does not match the agent instance")
		}
		if instance.State == "stopped" && len(instance.GuardianAttestation) > 0 {
			if !bytes.Equal(instance.GuardianAttestationHash, attestationHash[:]) {
				return domain.NewError(domain.CodeIdempotencyConflict, "Guardian stop attestation was already recorded differently")
			}
			replayed = true
			if currentTaskStatus == string(task.StatusCancelling) {
				return settleStoppedAgentChildSessions(ctx, tx, attestation.Fence)
			}
			return nil
		}
		if currentID != attestation.Fence.AgentInstanceID || currentGeneration != attestation.Fence.AgentGeneration ||
			currentSession != attestation.Fence.AgentSessionID {
			return agentStopUnconfirmed()
		}
		if instance.State != "active" && instance.State != "stopped" {
			return agentStopUnconfirmed()
		}
		_, err = tx.Exec(ctx, `UPDATE task_agent_instances SET state='stopped',guardian_stop_attestation=$3,
guardian_stop_attestation_digest=$4,stopped_at=COALESCE(stopped_at,clock_timestamp()),updated_at=clock_timestamp()
WHERE task_id=$1 AND agent_generation=$2`, attestation.Fence.TaskID, attestation.Fence.AgentGeneration, encoded, attestationHash[:])
		if err != nil {
			return fmt.Errorf("persist verified Guardian stop attestation: %w", err)
		}
		if currentTaskStatus == string(task.StatusCancelling) {
			return settleStoppedAgentChildSessions(ctx, tx, attestation.Fence)
		}
		return nil
	})
	return replayed, err
}

func (s *Store) ConfirmAccountAgentTaskStopped(ctx context.Context, proof app.VerifiedAccountAgentTaskStop) (bool, error) {
	attestation, encoded := proof.Value()
	if err := validateAccountAgentTaskStopProofForStore(attestation, encoded); err != nil {
		return false, err
	}
	nonceHash, err := hex.DecodeString(attestation.NonceSHA256)
	if err != nil || hex.EncodeToString(nonceHash) != attestation.NonceSHA256 {
		return false, domain.InvalidArgument("attestation", "Guardian Task nonce digest is invalid")
	}
	proofHash := sha256.Sum256(encoded)
	var replayed bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockOwnerExecutionControl(ctx, tx, attestation.TaskFence.OwnerID, false); err != nil {
			return err
		}
		runtime, err := lockAccountAgentRuntime(ctx, tx, attestation.RuntimeFence)
		if err != nil {
			return err
		}
		if runtime.State != app.AccountAgentRuntimeRunning && runtime.State != app.AccountAgentRuntimeStopping {
			return accountAgentRuntimeFenceError()
		}
		var currentID, currentSession string
		var currentGeneration int64
		if err := tx.QueryRow(ctx, `SELECT agent_instance_id,agent_generation,agent_session_id FROM tasks
WHERE id=$1 AND owner_id=$2 FOR UPDATE`, attestation.TaskFence.TaskID, attestation.TaskFence.OwnerID).
			Scan(&currentID, &currentGeneration, &currentSession); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "account Task stop owner is unavailable")
		} else if err != nil {
			return fmt.Errorf("lock Task for Guardian process stop: %w", err)
		}
		instance, err := lockAgentInstance(ctx, tx, attestation.TaskFence.TaskID, attestation.TaskFence.OwnerID,
			attestation.TaskFence.AgentGeneration)
		if err != nil {
			return err
		}
		if instance.Fence != attestation.TaskFence || instance.RuntimeServiceInstanceID != attestation.RuntimeFence.ServiceInstanceID ||
			instance.RuntimeID != attestation.RuntimeFence.RuntimeID || instance.RuntimeGeneration != attestation.RuntimeFence.RuntimeGeneration ||
			!bytes.Equal(instance.NonceHash, nonceHash) {
			return domain.NewError(domain.CodeUnauthenticated, "Guardian Task stop proof does not match the current Runtime-bound Agent")
		}
		if instance.State == "stopped" && len(instance.GuardianAttestation) > 0 {
			if !bytes.Equal(instance.GuardianAttestationHash, proofHash[:]) {
				return domain.NewError(domain.CodeIdempotencyConflict, "Guardian Task stop proof was already recorded differently")
			}
			replayed = true
			return nil
		}
		if currentID != attestation.TaskFence.AgentInstanceID || currentGeneration != attestation.TaskFence.AgentGeneration ||
			currentSession != attestation.TaskFence.AgentSessionID || instance.State != "active" {
			return agentStopUnconfirmed()
		}
		command, err := tx.Exec(ctx, `UPDATE task_agent_instances SET state='stopped',guardian_stop_attestation=$4,
guardian_stop_attestation_digest=$5,guardian_task_stop_ack_required=true,guardian_stop_acknowledged_at=NULL,
stopped_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE task_id=$1 AND owner_id=$2 AND agent_generation=$3 AND state='active'
 AND agent_instance_id=$6 AND agent_session_id=$7 AND lease_owner=$8 AND lease_version=$9 AND auth_version=$10
 AND service_instance_id=$11::uuid AND account_runtime_id=$12 AND account_runtime_generation=$13
 AND stop_nonce_digest=$14`, attestation.TaskFence.TaskID, attestation.TaskFence.OwnerID,
			attestation.TaskFence.AgentGeneration, encoded, proofHash[:], attestation.TaskFence.AgentInstanceID,
			attestation.TaskFence.AgentSessionID, attestation.TaskFence.LeaseOwner, attestation.TaskFence.LeaseVersion,
			attestation.TaskFence.AuthorizationVersion, attestation.RuntimeFence.ServiceInstanceID,
			attestation.RuntimeFence.RuntimeID, attestation.RuntimeFence.RuntimeGeneration, nonceHash)
		if err != nil {
			return fmt.Errorf("persist verified account Task process stop: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "account Task Agent changed while confirming process stop")
		}
		return nil
	})
	return replayed, err
}

func (s *Store) ListPendingAccountAgentTaskStopAttestations(ctx context.Context, limit int) ([]app.AccountAgentTaskStopAttestation, error) {
	if ctx == nil || limit < 1 || limit > app.MaxPendingAccountAgentTaskStopAttestations {
		return nil, domain.InvalidArgument("attestation", "pending Guardian Task stop query is invalid")
	}
	attestations := make([]app.AccountAgentTaskStopAttestation, 0)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		serviceInstanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT task_id,owner_id,agent_generation,service_instance_id::text,
account_runtime_id,account_runtime_generation,stop_nonce_digest,guardian_stop_attestation,guardian_stop_attestation_digest
FROM task_agent_instances
WHERE service_instance_id=$1::uuid AND state='stopped' AND guardian_task_stop_ack_required
 AND guardian_stop_acknowledged_at IS NULL
ORDER BY stopped_at,task_id,agent_generation LIMIT $2`, serviceInstanceID, limit)
		if err != nil {
			return fmt.Errorf("list pending Guardian Task stop acknowledgements: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var taskID, ownerID, storedServiceInstanceID, runtimeID string
			var generation, runtimeGeneration int64
			var nonceDigest, encoded, storedDigest []byte
			if err := rows.Scan(&taskID, &ownerID, &generation, &storedServiceInstanceID, &runtimeID,
				&runtimeGeneration, &nonceDigest, &encoded, &storedDigest); err != nil {
				return fmt.Errorf("read pending Guardian Task stop acknowledgement: %w", err)
			}
			var attestation app.AccountAgentTaskStopAttestation
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&attestation); err != nil {
				return domain.NewError(domain.CodeInvalidState, "stored Guardian Task stop attestation is invalid")
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				return domain.NewError(domain.CodeInvalidState, "stored Guardian Task stop attestation has trailing data")
			}
			canonical, err := json.Marshal(attestation)
			if err != nil || !bytes.Equal(canonical, encoded) {
				return domain.NewError(domain.CodeInvalidState, "stored Guardian Task stop attestation is not canonical")
			}
			digest := sha256.Sum256(encoded)
			attestationNonceDigest, nonceErr := hex.DecodeString(attestation.NonceSHA256)
			if validateAccountAgentTaskStopProofForStore(attestation, encoded) != nil ||
				!bytes.Equal(storedDigest, digest[:]) || nonceErr != nil || !validAgentNonceDigest(attestation.NonceSHA256) ||
				!bytes.Equal(nonceDigest, attestationNonceDigest) ||
				storedServiceInstanceID != serviceInstanceID || attestation.RuntimeFence.ServiceInstanceID != serviceInstanceID ||
				attestation.RuntimeFence.OwnerID != ownerID || attestation.RuntimeFence.RuntimeID != runtimeID ||
				attestation.RuntimeFence.RuntimeGeneration != runtimeGeneration || attestation.TaskFence.TaskID != taskID ||
				attestation.TaskFence.OwnerID != ownerID || attestation.TaskFence.AgentGeneration != generation {
				return domain.NewError(domain.CodeInvalidState, "stored Guardian Task stop attestation does not match its Agent fence")
			}
			attestations = append(attestations, attestation)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate pending Guardian Task stop acknowledgements: %w", err)
		}
		return nil
	})
	return attestations, err
}

func (s *Store) MarkAccountAgentTaskStopAttestationAcknowledged(ctx context.Context,
	proof app.VerifiedAccountAgentTaskStop) (bool, error) {
	if ctx == nil {
		return false, domain.InvalidArgument("context", "Guardian Task stop acknowledgement context is required")
	}
	attestation, encoded := proof.Value()
	if err := validateAccountAgentTaskStopProofForStore(attestation, encoded); err != nil {
		return false, err
	}
	proofDigest := sha256.Sum256(encoded)
	nonceDigest, err := hex.DecodeString(attestation.NonceSHA256)
	if err != nil || hex.EncodeToString(nonceDigest) != attestation.NonceSHA256 {
		return false, domain.InvalidArgument("attestation", "Guardian Task nonce digest is invalid")
	}
	var replayed bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		command, err := tx.Exec(ctx, `UPDATE task_agent_instances SET guardian_stop_acknowledged_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE task_id=$1 AND owner_id=$2 AND agent_instance_id=$3 AND agent_generation=$4 AND agent_session_id=$5
 AND supervisor_id=$6 AND lease_owner=$7 AND lease_version=$8 AND auth_version=$9 AND engine_version=$10 AND profile_version=$11
 AND state='stopped' AND service_instance_id=$12::uuid AND account_runtime_id=$13 AND account_runtime_generation=$14
 AND stop_nonce_digest=$15 AND guardian_stop_attestation=$16 AND guardian_stop_attestation_digest=$17
 AND guardian_task_stop_ack_required AND guardian_stop_acknowledged_at IS NULL`, attestation.TaskFence.TaskID,
			attestation.TaskFence.OwnerID, attestation.TaskFence.AgentInstanceID, attestation.TaskFence.AgentGeneration,
			attestation.TaskFence.AgentSessionID, attestation.TaskFence.SupervisorID, attestation.TaskFence.LeaseOwner,
			attestation.TaskFence.LeaseVersion, attestation.TaskFence.AuthorizationVersion, attestation.TaskFence.EngineVersion,
			attestation.TaskFence.ProfileVersion, attestation.RuntimeFence.ServiceInstanceID, attestation.RuntimeFence.RuntimeID,
			attestation.RuntimeFence.RuntimeGeneration, nonceDigest, encoded, proofDigest[:])
		if err != nil {
			return fmt.Errorf("mark Guardian Task stop proof acknowledged: %w", err)
		}
		if command.RowsAffected() == 1 {
			return nil
		}
		var acknowledgedAt time.Time
		err = tx.QueryRow(ctx, `SELECT guardian_stop_acknowledged_at FROM task_agent_instances
WHERE task_id=$1 AND owner_id=$2 AND agent_instance_id=$3 AND agent_generation=$4 AND agent_session_id=$5
 AND supervisor_id=$6 AND lease_owner=$7 AND lease_version=$8 AND auth_version=$9 AND engine_version=$10 AND profile_version=$11
 AND state='stopped' AND service_instance_id=$12::uuid AND account_runtime_id=$13 AND account_runtime_generation=$14
 AND stop_nonce_digest=$15 AND guardian_stop_attestation=$16 AND guardian_stop_attestation_digest=$17
 AND guardian_task_stop_ack_required AND guardian_stop_acknowledged_at IS NOT NULL`, attestation.TaskFence.TaskID,
			attestation.TaskFence.OwnerID, attestation.TaskFence.AgentInstanceID, attestation.TaskFence.AgentGeneration,
			attestation.TaskFence.AgentSessionID, attestation.TaskFence.SupervisorID, attestation.TaskFence.LeaseOwner,
			attestation.TaskFence.LeaseVersion, attestation.TaskFence.AuthorizationVersion, attestation.TaskFence.EngineVersion,
			attestation.TaskFence.ProfileVersion, attestation.RuntimeFence.ServiceInstanceID, attestation.RuntimeFence.RuntimeID,
			attestation.RuntimeFence.RuntimeGeneration, nonceDigest, encoded, proofDigest[:]).Scan(&acknowledgedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "Guardian Task stop acknowledgement does not match the persisted proof")
		}
		if err != nil {
			return fmt.Errorf("read Guardian Task stop acknowledgement: %w", err)
		}
		replayed = true
		return nil
	})
	return replayed, err
}

func validateAccountAgentTaskStopProofForStore(attestation app.AccountAgentTaskStopAttestation, encoded []byte) error {
	if attestation.RuntimeFence.Validate() != nil || attestation.TaskFence.Validate() != nil ||
		attestation.TaskFence.OwnerID != attestation.RuntimeFence.OwnerID || !attestation.Destroyed ||
		len(attestation.RuntimeContainerID) != sha256.Size*2 || !validAgentNonceDigest(attestation.RuntimeContainerID) ||
		attestation.ContainerPID < 1 || attestation.HostPID < 1 || attestation.StartTimeTick == 0 ||
		!filepath.IsAbs(attestation.CgroupPath) || filepath.Clean(attestation.CgroupPath) != attestation.CgroupPath ||
		attestation.CgroupPath == "/" || strings.ContainsAny(attestation.CgroupPath, "\x00\r\n") ||
		len(attestation.NonceSHA256) != sha256.Size*2 || !validAgentNonceDigest(attestation.NonceSHA256) ||
		len(attestation.ProofSHA256) != sha256.Size*2 || !validAgentNonceDigest(attestation.ProofSHA256) ||
		attestation.ExpiresAt.IsZero() || attestation.DestroyedAt.IsZero() || len(attestation.Signature) != 64 ||
		len(encoded) == 0 || len(encoded) > 8192 {
		return domain.InvalidArgument("attestation", "verified Guardian Task stop attestation is required")
	}
	return nil
}

func validAgentNonceDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func settleStoppedAgentChildSessions(ctx context.Context, tx pgx.Tx, fence app.AgentInstanceFence) error {
	var currentID, currentSession, status string
	var generation int64
	if err := tx.QueryRow(ctx, `SELECT agent_instance_id,agent_generation,agent_session_id,status
FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, fence.TaskID, fence.OwnerID).
		Scan(&currentID, &generation, &currentSession, &status); err != nil {
		return fmt.Errorf("verify cancelled child Agent stop fence: %w", err)
	}
	if status != string(task.StatusCancelling) || currentID != fence.AgentInstanceID ||
		generation != fence.AgentGeneration || currentSession != fence.AgentSessionID {
		return nil
	}
	updated, err := tx.Exec(ctx, `UPDATE task_steps st SET status='unknown',
summary='The parent Task was cancelled after the Agent process tree stopped.'
FROM task_agent_sessions s
	WHERE s.task_id=$1 AND s.step_id=st.id AND s.parent_session_id IS NOT NULL
  AND st.task_id=$1 AND st.status IN ('queued','running','waiting_user')`, fence.TaskID)
	if err != nil {
		return fmt.Errorf("settle cancelled child Agent TaskSteps after verified stop: %w", err)
	}
	if updated.RowsAffected() == 0 {
		return nil
	}
	sequence, err := incrementTaskEventSequence(ctx, tx, fence.TaskID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"phase": "subagents_stopped", "status": string(task.StepUnknown),
		"step_count": updated.RowsAffected()})
	if err != nil {
		return fmt.Errorf("encode cancelled child Agent event: %w", err)
	}
	return insertTaskEvent(ctx, tx, task.Event{TaskID: fence.TaskID, Sequence: sequence, Type: "task.step", Payload: payload})
}

func validAgentStopEvidence(instance agentInstanceRow) bool {
	hasReceipt := len(instance.Receipt) > 0 && len(instance.ReceiptHash) == sha256.Size
	if len(instance.Receipt) > 0 && !hasReceipt || len(instance.ReceiptHash) > 0 && !hasReceipt {
		return false
	}
	if hasReceipt {
		digest := sha256.Sum256(instance.Receipt)
		if !bytes.Equal(instance.ReceiptHash, digest[:]) {
			return false
		}
	}
	hasGuardian := len(instance.GuardianAttestation) > 0 && len(instance.GuardianAttestationHash) == sha256.Size
	if len(instance.GuardianAttestation) > 0 && !hasGuardian || len(instance.GuardianAttestationHash) > 0 && !hasGuardian {
		return false
	}
	if hasGuardian {
		digest := sha256.Sum256(instance.GuardianAttestation)
		if !bytes.Equal(instance.GuardianAttestationHash, digest[:]) {
			return false
		}
	}
	return hasReceipt || hasGuardian
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
stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state,stop_receipt,stop_receipt_digest,
guardian_stop_attestation,guardian_stop_attestation_digest,COALESCE(service_instance_id::text,''),
COALESCE(account_runtime_id,''),COALESCE(account_runtime_generation,0),COALESCE(runtime_sequence,0)
FROM task_agent_instances WHERE task_id=$1 AND owner_id=$2 AND agent_generation=$3 FOR UPDATE`, taskID, ownerID, generation).Scan(
		&row.Fence.AgentInstanceID, &row.Fence.AgentSessionID, &row.Fence.EngineVersion, &row.Fence.ProfileVersion,
		&row.Fence.SupervisorID, &row.NonceHash, &row.Fence.LeaseOwner, &row.Fence.LeaseVersion,
		&row.Fence.AuthorizationVersion, &row.InitialLeaseMillis, &row.ExpiresAt, &row.State, &row.Receipt, &row.ReceiptHash,
		&row.GuardianAttestation, &row.GuardianAttestationHash, &row.RuntimeServiceInstanceID, &row.RuntimeID,
		&row.RuntimeGeneration, &row.RuntimeSequence)
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
