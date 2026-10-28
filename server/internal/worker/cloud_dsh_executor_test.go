package worker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

// This test supervisor launches a direct child only; it does not implement the
// independent process-tree or expiry guarantees required in production.
type cloudDSHTestSupervisor struct {
	mu         sync.Mutex
	cmd        *exec.Cmd
	key        []byte
	launch     CloudDSHLaunch
	badReceipt bool
}

func (s *cloudDSHTestSupervisor) Start(_ context.Context, launch CloudDSHLaunch) (io.WriteCloser, io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.launch = launch
	cmd := exec.Command(launch.NodePath, launch.ScriptPath)
	cmd.Env = append([]string(nil), launch.Environment...)
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	s.cmd = cmd
	return in, out, nil
}

func (s *cloudDSHTestSupervisor) Renew(context.Context, app.AgentInstanceFence, time.Time) error {
	return nil
}

func (s *cloudDSHTestSupervisor) Stop(_ context.Context, fence app.AgentInstanceFence, nonce []byte) (app.AgentStopReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil {
		_ = s.cmd.Process.Kill()
		_ = s.cmd.Wait()
	}
	receipt := app.AgentStopReceipt{Fence: fence, SupervisorNonce: append([]byte(nil), nonce...), TreeStopped: true}
	if !s.badReceipt {
		payload, err := app.AgentStopReceiptSigningPayload(receipt)
		if err != nil {
			return receipt, err
		}
		mac := hmac.New(sha256.New, s.key)
		_, _ = mac.Write(payload)
		receipt.Signature = mac.Sum(nil)
	}
	return receipt, nil
}

type cloudDSHTestInstanceStore struct {
	mu          sync.Mutex
	stopped     int
	generation  int64
	active      app.AgentInstanceFence
	wasStopped  bool
	leaseLength time.Duration
	renewError  error
}

func (s *cloudDSHTestInstanceStore) StartAgentInstance(_ context.Context, request app.AgentInstanceStart) (app.AgentInstanceLease, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.generation > 0 && (!s.wasStopped || request.Fence.AgentSessionID != s.active.AgentSessionID ||
		request.Fence.AgentInstanceID == s.active.AgentInstanceID || request.Fence.EngineVersion != s.active.EngineVersion ||
		request.Fence.ProfileVersion != s.active.ProfileVersion) {
		return app.AgentInstanceLease{}, false, errors.New("prior agent cannot be recovered")
	}
	fence := request.Fence
	s.generation++
	fence.AgentGeneration = s.generation
	s.active, s.wasStopped = fence, false
	length := s.leaseLength
	if length == 0 {
		length = 15 * time.Second
	}
	return app.AgentInstanceLease{Fence: fence, ExpiresAt: time.Now().Add(length)}, false, nil
}

func (s *cloudDSHTestInstanceStore) RenewAgentInstance(_ context.Context, fence app.AgentInstanceFence, _ time.Duration) (app.AgentInstanceLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.renewError != nil {
		return app.AgentInstanceLease{}, s.renewError
	}
	if s.active != fence || s.wasStopped {
		return app.AgentInstanceLease{}, errors.New("agent instance is not active")
	}
	return app.AgentInstanceLease{Fence: fence, ExpiresAt: time.Now().Add(15 * time.Second)}, nil
}

func (s *cloudDSHTestInstanceStore) ConfirmAgentStopped(_ context.Context, proof app.VerifiedAgentStop) (bool, error) {
	receipt, _ := proof.Value()
	if !receipt.TreeStopped {
		return false, errors.New("not stopped")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if receipt.Fence != s.active || s.wasStopped {
		return false, errors.New("unexpected agent stop")
	}
	s.wasStopped = true
	s.stopped++
	return false, nil
}

type cloudDSHTestCheckpoints struct {
	commands  []app.AgentCheckpointCommand
	reads     []app.AgentCheckpointPageRequest
	err       error
	afterSave func(app.AgentCheckpointCommand)
}

func (s *cloudDSHTestCheckpoints) Save(_ context.Context, command app.AgentCheckpointCommand) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	sum := sha256.Sum256(command.Payload)
	if !bytes.Equal(sum[:], command.Digest) {
		return false, errors.New("wrong digest")
	}
	s.commands = append(s.commands, command)
	if s.afterSave != nil {
		s.afterSave(command)
	}
	return false, nil
}

func (s *cloudDSHTestCheckpoints) ReadPage(_ context.Context, request app.AgentCheckpointPageRequest) (app.AgentCheckpointPage, error) {
	s.reads = append(s.reads, request)
	for _, command := range s.commands {
		if command.Fence.AgentSessionID != request.SessionID || command.From != request.From || command.Through > request.Through {
			continue
		}
		if command.Through == request.Through && !bytes.Equal(command.Digest, request.ThroughDigest) {
			return app.AgentCheckpointPage{}, errors.New("pinned history changed")
		}
		return app.AgentCheckpointPage{From: command.From, Through: command.Through,
			Digest: append([]byte(nil), command.Digest...), Payload: append([]byte(nil), command.Payload...)}, nil
	}
	return app.AgentCheckpointPage{}, errors.New("checkpoint page not found")
}

type cloudDSHTestModels struct {
	calls     []app.ModelCall
	response  app.ModelResponse
	responses []app.ModelResponse
	block     bool
	complete  func(context.Context, app.ModelCall) (app.ModelResponse, error)
}

func (m *cloudDSHTestModels) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	m.calls = append(m.calls, call)
	if m.complete != nil {
		return m.complete(ctx, call)
	}
	if m.block {
		<-ctx.Done()
		return app.ModelResponse{}, ctx.Err()
	}
	if len(m.responses) >= len(m.calls) {
		return m.responses[len(m.calls)-1], nil
	}
	return m.response, nil
}

type cloudDSHTestTools struct {
	definitions       []app.AgentToolDefinition
	result            app.AgentToolResult
	calls             []app.ModelToolCall
	recoveryCalls     []app.ModelToolCall
	recoveryExpected  []*string
	pauseRecoveryCall int
	afterExecute      func()
}

type cloudDSHTestChildSessions struct {
	createCommand app.AgentChildSessionCommand
	settleCommand app.AgentChildSessionSettleCommand
	child         app.AgentChildSession
	creates       int
	settles       int
	lists         int
	gets          int
}

func (s *cloudDSHTestChildSessions) Create(_ context.Context, command app.AgentChildSessionCommand) (app.AgentChildSession, bool, error) {
	s.creates++
	s.createCommand = command
	if s.child.SessionID != "" {
		return s.child, true, nil
	}
	s.child = app.AgentChildSession{TaskID: command.Fence.TaskID, SessionID: "child_test", ParentSessionID: command.ParentSessionID,
		SpawnKey: command.SpawnKey, Background: command.Background,
		Step:              task.Step{ID: "step_child", TaskID: command.Fence.TaskID, Status: task.StepRunning, Title: command.Title},
		CheckpointThrough: -1, AllowedTools: append([]string{}, command.AllowedTools...)}
	return s.child, false, nil
}

func (s *cloudDSHTestChildSessions) List(_ context.Context, _ app.AgentInstanceFence, actorSessionID string) ([]app.AgentChildSession, error) {
	s.lists++
	if s.child.Background && s.child.ParentSessionID == actorSessionID {
		return []app.AgentChildSession{s.child}, nil
	}
	return []app.AgentChildSession{}, nil
}

func (s *cloudDSHTestChildSessions) Get(_ context.Context, _ app.AgentInstanceFence, actorSessionID, sessionID string) (app.AgentChildSession, error) {
	s.gets++
	if s.child.SessionID != sessionID || s.child.ParentSessionID != actorSessionID {
		return app.AgentChildSession{}, errors.New("child session is unavailable")
	}
	return s.child, nil
}

func (s *cloudDSHTestChildSessions) RequestCancel(_ context.Context, command app.AgentChildSessionCancelCommand) (app.AgentChildSession, bool, error) {
	if s.child.SessionID != command.SessionID || s.child.ParentSessionID != command.ActorSessionID || !s.child.Background {
		return app.AgentChildSession{}, false, errors.New("background job is unavailable")
	}
	if s.child.CancelRequested {
		return s.child, true, nil
	}
	s.child.CancelRequested = true
	return s.child, false, nil
}

func (s *cloudDSHTestChildSessions) Settle(_ context.Context, command app.AgentChildSessionSettleCommand) (app.AgentChildSession, bool, error) {
	s.settles++
	s.settleCommand = command
	s.child.CheckpointThrough = command.CheckpointThrough
	s.child.CheckpointDigest = append([]byte(nil), command.CheckpointDigest...)
	s.child.Step.Status, s.child.Step.Summary = command.Status, command.Summary
	return s.child, false, nil
}

func (t *cloudDSHTestTools) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return append([]app.AgentToolDefinition(nil), t.definitions...), nil
}

func (t *cloudDSHTestTools) Execute(_ context.Context, _ task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	t.calls = append(t.calls, call)
	if t.afterExecute != nil {
		t.afterExecute()
	}
	return t.result, nil
}

func (t *cloudDSHTestTools) Reconcile(_ context.Context, _ task.Task, call app.ModelToolCall, expected *string) (app.AgentToolResult, error) {
	t.recoveryCalls = append(t.recoveryCalls, call)
	if expected == nil {
		t.recoveryExpected = append(t.recoveryExpected, nil)
	} else {
		value := *expected
		t.recoveryExpected = append(t.recoveryExpected, &value)
	}
	return t.result, nil
}

func (t *cloudDSHTestTools) PauseRecovery(context.Context, task.Task) error {
	t.pauseRecoveryCall++
	return nil
}

type cloudDSHTestCompleter struct {
	instances  *cloudDSHTestInstanceStore
	completion app.CloudDSHTaskCompletion
	calls      int
}

func (c *cloudDSHTestCompleter) Complete(_ context.Context, _ task.Task, completion app.CloudDSHTaskCompletion) error {
	c.instances.mu.Lock()
	stopped := c.instances.stopped
	c.instances.mu.Unlock()
	if stopped != 1 {
		return errors.New("completion ran before process-tree stop confirmation")
	}
	c.calls++
	c.completion = completion
	return nil
}

func cloudDSHReadTool() app.AgentToolDefinition {
	return app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "file_read", Description: "Read a workspace file.", Parameters: json.RawMessage(`{"type":"object","properties":{"workspace_id":{"type":"string"},"path":{"type":"string"}},"required":["workspace_id","path"],"additionalProperties":false}`),
	}}
}

func cloudDSHWriteTool() app.AgentToolDefinition {
	return app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "file_write", Description: "Write a Workspace file.", Parameters: json.RawMessage(`{"type":"object","properties":{"workspace_id":{"type":"string"},"path":{"type":"string"},"content":{"type":"string"}},"required":["workspace_id","path","content"],"additionalProperties":false}`),
	}}
}

func cloudDSHFixture(t *testing.T) (*CloudDSHExecutor, *cloudDSHTestSupervisor, *cloudDSHTestInstanceStore, *cloudDSHTestCheckpoints, *cloudDSHTestModels, task.Task) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("local Node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "project", "nanoMuse", "harness", "morde-cloud-runtime", "bin.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, 32)
	store := &cloudDSHTestInstanceStore{}
	auth, err := app.NewAgentStopAuthenticator("supervisor-test", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, auth)
	if err != nil {
		t.Fatal(err)
	}
	supervisor := &cloudDSHTestSupervisor{key: key}
	checkpoints := &cloudDSHTestCheckpoints{}
	models := &cloudDSHTestModels{response: app.ModelResponse{Text: "local result", StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 2, OutputTokens: 1}}}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{NodePath: node, ScriptPath: script, SupervisorID: "supervisor-test",
		Supervisor: supervisor, Instances: instances, Checkpoints: checkpoints, Models: models})
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Minute)
	value := task.Task{ID: "q1234567", OwnerID: "u1234567", ModelID: "p1234567", Objective: "local test", Status: task.StatusRunning,
		ExecutionLocation: "server", Lease: task.Lease{Owner: "worker-test", Version: 1, ExpiresAt: &expires},
		Authorization: task.Authorization{Version: 1}, Budget: task.Budget{ActiveSeconds: 20, ToolCalls: 1, ModelTokens: 1000}, CheckpointThroughSequence: -1}
	return executor, supervisor, store, checkpoints, models, value
}

func TestCloudDSHExecutorRunsHostlessServerTaskWithoutCompletingIt(t *testing.T) {
	t.Setenv("MORDE_TEST_PRIVATE_ENV", "private")
	executor, supervisor, store, checkpoints, models, value := cloudDSHFixture(t)
	if value.HostID != "" {
		t.Fatalf("fixture HostID = %q, want no Host for a server-only task", value.HostID)
	}
	err := executor.ExecuteTask(context.Background(), value)
	if !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("candidate result = %v", err)
	}
	if len(models.calls) != 1 || len(checkpoints.commands) == 0 || store.stopped != 1 {
		t.Fatalf("models=%d checkpoints=%d stopped=%d", len(models.calls), len(checkpoints.commands), store.stopped)
	}
	call := models.calls[0]
	if call.Source != app.ModelCallTask || call.ModelID != value.ModelID || call.Task == nil || call.Task.TaskID != value.ID ||
		call.Task.ThroughSequence < 0 || !validCloudDSHModelRequest(call.RawRequest, value.ModelID, nil) {
		t.Fatalf("model call fence/request is invalid")
	}
	for _, env := range supervisor.launch.Environment {
		if strings.Contains(env, "MORDE_TEST_PRIVATE_ENV") || strings.Contains(env, "private") {
			t.Fatal("inherited secret reached child environment")
		}
	}
	if supervisor.launch.Fence.TaskID != value.ID || supervisor.launch.ExpiresAt.IsZero() {
		t.Fatal("supervisor lease fence missing")
	}
}

func TestCloudDSHExecutorRequiresCheckpointAndRejectsTools(t *testing.T) {
	for _, scenario := range []string{"checkpoint", "tool", "receipt"} {
		t.Run(scenario, func(t *testing.T) {
			executor, supervisor, store, checkpoints, models, value := cloudDSHFixture(t)
			switch scenario {
			case "checkpoint":
				checkpoints.err = errors.New("storage unavailable")
			case "tool":
				models.response.ToolCalls = []app.ModelToolCall{{CallID: "c1", Name: "shell", Arguments: "{}"}}
			case "receipt":
				supervisor.badReceipt = true
			}
			err := executor.ExecuteTask(context.Background(), value)
			if err == nil || errors.Is(err, ErrCloudDSHCompletionPending) {
				t.Fatalf("unsafe result: %v", err)
			}
			if scenario == "checkpoint" && len(models.calls) != 0 {
				t.Fatal("model called before durable checkpoint")
			}
			if scenario == "receipt" && store.stopped != 0 {
				t.Fatal("unverified stop receipt accepted")
			}
		})
	}
}

func TestCloudDSHExecutorRunsAuthorizedToolAndReturnsDurableResultToNextModelCall(t *testing.T) {
	executor, _, _, _, models, value := cloudDSHFixture(t)
	tool := cloudDSHReadTool()
	tools := &cloudDSHTestTools{definitions: []app.AgentToolDefinition{tool}, result: app.AgentToolResult{
		Status: app.AgentToolSucceeded, Output: `{"path":"README.md","text":"from Host"}`,
	}}
	executor.options.Tools = tools
	models.responses = []app.ModelResponse{
		{ToolCalls: []app.ModelToolCall{{CallID: "call_read_1", Name: tool.Name, Arguments: `{"workspace_id":"w1234567","path":"README.md"}`}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2}},
		{Text: "finished", StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 1}},
	}
	err := executor.ExecuteTask(context.Background(), value)
	if !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("tool run = %v", err)
	}
	if len(tools.calls) != 1 || tools.calls[0].CallID != "call_read_1" || tools.calls[0].Name != tool.Name ||
		len(models.calls) != 2 || !validCloudDSHModelRequest(models.calls[0].RawRequest, value.ModelID, []app.AgentToolDefinition{tool}) ||
		!validCloudDSHModelRequest(models.calls[1].RawRequest, value.ModelID, []app.AgentToolDefinition{tool}) {
		t.Fatalf("tool calls=%#v model calls=%d", tools.calls, len(models.calls))
	}
	var second struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
			Content    []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(models.calls[1].RawRequest, &second); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range second.Messages {
		if message.Role == "tool" && message.ToolCallID == "call_read_1" && len(message.Content) == 1 &&
			message.Content[0].Text == tools.result.Output {
			found = true
		}
	}
	if !found {
		t.Fatalf("next model request did not include the persisted tool result: %s", models.calls[1].RawRequest)
	}
}

func TestCloudDSHExecutorRunsAndSettlesPersistedChildSession(t *testing.T) {
	executor, _, _, checkpoints, models, value := cloudDSHFixture(t)
	tool := cloudDSHReadTool()
	tools := &cloudDSHTestTools{definitions: []app.AgentToolDefinition{tool}, result: app.AgentToolResult{
		Status: app.AgentToolSucceeded, Output: `{"path":"README.md","text":"from child"}`,
	}}
	children := &cloudDSHTestChildSessions{}
	executor.options.Tools, executor.options.Children = tools, children
	models.responses = []app.ModelResponse{
		{ToolCalls: []app.ModelToolCall{{CallID: "delegate_1", Name: cloudDSHDelegateToolName,
			Arguments: `{"title":"Inspect README","objective":"Read README and report the key point.","tool_names":["file_read"]}`}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2}},
		{ToolCalls: []app.ModelToolCall{{CallID: "child_read_1", Name: tool.Name,
			Arguments: `{"workspace_id":"w1234567","path":"README.md"}`}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 3, OutputTokens: 1}},
		{Text: "Child result", StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 5, OutputTokens: 1}},
		{Text: "Parent result", StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 1}},
	}
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("child Agent result = %v", err)
	}
	rootSessionID := executor.options.Supervisor.(*cloudDSHTestSupervisor).launch.Fence.AgentSessionID
	if children.creates != 1 || children.settles != 1 || children.createCommand.ParentSessionID != rootSessionID ||
		children.createCommand.SpawnKey != "delegate_1" || !sameWorkerToolNames(children.createCommand.AllowedTools, []string{"file_read"}) ||
		children.settleCommand.ActorSessionID != rootSessionID || children.settleCommand.SessionID != "child_test" ||
		children.settleCommand.Status != task.StepSucceeded || children.settleCommand.CheckpointThrough < 0 ||
		len(children.settleCommand.CheckpointDigest) != sha256.Size || children.settleCommand.Summary != "Child result" {
		t.Fatalf("child lifecycle create=%#v settle=%#v counts=%d/%d", children.createCommand, children.settleCommand, children.creates, children.settles)
	}
	if len(models.calls) != 4 || models.calls[1].Task == nil || models.calls[1].Task.AgentSessionID != "child_test" ||
		models.calls[1].Task.AgentRootSessionID != rootSessionID || models.calls[1].Task.TaskID != value.ID ||
		!validCloudDSHModelRequest(models.calls[1].RawRequest, value.ModelID, []app.AgentToolDefinition{tool}) {
		t.Fatalf("child model call was not scoped to its persisted Session: %#v", models.calls)
	}
	if len(tools.calls) != 1 || !strings.HasPrefix(tools.calls[0].CallID, "dsh_") {
		t.Fatalf("child tool call was not namespaced to its Session: %#v", tools.calls)
	}
	if len(checkpoints.commands) == 0 {
		t.Fatal("child Agent produced no durable checkpoints")
	}
	foundChildCheckpoint := false
	for _, command := range checkpoints.commands {
		if command.Fence.AgentSessionID == "child_test" && command.Fence.AgentRootSessionID == rootSessionID {
			foundChildCheckpoint = true
		}
	}
	if !foundChildCheckpoint {
		t.Fatal("child checkpoint was not bound to its root Agent Session")
	}
}

func TestCloudDSHExecutorBackgroundJobListStatusOutputAndCursor(t *testing.T) {
	executor, _, _, checkpoints, models, value := cloudDSHFixture(t)
	tool := cloudDSHReadTool()
	children := &cloudDSHTestChildSessions{}
	tools := &cloudDSHTestTools{definitions: []app.AgentToolDefinition{tool}}
	executor.options.Tools, executor.options.Children = tools, children
	checkpoints.afterSave = func(command app.AgentCheckpointCommand) {
		if command.Fence.AgentSessionID == children.child.SessionID {
			children.child.CheckpointThrough = command.Through
			children.child.CheckpointDigest = append([]byte(nil), command.Digest...)
		}
	}
	rootCalls := 0
	var rootSessionID string
	pendingGetID := "get_job"
	responseWithTool := func(id, name, arguments string) app.ModelResponse {
		return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: id, Name: name, Arguments: arguments}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 1}}
	}
	toolOutput := func(raw json.RawMessage, callID string) (string, bool) {
		var request struct {
			Messages []struct {
				Role       string `json:"role"`
				ToolCallID string `json:"toolCallId"`
				Content    []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(raw, &request) != nil {
			return "", false
		}
		for _, message := range request.Messages {
			if message.Role == "tool" && message.ToolCallID == callID && len(message.Content) == 1 {
				return message.Content[0].Text, true
			}
		}
		return "", false
	}
	models.complete = func(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
		if call.Task == nil {
			return app.ModelResponse{}, errors.New("model call is missing its Agent Session fence")
		}
		if call.Task.AgentSessionID == "child_test" {
			return app.ModelResponse{Text: "Persistent background output.", StopReason: "stop",
				Usage: app.ModelUsage{Known: true, InputTokens: 3, OutputTokens: 1}}, nil
		}
		rootSessionID = call.Task.AgentSessionID
		rootCalls++
		switch rootCalls {
		case 1:
			return responseWithTool("spawn_background", cloudDSHDelegateToolName,
				`{"title":"Persistent report","objective":"Produce a short report.","tool_names":["file_read"],"run_in_background":true}`), nil
		case 2:
			return responseWithTool("list_jobs", cloudDSHJobListToolName, `{}`), nil
		case 3:
			return responseWithTool("get_job", cloudDSHJobGetToolName, `{"job_id":"child_test"}`), nil
		default:
			if output, ok := toolOutput(call.RawRequest, "read_job_output"); ok {
				var result struct {
					JobID      string `json:"job_id"`
					Status     string `json:"status"`
					NextCursor int64  `json:"next_cursor"`
					Output     string `json:"output"`
				}
				if json.Unmarshal([]byte(output), &result) != nil || result.JobID != "child_test" || result.Status != "succeeded" ||
					result.NextCursor < 0 || result.Output != "Persistent background output." {
					return app.ModelResponse{}, errors.New("background output or cursor is not durable")
				}
				return app.ModelResponse{Text: "Background job result verified.", StopReason: "stop",
					Usage: app.ModelUsage{Known: true, InputTokens: 5, OutputTokens: 1}}, nil
			}
			if output, ok := toolOutput(call.RawRequest, pendingGetID); ok {
				var result cloudDSHJobView
				if json.Unmarshal([]byte(output), &result) != nil || result.JobID != "child_test" {
					return app.ModelResponse{}, errors.New("background job status is invalid")
				}
				if result.Status == "running" || result.Status == "cancelling" {
					pendingGetID = fmt.Sprintf("get_job_%d", rootCalls)
					return responseWithTool(pendingGetID, cloudDSHJobGetToolName, `{"job_id":"child_test"}`), nil
				}
				if result.Status != "succeeded" || result.OutputCursor < 0 {
					return app.ModelResponse{}, errors.New("background job did not reach a verified terminal state")
				}
				return responseWithTool("read_job_output", cloudDSHJobOutputToolName,
					`{"job_id":"child_test","after_seq":-1}`), nil
			}
			if output, ok := toolOutput(call.RawRequest, "list_jobs"); ok {
				var jobs []cloudDSHJobView
				if json.Unmarshal([]byte(output), &jobs) != nil || len(jobs) != 1 || jobs[0].JobID != "child_test" {
					return app.ModelResponse{}, errors.New("background job list omitted its persisted child")
				}
				return responseWithTool("get_job", cloudDSHJobGetToolName, `{"job_id":"child_test"}`), nil
			}
			return app.ModelResponse{}, errors.New("background job conversation is missing the expected tool result")
		}
	}
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("background job run = %v", err)
	}
	if rootSessionID == "" || children.creates != 1 || children.settles != 1 || !children.createCommand.Background ||
		children.settleCommand.Status != task.StepSucceeded || children.child.Step.Status != task.StepSucceeded || children.lists < 1 || children.gets < 1 {
		t.Fatalf("background job lifecycle root=%q create=%#v settle=%#v child=%#v list/get=%d/%d",
			rootSessionID, children.createCommand, children.settleCommand, children.child, children.lists, children.gets)
	}
	if len(tools.calls) != 0 || len(models.calls) < 5 {
		t.Fatalf("internal job tools escaped the child-session broker: tools=%#v models=%d", tools.calls, len(models.calls))
	}
	if rootTail, childTail := latestAgentCheckpoint(checkpoints.commands, rootSessionID), latestAgentCheckpoint(checkpoints.commands, "child_test"); rootTail.Fence.AgentSessionID == "" || childTail.Fence.AgentSessionID == "" {
		t.Fatalf("root and child checkpoints missing: root=%#v child=%#v", rootTail, childTail)
	}
}

func TestCloudDSHExecutorResumesBackgroundJobAfterRuntimeRestartWithoutReplayingTool(t *testing.T) {
	executor, supervisor, _, checkpoints, models, value := cloudDSHFixture(t)
	tool := cloudDSHReadTool()
	tools := &cloudDSHTestTools{definitions: []app.AgentToolDefinition{tool}, result: app.AgentToolResult{
		Status: app.AgentToolSucceeded, Output: `{"path":"README.md","text":"once"}`,
	}}
	children := &cloudDSHTestChildSessions{}
	executor.options.Tools, executor.options.Children = tools, children
	checkpoints.afterSave = func(command app.AgentCheckpointCommand) {
		if command.Fence.AgentSessionID == children.child.SessionID {
			children.child.CheckpointThrough = command.Through
			children.child.CheckpointDigest = append([]byte(nil), command.Digest...)
		}
	}
	rootCalls, interrupted := 0, false
	models.complete = func(_ context.Context, call app.ModelCall) (app.ModelResponse, error) {
		if call.Task == nil {
			return app.ModelResponse{}, errors.New("model call is missing its Agent Session fence")
		}
		if call.Task.AgentSessionID == "child_test" {
			if len(tools.recoveryCalls) == 0 {
				return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "background_read", Name: tool.Name,
					Arguments: `{"workspace_id":"w1234567","path":"README.md"}`}}, StopReason: "tool_calls",
					Usage: app.ModelUsage{Known: true, InputTokens: 3, OutputTokens: 1}}, nil
			}
			return app.ModelResponse{Text: "Background work complete.", StopReason: "stop",
				Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 1}}, nil
		}
		rootCalls++
		if rootCalls == 1 {
			return app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "spawn_background_restart", Name: cloudDSHDelegateToolName,
				Arguments: `{"title":"Restartable job","objective":"Read README and summarize it.","tool_names":["file_read"],"run_in_background":true}`}},
				StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 1}}, nil
		}
		return app.ModelResponse{Text: "Parent completed.", StopReason: "stop",
			Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 1}}, nil
	}
	tools.afterExecute = func() {
		if !interrupted {
			interrupted = true
			_ = supervisor.cmd.Process.Kill()
		}
	}
	if err := executor.ExecuteTask(context.Background(), value); err == nil || errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("interrupted background job result = %v", err)
	}
	rootFence := supervisor.launch.Fence
	rootTail, childTail := latestAgentCheckpoint(checkpoints.commands, rootFence.AgentSessionID), latestAgentCheckpoint(checkpoints.commands, "child_test")
	if !interrupted || !children.child.Background || children.child.Step.Status != task.StepRunning || rootTail.Fence.AgentSessionID == "" || childTail.Fence.AgentSessionID == "" {
		t.Fatalf("interrupted job was not durably recoverable: interrupted=%t child=%#v root=%#v child_tail=%#v",
			interrupted, children.child, rootTail, childTail)
	}
	childSessionID := children.child.SessionID
	children.child.CheckpointThrough = childTail.Through
	children.child.CheckpointDigest = append([]byte(nil), childTail.Digest...)
	value.AgentInstanceID, value.AgentGeneration, value.AgentSessionID = rootFence.AgentInstanceID, rootFence.AgentGeneration, rootFence.AgentSessionID
	value.AgentEngineVersion, value.AgentProfileVersion = rootFence.EngineVersion, rootFence.ProfileVersion
	value.CheckpointThroughSequence, value.CheckpointDigest = rootTail.Through, append([]byte(nil), rootTail.Digest...)
	value.Lease.Version++
	expires := time.Now().Add(time.Minute)
	value.Lease.ExpiresAt = &expires
	tools.afterExecute = nil
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("recovered background job result = %v", err)
	}
	if children.creates != 2 || children.settles != 1 || children.child.SessionID != childSessionID || !children.child.Background ||
		children.settleCommand.Status != task.StepSucceeded || len(tools.calls) != 1 || len(tools.recoveryCalls) != 1 ||
		tools.recoveryCalls[0].CallID != cloudDSHNamespacedToolCallID(value.ID, childSessionID, "background_read") || len(checkpoints.reads) < 2 {
		t.Fatalf("recovered background job create/settle=%d/%d child=%#v tools=%#v reconcile=%#v reads=%d",
			children.creates, children.settles, children.child, tools.calls, tools.recoveryCalls, len(checkpoints.reads))
	}
}

func latestAgentCheckpoint(commands []app.AgentCheckpointCommand, sessionID string) app.AgentCheckpointCommand {
	var latest app.AgentCheckpointCommand
	for _, command := range commands {
		if command.Fence.AgentSessionID == sessionID && command.Through > latest.Through {
			latest = command
		}
	}
	return latest
}

func TestCloudDSHExecutorRecoversChildSessionAfterRuntimeRestartWithoutReplayingTool(t *testing.T) {
	executor, supervisor, _, checkpoints, models, value := cloudDSHFixture(t)
	tool := cloudDSHReadTool()
	tools := &cloudDSHTestTools{definitions: []app.AgentToolDefinition{tool}, result: app.AgentToolResult{
		Status: app.AgentToolSucceeded, Output: `{"path":"README.md","text":"recovered child result"}`,
	}}
	children := &cloudDSHTestChildSessions{}
	executor.options.Tools, executor.options.Children = tools, children
	models.responses = []app.ModelResponse{
		{ToolCalls: []app.ModelToolCall{{CallID: "delegate_recovery", Name: cloudDSHDelegateToolName,
			Arguments: `{"title":"Inspect README","objective":"Read README and report the key point.","tool_names":["file_read"]}`}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2}},
		{ToolCalls: []app.ModelToolCall{{CallID: "child_read_recovery", Name: tool.Name,
			Arguments: `{"workspace_id":"w1234567","path":"README.md"}`}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 3, OutputTokens: 1}},
		{Text: "Child recovered", StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 5, OutputTokens: 1}},
		{Text: "Parent result", StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 1}},
	}
	tools.afterExecute = func() { _ = supervisor.cmd.Process.Kill() }
	if err := executor.ExecuteTask(context.Background(), value); err == nil || errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("interrupted child run = %v", err)
	}
	if children.creates != 1 || children.settles != 0 || len(tools.calls) != 1 || len(models.calls) != 2 {
		t.Fatalf("interrupted child state creates=%d settles=%d tools=%d models=%d", children.creates, children.settles, len(tools.calls), len(models.calls))
	}
	rootFence := supervisor.launch.Fence
	var rootTail, childTail app.AgentCheckpointCommand
	for _, command := range checkpoints.commands {
		switch command.Fence.AgentSessionID {
		case rootFence.AgentSessionID:
			if command.Through > rootTail.Through {
				rootTail = command
			}
		case children.child.SessionID:
			if command.Through > childTail.Through {
				childTail = command
			}
		}
	}
	if rootTail.Fence.AgentSessionID == "" || childTail.Fence.AgentSessionID == "" {
		t.Fatal("root and child checkpoints were not both persisted before interruption")
	}
	children.child.CheckpointThrough = childTail.Through
	children.child.CheckpointDigest = append([]byte(nil), childTail.Digest...)
	childSessionID := children.child.SessionID
	value.AgentInstanceID, value.AgentGeneration, value.AgentSessionID = rootFence.AgentInstanceID, rootFence.AgentGeneration, rootFence.AgentSessionID
	value.AgentEngineVersion, value.AgentProfileVersion = rootFence.EngineVersion, rootFence.ProfileVersion
	value.CheckpointThroughSequence, value.CheckpointDigest = rootTail.Through, append([]byte(nil), rootTail.Digest...)
	value.Lease.Version++
	expires := time.Now().Add(time.Minute)
	value.Lease.ExpiresAt = &expires
	tools.afterExecute = nil
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("recovered child run = %v", err)
	}
	if children.creates != 2 || children.settles != 1 || children.child.SessionID != childSessionID ||
		children.createCommand.SpawnKey != "delegate_recovery" ||
		len(tools.calls) != 1 || len(tools.recoveryCalls) != 1 ||
		tools.recoveryCalls[0].CallID != cloudDSHNamespacedToolCallID(value.ID, childSessionID, "child_read_recovery") ||
		len(models.calls) != 4 || len(checkpoints.reads) < 2 || children.settleCommand.Status != task.StepSucceeded {
		t.Fatalf("recovered child state creates=%d settles=%d tools=%#v reconciled=%#v models=%d reads=%d settle=%#v",
			children.creates, children.settles, tools.calls, tools.recoveryCalls, len(models.calls), len(checkpoints.reads), children.settleCommand)
	}
}

func TestCloudDSHExecutorCarriesScreenshotAcrossNodeIPCWithoutCheckpointBytes(t *testing.T) {
	executor, _, _, checkpoints, models, value := cloudDSHFixture(t)
	var screenshot bytes.Buffer
	if err := jpeg.Encode(&screenshot, image.NewRGBA(image.Rect(0, 0, 3, 2)), nil); err != nil {
		t.Fatal(err)
	}
	tool := app.AgentToolDefinition{ModelTool: app.ModelTool{Name: "desktop_screenshot", Description: "Capture the approved Host screen.",
		Parameters: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)}}
	tools := &cloudDSHTestTools{definitions: []app.AgentToolDefinition{tool}, result: app.AgentToolResult{
		Status: app.AgentToolSucceeded, Output: `{"mime_type":"image/jpeg","width":3,"height":2}`,
		Images: []app.ModelImage{{MediaType: "image/jpeg", Data: screenshot.Bytes()}},
	}}
	executor.options.Tools = tools
	models.responses = []app.ModelResponse{
		{ToolCalls: []app.ModelToolCall{{CallID: "call_screenshot_1", Name: tool.Name, Arguments: `{}`}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2}},
		{Text: "I can see the editor.", StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 2}},
	}
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("screenshot task = %v", err)
	}
	if len(tools.calls) != 1 || len(models.calls) != 2 {
		t.Fatalf("screenshot calls=%#v models=%d", tools.calls, len(models.calls))
	}
	if models.calls[1].ImageResolver == nil || !validCloudDSHModelRequest(models.calls[1].RawRequest, value.ModelID, []app.AgentToolDefinition{tool}) {
		t.Fatalf("screenshot model request or resolver is invalid: resolver=%v", models.calls[1].ImageResolver != nil)
	}
	var request struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
			Content    []struct {
				Type       string          `json:"type"`
				Attachment json.RawMessage `json:"attachment"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(models.calls[1].RawRequest, &request); err != nil {
		t.Fatal(err)
	}
	var resolved app.ModelImage
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if message.Role == "tool" && message.ToolCallID == "call_screenshot_1" && block.Type == "image" {
				var err error
				resolved, err = models.calls[1].ImageResolver.ResolveTaskModelImage(context.Background(), *models.calls[1].Task, block.Attachment)
				if err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if !bytes.Equal(resolved.Data, screenshot.Bytes()) {
		t.Fatal("Node IPC did not return the exact Host screenshot bytes to the authorized resolver")
	}
	foundReference := false
	for _, checkpoint := range checkpoints.commands {
		if bytes.Contains(checkpoint.Payload, []byte("data_base64")) {
			t.Fatal("raw screenshot bytes were persisted in the dsh checkpoint")
		}
		foundReference = foundReference || bytes.Contains(checkpoint.Payload, []byte("morde_img_"))
	}
	if !foundReference {
		t.Fatal("checkpoint did not retain the screenshot reference")
	}
}

func TestCloudDSHExecutorStopsCleanlyAfterDurableUserWait(t *testing.T) {
	executor, _, store, _, models, value := cloudDSHFixture(t)
	clarification := app.AgentToolDefinition{ModelTool: app.ModelTool{
		Name: "ask_user", Description: "Ask the user for one missing detail.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"question":{"type":"string","minLength":1,"maxLength":4096}},"required":["question"],"additionalProperties":false}`),
	}}
	tools := &cloudDSHTestTools{definitions: []app.AgentToolDefinition{clarification}, result: app.AgentToolResult{Status: app.AgentToolNeedsUser}}
	executor.options.Tools = tools
	models.response = app.ModelResponse{ToolCalls: []app.ModelToolCall{{CallID: "clarify_call_1", Name: clarification.Name, Arguments: `{"question":"Which workspace should I use?"}`}},
		StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2}}

	if err := executor.ExecuteTask(context.Background(), value); err != nil {
		t.Fatalf("ExecuteTask() = %v, want a clean wait after the broker persisted clarification", err)
	}
	if len(tools.calls) != 1 || tools.calls[0].CallID != "clarify_call_1" || store.stopped != 1 {
		t.Fatalf("tool calls=%#v stopped=%d, want one clarification and a confirmed process stop", tools.calls, store.stopped)
	}
}

func TestCloudDSHExecutorCompletesOnlyAfterStopAndPassesCheckpointedFileEvidence(t *testing.T) {
	executor, _, instances, _, models, value := cloudDSHFixture(t)
	tool := cloudDSHWriteTool()
	tools := &cloudDSHTestTools{definitions: []app.AgentToolDefinition{tool}, result: app.AgentToolResult{
		Status: app.AgentToolSucceeded, Output: `{"path":"notes.txt","bytes_written":5}`,
	}}
	completer := &cloudDSHTestCompleter{instances: instances}
	executor.options.Tools, executor.options.Completion = tools, completer
	models.responses = []app.ModelResponse{
		{ToolCalls: []app.ModelToolCall{{CallID: "call_write_1", Name: tool.Name,
			Arguments: `{"workspace_id":"w1234567","path":"notes.txt","content":"saved"}`}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2}},
		{Text: "Saved the file.", StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}},
	}
	if err := executor.ExecuteTask(context.Background(), value); err != nil {
		t.Fatalf("ExecuteTask() error = %v", err)
	}
	completion := completer.completion
	if completer.calls != 1 || len(tools.calls) != 1 || completion.AgentSessionID != instances.active.AgentSessionID ||
		completion.CheckpointThroughSequence < 0 || len(completion.CheckpointDigest) != 32 || completion.Result != "Saved the file." ||
		len(completion.Inputs) != 2 || completion.Inputs[0].CallID != "call_write_1" ||
		completion.Inputs[0].Name != "file_write" || completion.Inputs[1].Output != tools.result.Output {
		t.Fatalf("completion calls=%d tools=%#v candidate=%#v", completer.calls, tools.calls, completion)
	}
}

func TestCloudDSHExecutorCancellationStopsChild(t *testing.T) {
	executor, _, store, _, _, value := cloudDSHFixture(t)
	script := filepath.Join(t.TempDir(), "wait.mjs")
	if err := os.WriteFile(script, []byte(`console.log(JSON.stringify({type:'ready',version:1,engine:'dsh',engine_version:'0.2.0-rc.2'})); setInterval(()=>{},1000)`), 0600); err != nil {
		t.Fatal(err)
	}
	executor.options.ScriptPath = script
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	err := executor.ExecuteTask(ctx, value)
	if !errors.Is(err, context.DeadlineExceeded) || store.stopped != 1 {
		t.Fatalf("cancellation = %v, stopped=%d", err, store.stopped)
	}
}

func TestCloudDSHExecutorCancellationDuringModelCall(t *testing.T) {
	executor, _, store, _, models, value := cloudDSHFixture(t)
	models.block = true
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := executor.ExecuteTask(ctx, value)
	if !errors.Is(err, context.DeadlineExceeded) || len(models.calls) != 1 || store.stopped != 1 {
		t.Fatalf("cancelled model result = %v, calls=%d stopped=%d", err, len(models.calls), store.stopped)
	}
}

func TestCloudDSHExecutorLeaseRenewalFailureStopsChild(t *testing.T) {
	executor, _, store, _, _, value := cloudDSHFixture(t)
	store.leaseLength = 300 * time.Millisecond
	store.renewError = errors.New("lease lost")
	script := filepath.Join(t.TempDir(), "wait.mjs")
	if err := os.WriteFile(script, []byte(`console.log(JSON.stringify({type:'ready',version:1,engine:'dsh',engine_version:'0.2.0-rc.2'})); setInterval(()=>{},1000)`), 0600); err != nil {
		t.Fatal(err)
	}
	executor.options.ScriptPath = script
	err := executor.ExecuteTask(context.Background(), value)
	if err == nil || errors.Is(err, ErrCloudDSHCompletionPending) || store.stopped != 1 {
		t.Fatalf("lost lease result = %v, stopped=%d", err, store.stopped)
	}
}

func TestCloudDSHExecutorRejectsIncompleteRecoveryMetadata(t *testing.T) {
	executor, supervisor, _, _, _, value := cloudDSHFixture(t)
	value.AgentInstanceID = "agent_previous"
	value.AgentGeneration = 1
	value.AgentSessionID = "session_previous"
	value.AgentEngineVersion = cloudDSHEngineVersion
	value.AgentProfileVersion = cloudDSHProfileVersion
	value.CheckpointThroughSequence = 0
	if err := executor.ExecuteTask(context.Background(), value); err == nil || supervisor.cmd != nil {
		t.Fatalf("incomplete recovery = %v", err)
	}
}

func TestCloudDSHExecutorRecoversCompletedCheckpointWithoutReplayingModel(t *testing.T) {
	executor, supervisor, store, checkpoints, models, value := cloudDSHFixture(t)
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("initial run = %v", err)
	}
	if len(checkpoints.commands) == 0 {
		t.Fatal("initial run did not persist checkpoint history")
	}
	previous := supervisor.launch.Fence
	tail := checkpoints.commands[len(checkpoints.commands)-1]
	value.AgentInstanceID = previous.AgentInstanceID
	value.AgentGeneration = previous.AgentGeneration
	value.AgentSessionID = previous.AgentSessionID
	value.AgentEngineVersion = previous.EngineVersion
	value.AgentProfileVersion = previous.ProfileVersion
	value.CheckpointThroughSequence = tail.Through
	value.CheckpointDigest = append([]byte(nil), tail.Digest...)
	value.Lease.Version++
	expires := time.Now().Add(time.Minute)
	value.Lease.ExpiresAt = &expires
	modelCalls := len(models.calls)

	err := executor.ExecuteTask(context.Background(), value)
	if !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("recovery = %v", err)
	}
	if len(checkpoints.reads) == 0 || len(models.calls) != modelCalls || store.stopped != 2 {
		t.Fatalf("checkpoint reads=%d model calls=%d stopped=%d", len(checkpoints.reads), len(models.calls), store.stopped)
	}
	if supervisor.launch.Fence.AgentGeneration != previous.AgentGeneration+1 ||
		supervisor.launch.Fence.AgentSessionID != previous.AgentSessionID || supervisor.launch.Fence.AgentInstanceID == previous.AgentInstanceID {
		t.Fatalf("recovery did not retain the session under a new generation: %#v", supervisor.launch.Fence)
	}
	for i, request := range checkpoints.reads {
		if request.Fence != supervisor.launch.Fence || request.Through != tail.Through || !bytes.Equal(request.ThroughDigest, tail.Digest) ||
			(i == 0 && request.From != 0) || (i > 0 && request.From <= checkpoints.reads[i-1].From) {
			t.Fatalf("checkpoint read %d was not pinned to the active generation/history: %#v", i, request)
		}
	}
}

func TestCloudDSHExecutorReconcilesEveryPinnedToolCallBeforeSettling(t *testing.T) {
	executor, supervisor, instances, checkpoints, models, value := cloudDSHFixture(t)
	tool := cloudDSHWriteTool()
	result := app.AgentToolResult{Status: app.AgentToolSucceeded, Output: checkpointToolOutput}
	tools := &cloudDSHTestTools{definitions: []app.AgentToolDefinition{tool}, result: result}
	executor.options.Tools = tools

	value.AgentInstanceID, value.AgentGeneration, value.AgentSessionID = "agent_previous", 1, "session_previous"
	value.AgentEngineVersion, value.AgentProfileVersion = cloudDSHEngineVersion, cloudDSHProfileVersion
	value.CheckpointThroughSequence = 7
	value.Usage.ToolCalls, value.Budget.ToolCalls = 1, 1
	payload := cloudDSHCheckpointPayload(value.AgentSessionID, 0,
		checkpointTurnStart(0, "turn/start", map[string]any{"turn": 1}),
		checkpointTurnStart(1, "step/start", map[string]any{"turn": 1, "step": 1}),
		checkpointAssistantCall(2, "call_write_1"),
		checkpointToolCall(3, "call_write_1"),
		checkpointToolResult(4, "call_write_1", checkpointToolOutput, 3),
		checkpointAssistantText(5, "Saved the file."),
		checkpointTurnEnd(6, "step/end", map[string]any{"turn": 1, "step": 1}),
		checkpointTurnEnd(7, "turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}),
	)
	digest := sha256.Sum256(payload)
	value.CheckpointDigest = append([]byte(nil), digest[:]...)
	checkpoints.commands = []app.AgentCheckpointCommand{{From: 0, Through: 7, Digest: digest[:], Payload: payload}}
	previousFence := app.AgentInstanceFence{TaskID: value.ID, OwnerID: value.OwnerID, AgentInstanceID: value.AgentInstanceID,
		AgentSessionID: value.AgentSessionID, SupervisorID: "supervisor-test", AgentGeneration: 1,
		AuthorizationVersion: value.Authorization.Version, LeaseOwner: "previous-worker", LeaseVersion: value.Lease.Version - 1,
		EngineVersion: value.AgentEngineVersion, ProfileVersion: value.AgentProfileVersion}
	checkpoints.commands[0].Fence = checkpointFence(previousFence)
	instances.mu.Lock()
	instances.generation, instances.active, instances.wasStopped, instances.stopped = 1, previousFence, true, 1
	instances.mu.Unlock()

	script := filepath.Join(t.TempDir(), "reconcile.mjs")
	source := `import { createInterface } from 'node:readline'
const send = value => process.stdout.write(JSON.stringify(value) + '\n')
send({type:'ready',version:1,engine:'dsh',engine_version:'0.2.0-rc.2'})
let identity
createInterface({input:process.stdin}).on('line', line => {
  const frame = JSON.parse(line)
  if (frame.type === 'start') {
    identity = {task_id:frame.task_id,owner_id:frame.owner_id,agent_id:frame.agent_id,generation:frame.generation,
      authorization_version:frame.authorization_version,session_id:frame.session_id}
    send({type:'checkpoint_read',id:'page_1',...identity,from_seq:0})
  } else if (frame.type === 'checkpoint_page') {
    send({type:'tool_call',id:'tool_1',...identity,through_seq:7,call_id:'call_write_1',name:'file_write',
      arguments:{workspace_id:'w1234567',path:'notes.txt',content:'saved'}})
  } else if (frame.type === 'tool_result') {
    if (frame.status !== 'succeeded' || frame.output !== '{"path":"notes.txt","bytes_written":5}') process.exit(9)
    send({type:'settled',...identity,state:'completed_candidate',through_seq:7})
  }
})`
	if err := os.WriteFile(script, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	executor.options.ScriptPath = script
	value.Lease.Version++
	expires := time.Now().Add(time.Minute)
	value.Lease.ExpiresAt = &expires
	err := executor.ExecuteTask(context.Background(), value)
	if !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("recovery result = %v", err)
	}
	if len(tools.recoveryCalls) != 1 || tools.recoveryCalls[0].CallID != "call_write_1" || tools.recoveryCalls[0].Name != "file_write" ||
		len(tools.recoveryExpected) != 1 || tools.recoveryExpected[0] == nil || *tools.recoveryExpected[0] != checkpointToolOutput ||
		len(tools.calls) != 0 || len(models.calls) != 0 || tools.pauseRecoveryCall != 0 || supervisor.cmd == nil || instances.stopped != 2 {
		t.Fatalf("recovery calls=%#v expected=%#v normal=%#v models=%d pauses=%d stopped=%d",
			tools.recoveryCalls, tools.recoveryExpected, tools.calls, len(models.calls), tools.pauseRecoveryCall, instances.stopped)
	}
}

func TestCloudDSHExecutorRejectsAnotherTaskIdentity(t *testing.T) {
	executor, _, store, checkpoints, models, value := cloudDSHFixture(t)
	script := filepath.Join(t.TempDir(), "foreign.mjs")
	source := `console.log(JSON.stringify({type:'ready',version:1,engine:'dsh',engine_version:'0.2.0-rc.2'}));
process.stdin.once('data', data => { const f = JSON.parse(data.toString());
console.log(JSON.stringify({type:'checkpoint',id:'x',task_id:'q7654321',owner_id:f.owner_id,agent_id:f.agent_id,
generation:f.generation,authorization_version:f.authorization_version,session_id:f.session_id,
from_seq:0,through_seq:0,payload:'{}',sha256:'0'.repeat(64)})); }); setInterval(()=>{},1000);`
	if err := os.WriteFile(script, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	executor.options.ScriptPath = script
	err := executor.ExecuteTask(context.Background(), value)
	if err == nil || len(checkpoints.commands) != 0 || len(models.calls) != 0 || store.stopped != 1 {
		t.Fatalf("foreign identity error=%v checkpoints=%d models=%d stopped=%d", err, len(checkpoints.commands), len(models.calls), store.stopped)
	}
}

func TestCloudDSHExecutorRejectsUntrustedFramesAndMissingSupervisor(t *testing.T) {
	if _, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{}); err == nil {
		t.Fatal("missing supervisor accepted")
	}
	for _, raw := range []string{
		`{"type":"ready","version":1,"version":1,"engine":"dsh","engine_version":"0.2.0-rc.2"}`,
		`{"type":"ready","version":1,"engine":"dsh","engine_version":"0.2.0-rc.2","extra":true}`,
		`{"type":"model","request":{"tools":[],"tools":[]}}`,
	} {
		if _, err := decodeCloudDSHFrame([]byte(raw)); err == nil {
			t.Fatal("invalid frame accepted")
		}
	}
	if _, err := decodeCloudDSHFrame([]byte(`{"type":"checkpoint_read","id":"page-1","task_id":"q1234567","owner_id":"u1234567","agent_id":"agent_1","generation":2,"authorization_version":1,"session_id":"session_1","from_seq":0}`)); err != nil {
		t.Fatalf("valid checkpoint read frame rejected: %v", err)
	}
	if validCloudDSHModelRequest(json.RawMessage(`{"provider":"morde","model":"p1234567","tools":[{}],"messages":[{"role":"user","content":[{"type":"text"}]}]}`), "p1234567", nil) {
		t.Fatal("unapproved tools accepted")
	}
	frames := make(chan cloudDSHRead, 1)
	go readCloudDSHFrames(context.Background(), strings.NewReader(strings.Repeat("x", cloudDSHMaxFrame+1)), frames)
	if item := <-frames; item.err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestCloudDSHDelegateToolsAreRestrictedToParentDefinitions(t *testing.T) {
	parentTools := []app.AgentToolDefinition{cloudDSHReadTool(), cloudDSHWriteTool()}
	arguments, definitions, err := validateCloudDSHDelegateArguments([]byte(`{"title":"Inspect","objective":"Read the project","tool_names":["file_read"]}`), parentTools)
	if err != nil || arguments.Title != "Inspect" || arguments.Objective != "Read the project" || len(definitions) != 1 || definitions[0].Name != "file_read" {
		t.Fatalf("valid child tool subset = %#v tools=%#v err=%v", arguments, definitions, err)
	}
	for _, raw := range []string{
		`{"title":"Inspect","objective":"Read","tool_names":["file_read","file_read"]}`,
		`{"title":"Inspect","objective":"Read","tool_names":["shell"]}`,
		`{"title":"Inspect","objective":"Read","tool_names":["ask_user"]}`,
		`{"title":"Inspect","objective":"Read","tool_names":["dsh_delegate"]}`,
		`{"title":"Inspect","objective":"Read","tool_names":[],"extra":true}`,
	} {
		if _, _, err := validateCloudDSHDelegateArguments([]byte(raw), parentTools); err == nil {
			t.Fatalf("unauthorized child tool selection accepted: %s", raw)
		}
	}
	delegateTool := cloudDSHDelegateTool(parentTools)
	if !validCloudDSHToolDefinitions([]app.AgentToolDefinition{delegateTool}) {
		t.Fatal("delegate tool schema is invalid")
	}
	var schema struct {
		Properties map[string]struct {
			Items struct {
				Enum []string `json:"enum"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(delegateTool.Parameters, &schema); err != nil ||
		!sameWorkerToolNames(schema.Properties["tool_names"].Items.Enum, []string{"file_read", "file_write"}) {
		t.Fatalf("delegate tool schema does not constrain selections: %s; %v", delegateTool.Parameters, err)
	}
	if cloudDSHNamespacedToolCallID("q1234567", "child_1", "call_1") ==
		cloudDSHNamespacedToolCallID("q1234567", "child_2", "call_1") {
		t.Fatal("child tool ledger call ids are not session scoped")
	}
}

func TestCloudDSHModelRequestAllowsEquivalentToolsInDifferentOrder(t *testing.T) {
	definitions := []app.AgentToolDefinition{cloudDSHReadTool(), cloudDSHWriteTool()}
	tools := cloudDSHToolPayload(definitions)
	tools[0], tools[1] = tools[1], tools[0]
	request := map[string]any{
		"provider": "morde", "model": "p1234567", "tools": tools,
		"messages": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": "Write a file."}}}},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if !validCloudDSHModelRequest(raw, "p1234567", definitions) {
		t.Fatal("the same authorized tools were rejected because their order changed")
	}

	var altered struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(raw, &altered); err != nil {
		t.Fatal(err)
	}
	altered.Tools[0]["description"] = "a different tool"
	changed, err := json.Marshal(map[string]any{
		"provider": "morde", "model": "p1234567", "tools": altered.Tools,
		"messages": request["messages"],
	})
	if err != nil {
		t.Fatal(err)
	}
	if validCloudDSHModelRequest(changed, "p1234567", definitions) {
		t.Fatal("a tool with a changed description was accepted")
	}
}

type cloudDSHBufferCloser struct {
	bytes.Buffer
}

func (b *cloudDSHBufferCloser) Close() error { return nil }

func TestWriteCloudDSHFrameCheckpointPayloadRoundTrip(t *testing.T) {
	payload := []byte(`{"header":{"id":"session-1"},"from_seq":0,"events":[{"seq":0,"text":"` + strings.Repeat("<>&", 340000) + `"}]}`)
	if !json.Valid(payload) || len(payload) < 900<<10 || len(payload) > 1<<20 {
		t.Fatal("checkpoint fixture must be valid JSON and close to 1 MiB")
	}
	digest := sha256.Sum256(payload)
	frame := map[string]any{
		"type":    "checkpoint_page",
		"id":      "page-1",
		"payload": string(payload),
		"sha256":  hex.EncodeToString(digest[:]),
	}
	oldEncoding, err := json.Marshal(frame)
	if err != nil || len(oldEncoding)+1 <= cloudDSHMaxFrame {
		t.Fatal("fixture must exceed the frame limit with default HTML escaping")
	}
	var output cloudDSHBufferCloser
	if err := writeCloudDSHFrame(context.Background(), &output, frame); err != nil {
		t.Fatal(err)
	}
	if output.Len() > cloudDSHMaxFrame || output.Bytes()[output.Len()-1] != '\n' {
		t.Fatal("encoded frame must be bounded and newline terminated")
	}
	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if bytes.Contains(output.Bytes(), []byte(escaped)) {
			t.Fatalf("frame contains unexpected HTML escape %s", escaped)
		}
	}
	var decoded struct {
		Payload string `json:"payload"`
		SHA256  string `json:"sha256"`
	}
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(decoded.Payload), payload) || decoded.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("checkpoint payload or digest changed during transfer")
	}
	decodedDigest := sha256.Sum256([]byte(decoded.Payload))
	if decodedDigest != digest {
		t.Fatal("round-trip checkpoint digest does not match")
	}
}

func TestWriteCloudDSHFrameEncodedSizeLimit(t *testing.T) {
	var output cloudDSHBufferCloser
	if err := writeCloudDSHFrame(context.Background(), &output, strings.Repeat("x", cloudDSHMaxFrame-3)); err != nil {
		t.Fatal(err)
	}
	if output.Len() != cloudDSHMaxFrame {
		t.Fatal("frame at encoded size limit was not fully written")
	}
	output.Reset()
	if err := writeCloudDSHFrame(context.Background(), &output, strings.Repeat("x", cloudDSHMaxFrame-2)); err == nil {
		t.Fatal("frame above encoded size limit was accepted")
	}
	if output.Len() != 0 {
		t.Fatal("oversized frame must not be written")
	}
}
