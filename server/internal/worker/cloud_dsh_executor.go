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
	"path/filepath"
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
const cloudDSHMaxText = 256 << 10
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

var cloudDSHID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// CloudDSHLaunch is passed only to the separately trusted supervisor. Nonce and
// fence must never be placed in the child environment or command line.
type CloudDSHLaunch struct {
	NodePath    string
	ScriptPath  string
	Environment []string
	Fence       app.AgentInstanceFence
	Nonce       []byte
	ExpiresAt   time.Time
}

// CloudDSHSupervisor must isolate the child from Portal credentials and its
// filesystem, enforce ExpiresAt independently of the worker, and supervise the
// entire process tree. Stop must return a signed, verifiable tree-stop receipt.
// No production implementation is supplied by this adapter.
type CloudDSHSupervisor interface {
	Start(context.Context, CloudDSHLaunch) (io.WriteCloser, io.ReadCloser, error)
	Renew(context.Context, app.AgentInstanceFence, time.Time) error
	Stop(context.Context, app.AgentInstanceFence, []byte) (app.AgentStopReceipt, error)
}

type CloudDSHStopFinalizer interface {
	AgentStopConfirmed(app.AgentInstanceFence, []byte)
}

type CloudDSHInstances interface {
	Start(context.Context, app.AgentInstanceStart) (app.AgentInstanceLease, bool, error)
	Renew(context.Context, app.AgentInstanceFence, time.Duration) (app.AgentInstanceLease, error)
	ConfirmStopped(context.Context, app.AgentStopReceipt) (bool, error)
}

type CloudDSHCheckpoints interface {
	Save(context.Context, app.AgentCheckpointCommand) (bool, error)
	ReadPage(context.Context, app.AgentCheckpointPageRequest) (app.AgentCheckpointPage, error)
}

type CloudDSHModels interface {
	Complete(context.Context, app.ModelCall) (app.ModelResponse, error)
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

type CloudDSHCompleter interface {
	Complete(context.Context, task.Task, app.CloudDSHTaskCompletion) error
}

type CloudDSHExecutorOptions struct {
	NodePath        string
	ScriptPath      string
	SupervisorID    string
	Supervisor      CloudDSHSupervisor
	AccountRuntimes *CloudDSHAccountRuntimeManager
	Instances       CloudDSHInstances
	Checkpoints     CloudDSHCheckpoints
	Models          CloudDSHModels
	Tools           CloudDSHTools
	Children        CloudDSHChildSessions
	Completion      CloudDSHCompleter
}

type CloudDSHExecutor struct{ options CloudDSHExecutorOptions }

func NewCloudDSHExecutor(options CloudDSHExecutorOptions) (*CloudDSHExecutor, error) {
	if !filepath.IsAbs(options.NodePath) || !filepath.IsAbs(options.ScriptPath) ||
		!cloudDSHID.MatchString(options.SupervisorID) || options.Supervisor == nil ||
		options.Instances == nil || options.Checkpoints == nil || options.Models == nil {
		return nil, errors.New("dsh requires absolute paths and all trusted supervisor and service dependencies")
	}
	return &CloudDSHExecutor{options: options}, nil
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
	if ctx == nil || !domain.ValidTaskID(value.ID) || !domain.ValidUserID(value.OwnerID) ||
		!domain.ValidModelConnectionID(value.ModelID) || value.Status != task.StatusRunning || value.ExecutionLocation != "server" ||
		value.Lease.Owner == "" || value.Lease.Version < 1 || value.Lease.ExpiresAt == nil || !value.Lease.ExpiresAt.After(time.Now()) ||
		value.Authorization.Version < 1 || value.Budget.Validate() != nil || (!freshAgent && !recoveringAgent) ||
		!utf8.ValidString(value.Objective) || strings.TrimSpace(value.Objective) == "" || len(value.Objective) > cloudDSHMaxText {
		return errors.New("dsh task is invalid or requires unsupported recovery")
	}
	if err := ctx.Err(); err != nil {
		return err
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
	var accountRuntime *cloudDSHAccountRuntimeSession
	var runtimeFence *app.AccountAgentRuntimeFence
	if e.options.AccountRuntimes != nil {
		accountRuntime, err = e.options.AccountRuntimes.Acquire(ctx, value.OwnerID)
		if err != nil {
			return err
		}
		fence := accountRuntime.runtime.Fence
		runtimeFence = &fence
	}
	fence := app.AgentInstanceFence{TaskID: value.ID, OwnerID: value.OwnerID, AgentInstanceID: instanceID,
		AgentSessionID: sessionID, SupervisorID: e.options.SupervisorID, LeaseOwner: value.Lease.Owner,
		LeaseVersion: value.Lease.Version, AuthorizationVersion: value.Authorization.Version,
		EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
	var unlockRuntimeTaskStart func()
	if accountRuntime != nil {
		accountRuntime.startMu.Lock()
		unlockRuntimeTaskStart = accountRuntime.startMu.Unlock
		defer func() {
			if unlockRuntimeTaskStart != nil {
				unlockRuntimeTaskStart()
			}
		}()
	}
	lease, replayed, err := e.options.Instances.Start(ctx, app.AgentInstanceStart{Fence: fence, RuntimeFence: runtimeFence,
		SupervisorNonce: nonce, LeaseDuration: app.MaxAgentLease})
	if err != nil {
		return err
	}
	fence.AgentGeneration = lease.Fence.AgentGeneration
	if replayed || fence.AgentGeneration < 1 || lease.Fence != fence || !validCloudDSHLease(lease.ExpiresAt) ||
		(accountRuntime != nil && (lease.RuntimeFence == nil || *lease.RuntimeFence != accountRuntime.runtime.Fence || lease.RuntimeSequence < 1)) {
		return errors.New("dsh instance lease was not confirmed")
	}
	var completion app.CloudDSHTaskCompletion
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var stopErr error
		if accountRuntime != nil {
			stopErr = e.options.AccountRuntimes.StopTask(stopCtx, accountRuntime, fence)
		} else {
			receipt, err := e.options.Supervisor.Stop(stopCtx, fence, nonce)
			stopErr = err
			if stopErr == nil && (receipt.Fence != fence || subtle.ConstantTimeCompare(receipt.SupervisorNonce, nonce) != 1 || !receipt.TreeStopped) {
				stopErr = errors.New("supervisor returned an invalid process-tree stop receipt")
			}
			if stopErr == nil {
				_, stopErr = e.options.Instances.ConfirmStopped(stopCtx, receipt)
				if stopErr == nil {
					if finalizer, ok := e.options.Supervisor.(CloudDSHStopFinalizer); ok {
						finalizer.AgentStopConfirmed(fence, nonce)
					}
				}
			}
		}
		if stopErr != nil {
			if errors.Is(retErr, ErrCloudDSHCompletionPending) {
				retErr = nil
			}
			retErr = errors.Join(retErr, errors.New("dsh process-tree stop is unconfirmed"))
			return
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
	var input io.WriteCloser
	var output io.ReadCloser
	if accountRuntime != nil {
		input, output, err = accountRuntime.StartTask(runCtx, fence, nonce, lease)
	} else {
		input, output, err = e.options.Supervisor.Start(runCtx, CloudDSHLaunch{
			NodePath: e.options.NodePath, ScriptPath: e.options.ScriptPath,
			Environment: []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "NODE_ENV=production"},
			Fence:       fence, Nonce: append([]byte(nil), nonce...), ExpiresAt: lease.ExpiresAt,
		})
	}
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
		e.renewCloudDSHLease(runCtx, cancelCause, fence, lease.ExpiresAt, accountRuntime)
	}()
	releaseRuntimeTaskStart := func() {
		if unlockRuntimeTaskStart != nil {
			unlockRuntimeTaskStart()
			unlockRuntimeTaskStart = nil
		}
	}
	err = e.runCloudDSH(runCtx, value, fence, input, output,
		recoveringAgent && value.CheckpointThroughSequence >= 0, &completion, releaseRuntimeTaskStart)
	releaseRuntimeTaskStart()
	cancelCause(context.Canceled)
	<-renewDone
	if err != nil && !errors.Is(err, ErrCloudDSHCompletionPending) && recoveringAgent && ctx.Err() == nil {
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

func (e *CloudDSHExecutor) renewCloudDSHLease(ctx context.Context, cancel context.CancelCauseFunc, fence app.AgentInstanceFence, expires time.Time,
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
		if err == nil && accountRuntime != nil {
			err = e.options.AccountRuntimes.RenewTask(renewCtx, accountRuntime, fence, lease.ExpiresAt)
		} else if err == nil {
			err = e.options.Supervisor.Renew(renewCtx, fence, lease.ExpiresAt)
		}
		cancelRenew()
		if err != nil {
			cancel(errors.New("dsh instance lease renewal failed"))
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
	Version   int             `json:"version,omitempty"`
	Engine    string          `json:"engine,omitempty"`
	EngineVer string          `json:"engine_version,omitempty"`
	ID        string          `json:"id,omitempty"`
	From      *int64          `json:"from_seq,omitempty"`
	Through   *int64          `json:"through_seq,omitempty"`
	Payload   string          `json:"payload,omitempty"`
	SHA256    string          `json:"sha256,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	State     string          `json:"state,omitempty"`
	Status    string          `json:"status,omitempty"`
	Code      string          `json:"code,omitempty"`
	Summary   string          `json:"summary,omitempty"`
	JobID     string          `json:"job_id,omitempty"`
}

type cloudDSHRead struct {
	frame []byte
	err   error
}

func checkpointFence(fence app.AgentInstanceFence) app.AgentCheckpointFence {
	return app.AgentCheckpointFence{TaskID: fence.TaskID, OwnerID: fence.OwnerID, AgentInstanceID: fence.AgentInstanceID,
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
		"checkpoint":        {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "from_seq", "through_seq", "payload", "sha256"},
		"checkpoint_read":   {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "from_seq"},
		"model":             {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "through_seq", "request", "sha256"},
		"tool_call":         {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "through_seq", "call_id", "name", "arguments"},
		"child_create":      {"type", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "through_seq", "call_id", "arguments"},
		"child_settle":      {"type", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "through_seq", "call_id", "status", "sha256", "summary"},
		"job_cancel_result": {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "job_id", "status"},
		"model_cancel":      {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id"},
		"settled":           {"type", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "state", "through_seq"},
		"failed":            {"type", "code"},
	}
	required, ok := fields[frame.Type]
	if !ok || len(keys) != len(required) {
		return cloudDSHFrame{}, errors.New("unsupported dsh frame fields")
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
			Type                 string          `json:"type"`
			Properties           json.RawMessage `json:"properties"`
			AdditionalProperties *bool           `json:"additionalProperties"`
		}
		if json.Unmarshal(definition.Parameters, &schema) != nil || schema.Type != "object" ||
			len(schema.Properties) == 0 || string(schema.Properties) == "null" ||
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
	if !response.Usage.Known || response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 ||
		response.Usage.InputTokens > 1<<53-1 || response.Usage.OutputTokens > 1<<53-1 ||
		!utf8.ValidString(response.Text) || len(response.Text) > cloudDSHMaxText || len(response.ToolCalls) > 64 {
		return false
	}
	if len(response.ToolCalls) == 0 {
		return response.StopReason == "stop" && strings.TrimSpace(response.Text) != ""
	}
	if response.StopReason != "tool_calls" || len(definitions) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(response.ToolCalls))
	for _, call := range response.ToolCalls {
		if !cloudDSHID.MatchString(call.CallID) || !cloudDSHToolAllowed(definitions, call.Name) || !validCloudDSHJSONObject([]byte(call.Arguments)) {
			return false
		}
		if _, exists := seen[call.CallID]; exists {
			return false
		}
		seen[call.CallID] = struct{}{}
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
	chunks := make([]any, 0, 4+len(response.ToolCalls))
	if response.Text != "" {
		chunks = append(chunks,
			map[string]any{"type": "block-start", "index": 0, "blockType": "text"},
			map[string]any{"type": "text-delta", "index": 0, "text": response.Text},
			map[string]any{"type": "block-end", "index": 0, "block": map[string]any{"type": "text", "text": response.Text}},
		)
	}
	for index, call := range response.ToolCalls {
		chunks = append(chunks, map[string]any{"type": "tool-call-delta", "index": index,
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
