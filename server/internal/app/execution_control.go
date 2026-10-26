package app

import (
	"context"
	"fmt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/identity"
)

type ExecutionControlStore interface {
	GetExecutionControl(context.Context, string) (execution.Control, error)
	StopAllExecution(context.Context, IdempotencyRecord, int64) (execution.Control, error)
	EnableExecution(context.Context, IdempotencyRecord, int64) (execution.Control, error)
}

type ExecutionControlService struct {
	store ExecutionControlStore
	audit AuditWriter
}

func NewExecutionControlService(store ExecutionControlStore, writer AuditWriter) (*ExecutionControlService, error) {
	if store == nil || writer == nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "execution control dependencies are required")
	}
	return &ExecutionControlService{store: store, audit: writer}, nil
}

func (s *ExecutionControlService) Status(ctx context.Context, principal identity.Principal) (execution.Control, error) {
	if err := requireExecutionControlSession(principal); err != nil {
		return execution.Control{}, err
	}
	value, err := s.store.GetExecutionControl(ctx, principal.UserID)
	if err != nil {
		return execution.Control{}, err
	}
	if value.OwnerID != principal.UserID || !value.State.Valid() || value.Epoch < 1 || value.PendingTaskCount < 0 {
		return execution.Control{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	return value, nil
}

func (s *ExecutionControlService) StopAll(ctx context.Context, principal identity.Principal, expectedEpoch int64, idempotencyKey string) (execution.Control, error) {
	return s.set(ctx, principal, "stop_all", expectedEpoch, idempotencyKey)
}

func (s *ExecutionControlService) Enable(ctx context.Context, principal identity.Principal, expectedEpoch int64, idempotencyKey string) (execution.Control, error) {
	return s.set(ctx, principal, "enable", expectedEpoch, idempotencyKey)
}

func (s *ExecutionControlService) set(ctx context.Context, principal identity.Principal, action string, expectedEpoch int64, idempotencyKey string) (execution.Control, error) {
	if err := requireExecutionControlSession(principal); err != nil {
		return execution.Control{}, err
	}
	if err := validateIdempotencyKey(idempotencyKey); err != nil {
		return execution.Control{}, err
	}
	if expectedEpoch < 1 {
		return execution.Control{}, domain.InvalidArgument("expected_epoch", "a positive execution epoch is required")
	}
	request, err := idempotencyRecord(principal.UserID, "execution."+action, idempotencyKey, struct {
		Action        string `json:"action"`
		ExpectedEpoch int64  `json:"expected_epoch"`
	}{Action: action, ExpectedEpoch: expectedEpoch}, principal.UserID, "pending")
	if err != nil {
		return execution.Control{}, err
	}
	var result execution.Control
	switch action {
	case "stop_all":
		result, err = s.store.StopAllExecution(ctx, request, expectedEpoch)
	case "enable":
		result, err = s.store.EnableExecution(ctx, request, expectedEpoch)
	default:
		return execution.Control{}, domain.InvalidArgument("action", "execution action is invalid")
	}
	if err != nil {
		return execution.Control{}, err
	}
	if result.OwnerID != principal.UserID || !result.State.Valid() || result.Epoch < 1 {
		return execution.Control{}, domain.NewError(domain.CodeInternal, "internal error")
	}
	if err := s.audit.Write(ctx, audit.Entry{
		ActorID: principal.UserID, ActorKind: audit.ActorSession,
		Action: audit.ActionExecutionControl, Target: principal.UserID,
		Detail: map[string]any{"action": action, "state": result.State, "epoch": result.Epoch},
	}); err != nil {
		return execution.Control{}, fmt.Errorf("write execution control audit: %w", err)
	}
	return result, nil
}

func requireExecutionControlSession(principal identity.Principal) error {
	if err := validateTaskPrincipal(principal); err != nil {
		return err
	}
	if principal.Kind != identity.KindSession {
		return domain.NewError(domain.CodeForbidden, "execution control requires a Portal session")
	}
	return nil
}
