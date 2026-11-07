package workspace

import (
	"errors"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
)

type SharePermission string

const (
	ShareView SharePermission = "view"
	ShareEdit SharePermission = "edit"
)

type ShareState string

const (
	SharePending     ShareState = "pending"
	ShareClaiming    ShareState = "claiming"
	ShareClaimed     ShareState = "claimed"
	ShareExpired     ShareState = "expired"
	ShareInvalidated ShareState = "invalidated"
)

type ShareLink struct {
	ID                string
	OwnerID           string
	SourceWorkspaceID string
	SourceRevision    int64
	WorkspaceName     string
	Permission        SharePermission
	TokenHash         []byte
	ExpiresAt         time.Time
	State             ShareState
	ClaimantID        string
	TargetWorkspaceID string
	CreatedAt         time.Time
	ClaimedAt         *time.Time
}

func (value ShareLink) Validate() error {
	if !domain.ValidPendingRequestID(value.ID) || !domain.ValidUserID(value.OwnerID) ||
		!domain.ValidWorkspaceID(value.SourceWorkspaceID) || value.SourceRevision < 0 ||
		!utf8.ValidString(value.WorkspaceName) || utf8.RuneCountInString(value.WorkspaceName) < 1 || utf8.RuneCountInString(value.WorkspaceName) > 120 ||
		(value.Permission != ShareView && value.Permission != ShareEdit) || len(value.TokenHash) != 32 ||
		value.ExpiresAt.IsZero() || value.CreatedAt.IsZero() ||
		(value.State != SharePending && value.State != ShareClaiming && value.State != ShareClaimed &&
			value.State != ShareExpired && value.State != ShareInvalidated) {
		return errors.New("workspace share link fields are invalid")
	}
	if value.ClaimantID != "" && !domain.ValidUserID(value.ClaimantID) {
		return errors.New("workspace share claimant is invalid")
	}
	if value.TargetWorkspaceID != "" && !domain.ValidWorkspaceID(value.TargetWorkspaceID) {
		return errors.New("workspace share target is invalid")
	}
	if !value.ExpiresAt.After(value.CreatedAt) {
		return errors.New("workspace share expiry is invalid")
	}
	switch value.State {
	case SharePending, ShareExpired, ShareInvalidated:
		if value.ClaimantID != "" || value.TargetWorkspaceID != "" || value.ClaimedAt != nil {
			return errors.New("workspace share state contains claim data")
		}
	case ShareClaiming:
		if value.ClaimantID == "" || value.ClaimedAt != nil {
			return errors.New("workspace share claim state is incomplete")
		}
	case ShareClaimed:
		if value.ClaimantID == "" || value.TargetWorkspaceID == "" || value.ClaimedAt == nil || value.ClaimedAt.IsZero() {
			return errors.New("workspace share claim state is incomplete")
		}
	}
	if value.ClaimedAt != nil && value.ClaimedAt.Before(value.CreatedAt) {
		return errors.New("workspace share claim state is incomplete")
	}
	return nil
}
