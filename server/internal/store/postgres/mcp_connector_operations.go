package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"

	"github.com/jackc/pgx/v5"
)

const maxMCPConnectorOperationResultBytes = 64 << 10

func (s *Store) PrepareMCPConnectorOperation(ctx context.Context, value app.MCPConnectorOperation, leaseOwner string) (app.MCPConnectorOperation, bool, error) {
	if !validMCPConnectorOperation(value) || leaseOwner == "" || len(leaseOwner) > 255 {
		return app.MCPConnectorOperation{}, false, domain.InvalidArgument("operation", "MCP connector operation fields are invalid")
	}
	var result app.MCPConnectorOperation
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockMCPConnectorTaskLease(ctx, tx, value, leaseOwner); err != nil {
			return err
		}
		stored, err := getMCPConnectorOperationForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.ConnectorID, value.Key)
		if err == nil {
			if !sameMCPConnectorOperationRequest(stored, value) {
				return idempotencyConflict()
			}
			if stored.Status == task.OperationPrepared && stored.LeaseVersion != value.LeaseVersion {
				if err := lockCurrentTaskMCPConnectorGrant(ctx, tx, value); err != nil {
					return err
				}
				if err := adoptPreparedMCPConnectorOperation(ctx, tx, &stored, value.LeaseVersion); err != nil {
					return err
				}
			}
			result, replayed = stored, true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read MCP connector operation before prepare: %w", err)
		}
		if err := lockCurrentTaskMCPConnectorGrant(ctx, tx, value); err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `INSERT INTO task_mcp_connector_operations (
id, task_id, owner_id, connector_id, connector_revision, credential_version, grant_version, lease_version,
operation_key, tool_name, request_hash, status
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'prepared')
ON CONFLICT (task_id, connector_id, operation_key) DO NOTHING`,
			value.ID, value.TaskID, value.OwnerID, value.ConnectorID, value.ConnectorRevision, value.CredentialVersion,
			value.GrantVersion, value.LeaseVersion, value.Key, value.ToolName, value.RequestHash)
		if err != nil {
			return fmt.Errorf("prepare MCP connector operation: %w", err)
		}
		if command.RowsAffected() == 1 {
			result, err = getMCPConnectorOperationForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.ConnectorID, value.Key)
			if err != nil {
				return fmt.Errorf("read prepared MCP connector operation: %w", err)
			}
			return nil
		}
		stored, err = getMCPConnectorOperationForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.ConnectorID, value.Key)
		if err != nil {
			return fmt.Errorf("read existing MCP connector operation: %w", err)
		}
		if !sameMCPConnectorOperationRequest(stored, value) {
			return idempotencyConflict()
		}
		if stored.Status == task.OperationPrepared && stored.LeaseVersion != value.LeaseVersion {
			if err := adoptPreparedMCPConnectorOperation(ctx, tx, &stored, value.LeaseVersion); err != nil {
				return err
			}
		}
		result, replayed = stored, true
		return nil
	})
	return result, replayed, err
}

func adoptPreparedMCPConnectorOperation(ctx context.Context, tx pgx.Tx, stored *app.MCPConnectorOperation, leaseVersion int64) error {
	if err := tx.QueryRow(ctx, `UPDATE task_mcp_connector_operations
SET lease_version=$2, updated_at=clock_timestamp()
WHERE id=$1 AND status='prepared' RETURNING updated_at`, stored.ID, leaseVersion).Scan(&stored.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "MCP connector operation changed before lease adoption")
		}
		return fmt.Errorf("adopt prepared MCP connector operation: %w", err)
	}
	stored.LeaseVersion = leaseVersion
	return nil
}

func (s *Store) MarkMCPConnectorOperationDispatched(ctx context.Context, value app.MCPConnectorOperation, leaseOwner string) (app.MCPConnectorOperation, error) {
	if !validMCPConnectorOperation(value) || leaseOwner == "" || len(leaseOwner) > 255 {
		return app.MCPConnectorOperation{}, domain.InvalidArgument("operation", "MCP connector operation fields are invalid")
	}
	var result app.MCPConnectorOperation
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockMCPConnectorTaskLease(ctx, tx, value, leaseOwner); err != nil {
			return err
		}
		if err := lockCurrentTaskMCPConnectorGrant(ctx, tx, value); err != nil {
			return err
		}
		stored, err := getMCPConnectorOperationForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.ConnectorID, value.Key)
		if err != nil {
			return fmt.Errorf("read MCP connector operation for dispatch: %w", err)
		}
		if !sameMCPConnectorOperationRequest(stored, value) || stored.LeaseVersion != value.LeaseVersion {
			return idempotencyConflict()
		}
		if stored.Status != task.OperationPrepared {
			return domain.NewError(domain.CodeConflict, "MCP connector operation is no longer prepared")
		}
		if err := tx.QueryRow(ctx, `UPDATE task_mcp_connector_operations SET status='dispatched', updated_at=clock_timestamp()
WHERE id=$1 AND task_id=$2 AND owner_id=$3 AND connector_id=$4 AND operation_key=$5 AND status='prepared' RETURNING updated_at`,
			stored.ID, stored.TaskID, stored.OwnerID, stored.ConnectorID, stored.Key).Scan(&stored.UpdatedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeConflict, "MCP connector operation changed before dispatch")
			}
			return fmt.Errorf("mark MCP connector operation dispatched: %w", err)
		}
		stored.Status = task.OperationDispatched
		result = stored
		return nil
	})
	return result, err
}

func (s *Store) CompleteMCPConnectorOperation(ctx context.Context, value app.MCPConnectorOperation, leaseOwner string, status task.OperationStatus, output string) (app.MCPConnectorOperation, error) {
	if !validMCPConnectorOperation(value) || leaseOwner == "" || len(leaseOwner) > 255 ||
		(status != task.OperationSucceeded && status != task.OperationFailed) || len(output) > maxMCPConnectorOperationResultBytes {
		return app.MCPConnectorOperation{}, domain.InvalidArgument("operation", "MCP connector operation result is invalid")
	}
	var result app.MCPConnectorOperation
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockMCPConnectorTask(ctx, tx, value.TaskID, value.OwnerID); err != nil {
			return err
		}
		stored, err := getMCPConnectorOperationForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.ConnectorID, value.Key)
		if err != nil {
			return fmt.Errorf("read MCP connector operation for completion: %w", err)
		}
		if !sameMCPConnectorOperationRequest(stored, value) || stored.LeaseVersion != value.LeaseVersion {
			return idempotencyConflict()
		}
		if stored.Status == status && stored.Result == output {
			result = stored
			return nil
		}
		if stored.CancelRequested {
			return domain.NewError(domain.CodeConflict, "MCP connector operation was revoked before result commit")
		}
		if stored.Status != task.OperationDispatched || !task.CanTransitionOperation(stored.Status, status) {
			return domain.NewError(domain.CodeConflict, "MCP connector operation is not dispatchable to a terminal result")
		}
		if err := tx.QueryRow(ctx, `UPDATE task_mcp_connector_operations SET status=$2, result=$3, updated_at=clock_timestamp()
WHERE id=$1 AND status='dispatched' RETURNING updated_at`, stored.ID, string(status), output).Scan(&stored.UpdatedAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeConflict, "MCP connector operation changed before completion")
			}
			return fmt.Errorf("persist MCP connector operation result: %w", err)
		}
		stored.Status, stored.Result = status, output
		result = stored
		return nil
	})
	return result, err
}

func (s *Store) MarkMCPConnectorOperationUnknown(ctx context.Context, value app.MCPConnectorOperation, leaseOwner string, resultText string) error {
	if !validMCPConnectorOperation(value) || leaseOwner == "" || len(leaseOwner) > 255 || len(resultText) > maxMCPConnectorOperationResultBytes {
		return domain.InvalidArgument("operation", "MCP connector operation fields are invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockMCPConnectorTask(ctx, tx, value.TaskID, value.OwnerID); err != nil {
			return err
		}
		stored, err := getMCPConnectorOperationForUpdate(ctx, tx, value.OwnerID, value.TaskID, value.ConnectorID, value.Key)
		if err != nil {
			return fmt.Errorf("read MCP connector operation to mark unknown: %w", err)
		}
		if !sameMCPConnectorOperationRequest(stored, value) || stored.LeaseVersion != value.LeaseVersion {
			return idempotencyConflict()
		}
		if stored.Status == task.OperationUnknown && stored.Result == resultText {
			return nil
		}
		if stored.Status != task.OperationDispatched || !task.CanTransitionOperation(stored.Status, task.OperationUnknown) {
			return domain.NewError(domain.CodeConflict, "MCP connector operation is not dispatched")
		}
		command, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations SET status='unknown', result=$2, updated_at=clock_timestamp()
WHERE id=$1 AND status='dispatched'`, stored.ID, resultText)
		if err != nil {
			return fmt.Errorf("persist unknown MCP connector operation: %w", err)
		}
		if command.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "MCP connector operation changed before unknown settlement")
		}
		return nil
	})
}

func (s *Store) GetMCPConnectorOperation(ctx context.Context, ownerID, taskID, connectorID, key string) (app.MCPConnectorOperation, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || !domain.ValidMCPConnectorID(connectorID) || !validMCPConnectorOperationKey(key) {
		return app.MCPConnectorOperation{}, domain.InvalidArgument("operation", "MCP connector operation identity is invalid")
	}
	var result app.MCPConnectorOperation
	err := s.pool.QueryRow(ctx, `SELECT id, task_id, owner_id, connector_id, connector_revision, credential_version,
grant_version, lease_version, operation_key, tool_name, request_hash, status, cancel_requested, result, created_at, updated_at
FROM task_mcp_connector_operations WHERE owner_id=$1 AND task_id=$2 AND connector_id=$3 AND operation_key=$4`,
		ownerID, taskID, connectorID, key).Scan(
		&result.ID, &result.TaskID, &result.OwnerID, &result.ConnectorID, &result.ConnectorRevision, &result.CredentialVersion,
		&result.GrantVersion, &result.LeaseVersion, &result.Key, &result.ToolName, &result.RequestHash, &result.Status, &result.CancelRequested,
		&result.Result, &result.CreatedAt, &result.UpdatedAt)
	if err != nil {
		return app.MCPConnectorOperation{}, storeError("get MCP connector operation", "MCP connector operation", err)
	}
	return result, nil
}

func lockMCPConnectorTaskLease(ctx context.Context, tx pgx.Tx, value app.MCPConnectorOperation, leaseOwner string) error {
	var status, storedOwner string
	var leaseVersion int64
	var leaseValid bool
	err := tx.QueryRow(ctx, `SELECT status, lease_owner, lease_version, COALESCE(lease_expires_at > clock_timestamp(), false)
FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, value.TaskID, value.OwnerID).Scan(&status, &storedOwner, &leaseVersion, &leaseValid)
	if errors.Is(err, pgx.ErrNoRows) {
		return missingTaskError()
	}
	if err != nil {
		return fmt.Errorf("lock MCP connector task lease: %w", err)
	}
	if status != string(task.StatusRunning) || storedOwner != leaseOwner || leaseVersion != value.LeaseVersion || !leaseValid {
		return domain.NewError(domain.CodeInvalidState, "task lease is not current for MCP connector operation")
	}
	return nil
}

func lockMCPConnectorTask(ctx context.Context, tx pgx.Tx, taskID, ownerID string) error {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, taskID, ownerID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return missingTaskError()
	}
	if err != nil {
		return fmt.Errorf("lock task for MCP connector operation result: %w", err)
	}
	return nil
}

func lockCurrentTaskMCPConnectorGrant(ctx context.Context, tx pgx.Tx, value app.MCPConnectorOperation) error {
	var connectorRevision, credentialVersion, grantVersion int64
	var enabled bool
	err := tx.QueryRow(ctx, `SELECT g.connector_revision, g.credential_version, g.grant_version, c.enabled
FROM task_mcp_connector_grants g
JOIN mcp_connectors c ON c.id=g.connector_id AND c.owner_id=g.owner_id
WHERE g.task_id=$1 AND g.owner_id=$2 AND g.connector_id=$3 AND g.revoked_at IS NULL
FOR SHARE OF g, c`, value.TaskID, value.OwnerID, value.ConnectorID).Scan(
		&connectorRevision, &credentialVersion, &grantVersion, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeForbidden, "MCP connector grant is not available for this task")
	}
	if err != nil {
		return fmt.Errorf("lock task MCP connector grant: %w", err)
	}
	if !enabled || connectorRevision != value.ConnectorRevision || credentialVersion != value.CredentialVersion || grantVersion != value.GrantVersion {
		return domain.NewError(domain.CodeConflict, "MCP connector grant changed before dispatch")
	}
	return nil
}

func getMCPConnectorOperationForUpdate(ctx context.Context, tx pgx.Tx, ownerID, taskID, connectorID, key string) (app.MCPConnectorOperation, error) {
	var result app.MCPConnectorOperation
	err := tx.QueryRow(ctx, `SELECT id, task_id, owner_id, connector_id, connector_revision, credential_version,
grant_version, lease_version, operation_key, tool_name, request_hash, status, cancel_requested, result, created_at, updated_at
FROM task_mcp_connector_operations
WHERE owner_id=$1 AND task_id=$2 AND connector_id=$3 AND operation_key=$4 FOR UPDATE`,
		ownerID, taskID, connectorID, key).Scan(
		&result.ID, &result.TaskID, &result.OwnerID, &result.ConnectorID, &result.ConnectorRevision, &result.CredentialVersion,
		&result.GrantVersion, &result.LeaseVersion, &result.Key, &result.ToolName, &result.RequestHash, &result.Status, &result.CancelRequested,
		&result.Result, &result.CreatedAt, &result.UpdatedAt)
	return result, err
}

func validMCPConnectorOperation(value app.MCPConnectorOperation) bool {
	return domain.ValidOperationID(value.ID) && domain.ValidTaskID(value.TaskID) && domain.ValidUserID(value.OwnerID) &&
		domain.ValidMCPConnectorID(value.ConnectorID) && validMCPConnectorOperationKey(value.Key) &&
		value.ToolName != "" && len(value.ToolName) <= 128 && value.ConnectorRevision > 0 && value.CredentialVersion >= 0 &&
		value.GrantVersion > 0 && value.LeaseVersion > 0 && len(value.RequestHash) == 32 && len(value.Result) <= maxMCPConnectorOperationResultBytes
}

func validMCPConnectorOperationKey(value string) bool {
	return value != "" && len(value) <= 512 && strings.TrimSpace(value) == value
}

func sameMCPConnectorOperationRequest(stored, requested app.MCPConnectorOperation) bool {
	return stored.TaskID == requested.TaskID && stored.OwnerID == requested.OwnerID && stored.ConnectorID == requested.ConnectorID &&
		stored.ConnectorRevision == requested.ConnectorRevision && stored.CredentialVersion == requested.CredentialVersion &&
		stored.GrantVersion == requested.GrantVersion &&
		stored.Key == requested.Key && stored.ToolName == requested.ToolName && string(stored.RequestHash) == string(requested.RequestHash)
}

var _ app.MCPConnectorOperationStore = (*Store)(nil)
