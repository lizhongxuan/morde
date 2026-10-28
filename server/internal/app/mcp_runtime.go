package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/execution"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/runtimeclient"
)

const (
	mcpRuntimeLeaseDuration = 30 * time.Second
	mcpRuntimeLeaseRenewal  = 10 * time.Second
	// shortcut: authorization is polled every second; add shared invalidation when immediate cross-instance cancellation is required.
	mcpRuntimeAuthorizationCheckInterval = time.Second
	mcpRuntimeAuthorizationTimeout       = 2 * time.Second
)

type MCPComputerHostAuthorizer interface {
	GetForComputer(context.Context, identity.Principal, string, identity.Scope) (host.Host, error)
}

type MCPExecutionGateReader interface {
	GetExecutionControl(context.Context, string) (execution.Control, error)
}

type MCPRuntimeToolResult struct {
	Output  json.RawMessage
	Images  []ModelImage
	IsError bool
}

// MCPRuntimeToolExecutor is the single computer-operation path for MCP. It
// shares Agent tool contracts and RuntimeOperationRunner, while keeping MCP
// operation persistence independent from Agent task rows.
type MCPRuntimeToolExecutor struct {
	store      MCPRuntimeOperationStore
	hosts      MCPComputerHostAuthorizer
	authorizer TaskAuthorizer
	gate       MCPExecutionGateReader
	runtime    *RuntimeControl
	operations *RuntimeOperationRunner
}

func (e *MCPRuntimeToolExecutor) Applications(ctx context.Context, principal identity.Principal, hostID string) (ApplicationsStatus, error) {
	if ctx == nil || e.hosts == nil || e.runtime == nil {
		return ApplicationsStatus{}, domain.NewError(domain.CodeInvalidArgument, "Host application status request is invalid")
	}
	value, err := e.hosts.GetForComputer(ctx, principal, hostID, identity.ScopeComputerObserve)
	if err != nil {
		return ApplicationsStatus{}, err
	}
	if value.ID != hostID || value.State != host.StateRunning || value.DeletedAt != nil {
		return ApplicationsStatus{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	session, err := e.runtime.Connect(ctx, hostID, nil)
	if err != nil {
		return ApplicationsStatus{}, err
	}
	defer session.Close()
	applications, err := session.Applications(ctx)
	if err != nil {
		return ApplicationsStatus{}, err
	}
	return ApplicationsStatus{
		HostID: applications.HostID, RuntimeGeneration: applications.RuntimeGeneration,
		RuntimeInstanceID: applications.RuntimeInstanceID, ControlVersion: applications.ControlVersion,
		CheckedAt: applications.CheckedAt, Items: append([]runtimeclient.ApplicationStatusItem(nil), applications.Items...),
	}, nil
}

func NewMCPRuntimeToolExecutor(store MCPRuntimeOperationStore, hosts MCPComputerHostAuthorizer, authorizer TaskAuthorizer, gate MCPExecutionGateReader, runtime *RuntimeControl) (*MCPRuntimeToolExecutor, error) {
	if store == nil || hosts == nil || authorizer == nil || gate == nil || runtime == nil {
		return nil, errors.New("MCP runtime tool dependencies are required")
	}
	return &MCPRuntimeToolExecutor{
		store: store, hosts: hosts, authorizer: authorizer, gate: gate,
		runtime: runtime, operations: NewRuntimeOperationRunner(0),
	}, nil
}

func (e *MCPRuntimeToolExecutor) Execute(ctx context.Context, principal identity.Principal, protocolSessionID, hostID, idempotencyKey, toolName string, arguments json.RawMessage) (MCPRuntimeToolResult, error) {
	mapping, ok := runtimeToolMappingByName(toolName)
	if ctx == nil || !ok || !validMCPRuntimeSessionID(protocolSessionID) || !domain.ValidHostID(hostID) || !validMCPIdempotencyKey(idempotencyKey) ||
		principal.Kind != identity.KindAPIKey || !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		!principal.HasScope(mapping.scope) || !principal.AllowsHost(hostID) {
		return MCPRuntimeToolResult{}, domain.NewError(domain.CodeForbidden, "MCP runtime tool request is invalid or unauthorized")
	}
	hostValue, err := e.authorize(ctx, principal, "", hostID, mapping.scope)
	if err != nil {
		return MCPRuntimeToolResult{}, err
	}
	ownerID := hostValue.OwnerID
	returnAuthorized := func(result MCPRuntimeToolResult, resultErr error) (MCPRuntimeToolResult, error) {
		if resultErr != nil {
			return result, resultErr
		}
		if _, authErr := e.authorize(ctx, principal, ownerID, hostID, mapping.scope); authErr != nil {
			return MCPRuntimeToolResult{}, authErr
		}
		return result, nil
	}
	gate, err := e.gate.GetExecutionControl(ctx, ownerID)
	if err != nil {
		return MCPRuntimeToolResult{}, err
	}
	if gate.OwnerID != ownerID || gate.State != execution.StateEnabled || gate.Epoch < 1 {
		return MCPRuntimeToolResult{}, domain.NewError(domain.CodeInvalidState, "owner execution is stopped or stopping")
	}

	fence, err := e.runtime.ReadTaskFence(ctx, ownerID, hostID)
	if err != nil {
		return MCPRuntimeToolResult{}, err
	}
	if !fence.AIAllowed || fence.RuntimeGeneration < 1 || fence.ControlVersion < 0 {
		return MCPRuntimeToolResult{}, domain.NewError(domain.CodeInvalidState, "Host is not under AI control")
	}
	session, err := e.runtime.Connect(ctx, hostID, &fence)
	if err != nil {
		return MCPRuntimeToolResult{}, err
	}
	defer session.Close()
	if session.Mode != runtimeclient.ModeAI || !session.Supports(mapping.capability) {
		return MCPRuntimeToolResult{}, domain.NewError(domain.CodeUnsupported, "Host runtime does not support this MCP tool")
	}
	fence = agentFenceFromRuntime(session.Fence)

	arguments = bytes.TrimSpace(arguments)
	if !validRuntimeToolArguments(mapping, arguments) {
		return MCPRuntimeToolResult{}, domain.InvalidArgument("arguments", "runtime tool arguments are invalid")
	}
	workspaceID := runtimeToolWorkspaceID(mapping, arguments)
	bindingVersion := int64(0)
	if runtimeclient.WorkspaceScopedCapability(mapping.capability) {
		bindingVersion, err = session.SyncWorkspaceBindings(ctx, workspaceID, 0)
		if err != nil {
			return MCPRuntimeToolResult{}, err
		}
	}
	hash, err := agentOperationHash(toolName, arguments)
	if err != nil {
		return MCPRuntimeToolResult{}, domain.NewError(domain.CodeInvalidArgument, "runtime tool request is invalid")
	}
	runtimeTaskID, err := domain.NewTaskID()
	if err != nil {
		return MCPRuntimeToolResult{}, fmt.Errorf("generate MCP runtime task ID: %w", err)
	}
	operationID, err := domain.NewOperationID()
	if err != nil {
		return MCPRuntimeToolResult{}, fmt.Errorf("generate MCP runtime operation ID: %w", err)
	}
	key := MCPRuntimeOperationKey{
		OwnerID: ownerID, ActorUserID: principal.UserID, APIKeyID: principal.TokenID, SessionID: protocolSessionID,
		HostID: hostID, IdempotencyKey: idempotencyKey,
	}
	operation, replayed, err := e.store.BeginMCPRuntimeOperation(ctx, BeginMCPRuntimeOperation{
		Key: key, NewRuntimeTaskID: runtimeTaskID, NewOperationID: operationID,
		RequestHash: hash[:], Capability: mapping.capability, Action: mapping.action,
		Arguments: append(json.RawMessage(nil), arguments...), WorkspaceID: workspaceID, BindingVersion: bindingVersion, Fence: fence,
		ExpectedExecutionEpoch: gate.Epoch, LeaseDuration: mcpRuntimeLeaseDuration,
	})
	if err != nil {
		return MCPRuntimeToolResult{}, err
	}
	if !mcpOperationRequestMatches(operation, key, toolName, mapping, hash[:], workspaceID, bindingVersion) {
		return MCPRuntimeToolResult{}, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key is already bound to a different MCP operation")
	}
	if runtimeclient.WorkspaceScopedCapability(mapping.capability) {
		if _, err := session.SyncWorkspaceBindings(ctx, operation.WorkspaceID, operation.BindingVersion); err != nil {
			return MCPRuntimeToolResult{}, mcpUnknownOperationError()
		}
	}
	if operation.Fence != fence {
		if !replayed {
			return MCPRuntimeToolResult{}, mcpUnknownOperationError()
		}
		result, resultErr := e.reconcileMCPRuntimeOperationAfterRestart(ctx, session, operation, mapping)
		return returnAuthorized(result, resultErr)
	}
	if operation.Status == task.OperationSucceeded || operation.Status == task.OperationFailed {
		if operation.Status == task.OperationSucceeded && (mapping.name == runtimeToolBrowserScreenshot || mapping.name == runtimeToolDesktopScreenshot) {
			value, getErr := e.operations.Get(ctx, session, operation.RuntimeTaskID, operation.OperationID, operation.LeaseVersion)
			if getErr != nil || !mcpRuntimeResultMatches(value, operation, mapping) || value.Status != runtimeclient.OperationSucceeded {
				return MCPRuntimeToolResult{}, mcpUnknownOperationError()
			}
			result, resultErr := mapMCPRuntimeResult(mapping, value, operation.Arguments)
			return returnAuthorized(result, resultErr)
		}
		result, resultErr := mapMCPRuntimeOperation(mapping, operation)
		return returnAuthorized(result, resultErr)
	}
	if !replayed && operation.Status != task.OperationPrepared {
		return MCPRuntimeToolResult{}, domain.NewError(domain.CodeInvalidState, "new MCP operation is not prepared")
	}
	if operation.Status != task.OperationPrepared && operation.Status != task.OperationDispatched && operation.Status != task.OperationUnknown {
		return MCPRuntimeToolResult{}, mcpUnknownOperationError()
	}

	operationCtx, cancelOperation := context.WithCancel(ctx)
	leaseKeeperDone := make(chan struct{})
	leaseLost := make(chan struct{}, 1)
	go func() {
		defer close(leaseKeeperDone)
		e.keepMCPRuntimeLease(operationCtx, cancelOperation, leaseLost, session, operation, gate.Epoch, principal, mapping.scope)
	}()
	leaseKeeperStopped := false
	stopLeaseKeeper := func() {
		if leaseKeeperStopped {
			return
		}
		cancelOperation()
		<-leaseKeeperDone
		leaseKeeperStopped = true
	}
	defer stopLeaseKeeper()

	operation, err = e.store.RenewMCPRuntimeOperationLease(ctx, key, operation.RuntimeTaskID, operation.Fence, gate.Epoch, operation.LeaseVersion, mcpRuntimeLeaseDuration)
	if err != nil {
		return MCPRuntimeToolResult{}, err
	}
	if err := session.SetTaskLease(ctx, operation.RuntimeTaskID, operation.LeaseVersion, operation.LeaseExpiresAt); err != nil {
		return MCPRuntimeToolResult{}, err
	}
	if operation.Status == task.OperationPrepared {
		operation, err = e.store.MarkMCPRuntimeOperationDispatched(ctx, key, operation.RuntimeTaskID, operation.Fence, operation.LeaseVersion)
		if err != nil {
			return MCPRuntimeToolResult{}, err
		}
	}

	runtimeOperation, err := e.operations.Execute(operationCtx, session, operation.RuntimeTaskID, operation.OperationID,
		operation.LeaseVersion, mapping.capability, mapping.action, operation.Arguments, operation.WorkspaceID, operation.BindingVersion, nil)
	if err == nil {
		runtimeOperation, err = e.operations.Poll(operationCtx, session, operation.RuntimeTaskID, operation.OperationID, operation.LeaseVersion, runtimeOperation)
	}
	stopLeaseKeeper()
	if err != nil || len(leaseLost) != 0 || !mcpRuntimeResultMatches(runtimeOperation, operation, mapping) {
		if ctx.Err() != nil || len(leaseLost) != 0 {
			cancelCtx, cancel := context.WithTimeout(context.Background(), runtimeOperationReconcileTimeout)
			_, _ = session.CancelOperation(cancelCtx, operation.OperationID)
			cancel()
		}
		_, _ = e.store.SettleMCPRuntimeOperation(context.WithoutCancel(ctx), key, operation.RuntimeTaskID, task.OperationUnknown, nil)
		return MCPRuntimeToolResult{}, mcpUnknownOperationError()
	}

	storedStatus := mcpStoreOperationStatus(runtimeOperation.Status)
	if storedStatus != task.OperationSucceeded && storedStatus != task.OperationFailed {
		storedStatus = task.OperationUnknown
	}
	storedResult := mcpRuntimeResultForPersistence(mapping, runtimeOperation)
	settled, settleErr := e.store.SettleMCPRuntimeOperation(context.WithoutCancel(ctx), key, operation.RuntimeTaskID, storedStatus, storedResult)
	if settleErr != nil {
		return MCPRuntimeToolResult{}, settleErr
	}
	if mapping.name == runtimeToolBrowserScreenshot || mapping.name == runtimeToolDesktopScreenshot {
		result, resultErr := mapMCPRuntimeResult(mapping, runtimeOperation, operation.Arguments)
		return returnAuthorized(result, resultErr)
	}
	result, resultErr := mapMCPRuntimeOperation(mapping, settled)
	return returnAuthorized(result, resultErr)
}

func (e *MCPRuntimeToolExecutor) authorize(ctx context.Context, principal identity.Principal, expectedOwnerID, hostID string, scope identity.Scope) (host.Host, error) {
	value, err := e.hosts.GetForComputer(ctx, principal, hostID, scope)
	if err != nil {
		return host.Host{}, err
	}
	if !domain.ValidUserID(value.OwnerID) || expectedOwnerID != "" && value.OwnerID != expectedOwnerID {
		return host.Host{}, domain.NewError(domain.CodeNotFound, "host not found")
	}
	if value.OwnerID == principal.UserID {
		err = e.authorizer.AuthorizeTaskAction(ctx, value.OwnerID, principal.Kind, principal.TokenID, hostID, principal.Scopes, scope)
	} else if authorizer, ok := e.authorizer.(MCPRuntimeActionAuthorizer); ok {
		err = authorizer.AuthorizeMCPRuntimeAction(ctx, value.OwnerID, principal.UserID, principal.TokenID, hostID, principal.Scopes, scope)
	} else {
		err = domain.NewError(domain.CodeForbidden, "MCP runtime authorization is unavailable")
	}
	if err != nil {
		return host.Host{}, err
	}
	return value, nil
}

func (e *MCPRuntimeToolExecutor) reconcileMCPRuntimeOperationAfterRestart(ctx context.Context, session *RuntimeSession, operation MCPRuntimeOperation, mapping runtimeToolMapping) (MCPRuntimeToolResult, error) {
	key := operation.Key
	switch operation.Status {
	case task.OperationSucceeded, task.OperationFailed:
		if operation.Status == task.OperationSucceeded && (mapping.name == runtimeToolBrowserScreenshot || mapping.name == runtimeToolDesktopScreenshot) {
			value, err := e.operations.Get(ctx, session, operation.RuntimeTaskID, operation.OperationID, operation.LeaseVersion)
			if err != nil || !mcpRuntimeResultMatches(value, operation, mapping) || value.Status != runtimeclient.OperationSucceeded {
				return MCPRuntimeToolResult{}, mcpUnknownOperationError()
			}
			return mapMCPRuntimeResult(mapping, value, operation.Arguments)
		}
		return mapMCPRuntimeOperation(mapping, operation)
	case task.OperationCancelled:
		// The durable MCP row already records that cancellation was proven (or
		// the operation was never dispatched). Cancellation does not imply that
		// the requested side effect succeeded, so keep the MCP result unknown.
		return MCPRuntimeToolResult{}, mcpUnknownOperationError()
	case task.OperationPrepared:
		// Dispatch is persisted before Runtime Execute. A prepared row therefore
		// proves that this MCP operation never reached Runtime.
		_, err := e.store.SettleMCPRuntimeOperation(context.WithoutCancel(ctx), key, operation.RuntimeTaskID, task.OperationCancelled, nil)
		if err != nil {
			return MCPRuntimeToolResult{}, mcpUnknownOperationError()
		}
		return MCPRuntimeToolResult{}, mcpUnknownOperationError()
	case task.OperationDispatched, task.OperationUnknown:
		value, err := e.operations.Get(ctx, session, operation.RuntimeTaskID, operation.OperationID, operation.LeaseVersion)
		if err != nil || !mcpRuntimeResultMatches(value, operation, mapping) {
			e.markMCPRuntimeOperationUnknown(ctx, operation)
			return MCPRuntimeToolResult{}, mcpUnknownOperationError()
		}
		switch value.Status {
		case runtimeclient.OperationSucceeded, runtimeclient.OperationFailed:
			status := mcpStoreOperationStatus(value.Status)
			settled, settleErr := e.store.SettleMCPRuntimeOperation(context.WithoutCancel(ctx), key, operation.RuntimeTaskID, status, mcpRuntimeResultForPersistence(mapping, value))
			if settleErr != nil {
				return MCPRuntimeToolResult{}, mcpUnknownOperationError()
			}
			if status == task.OperationSucceeded && (mapping.name == runtimeToolBrowserScreenshot || mapping.name == runtimeToolDesktopScreenshot) {
				return mapMCPRuntimeResult(mapping, value, operation.Arguments)
			}
			return mapMCPRuntimeOperation(mapping, settled)
		case runtimeclient.OperationCancelled:
			proof, ok := mcpRuntimeCancellationReceipt(value, operation, mapping)
			if !ok {
				e.markMCPRuntimeOperationUnknown(ctx, operation)
				return MCPRuntimeToolResult{}, mcpUnknownOperationError()
			}
			if _, settleErr := e.store.SettleMCPRuntimeOperation(context.WithoutCancel(ctx), key, operation.RuntimeTaskID, task.OperationCancelled, proof); settleErr != nil {
				return MCPRuntimeToolResult{}, mcpUnknownOperationError()
			}
			return MCPRuntimeToolResult{}, mcpUnknownOperationError()
		default:
			e.markMCPRuntimeOperationUnknown(ctx, operation)
			return MCPRuntimeToolResult{}, mcpUnknownOperationError()
		}
	default:
		return MCPRuntimeToolResult{}, mcpUnknownOperationError()
	}
}

func (e *MCPRuntimeToolExecutor) markMCPRuntimeOperationUnknown(ctx context.Context, operation MCPRuntimeOperation) {
	if operation.Status != task.OperationDispatched {
		return
	}
	_, _ = e.store.SettleMCPRuntimeOperation(context.WithoutCancel(ctx), operation.Key, operation.RuntimeTaskID, task.OperationUnknown, nil)
}

func mcpRuntimeCancellationReceipt(value runtimeclient.Operation, operation MCPRuntimeOperation, mapping runtimeToolMapping) (json.RawMessage, bool) {
	if value.Status != runtimeclient.OperationCancelled || !mcpRuntimeResultMatches(value, operation, mapping) {
		return nil, false
	}
	proof, err := json.Marshal(struct {
		RuntimeStatus     string `json:"runtime_status"`
		OperationID       string `json:"operation_id"`
		RuntimeGeneration int64  `json:"runtime_generation"`
		RuntimeInstanceID string `json:"runtime_instance_id"`
		ControlVersion    int64  `json:"control_version"`
		LeaseVersion      int64  `json:"lease_version"`
		HostID            string `json:"host_id"`
		RuntimeTaskID     string `json:"runtime_task_id"`
	}{string(value.Status), value.OperationID, value.RuntimeGeneration, value.RuntimeInstanceID, value.ControlVersion, value.LeaseVersion, value.HostID, value.TaskID})
	return proof, err == nil
}

func (e *MCPRuntimeToolExecutor) keepMCPRuntimeLease(ctx context.Context, cancel context.CancelFunc, leaseLost chan<- struct{}, session *RuntimeSession, operation MCPRuntimeOperation, expectedExecutionEpoch int64, principal identity.Principal, scope identity.Scope) {
	authorizationTicker := time.NewTicker(mcpRuntimeAuthorizationCheckInterval)
	defer authorizationTicker.Stop()
	leaseTicker := time.NewTicker(mcpRuntimeLeaseRenewal)
	defer leaseTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-authorizationTicker.C:
			if !e.mcpRuntimeLeaseAuthorized(ctx, cancel, leaseLost, principal, operation, scope) {
				return
			}
			continue
		case <-leaseTicker.C:
		}
		if !e.mcpRuntimeLeaseAuthorized(ctx, cancel, leaseLost, principal, operation, scope) {
			return
		}
		renewed, err := e.store.RenewMCPRuntimeOperationLease(ctx, operation.Key, operation.RuntimeTaskID, operation.Fence, expectedExecutionEpoch, operation.LeaseVersion, mcpRuntimeLeaseDuration)
		if err != nil {
			select {
			case leaseLost <- struct{}{}:
			default:
			}
			cancel()
			return
		}
		if err := session.SetTaskLease(ctx, operation.RuntimeTaskID, operation.LeaseVersion, renewed.LeaseExpiresAt); err != nil {
			select {
			case leaseLost <- struct{}{}:
			default:
			}
			cancel()
			return
		}
	}
}

func (e *MCPRuntimeToolExecutor) mcpRuntimeLeaseAuthorized(ctx context.Context, cancel context.CancelFunc, leaseLost chan<- struct{}, principal identity.Principal, operation MCPRuntimeOperation, scope identity.Scope) bool {
	authorizationCtx, cancelAuthorization := context.WithTimeout(ctx, mcpRuntimeAuthorizationTimeout)
	defer cancelAuthorization()
	if _, err := e.authorize(authorizationCtx, principal, operation.Key.OwnerID, operation.Key.HostID, scope); err == nil {
		return true
	}
	if ctx.Err() != nil {
		return false
	}
	select {
	case leaseLost <- struct{}{}:
	default:
	}
	cancel()
	return false
}

// ReconcileMCPRuntimeCancellations retries durable Owner stop requests from the
// service worker. Runtime cancellation is idempotent by operation ID; an
// unknown response leaves the operation active and the Owner gate stopping.
func (e *MCPRuntimeToolExecutor) ReconcileMCPRuntimeCancellations(ctx context.Context) error {
	if ctx == nil {
		return domain.NewError(domain.CodeInvalidArgument, "cancellation context is required")
	}
	store, ok := e.store.(MCPRuntimeCancellationStore)
	if !ok {
		return domain.NewError(domain.CodeInternal, "MCP cancellation recovery is unavailable")
	}
	operations, err := store.ListMCPRuntimeOperationsPendingCancellation(ctx, 32)
	if err != nil {
		return err
	}
	for _, operation := range operations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !mcpRuntimeOperationActive(operation.Status) || operation.RuntimeTaskID == "" || !domain.ValidOperationID(operation.OperationID) {
			return domain.NewError(domain.CodeInternal, "stored MCP cancellation request is invalid")
		}
		if operation.Status == task.OperationPrepared {
			if _, err := e.store.SettleMCPRuntimeOperation(ctx, operation.Key, operation.RuntimeTaskID, task.OperationCancelled, nil); err != nil {
				return err
			}
			continue
		}
		if err := e.cancelMCPRuntimeOperation(ctx, store, operation); err != nil {
			return err
		}
	}
	return nil
}

func (e *MCPRuntimeToolExecutor) cancelMCPRuntimeOperation(ctx context.Context, store MCPRuntimeCancellationStore, operation MCPRuntimeOperation) error {
	session, err := e.runtime.Connect(ctx, operation.Key.HostID, nil)
	if err != nil {
		return err
	}
	defer session.Close()
	if session.Fence.HostID != operation.Key.HostID {
		return domain.NewError(domain.CodeInvalidState, "MCP operation Host changed during cancellation")
	}
	mapping, ok := runtimeToolMappingForOperation(operation)
	if !ok {
		return domain.NewError(domain.CodeInternal, "stored MCP operation has no matching runtime tool contract")
	}
	fenceChanged := session.Fence.RuntimeGeneration != operation.Fence.RuntimeGeneration ||
		session.Fence.RuntimeInstanceID != operation.Fence.RuntimeInstanceID ||
		session.Fence.ControlVersion != operation.Fence.ControlVersion
	if fenceChanged {
		observed, queryErr := session.GetOperation(ctx, operation.RuntimeTaskID, operation.OperationID, operation.LeaseVersion)
		if queryErr != nil {
			return queryErr
		}
		if !mcpRuntimeResultMatches(observed, operation, mapping) {
			return domain.NewError(domain.CodeUpstreamError, "runtime returned a different MCP operation during cancellation")
		}
	}
	value, err := session.CancelOperation(ctx, operation.OperationID)
	if err != nil {
		if isRuntimeOperationNotFound(err) {
			if fenceChanged {
				return err
			}
			_, settleErr := store.SettleMCPRuntimeOperationNotFound(ctx, operation.Key, operation.RuntimeTaskID)
			if domain.CodeOf(settleErr) == domain.CodeConflict {
				return nil // The operation lease is still live; retry after the persisted expiry.
			}
			return settleErr
		}
		return err
	}
	if !mcpRuntimeResultMatches(value, operation, mapping) {
		return domain.NewError(domain.CodeUpstreamError, "runtime returned a different MCP operation during cancellation")
	}
	var status task.OperationStatus
	switch value.Status {
	case runtimeclient.OperationSucceeded:
		status = task.OperationSucceeded
	case runtimeclient.OperationFailed:
		status = task.OperationFailed
	case runtimeclient.OperationCancelled:
		status = task.OperationCancelled
	case runtimeclient.OperationAccepted, runtimeclient.OperationRunning, runtimeclient.OperationCancelling, runtimeclient.OperationUnknown:
		return nil
	default:
		return domain.NewError(domain.CodeUpstreamError, "runtime returned an invalid MCP operation status")
	}
	result := mcpRuntimeResultForPersistence(mapping, value)
	if status == task.OperationCancelled {
		proof, ok := mcpRuntimeCancellationReceipt(value, operation, mapping)
		if !ok {
			return domain.NewError(domain.CodeUpstreamError, "runtime returned an invalid MCP cancellation receipt")
		}
		result = proof
	}
	_, err = e.store.SettleMCPRuntimeOperation(ctx, operation.Key, operation.RuntimeTaskID, status, result)
	return err
}

func mcpRuntimeResultForPersistence(mapping runtimeToolMapping, operation runtimeclient.Operation) json.RawMessage {
	if mapping.name == runtimeToolBrowserScreenshot || mapping.name == runtimeToolDesktopScreenshot {
		return json.RawMessage(`{"image_result_in_runtime_store":true}`)
	}
	return append(json.RawMessage(nil), operation.Result...)
}

func mcpRuntimeOperationActive(status task.OperationStatus) bool {
	return status == task.OperationPrepared || status == task.OperationDispatched || status == task.OperationUnknown
}

func runtimeToolMappingForOperation(operation MCPRuntimeOperation) (runtimeToolMapping, bool) {
	for _, mapping := range runtimeToolMappings {
		if mapping.capability == operation.Capability && mapping.action == operation.Action {
			return mapping, true
		}
	}
	return runtimeToolMapping{}, false
}

func mcpOperationRequestMatches(operation MCPRuntimeOperation, key MCPRuntimeOperationKey, toolName string, mapping runtimeToolMapping, hash []byte, workspaceID string, bindingVersion int64) bool {
	return normalizedMCPRuntimeOperationKey(operation.Key) == normalizedMCPRuntimeOperationKey(key) && operation.RuntimeTaskID != "" && domain.ValidTaskID(operation.RuntimeTaskID) &&
		domain.ValidOperationID(operation.OperationID) && operation.Capability == mapping.capability && operation.Action == mapping.action &&
		operation.WorkspaceID == workspaceID && operation.BindingVersion == bindingVersion &&
		bytes.Equal(operation.RequestHash, hash) && toolName == mapping.name &&
		operation.Fence.AIAllowed && operation.Fence.RuntimeGeneration > 0 && operation.Fence.RuntimeInstanceID != "" && operation.Fence.ControlVersion >= 0 &&
		operation.LeaseVersion > 0
}

func normalizedMCPRuntimeOperationKey(key MCPRuntimeOperationKey) MCPRuntimeOperationKey {
	if key.ActorUserID == "" {
		key.ActorUserID = key.OwnerID
	}
	return key
}

func mcpRuntimeResultMatches(result runtimeclient.Operation, operation MCPRuntimeOperation, mapping runtimeToolMapping) bool {
	return result.HostID == operation.Key.HostID && result.RuntimeGeneration == operation.Fence.RuntimeGeneration &&
		result.RuntimeInstanceID == operation.Fence.RuntimeInstanceID && result.ControlVersion == operation.Fence.ControlVersion &&
		result.TaskID == operation.RuntimeTaskID && result.OperationID == operation.OperationID && result.LeaseVersion == operation.LeaseVersion &&
		result.Capability == mapping.capability && result.Action == mapping.action &&
		result.WorkspaceID == operation.WorkspaceID && result.BindingVersion == operation.BindingVersion
}

func mcpStoreOperationStatus(status runtimeclient.OperationStatus) task.OperationStatus {
	switch status {
	case runtimeclient.OperationSucceeded:
		return task.OperationSucceeded
	case runtimeclient.OperationFailed:
		return task.OperationFailed
	default:
		return task.OperationUnknown
	}
}

func mapMCPRuntimeOperation(mapping runtimeToolMapping, persisted MCPRuntimeOperation) (MCPRuntimeToolResult, error) {
	var status runtimeclient.OperationStatus
	switch persisted.Status {
	case task.OperationSucceeded:
		status = runtimeclient.OperationSucceeded
	case task.OperationFailed:
		status = runtimeclient.OperationFailed
	default:
		return MCPRuntimeToolResult{}, mcpUnknownOperationError()
	}
	return mapMCPRuntimeResult(mapping, runtimeclient.Operation{
		HostID: persisted.Key.HostID, RuntimeGeneration: persisted.Fence.RuntimeGeneration,
		RuntimeInstanceID: persisted.Fence.RuntimeInstanceID, ControlVersion: persisted.Fence.ControlVersion,
		TaskID: persisted.RuntimeTaskID, OperationID: persisted.OperationID, LeaseVersion: persisted.LeaseVersion,
		Capability: mapping.capability, Action: mapping.action, Status: status,
		Result: append(json.RawMessage(nil), persisted.Result...),
	}, persisted.Arguments)
}

func mapMCPRuntimeResult(mapping runtimeToolMapping, operation runtimeclient.Operation, arguments json.RawMessage) (MCPRuntimeToolResult, error) {
	result, err := mapRuntimeToolResult(mapping, operation, arguments)
	if err != nil {
		return MCPRuntimeToolResult{}, err
	}
	output := json.RawMessage(result.Output)
	if !json.Valid(output) {
		output = json.RawMessage(`{"error":"invalid_runtime_result"}`)
		result.Status = AgentToolFailed
	}
	return MCPRuntimeToolResult{Output: output, Images: result.Images, IsError: result.Status == AgentToolFailed}, nil
}

func mcpUnknownOperationError() error {
	return domain.NewError(domain.CodeHostNotReady, "MCP operation result is unknown; retry only with the same idempotency_key")
}

func validMCPRuntimeSessionID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if !validMCPIdentifierCharacter(character, "-_") {
			return false
		}
	}
	return true
}

func ValidMCPRuntimeSessionID(value string) bool { return validMCPRuntimeSessionID(value) }

func validMCPIdempotencyKey(value string) bool {
	if len(value) < 8 || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if !validMCPIdentifierCharacter(character, "-_.:") {
			return false
		}
	}
	return true
}

func validMCPIdentifierCharacter(character rune, punctuation string) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9' || strings.ContainsRune(punctuation, character)
}

func ValidMCPIdempotencyKey(value string) bool { return validMCPIdempotencyKey(value) }
