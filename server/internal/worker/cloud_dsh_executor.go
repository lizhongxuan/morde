package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

const cloudDSHMaxFrame = 4 << 20
const cloudDSHMaxText = app.MaxDSHModelResponseTextBytes

// This is the persisted Morde checkpoint/protocol compatibility identifier, not the upstream DSH package release.
// Changing it requires migrating existing durable Task agent instances and checkpoints.
const cloudDSHEngineVersion = "0.2.0-rc.2"
const cloudDSHProfileVersion = "v1"
const cloudDSHDelegateToolName = "dsh_delegate"
const cloudDSHJobListToolName = "dsh_job_list"
const cloudDSHJobGetToolName = "dsh_job_get"
const cloudDSHJobOutputToolName = "dsh_job_output"
const cloudDSHJobCancelToolName = "dsh_job_cancel"

// ErrCloudDSHCompletionPending means dsh finished, but output and Task completion
// have not been independently verified and committed.
var ErrCloudDSHCompletionPending = errors.New("dsh completion requires output verification")

// ErrCloudDSHWaitingForUser means the broker durably paused the task before
// returning a tool result, so the dsh process must stop without failing it.
var ErrCloudDSHWaitingForUser = errors.New("dsh task is waiting for user input")

var errCloudDSHModelResultUnavailable = errors.New("dsh model result is unavailable or contains an unauthorized tool")

var errCloudDSHConnectionInterrupted = errors.New("dsh connection ended before a verified result")

// errCloudDSHBackgroundOutputPending asks the task worker to reclaim the Task
// after its lease expires so a completed parent checkpoint can resume with a
// server-authored prompt to read durable background-job output.
var errCloudDSHBackgroundOutputPending = errors.New("dsh background job output has not been consumed")

type cloudDSHModelFailureCategory interface {
	ModelFailureCategory() string
}

type cloudDSHModelResultError struct {
	category string
}

func (e cloudDSHModelResultError) Error() string { return errCloudDSHModelResultUnavailable.Error() }

func (e cloudDSHModelResultError) Is(target error) bool {
	return target == errCloudDSHModelResultUnavailable
}

func (e cloudDSHModelResultError) ModelFailureCategory() string { return e.category }

func cloudDSHModelErrorCategory(err error) string {
	if err == nil {
		return "invalid_response"
	}
	var category cloudDSHModelFailureCategory
	if errors.As(err, &category) {
		return category.ModelFailureCategory()
	}
	var domainErr *domain.Error
	if errors.As(err, &domainErr) {
		if domainErr.Details["reason"] == "task_model_budget_exhausted" {
			return "task_budget_exhausted"
		}
		if stage, ok := domainErr.Details["stage"].(string); ok {
			switch stage {
			case "client_validation":
				return "client_request_invalid"
			case "provider_status":
				switch domainErr.Details["provider_code"] {
				case "invalid_request_error":
					return "provider_invalid_request"
				case "unsupported_parameter":
					return "provider_unsupported_parameter"
				case "context_length_exceeded":
					return "provider_context_length_exceeded"
				case "model_not_found":
					return "provider_model_not_found"
				case "permission_denied":
					return "provider_permission_denied"
				case "invalid_api_key":
					return "provider_invalid_api_key"
				case "rate_limit_exceeded":
					return "provider_rate_limited"
				case "insufficient_quota", "billing_hard_limit_reached":
					return "provider_quota_exhausted"
				case "server_error":
					return "provider_server_error"
				default:
					return "provider_request_rejected"
				}
			}
		}
		if reason, ok := domainErr.Details["reason"].(string); ok {
			switch app.ModelClientErrorKind(reason) {
			case app.ModelErrorAuthentication, app.ModelErrorAccess, app.ModelErrorRateLimited, app.ModelErrorQuota,
				app.ModelErrorInvalidRequest, app.ModelErrorUnavailable, app.ModelErrorTimeout, app.ModelErrorTransport,
				app.ModelErrorInvalidResponse:
				return reason
			}
		}
		switch domainErr.Code {
		case domain.CodeResultTooLarge:
			return "context_length_exceeded"
		case domain.CodeInvalidArgument:
			return "invalid_request"
		case domain.CodeQuotaExceeded:
			return "quota_exhausted"
		case domain.CodeRateLimited:
			return "rate_limited"
		case domain.CodeUpstreamTimeout:
			return "timeout"
		case domain.CodeUpstreamError:
			return "upstream_error"
		case domain.CodeInvalidState:
			return "usage_unknown"
		case domain.CodeUnsupported:
			return "unsupported"
		}
	}
	return "unknown"
}

func cloudDSHModelResultErrorFor(err error) error {
	return cloudDSHModelResultError{category: cloudDSHModelErrorCategory(err)}
}

var cloudDSHID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type CloudDSHInstances interface {
	Start(context.Context, app.AgentInstanceStart) (app.AgentInstanceLease, bool, error)
	Renew(context.Context, app.AgentInstanceFence, time.Duration) (app.AgentInstanceLease, error)
}

type CloudDSHCheckpoints interface {
	Save(context.Context, app.AgentCheckpointCommand) (bool, error)
	ReadPage(context.Context, app.AgentCheckpointPageRequest) (app.AgentCheckpointPage, error)
	PendingMessages(context.Context, app.AgentCheckpointFence) ([]app.AgentConversationMessage, error)
}

type CloudDSHModels interface {
	Complete(context.Context, app.ModelCall) (app.ModelResponse, error)
}

// CloudDSHRevisionStopWitness proves that a lease was fenced specifically by
// a persisted, currently authorized revision stop request.
type CloudDSHRevisionStopWitness interface {
	RevisionStopProven(context.Context, task.Task) (bool, error)
}

// CloudDSHModelToolCapability checks the selected model using the task initiator's scope.
type CloudDSHModelToolCapability interface {
	TaskToolCapability(context.Context, task.Task) (bool, error)
}

// CloudDSHProjectContexts restores the original authorized project snapshot
// before a recovered Agent may start a Runtime.
type CloudDSHProjectContexts interface {
	LoadProjectTaskContext(context.Context, task.Task) (*task.ConversationContext, error)
	PauseProjectTaskRecovery(context.Context, task.Task) error
}

type CloudDSHTools interface {
	Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error)
	Execute(context.Context, task.Task, app.ModelToolCall) (app.AgentToolResult, error)
}

type CloudDSHChildSessions interface {
	Create(context.Context, app.AgentChildSessionCommand) (app.AgentChildSession, bool, error)
	List(context.Context, app.AgentInstanceFence, string) ([]app.AgentChildSession, error)
	Get(context.Context, app.AgentInstanceFence, string, string) (app.AgentChildSession, error)
	RequestCancel(context.Context, app.AgentChildSessionCancelCommand) (app.AgentChildSession, bool, error)
	Settle(context.Context, app.AgentChildSessionSettleCommand) (app.AgentChildSession, bool, error)
}

type CloudDSHRecoveryTools interface {
	Reconcile(context.Context, task.Task, app.ModelToolCall, *string) (app.AgentToolResult, error)
	PauseRecovery(context.Context, task.Task) error
}

type cloudDSHModelRecoveryTools interface {
	PauseModelRecovery(context.Context, task.Task, string) error
}

func pauseCloudDSHModelRecovery(ctx context.Context, tools CloudDSHRecoveryTools, value task.Task, cause error) error {
	var failure cloudDSHModelFailureCategory
	if errors.As(cause, &failure) {
		if detailed, ok := tools.(cloudDSHModelRecoveryTools); ok {
			return detailed.PauseModelRecovery(ctx, value, failure.ModelFailureCategory())
		}
	}
	return tools.PauseRecovery(ctx, value)
}

type CloudDSHCompleter interface {
	Complete(context.Context, task.Task, app.CloudDSHTaskCompletion) error
}

type CloudDSHExecutorOptions struct {
	SupervisorID     string
	SummarySealer    *app.Sealer
	AccountRuntimes  *CloudDSHAccountRuntimeManager
	Instances        CloudDSHInstances
	Checkpoints      CloudDSHCheckpoints
	Models           CloudDSHModels
	ToolCapabilities CloudDSHModelToolCapability
	ProjectContexts  CloudDSHProjectContexts
	StreamCapability CloudDSHTextStreamingCapability
	TextStreams      CloudDSHTextStreamStore
	AgentActivity    app.TaskAgentActivityRecorder
	RevisionStops    CloudDSHRevisionStopWitness
	Tools            CloudDSHTools
	Children         CloudDSHChildSessions
	Completion       CloudDSHCompleter
}

type CloudDSHExecutor struct{ options CloudDSHExecutorOptions }

func NewCloudDSHExecutor(options CloudDSHExecutorOptions) (*CloudDSHExecutor, error) {
	if !cloudDSHID.MatchString(options.SupervisorID) || options.AccountRuntimes == nil ||
		options.Instances == nil || options.Checkpoints == nil || options.Models == nil {
		return nil, errors.New("dsh requires a conversation runtime and all trusted service dependencies")
	}
	if options.ToolCapabilities == nil {
		options.ToolCapabilities, _ = options.Models.(CloudDSHModelToolCapability)
	}
	if options.StreamCapability == nil {
		options.StreamCapability, _ = options.Models.(CloudDSHTextStreamingCapability)
	}
	return &CloudDSHExecutor{options: options}, nil
}

func openCloudDSHTaskSummary(value task.Task, contextValue *task.ConversationContext, sealer *app.Sealer) error {
	if contextValue == nil || contextValue.Summary == nil {
		return nil
	}
	summary := contextValue.Summary
	expectedScope, expectedScopeID := app.ConversationSummaryPersonalScope, ""
	if value.ProjectID != "" {
		expectedScope, expectedScopeID = app.ConversationSummaryProjectScope, value.ProjectID
	}
	if summary.OwnerID != value.OwnerID || summary.ActorID != value.InitiatorID || summary.ConversationID != value.ConversationID ||
		summary.ScopeKind != expectedScope || summary.ScopeID != expectedScopeID ||
		summary.AuthorizationVersion != value.Authorization.Version {
		return errors.New("dsh conversation summary identity is invalid")
	}
	sources := make([]app.ConversationSummarySource, 0, len(summary.Sources))
	for _, source := range summary.Sources {
		sources = append(sources, app.ConversationSummarySource{Kind: source.Kind, ID: source.ID, ObjectType: source.ObjectType,
			Version: source.Version, Sequence: source.Sequence, SHA256: append([]byte(nil), source.SHA256...)})
	}
	document, err := app.OpenConversationSummary(sealer, app.StoredConversationSummary{
		Identity: app.ConversationSummaryIdentity{ServiceInstanceID: summary.ServiceInstanceID, OwnerID: summary.OwnerID,
			ActorID: summary.ActorID, ConversationID: summary.ConversationID, ScopeKind: summary.ScopeKind, ScopeID: summary.ScopeID,
			AuthorizationVersion: summary.AuthorizationVersion, ScopeVersion: summary.ScopeVersion},
		Version: summary.Version, OperationID: summary.OperationID, SummarySHA256: summary.SummarySHA256,
		SourcesSHA256: summary.SourcesSHA256, Ciphertext: summary.Ciphertext, Sources: sources,
	})
	if err != nil {
		return errors.New("dsh conversation summary could not be verified")
	}
	summary.Text = document.Text
	return nil
}

func cloudDSHDelegateTool(available []app.AgentToolDefinition) app.AgentToolDefinition {
	toolNames := make([]string, 0, len(available))
	for _, tool := range available {
		if tool.Name != cloudDSHDelegateToolName && tool.Name != "ask_user" {
			toolNames = append(toolNames, tool.Name)
		}
	}
	sort.Strings(toolNames)
	parameters, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"title":             map[string]any{"type": "string", "minLength": 1, "maxLength": 120},
			"objective":         map[string]any{"type": "string", "minLength": 1, "maxLength": 32768},
			"run_in_background": map[string]any{"type": "boolean"},
			"tool_names": map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": toolNames},
				"maxItems": 64, "uniqueItems": true},
		},
		"required":             []string{"title", "objective", "tool_names"},
		"additionalProperties": false,
	})
	return app.AgentToolDefinition{ModelTool: app.ModelTool{Name: cloudDSHDelegateToolName, Description: "Delegate a bounded task to a resumable child Agent with a selected subset of the current Task tools.",
		Parameters: parameters}}
}

func cloudDSHJobTools() []app.AgentToolDefinition {
	list, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{}, "required": []string{}, "additionalProperties": false})
	output, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{
		"job_id":    map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
		"after_seq": map[string]any{"type": "integer", "minimum": -1},
	}, "required": []string{"job_id", "after_seq"}, "additionalProperties": false})
	job, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{
		"job_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128},
	}, "required": []string{"job_id"}, "additionalProperties": false})
	cancel := job
	return []app.AgentToolDefinition{
		{ModelTool: app.ModelTool{Name: cloudDSHJobListToolName, Description: "List this task's delegated background jobs and their status.", Parameters: list}},
		{ModelTool: app.ModelTool{Name: cloudDSHJobGetToolName, Description: "Get the status of one delegated background job.", Parameters: job}},
		{ModelTool: app.ModelTool{Name: cloudDSHJobOutputToolName, Description: "Read new delegated job output after a durable event cursor.", Parameters: output}},
		{ModelTool: app.ModelTool{Name: cloudDSHJobCancelToolName, Description: "Cancel one delegated background job and wait for safe stop confirmation.", Parameters: cancel}},
	}
}

func cloudDSHJobTool(name string) bool {
	return name == cloudDSHJobListToolName || name == cloudDSHJobGetToolName || name == cloudDSHJobOutputToolName || name == cloudDSHJobCancelToolName
}

type cloudDSHDelegateArguments struct {
	Title           string   `json:"title"`
	Objective       string   `json:"objective"`
	ToolNames       []string `json:"tool_names"`
	RunInBackground bool     `json:"run_in_background"`
}

func validateCloudDSHDelegateArguments(raw []byte, parentDefinitions []app.AgentToolDefinition) (cloudDSHDelegateArguments, []app.AgentToolDefinition, error) {
	var arguments cloudDSHDelegateArguments
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) == 0 || len(raw) > 64<<10 || !uniqueCloudDSHJSON(raw) || decoder.Decode(&arguments) != nil || decoder.Decode(new(any)) != io.EOF ||
		strings.TrimSpace(arguments.Title) == "" || len(arguments.Title) > 120 || !utf8.ValidString(arguments.Title) ||
		strings.TrimSpace(arguments.Objective) == "" || len(arguments.Objective) > 32<<10 || !utf8.ValidString(arguments.Objective) ||
		arguments.ToolNames == nil || len(arguments.ToolNames) > 64 {
		return cloudDSHDelegateArguments{}, nil, errors.New("child Agent request is invalid")
	}
	selected := make(map[string]struct{}, len(arguments.ToolNames))
	for _, name := range arguments.ToolNames {
		if !cloudDSHID.MatchString(name) || name == cloudDSHDelegateToolName || name == "ask_user" {
			return cloudDSHDelegateArguments{}, nil, errors.New("child Agent tool selection is invalid")
		}
		if _, exists := selected[name]; exists {
			return cloudDSHDelegateArguments{}, nil, errors.New("child Agent tool selection is duplicated")
		}
		selected[name] = struct{}{}
	}
	definitions := make([]app.AgentToolDefinition, 0, len(selected))
	for _, definition := range parentDefinitions {
		if _, allowed := selected[definition.Name]; allowed {
			definitions = append(definitions, definition)
			delete(selected, definition.Name)
		}
	}
	if len(selected) != 0 {
		return cloudDSHDelegateArguments{}, nil, errors.New("child Agent requested a tool outside the parent authorization")
	}
	sort.Slice(definitions, func(i, j int) bool { return definitions[i].Name < definitions[j].Name })
	return arguments, definitions, nil
}

// ExecuteTask leaves completion pending unless the stop receipt and the
// separately injected completion service both confirm the result.
func (e *CloudDSHExecutor) ExecuteTask(ctx context.Context, value task.Task) (retErr error) {
	freshAgent := value.AgentInstanceID == "" && value.AgentGeneration == 0 && value.AgentSessionID == "" &&
		value.AgentEngineVersion == "" && value.AgentProfileVersion == "" && value.CheckpointThroughSequence == -1 && len(value.CheckpointDigest) == 0
	recoveringAgent := cloudDSHID.MatchString(value.AgentInstanceID) && value.AgentGeneration >= 1 &&
		cloudDSHID.MatchString(value.AgentSessionID) && value.AgentEngineVersion == cloudDSHEngineVersion &&
		value.AgentProfileVersion == cloudDSHProfileVersion &&
		((value.CheckpointThroughSequence == -1 && len(value.CheckpointDigest) == 0) ||
			(value.CheckpointThroughSequence >= 0 && value.CheckpointThroughSequence <= 1<<53-1 && len(value.CheckpointDigest) == sha256.Size))
	if ctx == nil || !domain.ValidTaskID(value.ID) || !domain.ValidUserID(value.OwnerID) || !domain.ValidUserID(value.InitiatorID) ||
		!domain.ValidModelConnectionID(value.ModelID) || value.Status != task.StatusRunning || value.ExecutionLocation != "server" ||
		value.Lease.Owner == "" || value.Lease.Version < 1 || value.Lease.ExpiresAt == nil || !value.Lease.ExpiresAt.After(time.Now()) ||
		value.Authorization.Version < 1 || value.Budget.Validate() != nil || (!freshAgent && !recoveringAgent) ||
		!utf8.ValidString(value.Objective) || strings.TrimSpace(value.Objective) == "" || len(value.Objective) > cloudDSHMaxText {
		return errors.New("dsh task is invalid or requires unsupported recovery")
	}
	if value.StepRetry != nil && !validCloudDSHStepRetryForTask(value, time.Now()) {
		return errors.New("dsh task step retry is expired or outside its immutable execution fence")
	}
	if value.StepRetry != nil {
		if _, ok := e.options.Checkpoints.(app.TaskStepRetryIntentConsumer); !ok || e.options.Tools == nil ||
			!app.SupportsSafeTaskStepRetryTool(value.StepRetry.ToolName) {
			return errors.New("dsh task step retry admission is unavailable")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	projectRecovery := recoveringAgent && value.ProjectID != "" && value.CheckpointThroughSequence >= 0
	if projectRecovery {
		if e.options.ProjectContexts == nil {
			return errors.New("project task context verification is unavailable")
		}
		original, err := e.options.ProjectContexts.LoadProjectTaskContext(ctx, value)
		if err != nil || original == nil || original.Scope != "project" || original.OwnerID != value.OwnerID ||
			original.InitiatorID != value.InitiatorID || original.ProjectID != value.ProjectID || original.ConversationID != value.ConversationID {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			pauseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return errors.Join(errors.New("project task context could not be verified"),
				e.options.ProjectContexts.PauseProjectTaskRecovery(pauseCtx, value))
		}
		if err := openCloudDSHTaskSummary(value, original, e.options.SummarySealer); err != nil {
			pauseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			return errors.Join(errors.New("project task summary could not be verified"),
				e.options.ProjectContexts.PauseProjectTaskRecovery(pauseCtx, value))
		}
		value.ConversationContext = original
	} else if err := openCloudDSHTaskSummary(value, value.ConversationContext, e.options.SummarySealer); err != nil {
		return err
	}
	toolsAllowed := false
	if e.options.ToolCapabilities != nil {
		allowed, err := e.options.ToolCapabilities.TaskToolCapability(ctx, value)
		if err != nil {
			return fmt.Errorf("dsh model tool capability is unavailable: %w", err)
		}
		toolsAllowed = allowed
	} else if e.options.Tools != nil || e.options.Children != nil {
		return errors.New("dsh model tool capability is unavailable")
	}
	if value.ToolsDisabled {
		toolsAllowed = false
	}
	streamAllowed := false
	if !toolsAllowed && e.options.TextStreams != nil && e.options.StreamCapability != nil {
		// An unavailable optional display capability must never prevent the
		// same authorized model from serving an ordinary non-streamed reply.
		allowed, err := e.options.StreamCapability.TaskTextStreamingCapability(ctx, value)
		streamAllowed = err == nil && allowed
	}
	instanceID, err := randomCloudDSHID("agent_")
	if err != nil {
		return err
	}
	sessionID := value.AgentSessionID
	if freshAgent {
		sessionID, err = randomCloudDSHID("session_")
		if err != nil {
			return err
		}
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	accountRuntime, err := e.options.AccountRuntimes.Acquire(ctx, value.OwnerID, value.ConversationID)
	if err != nil {
		return err
	}
	runtimeFence := accountRuntime.runtime.Fence
	fence := app.AgentInstanceFence{TaskID: value.ID, OwnerID: value.OwnerID, InitiatorID: value.InitiatorID, AgentInstanceID: instanceID,
		AgentSessionID: sessionID, SupervisorID: e.options.SupervisorID, LeaseOwner: value.Lease.Owner,
		LeaseVersion: value.Lease.Version, AuthorizationVersion: value.Authorization.Version,
		EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
	accountRuntime.startMu.Lock()
	unlockRuntimeTaskStart := accountRuntime.startMu.Unlock
	defer func() {
		if unlockRuntimeTaskStart != nil {
			unlockRuntimeTaskStart()
		}
	}()
	lease, replayed, err := e.options.Instances.Start(ctx, app.AgentInstanceStart{Fence: fence, RuntimeFence: &runtimeFence,
		SupervisorNonce: nonce, LeaseDuration: app.MaxAgentLease})
	if err != nil {
		return err
	}
	fence.AgentGeneration = lease.Fence.AgentGeneration
	if replayed || fence.AgentGeneration < 1 || lease.Fence != fence || !validCloudDSHLease(lease.ExpiresAt) ||
		lease.RuntimeFence == nil || *lease.RuntimeFence != accountRuntime.runtime.Fence || lease.RuntimeSequence < 1 {
		return errors.New("dsh instance lease was not confirmed")
	}
	// Tools receive the identity just granted by the instance service, rather
	// than the empty or previous-generation identity from the claimed Task.
	value.AgentInstanceID = fence.AgentInstanceID
	value.AgentGeneration = fence.AgentGeneration
	value.AgentSessionID = fence.AgentSessionID
	value.AgentEngineVersion = fence.EngineVersion
	value.AgentProfileVersion = fence.ProfileVersion
	var completion app.CloudDSHTaskCompletion
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		stopErr := e.options.AccountRuntimes.StopTask(stopCtx, accountRuntime, fence)
		if stopErr != nil {
			if errors.Is(retErr, ErrCloudDSHCompletionPending) {
				retErr = nil
			}
			retErr = errors.Join(retErr, errors.New("dsh process-tree stop is unconfirmed"))
			return
		}
		if errors.Is(retErr, errCloudDSHPersonalMemoryChanged) && ctx.Err() == nil {
			if pauser, ok := e.options.Tools.(interface {
				PausePersonalMemoryRecovery(context.Context, task.Task) error
			}); ok {
				pauseCtx, cancelPause := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				retErr = errors.Join(retErr, pauser.PausePersonalMemoryRecovery(pauseCtx, value))
				cancelPause()
			}
		}
		if errors.Is(retErr, errCloudDSHModelResultUnavailable) && ctx.Err() == nil {
			if recoveryTools, ok := e.options.Tools.(CloudDSHRecoveryTools); ok {
				pauseCtx, cancelPause := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				retErr = errors.Join(retErr, pauseCloudDSHModelRecovery(pauseCtx, recoveryTools, value, retErr))
				cancelPause()
			}
		}
		if errors.Is(retErr, ErrCloudDSHCompletionPending) && e.options.Completion != nil {
			if ctx.Err() != nil {
				retErr = ctx.Err()
				return
			}
			completeCtx, cancelComplete := context.WithTimeout(ctx, 30*time.Second)
			defer cancelComplete()
			if err := e.options.Completion.Complete(completeCtx, value, completion); err != nil {
				retErr = err
				return
			}
			retErr = nil
		}
	}()
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(value.Budget.ActiveSeconds)*time.Second)
	defer cancel()
	runCtx, cancelCause := context.WithCancelCause(runCtx)
	defer cancelCause(nil)
	input, output, err := accountRuntime.StartTask(runCtx, fence, nonce, lease)
	if err != nil {
		return errors.New("dsh supervisor could not start the process")
	}
	if input != nil {
		defer func() { _ = input.Close() }()
	}
	if output != nil {
		defer func() { _ = output.Close() }()
	}
	if input == nil || output == nil {
		return errors.New("dsh supervisor did not provide private pipes")
	}
	if !lease.ExpiresAt.After(time.Now()) {
		return errors.New("dsh instance lease expired before process startup")
	}
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		e.renewCloudDSHLease(runCtx, cancelCause, value, fence, lease.ExpiresAt, accountRuntime)
	}()
	releaseRuntimeTaskStart := func() {
		if unlockRuntimeTaskStart != nil {
			unlockRuntimeTaskStart()
			unlockRuntimeTaskStart = nil
		}
	}
	err = e.runCloudDSH(runCtx, value, fence, input, output,
		recoveringAgent && value.CheckpointThroughSequence >= 0, toolsAllowed, streamAllowed, &completion, releaseRuntimeTaskStart)
	releaseRuntimeTaskStart()
	cancelCause(context.Canceled)
	<-renewDone
	// A disconnected child is reclaimed from its durable checkpoint; the operation ledger decides whether a tool can be replayed.
	if err != nil && !errors.Is(err, errCloudDSHConnectionInterrupted) && !errors.Is(err, errCloudDSHBackgroundOutputPending) &&
		!errors.Is(err, ErrCloudDSHCompletionPending) && !errors.Is(err, errCloudDSHModelResultUnavailable) &&
		!errors.Is(err, errCloudDSHPersonalMemoryChanged) && recoveringAgent && ctx.Err() == nil {
		if recoveryTools, ok := e.options.Tools.(CloudDSHRecoveryTools); ok {
			pauseCtx, cancelPause := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err = errors.Join(err, recoveryTools.PauseRecovery(pauseCtx, value))
			cancelPause()
		}
	}
	if errors.Is(err, ErrCloudDSHCompletionPending) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cause := context.Cause(runCtx); cause != nil && !errors.Is(cause, context.Canceled) {
			return cause
		}
	}
	if errors.Is(err, ErrCloudDSHWaitingForUser) {
		return nil
	}
	return err
}

func validCloudDSHStepRetryForTask(value task.Task, now time.Time) bool {
	intent := value.StepRetry
	if intent == nil || intent.Consumed || !intent.ExpiresAt.After(now) || !domain.ValidTaskID(intent.TaskID) ||
		!domain.ValidUserID(intent.OwnerID) || !domain.ValidStepID(intent.SourceStepID) || !domain.ValidStepID(intent.AttemptStepID) ||
		intent.TaskID != value.ID || intent.OwnerID != value.OwnerID || intent.Attempt < 2 ||
		intent.RetryCallID != "morde_step_retry_"+intent.AttemptStepID || intent.SourceCallID == "" || len(intent.SourceCallID) > 256 ||
		intent.AgentSessionID != value.AgentSessionID || intent.TaskRevision < 0 || value.CurrentRevision == nil ||
		*value.CurrentRevision != intent.TaskRevision || value.DesiredRevision == nil || *value.DesiredRevision != intent.TaskRevision ||
		intent.AuthorizationVersion != value.Authorization.Version || intent.AuthorizationVersion < 1 || intent.HostID != value.HostID ||
		intent.HostRuntimeGeneration < 1 || intent.ToolName == "" || !app.SupportsSafeTaskStepRetryTool(intent.ToolName) ||
		len(intent.RequestDigest) != sha256.Size || intent.CheckpointThroughSequence != value.CheckpointThroughSequence ||
		len(intent.CheckpointDigest) != sha256.Size || !bytes.Equal(intent.CheckpointDigest, value.CheckpointDigest) ||
		(intent.WorkspaceID == "" && (value.WorkspaceID != "" || intent.InputRevision != nil || intent.WorkspaceCheckpointRevision != nil ||
			intent.WorkspaceBindingVersion != 0 || intent.WorkspaceRevision != nil)) ||
		(intent.WorkspaceID != "" && (intent.WorkspaceID != value.WorkspaceID || !sameCloudDSHInt64(intent.InputRevision, value.InputRevision) ||
			!sameCloudDSHInt64(intent.WorkspaceCheckpointRevision, value.WorkspaceCheckpointRevision) ||
			intent.WorkspaceBindingVersion < 1 || intent.WorkspaceRevision == nil)) {
		return false
	}
	return true
}

func sameCloudDSHInt64(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (e *CloudDSHExecutor) renewCloudDSHLease(ctx context.Context, cancel context.CancelCauseFunc, value task.Task, fence app.AgentInstanceFence, expires time.Time,
	accountRuntime *cloudDSHAccountRuntimeSession) {
	for {
		delay := time.Until(expires) / 3
		if delay <= 0 {
			cancel(errors.New("dsh instance lease expired"))
			return
		}
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		renewCtx, cancelRenew := context.WithTimeout(ctx, 5*time.Second)
		lease, err := e.options.Instances.Renew(renewCtx, fence, app.MaxAgentLease)
		if err == nil && (lease.Fence != fence || !validCloudDSHLease(lease.ExpiresAt)) {
			err = errors.New("invalid dsh instance renewal")
		}
		if err == nil {
			err = e.options.AccountRuntimes.RenewTask(renewCtx, accountRuntime, fence, lease.ExpiresAt)
		}
		cancelRenew()
		if err != nil {
			cause := error(errors.New("dsh instance lease renewal failed"))
			lifecycle := app.ModelTaskLifecycle(ctx)
			if e.options.RevisionStops != nil && ctx.Err() == nil && lifecycle != nil && lifecycle.Err() == nil {
				checkCtx, cancelCheck := context.WithTimeout(lifecycle, time.Second)
				proven, checkErr := e.options.RevisionStops.RevisionStopProven(checkCtx, value)
				cancelCheck()
				if checkErr == nil && proven && ctx.Err() == nil && lifecycle.Err() == nil {
					cause = app.ErrTaskRevisionInterrupted
				}
			}
			cancel(cause)
			return
		}
		expires = lease.ExpiresAt
	}
}

func validCloudDSHLease(expires time.Time) bool {
	remaining := time.Until(expires)
	return remaining > 0 && remaining <= app.MaxAgentLease
}

func randomCloudDSHID(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw), nil
}

type cloudDSHIdentity struct {
	TaskID               string `json:"task_id"`
	OwnerID              string `json:"owner_id"`
	AgentID              string `json:"agent_id"`
	SessionID            string `json:"session_id"`
	Generation           int64  `json:"generation"`
	AuthorizationVersion int64  `json:"authorization_version"`
}

type cloudDSHFrame struct {
	Type string `json:"type"`
	cloudDSHIdentity
	Version             int             `json:"version,omitempty"`
	Engine              string          `json:"engine,omitempty"`
	EngineVer           string          `json:"engine_version,omitempty"`
	ID                  string          `json:"id,omitempty"`
	From                *int64          `json:"from_seq,omitempty"`
	Through             *int64          `json:"through_seq,omitempty"`
	ObservationSequence *int64          `json:"observation_sequence,omitempty"`
	Payload             string          `json:"payload,omitempty"`
	SHA256              string          `json:"sha256,omitempty"`
	Request             json.RawMessage `json:"request,omitempty"`
	CallID              string          `json:"call_id,omitempty"`
	Name                string          `json:"name,omitempty"`
	Arguments           json.RawMessage `json:"arguments,omitempty"`
	State               string          `json:"state,omitempty"`
	Status              string          `json:"status,omitempty"`
	Code                string          `json:"code,omitempty"`
	Summary             string          `json:"summary,omitempty"`
	JobID               string          `json:"job_id,omitempty"`
}

type cloudDSHRead struct {
	frame []byte
	err   error
}

func checkpointFence(fence app.AgentInstanceFence) app.AgentCheckpointFence {
	return app.AgentCheckpointFence{TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID, AgentInstanceID: fence.AgentInstanceID,
		AgentGeneration: fence.AgentGeneration, AgentSessionID: fence.AgentSessionID, AgentRootSessionID: fence.AgentSessionID,
		AuthorizationVersion: fence.AuthorizationVersion,
		LeaseOwner:           fence.LeaseOwner, LeaseVersion: fence.LeaseVersion, EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion}
}

func readCloudDSHFrames(ctx context.Context, source io.Reader, out chan<- cloudDSHRead) {
	reader := bufio.NewReader(source)
	for {
		var line []byte
		for {
			part, err := reader.ReadSlice('\n')
			if len(line)+len(part) > cloudDSHMaxFrame {
				select {
				case out <- cloudDSHRead{err: errors.New("frame too large")}:
				case <-ctx.Done():
				}
				return
			}
			line = append(line, part...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				select {
				case out <- cloudDSHRead{err: err}:
				case <-ctx.Done():
				}
				return
			}
			break
		}
		select {
		case out <- cloudDSHRead{frame: line[:len(line)-1]}:
		case <-ctx.Done():
			return
		}
	}
}

func decodeCloudDSHFrame(raw []byte) (cloudDSHFrame, error) {
	var frame cloudDSHFrame
	if !utf8.Valid(raw) || !uniqueCloudDSHJSON(raw) {
		return frame, errors.New("invalid dsh JSON frame")
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil || keys == nil {
		return frame, errors.New("invalid dsh JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&frame) != nil || decoder.Decode(new(any)) != io.EOF {
		return cloudDSHFrame{}, errors.New("invalid dsh frame fields")
	}
	fields := map[string][]string{
		"ready":             {"type", "version", "engine", "engine_version"},
		"agent_status":      {"type", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "status", "observation_sequence"},
		"checkpoint":        {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "from_seq", "through_seq", "payload", "sha256"},
		"checkpoint_read":   {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "from_seq"},
		"model":             {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "through_seq", "request", "sha256"},
		"tool_call":         {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "through_seq", "call_id", "name", "arguments"},
		"child_create":      {"type", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "through_seq", "call_id", "arguments"},
		"child_settle":      {"type", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "through_seq", "call_id", "status", "sha256", "summary"},
		"job_cancel_result": {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "job_id", "status"},
		"model_cancel":      {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id"},
		"settled":           {"type", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "state", "through_seq"},
		"failed":            {"type", "code", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id"},
	}
	required, ok := fields[frame.Type]
	if !ok || len(keys) != len(required) {
		keyNames := make([]string, 0, len(keys))
		for key := range keys {
			keyNames = append(keyNames, key)
		}
		sort.Strings(keyNames)
		return cloudDSHFrame{}, fmt.Errorf("unsupported dsh frame fields: type=%q keys=%v", frame.Type, keyNames)
	}
	for _, name := range required {
		if _, ok := keys[name]; !ok {
			return cloudDSHFrame{}, errors.New("missing dsh frame field")
		}
	}
	return frame, nil
}

// Reject duplicate keys at every depth before either side interprets a frame.
func uniqueCloudDSHJSON(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var walk func(int) bool
	walk = func(depth int) bool {
		if depth > 64 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return false
				}
				if _, exists := seen[name]; exists {
					return false
				}
				seen[name] = struct{}{}
				if !walk(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for decoder.More() {
				if !walk(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	return walk(0) && decoder.Decode(new(any)) == io.EOF
}

func cloudDSHDigest(data []byte, encoded string) bool {
	want, err := hex.DecodeString(encoded)
	if err != nil || len(encoded) != 64 || encoded != strings.ToLower(encoded) {
		return false
	}
	sum := sha256.Sum256(data)
	return subtle.ConstantTimeCompare(sum[:], want) == 1
}

func cloudDSHToolPayload(definitions []app.AgentToolDefinition) []map[string]any {
	tools := make([]map[string]any, 0, len(definitions))
	for _, definition := range definitions {
		tools = append(tools, map[string]any{
			"name": definition.Name, "description": definition.Description, "parameters": definition.Parameters,
		})
	}
	return tools
}

func validCloudDSHToolDefinitions(definitions []app.AgentToolDefinition) bool {
	if len(definitions) > 64 {
		return false
	}
	seen := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		if !cloudDSHID.MatchString(definition.Name) || !utf8.ValidString(definition.Description) ||
			strings.TrimSpace(definition.Description) == "" || len(definition.Description) > 2048 ||
			len(definition.Parameters) == 0 || len(definition.Parameters) > 64<<10 || !uniqueCloudDSHJSON(definition.Parameters) {
			return false
		}
		var schema struct {
			Type                 string                     `json:"type"`
			Properties           map[string]json.RawMessage `json:"properties"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
		}
		if json.Unmarshal(definition.Parameters, &schema) != nil || schema.Type != "object" ||
			schema.Properties == nil ||
			schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			return false
		}
		if _, exists := seen[definition.Name]; exists {
			return false
		}
		seen[definition.Name] = struct{}{}
	}
	return true
}

func validCloudDSHModelRequest(raw []byte, modelID string, definitions []app.AgentToolDefinition) bool {
	type modelTool struct {
		Name         string          `json:"name"`
		Description  string          `json:"description"`
		Parameters   json.RawMessage `json:"parameters"`
		DeferLoading bool            `json:"deferLoading"`
	}
	type modelBlock struct {
		Type       string          `json:"type"`
		Text       string          `json:"text"`
		ID         string          `json:"id"`
		Name       string          `json:"name"`
		Arguments  string          `json:"arguments"`
		Attachment json.RawMessage `json:"attachment"`
	}
	type modelMessage struct {
		ID         string          `json:"id"`
		Role       string          `json:"role"`
		Source     json.RawMessage `json:"source"`
		Content    []modelBlock    `json:"content"`
		ToolCallID string          `json:"toolCallId"`
		IsError    bool            `json:"isError"`
	}
	var request struct {
		Provider    string            `json:"provider"`
		Model       string            `json:"model"`
		Tools       []json.RawMessage `json:"tools"`
		Messages    []modelMessage    `json:"messages"`
		MaxTokens   *int              `json:"max_tokens"`
		Temperature *float64          `json:"temperature"`
	}
	if !validCloudDSHToolDefinitions(definitions) || !uniqueCloudDSHJSON(raw) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || request.Provider != "morde" ||
		request.Model != modelID || request.Tools == nil || len(request.Tools) != len(definitions) || len(request.Messages) == 0 {
		return false
	}
	expectedTools := make(map[string]app.AgentToolDefinition, len(definitions))
	for _, definition := range definitions {
		expectedTools[definition.Name] = definition
	}
	allowed := make(map[string]struct{}, len(definitions))
	imageCount, imageBytes := 0, 0
	for _, rawTool := range request.Tools {
		var fields map[string]json.RawMessage
		if json.Unmarshal(rawTool, &fields) != nil || (len(fields) != 3 && len(fields) != 4) {
			return false
		}
		for key := range fields {
			if key != "name" && key != "description" && key != "parameters" && key != "deferLoading" {
				return false
			}
		}
		for _, key := range []string{"name", "description", "parameters"} {
			if _, ok := fields[key]; !ok {
				return false
			}
		}
		if _, ok := fields["deferLoading"]; ok {
			var deferred bool
			if json.Unmarshal(fields["deferLoading"], &deferred) != nil || deferred {
				return false
			}
		}
		var offered modelTool
		if json.Unmarshal(rawTool, &offered) != nil || offered.DeferLoading {
			return false
		}
		definition, ok := expectedTools[offered.Name]
		if !ok || offered.Description != definition.Description {
			return false
		}
		if _, duplicate := allowed[offered.Name]; duplicate {
			return false
		}
		parameters, ok := canonicalCloudDSHJSON(offered.Parameters)
		if !ok {
			return false
		}
		want, ok := canonicalCloudDSHJSON(definition.Parameters)
		if !ok || !bytes.Equal(parameters, want) {
			return false
		}
		allowed[offered.Name] = struct{}{}
	}
	for _, message := range request.Messages {
		if message.Role != "system" && message.Role != "developer" && message.Role != "user" && message.Role != "assistant" && message.Role != "tool" {
			return false
		}
		if message.Role == "tool" {
			if message.ToolCallID == "" || !cloudDSHID.MatchString(message.ToolCallID) {
				return false
			}
		} else if message.ToolCallID != "" || message.IsError {
			return false
		}
		for _, block := range message.Content {
			switch block.Type {
			case "text":
			case "reasoning":
				if message.Role != "assistant" || !utf8.ValidString(block.Text) || len(block.Text) > cloudDSHMaxText {
					return false
				}
			case "tool-call":
				if message.Role != "assistant" || !cloudDSHID.MatchString(block.ID) {
					return false
				}
				if _, ok := allowed[block.Name]; !ok || !validCloudDSHJSONObject([]byte(block.Arguments)) {
					return false
				}
			case "image":
				if message.Role != "tool" || message.ToolCallID == "" || len(block.Attachment) == 0 {
					return false
				}
				_, data, err := decodeCloudDSHImageAttachment(block.Attachment)
				if err != nil {
					return false
				}
				imageCount++
				imageBytes += len(data)
				if imageCount > cloudDSHMaxModelImageCount || imageBytes > cloudDSHMaxModelImageBytes {
					return false
				}
			default:
				return false
			}
		}
	}
	return true
}

func canonicalCloudDSHJSON(raw []byte) ([]byte, bool) {
	if !uniqueCloudDSHJSON(raw) {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, false
	}
	encoded, err := json.Marshal(value)
	return encoded, err == nil
}

func validCloudDSHJSONObject(raw []byte) bool {
	if len(raw) == 0 || len(raw) > 64<<10 || !uniqueCloudDSHJSON(raw) {
		return false
	}
	var value map[string]json.RawMessage
	return json.Unmarshal(raw, &value) == nil && value != nil
}

func validCloudDSHModelResponse(response app.ModelResponse, definitions []app.AgentToolDefinition) bool {
	if !app.ValidDSHModelResponse(response) {
		return false
	}
	if len(response.ToolCalls) == 0 {
		return true
	}
	if len(definitions) == 0 {
		return false
	}
	for _, call := range response.ToolCalls {
		if !cloudDSHToolAllowed(definitions, call.Name) || !validCloudDSHJSONObject([]byte(call.Arguments)) {
			return false
		}
	}
	return true
}

func cloudDSHToolAllowed(definitions []app.AgentToolDefinition, name string) bool {
	for _, definition := range definitions {
		if definition.Name == name {
			return true
		}
	}
	return false
}

func writeCloudDSHFrame(ctx context.Context, target io.WriteCloser, frame any) error {
	if writer, ok := target.(interface {
		writeFrame(context.Context, any) error
	}); ok {
		return writer.writeFrame(ctx, frame)
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(frame); err != nil || buffer.Len() > cloudDSHMaxFrame {
		return errors.New("dsh output frame is invalid")
	}
	encoded := buffer.Bytes()
	done := make(chan error, 1)
	go func() {
		for len(encoded) > 0 {
			n, err := target.Write(encoded)
			if err != nil {
				done <- err
				return
			}
			if n < 1 || n > len(encoded) {
				done <- io.ErrShortWrite
				return
			}
			encoded = encoded[n:]
		}
		done <- nil
	}()
	select {
	case <-ctx.Done():
		_ = target.Close()
		return context.Cause(ctx)
	case err := <-done:
		if err != nil {
			return fmt.Errorf("dsh output connection failed: %w", err)
		}
		return nil
	}
}

func writeCloudDSHModel(ctx context.Context, target io.WriteCloser, id string, identity cloudDSHIdentity, response app.ModelResponse) error {
	if response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 ||
		response.Usage.InputTokens > 1<<53-1 || response.Usage.OutputTokens > 1<<53-1 ||
		(len(response.ToolCalls) == 0 && response.StopReason != "stop") || (len(response.ToolCalls) > 0 && response.StopReason != "tool_calls") {
		return errors.New("dsh model usage is invalid")
	}
	chunks := make([]any, 0, 7+len(response.ToolCalls))
	blockIndex := 0
	if response.Reasoning != "" {
		chunks = append(chunks,
			map[string]any{"type": "block-start", "index": blockIndex, "blockType": "reasoning"},
			map[string]any{"type": "reasoning-delta", "index": blockIndex, "text": response.Reasoning},
			map[string]any{"type": "block-end", "index": blockIndex, "block": map[string]any{"type": "reasoning", "text": response.Reasoning}},
		)
		blockIndex++
	}
	if response.Text != "" {
		chunks = append(chunks,
			map[string]any{"type": "block-start", "index": blockIndex, "blockType": "text"},
			map[string]any{"type": "text-delta", "index": blockIndex, "text": response.Text},
			map[string]any{"type": "block-end", "index": blockIndex, "block": map[string]any{"type": "text", "text": response.Text}},
		)
		blockIndex++
	}
	for index, call := range response.ToolCalls {
		chunks = append(chunks, map[string]any{"type": "tool-call-delta", "index": blockIndex + index,
			"id": call.CallID, "name": call.Name, "argumentsDelta": call.Arguments})
	}
	finishReason := "stop"
	if len(response.ToolCalls) > 0 {
		finishReason = "tool-calls"
	}
	chunks = append(chunks,
		map[string]any{"type": "usage", "usage": map[string]any{"inputTokens": response.Usage.InputTokens, "outputTokens": response.Usage.OutputTokens}},
		map[string]any{"type": "finish", "reason": map[string]any{"kind": finishReason}},
	)
	frame := func(kind string) map[string]any {
		return map[string]any{"type": kind, "task_id": identity.TaskID, "owner_id": identity.OwnerID,
			"agent_id": identity.AgentID, "session_id": identity.SessionID, "generation": identity.Generation,
			"authorization_version": identity.AuthorizationVersion, "id": id}
	}
	for index, chunk := range chunks {
		message := frame("model_chunk")
		message["index"] = index
		message["chunk"] = chunk
		if err := writeCloudDSHFrame(ctx, target, message); err != nil {
			return err
		}
	}
	message := frame("model_end")
	message["ok"] = true
	return writeCloudDSHFrame(ctx, target, message)
}

var _ TaskExecutor = (*CloudDSHExecutor)(nil)
