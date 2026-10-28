package worker

import (
	"encoding/json"
	"testing"

	"morde/server/internal/app"
)

const checkpointToolArguments = `{"workspace_id":"w1234567","path":"notes.txt","content":"saved"}`
const checkpointToolOutput = `{"path":"notes.txt","bytes_written":5}`

func TestCloudDSHCheckpointHistoryReconcilesPinnedToolResult(t *testing.T) {
	history := newCloudDSHCheckpointHistory("session_1")
	history.beginRestore()
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
		checkpointTurnStart(0, "turn/start", map[string]any{"turn": 1}),
		checkpointTurnStart(1, "step/start", map[string]any{"turn": 1, "step": 1}),
		checkpointAssistantCall(2, "call_write_1"),
		checkpointToolCall(3, "call_write_1"),
		checkpointToolResult(4, "call_write_1", checkpointToolOutput, 3),
		checkpointAssistantText(5, "Saved the file."),
		checkpointTurnEnd(6, "step/end", map[string]any{"turn": 1, "step": 1}),
		checkpointTurnEnd(7, "turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}),
	), 0, 7); err != nil {
		t.Fatal(err)
	}
	history.finishRestore()
	if err := history.validateRecoveryToolCall("call_write_1", "file_write", []byte(checkpointToolArguments)); err != nil {
		t.Fatalf("validate recovered tool call: %v", err)
	}
	expected := history.recoveryOutput("call_write_1")
	if expected == nil || *expected != checkpointToolOutput {
		t.Fatalf("checkpoint output = %v", expected)
	}
	if err := history.noteToolResult("call_write_1", "file_write", []byte(checkpointToolArguments), app.AgentToolSucceeded, checkpointToolOutput, nil); err != nil {
		t.Fatalf("reconcile exact result: %v", err)
	}
	completion, err := history.completion(cloudDSHFence("session_1"), 7, make([]byte, 32))
	if err != nil {
		t.Fatalf("completion() error = %v", err)
	}
	if completion.Result != "Saved the file." || len(completion.Inputs) != 2 ||
		completion.Inputs[0].Type != "function_call" || completion.Inputs[1].Output != checkpointToolOutput ||
		completion.CheckpointThroughSequence != 7 || len(completion.CheckpointDigest) != 32 {
		t.Fatalf("completion = %#v", completion)
	}
}

func TestCloudDSHCheckpointHistoryRequiresRecoveredResultToBeDurable(t *testing.T) {
	history := newCloudDSHCheckpointHistory("session_1")
	history.beginRestore()
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
		checkpointTurnStart(0, "turn/start", map[string]any{"turn": 1}),
		checkpointTurnStart(1, "step/start", map[string]any{"turn": 1, "step": 1}),
		checkpointAssistantCall(2, "call_write_1"),
		checkpointToolCall(3, "call_write_1"),
	), 0, 3); err != nil {
		t.Fatal(err)
	}
	history.finishRestore()
	if history.recoveryOutput("call_write_1") != nil {
		t.Fatal("pending call unexpectedly has a checkpointed result")
	}
	if err := history.validateRecoveryToolCall("call_write_1", "file_write", []byte(checkpointToolArguments)); err != nil {
		t.Fatal(err)
	}
	if err := history.noteToolResult("call_write_1", "file_write", []byte(checkpointToolArguments), app.AgentToolSucceeded, checkpointToolOutput, nil); err != nil {
		t.Fatal(err)
	}
	if history.allCallsReconciled() {
		t.Fatal("runtime result was considered durable before its dsh checkpoint")
	}
	if err := history.append(cloudDSHCheckpointPayload("session_1", 4,
		checkpointToolResult(4, "call_write_1", checkpointToolOutput, 3),
		checkpointAssistantText(5, "Saved the file."),
		checkpointTurnEnd(6, "step/end", map[string]any{"turn": 1, "step": 1}),
		checkpointTurnEnd(7, "turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}),
	), 4, 7); err != nil {
		t.Fatal(err)
	}
	if !history.allCallsReconciled() {
		t.Fatal("matching durable result was not reconciled")
	}
	if _, err := history.completion(cloudDSHFence("session_1"), 7, make([]byte, 32)); err != nil {
		t.Fatalf("completion() error = %v", err)
	}
}

func TestCloudDSHCheckpointHistoryRejectsResultMismatchAndUnmatchedCalls(t *testing.T) {
	t.Run("result mismatch", func(t *testing.T) {
		history := newCloudDSHCheckpointHistory("session_1")
		history.beginRestore()
		if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
			checkpointTurnStart(0, "turn/start", map[string]any{"turn": 1}),
			checkpointTurnStart(1, "step/start", map[string]any{"turn": 1, "step": 1}),
			checkpointAssistantCall(2, "call_write_1"),
			checkpointToolCall(3, "call_write_1"),
			checkpointToolResult(4, "call_write_1", `{"path":"other.txt"}`, 3),
		), 0, 4); err != nil {
			t.Fatal(err)
		}
		history.finishRestore()
		if err := history.noteToolResult("call_write_1", "file_write", []byte(checkpointToolArguments), app.AgentToolSucceeded, checkpointToolOutput, nil); err == nil {
			t.Fatal("mismatched checkpoint result was accepted")
		}
	})
	t.Run("missing start", func(t *testing.T) {
		history := newCloudDSHCheckpointHistory("session_1")
		if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
			checkpointAssistantCall(0, "call_write_1"),
		), 0, 0); err != nil {
			t.Fatal(err)
		}
		if err := history.validateToolCall("call_write_1", "file_write", []byte(checkpointToolArguments)); err == nil {
			t.Fatal("tool call without a durable start was accepted during ordinary execution")
		}
	})
	t.Run("call identity mismatch", func(t *testing.T) {
		history := newCloudDSHCheckpointHistory("session_1")
		history.beginRestore()
		err := history.append(cloudDSHCheckpointPayload("session_1", 0,
			checkpointAssistantCall(0, "call_write_1"),
			checkpointToolCall(1, "call_write_other"),
		), 0, 1)
		if err == nil {
			t.Fatal("tool event with another ID was accepted")
		}
	})
}

func TestCloudDSHCheckpointHistoryRejectsStepEndWithPendingToolCall(t *testing.T) {
	history := newCloudDSHCheckpointHistory("session_1")
	history.beginRestore()
	err := history.append(cloudDSHCheckpointPayload("session_1", 0,
		checkpointTurnStart(0, "turn/start", map[string]any{"turn": 1}),
		checkpointTurnStart(1, "step/start", map[string]any{"turn": 1, "step": 1}),
		checkpointAssistantCall(2, "call_write_1"),
		checkpointToolCall(3, "call_write_1"),
		checkpointTurnEnd(4, "step/end", map[string]any{"turn": 1, "step": 1}),
	), 0, 4)
	if err == nil {
		t.Fatal("step end with a pending tool call was accepted")
	}
}

func TestCloudDSHCheckpointHistoryRejectsEmptyFinalAssistantResult(t *testing.T) {
	history := newCloudDSHCheckpointHistory("session_1")
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
		checkpointAssistantText(0, "  "),
	), 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := history.completion(cloudDSHFence("session_1"), 0, make([]byte, 32)); err == nil {
		t.Fatal("empty assistant result was accepted as a completed task")
	}
}

func checkpointAssistantCall(seq int64, callID string) cloudDSHCheckpointEvent {
	return cloudDSHCheckpointEvent{Sequence: seq, Type: "assistant/message", Data: map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool-call", "id": callID, "name": "file_write", "arguments": checkpointToolArguments},
		}},
	}}
}

func checkpointAssistantText(seq int64, text string) cloudDSHCheckpointEvent {
	return cloudDSHCheckpointEvent{Sequence: seq, Type: "assistant/message", Data: map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}},
	}}
}

func checkpointToolCall(seq int64, callID string) cloudDSHCheckpointEvent {
	return cloudDSHCheckpointEvent{Sequence: seq, Type: "tool/call", Data: map[string]any{
		"turn": 1, "step": 1, "callId": callID, "name": "file_write", "arguments": checkpointToolArguments,
	}}
}

func checkpointToolResult(seq int64, callID, output string, sourceSeq int64) cloudDSHCheckpointEvent {
	return cloudDSHCheckpointEvent{Sequence: seq, Type: "tool/result", Data: map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{"toolCallId": callID, "isError": false,
			"source":  map[string]any{"kind": "tool", "callId": callID},
			"content": []any{map[string]any{"type": "text", "text": output}}},
	}, SurfaceOp: "append", SourceEventSeqs: []int64{sourceSeq}}
}

func checkpointTurnStart(seq int64, eventType string, data map[string]any) cloudDSHCheckpointEvent {
	return cloudDSHCheckpointEvent{Sequence: seq, Type: eventType, Data: data}
}

func checkpointTurnEnd(seq int64, eventType string, data map[string]any) cloudDSHCheckpointEvent {
	return cloudDSHCheckpointEvent{Sequence: seq, Type: eventType, Data: data}
}

func cloudDSHFence(sessionID string) app.AgentInstanceFence {
	return app.AgentInstanceFence{AgentInstanceID: "agent_1", AgentGeneration: 1,
		AgentSessionID: sessionID, AuthorizationVersion: 1}
}

type cloudDSHCheckpointEvent struct {
	Sequence        int64   `json:"seq"`
	Type            string  `json:"type"`
	Data            any     `json:"data"`
	SurfaceOp       string  `json:"surfaceOp,omitempty"`
	SourceEventSeqs []int64 `json:"sourceEventSeqs,omitempty"`
}

func cloudDSHCheckpointPayload(sessionID string, from int64, events ...cloudDSHCheckpointEvent) []byte {
	payload, err := json.Marshal(struct {
		Header struct {
			ID string `json:"id"`
		} `json:"header"`
		From   int64                     `json:"from_seq"`
		Events []cloudDSHCheckpointEvent `json:"events"`
	}{Header: struct {
		ID string `json:"id"`
	}{ID: sessionID}, From: from, Events: events})
	if err != nil {
		panic(err)
	}
	return payload
}
