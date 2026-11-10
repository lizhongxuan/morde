package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

type coordinatorTestModels struct {
	completeCalls int
	recoverCalls  int
	response      app.ModelResponse
	request       json.RawMessage
}

func (m *coordinatorTestModels) ResolveConversationRouteModel(context.Context, app.ConversationRouteClaim) (string, error) {
	return "p1234567", nil
}

func (m *coordinatorTestModels) RecoverConversationCoordinatorModelOutput(context.Context,
	app.ConversationRouteClaim, app.ConversationRouteContext) (string, error) {
	m.recoverCalls++
	return "", errors.New("unexpected coordinator model recovery")
}

func (m *coordinatorTestModels) CompleteConversationCoordinatorModel(_ context.Context, _ app.ConversationRouteClaim,
	_ app.ConversationRouteContext, _ string, _ int64, request json.RawMessage) (app.ModelResponse, error) {
	m.completeCalls++
	m.request = append(json.RawMessage(nil), request...)
	return m.response, nil
}

type coordinatorTestCheckpoints struct {
	head      app.ConversationCoordinatorCheckpointHead
	context   []byte
	saves     []app.ConversationCoordinatorCheckpointCommand
	readCalls int
}

func (c *coordinatorTestCheckpoints) Head(_ context.Context, _ app.ConversationRouteClaim,
	_ app.ConversationCoordinatorIdentity, contextDigest []byte) (app.ConversationCoordinatorCheckpointHead, error) {
	c.context = append([]byte(nil), contextDigest...)
	return c.head, nil
}

func (c *coordinatorTestCheckpoints) Save(_ context.Context, command app.ConversationCoordinatorCheckpointCommand) (bool, error) {
	c.saves = append(c.saves, command)
	return false, nil
}

func (c *coordinatorTestCheckpoints) ReadPage(context.Context, app.ConversationCoordinatorCheckpointPageRequest) (app.ConversationCoordinatorCheckpointPage, error) {
	c.readCalls++
	return app.ConversationCoordinatorCheckpointPage{}, errors.New("unexpected checkpoint page read")
}

type coordinatorTestStream struct {
	frames []cloudDSHCoordinatorFrame
	index  int
}

func (s *coordinatorTestStream) read(context.Context) (cloudDSHCoordinatorFrame, error) {
	if s.index >= len(s.frames) {
		return cloudDSHCoordinatorFrame{}, errors.New("coordinator test stream exhausted")
	}
	frame := s.frames[s.index]
	s.index++
	return frame, nil
}

type coordinatorTestRuntimeManager struct {
	runtime      *coordinatorTestRuntime
	acquireCalls int
}

func (m *coordinatorTestRuntimeManager) Acquire(context.Context, string, string) (cloudDSHCoordinatorRuntime, error) {
	m.acquireCalls++
	return m.runtime, nil
}

type coordinatorTestRuntime struct {
	stream       cloudDSHCoordinatorStream
	startCalls   int
	writeCalls   int
	stopCalls    int
	startRequest map[string]any
}

func (r *coordinatorTestRuntime) StartCoordinator(_ context.Context, _ app.ConversationRouteClaim,
	_ app.ConversationCoordinatorIdentity, start map[string]any) (cloudDSHCoordinatorStream, error) {
	r.startCalls++
	r.startRequest = start
	return r.stream, nil
}

func (r *coordinatorTestRuntime) WriteCoordinatorFrame(context.Context, app.ConversationCoordinatorIdentity, map[string]any) error {
	r.writeCalls++
	return nil
}

func (r *coordinatorTestRuntime) StopCoordinator(context.Context, app.ConversationCoordinatorIdentity) error {
	r.stopCalls++
	return nil
}

func coordinatorTestClaimAndContext() (app.ConversationRouteClaim, app.ConversationRouteContext) {
	claim := app.ConversationRouteClaim{Message: conversation.Message{ID: "m1234567", ConversationID: "c1234567",
		OwnerID: "u1234567", ActorID: "u1234567", Role: conversation.RoleUser, Sequence: 1, Content: "hello"},
		ServiceInstanceID: "00000000-0000-4000-8000-000000000001", LeaseOwner: "route-worker", LeaseVersion: 1,
		Authorization: task.Authorization{Version: 3}}
	value := app.ConversationRouteContext{ActorID: claim.Message.ActorID, ModelID: "p1234567", Content: claim.Message.Content}
	return claim, value
}

func TestCloudDSHCoordinatorRejectsChangedCheckpointContextBeforeRuntimeOrModel(t *testing.T) {
	claim, value := coordinatorTestClaimAndContext()
	wrong := sha256.Sum256([]byte("different authorized context"))
	models := &coordinatorTestModels{}
	checkpoints := &coordinatorTestCheckpoints{head: app.ConversationCoordinatorCheckpointHead{Through: 0,
		Digest: make([]byte, sha256.Size), ContextDigest: wrong[:]}}
	runtimes := &coordinatorTestRuntimeManager{}
	coordinator := &CloudDSHConversationCoordinator{options: CloudDSHConversationCoordinatorOptions{
		Models: models, Checkpoints: checkpoints,
	}, runtimes: runtimes}

	if _, err := coordinator.ClassifyConversationRoute(context.Background(), claim, value); err == nil {
		t.Fatal("coordinator accepted a checkpoint from a different route context")
	}
	if runtimes.acquireCalls != 0 || models.completeCalls != 0 || models.recoverCalls != 0 {
		t.Fatalf("mismatched context crossed runtime/model boundary: acquire=%d complete=%d recover=%d",
			runtimes.acquireCalls, models.completeCalls, models.recoverCalls)
	}
}

func TestCloudDSHCoordinatorRunsMatchingTextOnlyTurn(t *testing.T) {
	claim, value := coordinatorTestClaimAndContext()
	output := `{"action":"reply","task_id":"","question":"","reply":"Hello"}`
	checkpointPayload := []byte(`{"events":[{"seq":0,"type":"message"}]}`)
	checkpointDigest := sha256.Sum256(checkpointPayload)
	modelRequest := json.RawMessage(`{"provider":"morde","model":"p1234567","messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}],"tools":[]}`)
	modelDigest := sha256.Sum256(modelRequest)
	identity := cloudDSHCoordinatorIdentity(claim)
	from, through := int64(0), int64(0)
	callID, err := app.DSHConversationCoordinatorModelCallID(identity.ConversationID, identity.MessageID, identity.SessionID, through)
	if err != nil {
		t.Fatal(err)
	}
	base := func(kind string) cloudDSHCoordinatorFrame {
		return cloudDSHCoordinatorFrame{Type: kind, Role: "coordinator", ConversationID: identity.ConversationID,
			MessageID: identity.MessageID, OwnerID: identity.OwnerID, ActorID: identity.ActorID, AgentID: identity.AgentID,
			Generation: identity.Generation, AuthorizationVersion: identity.AuthorizationVersion, SessionID: identity.SessionID}
	}
	checkpoint := base("checkpoint")
	checkpoint.ID, checkpoint.From, checkpoint.Through = "checkpoint-0", &from, &through
	checkpoint.Payload, checkpoint.SHA256 = string(checkpointPayload), hex.EncodeToString(checkpointDigest[:])
	model := base("model")
	model.ID, model.Through, model.Request, model.SHA256 = callID, &through, modelRequest, hex.EncodeToString(modelDigest[:])
	settled := base("coordinator_settled")
	settled.State, settled.Through, settled.Result = "completed", &through, output

	stream := &coordinatorTestStream{frames: []cloudDSHCoordinatorFrame{checkpoint, model, settled}}
	runtime := &coordinatorTestRuntime{stream: stream}
	runtimes := &coordinatorTestRuntimeManager{runtime: runtime}
	models := &coordinatorTestModels{response: app.ModelResponse{Text: output, StopReason: "stop",
		Usage: app.ModelUsage{Known: true, InputTokens: 10, OutputTokens: 5}}}
	checkpoints := &coordinatorTestCheckpoints{head: app.ConversationCoordinatorCheckpointHead{Through: -1}}
	coordinator := &CloudDSHConversationCoordinator{options: CloudDSHConversationCoordinatorOptions{
		Models: models, Checkpoints: checkpoints,
	}, runtimes: runtimes}

	decision, err := coordinator.ClassifyConversationRoute(context.Background(), claim, value)
	if err != nil {
		t.Fatal(err)
	}
	contextDigest, err := app.ConversationCoordinatorContextDigest(value)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Action != app.ConversationRouteReply || decision.Reply != "Hello" || decision.ModelOutput != output {
		t.Fatalf("coordinator decision = %+v", decision)
	}
	if runtimes.acquireCalls != 1 || runtime.startCalls != 1 || runtime.stopCalls != 1 || runtime.writeCalls != 7 {
		t.Fatalf("runtime lifecycle: acquire=%d start=%d write=%d stop=%d", runtimes.acquireCalls,
			runtime.startCalls, runtime.writeCalls, runtime.stopCalls)
	}
	if runtime.startRequest["context_role"] != "coordinator" || runtime.startRequest["model"] != value.ModelID ||
		runtime.startRequest["prompt"] == "" || len(runtime.startRequest["tools"].([]any)) != 0 {
		t.Fatalf("coordinator start did not preserve the text-only route context: %+v", runtime.startRequest)
	}
	if models.completeCalls != 1 || models.recoverCalls != 0 || checkpoints.readCalls != 0 || len(checkpoints.saves) != 1 {
		t.Fatalf("turn calls: model=%d recovery=%d page_reads=%d checkpoint_saves=%d", models.completeCalls,
			models.recoverCalls, checkpoints.readCalls, len(checkpoints.saves))
	}
	if string(checkpoints.context) != string(contextDigest) || string(checkpoints.saves[0].ContextDigest) != string(contextDigest) {
		t.Fatal("coordinator did not use one context digest for head and saved checkpoint")
	}
	var request struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(models.request, &request); err != nil || request.Provider != "morde" || request.Model != value.ModelID ||
		len(request.Tools) != 0 || len(request.Messages) != 1 || len(request.Messages[0].Content) != 1 ||
		request.Messages[0].Content[0].Type != "text" {
		t.Fatalf("model gateway request was not text-only and tool-free: %s err=%v", models.request, err)
	}
}
