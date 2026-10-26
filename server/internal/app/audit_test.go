package app

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
)

type auditServiceStore struct {
	AuditStore
	entry  audit.Entry
	filter AuditFilter
}

func (s *auditServiceStore) WriteAudit(_ context.Context, entry audit.Entry) error {
	s.entry = entry
	return nil
}

func (s *auditServiceStore) ListAuditLogs(_ context.Context, filter AuditFilter) ([]AuditLog, error) {
	s.filter = filter
	return []AuditLog{}, nil
}

func TestAuditServiceValidatesAndRedactsBeforeWrite(t *testing.T) {
	store := &auditServiceStore{}
	service, err := NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	entry := audit.Entry{
		ActorID: "u1234567", ActorKind: audit.ActorSession, Action: audit.ActionUserUpdate,
		Detail: map[string]any{
			"changed": "role", "Password": "secret", "nested": map[string]any{"api-key": "hidden", "safe": true},
		},
	}
	if err := service.Write(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if store.entry.Detail["Password"] != nil || store.entry.Detail["changed"] != "role" {
		t.Fatalf("audit detail = %#v", store.entry.Detail)
	}
	nested := store.entry.Detail["nested"].(map[string]any)
	if nested["api-key"] != nil || nested["safe"] != true {
		t.Fatalf("nested audit detail = %#v", nested)
	}
	if err := service.Write(context.Background(), audit.Entry{ActorKind: audit.ActorSession, Action: "unknown"}); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("invalid action error = %v", err)
	}
}

func TestAuditServiceAcceptsClientActor(t *testing.T) {
	store := &auditServiceStore{}
	service, err := NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	entry := audit.Entry{ActorID: "i1234567", ActorKind: audit.ActorClient, Action: audit.ActionHostEnter}
	if err := service.Write(context.Background(), entry); err != nil {
		t.Fatalf("Write(client actor) error = %v", err)
	}
	if store.entry.ActorKind != audit.ActorClient || store.entry.Action != audit.ActionHostEnter {
		t.Fatalf("stored audit entry = %#v", store.entry)
	}
}

func TestAuditServiceListsOnlyForAdminWithBoundedFilters(t *testing.T) {
	store := &auditServiceStore{}
	service, err := NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Role: identity.RoleAdmin, Kind: identity.KindSession}
	since := time.Date(2026, 10, 20, 0, 0, 0, 0, time.FixedZone("test", 3600))
	filter := AuditFilter{Actor: "u7654321", Action: string(audit.ActionLogin), Target: "h1234567", Since: &since, Limit: 12}
	if _, err := service.List(context.Background(), principal, filter); err != nil {
		t.Fatal(err)
	}
	if store.filter.Actor != filter.Actor || store.filter.Action != filter.Action || store.filter.Target != filter.Target ||
		store.filter.Limit != 12 || store.filter.Since == nil || store.filter.Since.Location() != time.UTC {
		t.Fatalf("stored audit filter = %#v", store.filter)
	}
	user := principal
	user.Role = identity.RoleUser
	if _, err := service.List(context.Background(), user, filter); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("non-admin List() error = %v", err)
	}
	filter.Limit = 501
	if _, err := service.List(context.Background(), principal, filter); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("oversized limit error = %v", err)
	}
}
