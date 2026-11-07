package app

import (
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestNewPendingApprovalRejectsInvalidDisplayText(t *testing.T) {
	call := ModelToolCall{CallID: "call-approval", Name: runtimeToolShellExec, Arguments: `{}`}
	hash, err := agentOperationHash(call.Name, json.RawMessage(call.Arguments))
	if err != nil {
		t.Fatal(err)
	}
	_, err = newPendingApproval(task.Task{ID: "t1234567"}, call, hash[:], AgentApprovalRequest{
		Object: "Workspace command", Content: "Run the command?", Impact: strings.Repeat("x", agentApprovalFieldMaxRunes+1),
	}, AgentHostFence{RuntimeGeneration: 1, RuntimeInstanceID: "runtime-1", ControlVersion: 1, AIAllowed: true}, 1)
	if domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("newPendingApproval() error = %v, want invalid state for unbounded display text", err)
	}
}
