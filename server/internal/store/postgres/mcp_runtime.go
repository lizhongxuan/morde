package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

const (
	maxMCPRuntimeArgumentsBytes = 32 << 10
	maxMCPRuntimeResultBytes    = 1 << 20
)

const mcpRuntimeOperationSelect = `
SELECT o.operation_id, o.owner_id, o.actor_user_id, o.api_key_id, o.session_id, o.host_id, o.idempotency_key,
       s.runtime_task_id, o.request_hash, o.capability, o.action, o.arguments,
       o.workspace_id, o.binding_version,
       o.runtime_generation, o.runtime_instance_id, o.control_version,
       o.lease_version, o.lease_expires_at, o.status, o.result
FROM mcp_runtime_operations o
JOIN mcp_runtime_sessions s
  ON s.owner_id=o.owner_id AND s.actor_user_id=o.actor_user_id AND s.api_key_id=o.api_key_id AND s.session_id=o.session_id AND s.host_id=o.host_id`

var _ app.MCPRuntimeOperationStore = (*Store)(nil)
var _ app.MCPRuntimeCancellationStore = (*Store)(nil)

func (s *Store) BeginMCPRuntimeOperation(ctx context.Context, request app.BeginMCPRuntimeOperation) (app.MCPRuntimeOperation, bool, error) {
	if err := validateBeginMCPRuntimeOperation(request); err != nil {
		return app.MCPRuntimeOperation{}, false, err
	}
	var result app.MCPRuntimeOperation
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := requireOwnerExecutionEnabled(ctx, tx, request.Key.OwnerID, &request.ExpectedExecutionEpoch); err != nil {
			return err
		}
		if err := requireCurrentMCPAuthorization(ctx, tx, request.Key); err != nil {
			return err
		}
		if err := lockAndValidateMCPHost(ctx, tx, request.Key.OwnerID, request.Key.HostID, request.Fence); err != nil {
			return err
		}

		_, err := tx.Exec(ctx, `
INSERT INTO mcp_runtime_sessions (owner_id, actor_user_id, api_key_id, session_id, host_id, runtime_task_id)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (owner_id, api_key_id, session_id, host_id) DO NOTHING`,
			request.Key.OwnerID, mcpRuntimeActorID(request.Key), request.Key.APIKeyID, request.Key.SessionID, request.Key.HostID, request.NewRuntimeTaskID)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.NewError(domain.CodeConflict, "runtime task id is already assigned to another MCP session")
			}
			return fmt.Errorf("create MCP runtime session: %w", err)
		}
		session, err := lockMCPRuntimeSession(ctx, tx, request.Key)
		if err != nil {
			return err
		}

		existing, err := loadMCPRuntimeOperation(ctx, tx, request.Key, false)
		if err == nil {
			if !bytes.Equal(existing.RequestHash, request.RequestHash) || existing.WorkspaceID != request.WorkspaceID ||
				existing.BindingVersion != request.BindingVersion {
				return idempotencyConflict()
			}
			result, replayed = existing, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read MCP runtime idempotency record: %w", err)
		}
		if session.activeOperationID != "" {
			// A prepared operation has not been dispatched, so an expired lease is safe to cancel.
			settled, err := tx.Exec(ctx, `
UPDATE mcp_runtime_operations
SET status='cancelled', result='null'::jsonb, updated_at=clock_timestamp()
WHERE operation_id=$5 AND owner_id=$1 AND api_key_id=$2 AND session_id=$3 AND host_id=$4
  AND status='prepared' AND lease_expires_at <= clock_timestamp()`,
				request.Key.OwnerID, request.Key.APIKeyID, request.Key.SessionID, request.Key.HostID, session.activeOperationID)
			if err != nil {
				return fmt.Errorf("settle expired prepared MCP runtime operation: %w", err)
			}
			if settled.RowsAffected() == 1 {
				if _, err := tx.Exec(ctx, `
UPDATE mcp_runtime_sessions SET active_operation_id=NULL, updated_at=clock_timestamp()
WHERE owner_id=$1 AND api_key_id=$2 AND session_id=$3 AND host_id=$4 AND active_operation_id=$5`,
					request.Key.OwnerID, request.Key.APIKeyID, request.Key.SessionID, request.Key.HostID, session.activeOperationID); err != nil {
					return fmt.Errorf("clear expired prepared MCP runtime operation: %w", err)
				}
				session.activeOperationID = ""
			}
			if session.activeOperationID != "" {
				return domain.NewError(domain.CodeConflict, "MCP session already has an active runtime operation")
			}
		}
		if session.leaseVersion == math.MaxInt64 {
			return domain.NewError(domain.CodeInvalidState, "MCP runtime lease version is exhausted")
		}

		leaseVersion := session.leaseVersion + 1
		var leaseExpiresAt time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp() + ($1::bigint * interval '1 microsecond')`, request.LeaseDuration.Microseconds()).Scan(&leaseExpiresAt); err != nil {
			return fmt.Errorf("calculate MCP runtime lease expiry: %w", err)
		}
		_, err = tx.Exec(ctx, `
INSERT INTO mcp_runtime_operations (
  operation_id, owner_id, actor_user_id, api_key_id, session_id, host_id, idempotency_key, request_hash,
	workspace_id, binding_version, runtime_generation, runtime_instance_id, control_version, execution_epoch,
	lease_version, lease_expires_at, capability, action, arguments, status
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19::jsonb,'prepared')`,
			request.NewOperationID, request.Key.OwnerID, mcpRuntimeActorID(request.Key), request.Key.APIKeyID, request.Key.SessionID, request.Key.HostID,
			request.Key.IdempotencyKey, request.RequestHash, request.WorkspaceID, request.BindingVersion, request.Fence.RuntimeGeneration,
			request.Fence.RuntimeInstanceID, request.Fence.ControlVersion, request.ExpectedExecutionEpoch,
			leaseVersion, leaseExpiresAt, request.Capability, request.Action, string(request.Arguments))
		if err != nil {
			if isUniqueViolation(err) {
				return domain.NewError(domain.CodeConflict, "MCP runtime operation identity is already in use")
			}
			return fmt.Errorf("insert prepared MCP runtime operation: %w", err)
		}
		_, err = tx.Exec(ctx, `
UPDATE mcp_runtime_sessions
SET lease_version=$5, lease_expires_at=$6, active_operation_id=$7, updated_at=clock_timestamp()
WHERE owner_id=$1 AND api_key_id=$2 AND session_id=$3 AND host_id=$4`,
			request.Key.OwnerID, request.Key.APIKeyID, request.Key.SessionID, request.Key.HostID, leaseVersion, leaseExpiresAt, request.NewOperationID)
		if err != nil {
			return fmt.Errorf("activate MCP runtime operation: %w", err)
		}
		result, err = loadMCPRuntimeOperation(ctx, tx, request.Key, false)
		if err != nil {
			return fmt.Errorf("read prepared MCP runtime operation: %w", err)
		}
		return nil
	})
	return result, replayed, err
}

func (s *Store) MarkMCPRuntimeOperationDispatched(ctx context.Context, key app.MCPRuntimeOperationKey, runtimeTaskID string, fence app.AgentHostFence, leaseVersion int64) (app.MCPRuntimeOperation, error) {
	if err := validateMCPRuntimeKey(key); err != nil {
		return app.MCPRuntimeOperation{}, err
	}
	if !domain.ValidTaskID(runtimeTaskID) || leaseVersion < 1 || !validMCPRuntimeFence(fence) {
		return app.MCPRuntimeOperation{}, domain.InvalidArgument("operation", "MCP runtime dispatch fence is invalid")
	}
	var result app.MCPRuntimeOperation
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var expectedEpoch int64
		if err := tx.QueryRow(ctx, `
SELECT execution_epoch FROM mcp_runtime_operations
WHERE owner_id=$1 AND actor_user_id=$2 AND api_key_id=$3 AND session_id=$4 AND host_id=$5 AND idempotency_key=$6`,
			key.OwnerID, mcpRuntimeActorID(key), key.APIKeyID, key.SessionID, key.HostID, key.IdempotencyKey).Scan(&expectedEpoch); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "MCP runtime operation not found")
		} else if err != nil {
			return fmt.Errorf("read MCP runtime operation epoch: %w", err)
		}
		if _, err := requireOwnerExecutionEnabled(ctx, tx, key.OwnerID, &expectedEpoch); err != nil {
			return err
		}
		if err := requireCurrentMCPAuthorization(ctx, tx, key); err != nil {
			return err
		}
		if err := lockAndValidateMCPHost(ctx, tx, key.OwnerID, key.HostID, fence); err != nil {
			return err
		}
		session, err := lockMCPRuntimeSession(ctx, tx, key)
		if err != nil {
			return err
		}
		result, err = loadMCPRuntimeOperation(ctx, tx, key, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "MCP runtime operation not found")
		}
		if err != nil {
			return fmt.Errorf("lock MCP runtime operation for dispatch: %w", err)
		}
		if result.RuntimeTaskID != runtimeTaskID || result.LeaseVersion != leaseVersion || result.Fence != fence {
			return mcpRuntimeFenceChangedError()
		}
		var cancellationRequested bool
		if err := tx.QueryRow(ctx, `SELECT cancel_requested_at IS NOT NULL FROM mcp_runtime_operations WHERE operation_id=$1`, result.OperationID).Scan(&cancellationRequested); err != nil {
			return fmt.Errorf("check MCP runtime cancellation request before dispatch: %w", err)
		}
		if cancellationRequested {
			return domain.NewError(domain.CodeConflict, "MCP runtime operation cancellation has been requested")
		}
		// A dispatched replay is a read of the exact committed operation. It may
		// arrive after a later operation has become active in this MCP session.
		if result.Status == task.OperationDispatched {
			return nil
		}
		if result.Status != task.OperationPrepared || session.activeOperationID != result.OperationID || session.leaseVersion != result.LeaseVersion {
			return domain.NewError(domain.CodeInvalidState, "MCP runtime operation is no longer prepared and active")
		}
		var leaseStillValid bool
		if err := tx.QueryRow(ctx, `SELECT $1::timestamptz > clock_timestamp()`, result.LeaseExpiresAt).Scan(&leaseStillValid); err != nil {
			return fmt.Errorf("check MCP runtime dispatch lease: %w", err)
		}
		if !leaseStillValid {
			return domain.NewError(domain.CodeInvalidState, "MCP runtime operation lease expired before dispatch")
		}
		_, err = tx.Exec(ctx, `
UPDATE mcp_runtime_operations SET status='dispatched', updated_at=clock_timestamp()
WHERE operation_id=$1 AND status='prepared'`, result.OperationID)
		if err != nil {
			return fmt.Errorf("mark MCP runtime operation dispatched: %w", err)
		}
		result.Status = task.OperationDispatched
		return nil
	})
	return result, err
}

func (s *Store) RenewMCPRuntimeOperationLease(ctx context.Context, key app.MCPRuntimeOperationKey, runtimeTaskID string, fence app.AgentHostFence, executionEpoch, leaseVersion int64, duration time.Duration) (app.MCPRuntimeOperation, error) {
	if err := validateMCPRuntimeKey(key); err != nil {
		return app.MCPRuntimeOperation{}, err
	}
	if !domain.ValidTaskID(runtimeTaskID) || !validMCPRuntimeFence(fence) || executionEpoch < 1 || leaseVersion < 1 || duration.Microseconds() < 1 {
		return app.MCPRuntimeOperation{}, domain.InvalidArgument("lease", "MCP runtime lease request is invalid")
	}
	var result app.MCPRuntimeOperation
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var operationEpoch int64
		if err := tx.QueryRow(ctx, `
SELECT execution_epoch FROM mcp_runtime_operations
WHERE owner_id=$1 AND actor_user_id=$2 AND api_key_id=$3 AND session_id=$4 AND host_id=$5 AND idempotency_key=$6`,
			key.OwnerID, mcpRuntimeActorID(key), key.APIKeyID, key.SessionID, key.HostID, key.IdempotencyKey).Scan(&operationEpoch); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "MCP runtime operation not found")
		} else if err != nil {
			return fmt.Errorf("read MCP runtime operation epoch for lease renewal: %w", err)
		}
		if operationEpoch != executionEpoch {
			return mcpRuntimeFenceChangedError()
		}
		if _, err := requireOwnerExecutionEnabled(ctx, tx, key.OwnerID, &operationEpoch); err != nil {
			return err
		}
		if err := requireCurrentMCPAuthorization(ctx, tx, key); err != nil {
			return err
		}
		if err := lockAndValidateMCPHost(ctx, tx, key.OwnerID, key.HostID, fence); err != nil {
			return err
		}
		session, err := lockMCPRuntimeSession(ctx, tx, key)
		if err != nil {
			return err
		}
		result, err = loadMCPRuntimeOperation(ctx, tx, key, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "MCP runtime operation not found")
		}
		if err != nil {
			return fmt.Errorf("lock MCP runtime operation for lease renewal: %w", err)
		}
		var cancellationRequested bool
		if err := tx.QueryRow(ctx, `SELECT cancel_requested_at IS NOT NULL FROM mcp_runtime_operations WHERE operation_id=$1`, result.OperationID).Scan(&cancellationRequested); err != nil {
			return fmt.Errorf("check MCP runtime cancellation request before lease renewal: %w", err)
		}
		if cancellationRequested {
			return domain.NewError(domain.CodeConflict, "MCP runtime operation cancellation has been requested")
		}
		if result.RuntimeTaskID != runtimeTaskID || result.Fence != fence || result.LeaseVersion != leaseVersion ||
			session.activeOperationID != result.OperationID ||
			session.leaseVersion != leaseVersion || !mcpRuntimeOperationActive(result.Status) {
			return mcpRuntimeFenceChangedError()
		}
		var leaseExpiresAt time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp() + ($1::bigint * interval '1 microsecond')`, duration.Microseconds()).Scan(&leaseExpiresAt); err != nil {
			return fmt.Errorf("calculate renewed MCP runtime lease expiry: %w", err)
		}
		if err := tx.QueryRow(ctx, `
UPDATE mcp_runtime_operations SET lease_expires_at=GREATEST(lease_expires_at, $2), updated_at=clock_timestamp()
WHERE operation_id=$1 RETURNING lease_expires_at`, result.OperationID, leaseExpiresAt).Scan(&leaseExpiresAt); err != nil {
			return fmt.Errorf("renew MCP runtime operation lease: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE mcp_runtime_sessions SET lease_expires_at=$5, updated_at=clock_timestamp()
WHERE owner_id=$1 AND api_key_id=$2 AND session_id=$3 AND host_id=$4`, key.OwnerID, key.APIKeyID, key.SessionID, key.HostID, leaseExpiresAt); err != nil {
			return fmt.Errorf("renew MCP runtime session lease: %w", err)
		}
		result.LeaseExpiresAt = leaseExpiresAt
		return nil
	})
	return result, err
}

func (s *Store) SettleMCPRuntimeOperation(ctx context.Context, key app.MCPRuntimeOperationKey, runtimeTaskID string, status task.OperationStatus, rawResult json.RawMessage) (app.MCPRuntimeOperation, error) {
	if err := validateMCPRuntimeKey(key); err != nil {
		return app.MCPRuntimeOperation{}, err
	}
	if !domain.ValidTaskID(runtimeTaskID) || !validMCPRuntimeSettleStatus(status) || !validMCPRuntimeResult(rawResult) {
		return app.MCPRuntimeOperation{}, domain.InvalidArgument("operation", "MCP runtime settlement is invalid")
	}
	if len(rawResult) == 0 {
		rawResult = json.RawMessage("null")
	}
	var result app.MCPRuntimeOperation
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		session, err := lockMCPRuntimeSession(ctx, tx, key)
		if err != nil {
			return err
		}
		if session.runtimeTaskID != runtimeTaskID {
			return mcpRuntimeFenceChangedError()
		}
		result, err = loadMCPRuntimeOperation(ctx, tx, key, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "MCP runtime operation not found")
		}
		if err != nil {
			return fmt.Errorf("lock MCP runtime operation for settlement: %w", err)
		}
		if result.RuntimeTaskID != runtimeTaskID {
			return mcpRuntimeFenceChangedError()
		}
		if result.Status == status {
			var sameResult bool
			if err := tx.QueryRow(ctx, `SELECT result = $1::jsonb FROM mcp_runtime_operations WHERE operation_id=$2`, string(rawResult), result.OperationID).Scan(&sameResult); err != nil {
				return fmt.Errorf("compare MCP runtime operation settlement replay: %w", err)
			}
			if !sameResult {
				return domain.NewError(domain.CodeInvalidState, "MCP runtime operation result changed after settlement")
			}
			return nil
		}
		if status == task.OperationCancelled && result.Status != task.OperationPrepared && !validMCPRuntimeCancellationReceipt(rawResult, result) {
			return domain.NewError(domain.CodeInvalidState, "runtime cancellation receipt does not match the MCP operation fence")
		}
		if !task.CanTransitionOperation(result.Status, status) {
			return domain.NewError(domain.CodeInvalidState, "MCP runtime operation status transition is invalid")
		}
		if _, err := tx.Exec(ctx, `
UPDATE mcp_runtime_operations SET status=$2, result=$3::jsonb, updated_at=clock_timestamp()
WHERE operation_id=$1`, result.OperationID, string(status), string(rawResult)); err != nil {
			return fmt.Errorf("settle MCP runtime operation: %w", err)
		}
		if status != task.OperationUnknown && session.activeOperationID == result.OperationID {
			if _, err := tx.Exec(ctx, `
UPDATE mcp_runtime_sessions SET active_operation_id=NULL, updated_at=clock_timestamp()
WHERE owner_id=$1 AND api_key_id=$2 AND session_id=$3 AND host_id=$4 AND active_operation_id=$5`,
				key.OwnerID, key.APIKeyID, key.SessionID, key.HostID, result.OperationID); err != nil {
				return fmt.Errorf("clear settled MCP runtime operation pointer: %w", err)
			}
			session.activeOperationID = ""
		}
		result.Status = status
		result.Result = append(json.RawMessage(nil), rawResult...)
		return nil
	})
	return result, err
}

// SettleMCPRuntimeOperationNotFound is the durable fallback for a stop request
// whose Runtime lease expired and whose exact operation ID is absent from the
// runtime journal. The caller must only use it after RuntimeControl verifies
// operation_not_found against the persisted Runtime fence.
func (s *Store) SettleMCPRuntimeOperationNotFound(ctx context.Context, key app.MCPRuntimeOperationKey, runtimeTaskID string) (app.MCPRuntimeOperation, error) {
	if err := validateMCPRuntimeKey(key); err != nil {
		return app.MCPRuntimeOperation{}, err
	}
	if !domain.ValidTaskID(runtimeTaskID) {
		return app.MCPRuntimeOperation{}, domain.InvalidArgument("runtime_task_id", "MCP runtime task id is invalid")
	}
	const notFoundResult = `{"reconciled":"operation_not_found_after_lease_expiry"}`
	var result app.MCPRuntimeOperation
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		session, err := lockMCPRuntimeSession(ctx, tx, key)
		if err != nil {
			return err
		}
		if session.runtimeTaskID != runtimeTaskID {
			return mcpRuntimeFenceChangedError()
		}
		result, err = loadMCPRuntimeOperation(ctx, tx, key, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "MCP runtime operation not found")
		}
		if err != nil {
			return fmt.Errorf("lock MCP runtime operation for operation-not-found settlement: %w", err)
		}
		if result.RuntimeTaskID != runtimeTaskID {
			return mcpRuntimeFenceChangedError()
		}
		if result.Status == task.OperationCancelled {
			var sameResult bool
			if err := tx.QueryRow(ctx, `SELECT result = $1::jsonb FROM mcp_runtime_operations WHERE operation_id=$2`, notFoundResult, result.OperationID).Scan(&sameResult); err != nil {
				return fmt.Errorf("compare operation-not-found settlement replay: %w", err)
			}
			if sameResult {
				return nil
			}
		}
		if (result.Status != task.OperationDispatched && result.Status != task.OperationUnknown) || session.activeOperationID != result.OperationID {
			return domain.NewError(domain.CodeInvalidState, "only the active dispatched or unknown operation can be reconciled as not found")
		}
		var cancelRequested, leaseExpired bool
		if err := tx.QueryRow(ctx, `
SELECT cancel_requested_at IS NOT NULL, lease_expires_at <= clock_timestamp()
FROM mcp_runtime_operations WHERE operation_id=$1`, result.OperationID).Scan(&cancelRequested, &leaseExpired); err != nil {
			return fmt.Errorf("check MCP runtime cancellation evidence: %w", err)
		}
		if !cancelRequested || !leaseExpired {
			return domain.NewError(domain.CodeConflict, "MCP runtime operation is not yet eligible for expired-lease cancellation")
		}
		if _, err := tx.Exec(ctx, `
UPDATE mcp_runtime_operations SET status='cancelled', result=$2::jsonb, updated_at=clock_timestamp()
WHERE operation_id=$1`, result.OperationID, notFoundResult); err != nil {
			return fmt.Errorf("settle MCP runtime operation not found: %w", err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE mcp_runtime_sessions SET active_operation_id=NULL, updated_at=clock_timestamp()
WHERE owner_id=$1 AND api_key_id=$2 AND session_id=$3 AND host_id=$4 AND active_operation_id=$5`,
			key.OwnerID, key.APIKeyID, key.SessionID, key.HostID, result.OperationID); err != nil {
			return fmt.Errorf("clear reconciled MCP runtime operation pointer: %w", err)
		}
		result.Status = task.OperationCancelled
		result.Result = json.RawMessage(notFoundResult)
		session.activeOperationID = ""
		return nil
	})
	return result, err
}

func (s *Store) GetMCPRuntimeOperation(ctx context.Context, key app.MCPRuntimeOperationKey) (app.MCPRuntimeOperation, error) {
	if err := validateMCPRuntimeKey(key); err != nil {
		return app.MCPRuntimeOperation{}, err
	}
	result, err := scanMCPRuntimeOperation(s.pool.QueryRow(ctx, mcpRuntimeOperationSelect+`
WHERE o.owner_id=$1 AND o.actor_user_id=$2 AND o.api_key_id=$3 AND o.session_id=$4 AND o.host_id=$5 AND o.idempotency_key=$6`,
		key.OwnerID, mcpRuntimeActorID(key), key.APIKeyID, key.SessionID, key.HostID, key.IdempotencyKey))
	if errors.Is(err, pgx.ErrNoRows) {
		return app.MCPRuntimeOperation{}, domain.NewError(domain.CodeNotFound, "MCP runtime operation not found")
	}
	if err != nil {
		return app.MCPRuntimeOperation{}, fmt.Errorf("get MCP runtime operation: %w", err)
	}
	return result, nil
}

func (s *Store) ListActiveMCPRuntimeTaskIDs(ctx context.Context, ownerID, hostID string, fence app.AgentHostFence) ([]string, error) {
	return s.listActiveMCPRuntimeTaskIDs(ctx, ownerID, hostID, fence)
}

func (s *Store) listActiveMCPRuntimeTaskIDs(ctx context.Context, ownerID, hostID string, fence app.AgentHostFence) ([]string, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidHostID(hostID) || !validMCPRuntimeFence(fence) {
		return nil, domain.InvalidArgument("host", "MCP runtime Host fence is invalid")
	}
	rows, err := s.pool.Query(ctx, `
SELECT DISTINCT s.runtime_task_id
FROM mcp_runtime_sessions s
JOIN mcp_runtime_operations o
  ON o.owner_id=s.owner_id AND o.actor_user_id=s.actor_user_id AND o.api_key_id=s.api_key_id AND o.session_id=s.session_id AND o.host_id=s.host_id
 AND o.operation_id=s.active_operation_id
WHERE s.owner_id=$1 AND s.host_id=$2
  AND o.runtime_generation=$3 AND o.runtime_instance_id=$4 AND o.control_version=$5
  AND o.status IN ('prepared','dispatched','unknown')
ORDER BY s.runtime_task_id`, ownerID, hostID, fence.RuntimeGeneration, fence.RuntimeInstanceID, fence.ControlVersion)
	if err != nil {
		return nil, fmt.Errorf("list active MCP runtime task ids: %w", err)
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read active MCP runtime task id: %w", err)
		}
		result = append(result, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active MCP runtime task ids: %w", err)
	}
	return result, nil
}

func (s *Store) ListOwnerActiveMCPRuntimeOperations(ctx context.Context, ownerID string) ([]app.MCPRuntimeOperation, error) {
	if !domain.ValidUserID(ownerID) {
		return nil, domain.InvalidArgument("owner_id", "owner id is invalid")
	}
	rows, err := s.pool.Query(ctx, mcpRuntimeOperationSelect+`
WHERE o.owner_id=$1 AND o.operation_id=s.active_operation_id
  AND o.status IN ('prepared','dispatched','unknown')
ORDER BY o.created_at, o.operation_id`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list owner active MCP runtime operations: %w", err)
	}
	defer rows.Close()
	result := make([]app.MCPRuntimeOperation, 0)
	for rows.Next() {
		value, err := scanMCPRuntimeOperation(rows)
		if err != nil {
			return nil, fmt.Errorf("read owner active MCP runtime operation: %w", err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate owner active MCP runtime operations: %w", err)
	}
	return result, nil
}

func (s *Store) ListMCPRuntimeOperationsPendingCancellation(ctx context.Context, limit int) ([]app.MCPRuntimeOperation, error) {
	if limit < 1 || limit > int(maxPageSize) {
		return nil, domain.InvalidArgument("limit", "MCP runtime cancellation limit is out of range")
	}
	rows, err := s.pool.Query(ctx, mcpRuntimeOperationSelect+`
WHERE o.operation_id=s.active_operation_id
  AND o.status IN ('prepared','dispatched','unknown')
  AND (o.cancel_requested_at IS NOT NULL OR (o.status='prepared' AND o.lease_expires_at <= clock_timestamp()))
ORDER BY COALESCE(o.cancel_requested_at, o.lease_expires_at), o.created_at, o.operation_id
LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list MCP runtime operations pending cancellation: %w", err)
	}
	defer rows.Close()
	result := make([]app.MCPRuntimeOperation, 0)
	for rows.Next() {
		value, err := scanMCPRuntimeOperation(rows)
		if err != nil {
			return nil, fmt.Errorf("read MCP runtime operation pending cancellation: %w", err)
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate MCP runtime operations pending cancellation: %w", err)
	}
	return result, nil
}

type mcpRuntimeSessionRow struct {
	runtimeTaskID     string
	leaseVersion      int64
	activeOperationID string
}

func lockMCPRuntimeSession(ctx context.Context, tx pgx.Tx, key app.MCPRuntimeOperationKey) (mcpRuntimeSessionRow, error) {
	var result mcpRuntimeSessionRow
	err := tx.QueryRow(ctx, `
SELECT runtime_task_id, lease_version, COALESCE(active_operation_id, '')
FROM mcp_runtime_sessions WHERE owner_id=$1 AND actor_user_id=$2 AND api_key_id=$3 AND session_id=$4 AND host_id=$5 FOR UPDATE`,
		key.OwnerID, mcpRuntimeActorID(key), key.APIKeyID, key.SessionID, key.HostID).Scan(&result.runtimeTaskID, &result.leaseVersion, &result.activeOperationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return mcpRuntimeSessionRow{}, domain.NewError(domain.CodeNotFound, "MCP runtime session not found")
	}
	if err != nil {
		return mcpRuntimeSessionRow{}, fmt.Errorf("lock MCP runtime session: %w", err)
	}
	return result, nil
}

func loadMCPRuntimeOperation(ctx context.Context, tx pgx.Tx, key app.MCPRuntimeOperationKey, forUpdate bool) (app.MCPRuntimeOperation, error) {
	query := mcpRuntimeOperationSelect + `
WHERE o.owner_id=$1 AND o.actor_user_id=$2 AND o.api_key_id=$3 AND o.session_id=$4 AND o.host_id=$5 AND o.idempotency_key=$6`
	if forUpdate {
		query += ` FOR UPDATE OF o`
	}
	return scanMCPRuntimeOperation(tx.QueryRow(ctx, query, key.OwnerID, mcpRuntimeActorID(key), key.APIKeyID, key.SessionID, key.HostID, key.IdempotencyKey))
}

func scanMCPRuntimeOperation(row pgx.Row) (app.MCPRuntimeOperation, error) {
	var result app.MCPRuntimeOperation
	var requestHash, arguments, rawResult []byte
	var status string
	err := row.Scan(
		&result.OperationID, &result.Key.OwnerID, &result.Key.ActorUserID, &result.Key.APIKeyID, &result.Key.SessionID, &result.Key.HostID,
		&result.Key.IdempotencyKey, &result.RuntimeTaskID, &requestHash, &result.Capability,
		&result.Action, &arguments, &result.WorkspaceID, &result.BindingVersion,
		&result.Fence.RuntimeGeneration, &result.Fence.RuntimeInstanceID,
		&result.Fence.ControlVersion, &result.LeaseVersion, &result.LeaseExpiresAt, &status, &rawResult)
	if err != nil {
		return app.MCPRuntimeOperation{}, err
	}
	result.RequestHash = append([]byte(nil), requestHash...)
	result.Arguments = append(json.RawMessage(nil), arguments...)
	result.Result = append(json.RawMessage(nil), rawResult...)
	result.Fence.AIAllowed = true
	result.Status = task.OperationStatus(status)
	return result, nil
}

func lockAndValidateMCPHost(ctx context.Context, tx pgx.Tx, ownerID, hostID string, fence app.AgentHostFence) error {
	var state, controlState, runtimeInstanceID string
	var runtimeGeneration, controlVersion int64
	var deletedAt *time.Time
	err := tx.QueryRow(ctx, `
SELECT state, deleted_at, control_state, runtime_generation, COALESCE(runtime_instance_id,''), control_version
FROM hosts WHERE id=$1 AND owner_id=$2 FOR UPDATE`, hostID, ownerID).
		Scan(&state, &deletedAt, &controlState, &runtimeGeneration, &runtimeInstanceID, &controlVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "host not found")
	}
	if err != nil {
		return fmt.Errorf("lock MCP runtime Host: %w", err)
	}
	if state != "running" || deletedAt != nil || controlState != "ai" ||
		runtimeGeneration != fence.RuntimeGeneration || runtimeInstanceID != fence.RuntimeInstanceID ||
		controlVersion != fence.ControlVersion {
		return mcpRuntimeFenceChangedError()
	}
	return nil
}

func validateBeginMCPRuntimeOperation(request app.BeginMCPRuntimeOperation) error {
	if err := validateMCPRuntimeKey(request.Key); err != nil {
		return err
	}
	if !domain.ValidTaskID(request.NewRuntimeTaskID) || !domain.ValidOperationID(request.NewOperationID) ||
		len(request.RequestHash) != 32 || strings.TrimSpace(request.Capability) == "" || len(request.Capability) > 128 ||
		strings.TrimSpace(request.Action) == "" || len(request.Action) > 128 || request.ExpectedExecutionEpoch < 1 ||
		request.LeaseDuration.Microseconds() < 1 || !validMCPRuntimeFence(request.Fence) || !validMCPRuntimeJSONObject(request.Arguments) {
		return domain.InvalidArgument("operation", "MCP runtime operation fields are invalid")
	}
	if !validOperationWorkspaceBinding(request.WorkspaceID, request.BindingVersion) {
		return domain.InvalidArgument("operation.workspace_id", "MCP runtime Workspace binding is invalid")
	}
	return nil
}

func validateMCPRuntimeKey(key app.MCPRuntimeOperationKey) error {
	if !domain.ValidUserID(key.OwnerID) || key.ActorUserID != "" && !domain.ValidUserID(key.ActorUserID) ||
		!domain.ValidTokenID(key.APIKeyID) || !domain.ValidHostID(key.HostID) ||
		!utf8.ValidString(key.SessionID) || strings.TrimSpace(key.SessionID) == "" || len(key.SessionID) > 255 ||
		!utf8.ValidString(key.IdempotencyKey) || strings.TrimSpace(key.IdempotencyKey) == "" || len(key.IdempotencyKey) > 255 {
		return domain.InvalidArgument("session", "MCP runtime session or idempotency key is invalid")
	}
	return nil
}

func mcpRuntimeActorID(key app.MCPRuntimeOperationKey) string {
	if key.ActorUserID != "" {
		return key.ActorUserID
	}
	return key.OwnerID
}

func requireCurrentMCPAuthorization(ctx context.Context, tx pgx.Tx, key app.MCPRuntimeOperationKey) error {
	actorID := mcpRuntimeActorID(key)
	userIDs := []string{key.OwnerID}
	if actorID != key.OwnerID {
		userIDs = append(userIDs, actorID)
	}
	for _, userID := range userIDs {
		var approved bool
		err := tx.QueryRow(ctx, `SELECT status='approved' FROM users WHERE id=$1 FOR SHARE`, userID).Scan(&approved)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !approved {
			return mcpAuthorizationUnavailable()
		}
		if err != nil {
			return fmt.Errorf("check mcp runtime account authorization: %w", err)
		}
	}
	var kind string
	var expiresAt time.Time
	var notRevoked bool
	var hostIDs, scopes []string
	err := tx.QueryRow(ctx, `SELECT kind, expires_at, revoked_at IS NULL, host_ids, scopes
FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`, key.APIKeyID, actorID).Scan(&kind, &expiresAt, &notRevoked, &hostIDs, &scopes)
	if errors.Is(err, pgx.ErrNoRows) {
		return mcpAuthorizationUnavailable()
	}
	if err != nil {
		return fmt.Errorf("check mcp runtime api key authorization: %w", err)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return fmt.Errorf("read database time for mcp runtime authorization: %w", err)
	}
	if kind != string(identity.KindAPIKey) || !notRevoked || !expiresAt.After(now) {
		return mcpAuthorizationUnavailable()
	}
	if len(hostIDs) > 0 {
		allowed := false
		for _, hostID := range hostIDs {
			if hostID == key.HostID {
				allowed = true
				break
			}
		}
		if !allowed {
			return mcpAuthorizationUnavailable()
		}
	}
	if len(scopes) > 0 {
		allowed := false
		for _, scope := range scopes {
			if scope == string(identity.ScopeComputerOperate) {
				allowed = true
				break
			}
		}
		if !allowed {
			return mcpAuthorizationUnavailable()
		}
	}
	if actorID != key.OwnerID {
		var permissions []string
		err := tx.QueryRow(ctx, `SELECT permissions FROM host_members
WHERE host_id=$1 AND owner_id=$2 AND user_id=$3 AND revoked_at IS NULL FOR SHARE`,
			key.HostID, key.OwnerID, actorID).Scan(&permissions)
		if errors.Is(err, pgx.ErrNoRows) {
			return mcpAuthorizationUnavailable()
		}
		if err != nil {
			return fmt.Errorf("check mcp runtime host membership: %w", err)
		}
		allowed := false
		for _, permission := range permissions {
			if permission == string(host.PermissionMCPControl) {
				allowed = true
				break
			}
		}
		if !allowed {
			return mcpAuthorizationUnavailable()
		}
	}
	return nil
}

func mcpAuthorizationUnavailable() error {
	return domain.NewError(domain.CodeUnauthenticated, "mcp runtime authorization is no longer valid")
}

func requestMCPRuntimeCancellationsForToken(ctx context.Context, tx pgx.Tx, actorID, tokenID string) error {
	if _, err := tx.Exec(ctx, `UPDATE mcp_runtime_operations o
SET cancel_requested_at=COALESCE(o.cancel_requested_at, clock_timestamp()), updated_at=clock_timestamp()
FROM mcp_runtime_sessions s
WHERE s.owner_id=o.owner_id AND s.actor_user_id=o.actor_user_id AND s.api_key_id=o.api_key_id
  AND s.session_id=o.session_id AND s.host_id=o.host_id AND s.active_operation_id=o.operation_id
  AND o.actor_user_id=$1 AND o.api_key_id=$2 AND o.status IN ('prepared','dispatched','unknown')`, actorID, tokenID); err != nil {
		return fmt.Errorf("persist mcp runtime token cancellation requests: %w", err)
	}
	return nil
}

func requestMCPRuntimeCancellationsForActor(ctx context.Context, tx pgx.Tx, actorID string) error {
	if _, err := tx.Exec(ctx, `UPDATE mcp_runtime_operations o
SET cancel_requested_at=COALESCE(o.cancel_requested_at, clock_timestamp()), updated_at=clock_timestamp()
FROM mcp_runtime_sessions s
WHERE s.owner_id=o.owner_id AND s.actor_user_id=o.actor_user_id AND s.api_key_id=o.api_key_id
  AND s.session_id=o.session_id AND s.host_id=o.host_id AND s.active_operation_id=o.operation_id
  AND (o.actor_user_id=$1 OR o.owner_id=$1) AND o.status IN ('prepared','dispatched','unknown')`, actorID); err != nil {
		return fmt.Errorf("persist mcp runtime account cancellation requests: %w", err)
	}
	return nil
}

func requestMCPRuntimeCancellationsForHostMember(ctx context.Context, tx pgx.Tx, ownerID, actorID, hostID string) error {
	if _, err := tx.Exec(ctx, `UPDATE mcp_runtime_operations o
SET cancel_requested_at=COALESCE(o.cancel_requested_at, clock_timestamp()), updated_at=clock_timestamp()
FROM mcp_runtime_sessions s
WHERE s.owner_id=o.owner_id AND s.actor_user_id=o.actor_user_id AND s.api_key_id=o.api_key_id
  AND s.session_id=o.session_id AND s.host_id=o.host_id AND s.active_operation_id=o.operation_id
  AND o.owner_id=$1 AND o.actor_user_id=$2 AND o.host_id=$3 AND o.status IN ('prepared','dispatched','unknown')`,
		ownerID, actorID, hostID); err != nil {
		return fmt.Errorf("persist mcp runtime host membership cancellation requests: %w", err)
	}
	return nil
}

func validMCPRuntimeFence(fence app.AgentHostFence) bool {
	return fence.AIAllowed && fence.RuntimeGeneration > 0 && fence.RuntimeInstanceID != "" &&
		strings.TrimSpace(fence.RuntimeInstanceID) != "" && len(fence.RuntimeInstanceID) <= 255 && fence.ControlVersion >= 0
}

func validMCPRuntimeJSONObject(raw json.RawMessage) bool {
	if len(raw) == 0 || len(raw) > maxMCPRuntimeArgumentsBytes || !json.Valid(raw) {
		return false
	}
	var value map[string]json.RawMessage
	return json.Unmarshal(raw, &value) == nil && value != nil
}

func validMCPRuntimeResult(raw json.RawMessage) bool {
	return len(raw) <= maxMCPRuntimeResultBytes && (len(raw) == 0 || json.Valid(raw))
}

func validMCPRuntimeSettleStatus(status task.OperationStatus) bool {
	switch status {
	case task.OperationSucceeded, task.OperationFailed, task.OperationUnknown, task.OperationCancelled:
		return true
	default:
		return false
	}
}

func validMCPRuntimeCancellationReceipt(raw json.RawMessage, operation app.MCPRuntimeOperation) bool {
	var receipt struct {
		RuntimeStatus     *string `json:"runtime_status"`
		OperationID       *string `json:"operation_id"`
		RuntimeGeneration *int64  `json:"runtime_generation"`
		RuntimeInstanceID *string `json:"runtime_instance_id"`
		ControlVersion    *int64  `json:"control_version"`
		LeaseVersion      *int64  `json:"lease_version"`
		HostID            *string `json:"host_id"`
		RuntimeTaskID     *string `json:"runtime_task_id"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &receipt) != nil || receipt.RuntimeStatus == nil ||
		receipt.OperationID == nil || receipt.RuntimeGeneration == nil || receipt.RuntimeInstanceID == nil ||
		receipt.ControlVersion == nil || receipt.LeaseVersion == nil || receipt.HostID == nil || receipt.RuntimeTaskID == nil {
		return false
	}
	return *receipt.RuntimeStatus == "cancelled" && *receipt.OperationID == operation.OperationID &&
		*receipt.RuntimeGeneration == operation.Fence.RuntimeGeneration &&
		*receipt.RuntimeInstanceID == operation.Fence.RuntimeInstanceID &&
		*receipt.ControlVersion == operation.Fence.ControlVersion && *receipt.LeaseVersion == operation.LeaseVersion &&
		*receipt.HostID == operation.Key.HostID && *receipt.RuntimeTaskID == operation.RuntimeTaskID
}

func mcpRuntimeOperationActive(status task.OperationStatus) bool {
	return status == task.OperationPrepared || status == task.OperationDispatched || status == task.OperationUnknown
}

func mcpRuntimeFenceChangedError() error {
	err := domain.NewError(domain.CodeInvalidState, "MCP runtime Host or operation fence changed")
	err.Details["reason"] = "control_changed"
	return err
}
