package postgres

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type workspaceMutationPermitKey struct{}

type workspaceMutationPermit struct {
	workspaceID, workspaceOwnerID string
	projectID, actorID, taskID    string
	taskOwnerID, leaseOwner       string
	grantHostID, taskHostID       string
	agentID, agentSessionID       string
	agentGeneration, leaseVersion int64
	resourceVersion               int64
	write                         bool
	authorization                 task.Authorization
	active                        atomic.Bool
}

// Serialize project publication with the creation and progression of a
// Workspace ownership/copy freeze without holding a row lock across block IO.
// Every participant takes this key before any project or Workspace row lock.
func lockWorkspaceMutationFence(ctx context.Context, tx pgx.Tx, workspaceID string, exclusive bool) error {
	if !domain.ValidWorkspaceID(workspaceID) {
		return domain.InvalidArgument("workspace_id", "workspace identity is invalid")
	}
	if !exclusive {
		permit, _ := ctx.Value(workspaceMutationPermitKey{}).(*workspaceMutationPermit)
		if permit != nil && permit.workspaceID == workspaceID && permit.active.Load() {
			return nil
		}
	}
	query := `SELECT pg_advisory_xact_lock_shared(hashtextextended('morde:workspace-mutation:' || $1,0))`
	if exclusive {
		query = `SELECT pg_advisory_xact_lock(hashtextextended('morde:workspace-mutation:' || $1,0))`
	}
	if _, err := tx.Exec(ctx, query, workspaceID); err != nil {
		return fmt.Errorf("lock Workspace mutation fence: %w", err)
	}
	return nil
}

func requireWorkspaceOwnershipTransferNotFrozen(ctx context.Context, tx pgx.Tx, workspaceID string) error {
	return requireWorkspaceOwnershipTransferNotFrozenExceptCopy(ctx, tx, workspaceID, "")
}

func requireWorkspaceOwnershipTransferNotFrozenExceptCopy(ctx context.Context, tx pgx.Tx, workspaceID, exceptCopyReservationID string) error {
	var frozen bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_ownership_transfers
WHERE workspace_id=$1 AND state IN ('pending','accepting','unknown')
)`, workspaceID).Scan(&frozen); err != nil {
		return fmt.Errorf("check Workspace ownership transfer freeze: %w", err)
	}
	if frozen {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace ownership transfer is in progress")
	}
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_copy_reservations
WHERE (source_workspace_id=$1 OR target_workspace_id=$1)
  AND (state IN ('reserved','copying','unknown') OR host_copy_pending)
  AND ($2='' OR id<>$2)
)`, workspaceID, exceptCopyReservationID).Scan(&frozen); err != nil {
		return fmt.Errorf("check Workspace copy freeze: %w", err)
	}
	if frozen {
		return domain.NewError(domain.CodeWorkspaceBusy, "Workspace copy is in progress")
	}
	return nil
}

func lockWorkspaceOwnershipForRead(ctx context.Context, tx pgx.Tx, workspaceID string) (string, error) {
	var ownerID string
	if err := tx.QueryRow(ctx, `SELECT owner_id FROM workspaces WHERE id=$1 FOR SHARE`, workspaceID).Scan(&ownerID); err != nil {
		return "", storeError("lock Workspace before reading its files", "workspace", err)
	}
	if err := requireWorkspaceOwnershipTransferNotFrozen(ctx, tx, workspaceID); err != nil {
		return "", err
	}
	return ownerID, nil
}
