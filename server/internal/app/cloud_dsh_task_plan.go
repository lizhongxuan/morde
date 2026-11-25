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
	"morde/server/internal/domain/workspace"
)

const cloudDSHTaskPlanToolName = "morde_task_plan_update"

type cloudDSHTaskPlanInput struct {
	Goal               string          `json:"goal"`
	Scope              string          `json:"scope,omitempty"`
	VerificationMethod string          `json:"verification_method,omitempty"`
	Steps              []task.PlanStep `json:"steps"`
	// FixPlan is optional: when present it marks the task as a bug fix.
	FixPlan *task.FixPlan `json:"fix_plan,omitempty"`
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

// CloudDSHTaskPlanRequirementSourceResolver resolves a plan-tool locator to
// the canonical source identity and digest using the current Task boundary.
// The model-provided digest is never the source of truth.
type CloudDSHTaskPlanRequirementSourceResolver interface {
	ResolveCloudDSHTaskPlanRequirementSource(context.Context, task.Task, task.PlanVerificationRequirementSource) (task.PlanVerificationRequirementSource, error)
}

func cloudDSHTaskPlanDefinition() AgentToolDefinition {
	return AgentToolDefinition{ModelTool: ModelTool{Name: cloudDSHTaskPlanToolName,
		Description: "When a complex task has multiple dependent steps, the main Agent should save a plan before doing the work and revise it when the approach changes. Do not call for simple questions. A plan never requests confirmation or expands permissions; cost is always marked unknown unless separately verified by the application. A submitted plan must carry a complete brief: scope and verification_method are required, and the step completion_criteria together form the acceptance criteria. A plan without them is rejected; complete the missing fields from the task context and resubmit instead of asking the user. For each step in a fix plan, set verification_applicability to check and include a bounded exact verification_check, or set it to not_applicable; do not omit this classification. The verification check includes a bounded exact command and explicit workspace-relative coverage_paths. Set basis to required only when a user message or project file explicitly requires that check. For user_message sources, use locator=current_task_input for the Task's initial accepted user input. morde_current_conversation_history_search is bounded by the Task's fixed through_sequence and can omit messages accepted after Task start. Use a later applied message_id only if the exact ID is present in this Task's authorized input or search results; the server resolves its content and SHA-256. Never infer or invent an ID or ask the user for an internal ID or hash; if the source is unavailable, leave its provenance unresolved rather than claiming it is verified. Project-file sources are accepted only when the server can resolve their immutable snapshot; if resolution fails, keep the required check explicit and do not relabel it risk_selected. Set risk_selected only for checks chosen by risk without claiming they were explicitly required. This metadata is advisory and never authorizes or executes a tool. Older plans without verification_check remain readable and mean unknown verification applicability, not that no checks are required. An optional fix_plan marks the task as a bug fix: it lists the minimal changed files, risks and alternatives, each at most 8 items of at most 512 characters. A fix_plan may carry an optional root_cause: the located root cause summary with the failing reproduction output (at most 4096 characters) and the code lines it points at (at most 8 workspace-relative path:line references); a root cause without any of the three pieces is rejected. A fix task may change files within its authorized scope before verification; H07 separately evaluates each planned check and does not treat a red test as a universal prerequisite.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"goal":{"type":"string","minLength":1,"maxLength":2048},"scope":{"type":"string","maxLength":2048},"verification_method":{"type":"string","maxLength":1024},"fix_plan":{"type":"object","properties":{"changed_files":{"type":"array","maxItems":8,"items":{"type":"string","maxLength":512}},"risks":{"type":"array","maxItems":8,"items":{"type":"string","maxLength":512}},"alternatives":{"type":"array","maxItems":8,"items":{"type":"string","maxLength":512}},"root_cause":{"type":"object","properties":{"summary":{"type":"string","minLength":1,"maxLength":1024},"repro_output":{"type":"string","minLength":1,"maxLength":4096},"code_lines":{"type":"array","minItems":1,"maxItems":8,"items":{"type":"string","maxLength":512}}},"required":["summary","repro_output","code_lines"],"additionalProperties":false}},"additionalProperties":false},"steps":{"type":"array","minItems":1,"maxItems":32,"items":{"type":"object","properties":{"id":{"type":"string","minLength":1,"maxLength":40},"title":{"type":"string","minLength":1,"maxLength":160},"description":{"type":"string","maxLength":1024},"depends_on":{"type":"array","maxItems":8,"items":{"type":"string"}},"outputs":{"type":"array","maxItems":8,"items":{"type":"string","maxLength":256}},"completion_criteria":{"type":"array","maxItems":8,"items":{"type":"string","maxLength":512}},"status":{"type":"string","enum":["pending","in_progress","completed","blocked"]},"verification_applicability":{"type":"string","enum":["check","not_applicable"]},"verification_check":{"type":"object","properties":{"kind":{"type":"string","enum":["test","build","lint","typecheck","other"]},"basis":{"type":"string","enum":["required","risk_selected"]},"command":{"type":"string","minLength":1,"maxLength":1024},"coverage_paths":{"type":"array","minItems":1,"maxItems":8,"items":{"type":"string","maxLength":512}},"requirement_source":{"type":"object","properties":{"kind":{"type":"string","enum":["user_message","project_file"]},"locator":{"type":"string","enum":["current_task_input"]},"message_id":{"type":"string","pattern":"^m[a-z0-9]{7}$"},"workspace_path":{"type":"string","maxLength":512},"workspace_revision":{"type":"integer","minimum":0},"content_sha256":{"type":"string","pattern":"^[0-9a-f]{64}$"}},"required":["kind"],"additionalProperties":false}},"required":["kind","basis","command","coverage_paths"],"additionalProperties":false}},"required":["id","title","status"],"additionalProperties":false}}},"required":["goal","steps"],"additionalProperties":false}`)}, RequiredScope: identity.ScopeTaskRead}
}

// requireCompleteTaskPlanBrief is the write gate (E01): a task that already
// has a submitted plan (revision >= 1) may only mutate the workspace once its
// brief is complete. Tasks without a plan are unaffected, so simple tasks
// that never build a plan are never blocked by this gate. The error names the
// missing brief piece for the agent; it must not surface as a user question.
func requireCompleteTaskPlanBrief(details TaskDetails) error {
	if details.Plan == nil || details.Plan.Revision < 1 {
		return nil
	}
	if _, err := task.BriefFromPlan(*details.Plan); err != nil {
		return domain.InvalidArgument("task_plan.brief", err.Error())
	}
	return nil
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
	if result, handled, err := b.advisoryGateResult(ctx, current, details, call, recovering, checkpointOutput, ""); handled {
		return result, err
	}
	plan := task.Plan{TaskRevision: *current.CurrentRevision, Goal: input.Goal, Scope: input.Scope,
		VerificationMethod: input.VerificationMethod, CostEstimateState: "unknown", Steps: input.Steps, FixPlan: input.FixPlan}
	var checkpointPlan *task.Plan
	if recovering && checkpointOutput != nil {
		var replay task.Plan
		if decodeStrictRuntimeResult([]byte(*checkpointOutput), &replay) != nil {
			return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "checkpointed task plan cannot be decoded")
		}
		plan.Revision = replay.Revision
		checkpointPlan = &replay
	} else if details.Plan != nil && details.Plan.TaskRevision == plan.TaskRevision {
		plan.Revision = details.Plan.Revision + 1
	} else {
		plan.Revision = 1
	}
	if recovering && checkpointOutput == nil {
		if reader, ok := b.store.(CloudDSHTaskPlanReceiptReader); ok {
			stored, err := reader.GetCloudDSHTaskPlanReceipt(ctx, current.ID, current.OwnerID, current.AgentSessionID, call.CallID)
			if err == nil {
				checkpointPlan = &stored
				plan.Revision = stored.Revision
			} else if domain.CodeOf(err) != domain.CodeNotFound {
				return AgentToolResult{}, err
			}
		}
	}
	if err := plan.ValidateSubmissionInput(); err != nil {
		if result, handled, gateErr := b.advisoryGateResult(ctx, current, details, call, recovering, checkpointOutput,
			"Task plan is invalid; no action was executed."); handled {
			return result, gateErr
		}
		return AgentToolResult{}, domain.InvalidArgument("task_plan.requirement_source", err.Error())
	}
	if checkpointPlan != nil {
		if err := preserveCheckpointProjectFileSources(&plan, *checkpointPlan); err != nil {
			return AgentToolResult{}, err
		}
	}
	if err := b.resolveTaskPlanRequirementSources(ctx, current, &plan, recovering); err != nil {
		if result, handled, gateErr := b.advisoryGateResult(ctx, current, details, call, recovering, checkpointOutput,
			"Task plan is invalid; no action was executed."); handled {
			return result, gateErr
		}
		return AgentToolResult{}, domain.InvalidArgument("task_plan.requirement_source", err.Error())
	}
	if checkpointPlan != nil {
		replay := *checkpointPlan
		if replay.TaskRevision != plan.TaskRevision || replay.Validate() != nil ||
			replay.Goal != input.Goal || replay.Scope != input.Scope || replay.VerificationMethod != input.VerificationMethod || !reflect.DeepEqual(replay.Steps, plan.Steps) || !reflect.DeepEqual(replay.FixPlan, input.FixPlan) {
			return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "checkpointed task plan does not match its original call")
		}
		plan = replay
	}
	validationErr := plan.Validate()
	if !recovering {
		validationErr = plan.ValidateSubmission()
	}
	if plan.Revision < 1 || validationErr != nil {
		if result, handled, err := b.advisoryGateResult(ctx, current, details, call, recovering, checkpointOutput,
			"Task plan is invalid; no action was executed."); handled {
			return result, err
		}
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "dsh task plan is invalid or has a dependency cycle")
	}
	// Plan submission gate (E01): once a plan is submitted it must carry a
	// complete brief. The rejection names the missing piece so the agent can
	// complete it from context and resubmit; it never asks the user.
	if _, err := task.BriefFromPlan(plan); err != nil {
		if result, handled, gateErr := b.advisoryGateResult(ctx, current, details, call, recovering, checkpointOutput,
			"Task plan brief is incomplete; no action was executed."); handled {
			return result, gateErr
		}
		return AgentToolResult{}, domain.InvalidArgument("task_plan.brief", err.Error())
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
		if err == nil && (stored.Validate() != nil || stored.TaskRevision != plan.TaskRevision || stored.Goal != input.Goal ||
			stored.Scope != input.Scope || stored.VerificationMethod != input.VerificationMethod || !reflect.DeepEqual(stored.Steps, plan.Steps) || !reflect.DeepEqual(stored.FixPlan, input.FixPlan)) {
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

func preserveCheckpointProjectFileSources(input *task.Plan, checkpoint task.Plan) error {
	if input == nil {
		return domain.NewError(domain.CodeInvalidState, "checkpointed task plan input is missing")
	}
	checkpointSteps := make(map[string]task.PlanStep, len(checkpoint.Steps))
	for _, step := range checkpoint.Steps {
		checkpointSteps[step.ID] = step
	}
	for index := range input.Steps {
		check := input.Steps[index].VerificationCheck
		if check == nil || check.RequirementSource == nil || check.RequirementSource.Kind != task.PlanVerificationRequirementProjectFile {
			continue
		}
		source := check.RequirementSource
		checkpointStep, exists := checkpointSteps[input.Steps[index].ID]
		if !exists || checkpointStep.VerificationCheck == nil || checkpointStep.VerificationCheck.RequirementSource == nil {
			return domain.NewError(domain.CodeInvalidState, "checkpointed project-file source receipt is unavailable")
		}
		checkpointSource := checkpointStep.VerificationCheck.RequirementSource
		if checkpointSource.Kind != task.PlanVerificationRequirementProjectFile || checkpointSource.Locator != "" || checkpointSource.MessageID != "" ||
			checkpointSource.WorkspacePath != source.WorkspacePath || checkpointSource.WorkspaceRevision == nil || source.WorkspaceRevision == nil ||
			*checkpointSource.WorkspaceRevision != *source.WorkspaceRevision || !validCloudDSHTaskPlanSourceDigest(checkpointSource.ContentSHA256) {
			return domain.NewError(domain.CodeInvalidState, "checkpointed project-file source does not match its original locator")
		}
		canonicalSource := *source
		canonicalSource.ContentSHA256 = checkpointSource.ContentSHA256
		checkCopy := *check
		checkCopy.RequirementSource = &canonicalSource
		input.Steps[index].VerificationCheck = &checkCopy
	}
	return nil
}

func (b *CloudDSHToolBroker) resolveTaskPlanRequirementSources(ctx context.Context, current task.Task, plan *task.Plan, recovering bool) error {
	if plan == nil {
		return domain.InvalidArgument("task_plan.requirement_source", "task plan is missing")
	}
	var resolver CloudDSHTaskPlanRequirementSourceResolver
	for stepIndex := range plan.Steps {
		check := plan.Steps[stepIndex].VerificationCheck
		if check == nil || check.Basis != task.PlanVerificationCheckRequired || check.RequirementSource == nil {
			continue
		}
		source := check.RequirementSource
		// A canonical project-file source in a recovered plan is already bound
		// by its durable plan receipt. New submissions are resolved from the
		// immutable Workspace snapshot below; model-provided hashes are ignored.
		if recovering && source.Kind == task.PlanVerificationRequirementProjectFile && source.Locator == "" &&
			validCloudDSHTaskPlanSourceDigest(source.ContentSHA256) {
			continue
		}
		if source.Kind == task.PlanVerificationRequirementProjectFile {
			resolved, err := b.resolveTaskPlanProjectFileSource(ctx, current, *source)
			if err != nil {
				return err
			}
			checkCopy := *check
			checkCopy.RequirementSource = &resolved
			plan.Steps[stepIndex].VerificationCheck = &checkCopy
			continue
		}
		if resolver == nil {
			value, ok := b.store.(CloudDSHTaskPlanRequirementSourceResolver)
			if !ok {
				return domain.NewError(domain.CodeUnsupported, "task plan requirement source resolution is unavailable")
			}
			resolver = value
		}
		resolved, err := resolver.ResolveCloudDSHTaskPlanRequirementSource(ctx, current, *source)
		if err != nil {
			return err
		}
		if resolved.Kind != source.Kind || resolved.Locator != "" {
			return domain.NewError(domain.CodeInvalidState, "task plan requirement source resolver returned a non-canonical source")
		}
		checkCopy := *check
		checkCopy.RequirementSource = &resolved
		plan.Steps[stepIndex].VerificationCheck = &checkCopy
	}
	return nil
}

func (b *CloudDSHToolBroker) resolveTaskPlanProjectFileSource(ctx context.Context, current task.Task,
	source task.PlanVerificationRequirementSource) (task.PlanVerificationRequirementSource, error) {
	if ctx == nil || source.Kind != task.PlanVerificationRequirementProjectFile || source.Locator != "" || source.MessageID != "" ||
		source.WorkspacePath == "" || source.WorkspaceRevision == nil || *source.WorkspaceRevision < 0 {
		return task.PlanVerificationRequirementSource{}, domain.InvalidArgument("task_plan.requirement_source", "project-file source locator is invalid")
	}
	if b.workspaceTools == nil || b.workspaceTools.workspaces == nil {
		return task.PlanVerificationRequirementSource{}, domain.NewError(domain.CodeUnsupported, "project-file source Workspace is unavailable")
	}
	if !cloudDSHWorkspaceToolsEligible(current) {
		return task.PlanVerificationRequirementSource{}, domain.NewError(domain.CodeForbidden, "project-file source is outside the Task Workspace boundary")
	}
	workspaces := b.workspaceTools.workspaces
	var resolved task.PlanVerificationRequirementSource
	resolve := func(operationCtx context.Context) error {
		liveDirectory, err := workspaces.UsesLiveDirectorySource(operationCtx, taskWorkspaceOwner(current), current.WorkspaceID)
		if err != nil {
			return err
		}
		if liveDirectory {
			return domain.NewError(domain.CodeUnsupported, "project-file requirement source needs an immutable Workspace snapshot")
		}
		revision := *source.WorkspaceRevision
		snapshot, err := b.workspaceTools.readSnapshot(operationCtx, current, revision)
		if err != nil {
			return err
		}
		if err := validateCloudDSHWorkspacePath(source.WorkspacePath); err != nil || cloudDSHWorkspacePathIsSensitive(source.WorkspacePath) ||
			workspacePathHasSymlinkAncestor(snapshot.files, source.WorkspacePath, true) {
			return domain.NewError(domain.CodeForbidden, "project-file source path is invalid or unavailable")
		}
		file, found := workspaceFileByPath(snapshot.files, source.WorkspacePath)
		if !found {
			return domain.NewError(domain.CodeNotFound, "project-file source was not found in the Task Workspace revision")
		}
		if workspace.CanonicalFileKind(file.Kind) != workspace.FileKindRegular || file.HardlinkTo != "" {
			return domain.NewError(domain.CodeUnsupported, "project-file source must be a regular, unlinked file")
		}
		// The submitted digest is untrusted. Verify the manifest digest against
		// the immutable blocks, then persist only the verified value.
		proof, err := workspaces.VerifyCommittedArtifact(operationCtx, TaskArtifactProof{
			OwnerID: taskWorkspaceOwner(current), WorkspaceID: current.WorkspaceID, Revision: revision,
			Path: file.Path, SizeBytes: file.SizeBytes, SHA256: file.SHA256,
		})
		if err != nil {
			return err
		}
		resolved = task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementProjectFile,
			WorkspacePath: file.Path, WorkspaceRevision: &revision, ContentSHA256: proof.SHA256}
		return nil
	}
	if current.ProjectID != "" {
		guard, ok := b.store.(projectTaskWorkspaceGuard)
		if !ok {
			return task.PlanVerificationRequirementSource{}, domain.NewError(domain.CodeForbidden, "project Workspace authorization is unavailable")
		}
		if err := guard.WithProjectTaskWorkspaceAccess(ctx, current, false, resolve); err != nil {
			return task.PlanVerificationRequirementSource{}, err
		}
	} else if err := resolve(ctx); err != nil {
		return task.PlanVerificationRequirementSource{}, err
	}
	return resolved, nil
}

func validCloudDSHTaskPlanSourceDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
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
