package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

const workspaceShareLifetime = 7 * 24 * time.Hour

type WorkspaceShareStore interface {
	CreateWorkspaceShare(context.Context, workspace.ShareLink) (workspace.ShareLink, error)
	GetWorkspaceShareByToken(context.Context, []byte) (workspace.ShareLink, error)
	StartWorkspaceShareClaim(context.Context, []byte, string) (workspace.ShareLink, error)
	SetWorkspaceShareTarget(context.Context, string, string, string) error
	CompleteWorkspaceShareClaim(context.Context, string, string, string) error
	InvalidateWorkspaceShare(context.Context, string, string) error
	WorkspaceShareSourceMatches(context.Context, string, string, int64) (bool, error)
}

type WorkspaceShareCopier interface {
	Copy(context.Context, identity.Principal, string, WorkspaceCopyCommand, string) (WorkspaceCopyResult, error)
}

type WorkspaceShareTransferService interface {
	RequestWorkspaceTransfer(context.Context, identity.Principal, string, string, int64, string) (workspace.OwnershipTransfer, bool, error)
	AcceptWorkspaceTransfer(context.Context, identity.Principal, string, string, string) (workspace.OwnershipTransfer, error)
	AcceptWorkspaceTransferReadOnly(context.Context, identity.Principal, string, string, string) (workspace.OwnershipTransfer, error)
}

type WorkspaceShareCreated struct {
	WorkspaceName string                    `json:"workspace_name"`
	Permission    workspace.SharePermission `json:"permission"`
	Token         string                    `json:"token"`
	ExpiresAt     time.Time                 `json:"expires_at"`
}

type WorkspaceSharePreview struct {
	WorkspaceName string                    `json:"workspace_name"`
	Permission    workspace.SharePermission `json:"permission"`
	ExpiresAt     time.Time                 `json:"expires_at"`
}

type WorkspaceShareAccepted struct {
	WorkspaceID   string                    `json:"workspace_id"`
	WorkspaceName string                    `json:"workspace_name"`
	Permission    workspace.SharePermission `json:"permission"`
}

type WorkspaceShareService struct {
	store     WorkspaceShareStore
	copies    WorkspaceShareCopier
	transfers WorkspaceShareTransferService
}

func NewWorkspaceShareService(store WorkspaceShareStore, copies WorkspaceShareCopier,
	transfers WorkspaceShareTransferService) (*WorkspaceShareService, error) {
	if store == nil || copies == nil || transfers == nil {
		return nil, errors.New("workspace share store, copy service, and transfer service are required")
	}
	return &WorkspaceShareService{store: store, copies: copies, transfers: transfers}, nil
}

func (s *WorkspaceShareService) Create(ctx context.Context, principal identity.Principal, workspaceID string,
	expectedRevision int64, permission workspace.SharePermission) (WorkspaceShareCreated, error) {
	if err := requireWorkspaceShareSession(principal); err != nil {
		return WorkspaceShareCreated{}, err
	}
	if !domain.ValidWorkspaceID(workspaceID) || expectedRevision < 0 ||
		(permission != workspace.ShareView && permission != workspace.ShareEdit) {
		return WorkspaceShareCreated{}, domain.InvalidArgument("workspace_share", "Workspace share request is invalid")
	}
	id, err := domain.NewPendingRequestID()
	if err != nil {
		return WorkspaceShareCreated{}, fmt.Errorf("generate Workspace share ID: %w", err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return WorkspaceShareCreated{}, fmt.Errorf("generate Workspace share token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	hash := sha256.Sum256(secret)
	now := time.Now().UTC()
	created, err := s.store.CreateWorkspaceShare(ctx, workspace.ShareLink{
		ID: id, OwnerID: principal.UserID, SourceWorkspaceID: workspaceID,
		SourceRevision: expectedRevision, Permission: permission, TokenHash: hash[:],
		ExpiresAt: now.Add(workspaceShareLifetime), State: workspace.SharePending, CreatedAt: now,
	})
	if err != nil {
		return WorkspaceShareCreated{}, err
	}
	if err := created.Validate(); err != nil || created.OwnerID != principal.UserID || created.SourceWorkspaceID != workspaceID ||
		created.SourceRevision != expectedRevision || created.Permission != permission || created.State != workspace.SharePending {
		return WorkspaceShareCreated{}, domain.NewError(domain.CodeInternal, "Workspace share store returned an invalid link")
	}
	return WorkspaceShareCreated{
		WorkspaceName: created.WorkspaceName, Permission: created.Permission,
		Token: token, ExpiresAt: created.ExpiresAt,
	}, nil
}

func (s *WorkspaceShareService) Preview(ctx context.Context, principal identity.Principal, token string) (WorkspaceSharePreview, error) {
	if err := requireWorkspaceShareSession(principal); err != nil {
		return WorkspaceSharePreview{}, err
	}
	hash, err := workspaceShareTokenHash(token)
	if err != nil {
		return WorkspaceSharePreview{}, domain.NewError(domain.CodeNotFound, "Workspace share link not found")
	}
	value, err := s.store.GetWorkspaceShareByToken(ctx, hash)
	if err != nil {
		return WorkspaceSharePreview{}, err
	}
	if value.State != workspace.SharePending || !time.Now().Before(value.ExpiresAt) || value.OwnerID == principal.UserID {
		return WorkspaceSharePreview{}, domain.NewError(domain.CodeNotFound, "Workspace share link not found")
	}
	return WorkspaceSharePreview{WorkspaceName: value.WorkspaceName, Permission: value.Permission, ExpiresAt: value.ExpiresAt}, nil
}

func (s *WorkspaceShareService) Accept(ctx context.Context, principal identity.Principal, token string) (WorkspaceShareAccepted, error) {
	if err := requireWorkspaceShareSession(principal); err != nil {
		return WorkspaceShareAccepted{}, err
	}
	hash, err := workspaceShareTokenHash(token)
	if err != nil {
		return WorkspaceShareAccepted{}, domain.NewError(domain.CodeNotFound, "Workspace share link not found")
	}
	share, err := s.store.StartWorkspaceShareClaim(ctx, hash, principal.UserID)
	if err != nil {
		return WorkspaceShareAccepted{}, err
	}
	if err := share.Validate(); err != nil || share.State != workspace.ShareClaiming || share.ClaimantID != principal.UserID ||
		share.OwnerID == principal.UserID {
		return WorkspaceShareAccepted{}, domain.NewError(domain.CodeInternal, "Workspace share claim state is invalid")
	}
	targetID := share.TargetWorkspaceID
	if targetID == "" {
		current, err := s.store.WorkspaceShareSourceMatches(ctx, share.OwnerID, share.SourceWorkspaceID, share.SourceRevision)
		if err != nil {
			return WorkspaceShareAccepted{}, err
		}
		if !current {
			if err := s.store.InvalidateWorkspaceShare(ctx, share.ID, principal.UserID); err != nil {
				return WorkspaceShareAccepted{}, errors.Join(domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed after this share link was created"), err)
			}
			return WorkspaceShareAccepted{}, domain.NewError(domain.CodeWorkspaceConflict, "Workspace changed after this share link was created; ask the owner for a new link")
		}
	}

	owner := identity.Principal{UserID: share.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	if targetID == "" {
		copied, err := s.copies.Copy(ctx, owner, share.SourceWorkspaceID, WorkspaceCopyCommand{
			Name: share.WorkspaceName, ExpectedCurrentRevision: share.SourceRevision,
		}, "ws-share-copy-"+share.ID)
		if err != nil {
			if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
				stillCurrent, checkErr := s.store.WorkspaceShareSourceMatches(ctx, share.OwnerID, share.SourceWorkspaceID, share.SourceRevision)
				if checkErr == nil && !stillCurrent {
					_ = s.store.InvalidateWorkspaceShare(ctx, share.ID, principal.UserID)
				}
			}
			return WorkspaceShareAccepted{}, err
		}
		targetID = copied.Workspace.ID
		if err := s.store.SetWorkspaceShareTarget(ctx, share.ID, principal.UserID, targetID); err != nil {
			return WorkspaceShareAccepted{}, err
		}
	}

	transfer, _, err := s.transfers.RequestWorkspaceTransfer(ctx, owner, targetID, principal.UserID, 1,
		"ws-share-transfer-"+share.ID)
	if err != nil {
		return WorkspaceShareAccepted{}, err
	}
	if share.Permission == workspace.ShareView {
		_, err = s.transfers.AcceptWorkspaceTransferReadOnly(ctx, principal, targetID, transfer.ID, "ws-share-accept-"+share.ID)
	} else {
		_, err = s.transfers.AcceptWorkspaceTransfer(ctx, principal, targetID, transfer.ID, "ws-share-accept-"+share.ID)
	}
	if err != nil {
		return WorkspaceShareAccepted{}, err
	}
	if err := s.store.CompleteWorkspaceShareClaim(ctx, share.ID, principal.UserID, targetID); err != nil {
		return WorkspaceShareAccepted{}, err
	}
	return WorkspaceShareAccepted{WorkspaceID: targetID, WorkspaceName: share.WorkspaceName, Permission: share.Permission}, nil
}

func requireWorkspaceShareSession(principal identity.Principal) error {
	if principal.Kind != identity.KindSession {
		return domain.NewError(domain.CodeForbidden, "Workspace sharing requires a Portal session")
	}
	return validatePrincipal(principal)
}

func workspaceShareTokenHash(token string) ([]byte, error) {
	if len(token) != 43 || strings.TrimSpace(token) != token {
		return nil, errors.New("invalid Workspace share token")
	}
	secret, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != token {
		return nil, errors.New("invalid Workspace share token")
	}
	hash := sha256.Sum256(secret)
	return hash[:], nil
}
