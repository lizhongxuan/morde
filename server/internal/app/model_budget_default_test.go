package app

import (
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type modelBudgetDefaultTestStore struct {
	value     ModelBudgetDefault
	command   UpdateModelBudgetDefaultCommand
	principal identity.Principal
	ip        string
	reads     int
	writes    int
}

func (s *modelBudgetDefaultTestStore) GetModelBudgetDefault(context.Context) (ModelBudgetDefault, error) {
	s.reads++
	return s.value, nil
}

func (s *modelBudgetDefaultTestStore) SetModelBudgetDefault(_ context.Context, principal identity.Principal, command UpdateModelBudgetDefaultCommand, ip string) (ModelBudgetDefault, error) {
	s.writes++
	s.principal, s.command, s.ip = principal, command, ip
	s.value = ModelBudgetDefault{Configured: true, TokenLimit: command.TokenLimit, MaxConcurrent: command.MaxConcurrent, Version: command.ExpectedVersion + 1}
	return s.value, nil
}

func TestAdminModelBudgetDefaultServiceAuthorizesAndValidates(t *testing.T) {
	store := &modelBudgetDefaultTestStore{}
	service, err := NewAdminModelBudgetDefaultService(store)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Kind: identity.KindSession, Role: identity.RoleAdmin}
	value, err := service.Get(context.Background(), admin)
	if err != nil || value.Configured || store.reads != 1 {
		t.Fatalf("initial default = %+v, %v; reads=%d", value, err, store.reads)
	}
	if _, err := service.Get(context.Background(), identity.Principal{UserID: admin.UserID, TokenID: admin.TokenID, Kind: identity.KindClient, Role: identity.RoleAdmin}); domain.CodeOf(err) != domain.CodeForbidden || store.reads != 1 {
		t.Fatalf("client read was not rejected before storage: %v; reads=%d", err, store.reads)
	}
	for _, command := range []UpdateModelBudgetDefaultCommand{
		{ExpectedVersion: -1, TokenLimit: 1000, MaxConcurrent: 2},
		{ExpectedVersion: 0, TokenLimit: 0, MaxConcurrent: 2},
		{ExpectedVersion: 0, TokenLimit: 1000, MaxConcurrent: 0},
		{ExpectedVersion: 0, TokenLimit: 1000, MaxConcurrent: 101},
	} {
		if _, err := service.Update(context.Background(), admin, command, "127.0.0.1"); domain.CodeOf(err) != domain.CodeInvalidArgument {
			t.Fatalf("invalid default %#v: %v", command, err)
		}
	}
	if store.writes != 0 {
		t.Fatalf("invalid updates reached store %d times", store.writes)
	}
	command := UpdateModelBudgetDefaultCommand{ExpectedVersion: 0, TokenLimit: 25000, MaxConcurrent: 3}
	value, err = service.Update(context.Background(), admin, command, "127.0.0.1")
	if err != nil || !value.Configured || value.TokenLimit != command.TokenLimit || value.MaxConcurrent != command.MaxConcurrent || value.Version != 1 ||
		store.writes != 1 || store.principal.UserID != admin.UserID || store.principal.TokenID != admin.TokenID ||
		store.principal.Kind != admin.Kind || store.principal.Role != admin.Role || store.command != command || store.ip != "127.0.0.1" {
		t.Fatalf("updated default = %+v, %v; store=%+v", value, err, store)
	}
}
