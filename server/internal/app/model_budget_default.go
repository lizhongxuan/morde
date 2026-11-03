package app

import (
	"context"
	"math"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

// ModelBudgetDefault is the optional platform model budget applied to first-time account approvals.
type ModelBudgetDefault struct {
	Configured    bool
	TokenLimit    int64
	MaxConcurrent int32
	Version       int64
}

// UpdateModelBudgetDefaultCommand replaces the default under a version check.
type UpdateModelBudgetDefaultCommand struct {
	ExpectedVersion int64
	TokenLimit      int64
	MaxConcurrent   int32
}

// AdminModelBudgetDefaultStore reads and updates the configured default model budget.
type AdminModelBudgetDefaultStore interface {
	GetModelBudgetDefault(context.Context) (ModelBudgetDefault, error)
	SetModelBudgetDefault(context.Context, identity.Principal, UpdateModelBudgetDefaultCommand, string) (ModelBudgetDefault, error)
}

// AdminModelBudgetDefaultService validates the administrator's default model budget policy.
type AdminModelBudgetDefaultService struct {
	store AdminModelBudgetDefaultStore
}

// NewAdminModelBudgetDefaultService constructs the default model budget service.
func NewAdminModelBudgetDefaultService(store AdminModelBudgetDefaultStore) (*AdminModelBudgetDefaultService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("model_budget_default", "model budget default store is required")
	}
	return &AdminModelBudgetDefaultService{store: store}, nil
}

func (s *AdminModelBudgetDefaultService) Get(ctx context.Context, principal identity.Principal) (ModelBudgetDefault, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return ModelBudgetDefault{}, err
	}
	value, err := s.store.GetModelBudgetDefault(ctx)
	if err != nil {
		return ModelBudgetDefault{}, err
	}
	if !validModelBudgetDefault(value) {
		return ModelBudgetDefault{}, domain.NewError(domain.CodeInternal, "default model budget is invalid")
	}
	return value, nil
}

func (s *AdminModelBudgetDefaultService) Update(ctx context.Context, principal identity.Principal, command UpdateModelBudgetDefaultCommand, ip string) (ModelBudgetDefault, error) {
	if err := authorizeModelUsageAdmin(principal); err != nil {
		return ModelBudgetDefault{}, err
	}
	if command.ExpectedVersion < 0 || command.TokenLimit <= 0 || command.MaxConcurrent < 1 || command.MaxConcurrent > 100 || len(ip) > 64 {
		return ModelBudgetDefault{}, domain.InvalidArgument("model_budget_default", "invalid default model budget")
	}
	value, err := s.store.SetModelBudgetDefault(ctx, principal, command, ip)
	if err != nil {
		return ModelBudgetDefault{}, err
	}
	if command.ExpectedVersion == math.MaxInt64 || !validModelBudgetDefault(value) || !value.Configured || value.Version != command.ExpectedVersion+1 {
		return ModelBudgetDefault{}, domain.NewError(domain.CodeInternal, "updated default model budget is invalid")
	}
	return value, nil
}

func validModelBudgetDefault(value ModelBudgetDefault) bool {
	if !value.Configured {
		return value.TokenLimit == 0 && value.MaxConcurrent == 0 && value.Version == 0
	}
	return value.TokenLimit > 0 && value.MaxConcurrent >= 1 && value.MaxConcurrent <= 100 && value.Version > 0
}
