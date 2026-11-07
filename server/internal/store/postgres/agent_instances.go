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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type agentTaskLock struct {
	HostID, InstanceID, SessionID, EngineVersion, ProfileVersion string
	InitiatorID, ProjectID, ConversationID                       string
	Generation                                                   int64
	CheckpointThrough                                            int64
	CheckpointDigest                                             []byte
}

type agentInstanceRow struct {
	Fence                    app.AgentInstanceFence
	HostFence                agentHostFence
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

// agentHostFence is an immutable snapshot of the Host authorization and
// runtime that the Agent was allowed to use when its lease was issued.
type agentHostFence struct {
	HostID, OwnerID, ActorID, Permission, AuthorizationKind string
	GrantFingerprint                                        string
	RuntimeGeneration, ControlVersion                       int64
	RuntimeInstanceID                                       string
}

type agentHostTarget struct {
	TaskHostID, TaskActorID string
	OwnerID                 string
	State, ControlState     string
	Deleted                 bool
	HasActiveGrant          bool
	GrantVersion            string
	GrantCreatedAt          time.Time
	GrantPermissions        []string
	RuntimeGeneration       int64
	RuntimeInstanceID       string
	ControlVersion          int64
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
			gate, err := lockOwnerExecutionControl(ctx, tx, request.RuntimeFence.OwnerID, false)
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
		hostTarget, err := lockAgentHostTarget(ctx, tx, request.Fence.TaskID, request.Fence.OwnerID)
		if err != nil {
			return err
		}
		current, err := lockAuthorizedAgentTask(ctx, tx, request.Fence)
		if err != nil {
			return err
		}
		if runtimeFence != nil && current.ConversationID != runtimeFence.ConversationID {
			return accountAgentRuntimeFenceError()
		}
		if current.HostID != hostTarget.TaskHostID || current.InitiatorID != hostTarget.TaskActorID {
			return agentHostFenceUnavailable()
		}
		if request.Fence.AgentInstanceID == current.HostID {
			return domain.InvalidArgument("agent_instance_id", "agent instance cannot be the Host")
		}
		hostFence, hostFenceErr := captureAgentHostFence(ctx, tx, current, hostTarget)
		if hostFenceErr != nil && domain.CodeOf(hostFenceErr) != domain.CodeInvalidState {
			return hostFenceErr
		}
		nonceHash := sha256.Sum256(request.SupervisorNonce)
		generation := int64(1)
		if current.InstanceID == "" && current.Generation == 0 {
			if hostFenceErr != nil {
				return hostFenceErr
			}
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
			if hostFenceErr != nil {
				// A temporarily unavailable Host cannot prevent recovery of a
				// text conversation. Keep the previous operation fence, but only
				// after proving that its owner, actor and exact grant remain valid.
				if previous.HostFence.HostID != hostTarget.TaskHostID ||
					(previous.HostFence.HostID != "" && (previous.HostFence.OwnerID != hostTarget.OwnerID ||
						previous.HostFence.ActorID != current.InitiatorID)) {
					return agentHostFenceUnavailable()
				}
				if err := verifyTaskModelHostAuthorization(ctx, tx, app.ModelTaskFence{TaskID: request.Fence.TaskID,
					OwnerID: request.Fence.OwnerID, InitiatorID: current.InitiatorID,
					AgentInstanceID: previous.Fence.AgentInstanceID, AgentGeneration: current.Generation}, hostTarget, current.HostID); err != nil {
					if domain.CodeOf(err) == domain.CodeUnauthenticated {
						return agentHostFenceUnavailable()
					}
					return err
				}
				hostFence = previous.HostFence
			} else if !sameAgentHostGrant(previous.HostFence, hostFence) {
				// Regranting access must not revive an old Agent's authority.
				return agentHostFenceUnavailable()
			}
			if previous.State == "active" {
				candidate := request.Fence
				candidate.AgentGeneration = current.Generation
				if previous.Fence == candidate && sameAccountAgentRuntimeBinding(previous, runtimeFence) &&
					sameAgentHostFence(previous.HostFence, hostFence) && bytes.Equal(previous.NonceHash, nonceHash[:]) {
					if previous.InitialLeaseMillis != request.LeaseDuration.Milliseconds() {
						return domain.NewError(domain.CodeIdempotencyConflict, "agent start was reused with a different lease duration")
					}
					var deadline time.Time
					err := tx.QueryRow(ctx, `UPDATE task_agent_instances i
SET lease_expires_at=LEAST(i.lease_expires_at,t.lease_expires_at,COALESCE(t.auth_expires_at,'infinity'::timestamptz)),updated_at=clock_timestamp()
FROM tasks t WHERE i.task_id=t.id AND i.task_id=$1 AND i.agent_generation=$2 AND i.lease_expires_at>clock_timestamp()
 AND t.lease_expires_at>clock_timestamp() AND (t.auth_kind='schedule_task' OR t.auth_expires_at>clock_timestamp()) RETURNING i.lease_expires_at`,
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
 service_instance_id,account_runtime_id,account_runtime_generation,runtime_sequence,
 host_id,host_owner_id,host_actor_id,host_permission,host_authorization_kind,
 host_runtime_generation,host_runtime_instance_id,host_control_version,host_grant_fingerprint)
SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'active',$15::uuid,$16,$17,$18,
 $19,$20,$21,$22,$23,$24,$25,$26,$27
WHERE $14::timestamptz>clock_timestamp()`,
			fence.TaskID, fence.OwnerID, fence.AgentInstanceID, fence.AgentGeneration, fence.AgentSessionID,
			fence.EngineVersion, fence.ProfileVersion, fence.SupervisorID, nonceHash[:], fence.LeaseOwner,
			fence.LeaseVersion, fence.AuthorizationVersion, request.LeaseDuration.Milliseconds(), expiresAt,
			runtimeServiceInstanceID, runtimeIDArg, runtimeGenerationArg, runtimeSequenceArg,
			hostFenceValue(hostFence, func(value agentHostFence) any { return value.HostID }),
			hostFenceValue(hostFence, func(value agentHostFence) any { return value.OwnerID }),
			hostFenceValue(hostFence, func(value agentHostFence) any { return value.ActorID }),
			hostFenceValue(hostFence, func(value agentHostFence) any { return value.Permission }),
			hostFenceValue(hostFence, func(value agentHostFence) any { return value.AuthorizationKind }),
			hostFenceValue(hostFence, func(value agentHostFence) any { return value.RuntimeGeneration }),
			hostFenceValue(hostFence, func(value agentHostFence) any { return value.RuntimeInstanceID }),
			hostFenceValue(hostFence, func(value agentHostFence) any { return value.ControlVersion }),
			hostGrantFingerprintValue(hostFence))
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
		hostTarget, err := lockAgentHostTarget(ctx, tx, fence.TaskID, fence.OwnerID)
		if err != nil {
			return err
		}
		current, err := lockAuthorizedAgentTask(ctx, tx, fence)
		if err != nil {
			return err
		}
		if runtimeFence != nil && current.ConversationID != runtimeFence.ConversationID {
			return accountAgentRuntimeFenceError()
		}
		if current.HostID != hostTarget.TaskHostID || current.InitiatorID != hostTarget.TaskActorID {
			return agentHostFenceUnavailable()
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
		// The Host is an optional tool. A stopped runtime or a control handoff
		// must not terminate an otherwise authorized text-model execution.
		// Operation admission still checks the full original runtime fence.
		if instance.HostFence.HostID != hostTarget.TaskHostID ||
			(instance.HostFence.HostID != "" && (instance.HostFence.OwnerID != hostTarget.OwnerID ||
				instance.HostFence.ActorID != current.InitiatorID)) {
			return agentHostFenceUnavailable()
		}
		if err := verifyTaskModelHostAuthorization(ctx, tx, app.ModelTaskFence{TaskID: fence.TaskID,
			OwnerID: fence.OwnerID, InitiatorID: current.InitiatorID, AgentInstanceID: fence.AgentInstanceID,
			AgentGeneration: fence.AgentGeneration}, hostTarget, current.HostID); err != nil {
			if domain.CodeOf(err) == domain.CodeUnauthenticated {
				return agentHostFenceUnavailable()
			}
			return err
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
  AND (t.auth_kind='schedule_task' OR t.auth_expires_at>clock_timestamp()) AND $3::timestamptz>clock_timestamp()`, fence.TaskID, fence.AgentGeneration, expiresAt)
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
		var initiatorID, sessionID, engineVersion, profileVersion, supervisorID, leaseOwner, nonceSHA256 string
		var leaseVersion, authorizationVersion int64
		err := tx.QueryRow(ctx, `SELECT t.initiator_id,t.agent_session_id,t.agent_engine_version,t.agent_profile_version,
i.supervisor_id,i.lease_owner,i.lease_version,i.auth_version,encode(i.stop_nonce_digest,'hex')
FROM tasks t JOIN task_agent_instances i ON i.task_id=t.id AND i.owner_id=t.owner_id
WHERE t.id=$1 AND t.owner_id=$2 AND t.execution_location='server'
  AND t.agent_instance_id=$3 AND t.agent_generation=$4 AND t.agent_session_id=i.agent_session_id
  AND t.agent_engine_version=i.engine_version AND t.agent_profile_version=i.profile_version
  AND i.agent_instance_id=$3 AND i.agent_generation=$4 AND i.state='active'
FOR UPDATE OF t,i`, taskID, ownerID, agentInstanceID, generation).Scan(
			&initiatorID, &sessionID, &engineVersion, &profileVersion, &supervisorID, &leaseOwner, &leaseVersion, &authorizationVersion, &nonceSHA256)
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
			Fence: app.AgentInstanceFence{TaskID: taskID, OwnerID: ownerID, InitiatorID: initiatorID, AgentInstanceID: agentInstanceID,
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
		if _, err := lockOwnerExecutionControl(ctx, tx, attestation.TaskFence.InitiatorID, false); err != nil {
			return err
		}
		runtime, err := lockAccountAgentRuntime(ctx, tx, attestation.RuntimeFence)
		if err != nil {
			return err
		}
		// A failed Runtime still needs a verified stop proof for each Task it owned.
		if runtime.State != app.AccountAgentRuntimeRunning && runtime.State != app.AccountAgentRuntimeStopping &&
			runtime.State != app.AccountAgentRuntimeFailed {
			return accountAgentRuntimeFenceError()
		}
		var currentID, currentSession, currentTaskStatus string
		var currentGeneration int64
		if err := tx.QueryRow(ctx, `SELECT agent_instance_id,agent_generation,agent_session_id,status FROM tasks
WHERE id=$1 AND owner_id=$2 FOR UPDATE`, attestation.TaskFence.TaskID, attestation.TaskFence.OwnerID).
			Scan(&currentID, &currentGeneration, &currentSession, &currentTaskStatus); errors.Is(err, pgx.ErrNoRows) {
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
			if currentTaskStatus == string(task.StatusCancelling) {
				return settleStoppedAgentChildSessions(ctx, tx, attestation.TaskFence)
			}
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
		if currentTaskStatus == string(task.StatusCancelling) {
			return settleStoppedAgentChildSessions(ctx, tx, attestation.TaskFence)
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
		rows, err := tx.Query(ctx, `SELECT i.task_id,i.owner_id,t.initiator_id,t.conversation_id,i.agent_generation,i.service_instance_id::text,
	i.account_runtime_id,i.account_runtime_generation,i.stop_nonce_digest,i.guardian_stop_attestation,i.guardian_stop_attestation_digest
FROM task_agent_instances i JOIN tasks t ON t.id=i.task_id AND t.owner_id=i.owner_id
WHERE i.service_instance_id=$1::uuid AND i.state='stopped' AND i.guardian_task_stop_ack_required
 AND i.guardian_stop_acknowledged_at IS NULL
ORDER BY i.stopped_at,i.task_id,i.agent_generation LIMIT $2`, serviceInstanceID, limit)
		if err != nil {
			return fmt.Errorf("list pending Guardian Task stop acknowledgements: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var taskID, ownerID, taskInitiatorID, conversationID, storedServiceInstanceID, runtimeID string
			var generation, runtimeGeneration int64
			var nonceDigest, encoded, storedDigest []byte
			if err := rows.Scan(&taskID, &ownerID, &taskInitiatorID, &conversationID, &generation, &storedServiceInstanceID, &runtimeID,
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
				attestation.RuntimeFence.OwnerID != ownerID || attestation.RuntimeFence.ConversationID != conversationID || attestation.RuntimeFence.RuntimeID != runtimeID ||
				attestation.RuntimeFence.RuntimeGeneration != runtimeGeneration || attestation.TaskFence.TaskID != taskID ||
				attestation.TaskFence.OwnerID != ownerID || attestation.TaskFence.InitiatorID != taskInitiatorID ||
				attestation.TaskFence.AgentGeneration != generation {
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

// settleStoppedCurrentTaskAgentChildrenIfProven covers the ordering where a
// Task stop proof was committed just before the cancellation transaction. It
// only settles descendants when the current fenced Agent row is already
// stopped with persisted proof.
func settleStoppedCurrentTaskAgentChildrenIfProven(ctx context.Context, tx pgx.Tx, taskID, ownerID string) error {
	var fence app.AgentInstanceFence
	var state string
	var hasStopProof bool
	err := tx.QueryRow(ctx, `SELECT t.id,t.owner_id,t.initiator_id,t.agent_instance_id,t.agent_generation,t.agent_session_id,
i.supervisor_id,i.lease_owner,i.lease_version,i.auth_version,i.engine_version,i.profile_version,i.state,
(i.stop_receipt IS NOT NULL AND i.stop_receipt_digest IS NOT NULL) OR
(i.guardian_stop_attestation IS NOT NULL AND i.guardian_stop_attestation_digest IS NOT NULL)
FROM tasks t JOIN task_agent_instances i
  ON i.task_id=t.id AND i.owner_id=t.owner_id AND i.agent_generation=t.agent_generation
 AND i.agent_instance_id=t.agent_instance_id AND i.agent_session_id=t.agent_session_id
WHERE t.id=$1 AND t.owner_id=$2 AND t.status='cancelling'
FOR UPDATE OF t,i`, taskID, ownerID).Scan(
		&fence.TaskID, &fence.OwnerID, &fence.InitiatorID, &fence.AgentInstanceID, &fence.AgentGeneration, &fence.AgentSessionID,
		&fence.SupervisorID, &fence.LeaseOwner, &fence.LeaseVersion, &fence.AuthorizationVersion,
		&fence.EngineVersion, &fence.ProfileVersion, &state, &hasStopProof)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read current Task Agent stop proof before child settlement: %w", err)
	}
	if state != "stopped" || !hasStopProof {
		return nil
	}
	return settleStoppedAgentChildSessions(ctx, tx, fence)
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
	rows, err := tx.Query(ctx, `WITH RECURSIVE descendants(session_id) AS (
  SELECT session_id FROM task_agent_sessions
  WHERE task_id=$1 AND owner_id=$2 AND parent_session_id=$3
  UNION
  SELECT child.session_id FROM task_agent_sessions child
  JOIN descendants parent ON child.parent_session_id=parent.session_id
  WHERE child.task_id=$1 AND child.owner_id=$2
)
SELECT s.session_id,s.parent_session_id,s.step_id
FROM descendants d
JOIN task_agent_sessions s ON s.task_id=$1 AND s.owner_id=$2 AND s.session_id=d.session_id
JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE st.status IN ('queued','running','waiting_user')
ORDER BY st.sequence
FOR UPDATE OF s,st`, fence.TaskID, fence.OwnerID, fence.AgentSessionID)
	if err != nil {
		return fmt.Errorf("lock cancelled child Agent Sessions after verified stop: %w", err)
	}
	type child struct{ sessionID, parentSessionID, stepID string }
	children := make([]child, 0)
	for rows.Next() {
		var value child
		if err := rows.Scan(&value.sessionID, &value.parentSessionID, &value.stepID); err != nil {
			rows.Close()
			return fmt.Errorf("read cancelled child Agent Session after verified stop: %w", err)
		}
		children = append(children, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate cancelled child Agent Sessions after verified stop: %w", err)
	}
	rows.Close()
	for _, child := range children {
		updated, err := tx.Exec(ctx, `UPDATE task_steps SET status='unknown',
summary='The parent Task was cancelled after the Agent process tree stopped.'
WHERE task_id=$1 AND id=$2 AND status IN ('queued','running','waiting_user')`, fence.TaskID, child.stepID)
		if err != nil {
			return fmt.Errorf("settle cancelled child Agent TaskStep after verified stop: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "child Agent TaskStep changed during stop settlement")
		}
		sequence, err := incrementTaskEventSequence(ctx, tx, fence.TaskID)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]string{"phase": "subagent_settled", "status": string(task.StepUnknown),
			"session_id": child.sessionID, "parent_session_id": child.parentSessionID, "step_id": child.stepID})
		if err != nil {
			return fmt.Errorf("encode cancelled child Agent settlement event: %w", err)
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: fence.TaskID, Sequence: sequence, Type: "task.step", Payload: payload}); err != nil {
			return err
		}
	}
	return nil
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
	projectID, err := lockTaskInitiatorInTx(ctx, tx, fence.TaskID, fence.OwnerID, fence.InitiatorID)
	if err != nil {
		return agentTaskLock{}, err
	}
	gate, err := requireOwnerExecutionEnabled(ctx, tx, fence.InitiatorID, nil)
	if err != nil {
		return agentTaskLock{}, err
	}
	var status string
	userLock := " FOR SHARE"
	if projectWorkspaceTaskPermit(ctx, fence.TaskID, fence.InitiatorID) != nil {
		userLock = ""
	}
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1`+userLock, fence.InitiatorID).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		return agentTaskLock{}, domain.NewError(domain.CodeUnauthenticated, "agent initiator is unavailable")
	} else if err != nil {
		return agentTaskLock{}, fmt.Errorf("lock agent initiator: %w", err)
	}
	if status != string(identity.StatusApproved) {
		return agentTaskLock{}, domain.NewError(domain.CodeUnauthenticated, "agent initiator is unavailable")
	}
	var current agentTaskLock
	var authKind, tokenID, installationID string
	var authScopes []string
	err = tx.QueryRow(ctx, `SELECT initiator_id,COALESCE(project_id,''),conversation_id,COALESCE(host_id,''),agent_instance_id,agent_generation,agent_session_id,agent_engine_version,
agent_profile_version,checkpoint_through_seq,checkpoint_digest,auth_kind,auth_token_id,auth_scopes,COALESCE(auth_client_installation_id,'') FROM tasks
WHERE id=$1 AND owner_id=$2 AND status='running' AND execution_location='server' AND execution_epoch=$3
 AND lease_owner=$4 AND lease_version=$5 AND lease_expires_at>clock_timestamp()
 AND auth_version=$6 AND (auth_kind='schedule_task' OR auth_expires_at>clock_timestamp()) FOR UPDATE`, fence.TaskID, fence.OwnerID, gate.epoch,
		fence.LeaseOwner, fence.LeaseVersion, fence.AuthorizationVersion).Scan(&current.InitiatorID, &current.ProjectID, &current.ConversationID, &current.HostID, &current.InstanceID, &current.Generation,
		&current.SessionID, &current.EngineVersion, &current.ProfileVersion, &current.CheckpointThrough, &current.CheckpointDigest,
		&authKind, &tokenID, &authScopes, &installationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentTaskLock{}, domain.NewError(domain.CodeUnauthenticated, "agent task authorization is unavailable")
	}
	if err != nil {
		return agentTaskLock{}, fmt.Errorf("lock agent task: %w", err)
	}
	if current.InitiatorID != fence.InitiatorID || current.ProjectID != projectID {
		return agentTaskLock{}, domain.NewError(domain.CodeUnauthenticated, "task initiator is unavailable")
	}
	if err := checkAgentCheckpointAuthorization(ctx, tx, fence.InitiatorID, fence.TaskID, authKind, tokenID, authScopes, installationID); err != nil {
		return agentTaskLock{}, err
	}
	return current, nil
}

// lockTaskInitiatorInTx binds worker execution to the immutable task actor.
// Project membership is locked below, which serializes execution authorization
// with membership changes. Do not lock the Task row here: callers may update it
// later in the same transaction, and retaining a shared row lock first can form
// a PostgreSQL lock-upgrade deadlock with concurrent Task lease renewal.
// OwnerID remains the conversation creator and the FK key for Task child rows.
func lockTaskInitiatorInTx(ctx context.Context, tx pgx.Tx, taskID, ownerID, expectedInitiatorID string) (string, error) {
	var initiatorID, projectID string
	err := tx.QueryRow(ctx, `SELECT initiator_id,COALESCE(project_id,'') FROM tasks WHERE id=$1 AND owner_id=$2`,
		taskID, ownerID).Scan(&initiatorID, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.NewError(domain.CodeUnauthenticated, "task initiator is unavailable")
	}
	if err != nil {
		return "", fmt.Errorf("lock task initiator: %w", err)
	}
	if !domain.ValidUserID(initiatorID) || (expectedInitiatorID != "" && initiatorID != expectedInitiatorID) {
		return "", domain.NewError(domain.CodeUnauthenticated, "task initiator is unavailable")
	}
	if projectID == "" {
		if initiatorID != ownerID {
			return "", domain.NewError(domain.CodeUnauthenticated, "personal task initiator is unavailable")
		}
		return "", nil
	}
	permit, _ := ctx.Value(workspaceMutationPermitKey{}).(*workspaceMutationPermit)
	if permit != nil && permit.active.Load() && permit.taskID == taskID && permit.actorID == initiatorID && permit.projectID == projectID {
		return projectID, nil
	}
	if _, _, err := lockProjectForActorInTx(ctx, tx, initiatorID, projectID, project.RoleMember, false); err != nil {
		return "", domain.NewError(domain.CodeUnauthenticated, "project task authorization is unavailable")
	}
	return projectID, nil
}

func agentLeaseDeadline(ctx context.Context, tx pgx.Tx, taskID string, duration time.Duration) (time.Time, error) {
	var deadline time.Time
	err := tx.QueryRow(ctx, `SELECT LEAST(clock_timestamp()+($2::bigint * interval '1 millisecond'),lease_expires_at,
COALESCE(auth_expires_at,'infinity'::timestamptz))
FROM tasks WHERE id=$1 AND lease_expires_at>clock_timestamp()
 AND (auth_kind='schedule_task' OR auth_expires_at>clock_timestamp())`, taskID, duration.Milliseconds()).Scan(&deadline)
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
COALESCE(account_runtime_id,''),COALESCE(account_runtime_generation,0),COALESCE(runtime_sequence,0),
COALESCE(host_id,''),COALESCE(host_owner_id,''),COALESCE(host_actor_id,''),COALESCE(host_permission,''),
COALESCE(host_authorization_kind,''),COALESCE(host_runtime_generation,0),COALESCE(host_runtime_instance_id,''),
COALESCE(host_control_version,0),COALESCE(host_grant_fingerprint,'')
FROM task_agent_instances WHERE task_id=$1 AND owner_id=$2 AND agent_generation=$3 FOR UPDATE`, taskID, ownerID, generation).Scan(
		&row.Fence.AgentInstanceID, &row.Fence.AgentSessionID, &row.Fence.EngineVersion, &row.Fence.ProfileVersion,
		&row.Fence.SupervisorID, &row.NonceHash, &row.Fence.LeaseOwner, &row.Fence.LeaseVersion,
		&row.Fence.AuthorizationVersion, &row.InitialLeaseMillis, &row.ExpiresAt, &row.State, &row.Receipt, &row.ReceiptHash,
		&row.GuardianAttestation, &row.GuardianAttestationHash, &row.RuntimeServiceInstanceID, &row.RuntimeID,
		&row.RuntimeGeneration, &row.RuntimeSequence,
		&row.HostFence.HostID, &row.HostFence.OwnerID, &row.HostFence.ActorID, &row.HostFence.Permission,
		&row.HostFence.AuthorizationKind, &row.HostFence.RuntimeGeneration, &row.HostFence.RuntimeInstanceID,
		&row.HostFence.ControlVersion, &row.HostFence.GrantFingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentInstanceRow{}, agentStopUnconfirmed()
	}
	if err != nil {
		return agentInstanceRow{}, fmt.Errorf("lock agent instance: %w", err)
	}
	if err := tx.QueryRow(ctx, `SELECT initiator_id FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, ownerID).
		Scan(&row.Fence.InitiatorID); err != nil {
		return agentInstanceRow{}, fmt.Errorf("read agent task initiator: %w", err)
	}
	return row, nil
}

// lockAgentHostTarget takes the Host lock before lockAuthorizedAgentTask locks
// the Task row. Host control and member-permission changes also lock Host first,
// so this ordering makes startup/renewal serialize with revocation.
func lockAgentHostTarget(ctx context.Context, tx pgx.Tx, taskID, ownerID string) (agentHostTarget, error) {
	var result agentHostTarget
	err := tx.QueryRow(ctx, `SELECT COALESCE(host_id,''),initiator_id FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, ownerID).
		Scan(&result.TaskHostID, &result.TaskActorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentHostTarget{}, agentStopUnconfirmed()
	}
	if err != nil {
		return agentHostTarget{}, fmt.Errorf("read Agent task Host target: %w", err)
	}
	if result.TaskHostID == "" {
		return result, nil
	}
	return lockAgentHostTargetIdentity(ctx, tx, taskID, ownerID, result.TaskHostID, result.TaskActorID)
}

// Callers first lock the observed Host owner's execution gate, then use the
// observed Host and actor here. They must recheck the Task identity once its
// row is locked; a concurrent target change must not switch the locked Host.
func lockAgentHostTargetIdentity(ctx context.Context, tx pgx.Tx, taskID, ownerID, hostID, actorID string) (agentHostTarget, error) {
	result := agentHostTarget{TaskHostID: hostID, TaskActorID: actorID}
	if hostID == "" {
		return result, nil
	}
	hostLock := " FOR SHARE OF h"
	if permit := projectWorkspaceTaskPermit(ctx, taskID, result.TaskActorID); permit != nil &&
		permit.taskOwnerID == ownerID && permit.grantHostID == result.TaskHostID {
		// The outer Workspace guard holds this Host lock for the lifetime of
		// the nested call. Reacquiring it behind a queued revocation can deadlock.
		hostLock = ""
	}
	err := tx.QueryRow(ctx, `SELECT h.owner_id,h.state,h.control_state,h.deleted_at IS NOT NULL,
h.runtime_generation,COALESCE(h.runtime_instance_id,''),h.control_version,
hm.host_id IS NOT NULL,COALESCE(hm.grant_version::text,''),COALESCE(hm.created_at,'epoch'::timestamptz),
COALESCE(hm.permissions,'{}'::text[])
FROM hosts h
LEFT JOIN host_members hm ON hm.host_id=h.id AND hm.owner_id=h.owner_id AND hm.user_id=$2 AND hm.revoked_at IS NULL
WHERE h.id=$1`+hostLock, result.TaskHostID, result.TaskActorID).Scan(&result.OwnerID, &result.State,
		&result.ControlState, &result.Deleted, &result.RuntimeGeneration, &result.RuntimeInstanceID, &result.ControlVersion,
		&result.HasActiveGrant, &result.GrantVersion, &result.GrantCreatedAt, &result.GrantPermissions)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentHostTarget{}, agentHostFenceUnavailable()
	}
	if err != nil {
		return agentHostTarget{}, fmt.Errorf("lock Agent task Host: %w", err)
	}
	return result, nil
}

func captureAgentHostFence(ctx context.Context, tx pgx.Tx, current agentTaskLock, target agentHostTarget) (agentHostFence, error) {
	if current.HostID != target.TaskHostID || current.InitiatorID != target.TaskActorID {
		return agentHostFence{}, agentHostFenceUnavailable()
	}
	if current.HostID == "" {
		return agentHostFence{}, nil
	}
	if target.OwnerID == "" || target.State != string(host.StateRunning) || target.ControlState != string(host.ControlAI) ||
		target.Deleted || target.RuntimeGeneration <= 0 || target.RuntimeInstanceID == "" {
		return agentHostFence{}, agentHostFenceUnavailable()
	}
	allowed, err := hasControlPermission(ctx, tx, current.HostID, target.OwnerID, current.InitiatorID, string(host.PermissionDesktopControl))
	if err != nil {
		return agentHostFence{}, fmt.Errorf("authorize Agent task Host control: %w", err)
	}
	if !allowed {
		return agentHostFence{}, agentHostFenceUnavailable()
	}
	authorizationKind := "member"
	grantFingerprint := ""
	if current.InitiatorID == target.OwnerID {
		authorizationKind = "owner"
	} else {
		if !target.HasActiveGrant || target.GrantVersion == "" || target.GrantCreatedAt.IsZero() || len(target.GrantPermissions) == 0 {
			return agentHostFence{}, agentHostFenceUnavailable()
		}
		grantFingerprint = fingerprintHostMemberGrant(target.GrantCreatedAt, target.GrantVersion, target.GrantPermissions)
	}
	return agentHostFence{
		HostID: current.HostID, OwnerID: target.OwnerID, ActorID: current.InitiatorID,
		Permission: string(host.PermissionDesktopControl), AuthorizationKind: authorizationKind,
		GrantFingerprint:  grantFingerprint,
		RuntimeGeneration: target.RuntimeGeneration, RuntimeInstanceID: target.RuntimeInstanceID,
		ControlVersion: target.ControlVersion,
	}, nil
}

// requireTaskAgentHostAuthorizationFence validates the current Host permit and,
// when the Task is bound to an Agent instance, proves that instance still holds
// the exact Host grant snapshot captured at startup. A revoke followed by a
// regrant therefore cannot revive an old Agent.
func requireTaskAgentHostAuthorizationFence(ctx context.Context, tx pgx.Tx, taskID, taskOwnerID string,
	operation task.Operation) error {
	target, err := lockAgentHostTarget(ctx, tx, taskID, taskOwnerID)
	if err != nil {
		return err
	}
	if target.TaskHostID == "" || target.TaskHostID != operation.HostID {
		return agentHostFenceUnavailable()
	}
	current := agentTaskLock{HostID: target.TaskHostID, InitiatorID: target.TaskActorID}
	currentHostFence, err := captureAgentHostFence(ctx, tx, current, target)
	if err != nil {
		return err
	}
	if operation.RuntimeGeneration != target.RuntimeGeneration || operation.RuntimeInstanceID != target.RuntimeInstanceID ||
		operation.ControlVersion != target.ControlVersion {
		return agentHostFenceUnavailable()
	}

	var agentInstanceID string
	var agentGeneration int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(agent_instance_id,''),agent_generation
FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, taskOwnerID).Scan(&agentInstanceID, &agentGeneration); err != nil {
		return fmt.Errorf("read Task Agent Host binding: %w", err)
	}
	if agentInstanceID == "" {
		return nil
	}
	if agentGeneration < 1 {
		return agentHostFenceUnavailable()
	}

	var stored agentHostFence
	var state string
	var leaseActive bool
	err = tx.QueryRow(ctx, `SELECT COALESCE(host_id,''),COALESCE(host_owner_id,''),COALESCE(host_actor_id,''),
COALESCE(host_permission,''),COALESCE(host_authorization_kind,''),COALESCE(host_grant_fingerprint,''),
COALESCE(host_runtime_generation,0),COALESCE(host_runtime_instance_id,''),COALESCE(host_control_version,0),
state,lease_expires_at>clock_timestamp()
FROM task_agent_instances
WHERE task_id=$1 AND owner_id=$2 AND agent_instance_id=$3 AND agent_generation=$4`,
		taskID, taskOwnerID, agentInstanceID, agentGeneration).Scan(&stored.HostID, &stored.OwnerID, &stored.ActorID,
		&stored.Permission, &stored.AuthorizationKind, &stored.GrantFingerprint, &stored.RuntimeGeneration,
		&stored.RuntimeInstanceID, &stored.ControlVersion, &state, &leaseActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return agentHostFenceUnavailable()
	}
	if err != nil {
		return fmt.Errorf("read Task Agent Host authorization snapshot: %w", err)
	}
	verifiedTaskOutput := isTaskWorkspaceVerificationOperation(operation)
	activeAgent := state == "active" && leaseActive
	stoppedAgentVerification := verifiedTaskOutput && state == "stopped"
	if (!activeAgent && !stoppedAgentVerification) || !sameAgentHostFence(stored, currentHostFence) {
		return agentHostFenceUnavailable()
	}
	return nil
}

func isTaskWorkspaceVerificationOperation(operation task.Operation) bool {
	parts := strings.Split(operation.Key, ":")
	if len(parts) != 5 || parts[0] != "agent" || parts[1] != "verify" ||
		!domain.ValidStepID(parts[2]) || !domain.ValidWorkspaceID(parts[3]) ||
		operation.WorkspaceID != parts[3] {
		return false
	}
	bindingVersion, err := strconv.ParseInt(parts[4], 10, 64)
	return err == nil && bindingVersion > 0 && operation.BindingVersion == bindingVersion
}

func sameAgentHostFence(left, right agentHostFence) bool {
	return left == right
}

func sameAgentHostGrant(left, right agentHostFence) bool {
	return left.HostID == right.HostID && left.OwnerID == right.OwnerID && left.ActorID == right.ActorID &&
		left.Permission == right.Permission && left.AuthorizationKind == right.AuthorizationKind &&
		left.GrantFingerprint == right.GrantFingerprint
}

func hostFenceValue(fence agentHostFence, value func(agentHostFence) any) any {
	if fence.HostID == "" {
		return nil
	}
	return value(fence)
}

func hostGrantFingerprintValue(fence agentHostFence) any {
	if fence.HostID == "" || fence.AuthorizationKind != "member" {
		return nil
	}
	return fence.GrantFingerprint
}

func fingerprintHostMemberGrant(createdAt time.Time, grantVersion string, permissions []string) string {
	canonicalPermissions := append([]string(nil), permissions...)
	sort.Strings(canonicalPermissions)
	payload, _ := json.Marshal(struct {
		CreatedAt    string   `json:"created_at"`
		GrantVersion string   `json:"grant_version"`
		Permissions  []string `json:"permissions"`
	}{CreatedAt: createdAt.UTC().Format(time.RFC3339Nano), GrantVersion: grantVersion, Permissions: canonicalPermissions})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func agentHostFenceUnavailable() error {
	err := domain.NewError(domain.CodeInvalidState, "Host runtime or control authorization changed")
	err.Details["reason"] = "control_changed"
	return err
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
