package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type WorkspaceOwnershipTransferStore interface {
	CreateWorkspaceOwnershipTransfer(context.Context, string, string, string, int64, string, IdempotencyRecord) (workspace.OwnershipTransfer, bool, error)
	GetWorkspaceOwnershipTransfer(context.Context, string, string, string) (workspace.OwnershipTransfer, error)
	ReconcileWorkspaceOwnershipTransfer(context.Context, string, string, string) (workspace.OwnershipTransfer, error)
	StartWorkspaceOwnershipTransferAcceptance(context.Context, string, string, string, string, IdempotencyRecord) (workspace.OwnershipTransfer, bool, error)
	FinishWorkspaceOwnershipTransferAcceptance(context.Context, string, string, string, string, int64) (workspace.OwnershipTransfer, error)
	CancelWorkspaceOwnershipTransfer(context.Context, string, string, string, IdempotencyRecord) (workspace.OwnershipTransfer, error)
}

type WorkspaceOwnershipTransferProjection struct {
	PrepareRecipient func(context.Context, workspace.OwnershipTransfer) error
	DiscardRecipient func(context.Context, workspace.OwnershipTransfer) error
	RetireFormer     func(context.Context, workspace.OwnershipTransfer) error
}

type WorkspaceTransferService struct {
	store         WorkspaceOwnershipTransferStore
	maxOwnerBytes int64
	projection    *WorkspaceOwnershipTransferProjection
}

func NewWorkspaceTransferService(store WorkspaceOwnershipTransferStore, maxOwnerBytes int64) (*WorkspaceTransferService, error) {
	if store == nil || maxOwnerBytes <= 0 {
		return nil, errors.New("workspace ownership transfer store and owner quota are required")
	}
	return &WorkspaceTransferService{store: store, maxOwnerBytes: maxOwnerBytes}, nil
}

func (s *WorkspaceTransferService) SetWorkspaceOwnershipTransferProjection(projection WorkspaceOwnershipTransferProjection) error {
	if s == nil || projection.PrepareRecipient == nil || projection.DiscardRecipient == nil || projection.RetireFormer == nil {
		return errors.New("complete Workspace ownership transfer projection callbacks are required")
	}
	s.projection = &projection
	return nil
}

func (s *WorkspaceTransferService) RequestWorkspaceTransfer(ctx context.Context, principal identity.Principal, workspaceID, targetUserID string,
	expectedRevision int64, idempotencyKey string) (workspace.OwnershipTransfer, bool, error) {
	if err := requireWorkspaceTransferSession(principal); err != nil {
		return workspace.OwnershipTransfer{}, false, err
	}
	if !domain.ValidWorkspaceID(workspaceID) || !domain.ValidUserID(targetUserID) || principal.UserID == targetUserID || expectedRevision < 0 {
		return workspace.OwnershipTransfer{}, false, domain.InvalidArgument("ownership_transfer", "workspace ownership transfer request is invalid")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return workspace.OwnershipTransfer{}, false, err
	}
	id, err := workspace.NewOwnershipTransferID()
	if err != nil {
		return workspace.OwnershipTransfer{}, false, err
	}
	record, err := idempotencyRecord(principal.UserID, "workspace.transfer.create", idempotencyKey, struct {
		WorkspaceID      string `json:"workspace_id"`
		TargetUserID     string `json:"target_user_id"`
		ExpectedRevision int64  `json:"expected_revision"`
	}{workspaceID, targetUserID, expectedRevision}, id, "")
	if err != nil {
		return workspace.OwnershipTransfer{}, false, err
	}
	result, replayed, err := s.store.CreateWorkspaceOwnershipTransfer(ctx, principal.UserID, workspaceID, targetUserID, expectedRevision, id, record)
	if err != nil {
		return workspace.OwnershipTransfer{}, false, err
	}
	if err := validateWorkspaceTransferReceipt(result, workspaceID, principal.UserID, targetUserID); err != nil {
		return workspace.OwnershipTransfer{}, false, err
	}
	return result, replayed, nil
}

func (s *WorkspaceTransferService) GetWorkspaceTransfer(ctx context.Context, principal identity.Principal, workspaceID, transferID string) (workspace.OwnershipTransfer, error) {
	if err := requireWorkspaceTransferSession(principal); err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidOwnershipTransferID(transferID) {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
	}
	result, err := s.store.GetWorkspaceOwnershipTransfer(ctx, principal.UserID, workspaceID, transferID)
	if err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	if err := result.Validate(); err != nil || result.WorkspaceID != workspaceID ||
		(result.FromOwnerID != principal.UserID && result.ToOwnerID != principal.UserID) {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeInternal, "workspace ownership transfer store returned an invalid record")
	}
	return result, nil
}

func (s *WorkspaceTransferService) AcceptWorkspaceTransfer(ctx context.Context, principal identity.Principal, workspaceID, transferID, idempotencyKey string) (workspace.OwnershipTransfer, error) {
	if err := requireWorkspaceTransferSession(principal); err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidOwnershipTransferID(transferID) {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	current, err := s.store.GetWorkspaceOwnershipTransfer(ctx, principal.UserID, workspaceID, transferID)
	if err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	if current.ToOwnerID != principal.UserID {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
	}
	if s.projection == nil {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeInternal, "workspace ownership transfer projection is unavailable")
	}
	if current.State == workspace.OwnershipTransferUnknown {
		current, err = s.store.ReconcileWorkspaceOwnershipTransfer(ctx, principal.UserID, workspaceID, transferID)
		if err != nil {
			return workspace.OwnershipTransfer{}, err
		}
		if current.ToOwnerID != principal.UserID {
			return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeInternal, "Workspace transfer reconciliation returned an invalid owner")
		}
	}
	if current.State != workspace.OwnershipTransferAccepted && current.State != workspace.OwnershipTransferPending && current.State != workspace.OwnershipTransferAccepting {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeInvalidState, "Workspace ownership transfer is no longer pending")
	}
	if current.State != workspace.OwnershipTransferAccepted {
		if err := s.projection.PrepareRecipient(ctx, current); err != nil {
			cleanupCtx, cancel := workspaceTransferCleanupContext(ctx)
			defer cancel()
			latest, readErr := s.store.GetWorkspaceOwnershipTransfer(cleanupCtx, principal.UserID, workspaceID, transferID)
			if readErr == nil && (latest.State == workspace.OwnershipTransferPending || latest.State == workspace.OwnershipTransferCancelled) {
				if discardErr := s.projection.DiscardRecipient(cleanupCtx, latest); discardErr != nil {
					return workspace.OwnershipTransfer{}, errors.Join(fmt.Errorf("prepare recipient Workspace projection: %w", err),
						fmt.Errorf("discard uncommitted recipient Workspace projection: %w", discardErr))
				}
			}
			return workspace.OwnershipTransfer{}, fmt.Errorf("prepare recipient Workspace projection: %w", err)
		}
	}
	operationID, err := domain.NewOperationID()
	if err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	requestHash, err := json.Marshal(struct {
		WorkspaceID string `json:"workspace_id"`
		TransferID  string `json:"transfer_id"`
	}{workspaceID, transferID})
	if err != nil {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeInternal, "workspace ownership transfer request cannot be encoded")
	}
	record, err := idempotencyRecord(principal.UserID, "workspace.transfer.accept", idempotencyKey, json.RawMessage(requestHash), transferID, "")
	if err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	started, _, err := s.store.StartWorkspaceOwnershipTransferAcceptance(ctx, principal.UserID, workspaceID, transferID, operationID, record)
	if err != nil {
		s.discardRecipientProjectionIfTransferNotAccepting(ctx, principal.UserID, workspaceID, transferID)
		return workspace.OwnershipTransfer{}, err
	}
	if err := started.Validate(); err != nil || started.WorkspaceID != workspaceID || started.ToOwnerID != principal.UserID {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeInternal, "workspace ownership transfer store returned an invalid acceptance")
	}
	if started.State == workspace.OwnershipTransferAccepted {
		return s.retireFormerWorkspaceProjection(ctx, started)
	}
	if started.State == workspace.OwnershipTransferUnknown {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace ownership transfer result is unknown and requires reconciliation")
	}
	if started.State != workspace.OwnershipTransferAccepting {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeWorkspaceBusy, "workspace ownership transfer acceptance is not ready")
	}
	result, err := s.store.FinishWorkspaceOwnershipTransferAcceptance(ctx, principal.UserID, workspaceID, transferID, started.OperationID, s.maxOwnerBytes)
	if err != nil {
		cleanupCtx, cancel := workspaceTransferCleanupContext(ctx)
		defer cancel()
		latest, readErr := s.store.GetWorkspaceOwnershipTransfer(cleanupCtx, principal.UserID, workspaceID, transferID)
		if readErr == nil {
			switch latest.State {
			case workspace.OwnershipTransferAccepted:
				return s.retireFormerWorkspaceProjection(cleanupCtx, latest)
			case workspace.OwnershipTransferPending, workspace.OwnershipTransferCancelled:
				if discardErr := s.projection.DiscardRecipient(cleanupCtx, latest); discardErr != nil {
					return workspace.OwnershipTransfer{}, errors.Join(err,
						fmt.Errorf("discard rejected recipient Workspace projection: %w", discardErr))
				}
			}
		}
		return workspace.OwnershipTransfer{}, err
	}
	if err := result.Validate(); err != nil || result.State != workspace.OwnershipTransferAccepted || result.WorkspaceID != workspaceID || result.ToOwnerID != principal.UserID {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeInternal, "workspace ownership transfer acceptance receipt is invalid")
	}
	return s.retireFormerWorkspaceProjection(ctx, result)
}

func (s *WorkspaceTransferService) CancelWorkspaceTransfer(ctx context.Context, principal identity.Principal, workspaceID, transferID, idempotencyKey string) (workspace.OwnershipTransfer, error) {
	if err := requireWorkspaceTransferSession(principal); err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	if !domain.ValidWorkspaceID(workspaceID) || !workspace.ValidOwnershipTransferID(transferID) {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeNotFound, "workspace ownership transfer not found")
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	record, err := idempotencyRecord(principal.UserID, "workspace.transfer.cancel", idempotencyKey,
		struct{ WorkspaceID, TransferID string }{workspaceID, transferID}, transferID, "")
	if err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	result, err := s.store.CancelWorkspaceOwnershipTransfer(ctx, principal.UserID, workspaceID, transferID, record)
	if err != nil {
		return workspace.OwnershipTransfer{}, err
	}
	if err := result.Validate(); err != nil || result.State != workspace.OwnershipTransferCancelled || result.WorkspaceID != workspaceID || result.FromOwnerID != principal.UserID {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeInternal, "workspace ownership transfer cancellation receipt is invalid")
	}
	if s.projection != nil {
		cleanupCtx, cancel := workspaceTransferCleanupContext(ctx)
		defer cancel()
		if err := s.projection.DiscardRecipient(cleanupCtx, result); err != nil {
			return workspace.OwnershipTransfer{}, fmt.Errorf("discard cancelled recipient Workspace projection: %w", err)
		}
	}
	return result, nil
}

func (s *WorkspaceTransferService) retireFormerWorkspaceProjection(ctx context.Context, transfer workspace.OwnershipTransfer) (workspace.OwnershipTransfer, error) {
	if s.projection == nil {
		return workspace.OwnershipTransfer{}, domain.NewError(domain.CodeInternal, "workspace ownership transfer projection is unavailable")
	}
	cleanupCtx, cancel := workspaceTransferCleanupContext(ctx)
	defer cancel()
	if err := s.projection.RetireFormer(cleanupCtx, transfer); err != nil {
		return workspace.OwnershipTransfer{}, fmt.Errorf("retire former Workspace owner projection after acceptance: %w", err)
	}
	return transfer, nil
}

func (s *WorkspaceTransferService) discardRecipientProjectionIfTransferNotAccepting(ctx context.Context, actorID, workspaceID, transferID string) {
	if s.projection == nil {
		return
	}
	cleanupCtx, cancel := workspaceTransferCleanupContext(ctx)
	defer cancel()
	latest, err := s.store.GetWorkspaceOwnershipTransfer(cleanupCtx, actorID, workspaceID, transferID)
	if err != nil || (latest.State != workspace.OwnershipTransferPending && latest.State != workspace.OwnershipTransferCancelled) {
		return
	}
	_ = s.projection.DiscardRecipient(cleanupCtx, latest)
}

func workspaceTransferCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

func requireWorkspaceTransferSession(principal identity.Principal) error {
	if principal.Kind != identity.KindSession {
		return domain.NewError(domain.CodeForbidden, "workspace ownership transfer requires a Portal session")
	}
	return validatePrincipal(principal)
}

func validateWorkspaceTransferReceipt(value workspace.OwnershipTransfer, workspaceID, fromOwnerID, targetUserID string) error {
	if err := value.Validate(); err != nil || value.WorkspaceID != workspaceID || value.FromOwnerID != fromOwnerID || value.ToOwnerID != targetUserID {
		return domain.NewError(domain.CodeInternal, "workspace ownership transfer store returned an invalid record")
	}
	return nil
}
