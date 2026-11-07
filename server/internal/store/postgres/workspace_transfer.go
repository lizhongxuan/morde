package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const workspaceTransferCreateOperation = "workspace.transfer.create"

func (s *Store) CreateWorkspaceOwnershipTransfer(ctx context.Context, actorID, workspaceID, targetID string,
	expectedRevision int64, id string, record app.IdempotencyRecord) (workspace.OwnershipTransfer, bool, error) {
	if !domain.ValidUserID(actorID) || !domain.ValidWorkspaceID(workspaceID) || !domain.ValidUserID(targetID) ||
		actorID == targetID || !workspace.ValidOwnershipTransferID(id) || expectedRevision < 0 ||
		record.OwnerID != actorID || record.Operation != workspaceTransferCreateOperation {
		return workspace.OwnershipTransfer{}, false, domain.InvalidArgument("ownership_transfer", "workspace ownership transfer request is invalid")
	}
	if err := validateIdempotency(record); err != nil {
		return workspace.OwnershipTransfer{}, false, err
	}

	var result workspace.OwnershipTransfer
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceMutationFence(ctx, tx, workspaceID, true); err != nil {
			return err
		}
		stored, found, err := findIdempotency(ctx, tx, record)
		if err != nil {
			return err
		}
		if found {
			value, err := getWorkspaceOwnershipTransfer(ctx, tx, actorID, workspaceID, stored.ResourceID, false)
			if err != nil {
				return err
			}
			if value.FromOwnerID != actorID || value.ToOwnerID != targetID || value.ExpectedRevision != expectedRevision {
				return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is bound to another Workspace transfer")
			}
			result, replayed = value, true
			return nil
		}

		var currentOwner, state string
		var readOnly bool
		var revision int64
		var deleted bool
		if err := tx.QueryRow(ctx, `SELECT owner_id, state, revision, deleted_at IS NOT NULL, is_read_only
FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&currentOwner, &state, &revision, &deleted, &readOnly); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		} else if err != nil {
			return fmt.Errorf("lock Workspace for ownership transfer: %w", err)
		}
		if currentOwner != actorID || deleted {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if readOnly {
			return domain.NewError(domain.CodeForbidden, "workspace is shared as read-only")
		}
		if state != string(workspace.StateReady) || revision != expectedRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed before ownership transfer")
		}
		if err := requireApprovedWorkspaceTransferTarget(ctx, tx, targetID); err != nil {
			return err
		}
		if err := validateWorkspaceTransferable(ctx, tx, workspaceID, revision); err != nil {
			return err
		}

		stored, replayed, err := reserveIdempotency(ctx, tx, record, id, workspaceID)
		if err != nil {
			return err
		}
		if replayed {
			value, err := getWorkspaceOwnershipTransfer(ctx, tx, actorID, workspaceID, stored.ResourceID, false)
			if err != nil {
				return err
			}
			if value.FromOwnerID != actorID || value.ToOwnerID != targetID || value.ExpectedRevision != expectedRevision {
				return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is bound to another Workspace transfer")
			}
			result = value
			return nil
		}

		if _, err := tx.Exec(ctx, `INSERT INTO workspace_ownership_transfers
(id, workspace_id, from_owner_id, to_owner_id, state, expected_revision)
VALUES ($1,$2,$3,$4,'pending',$5)`, id, workspaceID, actorID, targetID, expectedRevision); err != nil {
			if isUniqueViolation(err) {
				return domain.NewError(domain.CodeWorkspaceBusy, "Workspace already has an ownership transfer in progress")
			}
			return fmt.Errorf("create Workspace ownership transfer: %w", err)
		}
		result, err = getWorkspaceOwnershipTransfer(ctx, tx, actorID, workspaceID, id, false)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return workspace.OwnershipTransfer{}, false, err
	}
	return result, replayed, nil
}

func (s *Store) GetWorkspaceOwnershipTransfer(ctx context.Context, actorID, workspaceID, transferID string) (workspace.OwnershipTransfer, error) {
	if !domain.ValidUserID(actorID) || !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidOwnershipTransferID(transferID) {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
	}
	return getWorkspaceOwnershipTransfer(ctx, s.pool, actorID, workspaceID, transferID, false)
}

// ReconcileWorkspaceOwnershipTransfer clears an unknown operation only when
// every ownership record still proves that the atomic acceptance did not move
// the Workspace. Any mixed or unexpected state remains frozen.
func (s *Store) ReconcileWorkspaceOwnershipTransfer(ctx context.Context, actorID, workspaceID, transferID string) (workspace.OwnershipTransfer, error) {
	if !domain.ValidUserID(actorID) || !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidOwnershipTransferID(transferID) {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return workspace.OwnershipTransfer{}, fmt.Errorf("begin Workspace ownership transfer reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockWorkspaceMutationFence(ctx, tx, workspaceID, true); err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	if err := lockTransferWorkspace(ctx, tx, workspaceID); err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	value, err := getWorkspaceOwnershipTransfer(ctx, tx, actorID, workspaceID, transferID, true)
	if err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	if value.ToOwnerID != actorID {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
	}
	if value.State == workspace.OwnershipTransferAccepted || value.State == workspace.OwnershipTransferPending {
		if err := tx.Commit(ctx); err != nil {
			return workspace.OwnershipTransfer{}, fmt.Errorf("finish Workspace ownership transfer reconciliation read: %w", err)
		}
		return value, nil
	}
	if value.State != workspace.OwnershipTransferUnknown {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeWorkspaceBusy, "Workspace ownership transfer cannot be reconciled from its current state")
	}
	var currentOwner, workspaceState string
	var revision int64
	var deleted bool
	if err := tx.QueryRow(ctx, `SELECT owner_id,state,revision,deleted_at IS NOT NULL
FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&currentOwner, &workspaceState, &revision, &deleted); err != nil {
		return workspace.OwnershipTransfer{}, fmt.Errorf("read Workspace during ownership transfer reconciliation: %w", err)
	}
	if currentOwner != value.FromOwnerID || workspaceState != string(workspace.StateReady) || revision != value.ExpectedRevision || deleted {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeWorkspaceBusy, "Workspace ownership transfer result remains unknown because Workspace ownership changed")
	}
	var historyOwner string
	if err := tx.QueryRow(ctx, `SELECT owner_id FROM workspace_ownership_history
WHERE workspace_id=$1 AND ended_at IS NULL FOR UPDATE`, workspaceID).Scan(&historyOwner); errors.Is(err, pgx.ErrNoRows) {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeWorkspaceBusy, "Workspace ownership transfer result remains unknown because ownership history is inconsistent")
	} else if err != nil {
		return workspace.OwnershipTransfer{}, fmt.Errorf("read current Workspace ownership history during reconciliation: %w", err)
	}
	if historyOwner != value.FromOwnerID {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeWorkspaceBusy, "Workspace ownership transfer result remains unknown because ownership history changed")
	}
	var volumeOwner, volumeState, provisionedOwner string
	if err := tx.QueryRow(ctx, `SELECT owner_id,state,provisioned_owner_id
FROM workspace_volumes WHERE workspace_id=$1 FOR UPDATE`, workspaceID).Scan(&volumeOwner, &volumeState, &provisionedOwner); errors.Is(err, pgx.ErrNoRows) {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeWorkspaceBusy, "Workspace ownership transfer result remains unknown because volume ownership is unavailable")
	} else if err != nil {
		return workspace.OwnershipTransfer{}, fmt.Errorf("read Workspace volume during ownership transfer reconciliation: %w", err)
	}
	if volumeOwner != value.FromOwnerID || volumeState != "ready" || provisionedOwner == "" {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeWorkspaceBusy, "Workspace ownership transfer result remains unknown because volume ownership changed")
	}
	updated, err := tx.Exec(ctx, `UPDATE workspace_ownership_transfers
SET state='pending', operation_id=NULL, version=version+1, updated_at=clock_timestamp()
WHERE id=$1 AND workspace_id=$2 AND state='unknown' AND operation_id=$3`, transferID, workspaceID, value.OperationID)
	if err != nil {
		return workspace.OwnershipTransfer{}, fmt.Errorf("reset Workspace ownership transfer after safe reconciliation: %w", err)
	}
	if updated.RowsAffected() != 1 {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeWorkspaceBusy, "Workspace ownership transfer changed during reconciliation")
	}
	result, err := getWorkspaceOwnershipTransfer(ctx, tx, actorID, workspaceID, transferID, false)
	if err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return workspace.OwnershipTransfer{}, fmt.Errorf("commit safe Workspace ownership transfer reconciliation: %w", err)
	}
	return result, nil
}

func (s *Store) StartWorkspaceOwnershipTransferAcceptance(ctx context.Context, actorID, workspaceID, transferID, operationID string,
	record app.IdempotencyRecord) (workspace.OwnershipTransfer, bool, error) {
	if !domain.ValidUserID(actorID) || !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidOwnershipTransferID(transferID) ||
		!domain.ValidOperationID(operationID) || record.OwnerID != actorID || record.Operation != "workspace.transfer.accept" {
		return workspace.OwnershipTransfer{}, false, domain.InvalidArgument("ownership_transfer", "workspace ownership acceptance is invalid")
	}
	if err := validateIdempotency(record); err != nil {
		return workspace.OwnershipTransfer{}, false, err
	}

	var result workspace.OwnershipTransfer
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceMutationFence(ctx, tx, workspaceID, true); err != nil {
			return err
		}
		stored, found, err := findIdempotency(ctx, tx, record)
		if err != nil {
			return err
		}
		if found {
			value, err := getWorkspaceOwnershipTransfer(ctx, tx, actorID, workspaceID, stored.ResourceID, false)
			if err != nil {
				return err
			}
			if value.ToOwnerID != actorID || value.ID != transferID {
				return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is bound to another Workspace transfer")
			}
			if value.State != workspace.OwnershipTransferPending {
				result, replayed = value, true
				return nil
			}
			replayed = true
		}

		if err := lockTransferWorkspace(ctx, tx, workspaceID); err != nil {
			return err
		}
		value, err := getWorkspaceOwnershipTransfer(ctx, tx, actorID, workspaceID, transferID, true)
		if err != nil {
			return err
		}
		if value.ToOwnerID != actorID {
			return domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
		}
		if value.State == workspace.OwnershipTransferPending {
			var currentOwner, state string
			var revision int64
			var deleted bool
			if err := tx.QueryRow(ctx, `SELECT owner_id, state, revision, deleted_at IS NOT NULL
FROM workspaces WHERE id=$1`, workspaceID).Scan(&currentOwner, &state, &revision, &deleted); err != nil {
				return fmt.Errorf("read Workspace before transfer acceptance: %w", err)
			}
			if currentOwner != value.FromOwnerID || deleted {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace owner changed before acceptance")
			}
			if state != string(workspace.StateReady) || revision != value.ExpectedRevision {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed before acceptance")
			}
			if err := requireApprovedWorkspaceTransferTarget(ctx, tx, actorID); err != nil {
				return err
			}
			if err := validateWorkspaceTransferable(ctx, tx, workspaceID, revision); err != nil {
				return err
			}
			updated, err := tx.Exec(ctx, `UPDATE workspace_ownership_transfers
SET state='accepting', operation_id=$3, version=version+1, updated_at=clock_timestamp()
WHERE id=$1 AND workspace_id=$2 AND state='pending'`, transferID, workspaceID, operationID)
			if err != nil {
				return fmt.Errorf("start Workspace ownership acceptance: %w", err)
			}
			if updated.RowsAffected() != 1 {
				return domain.NewError(domain.CodeWorkspaceConflict, "Workspace ownership transfer changed before acceptance")
			}
		} else if value.State == workspace.OwnershipTransferAccepting || value.State == workspace.OwnershipTransferUnknown {
			// The persisted operation ID fences retries from starting a second acceptance.
		} else if value.State != workspace.OwnershipTransferAccepted {
			return domain.NewError(domain.CodeInvalidState, "Workspace ownership transfer is no longer pending")
		}

		result, err = getWorkspaceOwnershipTransfer(ctx, tx, actorID, workspaceID, transferID, false)
		if err != nil {
			return err
		}
		record.RelatedID = result.OperationID
		_, reserveReplayed, err := reserveIdempotency(ctx, tx, record, transferID, record.RelatedID)
		if err != nil {
			return err
		}
		if reserveReplayed {
			if _, err := tx.Exec(ctx, `UPDATE idempotency_keys SET related_id=$5
WHERE owner_id=$1 AND operation=$2 AND key=$3 AND request_hash=$4`,
				record.OwnerID, record.Operation, record.Key, record.RequestHash, record.RelatedID); err != nil {
				return fmt.Errorf("refresh Workspace acceptance idempotency operation: %w", err)
			}
		}
		replayed = replayed || reserveReplayed
		return nil
	})
	if err != nil {
		return workspace.OwnershipTransfer{}, false, err
	}
	return result, replayed, nil
}

func (s *Store) FinishWorkspaceOwnershipTransferAcceptance(ctx context.Context, actorID, workspaceID, transferID, operationID string,
	maxOwnerBytes int64) (workspace.OwnershipTransfer, error) {
	return s.FinishWorkspaceOwnershipTransferAcceptanceWithReadOnly(ctx, actorID, workspaceID, transferID, operationID, maxOwnerBytes, false)
}

func (s *Store) FinishWorkspaceOwnershipTransferAcceptanceWithReadOnly(ctx context.Context, actorID, workspaceID, transferID, operationID string,
	maxOwnerBytes int64, readOnly bool) (workspace.OwnershipTransfer, error) {
	if !domain.ValidUserID(actorID) || !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidOwnershipTransferID(transferID) ||
		!domain.ValidOperationID(operationID) || maxOwnerBytes <= 0 {
		return workspace.OwnershipTransfer{}, domain.InvalidArgument("ownership_transfer", "workspace ownership acceptance is invalid")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return workspace.OwnershipTransfer{}, fmt.Errorf("begin Workspace ownership acceptance: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := lockWorkspaceMutationFence(ctx, tx, workspaceID, true); err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	if err := lockWorkspaceProjectResourcesForTransfer(ctx, tx, workspaceID); err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	if err := lockTransferWorkspace(ctx, tx, workspaceID); err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	value, err := getWorkspaceOwnershipTransfer(ctx, tx, actorID, workspaceID, transferID, true)
	if err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	if value.ToOwnerID != actorID {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
	}
	if value.State == workspace.OwnershipTransferAccepted {
		if value.OperationID != operationID {
			return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace ownership transfer operation does not match")
		}
		var storedReadOnly bool
		if err := tx.QueryRow(ctx, `SELECT is_read_only FROM workspaces WHERE id=$1`, workspaceID).Scan(&storedReadOnly); err != nil {
			return workspace.OwnershipTransfer{}, storeError("verify Workspace ownership acceptance mode", "workspace", err)
		}
		if storedReadOnly != readOnly {
			return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace ownership acceptance mode does not match")
		}
		if err := tx.Commit(ctx); err != nil {
			return workspace.OwnershipTransfer{}, fmt.Errorf("finish Workspace ownership acceptance read: %w", err)
		}
		return value, nil
	}
	if value.State == workspace.OwnershipTransferUnknown {
		return value, domain.NewError(domain.CodeWorkspaceBusy, "Workspace ownership transfer result is unknown and requires reconciliation")
	}
	if value.State != workspace.OwnershipTransferAccepting || value.OperationID != operationID {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace ownership transfer operation changed")
	}

	var failureAfterCommit error
	var accepted workspace.OwnershipTransfer
	failToPending := func(businessErr error) error {
		updated, updateErr := tx.Exec(ctx, `UPDATE workspace_ownership_transfers
SET state='pending', operation_id=NULL, version=version+1, updated_at=clock_timestamp()
WHERE id=$1 AND workspace_id=$2 AND state='accepting' AND operation_id=$3`, transferID, workspaceID, operationID)
		if updateErr != nil {
			return fmt.Errorf("reset rejected Workspace transfer acceptance: %w", updateErr)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace ownership transfer changed before retry")
		}
		failureAfterCommit = businessErr
		return nil
	}
	var state, currentOwner string
	var revision int64
	var deleted bool
	if err := tx.QueryRow(ctx, `SELECT owner_id, state, revision, deleted_at IS NOT NULL
FROM workspaces WHERE id=$1`, workspaceID).Scan(&currentOwner, &state, &revision, &deleted); err != nil {
		return workspace.OwnershipTransfer{}, fmt.Errorf("read Workspace during transfer acceptance: %w", err)
	}
	if currentOwner != value.FromOwnerID || deleted || state != string(workspace.StateReady) || revision != value.ExpectedRevision {
		failure := domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed before ownership acceptance")
		if err := failToPending(failure); err != nil {
			return workspace.OwnershipTransfer{}, err
		}
	} else if err := requireApprovedWorkspaceTransferTarget(ctx, tx, actorID); err != nil {
		if resetErr := failToPending(err); resetErr != nil {
			return workspace.OwnershipTransfer{}, resetErr
		}
	} else if err := validateWorkspaceTransferable(ctx, tx, workspaceID, revision); err != nil {
		if resetErr := failToPending(err); resetErr != nil {
			return workspace.OwnershipTransfer{}, resetErr
		}
	} else {
		accepted, failureAfterCommit, err = applyWorkspaceOwnershipTransfer(ctx, tx, actorID, value, maxOwnerBytes, readOnly)
		if err != nil {
			if domain.CodeOf(err) == domain.CodeQuotaExceeded {
				if resetErr := failToPending(err); resetErr != nil {
					return workspace.OwnershipTransfer{}, resetErr
				}
			} else {
				return workspace.OwnershipTransfer{}, err
			}
		}
		if failureAfterCommit != nil {
			if domain.CodeOf(failureAfterCommit) != domain.CodeQuotaExceeded {
				return workspace.OwnershipTransfer{}, failureAfterCommit
			}
			if resetErr := failToPending(failureAfterCommit); resetErr != nil {
				return workspace.OwnershipTransfer{}, resetErr
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		markErr := s.markWorkspaceOwnershipTransferUnknown(transferID, workspaceID, operationID)
		if markErr != nil {
			return workspace.OwnershipTransfer{}, errors.Join(fmt.Errorf("commit Workspace ownership acceptance: %w", err), markErr)
		}
		reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		current, readErr := getWorkspaceOwnershipTransfer(reconcileCtx, s.pool, actorID, workspaceID, transferID, false)
		if readErr == nil && current.State == workspace.OwnershipTransferAccepted && current.OperationID == operationID {
			return current, nil
		}
		if readErr == nil && current.State == workspace.OwnershipTransferUnknown {
			return current, domain.NewError(domain.CodeWorkspaceBusy, "Workspace ownership transfer result is unknown and requires reconciliation")
		}
		return workspace.OwnershipTransfer{}, errors.Join(fmt.Errorf("commit Workspace ownership acceptance: %w", err), readErr)
	}
	if failureAfterCommit != nil {
		return workspace.OwnershipTransfer{}, failureAfterCommit
	}
	return accepted, nil
}

func (s *Store) CancelWorkspaceOwnershipTransfer(ctx context.Context, actorID, workspaceID, transferID string,
	record app.IdempotencyRecord) (workspace.OwnershipTransfer, error) {
	if !domain.ValidUserID(actorID) || !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidOwnershipTransferID(transferID) ||
		record.OwnerID != actorID || record.Operation != "workspace.transfer.cancel" {
		return workspace.OwnershipTransfer{}, domain.InvalidArgument("ownership_transfer", "workspace ownership cancellation is invalid")
	}
	if err := validateIdempotency(record); err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	var result workspace.OwnershipTransfer
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceMutationFence(ctx, tx, workspaceID, true); err != nil {
			return err
		}
		stored, found, err := findIdempotency(ctx, tx, record)
		if err != nil {
			return err
		}
		if found {
			value, err := getWorkspaceOwnershipTransfer(ctx, tx, actorID, workspaceID, stored.ResourceID, false)
			if err != nil {
				return err
			}
			if value.FromOwnerID != actorID || value.ID != transferID || value.State != workspace.OwnershipTransferCancelled {
				return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is bound to another Workspace transfer")
			}
			result = value
			return nil
		}
		if err := lockTransferWorkspace(ctx, tx, workspaceID); err != nil {
			return err
		}
		value, err := getWorkspaceOwnershipTransfer(ctx, tx, actorID, workspaceID, transferID, true)
		if err != nil {
			return err
		}
		if value.FromOwnerID != actorID {
			return domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
		}
		if value.State == workspace.OwnershipTransferCancelled {
			result = value
			_, _, err = reserveIdempotency(ctx, tx, record, transferID, "")
			return err
		}
		if value.State != workspace.OwnershipTransferPending {
			return domain.NewError(domain.CodeWorkspaceBusy, "Workspace ownership transfer can no longer be cancelled")
		}
		if _, _, err := reserveIdempotency(ctx, tx, record, transferID, ""); err != nil {
			return err
		}
		updated, err := tx.Exec(ctx, `UPDATE workspace_ownership_transfers
SET state='cancelled', version=version+1, updated_at=clock_timestamp()
WHERE id=$1 AND workspace_id=$2 AND state='pending'`, transferID, workspaceID)
		if err != nil {
			return fmt.Errorf("cancel Workspace ownership transfer: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace ownership transfer changed before cancellation")
		}
		result, err = getWorkspaceOwnershipTransfer(ctx, tx, actorID, workspaceID, transferID, false)
		return err
	})
	if err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	return result, nil
}

func lockTransferWorkspace(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	var lockedID string
	if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, workspaceID).Scan(&lockedID); errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
	} else if err != nil {
		return fmt.Errorf("lock Workspace during ownership transfer: %w", err)
	}
	return nil
}

func getWorkspaceOwnershipTransfer(ctx context.Context, queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, actorID, workspaceID, transferID string, forUpdate bool) (workspace.OwnershipTransfer, error) {
	if !domain.ValidUserID(actorID) || !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidOwnershipTransferID(transferID) {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
	}
	query := `SELECT id, workspace_id, from_owner_id, to_owner_id, state, expected_revision, operation_id,
version, created_at, updated_at, accepted_at
FROM workspace_ownership_transfers WHERE id=$1 AND workspace_id=$2 AND (from_owner_id=$3 OR to_owner_id=$3)`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var value workspace.OwnershipTransfer
	var operationID pgtype.Text
	var acceptedAt pgtype.Timestamptz
	err := queryer.QueryRow(ctx, query, transferID, workspaceID, actorID).Scan(
		&value.ID, &value.WorkspaceID, &value.FromOwnerID, &value.ToOwnerID, &value.State, &value.ExpectedRevision,
		&operationID, &value.Version, &value.CreatedAt, &value.UpdatedAt, &acceptedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
	}
	if err != nil {
		return workspace.OwnershipTransfer{}, fmt.Errorf("read Workspace ownership transfer: %w", err)
	}
	if operationID.Valid {
		value.OperationID = operationID.String
	}
	if acceptedAt.Valid {
		accepted := acceptedAt.Time
		value.AcceptedAt = &accepted
	}
	if err := value.Validate(); err != nil {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeInternal, "stored Workspace ownership transfer is invalid")
	}
	return value, nil
}

func requireApprovedWorkspaceTransferTarget(ctx context.Context, tx pgx.Tx, ownerID string) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1`, ownerID).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "Workspace transfer target account not found")
	} else if err != nil {
		return fmt.Errorf("read Workspace transfer target account: %w", err)
	}
	if status != string(identity.StatusApproved) {
		return domain.NewError(domain.CodeForbidden, "Workspace transfer target account is not approved")
	}
	return nil
}

func validateWorkspaceTransferable(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64) error {
	var blocked bool
	if err := tx.QueryRow(ctx, `SELECT
EXISTS (SELECT 1 FROM workspace_bindings WHERE workspace_id=$1 AND state <> 'detached') OR
EXISTS (SELECT 1 FROM workspace_sync_sessions WHERE workspace_id=$1 AND state IN ('planned','transferring','verifying','applying')) OR
EXISTS (SELECT 1 FROM workspace_sync_receive_reservations WHERE workspace_id=$1 AND state IN ('reserved','aborting')) OR
EXISTS (SELECT 1 FROM tasks WHERE workspace_id=$1 AND status NOT IN ('completed','failed','cancelled')) OR
EXISTS (SELECT 1 FROM task_operations o JOIN tasks t ON t.id=o.task_id WHERE t.workspace_id=$1 AND o.status IN ('prepared','dispatched','unknown')) OR
EXISTS (SELECT 1 FROM task_mcp_connector_operations o JOIN tasks t ON t.id=o.task_id WHERE t.workspace_id=$1 AND o.status IN ('prepared','dispatched','unknown')) OR
EXISTS (SELECT 1 FROM task_workspace_publish_receipts WHERE workspace_id=$1 AND state='pending') OR
EXISTS (SELECT 1 FROM task_hub_operations WHERE workspace_id=$1 AND status IN ('prepared','dispatched','unknown')) OR
EXISTS (SELECT 1 FROM workspace_copy_reservations WHERE source_workspace_id=$1 AND state IN ('reserved','copying','unknown'))`, workspaceID).Scan(&blocked); err != nil {
		return fmt.Errorf("check active Workspace references before transfer: %w", err)
	}
	if blocked {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has an active binding, Task, synchronization session, or publish")
	}

	var unsupported bool
	if err := tx.QueryRow(ctx, `SELECT
EXISTS (SELECT 1 FROM sites WHERE workspace_id=$1) OR
EXISTS (SELECT 1 FROM workspace_legacy_volumes WHERE workspace_id=$1) OR
NOT EXISTS (SELECT 1 FROM workspace_volumes WHERE workspace_id=$1 AND state='ready'
  AND volume_name='workspace-' || workspace_id AND legacy_host_id IS NULL AND ready_at IS NOT NULL)`, workspaceID).Scan(&unsupported); err != nil {
		return fmt.Errorf("check unsupported Workspace references before transfer: %w", err)
	}
	if unsupported {
		return domain.NewError(domain.CodeUnsupported, "Workspace has Site, legacy-volume, or unverified-volume references that cannot be transferred safely")
	}

	var desiredRevision, appliedRevision int64
	if err := tx.QueryRow(ctx, `SELECT desired_revision, applied_revision FROM workspace_current_materializations WHERE workspace_id=$1`, workspaceID).
		Scan(&desiredRevision, &appliedRevision); errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace current tree is not materialized")
	} else if err != nil {
		return fmt.Errorf("read Workspace current materialization before transfer: %w", err)
	}
	if desiredRevision != revision || appliedRevision != revision {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace current tree is not fully materialized")
	}

	var revisionFiles, manifestFiles, revisionBytes, manifestBytes, usedBytes int64
	if err := tx.QueryRow(ctx, `SELECT r.file_count, r.total_bytes, count(f.file_path), COALESCE(sum(f.size_bytes),0), w.used_bytes
FROM workspace_revisions r
JOIN workspaces w ON w.id=r.workspace_id
LEFT JOIN workspace_revision_files f ON f.workspace_id=r.workspace_id
  AND f.valid_from_revision <= r.revision AND (f.valid_to_revision IS NULL OR f.valid_to_revision > r.revision)
WHERE r.workspace_id=$1 AND r.revision=$2
GROUP BY r.file_count, r.total_bytes, w.used_bytes`, workspaceID, revision).Scan(
		&revisionFiles, &revisionBytes, &manifestFiles, &manifestBytes, &usedBytes,
	); errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace current revision metadata is missing")
	} else if err != nil {
		return fmt.Errorf("verify Workspace current revision before transfer: %w", err)
	}
	if revisionFiles != manifestFiles || revisionBytes != manifestBytes || usedBytes != manifestBytes {
		return domain.NewError(domain.CodeWorkspaceConflict, "Workspace current revision totals do not match its manifest")
	}

	var hasUnsettledBlocks bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workspace_block_usage
WHERE workspace_id=$1 AND (state <> 'stored' OR reserved_bytes <> 0))`, workspaceID).Scan(&hasUnsettledBlocks); err != nil {
		return fmt.Errorf("check Workspace physical block reservations before transfer: %w", err)
	}
	if hasUnsettledBlocks {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace has unsettled physical block reservations")
	}
	return nil
}

func applyWorkspaceOwnershipTransfer(ctx context.Context, tx pgx.Tx, actorID string, transfer workspace.OwnershipTransfer, maxOwnerBytes int64,
	readOnly bool) (workspace.OwnershipTransfer, error, error) {
	ownerIDs := []string{transfer.FromOwnerID, transfer.ToOwnerID}
	sort.Strings(ownerIDs)
	usage := make(map[string][2]int64, len(ownerIDs))
	for _, ownerID := range ownerIDs {
		allocated, reserved, err := lockWorkspaceOwnerUsage(ctx, tx, ownerID)
		if err != nil {
			return workspace.OwnershipTransfer{}, nil, err
		}
		usage[ownerID] = [2]int64{allocated, reserved}
	}

	var sourceAllocated, sourceReserved, targetAllocated, targetReserved int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(allocated_bytes),0), COALESCE(sum(reserved_bytes),0)
FROM workspace_block_usage WHERE workspace_id=$1`, transfer.WorkspaceID).Scan(&sourceAllocated, &sourceReserved); err != nil {
		return workspace.OwnershipTransfer{}, nil, fmt.Errorf("sum Workspace physical usage before transfer: %w", err)
	}
	if sourceReserved != 0 {
		return workspace.OwnershipTransfer{}, nil, domain.NewError(domain.CodeWorkspaceBusy, "Workspace has unsettled physical block reservations")
	}

	for _, ownerID := range ownerIDs {
		var actualAllocated, actualReserved int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(usage.allocated_bytes),0), COALESCE(sum(usage.reserved_bytes),0)
FROM workspaces w JOIN workspace_block_usage usage ON usage.workspace_id=w.id
WHERE w.owner_id=$1`, ownerID).Scan(&actualAllocated, &actualReserved); err != nil {
			return workspace.OwnershipTransfer{}, nil, fmt.Errorf("verify owner physical usage before transfer: %w", err)
		}
		stored := usage[ownerID]
		if stored[0] != actualAllocated || stored[1] != actualReserved {
			return workspace.OwnershipTransfer{}, nil, domain.NewError(domain.CodeInternal, "workspace owner physical usage counters are inconsistent")
		}
		if ownerID == transfer.FromOwnerID {
			if stored[0] < sourceAllocated || stored[1] < sourceReserved {
				return workspace.OwnershipTransfer{}, nil, domain.NewError(domain.CodeInternal, "workspace owner physical usage counters are inconsistent")
			}
		} else {
			targetAllocated, targetReserved = stored[0], stored[1]
		}
	}
	if targetAllocated > maxOwnerBytes || targetReserved > maxOwnerBytes-targetAllocated || sourceAllocated > maxOwnerBytes-targetAllocated-targetReserved {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeQuotaExceeded, "Workspace transfer would exceed the target owner's physical storage quota"), nil
	}
	var currentOwner string
	if err := tx.QueryRow(ctx, `SELECT owner_id FROM workspace_ownership_history
WHERE workspace_id=$1 AND ended_at IS NULL FOR UPDATE`, transfer.WorkspaceID).Scan(&currentOwner); errors.Is(err, pgx.ErrNoRows) {
		return workspace.OwnershipTransfer{}, nil, domain.NewError(domain.CodeInternal, "Workspace current ownership history is missing")
	} else if err != nil {
		return workspace.OwnershipTransfer{}, nil, fmt.Errorf("lock Workspace ownership history: %w", err)
	}
	if currentOwner != transfer.FromOwnerID {
		return workspace.OwnershipTransfer{}, nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace ownership history changed before acceptance")
	}
	if err := revokeWorkspaceProjectResourcesForTransfer(ctx, tx, transfer.WorkspaceID, transfer.FromOwnerID, actorID); err != nil {
		return workspace.OwnershipTransfer{}, nil, err
	}
	if err := clearWorkspaceSyncDevicesForOwnershipTransfer(ctx, tx, transfer.WorkspaceID, transfer.FromOwnerID); err != nil {
		return workspace.OwnershipTransfer{}, nil, err
	}
	oldUsage := usage[transfer.FromOwnerID]
	newUsage := usage[transfer.ToOwnerID]
	if _, err := tx.Exec(ctx, `UPDATE workspace_owner_block_usage
SET allocated_bytes=$2, reserved_bytes=$3, updated_at=clock_timestamp() WHERE owner_id=$1`, transfer.FromOwnerID,
		oldUsage[0]-sourceAllocated, oldUsage[1]-sourceReserved); err != nil {
		return workspace.OwnershipTransfer{}, nil, fmt.Errorf("debit former Workspace owner usage: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE workspace_owner_block_usage
SET allocated_bytes=$2, reserved_bytes=$3, updated_at=clock_timestamp() WHERE owner_id=$1`, transfer.ToOwnerID,
		newUsage[0]+sourceAllocated, newUsage[1]+sourceReserved); err != nil {
		return workspace.OwnershipTransfer{}, nil, fmt.Errorf("credit new Workspace owner usage: %w", err)
	}
	closed, err := tx.Exec(ctx, `UPDATE workspace_ownership_history SET ended_at=clock_timestamp()
WHERE workspace_id=$1 AND owner_id=$2 AND ended_at IS NULL`, transfer.WorkspaceID, transfer.FromOwnerID)
	if err != nil {
		return workspace.OwnershipTransfer{}, nil, fmt.Errorf("close former Workspace ownership history: %w", err)
	}
	if closed.RowsAffected() != 1 {
		return workspace.OwnershipTransfer{}, nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace current owner changed before acceptance")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workspace_ownership_history
(workspace_id, owner_id, transfer_id, started_at, ended_at)
VALUES ($1,$2,$3,clock_timestamp(),NULL)
ON CONFLICT (workspace_id,owner_id) DO UPDATE SET
  transfer_id=EXCLUDED.transfer_id, started_at=EXCLUDED.started_at, ended_at=NULL`, transfer.WorkspaceID, transfer.ToOwnerID, transfer.ID); err != nil {
		return workspace.OwnershipTransfer{}, nil, fmt.Errorf("record new Workspace owner: %w", err)
	}
	workspaceUpdate, err := tx.Exec(ctx, `UPDATE workspaces SET owner_id=$2, is_read_only=$5, updated_at=clock_timestamp()
WHERE id=$1 AND owner_id=$3 AND revision=$4 AND state='ready' AND deleted_at IS NULL`,
		transfer.WorkspaceID, transfer.ToOwnerID, transfer.FromOwnerID, transfer.ExpectedRevision, readOnly)
	if err != nil {
		return workspace.OwnershipTransfer{}, nil, fmt.Errorf("update Workspace owner: %w", err)
	}
	if workspaceUpdate.RowsAffected() != 1 {
		return workspace.OwnershipTransfer{}, nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed during ownership acceptance")
	}
	volumeUpdate, err := tx.Exec(ctx, `UPDATE workspace_volumes SET owner_id=$2, updated_at=clock_timestamp()
WHERE workspace_id=$1 AND owner_id=$3 AND state='ready' AND provisioned_owner_id IS NOT NULL`,
		transfer.WorkspaceID, transfer.ToOwnerID, transfer.FromOwnerID)
	if err != nil {
		return workspace.OwnershipTransfer{}, nil, fmt.Errorf("update Workspace volume owner: %w", err)
	}
	if volumeUpdate.RowsAffected() != 1 {
		return workspace.OwnershipTransfer{}, nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace volume changed during ownership acceptance")
	}
	now := time.Now().UTC()
	transferUpdate, err := tx.Exec(ctx, `UPDATE workspace_ownership_transfers SET state='accepted', accepted_at=$4,
version=version+1, updated_at=$4 WHERE id=$1 AND workspace_id=$2 AND state='accepting' AND operation_id=$3`,
		transfer.ID, transfer.WorkspaceID, transfer.OperationID, now)
	if err != nil {
		return workspace.OwnershipTransfer{}, nil, fmt.Errorf("complete Workspace ownership transfer: %w", err)
	}
	if transferUpdate.RowsAffected() != 1 {
		return workspace.OwnershipTransfer{}, nil, domain.NewError(domain.CodeWorkspaceConflict, "Workspace ownership transfer operation changed")
	}
	updated, err := getWorkspaceOwnershipTransfer(ctx, tx, transfer.ToOwnerID, transfer.WorkspaceID, transfer.ID, false)
	if err != nil {
		return workspace.OwnershipTransfer{}, nil, err
	}
	return updated, nil, nil
}

// clearWorkspaceSyncDevicesForOwnershipTransfer fences the former owner's
// device authorization in the same transaction that changes Workspace owner.
// Installation identities are intentionally retained: an identity can be
// shared by bindings to other Workspaces owned by the same account. The
// Workspace-scoped rows must be removed because their composite foreign keys
// bind them to the old workspaces.owner_id; no per-binding history table exists.
func clearWorkspaceSyncDevicesForOwnershipTransfer(ctx context.Context, tx pgx.Tx, workspaceID, ownerID string) error {
	var nodeStateChanged bool
	if err := tx.QueryRow(ctx, `SELECT
  EXISTS (SELECT 1 FROM workspace_sync_folders WHERE workspace_id=$1 AND owner_id=$2) OR
  EXISTS (SELECT 1 FROM workspace_sync_device_bindings WHERE workspace_id=$1 AND owner_id=$2)`,
		workspaceID, ownerID).Scan(&nodeStateChanged); err != nil {
		return fmt.Errorf("check Workspace sync state before ownership transfer: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_sync_device_registration_requests
WHERE workspace_id=$1 AND owner_id=$2`, workspaceID, ownerID); err != nil {
		return fmt.Errorf("clear Workspace sync registration requests during ownership transfer: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_sync_device_challenges
WHERE workspace_id=$1 AND owner_id=$2`, workspaceID, ownerID); err != nil {
		return fmt.Errorf("clear Workspace sync challenges during ownership transfer: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_sync_device_bindings
WHERE workspace_id=$1 AND owner_id=$2`, workspaceID, ownerID); err != nil {
		return fmt.Errorf("clear Workspace sync device bindings during ownership transfer: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM workspace_sync_folders
WHERE workspace_id=$1 AND owner_id=$2`, workspaceID, ownerID); err != nil {
		return fmt.Errorf("clear Workspace sync folder during ownership transfer: %w", err)
	}
	if nodeStateChanged {
		if _, err := bumpWorkspaceSyncNodeDesiredRevision(ctx, tx, ownerID); err != nil {
			return fmt.Errorf("schedule former owner's Workspace sync node cleanup: %w", err)
		}
	}
	return nil
}

func (s *Store) markWorkspaceOwnershipTransferUnknown(transferID, workspaceID, operationID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockWorkspaceMutationFence(ctx, tx, workspaceID, true); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE workspace_ownership_transfers SET state='unknown', version=version+1,
updated_at=clock_timestamp() WHERE id=$1 AND workspace_id=$2 AND state='accepting' AND operation_id=$3`,
			transferID, workspaceID, operationID)
		if err != nil {
			return fmt.Errorf("mark Workspace ownership transfer outcome unknown: %w", err)
		}
		return nil
	})
}
