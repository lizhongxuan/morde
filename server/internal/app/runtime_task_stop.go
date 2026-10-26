package app

import (
	"context"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/runtimeclient"
)

// StopTaskOperation asks the current Host runtime to stop one exact persisted
// task operation. A successful HTTP response is not sufficient: the returned
// operation identity and terminal status must match the journal row.
func (r *RuntimeControl) StopTaskOperation(ctx context.Context, claimed task.Task, operation task.Operation) (task.OperationStatus, string, bool, error) {
	if ctx == nil || !domain.ValidTaskID(claimed.ID) || !domain.ValidHostID(claimed.HostID) ||
		operation.ID == "" || !domain.ValidOperationID(operation.ID) || operation.TaskID != claimed.ID ||
		operation.HostID != claimed.HostID || operation.RuntimeGeneration < 1 || operation.RuntimeInstanceID == "" ||
		operation.ControlVersion < 0 || operation.RuntimeLeaseVersion < 1 ||
		(operation.Status != task.OperationDispatched && operation.Status != task.OperationUnknown) {
		return "", "", false, domain.NewError(domain.CodeInvalidArgument, "task operation stop identity is invalid")
	}
	session, err := r.Connect(ctx, claimed.HostID, nil)
	if err != nil {
		return "", "", false, err
	}
	defer session.Close()
	if session.Fence.HostID != operation.HostID || session.Fence.RuntimeGeneration != operation.RuntimeGeneration ||
		session.Fence.RuntimeInstanceID != operation.RuntimeInstanceID {
		return "", "", false, domain.NewError(domain.CodeInvalidState, "task operation runtime instance changed")
	}
	value, err := session.CancelOperation(ctx, operation.ID)
	if err != nil {
		if isRuntimeOperationNotFound(err) {
			return "", "", true, nil
		}
		return "", "", false, err
	}
	if !taskStopOperationMatchesRuntime(value, operation) {
		return "", "", false, domain.NewError(domain.CodeUpstreamError, "runtime returned a different task operation")
	}
	result := string(value.Result)
	switch value.Status {
	case runtimeclient.OperationSucceeded:
		return task.OperationSucceeded, result, false, nil
	case runtimeclient.OperationFailed:
		return task.OperationFailed, result, false, nil
	case runtimeclient.OperationCancelled:
		return task.OperationCancelled, result, false, nil
	case runtimeclient.OperationAccepted, runtimeclient.OperationRunning, runtimeclient.OperationCancelling, runtimeclient.OperationUnknown:
		return task.OperationUnknown, result, false, nil
	default:
		return "", "", false, domain.NewError(domain.CodeUpstreamError, "runtime returned an invalid operation status")
	}
}

func taskStopOperationMatchesRuntime(value runtimeclient.Operation, stored task.Operation) bool {
	return value.OperationID == stored.ID && value.TaskID == stored.TaskID && value.HostID == stored.HostID &&
		value.RuntimeGeneration == stored.RuntimeGeneration && value.RuntimeInstanceID == stored.RuntimeInstanceID &&
		value.ControlVersion == stored.ControlVersion && value.LeaseVersion == stored.RuntimeLeaseVersion
}
