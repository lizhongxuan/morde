package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"morde/server/internal/app"
)

func TestAccountAgentRuntimePreservesModelRequestBytes(t *testing.T) {
	ctx := context.Background()
	runtime := app.AccountAgentRuntime{Fence: app.AccountAgentRuntimeFence{
		ServiceInstanceID: "123e4567-e89b-12d3-a456-426614174000", OwnerID: "u1234567",
		RuntimeID: "runtime_1", RuntimeGeneration: 1,
	}}
	request := []byte(`{"provider":"morde","model":"p1234567","tools":[],"messages":[{"role":"user","content":[{"type":"text","text":"` +
		strings.Repeat("长文章", 4096) + " <tag> & 中文 " + string(rune(0x2028)) + string(rune(0x2029)) + `"}]}]}`)
	if !json.Valid(request) {
		t.Fatal("invalid test request")
	}
	digest := sha256.Sum256(request)
	frame := func(t *testing.T, fields map[string]any, body []byte, hash string) []byte {
		t.Helper()
		fields["type"] = "model"
		fields["id"] = "model_1"
		fields["through_seq"] = 0
		fields["sha256"] = hash
		fields["server_instance_id"] = runtime.Fence.ServiceInstanceID
		fields["owner_id"] = runtime.Fence.OwnerID
		fields["runtime_id"] = runtime.Fence.RuntimeID
		fields["runtime_generation"] = runtime.Fence.RuntimeGeneration
		fields["runtime_sequence"] = 1
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		raw = append(raw[:len(raw)-1], []byte(`,"request":`)...)
		raw = append(raw, body...)
		return append(raw, '}')
	}
	check := func(t *testing.T, actual []byte, actualDigest string) {
		t.Helper()
		if !bytes.Equal(actual, request) || !cloudDSHDigest(actual, actualDigest) {
			t.Fatal("model request bytes or their digest changed during account Runtime routing")
		}
	}
	hash := hex.EncodeToString(digest[:])

	t.Run("task", func(t *testing.T) {
		fence := app.AgentInstanceFence{TaskID: "q1234567", OwnerID: runtime.Fence.OwnerID,
			AgentInstanceID: "agent_1", AgentSessionID: "session_1", AgentGeneration: 1, AuthorizationVersion: 1}
		task := &cloudDSHAccountTask{ctx: ctx, fence: fence, sequence: 1, events: make(chan cloudDSHRead, 1), failed: make(chan struct{})}
		session := &cloudDSHAccountRuntimeSession{runtime: runtime, tasks: map[app.AgentInstanceFence]*cloudDSHAccountTask{fence: task}}
		fields := map[string]any{"task_id": fence.TaskID, "agent_id": fence.AgentInstanceID,
			"session_id": fence.AgentSessionID, "generation": fence.AgentGeneration, "authorization_version": fence.AuthorizationVersion}
		if err := session.routeFrame(frame(t, fields, request, hash)); err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeCloudDSHFrame((<-task.events).frame)
		if err != nil {
			t.Fatal(err)
		}
		check(t, decoded.Request, decoded.SHA256)
	})

	t.Run("task tool arguments", func(t *testing.T) {
		fence := app.AgentInstanceFence{TaskID: "q1234567", OwnerID: runtime.Fence.OwnerID,
			AgentInstanceID: "agent_1", AgentSessionID: "session_1", AgentGeneration: 1, AuthorizationVersion: 1}
		task := &cloudDSHAccountTask{ctx: ctx, fence: fence, sequence: 1, events: make(chan cloudDSHRead, 1), failed: make(chan struct{})}
		session := &cloudDSHAccountRuntimeSession{runtime: runtime, tasks: map[app.AgentInstanceFence]*cloudDSHAccountTask{fence: task}}
		fields := map[string]any{"type": "tool_call", "id": "tool_1", "call_id": "call_1", "name": "file_read",
			"through_seq": 0, "task_id": fence.TaskID, "agent_id": fence.AgentInstanceID, "session_id": fence.AgentSessionID,
			"generation": fence.AgentGeneration, "authorization_version": fence.AuthorizationVersion,
			"server_instance_id": runtime.Fence.ServiceInstanceID, "owner_id": runtime.Fence.OwnerID,
			"runtime_id": runtime.Fence.RuntimeID, "runtime_generation": runtime.Fence.RuntimeGeneration, "runtime_sequence": 1}
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		arguments := []byte(`{"path":"<tag> & ` + string(rune(0x2028)) + `"}`)
		raw = append(raw[:len(raw)-1], []byte(`,"arguments":`)...)
		raw = append(append(raw, arguments...), '}')
		if err := session.routeFrame(raw); err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeCloudDSHFrame((<-task.events).frame)
		if err != nil {
			t.Fatal(err)
		}
		activity, tracked, err := cloudDSHAgentRequestForFrame(decoded, nil)
		if err != nil || !tracked || !bytes.Equal(decoded.Arguments, arguments) ||
			activity.digest != cloudDSHAgentRequestDigest("tool_call", "file_read", string(arguments)) {
			t.Fatalf("tool arguments or their activity digest changed: tracked=%t err=%v", tracked, err)
		}
	})

	t.Run("coordinator", func(t *testing.T) {
		identity := app.ConversationCoordinatorIdentity{ServiceInstanceID: runtime.Fence.ServiceInstanceID,
			OwnerID: runtime.Fence.OwnerID, ActorID: runtime.Fence.OwnerID, ConversationID: "c1234567", MessageID: "m1234567",
			AgentID: "coord_1", Generation: 1, AuthorizationVersion: 1, SessionID: "coord_session_1", Purpose: "conversation"}
		coordinator := &cloudDSHAccountCoordinator{ctx: ctx, identity: identity, sequence: 1, events: make(chan cloudDSHRead, 1)}
		session := &cloudDSHAccountRuntimeSession{runtime: runtime, coordinators: map[string]*cloudDSHAccountCoordinator{identity.SessionID: coordinator}}
		fields := map[string]any{"context_role": "coordinator", "conversation_id": identity.ConversationID, "message_id": identity.MessageID,
			"actor_id": identity.ActorID, "agent_id": identity.AgentID, "generation": identity.Generation,
			"authorization_version": identity.AuthorizationVersion, "session_id": identity.SessionID,
			"purpose": identity.Purpose, "operation_id": identity.OperationID}
		if err := session.routeFrame(frame(t, fields, request, hash)); err != nil {
			t.Fatal(err)
		}
		decoded, err := coordinator.read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		check(t, decoded.Request, decoded.SHA256)
	})

	t.Run("task validation review", func(t *testing.T) {
		fence := taskValidationReviewProtocolFence()
		identity := cloudDSHTaskValidationReviewIdentityForFence(fence)
		review := &cloudDSHTaskValidationReview{ctx: ctx, fence: fence, identity: identity, sequence: 1,
			started: true, events: make(chan cloudDSHRead, 1)}
		session := &cloudDSHAccountRuntimeSession{runtime: runtime, reviews: map[string]*cloudDSHTaskValidationReview{identity.SessionID: review}}
		fields := taskValidationReviewProtocolIdentity(identity)
		if err := session.routeFrame(frame(t, fields, request, hash)); err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeCloudDSHTaskValidationReviewFrame((<-review.events).frame)
		if err != nil {
			t.Fatal(err)
		}
		check(t, decoded.Request, decoded.SHA256)
		changed := bytes.Replace(request, []byte("<tag>"), []byte("<bad>"), 1)
		if err := session.routeFrame(frame(t, fields, changed, hash)); err == nil {
			t.Fatal("review accepted a request whose bytes do not match the digest")
		}
	})
}

func TestCloudDSHExecutorAcceptsLongRequestWithSpecialCharacters(t *testing.T) {
	executor, _, _, _, models, value := cloudDSHFixture(t)
	value.Objective = "总结文章：" + strings.Repeat("长内容 <tag> & ", 256) + "\u2028\u2029"
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatalf("long request did not reach a candidate result: %v", err)
	}
	if len(models.calls) != 1 {
		t.Fatalf("model calls = %d, want 1", len(models.calls))
	}
	var request struct {
		Messages []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(models.calls[0].RawRequest, &request); err != nil {
		t.Fatal(err)
	}
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if strings.Contains(block.Text, value.Objective) {
				return
			}
		}
	}
	t.Fatal("model request lost the long objective")
}
