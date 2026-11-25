package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

// morde_impact_map (E02) is the read-only agent tool that maps the impact
// surface of one workspace component before it is changed. It reuses the
// workspace snapshot bottom layer of morde_workspace_read/search
// (readSnapshot plus verified file reads); it never writes files, changes
// task state, or needs approval, so it carries the read scope only. Live
// directory workspaces are not supported yet: the tool reports them as
// unsupported instead of reading a moving directory.
const cloudDSHImpactMapToolName = "morde_impact_map"

type cloudDSHImpactMapInput struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Component        string `json:"component"`
}

func cloudDSHImpactMapDefinition() AgentToolDefinition {
	return AgentToolDefinition{ModelTool: ModelTool{Name: cloudDSHImpactMapToolName,
		Description: "Map the impact surface of one workspace component before changing it: its direct dependencies, the files that call into it, and the tests that cover it. Read-only: it never writes files, changes task state, or needs approval. The mapping is heuristic text/import matching, not an exact call graph. Use expected_revision from the current task checkpoint.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"expected_revision":{"type":"integer","minimum":0},"component":{"type":"string","minLength":1,"maxLength":4096}},"required":["expected_revision","component"],"additionalProperties":false}`), Strict: true},
		RequiredScope: identity.ScopeTaskRead}
}

// deferredCloudDSHImpactMapDefinition relaxes expected_revision to -1 for
// deferred tasks, following deferredWorkspaceToolDefinitions: on the first
// call the model sets expected_revision to -1 and the server binds the
// current authorized snapshot.
func deferredCloudDSHImpactMapDefinition(authorization task.Authorization) (AgentToolDefinition, bool) {
	principal := identity.Principal{Kind: identity.Kind(authorization.Kind), Scopes: agentScopeSnapshot(authorization.Scopes)}
	if !principal.HasScope(identity.ScopeTaskRead) {
		return AgentToolDefinition{}, false
	}
	definition := cloudDSHImpactMapDefinition()
	definition.Description = "Use only the Workspace selected for this Task. On the first call set expected_revision to -1; the server fixes the current authorized snapshot. Use the returned revision for later calls. Never invent an operation result. " + definition.Description
	var schema map[string]any
	if json.Unmarshal(definition.Parameters, &schema) != nil {
		return AgentToolDefinition{}, false
	}
	properties, _ := schema["properties"].(map[string]any)
	expected, _ := properties["expected_revision"].(map[string]any)
	if expected == nil {
		return AgentToolDefinition{}, false
	}
	expected["minimum"] = -1
	definition.Parameters, _ = json.Marshal(schema)
	return definition, true
}

// parseCloudDSHImpactMapInput validates morde_impact_map arguments without
// touching the task or workspace, so argument errors are reported before any
// snapshot is read.
func parseCloudDSHImpactMapInput(raw string) (cloudDSHImpactMapInput, error) {
	var input cloudDSHImpactMapInput
	if len(raw) == 0 || len(raw) > maxAgentToolArgsBytes || !validAgentJSONObject([]byte(raw), maxAgentToolArgsBytes) ||
		decodeStrictRuntimeResult([]byte(raw), &input) != nil || input.ExpectedRevision < 0 || input.Component == "" {
		return cloudDSHImpactMapInput{}, domain.InvalidArgument("tool_call.arguments", "dsh impact map arguments are invalid")
	}
	return input, nil
}

func (b *CloudDSHToolBroker) executeImpactMap(ctx context.Context, claimed task.Task, call ModelToolCall, recovering bool, checkpointOutput *string) (AgentToolResult, error) {
	if b.workspaceTools == nil {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "impact map is not available for this task")
	}
	if ctx == nil || call.CallID == "" || len(call.CallID) > 256 || !utf8.ValidString(call.CallID) || strings.ContainsRune(call.CallID, '\x00') {
		return AgentToolResult{}, domain.InvalidArgument("tool_call", "dsh impact map call identity is invalid")
	}
	input, err := parseCloudDSHImpactMapInput(call.Arguments)
	if err != nil {
		return AgentToolResult{}, err
	}
	details, err := b.currentServerTask(ctx, claimed)
	if err != nil {
		return AgentToolResult{}, err
	}
	current := details.Task
	if !cloudDSHWorkspaceToolsEligible(current) || !sameCloudDSHAuthorization(current.Authorization, claimed.Authorization) {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "impact map is not authorized for this task")
	}
	liveDirectory, err := b.workspaceTools.workspaces.UsesLiveDirectorySource(ctx, taskWorkspaceOwner(current), current.WorkspaceID)
	if err != nil {
		return AgentToolResult{}, err
	}
	if liveDirectory {
		return AgentToolResult{}, domain.NewError(domain.CodeUnsupported, "impact map does not support live Workspace directories")
	}
	operationKey := agentOperationKey(call.CallID)
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepRunning,
		"Map impact of "+input.Component, "Reading the task Workspace snapshot.")
	if err != nil {
		return AgentToolResult{}, err
	}
	usage := cloneTaskUsage(current.Usage)
	if !recovering {
		if usage.ToolCalls >= current.Budget.ToolCalls {
			step.Status, step.Summary = task.StepWaitingUser, "The task tool-call budget is exhausted."
			if err := b.commitProgress(ctx, current, step, usage, task.StatusWaitingUser, "tool_call_budget", "task.waiting_user", nil); err != nil {
				return AgentToolResult{}, err
			}
			return AgentToolResult{Status: AgentToolNeedsUser, WaitingReason: "tool_call_budget"}, nil
		}
		usage.ToolCalls++
		if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
			"tool": call.Name, "operation_key": operationKey, "phase": "prepare",
		}); err != nil {
			return AgentToolResult{}, err
		}
	}
	current.Usage = usage
	result, err := b.runImpactMap(ctx, current, input)
	if err != nil {
		return AgentToolResult{}, err
	}
	if recovering && checkpointOutput != nil && *checkpointOutput != dshToolResultText(result) {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "checkpointed impact map result does not match the committed Workspace view")
	}
	if result.Status == AgentToolSucceeded {
		step.Status, step.Summary = task.StepSucceeded, "The impact map lists the component dependencies, callers, and covering tests."
	} else {
		step.Status, step.Summary = task.StepFailed, "The impact map could not be built for the requested component."
	}
	detail := map[string]any{"tool": call.Name, "operation_key": operationKey, "operation_status": result.Status, "component": input.Component}
	if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", detail); err != nil {
		return AgentToolResult{}, err
	}
	result.Output = boundedToolOutput(result.Output)
	return result, nil
}

// runImpactMap reads the task Workspace snapshot at the requested revision
// and runs the pure impact analysis over it. It never writes.
func (b *CloudDSHToolBroker) runImpactMap(ctx context.Context, current task.Task, input cloudDSHImpactMapInput) (AgentToolResult, error) {
	snapshot, err := b.workspaceTools.readSnapshot(ctx, current, input.ExpectedRevision)
	if err != nil {
		return cloudDSHWorkspaceFailure(err), nil
	}
	impact, err := AnalyzeImpactMap(newWorkspaceSnapshotFileIndex(b.workspaceTools, snapshot), input.Component)
	if err != nil {
		return cloudDSHWorkspaceFailure(err), nil
	}
	encoded, err := json.Marshal(impact)
	if err != nil {
		return AgentToolResult{}, fmt.Errorf("encode impact map result: %w", err)
	}
	if len(encoded) > maxAgentToolOutputBytes {
		return AgentToolResult{}, domain.NewError(domain.CodeResultTooLarge, "impact map output exceeds the task limit")
	}
	return AgentToolResult{Status: AgentToolSucceeded, Output: string(encoded)}, nil
}

// workspaceSnapshotFileIndex adapts a workspace snapshot to the FileIndex
// read by AnalyzeImpactMap. Files larger than the workspace read limit,
// linked files, sensitive paths, and files that are not valid UTF-8 text are
// not readable, following the workspace search bounds; the analysis skips
// files it cannot read.
type workspaceSnapshotFileIndex struct {
	tools    *cloudDSHWorkspaceTools
	snapshot cloudDSHWorkspaceSnapshot
	byPath   map[string]workspace.File
}

func newWorkspaceSnapshotFileIndex(tools *cloudDSHWorkspaceTools, snapshot cloudDSHWorkspaceSnapshot) *workspaceSnapshotFileIndex {
	index := &workspaceSnapshotFileIndex{tools: tools, snapshot: snapshot, byPath: make(map[string]workspace.File, len(snapshot.files))}
	for _, file := range snapshot.files {
		index.byPath[file.Path] = file
	}
	return index
}

func (x *workspaceSnapshotFileIndex) Files() []string {
	files := make([]string, 0, len(x.snapshot.files))
	for _, file := range x.snapshot.files {
		if workspace.CanonicalFileKind(file.Kind) != workspace.FileKindRegular || file.HardlinkTo != "" || cloudDSHWorkspacePathIsSensitive(file.Path) {
			continue
		}
		files = append(files, file.Path)
	}
	sort.Strings(files)
	return files
}

func (x *workspaceSnapshotFileIndex) ReadFile(filePath string) (string, error) {
	if cloudDSHWorkspacePathIsSensitive(filePath) {
		return "", domain.NewError(domain.CodeForbidden, "sensitive workspace paths are unavailable")
	}
	file, found := x.byPath[filePath]
	if !found {
		return "", domain.NewError(domain.CodeNotFound, "workspace file is not in the snapshot")
	}
	if workspace.CanonicalFileKind(file.Kind) != workspace.FileKindRegular || file.HardlinkTo != "" || file.SizeBytes > cloudDSHWorkspaceReadMaxBytes {
		return "", domain.NewError(domain.CodeRequestTooLarge, "workspace file is linked or exceeds the read limit")
	}
	content, err := x.tools.readVerifiedFileContent(x.snapshot.workspaceID, file)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(content) {
		return "", domain.NewError(domain.CodeWorkspaceConflict, "workspace file is not valid UTF-8 text")
	}
	return string(content), nil
}
