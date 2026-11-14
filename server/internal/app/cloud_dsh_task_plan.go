package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

const cloudDSHTaskPlanToolName = "morde_task_plan_update"

type cloudDSHTaskPlanInput struct {
	Goal  string          `json:"goal"`
	Steps []task.PlanStep `json:"steps"`
}

type cloudDSHTaskPlanEvent struct {
	StepID       string    `json:"step_id"`
	Sequence     int64     `json:"sequence"`
	TaskRevision int64     `json:"task_revision"`
	PlanRevision int64     `json:"plan_revision"`
	Plan         task.Plan `json:"plan"`
	Detail       struct {
		Tool                 string `json:"tool"`
		CallID               string `json:"call_id"`
		AgentSessionID       string `json:"agent_session_id"`
		AuthorizationVersion int64  `json:"authorization_version"`
		FingerprintSHA256    string `json:"fingerprint_sha256"`
	} `json:"detail"`
}

type CloudDSHTaskPlanReceiptReader interface {
	GetCloudDSHTaskPlanReceipt(context.Context, string, string, string, string) (task.Plan, error)
}

func cloudDSHTaskPlanDefinition() AgentToolDefinition {
	return AgentToolDefinition{ModelTool: ModelTool{Name: cloudDSHTaskPlanToolName,
		Description: "When a complex task has multiple dependent steps, the main Agent should save a plan before doing the work and revise it when the approach changes. Do not call for simple questions. A plan never requests confirmation or expands permissions; cost is always marked unknown unless separately verified by the application.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string","minLength":1,"maxLength":2048},"steps":{"type":"array","minItems":1,"maxItems":32,"items":{"type":"object","properties":{"id":{"type":"string","minLength":1,"maxLength":40},"title":{"type":"string","minLength":1,"maxLength":160},"description":{"type":"string","maxLength":1024},"depends_on":{"type":"array","maxItems":8,"items":{"type":"string"}},"outputs":{"type":"array","maxItems":8,"items":{"type":"string","maxLength":256}},"completion_criteria":{"type":"array","maxItems":8,"items":{"type":"string","maxLength":512}},"status":{"type":"string","enum":["pending","in_progress","completed","blocked"]}},"required":["id","title","status"],"additionalProperties":false}}},"required":["goal","steps"],"additionalProperties":false}`)}, RequiredScope: identity.ScopeTaskRead}
}

func (b *CloudDSHToolBroker) executeTaskPlan(ctx context.Context, claimed task.Task, call ModelToolCall, recovering bool, checkpointOutput *string) (AgentToolResult, error) {
	if ctx == nil || call.CallID == "" || len(call.CallID) > 256 || !utf8.ValidString(call.CallID) || strings.ContainsRune(call.CallID, '\x00') ||
		len(call.Arguments) == 0 || len(call.Arguments) > maxAgentToolArgsBytes || !validAgentJSONObject([]byte(call.Arguments), maxAgentToolArgsBytes) {
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "dsh task plan arguments are invalid")
	}
	var input cloudDSHTaskPlanInput
	if decodeStrictRuntimeResult([]byte(call.Arguments), &input) != nil {
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "dsh task plan arguments are invalid")
	}
	for index := range input.Steps {
		if input.Steps[index].Status == "" {
			input.Steps[index].Status = task.PlanStepPending
		}
		if input.Steps[index].DependsOn == nil {
			input.Steps[index].DependsOn = []string{}
		}
		if input.Steps[index].Outputs == nil {
			input.Steps[index].Outputs = []string{}
		}
		if input.Steps[index].CompletionCriteria == nil {
			input.Steps[index].CompletionCriteria = []string{}
		}
	}
	details, err := b.currentServerTask(ctx, claimed)
	if err != nil {
		return AgentToolResult{}, err
	}
	current := details.Task
	if current.CurrentRevision == nil || current.AgentSessionID == "" || current.Authorization.Version < 1 ||
		call.TaskFence == nil || call.TaskFence.AgentSessionID != current.AgentSessionID ||
		current.Authorization.Version != claimed.Authorization.Version ||
		(claimed.AgentSessionID != "" && current.AgentSessionID != claimed.AgentSessionID) ||
		!sameCloudDSHAuthorization(current.Authorization, claimed.Authorization) {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "task plan authorization or revision changed")
	}
	if err := b.authorizeSkillTask(ctx, current); err != nil {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "task authorization does not allow plan updates")
	}
	plan := task.Plan{TaskRevision: *current.CurrentRevision, Goal: input.Goal, CostEstimateState: "unknown", Steps: input.Steps}
	if recovering && checkpointOutput != nil {
		var replay task.Plan
		if decodeStrictRuntimeResult([]byte(*checkpointOutput), &replay) != nil || replay.TaskRevision != plan.TaskRevision || replay.Validate() != nil ||
			replay.Goal != input.Goal || !reflect.DeepEqual(replay.Steps, input.Steps) {
			return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "checkpointed task plan does not match its original call")
		}
		plan = replay
	} else if details.Plan != nil && details.Plan.TaskRevision == plan.TaskRevision {
		plan.Revision = details.Plan.Revision + 1
	} else {
		plan.Revision = 1
	}
	if plan.Revision < 1 || plan.Validate() != nil {
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "dsh task plan is invalid or has a dependency cycle")
	}
	output, err := json.Marshal(plan)
	if err != nil || len(output) > 48<<10 {
		return AgentToolResult{}, domain.NewError(domain.CodeResultTooLarge, "task plan exceeds its event limit")
	}
	if recovering && checkpointOutput != nil && *checkpointOutput != string(output) {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "checkpointed task plan output changed")
	}
	commit := CloudDSHTaskPlanCommit{TaskID: current.ID, OwnerID: current.OwnerID, AgentSessionID: current.AgentSessionID,
		AuthorizationVersion: current.Authorization.Version, CallID: call.CallID, Plan: plan}
	commit.FingerprintSHA256 = cloudDSHTaskPlanFingerprint(commit)
	if recovering && checkpointOutput == nil {
		reader, ok := b.store.(CloudDSHTaskPlanReceiptReader)
		if !ok {
			return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "task plan replay receipt is unavailable")
		}
		stored, err := reader.GetCloudDSHTaskPlanReceipt(ctx, current.ID, current.OwnerID, current.AgentSessionID, call.CallID)
		if err != nil && domain.CodeOf(err) != domain.CodeNotFound {
			return AgentToolResult{}, err
		}
		if err == nil && (stored.Validate() != nil || stored.TaskRevision != plan.TaskRevision || stored.Goal != input.Goal || !reflect.DeepEqual(stored.Steps, input.Steps)) {
			return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "stored task plan replay receipt is invalid")
		}
		if err == nil {
			plan = stored
			output, err = json.Marshal(plan)
			if err != nil {
				return AgentToolResult{}, err
			}
			commit.Plan = plan
			commit.FingerprintSHA256 = cloudDSHTaskPlanFingerprint(commit)
		}
	}
	usage := cloneTaskUsage(current.Usage)
	if usage.ToolCalls == int64(^uint64(0)>>1) {
		return AgentToolResult{}, domain.NewError(domain.CodeQuotaExceeded, "task tool call budget is exhausted")
	}
	usage.ToolCalls++
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepSucceeded,
		"Update task plan", "Saved the current advisory plan revision.")
	if err != nil {
		return AgentToolResult{}, err
	}
	step, toolCall, err := b.prepareTaskToolStep(ctx, current, step)
	if err != nil {
		return AgentToolResult{}, err
	}
	event := cloudDSHTaskPlanEvent{StepID: step.ID, Sequence: step.Sequence, TaskRevision: plan.TaskRevision, PlanRevision: plan.Revision, Plan: plan}
	event.Detail.Tool, event.Detail.CallID = cloudDSHTaskPlanToolName, call.CallID
	event.Detail.AgentSessionID, event.Detail.AuthorizationVersion, event.Detail.FingerprintSHA256 = current.AgentSessionID, current.Authorization.Version, commit.FingerprintSHA256
	payload, err := json.Marshal(event)
	if err != nil || len(payload) > 64<<10 {
		return AgentToolResult{}, domain.NewError(domain.CodeResultTooLarge, "task plan event exceeds its limit")
	}
	_, err = b.store.CommitTaskProgress(ctx, TaskProgress{TaskID: current.ID, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		Status: task.StatusRunning, CurrentStep: step.ID, Result: current.Result, Usage: usage, Steps: []task.Step{step}, ToolCall: toolCall,
		Event: task.Event{TaskID: current.ID, Type: "task.plan", Payload: payload}, CloudDSHTaskPlan: &commit})
	if err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolSucceeded, Output: string(output)}, nil
}

func cloudDSHTaskPlanFingerprint(value CloudDSHTaskPlanCommit) string {
	value.FingerprintSHA256 = ""
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func validCloudDSHTaskPlanCommit(value CloudDSHTaskPlanCommit) bool {
	return domain.ValidTaskID(value.TaskID) && domain.ValidUserID(value.OwnerID) && value.AgentSessionID != "" && len(value.AgentSessionID) <= 128 &&
		utf8.ValidString(value.AgentSessionID) && value.AuthorizationVersion > 0 && value.CallID != "" && len(value.CallID) <= 256 &&
		utf8.ValidString(value.CallID) && value.Plan.Validate() == nil && len(value.FingerprintSHA256) == 64 &&
		value.FingerprintSHA256 == cloudDSHTaskPlanFingerprint(value)
}

func ValidCloudDSHTaskPlanCommit(value CloudDSHTaskPlanCommit) bool {
	return validCloudDSHTaskPlanCommit(value)
}

func validateCloudDSHTaskPlanEvent(payload []byte, expected CloudDSHTaskPlanCommit) error {
	if len(payload) < 2 || len(payload) > 64<<10 || !validCloudDSHTaskPlanCommit(expected) {
		return domain.NewError(domain.CodeInvalidState, "task plan event is invalid")
	}
	var value cloudDSHTaskPlanEvent
	if decodeStrictRuntimeResult(payload, &value) != nil || value.StepID == "" || value.Sequence < 1 ||
		value.TaskRevision != expected.Plan.TaskRevision || value.PlanRevision != expected.Plan.Revision ||
		!reflect.DeepEqual(value.Plan, expected.Plan) || value.Detail.Tool != cloudDSHTaskPlanToolName ||
		value.Detail.CallID != expected.CallID || value.Detail.AgentSessionID != expected.AgentSessionID ||
		value.Detail.AuthorizationVersion != expected.AuthorizationVersion || value.Detail.FingerprintSHA256 != expected.FingerprintSHA256 {
		return domain.NewError(domain.CodeInvalidState, "task plan event fingerprint changed")
	}
	return nil
}

func ValidateCloudDSHTaskPlanEvent(payload []byte, expected CloudDSHTaskPlanCommit) error {
	return validateCloudDSHTaskPlanEvent(payload, expected)
}

func ValidateCloudDSHTaskPlanEventForStep(payload []byte, expected CloudDSHTaskPlanCommit, stepID string, sequence int64) error {
	if err := validateCloudDSHTaskPlanEvent(payload, expected); err != nil {
		return err
	}
	var event cloudDSHTaskPlanEvent
	if decodeStrictRuntimeResult(payload, &event) != nil || event.StepID != stepID || event.Sequence != sequence {
		return domain.NewError(domain.CodeInvalidState, "task plan event step identity changed")
	}
	return nil
}

func ReadCloudDSHTaskPlanReceipt(payload []byte, taskID, ownerID, sessionID, callID string) (task.Plan, error) {
	var event cloudDSHTaskPlanEvent
	if decodeStrictRuntimeResult(payload, &event) != nil {
		return task.Plan{}, domain.NewError(domain.CodeInvalidState, "task plan receipt is invalid")
	}
	commit := CloudDSHTaskPlanCommit{TaskID: taskID, OwnerID: ownerID, AgentSessionID: event.Detail.AgentSessionID,
		AuthorizationVersion: event.Detail.AuthorizationVersion, CallID: event.Detail.CallID, Plan: event.Plan,
		FingerprintSHA256: event.Detail.FingerprintSHA256}
	if event.Detail.AgentSessionID != sessionID || event.Detail.CallID != callID || validateCloudDSHTaskPlanEvent(payload, commit) != nil {
		return task.Plan{}, domain.NewError(domain.CodeInvalidState, "task plan receipt identity is invalid")
	}
	return event.Plan, nil
}
