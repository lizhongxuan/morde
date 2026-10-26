package app

import (
	"context"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/control"
	"morde/server/internal/runtimeclient"
)

const ApplicationInstallUnknown = "unknown"

// ApplicationInstall reserves an idempotent Runtime install before dispatch.
// Unknown means that the dispatch outcome is not yet known, so Host release
// must remain closed until the same request is retried and reconciled.
type ApplicationInstall struct {
	HostID            string
	OwnerID           string
	RequestID         string
	ApplicationID     string
	RuntimeGeneration int64
	RuntimeInstanceID string
	ControlVersion    int64
	Status            string
	Operation         *runtimeclient.ApplicationInstallOperation
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (value ApplicationInstall) Validate() error {
	if !domain.ValidHostID(value.HostID) || !domain.ValidUserID(value.OwnerID) ||
		!control.ValidRequestID(value.RequestID) || !ValidHostApplicationID(value.ApplicationID) ||
		value.RuntimeGeneration < 1 || value.RuntimeInstanceID == "" || len(value.RuntimeInstanceID) > 128 || value.ControlVersion < 0 {
		return domain.InvalidArgument("application_install", "application install identity is invalid")
	}
	if value.Status == ApplicationInstallUnknown && value.Operation == nil {
		return nil
	}
	if value.Operation == nil || value.Operation.ApplicationID != value.ApplicationID ||
		string(value.Operation.Status) != value.Status || !runtimeclient.ValidApplicationInstallOperation(*value.Operation) {
		return domain.InvalidArgument("application_install", "application install operation is invalid")
	}
	return nil
}

func (value ApplicationInstall) Terminal() bool {
	return value.Status == string(runtimeclient.OperationSucceeded) || value.Status == string(runtimeclient.OperationFailed)
}

type ApplicationInstallStore interface {
	ReserveApplicationInstall(context.Context, ApplicationInstall) (ApplicationInstall, bool, error)
	GetPendingApplicationInstall(context.Context, string, string) (ApplicationInstall, error)
	RecordApplicationInstallOperation(context.Context, string, string, string, runtimeclient.ApplicationInstallOperation) (ApplicationInstall, error)
}
