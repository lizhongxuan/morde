package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type workspaceTransferStoreStub struct {
	created      workspace.OwnershipTransfer
	started      workspace.OwnershipTransfer
	finished     workspace.OwnershipTransfer
	reconcileErr error
}

func (s *workspaceTransferStoreStub) CreateWorkspaceOwnershipTransfer(_ context.Context, actorID, workspaceID, targetID string,
	revision int64, id string, _ IdempotencyRecord) (workspace.OwnershipTransfer, bool, error) {
	value := validWorkspaceTransfer()
	value.ID, value.WorkspaceID, value.FromOwnerID, value.ToOwnerID = id, workspaceID, actorID, targetID
	value.ExpectedRevision = revision
	s.created = value
	return value, false, nil
}

func (s *workspaceTransferStoreStub) GetWorkspaceOwnershipTransfer(context.Context, string, string, string) (workspace.OwnershipTransfer, error) {
	return s.created, nil
}

func (s *workspaceTransferStoreStub) ReconcileWorkspaceOwnershipTransfer(context.Context, string, string, string) (workspace.OwnershipTransfer, error) {
	if s.reconcileErr != nil {
		return workspace.OwnershipTransfer{}, s.reconcileErr
	}
	if s.created.State == workspace.OwnershipTransferUnknown {
		s.created.State = workspace.OwnershipTransferPending
		s.created.OperationID = ""
		s.created.Version++
	}
	return s.created, nil
}

func (s *workspaceTransferStoreStub) StartWorkspaceOwnershipTransferAcceptance(_ context.Context, actorID, workspaceID, transferID, operationID string,
	_ IdempotencyRecord) (workspace.OwnershipTransfer, bool, error) {
	if s.created.State == workspace.OwnershipTransferAccepted {
		return s.created, true, nil
	}
	value := s.created
	value.ID, value.WorkspaceID, value.ToOwnerID = transferID, workspaceID, actorID
	value.State, value.OperationID = workspace.OwnershipTransferAccepting, operationID
	s.started = value
	return value, false, nil
}

func (s *workspaceTransferStoreStub) FinishWorkspaceOwnershipTransferAcceptance(_ context.Context, actorID, workspaceID, transferID, operationID string,
	_ int64) (workspace.OwnershipTransfer, error) {
	value := s.started
	value.ID, value.WorkspaceID, value.ToOwnerID = transferID, workspaceID, actorID
	value.State, value.OperationID = workspace.OwnershipTransferAccepted, operationID
	accepted := value.UpdatedAt
	value.AcceptedAt = &accepted
	s.finished = value
	s.created = value
	return value, nil
}

func (s *workspaceTransferStoreStub) CancelWorkspaceOwnershipTransfer(context.Context, string, string, string, IdempotencyRecord) (workspace.OwnershipTransfer, error) {
	s.created.State = workspace.OwnershipTransferCancelled
	s.created.Version++
	return s.created, nil
}

func TestWorkspaceTransferProjectionFailurePreservesAcceptingAndUnknown(t *testing.T) {
	for _, state := range []workspace.OwnershipTransferState{workspace.OwnershipTransferPending, workspace.OwnershipTransferAccepting, workspace.OwnershipTransferUnknown} {
		t.Run(string(state), func(t *testing.T) {
			value := validWorkspaceTransfer()
			value.State = state
			if state == workspace.OwnershipTransferAccepting || state == workspace.OwnershipTransferUnknown {
				value.OperationID = "o1234567"
			}
			store := &workspaceTransferStoreStub{created: value}
			if state == workspace.OwnershipTransferUnknown {
				store.reconcileErr = domain.NewError(domain.CodeWorkspaceBusy, "ownership state mismatch")
			}
			service, err := NewWorkspaceTransferService(store, 100)
			if err != nil {
				t.Fatal(err)
			}
			prepareErr := errors.New("projection unavailable")
			prepares, discards := 0, 0
			if err := service.SetWorkspaceOwnershipTransferProjection(WorkspaceOwnershipTransferProjection{
				PrepareRecipient: func(context.Context, workspace.OwnershipTransfer) error {
					prepares++
					return prepareErr
				},
				DiscardRecipient: func(context.Context, workspace.OwnershipTransfer) error {
					discards++
					return nil
				},
				RetireFormer: func(context.Context, workspace.OwnershipTransfer) error { return nil },
			}); err != nil {
				t.Fatal(err)
			}
			principal := identity.Principal{UserID: value.ToOwnerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t7654321"}
			_, err = service.AcceptWorkspaceTransfer(context.Background(), principal, value.WorkspaceID, value.ID, "transfer-accept-failure")
			if err == nil {
				t.Fatal("acceptance succeeded without a recipient projection")
			}
			if state == workspace.OwnershipTransferUnknown {
				if prepares != 0 || discards != 0 {
					t.Fatalf("unknown transfer touched projections: prepare=%d discard=%d", prepares, discards)
				}
				return
			}
			wantDiscards := 0
			if state == workspace.OwnershipTransferPending {
				wantDiscards = 1
			}
			if prepares != 1 || discards != wantDiscards || store.started.OperationID != "" {
				t.Fatalf("projection failure handling = prepare:%d discard:%d started:%#v", prepares, discards, store.started)
			}
		})
	}
}

func TestWorkspaceTransferUnknownIsReconciledBeforeProjection(t *testing.T) {
	value := validWorkspaceTransfer()
	value.State = workspace.OwnershipTransferUnknown
	value.OperationID = "o1234567"
	store := &workspaceTransferStoreStub{created: value}
	service, err := NewWorkspaceTransferService(store, 100)
	if err != nil {
		t.Fatal(err)
	}
	var preparedState workspace.OwnershipTransferState
	if err := service.SetWorkspaceOwnershipTransferProjection(WorkspaceOwnershipTransferProjection{
		PrepareRecipient: func(_ context.Context, transfer workspace.OwnershipTransfer) error {
			preparedState = transfer.State
			return nil
		},
		DiscardRecipient: func(context.Context, workspace.OwnershipTransfer) error { return nil },
		RetireFormer:     func(context.Context, workspace.OwnershipTransfer) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: value.ToOwnerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t7654321"}
	accepted, err := service.AcceptWorkspaceTransfer(context.Background(), principal, value.WorkspaceID, value.ID, "transfer-unknown-retry")
	if err != nil || accepted.State != workspace.OwnershipTransferAccepted || preparedState != workspace.OwnershipTransferPending {
		t.Fatalf("accept reconciled unknown transfer = %#v, prepared state=%q, err=%v", accepted, preparedState, err)
	}
}

func TestWorkspaceTransferCancellationDiscardsRecipientProjection(t *testing.T) {
	store := &workspaceTransferStoreStub{created: validWorkspaceTransfer()}
	service, err := NewWorkspaceTransferService(store, 100)
	if err != nil {
		t.Fatal(err)
	}
	discarded := false
	if err := service.SetWorkspaceOwnershipTransferProjection(WorkspaceOwnershipTransferProjection{
		PrepareRecipient: func(context.Context, workspace.OwnershipTransfer) error { return nil },
		DiscardRecipient: func(_ context.Context, value workspace.OwnershipTransfer) error {
			discarded = value.State == workspace.OwnershipTransferCancelled
			return nil
		},
		RetireFormer: func(context.Context, workspace.OwnershipTransfer) error { return nil },
	}); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: store.created.FromOwnerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	if _, err := service.CancelWorkspaceTransfer(context.Background(), principal, store.created.WorkspaceID, store.created.ID, "transfer-cancel-key"); err != nil {
		t.Fatal(err)
	}
	if !discarded {
		t.Fatal("cancellation did not discard the unowned recipient projection")
	}
}

func TestWorkspaceTransferServiceRequiresPortalSessionAndKeepsAcceptanceOperation(t *testing.T) {
	store := &workspaceTransferStoreStub{created: validWorkspaceTransfer()}
	service, err := NewWorkspaceTransferService(store, 100)
	if err != nil {
		t.Fatal(err)
	}
	var events []string
	retireFailures := 1
	if err := service.SetWorkspaceOwnershipTransferProjection(WorkspaceOwnershipTransferProjection{
		PrepareRecipient: func(_ context.Context, value workspace.OwnershipTransfer) error {
			events = append(events, "prepare:"+string(value.State))
			return nil
		},
		DiscardRecipient: func(context.Context, workspace.OwnershipTransfer) error {
			events = append(events, "discard")
			return nil
		},
		RetireFormer: func(_ context.Context, value workspace.OwnershipTransfer) error {
			events = append(events, "retire:"+string(value.State))
			if retireFailures > 0 {
				retireFailures--
				return errors.New("injected projection cleanup failure")
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	created, replayed, err := service.RequestWorkspaceTransfer(context.Background(), principal, "w1234567", "u7654321", 2, "transfer-create-key")
	if err != nil || replayed || created.ExpectedRevision != 2 || store.created.ID != created.ID {
		t.Fatalf("request transfer = %#v replayed=%t err=%v", created, replayed, err)
	}
	accepted, err := service.AcceptWorkspaceTransfer(context.Background(), identity.Principal{UserID: "u7654321", Role: identity.RoleUser,
		Kind: identity.KindSession, TokenID: "t7654321"}, "w1234567", created.ID, "transfer-accept-key")
	if err == nil || store.finished.State != workspace.OwnershipTransferAccepted || store.finished.OperationID != store.started.OperationID {
		t.Fatalf("accept transfer with former projection cleanup failure = %#v err=%v", accepted, err)
	}
	accepted, err = service.AcceptWorkspaceTransfer(context.Background(), identity.Principal{UserID: "u7654321", Role: identity.RoleUser,
		Kind: identity.KindSession, TokenID: "t7654321"}, "w1234567", created.ID, "transfer-accept-key")
	if err != nil || accepted.State != workspace.OwnershipTransferAccepted || accepted.OperationID != store.started.OperationID ||
		store.finished.OperationID != store.started.OperationID {
		t.Fatalf("retry accepted transfer projection cleanup = %#v err=%v", accepted, err)
	}
	if len(events) != 3 || events[0] != "prepare:pending" || events[1] != "retire:accepted" || events[2] != "retire:accepted" {
		t.Fatalf("Workspace current projection callback order = %#v", events)
	}
	apiKeyPrincipal := principal
	apiKeyPrincipal.Kind = identity.KindAPIKey
	if _, _, err := service.RequestWorkspaceTransfer(context.Background(), apiKeyPrincipal, "w1234567", "u7654321", 2, "transfer-api-key"); err == nil {
		t.Fatal("API key requested ownership transfer")
	}
}

func validWorkspaceTransfer() workspace.OwnershipTransfer {
	now := time.Now().UTC()
	return workspace.OwnershipTransfer{
		ID: "r1234567", WorkspaceID: "w1234567", FromOwnerID: "u1234567", ToOwnerID: "u7654321",
		State: workspace.OwnershipTransferPending, ExpectedRevision: 2, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
}
