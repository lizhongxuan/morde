package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

const (
	cloudDSHGoalListToolName       = "morde_goal_list"
	cloudDSHGoalCreateToolName     = "morde_goal_create"
	cloudDSHGoalUpdateToolName     = "morde_goal_update"
	cloudDSHGoalDeleteToolName     = "morde_goal_delete"
	cloudDSHScheduleListToolName   = "morde_schedule_list"
	cloudDSHScheduleCreateToolName = "morde_schedule_create"
	cloudDSHScheduleUpdateToolName = "morde_schedule_update"
	cloudDSHScheduleActionToolName = "morde_schedule_action"
	cloudDSHScheduleRunsToolName   = "morde_schedule_run_history"
	cloudDSHGoalListLimit          = int32(10)
	cloudDSHScheduleListLimit      = int32(20)
	cloudDSHScheduleRunsLimit      = int32(50)
	cloudDSHScheduleTextLimit      = 300
)

type cloudDSHGoalStep struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Note   string `json:"note"`
}

type cloudDSHGoalBody struct {
	Title              string             `json:"title"`
	Description        string             `json:"description"`
	Status             string             `json:"status"`
	Steps              []cloudDSHGoalStep `json:"steps"`
	RelatedMaterialIDs []string           `json:"related_material_ids,omitempty"`
}

type cloudDSHGoalListArguments struct {
	Limit  int32  `json:"limit"`
	Cursor string `json:"cursor"`
}

type cloudDSHGoalCreateStep struct {
	Title  string `json:"title"`
	Status string `json:"status"`
	Note   string `json:"note"`
}

type cloudDSHGoalCreateArguments struct {
	Title              string                   `json:"title"`
	Description        string                   `json:"description"`
	Status             string                   `json:"status"`
	Steps              []cloudDSHGoalCreateStep `json:"steps"`
	RelatedMaterialIDs []string                 `json:"related_material_ids"`
}

type cloudDSHGoalStepStatusPatch struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type cloudDSHGoalUpdateArguments struct {
	GoalID           string          `json:"goal_id"`
	ExpectedRevision int64           `json:"expected_revision"`
	Title            *string         `json:"title"`
	Description      *string         `json:"description"`
	Status           *string         `json:"status"`
	Steps            json.RawMessage `json:"steps"`
}

type cloudDSHGoalDeleteArguments struct {
	GoalID           string `json:"goal_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

type cloudDSHPreparedGoalMutation struct {
	mutation personal.Mutation
	output   any
	status   string
}

type cloudDSHScheduleListArguments struct {
	Status string `json:"status"`
	Limit  int32  `json:"limit"`
	Cursor string `json:"cursor"`
}

type cloudDSHScheduleCreateArguments struct {
	Title     string          `json:"title"`
	Objective string          `json:"objective"`
	Timing    schedule.Timing `json:"timing"`
}

type cloudDSHScheduleUpdateArguments struct {
	ScheduleID      string           `json:"schedule_id"`
	ExpectedVersion int64            `json:"expected_version"`
	Title           *string          `json:"title"`
	Objective       *string          `json:"objective"`
	Timing          *schedule.Timing `json:"timing"`
}

type cloudDSHScheduleActionArguments struct {
	ScheduleID       string `json:"schedule_id"`
	ExpectedVersion  int64  `json:"expected_version"`
	Action           string `json:"action"`
	CurrentRunAction string `json:"current_run_action"`
}

type cloudDSHScheduleRunsArguments struct {
	ScheduleID string `json:"schedule_id"`
	Limit      int32  `json:"limit"`
	Cursor     string `json:"cursor"`
}

func cloudDSHGoalToolDefinitions() []AgentToolDefinition {
	return []AgentToolDefinition{
		cloudDSHGoalTool(cloudDSHGoalListToolName,
			"List the current user's personal goals. The result includes each goal ID and revision for later updates.",
			`{"type":"object","properties":{"limit":{"type":"integer","minimum":1,"maximum":10},"cursor":{"type":"string","maxLength":1024}},"required":["limit","cursor"],"additionalProperties":false}`, identity.ScopeTaskRead),
		cloudDSHGoalTool(cloudDSHGoalCreateToolName,
			"Create a personal goal and its plan steps. Use a concise title, status active, and [] when there are no steps. Step IDs are assigned by Morde.",
			`{"type":"object","properties":{"title":{"type":"string","minLength":1,"maxLength":200},"description":{"type":"string","maxLength":4096},"status":{"type":"string","enum":["active","paused","done","cancelled"]},"steps":{"type":"array","maxItems":100,"items":{"type":"object","properties":{"title":{"type":"string","minLength":1,"maxLength":200},"status":{"type":"string","enum":["pending","in_progress","done","blocked","skipped"]},"note":{"type":"string","maxLength":2000}},"required":["title","status","note"],"additionalProperties":false}},"related_material_ids":{"type":"array","maxItems":100,"items":{"type":"string","minLength":1,"maxLength":128}}},"required":["title","description","status","steps","related_material_ids"],"additionalProperties":false}`, identity.ScopeTaskRead),
		cloudDSHGoalTool(cloudDSHGoalUpdateToolName,
			"Update a personal goal by ID and current revision. Set title, description, or status to null to leave it unchanged. steps is null to leave steps unchanged, or a list of existing step IDs and new statuses to update only those steps.",
			`{"type":"object","properties":{"goal_id":{"type":"string","minLength":1,"maxLength":128},"expected_revision":{"type":"integer","minimum":1},"title":{"type":["string","null"],"maxLength":200},"description":{"type":["string","null"],"maxLength":4096},"status":{"type":["string","null"],"enum":["active","paused","done","cancelled",null]},"steps":{"type":["array","null"],"maxItems":100,"items":{"type":"object","properties":{"id":{"type":"string","minLength":1,"maxLength":128},"status":{"type":"string","enum":["pending","in_progress","done","blocked","skipped"]}},"required":["id","status"],"additionalProperties":false}}},"required":["goal_id","expected_revision","title","description","status","steps"],"additionalProperties":false}`, identity.ScopeTaskRead),
		cloudDSHGoalTool(cloudDSHGoalDeleteToolName,
			"Delete a personal goal using its current revision. Ask the user before deletion if their intent is ambiguous.",
			`{"type":"object","properties":{"goal_id":{"type":"string","minLength":1,"maxLength":128},"expected_revision":{"type":"integer","minimum":1}},"required":["goal_id","expected_revision"],"additionalProperties":false}`, identity.ScopeTaskRead),
	}
}

func cloudDSHScheduleToolDefinitions() []AgentToolDefinition {
	return []AgentToolDefinition{
		cloudDSHScheduleTool(cloudDSHScheduleListToolName,
			"List this user's schedules. Use status all to include enabled, disabled, and completed schedules.",
			`{"type":"object","properties":{"status":{"type":"string","enum":["all","enabled","disabled","completed"]},"limit":{"type":"integer","minimum":1,"maximum":20},"cursor":{"type":"string","maxLength":1024}},"required":["status","limit","cursor"],"additionalProperties":false}`, identity.ScopeScheduleRead),
		cloudDSHScheduleTool(cloudDSHScheduleCreateToolName,
			"Create a recurring or one-time personal schedule. It runs using this conversation task's selected model and Host (if any), with the same budget. Always include an IANA time_zone; ask the user if the intended timezone is unclear. Hostless schedules cannot request computer or workspace work.",
			`{"type":"object","properties":{"title":{"type":"string","maxLength":32768},"objective":{"type":"string","minLength":1,"maxLength":32768},"timing":{"type":"object","properties":{"kind":{"type":"string","enum":["once","daily","weekly","every_n_hours"]},"run_at":{"type":"string"},"starts_on":{"type":"string"},"local_time":{"type":"string"},"time_zone":{"type":"string","minLength":1,"maxLength":64},"weekdays":{"type":"array","items":{"type":"integer"}},"interval_hours":{"type":"integer"},"anchor_at":{"type":"string"}},"required":["kind","run_at","starts_on","local_time","time_zone","weekdays","interval_hours","anchor_at"],"additionalProperties":false}},"required":["title","objective","timing"],"additionalProperties":false}`, identity.ScopeScheduleManage),
		cloudDSHScheduleTool(cloudDSHScheduleUpdateToolName,
			"Update a schedule with its current version. Set title, objective, or timing to null to leave it unchanged. The owner, model, and Host binding cannot be changed here.",
			`{"type":"object","properties":{"schedule_id":{"type":"string","minLength":8,"maxLength":8},"expected_version":{"type":"integer","minimum":1},"title":{"type":["string","null"],"maxLength":32768},"objective":{"type":["string","null"],"maxLength":32768},"timing":{"type":["object","null"],"properties":{"kind":{"type":"string","enum":["once","daily","weekly","every_n_hours"]},"run_at":{"type":"string"},"starts_on":{"type":"string"},"local_time":{"type":"string"},"time_zone":{"type":"string","minLength":1,"maxLength":64},"weekdays":{"type":"array","items":{"type":"integer"}},"interval_hours":{"type":"integer"},"anchor_at":{"type":"string"}},"required":["kind","run_at","starts_on","local_time","time_zone","weekdays","interval_hours","anchor_at"],"additionalProperties":false}},"required":["schedule_id","expected_version","title","objective","timing"],"additionalProperties":false}`, identity.ScopeScheduleManage),
		cloudDSHScheduleTool(cloudDSHScheduleActionToolName,
			"Enable or disable a schedule using its current version. current_run_action may be empty, continue, or cancel; choose cancel only when the user asks to stop an active run.",
			`{"type":"object","properties":{"schedule_id":{"type":"string","minLength":8,"maxLength":8},"expected_version":{"type":"integer","minimum":1},"action":{"type":"string","enum":["enable","disable"]},"current_run_action":{"type":"string","enum":["","continue","cancel"]}},"required":["schedule_id","expected_version","action","current_run_action"],"additionalProperties":false}`, identity.ScopeScheduleManage),
		cloudDSHScheduleTool(cloudDSHScheduleRunsToolName,
			"Read run history for a schedule owned by the current user.",
			`{"type":"object","properties":{"schedule_id":{"type":"string","minLength":8,"maxLength":8},"limit":{"type":"integer","minimum":1,"maximum":50},"cursor":{"type":"string","maxLength":1024}},"required":["schedule_id","limit","cursor"],"additionalProperties":false}`, identity.ScopeScheduleRead),
	}
}

func cloudDSHGoalTool(name, description, schema string, scope identity.Scope) AgentToolDefinition {
	return AgentToolDefinition{ModelTool: ModelTool{Name: name, Description: description, Parameters: json.RawMessage(schema), Strict: true}, RequiredScope: scope}
}

func cloudDSHScheduleTool(name, description, schema string, scope identity.Scope) AgentToolDefinition {
	return cloudDSHGoalTool(name, description, schema, scope)
}

func cloudDSHGoalScheduleToolName(name string) bool {
	switch name {
	case cloudDSHGoalListToolName, cloudDSHGoalCreateToolName, cloudDSHGoalUpdateToolName, cloudDSHGoalDeleteToolName,
		cloudDSHScheduleListToolName, cloudDSHScheduleCreateToolName, cloudDSHScheduleUpdateToolName,
		cloudDSHScheduleActionToolName, cloudDSHScheduleRunsToolName:
		return true
	default:
		return false
	}
}

func cloudDSHScheduleContextEligible(value task.Task) bool {
	contextValue := value.ConversationContext
	if contextValue == nil || value.ProjectID != "" || value.InitiatorID != value.OwnerID ||
		contextValue.OwnerID != value.OwnerID || contextValue.InitiatorID != value.InitiatorID ||
		contextValue.ProjectID != "" || contextValue.ConversationID != value.ConversationID || contextValue.Scope != "personal" {
		return false
	}
	_, ok := cloudDSHSchedulePrincipal(value)
	return ok
}

func cloudDSHSchedulePrincipal(value task.Task) (identity.Principal, bool) {
	kind := identity.Kind(value.Authorization.Kind)
	if value.ProjectID != "" || value.InitiatorID != value.OwnerID ||
		(kind != identity.KindSession && kind != identity.KindClient) || !domain.ValidUserID(value.OwnerID) ||
		!domain.ValidTokenID(value.Authorization.TokenID) || value.Authorization.Version < 1 ||
		value.Authorization.ExpiresAt == nil || !value.Authorization.ExpiresAt.After(time.Now()) {
		return identity.Principal{}, false
	}
	principal := identity.Principal{UserID: value.OwnerID, Role: identity.RoleUser, Kind: kind,
		TokenID: value.Authorization.TokenID, ClientInstallationID: value.Authorization.ClientInstallationID}
	if kind == identity.KindClient {
		scopes := agentScopeSnapshot(value.Authorization.Scopes)
		if !domain.ValidClientInstallationID(principal.ClientInstallationID) || !identity.IsScheduleGrantScopeSet(scopes) {
			return identity.Principal{}, false
		}
		principal.Scopes = scopes
	}
	return principal, true
}

func (b *CloudDSHToolBroker) executeGoalScheduleTool(ctx context.Context, claimed task.Task, call ModelToolCall, recovering bool, checkpointOutput *string) (AgentToolResult, error) {
	if ctx == nil || !cloudDSHGoalScheduleToolName(call.Name) {
		return AgentToolResult{}, domain.InvalidArgument("tool_call", "goal or schedule tool call is invalid")
	}
	arguments, err := validateAgentToolArguments(call.Arguments)
	if err != nil {
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "goal or schedule tool arguments are invalid")
	}
	details, err := b.currentServerTask(ctx, claimed)
	if err != nil {
		return AgentToolResult{}, err
	}
	current := details.Task
	if !sameCloudDSHAuthorization(current.Authorization, claimed.Authorization) ||
		(current.AgentSessionID != "" && claimed.AgentSessionID != "" && current.AgentSessionID != claimed.AgentSessionID) {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "goal or schedule authorization changed")
	}
	if current.Usage.ToolCalls == int64(^uint64(0)>>1) || current.Usage.ToolCalls >= current.Budget.ToolCalls {
		return AgentToolResult{}, domain.NewError(domain.CodeQuotaExceeded, "task tool call budget is exhausted")
	}
	mutationID := cloudDSHPersonalMutationID(current.ID, current.AgentSessionID, call.CallID)
	var result any
	var goalMutation *cloudDSHPreparedGoalMutation
	var stepTitle string
	stepSummary := "Used the existing account-scoped Goal service."
	switch call.Name {
	case cloudDSHGoalListToolName, cloudDSHGoalCreateToolName, cloudDSHGoalUpdateToolName, cloudDSHGoalDeleteToolName:
		if b.goals == nil || !cloudDSHPersonalContextEligible(current) {
			return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "personal goal tools are unavailable for this Task")
		}
		principal, ok := cloudDSHPersonalPrincipal(current)
		if !ok {
			return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "personal goal tools are unavailable for this Task")
		}
		result, stepTitle, err = b.executeGoalServiceTool(ctx, current, call.Name, arguments, principal, mutationID)
		if err != nil {
			return AgentToolResult{}, err
		}
		if prepared, ok := result.(cloudDSHPreparedGoalMutation); ok {
			goalMutation = &prepared
			result = prepared.output
		}
	case cloudDSHScheduleListToolName, cloudDSHScheduleCreateToolName, cloudDSHScheduleUpdateToolName,
		cloudDSHScheduleActionToolName, cloudDSHScheduleRunsToolName:
		if b.schedules == nil || !cloudDSHScheduleContextEligible(current) {
			return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "schedule tools are unavailable for this Task")
		}
		principal, ok := cloudDSHSchedulePrincipal(current)
		if !ok {
			return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "schedule tools are unavailable for this Task")
		}
		result, stepTitle, err = b.executeScheduleServiceTool(ctx, current, call.Name, arguments, principal, mutationID)
		if err != nil {
			return AgentToolResult{}, err
		}
	default:
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "goal or schedule tool is not available for this Task")
	}
	outputBytes, err := json.Marshal(result)
	if err != nil || len(outputBytes) > maxAgentToolOutputBytes || !utf8.Valid(outputBytes) {
		return AgentToolResult{}, domain.NewError(domain.CodeResultTooLarge, "goal or schedule tool result exceeds its limit")
	}
	output := string(outputBytes)
	if recovering && checkpointOutput != nil && *checkpointOutput != output {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "checkpointed goal or schedule result changed")
	}
	usage := cloneTaskUsage(current.Usage)
	usage.ToolCalls++
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepSucceeded, stepTitle, stepSummary)
	if err != nil {
		return AgentToolResult{}, err
	}
	if goalMutation != nil {
		argumentsHash := sha256.Sum256(arguments)
		commit := CloudDSHPersonalMutationCommit{TaskID: current.ID, OwnerID: current.OwnerID,
			AgentSessionID: current.AgentSessionID, AuthorizationVersion: current.Authorization.Version,
			CallID: call.CallID, Mutation: goalMutation.mutation, GoalToolName: call.Name,
			GoalArgumentsSHA256: hex.EncodeToString(argumentsHash[:]), GoalStatus: goalMutation.status}
		commit.FingerprintSHA256 = cloudDSHPersonalMutationFingerprint(commit)
		detail := map[string]any{
			"tool": call.Name, "personal_call_id": commit.CallID, "personal_mutation_id": commit.Mutation.MutationID,
			"agent_session_id": commit.AgentSessionID, "authorization_version": commit.AuthorizationVersion,
			"object_type": commit.Mutation.ObjectType, "object_id": commit.Mutation.ObjectID,
			"revision": commit.Mutation.BaseRevision + 1, "request_sha256": hex.EncodeToString(commit.Mutation.RequestHash),
			"fingerprint_sha256": commit.FingerprintSHA256, "goal_arguments_sha256": commit.GoalArgumentsSHA256,
			"goal_status": commit.GoalStatus,
		}
		if err := b.commitTaskPersonalMutation(ctx, current, step, usage, detail, commit); err != nil {
			return AgentToolResult{}, err
		}
	} else if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
		"tool": call.Name, "call_id": call.CallID, "authorization_version": current.Authorization.Version,
	}); err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolSucceeded, Output: output}, nil
}

func (b *CloudDSHToolBroker) executeGoalServiceTool(ctx context.Context, current task.Task, name string, raw json.RawMessage,
	principal identity.Principal, mutationID string) (any, string, error) {
	switch name {
	case cloudDSHGoalListToolName:
		var args cloudDSHGoalListArguments
		if decodeStrictRuntimeResult(raw, &args) != nil || args.Limit < 1 || args.Limit > cloudDSHGoalListLimit || len(args.Cursor) > 1024 {
			return nil, "", domain.InvalidArgument("goal_list", "goal list arguments are invalid")
		}
		page, err := b.goals.List(ctx, principal, PageRequest(args))
		if err != nil {
			return nil, "", err
		}
		items := make([]map[string]any, 0, len(page.Items))
		for _, item := range page.Items {
			if item.ObjectType != personal.ObjectTypeGoal || personal.ValidateObjectBody(item.ObjectType, item.ObjectID, item.Body) != nil || item.Revision < 1 {
				return nil, "", domain.NewError(domain.CodeInternal, "stored goal is invalid")
			}
			var body cloudDSHGoalBody
			if decodeStrictRuntimeResult(item.Body, &body) != nil {
				return nil, "", domain.NewError(domain.CodeInternal, "stored goal is invalid")
			}
			steps := body.Steps
			stepsTruncated := len(steps) > 20
			if stepsTruncated {
				steps = steps[:20]
			}
			stepSummaries := make([]map[string]string, 0, len(steps))
			for _, step := range steps {
				stepSummaries = append(stepSummaries, map[string]string{"id": step.ID, "title": cloudDSHClipText(step.Title, 100), "status": step.Status})
			}
			items = append(items, map[string]any{
				"id": item.ObjectID, "revision": item.Revision, "title": body.Title,
				"description": cloudDSHClipText(body.Description, 500), "status": body.Status,
				"steps": stepSummaries, "step_count": len(body.Steps), "steps_truncated": stepsTruncated,
			})
		}
		return map[string]any{"goals": items, "next_cursor": page.NextCursor}, "List personal goals", nil
	case cloudDSHGoalCreateToolName:
		if b.personalData == nil {
			return nil, "", domain.NewError(domain.CodeForbidden, "personal goal writes are unavailable")
		}
		var args cloudDSHGoalCreateArguments
		if decodeStrictRuntimeResult(raw, &args) != nil || len(args.Steps) > 100 || len(args.RelatedMaterialIDs) > 100 {
			return nil, "", domain.InvalidArgument("goal_create", "goal creation arguments are invalid")
		}
		body := cloudDSHGoalBody{Title: args.Title, Description: args.Description, Status: args.Status,
			Steps: make([]cloudDSHGoalStep, 0, len(args.Steps)), RelatedMaterialIDs: args.RelatedMaterialIDs}
		for index, step := range args.Steps {
			body.Steps = append(body.Steps, cloudDSHGoalStep{
				ID:    cloudDSHStablePersonalID("step", current.ID, current.AgentSessionID, mutationID, index),
				Title: step.Title, Status: step.Status, Note: step.Note,
			})
		}
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			return nil, "", err
		}
		goalID := cloudDSHStablePersonalID("goal", current.ID, current.AgentSessionID, mutationID, 0)
		mutation, err := b.personalData.PreparePersonalMutation(principal, PersonalMutationCommand{
			MutationID: mutationID, ObjectType: personal.ObjectTypeGoal, ObjectID: goalID, Body: bodyBytes,
		})
		if err != nil {
			return nil, "", err
		}
		return cloudDSHPreparedGoalMutation{mutation: mutation, status: body.Status,
			output: map[string]any{"id": goalID, "revision": int64(1), "title": body.Title, "status": body.Status, "step_count": len(body.Steps)}}, "Create personal goal", nil
	case cloudDSHGoalUpdateToolName:
		if b.personalData == nil {
			return nil, "", domain.NewError(domain.CodeForbidden, "personal goal writes are unavailable")
		}
		var args cloudDSHGoalUpdateArguments
		if decodeStrictRuntimeResult(raw, &args) != nil || args.ExpectedRevision < 1 || args.ExpectedRevision >= 1<<53-1 {
			return nil, "", domain.InvalidArgument("goal_update", "goal update arguments are invalid")
		}
		currentObject, err := b.goals.Get(ctx, principal, args.GoalID)
		if err != nil {
			return nil, "", err
		}
		if currentObject.ObjectType != personal.ObjectTypeGoal || currentObject.ObjectID != args.GoalID ||
			personal.ValidateObjectBody(currentObject.ObjectType, currentObject.ObjectID, currentObject.Body) != nil {
			return nil, "", domain.NewError(domain.CodeInternal, "stored goal is invalid")
		}
		if currentObject.Revision != args.ExpectedRevision {
			return nil, "", domain.NewError(domain.CodeConflict, "goal revision changed")
		}
		var body cloudDSHGoalBody
		if decodeStrictRuntimeResult(currentObject.Body, &body) != nil {
			return nil, "", domain.NewError(domain.CodeInternal, "stored goal is invalid")
		}
		changed := false
		if args.Title != nil {
			body.Title, changed = *args.Title, true
		}
		if args.Description != nil {
			body.Description, changed = *args.Description, true
		}
		if args.Status != nil {
			body.Status, changed = *args.Status, true
		}
		if len(args.Steps) > 0 && string(args.Steps) != "null" {
			var patches []cloudDSHGoalStepStatusPatch
			if decodeStrictRuntimeResult(args.Steps, &patches) != nil || len(patches) > 100 {
				return nil, "", domain.InvalidArgument("goal_update.steps", "goal step updates are invalid")
			}
			if patches != nil {
				seen := make(map[string]struct{}, len(patches))
				for _, patch := range patches {
					if _, exists := seen[patch.ID]; exists {
						return nil, "", domain.InvalidArgument("goal_update.steps", "goal step updates contain duplicate IDs")
					}
					seen[patch.ID] = struct{}{}
					found := false
					for index := range body.Steps {
						if body.Steps[index].ID == patch.ID {
							body.Steps[index].Status, found = patch.Status, true
							break
						}
					}
					if !found {
						return nil, "", domain.InvalidArgument("goal_update.steps", "goal step ID does not exist")
					}
				}
				changed = changed || len(patches) > 0
			}
		}
		if !changed {
			return nil, "", domain.InvalidArgument("goal_update", "at least one goal field must change")
		}
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			return nil, "", err
		}
		mutation, err := b.personalData.PreparePersonalMutation(principal, PersonalMutationCommand{
			MutationID: mutationID, ObjectType: personal.ObjectTypeGoal, ObjectID: args.GoalID,
			BaseRevision: args.ExpectedRevision, Body: bodyBytes,
		})
		if err != nil {
			return nil, "", err
		}
		return cloudDSHPreparedGoalMutation{mutation: mutation, status: body.Status,
			output: map[string]any{"id": args.GoalID, "revision": args.ExpectedRevision + 1, "status": body.Status}}, "Update personal goal", nil
	case cloudDSHGoalDeleteToolName:
		if b.personalData == nil {
			return nil, "", domain.NewError(domain.CodeForbidden, "personal goal writes are unavailable")
		}
		var args cloudDSHGoalDeleteArguments
		if decodeStrictRuntimeResult(raw, &args) != nil || args.ExpectedRevision < 1 || args.ExpectedRevision >= 1<<53-1 {
			return nil, "", domain.InvalidArgument("goal_delete", "goal deletion arguments are invalid")
		}
		mutation, err := b.personalData.PreparePersonalMutation(principal, PersonalMutationCommand{
			MutationID: mutationID, ObjectType: personal.ObjectTypeGoal, ObjectID: args.GoalID,
			BaseRevision: args.ExpectedRevision, Delete: true,
		})
		if err != nil {
			return nil, "", err
		}
		return cloudDSHPreparedGoalMutation{mutation: mutation,
			output: map[string]any{"id": args.GoalID, "deleted": true, "revision": args.ExpectedRevision + 1}}, "Delete personal goal", nil
	default:
		return nil, "", domain.NewError(domain.CodeForbidden, "goal tool is unavailable")
	}
}

func (b *CloudDSHToolBroker) executeScheduleServiceTool(ctx context.Context, current task.Task, name string, raw json.RawMessage,
	principal identity.Principal, idempotencyKey string) (any, string, error) {
	switch name {
	case cloudDSHScheduleListToolName:
		var args cloudDSHScheduleListArguments
		if decodeStrictRuntimeResult(raw, &args) != nil || args.Limit < 1 || args.Limit > cloudDSHScheduleListLimit || len(args.Cursor) > 1024 {
			return nil, "", domain.InvalidArgument("schedule_list", "schedule list arguments are invalid")
		}
		status := schedule.Status(args.Status)
		if args.Status == "all" {
			status = ""
		}
		page, err := b.schedules.List(ctx, principal, status, PageRequest{Limit: args.Limit, Cursor: args.Cursor})
		if err != nil {
			return nil, "", err
		}
		items := make([]map[string]any, 0, len(page.Items))
		for _, value := range page.Items {
			items = append(items, cloudDSHScheduleSummary(value))
		}
		return map[string]any{"schedules": items, "next_cursor": page.NextCursor}, "List schedules", nil
	case cloudDSHScheduleCreateToolName:
		var args cloudDSHScheduleCreateArguments
		if decodeStrictRuntimeResult(raw, &args) != nil {
			return nil, "", domain.InvalidArgument("schedule_create", "schedule creation arguments are invalid")
		}
		budget := task.Budget{
			ActiveSeconds: current.Budget.ActiveSeconds,
			ToolCalls:     current.Budget.ToolCalls,
			ModelTokens:   current.Budget.ModelTokens,
		}
		if err := budget.Validate(); err != nil {
			return nil, "", domain.InvalidArgument("schedule.budget", "current task budget cannot be used for a schedule")
		}
		var title *string
		if strings.TrimSpace(args.Title) != "" {
			title = &args.Title
		}
		value, err := b.schedules.Create(ctx, principal, CreateScheduleCommand{
			HostID: current.HostID, ModelID: current.ModelID, Title: title, Objective: args.Objective,
			Timing: args.Timing, Budget: budget, MisfirePolicy: schedule.MisfireSkip,
		}, idempotencyKey)
		if err != nil {
			return nil, "", err
		}
		return cloudDSHScheduleSummary(value), "Create schedule", nil
	case cloudDSHScheduleUpdateToolName:
		var args cloudDSHScheduleUpdateArguments
		if decodeStrictRuntimeResult(raw, &args) != nil || args.ExpectedVersion < 1 ||
			(args.Title == nil && args.Objective == nil && args.Timing == nil) {
			return nil, "", domain.InvalidArgument("schedule_update", "schedule update arguments are invalid")
		}
		value, err := b.schedules.Update(ctx, principal, args.ScheduleID, UpdateScheduleCommand{
			ExpectedVersion: args.ExpectedVersion, Title: args.Title, Objective: args.Objective, Timing: args.Timing,
		}, idempotencyKey)
		if err != nil {
			return nil, "", err
		}
		return cloudDSHScheduleSummary(value), "Update schedule", nil
	case cloudDSHScheduleActionToolName:
		var args cloudDSHScheduleActionArguments
		if decodeStrictRuntimeResult(raw, &args) != nil || args.ExpectedVersion < 1 {
			return nil, "", domain.InvalidArgument("schedule_action", "schedule action arguments are invalid")
		}
		result, err := b.schedules.Action(ctx, principal, args.ScheduleID, ScheduleActionCommand{
			Action: schedule.Action(args.Action), ExpectedVersion: args.ExpectedVersion,
			CurrentRunAction: schedule.CurrentRunAction(args.CurrentRunAction),
		}, idempotencyKey)
		if err != nil {
			return nil, "", err
		}
		return map[string]any{"schedule": cloudDSHScheduleSummary(result.Schedule), "current_run": cloudDSHRunSummary(result.CurrentRun)}, "Update schedule state", nil
	case cloudDSHScheduleRunsToolName:
		var args cloudDSHScheduleRunsArguments
		if decodeStrictRuntimeResult(raw, &args) != nil || args.Limit < 1 || args.Limit > cloudDSHScheduleRunsLimit || len(args.Cursor) > 1024 {
			return nil, "", domain.InvalidArgument("schedule_runs", "schedule run history arguments are invalid")
		}
		page, err := b.schedules.Runs(ctx, principal, args.ScheduleID, PageRequest{Limit: args.Limit, Cursor: args.Cursor})
		if err != nil {
			return nil, "", err
		}
		items := make([]map[string]any, 0, len(page.Items))
		for index := range page.Items {
			items = append(items, cloudDSHRunSummary(&page.Items[index]))
		}
		return map[string]any{"runs": items, "next_cursor": page.NextCursor}, "Read schedule run history", nil
	default:
		return nil, "", domain.NewError(domain.CodeForbidden, "schedule tool is unavailable")
	}
}

func cloudDSHScheduleSummary(value schedule.Schedule) map[string]any {
	return map[string]any{
		"id": value.ID, "version": value.Version, "status": value.Status,
		"title":     cloudDSHClipText(value.Title, cloudDSHScheduleTextLimit),
		"objective": cloudDSHClipText(value.Objective, cloudDSHScheduleTextLimit),
		"host_id":   value.HostID, "model_id": value.ModelID, "timing": value.Timing,
		"next_run_at": value.NextRunAt, "last_run": cloudDSHRunSummary(value.LastRun),
		"current_run": cloudDSHRunSummary(value.CurrentRun),
	}
}

func cloudDSHRunSummary(value *schedule.Run) map[string]any {
	if value == nil {
		return nil
	}
	return map[string]any{
		"id": value.ID, "schedule_version": value.ScheduleVersion, "scheduled_at": value.ScheduledAt,
		"status": value.Status, "skip_reason": value.SkipReason, "task_id": value.TaskID,
		"created_at": value.CreatedAt, "finished_at": value.FinishedAt,
	}
}

func cloudDSHStablePersonalID(kind, taskID, sessionID, mutationID string, index int) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("morde-dsh-%s-v1\n%s\n%s\n%s\n%d", kind, taskID, sessionID, mutationID, index)))
	prefix := "g_"
	if kind == "step" {
		prefix = "s_"
	}
	return prefix + hex.EncodeToString(digest[:16])
}

func cloudDSHClipText(value string, maxBytes int) string {
	if len(value) <= maxBytes || !utf8.ValidString(value) {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "…"
}
