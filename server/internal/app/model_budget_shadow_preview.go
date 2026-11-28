package app

import (
	"context"
	"time"

	"morde/server/internal/domain/project"
)

// ModelTaskBudgetShadowPreviewRequest describes the budget dimensions needed
// to evaluate a Task model call without reserving capacity or changing Task
// state. The persisted Task fence is used to reject stale snapshots.
type ModelTaskBudgetShadowPreviewRequest struct {
	Fence                 ModelTaskFence
	OwnerID               string
	ModelID               string
	ProviderEndpoint      string
	ProviderModel         string
	ConnectionVersion     time.Time
	ProjectID             string
	PersonalModel         bool
	InputTokenReservation int64
	ProjectRate           *project.RateSnapshot
}

// ModelTaskBudgetShadowPreview is a point-in-time, read-only view of output
// capacity after existing settled usage and active reservations. A nil output
// limit with Applicable=true means the dimension could not be established;
// it must never be interpreted as unlimited.
type ModelTaskBudgetShadowPreview struct {
	CapturedAt time.Time

	AccountApplicable  bool
	AccountOutputLimit *int64

	TaskApplicable  bool
	TaskOutputLimit *int64

	ProjectApplicable  bool
	ProjectOutputLimit *int64

	UnknownReasons []string
}

// ModelTaskBudgetShadowPreviewReader must only read a consistent snapshot. It
// must not create or release holds, update Task state, or acquire budget locks.
type ModelTaskBudgetShadowPreviewReader interface {
	PreviewTaskModelBudget(context.Context, ModelTaskBudgetShadowPreviewRequest) (ModelTaskBudgetShadowPreview, error)
}
