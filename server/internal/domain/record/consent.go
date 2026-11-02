package record

import (
	"time"

	"morde/server/internal/domain"
)

type Purpose string

const (
	PurposeFullRecord          Purpose = "full_record"
	PurposeTrainingContribution Purpose = "training_contribution"
)

func (p Purpose) Valid() bool {
	return p == PurposeFullRecord || p == PurposeTrainingContribution
}

type Consent struct {
	OwnerID   string
	Purpose   Purpose
	Enabled   bool
	Version   int64
	UpdatedAt time.Time
}

func ValidateConsent(value Consent) error {
	if !domain.ValidUserID(value.OwnerID) || !value.Purpose.Valid() || value.Version < 0 {
		return domain.InvalidArgument("consent", "record consent fields are invalid")
	}
	if value.Version > 0 && value.UpdatedAt.IsZero() {
		return domain.InvalidArgument("consent", "record consent timestamp is required")
	}
	return nil
}

type Quota struct {
	OwnerID       string
	RetentionDays int32
	LimitBytes    int64
	UsedBytes     int64
	ReservedBytes int64
	Version       int64
	UpdatedAt     time.Time
}

const (
	DefaultRetentionDays int32 = 30
	DefaultQuotaBytes    int64 = 1 << 30
	MaxRetentionDays     int32 = 3650
	MaxQuotaBytes        int64 = 1 << 40
)

func ValidateQuota(value Quota) error {
	if !domain.ValidUserID(value.OwnerID) || value.RetentionDays < 1 || value.RetentionDays > MaxRetentionDays ||
		value.LimitBytes < 1 || value.LimitBytes > MaxQuotaBytes || value.UsedBytes < 0 ||
		value.ReservedBytes < 0 || value.Version < 1 || value.UpdatedAt.IsZero() {
		return domain.InvalidArgument("record_quota", "record quota fields are invalid")
	}
	return nil
}
