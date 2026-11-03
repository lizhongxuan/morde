package workspace

import (
	"errors"
	"strings"
	"time"

	"morde/server/internal/domain"
)

type CopyState string

const (
	CopyReserved  CopyState = "reserved"
	Copying       CopyState = "copying"
	CopyUnknown   CopyState = "unknown"
	CopyCompleted CopyState = "completed"
	CopyFailed    CopyState = "failed"
)

type CopyReservation struct {
	ID                string
	OwnerID           string
	SourceWorkspaceID string
	TargetWorkspaceID string
	ServiceInstanceID string
	SourceRevision    int64
	ReservedBytes     int64
	State             CopyState
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (reservation CopyReservation) Validate() error {
	if !domain.ValidPendingRequestID(reservation.ID) ||
		!domain.ValidUserID(reservation.OwnerID) || !domain.ValidWorkspaceID(reservation.SourceWorkspaceID) ||
		!domain.ValidWorkspaceID(reservation.TargetWorkspaceID) || reservation.SourceWorkspaceID == reservation.TargetWorkspaceID ||
		strings.TrimSpace(reservation.ServiceInstanceID) == "" || len(reservation.ServiceInstanceID) > 128 ||
		reservation.SourceRevision < 0 || reservation.ReservedBytes < 0 || !reservation.State.Valid() ||
		reservation.CreatedAt.IsZero() || reservation.UpdatedAt.IsZero() {
		return errors.New("workspace copy reservation fields are invalid")
	}
	return nil
}

func (state CopyState) Valid() bool {
	switch state {
	case CopyReserved, Copying, CopyUnknown, CopyCompleted, CopyFailed:
		return true
	default:
		return false
	}
}

func (state CopyState) Active() bool {
	return state == CopyReserved || state == Copying || state == CopyUnknown
}
