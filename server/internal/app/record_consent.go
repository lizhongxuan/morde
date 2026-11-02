package app

import (
	"context"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/record"
)

type RecordConsentStore interface {
	ListRecordConsents(context.Context, identity.Principal) ([]record.Consent, error)
	SetRecordConsent(context.Context, identity.Principal, record.Purpose, bool) (record.Consent, error)
	GetRecordQuota(context.Context, identity.Principal, string) (record.Quota, error)
	SetRecordQuota(context.Context, identity.Principal, string, int64, int32, int64) (record.Quota, error)
}

type RecordConsentService struct {
	store RecordConsentStore
}

func NewRecordConsentService(store RecordConsentStore) (*RecordConsentService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("record_consent", "record consent store is required")
	}
	return &RecordConsentService{store: store}, nil
}

func (s *RecordConsentService) List(ctx context.Context, principal identity.Principal) ([]record.Consent, error) {
	if err := authorizeRecordOwner(principal); err != nil {
		return nil, err
	}
	return s.store.ListRecordConsents(ctx, principal)
}

func (s *RecordConsentService) Set(ctx context.Context, principal identity.Principal, purpose record.Purpose, enabled bool) (record.Consent, error) {
	if err := authorizeRecordOwner(principal); err != nil {
		return record.Consent{}, err
	}
	if !purpose.Valid() {
		return record.Consent{}, domain.InvalidArgument("purpose", "record consent purpose is invalid")
	}
	return s.store.SetRecordConsent(ctx, principal, purpose, enabled)
}

func (s *RecordConsentService) Quota(ctx context.Context, principal identity.Principal, ownerID string) (record.Quota, error) {
	if err := authorizeRecordOwner(principal); err != nil {
		return record.Quota{}, err
	}
	if ownerID == "" {
		ownerID = principal.UserID
	}
	if ownerID != principal.UserID && (principal.Role != identity.RoleSuperAdmin || principal.Kind != identity.KindSession) {
		return record.Quota{}, domain.NewError(domain.CodeForbidden, "record quota access is denied")
	}
	if !domain.ValidUserID(ownerID) {
		return record.Quota{}, domain.InvalidArgument("owner_id", "record quota owner is invalid")
	}
	return s.store.GetRecordQuota(ctx, principal, ownerID)
}

func (s *RecordConsentService) SetQuota(ctx context.Context, principal identity.Principal, ownerID string, expectedVersion int64, retentionDays int32, limitBytes int64) (record.Quota, error) {
	if principal.Role != identity.RoleSuperAdmin || principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) {
		return record.Quota{}, domain.NewError(domain.CodeForbidden, "record quota management is denied")
	}
	if !domain.ValidUserID(ownerID) || expectedVersion < 1 || retentionDays < 1 || retentionDays > record.MaxRetentionDays ||
		limitBytes < 1 || limitBytes > record.MaxQuotaBytes {
		return record.Quota{}, domain.InvalidArgument("record_quota", "record quota settings are invalid")
	}
	return s.store.SetRecordQuota(ctx, principal, ownerID, expectedVersion, retentionDays, limitBytes)
}

func authorizeRecordOwner(principal identity.Principal) error {
	if !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		(principal.Kind != identity.KindSession && principal.Kind != identity.KindClient) ||
		(principal.Kind == identity.KindClient && !domain.ValidClientInstallationID(principal.ClientInstallationID)) {
		return domain.NewError(domain.CodeUnauthenticated, "authentication required")
	}
	return nil
}
