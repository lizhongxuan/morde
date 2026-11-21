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
