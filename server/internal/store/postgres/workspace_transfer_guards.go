package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
)

func requireWorkspaceOwnershipTransferNotFrozen(ctx context.Context, tx pgx.Tx, workspaceID string) error {
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
WHERE (source_workspace_id=$1 OR target_workspace_id=$1) AND state IN ('reserved','copying','unknown')
)`, workspaceID).Scan(&frozen); err != nil {
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
