package postgres

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/identity"
)

func (s *Store) StartAccountAgentRuntime(ctx context.Context, request app.AccountAgentRuntimeStart) (app.AccountAgentRuntime, bool, error) {
	if err := request.Validate(); err != nil {
		return app.AccountAgentRuntime{}, false, err
	}
	var result app.AccountAgentRuntime
	var replayed bool
	nonceDigest := sha256.Sum256(request.SupervisorNonce)
	nonceSHA256 := hex.EncodeToString(nonceDigest[:])
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		gate, err := lockOwnerExecutionControl(ctx, tx, request.OwnerID, true)
		if err != nil {
			return err
		}
		if gate.state != "enabled" {
			return ownerExecutionDisabledError(gate)
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR SHARE`, request.OwnerID).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "account runtime owner is unavailable")
		} else if err != nil {
			return fmt.Errorf("lock account runtime owner: %w", err)
		}
		if status != string(identity.StatusApproved) {
			return domain.NewError(domain.CodeUnauthenticated, "account runtime owner is unavailable")
		}
		serviceInstanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		current, err := loadAccountAgentRuntime(ctx, tx, serviceInstanceID, request.OwnerID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return insertAccountAgentRuntime(ctx, tx, request, gate.epoch, serviceInstanceID, nonceSHA256, &result)
		}
		if err != nil {
			return err
		}
		var leaseActive bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE(lease_expires_at>clock_timestamp(),false) FROM account_agent_runtimes
WHERE service_instance_id=$1::uuid AND owner_id=$2`, serviceInstanceID, request.OwnerID).Scan(&leaseActive); err != nil {
			return fmt.Errorf("check account Agent Runtime lease: %w", err)
		}
		if current.Fence.RuntimeID != request.RuntimeID || current.Fence.LeaseOwner != request.LeaseOwner ||
			current.Fence.ExecutionEpoch != gate.epoch || current.MaxConcurrentTasks != request.MaxConcurrentTasks ||
			(current.State != app.AccountAgentRuntimeStarting && current.State != app.AccountAgentRuntimeRunning) ||
			!leaseActive || current.NonceSHA256 != nonceSHA256 {
			if current.State == app.AccountAgentRuntimeStopped && nonceSHA256 != current.NonceSHA256 &&
				len(current.GuardianStopAttestationDigest) == sha256.Size &&
				current.Fence.RuntimeGeneration < math.MaxInt64 {
				confirmed, err := accountAgentRuntimeStopConfirmed(ctx, tx, request.OwnerID)
				if err != nil {
					return err
				}
				if !confirmed {
					return domain.NewError(domain.CodeConflict, "account runtime stop proof is unavailable or corrupted")
				}
				return replaceAccountAgentRuntime(ctx, tx, request, gate.epoch, nonceSHA256, current, &result)
			}
			return domain.NewError(domain.CodeConflict, "account runtime must be stopped and Guardian-confirmed before replacement")
		}
		result = current
		replayed = true
		return nil
	})
	return result, replayed, err
}

func insertAccountAgentRuntime(ctx context.Context, tx pgx.Tx, request app.AccountAgentRuntimeStart, executionEpoch int64,
	serviceInstanceID, nonceSHA256 string, result *app.AccountAgentRuntime) error {
	var createdAt, expiresAt, updatedAt time.Time
	err := tx.QueryRow(ctx, `INSERT INTO account_agent_runtimes
(service_instance_id,owner_id,runtime_id,runtime_generation,state,lease_owner,lease_version,execution_epoch,
 lease_expires_at,max_concurrent_tasks,supervisor_nonce_sha256)
VALUES ($1::uuid,$2,$3,1,'starting',$4,1,$5,clock_timestamp()+($6::bigint*interval '1 millisecond'),$7,$8)
RETURNING lease_expires_at,control_version,updated_at,created_at`,
		serviceInstanceID, request.OwnerID, request.RuntimeID, request.LeaseOwner, executionEpoch,
		request.LeaseDuration.Milliseconds(), request.MaxConcurrentTasks, nonceSHA256).Scan(&expiresAt, &result.ControlVersion, &updatedAt, &createdAt)
	if err != nil {
		return fmt.Errorf("create account Agent Runtime lease: %w", err)
	}
	result.Fence = app.AccountAgentRuntimeFence{ServiceInstanceID: serviceInstanceID, OwnerID: request.OwnerID,
		RuntimeID: request.RuntimeID, RuntimeGeneration: 1, LeaseOwner: request.LeaseOwner, LeaseVersion: 1, ExecutionEpoch: executionEpoch}
	result.State, result.LeaseExpiresAt, result.MaxConcurrentTasks, result.RuntimeSequence, result.UpdatedAt =
		app.AccountAgentRuntimeStarting, expiresAt, request.MaxConcurrentTasks, 0, updatedAt
	result.NonceSHA256 = nonceSHA256
	return nil
}

func replaceAccountAgentRuntime(ctx context.Context, tx pgx.Tx, request app.AccountAgentRuntimeStart, executionEpoch int64,
	nonceSHA256 string, current app.AccountAgentRuntime, result *app.AccountAgentRuntime) error {
	if current.Fence.RuntimeGeneration == math.MaxInt64 || current.ControlVersion == math.MaxInt64 {
		return domain.NewError(domain.CodeInvalidState, "account runtime generation is exhausted")
	}
	var expiresAt, updatedAt time.Time
	var generation, controlVersion int64
	err := tx.QueryRow(ctx, `UPDATE account_agent_runtimes SET runtime_id=$3,runtime_generation=runtime_generation+1,
state='starting',lease_owner=$4,lease_version=1,execution_epoch=$5,
lease_expires_at=clock_timestamp()+($6::bigint*interval '1 millisecond'),max_concurrent_tasks=$7,runtime_sequence=0,
control_version=control_version+1,supervisor_nonce_sha256=$8,guardian_stop_attestation=NULL,
guardian_stop_attestation_digest=NULL,guardian_stop_acknowledged_at=NULL,stopped_at=NULL,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND state='stopped'
 AND guardian_stop_attestation IS NOT NULL AND guardian_stop_attestation_digest IS NOT NULL
 AND guardian_stop_acknowledged_at IS NOT NULL
RETURNING runtime_generation,lease_expires_at,control_version,updated_at`,
		current.Fence.ServiceInstanceID, request.OwnerID, request.RuntimeID, request.LeaseOwner, executionEpoch,
		request.LeaseDuration.Milliseconds(), request.MaxConcurrentTasks, nonceSHA256).
		Scan(&generation, &expiresAt, &controlVersion, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeConflict, "account runtime must be stopped and Guardian-confirmed before replacement")
	}
	if err != nil {
		return fmt.Errorf("replace Guardian-stopped account Agent Runtime: %w", err)
	}
	result.Fence = app.AccountAgentRuntimeFence{ServiceInstanceID: current.Fence.ServiceInstanceID, OwnerID: request.OwnerID,
		RuntimeID: request.RuntimeID, RuntimeGeneration: generation, LeaseOwner: request.LeaseOwner, LeaseVersion: 1, ExecutionEpoch: executionEpoch}
	result.State, result.LeaseExpiresAt, result.MaxConcurrentTasks = app.AccountAgentRuntimeStarting, expiresAt, request.MaxConcurrentTasks
	result.RuntimeSequence, result.ControlVersion, result.NonceSHA256, result.UpdatedAt = 0, controlVersion, nonceSHA256, updatedAt
	return nil
}

func (s *Store) MarkAccountAgentRuntimeReady(ctx context.Context, fence app.AccountAgentRuntimeFence) (app.AccountAgentRuntime, error) {
	if err := fence.Validate(); err != nil {
		return app.AccountAgentRuntime{}, err
	}
	var result app.AccountAgentRuntime
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		gate, err := lockOwnerExecutionControl(ctx, tx, fence.OwnerID, false)
		if err != nil {
			return err
		}
		if gate.state != "enabled" || gate.epoch != fence.ExecutionEpoch {
			return ownerExecutionDisabledError(gate)
		}
		current, err := lockAccountAgentRuntime(ctx, tx, fence)
		if err != nil {
			return err
		}
		if current.State == app.AccountAgentRuntimeRunning {
			var leaseActive bool
			if err := tx.QueryRow(ctx, `SELECT lease_expires_at>clock_timestamp() FROM account_agent_runtimes
WHERE service_instance_id=$1::uuid AND owner_id=$2`, fence.ServiceInstanceID, fence.OwnerID).Scan(&leaseActive); err != nil {
				return fmt.Errorf("check account Agent Runtime lease: %w", err)
			}
			if !leaseActive {
				return accountAgentRuntimeFenceError()
			}
			result = current
			return nil
		}
		if current.State != app.AccountAgentRuntimeStarting {
			return accountAgentRuntimeFenceError()
		}
		if err := tx.QueryRow(ctx, `UPDATE account_agent_runtimes SET state='running',updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3 AND runtime_generation=$4
 AND lease_owner=$5 AND lease_version=$6 AND execution_epoch=$7 AND state='starting' AND lease_expires_at>clock_timestamp()
RETURNING lease_expires_at,max_concurrent_tasks,runtime_sequence,control_version,updated_at`,
			fence.ServiceInstanceID, fence.OwnerID, fence.RuntimeID, fence.RuntimeGeneration, fence.LeaseOwner,
			fence.LeaseVersion, fence.ExecutionEpoch).Scan(&current.LeaseExpiresAt, &current.MaxConcurrentTasks,
			&current.RuntimeSequence, &current.ControlVersion, &current.UpdatedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return accountAgentRuntimeFenceError()
			}
			return fmt.Errorf("mark account Agent Runtime ready: %w", err)
		}
		current.State = app.AccountAgentRuntimeRunning
		result = current
		return nil
	})
	return result, err
}

func (s *Store) RenewAccountAgentRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence, duration time.Duration) (app.AccountAgentRuntime, error) {
	if err := fence.Validate(); err != nil || duration < time.Millisecond || duration > app.MaxAgentLease {
		return app.AccountAgentRuntime{}, domain.InvalidArgument("account_runtime", "account runtime fence or lease duration is invalid")
	}
	var result app.AccountAgentRuntime
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		gate, err := lockOwnerExecutionControl(ctx, tx, fence.OwnerID, false)
		if err != nil {
			return err
		}
		if gate.state != "enabled" || gate.epoch != fence.ExecutionEpoch {
			return ownerExecutionDisabledError(gate)
		}
		current, err := lockAccountAgentRuntime(ctx, tx, fence)
		if err != nil {
			return err
		}
		if current.State != app.AccountAgentRuntimeRunning {
			return accountAgentRuntimeFenceError()
		}
		err = tx.QueryRow(ctx, `UPDATE account_agent_runtimes SET
 lease_expires_at=clock_timestamp()+($8::bigint*interval '1 millisecond'),updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3 AND runtime_generation=$4
 AND lease_owner=$5 AND lease_version=$6 AND execution_epoch=$7 AND state='running' AND lease_expires_at>clock_timestamp()
			RETURNING lease_expires_at,updated_at`,
			fence.ServiceInstanceID, fence.OwnerID, fence.RuntimeID, fence.RuntimeGeneration, fence.LeaseOwner,
			fence.LeaseVersion, fence.ExecutionEpoch, duration.Milliseconds()).Scan(&current.LeaseExpiresAt, &current.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return accountAgentRuntimeFenceError()
		}
		if err != nil {
			return fmt.Errorf("renew account Agent Runtime lease: %w", err)
		}
		current.State = app.AccountAgentRuntimeRunning
		result = current
		return nil
	})
	return result, err
}

func (s *Store) GetAccountAgentRuntime(ctx context.Context, ownerID string) (app.AccountAgentRuntime, bool, error) {
	if !domain.ValidUserID(ownerID) {
		return app.AccountAgentRuntime{}, false, domain.InvalidArgument("owner_id", "account Runtime owner is invalid")
	}
	var result app.AccountAgentRuntime
	found := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		serviceInstanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		result, err = loadAccountAgentRuntime(ctx, tx, serviceInstanceID, ownerID, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read account Agent Runtime: %w", err)
		}
		found = true
		return nil
	})
	return result, found, err
}

func (s *Store) AccountAgentRuntimeStopIdentity(ctx context.Context, ownerID string) (app.AccountAgentRuntimeStopIdentity, error) {
	runtime, found, err := s.GetAccountAgentRuntime(ctx, ownerID)
	if err != nil {
		return app.AccountAgentRuntimeStopIdentity{}, err
	}
	if !found || (runtime.State != app.AccountAgentRuntimeStarting && runtime.State != app.AccountAgentRuntimeRunning &&
		runtime.State != app.AccountAgentRuntimeStopping && runtime.State != app.AccountAgentRuntimeFailed) ||
		!isLowerHexRuntimeNonce(runtime.NonceSHA256) || runtime.LeaseExpiresAt.IsZero() {
		return app.AccountAgentRuntimeStopIdentity{}, domain.NewError(domain.CodeInvalidState, "account Runtime stop identity is unavailable")
	}
	return app.AccountAgentRuntimeStopIdentity{Fence: runtime.Fence, NonceSHA256: runtime.NonceSHA256,
		ExpiresAt: runtime.LeaseExpiresAt}, nil
}

func (s *Store) ListAccountAgentRuntimeStopCandidates(ctx context.Context, limit int) ([]app.AccountAgentRuntimeStopIdentity, error) {
	if ctx == nil || limit < 1 || limit > app.MaxAccountAgentRuntimeStopCandidates {
		return nil, domain.InvalidArgument("account_runtime", "account Runtime stop candidate query is invalid")
	}
	candidates := make([]app.AccountAgentRuntimeStopIdentity, 0)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		serviceInstanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT owner_id,runtime_id,runtime_generation,lease_owner,lease_version,execution_epoch,
lease_expires_at,supervisor_nonce_sha256
FROM account_agent_runtimes
WHERE service_instance_id=$1::uuid AND supervisor_nonce_sha256 IS NOT NULL AND lease_expires_at IS NOT NULL
 AND (state='stopping' OR (state IN ('starting','running','failed') AND lease_expires_at<=clock_timestamp()))
ORDER BY CASE WHEN state='stopping' THEN 0 ELSE 1 END,updated_at,owner_id LIMIT $2`, serviceInstanceID, limit)
		if err != nil {
			return fmt.Errorf("list account Agent Runtime stop candidates: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var identity app.AccountAgentRuntimeStopIdentity
			if err := rows.Scan(&identity.Fence.OwnerID, &identity.Fence.RuntimeID, &identity.Fence.RuntimeGeneration,
				&identity.Fence.LeaseOwner, &identity.Fence.LeaseVersion, &identity.Fence.ExecutionEpoch,
				&identity.ExpiresAt, &identity.NonceSHA256); err != nil {
				return fmt.Errorf("read account Agent Runtime stop candidate: %w", err)
			}
			identity.Fence.ServiceInstanceID = serviceInstanceID
			if err := identity.Fence.Validate(); err != nil || !isLowerHexRuntimeNonce(identity.NonceSHA256) || identity.ExpiresAt.IsZero() {
				return domain.NewError(domain.CodeInvalidState, "account Runtime stop candidate is invalid")
			}
			candidates = append(candidates, identity)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate account Agent Runtime stop candidates: %w", err)
		}
		return nil
	})
	return candidates, err
}

func (s *Store) RequestAccountAgentRuntimeStop(ctx context.Context, fence app.AccountAgentRuntimeFence) (app.AccountAgentRuntime, error) {
	if err := fence.Validate(); err != nil {
		return app.AccountAgentRuntime{}, err
	}
	var result app.AccountAgentRuntime
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		gate, err := lockOwnerExecutionControl(ctx, tx, fence.OwnerID, true)
		if err != nil {
			return err
		}
		current, err := lockAccountAgentRuntime(ctx, tx, fence)
		if err != nil {
			return err
		}
		if current.State == app.AccountAgentRuntimeStopped {
			result = current
			return nil
		}
		if gate.state != execution.StateStopping {
			gate, err = transitionOwnerExecutionControl(ctx, tx, fence.OwnerID, gate, execution.StateStopping)
			if err != nil {
				return err
			}
		}
		if err := requestOwnerAccountAgentRuntimeStop(ctx, tx, fence.OwnerID); err != nil {
			return err
		}
		if err := disableOwnerSchedules(ctx, tx, fence.OwnerID); err != nil {
			return err
		}
		if err := stopAllOwnerTasks(ctx, tx, fence.OwnerID); err != nil {
			return err
		}
		if err := requestOwnerMCPRuntimeCancellations(ctx, tx, fence.OwnerID); err != nil {
			return err
		}
		result, err = loadAccountAgentRuntime(ctx, tx, fence.ServiceInstanceID, fence.OwnerID, false)
		if err != nil {
			return fmt.Errorf("read stopped-intent account Agent Runtime: %w", err)
		}
		return nil
	})
	return result, err
}

func (s *Store) ConfirmAccountAgentRuntimeStopped(ctx context.Context, proof app.VerifiedAccountAgentRuntimeStop) (bool, error) {
	attestation, encoded := proof.Value()
	if err := validateRuntimeStopProofForStore(attestation, encoded); err != nil {
		return false, err
	}
	nonceHash, err := hex.DecodeString(attestation.NonceSHA256)
	if err != nil || hex.EncodeToString(nonceHash) != attestation.NonceSHA256 {
		return false, domain.InvalidArgument("attestation", "Guardian Runtime nonce digest is invalid")
	}
	attestationHash := sha256.Sum256(encoded)
	var replayed bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		gate, err := lockOwnerExecutionControl(ctx, tx, attestation.Fence.OwnerID, true)
		if err != nil {
			return err
		}
		current, err := lockAccountAgentRuntime(ctx, tx, attestation.Fence)
		if err != nil {
			return err
		}
		if current.NonceSHA256 != attestation.NonceSHA256 {
			return domain.NewError(domain.CodeUnauthenticated, "Guardian Runtime stop proof does not match the current nonce")
		}
		if current.State == app.AccountAgentRuntimeStopped {
			var storedProof, storedDigest []byte
			if err := tx.QueryRow(ctx, `SELECT guardian_stop_attestation,guardian_stop_attestation_digest FROM account_agent_runtimes
WHERE service_instance_id=$1::uuid AND owner_id=$2`, attestation.Fence.ServiceInstanceID, attestation.Fence.OwnerID).Scan(&storedProof, &storedDigest); err != nil {
				return fmt.Errorf("read stored Guardian Runtime proof: %w", err)
			}
			if !bytes.Equal(storedDigest, attestationHash[:]) {
				var previous app.AccountAgentRuntimeStopAttestation
				previousDigest := sha256.Sum256(storedProof)
				if !attestation.StartAborted || !bytes.Equal(storedDigest, previousDigest[:]) || json.Unmarshal(storedProof, &previous) != nil ||
					!previous.StartAborted || previous.Fence != attestation.Fence || previous.NonceSHA256 != attestation.NonceSHA256 ||
					previous.ContainerName != attestation.ContainerName {
					return domain.NewError(domain.CodeIdempotencyConflict, "Guardian Runtime stop proof was already recorded differently")
				}
			}
			replayed = true
			return nil
		}
		if gate.state == execution.StateEnabled {
			// A crashed Portal can recover only after this exact Runtime lease expires.
			var leaseExpired bool
			err := tx.QueryRow(ctx, `SELECT COALESCE(lease_expires_at<=clock_timestamp(),false) FROM account_agent_runtimes
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3 AND runtime_generation=$4
 AND lease_owner=$5 AND lease_version=$6 AND execution_epoch=$7`, attestation.Fence.ServiceInstanceID,
				attestation.Fence.OwnerID, attestation.Fence.RuntimeID, attestation.Fence.RuntimeGeneration,
				attestation.Fence.LeaseOwner, attestation.Fence.LeaseVersion, attestation.Fence.ExecutionEpoch).Scan(&leaseExpired)
			if errors.Is(err, pgx.ErrNoRows) {
				return accountAgentRuntimeFenceError()
			}
			if err != nil {
				return fmt.Errorf("check account Runtime lease before crash recovery: %w", err)
			}
			if !leaseExpired {
				return domain.NewError(domain.CodeConflict, "active account Runtime lease must expire before crash recovery")
			}
		}
		if current.State != app.AccountAgentRuntimeStarting && current.State != app.AccountAgentRuntimeRunning &&
			current.State != app.AccountAgentRuntimeStopping && current.State != app.AccountAgentRuntimeFailed {
			return domain.NewError(domain.CodeConflict, "account Runtime is not awaiting Guardian stop proof")
		}
		if attestation.StartAborted && (current.State != app.AccountAgentRuntimeStarting && current.State != app.AccountAgentRuntimeStopping ||
			current.RuntimeSequence != 0) {
			return domain.NewError(domain.CodeConflict, "aborted account Runtime start has already admitted work")
		}
		if current.ControlVersion == math.MaxInt64 {
			return domain.NewError(domain.CodeInvalidState, "account Runtime control version is exhausted")
		}
		rows, err := tx.Query(ctx, `UPDATE task_agent_instances SET state='stopped',guardian_stop_attestation=$5,
guardian_stop_attestation_digest=$6,stopped_at=COALESCE(stopped_at,clock_timestamp()),updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND account_runtime_id=$3 AND account_runtime_generation=$4 AND state='active'
RETURNING task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,supervisor_id,lease_owner,
lease_version,auth_version,engine_version,profile_version`,
			attestation.Fence.ServiceInstanceID, attestation.Fence.OwnerID, attestation.Fence.RuntimeID,
			attestation.Fence.RuntimeGeneration, encoded, attestationHash[:])
		if err != nil {
			return fmt.Errorf("settle Task Agents after Guardian Runtime destruction: %w", err)
		}
		stoppedAgents := make([]app.AgentInstanceFence, 0)
		for rows.Next() {
			var fence app.AgentInstanceFence
			if err := rows.Scan(&fence.TaskID, &fence.OwnerID, &fence.AgentInstanceID, &fence.AgentGeneration,
				&fence.AgentSessionID, &fence.SupervisorID, &fence.LeaseOwner, &fence.LeaseVersion,
				&fence.AuthorizationVersion, &fence.EngineVersion, &fence.ProfileVersion); err != nil {
				rows.Close()
				return fmt.Errorf("read Task Agent settled by Runtime proof: %w", err)
			}
			stoppedAgents = append(stoppedAgents, fence)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate Task Agents settled by Runtime proof: %w", err)
		}
		rows.Close()
		if attestation.StartAborted && len(stoppedAgents) != 0 {
			return domain.NewError(domain.CodeConflict, "aborted account Runtime start has active Task Agents")
		}
		for _, fence := range stoppedAgents {
			if err := settleStoppedAgentChildSessions(ctx, tx, fence); err != nil {
				return err
			}
		}
		command, err := tx.Exec(ctx, `UPDATE account_agent_runtimes SET state='stopped',lease_expires_at=NULL,
guardian_stop_attestation=$3,guardian_stop_attestation_digest=$4,stopped_at=clock_timestamp(),
	control_version=control_version+1,guardian_stop_acknowledged_at=CASE WHEN $11 THEN clock_timestamp() ELSE NULL END,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$5 AND runtime_generation=$6
 AND lease_owner=$7 AND lease_version=$8 AND execution_epoch=$9 AND supervisor_nonce_sha256=$10
 AND state IN ('starting','running','stopping','failed') AND control_version<9223372036854775807`,
			attestation.Fence.ServiceInstanceID, attestation.Fence.OwnerID, encoded, attestationHash[:],
			attestation.Fence.RuntimeID, attestation.Fence.RuntimeGeneration, attestation.Fence.LeaseOwner,
			attestation.Fence.LeaseVersion, attestation.Fence.ExecutionEpoch, attestation.NonceSHA256, attestation.StartAborted)
		if err != nil {
			return fmt.Errorf("persist verified Guardian Runtime stop proof: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "account Runtime changed while confirming Guardian stop")
		}
		return nil
	})
	return replayed, err
}

func (s *Store) ListPendingAccountAgentRuntimeStopAttestations(ctx context.Context, limit int) ([]app.AccountAgentRuntimeStopAttestation, error) {
	if ctx == nil || limit < 1 || limit > app.MaxPendingAccountAgentRuntimeStopAttestations {
		return nil, domain.InvalidArgument("attestation", "pending Guardian Runtime stop query is invalid")
	}
	attestations := make([]app.AccountAgentRuntimeStopAttestation, 0)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		serviceInstanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT service_instance_id::text,owner_id,runtime_id,runtime_generation,lease_owner,
lease_version,execution_epoch,supervisor_nonce_sha256,guardian_stop_attestation,guardian_stop_attestation_digest
FROM account_agent_runtimes
WHERE service_instance_id=$1::uuid AND state='stopped' AND guardian_stop_attestation IS NOT NULL
 AND guardian_stop_acknowledged_at IS NULL
ORDER BY stopped_at,owner_id LIMIT $2`, serviceInstanceID, limit)
		if err != nil {
			return fmt.Errorf("list pending Guardian Runtime stop acknowledgements: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var storedServiceInstanceID, ownerID, runtimeID, leaseOwner, nonceSHA256 string
			var runtimeGeneration, leaseVersion, executionEpoch int64
			var encoded, storedDigest []byte
			if err := rows.Scan(&storedServiceInstanceID, &ownerID, &runtimeID, &runtimeGeneration, &leaseOwner,
				&leaseVersion, &executionEpoch, &nonceSHA256, &encoded, &storedDigest); err != nil {
				return fmt.Errorf("read pending Guardian Runtime stop acknowledgement: %w", err)
			}
			var attestation app.AccountAgentRuntimeStopAttestation
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&attestation); err != nil {
				return domain.NewError(domain.CodeInvalidState, "stored Guardian Runtime stop attestation is invalid")
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				return domain.NewError(domain.CodeInvalidState, "stored Guardian Runtime stop attestation has trailing data")
			}
			canonical, err := json.Marshal(attestation)
			if err != nil || !bytes.Equal(canonical, encoded) {
				return domain.NewError(domain.CodeInvalidState, "stored Guardian Runtime stop attestation is not canonical")
			}
			digest := sha256.Sum256(encoded)
			if validateRuntimeStopProofForStore(attestation, encoded) != nil || !bytes.Equal(storedDigest, digest[:]) ||
				storedServiceInstanceID != serviceInstanceID || attestation.Fence.ServiceInstanceID != serviceInstanceID ||
				attestation.Fence.OwnerID != ownerID || attestation.Fence.RuntimeID != runtimeID ||
				attestation.Fence.RuntimeGeneration != runtimeGeneration || attestation.Fence.LeaseOwner != leaseOwner ||
				attestation.Fence.LeaseVersion != leaseVersion || attestation.Fence.ExecutionEpoch != executionEpoch ||
				attestation.NonceSHA256 != nonceSHA256 {
				return domain.NewError(domain.CodeInvalidState, "stored Guardian Runtime stop attestation does not match its Runtime fence")
			}
			attestations = append(attestations, attestation)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate pending Guardian Runtime stop acknowledgements: %w", err)
		}
		return nil
	})
	return attestations, err
}

func (s *Store) MarkAccountAgentRuntimeStopAttestationAcknowledged(ctx context.Context,
	proof app.VerifiedAccountAgentRuntimeStop) (bool, error) {
	if ctx == nil {
		return false, domain.InvalidArgument("context", "Guardian Runtime stop acknowledgement context is required")
	}
	attestation, encoded := proof.Value()
	if err := validateRuntimeStopProofForStore(attestation, encoded); err != nil {
		return false, err
	}
	proofDigest := sha256.Sum256(encoded)
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		command, err := tx.Exec(ctx, `UPDATE account_agent_runtimes SET guardian_stop_acknowledged_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3 AND runtime_generation=$4
 AND lease_owner=$5 AND lease_version=$6 AND execution_epoch=$7 AND supervisor_nonce_sha256=$8
 AND state='stopped' AND guardian_stop_attestation=$9 AND guardian_stop_attestation_digest=$10
 AND guardian_stop_acknowledged_at IS NULL`, attestation.Fence.ServiceInstanceID, attestation.Fence.OwnerID,
			attestation.Fence.RuntimeID, attestation.Fence.RuntimeGeneration, attestation.Fence.LeaseOwner,
			attestation.Fence.LeaseVersion, attestation.Fence.ExecutionEpoch, attestation.NonceSHA256, encoded, proofDigest[:])
		if err != nil {
			return fmt.Errorf("mark Guardian Runtime stop proof acknowledged: %w", err)
		}
		if command.RowsAffected() == 1 {
			return nil
		}
		var acknowledgedAt time.Time
		err = tx.QueryRow(ctx, `SELECT guardian_stop_acknowledged_at FROM account_agent_runtimes
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3 AND runtime_generation=$4
 AND lease_owner=$5 AND lease_version=$6 AND execution_epoch=$7 AND supervisor_nonce_sha256=$8
 AND state='stopped' AND guardian_stop_attestation=$9 AND guardian_stop_attestation_digest=$10
 AND guardian_stop_acknowledged_at IS NOT NULL`, attestation.Fence.ServiceInstanceID, attestation.Fence.OwnerID,
			attestation.Fence.RuntimeID, attestation.Fence.RuntimeGeneration, attestation.Fence.LeaseOwner,
			attestation.Fence.LeaseVersion, attestation.Fence.ExecutionEpoch, attestation.NonceSHA256, encoded, proofDigest[:]).Scan(&acknowledgedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "Guardian Runtime stop acknowledgement does not match the persisted proof")
		}
		if err != nil {
			return fmt.Errorf("read Guardian Runtime stop acknowledgement: %w", err)
		}
		replayed = true
		return nil
	})
	return replayed, err
}

func requestOwnerAccountAgentRuntimeStop(ctx context.Context, tx pgx.Tx, ownerID string) error {
	serviceInstanceID, err := serviceInstanceIDInTx(ctx, tx)
	if err != nil {
		return err
	}
	current, err := loadAccountAgentRuntime(ctx, tx, serviceInstanceID, ownerID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock account Agent Runtime for stop: %w", err)
	}
	if current.State != app.AccountAgentRuntimeStarting && current.State != app.AccountAgentRuntimeRunning {
		return nil
	}
	if current.ControlVersion == math.MaxInt64 {
		return domain.NewError(domain.CodeInvalidState, "account Runtime control version is exhausted")
	}
	command, err := tx.Exec(ctx, `UPDATE account_agent_runtimes SET state='stopping',control_version=control_version+1,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3 AND runtime_generation=$4
 AND lease_owner=$5 AND lease_version=$6 AND execution_epoch=$7 AND state IN ('starting','running')
 AND control_version<9223372036854775807`, current.Fence.ServiceInstanceID, current.Fence.OwnerID,
		current.Fence.RuntimeID, current.Fence.RuntimeGeneration, current.Fence.LeaseOwner, current.Fence.LeaseVersion,
		current.Fence.ExecutionEpoch)
	if err != nil {
		return fmt.Errorf("persist account Agent Runtime stop intent: %w", err)
	}
	if command.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "account Runtime changed while requesting stop")
	}
	return nil
}

func validateRuntimeStopProofForStore(attestation app.AccountAgentRuntimeStopAttestation, encoded []byte) error {
	validContainerID := attestation.StartAborted && attestation.ContainerID == "" || isLowerHexRuntimeNonce(attestation.ContainerID)
	validExpiry := attestation.StartAborted && attestation.ExpiresAt.IsZero() || !attestation.StartAborted && !attestation.ExpiresAt.IsZero()
	if attestation.Fence.Validate() != nil || !attestation.Destroyed || !validContainerID || attestation.ContainerName == "" || !validExpiry ||
		!isLowerHexRuntimeNonce(attestation.NonceSHA256) || !isLowerHexRuntimeNonce(attestation.ProofSHA256) ||
		attestation.DestroyedAt.IsZero() || len(attestation.Signature) != ed25519.SignatureSize ||
		len(encoded) == 0 || len(encoded) > 16384 {
		return domain.InvalidArgument("attestation", "verified Guardian Runtime stop attestation is required")
	}
	return nil
}

func isLowerHexRuntimeNonce(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value && len(decoded) == sha256.Size
}

func accountAgentRuntimeLeaseActive(ctx context.Context, tx pgx.Tx, fence app.AccountAgentRuntimeFence) (bool, error) {
	var active bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(lease_expires_at>clock_timestamp(),false) FROM account_agent_runtimes
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3 AND runtime_generation=$4
 AND lease_owner=$5 AND lease_version=$6 AND execution_epoch=$7`, fence.ServiceInstanceID, fence.OwnerID,
		fence.RuntimeID, fence.RuntimeGeneration, fence.LeaseOwner, fence.LeaseVersion, fence.ExecutionEpoch).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, accountAgentRuntimeFenceError()
	}
	if err != nil {
		return false, fmt.Errorf("check account Agent Runtime lease: %w", err)
	}
	return active, nil
}

func incrementAccountAgentRuntimeSequence(ctx context.Context, tx pgx.Tx, fence app.AccountAgentRuntimeFence) (int64, error) {
	var sequence int64
	err := tx.QueryRow(ctx, `UPDATE account_agent_runtimes SET runtime_sequence=runtime_sequence+1,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND runtime_id=$3 AND runtime_generation=$4
 AND lease_owner=$5 AND lease_version=$6 AND execution_epoch=$7 AND state='running'
 AND lease_expires_at>clock_timestamp() AND runtime_sequence<9223372036854775807
RETURNING runtime_sequence`, fence.ServiceInstanceID, fence.OwnerID, fence.RuntimeID, fence.RuntimeGeneration,
		fence.LeaseOwner, fence.LeaseVersion, fence.ExecutionEpoch).Scan(&sequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, accountAgentRuntimeFenceError()
	}
	if err != nil {
		return 0, fmt.Errorf("allocate account Agent Runtime sequence: %w", err)
	}
	return sequence, nil
}

func lockCurrentAccountAgentRuntimeForTask(ctx context.Context, tx pgx.Tx, ownerID, serviceInstanceID, runtimeID string,
	runtimeGeneration int64) (app.AccountAgentRuntime, error) {
	currentServiceID, err := serviceInstanceIDInTx(ctx, tx)
	if err != nil {
		return app.AccountAgentRuntime{}, err
	}
	if currentServiceID != serviceInstanceID {
		return app.AccountAgentRuntime{}, accountAgentRuntimeFenceError()
	}
	current, err := loadAccountAgentRuntime(ctx, tx, serviceInstanceID, ownerID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.AccountAgentRuntime{}, accountAgentRuntimeFenceError()
	}
	if err != nil {
		return app.AccountAgentRuntime{}, fmt.Errorf("lock current account Agent Runtime: %w", err)
	}
	leaseActive, err := accountAgentRuntimeLeaseActive(ctx, tx, current.Fence)
	if err != nil {
		return app.AccountAgentRuntime{}, err
	}
	if current.Fence.RuntimeID != runtimeID || current.Fence.RuntimeGeneration != runtimeGeneration ||
		current.State != app.AccountAgentRuntimeRunning || !leaseActive {
		return app.AccountAgentRuntime{}, accountAgentRuntimeFenceError()
	}
	return current, nil
}

func sameAccountAgentRuntimeBinding(instance agentInstanceRow, fence *app.AccountAgentRuntimeFence) bool {
	if fence == nil {
		return instance.RuntimeServiceInstanceID == "" && instance.RuntimeID == "" && instance.RuntimeGeneration == 0 && instance.RuntimeSequence == 0
	}
	return instance.RuntimeServiceInstanceID == fence.ServiceInstanceID && instance.Fence.OwnerID == fence.OwnerID &&
		instance.RuntimeID == fence.RuntimeID && instance.RuntimeGeneration == fence.RuntimeGeneration && instance.RuntimeSequence > 0
}

func copyAccountAgentRuntimeFence(fence *app.AccountAgentRuntimeFence) *app.AccountAgentRuntimeFence {
	if fence == nil {
		return nil
	}
	copy := *fence
	return &copy
}

func serviceInstanceIDInTx(ctx context.Context, tx pgx.Tx) (string, error) {
	var id string
	if err := tx.QueryRow(ctx, `SELECT instance_id::text FROM service_identity WHERE singleton=true`).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
		return "", domain.NewError(domain.CodeInternal, "service instance identity is unavailable")
	} else if err != nil {
		return "", fmt.Errorf("read service instance identity: %w", err)
	}
	return id, nil
}

func loadAccountAgentRuntime(ctx context.Context, tx pgx.Tx, serviceInstanceID, ownerID string, forUpdate bool) (app.AccountAgentRuntime, error) {
	lock := ""
	if forUpdate {
		lock = " FOR UPDATE"
	}
	var result app.AccountAgentRuntime
	var state string
	var expiresAt pgtype.Timestamptz
	err := tx.QueryRow(ctx, `SELECT runtime_id,runtime_generation,state,lease_owner,lease_version,execution_epoch,
lease_expires_at,max_concurrent_tasks,runtime_sequence,control_version,COALESCE(supervisor_nonce_sha256,''),
guardian_stop_attestation_digest,updated_at
FROM account_agent_runtimes WHERE service_instance_id=$1::uuid AND owner_id=$2`+lock, serviceInstanceID, ownerID).Scan(
		&result.Fence.RuntimeID, &result.Fence.RuntimeGeneration, &state, &result.Fence.LeaseOwner,
		&result.Fence.LeaseVersion, &result.Fence.ExecutionEpoch, &expiresAt,
		&result.MaxConcurrentTasks, &result.RuntimeSequence, &result.ControlVersion, &result.NonceSHA256,
		&result.GuardianStopAttestationDigest, &result.UpdatedAt)
	if err != nil {
		return app.AccountAgentRuntime{}, err
	}
	result.Fence.ServiceInstanceID, result.Fence.OwnerID = serviceInstanceID, ownerID
	if expiresAt.Valid {
		result.LeaseExpiresAt = expiresAt.Time
	}
	result.State = app.AccountAgentRuntimeState(state)
	return result, nil
}

func lockAccountAgentRuntime(ctx context.Context, tx pgx.Tx, fence app.AccountAgentRuntimeFence) (app.AccountAgentRuntime, error) {
	serviceInstanceID, err := serviceInstanceIDInTx(ctx, tx)
	if err != nil {
		return app.AccountAgentRuntime{}, err
	}
	if serviceInstanceID != fence.ServiceInstanceID {
		return app.AccountAgentRuntime{}, accountAgentRuntimeFenceError()
	}
	current, err := loadAccountAgentRuntime(ctx, tx, serviceInstanceID, fence.OwnerID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.AccountAgentRuntime{}, accountAgentRuntimeFenceError()
	}
	if err != nil {
		return app.AccountAgentRuntime{}, fmt.Errorf("lock account Agent Runtime: %w", err)
	}
	if current.Fence != fence {
		return app.AccountAgentRuntime{}, accountAgentRuntimeFenceError()
	}
	return current, nil
}

func accountAgentRuntimeFenceError() error {
	return domain.NewError(domain.CodeUnauthenticated, "account runtime lease is unavailable")
}

var _ app.AccountAgentRuntimeStore = (*Store)(nil)

var _ = pgtype.Timestamptz{}
