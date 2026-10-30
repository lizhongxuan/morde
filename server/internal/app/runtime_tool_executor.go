package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"mime"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
	"morde/server/internal/runtimeclient"
)

const (
	runtimeToolShellExec           = "shell_exec"
	runtimeToolFileRead            = "file_read"
	runtimeToolFileWrite           = "file_write"
	runtimeToolFileList            = "file_list"
	runtimeToolFileFind            = "file_find"
	runtimeToolBrowserOpenTab      = "browser_open_tab"
	runtimeToolBrowserNavigate     = "browser_navigate"
	runtimeToolBrowserSnapshot     = "browser_snapshot"
	runtimeToolBrowserClick        = "browser_click"
	runtimeToolBrowserType         = "browser_type"
	runtimeToolBrowserPressKey     = "browser_press_key"
	runtimeToolBrowserObserveField = "browser_observe_field"
	runtimeToolBrowserScreenshot   = "browser_screenshot"
	runtimeToolDesktopScreenshot   = "desktop_screenshot"
	runtimeToolWindowList          = "window_list"
	runtimeToolWindowActivate      = "window_activate"
	runtimeToolDesktopClick        = "desktop_click"
	runtimeToolDesktopType         = "desktop_type"
	runtimeToolDesktopPressKey     = "desktop_press_key"
	runtimeFileVerifyCapability    = "file.verify"
	runtimeFileVerifyAction        = "verify"

	runtimeToolLeaseRefresh     = time.Second
	maxRuntimeToolResultBytes   = 1 << 20
	maxBrowserSnapshotTextBytes = 8 << 10
	maxDesktopScreenshotBytes   = 512 << 10
	maxDesktopScreenshotWidth   = 1280
	maxDesktopScreenshotHeight  = 720
	maxRuntimeWindows           = 128
)

type runtimeToolMapping struct {
	name        string
	capability  string
	action      string
	scope       identity.Scope
	description string
	parameters  json.RawMessage
	internal    bool
}

type runtimeToolRiskTier string

const (
	runtimeToolRiskSafe      runtimeToolRiskTier = "safe"
	runtimeToolRiskModerate  runtimeToolRiskTier = "moderate"
	runtimeToolRiskSensitive runtimeToolRiskTier = "sensitive"
)

var runtimeToolMappings = []runtimeToolMapping{
	{
		name: runtimeToolShellExec, capability: "shell.exec", action: "run", scope: identity.ScopeComputerOperate,
		description: "Run a bounded shell command in the Host Workspace.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"workspace_id":{"type":"string","pattern":"^w[a-z0-9]{7}$"},"command":{"type":"string","description":"Shell command to run."},"cwd":{"type":"string","description":"Optional absolute working directory within the selected Workspace."},"timeout":{"type":"integer","minimum":1,"maximum":300,"description":"Optional timeout in seconds."}},"required":["workspace_id","command"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolFileRead, capability: "file.read", action: "read", scope: identity.ScopeComputerObserve,
		description: "Read a Workspace file, optionally selecting a line range.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"workspace_id":{"type":"string","pattern":"^w[a-z0-9]{7}$"},"path":{"type":"string"},"start_line":{"type":"integer","minimum":1},"end_line":{"type":"integer","minimum":1}},"required":["workspace_id","path"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolFileWrite, capability: "file.write", action: "write", scope: identity.ScopeComputerOperate,
		description: "Write a file inside the Host Workspace.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"workspace_id":{"type":"string","pattern":"^w[a-z0-9]{7}$"},"path":{"type":"string"},"content":{"type":"string"}},"required":["workspace_id","path","content"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolFileList, capability: "file.list", action: "list", scope: identity.ScopeComputerObserve,
		description: "List files and folders in the Host Workspace.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"workspace_id":{"type":"string","pattern":"^w[a-z0-9]{7}$"},"path":{"type":"string"}},"required":["workspace_id"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolFileFind, capability: "file.find", action: "find", scope: identity.ScopeComputerObserve,
		description: "Find Workspace paths whose names contain a query string.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"workspace_id":{"type":"string","pattern":"^w[a-z0-9]{7}$"},"path":{"type":"string"},"query":{"type":"string","minLength":1}},"required":["workspace_id","query"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolBrowserOpenTab, capability: "browser.actions", action: "open_tab", scope: identity.ScopeComputerOperate,
		description: "Open an AI-owned browser tab at an HTTP or HTTPS URL. Existing user tabs are never controlled.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","minLength":1,"maxLength":2048}},"required":["url"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolBrowserNavigate, capability: "browser.actions", action: "navigate", scope: identity.ScopeComputerOperate,
		description: "Navigate an AI-owned browser tab to an HTTP or HTTPS URL.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"target_id":{"type":"string","minLength":1,"maxLength":128},"url":{"type":"string","minLength":1,"maxLength":2048}},"required":["target_id","url"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolBrowserSnapshot, capability: "browser.observe", action: "snapshot", scope: identity.ScopeComputerObserve,
		description: "Read bounded text from an AI-owned browser tab.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"target_id":{"type":"string","minLength":1,"maxLength":128}},"required":["target_id"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolBrowserObserveField, capability: "browser.observe", action: "field", scope: identity.ScopeComputerObserve,
		parameters: json.RawMessage(`{"type":"object","properties":{"url":{"type":"string","minLength":1,"maxLength":2048},"selector":{"type":"string","minLength":1,"maxLength":1024},"field":{"type":"string","enum":["text","value"]}},"required":["url","selector","field"],"additionalProperties":false}`),
		internal:   true,
	},
	{
		name: runtimeToolBrowserClick, capability: "browser.actions", action: "click", scope: identity.ScopeComputerOperate,
		description: "Click the unique visible accessible element matching its role and exact name in an AI-owned browser tab.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"target_id":{"type":"string","minLength":1,"maxLength":128},"role":{"type":"string","enum":["button","link","textbox","searchbox","checkbox","radio","option","menuitem","tab","combobox","switch"]},"name":{"type":"string","minLength":1,"maxLength":512}},"required":["target_id","role","name"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolBrowserType, capability: "browser.actions", action: "type", scope: identity.ScopeComputerOperate,
		description: "Type bounded text into the unique visible accessible element matching its role and exact name in an AI-owned browser tab.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"target_id":{"type":"string","minLength":1,"maxLength":128},"role":{"type":"string","enum":["button","link","textbox","searchbox","checkbox","radio","option","menuitem","tab","combobox","switch"]},"name":{"type":"string","minLength":1,"maxLength":512},"text":{"type":"string","maxLength":4096}},"required":["target_id","role","name","text"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolBrowserPressKey, capability: "browser.actions", action: "press_key", scope: identity.ScopeComputerOperate,
		description: "Press one allowlisted navigation key in an AI-owned browser tab.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"target_id":{"type":"string","minLength":1,"maxLength":128},"key":{"type":"string","enum":["Enter","Tab","Escape","ArrowUp","ArrowDown","ArrowLeft","ArrowRight"]}},"required":["target_id","key"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolBrowserScreenshot, capability: "browser.observe", action: "screenshot", scope: identity.ScopeComputerObserve,
		description: "Capture a bounded JPEG screenshot from an AI-owned browser tab.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"target_id":{"type":"string","minLength":1,"maxLength":128}},"required":["target_id"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolDesktopScreenshot, capability: "desktop.observe", action: "screenshot", scope: identity.ScopeComputerObserve,
		description: "Capture a bounded screenshot of the Host desktop.",
		parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	},
	{
		name: runtimeToolWindowList, capability: "window.list", action: "list", scope: identity.ScopeComputerObserve,
		description: "List visible windows on the Host desktop.",
		parameters:  json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	},
	{
		name: runtimeToolWindowActivate, capability: "window.activate", action: "activate", scope: identity.ScopeComputerOperate,
		description: "Activate a currently visible Host desktop window by its window ID.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"window_id":{"type":"string","pattern":"^0x[1-9a-f][0-9a-f]*$","maxLength":10}},"required":["window_id"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolDesktopClick, capability: "desktop.input", action: "click", scope: identity.ScopeComputerOperate,
		description: "Click a point on the Host desktop.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"window_id":{"type":"string","pattern":"^0x[1-9a-f][0-9a-f]*$","maxLength":10},"x":{"type":"integer","minimum":0,"maximum":65535},"y":{"type":"integer","minimum":0,"maximum":65535},"button":{"type":"string","enum":["left","middle","right"]}},"required":["window_id","x","y"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolDesktopType, capability: "desktop.input", action: "type", scope: identity.ScopeComputerOperate,
		description: "Type up to 4096 UTF-8 bytes on the Host desktop.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"window_id":{"type":"string","pattern":"^0x[1-9a-f][0-9a-f]*$","maxLength":10},"text":{"type":"string","maxLength":4096}},"required":["window_id","text"],"additionalProperties":false}`),
	},
	{
		name: runtimeToolDesktopPressKey, capability: "desktop.input", action: "press_key", scope: identity.ScopeComputerOperate,
		description: "Press one allowlisted key on the Host desktop.",
		parameters:  json.RawMessage(`{"type":"object","properties":{"window_id":{"type":"string","pattern":"^0x[1-9a-f][0-9a-f]*$","maxLength":10},"key":{"type":"string","enum":["Enter","Tab","Escape","BackSpace","Delete","Home","End","Page_Up","Page_Down","ArrowUp","ArrowDown","ArrowLeft","ArrowRight"]}},"required":["window_id","key"],"additionalProperties":false}`),
	},
}

// RuntimeToolExecutor exposes only operations reported by the current,
// TLS-pinned Morde runtime session. It deliberately has no AIO credential or
// direct AIO adapter.
type RuntimeToolExecutor struct {
	store      TaskStore
	authorizer TaskAuthorizer
	fences     AgentHostFenceReader
	runtime    *RuntimeControl
	workspaces *WorkspaceSyncService
	operations *RuntimeOperationRunner
	leaseEvery time.Duration
}

func NewRuntimeToolExecutor(store TaskStore, authorizer TaskAuthorizer, fences AgentHostFenceReader, runtime *RuntimeControl, workspaces *WorkspaceSyncService) (*RuntimeToolExecutor, error) {
	if store == nil || authorizer == nil || fences == nil || runtime == nil || workspaces == nil {
		return nil, errors.New("runtime tool executor dependencies are required")
	}
	return &RuntimeToolExecutor{
		store: store, authorizer: authorizer, fences: fences, runtime: runtime, workspaces: workspaces,
		operations: NewRuntimeOperationRunner(0), leaseEvery: runtimeToolLeaseRefresh,
	}, nil
}

func (e *RuntimeToolExecutor) Definitions(ctx context.Context, claimed task.Task) ([]AgentToolDefinition, error) {
	current, err := e.currentTask(ctx, claimed)
	if err != nil {
		return nil, err
	}
	if err := e.authorize(ctx, current, identity.ScopeComputerObserve); err != nil {
		return nil, err
	}
	fence, err := e.readFence(ctx, current, nil)
	if err != nil {
		return nil, err
	}
	session, err := e.connect(ctx, current, fence)
	if err != nil {
		return nil, err
	}
	defer session.Close()

	canOperate := e.authorize(ctx, current, identity.ScopeComputerOperate) == nil
	definitions := make([]AgentToolDefinition, 0, len(runtimeToolMappings))
	for _, mapping := range runtimeToolMappings {
		if mapping.internal || !session.Supports(mapping.capability) || (mapping.scope == identity.ScopeComputerOperate && !canOperate) {
			continue
		}
		if runtimeclient.WorkspaceScopedCapability(mapping.capability) && !session.Supports("workspace.task_view") {
			continue
		}
		definitions = append(definitions, AgentToolDefinition{
			ModelTool: ModelTool{
				Name: mapping.name, Description: mapping.description,
				Parameters: append(json.RawMessage(nil), mapping.parameters...),
			},
			RequiredScope: mapping.scope,
		})
	}
	return definitions, nil
}

func (e *RuntimeToolExecutor) Prepare(ctx context.Context, request AgentToolRequest) (AgentToolPreparation, error) {
	mapping, ok := runtimeToolMappingByName(request.Call.Name)
	if !ok || mapping.scope != request.RequiredScope || !validRuntimeToolArguments(mapping, request.Arguments) {
		return AgentToolPreparation{}, domain.NewError(domain.CodeInvalidArgument, "runtime tool request is invalid")
	}
	current, err := e.currentTask(ctx, request.Task)
	if err != nil {
		return AgentToolPreparation{}, err
	}
	if err := checkRuntimeToolBudget(current); err != nil {
		return AgentToolPreparation{}, err
	}
	if err := e.authorize(ctx, current, mapping.scope); err != nil {
		return AgentToolPreparation{}, err
	}
	fence, err := e.readFence(ctx, current, &request.Fence)
	if err != nil {
		return AgentToolPreparation{}, err
	}
	session, err := e.connect(ctx, current, fence)
	if err != nil {
		return AgentToolPreparation{}, err
	}
	defer session.Close()
	if !session.Supports(mapping.capability) {
		return AgentToolPreparation{}, domain.NewError(domain.CodeUnsupported, "Host runtime does not support this tool")
	}
	workspaceID, err := runtimeToolOperationWorkspaceID(mapping, request.Arguments, current)
	if err != nil {
		return AgentToolPreparation{}, err
	}
	if request.WorkspaceID != "" && request.WorkspaceID != workspaceID {
		return AgentToolPreparation{}, domain.NewError(domain.CodeInvalidArgument, "runtime Workspace binding is invalid")
	}
	bindingVersion := int64(0)
	request.WorkspaceID = workspaceID
	if workspaceID != "" {
		if runtimeclient.WorkspaceScopedCapability(mapping.capability) {
			bindingVersion, _, err = session.SetTaskWorkspaceView(ctx, current, workspaceID, 0)
		} else {
			bindingVersion, err = session.SyncTaskWorkspaceBindings(ctx, current, workspaceID, 0)
		}
		if err != nil {
			return AgentToolPreparation{}, err
		}
	}
	request.WorkspaceID = workspaceID
	fence = agentFenceFromRuntime(session.Fence)
	if runtimeToolRisk(mapping.name, request.Arguments) == runtimeToolRiskSensitive {
		approved, approvalErr := e.hasCurrentRuntimeToolApproval(ctx, current, request, fence)
		if approvalErr != nil {
			return AgentToolPreparation{}, approvalErr
		}
		if !approved {
			return AgentToolPreparation{
				Status: AgentToolApprovalRequired, Approval: runtimeToolApprovalRequest(mapping),
				Fence: fence, WorkspaceID: workspaceID, BindingVersion: bindingVersion,
			}, nil
		}
	}
	return AgentToolPreparation{
		Status: AgentToolReady, Fence: fence, WorkspaceID: workspaceID, BindingVersion: bindingVersion,
		Token: runtimeToolPreparationToken(request, fence, bindingVersion),
	}, nil
}

func (e *RuntimeToolExecutor) hasCurrentRuntimeToolApproval(ctx context.Context, current task.Task, request AgentToolRequest, fence AgentHostFence) (bool, error) {
	decision := request.Approval
	if decision == nil {
		return false, nil
	}
	hash, err := agentOperationHash(request.Call.Name, request.Arguments)
	if err != nil {
		return false, domain.NewError(domain.CodeInvalidArgument, "runtime tool approval request is invalid")
	}
	details, err := e.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		return false, err
	}
	pending := details.PendingRequest
	if pending == nil || pending.TaskID != current.ID || pending.Kind != "approval" || pending.ID != decision.PendingID ||
		pending.Version != decision.Version || pending.CallID != request.Call.CallID || pending.ToolName != request.Call.Name ||
		!bytes.Equal(pending.RequestHash, hash[:]) || !bytes.Equal(decision.RequestHash, hash[:]) {
		return false, domain.NewError(domain.CodeForbidden, "Host action approval does not match this exact tool call")
	}
	if pending.Status == task.PendingRequestRejected || decision.Decision != task.PendingDecisionApprove {
		return false, domain.NewError(domain.CodeForbidden, "Host action approval was rejected")
	}
	if pending.Status != task.PendingRequestApproved || pending.Decision != task.PendingDecisionApprove ||
		!samePendingApprovalFence(pending, fence) {
		return false, nil
	}
	return true, nil
}

func runtimeToolRisk(name string, arguments json.RawMessage) runtimeToolRiskTier {
	switch name {
	case runtimeToolFileRead, runtimeToolFileList, runtimeToolFileFind, runtimeToolBrowserSnapshot,
		runtimeToolBrowserScreenshot, runtimeToolDesktopScreenshot, runtimeToolWindowList, runtimeToolBrowserObserveField:
		return runtimeToolRiskSafe
	case runtimeToolFileWrite, runtimeToolBrowserOpenTab, runtimeToolBrowserNavigate, runtimeToolBrowserClick,
		runtimeToolBrowserPressKey, runtimeToolWindowActivate:
		return runtimeToolRiskModerate
	case runtimeToolDesktopPressKey:
		var input struct {
			Key string `json:"key"`
		}
		if json.Unmarshal(arguments, &input) == nil && input.Key != "Enter" && input.Key != "BackSpace" && input.Key != "Delete" {
			return runtimeToolRiskModerate
		}
		return runtimeToolRiskSensitive
	default:
		return runtimeToolRiskSensitive
	}
}

func runtimeToolApprovalRequest(mapping runtimeToolMapping) AgentApprovalRequest {
	switch mapping.name {
	case runtimeToolShellExec:
		return AgentApprovalRequest{Object: "Host Workspace command", Content: "Run a command in the Host Workspace?", Impact: "The command can change files or send data from the authorized Workspace."}
	case runtimeToolBrowserType:
		return AgentApprovalRequest{Object: "Browser input", Content: "Type text into the selected browser field?", Impact: "The website may receive and store the text."}
	case runtimeToolDesktopClick:
		return AgentApprovalRequest{Object: "Host desktop input", Content: "Click a point on the Host desktop?", Impact: "The current window target cannot be confirmed for this action."}
	case runtimeToolDesktopType:
		return AgentApprovalRequest{Object: "Host desktop input", Content: "Type text into the current Host desktop window?", Impact: "The text may change app data or be submitted externally."}
	case runtimeToolDesktopPressKey:
		return AgentApprovalRequest{Object: "Host desktop input", Content: "Press a potentially submitting or destructive key on the Host desktop?", Impact: "The current window target cannot be confirmed for this action."}
	default:
		return AgentApprovalRequest{Object: "Host tool action", Content: "Run this sensitive Host action?", Impact: "The action may have effects that cannot be assessed from the available tool context."}
	}
}

func (e *RuntimeToolExecutor) Execute(ctx context.Context, request AgentToolRequest, preparation AgentToolPreparation) (AgentToolResult, error) {
	mapping, ok := runtimeToolMappingByName(request.Call.Name)
	if !ok || mapping.scope != request.RequiredScope || !validRuntimeToolArguments(mapping, request.Arguments) ||
		preparation.Status != AgentToolReady || preparation.Fence != request.Fence || preparation.Token == "" ||
		!domain.ValidOperationID(request.OperationID) {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidArgument, "runtime tool preparation is invalid")
	}
	current, err := e.currentTask(ctx, request.Task)
	if err != nil {
		return AgentToolResult{}, err
	}
	workspaceID, err := runtimeToolOperationWorkspaceID(mapping, request.Arguments, current)
	if err != nil || preparation.WorkspaceID != workspaceID || request.WorkspaceID != workspaceID ||
		(workspaceID != "" && preparation.BindingVersion < 1) || (workspaceID == "" && preparation.BindingVersion != 0) ||
		preparation.Token != runtimeToolPreparationToken(request, preparation.Fence, preparation.BindingVersion) {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidArgument, "runtime tool preparation is invalid")
	}
	if err := e.authorize(ctx, current, mapping.scope); err != nil {
		return AgentToolResult{}, err
	}
	fence, err := e.readFence(ctx, current, &request.Fence)
	if err != nil {
		return AgentToolResult{}, err
	}
	session, err := e.connect(ctx, current, fence)
	if err != nil {
		return AgentToolResult{}, err
	}
	defer session.Close()
	if !session.Supports(mapping.capability) {
		return AgentToolResult{}, domain.NewError(domain.CodeUnsupported, "Host runtime does not support this tool")
	}
	if runtimeToolRisk(mapping.name, request.Arguments) == runtimeToolRiskSensitive {
		approved, approvalErr := e.hasCurrentRuntimeToolApproval(ctx, current, request, fence)
		if approvalErr != nil {
			return AgentToolResult{}, approvalErr
		}
		if !approved {
			return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "sensitive Host action has no current approval")
		}
	}
	workspaceViewID := ""
	workspaceCheckpointRevision := int64(0)
	if preparation.WorkspaceID != "" {
		if runtimeclient.WorkspaceScopedCapability(mapping.capability) {
			_, workspaceViewID, err = session.SetTaskWorkspaceView(ctx, current, preparation.WorkspaceID, preparation.BindingVersion)
			if err != nil {
				if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
					return workspaceInputChangedResult(), nil
				}
				return AgentToolResult{Status: AgentToolFailed}, err
			}
			workspaceCheckpointRevision = *current.WorkspaceCheckpointRevision
		} else if _, err = session.SyncTaskWorkspaceBindings(ctx, current, preparation.WorkspaceID, preparation.BindingVersion); err != nil {
			if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
				return workspaceInputChangedResult(), nil
			}
			return AgentToolResult{Status: AgentToolFailed}, err
		}
	}
	if err := e.validatePersistedOperation(ctx, current, request, fence, preparation); err != nil {
		return AgentToolResult{}, err
	}
	if current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "task lease is no longer active")
	}
	runtimeArguments, err := runtimeToolExecutionArguments(mapping, request.Arguments)
	if err != nil {
		return AgentToolResult{}, err
	}

	// The runtime lease mirrors the task's durable worker lease. The operation
	// is sent only after the current Host fence, authorization, and persisted
	// operation row have all been checked again.
	if err := session.SetTaskLease(ctx, current.ID, current.Lease.Version, *current.Lease.ExpiresAt); err != nil {
		return AgentToolResult{}, err
	}
	var inputRevision *int64
	runtimeWorkspaceID := ""
	runtimeBindingVersion := int64(0)
	if runtimeclient.WorkspaceScopedCapability(mapping.capability) {
		inputRevision = current.InputRevision
		runtimeWorkspaceID = preparation.WorkspaceID
		runtimeBindingVersion = preparation.BindingVersion
	}
	operationCtx, cancelOperation := context.WithCancel(ctx)
	leaseKeeperDone := make(chan struct{})
	go func() {
		defer close(leaseKeeperDone)
		e.keepRuntimeTaskLease(operationCtx, cancelOperation, session, current, request.OperationID)
	}()
	defer func() {
		cancelOperation()
		<-leaseKeeperDone
	}()
	operation, err := e.operations.Execute(operationCtx, session, current.ID, request.OperationID, current.Lease.Version,
		mapping.capability, mapping.action, runtimeArguments, runtimeWorkspaceID, runtimeBindingVersion,
		workspaceViewID, workspaceCheckpointRevision, inputRevision)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
			return workspaceInputChangedResult(), nil
		}
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	operation, err = e.operations.Poll(operationCtx, session, current.ID, request.OperationID, current.Lease.Version, operation)
	if err != nil {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	result, err := mapRuntimeToolResult(mapping, operation, request.Arguments)
	if err == nil && desktopFocusChanged(mapping, operation) {
		result.Status, result.WaitingReason = AgentToolNeedsUser, "desktop_focus_changed"
	}
	return result, err
}

func workspaceInputChangedResult() AgentToolResult {
	return AgentToolResult{
		Status: AgentToolNeedsUser, WaitingReason: "workspace_input_changed",
		Output: "The Workspace changed after this task was submitted. Submit a new task to use the updated files.",
	}
}

// ReconcileOperation reads an operation by its durable runtime operation ID.
// It never sends Execute, and it refuses recovery when the original runtime
// query fence was not captured or no longer matches the returned journal row.
func (e *RuntimeToolExecutor) ReconcileOperation(ctx context.Context, request AgentToolRequest, persisted task.Operation) (AgentToolResult, error) {
	mapping, ok := runtimeToolMappingByName(request.Call.Name)
	if !ok {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	requestHash, hashErr := agentOperationHash(request.Call.Name, request.Arguments)
	terminal := persisted.Status == task.OperationSucceeded || persisted.Status == task.OperationFailed
	leaseMatches := persisted.LeaseVersion == request.Task.Lease.Version
	if terminal {
		leaseMatches = persisted.LeaseVersion > 0 && persisted.LeaseVersion <= request.Task.Lease.Version
	}
	expectedWorkspaceID, workspaceErr := runtimeToolOperationWorkspaceID(mapping, request.Arguments, request.Task)
	if workspaceErr != nil || mapping.scope != request.RequiredScope || !validRuntimeToolArguments(mapping, request.Arguments) ||
		hashErr != nil || !bytes.Equal(persisted.RequestHash, requestHash[:]) ||
		!domain.ValidOperationID(persisted.ID) || request.OperationID != "" && request.OperationID != persisted.ID ||
		persisted.RuntimeLeaseVersion < 1 || persisted.RuntimeInstanceID == "" ||
		persisted.TaskID != request.Task.ID || persisted.Key != request.OperationKey || persisted.HostID != request.Task.HostID ||
		persisted.WorkspaceID != expectedWorkspaceID || !sameTaskInputRevision(persisted.InputRevision, request.Task.InputRevision) ||
		!sameTaskInputRevision(persisted.WorkspaceCheckpointRevision, request.Task.WorkspaceCheckpointRevision) ||
		(persisted.WorkspaceID != "" && persisted.BindingVersion < 1) ||
		(persisted.WorkspaceID == "" && persisted.BindingVersion != 0) ||
		!leaseMatches || (persisted.Status != task.OperationUnknown && !terminal) {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	current, err := e.currentTaskForOperationReconciliation(ctx, request.Task)
	if err != nil {
		return AgentToolResult{}, err
	}
	currentWorkspaceID, workspaceErr := runtimeToolOperationWorkspaceID(mapping, request.Arguments, current)
	if workspaceErr != nil || currentWorkspaceID != persisted.WorkspaceID || !sameTaskInputRevision(current.InputRevision, persisted.InputRevision) ||
		!sameTaskInputRevision(current.WorkspaceCheckpointRevision, persisted.WorkspaceCheckpointRevision) ||
		(persisted.WorkspaceID != "" && (current.WorkspaceID != persisted.WorkspaceID || current.InputRevision == nil || *current.InputRevision < 0)) {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	if err := e.authorize(ctx, current, mapping.scope); err != nil {
		return AgentToolResult{}, err
	}
	expectedFence := AgentHostFence{
		RuntimeGeneration: persisted.RuntimeGeneration, RuntimeInstanceID: persisted.RuntimeInstanceID,
		ControlVersion: persisted.ControlVersion, AIAllowed: true,
	}
	fence, err := e.readFence(ctx, current, &expectedFence)
	if err != nil {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	session, err := e.connect(ctx, current, fence)
	if err != nil {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	defer session.Close()
	if !session.Supports(mapping.capability) || current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	if persisted.WorkspaceID != "" && !terminal {
		if runtimeclient.WorkspaceScopedCapability(mapping.capability) {
			if _, _, err := session.SetTaskWorkspaceView(ctx, current, persisted.WorkspaceID, persisted.BindingVersion); err != nil {
				return AgentToolResult{Status: AgentToolUnknown}, nil
			}
		} else if _, err := session.SyncTaskWorkspaceBindings(ctx, current, persisted.WorkspaceID, persisted.BindingVersion); err != nil {
			return AgentToolResult{Status: AgentToolUnknown}, nil
		}
	}
	if err := session.SetTaskLease(ctx, current.ID, current.Lease.Version, *current.Lease.ExpiresAt); err != nil {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	if terminal {
		status := AgentToolSucceeded
		if persisted.Status == task.OperationFailed {
			status = AgentToolFailed
			if desktopFocusChanged(mapping, runtimeclient.Operation{Status: runtimeclient.OperationFailed, Result: []byte(persisted.Result)}) {
				status = AgentToolNeedsUser
				return AgentToolResult{Status: status, WaitingReason: "desktop_focus_changed", ExternalID: persisted.ExternalID, Output: persisted.Result}, nil
			}
		}
		return AgentToolResult{Status: status, ExternalID: persisted.ExternalID, Output: persisted.Result}, nil
	}
	operation, err := e.operations.Get(ctx, session, current.ID, persisted.ID, persisted.RuntimeLeaseVersion)
	if err != nil {
		if isRuntimeOperationNotFound(err) {
			// The query went to the same authenticated runtime instance under the
			// original Host/control/task fence and original runtime lease. Its
			// explicit operation_not_found is the only state that may be rearmed.
			return AgentToolResult{Status: AgentToolNotFound, ExternalID: persisted.ID}, nil
		}
		// operation_not_found is only actionable when the runtime returned a
		// successful, exact-fence query against the same durable runtime instance.
		// Recovery deliberately pauses here until the persisted call checkpoint
		// can safely re-dispatch the same ID; this method never retries it.
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	if !runtimeToolOperationMatchesTaskOperation(operation, current, persisted, mapping) {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	operation, err = e.operations.Poll(ctx, session, current.ID, persisted.ID, persisted.RuntimeLeaseVersion, operation)
	if err != nil || !runtimeToolOperationMatchesTaskOperation(operation, current, persisted, mapping) {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	result, err := mapRuntimeToolResult(mapping, operation, request.Arguments)
	if err == nil && desktopFocusChanged(mapping, operation) {
		result.Status, result.WaitingReason = AgentToolNeedsUser, "desktop_focus_changed"
	}
	return result, err
}

func desktopFocusChanged(mapping runtimeToolMapping, operation runtimeclient.Operation) bool {
	if operation.Status != runtimeclient.OperationFailed || mapping.capability != "desktop.input" {
		return false
	}
	var result struct {
		Code string `json:"code"`
	}
	return json.Unmarshal(operation.Result, &result) == nil && result.Code == "desktop_focus_changed"
}

func runtimeOperationMatchesTaskOperation(operation runtimeclient.Operation, current task.Task, persisted task.Operation) bool {
	return operation.OperationID == persisted.ID && operation.TaskID == current.ID && operation.HostID == current.HostID &&
		operation.RuntimeGeneration == persisted.RuntimeGeneration && operation.RuntimeInstanceID == persisted.RuntimeInstanceID &&
		operation.ControlVersion == persisted.ControlVersion && operation.LeaseVersion == persisted.RuntimeLeaseVersion &&
		operation.WorkspaceID == persisted.WorkspaceID && operation.BindingVersion == persisted.BindingVersion
}

func runtimeToolOperationMatchesTaskOperation(operation runtimeclient.Operation, current task.Task, persisted task.Operation, mapping runtimeToolMapping) bool {
	workspaceID, bindingVersion := "", int64(0)
	if runtimeclient.WorkspaceScopedCapability(mapping.capability) {
		workspaceID, bindingVersion = persisted.WorkspaceID, persisted.BindingVersion
	}
	return operation.OperationID == persisted.ID && operation.TaskID == current.ID && operation.HostID == current.HostID &&
		operation.RuntimeGeneration == persisted.RuntimeGeneration && operation.RuntimeInstanceID == persisted.RuntimeInstanceID &&
		operation.ControlVersion == persisted.ControlVersion && operation.LeaseVersion == persisted.RuntimeLeaseVersion &&
		operation.WorkspaceID == workspaceID && operation.BindingVersion == bindingVersion
}

func agentFenceFromRuntime(fence runtimeclient.HostFence) AgentHostFence {
	return AgentHostFence{
		RuntimeGeneration: fence.RuntimeGeneration,
		RuntimeInstanceID: fence.RuntimeInstanceID,
		ControlVersion:    fence.ControlVersion,
		AIAllowed:         true,
	}
}

func (e *RuntimeToolExecutor) keepRuntimeTaskLease(ctx context.Context, cancelOperation context.CancelFunc, session *RuntimeSession, claimed task.Task, operationID string) {
	interval := e.leaseEvery
	if interval <= 0 {
		interval = runtimeToolLeaseRefresh
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastExpiry := *claimed.Lease.ExpiresAt
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		current, err := e.currentTask(ctx, claimed)
		if err != nil {
			cancelOperation()
			e.cancelRuntimeOperation(session, operationID)
			return
		}
		expiresAt := *current.Lease.ExpiresAt
		if expiresAt.Before(lastExpiry) {
			cancelOperation()
			e.cancelRuntimeOperation(session, operationID)
			return
		}
		if expiresAt.Equal(lastExpiry) {
			continue
		}
		if err := session.SetTaskLease(ctx, current.ID, current.Lease.Version, expiresAt); err != nil {
			cancelOperation()
			e.cancelRuntimeOperation(session, operationID)
			return
		}
		lastExpiry = expiresAt
	}
}

func (e *RuntimeToolExecutor) cancelRuntimeOperation(session *RuntimeSession, operationID string) {
	ctx, cancel := context.WithTimeout(context.Background(), runtimeOperationReconcileTimeout)
	defer cancel()
	_, _ = session.CancelOperation(ctx, operationID)
}

func (e *RuntimeToolExecutor) Verify(ctx context.Context, request AgentVerificationRequest) (AgentVerificationResult, error) {
	if !domain.ValidStepID(request.StepID) || (len(request.Evidence) > 0 && !validAgentFileEvidence(request.Evidence)) ||
		(len(request.Evidence) == 0 && request.ScheduleObservation == nil) {
		return unknownAgentVerification("Runtime file evidence is missing or invalid."), nil
	}
	if request.ScheduleObservation != nil && request.ScheduleObservation.Validate() != nil {
		return unknownAgentVerification("The scheduled web field observation is invalid."), nil
	}
	current, err := e.currentTask(ctx, request.Task)
	if err != nil {
		return unknownAgentVerification("The task lease is no longer active."), nil
	}
	if len(request.Evidence) == 0 {
		return e.observeScheduledField(ctx, current, request), nil
	}
	if err := e.authorize(ctx, current, identity.ScopeComputerObserve); err != nil {
		return unknownAgentVerification("Current task observation permission is unavailable."), nil
	}
	fence, err := e.readFence(ctx, current, &request.Fence)
	if err != nil {
		return unknownAgentVerification("Host control changed before file verification."), nil
	}
	session, err := e.connect(ctx, current, fence)
	if err != nil {
		return unknownAgentVerification("The current Host runtime is unavailable."), nil
	}
	defer session.Close()
	// Persist the authenticated session's exact runtime instance together with
	// the Host generation/control version. The reader may only provide a partial
	// fence on older Host records, while operation rows require this instance ID.
	fence = agentFenceFromRuntime(session.Fence)
	if !session.Supports(runtimeFileVerifyCapability) {
		return unknownAgentVerification("The current Host runtime cannot independently verify files."), nil
	}
	if current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) {
		return unknownAgentVerification("The task lease is no longer active."), nil
	}

	groups, invalid := e.groupFileEvidence(ctx, current, request.Evidence)
	if invalid.Status != "" {
		return invalid, nil
	}
	verified := AgentVerificationResult{Status: AgentVerified}
	for _, group := range groups {
		publishKey := taskWorkspacePublishOperationKey(request.StepID, group.workspaceID)
		if publishKey == "" {
			return unknownAgentVerification("The Task Workspace publish identity is invalid."), nil
		}
		receipt, receiptErr := e.workspaces.FindTaskWorkspacePublishReceipt(ctx, current.OwnerID, current.ID, group.workspaceID, publishKey)
		if receiptErr == nil {
			switch receipt.State {
			case TaskWorkspacePublishSucceeded:
				result := e.verifyPublishedTaskWorkspaceArtifacts(ctx, session, current, group, receipt, nil)
				if result.Status != AgentVerified {
					return result, nil
				}
				verified.Artifacts = append(verified.Artifacts, result.Artifacts...)
				continue
			case TaskWorkspacePublishConflict:
				return workspaceInputChangedVerification(), nil
			case TaskWorkspacePublishQuotaRejected:
				return workspacePublishQuotaVerification(), nil
			case TaskWorkspacePublishPending:
			default:
				return unknownAgentVerification("The Task Workspace publish receipt is invalid."), nil
			}
		} else if domain.CodeOf(receiptErr) != domain.CodeNotFound {
			return unknownAgentVerification("The Task Workspace publish receipt could not be read."), nil
		}
		_, group.viewID, err = session.SetTaskWorkspaceView(ctx, current, group.workspaceID, group.bindingVersion)
		if err != nil {
			if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
				return AgentVerificationResult{Status: AgentVerifyUnknown, WaitingReason: "workspace_input_changed",
					Summary: "Workspace changed after this task was submitted."}, nil
			}
			return unknownAgentVerification("The file evidence belongs to an inactive or changed Workspace binding."), nil
		}
		result := e.verifyFileEvidenceGroup(ctx, session, current, fence, request.StepID, group)
		if result.Status != AgentVerified {
			return result, nil
		}
		verified.Artifacts = append(verified.Artifacts, result.Artifacts...)
	}
	verified.Summary = fmt.Sprintf("The runtime independently matched %d Workspace file(s) across %d binding(s).", len(verified.Artifacts), len(groups))
	return e.withScheduledObservation(ctx, current, request, verified), nil
}

type runtimeFileEvidenceGroup struct {
	workspaceID    string
	bindingVersion int64
	viewID         string
	files          []AgentFileEvidence
}

func (e *RuntimeToolExecutor) groupFileEvidence(ctx context.Context, current task.Task, evidence []AgentFileEvidence) ([]runtimeFileEvidenceGroup, AgentVerificationResult) {
	type groupKey struct {
		workspaceID    string
		bindingVersion int64
	}
	groups := make(map[groupKey][]AgentFileEvidence)
	pathWorkspace := make(map[string]string, len(evidence))
	for _, item := range evidence {
		operation, err := e.store.GetOperation(ctx, current.OwnerID, current.ID, item.OperationKey)
		if err != nil || operation.TaskID != current.ID || operation.Key != item.OperationKey || operation.HostID != current.HostID ||
			operation.WorkspaceID != item.WorkspaceID || operation.BindingVersion < 1 ||
			!bytes.Equal(operation.RequestHash, item.RequestHash) {
			return nil, unknownAgentVerification("A file_write candidate has no matching durable Workspace operation.")
		}
		if operation.Status != task.OperationSucceeded {
			return nil, unknownAgentVerification("A file_write candidate is not durably recorded as successful.")
		}
		if operation.Result != item.Result {
			return nil, unknownAgentVerification("A file_write candidate result does not match its durable operation.")
		}
		if workspaceID, exists := pathWorkspace[item.Path]; exists && workspaceID != item.WorkspaceID {
			return nil, AgentVerificationResult{
				Status: AgentNotVerified,
				Summary: fmt.Sprintf("Artifact path conflict: %q was written in Workspaces %s and %s; verification cannot choose one.",
					item.Path, workspaceID, item.WorkspaceID),
			}
		}
		pathWorkspace[item.Path] = item.WorkspaceID
		key := groupKey{workspaceID: item.WorkspaceID, bindingVersion: operation.BindingVersion}
		groups[key] = append(groups[key], item)
	}
	keys := make([]groupKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].workspaceID != keys[j].workspaceID {
			return keys[i].workspaceID < keys[j].workspaceID
		}
		return keys[i].bindingVersion < keys[j].bindingVersion
	})
	result := make([]runtimeFileEvidenceGroup, 0, len(keys))
	for _, key := range keys {
		result = append(result, runtimeFileEvidenceGroup{
			workspaceID: key.workspaceID, bindingVersion: key.bindingVersion, files: groups[key],
		})
	}
	return result, AgentVerificationResult{}
}

func (e *RuntimeToolExecutor) verifyFileEvidenceGroup(ctx context.Context, session *RuntimeSession, current task.Task, fence AgentHostFence, stepID string, group runtimeFileEvidenceGroup) AgentVerificationResult {
	arguments, err := json.Marshal(struct {
		Files []AgentFileEvidence `json:"files"`
	}{Files: group.files})
	if err != nil {
		return unknownAgentVerification("The Workspace file verification request is invalid.")
	}
	operationKey := runtimeVerificationOperationKey(stepID, group.workspaceID, group.bindingVersion)
	requestHash, err := agentOperationHash(runtimeFileVerifyCapability+"/"+runtimeFileVerifyAction, arguments)
	if err != nil || operationKey == "" {
		return unknownAgentVerification("The Workspace file verification identity is invalid.")
	}
	operationID, err := domain.NewOperationID()
	if err != nil {
		return unknownAgentVerification("The file verification operation could not be registered.")
	}
	persisted, replayed, err := e.store.CreateOperation(ctx, task.Operation{
		ID: operationID, TaskID: current.ID, Key: operationKey, RequestHash: requestHash[:], HostID: current.HostID,
		WorkspaceID: group.workspaceID, BindingVersion: group.bindingVersion,
		InputRevision: current.InputRevision, WorkspaceCheckpointRevision: current.WorkspaceCheckpointRevision,
		RuntimeGeneration: fence.RuntimeGeneration, RuntimeInstanceID: fence.RuntimeInstanceID,
		ControlVersion: fence.ControlVersion, LeaseVersion: current.Lease.Version, RuntimeLeaseVersion: current.Lease.Version,
		Status: task.OperationPrepared,
	}, current.Lease.Owner)
	if err != nil {
		if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
			return AgentVerificationResult{Status: AgentVerifyUnknown, WaitingReason: "workspace_input_changed",
				Summary: "Workspace changed after this task was submitted."}
		}
		return unknownAgentVerification("The file verification operation could not be registered.")
	}
	if persisted.TaskID != current.ID || persisted.Key != operationKey || persisted.HostID != current.HostID ||
		persisted.WorkspaceID != group.workspaceID || persisted.BindingVersion != group.bindingVersion ||
		!bytes.Equal(persisted.RequestHash, requestHash[:]) {
		return unknownAgentVerification("The registered file verification operation does not match this Workspace binding.")
	}
	if replayed {
		if persisted.Status == task.OperationSucceeded || persisted.Status == task.OperationFailed {
			if persisted.RuntimeGeneration != fence.RuntimeGeneration || persisted.RuntimeInstanceID != fence.RuntimeInstanceID ||
				persisted.ControlVersion != fence.ControlVersion {
				return unknownAgentVerification("The verification result belongs to a different Host control fence.")
			}
			return e.publishWorkspaceArtifacts(ctx, session, current, fence, stepID, group, mapFileVerificationOperation(persisted, group.files))
		}
		if persisted.Status == task.OperationDispatched {
			persisted, err = e.store.UpdateOperation(ctx, OperationUpdate{
				TaskID: current.ID, OperationKey: operationKey, LeaseOwner: current.Lease.Owner,
				LeaseVersion: current.Lease.Version, Status: task.OperationUnknown,
			})
			if err != nil {
				return unknownAgentVerification("The dispatched file verification result could not be safely reconciled.")
			}
		}
		if persisted.Status == task.OperationUnknown {
			if persisted.LeaseVersion != current.Lease.Version || persisted.RuntimeLeaseVersion < 1 ||
				persisted.RuntimeGeneration != fence.RuntimeGeneration || persisted.RuntimeInstanceID != fence.RuntimeInstanceID ||
				persisted.ControlVersion != fence.ControlVersion {
				return unknownAgentVerification("The unknown verification operation belongs to a different lease or Host fence.")
			}
			if err := session.SetTaskLease(ctx, current.ID, current.Lease.Version, *current.Lease.ExpiresAt); err != nil {
				return unknownAgentVerification("The runtime task lease could not be synchronized for reconciliation.")
			}
			return e.reconcileFileVerificationOperation(ctx, session, current, fence, stepID, operationKey, persisted, group)
		}
		if persisted.Status != task.OperationPrepared {
			return unknownAgentVerification("The file verification operation is in an unsupported state.")
		}
		if persisted.LeaseVersion != current.Lease.Version {
			persisted, err = e.store.UpdateOperation(ctx, OperationUpdate{
				TaskID: current.ID, OperationKey: operationKey, LeaseOwner: current.Lease.Owner,
				LeaseVersion: current.Lease.Version, RuntimeGeneration: fence.RuntimeGeneration,
				RuntimeInstanceID: fence.RuntimeInstanceID, ControlVersion: fence.ControlVersion,
				RuntimeLeaseVersion: current.Lease.Version, AdoptPrepared: true, Status: task.OperationPrepared,
			})
			if err != nil {
				return unknownAgentVerification("The prepared verification operation could not be adopted safely.")
			}
		} else if persisted.RuntimeGeneration != fence.RuntimeGeneration || persisted.RuntimeInstanceID != fence.RuntimeInstanceID ||
			persisted.ControlVersion != fence.ControlVersion {
			return unknownAgentVerification("The prepared verification operation belongs to a different Host fence.")
		}
	}
	if current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) {
		return unknownAgentVerification("The task lease is no longer active.")
	}
	if err := session.SetTaskLease(ctx, current.ID, current.Lease.Version, *current.Lease.ExpiresAt); err != nil {
		return unknownAgentVerification("The runtime task lease could not be synchronized.")
	}
	if _, err := e.store.UpdateOperation(ctx, OperationUpdate{
		TaskID: current.ID, OperationKey: operationKey, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		RuntimeGeneration: fence.RuntimeGeneration, RuntimeInstanceID: fence.RuntimeInstanceID,
		ControlVersion: fence.ControlVersion, RuntimeLeaseVersion: current.Lease.Version, Status: task.OperationDispatched,
	}); err != nil {
		return unknownAgentVerification("The file verification operation could not pass its persisted fence.")
	}
	operationCtx, cancelOperation := context.WithCancel(ctx)
	leaseKeeperDone := make(chan struct{})
	go func() {
		defer close(leaseKeeperDone)
		e.keepRuntimeTaskLease(operationCtx, cancelOperation, session, current, persisted.ID)
	}()
	defer func() {
		cancelOperation()
		<-leaseKeeperDone
	}()
	if group.viewID == "" || current.WorkspaceCheckpointRevision == nil {
		return unknownAgentVerification("The Task Workspace checkpoint is unavailable.")
	}
	operation, err := session.Execute(operationCtx, current.ID, persisted.ID, current.Lease.Version,
		runtimeFileVerifyCapability, runtimeFileVerifyAction, arguments, group.workspaceID, group.bindingVersion,
		group.viewID, *current.WorkspaceCheckpointRevision, current.InputRevision)
	if err == nil && runtimeOperationMatchesTaskOperation(operation, current, persisted) {
		operation, err = e.operations.Poll(operationCtx, session, current.ID, persisted.ID, current.Lease.Version, operation)
	} else if err == nil {
		err = errors.New("runtime verification operation fence mismatch")
	}
	if err != nil || !runtimeOperationMatchesTaskOperation(operation, current, persisted) {
		_, _ = e.store.UpdateOperation(context.WithoutCancel(ctx), OperationUpdate{
			TaskID: current.ID, OperationKey: operationKey, LeaseOwner: current.Lease.Owner,
			LeaseVersion: current.Lease.Version, Status: task.OperationUnknown,
		})
		return unknownAgentVerification("The runtime file verification result is unknown and will not be replayed.")
	}
	switch operation.Status {
	case runtimeclient.OperationSucceeded:
		if len(operation.Result) == 0 || len(operation.Result) > maxRuntimeToolResultBytes {
			_, _ = e.store.UpdateOperation(ctx, OperationUpdate{
				TaskID: current.ID, OperationKey: operationKey, LeaseOwner: current.Lease.Owner,
				LeaseVersion: current.Lease.Version, Status: task.OperationUnknown,
			})
			return unknownAgentVerification("The runtime returned an invalid file verification result.")
		}
		if _, err := e.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: current.ID, OperationKey: operationKey, LeaseOwner: current.Lease.Owner,
			LeaseVersion: current.Lease.Version, Status: task.OperationSucceeded,
			ExternalID: operation.OperationID, Result: string(operation.Result),
		}); err != nil {
			return unknownAgentVerification("The file verification result could not be persisted.")
		}
		persisted.Status = task.OperationSucceeded
		persisted.ExternalID = operation.OperationID
		persisted.Result = string(operation.Result)
		return e.publishWorkspaceArtifacts(ctx, session, current, fence, stepID, group, mapFileVerificationOperation(persisted, group.files))
	case runtimeclient.OperationFailed:
		if _, err := e.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: current.ID, OperationKey: operationKey, LeaseOwner: current.Lease.Owner,
			LeaseVersion: current.Lease.Version, Status: task.OperationFailed, ExternalID: operation.OperationID,
		}); err != nil {
			return unknownAgentVerification("The file verification failure could not be persisted.")
		}
		return AgentVerificationResult{Status: AgentNotVerified, Summary: "The runtime could not verify the requested files."}
	default:
		_, _ = e.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: current.ID, OperationKey: operationKey, LeaseOwner: current.Lease.Owner,
			LeaseVersion: current.Lease.Version, Status: task.OperationUnknown,
		})
		return unknownAgentVerification("The runtime file verification result is unknown.")
	}
}

func (e *RuntimeToolExecutor) withScheduledObservation(ctx context.Context, current task.Task, verification AgentVerificationRequest, result AgentVerificationResult) AgentVerificationResult {
	if result.Status != AgentVerified || verification.ScheduleObservation == nil {
		return result
	}
	observed := e.observeScheduledField(ctx, current, verification)
	if observed.Status == AgentVerified {
		result.ObservationFingerprint = observed.ObservationFingerprint
	}
	return result
}

func (e *RuntimeToolExecutor) observeScheduledField(ctx context.Context, current task.Task, verification AgentVerificationRequest) AgentVerificationResult {
	spec := verification.ScheduleObservation
	if spec == nil || spec.Validate() != nil {
		return unknownAgentVerification("The scheduled web field observation is invalid.")
	}
	arguments, err := json.Marshal(spec)
	if err != nil {
		return unknownAgentVerification("The scheduled web field observation is invalid.")
	}
	key := "agent:observation:" + verification.StepID
	mapping, ok := runtimeToolMappingByName(runtimeToolBrowserObserveField)
	if !ok {
		return unknownAgentVerification("The scheduled web field observation is invalid.")
	}
	workspaceID, err := runtimeToolOperationWorkspaceID(mapping, arguments, current)
	if err != nil {
		return unknownAgentVerification("The current Task Workspace binding is invalid.")
	}
	request := AgentToolRequest{
		Task: current, Call: ModelToolCall{CallID: "scheduled-observation:" + verification.StepID, Name: runtimeToolBrowserObserveField},
		Arguments: arguments, OperationKey: key, RequiredScope: identity.ScopeComputerObserve, Fence: verification.Fence, WorkspaceID: workspaceID,
	}
	preparation, err := e.Prepare(ctx, request)
	if err != nil || preparation.Status != AgentToolReady || !sameAgentControlFence(preparation.Fence, verification.Fence) {
		return unknownAgentVerification("The Host could not safely prepare the scheduled field observation.")
	}
	request.Fence = preparation.Fence
	request.WorkspaceID = preparation.WorkspaceID
	hash, err := agentOperationHash(request.Call.Name, request.Arguments)
	if err != nil {
		return unknownAgentVerification("The scheduled web field observation is invalid.")
	}
	operationID, err := domain.NewOperationID()
	if err != nil {
		return unknownAgentVerification("The scheduled field observation could not be registered.")
	}
	operation, replayed, err := e.store.CreateOperation(ctx, task.Operation{
		ID: operationID, TaskID: current.ID, Key: key, RequestHash: hash[:], HostID: current.HostID,
		WorkspaceID: preparation.WorkspaceID, BindingVersion: preparation.BindingVersion, InputRevision: current.InputRevision,
		WorkspaceCheckpointRevision: current.WorkspaceCheckpointRevision,
		RuntimeGeneration:           preparation.Fence.RuntimeGeneration, RuntimeInstanceID: preparation.Fence.RuntimeInstanceID,
		ControlVersion: preparation.Fence.ControlVersion, LeaseVersion: current.Lease.Version,
		RuntimeLeaseVersion: current.Lease.Version, Status: task.OperationPrepared,
	}, current.Lease.Owner)
	if err != nil || operation.TaskID != current.ID || operation.Key != key || operation.HostID != current.HostID ||
		operation.WorkspaceID != preparation.WorkspaceID || operation.BindingVersion != preparation.BindingVersion ||
		!sameTaskInputRevision(operation.InputRevision, current.InputRevision) ||
		!sameTaskInputRevision(operation.WorkspaceCheckpointRevision, current.WorkspaceCheckpointRevision) ||
		!bytes.Equal(operation.RequestHash, hash[:]) {
		return unknownAgentVerification("The scheduled field observation could not be registered safely.")
	}
	request.OperationID = operation.ID
	if replayed {
		switch operation.Status {
		case task.OperationSucceeded:
			if operation.RuntimeGeneration != preparation.Fence.RuntimeGeneration || operation.RuntimeInstanceID != preparation.Fence.RuntimeInstanceID ||
				operation.ControlVersion != preparation.Fence.ControlVersion {
				return unknownAgentVerification("The scheduled observation belongs to a different Host fence.")
			}
			return scheduledObservationResult(operation.Result)
		case task.OperationFailed:
			return unknownAgentVerification("The scheduled field observation failed.")
		case task.OperationDispatched:
			operation, err = e.store.UpdateOperation(ctx, OperationUpdate{
				TaskID: current.ID, OperationKey: key, LeaseOwner: current.Lease.Owner,
				LeaseVersion: current.Lease.Version, Status: task.OperationUnknown,
			})
			if err != nil {
				return unknownAgentVerification("The original scheduled observation could not be reconciled safely.")
			}
			fallthrough
		case task.OperationUnknown:
			if operation.LeaseVersion != current.Lease.Version || operation.RuntimeGeneration != preparation.Fence.RuntimeGeneration ||
				operation.RuntimeInstanceID != preparation.Fence.RuntimeInstanceID || operation.ControlVersion != preparation.Fence.ControlVersion {
				return unknownAgentVerification("The scheduled observation belongs to a different task lease or Host fence.")
			}
			result, reconcileErr := e.ReconcileOperation(ctx, request, operation)
			if reconcileErr != nil {
				return unknownAgentVerification("The original scheduled observation could not be reconciled safely.")
			}
			switch result.Status {
			case AgentToolSucceeded:
				if _, err := e.store.UpdateOperation(ctx, OperationUpdate{
					TaskID: current.ID, OperationKey: key, LeaseOwner: current.Lease.Owner,
					LeaseVersion: current.Lease.Version, Status: task.OperationSucceeded, ExternalID: result.ExternalID, Result: result.Output,
				}); err != nil {
					return unknownAgentVerification("The reconciled scheduled observation could not be persisted.")
				}
				return scheduledObservationResult(result.Output)
			case AgentToolFailed:
				_, _ = e.store.UpdateOperation(ctx, OperationUpdate{
					TaskID: current.ID, OperationKey: key, LeaseOwner: current.Lease.Owner,
					LeaseVersion: current.Lease.Version, Status: task.OperationFailed, ExternalID: result.ExternalID,
				})
				return unknownAgentVerification("The scheduled field observation failed.")
			case AgentToolNotFound:
				operation, err = e.store.UpdateOperation(ctx, OperationUpdate{
					TaskID: current.ID, OperationKey: key, LeaseOwner: current.Lease.Owner,
					LeaseVersion: current.Lease.Version, RuntimeGeneration: preparation.Fence.RuntimeGeneration,
					RuntimeInstanceID: preparation.Fence.RuntimeInstanceID, ControlVersion: preparation.Fence.ControlVersion,
					RuntimeLeaseVersion: current.Lease.Version, RearmAfterNotFound: true, Status: task.OperationPrepared,
				})
				if err != nil {
					return unknownAgentVerification("The not-found scheduled observation could not be rearmed safely.")
				}
			default:
				return unknownAgentVerification("The original scheduled observation remains unknown.")
			}
		case task.OperationPrepared:
			if operation.LeaseVersion != current.Lease.Version {
				operation, err = e.store.UpdateOperation(ctx, OperationUpdate{
					TaskID: current.ID, OperationKey: key, LeaseOwner: current.Lease.Owner,
					LeaseVersion: current.Lease.Version, RuntimeGeneration: preparation.Fence.RuntimeGeneration,
					RuntimeInstanceID: preparation.Fence.RuntimeInstanceID, ControlVersion: preparation.Fence.ControlVersion,
					RuntimeLeaseVersion: current.Lease.Version, AdoptPrepared: true, Status: task.OperationPrepared,
				})
				if err != nil {
					return unknownAgentVerification("The prepared scheduled observation could not be adopted safely.")
				}
			} else if operation.RuntimeGeneration != preparation.Fence.RuntimeGeneration || operation.RuntimeInstanceID != preparation.Fence.RuntimeInstanceID ||
				operation.ControlVersion != preparation.Fence.ControlVersion {
				return unknownAgentVerification("The prepared scheduled observation belongs to a different Host fence.")
			}
		default:
			return unknownAgentVerification("The scheduled field observation is in an unsupported state.")
		}
	}
	if _, err := e.store.UpdateOperation(ctx, OperationUpdate{
		TaskID: current.ID, OperationKey: key, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		RuntimeGeneration: preparation.Fence.RuntimeGeneration, RuntimeInstanceID: preparation.Fence.RuntimeInstanceID,
		ControlVersion: preparation.Fence.ControlVersion, RuntimeLeaseVersion: current.Lease.Version, Status: task.OperationDispatched,
	}); err != nil {
		return unknownAgentVerification("The scheduled field observation did not pass its persisted Host fence.")
	}
	result, err := e.Execute(ctx, request, preparation)
	if err != nil || result.Status == AgentToolUnknown {
		_, _ = e.store.UpdateOperation(context.WithoutCancel(ctx), OperationUpdate{
			TaskID: current.ID, OperationKey: key, LeaseOwner: current.Lease.Owner,
			LeaseVersion: current.Lease.Version, Status: task.OperationUnknown,
		})
		return unknownAgentVerification("The scheduled field observation result is unknown.")
	}
	status := task.OperationSucceeded
	if result.Status == AgentToolFailed {
		status = task.OperationFailed
	}
	if _, err := e.store.UpdateOperation(ctx, OperationUpdate{
		TaskID: current.ID, OperationKey: key, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		Status: status, ExternalID: result.ExternalID, Result: result.Output,
	}); err != nil || status != task.OperationSucceeded {
		return unknownAgentVerification("The scheduled field observation could not be saved safely.")
	}
	return scheduledObservationResult(result.Output)
}

func scheduledObservationResult(raw string) AgentVerificationResult {
	var result struct {
		Fingerprint string `json:"fingerprint"`
		Origin      string `json:"origin"`
		MatchCount  int    `json:"match_count"`
	}
	if decodeStrictRuntimeResult([]byte(raw), &result) != nil || !validAgentObservationFingerprint(result.Fingerprint) ||
		!validBrowserOrigin(result.Origin) || result.MatchCount != 1 {
		return unknownAgentVerification("The scheduled field observation returned an invalid fingerprint.")
	}
	return AgentVerificationResult{
		Status: AgentVerified, Summary: "The scheduled web field was observed independently.",
		ObservationFingerprint: result.Fingerprint,
	}
}

// reconcileFileVerificationOperation reads only the original runtime journal
// row. It never dispatches or rearms an uncertain verification action.
func (e *RuntimeToolExecutor) reconcileFileVerificationOperation(ctx context.Context, session *RuntimeSession, current task.Task, fence AgentHostFence, stepID, operationKey string, persisted task.Operation, group runtimeFileEvidenceGroup) AgentVerificationResult {
	operationCtx, cancelOperation := context.WithCancel(ctx)
	leaseKeeperDone := make(chan struct{})
	go func() {
		defer close(leaseKeeperDone)
		e.keepRuntimeTaskLease(operationCtx, cancelOperation, session, current, persisted.ID)
	}()
	defer func() {
		cancelOperation()
		<-leaseKeeperDone
	}()
	operation, err := e.operations.Get(operationCtx, session, current.ID, persisted.ID, persisted.RuntimeLeaseVersion)
	if err != nil {
		return unknownAgentVerification("The runtime could not confirm the original verification operation.")
	}
	if !runtimeOperationMatchesTaskOperation(operation, current, persisted) {
		return unknownAgentVerification("The runtime operation does not match its persisted Host and lease fence.")
	}
	operation, err = e.operations.Poll(operationCtx, session, current.ID, persisted.ID, persisted.RuntimeLeaseVersion, operation)
	if err != nil || !runtimeOperationMatchesTaskOperation(operation, current, persisted) {
		return unknownAgentVerification("The original runtime verification operation remains unknown.")
	}
	switch operation.Status {
	case runtimeclient.OperationSucceeded:
		if len(operation.Result) == 0 || len(operation.Result) > maxRuntimeToolResultBytes {
			return unknownAgentVerification("The original runtime verification result is invalid.")
		}
		if _, err := e.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: current.ID, OperationKey: operationKey, LeaseOwner: current.Lease.Owner,
			LeaseVersion: current.Lease.Version, Status: task.OperationSucceeded,
			ExternalID: operation.OperationID, Result: string(operation.Result),
		}); err != nil {
			return unknownAgentVerification("The reconciled file verification result could not be persisted.")
		}
		persisted.Status = task.OperationSucceeded
		persisted.ExternalID = operation.OperationID
		persisted.Result = string(operation.Result)
		return e.publishWorkspaceArtifacts(ctx, session, current, fence, stepID, group, mapFileVerificationOperation(persisted, group.files))
	case runtimeclient.OperationFailed:
		if _, err := e.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: current.ID, OperationKey: operationKey, LeaseOwner: current.Lease.Owner,
			LeaseVersion: current.Lease.Version, Status: task.OperationFailed, ExternalID: operation.OperationID,
		}); err != nil {
			return unknownAgentVerification("The reconciled file verification failure could not be persisted.")
		}
		return AgentVerificationResult{Status: AgentNotVerified, Summary: "The runtime could not verify the requested files."}
	default:
		return unknownAgentVerification("The original runtime verification operation remains unknown.")
	}
}

func runtimeVerificationOperationKey(stepID, workspaceID string, bindingVersion int64) string {
	if !domain.ValidStepID(stepID) || !domain.ValidWorkspaceID(workspaceID) || bindingVersion < 1 {
		return ""
	}
	return fmt.Sprintf("agent:verify:%s:%s:%d", stepID, workspaceID, bindingVersion)
}

func validAgentFileEvidence(evidence []AgentFileEvidence) bool {
	if len(evidence) == 0 || len(evidence) > maxAgentVerifyFiles {
		return false
	}
	seen := make(map[string]struct{}, len(evidence))
	var total int64
	for _, item := range evidence {
		if !validWorkspaceRelativePath(item.Path) || !domain.ValidWorkspaceID(item.WorkspaceID) ||
			item.OperationKey == "" || len(item.OperationKey) > 255 || len(item.RequestHash) != sha256.Size ||
			item.Size < 0 || item.Size > maxAgentVerifyFileBytes || len(item.SHA256) != 64 {
			return false
		}
		if _, err := hex.DecodeString(item.SHA256); err != nil || strings.ToLower(item.SHA256) != item.SHA256 {
			return false
		}
		key := item.WorkspaceID + "\x00" + item.Path
		if _, exists := seen[key]; exists {
			return false
		}
		seen[key] = struct{}{}
		total += item.Size
		if total > maxAgentVerifyTotalBytes {
			return false
		}
	}
	return true
}

func unknownAgentVerification(summary string) AgentVerificationResult {
	return AgentVerificationResult{Status: AgentVerifyUnknown, Summary: summary}
}

func mapFileVerificationOperation(operation task.Operation, expected []AgentFileEvidence) AgentVerificationResult {
	if operation.Status == task.OperationFailed {
		return AgentVerificationResult{Status: AgentNotVerified, Summary: "The runtime could not verify the requested files."}
	}
	if operation.Status != task.OperationSucceeded || !validRuntimeJSONObject([]byte(operation.Result), maxRuntimeToolResultBytes) {
		return unknownAgentVerification("The runtime file verification result is invalid.")
	}
	var response struct {
		Files []struct {
			Path    string `json:"path"`
			Size    int64  `json:"size"`
			SHA256  string `json:"sha256"`
			Matches bool   `json:"matches"`
		} `json:"files"`
	}
	if decodeStrictRuntimeResult([]byte(operation.Result), &response) != nil || len(response.Files) != len(expected) {
		return AgentVerificationResult{Status: AgentNotVerified, Summary: "The runtime did not return a matching file verification result."}
	}
	want := make(map[string]AgentFileEvidence, len(expected))
	for _, item := range expected {
		want[item.Path] = item
	}
	seen := make(map[string]struct{}, len(response.Files))
	artifacts := make([]AgentVerifiedArtifact, 0, len(expected))
	for _, actual := range response.Files {
		candidate, exists := want[actual.Path]
		if !exists {
			return AgentVerificationResult{Status: AgentNotVerified, Summary: "The runtime file result did not match the requested evidence."}
		}
		if _, exists := seen[actual.Path]; exists {
			return AgentVerificationResult{Status: AgentNotVerified, Summary: "The runtime returned duplicate file verification results."}
		}
		seen[actual.Path] = struct{}{}
		if !actual.Matches || actual.Size != candidate.Size || actual.SHA256 != candidate.SHA256 {
			return AgentVerificationResult{
				Status: AgentNotVerified, Summary: "The current Workspace file does not match the successful write evidence.",
				Observation: "At least one file's current size or SHA-256 differs from the recorded successful file_write result.",
			}
		}
		mediaType := mime.TypeByExtension(path.Ext(actual.Path))
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		artifacts = append(artifacts, AgentVerifiedArtifact{
			Label: path.Base(actual.Path), Path: actual.Path, MediaType: mediaType,
			WorkspaceID: candidate.WorkspaceID, SizeBytes: actual.Size, SHA256: actual.SHA256,
		})
	}
	return AgentVerificationResult{
		Status: AgentVerified, Summary: fmt.Sprintf("The runtime independently matched %d Workspace file(s) by size and SHA-256.", len(artifacts)),
		Artifacts: artifacts,
	}
}

func (e *RuntimeToolExecutor) publishWorkspaceArtifacts(ctx context.Context, session *RuntimeSession, current task.Task, fence AgentHostFence, stepID string, group runtimeFileEvidenceGroup, result AgentVerificationResult) AgentVerificationResult {
	if result.Status != AgentVerified || len(result.Artifacts) == 0 {
		return result
	}
	if current.InputRevision == nil || current.WorkspaceCheckpointRevision == nil || group.viewID == "" || group.workspaceID != current.WorkspaceID {
		return unknownAgentVerification("The Task Workspace checkpoint is unavailable for publishing.")
	}
	inputManifestSHA := hex.EncodeToString(current.ContextDigest)
	operationKey := taskWorkspacePublishOperationKey(stepID, group.workspaceID)
	requestArguments, err := json.Marshal(struct {
		OwnerID, TaskID, WorkspaceID, StepID, ViewID, InputManifestSHA256 string
		BindingVersion, InputRevision, BaseRevision                       int64
	}{
		OwnerID: current.OwnerID, TaskID: current.ID, WorkspaceID: group.workspaceID, StepID: stepID,
		ViewID: group.viewID, InputManifestSHA256: inputManifestSHA, BindingVersion: group.bindingVersion,
		InputRevision: *current.InputRevision, BaseRevision: *current.WorkspaceCheckpointRevision,
	})
	if err != nil || operationKey == "" {
		return unknownAgentVerification("The Task Workspace publish request is invalid.")
	}
	requestHash, err := agentOperationHash("task.workspace.publish", requestArguments)
	if err != nil {
		return unknownAgentVerification("The Task Workspace publish request is invalid.")
	}
	command := TaskWorkspacePublishCommand{
		OwnerID: current.OwnerID, TaskID: current.ID, WorkspaceID: group.workspaceID, HostID: current.HostID,
		LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, BindingVersion: group.bindingVersion,
		InputRevision: *current.InputRevision, BaseRevision: *current.WorkspaceCheckpointRevision,
		HostFence: fence, ViewID: group.viewID, InputManifestSHA256: inputManifestSHA,
		OperationKey: operationKey, RequestHash: requestHash[:],
	}
	receipt, err := e.workspaces.ReserveTaskWorkspacePublish(ctx, command)
	if err != nil {
		return taskWorkspacePublishErrorVerification(err)
	}
	if receipt.State == TaskWorkspacePublishSucceeded {
		return e.verifyPublishedTaskWorkspaceArtifacts(ctx, session, current, group, receipt, &result)
	}
	if receipt.State == TaskWorkspacePublishConflict {
		return workspaceInputChangedVerification()
	}
	if receipt.State == TaskWorkspacePublishQuotaRejected {
		return workspacePublishQuotaVerification()
	}
	files, manifestSHA, err := taskWorkspaceOutputManifest(ctx, session, current, group)
	if err != nil {
		return taskWorkspacePublishErrorVerification(err)
	}
	receipt, err = e.workspaces.CommitTaskWorkspacePublish(ctx, command, files, manifestSHA, func(ctx context.Context, digest string) ([]byte, error) {
		block, err := session.ReadTaskWorkspaceBlock(ctx, current, group.bindingVersion, group.viewID, manifestSHA, digest)
		if err != nil {
			return nil, err
		}
		return block.Data, nil
	})
	if err != nil {
		return taskWorkspacePublishErrorVerification(err)
	}
	return e.verifyPublishedTaskWorkspaceArtifacts(ctx, session, current, group, receipt, &result)
}

func taskWorkspacePublishOperationKey(stepID, workspaceID string) string {
	if !domain.ValidStepID(stepID) || !domain.ValidWorkspaceID(workspaceID) {
		return ""
	}
	return "task-workspace-publish:" + stepID + ":" + workspaceID
}

func taskWorkspaceOutputManifest(ctx context.Context, session *RuntimeSession, current task.Task, group runtimeFileEvidenceGroup) ([]workspace.File, string, error) {
	files := make([]workspace.File, 0)
	afterPath, manifestSHA := "", ""
	for {
		page, err := session.TaskWorkspaceManifestPage(ctx, current, group.bindingVersion, group.viewID, manifestSHA, afterPath, 1000)
		if err != nil {
			return nil, "", err
		}
		if page.TaskID != current.ID || page.WorkspaceID != group.workspaceID || page.BindingVersion != group.bindingVersion ||
			page.InputRevision != *current.InputRevision || page.WorkspaceCheckpointRevision != *current.WorkspaceCheckpointRevision ||
			page.InputManifestSHA256 != hex.EncodeToString(current.ContextDigest) || page.ViewID != group.viewID ||
			(manifestSHA != "" && page.ManifestSHA256 != manifestSHA) || (page.NextCursor != "" && page.NextCursor <= afterPath) {
			return nil, "", domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace output manifest changed during export")
		}
		if manifestSHA == "" {
			manifestSHA = page.ManifestSHA256
		}
		for _, item := range page.Files {
			chunks := make([]workspace.Chunk, len(item.Chunks))
			for index, chunk := range item.Chunks {
				chunks[index] = workspace.Chunk{SHA256: chunk.SHA256, SizeBytes: chunk.SizeBytes}
			}
			files = append(files, workspace.File{Path: item.Path, SizeBytes: item.SizeBytes, SHA256: item.SHA256, Mode: item.Mode, Chunks: chunks})
		}
		if page.NextCursor == "" {
			break
		}
		afterPath = page.NextCursor
	}
	actualManifestSHA, err := workspace.ManifestSHA256(files)
	if err != nil || manifestSHA == "" || actualManifestSHA != manifestSHA {
		return nil, "", domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace output manifest failed integrity verification")
	}
	return files, manifestSHA, nil
}

func (e *RuntimeToolExecutor) verifyPublishedTaskWorkspaceArtifacts(ctx context.Context, session *RuntimeSession, current task.Task, group runtimeFileEvidenceGroup, receipt TaskWorkspacePublishReceipt, result *AgentVerificationResult) AgentVerificationResult {
	if receipt.State != TaskWorkspacePublishSucceeded || receipt.WorkspaceID != group.workspaceID || receipt.PublishedRevision < 0 ||
		!validSyncDigest(receipt.ManifestSHA256) || current.WorkspaceCheckpointRevision == nil {
		return unknownAgentVerification("The committed Task Workspace publish receipt is invalid.")
	}
	if *current.WorkspaceCheckpointRevision > receipt.PublishedRevision {
		return unknownAgentVerification("The Task Workspace publish receipt is older than the current Task checkpoint.")
	}
	updated := current
	checkpoint := receipt.PublishedRevision
	updated.WorkspaceCheckpointRevision = &checkpoint
	if err := session.AdvanceTaskWorkspaceCheckpoint(ctx, updated, group.bindingVersion); err != nil {
		return unknownAgentVerification("The Runtime could not confirm the committed Task Workspace checkpoint.")
	}
	if result == nil {
		artifacts := make([]AgentVerifiedArtifact, 0, len(group.files))
		for _, file := range group.files {
			mediaType := mime.TypeByExtension(path.Ext(file.Path))
			if mediaType == "" {
				mediaType = "application/octet-stream"
			}
			artifacts = append(artifacts, AgentVerifiedArtifact{
				Label: path.Base(file.Path), Path: file.Path, MediaType: mediaType,
				WorkspaceID: group.workspaceID, SizeBytes: file.Size, SHA256: file.SHA256,
			})
		}
		result = &AgentVerificationResult{Status: AgentVerified, Artifacts: artifacts}
	}
	for index := range result.Artifacts {
		artifact := &result.Artifacts[index]
		if artifact.WorkspaceID != group.workspaceID {
			return AgentVerificationResult{Status: AgentNotVerified, Summary: "The verified file is outside the Task Workspace."}
		}
		proof, err := e.workspaces.VerifyCommittedArtifact(ctx, TaskArtifactProof{
			OwnerID: current.OwnerID, WorkspaceID: group.workspaceID, Revision: receipt.PublishedRevision,
			Path: artifact.Path, SizeBytes: artifact.SizeBytes, SHA256: artifact.SHA256,
		})
		if err != nil {
			return unknownAgentVerification("The committed Morde Workspace revision or stored file blocks could not be verified.")
		}
		artifact.Revision = proof.Revision
	}
	result.Summary = fmt.Sprintf("Published and verified %d file(s) in Morde Workspace revision %d.", len(result.Artifacts), receipt.PublishedRevision)
	return *result
}

func workspaceInputChangedVerification() AgentVerificationResult {
	return AgentVerificationResult{Status: AgentVerifyUnknown, WaitingReason: "workspace_input_changed", Summary: "Workspace changed after this task was submitted."}
}

func workspacePublishQuotaVerification() AgentVerificationResult {
	return AgentVerificationResult{
		Status: AgentNotVerified, Summary: "The output could not be published because the Workspace quota was exceeded.",
		Observation: "The Workspace storage quota rejected this output. Reduce the files to publish or remove unneeded files, then try again.",
	}
}

func taskWorkspacePublishErrorVerification(err error) AgentVerificationResult {
	switch domain.CodeOf(err) {
	case domain.CodeWorkspaceConflict:
		return workspaceInputChangedVerification()
	case domain.CodeQuotaExceeded, domain.CodeRequestTooLarge:
		return workspacePublishQuotaVerification()
	default:
		return unknownAgentVerification("The Task Workspace output could not be safely published.")
	}
}

func validRuntimeJSONObject(raw []byte, limit int) bool {
	return validAgentJSONObject(raw, limit)
}

func (e *RuntimeToolExecutor) currentTask(ctx context.Context, claimed task.Task) (task.Task, error) {
	return e.loadCurrentTask(ctx, claimed, true)
}

func (e *RuntimeToolExecutor) currentTaskForOperationReconciliation(ctx context.Context, claimed task.Task) (task.Task, error) {
	return e.loadCurrentTask(ctx, claimed, false)
}

func (e *RuntimeToolExecutor) loadCurrentTask(ctx context.Context, claimed task.Task, checkToolBudget bool) (task.Task, error) {
	if ctx == nil || !domain.ValidTaskID(claimed.ID) || !domain.ValidUserID(claimed.OwnerID) || !domain.ValidHostID(claimed.HostID) {
		return task.Task{}, domain.NewError(domain.CodeInvalidArgument, "runtime task is invalid")
	}
	details, err := e.store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil {
		return task.Task{}, err
	}
	current := details.Task
	if current.ID != claimed.ID || current.OwnerID != claimed.OwnerID || current.HostID != claimed.HostID ||
		current.Status != task.StatusRunning || current.Lease.Owner == "" || current.Lease.Version < 1 ||
		current.Lease.Owner != claimed.Lease.Owner || current.Lease.Version != claimed.Lease.Version ||
		current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) {
		return task.Task{}, domain.NewError(domain.CodeInvalidState, "task lease or Host binding changed")
	}
	if err := checkRuntimeNonToolBudget(current); err != nil {
		return task.Task{}, err
	}
	if current.Usage.ToolCalls < 0 || (checkToolBudget && current.Usage.ToolCalls > current.Budget.ToolCalls) {
		return task.Task{}, domain.NewError(domain.CodeQuotaExceeded, "task execution budget is exhausted")
	}
	return current, nil
}

func checkRuntimeToolBudget(current task.Task) error {
	if err := checkRuntimeNonToolBudget(current); err != nil {
		return err
	}
	if current.Usage.ToolCalls < 0 || current.Usage.ToolCalls > current.Budget.ToolCalls {
		return domain.NewError(domain.CodeQuotaExceeded, "task execution budget is exhausted")
	}
	return nil
}

func checkRuntimeNonToolBudget(current task.Task) error {
	if current.Budget.Validate() != nil || current.Usage.ActiveSeconds < 0 || current.Usage.ActiveSeconds >= current.Budget.ActiveSeconds ||
		(current.Usage.ModelTokens != nil && (*current.Usage.ModelTokens < 0 || *current.Usage.ModelTokens > current.Budget.ModelTokens)) {
		return domain.NewError(domain.CodeQuotaExceeded, "task execution budget is exhausted")
	}
	return nil
}

func (e *RuntimeToolExecutor) authorize(ctx context.Context, current task.Task, scope identity.Scope) error {
	if scope != identity.ScopeComputerObserve && scope != identity.ScopeComputerOperate {
		return domain.NewError(domain.CodeForbidden, "task authorization does not allow this tool")
	}
	return e.authorizer.AuthorizeTaskAction(ctx, current.OwnerID, identity.Kind(current.Authorization.Kind), current.Authorization.TokenID,
		current.HostID, agentScopeSnapshot(current.Authorization.Scopes), scope)
}

func (e *RuntimeToolExecutor) readFence(ctx context.Context, current task.Task, expected *AgentHostFence) (AgentHostFence, error) {
	fence, err := e.fences.ReadTaskFence(ctx, current.OwnerID, current.HostID)
	if err != nil || !fence.AIAllowed || fence.RuntimeGeneration < 1 || fence.ControlVersion < 0 ||
		(expected != nil && (fence.RuntimeGeneration != expected.RuntimeGeneration || fence.ControlVersion != expected.ControlVersion ||
			(expected.RuntimeInstanceID != "" && fence.RuntimeInstanceID != expected.RuntimeInstanceID))) {
		return AgentHostFence{}, domain.NewError(domain.CodeInvalidState, "Host control fence changed")
	}
	return fence, nil
}

func (e *RuntimeToolExecutor) connect(ctx context.Context, current task.Task, expected AgentHostFence) (*RuntimeSession, error) {
	session, err := e.runtime.Connect(ctx, current.HostID, &expected)
	if err != nil {
		return nil, err
	}
	if session.Mode != runtimeclient.ModeAI || session.Fence.RuntimeGeneration != expected.RuntimeGeneration ||
		session.Fence.ControlVersion != expected.ControlVersion ||
		(expected.RuntimeInstanceID != "" && session.Fence.RuntimeInstanceID != expected.RuntimeInstanceID) {
		session.Close()
		return nil, domain.NewError(domain.CodeInvalidState, "Host runtime fence changed")
	}
	return session, nil
}

func (e *RuntimeToolExecutor) validatePersistedOperation(ctx context.Context, current task.Task, request AgentToolRequest, fence AgentHostFence, preparation AgentToolPreparation) error {
	if request.OperationKey == "" || len(request.OperationKey) > 255 {
		return domain.NewError(domain.CodeInvalidArgument, "runtime operation key is invalid")
	}
	operation, err := e.store.GetOperation(ctx, current.OwnerID, current.ID, request.OperationKey)
	if err != nil {
		return err
	}
	var inputRevision *int64
	if preparation.WorkspaceID != "" {
		inputRevision = current.InputRevision
	}
	hash, err := agentOperationHash(request.Call.Name, request.Arguments)
	if err != nil {
		return domain.NewError(domain.CodeInvalidArgument, "runtime operation request is invalid")
	}
	if operation.ID != request.OperationID || operation.TaskID != current.ID || operation.Key != request.OperationKey ||
		operation.HostID != current.HostID || operation.RuntimeGeneration != fence.RuntimeGeneration ||
		operation.RuntimeInstanceID != fence.RuntimeInstanceID || operation.ControlVersion != fence.ControlVersion ||
		operation.RuntimeLeaseVersion != current.Lease.Version || operation.LeaseVersion != current.Lease.Version ||
		operation.WorkspaceID != preparation.WorkspaceID || operation.BindingVersion != preparation.BindingVersion ||
		!sameTaskInputRevision(operation.InputRevision, inputRevision) ||
		!sameTaskInputRevision(operation.WorkspaceCheckpointRevision, current.WorkspaceCheckpointRevision) || operation.Status != task.OperationDispatched ||
		!bytes.Equal(operation.RequestHash, hash[:]) {
		return domain.NewError(domain.CodeInvalidState, "persisted operation fence changed")
	}
	return nil
}

func mapRuntimeToolResult(mapping runtimeToolMapping, operation runtimeclient.Operation, arguments json.RawMessage) (AgentToolResult, error) {
	result := AgentToolResult{ExternalID: operation.OperationID}
	switch operation.Status {
	case runtimeclient.OperationSucceeded:
		if !validRuntimeJSONResult(operation.Result) {
			return AgentToolResult{Status: AgentToolFailed, ExternalID: operation.OperationID, Output: `{"error":"invalid_runtime_result"}`}, nil
		}
		if strings.HasPrefix(mapping.capability, "browser.") {
			return mapBrowserToolResult(mapping, operation, arguments)
		}
		if strings.HasPrefix(mapping.capability, "desktop.") || strings.HasPrefix(mapping.capability, "window.") {
			return mapDesktopToolResult(mapping, operation, arguments)
		}
		if mapping.capability == "shell.exec" {
			var shell struct {
				ExitCode        int    `json:"exit_code"`
				TimedOut        bool   `json:"timed_out"`
				Stdout          string `json:"stdout"`
				Stderr          string `json:"stderr"`
				StdoutTruncated bool   `json:"stdout_truncated"`
				StderrTruncated bool   `json:"stderr_truncated"`
			}
			if err := decodeStrictRuntimeResult(operation.Result, &shell); err != nil || shell.ExitCode < 0 || shell.ExitCode > 255 {
				return AgentToolResult{Status: AgentToolFailed, ExternalID: operation.OperationID, Output: `{"error":"invalid_shell_result"}`}, nil
			}
			encoded, err := json.Marshal(shell)
			if err != nil {
				return AgentToolResult{Status: AgentToolFailed, ExternalID: operation.OperationID, Output: `{"error":"invalid_shell_result"}`}, nil
			}
			result.Output = boundedToolOutput(string(encoded))
			if shell.ExitCode != 0 || shell.TimedOut {
				result.Status = AgentToolFailed
			} else {
				result.Status = AgentToolSucceeded
			}
			return result, nil
		}
		result.Status = AgentToolSucceeded
		result.Output = boundedToolOutput(string(operation.Result))
		return result, nil
	case runtimeclient.OperationFailed:
		result.Status = AgentToolFailed
		result.Output = `{"error":"runtime_operation_failed"}`
		return result, nil
	case runtimeclient.OperationCancelled:
		// Cancellation proves the handler stopped, not that a side effect was
		// absent or reversible. Keep the task paused for reconciliation.
		result.Status = AgentToolUnknown
		return result, nil
	case runtimeclient.OperationUnknown, runtimeclient.OperationAccepted, runtimeclient.OperationRunning, runtimeclient.OperationCancelling:
		result.Status = AgentToolUnknown
		return result, nil
	default:
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
}

type browserToolTargetResult struct {
	TargetID string `json:"target_id"`
	URL      string `json:"url"`
	Title    string `json:"title"`
}

func mapBrowserToolResult(mapping runtimeToolMapping, operation runtimeclient.Operation, arguments json.RawMessage) (AgentToolResult, error) {
	invalid := func(code string) (AgentToolResult, error) {
		return AgentToolResult{Status: AgentToolFailed, ExternalID: operation.OperationID, Output: `{"error":"` + code + `"}`}, nil
	}
	result := AgentToolResult{Status: AgentToolSucceeded, ExternalID: operation.OperationID}
	switch mapping.name {
	case runtimeToolBrowserOpenTab, runtimeToolBrowserNavigate:
		var value browserToolTargetResult
		if decodeStrictRuntimeResult(operation.Result, &value) != nil || !validBrowserTargetID(value.TargetID) ||
			!browserResultTargetMatches(mapping.name, arguments, value.TargetID) || !validBrowserURL(value.URL) || len(value.Title) > 512 {
			return invalid("invalid_browser_result")
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return invalid("invalid_browser_result")
		}
		result.Output = string(encoded)
	case runtimeToolBrowserSnapshot:
		var value struct {
			TargetID string `json:"target_id"`
			URL      string `json:"url"`
			Title    string `json:"title"`
			Text     string `json:"text"`
		}
		if decodeStrictRuntimeResult(operation.Result, &value) != nil || !validBrowserTargetID(value.TargetID) ||
			!browserResultTargetMatches(mapping.name, arguments, value.TargetID) || !validBrowserURL(value.URL) || len(value.Title) > 512 || len(value.Text) > maxBrowserSnapshotTextBytes || !utf8.ValidString(value.Text) {
			return invalid("invalid_browser_snapshot")
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return invalid("invalid_browser_snapshot")
		}
		result.Output = boundedToolOutput(string(encoded))
	case runtimeToolBrowserObserveField:
		var value struct {
			Fingerprint string `json:"fingerprint"`
			Origin      string `json:"origin"`
			MatchCount  int    `json:"match_count"`
		}
		if decodeStrictRuntimeResult(operation.Result, &value) != nil || !validAgentObservationFingerprint(value.Fingerprint) ||
			!validBrowserOrigin(value.Origin) || value.MatchCount != 1 {
			return invalid("invalid_browser_observation")
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return invalid("invalid_browser_observation")
		}
		result.Output = string(encoded)
	case runtimeToolBrowserClick:
		var value struct {
			TargetID string `json:"target_id"`
			Matched  bool   `json:"matched"`
		}
		if decodeStrictRuntimeResult(operation.Result, &value) != nil || !validBrowserTargetID(value.TargetID) ||
			!browserResultTargetMatches(mapping.name, arguments, value.TargetID) || !value.Matched {
			return invalid("browser_target_not_matched")
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return invalid("invalid_browser_result")
		}
		result.Output = string(encoded)
	case runtimeToolBrowserType:
		var value struct {
			TargetID   string `json:"target_id"`
			TypedChars int    `json:"typed_chars"`
		}
		if decodeStrictRuntimeResult(operation.Result, &value) != nil || !validBrowserTargetID(value.TargetID) ||
			!browserResultTargetMatches(mapping.name, arguments, value.TargetID) || value.TypedChars < 0 || value.TypedChars > 4096 {
			return invalid("invalid_browser_result")
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return invalid("invalid_browser_result")
		}
		result.Output = string(encoded)
	case runtimeToolBrowserPressKey:
		var value struct {
			TargetID   string `json:"target_id"`
			Dispatched bool   `json:"dispatched"`
		}
		if decodeStrictRuntimeResult(operation.Result, &value) != nil || !validBrowserTargetID(value.TargetID) ||
			!browserResultTargetMatches(mapping.name, arguments, value.TargetID) || !value.Dispatched {
			return invalid("invalid_browser_result")
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return invalid("invalid_browser_result")
		}
		result.Output = string(encoded)
	case runtimeToolBrowserScreenshot:
		var value struct {
			TargetID   string `json:"target_id"`
			MIMEType   string `json:"mime_type"`
			DataBase64 string `json:"data_base64"`
		}
		if decodeStrictRuntimeResult(operation.Result, &value) != nil || !validBrowserTargetID(value.TargetID) ||
			!browserResultTargetMatches(mapping.name, arguments, value.TargetID) || value.MIMEType != "image/jpeg" || len(value.DataBase64) == 0 || len(value.DataBase64) > 700<<10 {
			return invalid("invalid_browser_screenshot")
		}
		data, err := base64.StdEncoding.DecodeString(value.DataBase64)
		if err != nil || len(data) < 4 || len(data) > 512<<10 || !bytes.HasPrefix(data, []byte{0xff, 0xd8}) || !bytes.HasSuffix(data, []byte{0xff, 0xd9}) {
			return invalid("invalid_browser_screenshot")
		}
		result.Images = []ModelImage{{MediaType: value.MIMEType, Data: data}}
		result.Output = `{"target_id":"` + value.TargetID + `","mime_type":"image/jpeg"}`
	default:
		return invalid("unsupported_browser_tool")
	}
	return result, nil
}

func browserResultTargetMatches(toolName string, arguments json.RawMessage, targetID string) bool {
	if toolName == runtimeToolBrowserOpenTab {
		return true
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(arguments, &request) != nil {
		return false
	}
	var requestedTargetID string
	return json.Unmarshal(request["target_id"], &requestedTargetID) == nil && requestedTargetID == targetID && validBrowserTargetID(requestedTargetID)
}

func validBrowserTargetID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9', character == '_', character == '-':
		default:
			return false
		}
	}
	return true
}

func validBrowserURL(value string) bool {
	if value == "" || len(value) > 2048 || strings.TrimSpace(value) != value {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	return scheme == "http" || scheme == "https"
}

func validBrowserOrigin(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed != nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Hostname() != "" &&
		parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.String() == value
}

type runtimeDesktopWindow struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Active bool   `json:"active"`
}

func mapDesktopToolResult(mapping runtimeToolMapping, operation runtimeclient.Operation, arguments json.RawMessage) (AgentToolResult, error) {
	invalid := func(code string) (AgentToolResult, error) {
		return AgentToolResult{Status: AgentToolFailed, ExternalID: operation.OperationID, Output: `{"error":"` + code + `"}`}, nil
	}
	result := AgentToolResult{Status: AgentToolSucceeded, ExternalID: operation.OperationID}
	switch mapping.name {
	case runtimeToolDesktopScreenshot:
		var value struct {
			MIMEType   string `json:"mime_type"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			DataBase64 string `json:"data_base64"`
		}
		if decodeStrictRuntimeResult(operation.Result, &value) != nil || value.MIMEType != "image/jpeg" ||
			value.Width < 1 || value.Width > maxDesktopScreenshotWidth || value.Height < 1 || value.Height > maxDesktopScreenshotHeight ||
			len(value.DataBase64) == 0 || len(value.DataBase64) > 700<<10 {
			return invalid("invalid_desktop_screenshot")
		}
		data, err := base64.StdEncoding.DecodeString(value.DataBase64)
		if err != nil || len(data) < 4 || len(data) > maxDesktopScreenshotBytes || !bytes.HasPrefix(data, []byte{0xff, 0xd8}) || !bytes.HasSuffix(data, []byte{0xff, 0xd9}) {
			return invalid("invalid_desktop_screenshot")
		}
		config, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil || config.Width != value.Width || config.Height != value.Height {
			return invalid("invalid_desktop_screenshot")
		}
		result.Images = []ModelImage{{MediaType: value.MIMEType, Data: data}}
		result.Output = fmt.Sprintf(`{"mime_type":"image/jpeg","width":%d,"height":%d}`, value.Width, value.Height)
	case runtimeToolWindowList:
		var value struct {
			Windows []runtimeDesktopWindow `json:"windows"`
		}
		if decodeStrictRuntimeResult(operation.Result, &value) != nil || len(value.Windows) > maxRuntimeWindows {
			return invalid("invalid_window_list")
		}
		seen := make(map[string]struct{}, len(value.Windows))
		for _, window := range value.Windows {
			if !validDesktopWindowID(window.ID) || len(window.Title) > 512 || !utf8.ValidString(window.Title) ||
				window.X < -65535 || window.X > 65535 || window.Y < -65535 || window.Y > 65535 ||
				window.Width < 1 || window.Width > 65535 || window.Height < 1 || window.Height > 65535 {
				return invalid("invalid_window_list")
			}
			if _, duplicate := seen[window.ID]; duplicate {
				return invalid("invalid_window_list")
			}
			seen[window.ID] = struct{}{}
		}
		encoded, err := json.Marshal(value)
		if err != nil || len(encoded) > maxRuntimeToolResultBytes {
			return invalid("invalid_window_list")
		}
		result.Output = string(encoded)
	case runtimeToolWindowActivate:
		var request struct {
			WindowID string `json:"window_id"`
		}
		var value struct {
			WindowID  string `json:"window_id"`
			Activated bool   `json:"activated"`
		}
		if json.Unmarshal(arguments, &request) != nil || !validDesktopWindowID(request.WindowID) ||
			decodeStrictRuntimeResult(operation.Result, &value) != nil || value.WindowID != request.WindowID || !value.Activated {
			return invalid("window_activation_failed")
		}
		result.Output = `{"window_id":"` + value.WindowID + `","activated":true}`
	case runtimeToolDesktopClick:
		var request struct {
			WindowID string `json:"window_id"`
			X        int    `json:"x"`
			Y        int    `json:"y"`
			Button   string `json:"button"`
		}
		var value struct {
			WindowID string `json:"window_id"`
			X        int    `json:"x"`
			Y        int    `json:"y"`
			Button   string `json:"button"`
			Clicked  bool   `json:"clicked"`
		}
		if json.Unmarshal(arguments, &request) != nil || !validDesktopWindowID(request.WindowID) || decodeStrictRuntimeResult(operation.Result, &value) != nil {
			return invalid("desktop_click_failed")
		}
		if request.Button == "" {
			request.Button = "left"
		}
		if value.WindowID != request.WindowID || value.X != request.X || value.Y != request.Y || value.Button != request.Button || !value.Clicked {
			return invalid("desktop_click_failed")
		}
		encoded, _ := json.Marshal(value)
		result.Output = string(encoded)
	case runtimeToolDesktopType:
		var request struct {
			WindowID string `json:"window_id"`
			Text     string `json:"text"`
		}
		var value struct {
			WindowID   string `json:"window_id"`
			TypedChars int    `json:"typed_chars"`
		}
		if json.Unmarshal(arguments, &request) != nil || !validDesktopWindowID(request.WindowID) || !utf8.ValidString(request.Text) || len(request.Text) > 4096 ||
			decodeStrictRuntimeResult(operation.Result, &value) != nil || value.WindowID != request.WindowID || value.TypedChars != utf8.RuneCountInString(request.Text) {
			return invalid("desktop_type_failed")
		}
		encoded, _ := json.Marshal(value)
		result.Output = string(encoded)
	case runtimeToolDesktopPressKey:
		var request struct {
			WindowID string `json:"window_id"`
			Key      string `json:"key"`
		}
		var value struct {
			WindowID string `json:"window_id"`
			Key      string `json:"key"`
			Pressed  bool   `json:"pressed"`
		}
		if json.Unmarshal(arguments, &request) != nil || !validDesktopWindowID(request.WindowID) || !validDesktopKey(request.Key) ||
			decodeStrictRuntimeResult(operation.Result, &value) != nil || value.WindowID != request.WindowID || value.Key != request.Key || !value.Pressed {
			return invalid("desktop_key_failed")
		}
		encoded, _ := json.Marshal(value)
		result.Output = string(encoded)
	default:
		return invalid("unsupported_desktop_tool")
	}
	return result, nil
}

func validDesktopWindowID(value string) bool {
	if len(value) < 3 || len(value) > 10 || !strings.HasPrefix(value, "0x") || strings.ToLower(value) != value {
		return false
	}
	parsed, err := strconv.ParseUint(value[2:], 16, 32)
	return err == nil && parsed != 0 && fmt.Sprintf("0x%x", parsed) == value
}

func validDesktopKey(value string) bool {
	switch value {
	case "Enter", "Tab", "Escape", "BackSpace", "Delete", "Home", "End", "Page_Up", "Page_Down", "ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight":
		return true
	default:
		return false
	}
}

func decodeStrictRuntimeResult(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("runtime result has trailing JSON")
	}
	return nil
}

func validRuntimeJSONResult(raw []byte) bool {
	return len(raw) > 0 && len(raw) <= maxRuntimeToolResultBytes && validAgentJSONObject(raw, maxRuntimeToolResultBytes)
}

func runtimeToolMappingByName(name string) (runtimeToolMapping, bool) {
	for _, mapping := range runtimeToolMappings {
		if mapping.name == name {
			return mapping, true
		}
	}
	return runtimeToolMapping{}, false
}

func runtimeToolPreparationToken(request AgentToolRequest, fence AgentHostFence, bindingVersion ...int64) string {
	argumentsHash := sha256.Sum256(request.Arguments)
	version := int64(0)
	if len(bindingVersion) == 1 {
		version = bindingVersion[0]
	}
	value := struct {
		TaskID         string                     `json:"task_id"`
		OwnerID        string                     `json:"owner_id"`
		HostID         string                     `json:"host_id"`
		LeaseOwner     string                     `json:"lease_owner"`
		LeaseVersion   int64                      `json:"lease_version"`
		OperationKey   string                     `json:"operation_key"`
		CallID         string                     `json:"call_id"`
		Tool           string                     `json:"tool"`
		ArgumentsHash  string                     `json:"arguments_hash"`
		Scope          identity.Scope             `json:"scope"`
		Fence          AgentHostFence             `json:"fence"`
		WorkspaceID    string                     `json:"workspace_id"`
		BindingVersion int64                      `json:"binding_version"`
		Approval       *AgentToolApprovalDecision `json:"approval,omitempty"`
	}{
		TaskID: request.Task.ID, OwnerID: request.Task.OwnerID, HostID: request.Task.HostID,
		LeaseOwner: request.Task.Lease.Owner, LeaseVersion: request.Task.Lease.Version,
		OperationKey: request.OperationKey, CallID: request.Call.CallID,
		Tool: request.Call.Name, ArgumentsHash: hex.EncodeToString(argumentsHash[:]),
		Scope: request.RequiredScope, Fence: fence, WorkspaceID: request.WorkspaceID, BindingVersion: version, Approval: request.Approval,
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func validRuntimeToolArguments(mapping runtimeToolMapping, raw json.RawMessage) bool {
	if !validAgentJSONObject(raw, maxAgentToolArgsBytes) {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	allowed := map[string]map[string]bool{
		runtimeToolShellExec:           {"workspace_id": true, "command": true, "cwd": true, "timeout": true},
		runtimeToolFileRead:            {"workspace_id": true, "path": true, "start_line": true, "end_line": true},
		runtimeToolFileWrite:           {"workspace_id": true, "path": true, "content": true},
		runtimeToolFileList:            {"workspace_id": true, "path": true},
		runtimeToolFileFind:            {"workspace_id": true, "path": true, "query": true},
		runtimeToolBrowserOpenTab:      {"url": true},
		runtimeToolBrowserNavigate:     {"target_id": true, "url": true},
		runtimeToolBrowserSnapshot:     {"target_id": true},
		runtimeToolBrowserObserveField: {"url": true, "selector": true, "field": true},
		runtimeToolBrowserClick:        {"target_id": true, "role": true, "name": true},
		runtimeToolBrowserType:         {"target_id": true, "role": true, "name": true, "text": true},
		runtimeToolBrowserPressKey:     {"target_id": true, "key": true},
		runtimeToolBrowserScreenshot:   {"target_id": true},
		runtimeToolDesktopScreenshot:   {},
		runtimeToolWindowList:          {},
		runtimeToolWindowActivate:      {"window_id": true},
		runtimeToolDesktopClick:        {"window_id": true, "x": true, "y": true, "button": true},
		runtimeToolDesktopType:         {"window_id": true, "text": true},
		runtimeToolDesktopPressKey:     {"window_id": true, "key": true},
	}
	accepted, exists := allowed[mapping.name]
	if !exists {
		return false
	}
	for key, value := range fields {
		if !accepted[key] || len(value) == 0 {
			return false
		}
	}
	if runtimeclient.WorkspaceScopedCapability(mapping.capability) && !validRuntimeWorkspaceID(runtimeToolWorkspaceID(mapping, raw)) {
		return false
	}
	stringValue := func(key string, required bool) bool {
		value, exists := fields[key]
		if !exists {
			return !required
		}
		var parsed string
		return json.Unmarshal(value, &parsed) == nil && (!required || strings.TrimSpace(parsed) != "")
	}
	intValue := func(key string) bool {
		value, exists := fields[key]
		if !exists {
			return true
		}
		var parsed int64
		return json.Unmarshal(value, &parsed) == nil && parsed >= 1
	}
	nonnegativeIntValue := func(key string, maximum int64) bool {
		value, exists := fields[key]
		if !exists {
			return false
		}
		var parsed int64
		return json.Unmarshal(value, &parsed) == nil && parsed >= 0 && parsed <= maximum
	}
	switch mapping.name {
	case runtimeToolShellExec:
		if !stringValue("command", true) || !stringValue("cwd", false) || !intValue("timeout") {
			return false
		}
		if value, exists := fields["timeout"]; exists {
			var timeout int64
			if json.Unmarshal(value, &timeout) != nil || timeout > 300 {
				return false
			}
		}
	case runtimeToolFileRead:
		if !stringValue("path", true) || !intValue("start_line") || !intValue("end_line") {
			return false
		}
		var startLine, endLine int64
		if value, exists := fields["start_line"]; exists {
			_ = json.Unmarshal(value, &startLine)
		}
		if value, exists := fields["end_line"]; exists {
			_ = json.Unmarshal(value, &endLine)
		}
		if startLine > 0 && endLine > 0 && endLine < startLine {
			return false
		}
	case runtimeToolFileWrite:
		if !stringValue("path", true) || !stringValue("content", false) {
			return false
		}
	case runtimeToolFileList:
		if !stringValue("path", false) {
			return false
		}
	case runtimeToolFileFind:
		if !stringValue("path", false) || !stringValue("query", true) {
			return false
		}
	case runtimeToolBrowserOpenTab:
		var browserURL string
		if !stringValue("url", true) || json.Unmarshal(fields["url"], &browserURL) != nil || !validBrowserURL(browserURL) {
			return false
		}
	case runtimeToolBrowserNavigate:
		var targetID, browserURL string
		if !stringValue("target_id", true) || json.Unmarshal(fields["target_id"], &targetID) != nil || !validBrowserTargetID(targetID) ||
			!stringValue("url", true) || json.Unmarshal(fields["url"], &browserURL) != nil || !validBrowserURL(browserURL) {
			return false
		}
	case runtimeToolBrowserObserveField:
		var observe struct {
			URL      string `json:"url"`
			Selector string `json:"selector"`
			Field    string `json:"field"`
		}
		if decodeStrictRuntimeResult(raw, &observe) != nil || !validBrowserURL(observe.URL) || len(observe.Selector) == 0 ||
			len(observe.Selector) > 1024 || !utf8.ValidString(observe.Selector) || observe.Field != "text" && observe.Field != "value" {
			return false
		}
	case runtimeToolBrowserSnapshot, runtimeToolBrowserScreenshot:
		var targetID string
		if !stringValue("target_id", true) || json.Unmarshal(fields["target_id"], &targetID) != nil || !validBrowserTargetID(targetID) {
			return false
		}
	case runtimeToolBrowserClick:
		var targetID, role, name string
		if !stringValue("target_id", true) || json.Unmarshal(fields["target_id"], &targetID) != nil || !validBrowserTargetID(targetID) ||
			!stringValue("role", true) || json.Unmarshal(fields["role"], &role) != nil || !validBrowserRole(role) ||
			!stringValue("name", true) || json.Unmarshal(fields["name"], &name) != nil || len(name) > 512 {
			return false
		}
	case runtimeToolBrowserType:
		var targetID, role, name, value string
		textRaw, textPresent := fields["text"]
		if !stringValue("target_id", true) || json.Unmarshal(fields["target_id"], &targetID) != nil || !validBrowserTargetID(targetID) ||
			!stringValue("role", true) || json.Unmarshal(fields["role"], &role) != nil || !validBrowserRole(role) ||
			!stringValue("name", true) || json.Unmarshal(fields["name"], &name) != nil || len(name) > 512 ||
			!textPresent || json.Unmarshal(textRaw, &value) != nil || len(value) > 4096 || !utf8.ValidString(value) {
			return false
		}
	case runtimeToolBrowserPressKey:
		var targetID, key string
		if !stringValue("target_id", true) || json.Unmarshal(fields["target_id"], &targetID) != nil || !validBrowserTargetID(targetID) ||
			!stringValue("key", true) || json.Unmarshal(fields["key"], &key) != nil || !validBrowserKey(key) {
			return false
		}
	case runtimeToolDesktopScreenshot, runtimeToolWindowList:
		// These observations intentionally take no arguments.
	case runtimeToolWindowActivate:
		var windowID string
		if !stringValue("window_id", true) || json.Unmarshal(fields["window_id"], &windowID) != nil || !validDesktopWindowID(windowID) {
			return false
		}
	case runtimeToolDesktopClick:
		var windowID string
		if !stringValue("window_id", true) || json.Unmarshal(fields["window_id"], &windowID) != nil || !validDesktopWindowID(windowID) ||
			!nonnegativeIntValue("x", 65535) || !nonnegativeIntValue("y", 65535) {
			return false
		}
		if value, exists := fields["button"]; exists {
			var button string
			if json.Unmarshal(value, &button) != nil || button != "left" && button != "middle" && button != "right" {
				return false
			}
		}
	case runtimeToolDesktopType:
		var windowID, value string
		if !stringValue("window_id", true) || json.Unmarshal(fields["window_id"], &windowID) != nil || !validDesktopWindowID(windowID) ||
			!stringValue("text", true) || json.Unmarshal(fields["text"], &value) != nil || len(value) > 4096 || !utf8.ValidString(value) {
			return false
		}
	case runtimeToolDesktopPressKey:
		var windowID, key string
		if !stringValue("window_id", true) || json.Unmarshal(fields["window_id"], &windowID) != nil || !validDesktopWindowID(windowID) ||
			!stringValue("key", true) || json.Unmarshal(fields["key"], &key) != nil || !validDesktopKey(key) {
			return false
		}
	}
	return true
}

func runtimeToolWorkspaceID(mapping runtimeToolMapping, raw json.RawMessage) string {
	if !runtimeclient.WorkspaceScopedCapability(mapping.capability) {
		return ""
	}
	var args struct {
		WorkspaceID string `json:"workspace_id"`
	}
	if json.Unmarshal(raw, &args) != nil || !domain.ValidWorkspaceID(args.WorkspaceID) {
		return ""
	}
	return args.WorkspaceID
}

func runtimeToolOperationWorkspaceID(mapping runtimeToolMapping, raw json.RawMessage, current task.Task) (string, error) {
	if runtimeclient.WorkspaceScopedCapability(mapping.capability) {
		workspaceID := runtimeToolWorkspaceID(mapping, raw)
		if workspaceID == "" {
			return "", domain.NewError(domain.CodeInvalidArgument, "runtime Workspace argument is invalid")
		}
		return workspaceID, nil
	}
	if current.WorkspaceID == "" && current.InputRevision == nil && current.WorkspaceCheckpointRevision == nil {
		return "", nil
	}
	if !domain.ValidWorkspaceID(current.WorkspaceID) || current.InputRevision == nil || current.WorkspaceCheckpointRevision == nil ||
		*current.InputRevision < 0 || *current.WorkspaceCheckpointRevision < *current.InputRevision {
		return "", domain.NewError(domain.CodeInvalidState, "task Workspace input binding is invalid")
	}
	return current.WorkspaceID, nil
}

func sameTaskInputRevision(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func runtimeToolExecutionArguments(mapping runtimeToolMapping, raw json.RawMessage) (json.RawMessage, error) {
	if !runtimeclient.WorkspaceScopedCapability(mapping.capability) {
		return append(json.RawMessage(nil), raw...), nil
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || fields["workspace_id"] == nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "runtime Workspace argument is invalid")
	}
	delete(fields, "workspace_id")
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "runtime Workspace argument is invalid")
	}
	return encoded, nil
}

func validRuntimeWorkspaceID(value string) bool { return domain.ValidWorkspaceID(value) }

func validBrowserRole(value string) bool {
	switch value {
	case "button", "link", "textbox", "searchbox", "checkbox", "radio", "option", "menuitem", "tab", "combobox", "switch":
		return true
	default:
		return false
	}
}

func validBrowserKey(value string) bool {
	switch value {
	case "Enter", "Tab", "Escape", "ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight":
		return true
	default:
		return false
	}
}
