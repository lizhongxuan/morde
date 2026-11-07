package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/runtimeclient"
)

// DeferredTaskTarget is the immutable user-selected target on the original
// routed message. It is only an intent: looking it up must not probe the Host
// or bind a Workspace snapshot.
type DeferredTaskTarget struct {
	HostID        string
	WorkspaceID   string
	ApplicationID string
	// The original selection belongs to a different service instance. Keep
	// ordinary conversation available, but do not use or disclose that target.
	Unavailable bool
}

// DeferredTaskTargetStore provides a fenced lookup and one-way binding when a
// tool actually needs the target. Legacy Tasks have no routed target record.
type DeferredTaskTargetStore interface {
	LoadDeferredTaskTarget(context.Context, task.Task) (DeferredTaskTarget, bool, error)
	BindDeferredTaskTarget(context.Context, task.Task, DeferredTaskTarget, *int64, []byte) (task.Task, error)
}

// A Workspace is a tool in its own right; selecting one must not require a
// running computer. The revision is frozen only when the tool is first used.
type DeferredTaskWorkspaceStore interface {
	BindDeferredTaskWorkspace(context.Context, task.Task, string, int64, []byte) (task.Task, error)
}

// ProjectTaskWorkspaceAccess rechecks the Task actor, explicit selection,
// project resource and the actor's independent Workspace permission.
type ProjectTaskWorkspaceAccess interface {
	ValidateProjectTaskWorkspaceAccess(context.Context, task.Task, string, bool) (string, error)
}

// ProjectTaskWorkspaceSelection holds the original actor's authorization
// across the first manifest read and the one-way Task binding.
type ProjectTaskWorkspaceSelection interface {
	WithProjectTaskWorkspaceSelection(context.Context, task.Task, DeferredTaskTarget, bool, func(context.Context, string) error) error
}

func deferredWorkspaceToolDefinitions(authorization task.Authorization) []AgentToolDefinition {
	definitions := cloudDSHWorkspaceToolDefinitions(-1)
	principal := identity.Principal{Kind: identity.Kind(authorization.Kind), Scopes: agentScopeSnapshot(authorization.Scopes)}
	filtered := definitions[:0]
	for i := range definitions {
		if !principal.HasScope(definitions[i].RequiredScope) {
			continue
		}
		definitions[i].Description = "Use the Workspace explicitly selected by the user. " +
			"On the first call set expected_revision to -1; the server fixes the current authorized snapshot. " +
			"Use the returned revision for later calls. Never invent an operation result. " + definitions[i].Description
		var schema map[string]any
		if json.Unmarshal(definitions[i].Parameters, &schema) != nil {
			continue
		}
		properties, _ := schema["properties"].(map[string]any)
		expected, _ := properties["expected_revision"].(map[string]any)
		if expected != nil {
			expected["minimum"] = -1
			definitions[i].Parameters, _ = json.Marshal(schema)
		}
		filtered = append(filtered, definitions[i])
	}
	return filtered
}

func (b *CloudDSHToolBroker) prepareDeferredWorkspaceCall(ctx context.Context, claimed task.Task,
	target DeferredTaskTarget, call ModelToolCall, recovering bool, checkpointOutput *string) (task.Task, ModelToolCall, string, error) {
	if (claimed.ProjectID == "" && claimed.OwnerID != claimed.InitiatorID) || target.WorkspaceID == "" || b.workspaceTools == nil {
		return task.Task{}, call, "workspace_unavailable", nil
	}
	requiredScope := identity.ScopeTaskRead
	if call.Name == cloudDSHWorkspaceWriteToolName {
		requiredScope = identity.ScopeTaskSubmit
	}
	principal := identity.Principal{Kind: identity.Kind(claimed.Authorization.Kind), Scopes: agentScopeSnapshot(claimed.Authorization.Scopes)}
	if !principal.HasScope(requiredScope) {
		return task.Task{}, call, "", domain.NewError(domain.CodeForbidden, "Workspace tool is not authorized")
	}
	if claimed.HostID != "" || (claimed.WorkspaceID != "" && claimed.WorkspaceID != target.WorkspaceID) {
		return task.Task{}, call, "workspace_target_changed", nil
	}
	arguments, err := validateAgentToolArguments(call.Arguments)
	if err != nil {
		return task.Task{}, call, "", domain.InvalidArgument("tool_call.arguments", "Workspace arguments are invalid")
	}
	var fields map[string]json.RawMessage
	var expected int64
	if json.Unmarshal(arguments, &fields) != nil || json.Unmarshal(fields["expected_revision"], &expected) != nil || expected < -1 {
		return task.Task{}, call, "", domain.InvalidArgument("tool_call.arguments", "Workspace revision is required")
	}
	workspaceOwner := claimed.InitiatorID
	if claimed.ProjectID != "" {
		access, ok := b.store.(ProjectTaskWorkspaceAccess)
		if !ok {
			return task.Task{}, call, "workspace_unavailable", nil
		}
		workspaceOwner, err = access.ValidateProjectTaskWorkspaceAccess(ctx, claimed, target.WorkspaceID, call.Name == cloudDSHWorkspaceWriteToolName)
		if err != nil {
			return task.Task{}, call, "workspace_unavailable", nil
		}
	}
	if claimed.WorkspaceID == "" {
		if recovering && checkpointOutput != nil {
			return task.Task{}, call, "target_result_unknown", nil
		}
		reader, ok := b.store.(taskWorkspaceSnapshotReader)
		if !ok {
			return task.Task{}, call, "workspace_unavailable", nil
		}
		store, ok := b.store.(DeferredTaskWorkspaceStore)
		if !ok {
			return task.Task{}, call, "workspace_unavailable", nil
		}
		var bound task.Task
		bind := func(bindingCtx context.Context, ownerID string) error {
			value, err := reader.GetWorkspace(bindingCtx, target.WorkspaceID)
			if err != nil || value.OwnerID != ownerID {
				return domain.NewError(domain.CodeForbidden, "Workspace selection is unavailable")
			}
			revision, digest, err := (&TaskService{store: b.store}).workspaceInputSnapshot(bindingCtx, ownerID,
				"", target.WorkspaceID, &value.Revision)
			if err != nil {
				return err
			}
			bound, err = store.BindDeferredTaskWorkspace(bindingCtx, claimed, target.WorkspaceID, *revision, digest)
			return err
		}
		if claimed.ProjectID != "" {
			selection, ok := b.store.(ProjectTaskWorkspaceSelection)
			if !ok {
				return task.Task{}, call, "workspace_unavailable", nil
			}
			err = selection.WithProjectTaskWorkspaceSelection(ctx, claimed, DeferredTaskTarget{WorkspaceID: target.WorkspaceID},
				call.Name == cloudDSHWorkspaceWriteToolName, bind)
		} else {
			err = bind(ctx, workspaceOwner)
		}
		if err != nil {
			if ctx.Err() != nil {
				return task.Task{}, call, "", ctx.Err()
			}
			return task.Task{}, call, "workspace_unavailable", nil
		}
		claimed = bound
	}
	if !cloudDSHWorkspaceToolsEligible(claimed) || claimed.WorkspaceID != target.WorkspaceID {
		return task.Task{}, call, "", domain.NewError(domain.CodeInvalidState, "Workspace binding did not match the task")
	}
	if expected == -1 {
		// Resolve -1 against the immutable input revision, including after a
		// restart. A later write cannot silently change an earlier call's hash.
		fields["expected_revision"], _ = json.Marshal(*claimed.InputRevision)
		resolved, err := json.Marshal(fields)
		if err != nil {
			return task.Task{}, call, "", err
		}
		call.Arguments = string(resolved)
	}
	return claimed, call, "", nil
}

// refreshDeferredTask accepts only the one-way target binding recorded against
// the original message and the exact current Agent generation and task lease.
func (b *CloudDSHToolBroker) refreshDeferredTask(ctx context.Context, claimed task.Task) (task.Task, DeferredTaskTarget, bool, error) {
	targets, ok := b.store.(DeferredTaskTargetStore)
	if !ok {
		return claimed, DeferredTaskTarget{}, false, nil
	}
	target, found, err := targets.LoadDeferredTaskTarget(ctx, claimed)
	if err != nil || !found {
		return claimed, target, found, err
	}
	details, err := b.store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil {
		return task.Task{}, target, true, err
	}
	current := details.Task
	if current.ID != claimed.ID || current.OwnerID != claimed.OwnerID || current.InitiatorID != claimed.InitiatorID ||
		current.ProjectID != claimed.ProjectID || current.ConversationID != claimed.ConversationID ||
		current.ModelID != claimed.ModelID || current.ExecutionLocation != claimed.ExecutionLocation ||
		current.AgentInstanceID != claimed.AgentInstanceID || current.AgentGeneration != claimed.AgentGeneration ||
		current.AgentSessionID != claimed.AgentSessionID || current.Lease.Owner != claimed.Lease.Owner ||
		current.Lease.Version != claimed.Lease.Version || !sameCloudDSHAuthorization(current.Authorization, claimed.Authorization) ||
		(!target.Unavailable && claimed.HostID == "" && current.HostID != "" && current.HostID != target.HostID) ||
		(!target.Unavailable && claimed.WorkspaceID == "" && current.WorkspaceID != "" && current.WorkspaceID != target.WorkspaceID) ||
		(claimed.HostID != "" && current.HostID != claimed.HostID) ||
		(claimed.WorkspaceID != "" && current.WorkspaceID != claimed.WorkspaceID) {
		return task.Task{}, target, true, domain.NewError(domain.CodeInvalidState, "deferred Task identity or target changed")
	}
	current.ConversationContext = claimed.ConversationContext
	return current, target, true, nil
}

func deferredRuntimeToolDefinitions(target DeferredTaskTarget, authorization task.Authorization) []AgentToolDefinition {
	principal := identity.Principal{Kind: identity.Kind(authorization.Kind), Scopes: agentScopeSnapshot(authorization.Scopes)}
	definitions := make([]AgentToolDefinition, 0, len(runtimeToolMappings))
	for _, mapping := range runtimeToolMappings {
		if mapping.internal || !principal.HasScope(mapping.scope) {
			continue
		}
		description := mapping.description + " The selected computer is checked only when this tool is called."
		if runtimeclient.WorkspaceScopedCapability(mapping.capability) && target.WorkspaceID != "" {
			description += " The user's selected workspace_id is " + target.WorkspaceID + "."
		}
		definitions = append(definitions, AgentToolDefinition{ModelTool: ModelTool{
			Name: mapping.name, Description: description, Parameters: append(json.RawMessage(nil), mapping.parameters...),
		}, RequiredScope: mapping.scope})
	}
	return definitions
}

func (b *CloudDSHToolBroker) pauseDeferredTarget(ctx context.Context, claimed task.Task, reason string) (AgentToolResult, error) {
	details, err := b.currentTaskDetails(ctx, claimed)
	if err != nil {
		return AgentToolResult{}, err
	}
	current := details.Task
	if current.Status != task.StatusRunning || current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "task is not running")
	}
	message := "所选云端电脑当前不可用或无法确认访问权限，本次操作未执行。"
	if reason == "computer_target_missing" {
		message = "尚未选择云端电脑，本次操作未执行。"
	} else if reason == "computer_target_changed" {
		message = "任务已绑定其他云端电脑，无法安全切换；请为新目标另发起任务。"
	} else if reason == "workspace_unavailable" {
		message = "所选 Workspace 当前不可用、版本未就绪或无法确认访问权限，本次操作未执行。"
	} else if reason == "workspace_target_changed" {
		message = "任务已绑定其他 Workspace，无法安全切换；请为新目标另发起任务。"
	} else if reason == "target_result_unknown" {
		message = "上次电脑操作的结果需要核对，暂不能重复执行。"
	}
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepWaitingUser, "电脑或工作区需要处理", message)
	if err != nil {
		return AgentToolResult{}, err
	}
	if err := b.commitProgress(ctx, current, step, current.Usage, task.StatusWaitingUser, reason, "task.waiting_user", map[string]any{
		"phase": "target_check", "message": message,
	}); err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolNeedsUser, WaitingReason: reason, Output: message}, nil
}

// bindDeferredRuntimeTarget performs no remote action. It freezes the target
// and Workspace snapshot only for an actual Host tool call.
func (b *CloudDSHToolBroker) bindDeferredRuntimeTarget(ctx context.Context, claimed task.Task,
	target DeferredTaskTarget, mapping runtimeToolMapping, arguments json.RawMessage) (task.Task, string, error) {
	if target.Unavailable {
		return task.Task{}, "computer_unavailable", nil
	}
	if target.HostID == "" {
		return task.Task{}, "computer_target_missing", nil
	}
	if claimed.HostID == "" && claimed.WorkspaceID != "" {
		// Moving a live hostless snapshot onto a computer requires a separate
		// reconciliation of prior Workspace operations and bindings.
		return task.Task{}, "computer_target_changed", nil
	}
	if claimed.HostID != "" && claimed.HostID != target.HostID {
		return task.Task{}, "computer_target_changed", nil
	}
	if !validRuntimeToolArguments(mapping, arguments) {
		return task.Task{}, "", domain.InvalidArgument("tool_call.arguments", "runtime tool arguments are invalid")
	}
	proposed := claimed
	proposed.HostID = target.HostID
	if err := b.runtime.authorize(ctx, proposed, mapping.scope); err != nil {
		return task.Task{}, "computer_unavailable", nil
	}
	if _, err := b.runtime.readFence(ctx, proposed, nil); err != nil {
		return task.Task{}, "computer_unavailable", nil
	}
	selected := DeferredTaskTarget{HostID: target.HostID, ApplicationID: target.ApplicationID}
	workspaceOwner := claimed.InitiatorID
	write := mapping.capability != "file.read" && mapping.capability != "file.list" && mapping.capability != "file.find"
	if runtimeclient.WorkspaceScopedCapability(mapping.capability) {
		if target.WorkspaceID == "" || runtimeToolWorkspaceID(mapping, arguments) != target.WorkspaceID {
			return task.Task{}, "workspace_unavailable", nil
		}
		if claimed.WorkspaceID != "" && claimed.WorkspaceID != target.WorkspaceID {
			return task.Task{}, "workspace_target_changed", nil
		}
		selected.WorkspaceID = target.WorkspaceID
		if claimed.ProjectID != "" {
			access, ok := b.store.(ProjectTaskWorkspaceAccess)
			if !ok {
				return task.Task{}, "workspace_unavailable", nil
			}
			var err error
			workspaceOwner, err = access.ValidateProjectTaskWorkspaceAccess(ctx, claimed, target.WorkspaceID, write)
			if err != nil {
				return task.Task{}, "workspace_unavailable", nil
			}
		}
	}
	store, ok := b.store.(DeferredTaskTargetStore)
	if !ok {
		return task.Task{}, "", fmt.Errorf("deferred target storage is unavailable")
	}
	var bound task.Task
	bind := func(bindingCtx context.Context, ownerID string) error {
		var revision *int64
		var digest []byte
		if selected.WorkspaceID != "" && claimed.WorkspaceID == target.WorkspaceID && claimed.InputRevision != nil {
			revision, digest = claimed.InputRevision, claimed.ContextDigest
		} else if selected.WorkspaceID != "" {
			reader, ok := b.store.(taskWorkspaceSnapshotReader)
			if !ok {
				return domain.NewError(domain.CodeForbidden, "Workspace snapshot is unavailable")
			}
			value, err := reader.GetWorkspace(bindingCtx, target.WorkspaceID)
			if err != nil || value.OwnerID != ownerID {
				return domain.NewError(domain.CodeForbidden, "Workspace selection is unavailable")
			}
			revision, digest, err = (&TaskService{store: b.store}).workspaceInputSnapshot(bindingCtx, ownerID,
				target.HostID, target.WorkspaceID, &value.Revision)
			if err != nil {
				return err
			}
		}
		var err error
		bound, err = store.BindDeferredTaskTarget(bindingCtx, claimed, selected, revision, digest)
		return err
	}
	var err error
	if claimed.ProjectID != "" && selected.WorkspaceID != "" {
		selection, ok := b.store.(ProjectTaskWorkspaceSelection)
		if !ok {
			return task.Task{}, "workspace_unavailable", nil
		}
		err = selection.WithProjectTaskWorkspaceSelection(ctx, claimed, selected, write, bind)
	} else {
		err = bind(ctx, workspaceOwner)
	}
	if err != nil {
		if ctx.Err() != nil {
			return task.Task{}, "", ctx.Err()
		}
		if domain.CodeOf(err) == domain.CodeWorkspaceConflict || domain.CodeOf(err) == domain.CodeWorkspaceBusy {
			return task.Task{}, "workspace_unavailable", nil
		}
		if domain.CodeOf(err) == domain.CodeConflict {
			return task.Task{}, "target_result_unknown", nil
		}
		if selected.WorkspaceID != "" {
			return task.Task{}, "workspace_unavailable", nil
		}
		return task.Task{}, "computer_unavailable", nil
	}
	if bound.HostID != target.HostID || bound.InitiatorID != claimed.InitiatorID ||
		bound.AgentInstanceID != claimed.AgentInstanceID || bound.AgentGeneration != claimed.AgentGeneration ||
		bound.Lease.Owner != claimed.Lease.Owner || bound.Lease.Version != claimed.Lease.Version {
		return task.Task{}, "", domain.NewError(domain.CodeInvalidState, "deferred target binding did not match the task")
	}
	return bound, "", nil
}
