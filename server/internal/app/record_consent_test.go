package app

import (
	"context"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/record"
)

type recordConsentTestStore struct {
	setCalls int
	quota    record.Quota
}

func (s *recordConsentTestStore) ListRecordConsents(context.Context, identity.Principal) ([]record.Consent, error) {
	return []record.Consent{}, nil
}
func (s *recordConsentTestStore) SetRecordConsent(_ context.Context, principal identity.Principal, purpose record.Purpose, enabled bool) (record.Consent, error) {
	s.setCalls++
	return record.Consent{OwnerID: principal.UserID, Purpose: purpose, Enabled: enabled, Version: 1}, nil
}
func (s *recordConsentTestStore) GetRecordQuota(_ context.Context, _ identity.Principal, ownerID string) (record.Quota, error) {
	s.quota.OwnerID = ownerID
	return s.quota, nil
}
func (s *recordConsentTestStore) SetRecordQuota(_ context.Context, _ identity.Principal, ownerID string, expectedVersion int64, retentionDays int32, limitBytes int64) (record.Quota, error) {
	s.quota = record.Quota{OwnerID: ownerID, RetentionDays: retentionDays, LimitBytes: limitBytes, Version: expectedVersion + 1}
	return s.quota, nil
}

func TestRecordConsentServiceValidatesPurposeAndQuotaAdmin(t *testing.T) {
	store := &recordConsentTestStore{}
	service, err := NewRecordConsentService(store)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: "u1234567", TokenID: "t1234567", Kind: identity.KindSession, Role: identity.RoleUser}
	if _, err := service.Set(context.Background(), principal, "", true); domain.CodeOf(err) != domain.CodeInvalidArgument || store.setCalls != 0 {
		t.Fatalf("invalid consent purpose reached store: err=%v calls=%d", err, store.setCalls)
	}
	if value, err := service.Set(context.Background(), principal, record.PurposeTrainingContribution, true); err != nil || !value.Enabled || store.setCalls != 1 {
		t.Fatalf("set independent training consent: %#v err=%v calls=%d", value, err, store.setCalls)
	}
	if _, err := service.SetQuota(context.Background(), principal, principal.UserID, 1, 30, record.DefaultQuotaBytes); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("non-superadmin quota write: %v", err)
	}
	superAdmin := principal
	superAdmin.Role = identity.RoleSuperAdmin
	quota, err := service.SetQuota(context.Background(), superAdmin, "u7654321", 1, 30, record.DefaultQuotaBytes)
	if err != nil || quota.OwnerID != "u7654321" {
		t.Fatalf("superadmin quota write: %#v %v", quota, err)
	}
}
