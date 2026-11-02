package app

import (
	"context"
	"errors"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

const (
	DefaultPersonalDataQuotaBytes         int64 = 5 << 30
	DefaultPersonalDataMaxAttachmentBytes int64 = personal.DefaultMaxAttachmentBytes
	maxPersonalDataQuotaCleanupLimit      int32 = 1000
)

type PersonalDataQuota struct {
	OwnerID            string
	UsedBytes          int64
	ReservedBytes      int64
	QuotaBytes         int64
	MaxAttachmentBytes int64
	Version            int64
}

type PersonalDataQuotaStore interface {
	GetAdminPersonalDataQuota(context.Context, identity.Principal, string) (PersonalDataQuota, error)
	SetAdminPersonalDataQuota(context.Context, identity.Principal, string, int64, int64, int64, string) (PersonalDataQuota, error)
	ReservePersonalAttachment(context.Context, identity.Principal, string, int64) (string, error)
	ReleasePersonalAttachmentReservation(context.Context, string, string) error
	CleanupExpiredPersonalDataQuotaReservations(context.Context, time.Time, int32) (int32, error)
}

type PersonalDataQuotaService struct {
	store PersonalDataQuotaStore
}

func NewPersonalDataQuotaService(store PersonalDataQuotaStore) (*PersonalDataQuotaService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("personal_data_quota", "personal data quota store is required")
	}
	return &PersonalDataQuotaService{store: store}, nil
}

func (s *PersonalDataQuotaService) GetAdminPersonalDataQuota(ctx context.Context, principal identity.Principal, ownerID string) (PersonalDataQuota, error) {
	if err := authorizePersonalDataQuotaAdmin(principal); err != nil {
		return PersonalDataQuota{}, err
	}
	if !domain.ValidUserID(ownerID) {
		return PersonalDataQuota{}, domain.InvalidArgument("owner_id", "personal data quota owner is invalid")
	}
	value, err := s.store.GetAdminPersonalDataQuota(ctx, principal, ownerID)
	if err != nil {
		return PersonalDataQuota{}, err
	}
	if err := validatePersonalDataQuota(value, ownerID); err != nil {
		return PersonalDataQuota{}, domain.NewError(domain.CodeInternal, "personal data quota receipt is invalid")
	}
	return value, nil
}

func (s *PersonalDataQuotaService) SetAdminPersonalDataQuota(ctx context.Context, principal identity.Principal, ownerID string, expectedVersion, quotaBytes, maxAttachmentBytes int64, ip string) (PersonalDataQuota, error) {
	if err := authorizePersonalDataQuotaAdmin(principal); err != nil {
		return PersonalDataQuota{}, err
	}
	if !domain.ValidUserID(ownerID) || expectedVersion < 1 || quotaBytes < 1 || maxAttachmentBytes < 1 ||
		maxAttachmentBytes > personal.MaxAttachmentBytes {
		return PersonalDataQuota{}, domain.InvalidArgument("personal_data_quota", "personal data quota limits or version are invalid")
	}
	value, err := s.store.SetAdminPersonalDataQuota(ctx, principal, ownerID, expectedVersion, quotaBytes, maxAttachmentBytes, ip)
	if err != nil {
		return PersonalDataQuota{}, err
	}
	if err := validatePersonalDataQuota(value, ownerID); err != nil {
		return PersonalDataQuota{}, domain.NewError(domain.CodeInternal, "personal data quota receipt is invalid")
	}
	return value, nil
}

func (s *PersonalDataQuotaService) ReservePersonalAttachment(ctx context.Context, principal identity.Principal, uploadID string, sizeBytes int64) (string, error) {
	if err := validatePersonalAttachmentPrincipal(principal); err != nil {
		return "", err
	}
	if !personal.ValidPersonalAttachmentUploadID(uploadID) || sizeBytes < 0 || sizeBytes > personal.MaxAttachmentBytes {
		return "", domain.InvalidArgument("attachment_reservation", "personal attachment reservation is invalid")
	}
	reservationID, err := s.store.ReservePersonalAttachment(ctx, principal, uploadID, sizeBytes)
	if err != nil {
		return "", err
	}
	if !validAttachmentReservationID(reservationID) {
		return "", domain.NewError(domain.CodeInternal, "personal attachment reservation receipt is invalid")
	}
	return reservationID, nil
}

func (s *PersonalDataQuotaService) ReleasePersonalAttachmentReservation(ctx context.Context, ownerID, reservationID string) error {
	if !domain.ValidUserID(ownerID) || !validAttachmentReservationID(reservationID) {
		return domain.InvalidArgument("attachment_reservation", "personal attachment reservation is invalid")
	}
	return s.store.ReleasePersonalAttachmentReservation(ctx, ownerID, reservationID)
}

func (s *PersonalDataQuotaService) CleanupExpiredPersonalDataQuotaReservations(ctx context.Context, now time.Time, limit int32) (int32, error) {
	if ctx == nil || now.IsZero() || limit < 1 || limit > maxPersonalDataQuotaCleanupLimit {
		return 0, domain.InvalidArgument("limit", "personal data quota cleanup request is invalid")
	}
	released, err := s.store.CleanupExpiredPersonalDataQuotaReservations(ctx, now.UTC(), limit)
	if err != nil {
		return 0, err
	}
	if released < 0 || released > limit {
		return 0, domain.NewError(domain.CodeInternal, "personal data quota cleanup count is invalid")
	}
	return released, nil
}

func validatePersonalDataQuota(value PersonalDataQuota, ownerID string) error {
	if value.OwnerID != ownerID || !domain.ValidUserID(value.OwnerID) || value.UsedBytes < 0 || value.ReservedBytes < 0 ||
		value.QuotaBytes < 1 || value.MaxAttachmentBytes < 1 || value.MaxAttachmentBytes > personal.MaxAttachmentBytes || value.Version < 1 {
		return errors.New("invalid personal data quota")
	}
	return nil
}

func authorizePersonalDataQuotaAdmin(principal identity.Principal) error {
	if principal.Kind != identity.KindSession || principal.ClientInstallationID != "" {
		return domain.NewError(domain.CodeForbidden, "unbound admin session required")
	}
	return authorizeAdmin(principal)
}

var _ PersonalAttachmentQuota = (*PersonalDataQuotaService)(nil)
