package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

const (
	hubTaskInfoTimeout = 3 * time.Second
	hubTaskCallSlack   = 5 * time.Second
)

func TaskHubFilePutCloudReceiptHash(value TaskHubOperation) ([32]byte, error) {
	if !domain.ValidUserID(value.OwnerID) || !domain.ValidTaskID(value.TaskID) || !hub.ValidCallID(value.Key) ||
		!domain.ValidClientInstallationID(value.TargetInstallationID) || value.Action != hub.ActionFilePut ||
		value.TargetGrantVersion < 1 || value.SourceCheckpointRevision < 0 || len(value.LocalReceiptHash) != 32 ||
		!workspace.ValidSessionID(value.SyncSessionID) || value.SyncRevision < value.SourceCheckpointRevision ||
		value.SyncStage != TaskHubWriteStageCloudSynced || value.Status != task.OperationSucceeded {
		return [32]byte{}, domain.InvalidArgument("file_put_receipt", "cloud-synced Task file.put receipt is invalid")
	}
	request, err := json.Marshal(hub.WorkspaceWriteArgs{
		WorkspaceID: value.WorkspaceID, Path: value.WorkspacePath, SizeBytes: value.WorkspaceSizeBytes, SHA256: value.WorkspaceSHA256,
	})
	if err != nil {
		return [32]byte{}, fmt.Errorf("encode Task file.put request for receipt: %w", err)
	}
	if _, err := hub.ValidateWorkspaceWriteArgs(request); err != nil {
		return [32]byte{}, domain.InvalidArgument("file_put_receipt", "cloud-synced Task file.put receipt is invalid")
	}
	canonical, err := json.Marshal(struct {
		Version                  string `json:"version"`
		OwnerID                  string `json:"owner_id"`
		TaskID                   string `json:"task_id"`
		OperationKey             string `json:"operation_key"`
		TargetInstallationID     string `json:"target_installation_id"`
		WorkspaceID              string `json:"workspace_id"`
		Path                     string `json:"path"`
		SizeBytes                int64  `json:"size_bytes"`
		SHA256                   string `json:"sha256"`
		SourceCheckpointRevision int64  `json:"source_checkpoint_revision"`
		LocalReceiptHash         []byte `json:"local_receipt_hash"`
		SyncSessionID            string `json:"sync_session_id"`
		SyncRevision             int64  `json:"sync_revision"`
		Stage                    string `json:"stage"`
	}{"morde.task-hub.file-put.v1", value.OwnerID, value.TaskID, value.Key, value.TargetInstallationID,
		value.WorkspaceID, value.WorkspacePath, value.WorkspaceSizeBytes, value.WorkspaceSHA256,
		value.SourceCheckpointRevision, value.LocalReceiptHash, value.SyncSessionID, value.SyncRevision, value.SyncStage})
	if err != nil {
		return [32]byte{}, fmt.Errorf("encode Task file.put cloud receipt: %w", err)
	}
	return sha256.Sum256(canonical), nil
}

func TaskHubFilePutCloudSyncedOutput(value TaskHubOperation) (string, error) {
	if _, err := TaskHubFilePutCloudReceiptHash(value); err != nil {
		return "", err
	}
	output, err := json.Marshal(struct {
		OperationID          string `json:"operation_id"`
		TargetInstallationID string `json:"target_installation_id"`
		TargetGrantVersion   int64  `json:"target_grant_version"`
		WorkspaceID          string `json:"workspace_id"`
		Path                 string `json:"path"`
		SizeBytes            int64  `json:"size_bytes"`
		SHA256               string `json:"sha256"`
		Revision             int64  `json:"revision"`
		Stage                string `json:"stage"`
	}{value.Key, value.TargetInstallationID, value.TargetGrantVersion, value.WorkspaceID, value.WorkspacePath,
		value.WorkspaceSizeBytes, value.WorkspaceSHA256, value.SyncRevision, TaskHubWriteStageCloudSynced})
	if err != nil {
		return "", fmt.Errorf("encode Task file.put tool result: %w", err)
	}
	return string(output), nil
}

type TaskHubOperation struct {
	OwnerID                  string
	TaskID                   string
	Key                      string
	ToolName                 string
	TargetInstallationID     string
	Action                   string
	TaskAuthorizationVersion int64
	TargetGrantVersion       int64
	TaskRevision             int64
	LeaseVersion             int64
	RequestHash              []byte
	ResultHash               []byte
	Status                   task.OperationStatus
	WorkspaceID              string
	WorkspacePath            string
	WorkspaceSizeBytes       int64
	WorkspaceSHA256          string
	SourceCheckpointRevision int64
	LocalReceiptHash         []byte
	SyncStage                string
	SyncSessionID            string
	SyncRevision             int64
}

const (
	TaskHubWriteStagePending     = "pending"
	TaskHubWriteStageLocal       = "local_applied"
	TaskHubWriteStageSyncPending = "sync_pending"
	TaskHubWriteStageCloudSynced = "cloud_synced"
)

// TaskHubOperationStore is implemented by the PostgreSQL integration layer.
// Results are hashed so sensitive file contents and screenshots never enter this ledger.
type TaskHubOperationStore interface {
	PrepareTaskHubOperation(context.Context, TaskHubOperation, string) (TaskHubOperation, bool, error)
	MarkTaskHubOperationDispatched(context.Context, TaskHubOperation, string) error
	CompleteTaskHubOperation(context.Context, TaskHubOperation, string, task.OperationStatus, []byte) error
	MarkTaskHubOperationUnknown(context.Context, TaskHubOperation, string) error
	GetTaskHubOperation(context.Context, string, string, string) (TaskHubOperation, error)
}

// TaskHubWorkspaceWriteStore is an optional extension for file.put. Keeping it
// separate lets existing stores for ordinary Hub actions remain compatible.
type TaskHubWorkspaceWriteStore interface {
	RecordTaskHubOperationLocalReceipt(context.Context, TaskHubOperation, string, []byte) error
}

type HubTaskCallResult struct {
	OK      bool
	Body    json.RawMessage
	Error   string
	Message string
	Unknown bool
}

type HubTaskDevice struct {
	ID                 string
	Name               string
	Platform           string
	Online             bool
	Actions            []string
	TargetGrantVersion int64
}

type hubTaskTool struct {
	action string
	scope  identity.Scope
	base   string
	params json.RawMessage
}

var hubTaskTools = []hubTaskTool{
	{action: hub.ActionScreenshot, scope: identity.ScopeComputerObserve, base: "morde_hub_screenshot", params: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	{action: hub.ActionFiles, scope: identity.ScopeComputerObserve, base: "morde_hub_files", params: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","maxLength":1024}},"additionalProperties":false}`)},
	{action: hub.ActionFileGet, scope: identity.ScopeComputerObserve, base: "morde_hub_file_get", params: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":1024},"max_bytes":{"type":"integer","minimum":1,"maximum":32768}},"required":["path","max_bytes"],"additionalProperties":false}`)},
	{action: hub.ActionFileFetch, scope: identity.ScopeComputerObserve, base: "morde_hub_file_fetch", params: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","minLength":1,"maxLength":1024}},"required":["path"],"additionalProperties":false}`)},
	{action: hub.ActionCommand, scope: identity.ScopeComputerOperate, base: "morde_hub_command", params: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","minLength":1,"maxLength":8192},"timeout_ms":{"type":"integer","minimum":1000,"maximum":300000}},"required":["command","timeout_ms"],"additionalProperties":false}`)},
}

func (s *HubService) CallTaskAction(ctx context.Context, ownerID, targetID string, taskContext *hub.TaskContext, action string, args json.RawMessage) HubTaskCallResult {
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidClientInstallationID(targetID) || s == nil {
		return HubTaskCallResult{Error: "invalid_argument", Message: "task Hub call is invalid"}
	}
	if action == hub.ActionInfo {
		if taskContext != nil || hub.ValidateCall(hub.Frame{Type: "call", ID: "task-info", To: targetID, Action: action, Args: args}) != nil {
			return HubTaskCallResult{Error: "invalid_argument", Message: "task Hub info call is invalid"}
		}
	} else if taskContext == nil || hub.ValidateTaskContext(*taskContext) != nil ||
		hub.ValidateTaskActionArgs(action, args) != nil || !validTaskHubRevisionBinding(action, *taskContext, args) {
		return HubTaskCallResult{Error: "invalid_argument", Message: "task Hub action is invalid"}
	}

	installation, err := s.store.GetClientInstallation(ctx, targetID)
	if err != nil || installation.OwnerID != ownerID || installation.RevokedAt != nil || !installation.ExpiresAt.After(time.Now()) {
		return HubTaskCallResult{Error: "device_unavailable", Message: "target installation is unavailable"}
	}
	s.mu.Lock()
	target := s.sessions[ownerID][targetID]
	s.mu.Unlock()
	if target == nil {
		return HubTaskCallResult{Error: "device_offline", Message: "target installation is offline"}
	}
	if _, err := s.authorizeInstallation(ctx, target.principal); err != nil {
		s.terminate(target, 4001, "installation revoked")
		return HubTaskCallResult{Error: "device_unavailable", Message: "target installation is unavailable"}
	}

	callToken, err := domain.NewOperationID()
	if err != nil {
		return HubTaskCallResult{Error: "internal_error", Message: "task Hub call could not be prepared"}
	}
	callID := "hub:" + callToken
	callerKey := "task/" + ownerID + "/" + targetID
	var taskFrom map[string]any
	if taskContext != nil {
		callerKey = "task/" + ownerID + "/" + taskContext.TaskID
		taskFrom = map[string]any{
			"task_id": taskContext.TaskID, "authorization_version": taskContext.AuthorizationVersion,
			"target_grant_version": taskContext.TargetGrantVersion, "operation_id": taskContext.OperationID,
		}
		if taskContext.TaskRevision > 0 {
			taskFrom["task_revision"] = taskContext.TaskRevision
		}
	}
	if !hub.ValidCallID(callID) {
		return HubTaskCallResult{Error: "invalid_argument", Message: "task Hub operation id is invalid"}
	}
	key := pendingKey(ownerID, callID)
	pending := &hubPendingCall{
		key: key, ownerID: ownerID, id: callID, action: action, target: target, callerKey: callerKey,
		taskResult: make(chan HubTaskCallResult, 1), taskContext: taskContext,
	}
	timeout := hubCallTimeout
	if action == hub.ActionCommand {
		var command struct {
			TimeoutMS int `json:"timeout_ms"`
		}
		if json.Unmarshal(args, &command) == nil && command.TimeoutMS > 0 {
			timeout = time.Duration(command.TimeoutMS)*time.Millisecond + hubTaskCallSlack
		}
	}
	s.mu.Lock()
	if s.closed || s.sessions[ownerID][targetID] != target {
		s.mu.Unlock()
		return HubTaskCallResult{Error: "device_offline", Message: "target installation is offline"}
	}
	if _, exists := s.pending[key]; exists {
		s.mu.Unlock()
		return HubTaskCallResult{Error: "duplicate_call", Message: "task operation is already pending"}
	}
	if len(s.pending) >= HubMaxPendingCalls || s.pendingByAccount[ownerID] >= HubMaxPendingCallsPerAccount ||
		s.pendingByCaller[callerKey] >= HubMaxPendingCallsPerDevice {
		s.mu.Unlock()
		return HubTaskCallResult{Error: "too_many_calls", Message: "Hub pending call limit reached"}
	}
	s.pending[key] = pending
	s.pendingByAccount[ownerID]++
	s.pendingByCaller[callerKey]++
	pending.timer = time.AfterFunc(timeout, func() { s.expireCall(pending) })
	s.mu.Unlock()

	from := map[string]any{"id": "task", "name": "Morde Task", "kind": "task"}
	for name, value := range taskFrom {
		from[name] = value
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if !s.send(target, map[string]any{"type": "call", "id": callID, "from": from, "action": action, "args": args}) {
		s.completeTaskPending(pending, HubTaskCallResult{Unknown: true, Error: "unknown_result", Message: "target did not confirm receipt"})
	}
	select {
	case result := <-pending.taskResult:
		return result
	case <-ctx.Done():
		s.cancelTaskPending(pending)
		return HubTaskCallResult{Unknown: true, Error: "unknown_result", Message: "task Hub call was interrupted"}
	}
}

func validTaskHubRevisionBinding(action string, taskContext hub.TaskContext, raw json.RawMessage) bool {
	if action != hub.ActionFileFetch {
		return taskContext.TaskRevision == 0
	}
	var args hub.FileFetchArgs
	return taskContext.TaskRevision > 0 && json.Unmarshal(raw, &args) == nil &&
		args.TaskRevision == taskContext.TaskRevision && args.FetchID == taskContext.OperationID
}

func (s *HubService) ListTaskDevices(ctx context.Context, ownerID, taskID string, authorizationVersion int64) ([]HubTaskDevice, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || authorizationVersion < 1 {
		return nil, domain.InvalidArgument("task_hub_devices", "task Hub device query is invalid")
	}
	installations, err := s.store.ListClientInstallations(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list task Hub installations: %w", err)
	}
	devices := make([]HubTaskDevice, 0, min(len(installations), HubMaxConnectionsPerAccount))
	for _, installation := range installations {
		if installation.OwnerID != ownerID || installation.RevokedAt != nil || !installation.ExpiresAt.After(time.Now()) ||
			!s.InstallationOnline(ownerID, installation.ID) {
			continue
		}
		devices = append(devices, HubTaskDevice{ID: installation.ID, Name: installation.DisplayName, Platform: installation.Platform,
			Online: true})
		if len(devices) == HubMaxConnectionsPerAccount {
			break
		}
	}
	semaphore := make(chan struct{}, 8)
	var wait sync.WaitGroup
	for index := range devices {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-semaphore }()
			infoCtx, cancel := context.WithTimeout(ctx, hubTaskInfoTimeout)
			defer cancel()
			result := s.CallTaskAction(infoCtx, ownerID, devices[index].ID, nil, hub.ActionInfo, json.RawMessage(`{}`))
			if !result.OK {
				return
			}
			var info struct {
				Actions            []string `json:"actions"`
				TargetGrantVersion *int64   `json:"target_grant_version"`
			}
			if json.Unmarshal(result.Body, &info) != nil {
				return
			}
			devices[index].Actions = info.Actions
			if info.TargetGrantVersion != nil {
				devices[index].TargetGrantVersion = *info.TargetGrantVersion
			}
		}()
	}
	wait.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	return devices, nil
}

func (s *HubService) GetTaskDevice(ctx context.Context, ownerID, targetID string) (HubTaskDevice, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) || !domain.ValidClientInstallationID(targetID) {
		return HubTaskDevice{}, domain.InvalidArgument("task_hub_device", "task Hub device query is invalid")
	}
	installation, err := s.store.GetClientInstallation(ctx, targetID)
	if err != nil || installation.OwnerID != ownerID || installation.RevokedAt != nil || !installation.ExpiresAt.After(time.Now()) {
		return HubTaskDevice{}, domain.NewError(domain.CodeNotFound, "Hub Task target installation is unavailable")
	}
	device := HubTaskDevice{ID: installation.ID, Name: installation.DisplayName, Platform: installation.Platform,
		Online: s.InstallationOnline(ownerID, targetID)}
	if !device.Online {
		return device, nil
	}
	infoCtx, cancel := context.WithTimeout(ctx, hubTaskInfoTimeout)
	defer cancel()
	result := s.CallTaskAction(infoCtx, ownerID, targetID, nil, hub.ActionInfo, json.RawMessage(`{}`))
	if !result.OK {
		return HubTaskDevice{}, domain.NewError(domain.CodeNotFound, "Hub Task target installation is unavailable")
	}
	var info struct {
		Actions            []string `json:"actions"`
		TargetGrantVersion *int64   `json:"target_grant_version"`
	}
	if json.Unmarshal(result.Body, &info) != nil {
		return HubTaskDevice{}, domain.NewError(domain.CodeInvalidState, "Hub Task target authorization response is invalid")
	}
	device.Actions = info.Actions
	if info.TargetGrantVersion != nil {
		device.TargetGrantVersion = *info.TargetGrantVersion
	}
	return device, nil
}

func (b *CloudDSHToolBroker) hubTaskDefinitions(ctx context.Context, current task.Task) ([]AgentToolDefinition, error) {
	if b.hub == nil {
		return nil, nil
	}
	operationStore, ok := b.store.(TaskHubOperationStore)
	if !ok || operationStore == nil {
		return nil, nil
	}
	if err := b.authorizeServerTaskScope(ctx, current, identity.ScopeTaskRead); err != nil {
		return nil, nil
	}
	devices, err := b.hub.ListTaskDevices(ctx, current.OwnerID, current.ID, current.Authorization.Version)
	if err != nil {
		return nil, err
	}
	definitions := make([]AgentToolDefinition, 0)
	for _, tool := range hubTaskTools {
		if b.authorizeServerTaskScope(ctx, current, tool.scope) != nil {
			continue
		}
		for _, device := range devices {
			if device.TargetGrantVersion < 1 || !containsAction(device.Actions, tool.action) {
				continue
			}
			name := hubTaskToolName(tool.base, device.ID, device.TargetGrantVersion)
			if name == "" {
				continue
			}
			description := fmt.Sprintf("Use %s on %s (installation %s). The target checks its own permission for each Task action.", tool.action, device.Name, device.ID)
			definitions = append(definitions, AgentToolDefinition{
				ModelTool:     ModelTool{Name: name, Description: description, Parameters: tool.params, Strict: true},
				RequiredScope: tool.scope,
			})
		}
	}
	return definitions, nil
}

func (b *CloudDSHToolBroker) executeHubTaskAction(ctx context.Context, claimed task.Task, call ModelToolCall, recovering bool, checkpointOutput *string) (AgentToolResult, error) {
	if b.hub == nil {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "Task Hub actions are unavailable")
	}
	operationStore, ok := b.store.(TaskHubOperationStore)
	if !ok || operationStore == nil {
		return AgentToolResult{}, domain.NewError(domain.CodeUnsupported, "Task Hub operation storage is unavailable")
	}
	tool, targetID, grantVersion, ok := parseHubTaskToolName(call.Name)
	if !ok {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "Task Hub action is unavailable")
	}
	details, err := b.currentServerTask(ctx, claimed)
	if err != nil {
		return AgentToolResult{}, err
	}
	current := details.Task
	if err := b.authorizeServerTaskScope(ctx, current, tool.scope); err != nil {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "Task authorization does not allow this Hub action")
	}
	args, err := hubTaskActionArguments(call.Arguments)
	if err != nil {
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "Task Hub action arguments are invalid")
	}
	var fileFetchPath string
	if tool.action == hub.ActionFileFetch {
		fileFetchPath, err = hub.ValidateFileFetchToolArgs(args)
	} else {
		err = hub.ValidateTaskActionArgs(tool.action, args)
	}
	if err != nil {
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "Task Hub action arguments are invalid")
	}
	var writeRequest hub.WorkspaceWriteArgs
	if tool.action == hub.ActionFilePut {
		writeRequest, err = hub.ValidateWorkspaceWriteArgs(args)
		if err != nil || current.WorkspaceID != writeRequest.WorkspaceID || current.WorkspaceCheckpointRevision == nil {
			return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "Task file.put must target the current Workspace checkpoint")
		}
		if _, ok := b.store.(TaskHubWorkspaceWriteStore); !ok {
			return AgentToolResult{}, domain.NewError(domain.CodeUnsupported, "Task Workspace write receipt storage is unavailable")
		}
	}
	var taskRevision int64
	if tool.action == hub.ActionFileFetch {
		if current.Status != task.StatusRunning || current.CurrentRevision == nil || current.DesiredRevision == nil ||
			*current.CurrentRevision < 1 || *current.CurrentRevision != *current.DesiredRevision {
			return AgentToolResult{}, domain.NewError(domain.CodeConflict, "file.fetch requires the current running Task revision")
		}
		taskRevision = *current.CurrentRevision
	}
	requestHash, err := hubTaskOperationHash(call.Name, targetID, grantVersion, args)
	if err != nil {
		return AgentToolResult{}, err
	}
	operationKey := agentOperationKey(call.CallID)
	operation := TaskHubOperation{
		OwnerID: current.OwnerID, TaskID: current.ID, Key: operationKey, ToolName: call.Name,
		TargetInstallationID: targetID, Action: tool.action,
		TaskAuthorizationVersion: current.Authorization.Version, TargetGrantVersion: grantVersion,
		TaskRevision: taskRevision, LeaseVersion: current.Lease.Version, RequestHash: requestHash[:], Status: task.OperationPrepared,
	}
	if tool.action == hub.ActionFilePut {
		operation.WorkspaceID, operation.WorkspacePath = writeRequest.WorkspaceID, writeRequest.Path
		operation.WorkspaceSizeBytes, operation.WorkspaceSHA256 = writeRequest.SizeBytes, writeRequest.SHA256
		operation.SourceCheckpointRevision, operation.SyncStage = *current.WorkspaceCheckpointRevision, TaskHubWriteStagePending
	}
	expectedOperation := operation
	stored, lookupErr := operationStore.GetTaskHubOperation(ctx, current.OwnerID, current.ID, operationKey)
	if lookupErr == nil {
		if result, handled, err := b.reconcileHubTaskOperation(ctx, current, call, expectedOperation, stored, recovering, checkpointOutput); err != nil || handled {
			return result, err
		}
	} else if domain.CodeOf(lookupErr) != domain.CodeNotFound {
		return AgentToolResult{}, lookupErr
	} else if recovering && checkpointOutput != nil {
		return b.pauseHubTaskUnknown(ctx, current, call, operationKey, "A checkpointed Hub result has no matching operation ledger entry.")
	}
	devices, err := b.hub.ListTaskDevices(ctx, current.OwnerID, current.ID, current.Authorization.Version)
	if err != nil {
		return AgentToolResult{}, err
	}
	var target *HubTaskDevice
	for index := range devices {
		if devices[index].ID == targetID {
			target = &devices[index]
			break
		}
	}
	if target == nil && tool.action == hub.ActionFileFetch {
		candidate, targetErr := b.hub.GetTaskDevice(ctx, current.OwnerID, targetID)
		if targetErr == nil {
			target = &candidate
		}
	}
	if target == nil || (target.Online && (target.TargetGrantVersion != grantVersion || !containsAction(target.Actions, tool.action))) ||
		(!target.Online && tool.action != hub.ActionFileFetch) {
		return AgentToolResult{Status: AgentToolNeedsUser, WaitingReason: "hub_target_authorization_changed", Output: "The target device or its action authorization changed. Review the target and try again."}, nil
	}
	operation, replayed, err := operationStore.PrepareTaskHubOperation(ctx, operation, current.Lease.Owner)
	if err != nil {
		return AgentToolResult{}, err
	}
	if replayed {
		if result, handled, err := b.reconcileHubTaskOperation(ctx, current, call, expectedOperation, operation, recovering, checkpointOutput); err != nil || handled {
			return result, err
		}
	}
	if err := b.authorizeServerTaskScope(ctx, current, tool.scope); err != nil {
		return b.failHubTaskBeforeDispatch(ctx, current, call, operation, "Task authorization changed before the Hub action was dispatched.")
	}
	if tool.action == hub.ActionFileFetch {
		return b.executeTaskFileFetch(ctx, current, call, operation, target, fileFetchPath)
	}
	if err := operationStore.MarkTaskHubOperationDispatched(ctx, operation, current.Lease.Owner); err != nil {
		return AgentToolResult{}, err
	}
	taskContext := &hub.TaskContext{
		TaskID: current.ID, AuthorizationVersion: current.Authorization.Version,
		TargetGrantVersion: grantVersion, OperationID: operationKey, TaskRevision: taskRevision,
	}
	callCtx := ctx
	cancel := func() {}
	if tool.action == hub.ActionCommand {
		var argsValue struct {
			TimeoutMS int `json:"timeout_ms"`
		}
		if json.Unmarshal(args, &argsValue) == nil && argsValue.TimeoutMS > 0 {
			callCtx, cancel = context.WithTimeout(ctx, time.Duration(argsValue.TimeoutMS)*time.Millisecond+hubTaskCallSlack)
		}
	}
	result := b.hub.CallTaskAction(callCtx, current.OwnerID, targetID, taskContext, tool.action, args)
	cancel()
	if result.Unknown {
		if err := markTaskHubOperationUnknown(ctx, operationStore, operation, current.Lease.Owner); err != nil {
			return AgentToolResult{}, err
		}
		return b.pauseHubTaskUnknown(ctx, current, call, operationKey, "The target did not confirm whether the Hub action completed.")
	}
	if err := b.authorizeServerTaskScope(ctx, current, tool.scope); err != nil {
		if err := markTaskHubOperationUnknown(ctx, operationStore, operation, current.Lease.Owner); err != nil {
			return AgentToolResult{}, err
		}
		return b.pauseHubTaskUnknown(ctx, current, call, operationKey, "Task authorization changed while the Hub action was in flight.")
	}
	if tool.action == hub.ActionFilePut && result.OK {
		return b.recordHubTaskFilePutReceipt(ctx, current, call, operationStore, operation, writeRequest, *taskContext, result)
	}
	output, images, err := hubTaskToolOutput(tool.action, grantVersion, result)
	if err != nil {
		if err := markTaskHubOperationUnknown(ctx, operationStore, operation, current.Lease.Owner); err != nil {
			return AgentToolResult{}, err
		}
		return b.pauseHubTaskUnknown(ctx, current, call, operationKey, "The target result did not match the Hub action contract.")
	}
	operationStatus := task.OperationSucceeded
	agentStatus := AgentToolSucceeded
	if !result.OK {
		operationStatus, agentStatus = task.OperationFailed, AgentToolFailed
	}
	resultHash := sha256.Sum256([]byte(output))
	if err := operationStore.CompleteTaskHubOperation(ctx, operation, current.Lease.Owner, operationStatus, resultHash[:]); err != nil {
		return AgentToolResult{}, err
	}
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepSucceeded,
		"Run Morde Hub action: "+tool.action, "The target returned a bounded result under its current authorization.")
	if operationStatus == task.OperationFailed {
		step.Status, step.Summary = task.StepFailed, "The target refused or failed this action; it was not reported as successful."
	}
	if err != nil {
		return AgentToolResult{}, err
	}
	if err := b.commitProgress(ctx, current, step, current.Usage, task.StatusRunning, "", "task.step", map[string]any{
		"tool": call.Name, "operation_key": operationKey, "operation_status": operationStatus,
		"target_installation_id": targetID, "task_authorization_version": current.Authorization.Version,
		"target_grant_version": grantVersion,
	}); err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: agentStatus, ExternalID: operationKey, Output: output, Images: images}, nil
}

func (b *CloudDSHToolBroker) executeTaskFileFetch(ctx context.Context, current task.Task, call ModelToolCall,
	operation TaskHubOperation, target *HubTaskDevice, relativePath string) (AgentToolResult, error) {
	fetchStore, ok := b.store.(TaskFileFetchStore)
	if !ok || fetchStore == nil {
		return AgentToolResult{}, domain.NewError(domain.CodeUnsupported, "Task file fetch storage is unavailable")
	}
	fetchID, err := domain.NewOperationID()
	if err != nil {
		return AgentToolResult{}, err
	}
	fetch := TaskFileFetch{
		ID: fetchID, OwnerID: current.OwnerID, TaskID: current.ID, OperationKey: operation.Key,
		SourceInstallationID: operation.TargetInstallationID, TargetGrantVersion: operation.TargetGrantVersion,
		TaskAuthorizationVersion: operation.TaskAuthorizationVersion, TaskRevision: operation.TaskRevision,
		RelativePath: relativePath, Status: task.FileFetchQueued,
	}
	fetch, replayedFetch, err := fetchStore.CreateTaskFileFetch(ctx, TaskFileFetchCreate{Operation: operation, Fetch: fetch}, current.Lease.Owner)
	if err != nil {
		return AgentToolResult{}, err
	}
	if !validTaskFileFetchProjection(fetch) {
		return AgentToolResult{}, domain.NewError(domain.CodeInternal, "Task file fetch receipt is invalid")
	}
	if replayedFetch && !target.Online && (fetch.Status == task.FileFetchTransferring || fetch.Status == task.FileFetchVerifying) {
		return b.pauseHubTaskUnknown(ctx, current, call, operation.Key, "The PC is offline while a prior file transfer is still staged; the same fetch will resume when the device is available.")
	}
	var ticket string
	var ticketHash []byte
	var expiresAt *time.Time
	if target.Online {
		randomToken := make([]byte, 32)
		if _, err := rand.Read(randomToken); err != nil {
			return AgentToolResult{}, fmt.Errorf("generate PC file fetch ticket: %w", err)
		}
		ticket = base64.RawURLEncoding.EncodeToString(randomToken)
		hash := sha256.Sum256([]byte(ticket))
		ticketHash = hash[:]
		expires := time.Now().UTC().Add(TaskFileFetchTicketLifetime)
		expiresAt = &expires
	}
	dispatched, err := fetchStore.DispatchTaskFileFetch(ctx, operation, fetch.ID, ticketHash, expiresAt, target.Online, current.Lease.Owner)
	if err != nil {
		return AgentToolResult{}, err
	}
	if !dispatched {
		return b.pauseHubTaskUnknown(ctx, current, call, operation.Key, "A concurrent file fetch attempt already owns this Task operation.")
	}
	if !target.Online {
		waiting, err := fetchStore.GetTaskFileFetch(ctx, current.OwnerID, current.ID, fetch.ID)
		if err != nil || waiting.Status != task.FileFetchWaitingDevice || waiting.WaitingReason != "device_offline" {
			return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "offline Task file fetch was not durably recorded")
		}
		return b.finishTaskFileFetch(ctx, current, call, operation, waiting, task.OperationSucceeded,
			map[string]any{"fetch_id": waiting.ID, "status": waiting.Status})
	}
	if err := b.authorizeServerTaskScope(ctx, current, identity.ScopeComputerObserve); err != nil {
		if err := markTaskHubOperationUnknown(ctx, b.store.(TaskHubOperationStore), operation, current.Lease.Owner); err != nil {
			return AgentToolResult{}, err
		}
		return b.pauseHubTaskUnknown(ctx, current, call, operation.Key, "Task authorization changed before the PC file request was dispatched.")
	}
	args, err := json.Marshal(hub.FileFetchArgs{
		FetchID: fetch.ID, Path: relativePath, TaskRevision: operation.TaskRevision,
		Ticket: ticket, TicketExpiresAt: *expiresAt,
	})
	if err != nil {
		return AgentToolResult{}, fmt.Errorf("encode Task file fetch Hub action: %w", err)
	}
	taskContext := &hub.TaskContext{
		TaskID: current.ID, AuthorizationVersion: current.Authorization.Version, TargetGrantVersion: operation.TargetGrantVersion,
		OperationID: fetch.ID, TaskRevision: operation.TaskRevision,
	}
	result := b.hub.CallTaskAction(ctx, current.OwnerID, operation.TargetInstallationID, taskContext, hub.ActionFileFetch, args)
	if result.Unknown {
		if err := markTaskHubOperationUnknown(ctx, b.store.(TaskHubOperationStore), operation, current.Lease.Owner); err != nil {
			return AgentToolResult{}, err
		}
		return b.pauseHubTaskUnknown(ctx, current, call, operation.Key, "The PC file request outcome is unknown; reconcile its fetch record before retrying.")
	}
	if err := b.authorizeServerTaskScope(ctx, current, identity.ScopeComputerObserve); err != nil {
		if err := markTaskHubOperationUnknown(ctx, b.store.(TaskHubOperationStore), operation, current.Lease.Owner); err != nil {
			return AgentToolResult{}, err
		}
		return b.pauseHubTaskUnknown(ctx, current, call, operation.Key, "Task authorization changed while the PC file request was in flight.")
	}
	if !result.OK {
		if result.Error == "device_offline" {
			waiting, err := fetchStore.MarkTaskFileFetchWaiting(ctx, operation, fetch.ID, "device_offline")
			if err != nil {
				return AgentToolResult{}, err
			}
			return b.finishTaskFileFetch(ctx, current, call, operation, waiting, task.OperationSucceeded,
				map[string]any{"fetch_id": waiting.ID, "status": waiting.Status, "waiting_reason": waiting.WaitingReason})
		}
		failureReason := TaskFileFetchFailureReason(result.Error)
		if err := fetchStore.FailTaskFileFetch(ctx, operation, fetch.ID, failureReason); err != nil {
			return AgentToolResult{}, err
		}
		failed, err := fetchStore.GetTaskFileFetch(ctx, current.OwnerID, current.ID, fetch.ID)
		if err != nil {
			return AgentToolResult{}, err
		}
		return b.finishTaskFileFetch(ctx, current, call, operation, failed, task.OperationFailed,
			map[string]any{"fetch_id": failed.ID, "status": failed.Status, "failure_reason": failureReason})
	}
	ok = true
	if err := hub.ValidateActionResult(hub.ActionFileFetch, hub.Frame{Type: "result", ID: "task-result", OK: &ok, Body: result.Body}); err != nil {
		if err := markTaskHubOperationUnknown(ctx, b.store.(TaskHubOperationStore), operation, current.Lease.Owner); err != nil {
			return AgentToolResult{}, err
		}
		return b.pauseHubTaskUnknown(ctx, current, call, operation.Key, "The PC file receipt did not match the bounded K4 contract.")
	}
	var receipt hub.FileFetchReceipt
	if err := json.Unmarshal(result.Body, &receipt); err != nil || receipt.FetchID != fetch.ID || receipt.TaskRevision != operation.TaskRevision ||
		receipt.TargetGrantVersion != operation.TargetGrantVersion {
		if err := markTaskHubOperationUnknown(ctx, b.store.(TaskHubOperationStore), operation, current.Lease.Owner); err != nil {
			return AgentToolResult{}, err
		}
		return b.pauseHubTaskUnknown(ctx, current, call, operation.Key, "The PC file receipt did not match this Task revision or target authorization.")
	}
	completed, err := fetchStore.GetTaskFileFetch(ctx, current.OwnerID, current.ID, fetch.ID)
	if err != nil || completed.Status != task.FileFetchCompleted || completed.SourceVersion != receipt.SourceVersion ||
		completed.SourceSHA256 != receipt.SourceSHA256 || completed.SizeBytes != receipt.SizeBytes || completed.ContentID != receipt.ContentID ||
		completed.ExpiresAt == nil || !completed.ExpiresAt.Equal(receipt.ExpiresAt) {
		if err := markTaskHubOperationUnknown(ctx, b.store.(TaskHubOperationStore), operation, current.Lease.Owner); err != nil {
			return AgentToolResult{}, err
		}
		return b.pauseHubTaskUnknown(ctx, current, call, operation.Key, "The PC completion receipt is not backed by a committed server file copy.")
	}
	return b.finishTaskFileFetch(ctx, current, call, operation, completed, task.OperationSucceeded,
		map[string]any{"fetch_id": completed.ID, "status": completed.Status, "source_sha256": completed.SourceSHA256,
			"size_bytes": completed.SizeBytes, "content_id": completed.ContentID, "expires_at": completed.ExpiresAt})
}

func (b *CloudDSHToolBroker) finishTaskFileFetch(ctx context.Context, current task.Task, call ModelToolCall,
	operation TaskHubOperation, fetch TaskFileFetch, operationStatus task.OperationStatus, safeOutput any) (AgentToolResult, error) {
	modelContent := taskFileFetchModelContent{Status: fetch.ParseStatus, ParseError: fetch.ParseError}
	if operationStatus == task.OperationSucceeded && fetch.Status == task.FileFetchCompleted {
		parsed, err := b.taskFileFetchContentForModel(ctx, fetch)
		if err != nil {
			return AgentToolResult{}, err
		}
		modelContent = parsed
		if metadata, ok := safeOutput.(map[string]any); ok {
			metadata["parse_status"] = parsed.Status
			metadata["parse_error"] = nullableTaskFileFetchParseError(parsed.ParseError)
		}
	}
	outputBytes, err := json.Marshal(safeOutput)
	if err != nil {
		return AgentToolResult{}, fmt.Errorf("encode safe Task file fetch result: %w", err)
	}
	operationStore := b.store.(TaskHubOperationStore)
	resultHash := sha256.Sum256(outputBytes)
	if err := operationStore.CompleteTaskHubOperation(ctx, operation, current.Lease.Owner, operationStatus, resultHash[:]); err != nil {
		return AgentToolResult{}, err
	}
	details, err := b.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		return AgentToolResult{}, err
	}
	stepStatus := task.StepSucceeded
	summary := "The PC file fetch completed; its transfer and safe content delivery states were recorded."
	agentStatus := AgentToolSucceeded
	if operationStatus == task.OperationFailed {
		stepStatus, agentStatus = task.StepFailed, AgentToolFailed
		summary = "The PC refused or failed the file fetch; the source file was not published."
	} else if fetch.Status == task.FileFetchCompleted && modelContent.Status != task.FileFetchParseExtracted {
		summary = "The PC file transfer completed, but the file was not extracted for model input."
	}
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", stepStatus, "Request PC file fetch", summary)
	if err != nil {
		return AgentToolResult{}, err
	}
	if err := b.commitProgress(ctx, current, step, current.Usage, task.StatusRunning, "", "task.step", map[string]any{
		"tool": call.Name, "operation_key": operation.Key, "operation_status": operationStatus,
		"fetch_id": fetch.ID, "fetch_status": fetch.Status, "target_installation_id": fetch.SourceInstallationID,
		"target_grant_version": fetch.TargetGrantVersion, "task_revision": fetch.TaskRevision,
		"content_state": fetch.ContentState, "parse_status": modelContent.Status,
		"parse_error": nullableTaskFileFetchParseError(modelContent.ParseError),
	}); err != nil {
		return AgentToolResult{}, err
	}
	output := string(outputBytes)
	var images []ModelImage
	if modelContent.Output != "" {
		output, images = modelContent.Output, modelContent.Images
	}
	return AgentToolResult{Status: agentStatus, ExternalID: operation.Key, Output: output, Images: images}, nil
}

func nullableTaskFileFetchParseError(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (b *CloudDSHToolBroker) recordHubTaskFilePutReceipt(ctx context.Context, current task.Task, call ModelToolCall,
	operationStore TaskHubOperationStore, operation TaskHubOperation, request hub.WorkspaceWriteArgs,
	taskContext hub.TaskContext, result HubTaskCallResult) (AgentToolResult, error) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	unknown := func(summary string) (AgentToolResult, error) {
		if err := markTaskHubOperationUnknown(persistCtx, operationStore, operation, current.Lease.Owner); err != nil {
			return AgentToolResult{}, err
		}
		return b.pauseHubTaskUnknown(persistCtx, current, call, operation.Key, summary)
	}
	if result.Unknown || !result.OK || hub.ValidateWorkspaceWriteReceipt(result.Body, request, taskContext) != nil {
		return unknown("The target did not return a matching durable local Workspace write receipt.")
	}
	var receipt hub.WorkspaceWriteReceipt
	if err := json.Unmarshal(result.Body, &receipt); err != nil {
		return unknown("The target did not return a matching durable local Workspace write receipt.")
	}
	canonicalReceipt, err := json.Marshal(receipt)
	if err != nil {
		return unknown("The target local Workspace write receipt could not be recorded.")
	}
	receiptHash := sha256.Sum256(canonicalReceipt)
	writeStore, ok := b.store.(TaskHubWorkspaceWriteStore)
	if !ok {
		return unknown("Task Workspace write receipt storage is unavailable after the target write.")
	}
	if err := writeStore.RecordTaskHubOperationLocalReceipt(persistCtx, operation, current.Lease.Owner, receiptHash[:]); err != nil {
		return unknown("The target write receipt could not be durably recorded.")
	}
	operation.SyncStage = TaskHubWriteStageLocal
	return b.pauseHubTaskWorkspaceSync(persistCtx, current, call, operation)
}

func (b *CloudDSHToolBroker) reconcileHubTaskOperation(ctx context.Context, current task.Task, call ModelToolCall,
	expected, operation TaskHubOperation, recovering bool, checkpointOutput *string) (AgentToolResult, bool, error) {
	if !sameTaskHubOperation(operation, expected) {
		result, err := b.pauseHubTaskUnknown(ctx, current, call, expected.Key, "The stored Hub operation does not match this exact Task action.")
		return result, true, err
	}
	if expected.Action == hub.ActionFileFetch && (operation.Status == task.OperationPrepared ||
		operation.Status == task.OperationDispatched || operation.Status == task.OperationUnknown) {
		fetchStore, ok := b.store.(TaskFileFetchStore)
		if !ok || fetchStore == nil {
			return AgentToolResult{}, true, domain.NewError(domain.CodeUnsupported, "Task file fetch storage is unavailable")
		}
		fetch, fetchErr := fetchStore.GetTaskFileFetchByOperation(ctx, current.OwnerID, current.ID, expected.Key)
		if fetchErr != nil {
			if domain.CodeOf(fetchErr) != domain.CodeNotFound {
				return AgentToolResult{}, true, fetchErr
			}
			if operation.Status == task.OperationPrepared {
				return AgentToolResult{}, false, nil
			}
			result, err := b.pauseHubTaskUnknown(ctx, current, call, expected.Key, "The file.fetch operation has no durable fetch record and will not create a second transfer.")
			return result, true, err
		}
		if !validTaskFileFetchProjection(fetch) || fetch.OperationKey != expected.Key || fetch.TaskRevision != expected.TaskRevision ||
			fetch.TargetGrantVersion != expected.TargetGrantVersion || fetch.SourceInstallationID != expected.TargetInstallationID {
			result, err := b.pauseHubTaskUnknown(ctx, current, call, expected.Key, "The stored file.fetch record does not match this Task action.")
			return result, true, err
		}
		switch fetch.Status {
		case task.FileFetchCompleted:
			result, err := b.finishTaskFileFetch(ctx, current, call, operation, fetch, task.OperationSucceeded, taskFileFetchSafeOutput(fetch))
			return result, true, err
		case task.FileFetchWaitingDevice:
			result, err := b.finishTaskFileFetch(ctx, current, call, operation, fetch, task.OperationSucceeded, taskFileFetchSafeOutput(fetch))
			return result, true, err
		case task.FileFetchQueued, task.FileFetchTransferring, task.FileFetchVerifying:
			if operation.Status == task.OperationDispatched {
				if err := markTaskHubOperationUnknown(ctx, b.store.(TaskHubOperationStore), operation, current.Lease.Owner); err != nil {
					return AgentToolResult{}, true, err
				}
			}
			return AgentToolResult{}, false, nil
		case task.FileFetchFailed, task.FileFetchCancelled, task.FileFetchCancelling:
			result, err := b.pauseHubTaskUnknown(ctx, current, call, expected.Key, "The prior file.fetch did not commit a retrievable copy; inspect its status before starting a new operation.")
			return result, true, err
		default:
			result, err := b.pauseHubTaskUnknown(ctx, current, call, expected.Key, "The file.fetch record has an unsupported recovery state.")
			return result, true, err
		}
	}
	if expected.Action == hub.ActionFilePut {
		switch operation.SyncStage {
		case TaskHubWriteStageLocal, TaskHubWriteStageSyncPending:
			if (operation.Status == task.OperationDispatched || operation.Status == task.OperationUnknown) &&
				len(operation.LocalReceiptHash) == 32 && current.WorkspaceID == operation.WorkspaceID &&
				current.WorkspaceCheckpointRevision != nil && *current.WorkspaceCheckpointRevision == operation.SourceCheckpointRevision {
				result, err := b.pauseHubTaskWorkspaceSync(ctx, current, call, operation)
				return result, true, err
			}
			result, err := b.pauseHubTaskUnknown(ctx, current, call, expected.Key, "The target file write cannot be matched to its current Workspace checkpoint.")
			return result, true, err
		case TaskHubWriteStageCloudSynced:
			resultHash, hashErr := TaskHubFilePutCloudReceiptHash(operation)
			output, outputErr := TaskHubFilePutCloudSyncedOutput(operation)
			if operation.Status != task.OperationSucceeded || hashErr != nil || outputErr != nil ||
				!bytes.Equal(operation.ResultHash, resultHash[:]) || current.WorkspaceID != operation.WorkspaceID ||
				current.WorkspaceCheckpointRevision == nil || *current.WorkspaceCheckpointRevision != operation.SyncRevision ||
				(checkpointOutput != nil && *checkpointOutput != output) {
				result, err := b.pauseHubTaskUnknown(ctx, current, call, expected.Key, "The saved Workspace sync receipt does not match the Task file write.")
				return result, true, err
			}
			return AgentToolResult{Status: AgentToolSucceeded, Output: output}, true, nil
		}
	}
	switch operation.Status {
	case task.OperationSucceeded, task.OperationFailed:
		if checkpointOutput == nil || !bytes.Equal(operation.ResultHash, sha256Bytes([]byte(*checkpointOutput))) || expected.Action == hub.ActionScreenshot {
			result, err := b.pauseHubTaskUnknown(ctx, current, call, expected.Key, "The prior Hub result cannot be safely restored from the Task checkpoint.")
			return result, true, err
		}
		status := AgentToolSucceeded
		if operation.Status == task.OperationFailed {
			status = AgentToolFailed
		}
		return AgentToolResult{Status: status, Output: *checkpointOutput}, true, nil
	case task.OperationDispatched, task.OperationUnknown:
		if operation.Status == task.OperationDispatched {
			if err := markTaskHubOperationUnknown(ctx, b.store.(TaskHubOperationStore), operation, current.Lease.Owner); err != nil {
				return AgentToolResult{}, true, err
			}
		}
		result, err := b.pauseHubTaskUnknown(ctx, current, call, expected.Key, "The prior Hub action has an unknown result and will not be replayed.")
		return result, true, err
	case task.OperationPrepared:
		if recovering && checkpointOutput != nil {
			result, err := b.pauseHubTaskUnknown(ctx, current, call, expected.Key, "A checkpointed Hub result has no dispatched operation record.")
			return result, true, err
		}
		return AgentToolResult{}, false, nil
	default:
		result, err := b.pauseHubTaskUnknown(ctx, current, call, expected.Key, "The prior Hub operation has an invalid state.")
		return result, true, err
	}
}

func taskFileFetchSafeOutput(fetch TaskFileFetch) map[string]any {
	output := map[string]any{"fetch_id": fetch.ID, "status": fetch.Status}
	if fetch.WaitingReason != "" {
		output["waiting_reason"] = fetch.WaitingReason
	}
	if fetch.FailureReason != "" {
		output["failure_reason"] = fetch.FailureReason
	}
	if fetch.Status == task.FileFetchCompleted {
		output["source_sha256"] = fetch.SourceSHA256
		output["size_bytes"] = fetch.SizeBytes
		output["content_id"] = fetch.ContentID
		output["expires_at"] = fetch.ExpiresAt
		output["content_state"] = fetch.ContentState
		output["parse_status"] = fetch.ParseStatus
		output["parse_error"] = nullableTaskFileFetchParseError(fetch.ParseError)
	}
	return output
}

func markTaskHubOperationUnknown(ctx context.Context, store TaskHubOperationStore, operation TaskHubOperation, leaseOwner string) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	return store.MarkTaskHubOperationUnknown(persistCtx, operation, leaseOwner)
}

func (b *CloudDSHToolBroker) authorizeServerTaskScope(ctx context.Context, current task.Task, scope identity.Scope) error {
	authorization := current.Authorization
	if authorization.Version < 1 || authorization.ExpiresAt == nil || !authorization.ExpiresAt.After(time.Now()) {
		return domain.NewError(domain.CodeUnauthenticated, "Task authorization is unavailable")
	}
	principal := identity.Principal{
		UserID: current.OwnerID, Role: identity.RoleUser, Kind: identity.Kind(authorization.Kind),
		TokenID: authorization.TokenID, ClientInstallationID: authorization.ClientInstallationID,
		Scopes: agentScopeSnapshot(authorization.Scopes),
	}
	if !principal.HasScope(scope) {
		return domain.NewError(domain.CodeForbidden, "Task authorization does not allow this Hub action")
	}
	authorizer, ok := b.runtime.authorizer.(interface {
		AuthorizeTaskExecution(context.Context, identity.Principal, string, identity.Scope) (task.Authorization, error)
	})
	if !ok {
		return domain.NewError(domain.CodeForbidden, "Task authorization cannot be revalidated")
	}
	fresh, err := authorizer.AuthorizeTaskExecution(ctx, principal, current.HostID, scope)
	if err != nil {
		return err
	}
	if fresh.Kind != authorization.Kind || fresh.TokenID != authorization.TokenID ||
		fresh.ClientInstallationID != authorization.ClientInstallationID || fresh.Version != authorization.Version ||
		!sameHubAuthorizationScopes(fresh.Scopes, authorization.Scopes) || fresh.ExpiresAt == nil || !fresh.ExpiresAt.Equal(*authorization.ExpiresAt) {
		return domain.NewError(domain.CodeUnauthenticated, "Task authorization changed")
	}
	return nil
}

func sameHubAuthorizationScopes(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	left, right = append([]string(nil), left...), append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func hubTaskToolName(base, targetID string, grantVersion int64) string {
	if !domain.ValidClientInstallationID(targetID) || grantVersion < 1 {
		return ""
	}
	name := base + "_" + targetID + "_v" + strconv.FormatInt(grantVersion, 10)
	if len(name) > 64 {
		return ""
	}
	return name
}

func parseHubTaskToolName(name string) (hubTaskTool, string, int64, bool) {
	for _, tool := range hubTaskTools {
		prefix := tool.base + "_"
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(name, prefix), "_v")
		if len(parts) != 2 || !domain.ValidClientInstallationID(parts[0]) {
			return hubTaskTool{}, "", 0, false
		}
		version, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || version < 1 || hubTaskToolName(tool.base, parts[0], version) != name {
			return hubTaskTool{}, "", 0, false
		}
		return tool, parts[0], version, true
	}
	return hubTaskTool{}, "", 0, false
}

func hubTaskActionArguments(raw string) (json.RawMessage, error) {
	validated, err := validateAgentToolArguments(raw)
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(validated, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("tool arguments must be an object")
	}
	if _, exists := object["target_installation_id"]; exists {
		return nil, errors.New("target installation is fixed by the tool definition")
	}
	return validated, nil
}

func hubTaskOperationHash(toolName, targetID string, grantVersion int64, args json.RawMessage) ([32]byte, error) {
	encoded, err := json.Marshal(struct {
		ToolName     string          `json:"tool_name"`
		TargetID     string          `json:"target_installation_id"`
		GrantVersion int64           `json:"target_grant_version"`
		Arguments    json.RawMessage `json:"arguments"`
	}{toolName, targetID, grantVersion, args})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(encoded), nil
}

func sameTaskHubOperation(actual, expected TaskHubOperation) bool {
	return actual.OwnerID == expected.OwnerID && actual.TaskID == expected.TaskID && actual.Key == expected.Key &&
		actual.ToolName == expected.ToolName && actual.TargetInstallationID == expected.TargetInstallationID &&
		actual.Action == expected.Action && actual.TaskAuthorizationVersion == expected.TaskAuthorizationVersion &&
		actual.TargetGrantVersion == expected.TargetGrantVersion && actual.TaskRevision == expected.TaskRevision &&
		bytes.Equal(actual.RequestHash, expected.RequestHash) && actual.WorkspaceID == expected.WorkspaceID &&
		actual.WorkspacePath == expected.WorkspacePath && actual.WorkspaceSizeBytes == expected.WorkspaceSizeBytes &&
		actual.WorkspaceSHA256 == expected.WorkspaceSHA256 && actual.SourceCheckpointRevision == expected.SourceCheckpointRevision
}

func hubTaskToolOutput(action string, targetGrantVersion int64, result HubTaskCallResult) (string, []ModelImage, error) {
	if result.Unknown {
		return "", nil, errors.New("action result is unknown")
	}
	if !result.OK {
		encoded, err := json.Marshal(map[string]string{"error": result.Error})
		return string(encoded), nil, err
	}
	ok := true
	if err := hub.ValidateActionResult(action, hub.Frame{Type: "result", ID: "task-result", OK: &ok, Body: result.Body}); err != nil {
		return "", nil, err
	}
	var targetGrant struct {
		Version int64 `json:"target_grant_version"`
	}
	if err := json.Unmarshal(result.Body, &targetGrant); err != nil || targetGrant.Version != targetGrantVersion {
		return "", nil, errors.New("target authorization version changed during the action")
	}
	if action == hub.ActionScreenshot {
		var imageResult struct {
			MIMEType   string `json:"mime_type"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			DataBase64 string `json:"data_base64"`
		}
		if json.Unmarshal(result.Body, &imageResult) != nil {
			return "", nil, errors.New("screenshot result is invalid")
		}
		data, err := base64.StdEncoding.DecodeString(imageResult.DataBase64)
		if err != nil {
			return "", nil, err
		}
		output, err := json.Marshal(map[string]any{"mime_type": imageResult.MIMEType, "width": imageResult.Width, "height": imageResult.Height})
		return string(output), []ModelImage{{MediaType: imageResult.MIMEType, Data: data}}, err
	}
	return string(result.Body), nil, nil
}

func containsAction(actions []string, want string) bool {
	for _, action := range actions {
		if action == want {
			return true
		}
	}
	return false
}

func (b *CloudDSHToolBroker) pauseHubTaskUnknown(ctx context.Context, current task.Task, call ModelToolCall, operationKey, summary string) (AgentToolResult, error) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	details, err := b.store.GetTask(persistCtx, current.ID, current.OwnerID)
	if err != nil {
		return AgentToolResult{}, err
	}
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepUnknown,
		"Review Morde Hub action", summary)
	if err != nil {
		return AgentToolResult{}, err
	}
	err = b.commitProgress(persistCtx, current, step, current.Usage, task.StatusWaitingUser, "hub_action_unknown", "task.waiting_user", map[string]any{
		"tool": call.Name, "operation_key": operationKey, "operation_status": task.OperationUnknown,
	})
	return AgentToolResult{Status: AgentToolUnknown}, err
}

func (b *CloudDSHToolBroker) pauseHubTaskWorkspaceSync(ctx context.Context, current task.Task, call ModelToolCall,
	operation TaskHubOperation) (AgentToolResult, error) {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	details, err := b.store.GetTask(persistCtx, current.ID, current.OwnerID)
	if err != nil {
		return AgentToolResult{}, err
	}
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepWaitingUser,
		"Waiting for target Workspace sync", "The target has applied the file; its Workspace sync receipt is still pending.")
	if err != nil {
		return AgentToolResult{}, err
	}
	err = b.commitProgress(persistCtx, current, step, current.Usage, task.StatusWaitingUser, "task_hub_workspace_sync", "task.waiting_user", map[string]any{
		"tool": call.Name, "operation_key": operation.Key, "operation_status": operation.Status,
		"workspace_id": operation.WorkspaceID, "path": operation.WorkspacePath, "sync_stage": operation.SyncStage,
	})
	return AgentToolResult{Status: AgentToolNeedsUser, WaitingReason: "task_hub_workspace_sync"}, err
}

func (b *CloudDSHToolBroker) failHubTaskBeforeDispatch(ctx context.Context, current task.Task, call ModelToolCall, operation TaskHubOperation, message string) (AgentToolResult, error) {
	output, _ := json.Marshal(map[string]string{"error": "not_dispatched", "message": message})
	hash := sha256.Sum256(output)
	store, ok := b.store.(TaskHubOperationStore)
	if !ok {
		return AgentToolResult{}, domain.NewError(domain.CodeUnsupported, "Task Hub operation storage is unavailable")
	}
	if err := store.CompleteTaskHubOperation(ctx, operation, current.Lease.Owner, task.OperationFailed, hash[:]); err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolFailed, Output: string(output)}, nil
}
