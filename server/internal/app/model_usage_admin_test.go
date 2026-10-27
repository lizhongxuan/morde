package app

import (
	"context"
	"math"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type modelUsageAdminTestStore struct {
	owner      string
	limit      int
	command    ModelUsageReconcileCommand
	writes     int
	budget     ModelAccountBudget
	budgetEdit ModelAccountBudgetUpdate
}

func (s *modelUsageAdminTestStore) ListUnknownModelCalls(_ context.Context, _ identity.Principal, owner string, limit int) ([]UnknownModelCall, error) {
	s.owner, s.limit = owner, limit
	return []UnknownModelCall{}, nil
}

func (s *modelUsageAdminTestStore) ReconcileUnknownModelCall(_ context.Context, _ identity.Principal, command ModelUsageReconcileCommand) (bool, error) {
	s.command = command
	s.writes++
	return false, nil
}

func (s *modelUsageAdminTestStore) GetModelAccountBudget(_ context.Context, _ identity.Principal, ownerID string) (ModelAccountBudget, error) {
	s.owner = ownerID
	return s.budget, nil
}

func (s *modelUsageAdminTestStore) SetModelAccountBudget(_ context.Context, _ identity.Principal, command ModelAccountBudgetUpdate) (ModelAccountBudget, error) {
	s.budgetEdit = command
	return s.budget, nil
}

func TestModelUsageAdminRequiresUnboundAdminSessionAndValidEvidence(t *testing.T) {
	store := &modelUsageAdminTestStore{}
	service, err := NewModelUsageAdminService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Kind: identity.KindSession, Role: identity.RoleAdmin}
	ctx := context.Background()
	if _, err := service.ListUnknown(ctx, principal, "u7654321", 0); err != nil || store.owner != "u7654321" || store.limit != 100 {
		t.Fatalf("list unknown = %v, owner=%s limit=%d", err, store.owner, store.limit)
	}
	command := ModelUsageReconcileCommand{OwnerID: "u7654321", Source: ModelCallTask, CallID: "model-call-001",
		InputTokens: 8, OutputTokens: 4, Evidence: "  provider reference 123  ", IP: "127.0.0.1"}
	if _, err := service.Reconcile(ctx, principal, command); err != nil || store.command.Evidence != "provider reference 123" || store.writes != 1 {
		t.Fatalf("reconcile = %#v, %v", store.command, err)
	}
	for _, denied := range []identity.Principal{
		{UserID: principal.UserID, TokenID: principal.TokenID, Kind: identity.KindClient, Role: identity.RoleAdmin},
		{UserID: principal.UserID, TokenID: principal.TokenID, Kind: identity.KindSession, Role: identity.RoleUser},
		{UserID: principal.UserID, TokenID: principal.TokenID, Kind: identity.KindSession, Role: identity.RoleAdmin, ClientInstallationID: "i1234567"},
		{Kind: identity.KindSystem, Role: identity.RoleAdmin},
	} {
		if _, err := service.Reconcile(ctx, denied, command); domain.CodeOf(err) != domain.CodeForbidden {
			t.Fatalf("principal %#v: %v", denied, err)
		}
	}
	for _, invalid := range []ModelUsageReconcileCommand{
		{OwnerID: command.OwnerID, Source: command.Source, CallID: command.CallID, InputTokens: -1, Evidence: command.Evidence},
		{OwnerID: command.OwnerID, Source: command.Source, CallID: command.CallID, InputTokens: math.MaxInt64, OutputTokens: 1, Evidence: command.Evidence},
		{OwnerID: command.OwnerID, Source: command.Source, CallID: command.CallID, Evidence: "short"},
		{OwnerID: command.OwnerID, Source: command.Source, CallID: command.CallID, Evidence: "provider\nreference"},
		{OwnerID: command.OwnerID, Source: "invalid", CallID: command.CallID, Evidence: command.Evidence},
	} {
		if _, err := service.Reconcile(ctx, principal, invalid); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("invalid command %#v: %v", invalid, err)
		}
	}
	if store.writes != 1 {
		t.Fatalf("invalid commands reached store %d times", store.writes)
	}
}

func TestModelUsageAdminReadsAndSetsAccountBudget(t *testing.T) {
	store := &modelUsageAdminTestStore{budget: ModelAccountBudget{OwnerID: "u7654321", Configured: true, TokenLimit: 5000, MaxConcurrent: 2}}
	service, err := NewModelUsageAdminService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Kind: identity.KindSession, Role: identity.RoleAdmin}
	ctx := context.Background()
	budget, err := service.Budget(ctx, principal, "u7654321")
	if err != nil || !budget.Configured || budget.TokenLimit != 5000 || store.owner != "u7654321" {
		t.Fatalf("get budget = %#v, owner=%q, err=%v", budget, store.owner, err)
	}
	updated, err := service.SetBudget(ctx, principal, ModelAccountBudgetUpdate{
		OwnerID: "u7654321", TokenLimit: 7000, MaxConcurrent: 4, IP: "127.0.0.1",
	})
	if err != nil || updated.TokenLimit != 5000 || store.budgetEdit.TokenLimit != 7000 || store.budgetEdit.MaxConcurrent != 4 {
		t.Fatalf("set budget = %#v, stored=%#v, err=%v", updated, store.budgetEdit, err)
	}
	for _, invalid := range []ModelAccountBudgetUpdate{
		{OwnerID: "u7654321", TokenLimit: 0, MaxConcurrent: 1},
		{OwnerID: "u7654321", TokenLimit: 1, MaxConcurrent: 101},
		{OwnerID: "bad", TokenLimit: 1, MaxConcurrent: 1},
	} {
		if _, err := service.SetBudget(ctx, principal, invalid); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("invalid budget %#v: %v", invalid, err)
		}
	}
}
