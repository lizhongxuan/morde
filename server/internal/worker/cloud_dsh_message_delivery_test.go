package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"morde/server/internal/app"
)

type cloudDSHFrameBuffer struct{ bytes.Buffer }

func (cloudDSHFrameBuffer) Close() error { return nil }

func TestCloudDSHMessageDeliveryUsesV3ModeAndDeduplicatesByMode(t *testing.T) {
	checkpoints := &cloudDSHTestCheckpoints{pending: []app.AgentConversationMessage{{
		ID: "m1234567", Content: "add a brief note", Mode: app.AgentMessageDeliverySteer,
	}}}
	executor := &CloudDSHExecutor{options: CloudDSHExecutorOptions{Checkpoints: checkpoints}}
	fence := app.AgentInstanceFence{TaskID: "task_123", OwnerID: "user_123", InitiatorID: "user_123",
		AgentInstanceID: "agent_123", AgentGeneration: 2, AgentSessionID: "session_123", AuthorizationVersion: 3,
		LeaseOwner: "worker-123", LeaseVersion: 4, EngineVersion: "0.2.2-rc.1", ProfileVersion: "v1"}
	writer := &cloudDSHFrameBuffer{}
	seen := make(map[string]string)
	if err := executor.deliverPendingMessagesOnce(context.Background(), fence, writer, seen); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(writer.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("delivery frame count = %d, want 1", len(lines))
	}
	var frame struct {
		Type                 string   `json:"type"`
		Version              int      `json:"version"`
		TaskID               string   `json:"task_id"`
		AgentID              string   `json:"agent_id"`
		Generation           int64    `json:"generation"`
		AuthorizationVersion int64    `json:"authorization_version"`
		SessionID            string   `json:"session_id"`
		MessageID            string   `json:"message_id"`
		Mode                 string   `json:"mode"`
		AttachmentIDs        []string `json:"attachment_ids"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "message_delivery" || frame.Version != 3 || frame.TaskID != fence.TaskID || frame.AgentID != fence.AgentInstanceID ||
		frame.Generation != fence.AgentGeneration || frame.AuthorizationVersion != fence.AuthorizationVersion || frame.SessionID != fence.AgentSessionID ||
		frame.MessageID != "m1234567" || frame.Mode != string(app.AgentMessageDeliverySteer) || frame.AttachmentIDs == nil {
		t.Fatalf("delivery frame lost its fence or mode: %+v", frame)
	}
	if err := executor.deliverPendingMessagesOnce(context.Background(), fence, writer, seen); err != nil || strings.Count(writer.String(), "message_delivery") != 1 {
		t.Fatalf("identical delivery replay was not deduplicated: err=%v frames=%q", err, writer.String())
	}
	checkpoints.pending[0].Mode = app.AgentMessageDeliveryFollowup
	if err := executor.deliverPendingMessagesOnce(context.Background(), fence, writer, seen); err == nil || strings.Count(writer.String(), "message_delivery") != 1 {
		t.Fatalf("same message ID with a changed mode was accepted: err=%v frames=%q", err, writer.String())
	}
	var _ io.WriteCloser = writer
}

func TestCloudDSHSupersedingDeliveryUpgradesTheSameSnapshotWithoutRequery(t *testing.T) {
	message := app.AgentConversationMessage{ID: "m1234567", Content: "apply the new heading", Mode: app.AgentMessageDeliverySteer}
	checkpoints := &cloudDSHTestCheckpoints{pending: []app.AgentConversationMessage{message}}
	executor := &CloudDSHExecutor{options: CloudDSHExecutorOptions{Checkpoints: checkpoints}}
	fence := app.AgentInstanceFence{TaskID: "task_123", OwnerID: "user_123", InitiatorID: "user_123",
		AgentInstanceID: "agent_123", AgentGeneration: 2, AgentSessionID: "session_123", AuthorizationVersion: 3,
		LeaseOwner: "worker-123", LeaseVersion: 4, EngineVersion: "0.2.2-rc.1", ProfileVersion: "v1"}
	writer := &cloudDSHFrameBuffer{}
	seen := make(map[string]string)
	if err := executor.deliverPendingMessagesOnce(context.Background(), fence, writer, seen); err != nil {
		t.Fatal(err)
	}
	checkpointReads := len(checkpoints.pendingFences)
	if err := executor.deliverSupersedingMessages(context.Background(), fence, writer,
		[]app.AgentConversationMessage{message}, seen, fence.AgentSessionID, "call_supersede_1"); err != nil {
		t.Fatal(err)
	}
	if len(checkpoints.pendingFences) != checkpointReads {
		t.Fatalf("superseding delivery re-read pending messages: reads=%d before=%d", len(checkpoints.pendingFences), checkpointReads)
	}
	lines := strings.Split(strings.TrimSpace(writer.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("delivery frame count = %d, want v3 then v4: %q", len(lines), writer.String())
	}
	var first, second struct {
		Version          int    `json:"version"`
		MessageID        string `json:"message_id"`
		Content          string `json:"content"`
		Mode             string `json:"mode"`
		SupersedeActive  bool   `json:"supersede_active"`
		SupersedeSession string `json:"supersede_session_id"`
		SupersedeCall    string `json:"supersede_call_id"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatal(err)
	}
	if first.Version != 3 || first.MessageID != message.ID || first.Content != message.Content || first.Mode != string(message.Mode) ||
		second.Version != 4 || second.MessageID != first.MessageID || second.Content != first.Content || second.Mode != first.Mode ||
		!second.SupersedeActive || second.SupersedeSession != fence.AgentSessionID || second.SupersedeCall != "call_supersede_1" {
		t.Fatalf("message identity changed during superseding upgrade: first=%+v second=%+v", first, second)
	}
}

func TestCloudDSHChildAbortFrameTargetsOnlyTheValidatedChildCall(t *testing.T) {
	executor := &CloudDSHExecutor{}
	fence := app.AgentInstanceFence{TaskID: "task_123", OwnerID: "user_123", AgentInstanceID: "agent_123",
		AgentGeneration: 2, AgentSessionID: "root_session", AuthorizationVersion: 3}
	writer := &cloudDSHFrameBuffer{}
	const reason = "This delegated Agent session predates checkpoint fencing and cannot safely continue after newer user input."
	if err := executor.writeCloudDSHChildAbort(context.Background(), fence, writer, "child_session", "child_call_1", reason); err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Type                 string `json:"type"`
		TaskID               string `json:"task_id"`
		OwnerID              string `json:"owner_id"`
		AgentID              string `json:"agent_id"`
		Generation           int64  `json:"generation"`
		AuthorizationVersion int64  `json:"authorization_version"`
		SessionID            string `json:"session_id"`
		CallID               string `json:"call_id"`
		Reason               string `json:"reason"`
	}
	if err := json.Unmarshal(writer.Bytes(), &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "child_abort" || frame.TaskID != fence.TaskID || frame.OwnerID != fence.OwnerID ||
		frame.AgentID != fence.AgentInstanceID || frame.Generation != fence.AgentGeneration ||
		frame.AuthorizationVersion != fence.AuthorizationVersion || frame.SessionID != "child_session" ||
		frame.CallID != "child_call_1" || frame.Reason != reason {
		t.Fatalf("child abort frame lost its target identity or reason: %+v", frame)
	}
	if err := executor.writeCloudDSHChildAbort(context.Background(), fence, writer, fence.AgentSessionID, "child_call_1", reason); err == nil {
		t.Fatal("root session was accepted as a child abort target")
	}
}
