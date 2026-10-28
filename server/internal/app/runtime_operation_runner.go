package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/runtimeclient"
)

const (
	runtimeOperationPollInterval     = 150 * time.Millisecond
	runtimeOperationReconcileTimeout = 3 * time.Second
)

// RuntimeOperationRunner owns the shared Runtime operation transport state
// machine. Durable callers must persist their operation and lease before
// calling Execute; the runner never allocates replacement IDs or stores state.
type RuntimeOperationRunner struct {
	pollEvery time.Duration
}

func NewRuntimeOperationRunner(pollEvery time.Duration) *RuntimeOperationRunner {
	if pollEvery <= 0 {
		pollEvery = runtimeOperationPollInterval
	}
	return &RuntimeOperationRunner{pollEvery: pollEvery}
}

// RuntimeComputerTools returns a copy of the single computer-tool contract
// shared by Agent and MCP adapters.
func RuntimeComputerTools() []RuntimeComputerToolDefinition {
	result := make([]RuntimeComputerToolDefinition, 0, len(runtimeToolMappings))
	for _, mapping := range runtimeToolMappings {
		if mapping.internal {
			continue
		}
		result = append(result, RuntimeComputerToolDefinition{
			Name: mapping.name, Capability: mapping.capability, Action: mapping.action,
			Scope: mapping.scope, Description: mapping.description,
			Parameters: append(json.RawMessage(nil), mapping.parameters...),
		})
	}
	return result
}

// Execute dispatches one already-persisted operation. It resolves an uncertain
// response by querying the same ID and retries only when the exact runtime
// instance reports that ID as not found.
func (r *RuntimeOperationRunner) Execute(ctx context.Context, session *RuntimeSession, runtimeTaskID, operationID string, leaseVersion int64, capability, action string, arguments []byte, workspaceID string, bindingVersion int64, inputRevision *int64) (runtimeclient.Operation, error) {
	if err := validateRuntimeOperationCall(session, runtimeTaskID, operationID, leaseVersion, capability, action); err != nil {
		return runtimeclient.Operation{}, err
	}
	trimmed := bytes.TrimSpace(arguments)
	if !json.Valid(trimmed) || len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return runtimeclient.Operation{}, domain.NewError(domain.CodeInvalidArgument, "runtime operation arguments are invalid")
	}
	operation, err := session.Execute(ctx, runtimeTaskID, operationID, leaseVersion, capability, action, arguments, workspaceID, bindingVersion, inputRevision)
	if err == nil {
		return operation, nil
	}
	if inputRevision != nil && domain.CodeOf(err) == domain.CodeWorkspaceConflict {
		return runtimeclient.Operation{}, err
	}

	operation, queryErr := r.queryAfterDispatchError(ctx, session, runtimeTaskID, operationID, leaseVersion)
	if queryErr == nil {
		return operation, nil
	}
	if !isRuntimeOperationNotFound(queryErr) || ctx.Err() != nil {
		return runtimeclient.Operation{}, queryErr
	}

	operation, retryErr := session.Execute(ctx, runtimeTaskID, operationID, leaseVersion, capability, action, arguments, workspaceID, bindingVersion, inputRevision)
	if retryErr == nil {
		return operation, nil
	}
	if inputRevision != nil && domain.CodeOf(retryErr) == domain.CodeWorkspaceConflict {
		return runtimeclient.Operation{}, retryErr
	}
	operation, queryErr = r.queryAfterDispatchError(ctx, session, runtimeTaskID, operationID, leaseVersion)
	if queryErr == nil {
		return operation, nil
	}
	return runtimeclient.Operation{}, queryErr
}

// Get reads the original operation without dispatching it. Callers use this
// for recovery and must treat every non-not-found error as unknown.
func (r *RuntimeOperationRunner) Get(ctx context.Context, session *RuntimeSession, runtimeTaskID, operationID string, leaseVersion int64) (runtimeclient.Operation, error) {
	if !validRuntimeOperationSession(session) || !domain.ValidTaskID(runtimeTaskID) || !domain.ValidOperationID(operationID) || leaseVersion < 1 {
		return runtimeclient.Operation{}, domain.NewError(domain.CodeInvalidArgument, "runtime operation query is invalid")
	}
	return session.GetOperation(ctx, runtimeTaskID, operationID, leaseVersion)
}

// Poll waits for a previously returned operation to reach a terminal state.
// A context error leaves the durable caller responsible for recording unknown.
func (r *RuntimeOperationRunner) Poll(ctx context.Context, session *RuntimeSession, runtimeTaskID, operationID string, leaseVersion int64, operation runtimeclient.Operation) (runtimeclient.Operation, error) {
	if !validRuntimeOperationSession(session) || !domain.ValidTaskID(runtimeTaskID) || !domain.ValidOperationID(operationID) || leaseVersion < 1 {
		return runtimeclient.Operation{}, domain.NewError(domain.CodeInvalidArgument, "runtime operation poll is invalid")
	}
	interval := runtimeOperationPollInterval
	if r != nil && r.pollEvery > 0 {
		interval = r.pollEvery
	}
	for operation.Status == runtimeclient.OperationAccepted || operation.Status == runtimeclient.OperationRunning || operation.Status == runtimeclient.OperationCancelling {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return runtimeclient.Operation{}, ctx.Err()
		case <-timer.C:
		}
		var err error
		operation, err = r.Get(ctx, session, runtimeTaskID, operationID, leaseVersion)
		if err != nil {
			return runtimeclient.Operation{}, err
		}
	}
	return operation, nil
}

func validateRuntimeOperationCall(session *RuntimeSession, runtimeTaskID, operationID string, leaseVersion int64, capability, action string) error {
	if !validRuntimeOperationSession(session) || !domain.ValidTaskID(runtimeTaskID) || !domain.ValidOperationID(operationID) || leaseVersion < 1 || strings.TrimSpace(capability) == "" || strings.TrimSpace(action) == "" {
		return domain.NewError(domain.CodeInvalidArgument, "runtime operation request is invalid")
	}
	return nil
}

func validRuntimeOperationSession(session *RuntimeSession) bool {
	return session != nil && session.Mode == runtimeclient.ModeAI && domain.ValidHostID(session.Fence.HostID) &&
		session.Fence.RuntimeGeneration > 0 && session.Fence.RuntimeInstanceID != "" && session.Fence.ControlVersion >= 0
}

func (r *RuntimeOperationRunner) queryAfterDispatchError(ctx context.Context, session *RuntimeSession, runtimeTaskID, operationID string, leaseVersion int64) (runtimeclient.Operation, error) {
	queryCtx := ctx
	var cancel context.CancelFunc
	if ctx.Err() != nil {
		queryCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), runtimeOperationReconcileTimeout)
		defer cancel()
	}
	return r.Get(queryCtx, session, runtimeTaskID, operationID, leaseVersion)
}

func isRuntimeOperationNotFound(err error) bool {
	var runtimeErr *runtimeclient.Error
	return errors.As(err, &runtimeErr) && runtimeErr.Code == runtimeclient.ErrorOperationNotFound
}
