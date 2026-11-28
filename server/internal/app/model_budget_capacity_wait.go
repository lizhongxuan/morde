package app

import (
	"context"
	"errors"
	"time"
)

var ErrTaskModelBudgetCapacityWaiting = errors.New("task is waiting for model capacity")

const (
	TaskModelBudgetCapacityWaitReason         = "model_budget_capacity"
	TaskModelBudgetCapacityExpiredReason      = "model_budget_capacity_timeout"
	TaskModelBudgetCapacityExhaustedReason    = "model_budget_exhausted"
	TaskModelBudgetCapacityInvalidatedReason  = "model_budget_capacity_invalidated"
	TaskModelBudgetCapacityWaitTimeout        = 5 * time.Minute
	TaskModelBudgetCapacityReconcileBatchSize = 100
)

// TaskModelBudgetCapacityWaitRequest is the complete, non-prompt identity for
// suspending a Task after a temporary model concurrency limit. The store must
// verify the live Task/Agent/lease fence and commit the waiting Task state and
// durable wait row atomically. Input and minimum output reservations let the
// reconciler distinguish releasable in-flight capacity from spent quota.
type TaskModelBudgetCapacityWaitRequest struct {
	Fence                 ModelTaskFence
	ModelID               string
	ProjectID             string
	DSHCallID             string
	LedgerCallID          string
	BudgetCallID          string
	RequestDigest         string
	InputTokenReservation int64
	MinimumOutputTokens   int64
	MinimumProjectMicros  int64
	ProjectBudgetCurrency string
}

type TaskModelBudgetCapacityReconcileResult struct {
	Requeued  int32
	Paused    int32
	Discarded int32
	Deferred  int32
}

// TaskModelBudgetCapacityWaitStore is implemented by stores that can persist
// a capacity wait and fairly requeue it after ledger capacity is available.
type TaskModelBudgetCapacityWaitStore interface {
	WaitForTaskModelBudgetCapacity(context.Context, TaskModelBudgetCapacityWaitRequest) error
	ReconcileTaskModelBudgetCapacityWaits(context.Context, int32) (TaskModelBudgetCapacityReconcileResult, error)
}
