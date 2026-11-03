package workspace

import (
	"errors"
	"time"

	"morde/server/internal/domain"
)

type OwnershipTransferState string

const (
	OwnershipTransferPending   OwnershipTransferState = "pending"
	OwnershipTransferAccepting OwnershipTransferState = "accepting"
	OwnershipTransferUnknown   OwnershipTransferState = "unknown"
	OwnershipTransferAccepted  OwnershipTransferState = "accepted"
	OwnershipTransferCancelled OwnershipTransferState = "cancelled"
)

type OwnershipTransfer struct {
	ID               string                 `json:"id"`
	WorkspaceID      string                 `json:"workspace_id"`
	FromOwnerID      string                 `json:"from_owner_id"`
	ToOwnerID        string                 `json:"to_owner_id"`
	State            OwnershipTransferState `json:"state"`
	ExpectedRevision int64                  `json:"expected_revision"`
	OperationID      string                 `json:"operation_id,omitempty"`
	Version          int64                  `json:"version"`
	CreatedAt        time.Time              `json:"created_at"`
	UpdatedAt        time.Time              `json:"updated_at"`
	AcceptedAt       *time.Time             `json:"accepted_at,omitempty"`
}

func NewOwnershipTransferID() (string, error) { return domain.NewPendingRequestID() }

func ValidOwnershipTransferID(value string) bool { return domain.ValidPendingRequestID(value) }

func (state OwnershipTransferState) Valid() bool {
	switch state {
	case OwnershipTransferPending, OwnershipTransferAccepting, OwnershipTransferUnknown, OwnershipTransferAccepted, OwnershipTransferCancelled:
		return true
	default:
		return false
	}
}

func (transfer OwnershipTransfer) Validate() error {
	if !ValidOwnershipTransferID(transfer.ID) || !domain.ValidWorkspaceID(transfer.WorkspaceID) ||
		!domain.ValidUserID(transfer.FromOwnerID) || !domain.ValidUserID(transfer.ToOwnerID) || transfer.FromOwnerID == transfer.ToOwnerID ||
		!transfer.State.Valid() || transfer.ExpectedRevision < 0 || transfer.Version < 1 || transfer.CreatedAt.IsZero() || transfer.UpdatedAt.IsZero() {
		return errors.New("workspace ownership transfer fields are invalid")
	}
	if transfer.State == OwnershipTransferAccepting || transfer.State == OwnershipTransferUnknown || transfer.State == OwnershipTransferAccepted {
		if !domain.ValidOperationID(transfer.OperationID) {
			return errors.New("workspace ownership transfer operation is invalid")
		}
	} else if transfer.OperationID != "" {
		return errors.New("workspace ownership transfer has an unexpected operation")
	}
	if (transfer.State == OwnershipTransferAccepted) != (transfer.AcceptedAt != nil) {
		return errors.New("workspace ownership transfer acceptance time is invalid")
	}
	if transfer.AcceptedAt != nil && (transfer.AcceptedAt.IsZero() || transfer.AcceptedAt.Before(transfer.CreatedAt)) {
		return errors.New("workspace ownership transfer acceptance time is invalid")
	}
	return nil
}
