package worker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	leaseLength time.Duration
	renewError  error
}

func (s *cloudDSHTestInstanceStore) StartAgentInstance(_ context.Context, request app.AgentInstanceStart) (app.AgentInstanceLease, bool, error) {
	fence := request.Fence
	fence.AgentGeneration = 1
	length := s.leaseLength
	if length == 0 {
		length = 15 * time.Second
	}
	return app.AgentInstanceLease{Fence: fence, ExpiresAt: time.Now().Add(length)}, false, nil
}

func (s *cloudDSHTestInstanceStore) RenewAgentInstance(_ context.Context, fence app.AgentInstanceFence, _ time.Duration) (app.AgentInstanceLease, error) {
	if s.renewError != nil {
		return app.AgentInstanceLease{}, s.renewError
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
	s.stopped++
	return false, nil
}

type cloudDSHTestCheckpoints struct {
	commands []app.AgentCheckpointCommand
	err      error
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
	return false, nil
}

type cloudDSHTestModels struct {
	calls    []app.ModelCall
	response app.ModelResponse
	block    bool
}

func (m *cloudDSHTestModels) Complete(ctx context.Context, call app.ModelCall) (app.ModelResponse, error) {
	m.calls = append(m.calls, call)
	if m.block {
		<-ctx.Done()
		return app.ModelResponse{}, ctx.Err()
	}
	return m.response, nil
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

func TestCloudDSHExecutorRunsRealDSHButDoesNotCompleteTask(t *testing.T) {
	t.Setenv("MORDE_TEST_PRIVATE_ENV", "private")
	executor, supervisor, store, checkpoints, models, value := cloudDSHFixture(t)
	err := executor.ExecuteTask(context.Background(), value)
	if !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("candidate result = %v", err)
	}
	if len(models.calls) != 1 || len(checkpoints.commands) == 0 || store.stopped != 1 {
		t.Fatalf("models=%d checkpoints=%d stopped=%d", len(models.calls), len(checkpoints.commands), store.stopped)
	}
	call := models.calls[0]
	if call.Source != app.ModelCallTask || call.ModelID != value.ModelID || call.Task == nil || call.Task.TaskID != value.ID ||
		call.Task.ThroughSequence < 0 || !validCloudDSHModelRequest(call.RawRequest, value.ModelID) {
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

func TestCloudDSHExecutorDoesNotResumeExistingCheckpoint(t *testing.T) {
	executor, supervisor, _, _, _, value := cloudDSHFixture(t)
	value.CheckpointThroughSequence = 0
	if err := executor.ExecuteTask(context.Background(), value); err == nil || supervisor.cmd != nil {
		t.Fatalf("unsafe recovery = %v", err)
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
	if validCloudDSHModelRequest(json.RawMessage(`{"provider":"morde","model":"p1234567","tools":[{}],"messages":[{"role":"user","content":[{"type":"text"}]}]}`), "p1234567") {
		t.Fatal("unapproved tools accepted")
	}
	frames := make(chan cloudDSHRead, 1)
	go readCloudDSHFrames(context.Background(), strings.NewReader(strings.Repeat("x", cloudDSHMaxFrame+1)), frames)
	if item := <-frames; item.err == nil {
		t.Fatal("oversized frame accepted")
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
