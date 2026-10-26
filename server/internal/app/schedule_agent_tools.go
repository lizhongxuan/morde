package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

const (
	agentScheduleCreateTool = "schedule_create"
	agentScheduleUpdateTool = "schedule_update"
	agentScheduleListTool   = "schedule_list"
)

type ScheduleAgentToolExecutor struct {
	runtime   *RuntimeToolExecutor
	schedules *ScheduleService
}

func NewScheduleAgentToolExecutor(runtime *RuntimeToolExecutor, schedules *ScheduleService) (*ScheduleAgentToolExecutor, error) {
	if runtime == nil || schedules == nil {
		return nil, errors.New("schedule agent tool dependencies are required")
	}
	return &ScheduleAgentToolExecutor{runtime: runtime, schedules: schedules}, nil
}

func (e *ScheduleAgentToolExecutor) Definitions(ctx context.Context, claimed task.Task) ([]AgentToolDefinition, error) {
	definitions, err := e.runtime.Definitions(ctx, claimed)
	if err != nil {
		return nil, err
	}
	if identity.Kind(claimed.Authorization.Kind) == identity.KindSession || hasAgentScope(claimed.Authorization.Scopes, identity.ScopeScheduleManage) {
		definitions = append(definitions, AgentToolDefinition{ModelTool: ModelTool{Name: agentScheduleCreateTool,
			Description: "Create a schedule for this task's Host. Present its target, timezone, timing and budget for the user's approval before saving.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"host_id":{"type":"string"},"model_id":{"type":"string","minLength":1,"maxLength":256},"objective":{"type":"string","minLength":1,"maxLength":32768},"timing":{"type":"object","properties":{"kind":{"type":"string","enum":["once","daily","weekly"]},"run_at":{"type":"string"},"starts_on":{"type":"string"},"local_time":{"type":"string"},"time_zone":{"type":"string"},"weekdays":{"type":"array","items":{"type":"integer","minimum":1,"maximum":7}}},"required":["kind","time_zone"],"additionalProperties":false},"budget":{"type":"object","properties":{"active_seconds":{"type":"integer","minimum":1,"maximum":1800},"tool_calls":{"type":"integer","minimum":1,"maximum":100},"model_tokens":{"type":"integer","minimum":1,"maximum":50000}},"required":["active_seconds","tool_calls","model_tokens"],"additionalProperties":false},"misfire_policy":{"type":"string","enum":["skip","run_once"]},"notification_events":{"type":"array","items":{"type":"string","enum":["completed","failed","waiting_user"]},"uniqueItems":true},"observation":{"type":"object","properties":{"url":{"type":"string","format":"uri","maxLength":2048,"pattern":"^https?://"},"selector":{"type":"string","minLength":1,"maxLength":1024},"field":{"type":"string","enum":["text","value"]}},"required":["url","selector","field"],"additionalProperties":false}},"required":["host_id","model_id","objective","timing"],"additionalProperties":false}`), Strict: true}, RequiredScope: identity.ScopeScheduleManage})
		definitions = append(definitions, AgentToolDefinition{ModelTool: ModelTool{Name: agentScheduleUpdateTool,
			Description: "Update a schedule on this task's Host. Present the current version and changes for the user's approval. Set observation to null to clear it; changing its source resets the baseline.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"schedule_id":{"type":"string"},"expected_version":{"type":"integer","minimum":1},"host_id":{"type":"string"},"model_id":{"type":"string","minLength":1,"maxLength":256},"objective":{"type":"string","minLength":1,"maxLength":32768},"timing":{"type":"object","properties":{"kind":{"type":"string","enum":["once","daily","weekly"]},"run_at":{"type":"string"},"starts_on":{"type":"string"},"local_time":{"type":"string"},"time_zone":{"type":"string"},"weekdays":{"type":"array","items":{"type":"integer","minimum":1,"maximum":7}}},"required":["kind","time_zone"],"additionalProperties":false},"budget":{"type":"object","properties":{"active_seconds":{"type":"integer","minimum":1,"maximum":1800},"tool_calls":{"type":"integer","minimum":1,"maximum":100},"model_tokens":{"type":"integer","minimum":1,"maximum":50000}},"required":["active_seconds","tool_calls","model_tokens"],"additionalProperties":false},"misfire_policy":{"type":"string","enum":["skip","run_once"]},"notification_events":{"type":"array","items":{"type":"string","enum":["completed","failed","waiting_user"]},"uniqueItems":true},"observation":{"oneOf":[{"type":"object","properties":{"url":{"type":"string","format":"uri","maxLength":2048,"pattern":"^https?://"},"selector":{"type":"string","minLength":1,"maxLength":1024},"field":{"type":"string","enum":["text","value"]}},"required":["url","selector","field"],"additionalProperties":false},{"type":"null"}]},"required":["schedule_id","expected_version"],"additionalProperties":false}`), Strict: true}, RequiredScope: identity.ScopeScheduleManage})
	}
	if identity.Kind(claimed.Authorization.Kind) == identity.KindSession || hasAgentScope(claimed.Authorization.Scopes, identity.ScopeScheduleRead) {
		definitions = append(definitions, AgentToolDefinition{ModelTool: ModelTool{Name: agentScheduleListTool,
			Description: "List schedules on this task's Host.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","enum":["enabled","disabled","completed"]},"limit":{"type":"integer","minimum":1,"maximum":100},"cursor":{"type":"string","maxLength":1024}},"additionalProperties":false}`), Strict: true}, RequiredScope: identity.ScopeScheduleRead})
	}
	return definitions, nil
}

func (e *ScheduleAgentToolExecutor) Prepare(ctx context.Context, request AgentToolRequest) (AgentToolPreparation, error) {
	if request.Call.Name == agentScheduleListTool {
		if request.RequiredScope != identity.ScopeScheduleRead {
			return AgentToolPreparation{}, domain.InvalidArgument("scope", "schedule read scope is required")
		}
		if _, err := decodeAgentScheduleList(request.Arguments); err != nil {
			return AgentToolPreparation{}, err
		}
		current, err := e.runtime.currentTask(ctx, request.Task)
		if err != nil {
			return AgentToolPreparation{}, err
		}
		if err := e.authorize(ctx, current, identity.ScopeScheduleRead); err != nil {
			return AgentToolPreparation{}, err
		}
		fence, err := e.runtime.readFence(ctx, current, &request.Fence)
		if err != nil {
			return AgentToolPreparation{}, err
		}
		return AgentToolPreparation{Status: AgentToolReady, Fence: fence, Token: runtimeToolPreparationToken(request, fence)}, nil
	}
	if request.Call.Name == agentScheduleUpdateTool {
		if request.RequiredScope != identity.ScopeScheduleManage {
			return AgentToolPreparation{}, domain.InvalidArgument("scope", "schedule manage scope is required")
		}
		return e.prepareUpdate(ctx, request)
	}
	if request.Call.Name != agentScheduleCreateTool || request.RequiredScope != identity.ScopeScheduleManage {
		return e.runtime.Prepare(ctx, request)
	}
	input, command, err := decodeAgentScheduleCreate(request.Arguments)
	if err != nil {
		return AgentToolPreparation{}, err
	}
	current, err := e.runtime.currentTask(ctx, request.Task)
	if err != nil {
		return AgentToolPreparation{}, err
	}
	if input.HostID != current.HostID {
		return AgentToolPreparation{}, domain.NewError(domain.CodeForbidden, "schedule tools are limited to the task Host")
	}
	if err := e.runtime.authorizer.AuthorizeTaskAction(ctx, current.OwnerID, identity.Kind(current.Authorization.Kind), current.Authorization.TokenID,
		current.HostID, agentScopeSnapshot(current.Authorization.Scopes), identity.ScopeScheduleManage); err != nil {
		return AgentToolPreparation{}, err
	}
	principal := schedulePrincipalForTask(current)
	if _, err := e.schedules.authorizeHost(ctx, principal, current.HostID, identity.ScopeScheduleManage); err != nil {
		return AgentToolPreparation{}, err
	}
	if err := e.schedules.validateModel(ctx, current.OwnerID, command.ModelID); err != nil {
		return AgentToolPreparation{}, err
	}
	fence, err := e.runtime.readFence(ctx, current, &request.Fence)
	if err != nil {
		return AgentToolPreparation{}, err
	}
	approval := AgentApprovalRequest{
		Object: "Scheduled task on Host " + current.HostID,
		Content: fmt.Sprintf("Create a %s schedule for model %s in %s. Objective: %q. Timing: %s. Budget: %d active seconds, %d tool calls, %d model tokens. Next occurrence: %s.",
			command.Timing.Kind, command.ModelID, command.Timing.TimeZone, agentSummaryText(command.Objective), agentScheduleTimingSummary(command.Timing),
			command.Budget.ActiveSeconds, command.Budget.ToolCalls, command.Budget.ModelTokens, agentScheduleNext(command.Timing)),
		Impact: "This schedule can submit future tasks until it is disabled.",
	}
	if command.Observation != nil {
		approval.Content += fmt.Sprintf(" After each task, inspect %s selector %q field %s; the first successful sample becomes the baseline.",
			command.Observation.URL, command.Observation.Selector, command.Observation.Field)
	}
	if request.Approval == nil || request.Approval.Decision != task.PendingDecisionApprove || request.Approval.PendingID == "" || request.Approval.Version < 1 {
		return AgentToolPreparation{Status: AgentToolApprovalRequired, Approval: approval, Fence: fence}, nil
	}
	hash, err := agentOperationHash(request.Call.Name, request.Arguments)
	if err != nil || !bytes.Equal(request.Approval.RequestHash, hash[:]) {
		return AgentToolPreparation{}, domain.NewError(domain.CodeForbidden, "schedule approval does not match this request")
	}
	return AgentToolPreparation{Status: AgentToolReady, Fence: fence, Token: runtimeToolPreparationToken(request, fence)}, nil
}

func (e *ScheduleAgentToolExecutor) Execute(ctx context.Context, request AgentToolRequest, preparation AgentToolPreparation) (AgentToolResult, error) {
	if request.Call.Name == agentScheduleListTool {
		return e.list(ctx, request, preparation)
	}
	if request.Call.Name == agentScheduleUpdateTool {
		return e.executeUpdate(ctx, request, preparation)
	}
	if request.Call.Name != agentScheduleCreateTool || request.RequiredScope != identity.ScopeScheduleManage ||
		request.Approval == nil || request.Approval.Decision != task.PendingDecisionApprove || preparation.Status != AgentToolReady ||
		preparation.Fence != request.Fence || preparation.Token == "" || preparation.Token != runtimeToolPreparationToken(request, preparation.Fence) {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidArgument, "schedule tool preparation is invalid")
	}
	input, command, err := decodeAgentScheduleCreate(request.Arguments)
	if err != nil {
		return AgentToolResult{}, err
	}
	current, err := e.runtime.currentTask(ctx, request.Task)
	if err != nil {
		return AgentToolResult{}, err
	}
	if input.HostID != current.HostID {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "schedule tools are limited to the task Host")
	}
	if err := e.runtime.authorizer.AuthorizeTaskAction(ctx, current.OwnerID, identity.Kind(current.Authorization.Kind), current.Authorization.TokenID,
		current.HostID, agentScopeSnapshot(current.Authorization.Scopes), identity.ScopeScheduleManage); err != nil {
		return AgentToolResult{}, err
	}
	fence, err := e.runtime.readFence(ctx, current, &request.Fence)
	if err != nil {
		return AgentToolResult{}, err
	}
	if err := e.runtime.validatePersistedOperation(ctx, current, request, fence); err != nil {
		return AgentToolResult{}, err
	}
	if request.Approval.Version < 1 || request.Approval.PendingID == "" {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "schedule approval is invalid")
	}
	hash, err := agentOperationHash(request.Call.Name, request.Arguments)
	if err != nil || !bytes.Equal(request.Approval.RequestHash, hash[:]) {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "schedule approval does not match this request")
	}
	keySum := sha256.Sum256([]byte(current.ID + "\x00" + request.Call.CallID + "\x00" + hex.EncodeToString(request.Approval.RequestHash)))
	created, err := e.schedules.Create(ctx, schedulePrincipalForTask(current), command, "agent-schedule-"+hex.EncodeToString(keySum[:]))
	if err != nil {
		return AgentToolResult{}, err
	}
	output, err := json.Marshal(map[string]any{"schedule_id": created.ID, "version": created.Version, "status": created.Status, "host_id": created.HostID, "timing": created.Timing, "next_run_at": created.NextRunAt})
	if err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolSucceeded, ExternalID: created.ID, Output: string(output)}, nil
}

func (e *ScheduleAgentToolExecutor) prepareUpdate(ctx context.Context, request AgentToolRequest) (AgentToolPreparation, error) {
	input, command, err := decodeAgentScheduleUpdate(request.Arguments)
	if err != nil {
		return AgentToolPreparation{}, err
	}
	current, err := e.runtime.currentTask(ctx, request.Task)
	if err != nil {
		return AgentToolPreparation{}, err
	}
	if err := e.authorize(ctx, current, identity.ScopeScheduleManage); err != nil {
		return AgentToolPreparation{}, err
	}
	principal := schedulePrincipalForTask(current)
	value, err := e.schedules.getAuthorized(ctx, principal, input.ScheduleID, identity.ScopeScheduleManage)
	if err != nil {
		return AgentToolPreparation{}, err
	}
	if value.HostID != current.HostID || (command.HostID != nil && *command.HostID != current.HostID) {
		return AgentToolPreparation{}, domain.NewError(domain.CodeForbidden, "schedule tools are limited to the task Host")
	}
	if value.Version != input.ExpectedVersion {
		return AgentToolPreparation{}, scheduleVersionConflict(input.ExpectedVersion, value.Version)
	}
	if command.ModelID != nil {
		if err := e.schedules.validateModel(ctx, current.OwnerID, strings.TrimSpace(*command.ModelID)); err != nil {
			return AgentToolPreparation{}, err
		}
	}
	if command.Timing != nil {
		if err := command.Timing.Validate(); err != nil {
			return AgentToolPreparation{}, domain.InvalidArgument("timing", err.Error())
		}
	}
	if command.Budget != nil {
		if err := command.Budget.Validate(); err != nil {
			return AgentToolPreparation{}, domain.InvalidArgument("budget", err.Error())
		}
	}
	fence, err := e.runtime.readFence(ctx, current, &request.Fence)
	if err != nil {
		return AgentToolPreparation{}, err
	}
	approval := AgentApprovalRequest{
		Object:  "Schedule " + value.ID + " on Host " + current.HostID,
		Content: fmt.Sprintf("Update schedule version %d: %s. Changing or clearing the observation source discards its previous baseline.", value.Version, agentScheduleUpdateSummary(command)),
		Impact:  "This changes future scheduled tasks.",
	}
	if request.Approval == nil || request.Approval.Decision != task.PendingDecisionApprove || request.Approval.PendingID == "" || request.Approval.Version < 1 {
		return AgentToolPreparation{Status: AgentToolApprovalRequired, Approval: approval, Fence: fence}, nil
	}
	hash, err := agentOperationHash(request.Call.Name, request.Arguments)
	if err != nil || !bytes.Equal(request.Approval.RequestHash, hash[:]) {
		return AgentToolPreparation{}, domain.NewError(domain.CodeForbidden, "schedule approval does not match this request")
	}
	return AgentToolPreparation{Status: AgentToolReady, Fence: fence, Token: runtimeToolPreparationToken(request, fence)}, nil
}

func (e *ScheduleAgentToolExecutor) executeUpdate(ctx context.Context, request AgentToolRequest, preparation AgentToolPreparation) (AgentToolResult, error) {
	if request.RequiredScope != identity.ScopeScheduleManage || request.Approval == nil || request.Approval.Decision != task.PendingDecisionApprove ||
		preparation.Status != AgentToolReady || preparation.Fence != request.Fence || preparation.Token == "" ||
		preparation.Token != runtimeToolPreparationToken(request, preparation.Fence) {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidArgument, "schedule tool preparation is invalid")
	}
	input, command, err := decodeAgentScheduleUpdate(request.Arguments)
	if err != nil {
		return AgentToolResult{}, err
	}
	current, err := e.runtime.currentTask(ctx, request.Task)
	if err != nil {
		return AgentToolResult{}, err
	}
	if err := e.authorize(ctx, current, identity.ScopeScheduleManage); err != nil {
		return AgentToolResult{}, err
	}
	fence, err := e.runtime.readFence(ctx, current, &request.Fence)
	if err != nil {
		return AgentToolResult{}, err
	}
	if err := e.runtime.validatePersistedOperation(ctx, current, request, fence); err != nil {
		return AgentToolResult{}, err
	}
	principal := schedulePrincipalForTask(current)
	stored, err := e.schedules.getAuthorized(ctx, principal, input.ScheduleID, identity.ScopeScheduleManage)
	if err != nil {
		return AgentToolResult{}, err
	}
	if stored.HostID != current.HostID || (command.HostID != nil && *command.HostID != current.HostID) {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "schedule tools are limited to the task Host")
	}
	if request.Approval.Version < 1 || request.Approval.PendingID == "" {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "schedule approval is invalid")
	}
	hash, err := agentOperationHash(request.Call.Name, request.Arguments)
	if err != nil || !bytes.Equal(request.Approval.RequestHash, hash[:]) {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "schedule approval does not match this request")
	}
	updated, err := e.schedules.Update(ctx, principal, input.ScheduleID, command)
	if err != nil {
		return AgentToolResult{}, err
	}
	output, err := json.Marshal(map[string]any{"schedule_id": updated.ID, "version": updated.Version, "status": updated.Status, "observation": updated.Observation})
	if err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolSucceeded, ExternalID: updated.ID, Output: string(output)}, nil
}

func (e *ScheduleAgentToolExecutor) Verify(ctx context.Context, request AgentVerificationRequest) (AgentVerificationResult, error) {
	return e.runtime.Verify(ctx, request)
}

func (e *ScheduleAgentToolExecutor) ReconcileOperation(ctx context.Context, request AgentToolRequest, persisted task.Operation) (AgentToolResult, error) {
	if request.Call.Name == agentScheduleCreateTool || request.Call.Name == agentScheduleUpdateTool {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	if request.Call.Name == agentScheduleListTool {
		current, err := e.runtime.currentTask(ctx, request.Task)
		if err != nil {
			return AgentToolResult{Status: AgentToolUnknown}, nil
		}
		if err := e.authorize(ctx, current, identity.ScopeScheduleRead); err != nil {
			return AgentToolResult{Status: AgentToolUnknown}, nil
		}
		fence, err := e.runtime.readFence(ctx, current, &request.Fence)
		if err != nil {
			return AgentToolResult{Status: AgentToolUnknown}, nil
		}
		if persisted.TaskID != current.ID || persisted.Key != request.OperationKey || persisted.HostID != current.HostID ||
			persisted.RuntimeGeneration != fence.RuntimeGeneration || persisted.RuntimeInstanceID != fence.RuntimeInstanceID || persisted.ControlVersion != fence.ControlVersion {
			return AgentToolResult{Status: AgentToolUnknown}, nil
		}
		result, err := e.listSchedules(ctx, current, request.Arguments)
		if err != nil {
			return AgentToolResult{Status: AgentToolUnknown}, nil
		}
		return result, nil
	}
	return e.runtime.ReconcileOperation(ctx, request, persisted)
}

func (e *ScheduleAgentToolExecutor) list(ctx context.Context, request AgentToolRequest, preparation AgentToolPreparation) (AgentToolResult, error) {
	if request.RequiredScope != identity.ScopeScheduleRead || preparation.Status != AgentToolReady || preparation.Fence != request.Fence ||
		preparation.Token == "" || preparation.Token != runtimeToolPreparationToken(request, preparation.Fence) {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidArgument, "schedule tool preparation is invalid")
	}
	current, err := e.runtime.currentTask(ctx, request.Task)
	if err != nil {
		return AgentToolResult{}, err
	}
	if err := e.authorize(ctx, current, identity.ScopeScheduleRead); err != nil {
		return AgentToolResult{}, err
	}
	fence, err := e.runtime.readFence(ctx, current, &request.Fence)
	if err != nil {
		return AgentToolResult{}, err
	}
	if err := e.runtime.validatePersistedOperation(ctx, current, request, fence); err != nil {
		return AgentToolResult{}, err
	}
	return e.listSchedules(ctx, current, request.Arguments)
}

func (e *ScheduleAgentToolExecutor) listSchedules(ctx context.Context, current task.Task, raw []byte) (AgentToolResult, error) {
	input, err := decodeAgentScheduleList(raw)
	if err != nil {
		return AgentToolResult{}, err
	}
	if input.Limit == 0 {
		input.Limit = 20
	}
	if input.Limit < 1 || input.Limit > 100 || len(input.Cursor) > 1024 {
		return AgentToolResult{}, domain.InvalidArgument("page", "schedule page is invalid")
	}
	page, err := e.schedules.List(ctx, schedulePrincipalForTask(current), ScheduleListRequest{Status: schedule.Status(input.Status), Page: PageRequest{Limit: int32(input.Limit), Cursor: input.Cursor}})
	if err != nil {
		return AgentToolResult{}, err
	}
	items := make([]map[string]any, 0, len(page.Items))
	for _, value := range page.Items {
		items = append(items, map[string]any{"id": value.ID, "version": value.Version, "status": value.Status, "host_id": value.HostID, "model_id": value.ModelID, "objective": value.Objective, "timing": value.Timing, "next_run_at": value.NextRunAt})
	}
	output, err := json.Marshal(map[string]any{"items": items, "next_cursor": page.NextCursor})
	if err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolSucceeded, Output: string(output)}, nil
}

func (e *ScheduleAgentToolExecutor) authorize(ctx context.Context, current task.Task, scope identity.Scope) error {
	return e.runtime.authorizer.AuthorizeTaskAction(ctx, current.OwnerID, identity.Kind(current.Authorization.Kind), current.Authorization.TokenID,
		current.HostID, agentScopeSnapshot(current.Authorization.Scopes), scope)
}

func decodeAgentScheduleList(raw []byte) (struct {
	Status string `json:"status,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}, error) {
	var input struct {
		Status string `json:"status,omitempty"`
		Limit  int    `json:"limit,omitempty"`
		Cursor string `json:"cursor,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, domain.InvalidArgument("schedule", "schedule list request is invalid")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return input, domain.InvalidArgument("schedule", "schedule list request is invalid")
	}
	return input, nil
}

func decodeAgentScheduleCreate(raw []byte) (struct {
	HostID      string                    `json:"host_id"`
	ModelID     string                    `json:"model_id"`
	Objective   string                    `json:"objective"`
	Timing      schedule.Timing           `json:"timing"`
	Observation *schedule.ObservationSpec `json:"observation,omitempty"`
	Budget      *struct {
		ActiveSeconds int64 `json:"active_seconds"`
		ToolCalls     int64 `json:"tool_calls"`
		ModelTokens   int64 `json:"model_tokens"`
	} `json:"budget,omitempty"`
	MisfirePolicy      schedule.MisfirePolicy       `json:"misfire_policy,omitempty"`
	NotificationEvents []schedule.NotificationEvent `json:"notification_events,omitempty"`
}, CreateScheduleCommand, error) {
	type inputType struct {
		HostID      string                    `json:"host_id"`
		ModelID     string                    `json:"model_id"`
		Objective   string                    `json:"objective"`
		Timing      schedule.Timing           `json:"timing"`
		Observation *schedule.ObservationSpec `json:"observation,omitempty"`
		Budget      *struct {
			ActiveSeconds int64 `json:"active_seconds"`
			ToolCalls     int64 `json:"tool_calls"`
			ModelTokens   int64 `json:"model_tokens"`
		} `json:"budget,omitempty"`
		MisfirePolicy      schedule.MisfirePolicy       `json:"misfire_policy,omitempty"`
		NotificationEvents []schedule.NotificationEvent `json:"notification_events,omitempty"`
	}
	var input inputType
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, CreateScheduleCommand{}, domain.InvalidArgument("schedule", "schedule request is invalid")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return input, CreateScheduleCommand{}, domain.InvalidArgument("schedule", "schedule request is invalid")
	}
	if !domain.ValidHostID(input.HostID) || strings.TrimSpace(input.ModelID) == "" || len(input.ModelID) > 256 || strings.TrimSpace(input.Objective) == "" || len(input.Objective) > 32768 {
		return input, CreateScheduleCommand{}, domain.InvalidArgument("schedule", "host, model, and objective are required")
	}
	command := CreateScheduleCommand{HostID: input.HostID, ModelID: input.ModelID, Objective: input.Objective, Timing: input.Timing, Observation: input.Observation, MisfirePolicy: input.MisfirePolicy, NotificationEvents: input.NotificationEvents}
	if input.Budget == nil {
		command.Budget = task.Budget{ActiveSeconds: task.MaxActiveSeconds, ToolCalls: task.MaxToolCalls, ModelTokens: task.MaxModelTokens}
	} else {
		command.Budget = task.Budget{ActiveSeconds: input.Budget.ActiveSeconds, ToolCalls: input.Budget.ToolCalls, ModelTokens: input.Budget.ModelTokens}
	}
	if err := command.Budget.Validate(); err != nil {
		return input, CreateScheduleCommand{}, domain.InvalidArgument("budget", err.Error())
	}
	if command.MisfirePolicy == "" {
		command.MisfirePolicy = schedule.MisfireSkip
	}
	if !command.MisfirePolicy.Valid() {
		return input, CreateScheduleCommand{}, domain.InvalidArgument("misfire_policy", "misfire policy is invalid")
	}
	if command.NotificationEvents == nil {
		command.NotificationEvents = []schedule.NotificationEvent{schedule.NotifyCompleted, schedule.NotifyFailed, schedule.NotifyWaitingUser}
	}
	if err := validateNotificationEvents(command.NotificationEvents); err != nil {
		return input, CreateScheduleCommand{}, err
	}
	if err := command.Timing.Validate(); err != nil {
		return input, CreateScheduleCommand{}, domain.InvalidArgument("timing", err.Error())
	}
	if command.Observation != nil {
		if err := command.Observation.Validate(); err != nil {
			return input, CreateScheduleCommand{}, domain.InvalidArgument("observation", err.Error())
		}
	}
	return input, command, nil
}

type agentScheduleUpdateInput struct {
	ScheduleID      string           `json:"schedule_id"`
	ExpectedVersion int64            `json:"expected_version"`
	HostID          *string          `json:"host_id,omitempty"`
	ModelID         *string          `json:"model_id,omitempty"`
	Objective       *string          `json:"objective,omitempty"`
	Timing          *schedule.Timing `json:"timing,omitempty"`
	Budget          *struct {
		ActiveSeconds int64 `json:"active_seconds"`
		ToolCalls     int64 `json:"tool_calls"`
		ModelTokens   int64 `json:"model_tokens"`
	} `json:"budget,omitempty"`
	MisfirePolicy      *schedule.MisfirePolicy       `json:"misfire_policy,omitempty"`
	NotificationEvents *[]schedule.NotificationEvent `json:"notification_events,omitempty"`
	Observation        json.RawMessage               `json:"observation,omitempty"`
}

func decodeAgentScheduleUpdate(raw []byte) (agentScheduleUpdateInput, UpdateScheduleCommand, error) {
	var input agentScheduleUpdateInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, UpdateScheduleCommand{}, domain.InvalidArgument("schedule", "schedule update request is invalid")
	}
	if err := decoder.Decode(new(any)); err != io.EOF || !schedule.ValidID(input.ScheduleID) || input.ExpectedVersion < 1 ||
		(input.HostID == nil && input.ModelID == nil && input.Objective == nil && input.Timing == nil && input.Budget == nil && input.MisfirePolicy == nil && input.NotificationEvents == nil && input.Observation == nil) {
		return input, UpdateScheduleCommand{}, domain.InvalidArgument("schedule", "schedule update request is invalid")
	}
	command := UpdateScheduleCommand{ExpectedVersion: input.ExpectedVersion, HostID: input.HostID, ModelID: input.ModelID,
		Objective: input.Objective, Timing: input.Timing, MisfirePolicy: input.MisfirePolicy, NotificationEvents: input.NotificationEvents}
	if input.Budget != nil {
		budget := task.Budget{ActiveSeconds: input.Budget.ActiveSeconds, ToolCalls: input.Budget.ToolCalls, ModelTokens: input.Budget.ModelTokens}
		command.Budget = &budget
	}
	if input.Observation != nil {
		command.ObservationSet = true
		if !bytes.Equal(bytes.TrimSpace(input.Observation), []byte("null")) {
			var observation schedule.ObservationSpec
			if err := decodeStrictObservation(input.Observation, &observation); err != nil {
				return input, UpdateScheduleCommand{}, domain.InvalidArgument("observation", "observation configuration is invalid")
			}
			if err := observation.Validate(); err != nil {
				return input, UpdateScheduleCommand{}, domain.InvalidArgument("observation", err.Error())
			}
			command.Observation = &observation
		}
	}
	if command.Timing != nil {
		if err := command.Timing.Validate(); err != nil {
			return input, UpdateScheduleCommand{}, domain.InvalidArgument("timing", err.Error())
		}
	}
	if command.Budget != nil {
		if err := command.Budget.Validate(); err != nil {
			return input, UpdateScheduleCommand{}, domain.InvalidArgument("budget", err.Error())
		}
	}
	if command.HostID != nil && !domain.ValidHostID(*command.HostID) {
		return input, UpdateScheduleCommand{}, domain.InvalidArgument("host_id", "host id is invalid")
	}
	if command.ModelID != nil && (strings.TrimSpace(*command.ModelID) == "" || len(*command.ModelID) > 256) {
		return input, UpdateScheduleCommand{}, domain.InvalidArgument("model_id", "model id is invalid")
	}
	if command.Objective != nil && !validBoundedText(*command.Objective, 32768) {
		return input, UpdateScheduleCommand{}, domain.InvalidArgument("objective", "objective is invalid")
	}
	if command.MisfirePolicy != nil && !command.MisfirePolicy.Valid() {
		return input, UpdateScheduleCommand{}, domain.InvalidArgument("misfire_policy", "misfire policy is invalid")
	}
	if command.NotificationEvents != nil {
		if err := validateNotificationEvents(*command.NotificationEvents); err != nil {
			return input, UpdateScheduleCommand{}, err
		}
	}
	return input, command, nil
}

func decodeStrictObservation(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("observation has trailing JSON")
	}
	return nil
}

func schedulePrincipalForTask(value task.Task) identity.Principal {
	kind := identity.Kind(value.Authorization.Kind)
	principal := identity.Principal{UserID: value.OwnerID, Role: identity.RoleUser, Kind: kind, TokenID: value.Authorization.TokenID}
	if kind != identity.KindSession {
		principal.HostIDs = []string{value.HostID}
		principal.Scopes = agentScopeSnapshot(value.Authorization.Scopes)
	}
	return principal
}

func hasAgentScope(scopes []string, required identity.Scope) bool {
	for _, scope := range scopes {
		if scope == string(required) {
			return true
		}
	}
	return false
}

func agentScheduleTimingSummary(value schedule.Timing) string {
	if value.Kind == schedule.TimingOnce {
		return value.RunAt
	}
	if value.Kind == schedule.TimingWeekly {
		return fmt.Sprintf("%s on weekdays %v", value.LocalTime, value.Weekdays)
	}
	return value.LocalTime + " daily"
}

func agentScheduleNext(value schedule.Timing) string {
	next, err := schedule.FirstOccurrence(value, time.Now().UTC())
	if err != nil || next.ScheduledAt == nil {
		return "next available recurring slot"
	}
	return next.ScheduledAt.Format("2006-01-02T15:04:05Z07:00")
}

func agentScheduleUpdateSummary(command UpdateScheduleCommand) string {
	changes := make([]string, 0, 8)
	if command.HostID != nil {
		changes = append(changes, "Host "+*command.HostID)
	}
	if command.ModelID != nil {
		changes = append(changes, "model "+*command.ModelID)
	}
	if command.Objective != nil {
		changes = append(changes, "task objective "+fmt.Sprintf("%q", agentSummaryText(*command.Objective)))
	}
	if command.Timing != nil {
		changes = append(changes, "timing "+agentScheduleTimingSummary(*command.Timing))
	}
	if command.Budget != nil {
		changes = append(changes, fmt.Sprintf("budget %d seconds, %d tool calls, %d model tokens", command.Budget.ActiveSeconds, command.Budget.ToolCalls, command.Budget.ModelTokens))
	}
	if command.MisfirePolicy != nil {
		changes = append(changes, "misfire policy "+string(*command.MisfirePolicy))
	}
	if command.NotificationEvents != nil {
		changes = append(changes, "notification events "+strings.Join(notificationEventNames(*command.NotificationEvents), ", "))
	}
	if command.ObservationSet {
		if command.Observation == nil {
			changes = append(changes, "clear web observation")
		} else {
			changes = append(changes, fmt.Sprintf("observe %s selector %q field %s", command.Observation.URL, command.Observation.Selector, command.Observation.Field))
		}
	}
	return strings.Join(changes, "; ")
}

func notificationEventNames(values []schedule.NotificationEvent) []string {
	names := make([]string, len(values))
	for i, value := range values {
		names[i] = string(value)
	}
	return names
}

func agentSummaryText(value string) string {
	runes := []rune(value)
	if len(runes) > 240 {
		return string(runes[:240]) + "…"
	}
	return value
}
