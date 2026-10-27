package app

import (
	"context"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type UnknownModelCall struct {
	OwnerID        string
	Source         ModelCallSource
	CallID         string
	ModelID        string
	ProviderModel  string
	TaskID         string
	ReservedTokens int64
	CreatedAt      time.Time
	UnknownAt      time.Time
}

type ModelAccountBudget struct {
	OwnerID       string
	Configured    bool
	TokenLimit    int64
	MaxConcurrent int32
	UsedTokens    int64
	InFlight      int32
	HasUnknown    bool
}

type ModelAccountBudgetUpdate struct {
	OwnerID       string
	TokenLimit    int64
	MaxConcurrent int32
	IP            string
}

type ModelUsageReconcileCommand struct {
	OwnerID      string
	Source       ModelCallSource
	CallID       string
	InputTokens  int64
	OutputTokens int64
	Evidence     string
	IP           string
}

type ModelUsageAdminStore interface {
	ListUnknownModelCalls(context.Context, identity.Principal, string, int) ([]UnknownModelCall, error)
	ReconcileUnknownModelCall(context.Context, identity.Principal, ModelUsageReconcileCommand) (bool, error)
	GetModelAccountBudget(context.Context, identity.Principal, string) (ModelAccountBudget, error)
	SetModelAccountBudget(context.Context, identity.Principal, ModelAccountBudgetUpdate) (ModelAccountBudget, error)
}

type ModelUsageAdminService struct {
	store ModelUsageAdminStore
}

func NewModelUsageAdminService(store ModelUsageAdminStore) (*ModelUsageAdminService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("model_usage", "model usage store is required")
	}
	return &ModelUsageAdminService{store: store}, nil
}

func (s *ModelUsageAdminService) ListUnknown(ctx context.Context, principal identity.Principal, ownerID string, limit int) ([]UnknownModelCall, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return nil, err
	}
	if ownerID != "" && !domain.ValidUserID(ownerID) {
		return nil, domain.InvalidArgument("owner_id", "invalid owner")
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 100 {
		return nil, domain.InvalidArgument("limit", "limit must be between 1 and 100")
	}
	return s.store.ListUnknownModelCalls(ctx, principal, ownerID, limit)
}

func (s *ModelUsageAdminService) Reconcile(ctx context.Context, principal identity.Principal, command ModelUsageReconcileCommand) (bool, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return false, err
	}
	command.Evidence = strings.TrimSpace(command.Evidence)
	if !domain.ValidUserID(command.OwnerID) || !validModelCallSource(command.Source) ||
		!idempotencyKeyPattern.MatchString(command.CallID) || command.InputTokens < 0 || command.OutputTokens < 0 ||
		command.InputTokens > math.MaxInt64-command.OutputTokens || !utf8.ValidString(command.Evidence) ||
		utf8.RuneCountInString(command.Evidence) < 8 || len(command.Evidence) > 512 ||
		strings.IndexFunc(command.Evidence, unicode.IsControl) >= 0 || len(command.IP) > 64 {
		return false, domain.InvalidArgument("model_usage", "invalid usage reconciliation")
	}
	return s.store.ReconcileUnknownModelCall(ctx, principal, command)
}

func (s *ModelUsageAdminService) Budget(ctx context.Context, principal identity.Principal, ownerID string) (ModelAccountBudget, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return ModelAccountBudget{}, err
	}
	if !domain.ValidUserID(ownerID) {
		return ModelAccountBudget{}, domain.InvalidArgument("owner_id", "invalid owner")
	}
	return s.store.GetModelAccountBudget(ctx, principal, ownerID)
}

func (s *ModelUsageAdminService) SetBudget(ctx context.Context, principal identity.Principal, command ModelAccountBudgetUpdate) (ModelAccountBudget, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return ModelAccountBudget{}, err
	}
	if !domain.ValidUserID(command.OwnerID) || command.TokenLimit < 1 || command.MaxConcurrent < 1 ||
		command.MaxConcurrent > 100 || len(command.IP) > 64 {
		return ModelAccountBudget{}, domain.InvalidArgument("model_budget", "invalid model budget")
	}
	return s.store.SetModelAccountBudget(ctx, principal, command)
}

func validModelCallSource(source ModelCallSource) bool {
	switch source {
	case ModelCallClient, ModelCallChat, ModelCallTask, ModelCallProbe:
		return true
	default:
		return false
	}
}

func authorizeModelUsageAdmin(principal identity.Principal) error {
	if principal.Kind != identity.KindSession || principal.ClientInstallationID != "" || authorizeAdmin(principal) != nil {
		return domain.NewError(domain.CodeForbidden, "unbound admin session required")
	}
	return nil
}
