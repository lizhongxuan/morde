package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/record"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

// CloudDSHToolBroker routes dsh calls through Morde's durable task gates. It does not run a model loop.
type CloudDSHToolBroker struct {
	store                    TaskStore
	runtime                  *RuntimeToolExecutor
	historyCursorKey         []byte
	historyCursorInstanceID  string
	recordCapture            *RecordCaptureService
	mcpCredentials           MCPConnectorCredentialProvider
	mcpClient                MCPConnectorToolClient
	builtinSearch            MCPConnectorToolClient
	builtinSearchCache       *cloudDSHBuiltinSearchCache
	hub                      *HubService
	personalData             *PersonalMutationService
	personalFiles            *PersonalAttachmentService
	workspaceTools           *cloudDSHWorkspaceTools
	personalSync             cloudDSHPersonalSnapshotReader
	scheduledPersonalContext cloudDSHScheduledPersonalContextReader
	goals                    *GoalService
	schedules                *ScheduleService
	projectContext           *ProjectService
	stepDetailSealer         *Sealer
}

// AgentToolCallAdmissionStatus reports whether a DSH call may begin under the
// current Task inbox fence. Superseded is a successful no-dispatch outcome.
type AgentToolCallAdmissionStatus string

const (
	AgentToolCallAdmitted   AgentToolCallAdmissionStatus = "admitted"
	AgentToolCallSuperseded AgentToolCallAdmissionStatus = "superseded"
	// AgentToolCallBaselineUnverifiable is terminal for a legacy child Agent
	// whose root checkpoint baseline predates durable fence evidence. The caller
	// must settle that child as failed instead of restarting it against an
	// unverifiable history boundary.
	AgentToolCallBaselineUnverifiable AgentToolCallAdmissionStatus = "baseline_unverifiable"
)

type AgentToolCallAdmission struct {
	Status          AgentToolCallAdmissionStatus
	PendingMessages []AgentConversationMessage
	Reason          string
}

// CloudDSHToolCallAdmitter is called by the Worker before executing each
// DSH tool_call frame, including read-only and internal tools.
type CloudDSHToolCallAdmitter interface {
	AdmitToolCall(context.Context, task.Task, ModelToolCall, bool) (AgentToolCallAdmission, error)
}

// AgentToolCallAdmissionStore serializes call admission with steer acceptance
// through the Task row lock.
type AgentToolCallAdmissionStore interface {
	AdmitAgentToolCall(context.Context, ModelToolCall, bool) (AgentToolCallAdmission, error)
}

// ErrTaskInputPending identifies a final dispatch gate that committed a
// terminal zero-dispatch marker because a steer was accepted first.
var ErrTaskInputPending = errors.New("task input superseded the tool call")

type TaskInputPendingError struct {
	PendingMessages []AgentConversationMessage
}

func (e *TaskInputPendingError) Error() string { return ErrTaskInputPending.Error() }

func (e *TaskInputPendingError) Is(target error) bool { return target == ErrTaskInputPending }

func (e *TaskInputPendingError) Unwrap() error { return ErrTaskInputPending }

func NewTaskInputPendingError(messages []AgentConversationMessage) error {
	return &TaskInputPendingError{PendingMessages: append([]AgentConversationMessage(nil), messages...)}
}

const TaskAgentToolSupersededOperationResult = `{"morde_dispatch":"superseded","version":1}`

const AgentToolSuperseded AgentToolResultStatus = "superseded"

func (b *CloudDSHToolBroker) AdmitToolCall(ctx context.Context, claimed task.Task, call ModelToolCall,
	recovering bool) (AgentToolCallAdmission, error) {
	if ctx == nil || call.CallID == "" || len(call.CallID) > 256 || len(call.Name) == 0 || len(call.Name) > 128 ||
		call.TaskFence == nil || call.TaskFence.TaskID != claimed.ID || call.TaskFence.OwnerID != claimed.OwnerID ||
		call.TaskFence.InitiatorID != claimed.InitiatorID {
		return AgentToolCallAdmission{}, domain.InvalidArgument("tool_call", "dsh tool call admission identity is invalid")
	}
	store, ok := b.store.(AgentToolCallAdmissionStore)
	if !ok {
		return AgentToolCallAdmission{}, domain.NewError(domain.CodeInvalidState, "task tool-call admission storage is unavailable")
	}
	return store.AdmitAgentToolCall(ctx, call, recovering)
}

func (b *CloudDSHToolBroker) SetTaskStepDetailSealer(sealer *Sealer) error {
	if b == nil || sealer == nil {
		return domain.NewError(domain.CodeInvalidArgument, "task step detail sealer is required")
	}
	if b.stepDetailSealer != nil && b.stepDetailSealer != sealer {
		return domain.NewError(domain.CodeInvalidState, "task step detail sealer cannot be replaced")
	}
	b.stepDetailSealer = sealer
	return nil
}

// SetHistorySearchCursorSigning configures stable, service-bound signatures
// for paged current-conversation history search results.
func (b *CloudDSHToolBroker) SetHistorySearchCursorSigning(serviceInstanceID string, dataKey []byte) error {
	if b == nil || !domain.ValidServiceInstanceID(serviceInstanceID) || len(dataKey) != 32 {
		return domain.InvalidArgument("history_cursor_signing", "history cursor signing configuration is invalid")
	}
	if len(b.historyCursorKey) > 0 && (b.historyCursorInstanceID != serviceInstanceID || !bytes.Equal(b.historyCursorKey, dataKey)) {
		return domain.NewError(domain.CodeInvalidState, "history cursor signing configuration cannot be replaced")
	}
	b.historyCursorInstanceID = serviceInstanceID
	b.historyCursorKey = append([]byte(nil), dataKey...)
	return nil
}

type CloudDSHTaskCompletion struct {
	AgentInstanceID           string
	AgentGeneration           int64
	AuthorizationVersion      int64
	AgentSessionID            string
	CheckpointThroughSequence int64
	CheckpointDigest          []byte
	Result                    string
	Inputs                    []ModelInput
	ValidationClaims          []TaskResultClaim
	StreamMessageID           string
	StreamTurnID              string
}

func NewCloudDSHToolBroker(store TaskStore, runtime *RuntimeToolExecutor, mcpCredentials MCPConnectorCredentialProvider, mcpClient MCPConnectorToolClient, hubs ...*HubService) (*CloudDSHToolBroker, error) {
	return newCloudDSHToolBroker(store, runtime, nil, mcpCredentials, mcpClient, hubs...)
}

// NewCloudDSHToolBrokerWithRecordCapture enables best-effort full-record capture after successful task completion.
func NewCloudDSHToolBrokerWithRecordCapture(store TaskStore, runtime *RuntimeToolExecutor, mcpCredentials MCPConnectorCredentialProvider,
	mcpClient MCPConnectorToolClient, recordCapture *RecordCaptureService, hubs ...*HubService) (*CloudDSHToolBroker, error) {
	if recordCapture == nil {
		return nil, errors.New("record capture dependency is required")
	}
	return newCloudDSHToolBroker(store, runtime, recordCapture, mcpCredentials, mcpClient, hubs...)
}

// NewCloudDSHToolBrokerWithPersonalData connects personal context to the same
// account-scoped business services used by the Portal APIs.
func NewCloudDSHToolBrokerWithPersonalData(store TaskStore, runtime *RuntimeToolExecutor, mcpCredentials MCPConnectorCredentialProvider,
	mcpClient MCPConnectorToolClient, recordCapture *RecordCaptureService, personalData *PersonalMutationService,
	personalSync cloudDSHPersonalSnapshotReader, goals *GoalService, personalFiles *PersonalAttachmentService,
	projectContext *ProjectService, hubs ...*HubService) (*CloudDSHToolBroker, error) {
	return NewCloudDSHToolBrokerWithPersonalDataAndSchedules(store, runtime, mcpCredentials, mcpClient, recordCapture,
		personalData, personalSync, goals, personalFiles, projectContext, nil, hubs...)
}

// NewCloudDSHToolBrokerWithPersonalDataAndSchedules adds the existing account-scoped
// ScheduleService to the broker. Passing no ScheduleService keeps schedule tools unavailable.
func NewCloudDSHToolBrokerWithPersonalDataAndSchedules(store TaskStore, runtime *RuntimeToolExecutor, mcpCredentials MCPConnectorCredentialProvider,
	mcpClient MCPConnectorToolClient, recordCapture *RecordCaptureService, personalData *PersonalMutationService,
	personalSync cloudDSHPersonalSnapshotReader, goals *GoalService, personalFiles *PersonalAttachmentService,
	projectContext *ProjectService, schedules *ScheduleService, hubs ...*HubService) (*CloudDSHToolBroker, error) {
	if personalData == nil || personalSync == nil || goals == nil || personalFiles == nil {
		return nil, errors.New("personal Task context dependencies are required")
	}
	broker, err := NewCloudDSHToolBrokerWithRecordCapture(store, runtime, mcpCredentials, mcpClient, recordCapture, hubs...)
	if err != nil {
		return nil, err
	}
	broker.personalData, broker.personalSync, broker.goals, broker.personalFiles = personalData, personalSync, goals, personalFiles
	broker.schedules = schedules
	broker.projectContext = projectContext
	return broker, nil
}

func newCloudDSHToolBroker(store TaskStore, runtime *RuntimeToolExecutor, recordCapture *RecordCaptureService,
	mcpCredentials MCPConnectorCredentialProvider, mcpClient MCPConnectorToolClient, hubs ...*HubService) (*CloudDSHToolBroker, error) {
	if store == nil || runtime == nil {
		return nil, errors.New("dsh tool broker dependencies are required")
	}
	if (mcpCredentials == nil) != (mcpClient == nil) {
		return nil, errors.New("MCP connector execution dependencies must be configured together")
	}
	if len(hubs) > 1 {
		return nil, errors.New("at most one Hub service may be configured")
	}
	broker := &CloudDSHToolBroker{
		store: store, runtime: runtime, recordCapture: recordCapture,
		mcpCredentials: mcpCredentials, mcpClient: mcpClient,
		workspaceTools: newCloudDSHWorkspaceTools(runtime.workspaces),
	}
	if reader, ok := store.(cloudDSHScheduledPersonalContextReader); ok {
		broker.scheduledPersonalContext = reader
	}
	if len(hubs) == 1 {
		broker.hub = hubs[0]
	}
	return broker, nil
}

func (b *CloudDSHToolBroker) currentTaskDetails(ctx context.Context, claimed task.Task) (TaskDetails, error) {
	details, err := b.store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil {
		return TaskDetails{}, err
	}
	current := details.Task
	if current.HostID != claimed.HostID || current.WorkspaceID != claimed.WorkspaceID || current.ApplicationID != claimed.ApplicationID {
		refreshed, _, routed, refreshErr := b.refreshDeferredTask(ctx, claimed)
		if refreshErr != nil {
			return TaskDetails{}, refreshErr
		}
		if routed {
			claimed = refreshed
			details, err = b.store.GetTask(ctx, claimed.ID, claimed.OwnerID)
			if err != nil {
				return TaskDetails{}, err
			}
			current = details.Task
		}
	}
	if current.ID != claimed.ID || current.OwnerID != claimed.OwnerID || current.InitiatorID != claimed.InitiatorID ||
		current.ProjectID != claimed.ProjectID || current.ConversationID != claimed.ConversationID ||
		current.HostID != claimed.HostID || current.ExecutionLocation != claimed.ExecutionLocation ||
		current.ApplicationID != claimed.ApplicationID || current.ModelID != claimed.ModelID || current.WorkspaceID != claimed.WorkspaceID {
		return TaskDetails{}, domain.NewError(domain.CodeInvalidState, "cloud task identity changed")
	}
	if current.Status == task.StatusRunning && (current.Lease.Owner != claimed.Lease.Owner ||
		current.Lease.Version != claimed.Lease.Version || current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now())) {
		return TaskDetails{}, domain.NewError(domain.CodeInvalidState, "cloud task lease changed")
	}
	if current.ConversationContext == nil && claimed.ConversationContext != nil {
		contextValue := claimed.ConversationContext
		scope := "personal"
		if current.ProjectID != "" {
			scope = "project"
		}
		if contextValue.OwnerID != current.OwnerID || contextValue.InitiatorID != current.InitiatorID ||
			contextValue.ProjectID != current.ProjectID || contextValue.ConversationID != current.ConversationID ||
			contextValue.Scope != scope || !sameCloudDSHAuthorization(current.Authorization, claimed.Authorization) {
			return TaskDetails{}, domain.NewError(domain.CodeForbidden, "cloud task conversation context authorization changed")
		}
		// The worker's claimed task owns the immutable conversation snapshot; GetTask omits it.
		details.Task.ConversationContext = contextValue
	}
	if details.Task.ConversationContext != nil && details.Task.ConversationContext.Summary != nil {
		validator, ok := b.store.(TaskConversationSummaryValidator)
		if !ok {
			return TaskDetails{}, domain.NewError(domain.CodeInvalidState, "Task summary authorization cannot be revalidated")
		}
		if err := validator.ValidateTaskConversationSummary(ctx, details.Task); err != nil {
			return TaskDetails{}, err
		}
	}
	return details, nil
}

func (b *CloudDSHToolBroker) Definitions(ctx context.Context, claimed task.Task) ([]AgentToolDefinition, error) {
	definitions := make([]AgentToolDefinition, 0)
	if claimed.ToolsDisabled {
		return definitions, nil
	}
	claimed, target, routed, err := b.refreshDeferredTask(ctx, claimed)
	if err != nil {
		return nil, err
	}
	goalFollowupReadOnly := cloudDSHGoalFollowupReadOnlyTask(claimed)
	if routed {
		definitions = append(definitions, deferredRuntimeToolDefinitions(target, claimed.Authorization)...)
	} else if claimed.HostID != "" {
		hostDefinitions, err := b.runtime.Definitions(ctx, claimed)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, hostDefinitions...)
	}
	var selectedMCP []AgentToolDefinition
	if !goalFollowupReadOnly {
		selectedMCP = b.selectedMCPDefinitions(claimed)
	}
	if claimed.ExecutionLocation == "server" {
		details, err := b.currentServerTask(ctx, claimed)
		if err != nil {
			return nil, err
		}
		if cloudDSHCurrentConversationHistoryEligible(details.Task) {
			definitions = append(definitions, cloudDSHCurrentConversationHistoryDefinition())
			if _, ok := b.store.(currentConversationMessageChunkStore); ok {
				definitions = append(definitions, cloudDSHCurrentConversationMessageReadDefinition())
				if ValidConversationMessageSourceManifest(details.Task.ConversationContext.CurrentMessageSource) {
					definitions = append(definitions, cloudDSHCurrentConversationMessageSearchDefinition())
				}
			}
		}
		if !goalFollowupReadOnly {
			definitions = append(definitions, cloudDSHTaskPlanDefinition())
		}
		if b.workspaceTools != nil && routed && details.Task.HostID == "" && target.HostID == "" && target.WorkspaceID != "" {
			liveDirectory := false
			if details.Task.ProjectID == "" {
				liveDirectory, err = b.workspaceTools.workspaces.UsesLiveDirectorySource(ctx, taskWorkspaceOwner(details.Task), target.WorkspaceID)
				if err != nil {
					return nil, err
				}
			}
			definitions = append(definitions, deferredWorkspaceToolDefinitions(details.Task.Authorization, liveDirectory)...)
			if !liveDirectory {
				if definition, ok := deferredCloudDSHImpactMapDefinition(details.Task.Authorization); ok {
					definitions = append(definitions, definition)
				}
			}
		} else if b.workspaceTools != nil && cloudDSHWorkspaceToolsEligible(details.Task) {
			liveDirectory, err := b.workspaceTools.workspaces.UsesLiveDirectorySource(ctx, taskWorkspaceOwner(details.Task), details.Task.WorkspaceID)
			if err != nil {
				return nil, err
			}
			definitions = append(definitions, cloudDSHWorkspaceToolDefinitions(*details.Task.WorkspaceCheckpointRevision, liveDirectory)...)
			if !liveDirectory {
				definitions = append(definitions, cloudDSHImpactMapDefinition())
			}
		}
		if !goalFollowupReadOnly && b.builtinSearch != nil && b.authorizeBuiltinSearch(ctx, details.Task) == nil {
			definitions = append(definitions, cloudDSHBuiltinSearchDefinitions()...)
		}
		if b.authorizeSkillTask(ctx, details.Task) == nil {
			hostAvailable := domain.ValidHostID(claimed.HostID) || routed && !target.Unavailable && domain.ValidHostID(target.HostID)
			definitions = append(definitions, cloudDSHSkillReadDefinition(hostAvailable))
		}
		if b.personalData != nil && cloudDSHPersonalContextEligible(details.Task) {
			definitions = append(definitions, cloudDSHPersonalUpdateDefinition())
			principal, ok := cloudDSHPersonalPrincipal(details.Task)
			if ok {
				memoryEnabled, _, err := b.personalMemorySetting(ctx, principal)
				if err != nil {
					return nil, err
				}
				if memoryEnabled {
					definitions = append(definitions, cloudDSHConversationHistorySearchDefinition())
					definitions = append(definitions, cloudDSHRecentHistorySummaryDefinition())
				}
			}
		}
		if b.goals != nil && cloudDSHPersonalContextEligible(details.Task) {
			definitions = append(definitions, cloudDSHGoalToolDefinitions()...)
		}
		if b.schedules != nil && cloudDSHScheduleContextEligible(details.Task) {
			definitions = append(definitions, cloudDSHScheduleToolDefinitions()...)
		}
		attachments, err := b.currentTaskAttachments(ctx, details.Task)
		if err != nil {
			return nil, err
		}
		if len(attachments) > 0 {
			definitions = append(definitions, cloudDSHTaskAttachmentReadDefinition(attachments))
		}
		if !goalFollowupReadOnly {
			hubDefinitions, err := b.hubTaskDefinitions(ctx, details.Task)
			if err != nil {
				return nil, err
			}
			available := maxCloudDSHToolDefinitions - len(definitions) - 1
			if available >= len(selectedMCP) {
				available -= len(selectedMCP)
			}
			if available > 0 {
				definitions = append(definitions, hubDefinitions[:min(len(hubDefinitions), available)]...)
			}
		}
	}
	if goalFollowupReadOnly {
		definitions = filterGoalFollowupReadOnlyTools(definitions)
	}
	definitions = appendSelectedMCPDefinitionsWithinLimit(definitions, selectedMCP)
	if !goalFollowupReadOnly && len(definitions)+1 > maxCloudDSHToolDefinitions {
		return nil, domain.NewError(domain.CodeResultTooLarge, "the task has more tools than the model can accept")
	}
	seen := make(map[string]struct{}, len(definitions)+1)
	for _, definition := range definitions {
		if definition.Name == agentAskUserToolName {
			return nil, errors.New("host tool conflicts with the Agent clarification tool")
		}
		if _, exists := seen[definition.Name]; exists {
			return nil, errors.New("cloud dsh tools contain a duplicate name")
		}
		seen[definition.Name] = struct{}{}
	}
	if goalFollowupReadOnly {
		return definitions, nil
	}
	return append(definitions, agentAskUserTool), nil
}

func cloudDSHGoalFollowupReadOnlyTask(value task.Task) bool {
	return value.ExecutionLocation == "server" && value.HostID == "" && value.ProjectID == "" &&
		identity.Kind(value.Authorization.Kind) == identity.KindScheduleTask &&
		identity.IsGoalFollowupScheduleTaskExecutionScopeSet(agentScopeSnapshot(value.Authorization.Scopes))
}

func goalFollowupReadOnlyToolAllowed(name string) bool {
	switch name {
	case cloudDSHSkillReadToolName, cloudDSHTaskAttachmentReadToolName,
		cloudDSHWorkspaceListToolName, cloudDSHWorkspaceReadToolName, cloudDSHWorkspaceSearchToolName, cloudDSHImpactMapToolName:
		return true
	default:
		return false
	}
}

func filterGoalFollowupReadOnlyTools(definitions []AgentToolDefinition) []AgentToolDefinition {
	filtered := make([]AgentToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		if goalFollowupReadOnlyToolAllowed(definition.Name) {
			filtered = append(filtered, definition)
		}
	}
	return filtered
}

func appendSelectedMCPDefinitionsWithinLimit(definitions, selectedMCP []AgentToolDefinition) []AgentToolDefinition {
	if len(definitions)+len(selectedMCP)+1 > maxCloudDSHToolDefinitions {
		return definitions
	}
	return append(definitions, selectedMCP...)
}

// Complete only commits a dsh result after the worker has confirmed its
// process tree stopped. Host tasks must also prove file writes through the
// operation ledger, Host runtime, and committed Morde Workspace.
func (b *CloudDSHToolBroker) Complete(ctx context.Context, claimed task.Task, completion CloudDSHTaskCompletion) error {
	if ctx == nil || len(completion.CheckpointDigest) != 32 || completion.CheckpointThroughSequence < 0 ||
		len(completion.Inputs) > int(task.MaxToolCalls)*2 || len(completion.Result) > agentUserVisibleResultBytes ||
		(completion.StreamMessageID == "") != (completion.StreamTurnID == "") ||
		completion.StreamMessageID != "" && (!domain.ValidMessageID(completion.StreamMessageID) || !domain.ValidStepID(completion.StreamTurnID)) {
		return domain.InvalidArgument("dsh_completion", "cloud dsh completion evidence is invalid")
	}
	if len(completion.ValidationClaims) > 0 && ValidateTaskResultClaims(completion.Result, completion.ValidationClaims) != nil {
		completion.ValidationClaims = nil
	}
	inputBytes := len(completion.Result)
	for _, input := range completion.Inputs {
		inputBytes += len(input.Arguments) + len(input.Output) + len(input.CallID) + len(input.Name) + len(input.Type)
		if inputBytes > 12<<20 {
			return domain.InvalidArgument("dsh_completion", "cloud dsh completion evidence exceeds its limit")
		}
	}
	claimed, _, _, err := b.refreshDeferredTask(ctx, claimed)
	if err != nil {
		return err
	}
	details, err := b.currentTaskDetails(ctx, claimed)
	if err != nil {
		return err
	}
	current := details.Task
	completionFence := &TaskAgentCompletionFence{
		AgentSessionID: completion.AgentSessionID, AgentInstanceID: completion.AgentInstanceID,
		AgentGeneration: completion.AgentGeneration, AuthorizationVersion: completion.AuthorizationVersion,
		CheckpointThroughSequence: completion.CheckpointThroughSequence,
		CheckpointDigest:          append([]byte(nil), completion.CheckpointDigest...),
	}
	if current.Status != task.StatusRunning || current.ExecutionLocation != "server" ||
		current.AgentInstanceID != completion.AgentInstanceID || current.AgentGeneration != completion.AgentGeneration ||
		current.Authorization.Version != completion.AuthorizationVersion || current.AgentSessionID != completion.AgentSessionID ||
		current.AgentSessionID == "" {
		return domain.NewError(domain.CodeInvalidState, "cloud dsh completion checkpoint changed")
	}
	// The persisted intent comes from the task's routing or submission
	// contract. A missing or invalid value cannot be reconstructed safely
	// from the user's prose at completion time.
	if !current.FileDeliveryIntent.Valid() {
		return domain.NewError(domain.CodeInvalidState, "task file delivery intent is unavailable")
	}
	if current.CheckpointThroughSequence != completion.CheckpointThroughSequence || !bytes.Equal(current.CheckpointDigest, completion.CheckpointDigest) {
		step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "result", task.StepSucceeded,
			"Complete cloud task", "The cloud dsh completion checkpoint changed before result commit.")
		if err != nil {
			return err
		}
		useCloudDSHStreamResultStep(&step, details, completion)
		// Minimal-diff recheck (E06): a stale completion still may not
		// deliver files changed outside the fix plan allow set.
		if evidence, evidenceErr := agentFileWriteEvidence(completion.Inputs); evidenceErr == nil {
			if violations := minimalDiffOutOfScopeFiles(details.Plan, agentFileEvidencePaths(evidence)); len(violations) > 0 {
				return b.pauseCompletion(ctx, current, step, "minimal_diff_violation", "The task changed files outside fix_plan.changed_files: "+strings.Join(violations, ", "))
			}
		}
		// Test gate (E07): after the minimal-diff recheck, a stale
		// completion still may not deliver an untested fix.
		if paused, err := b.pauseForTestGate(ctx, current, step, details); err != nil {
			return err
		} else if paused {
			return nil
		}
		return b.completeWithRecordCapture(ctx, current, step, current.Usage, completion.Result, completion.ValidationClaims,
			map[string]any{"source": "cloud_dsh"}, "", nil, completionFence)
	}
	fileIntent := current.FileDeliveryIntent
	if current.HostID == "" {
		if cloudDSHHasHostToolHistory(completion.Inputs) {
			return domain.NewError(domain.CodeInvalidState, "hostless dsh task contains computer tool history")
		}
		verifiedArtifacts, err := b.verifyHostlessWorkspaceToolHistory(ctx, current, completion.Inputs)
		if err != nil {
			return err
		}
		workspaceWrite := len(verifiedArtifacts) > 0
		// Minimal-diff recheck (E06): hostless writes are summarized by the
		// verified artifacts, not by agentFileWriteEvidence, which only
		// covers host file_write calls. Any verified file outside the fix
		// plan allow set pauses completion instead of delivering.
		if violations := minimalDiffOutOfScopeFiles(details.Plan, verifiedArtifactPaths(verifiedArtifacts)); len(violations) > 0 {
			step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "result", task.StepSucceeded,
				"Complete cloud task", "The cloud dsh session completed and its process tree stopped.")
			if err != nil {
				return err
			}
			return b.pauseCompletion(ctx, current, step, "minimal_diff_violation", "The task changed files outside fix_plan.changed_files: "+strings.Join(violations, ", "))
		}
		if fileIntent != task.FileDeliveryNone && !workspaceWrite {
			return b.pauseForFileDelivery(ctx, current, details, fileIntent, current.WorkspaceID != "")
		}
		if !workspaceWrite && HasUnverifiedFileDeliveryClaim(completion.Result) {
			return b.pauseForUnverifiedFileClaim(ctx, current, details)
		}
		step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "result", task.StepSucceeded,
			"Complete cloud task", "The cloud dsh session completed and its process tree stopped.")
		if err != nil {
			return err
		}
		useCloudDSHStreamResultStep(&step, details, completion)
		artifacts, err := prepareCloudDSHVerifiedArtifacts(details, step, verifiedArtifacts)
		if err != nil {
			return err
		}
		return b.completeWithRecordCapture(ctx, current, step, current.Usage, completion.Result, completion.ValidationClaims,
			map[string]any{"source": "cloud_dsh"}, "", artifacts, completionFence)
	}

	evidence, evidenceErr := agentFileWriteEvidence(completion.Inputs)
	if evidenceErr == nil && !cloudDSHHasUnverifiedFileWrite(completion.Inputs, evidence) && len(evidence) == 0 {
		if fileIntent != task.FileDeliveryNone && fileIntent != task.FileDeliveryRequired {
			return b.pauseForFileDelivery(ctx, current, details, fileIntent, current.WorkspaceID != "")
		}
		if fileIntent != task.FileDeliveryRequired && HasUnverifiedFileDeliveryClaim(completion.Result) {
			return b.pauseForUnverifiedFileClaim(ctx, current, details)
		}
		if fileIntent != task.FileDeliveryRequired {
			step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "result", task.StepSucceeded,
				"Complete cloud task", "The cloud dsh session completed without producing a Workspace file.")
			if err != nil {
				return err
			}
			useCloudDSHStreamResultStep(&step, details, completion)
			// A Host fix plan still requires E07 when it records no file writes.
			if paused, err := b.pauseForTestGate(ctx, current, step, details); err != nil {
				return err
			} else if paused {
				return nil
			}
			return b.completeWithRecordCapture(ctx, current, step, current.Usage, completion.Result, completion.ValidationClaims,
				map[string]any{"source": "cloud_dsh"}, "", nil, completionFence)
		}
	}
	step, err := cloudDSHCompletionVerificationStep(current, details.Steps)
	if err != nil {
		return err
	}
	if err := b.commitProgress(ctx, current, step, current.Usage, task.StatusRunning, "", "task.step", map[string]any{
		"phase": "verify", "source": "cloud_dsh",
	}); err != nil {
		return err
	}
	// Minimal-diff recheck (E06): the evidence summarizes every file this
	// task wrote, including files written before the fix plan was submitted,
	// so a plan that does not list them cannot smuggle them into delivery.
	if evidenceErr == nil {
		if violations := minimalDiffOutOfScopeFiles(details.Plan, agentFileEvidencePaths(evidence)); len(violations) > 0 {
			return b.pauseCompletion(ctx, current, step, "minimal_diff_violation", "The task changed files outside fix_plan.changed_files: "+strings.Join(violations, ", "))
		}
	}
	// Test gate (E07): after the minimal-diff recheck and before regression
	// verification, an untested fix never reaches Verify or delivery.
	if paused, err := b.pauseForTestGate(ctx, current, step, details); err != nil {
		return err
	} else if paused {
		return nil
	}
	if evidenceErr != nil || cloudDSHHasUnverifiedFileWrite(completion.Inputs, evidence) ||
		(len(evidence) == 0 && fileIntent != task.FileDeliveryRequired) {
		return b.pauseCompletion(ctx, current, step, "output_verification_pending", "The task output has no verifiable successful Workspace file write.")
	}
	fence, err := b.runtime.readFence(ctx, current, nil)
	if err != nil {
		return b.pauseCompletion(ctx, current, step, "output_verification_pending", "Host authorization or control changed before output verification.")
	}
	var verification AgentVerificationResult
	var verifyErr error
	if len(evidence) == 0 {
		verification, verifyErr = b.runtime.VerifyTaskWorkspaceOutput(ctx, current, step.ID, completion.Inputs, fence)
	} else {
		verification, verifyErr = b.runtime.Verify(ctx, AgentVerificationRequest{
			Task: current, StepID: step.ID, Claim: completion.Result, Evidence: evidence, Fence: fence,
		})
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var pendingInput *TaskInputPendingError
	if errors.As(verifyErr, &pendingInput) {
		return verifyErr
	}
	if verifyErr == nil && verification.Status == AgentVerifyUnknown && verification.WaitingReason == "workspace_input_changed" &&
		len(verification.Artifacts) == 0 {
		_, err := b.pauseForWorkspaceInputChange(ctx, current, step, current.Usage, "file.verify")
		return err
	}
	if verifyErr == nil && len(evidence) == 0 && verification.Status == AgentNotVerified {
		return b.pauseCompletion(ctx, current, step, "file_delivery_required",
			safeStepSummary(verification.Summary, "No new or modified Host Workspace file was produced for delivery."))
	}
	if verifyErr != nil || verification.Status != AgentVerified || validateAgentVerificationArtifacts(verification) != nil || len(verification.Artifacts) == 0 {
		return b.pauseCompletion(ctx, current, step, "output_verification_pending",
			safeStepSummary(verification.Summary, "Host or committed Workspace could not verify the task output."))
	}
	artifacts, err := prepareCloudDSHVerifiedArtifacts(details, step, verification.Artifacts)
	if err != nil {
		return err
	}
	step.Status = task.StepSucceeded
	step.Summary = safeStepSummary(verification.Summary, "Host and committed Workspace verified the task output.")
	return b.completeWithRecordCapture(ctx, current, step, current.Usage, completion.Result, completion.ValidationClaims,
		map[string]any{"source": "cloud_dsh", "verification": verification.Summary}, "", artifacts, completionFence)
}

func (b *CloudDSHToolBroker) pauseForUnverifiedFileClaim(ctx context.Context, current task.Task, details TaskDetails) error {
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "verification", task.StepUnknown,
		"核验文件交付", "没有可验证的文件产物，无法确认文件已保存。")
	if err != nil {
		return err
	}
	return b.pauseCompletion(ctx, current, step, "output_verification_pending", step.Summary)
}

// The final Task result may reuse a stream only when the current snapshot
// belongs to this exact message and revision and is a prefix of the verified
// checkpointed result. The store repeats the turn/identity check at commit.
func useCloudDSHStreamResultStep(step *task.Step, details TaskDetails, completion CloudDSHTaskCompletion) {
	snapshot := details.AssistantMessage
	if completion.StreamMessageID == "" || completion.StreamTurnID == "" || snapshot == nil ||
		snapshot.Message.ID != completion.StreamMessageID || snapshot.Message.Content == "" ||
		(snapshot.Status != AssistantMessageStreaming && snapshot.Status != AssistantMessageInterrupted) ||
		details.Task.CurrentRevision == nil || snapshot.TaskRevision == nil ||
		*details.Task.CurrentRevision != *snapshot.TaskRevision || !strings.HasPrefix(completion.Result, snapshot.Message.Content) {
		return
	}
	step.ID = completion.StreamTurnID
}

func prepareCloudDSHVerifiedArtifacts(details TaskDetails, step task.Step, verifiedArtifacts []AgentVerifiedArtifact) ([]task.Artifact, error) {
	if len(verifiedArtifacts) == 0 {
		return nil, nil
	}
	current := details.Task
	if len(verifiedArtifacts) > maxAgentVerifiedArtifacts || step.TaskID != current.ID || !domain.ValidStepID(step.ID) {
		return nil, domain.NewError(domain.CodeInvalidArgument, "verified artifact metadata is invalid")
	}
	prepared := make([]task.Artifact, 0, len(verifiedArtifacts))
	for _, verified := range verifiedArtifacts {
		if err := validateAgentVerificationArtifacts(AgentVerificationResult{Status: AgentVerified, Artifacts: []AgentVerifiedArtifact{verified}}); err != nil {
			return nil, domain.NewError(domain.CodeInvalidArgument, "verified artifact metadata is invalid")
		}
		if verified.WorkspaceID != current.WorkspaceID {
			return nil, domain.NewError(domain.CodeInvalidState, "verified artifact is outside the task workspace")
		}
		var existing *task.Artifact
		for index := range details.Artifacts {
			candidate := &details.Artifacts[index]
			if candidate.TaskID == current.ID && candidate.BatchItemID == "" && candidate.BatchAttempt == 0 && candidate.Path == verified.Path {
				existing = candidate
				break
			}
		}
		if existing != nil {
			if existing.Label != verified.Label || existing.MediaType != verified.MediaType || existing.WorkspaceID != verified.WorkspaceID ||
				existing.Revision != verified.Revision || existing.SizeBytes != verified.SizeBytes || existing.SHA256 != verified.SHA256 {
				return nil, domain.NewError(domain.CodeInvalidState, "verified artifact metadata changed during recovery")
			}
			prepared = append(prepared, *existing)
			continue
		}
		artifactID, err := domain.NewArtifactID()
		if err != nil {
			return nil, err
		}
		prepared = append(prepared, task.Artifact{
			ID: artifactID, TaskID: current.ID, StepID: step.ID, SourceCallID: verified.SourceCallID, Label: verified.Label, Path: verified.Path,
			WorkspaceID: verified.WorkspaceID, Revision: verified.Revision, SizeBytes: verified.SizeBytes, SHA256: verified.SHA256,
			MediaType: verified.MediaType,
		})
	}
	return prepared, nil
}

func (b *CloudDSHToolBroker) completeWithRecordCapture(ctx context.Context, current task.Task, step task.Step,
	usage task.Usage, result string, validationClaims []TaskResultClaim, detail any, observationFingerprint string,
	artifacts []task.Artifact, completionFence *TaskAgentCompletionFence) error {
	if len(result) > agentUserVisibleResultBytes || !validUTF8(result) {
		step.Status = task.StepFailed
		step.Summary = "The cloud task returned invalid text output."
		usage = boundedTaskUsage(current, usage)
		payload, err := agentStepEvent(step, usage, map[string]any{"error": "invalid_model_output"})
		if err != nil {
			return err
		}
		_, err = b.store.CommitTaskProgress(ctx, TaskProgress{
			TaskID: current.ID, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
			Status: task.StatusFailed, CurrentStep: step.ID, Result: "Task stopped: invalid_model_output", Usage: usage,
			Steps: []task.Step{step}, AgentCompletionFence: completionFence,
			Event: task.Event{TaskID: current.ID, Type: "task.result", Payload: payload},
		})
		return err
	}
	usage = boundedTaskUsage(current, usage)
	payload, err := agentStepEvent(step, usage, detail)
	if err != nil {
		return err
	}
	progress := TaskProgress{
		TaskID: current.ID, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		Status: task.StatusCompleted, CurrentStep: step.ID, Result: result, Usage: usage,
		PublishResultMessage:   strings.TrimSpace(result) != "",
		ObservationFingerprint: observationFingerprint, Steps: []task.Step{step}, Artifacts: artifacts,
		ValidationClaims:     validationClaims,
		AgentCompletionFence: completionFence,
		Event:                task.Event{TaskID: current.ID, Type: "task.result", Payload: payload},
	}
	_, err = b.store.CommitTaskProgress(ctx, progress)
	if isOptionalTaskValidationEvidenceError(err) {
		progress.ValidationClaims = nil
		_, err = b.store.CommitTaskProgress(ctx, progress)
	}
	if err != nil {
		if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
			_, pauseErr := b.pauseForWorkspaceInputChange(ctx, current, step, usage, "task.complete")
			return pauseErr
		}
		return err
	}
	b.captureCompletedTurn(ctx, current, result)
	return nil
}

func (b *CloudDSHToolBroker) captureCompletedTurn(ctx context.Context, current task.Task, result string) {
	if b.recordCapture == nil {
		return
	}
	kind := identity.Kind(current.Authorization.Kind)
	if kind != identity.KindSession && kind != identity.KindClient {
		return
	}
	principal := identity.Principal{
		UserID: current.OwnerID, Kind: kind, TokenID: current.Authorization.TokenID,
		ClientInstallationID: current.Authorization.ClientInstallationID,
	}
	payload, err := json.Marshal(struct {
		Objective string `json:"objective"`
		Result    string `json:"result"`
	}{Objective: current.Objective, Result: result})
	if err != nil {
		return
	}
	captureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	_, _ = b.recordCapture.Capture(captureCtx, principal, record.CaptureInput{
		Purpose: record.PurposeFullRecord, Kind: record.ItemTurn, TaskID: current.ID, Payload: payload,
	})
}

func cloudDSHCompletionVerificationStep(current task.Task, steps []task.Step) (task.Step, error) {
	var resumed task.Step
	for _, step := range steps {
		if step.Kind == "verification" && step.Status == task.StepRunning && step.Title == "Verify cloud task output" &&
			step.Sequence > resumed.Sequence {
			resumed = step
		}
	}
	if resumed.ID != "" {
		return resumed, nil
	}
	return newAgentStep(current.ID, maxStepSequence(steps)+1, "verification", task.StepRunning,
		"Verify cloud task output", "Checking successful file writes against the Host and committed Workspace.")
}

// shortcut: fixed phrase matching can miss uncommon wording, upgrade when broader language coverage is required.
func ClassifyFileDeliveryIntent(objective string, hasWorkspace bool) task.FileDeliveryIntent {
	text := strings.ToLower(strings.TrimSpace(objective))
	if text == "" {
		return task.FileDeliveryUnclear
	}
	if fileDeliveryReadOutput.MatchString(text) {
		return task.FileDeliveryNone
	}
	noFileRequested := fileDeliveryNegative.MatchString(text)
	text = fileDeliveryNegative.ReplaceAllString(text, " ")
	for _, phrase := range []string{
		"不要生成文件", "不要创建文件", "不要写文件", "不要写入文件", "不要保存文件", "不要输出文件",
		"不要在工作区写文件", "不要在工作区保存文件",
		"不需要生成文件", "无需生成文件", "不用生成文件", "不需要写文件", "无需写文件", "不用写文件",
		"不需要保存文件", "无需保存文件", "不用保存文件",
		"do not create a file", "don't create a file", "do not create the file", "don't create the file",
		"do not create any file", "don't create any file",
		"do not generate a file", "don't generate a file", "do not generate the file", "don't generate the file",
		"do not generate any file", "don't generate any file",
		"do not write a file", "don't write a file", "do not write the file", "don't write the file",
		"do not write to a file", "don't write to a file", "do not save a file", "don't save a file",
		"do not save the file", "don't save the file", "do not save to a file", "don't save to a file",
		"do not output a file", "don't output a file",
	} {
		if strings.Contains(text, phrase) {
			noFileRequested = true
			text = strings.ReplaceAll(text, phrase, " ")
		}
	}
	noFileRequested = noFileRequested || containsAny(text, "不需要文件", "无需文件", "不用文件", "不要文件", "无需附件", "只要文字", "只需要文字",
		"只在聊天回答", "只在聊天里回答", "只在聊天中回答", "直接回答", "直接回复文字", "在聊天里回答", "no file needed", "no need for a file",
		"no attachment", "no file", "without a file", "text only", "text-only", "just answer", "answer in chat", "reply in chat")
	if fileDeliveryQuestionBeforeAction(text) {
		if containsAny(text, "保存为", "保存到", "保存至", "写入工作区", "提供下载", "下载到", "save as", "save to", "to the workspace") {
			return task.FileDeliveryUnclear
		}
		return task.FileDeliveryNone
	}
	outputAction := containsAny(text, "生成", "生产", "创建", "新建", "制作", "做一份", "做个", "做一个", "导出", "下载", "保存", "另存", "交付", "提供",
		"输出", "上传", "写成", "写", "撰写", "整理成", "draft", "generate", "create", "make", "produce", "export", "download", "save", "deliver",
		"attach", "provide", "output", "write", "prepare")
	fileTarget := fileDeliveryTargetAfterAction(text)
	workspaceDestination := containsAny(text, "在工作区", "到工作区", "写入工作区", "保存到工作区", "保存至工作区", "in the workspace", "in the attached workspace", "in the cloud workspace", "to the workspace", "to the attached workspace", "to the cloud workspace", "into the workspace", "into the attached workspace", "into the cloud workspace")
	if !fileTarget && !workspaceDestination && containsAny(text, "一段总结", "写一段总结", "总结一下", "a short summary", "a brief summary", "summarize it in chat") {
		return task.FileDeliveryNone
	}
	if outputAction && workspaceDestination {
		if noFileRequested {
			return task.FileDeliveryUnclear
		}
		return task.FileDeliveryRequired
	}
	if outputAction && fileTarget {
		if noFileRequested {
			return task.FileDeliveryUnclear
		}
		return task.FileDeliveryRequired
	}
	deliverable := containsAny(text, "报告", "方案", "计划", "总结", "论文", "表格", "幻灯片", "演示文稿", "代码", "脚本", "源码", "项目", "仓库", "report", "plan", "summary", "spreadsheet", "slides", "code", "script", "project", "repository", "repo", "deliverable", "artifact")
	editAction := containsAny(text, "修改", "修复", "实现", "开发", "重构", "编辑", "更新", "改造", "fix", "implement", "develop", "refactor", "edit", "modify", "update")
	codeTarget := containsAny(text, "代码", "源码", "脚本", "code", "source", "script")
	projectTarget := containsAny(text, "项目", "仓库", "project", "repository", "repo")
	if hasWorkspace && ((editAction && (codeTarget || projectTarget)) || (outputAction && codeTarget)) {
		if noFileRequested {
			return task.FileDeliveryUnclear
		}
		return task.FileDeliveryRequired
	}
	if noFileRequested {
		return task.FileDeliveryNone
	}
	if (outputAction || editAction) && deliverable {
		return task.FileDeliveryUnclear
	}
	if fileTarget && !containsAny(text, "读取", "阅读", "查看", "审阅", "检查", "分析", "总结", "解释", "read", "review", "inspect", "analyze", "summarize", "explain") {
		return task.FileDeliveryUnclear
	}
	if outputAction {
		return task.FileDeliveryUnclear
	}
	return task.FileDeliveryNone
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func (b *CloudDSHToolBroker) pauseForFileDelivery(ctx context.Context, current task.Task, details TaskDetails, intent task.FileDeliveryIntent, hasWorkspace bool) error {
	if intent == task.FileDeliveryUnclear {
		version := nextClarificationVersion(details.ClarificationRequests)
		if version < 1 {
			return domain.NewError(domain.CodeInvalidState, "clarification version is exhausted")
		}
		initialSteps, request, err := newFileDeliveryIntentClarification(current.ID, hasWorkspace, maxStepSequence(details.Steps), version)
		if err != nil {
			return err
		}
		clarificationStep := initialSteps[1]
		usage := cloneTaskUsage(current.Usage)
		payload, err := agentStepEvent(clarificationStep, usage, map[string]any{
			"clarification_id": request.ID, "clarification_version": request.Version,
			"call_id": request.CallID, "source": "cloud_dsh",
		})
		if err != nil {
			return err
		}
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
		defer cancel()
		_, err = b.store.CommitTaskProgress(persistCtx, TaskProgress{
			TaskID: current.ID, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
			Status: task.StatusWaitingUser, CurrentStep: clarificationStep.ID, Result: current.Result,
			WaitingReason: "clarification_required", Usage: usage, Steps: initialSteps,
			Event:                task.Event{TaskID: current.ID, Type: "task.waiting_user", Payload: payload},
			ClarificationRequest: request,
		})
		return err
	}
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "verification", task.StepRunning,
		"Check requested file delivery", "")
	if err != nil {
		return err
	}
	summary := "The task requests a file deliverable, but no verified Workspace file was produced."
	if !hasWorkspace {
		summary = "The task requests a file deliverable, but no Host Workspace is available to store it."
	}
	return b.pauseCompletion(ctx, current, step, "file_delivery_required", summary)
}

func cloudDSHHasUnverifiedFileWrite(inputs []ModelInput, evidence []AgentFileEvidence) bool {
	attempts := make(map[string]int)
	for _, input := range inputs {
		if input.Type == "function_call" && input.Name == runtimeToolFileWrite {
			attempts[input.CallID]++
		}
	}
	if len(attempts) == 0 {
		return len(evidence) != 0
	}
	callByOperation := make(map[string]string, len(attempts))
	for callID, count := range attempts {
		if callID == "" || count != 1 {
			return true
		}
		callByOperation[agentOperationKey(callID)] = callID
	}
	verified := make(map[string]struct{}, len(evidence))
	for _, item := range evidence {
		callID, exists := callByOperation[item.OperationKey]
		if !exists {
			return true
		}
		if _, duplicate := verified[callID]; duplicate {
			return true
		}
		verified[callID] = struct{}{}
	}
	return len(verified) != len(attempts)
}

func cloudDSHHasHostToolHistory(inputs []ModelInput) bool {
	for _, input := range inputs {
		if input.Type != "function_call" {
			continue
		}
		if input.Name == agentAskUserToolName || input.Name == cloudDSHSkillReadToolName || cloudDSHWorkspaceToolName(input.Name) ||
			strings.HasPrefix(input.Name, cloudDSHMCPToolPrefix) {
			continue
		}
		return true
	}
	return false
}

func (b *CloudDSHToolBroker) pauseCompletion(ctx context.Context, current task.Task, step task.Step, reason, summary string) error {
	step.Status, step.Summary = task.StepUnknown, summary
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	return b.commitProgress(persistCtx, current, step, current.Usage, task.StatusWaitingUser, reason, "task.waiting_user", map[string]any{
		"phase": "verify", "source": "cloud_dsh",
	})
}

// pauseForTestGate pauses completion with test_gate_failed when the E07 test
// gate fails for a fix task. It reports whether it paused: false when the gate
// passes, when the task has no fix plan, or when the task is hostless.
// Hostless tasks are exempt because they have no shell tool, so no test run
// can ever be recorded; the missing hostless test gate is a known
// limitation. The caller reuses its own result step, as the minimal-diff
// recheck does, and returns nil when paused.
func (b *CloudDSHToolBroker) pauseForTestGate(ctx context.Context, current task.Task, step task.Step, details TaskDetails) (bool, error) {
	if current.HostID == "" {
		return false, nil
	}
	// shortcut: impact maps are heuristic and cannot prove coverage; the
	// independent H07 review stays pending until immutable coverage exists.
	missing := testGateMissing(details)
	if missing == "" {
		return false, nil
	}
	return true, b.pauseCompletion(ctx, current, step, "test_gate_failed", "The fix cannot complete: "+missing)
}

func (b *CloudDSHToolBroker) Execute(ctx context.Context, claimed task.Task, call ModelToolCall) (AgentToolResult, error) {
	ctx = b.toolActivityContext(ctx, claimed, call)
	result, err := b.execute(ctx, claimed, call, false, nil)
	return mapTaskInputPendingResult(result, err)
}

// Reconcile handles a tool call reconstructed from a pinned dsh session. An
// existing operation is read from the same durable ledger without consuming
// another tool-call budget unit. A missing result may be run only when no
// operation exists; a checkpointed result must match a terminal ledger result.
func (b *CloudDSHToolBroker) Reconcile(ctx context.Context, claimed task.Task, call ModelToolCall, checkpointOutput *string) (AgentToolResult, error) {
	ctx = b.toolActivityContext(ctx, claimed, call)
	result, err := b.execute(ctx, claimed, call, true, checkpointOutput)
	return mapTaskInputPendingResult(result, err)
}

func mapTaskInputPendingResult(result AgentToolResult, err error) (AgentToolResult, error) {
	var pending *TaskInputPendingError
	if !errors.As(err, &pending) {
		return result, err
	}
	return AgentToolResult{Status: AgentToolSuperseded,
		SupersedingMessages: append([]AgentConversationMessage(nil), pending.PendingMessages...)}, nil
}

func (b *CloudDSHToolBroker) toolActivityContext(ctx context.Context, claimed task.Task, call ModelToolCall) context.Context {
	if ctx == nil || call.TaskFence == nil {
		return ctx
	}
	current := claimed
	if current.CurrentRevision == nil {
		details, err := b.store.GetTask(ctx, claimed.ID, claimed.OwnerID)
		if err != nil {
			return ctx
		}
		current = details.Task
	}
	return withCloudDSHToolActivity(ctx, current, call, b.stepDetailSealer)
}

func withBoundTaskToolActivity(ctx context.Context, bound task.Task) context.Context {
	activity, ok := cloudDSHToolActivityFromContext(ctx)
	if !ok {
		return ctx
	}
	before := activity.current
	if bound.ID != before.ID || bound.OwnerID != before.OwnerID || bound.InitiatorID != before.InitiatorID ||
		bound.ProjectID != before.ProjectID || bound.ConversationID != before.ConversationID || bound.ModelID != before.ModelID ||
		bound.ExecutionLocation != before.ExecutionLocation || bound.AgentInstanceID != before.AgentInstanceID ||
		bound.AgentGeneration != before.AgentGeneration || bound.AgentSessionID != before.AgentSessionID ||
		bound.Lease.Owner != before.Lease.Owner || bound.Lease.Version != before.Lease.Version ||
		!sameTaskInputRevision(bound.CurrentRevision, before.CurrentRevision) ||
		!sameCloudDSHAuthorization(bound.Authorization, before.Authorization) ||
		(before.HostID != "" && bound.HostID != before.HostID) ||
		(before.WorkspaceID != "" && bound.WorkspaceID != before.WorkspaceID) {
		return ctx
	}
	activity.current = bound
	return context.WithValue(ctx, cloudDSHToolActivityKey{}, activity)
}

func (b *CloudDSHToolBroker) execute(ctx context.Context, claimed task.Task, call ModelToolCall, recovering bool, checkpointOutput *string) (AgentToolResult, error) {
	if ctx == nil || call.CallID == "" || len(call.CallID) > 256 || len(call.Name) == 0 || len(call.Name) > 128 {
		return AgentToolResult{}, domain.InvalidArgument("tool_call", "dsh tool call identity is invalid")
	}
	if cloudDSHGoalFollowupReadOnlyTask(claimed) && !goalFollowupReadOnlyToolAllowed(call.Name) {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "tool is not available to read-only Goal follow-up Tasks")
	}
	if call.Name == cloudDSHTaskPlanToolName {
		return b.executeTaskPlan(ctx, claimed, call, recovering, checkpointOutput)
	}
	claimed, target, routed, err := b.refreshDeferredTask(ctx, claimed)
	if err != nil {
		return AgentToolResult{}, err
	}
	if routed && target.HostID != "" && cloudDSHWorkspaceToolName(call.Name) {
		return AgentToolResult{Status: AgentToolFailed,
			Output: "This task targets a cloud computer. Use that Host's authorized Workspace tools instead."}, nil
	}
	if mapping, hostTool := runtimeToolMappingByName(call.Name); routed && hostTool && !mapping.internal {
		if recovering && checkpointOutput != nil && claimed.HostID == "" {
			return b.pauseDeferredTarget(ctx, claimed, "target_result_unknown")
		}
		arguments, parseErr := validateAgentToolArguments(call.Arguments)
		if parseErr != nil {
			if result, handled, handleErr := b.returnInvalidRuntimeToolArguments(ctx, claimed, call, recovering, checkpointOutput); handled {
				return result, handleErr
			}
			return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "dsh tool arguments are invalid")
		}
		call.Arguments = string(arguments)
		bound, resolvedCall, reason, bindErr := b.bindDeferredRuntimeTarget(ctx, claimed, target, mapping, call)
		if bindErr != nil {
			return AgentToolResult{}, bindErr
		}
		if reason != "" {
			return b.pauseDeferredTarget(ctx, claimed, reason)
		}
		ctx = withBoundTaskToolActivity(ctx, bound)
		claimed, call = bound, resolvedCall
	}
	if call.Name == agentAskUserToolName {
		return b.clarify(ctx, claimed, call, recovering)
	}
	if cloudDSHWorkspaceToolName(call.Name) {
		if routed {
			bound, resolvedCall, reason, bindErr := b.prepareDeferredWorkspaceCall(ctx, claimed, target, call, recovering, checkpointOutput)
			if bindErr != nil {
				return AgentToolResult{}, bindErr
			}
			if reason != "" {
				return b.pauseDeferredTarget(ctx, claimed, reason)
			}
			ctx = withBoundTaskToolActivity(ctx, bound)
			claimed, call = bound, resolvedCall
		}
		return b.executeWorkspaceTool(ctx, claimed, call, recovering, checkpointOutput)
	}
	if call.Name == cloudDSHImpactMapToolName {
		if routed {
			bound, resolvedCall, reason, bindErr := b.prepareDeferredWorkspaceCall(ctx, claimed, target, call, recovering, checkpointOutput)
			if bindErr != nil {
				return AgentToolResult{}, bindErr
			}
			if reason != "" {
				return b.pauseDeferredTarget(ctx, claimed, reason)
			}
			ctx = withBoundTaskToolActivity(ctx, bound)
			claimed, call = bound, resolvedCall
		}
		return b.executeImpactMap(ctx, claimed, call, recovering, checkpointOutput)
	}
	if call.Name == cloudDSHSkillReadToolName {
		return b.executeSkillRead(ctx, claimed, call, recovering, checkpointOutput)
	}
	if call.Name == cloudDSHPersonalUpdateToolName {
		return b.executePersonalUpdate(ctx, claimed, call, recovering, checkpointOutput)
	}
	if call.Name == cloudDSHConversationHistorySearchToolName {
		return b.executeConversationHistorySearch(ctx, claimed, call, recovering, checkpointOutput)
	}
	if call.Name == cloudDSHCurrentHistorySearchToolName {
		return b.executeCurrentConversationHistorySearch(ctx, claimed, call, recovering, checkpointOutput)
	}
	if call.Name == cloudDSHCurrentMessageReadToolName {
		return b.executeCurrentConversationMessageRead(ctx, claimed, call, recovering, checkpointOutput)
	}
	if call.Name == cloudDSHCurrentMessageSearchToolName {
		return b.executeCurrentConversationMessageSearch(ctx, claimed, call, recovering, checkpointOutput)
	}
	if call.Name == cloudDSHRecentHistorySummaryToolName {
		return b.executeRecentHistorySummary(ctx, claimed, call, recovering, checkpointOutput)
	}
	if cloudDSHGoalScheduleToolName(call.Name) {
		return b.executeGoalScheduleTool(ctx, claimed, call, recovering, checkpointOutput)
	}
	if call.Name == cloudDSHTaskAttachmentReadToolName {
		return b.executeTaskAttachmentRead(ctx, claimed, call, recovering, checkpointOutput)
	}
	if call.Name == cloudDSHWebSearchToolName || call.Name == cloudDSHWebReadToolName {
		return b.executeBuiltinSearch(ctx, claimed, call, recovering, checkpointOutput)
	}
	if strings.HasPrefix(call.Name, "morde_hub_") {
		return b.executeHubTaskAction(ctx, claimed, call, recovering, checkpointOutput)
	}
	if call.Name == cloudDSHMCPDiscoverToolName || call.Name == cloudDSHMCPCallToolName {
		return b.executeSelectedMCP(ctx, claimed, call, recovering, checkpointOutput)
	}
	current, err := b.runtime.currentTask(ctx, claimed)
	if err != nil {
		return AgentToolResult{}, err
	}
	arguments, err := validateAgentToolArguments(call.Arguments)
	if err != nil {
		if result, handled, handleErr := b.returnInvalidRuntimeToolArguments(ctx, current, call, recovering, checkpointOutput); handled {
			return result, handleErr
		}
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "dsh tool arguments are invalid")
	}
	if mapping, runtimeTool := runtimeToolMappingByName(call.Name); runtimeTool && !mapping.internal &&
		!validRuntimeToolArguments(mapping, arguments) {
		if result, handled, handleErr := b.returnInvalidRuntimeToolArguments(ctx, current, call, recovering, checkpointOutput); handled {
			return result, handleErr
		}
		// A schema-invalid Runtime tool call must never reach Prepare or the
		// checkpoint reconciler unless the exact Task/Agent fence and current
		// authorization were verified by returnInvalidRuntimeToolArguments.
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "dsh tool arguments are invalid")
	}
	hash, err := agentOperationHash(call.Name, arguments)
	if err != nil {
		return AgentToolResult{}, err
	}
	details, err := b.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		return AgentToolResult{}, err
	}
	if call.Name == runtimeToolFileWrite {
		if result, handled, gateErr := b.advisoryGateResult(ctx, current, details, call, recovering, checkpointOutput, ""); handled {
			return result, gateErr
		}
	}
	// Write gates (E01, E03, E06): a host file write is refused while the task has
	// a submitted plan with an incomplete brief, while no failing reproduction
	// test has run on a fix task, and while the written file is outside the
	// fix plan's changed_files allow set. The brief is checked first, then the
	// failing test, then the minimal diff. Read-only calls are unaffected.
	if call.Name == runtimeToolFileWrite {
		if err := requireCompleteTaskPlanBrief(details); err != nil {
			if result, handled, gateErr := b.advisoryGateResult(ctx, current, details, call, recovering, checkpointOutput,
				"Task plan brief is incomplete; no action was executed."); handled {
				return result, gateErr
			}
			return AgentToolResult{}, err
		}
		if err := requireFailingTestRecord(details); err != nil {
			if result, handled, gateErr := b.advisoryGateResult(ctx, current, details, call, recovering, checkpointOutput,
				"Reproduction test is missing; no action was executed."); handled {
				return result, gateErr
			}
			return AgentToolResult{}, err
		}
		var writeArguments struct {
			WorkspaceID string `json:"workspace_id"`
			Path        string `json:"path"`
			Content     string `json:"content"`
		}
		if decodeStrictRuntimeResult([]byte(call.Arguments), &writeArguments) != nil || writeArguments.Path == "" {
			return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "dsh tool arguments are invalid")
		}
		if err := requireMinimalDiffWrite(details, writeArguments.Path); err != nil {
			if result, handled, gateErr := b.advisoryGateResult(ctx, current, details, call, recovering, checkpointOutput,
				"File is outside the fix plan; no action was executed."); handled {
				return result, gateErr
			}
			return AgentToolResult{}, err
		}
	}
	pendingApproval := details.PendingRequest
	if pendingApproval != nil && pendingApproval.MediaPreviewOperationID != "" {
		if pendingApproval.CallID != call.CallID || pendingApproval.TaskID != current.ID || pendingApproval.Kind != "approval" ||
			pendingApproval.ToolName != call.Name || !bytes.Equal(pendingApproval.RequestHash, hash[:]) {
			return b.commitApprovalMismatch(ctx, current, call.Name, details.Steps)
		}
		return b.executeApprovedTaskMediaSave(ctx, current, call, details, *pendingApproval, checkpointOutput)
	}
	if recovering {
		result, handled, err := b.reconcileCheckpointedCall(ctx, claimed, call, checkpointOutput)
		if err != nil || handled {
			return result, err
		}
	}
	definitions, err := b.Definitions(ctx, current)
	if err != nil {
		return AgentToolResult{}, err
	}
	var definition AgentToolDefinition
	for _, candidate := range definitions {
		if candidate.Name == call.Name {
			definition = candidate
			break
		}
	}
	if definition.Name == "" {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "dsh tool is not available for this Host")
	}
	fence, err := b.runtime.readFence(ctx, current, nil)
	if err != nil {
		return AgentToolResult{}, err
	}
	workspaceID := ""
	if mapping, ok := runtimeToolMappingByName(call.Name); ok {
		workspaceID = runtimeToolWorkspaceID(mapping, arguments)
	}
	request := AgentToolRequest{
		Task: current, Call: call, Arguments: arguments, OperationKey: agentOperationKey(call.CallID),
		RequiredScope: definition.RequiredScope, Fence: fence, WorkspaceID: workspaceID,
	}
	approvalCall := pendingApproval != nil && pendingApproval.CallID == call.CallID
	if approvalCall && (pendingApproval.TaskID != current.ID || pendingApproval.Kind != "approval" || pendingApproval.ToolName != call.Name ||
		(pendingApproval.Status != task.PendingRequestPending && pendingApproval.Status != task.PendingRequestApproved &&
			pendingApproval.Status != task.PendingRequestRejected && pendingApproval.Status != task.PendingRequestExpired) ||
		!bytes.Equal(pendingApproval.RequestHash, hash[:]) ||
		pendingApproval.TaskRevision == nil || current.CurrentRevision == nil || *pendingApproval.TaskRevision != *current.CurrentRevision ||
		(pendingApproval.Status == task.PendingRequestApproved && pendingApproval.Decision != task.PendingDecisionApprove) ||
		((pendingApproval.Status == task.PendingRequestRejected || pendingApproval.Status == task.PendingRequestExpired) && pendingApproval.Decision != task.PendingDecisionReject) ||
		(pendingApproval.Status == task.PendingRequestExpired && (pendingApproval.ExpiresAt == nil || pendingApproval.RespondedAt == nil || pendingApproval.RespondedAt.Before(*pendingApproval.ExpiresAt)))) {
		return b.commitApprovalMismatch(ctx, current, call.Name, details.Steps)
	}
	usage := cloneTaskUsage(current.Usage)
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepRunning,
		"Run cloud dsh tool: "+call.Name, "Checking current authorization, budget, and Host control.")
	if err != nil {
		return AgentToolResult{}, err
	}
	if approvalCall && pendingApproval.Status == task.PendingRequestApproved {
		request.Approval = &AgentToolApprovalDecision{PendingID: pendingApproval.ID, Version: pendingApproval.Version,
			Decision: task.PendingDecisionApprove, RequestHash: append([]byte(nil), hash[:]...)}
	}
	recoveryOperation := false
	if recovering {
		operation, lookupErr := b.store.GetOperation(ctx, current.OwnerID, current.ID, request.OperationKey)
		switch {
		case domain.CodeOf(lookupErr) == domain.CodeNotFound:
			if checkpointOutput != nil {
				return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "A checkpointed tool result has no matching operation ledger entry.")
			}
		case lookupErr != nil:
			return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The prior tool operation could not be read safely.")
		default:
			if operation.ID == "" || operation.TaskID != current.ID || operation.Key != request.OperationKey || operation.HostID != current.HostID ||
				!bytes.Equal(operation.RequestHash, hash[:]) {
				return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The stored tool operation does not match this exact checkpointed request.")
			}
			recoveryOperation = true
		}
	}
	if approvalCall && (pendingApproval.Status == task.PendingRequestRejected || pendingApproval.Status == task.PendingRequestExpired) {
		if recoveryOperation {
			reason := "A rejected approval has a conflicting operation ledger entry."
			if pendingApproval.Status == task.PendingRequestExpired {
				reason = "An expired approval has a conflicting operation ledger entry."
			}
			return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, reason)
		}
		if pendingApproval.Status == task.PendingRequestExpired {
			return b.expireApprovedToolCall(ctx, current, step, usage, request)
		}
		return b.rejectApprovedToolCall(ctx, current, step, usage, request)
	}
	if approvalCall && pendingApproval.Status == task.PendingRequestPending {
		step.Status, step.Title, step.Summary = task.StepWaitingUser, "Approval required: "+call.Name, pendingApproval.Content
		if err := b.commitProgress(ctx, current, step, usage, task.StatusWaitingUser, "approval_required", "task.waiting_user", AgentApprovalRequest{
			Object: pendingApproval.Object, Content: pendingApproval.Content, Impact: pendingApproval.Impact,
			RejectionConsequence: pendingApproval.RejectionConsequence,
		}); err != nil {
			return AgentToolResult{}, err
		}
		return AgentToolResult{Status: AgentToolNeedsUser}, nil
	}
	if !recoveryOperation && !approvalCall && usage.ToolCalls >= current.Budget.ToolCalls {
		step.Status, step.Summary = task.StepWaitingUser, "The task tool call budget is exhausted."
		if err := b.commitProgress(ctx, current, step, usage, task.StatusWaitingUser, "tool_call_budget", "task.waiting_user", nil); err != nil {
			return AgentToolResult{}, err
		}
		return AgentToolResult{Status: AgentToolNeedsUser}, nil
	}
	if !recoveryOperation && !approvalCall {
		usage.ToolCalls++
		if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
			"tool": call.Name, "operation_key": request.OperationKey, "phase": "prepare",
		}); err != nil {
			return AgentToolResult{}, err
		}
	}
	current.Usage = usage
	request.Task = current
	preparation, err := b.runtime.Prepare(ctx, request)
	if err != nil {
		if recoveryOperation {
			return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The prior tool operation is no longer authorized for safe recovery.")
		}
		if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
			return b.pauseForWorkspaceInputChange(ctx, current, step, usage, call.Name)
		}
		return b.failBeforeDispatch(ctx, current, step, usage, call.Name, err)
	}
	if preparation.Status == AgentToolApprovalRequired {
		return b.pauseForApproval(ctx, current, step, usage, request, hash[:], preparation, details.PendingRequest, TaskBillingTotalActualMicros(details.Billing))
	}
	if preparation.Status != AgentToolReady || !sameAgentControlFence(preparation.Fence, fence) {
		return b.failBeforeDispatch(ctx, current, step, usage, call.Name, domain.NewError(domain.CodeInvalidState, "Host control fence changed"))
	}
	if runtimeToolRisk(call.Name, arguments) == runtimeToolRiskSensitive && request.Approval == nil {
		return b.failBeforeDispatch(ctx, current, step, usage, call.Name,
			domain.NewError(domain.CodeForbidden, "sensitive Host action has no exact approval binding"))
	}
	if current.CurrentRevision == nil || *current.CurrentRevision < 0 {
		return b.failBeforeDispatch(ctx, current, step, usage, call.Name,
			domain.NewError(domain.CodeInvalidState, "Task revision is unavailable for Host operation binding"))
	}
	request.Fence, request.WorkspaceID = preparation.Fence, preparation.WorkspaceID
	operationID, err := domain.NewOperationID()
	if err != nil {
		return AgentToolResult{}, fmt.Errorf("generate dsh operation ID: %w", err)
	}
	var inputRevision *int64
	if preparation.WorkspaceID != "" {
		inputRevision = current.InputRevision
	}
	operationValue := task.Operation{
		ID: operationID, TaskID: current.ID, Key: request.OperationKey, RequestHash: hash[:], HostID: current.HostID,
		TaskRevision: current.CurrentRevision, CallID: call.CallID, ToolName: call.Name,
		WorkspaceID: preparation.WorkspaceID, BindingVersion: preparation.BindingVersion,
		InputRevision: inputRevision, WorkspaceCheckpointRevision: current.WorkspaceCheckpointRevision,
		RuntimeGeneration: request.Fence.RuntimeGeneration, RuntimeInstanceID: request.Fence.RuntimeInstanceID,
		ControlVersion: request.Fence.ControlVersion, LeaseVersion: current.Lease.Version,
		RuntimeLeaseVersion: current.Lease.Version, Status: task.OperationPrepared,
	}
	if request.Approval != nil {
		approvalVersion := request.Approval.Version
		operationValue.ApprovalID = request.Approval.PendingID
		operationValue.ApprovalVersion = &approvalVersion
		operationValue.ApprovalRequestHash = append([]byte(nil), request.Approval.RequestHash...)
	}
	operation, replayed, err := b.store.CreateOperation(ctx, operationValue, current.Lease.Owner)
	if err != nil {
		if recoveryOperation {
			return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The stored tool operation could not be matched safely.")
		}
		if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
			return b.pauseForWorkspaceInputChange(ctx, current, step, usage, call.Name)
		}
		return b.failBeforeDispatch(ctx, current, step, usage, call.Name, err)
	}
	if recovering && recoveryOperation && !replayed {
		return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The exact prior operation disappeared during recovery.")
	}
	if operation.ID == "" || operation.HostID != current.HostID || operation.WorkspaceID != preparation.WorkspaceID ||
		operation.BindingVersion != preparation.BindingVersion || !sameTaskInputRevision(operation.InputRevision, inputRevision) ||
		!operationWorkspaceCheckpointMatches(operation, current) ||
		!bytes.Equal(operation.RequestHash, hash[:]) || operation.TaskRevision == nil ||
		*operation.TaskRevision != *current.CurrentRevision || operation.CallID != call.CallID || operation.ToolName != call.Name ||
		operation.ApprovalID != operationValue.ApprovalID || !sameTaskInputRevision(operation.ApprovalVersion, operationValue.ApprovalVersion) ||
		!bytes.Equal(operation.ApprovalRequestHash, operationValue.ApprovalRequestHash) {
		return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The stored tool operation does not match this exact request.")
	}
	request.OperationID = operation.ID
	if replayed {
		var result AgentToolResult
		var terminal bool
		operation, result, terminal, err = b.reconcile(ctx, request, operation, step, usage)
		if err != nil || terminal {
			if terminal && err == nil && result.Status != AgentToolUnknown && result.Status != AgentToolNeedsUser {
				if recovering && checkpointOutput != nil && *checkpointOutput != dshToolResultText(result) {
					return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The checkpointed tool result differs from the exact ledger result.")
				}
				return b.commitResult(ctx, current, step, usage, call.Name, result)
			}
			return result, err
		}
	}
	if recovering && checkpointOutput != nil {
		return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The checkpointed tool result is not backed by a terminal ledger entry.")
	}
	if operation.Status != task.OperationPrepared {
		return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The prior tool operation cannot be proven safe to dispatch again.")
	}
	if operation.LeaseVersion != current.Lease.Version {
		operation, err = b.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: current.ID, OperationKey: request.OperationKey, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
			RuntimeGeneration: request.Fence.RuntimeGeneration, RuntimeInstanceID: request.Fence.RuntimeInstanceID,
			ControlVersion: request.Fence.ControlVersion, RuntimeLeaseVersion: current.Lease.Version,
			AdoptPrepared: true, Status: task.OperationPrepared,
		})
		if err != nil {
			return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The prepared tool operation could not be safely adopted.")
		}
	}
	if _, err := b.store.UpdateOperation(ctx, OperationUpdate{
		TaskID: current.ID, OperationKey: request.OperationKey, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		RuntimeGeneration: request.Fence.RuntimeGeneration, RuntimeInstanceID: request.Fence.RuntimeInstanceID,
		ControlVersion: request.Fence.ControlVersion, RuntimeLeaseVersion: current.Lease.Version, Status: task.OperationDispatched,
	}); err != nil {
		return AgentToolResult{}, err
	}
	var result AgentToolResult
	var executeErr error
	if current.ProjectID != "" && request.WorkspaceID != "" {
		guard, ok := b.store.(projectTaskWorkspaceGuard)
		mapping, mapped := runtimeToolMappingByName(call.Name)
		if !ok || !mapped {
			executeErr = domain.NewError(domain.CodeForbidden, "project Workspace authorization is unavailable")
		} else {
			write := mapping.capability != "file.read" && mapping.capability != "file.list" && mapping.capability != "file.find"
			executeErr = guard.WithProjectTaskWorkspaceAccess(ctx, current, write, func(operationCtx context.Context) error {
				var err error
				ctx = withTaskToolExecutionStart(ctx)
				result, err = b.runtime.Execute(operationCtx, request, preparation)
				ctx = withTaskToolExecutionEnd(ctx)
				return err
			})
		}
	} else {
		ctx = withTaskToolExecutionStart(ctx)
		result, executeErr = b.runtime.Execute(ctx, request, preparation)
		ctx = withTaskToolExecutionEnd(ctx)
	}
	if executeErr != nil || result.Status == AgentToolUnknown || len(result.Output) > maxAgentToolOutputBytes ||
		!validCloudDSHToolImages(call.Name, result) {
		if err := b.markUnknown(ctx, current, request.OperationKey); err != nil {
			return AgentToolResult{}, errors.Join(executeErr, err)
		}
		return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The tool result is unknown; the action will not be replayed.")
	}
	if result.Status == AgentToolNeedsUser {
		if _, err := b.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: current.ID, OperationKey: request.OperationKey, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
			Status: task.OperationFailed, ExternalID: boundedExternalID(result.ExternalID), Result: boundedToolOutput(result.Output),
		}); err != nil {
			return AgentToolResult{}, err
		}
		reason := result.WaitingReason
		var message string
		switch reason {
		case "workspace_input_changed":
			message = "Workspace changed after this task was submitted. Submit a new task to use the updated files."
		case "desktop_focus_changed":
			message = "The target application lost focus. No input was sent; focus the target application before continuing."
		default:
			reason, message = "tool_needs_user", "The Host requires user attention before the task can continue."
		}
		step.Status, step.Summary = task.StepWaitingUser, message
		if err := b.commitProgress(ctx, current, step, usage, task.StatusWaitingUser, reason, "task.waiting_user", map[string]any{
			"tool": call.Name, "operation_key": request.OperationKey, "message": message,
		}); err != nil {
			return AgentToolResult{}, err
		}
		return result, nil
	}
	if result.Status != AgentToolSucceeded && result.Status != AgentToolFailed {
		if err := b.markUnknown(ctx, current, request.OperationKey); err != nil {
			return AgentToolResult{}, err
		}
		return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The tool returned an unsupported result; the action will not be replayed.")
	}
	operationStatus, stepStatus := task.OperationSucceeded, task.StepSucceeded
	summary := "The cloud dsh tool completed and its result was saved."
	eventDetail := map[string]any{
		"tool": call.Name, "operation_key": request.OperationKey, "operation_status": operationStatus,
	}
	if result.Status == AgentToolFailed {
		operationStatus, stepStatus = task.OperationFailed, task.StepFailed
		summary = "The Host runtime reported a definitive tool failure."
		ctx = withTaskToolResultEvidence(ctx, "execute", taskToolExecutionFailureReason)
		eventDetail["operation_status"] = operationStatus
		eventDetail["failure_reason"] = taskToolExecutionFailureReason
	}
	output := boundedToolOutput(result.Output)
	if _, err := b.store.UpdateOperation(ctx, OperationUpdate{
		TaskID: current.ID, OperationKey: request.OperationKey, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		Status: operationStatus, ExternalID: boundedExternalID(result.ExternalID), Result: output,
	}); err != nil {
		if persistErr := b.markUnknown(ctx, current, request.OperationKey); persistErr != nil {
			return AgentToolResult{}, errors.Join(err, persistErr)
		}
		return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The Host action completed but its result could not be committed safely.")
	}
	step.Status, step.Summary = stepStatus, summary
	ctx = withTaskToolShellExecutionEvidence(ctx, call.Name, result)
	if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", eventDetail); err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: result.Status, ExternalID: result.ExternalID, Output: output, Images: result.Images}, nil
}

func (b *CloudDSHToolBroker) executeApprovedTaskMediaSave(ctx context.Context, current task.Task, call ModelToolCall, details TaskDetails,
	pending task.PendingRequest, checkpointOutput *string) (AgentToolResult, error) {
	stepID := taskMediaSaveStepID(pending.MediaPreviewOperationID)
	if stepID == "" {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "approved Task media step identity is invalid")
	}
	step, exists := taskStepByID(details.Steps, stepID)
	if exists && (step.TaskID != current.ID || step.Kind != "artifact" || step.Title != "Save generated media") {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "Task media step identity conflicts with an existing step")
	}
	if !exists {
		if maxStepSequence(details.Steps) == int64(^uint64(0)>>1) {
			return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "task step sequence is exhausted")
		}
		step = task.Step{ID: stepID, TaskID: current.ID, Sequence: maxStepSequence(details.Steps) + 1,
			Kind: "artifact", Status: task.StepRunning, Title: "Save generated media",
			Summary: "Saving the result approved by the user to the Task Workspace."}
	}
	usage := cloneTaskUsage(current.Usage)
	switch pending.Status {
	case task.PendingRequestPending:
		step.Status, step.Summary = task.StepWaitingUser, "The generated media is waiting for the user's save decision."
		if err := b.commitProgress(ctx, current, step, usage, task.StatusWaitingUser, "approval_required", "task.waiting_user", map[string]any{
			"pending_request_id": pending.ID, "pending_request_version": pending.Version,
		}); err != nil {
			return AgentToolResult{}, err
		}
		return AgentToolResult{Status: AgentToolNeedsUser}, nil
	case task.PendingRequestRejected, task.PendingRequestExpired:
		if pending.Decision != task.PendingDecisionReject {
			return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "Task media rejection decision is invalid")
		}
		output := "The user declined to save the generated media. No Workspace file was written."
		reason := "reject"
		summary := "The user declined to save the generated media."
		if pending.Status == task.PendingRequestExpired {
			if pending.ExpiresAt == nil || pending.RespondedAt == nil || pending.RespondedAt.Before(*pending.ExpiresAt) {
				return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "Task media approval expiry is invalid")
			}
			output = "The approval to save generated media expired. No Workspace file was written."
			reason = "expired"
			summary = "The approval to save generated media expired; no Workspace file was written."
		}
		if checkpointOutput != nil && *checkpointOutput != output {
			return b.pauseUnknown(ctx, current, step, usage, "task-media-save:"+pending.MediaPreviewOperationID,
				"The checkpointed media result differs from the approved Task decision.")
		}
		if step.Status != task.StepFailed {
			step.Status, step.Summary = task.StepFailed, summary
			if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
				"tool": call.Name, "approval_decision": reason, "workspace_published": false,
			}); err != nil {
				return AgentToolResult{}, err
			}
		}
		return AgentToolResult{Status: AgentToolFailed, Output: output}, nil
	case task.PendingRequestApproved:
		if pending.Decision != task.PendingDecisionApprove {
			return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "Task media approval decision is invalid")
		}
	default:
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "Task media approval state is invalid")
	}
	if step.Status == task.StepFailed {
		output := "The approved media could not be saved to the Task Workspace. Submit a new task after resolving the Workspace issue."
		if checkpointOutput != nil && *checkpointOutput != output {
			return b.pauseUnknown(ctx, current, step, usage, "task-media-save:"+pending.MediaPreviewOperationID,
				"The checkpointed media result differs from the saved Task step.")
		}
		return AgentToolResult{Status: AgentToolFailed, Output: output}, nil
	}
	if step.Status != task.StepRunning && step.Status != task.StepSucceeded {
		step.Status, step.Summary = task.StepRunning, "Saving the result approved by the user to the Task Workspace."
		if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
			"tool": call.Name, "phase": "workspace_publish",
		}); err != nil {
			return AgentToolResult{}, err
		}
	} else if !exists {
		if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
			"tool": call.Name, "phase": "workspace_publish",
		}); err != nil {
			return AgentToolResult{}, err
		}
	}
	saved, err := b.runtime.SaveApprovedTaskMedia(ctx, current, pending, stepID)
	if err != nil {
		if domain.CodeOf(err) != domain.CodeWorkspaceConflict && domain.CodeOf(err) != domain.CodeQuotaExceeded {
			return AgentToolResult{}, err
		}
		output := "The approved media could not be saved because the Task Workspace changed or its storage quota was reached. Submit a new task after resolving the Workspace issue."
		if checkpointOutput != nil && *checkpointOutput != output {
			return b.pauseUnknown(ctx, current, step, usage, "task-media-save:"+pending.MediaPreviewOperationID,
				"The checkpointed media result differs from the failed Workspace save.")
		}
		step.Status, step.Summary = task.StepFailed, "The approved media could not be committed to the Task Workspace."
		if commitErr := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
			"tool": call.Name, "workspace_published": false,
		}); commitErr != nil {
			return AgentToolResult{}, errors.Join(err, commitErr)
		}
		return AgentToolResult{Status: AgentToolFailed, Output: output}, nil
	}
	if workspace.ValidateRelativePath(saved.Path) != nil || saved.Revision < 0 || !taskMediaPreviewTypeAllowed(saved.MediaType) ||
		saved.SizeBytes < 1 || !validSyncDigest(saved.SHA256) {
		return b.pauseUnknown(ctx, current, step, usage, "task-media-save:"+pending.MediaPreviewOperationID,
			"The committed media Artifact proof is invalid.")
	}
	output := fmt.Sprintf("Saved approved generated media to Workspace path %q at revision %d.", saved.Path, saved.Revision)
	if checkpointOutput != nil && *checkpointOutput != output {
		return b.pauseUnknown(ctx, current, step, usage, "task-media-save:"+pending.MediaPreviewOperationID,
			"The checkpointed media result differs from the committed Workspace Artifact.")
	}
	if step.Status != task.StepSucceeded {
		step.Status, step.Summary = task.StepSucceeded, "The approved generated media is committed and verified in the Task Workspace."
		if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
			"tool": call.Name, "workspace_path": saved.Path, "workspace_revision": saved.Revision, "media_type": saved.MediaType,
		}); err != nil {
			return AgentToolResult{}, err
		}
	}
	return AgentToolResult{Status: AgentToolSucceeded, Output: output}, nil
}

func (b *CloudDSHToolBroker) pauseForWorkspaceInputChange(ctx context.Context, current task.Task, step task.Step, usage task.Usage, tool string) (AgentToolResult, error) {
	message := "Workspace changed after this task was submitted. Submit a new task to use the updated files."
	step.Status, step.Summary = task.StepWaitingUser, message
	if err := b.commitProgress(ctx, current, step, usage, task.StatusWaitingUser, "workspace_input_changed", "task.waiting_user", map[string]any{
		"tool": tool, "message": message,
	}); err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolNeedsUser, WaitingReason: "workspace_input_changed", Output: message}, nil
}

func validCloudDSHToolImages(name string, result AgentToolResult) bool {
	if result.Status != AgentToolSucceeded {
		return len(result.Images) == 0
	}
	if strings.HasPrefix(name, cloudDSHMCPToolPrefix) {
		return len(result.Images) <= 1 && (len(result.Images) == 0 ||
			validCloudDSHMCPImage(result.Images[0].MediaType, result.Images[0].Data))
	}
	if name == cloudDSHTaskAttachmentReadToolName || strings.HasPrefix(name, "morde_hub_file_fetch_") {
		if len(result.Images) > maxCloudDSHTaskAttachmentImages {
			return false
		}
		totalBytes := 0
		for _, image := range result.Images {
			if len(image.Data) < 4 || len(image.Data) > maxCloudDSHTaskAttachmentImageBytes ||
				!validCloudDSHTaskAttachmentImage(image.MediaType, image.Data) {
				return false
			}
			totalBytes += len(image.Data)
		}
		return totalBytes <= maxCloudDSHTaskAttachmentImagesBytes
	}
	if name != runtimeToolDesktopScreenshot && name != runtimeToolBrowserScreenshot {
		return len(result.Images) == 0
	}
	return len(result.Images) == 1 && result.Images[0].MediaType == "image/jpeg" &&
		len(result.Images[0].Data) >= 4 && len(result.Images[0].Data) <= 512<<10
}

func (b *CloudDSHToolBroker) clarify(ctx context.Context, claimed task.Task, call ModelToolCall, recovering bool) (AgentToolResult, error) {
	question, err := parseAgentClarificationQuestion(call.Arguments)
	if err != nil || !utf8.ValidString(call.CallID) {
		return AgentToolResult{}, domain.InvalidArgument("clarification_request", "dsh clarification call is invalid")
	}
	details, err := b.store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil {
		return AgentToolResult{}, err
	}
	current := details.Task
	if current.ID != claimed.ID || current.OwnerID != claimed.OwnerID || current.HostID != claimed.HostID {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	var matched *task.ClarificationRequest
	var hasPending bool
	for i := range details.ClarificationRequests {
		request := &details.ClarificationRequests[i]
		hasPending = hasPending || request.Status == task.ClarificationPending
		if request.TaskID != current.ID || request.CallID != call.CallID {
			continue
		}
		if matched != nil || request.Question != question {
			return AgentToolResult{Status: AgentToolUnknown}, nil
		}
		matched = request
	}
	if matched != nil {
		switch matched.Status {
		case task.ClarificationPending:
			if matched.Answer != "" || matched.RespondedAt != nil {
				return AgentToolResult{Status: AgentToolUnknown}, nil
			}
			return AgentToolResult{Status: AgentToolNeedsUser}, nil
		case task.ClarificationAnswered:
			if matched.Answer == "" || matched.RespondedAt == nil || !utf8.ValidString(matched.Answer) || utf8.RuneCountInString(matched.Answer) > 16384 ||
				current.Status != task.StatusRunning || current.Lease.Owner != claimed.Lease.Owner ||
				current.Lease.Version != claimed.Lease.Version || current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) {
				return AgentToolResult{Status: AgentToolUnknown}, nil
			}
			return AgentToolResult{Status: AgentToolSucceeded, Output: matched.Answer}, nil
		default:
			return AgentToolResult{Status: AgentToolUnknown}, nil
		}
	}
	if recovering || hasPending {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	if current.Status != task.StatusRunning || current.Lease.Owner == "" || current.Lease.Owner != claimed.Lease.Owner ||
		current.Lease.Version != claimed.Lease.Version || current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	sequence := maxStepSequence(details.Steps)
	if current.Usage.ToolCalls >= current.Budget.ToolCalls {
		if sequence == int64(^uint64(0)>>1) {
			return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "task step sequence is exhausted")
		}
		step, err := newAgentStep(current.ID, sequence+1, "budget", task.StepWaitingUser,
			"Tool call budget reached", "The task is paused before another tool call.")
		if err != nil {
			return AgentToolResult{}, err
		}
		if err := b.commitProgress(ctx, current, step, current.Usage, task.StatusWaitingUser, "tool_call_budget", "task.waiting_user", nil); err != nil {
			return AgentToolResult{}, err
		}
		return AgentToolResult{Status: AgentToolNeedsUser}, nil
	}
	if sequence > int64(^uint64(0)>>1)-2 {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "task step sequence is exhausted")
	}
	version := nextClarificationVersion(details.ClarificationRequests)
	if version < 1 {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "clarification version is exhausted")
	}
	id, err := domain.NewPendingRequestID()
	if err != nil {
		return AgentToolResult{}, fmt.Errorf("generate dsh clarification ID: %w", err)
	}
	modelStep, err := newAgentStep(current.ID, sequence+1, "model", task.StepSucceeded,
		"DSH model asked for clarification", "The DSH model paused before continuing the task.")
	if err != nil {
		return AgentToolResult{}, err
	}
	step, err := newAgentStep(current.ID, sequence+2, "clarification", task.StepWaitingUser,
		"The Agent needs one detail", question)
	if err != nil {
		return AgentToolResult{}, err
	}
	request := &task.ClarificationRequest{
		ID: id, TaskID: current.ID, Version: version, ModelStepID: modelStep.ID,
		CallID: call.CallID, Question: question, Status: task.ClarificationPending,
	}
	usage := cloneTaskUsage(current.Usage)
	usage.ToolCalls++
	payload, err := agentStepEvent(step, usage, map[string]any{
		"clarification_id": request.ID, "clarification_version": request.Version,
		"call_id": call.CallID, "source": "cloud_dsh",
	})
	if err != nil {
		return AgentToolResult{}, err
	}
	_, err = b.store.CommitTaskProgress(ctx, TaskProgress{
		TaskID: current.ID, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: step.ID, Result: current.Result,
		WaitingReason: "clarification_required", Usage: usage,
		Steps: []task.Step{modelStep, step}, Event: task.Event{TaskID: current.ID, Type: "task.waiting_user", Payload: payload},
		ClarificationRequest: request,
	})
	if err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolNeedsUser}, nil
}

// reconcileCheckpointedCall resolves an operation already present in the
// durable Morde ledger without running ordinary Prepare or charging another
// tool call. It returns handled=false only when a fresh call or a previously
// prepared operation can continue through the normal budgeted path.
func (b *CloudDSHToolBroker) reconcileCheckpointedCall(ctx context.Context, claimed task.Task, call ModelToolCall, checkpointOutput *string) (AgentToolResult, bool, error) {
	current, err := b.runtime.currentTaskForOperationReconciliation(ctx, claimed)
	if err != nil {
		return AgentToolResult{}, true, err
	}
	arguments, err := validateAgentToolArguments(call.Arguments)
	if err != nil {
		return AgentToolResult{}, true, domain.InvalidArgument("tool_call.arguments", "dsh tool arguments are invalid")
	}
	mapping, ok := runtimeToolMappingByName(call.Name)
	if !ok || !validRuntimeToolArguments(mapping, arguments) {
		return AgentToolResult{}, true, domain.NewError(domain.CodeForbidden, "dsh tool is not available for this Host")
	}
	operationKey := agentOperationKey(call.CallID)
	hash, err := agentOperationHash(call.Name, arguments)
	if err != nil {
		return AgentToolResult{}, true, err
	}
	details, err := b.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		return AgentToolResult{}, true, err
	}
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepRunning,
		"Recover cloud dsh tool: "+call.Name, "Checking the exact prior Host operation and current authorization.")
	if err != nil {
		return AgentToolResult{}, true, err
	}
	usage := cloneTaskUsage(current.Usage)
	pause := func(summary string) (AgentToolResult, bool, error) {
		result, err := b.pauseUnknown(ctx, current, step, usage, operationKey, summary)
		return result, true, err
	}
	pendingApproval := details.PendingRequest
	if pendingApproval != nil && pendingApproval.CallID == call.CallID {
		if pendingApproval.TaskID != current.ID || pendingApproval.Kind != "approval" || pendingApproval.ToolName != call.Name ||
			!bytes.Equal(pendingApproval.RequestHash, hash[:]) {
			_, lookupErr := b.store.GetOperation(ctx, current.OwnerID, current.ID, operationKey)
			if domain.CodeOf(lookupErr) == domain.CodeNotFound && checkpointOutput == nil {
				result, err := b.commitApprovalMismatch(ctx, current, call.Name, details.Steps)
				return result, true, err
			}
			return pause("The saved approval does not match this exact checkpointed tool call.")
		}
		if pendingApproval.Status == task.PendingRequestRejected || pendingApproval.Status == task.PendingRequestExpired {
			if pendingApproval.Decision != task.PendingDecisionReject {
				return pause("The saved rejection decision is invalid.")
			}
			result := AgentToolResult{Status: AgentToolFailed, Output: "The user rejected this exact action. It was not executed."}
			decision := "reject"
			failureSummary := "The user rejected this exact action; it was not dispatched."
			if pendingApproval.Status == task.PendingRequestExpired {
				if pendingApproval.ExpiresAt == nil || pendingApproval.RespondedAt == nil || pendingApproval.RespondedAt.Before(*pendingApproval.ExpiresAt) {
					return pause("The saved approval expiry is invalid.")
				}
				result.Output = "The approval expired. This exact action was not executed."
				decision = "expired"
				failureSummary = "The approval expired; this exact action was not dispatched."
			}
			if checkpointOutput != nil && *checkpointOutput != dshToolResultText(result) {
				return pause("The checkpointed rejection result differs from the saved user decision.")
			}
			_, lookupErr := b.store.GetOperation(ctx, current.OwnerID, current.ID, operationKey)
			if domain.CodeOf(lookupErr) != domain.CodeNotFound {
				return pause("A rejected approval has a conflicting operation ledger entry.")
			}
			if checkpointOutput == nil {
				step.Status, step.Summary = task.StepFailed, failureSummary
				if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
					"tool": call.Name, "operation_key": operationKey, "operation_status": result.Status, "approval_decision": decision,
				}); err != nil {
					return AgentToolResult{}, true, err
				}
			}
			return result, true, nil
		}
	}
	operation, err := b.store.GetOperation(ctx, current.OwnerID, current.ID, operationKey)
	if domain.CodeOf(err) == domain.CodeNotFound {
		if checkpointOutput != nil {
			return pause("A checkpointed tool result has no matching operation ledger entry.")
		}
		return AgentToolResult{}, false, nil
	}
	if err != nil {
		return pause("The prior tool operation could not be read safely.")
	}
	workspaceID, workspaceErr := runtimeToolOperationWorkspaceID(mapping, arguments, current)
	if operation.ID == "" || !domain.ValidOperationID(operation.ID) || operation.TaskID != current.ID || operation.Key != operationKey ||
		workspaceErr != nil || operation.HostID != current.HostID || operation.WorkspaceID != workspaceID ||
		!sameTaskInputRevision(operation.InputRevision, current.InputRevision) ||
		!operationWorkspaceCheckpointMatches(operation, current) ||
		(operation.WorkspaceID != "" && operation.BindingVersion < 1) ||
		(operation.WorkspaceID == "" && operation.BindingVersion != 0) || !bytes.Equal(operation.RequestHash, hash[:]) {
		return pause("The stored tool operation does not match this exact checkpointed request.")
	}
	if operation.Status == task.OperationPrepared {
		if checkpointOutput != nil {
			return pause("A checkpointed tool result is not backed by a dispatched ledger operation.")
		}
		return AgentToolResult{}, false, nil
	}
	request := AgentToolRequest{
		Task: current, Call: call, Arguments: arguments, OperationKey: operationKey, OperationID: operation.ID,
		RequiredScope: mapping.scope, WorkspaceID: workspaceID,
	}
	if operation.Status == task.OperationDispatched {
		operation, err = b.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: current.ID, OperationKey: operationKey, LeaseOwner: current.Lease.Owner,
			LeaseVersion: current.Lease.Version, Status: task.OperationUnknown,
		})
		if err != nil {
			return pause("The dispatched operation could not be durably marked for reconciliation.")
		}
	}
	if operation.Status != task.OperationUnknown && operation.Status != task.OperationSucceeded && operation.Status != task.OperationFailed {
		return pause("The prior tool operation is not in a recoverable ledger state.")
	}
	operation, result, terminal, err := b.reconcile(ctx, request, operation, step, usage)
	if err != nil || !terminal {
		return result, true, err
	}
	if result.Status == AgentToolUnknown || result.Status == AgentToolNeedsUser {
		return result, true, nil
	}
	if checkpointOutput != nil && *checkpointOutput != dshToolResultText(result) {
		return pause("The checkpointed tool result differs from the exact ledger result.")
	}
	result, err = b.commitResult(ctx, current, step, usage, call.Name, result)
	return result, true, err
}

func dshToolResultText(result AgentToolResult) string {
	if result.Status == AgentToolFailed {
		return "Tool failed: " + result.Output
	}
	return result.Output
}

// PauseRecovery moves an unresolved restored session out of the worker claim
// queue. It records no checkpoint contents or tool arguments in the task event.
func (b *CloudDSHToolBroker) PauseRecovery(ctx context.Context, claimed task.Task) error {
	return b.pauseRecovery(ctx, claimed, AgentModelFailureDiagnostic{})
}

// PauseModelRecovery records an allowlisted model failure category without
// persisting provider response content, request data, or credentials.
func (b *CloudDSHToolBroker) PauseModelRecovery(ctx context.Context, claimed task.Task, category string) error {
	return b.PauseModelRecoveryDiagnostic(ctx, claimed, AgentModelFailureDiagnostic{Category: category})
}

// PauseModelRecoveryDiagnostic persists only allowlisted response-validation
// details. It excludes model text, tool arguments, and provider credentials.
func (b *CloudDSHToolBroker) PauseModelRecoveryDiagnostic(ctx context.Context, claimed task.Task, diagnostic AgentModelFailureDiagnostic) error {
	diagnostic = safeAgentModelFailureDiagnostic(diagnostic)
	return b.pauseRecovery(ctx, claimed, diagnostic)
}

// PauseModelRecoveryAfterLadderExhaustion records the terminal state once a
// retry ladder spent its budget (attempts retries on reason). The persisted
// step carries the ladder-exhausted marker and the §4.3 user-visible text
// for that reason; no provider text, stack trace, or raw error is stored.
// The route worker owns its durable retry ladder independently.
func (b *CloudDSHToolBroker) PauseModelRecoveryAfterLadderExhaustion(ctx context.Context, claimed task.Task, reason FailoverReason, attempts int) error {
	if _, ok := failoverReasonStrategies[reason]; !ok {
		return domain.InvalidArgument("failover_reason", "unknown failover reason cannot end a retry ladder")
	}
	return b.PauseModelRecoveryDiagnostic(ctx, claimed, AgentModelFailureDiagnostic{
		Category: "model_retry_ladder_exhausted", LadderExhausted: true, Attempts: attempts, Reason: reason,
	})
}

func safeAgentModelFailureDiagnostic(value AgentModelFailureDiagnostic) AgentModelFailureDiagnostic {
	if value.Stage != "model_response_validation" {
		value.Stage = ""
	}
	switch value.Category {
	case "authentication_rejected", "access_rejected", "rate_limited", "quota_exhausted", "invalid_request",
		"unavailable", "timeout", "transport", "invalid_response", "empty_response", "truncated_response", "upstream_error", "usage_unknown", "unsupported",
		"client_request_invalid", "context_length_exceeded", "provider_invalid_request", "provider_unsupported_parameter", "provider_context_length_exceeded",
		"provider_model_not_found", "provider_permission_denied", "provider_invalid_api_key", "provider_rate_limited",
		"provider_quota_exhausted", "provider_server_error", "provider_request_rejected", "attachment_reader_unavailable", "stream_result_mismatch", "unauthorized_tool",
		"model_retry_ladder_exhausted":
	default:
		value.Category = "unknown"
	}
	if _, ok := failoverReasonStrategies[value.Reason]; !ok {
		value.Reason = ""
	}
	if value.Attempts < 0 {
		value.Attempts = 0
	}
	switch value.ReasonCode {
	case "response_shape_invalid", "tool_call_metadata_invalid", "task_has_no_authorized_tools", "tool_not_in_task_allowlist", "tool_arguments_not_object":
	default:
		value.ReasonCode = ""
	}
	if !agentToolNamePattern.MatchString(value.ToolName) {
		value.ToolName = ""
	}
	if !ValidDSHModelToolCallID(value.ToolCallID) {
		value.ToolCallID = ""
	}
	return value
}

func (b *CloudDSHToolBroker) pauseRecovery(ctx context.Context, claimed task.Task, diagnostic AgentModelFailureDiagnostic) error {
	if ctx == nil {
		return domain.InvalidArgument("context", "recovery context is required")
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	details, err := b.store.GetTask(persistCtx, claimed.ID, claimed.OwnerID)
	if err != nil {
		return err
	}
	current := details.Task
	if current.Status != task.StatusRunning || current.Lease.Owner != claimed.Lease.Owner || current.Lease.Version != claimed.Lease.Version ||
		current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) {
		return nil
	}
	title := "Review cloud task recovery"
	summary := "The previous cloud task checkpoint could not be reconciled safely."
	waitingReason := "dsh_recovery_unresolved"
	if diagnostic.Category != "" {
		summary = "The model request could not be verified (category: " + diagnostic.Category + "); retry after checking the model connection."
		if diagnostic.Stage != "" {
			summary += " failure_stage: " + diagnostic.Stage + "."
		}
		if diagnostic.ReasonCode != "" {
			summary += " diagnostic_reason: " + diagnostic.ReasonCode + "."
		}
		if diagnostic.ToolName != "" {
			summary += " tool_name: " + diagnostic.ToolName + "."
		}
		if diagnostic.ToolCallID != "" {
			summary += " tool_call_id: " + diagnostic.ToolCallID + "."
		}
		if modelFailureToolNotDispatched(diagnostic) {
			summary += " tool_dispatched: false."
		}
	}
	if diagnostic.Category == "context_length_exceeded" || diagnostic.Category == "provider_context_length_exceeded" {
		title = "消息超过模型输入上限"
		summary = "消息及上下文超过当前模型输入上限，原文已保留；请缩短后重新发送。"
		waitingReason = "model_context_limit"
	}
	if diagnostic.Category == "usage_unknown" {
		title = "模型调用结果待核对"
		summary = "模型调用的结果或用量尚未确认，任务已暂停；请先核对后再决定是否恢复。"
	}
	if diagnostic.Category == "authentication_rejected" || diagnostic.Category == "access_rejected" ||
		diagnostic.Category == "provider_invalid_api_key" || diagnostic.Category == "provider_permission_denied" {
		title = "模型服务认证未通过"
		summary = "模型服务的认证或授权未通过，任务已暂停；请检查模型连接及授权。"
	}
	if diagnostic.Category == "rate_limited" || diagnostic.Category == "provider_rate_limited" {
		title = "模型服务暂时限流"
		summary = "模型服务暂时限流，任务已暂停；本次调用结果和用量如尚未确认，需先核对后再恢复。"
	}
	if diagnostic.Category == "attachment_reader_unavailable" {
		title = "当前模型无法读取附件"
		summary = "当前模型无法读取附件，请切换到支持工具的模型，或将附件内容粘贴到对话中。"
		waitingReason = "model_attachment_unavailable"
	}
	if diagnostic.Category == "stream_result_mismatch" {
		title = "回复已中断"
		summary = "已显示的部分回复与模型最终结果不一致，回复已中断；请重新发送消息。"
		waitingReason = "model_stream_interrupted"
	}
	if diagnostic.LadderExhausted {
		// Terminal state: the retry ladder spent its budget. The user
		// text comes from the ladder message builder (reason case and
		// attempt count only), never from provider text or a stack trace,
		// and only names where it got stuck, what was tried, and what is
		// needed. It is shown only when the failure maps to a §4.3 case;
		// other exhausted ladders keep the generic pause text.
		title = "模型重试已耗尽"
		waitingReason = "model_retry_ladder_exhausted"
		attempts := diagnostic.Attempts
		if attempts < 0 {
			attempts = 0
		}
		classification := ErrorClassification{Reason: diagnostic.Reason, Strategy: FailoverStrategyFor(diagnostic.Reason)}
		if message, ok := LadderExhaustedUserMessage(classification, attempts); ok {
			summary = message
		} else {
			summary = "模型服务暂时无法恢复，任务已暂停；请稍后再试。"
		}
	}
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "verification", task.StepUnknown,
		title, summary)
	if err != nil {
		return err
	}
	step.ToolName = diagnostic.ToolName
	metadata := map[string]any{"phase": "recovery", "source": "cloud_dsh"}
	if diagnostic.Stage != "" {
		metadata["failure_stage"] = diagnostic.Stage
	}
	if diagnostic.Category != "" {
		metadata["failure_category"] = diagnostic.Category
	}
	if diagnostic.ReasonCode != "" {
		metadata["failure_reason"] = diagnostic.ReasonCode
	}
	if diagnostic.ToolName != "" {
		metadata["tool_name"] = diagnostic.ToolName
	}
	if diagnostic.ToolCallID != "" {
		metadata["tool_call_id"] = diagnostic.ToolCallID
	}
	if modelFailureToolNotDispatched(diagnostic) {
		metadata["tool_dispatched"] = false
	}
	if diagnostic.LadderExhausted {
		metadata["ladder_exhausted"] = true
		metadata["retry_attempts"] = max(diagnostic.Attempts, 0)
		if _, ok := failoverReasonStrategies[diagnostic.Reason]; ok {
			metadata["failover_reason"] = string(diagnostic.Reason)
		}
	}
	return b.commitProgress(persistCtx, current, step, current.Usage, task.StatusWaitingUser, waitingReason, "task.waiting_user", metadata)
}

func modelFailureToolNotDispatched(diagnostic AgentModelFailureDiagnostic) bool {
	return diagnostic.Category == "unauthorized_tool" || diagnostic.ReasonCode == "tool_call_metadata_invalid" ||
		diagnostic.ReasonCode == "tool_arguments_not_object"
}

func (b *CloudDSHToolBroker) commitResult(ctx context.Context, current task.Task, step task.Step, usage task.Usage, name string, result AgentToolResult) (AgentToolResult, error) {
	ctx = withTaskToolShellExecutionEvidence(ctx, name, result)
	step.Status, step.Summary = task.StepSucceeded, "The cloud dsh tool completed and its result was saved."
	eventDetail := map[string]any{"tool": name, "operation_status": result.Status, "reused": true}
	if result.Status == AgentToolFailed {
		step.Status, step.Summary = task.StepFailed, "The Host runtime reported a definitive tool failure."
		ctx = withTaskToolResultEvidence(ctx, "execute", taskToolExecutionFailureReason)
		eventDetail["failure_reason"] = taskToolExecutionFailureReason
	}
	if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", eventDetail); err != nil {
		return AgentToolResult{}, err
	}
	result.Output = boundedToolOutput(result.Output)
	return result, nil
}

func (b *CloudDSHToolBroker) reconcile(ctx context.Context, request AgentToolRequest, operation task.Operation, step task.Step, usage task.Usage) (task.Operation, AgentToolResult, bool, error) {
	if operation.Status == task.OperationDispatched {
		var err error
		operation, err = b.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: request.Task.ID, OperationKey: request.OperationKey, LeaseOwner: request.Task.Lease.Owner,
			LeaseVersion: request.Task.Lease.Version, Status: task.OperationUnknown,
		})
		if err != nil {
			return task.Operation{}, AgentToolResult{}, false, err
		}
	}
	if operation.Status == task.OperationUnknown || operation.Status == task.OperationSucceeded || operation.Status == task.OperationFailed {
		result, err := b.runtime.ReconcileOperation(ctx, request, operation)
		if err != nil || result.Status == AgentToolUnknown {
			paused, pauseErr := b.pauseUnknown(ctx, request.Task, step, usage, request.OperationKey, "The prior operation could not be safely reconciled.")
			return operation, paused, true, errors.Join(err, pauseErr)
		}
		if result.Status == AgentToolNotFound {
			if operation.Status != task.OperationUnknown {
				paused, pauseErr := b.pauseUnknown(ctx, request.Task, step, usage, request.OperationKey, "The terminal operation no longer matches the authenticated Host runtime.")
				return operation, paused, true, pauseErr
			}
			operation, err = b.store.UpdateOperation(ctx, OperationUpdate{
				TaskID: request.Task.ID, OperationKey: request.OperationKey, LeaseOwner: request.Task.Lease.Owner,
				LeaseVersion: request.Task.Lease.Version, RuntimeGeneration: operation.RuntimeGeneration,
				RuntimeInstanceID: operation.RuntimeInstanceID, ControlVersion: operation.ControlVersion,
				RuntimeLeaseVersion: request.Task.Lease.Version, RearmAfterNotFound: true, Status: task.OperationPrepared,
			})
			if err != nil {
				paused, pauseErr := b.pauseUnknown(ctx, request.Task, step, usage, request.OperationKey, "The confirmed absent operation could not be safely rearmed.")
				return task.Operation{}, paused, true, errors.Join(err, pauseErr)
			}
			return operation, AgentToolResult{}, false, nil
		}
		if operation.Status == task.OperationSucceeded || operation.Status == task.OperationFailed {
			wantStatus := AgentToolSucceeded
			if operation.Status == task.OperationFailed {
				wantStatus = AgentToolFailed
			}
			if (result.Status != wantStatus && result.Status != AgentToolNeedsUser) ||
				!validCloudDSHToolImages(request.Call.Name, result) {
				paused, pauseErr := b.pauseUnknown(ctx, request.Task, step, usage, request.OperationKey, "The terminal ledger result changed during recovery.")
				return operation, paused, true, pauseErr
			}
			return operation, result, true, nil
		}
		operationStatus := task.OperationSucceeded
		if result.Status == AgentToolFailed {
			operationStatus = task.OperationFailed
		} else if result.Status != AgentToolSucceeded || len(result.Output) > maxAgentToolOutputBytes ||
			!validCloudDSHToolImages(request.Call.Name, result) {
			paused, pauseErr := b.pauseUnknown(ctx, request.Task, step, usage, request.OperationKey, "The reconciled operation result is unsupported.")
			return task.Operation{}, paused, true, pauseErr
		}
		operation, err = b.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: request.Task.ID, OperationKey: request.OperationKey, LeaseOwner: request.Task.Lease.Owner,
			LeaseVersion: request.Task.Lease.Version, Status: operationStatus,
			ExternalID: boundedExternalID(result.ExternalID), Result: boundedToolOutput(result.Output),
		})
		if err != nil {
			return task.Operation{}, AgentToolResult{}, false, err
		}
		result.Output = boundedToolOutput(result.Output)
		return operation, result, true, nil
	}
	return operation, AgentToolResult{}, false, nil
}

func (b *CloudDSHToolBroker) failBeforeDispatch(ctx context.Context, current task.Task, step task.Step, usage task.Usage, name string, cause error) (AgentToolResult, error) {
	summary := "The Host tool request could not be prepared."
	output := "The Host tool request was not executed because authorization or Host readiness could not be confirmed."
	detail := map[string]any{"tool": name, "phase": "prepare", "failed": true}
	var argumentError *runtimeToolArgumentValidationPreDispatchError
	if errors.As(cause, &argumentError) {
		summary = "The tool arguments were invalid; the action was not executed."
		output = invalidRuntimeToolArgumentsResult
		detail["failure_reason"] = taskToolInvalidArgumentsFailureReason
		ctx = withTaskToolResultEvidence(ctx, "prepare", taskToolInvalidArgumentsFailureReason)
	}
	step.Status, step.Summary = task.StepFailed, summary
	if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", detail); err != nil {
		return AgentToolResult{}, errors.Join(cause, err)
	}
	return AgentToolResult{Status: AgentToolFailed, Output: output}, nil
}

const invalidRuntimeToolArgumentsResult = "The tool call was not executed because its arguments do not match the declared input format. Correct the arguments and try again."

// returnInvalidRuntimeToolArguments returns a deterministic, sanitized
// pre-dispatch result to the same DSH session only when the exact runtime tool
// and scope are currently declared, the DSH call fence is valid, and the
// operation journal proves that no operation exists. Any ambiguity follows
// the unknown/error path rather than inviting the Agent to retry a possibly
// dispatched side effect.
func (b *CloudDSHToolBroker) returnInvalidRuntimeToolArguments(ctx context.Context, claimed task.Task, call ModelToolCall,
	recovering bool, checkpointOutput *string) (AgentToolResult, bool, error) {
	mapping, known := runtimeToolMappingByName(call.Name)
	if !known || mapping.internal || len(call.Arguments) > maxAgentToolArgsBytes || !utf8.ValidString(call.Arguments) {
		return AgentToolResult{}, false, nil
	}
	details, err := b.currentTaskDetails(ctx, claimed)
	if err != nil {
		return AgentToolResult{}, true, err
	}
	current := details.Task
	if current.Status != task.StatusRunning {
		return AgentToolResult{}, true, domain.NewError(domain.CodeInvalidState, "Task is not running")
	}
	definitions, err := b.Definitions(ctx, current)
	if err != nil {
		return AgentToolResult{}, true, err
	}
	declared := false
	for _, definition := range definitions {
		if definition.Name != call.Name {
			continue
		}
		if definition.RequiredScope != mapping.scope {
			return AgentToolResult{}, true, domain.NewError(domain.CodeForbidden, "dsh tool scope is not available for this Host")
		}
		declared = true
		break
	}
	if !declared {
		return AgentToolResult{}, true, domain.NewError(domain.CodeForbidden, "dsh tool is not available for this Host")
	}

	activityContext := withInvalidRuntimeToolArgumentActivity(ctx, current, call, b.stepDetailSealer)
	activity, ok := cloudDSHToolActivityFromContext(activityContext)
	if !ok {
		// Without the exact live Task/Agent fence, do not turn argument failures
		// into Agent-visible tool results.
		return AgentToolResult{}, false, nil
	}

	operationKey := agentOperationKey(call.CallID)
	operation, lookupErr := b.store.GetOperation(ctx, current.OwnerID, current.ID, operationKey)
	if lookupErr != nil && domain.CodeOf(lookupErr) != domain.CodeNotFound {
		return b.invalidRuntimeToolArgumentsUnknown(activityContext, current, details.Steps, operationKey,
			"The prior tool operation could not be checked safely.")
	}
	if lookupErr == nil {
		summary := "A prior tool operation exists for this call, so its result cannot be retried safely."
		if operation.ID == "" {
			summary = "The prior tool operation record is incomplete, so its result cannot be retried safely."
		}
		return b.invalidRuntimeToolArgumentsUnknown(activityContext, current, details.Steps, operationKey,
			summary)
	}

	prior, found, err := matchingInvalidRuntimeArgumentStep(details.Steps, current, activity)
	if err != nil {
		return AgentToolResult{}, true, err
	}
	if checkpointOutput != nil && (!recovering || !found || prior.Status != task.StepFailed || prior.Phase != "prepare" ||
		prior.FailureReason != "" || prior.Retryable || prior.Summary != "The tool arguments were invalid; the action was not executed." ||
		*checkpointOutput != dshToolResultText(AgentToolResult{
			Status: AgentToolFailed, Output: invalidRuntimeToolArgumentsResult,
		})) {
		return b.invalidRuntimeToolArgumentsUnknown(activityContext, current, details.Steps, operationKey,
			"The checkpointed tool result does not match a recorded pre-dispatch argument failure.")
	}
	if found && prior.Status == task.StepFailed {
		if prior.Phase == "prepare" && prior.FailureReason == "" && !prior.Retryable &&
			prior.Summary == "The tool arguments were invalid; the action was not executed." {
			return AgentToolResult{Status: AgentToolFailed, Output: invalidRuntimeToolArgumentsResult}, true, nil
		}
		return b.invalidRuntimeToolArgumentsUnknown(activityContext, current, details.Steps, operationKey,
			"The recorded Task step is not the expected pre-dispatch argument failure.")
	}
	if found && (prior.Phase != "prepare" || (prior.Status != task.StepQueued && prior.Status != task.StepRunning)) {
		return b.invalidRuntimeToolArgumentsUnknown(activityContext, current, details.Steps, operationKey,
			"The recorded Task step is not in a safe pre-dispatch state.")
	}

	usage := cloneTaskUsage(current.Usage)
	step := prior
	if !found {
		step, err = newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepRunning,
			"Run cloud dsh tool: "+call.Name, "Checking current authorization and validating tool arguments.")
		if err != nil {
			return AgentToolResult{}, true, err
		}
		if usage.ToolCalls >= current.Budget.ToolCalls {
			step.Status, step.Summary = task.StepWaitingUser, "The task tool call budget is exhausted."
			if err := b.commitProgress(activityContext, current, step, usage, task.StatusWaitingUser, "tool_call_budget", "task.waiting_user", nil); err != nil {
				return AgentToolResult{}, true, err
			}
			return AgentToolResult{Status: AgentToolNeedsUser}, true, nil
		}
		usage.ToolCalls++
		if err := b.commitProgress(activityContext, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
			"tool": call.Name, "operation_key": operationKey, "phase": "prepare",
		}); err != nil {
			return AgentToolResult{}, true, err
		}
	}
	result, err := b.failBeforeDispatch(activityContext, current, step, usage, call.Name,
		&runtimeToolArgumentValidationPreDispatchError{cause: domain.NewError(domain.CodeInvalidArgument, "runtime tool arguments are invalid")})
	if err != nil {
		return AgentToolResult{}, true, err
	}
	return result, true, nil
}

func matchingInvalidRuntimeArgumentStep(steps []task.Step, current task.Task, activity cloudDSHToolActivity) (task.Step, bool, error) {
	fence := activity.call.TaskFence
	if fence == nil || current.CurrentRevision == nil {
		return task.Step{}, false, domain.NewError(domain.CodeInvalidState, "tool call fence is unavailable")
	}
	var match task.Step
	for _, step := range steps {
		if step.AgentSessionID != fence.AgentSessionID || step.ToolCallKey != activity.call.CallID {
			continue
		}
		if match.ID != "" || step.ToolName != activity.call.Name || step.TaskRevision == nil ||
			*step.TaskRevision != *current.CurrentRevision || !bytes.Equal(step.RequestDigest, activity.digest) {
			return task.Step{}, false, domain.NewError(domain.CodeIdempotencyConflict, "tool call identity was reused with different inputs")
		}
		match = step
	}
	return match, match.ID != "", nil
}

func (b *CloudDSHToolBroker) invalidRuntimeToolArgumentsUnknown(ctx context.Context, current task.Task, steps []task.Step,
	operationKey, summary string) (AgentToolResult, bool, error) {
	activity, ok := cloudDSHToolActivityFromContext(ctx)
	if !ok {
		return AgentToolResult{Status: AgentToolUnknown}, true, nil
	}
	step, found, err := matchingInvalidRuntimeArgumentStep(steps, current, activity)
	if err != nil {
		return AgentToolResult{}, true, err
	}
	if !found {
		step, err = newAgentStep(current.ID, maxStepSequence(steps)+1, "tool", task.StepUnknown,
			"Tool result needs reconciliation", summary)
		if err != nil {
			return AgentToolResult{}, true, err
		}
	}
	result, err := b.pauseUnknown(ctx, current, step, cloneTaskUsage(current.Usage), operationKey, summary)
	return result, true, err
}

func (b *CloudDSHToolBroker) pauseForApproval(ctx context.Context, current task.Task, step task.Step, usage task.Usage, request AgentToolRequest, hash []byte, preparation AgentToolPreparation, previous *task.PendingRequest, totalCostMicros *int64) (AgentToolResult, error) {
	if current.CurrentRevision == nil || *current.CurrentRevision < 0 {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "Task revision is unavailable for Host approval binding")
	}
	version := int64(1)
	if previous != nil {
		version = previous.Version + 1
	}
	pending, err := newPendingApproval(current, request.Call, hash, preparation, version)
	if err != nil {
		return AgentToolResult{}, err
	}
	// EvidencePointer anchors the card to the step whose command and output
	// justify the action; TotalCostMicros is the billing snapshot at card
	// creation. Both are set here because only this pause owns the step and
	// the caller owns the billing snapshot.
	pointer, ok := task.EvidencePointerForStep(step.ID)
	if !ok {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "approval evidence step ID is invalid")
	}
	pending.EvidencePointer = pointer
	pending.TotalCostMicros = totalCostMicros
	revision := *current.CurrentRevision
	pending.TaskRevision = &revision
	step.Status, step.Summary = task.StepWaitingUser, preparation.Approval.Content
	if !strings.HasPrefix(step.Title, "Run cloud dsh MCP tool: ") {
		step.Title = "Approval required: " + request.Call.Name
	}
	if err := b.commitProgress(ctx, current, step, usage, task.StatusWaitingUser, "approval_required", "task.waiting_user", preparation.Approval, pending); err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolNeedsUser}, nil
}

func (b *CloudDSHToolBroker) commitApprovalMismatch(ctx context.Context, current task.Task, name string, steps []task.Step) (AgentToolResult, error) {
	step, err := newAgentStep(current.ID, maxStepSequence(steps)+1, "tool", task.StepFailed,
		"Approval no longer matches tool request", "The approved Host action changed; no action was executed.")
	if err != nil {
		return AgentToolResult{}, err
	}
	result := AgentToolResult{Status: AgentToolFailed, Output: "The saved approval no longer matches this exact action. It was not executed."}
	if err := b.commitProgress(ctx, current, step, current.Usage, task.StatusRunning, "", "task.step", map[string]any{
		"tool": name, "operation_status": result.Status, "approval_decision": "mismatch",
	}); err != nil {
		return AgentToolResult{}, err
	}
	return result, nil
}

func (b *CloudDSHToolBroker) rejectApprovedToolCall(ctx context.Context, current task.Task, step task.Step, usage task.Usage, request AgentToolRequest) (AgentToolResult, error) {
	step.Status, step.Summary = task.StepFailed, "The user rejected this exact action; it was not dispatched."
	result := AgentToolResult{Status: AgentToolFailed, Output: "The user rejected this exact action. It was not executed."}
	if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
		"tool": request.Call.Name, "operation_key": request.OperationKey, "operation_status": result.Status, "approval_decision": "reject",
	}); err != nil {
		return AgentToolResult{}, err
	}
	return result, nil
}

func (b *CloudDSHToolBroker) expireApprovedToolCall(ctx context.Context, current task.Task, step task.Step, usage task.Usage, request AgentToolRequest) (AgentToolResult, error) {
	step.Status, step.Summary = task.StepFailed, "The approval expired; this exact action was not dispatched."
	result := AgentToolResult{Status: AgentToolFailed, Output: "The approval expired. This exact action was not executed."}
	if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
		"tool": request.Call.Name, "operation_key": request.OperationKey, "operation_status": result.Status, "approval_decision": "expired",
	}); err != nil {
		return AgentToolResult{}, err
	}
	return result, nil
}

func (b *CloudDSHToolBroker) pauseUnknown(ctx context.Context, current task.Task, step task.Step, usage task.Usage, operationKey, summary string) (AgentToolResult, error) {
	step.Status, step.Summary = task.StepUnknown, summary
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	err := b.commitProgress(persistCtx, current, step, usage, task.StatusWaitingUser, "tool_result_unknown", "task.waiting_user", map[string]any{
		"operation_key": operationKey, "operation_status": task.OperationUnknown,
	})
	return AgentToolResult{Status: AgentToolUnknown}, err
}

func (b *CloudDSHToolBroker) markUnknown(ctx context.Context, current task.Task, operationKey string) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	_, err := b.store.UpdateOperation(persistCtx, OperationUpdate{
		TaskID: current.ID, OperationKey: operationKey, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.OperationUnknown,
	})
	return err
}

func isOptionalTaskValidationEvidenceError(err error) bool {
	var domainErr *domain.Error
	return errors.As(err, &domainErr) && domainErr.Code == domain.CodeInvalidArgument && domainErr.Details["field"] == "validation_evidence"
}

func (b *CloudDSHToolBroker) prepareTaskToolStep(ctx context.Context, current task.Task, step task.Step) (task.Step, *TaskToolCallProgress, error) {
	activity, ok := cloudDSHToolActivityFromContext(ctx)
	if !ok {
		return step, nil, nil
	}
	if step.Status == task.StepWaitingUser {
		step.Summary = "The task is waiting for your response before continuing."
	}
	details, err := b.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		return task.Step{}, nil, err
	}
	for _, existing := range details.Steps {
		if existing.AgentSessionID != activity.call.TaskFence.AgentSessionID || existing.ToolCallKey != activity.call.CallID {
			continue
		}
		if existing.ToolName != activity.call.Name || existing.TaskRevision == nil ||
			*existing.TaskRevision != *activity.current.CurrentRevision || !bytes.Equal(existing.RequestDigest, activity.digest) {
			return task.Step{}, nil, domain.NewError(domain.CodeIdempotencyConflict, "tool call identity was reused with different inputs")
		}
		step.ID, step.Sequence = existing.ID, existing.Sequence
		break
	}
	call, err := taskToolCallProgress(activity, step)
	if err != nil || call == nil {
		return step, call, err
	}
	if call.FailureReason == taskToolTestFailureReason || call.FailureReason == taskToolTestPassedReason ||
		call.FailureReason == taskToolTestSuitePassedReason {
		step.Summary = directTestEvidenceSummary
	}
	step.AgentSessionID, step.TaskRevision = call.AgentSessionID, &call.TaskRevision
	step.ToolCallKey, step.ToolName = call.CallKey, call.ToolName
	step.RequestDigest, step.Phase, step.DetailState = append([]byte(nil), call.RequestDigest...), call.Phase, call.DetailState
	return step, call, nil
}

func (b *CloudDSHToolBroker) commitProgress(ctx context.Context, current task.Task, step task.Step, usage task.Usage, status task.Status, waitingReason, eventType string, detail any, pending ...*task.PendingRequest) error {
	return b.commitProgressWithPersonalHistoryReadFence(ctx, current, step, usage, status, waitingReason, eventType, detail, nil, 0, pending...)
}

func (b *CloudDSHToolBroker) commitPersonalHistoryReadProgress(ctx context.Context, current task.Task, step task.Step,
	usage task.Usage, eventType string, detail any, settingRevision, personalChangeWatermark int64) error {
	return b.commitProgressWithPersonalHistoryReadFence(ctx, current, step, usage, task.StatusRunning, "", eventType,
		detail, &settingRevision, personalChangeWatermark)
}

func (b *CloudDSHToolBroker) commitProgressWithPersonalHistoryReadFence(ctx context.Context, current task.Task, step task.Step,
	usage task.Usage, status task.Status, waitingReason, eventType string, detail any,
	settingRevision *int64, personalChangeWatermark int64, pending ...*task.PendingRequest) error {
	if fields, ok := detail.(map[string]any); ok && fields["phase"] == "prepare" {
		step.Phase = "prepare"
	}
	step, toolCall, err := b.prepareTaskToolStep(ctx, current, step)
	if err != nil {
		return err
	}
	payload, err := agentStepEvent(step, usage, detail)
	if err != nil {
		return err
	}
	progress := TaskProgress{
		TaskID: current.ID, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		Status: status, CurrentStep: step.ID, Result: current.Result, WaitingReason: waitingReason,
		Usage: usage, Steps: []task.Step{step}, Event: task.Event{TaskID: current.ID, Type: eventType, Payload: payload},
		ToolCall: toolCall,
	}
	if settingRevision != nil {
		if toolCall == nil {
			return domain.NewError(domain.CodeInvalidState, "personal history read has no durable tool call")
		}
		progress.CloudDSHPersonalHistoryRead = &CloudDSHPersonalHistoryReadCommit{
			SettingRevision: *settingRevision, PersonalChangeWatermark: personalChangeWatermark,
			CallID: toolCall.CallKey, ToolName: toolCall.ToolName,
			AgentSessionID: toolCall.AgentSessionID, AuthorizationVersion: toolCall.AuthorizationVersion,
			TaskRevision: toolCall.TaskRevision,
		}
	}
	if len(pending) > 1 {
		return domain.InvalidArgument("pending_request", "at most one pending request may be committed")
	}
	if len(pending) == 1 {
		progress.PendingRequest = pending[0]
	}
	_, err = b.store.CommitTaskProgress(ctx, progress)
	return err
}
