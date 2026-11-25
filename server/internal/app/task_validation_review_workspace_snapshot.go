package app

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

// TaskValidationFinalWorkspaceSnapshotReader reads the Task's current private
// Workspace manifest under the same Host fence that ran its typed checks.
type TaskValidationFinalWorkspaceSnapshotReader interface {
	ReadTaskValidationFinalWorkspaceSnapshot(context.Context, TaskValidationReviewClaim, task.Task,
		*TaskValidationReplayEvidence) (*TaskValidationFinalWorkspaceSnapshotProof, func(context.Context) error, error)
}

// RuntimeTaskValidationFinalWorkspaceSnapshotReader uses the Host Runtime's
// immutable Task Workspace manifest export for a fresh review-time snapshot.
// The export endpoint also fences Workspace writes while it captures a
// manifest; SetTaskWorkspaceView below releases that fence without advancing
// the Task's existing view.
type RuntimeTaskValidationFinalWorkspaceSnapshotReader struct {
	runtime *RuntimeControl
}

func NewRuntimeTaskValidationFinalWorkspaceSnapshotReader(runtime *RuntimeControl) (*RuntimeTaskValidationFinalWorkspaceSnapshotReader, error) {
	if runtime == nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "Runtime control is required for Task Workspace snapshot reads")
	}
	return &RuntimeTaskValidationFinalWorkspaceSnapshotReader{runtime: runtime}, nil
}

func (r *RuntimeTaskValidationFinalWorkspaceSnapshotReader) ReadTaskValidationFinalWorkspaceSnapshot(ctx context.Context,
	claim TaskValidationReviewClaim, current task.Task, replay *TaskValidationReplayEvidence) (*TaskValidationFinalWorkspaceSnapshotProof, func(context.Context) error, error) {
	if ctx == nil || r == nil || r.runtime == nil || !ValidTaskValidationReviewClaim(claim) || replay == nil ||
		replay.PlanRevision < 1 || current.ID != claim.TaskID || current.OwnerID != claim.OwnerID ||
		current.InitiatorID != claim.InitiatorID || current.ConversationID != claim.ConversationID ||
		current.Status != task.StatusCompleted || current.CurrentRevision == nil || *current.CurrentRevision != claim.TaskRevision ||
		current.DesiredRevision == nil || *current.DesiredRevision != claim.TaskRevision {
		return nil, nil, domain.NewError(domain.CodeInvalidArgument, "Task validation final Workspace snapshot identity is invalid")
	}
	check, ok := passingReplayCheck(replay)
	if !ok {
		return nil, nil, nil
	}
	if !domain.ValidHostID(current.HostID) || !domain.ValidWorkspaceID(current.WorkspaceID) ||
		current.InputRevision == nil || current.WorkspaceCheckpointRevision == nil || len(current.ContextDigest) != 32 ||
		check.TaskID != current.ID || check.HostID != current.HostID || check.WorkspaceID != current.WorkspaceID ||
		check.HostRuntimeGeneration < 1 || check.HostRuntimeInstanceID == "" || check.HostControlVersion < 0 ||
		check.WorkspaceBindingVersion < 1 || check.InputRevision != *current.InputRevision ||
		check.WorkspaceCheckpointRevision != *current.WorkspaceCheckpointRevision ||
		check.InputManifestSHA256 != hex.EncodeToString(current.ContextDigest) ||
		check.WorkspaceViewID == "" || !validSyncDigest(check.InputManifestSHA256) {
		return nil, nil, domain.NewError(domain.CodeWorkspaceConflict, "Task validation check Workspace identity is incomplete")
	}
	expected := AgentHostFence{RuntimeGeneration: check.HostRuntimeGeneration,
		RuntimeInstanceID: check.HostRuntimeInstanceID, ControlVersion: check.HostControlVersion, AIAllowed: true}
	session, err := r.runtime.Connect(ctx, current.HostID, &expected)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to the check Runtime fence for Task validation: %w", err)
	}
	if session.Fence.HostID != current.HostID || session.Fence.RuntimeGeneration != check.HostRuntimeGeneration ||
		session.Fence.RuntimeInstanceID != check.HostRuntimeInstanceID || session.Fence.ControlVersion != check.HostControlVersion {
		session.Close()
		return nil, nil, domain.NewError(domain.CodeWorkspaceConflict, "Task validation check Runtime fence changed")
	}
	var releaseOnce sync.Once
	var releaseErr error
	release := func(releaseCtx context.Context) error {
		releaseOnce.Do(func() {
			defer session.Close()
			if releaseCtx == nil {
				releaseErr = domain.NewError(domain.CodeInvalidArgument, "Task Workspace snapshot release context is required")
				return
			}
			bindingVersion, viewID, err := session.SetTaskWorkspaceView(releaseCtx, current, current.WorkspaceID,
				check.WorkspaceBindingVersion)
			if err != nil {
				releaseErr = err
				return
			}
			if bindingVersion != check.WorkspaceBindingVersion || viewID != check.WorkspaceViewID {
				releaseErr = domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace manifest fence could not be released")
			}
		})
		return releaseErr
	}
	page, readErr := session.TaskWorkspaceManifestPage(ctx, current, check.WorkspaceBindingVersion,
		check.WorkspaceViewID, "", "", 1000)
	if readErr != nil {
		cleanupErr := releaseTaskValidationWorkspaceSnapshotFence(ctx, release)
		return nil, nil, errors.Join(fmt.Errorf("read current Task Workspace manifest for validation: %w", readErr), cleanupErr)
	}
	if page.HostFence.HostID != session.Fence.HostID || page.HostFence.RuntimeGeneration != session.Fence.RuntimeGeneration ||
		page.HostFence.RuntimeInstanceID != session.Fence.RuntimeInstanceID || page.HostFence.ControlVersion != session.Fence.ControlVersion ||
		page.TaskID != current.ID || page.WorkspaceID != current.WorkspaceID ||
		page.BindingVersion != check.WorkspaceBindingVersion || page.InputRevision != *current.InputRevision ||
		page.WorkspaceCheckpointRevision != *current.WorkspaceCheckpointRevision ||
		page.InputManifestSHA256 != check.InputManifestSHA256 || page.ViewID != check.WorkspaceViewID ||
		!validSyncDigest(page.ManifestSHA256) || !validSyncDigest(page.WorkspaceSnapshotSHA256) {
		cleanupErr := releaseTaskValidationWorkspaceSnapshotFence(ctx, release)
		return nil, nil, errors.Join(domain.NewError(domain.CodeWorkspaceConflict, "current Task Workspace manifest identity changed"), cleanupErr)
	}
	return &TaskValidationFinalWorkspaceSnapshotProof{
		Claim: claim, TaskID: current.ID, TaskRevision: claim.TaskRevision,
		HostID: session.Fence.HostID, RuntimeGeneration: session.Fence.RuntimeGeneration,
		RuntimeInstanceID: session.Fence.RuntimeInstanceID, ControlVersion: session.Fence.ControlVersion,
		WorkspaceID: page.WorkspaceID, BindingVersion: page.BindingVersion,
		InputRevision: page.InputRevision, WorkspaceCheckpointRevision: page.WorkspaceCheckpointRevision,
		InputManifestSHA256: page.InputManifestSHA256, ViewID: page.ViewID, SnapshotSHA256: page.WorkspaceSnapshotSHA256,
	}, release, nil
}

func releaseTaskValidationWorkspaceSnapshotFence(ctx context.Context, release func(context.Context) error) error {
	if release == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	return release(cleanupCtx)
}

func passingReplayCheck(replay *TaskValidationReplayEvidence) (TaskValidationReplayCheckEvidence, bool) {
	for _, check := range replay.Checks {
		if check.Status == TaskValidationReplayPass {
			return check, true
		}
	}
	return TaskValidationReplayCheckEvidence{}, false
}
