package app

import (
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type adminWorkspaceStoreStub struct {
	page      workspace.AdminPage
	item      workspace.AdminSummary
	err       error
	listCalls int
	getCalls  int
}

func (s *adminWorkspaceStoreStub) ListAdminWorkspaces(context.Context, PageRequest) (workspace.AdminPage, error) {
	s.listCalls++
	return s.page, s.err
}

func (s *adminWorkspaceStoreStub) GetAdminWorkspace(context.Context, string) (workspace.AdminSummary, error) {
	s.getCalls++
	return s.item, s.err
}

func TestAdminWorkspaceServiceAuthorizesAndValidatesRequests(t *testing.T) {
	store := &adminWorkspaceStoreStub{page: workspace.AdminPage{Items: []workspace.AdminSummary{}}}
	service, err := NewAdminWorkspaceService(store)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleAdmin, Kind: identity.KindSession}
	if _, err := service.List(context.Background(), admin, PageRequest{Limit: 1}); err != nil || store.listCalls != 1 {
		t.Fatalf("List(admin) = %v calls=%d", err, store.listCalls)
	}
	if _, err := service.List(context.Background(), admin, PageRequest{Limit: 101}); domain.CodeOf(err) != domain.CodeInvalidArgument || store.listCalls != 1 {
		t.Fatalf("List(invalid page) = %v calls=%d", err, store.listCalls)
	}
	user := admin
	user.Role = identity.RoleUser
	if _, err := service.List(context.Background(), user, PageRequest{Limit: 1}); domain.CodeOf(err) != domain.CodeForbidden || store.listCalls != 1 {
		t.Fatalf("List(user) = %v calls=%d", err, store.listCalls)
	}
	if _, err := service.Get(context.Background(), admin, "bad-id"); domain.CodeOf(err) != domain.CodeNotFound || store.getCalls != 0 {
		t.Fatalf("Get(invalid ID) = %v calls=%d", err, store.getCalls)
	}
	if _, err := service.Get(context.Background(), admin, "w1234567"); err != nil || store.getCalls != 1 {
		t.Fatalf("Get(admin) = %v calls=%d", err, store.getCalls)
	}
}
