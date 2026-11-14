package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

func (s *Store) CreateWorkspaceShare(ctx context.Context, value workspace.ShareLink) (workspace.ShareLink, error) {
	if !domain.ValidPendingRequestID(value.ID) || !domain.ValidUserID(value.OwnerID) || !domain.ValidWorkspaceID(value.SourceWorkspaceID) ||
		value.SourceRevision < 0 || (value.Permission != workspace.ShareView && value.Permission != workspace.ShareEdit) ||
		len(value.TokenHash) != 32 || value.ExpiresAt.IsZero() || value.State != workspace.SharePending ||
		value.ClaimantID != "" || value.TargetWorkspaceID != "" || value.ClaimedAt != nil {
		return workspace.ShareLink{}, domain.InvalidArgument("workspace_share", "Workspace share link fields are invalid")
	}
	var created workspace.ShareLink
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID, name, state string
		var revision int64
		var readOnly, deleted bool
		if err := tx.QueryRow(ctx, `SELECT owner_id,name,state,revision,is_read_only,deleted_at IS NOT NULL
FROM workspaces WHERE id=$1 FOR UPDATE`, value.SourceWorkspaceID).Scan(&ownerID, &name, &state, &revision, &readOnly, &deleted); err != nil {
			return storeError("read Workspace before creating share link", "workspace", err)
		}
		if ownerID != value.OwnerID || deleted {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		bound, err := workspaceHasHostBindingTx(ctx, tx, value.SourceWorkspaceID)
		if err != nil {
			return err
		}
		if bound {
			return domain.NewError(domain.CodeForbidden, "computer storage cannot be shared as an independent Workspace")
		}
		if readOnly {
			return domain.NewError(domain.CodeForbidden, "workspace is shared as read-only")
		}
		if state != string(workspace.StateReady) || revision != value.SourceRevision {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed before share link creation")
		}
		value.WorkspaceName = name
		value.CreatedAt = time.Now().UTC()
		row := tx.QueryRow(ctx, `INSERT INTO workspace_share_links
(id,owner_id,source_workspace_id,source_revision,workspace_name,permission,token_hash,expires_at,state)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'pending')
RETURNING id,owner_id,source_workspace_id,source_revision,workspace_name,permission,token_hash,expires_at,state,
COALESCE(claimant_id,''),COALESCE(target_workspace_id,''),created_at,claimed_at`, value.ID, value.OwnerID,
			value.SourceWorkspaceID, value.SourceRevision, value.WorkspaceName, string(value.Permission), value.TokenHash, value.ExpiresAt)
		return scanWorkspaceShare(row, &created)
	})
	if err != nil {
		return workspace.ShareLink{}, err
	}
	return created, nil
}

func (s *Store) GetWorkspaceShareByToken(ctx context.Context, tokenHash []byte) (workspace.ShareLink, error) {
	if len(tokenHash) != 32 {
		return workspace.ShareLink{}, domain.NewError(domain.CodeNotFound, "Workspace share link not found")
	}
	var result workspace.ShareLink
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT id,owner_id,source_workspace_id,source_revision,workspace_name,permission,token_hash,expires_at,state,
COALESCE(claimant_id,''),COALESCE(target_workspace_id,''),created_at,claimed_at
FROM workspace_share_links WHERE token_hash=$1 FOR UPDATE`, tokenHash)
		if err := scanWorkspaceShare(row, &result); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "Workspace share link not found")
			}
			return fmt.Errorf("read Workspace share link: %w", err)
		}
		if result.State == workspace.SharePending && !time.Now().Before(result.ExpiresAt) {
			if _, err := tx.Exec(ctx, `UPDATE workspace_share_links SET state='expired' WHERE id=$1 AND state='pending'`, result.ID); err != nil {
				return fmt.Errorf("expire Workspace share link: %w", err)
			}
			return domain.NewError(domain.CodeNotFound, "Workspace share link not found")
		}
		if result.State == workspace.SharePending {
			bound, err := workspaceHasHostBindingTx(ctx, tx, result.SourceWorkspaceID)
			if err != nil {
				return err
			}
			if bound {
				return domain.NewError(domain.CodeNotFound, "Workspace share link not found")
			}
		}
		if err := result.Validate(); err != nil {
			return domain.NewError(domain.CodeInternal, "stored Workspace share link is invalid")
		}
		return nil
	})
	if err != nil {
		return workspace.ShareLink{}, err
	}
	return result, nil
}

func (s *Store) StartWorkspaceShareClaim(ctx context.Context, tokenHash []byte, claimantID string) (workspace.ShareLink, error) {
	if len(tokenHash) != 32 || !domain.ValidUserID(claimantID) {
		return workspace.ShareLink{}, domain.NewError(domain.CodeNotFound, "Workspace share link not found")
	}
	var result workspace.ShareLink
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT id,owner_id,source_workspace_id,source_revision,workspace_name,permission,token_hash,expires_at,state,
COALESCE(claimant_id,''),COALESCE(target_workspace_id,''),created_at,claimed_at
FROM workspace_share_links WHERE token_hash=$1 FOR UPDATE`, tokenHash)
		if err := scanWorkspaceShare(row, &result); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "Workspace share link not found")
			}
			return fmt.Errorf("lock Workspace share link for claim: %w", err)
		}
		switch result.State {
		case workspace.SharePending:
			if !time.Now().Before(result.ExpiresAt) || claimantID == result.OwnerID {
				if !time.Now().Before(result.ExpiresAt) {
					if _, err := tx.Exec(ctx, `UPDATE workspace_share_links SET state='expired' WHERE id=$1 AND state='pending'`, result.ID); err != nil {
						return fmt.Errorf("expire Workspace share link: %w", err)
					}
				}
				return domain.NewError(domain.CodeNotFound, "Workspace share link not found")
			}
			if _, err := tx.Exec(ctx, `UPDATE workspace_share_links SET state='claiming',claimant_id=$2
WHERE id=$1 AND state='pending'`, result.ID, claimantID); err != nil {
				return fmt.Errorf("reserve Workspace share claim: %w", err)
			}
			result.State, result.ClaimantID = workspace.ShareClaiming, claimantID
		case workspace.ShareClaiming:
			if result.ClaimantID != claimantID {
				return domain.NewError(domain.CodeNotFound, "Workspace share link not found")
			}
		default:
			return domain.NewError(domain.CodeNotFound, "Workspace share link not found")
		}
		var lockedWorkspaceID string
		if err := tx.QueryRow(ctx, `SELECT id FROM workspaces WHERE id=$1 FOR UPDATE`, result.SourceWorkspaceID).Scan(&lockedWorkspaceID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeNotFound, "Workspace share link not found")
			}
			return fmt.Errorf("lock Workspace share source before claim: %w", err)
		}
		bound, err := workspaceHasHostBindingTx(ctx, tx, result.SourceWorkspaceID)
		if err != nil {
			return err
		}
		if bound {
			return domain.NewError(domain.CodeNotFound, "Workspace share link not found")
		}
		if err := result.Validate(); err != nil {
			return domain.NewError(domain.CodeInternal, "stored Workspace share claim is invalid")
		}
		return nil
	})
	if err != nil {
		return workspace.ShareLink{}, err
	}
	return result, nil
}

func (s *Store) CompleteWorkspaceShareClaim(ctx context.Context, id, claimantID, targetWorkspaceID string) error {
	if !domain.ValidPendingRequestID(id) || !domain.ValidUserID(claimantID) || !domain.ValidWorkspaceID(targetWorkspaceID) {
		return domain.InvalidArgument("workspace_share", "Workspace share completion is invalid")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var state, storedClaimant, storedTarget, permission string
		if err := tx.QueryRow(ctx, `SELECT state,COALESCE(claimant_id,''),COALESCE(target_workspace_id,''),permission
FROM workspace_share_links WHERE id=$1 FOR UPDATE`, id).Scan(&state, &storedClaimant, &storedTarget, &permission); err != nil {
			return storeError("lock Workspace share claim completion", "Workspace share link", err)
		}
		alreadyClaimed := state == string(workspace.ShareClaimed) && storedClaimant == claimantID && storedTarget == targetWorkspaceID
		if !alreadyClaimed && (state != string(workspace.ShareClaiming) || storedClaimant != claimantID || storedTarget != targetWorkspaceID) {
			return domain.NewError(domain.CodeNotFound, "Workspace share link not found")
		}
		var targetOwner, targetState string
		var readOnly bool
		if err := tx.QueryRow(ctx, `SELECT owner_id,state,is_read_only FROM workspaces WHERE id=$1 FOR SHARE`, targetWorkspaceID).
			Scan(&targetOwner, &targetState, &readOnly); err != nil {
			return storeError("verify recipient-owned Workspace copy", "workspace", err)
		}
		var ownedWithoutTransfer bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_ownership_history h
WHERE h.workspace_id=$1 AND h.owner_id=$2 AND h.ended_at IS NULL AND h.transfer_id IS NULL
) AND NOT EXISTS (
SELECT 1 FROM workspace_ownership_transfers t WHERE t.workspace_id=$1
)`, targetWorkspaceID, claimantID).Scan(&ownedWithoutTransfer); err != nil {
			return fmt.Errorf("verify Workspace copy ownership history: %w", err)
		}
		wantReadOnly := permission == string(workspace.ShareView)
		if targetOwner != claimantID || targetState != string(workspace.StateReady) || readOnly != wantReadOnly || !ownedWithoutTransfer {
			return domain.NewError(domain.CodeWorkspaceConflict, "Workspace share copy is not an independent recipient-owned copy")
		}
		if !alreadyClaimed {
			if _, err := tx.Exec(ctx, `UPDATE workspace_share_links SET state='claimed',claimed_at=clock_timestamp()
WHERE id=$1 AND state='claiming' AND claimant_id=$2 AND target_workspace_id=$3`, id, claimantID, targetWorkspaceID); err != nil {
				return fmt.Errorf("complete Workspace share claim: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) InvalidateWorkspaceShare(ctx context.Context, id, claimantID string) error {
	if !domain.ValidPendingRequestID(id) || !domain.ValidUserID(claimantID) {
		return domain.InvalidArgument("workspace_share", "Workspace share invalidation is invalid")
	}
	result, err := s.pool.Exec(ctx, `UPDATE workspace_share_links SET state='invalidated',claimant_id=NULL
WHERE id=$1 AND state='claiming' AND claimant_id=$2 AND target_workspace_id IS NULL`, id, claimantID)
	if err != nil {
		return fmt.Errorf("invalidate stale Workspace share link: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeNotFound, "Workspace share link not found")
	}
	return nil
}

func (s *Store) WorkspaceShareSourceMatches(ctx context.Context, ownerID, workspaceID string, revision int64) (bool, error) {
	if !domain.ValidUserID(ownerID) || !domain.ValidWorkspaceID(workspaceID) || revision < 0 {
		return false, domain.InvalidArgument("workspace_share", "Workspace share source is invalid")
	}
	var matches bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspaces WHERE id=$1 AND owner_id=$2 AND state='ready' AND revision=$3 AND
deleted_at IS NULL AND is_read_only=false AND NOT EXISTS (
  SELECT 1 FROM workspace_bindings WHERE workspace_id=$1
))`, workspaceID, ownerID, revision).Scan(&matches)
	if err != nil {
		return false, fmt.Errorf("verify Workspace share source: %w", err)
	}
	return matches, nil
}

func workspaceHasHostBindingTx(ctx context.Context, tx pgx.Tx, workspaceID string) (bool, error) {
	var bound bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM workspace_bindings WHERE workspace_id=$1
)`, workspaceID).Scan(&bound); err != nil {
		return false, fmt.Errorf("check Host binding for Workspace: %w", err)
	}
	return bound, nil
}

type workspaceShareRow interface {
	Scan(...any) error
}

func scanWorkspaceShare(row workspaceShareRow, target *workspace.ShareLink) error {
	var permission, state string
	var claimedAt *time.Time
	err := row.Scan(&target.ID, &target.OwnerID, &target.SourceWorkspaceID, &target.SourceRevision,
		&target.WorkspaceName, &permission, &target.TokenHash, &target.ExpiresAt, &state,
		&target.ClaimantID, &target.TargetWorkspaceID, &target.CreatedAt, &claimedAt)
	if err != nil {
		return err
	}
	target.Permission, target.State, target.ClaimedAt = workspace.SharePermission(permission), workspace.ShareState(state), claimedAt
	return nil
}
