package app

import (
	"context"
	"encoding/hex"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type TaskRequirementManifestConfirmationInput struct {
	TaskRevision      int64
	PlanRevision      int64
	PlanEventSequence int64
	ManifestDigest    string
	IdempotencyKey    string
}

type TaskRequirementManifestConfirmationCommand struct {
	TaskID             string
	ActorUserID        string
	ActorPrincipalKind string
	TaskRevision       int64
	PlanRevision       int64
	PlanEventSequence  int64
	ManifestDigest     string
	IdempotencyKey     string
}

type TaskRequirementManifestConfirmation struct {
	TaskID                   string    `json:"task_id"`
	TaskRevision             int64     `json:"task_revision"`
	PlanRevision             int64     `json:"plan_revision"`
	PlanEventSequence        int64     `json:"plan_event_sequence"`
	ManifestDigest           string    `json:"manifest_digest"`
	ConfirmedByUserID        string    `json:"confirmed_by_user_id"`
	ConfirmedByPrincipalKind string    `json:"confirmed_by_principal_kind"`
	TaskAuthorizationVersion int64     `json:"task_authorization_version"`
	ConfirmedAt              time.Time `json:"confirmed_at"`
}

type TaskRequirementManifestConfirmationStore interface {
	ConfirmTaskRequirementManifest(context.Context, TaskRequirementManifestConfirmationCommand) (TaskRequirementManifestConfirmation, error)
}

func (s *TaskService) ConfirmTaskRequirementManifest(ctx context.Context, principal identity.Principal, taskID string,
	input TaskRequirementManifestConfirmationInput) (TaskRequirementManifestConfirmation, error) {
	if s == nil || ctx == nil {
		return TaskRequirementManifestConfirmation{}, domain.NewError(domain.CodeInvalidArgument, "task requirement confirmation request is invalid")
	}
	if err := validateTaskReadPrincipal(principal); err != nil {
		return TaskRequirementManifestConfirmation{}, err
	}
	if !domain.ValidTaskID(taskID) || input.TaskRevision < 0 || input.PlanRevision < 1 || input.PlanEventSequence < 1 ||
		!validTaskRequirementManifestDigest(input.ManifestDigest) {
		return TaskRequirementManifestConfirmation{}, domain.InvalidArgument("task_requirement_manifest_confirmation", "confirmation fence or idempotency key is invalid")
	}
	if err := validateIdempotencyKey(input.IdempotencyKey); err != nil {
		return TaskRequirementManifestConfirmation{}, err
	}
	store, ok := s.store.(TaskRequirementManifestConfirmationStore)
	if !ok {
		return TaskRequirementManifestConfirmation{}, domain.NewError(domain.CodeUnsupported, "task requirement confirmation storage is unavailable")
	}
	// Apply the same current Task read policy as the existing Task snapshot API.
	// The Store repeats all revision and authorization checks in the write transaction.
	details, err := s.Get(ctx, principal, taskID)
	if err != nil {
		return TaskRequirementManifestConfirmation{}, err
	}
	if details.Task.CurrentRevision == nil || details.Plan == nil ||
		*details.Task.CurrentRevision != input.TaskRevision || details.Plan.TaskRevision != input.TaskRevision ||
		details.Plan.Revision != input.PlanRevision {
		return TaskRequirementManifestConfirmation{}, domain.NewError(domain.CodeConflict, "task requirement plan revision is stale")
	}
	digest, err := details.Plan.RequirementManifestDigest()
	if err != nil || digest != input.ManifestDigest {
		return TaskRequirementManifestConfirmation{}, domain.NewError(domain.CodeConflict, "task requirement manifest digest is stale")
	}
	return store.ConfirmTaskRequirementManifest(ctx, TaskRequirementManifestConfirmationCommand{
		TaskID: taskID, ActorUserID: principal.UserID, ActorPrincipalKind: string(principal.Kind),
		TaskRevision: input.TaskRevision, PlanRevision: input.PlanRevision, PlanEventSequence: input.PlanEventSequence,
		ManifestDigest: input.ManifestDigest, IdempotencyKey: input.IdempotencyKey,
	})
}

func validTaskRequirementManifestDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
}
