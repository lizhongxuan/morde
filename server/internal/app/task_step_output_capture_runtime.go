package app

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/runtimeclient"
)

const (
	taskStepOutputCaptureAuthorizationLookupTimeout = 500 * time.Millisecond
	taskStepOutputCaptureDrainTimeout               = 15 * time.Second
	taskStepOutputCaptureReleaseTimeout             = 2 * time.Second
)

func (e *RuntimeToolExecutor) prepareTaskStepOutputCaptureIdentity(ctx context.Context, session *RuntimeSession,
	current task.Task, request AgentToolRequest, preparation AgentToolPreparation, workspaceViewID string,
	workspaceCheckpointRevision int64) (runtimeclient.SupplementalOutputCaptureIdentity, TaskStepOutputCaptureIdentity, error) {
	return e.taskStepOutputCaptureIdentity(ctx, current, request, preparation.WorkspaceID, preparation.BindingVersion,
		workspaceViewID, workspaceCheckpointRevision)
}

func (e *RuntimeToolExecutor) taskStepOutputCaptureIdentity(ctx context.Context, current task.Task, request AgentToolRequest,
	workspaceID string, bindingVersion int64, workspaceViewID string, workspaceCheckpointRevision int64) (
	runtimeclient.SupplementalOutputCaptureIdentity, TaskStepOutputCaptureIdentity, error) {
	if e == nil || e.store == nil || ctx == nil || current.CurrentRevision == nil ||
		*current.CurrentRevision < 0 || current.Authorization.Version < 1 || current.Lease.Version < 1 ||
		request.StepID == "" || !domain.ValidStepID(request.StepID) || request.OperationID == "" || !domain.ValidOperationID(request.OperationID) ||
		request.Call.CallID == "" || current.WorkspaceCheckpointRevision == nil || workspaceID == "" ||
		request.WorkspaceID != workspaceID || current.WorkspaceID != workspaceID ||
		workspaceViewID == "" || bindingVersion < 1 || workspaceCheckpointRevision < 0 ||
		*current.WorkspaceCheckpointRevision != workspaceCheckpointRevision {
		return runtimeclient.SupplementalOutputCaptureIdentity{}, TaskStepOutputCaptureIdentity{},
			domain.NewError(domain.CodeInvalidState, "supplemental output capture identity is unavailable")
	}
	reader, ok := e.store.(TaskStepOutputCaptureWorkspaceAuthorizationReader)
	if !ok {
		return runtimeclient.SupplementalOutputCaptureIdentity{}, TaskStepOutputCaptureIdentity{},
			domain.NewError(domain.CodeUnsupported, "supplemental output authorization storage is unavailable")
	}
	lookupCtx, cancel := context.WithTimeout(ctx, taskStepOutputCaptureAuthorizationLookupTimeout)
	workspaceAuthorizationVersion, err := reader.GetTaskStepOutputCaptureWorkspaceAuthorizationVersion(
		lookupCtx, current.InitiatorID, current.ID, current.OwnerID)
	cancel()
	if err != nil || workspaceAuthorizationVersion < 0 || current.ProjectID != "" && workspaceAuthorizationVersion < 1 ||
		current.ProjectID == "" && workspaceAuthorizationVersion != 0 {
		if err == nil {
			err = domain.NewError(domain.CodeInvalidState, "supplemental output Workspace authorization version is invalid")
		}
		return runtimeclient.SupplementalOutputCaptureIdentity{}, TaskStepOutputCaptureIdentity{}, err
	}
	appIdentity := TaskStepOutputCaptureIdentity{
		OwnerID: current.OwnerID, TaskID: current.ID, AuthorizationVersion: current.Authorization.Version,
		TaskRevision: *current.CurrentRevision, WorkspaceID: workspaceID,
		WorkspaceAuthorizationVersion: workspaceAuthorizationVersion, WorkspaceBindingVersion: bindingVersion,
		WorkspaceViewID: workspaceViewID, WorkspaceCheckpointRevision: workspaceCheckpointRevision,
		StepID: request.StepID, CallID: request.Call.CallID, OperationID: request.OperationID,
	}
	runtimeIdentity := runtimeclient.SupplementalOutputCaptureIdentity{
		OwnerID: appIdentity.OwnerID, TaskID: appIdentity.TaskID, AuthorizationVersion: appIdentity.AuthorizationVersion,
		TaskRevision: appIdentity.TaskRevision, WorkspaceID: appIdentity.WorkspaceID,
		WorkspaceAuthorizationVersion: appIdentity.WorkspaceAuthorizationVersion,
		WorkspaceBindingVersion:       appIdentity.WorkspaceBindingVersion, WorkspaceViewID: appIdentity.WorkspaceViewID,
		WorkspaceCheckpointRevision: appIdentity.WorkspaceCheckpointRevision, StepID: appIdentity.StepID, CallID: appIdentity.CallID,
	}
	if !taskStepOutputCaptureIdentityValid(appIdentity) || !runtimeclient.ValidSupplementalOutputCaptureIdentity(runtimeIdentity) {
		return runtimeclient.SupplementalOutputCaptureIdentity{}, TaskStepOutputCaptureIdentity{},
			domain.NewError(domain.CodeInvalidState, "supplemental output capture identity is invalid")
	}
	return runtimeIdentity, appIdentity, nil
}

func (e *RuntimeToolExecutor) collectTaskStepOutputCapture(ctx context.Context, session *RuntimeSession, current task.Task,
	request AgentToolRequest, leaseVersion int64, identity runtimeclient.SupplementalOutputCaptureIdentity) (TaskStepOutputCaptureState, []byte, []byte) {
	if ctx == nil || session == nil || leaseVersion < 1 {
		return TaskStepOutputCaptureStateFailed, nil, nil
	}
	drainCtx, cancel := context.WithTimeout(ctx, taskStepOutputCaptureDrainTimeout)
	stdoutState, stdout := drainTaskStepOutputCaptureStream(drainCtx, session, current, request, leaseVersion, identity,
		runtimeclient.SupplementalOutputCaptureStdout)
	stderrState, stderr := TaskStepOutputCaptureStateComplete, []byte{}
	if stdoutState == TaskStepOutputCaptureStateComplete {
		stderrState, stderr = drainTaskStepOutputCaptureStream(drainCtx, session, current, request, leaseVersion, identity,
			runtimeclient.SupplementalOutputCaptureStderr)
	}
	cancel()
	releaseCtx, releaseCancel := context.WithTimeout(context.Background(), taskStepOutputCaptureReleaseTimeout)
	releaseErr := session.ReleaseSupplementalOutputCapture(releaseCtx, current.ID, request.OperationID, leaseVersion, identity)
	releaseCancel()
	if releaseErr != nil {
		clear(stdout)
		clear(stderr)
		return TaskStepOutputCaptureStateFailed, nil, nil
	}
	if stdoutState != TaskStepOutputCaptureStateComplete || stderrState != TaskStepOutputCaptureStateComplete {
		state := combineTaskStepOutputCaptureStates(stdoutState, stderrState)
		clear(stdout)
		clear(stderr)
		return state, nil, nil
	}
	if len(stdout) > TaskStepOutputCaptureMaxBytes || len(stderr) > TaskStepOutputCaptureMaxBytes-len(stdout) ||
		!utf8.Valid(stdout) || !utf8.Valid(stderr) || strings.ContainsRune(string(stdout), '\x00') || strings.ContainsRune(string(stderr), '\x00') {
		clear(stdout)
		clear(stderr)
		return TaskStepOutputCaptureStateSourceTruncated, nil, nil
	}
	return TaskStepOutputCaptureStateComplete, stdout, stderr
}

func (e *RuntimeToolExecutor) captureTaskStepOutputFromTerminalOperation(ctx context.Context, session *RuntimeSession,
	current task.Task, request AgentToolRequest, operation runtimeclient.Operation, fallbackResult []byte, leaseVersion int64) {
	capture := taskStepOutputCaptureExecutionFromContext(ctx)
	if capture == nil {
		return
	}
	var shell taskStepShellOutput
	validShell := len(operation.Result) > 0 && decodeStrictRuntimeResult(operation.Result, &shell) == nil
	if !validShell {
		shell = taskStepShellOutput{}
		validShell = len(fallbackResult) > 0 && decodeStrictRuntimeResult(fallbackResult, &shell) == nil
	}
	if !validShell || shell.TaskID != current.ID || shell.WorkspaceID != request.WorkspaceID ||
		shell.BindingVersion < 1 || shell.WorkspaceViewID == "" || shell.WorkspaceCheckpointRevision == nil ||
		current.WorkspaceCheckpointRevision == nil || *shell.WorkspaceCheckpointRevision != *current.WorkspaceCheckpointRevision {
		capture.setStatus(TaskStepOutputCaptureStateFailed)
		return
	}
	runtimeIdentity, appIdentity, err := e.taskStepOutputCaptureIdentity(ctx, current, request, shell.WorkspaceID,
		shell.BindingVersion, shell.WorkspaceViewID, *shell.WorkspaceCheckpointRevision)
	if err != nil || !capture.setIdentity(appIdentity) {
		capture.setStatus(TaskStepOutputCaptureStateFailed)
		return
	}
	// Only the exact Runtime result is suitable for cross-boundary redaction.
	// A durable Operation.Result is already sanitized and cannot reconstruct a
	// secret split between its prefix and the supplemental spill.
	if len(operation.Result) == 0 || !capture.setLegacyRaw(operation.Result) {
		capture.setStatus(TaskStepOutputCaptureStateFailed)
		return
	}
	if operation.OperationID != "" && (operation.OperationID != request.OperationID || operation.TaskID != current.ID ||
		operation.WorkspaceID != shell.WorkspaceID || operation.BindingVersion != shell.BindingVersion ||
		operation.WorkspaceViewID != shell.WorkspaceViewID ||
		operation.WorkspaceCheckpointRevision != *shell.WorkspaceCheckpointRevision) {
		capture.setStatus(TaskStepOutputCaptureStateFailed)
		return
	}
	if operation.Status != runtimeclient.OperationSucceeded || session == nil ||
		!session.Supports(runtimeclient.SupplementalOutputCaptureCapability) {
		capture.setStatus(TaskStepOutputCaptureStateFailed)
		return
	}
	state, stdout, stderr := e.collectTaskStepOutputCapture(ctx, session, current, request, leaseVersion, runtimeIdentity)
	if state != TaskStepOutputCaptureStateComplete {
		capture.setStatus(state)
		return
	}
	if !capture.setComplete(appIdentity, stdout, stderr) {
		clear(stdout)
		clear(stderr)
		capture.setStatus(TaskStepOutputCaptureStateSourceTruncated)
	}
}

func drainTaskStepOutputCaptureStream(ctx context.Context, session *RuntimeSession, current task.Task,
	request AgentToolRequest, leaseVersion int64, identity runtimeclient.SupplementalOutputCaptureIdentity,
	stream runtimeclient.SupplementalOutputCaptureStream) (TaskStepOutputCaptureState, []byte) {
	data := make([]byte, 0, runtimeclient.SupplementalOutputCapturePageBytes)
	totalBytes := -1
	offset := 0
	for pageIndex := 0; pageIndex < TaskStepOutputCaptureMaxPages; pageIndex++ {
		page, err := session.SupplementalOutputCapturePage(ctx, current.ID, request.OperationID, leaseVersion,
			identity, stream, offset)
		if err != nil {
			clear(data)
			return TaskStepOutputCaptureStateFailed, nil
		}
		switch page.State {
		case runtimeclient.SupplementalOutputCaptureIncomplete:
			clear(data)
			if page.Reason == "capacity_exceeded" {
				return TaskStepOutputCaptureStateCapacityTruncated, nil
			}
			return TaskStepOutputCaptureStateSourceTruncated, nil
		case runtimeclient.SupplementalOutputCaptureFailed:
			clear(data)
			return TaskStepOutputCaptureStateFailed, nil
		case runtimeclient.SupplementalOutputCaptureComplete:
		default:
			clear(data)
			return TaskStepOutputCaptureStateSourceTruncated, nil
		}
		if totalBytes == -1 {
			totalBytes = page.TotalBytes
		} else if page.TotalBytes != totalBytes {
			clear(data)
			return TaskStepOutputCaptureStateSourceTruncated, nil
		}
		if page.Offset != offset || page.NextOffset != offset+len(page.Data) || page.NextOffset > totalBytes ||
			len(page.Data) > TaskStepOutputCapturePageBytes || offset > totalBytes-len(page.Data) ||
			(len(page.Data) == 0 && !page.EOF) || page.EOF != (page.NextOffset == totalBytes) {
			clear(data)
			return TaskStepOutputCaptureStateSourceTruncated, nil
		}
		if len(page.Data) > TaskStepOutputCaptureMaxBytes-len(data) {
			clear(data)
			return TaskStepOutputCaptureStateCapacityTruncated, nil
		}
		data = append(data, page.Data...)
		if page.EOF {
			if len(data) != totalBytes {
				clear(data)
				return TaskStepOutputCaptureStateSourceTruncated, nil
			}
			return TaskStepOutputCaptureStateComplete, data
		}
		offset = page.NextOffset
	}
	clear(data)
	return TaskStepOutputCaptureStateSourceTruncated, nil
}

func combineTaskStepOutputCaptureStates(stdout, stderr TaskStepOutputCaptureState) TaskStepOutputCaptureState {
	if stdout == TaskStepOutputCaptureStateFailed || stderr == TaskStepOutputCaptureStateFailed {
		return TaskStepOutputCaptureStateFailed
	}
	if stdout == TaskStepOutputCaptureStateCapacityTruncated || stderr == TaskStepOutputCaptureStateCapacityTruncated {
		return TaskStepOutputCaptureStateCapacityTruncated
	}
	if stdout == TaskStepOutputCaptureStateSourceTruncated || stderr == TaskStepOutputCaptureStateSourceTruncated {
		return TaskStepOutputCaptureStateSourceTruncated
	}
	return TaskStepOutputCaptureStateFailed
}
