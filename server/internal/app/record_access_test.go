package app

import (
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/record"
)

type recordAccessTestStore struct {
	grantCalls  int
	readCalls   int
	exportCalls int
}

func (s *recordAccessTestStore) SetRecordAdminGrant(context.Context, identity.Principal, string, string, record.AdminCapability, bool) (record.AdminGrant, error) {
	s.grantCalls++
	return record.AdminGrant{}, nil
}
func (*recordAccessTestStore) GetAdminRecordSettings(context.Context, identity.Principal, string) (RecordAdminSettings, error) {
	return RecordAdminSettings{Consents: []record.Consent{}, Grants: []record.AdminGrant{}}, nil
}
func (*recordAccessTestStore) GetRecordAdminAccess(context.Context, identity.Principal, string) (RecordAdminAccess, error) {
	return RecordAdminAccess{Consents: []record.Consent{}, Grants: []record.AdminGrant{}}, nil
}
func (*recordAccessTestStore) ListRecordMetadata(context.Context, identity.Principal, string, record.Query) (record.Page, error) {
	return record.Page{Items: []record.RecordMetadata{}}, nil
}
func (s *recordAccessTestStore) ReadRecord(context.Context, identity.Principal, string, string) (record.Content, error) {
	s.readCalls++
	return record.Content{}, nil
}
func (s *recordAccessTestStore) ExportRecord(context.Context, identity.Principal, string, string) (record.Content, error) {
	s.exportCalls++
	return record.Content{}, nil
}
func (*recordAccessTestStore) ListRecordAttachments(context.Context, identity.Principal, string, string) ([]record.AttachmentMetadata, error) {
	return []record.AttachmentMetadata{}, nil
}
func (*recordAccessTestStore) ReadRecordAttachment(context.Context, identity.Principal, string, string, string) (record.AttachmentContent, error) {
	return record.AttachmentContent{}, nil
}
func (*recordAccessTestStore) RequestRecordDelete(context.Context, identity.Principal, string, string) (record.DeleteTombstone, error) {
	return record.DeleteTombstone{}, nil
}
func (*recordAccessTestStore) ProcessRecordDeletes(context.Context, int32) (int32, error) {
	return 0, nil
}

func TestRecordAccessSeparatesGrantMutationAndExportCalls(t *testing.T) {
	store := &recordAccessTestStore{}
	service, err := NewRecordAccessService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Kind: identity.KindSession, Role: identity.RoleAdmin}
	if _, err := service.SetGrant(context.Background(), principal, "u7654321", "u1357924", record.CapabilityView, true); domain.CodeOf(err) != domain.CodeForbidden || store.grantCalls != 0 {
		t.Fatalf("non-superadmin grant mutation: err=%v calls=%d", err, store.grantCalls)
	}
	if _, err := service.AdminSettings(context.Background(), principal, "u7654321"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("non-superadmin record settings read: %v", err)
	}
	principal.Role = identity.RoleSuperAdmin
	if _, err := service.SetGrant(context.Background(), principal, "u7654321", "u1357924", record.CapabilityView, true); err != nil || store.grantCalls != 1 {
		t.Fatalf("superadmin grant mutation: err=%v calls=%d", err, store.grantCalls)
	}
	if settings, err := service.AdminSettings(context.Background(), principal, "u7654321"); err != nil || len(settings.Consents) != 0 {
		t.Fatalf("superadmin record settings read: %#v %v", settings, err)
	}
	if settings, err := service.AccessSettings(context.Background(), principal, "u7654321"); err != nil || len(settings.Grants) != 0 {
		t.Fatalf("superadmin own record access read: %#v %v", settings, err)
	}
	principal.Role = identity.RoleAdmin
	if settings, err := service.AccessSettings(context.Background(), principal, "u7654321"); err != nil || len(settings.Grants) != 0 {
		t.Fatalf("admin own record access read: %#v %v", settings, err)
	}
	if _, err := service.Read(context.Background(), principal, "u7654321", "v1234567"); err != nil || store.readCalls != 1 || store.exportCalls != 0 {
		t.Fatalf("ordinary detail read/export separation: err=%v read=%d export=%d", err, store.readCalls, store.exportCalls)
	}
	if _, err := service.Export(context.Background(), principal, "u7654321", "v1234567"); err != nil || store.exportCalls != 1 {
		t.Fatalf("explicit export: err=%v export=%d", err, store.exportCalls)
	}
}
