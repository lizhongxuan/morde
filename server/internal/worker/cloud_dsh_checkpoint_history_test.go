package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"morde/server/internal/app"
)

const checkpointToolArguments = `{"workspace_id":"w1234567","path":"notes.txt","content":"saved"}`
const checkpointToolOutput = `{"path":"notes.txt","bytes_written":5}`

func TestCloudDSHCheckpointHistorySystemReplacement(t *testing.T) {
	system := cloudDSHCheckpointEvent{Sequence: 0, Type: "system/message", SurfaceOp: "append",
		Data: map[string]any{"message": map[string]any{"role": "system"}}}
	replacement := cloudDSHCheckpointEvent{Sequence: 1, Type: "system/message",
		SurfaceOp: map[string]any{"op": "replace", "startSeq": 0, "endSeq": 0}, SourceEventSeqs: []int64{0},
		Data: map[string]any{"message": map[string]any{"role": "system"}}}
	history := newCloudDSHCheckpointHistory("session_1")
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0, system), 0, 0); err != nil {
		t.Fatal(err)
	}
	payload := cloudDSHCheckpointPayload("session_1", 1, replacement)
	_, err := saveCloudDSHCheckpoint(context.Background(), &cloudDSHTestCheckpoints{err: errors.New("save failed")}, history,
		app.AgentCheckpointCommand{From: 1, Through: 1, Payload: payload})
	if err == nil {
		t.Fatal("checkpoint save failure was ignored")
	}
	if err := history.append(payload, 1, 1); err != nil {
		t.Fatalf("failed save changed the original system nodes: %v", err)
	}
	stale := replacement
	stale.Sequence = 2
	if err := history.clone().append(cloudDSHCheckpointPayload("session_1", 2, stale), 2, 2); err == nil {
		t.Fatal("replaced system node was accepted as an active target")
	}
	replacement.Sequence, replacement.SourceEventSeqs = 2, []int64{1}
	replacement.SurfaceOp = map[string]any{"op": "replace", "startSeq": 1, "endSeq": 1}
	if err := history.append(cloudDSHCheckpointPayload("session_1", 2, replacement), 2, 2); err != nil {
		t.Fatalf("replacement did not update the active system node: %v", err)
	}
}

func TestCloudDSHCheckpointHistoryRejectsInvalidSystemReplacement(t *testing.T) {
	valid := map[string]any{"op": "replace", "startSeq": 0, "endSeq": 0}
	for _, test := range []struct {
		name    string
		kind    string
		op      any
		sources []int64
		role    string
	}{
		{name: "missing end", kind: "system/message", op: map[string]any{"op": "replace", "startSeq": 0}, sources: []int64{0}, role: "system"},
		{name: "extra field", kind: "system/message", op: map[string]any{"op": "replace", "startSeq": 0, "endSeq": 0, "extra": true}, sources: []int64{0}, role: "system"},
		{name: "wrong operation", kind: "system/message", op: map[string]any{"op": "append", "startSeq": 0, "endSeq": 0}, sources: []int64{0}, role: "system"},
		{name: "negative target", kind: "system/message", op: map[string]any{"op": "replace", "startSeq": -1, "endSeq": -1}, sources: []int64{-1}, role: "system"},
		{name: "negative zero", kind: "system/message", op: map[string]any{"op": "replace", "startSeq": json.Number("-0"), "endSeq": 0}, sources: []int64{0}, role: "system"},
		{name: "fractional target", kind: "system/message", op: map[string]any{"op": "replace", "startSeq": 0.5, "endSeq": 0}, sources: []int64{0}, role: "system"},
		{name: "null target", kind: "system/message", op: map[string]any{"op": "replace", "startSeq": nil, "endSeq": 0}, sources: []int64{0}, role: "system"},
		{name: "unsafe target", kind: "system/message", op: map[string]any{"op": "replace", "startSeq": int64(1 << 53), "endSeq": int64(1 << 53)}, sources: []int64{1 << 53}, role: "system"},
		{name: "future target", kind: "system/message", op: map[string]any{"op": "replace", "startSeq": 2, "endSeq": 2}, sources: []int64{2}, role: "system"},
		{name: "non-system target", kind: "system/message", op: map[string]any{"op": "replace", "startSeq": 1, "endSeq": 1}, sources: []int64{1}, role: "system"},
		{name: "multiple nodes", kind: "system/message", op: map[string]any{"op": "replace", "startSeq": 0, "endSeq": 1}, sources: []int64{0, 1}, role: "system"},
		{name: "wrong source", kind: "system/message", op: valid, sources: []int64{1}, role: "system"},
		{name: "missing source", kind: "system/message", op: valid, role: "system"},
		{name: "wrong message role", kind: "system/message", op: valid, sources: []int64{0}, role: "user"},
		{name: "tool result object", kind: "tool/result", op: valid, sources: []int64{0}},
		{name: "other message object", kind: "user/message", op: valid, sources: []int64{0}},
		{name: "array", kind: "system/message", op: []any{}, sources: []int64{0}, role: "system"},
		{name: "boolean", kind: "system/message", op: true, sources: []int64{0}, role: "system"},
		{name: "number", kind: "system/message", op: 1, sources: []int64{0}, role: "system"},
	} {
		t.Run(test.name, func(t *testing.T) {
			history := newCloudDSHCheckpointHistory("session_1")
			err := history.append(cloudDSHCheckpointPayload("session_1", 0,
				cloudDSHCheckpointEvent{Sequence: 0, Type: "system/message", SurfaceOp: "append", Data: map[string]any{"message": map[string]any{"role": "system"}}},
				cloudDSHCheckpointEvent{Sequence: 1, Type: "user/message", SurfaceOp: "append", Data: map[string]any{"message": map[string]any{"role": "user"}}},
				cloudDSHCheckpointEvent{Sequence: 2, Type: test.kind, SurfaceOp: test.op, SourceEventSeqs: test.sources, Data: map[string]any{"message": map[string]any{"role": test.role}}},
			), 0, 2)
			if err == nil {
				t.Fatal("invalid replacement was accepted")
			}
		})
	}
}

func TestCloudDSHCheckpointHistoryAcceptsVerifiedCompactionReplacement(t *testing.T) {
	compactionID := "compact_1"
	preamble := cloudDSHCompactionPreamble + "\n\n<compacted-summary>"
	replacement := cloudDSHCheckpointEvent{Sequence: 6, Type: "user/message", SurfaceOp: map[string]any{
		"op": "replace", "startSeq": 2, "endSeq": 3}, SourceEventSeqs: []int64{4, 5, 2, 3}, Data: map[string]any{
		"role": "user", "source": map[string]any{"kind": "compact-checkpoint", "compactionId": compactionID},
		"content": []any{map[string]any{"type": "text", "text": preamble}, map[string]any{"type": "text", "text": "verified summary"},
			map[string]any{"type": "text", "text": "</compacted-summary>"}},
	}}
	events := []cloudDSHCheckpointEvent{
		{Sequence: 0, Type: "system/message", SurfaceOp: "append", Data: map[string]any{"message": map[string]any{"role": "system"}}},
		checkpointTurnStart(1, "turn/start", map[string]any{"turn": 1}),
		{Sequence: 2, Type: "user/message", SurfaceOp: "append", Data: map[string]any{"message": map[string]any{"role": "user"}}},
		checkpointAssistantText(3, "older assistant answer"),
		{Sequence: 4, Type: "compaction/start", Data: map[string]any{"compactionId": compactionID, "turn": 1}},
		{Sequence: 5, Type: "compaction/summary", Data: map[string]any{
			"compactionId": compactionID, "summary": []any{map[string]any{"type": "text", "text": "verified summary"}},
			"rawOutput": []any{map[string]any{"type": "text", "text": "verified summary"}}, "llmStreamCall": true,
			"shadowedRange": map[string]any{"start": 2, "end": 3}, "shadowedSeqs": []int64{2, 3}, "shadowedTokenCount": 100,
			"provider": "morde", "model": "test-model", "maxTokens": 256,
		}},
		replacement,
		{Sequence: 7, Type: "compaction/end", Data: map[string]any{"compactionId": compactionID, "turn": 1}},
	}
	history := newCloudDSHCheckpointHistory("session_1")
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0, events...), 0, 7); err != nil {
		t.Fatalf("verified DSH compaction was rejected: %v", err)
	}
	if _, active := history.surfaceNodes[2]; active {
		t.Fatal("shadowed source remained in the active surface")
	}
	if _, active := history.surfaceNodes[6]; !active || !history.compactions[compactionID].ended {
		t.Fatal("verified replacement or compaction end was not recorded")
	}
}

func TestCloudDSHCheckpointHistoryAcceptsRepeatedCompactionWithNonMonotonicSurfaceRange(t *testing.T) {
	compactionEvent := func(sequence int64, compactionID string, start, end int64, shadowed []int64) cloudDSHCheckpointEvent {
		return cloudDSHCheckpointEvent{Sequence: sequence, Type: "compaction/summary", Data: map[string]any{
			"compactionId": compactionID, "summary": []any{map[string]any{"type": "text", "text": "verified summary " + compactionID}},
			"rawOutput": []any{map[string]any{"type": "text", "text": "verified summary " + compactionID}}, "llmStreamCall": true,
			"shadowedRange": map[string]any{"start": start, "end": end}, "shadowedSeqs": shadowed, "shadowedTokenCount": 100,
			"provider": "morde", "model": "test-model", "maxTokens": 256,
		}}
	}
	replacement := func(sequence, start, end int64, compactionID, summary string, sources []int64) cloudDSHCheckpointEvent {
		return cloudDSHCheckpointEvent{Sequence: sequence, Type: "user/message", SurfaceOp: map[string]any{
			"op": "replace", "startSeq": start, "endSeq": end}, SourceEventSeqs: sources, Data: map[string]any{
			"role": "user", "source": map[string]any{"kind": "compact-checkpoint", "compactionId": compactionID},
			"content": []any{
				map[string]any{"type": "text", "text": cloudDSHCompactionPreamble + "\n\n<compacted-summary>"},
				map[string]any{"type": "text", "text": summary},
				map[string]any{"type": "text", "text": "</compacted-summary>"},
			},
		}}
	}
	firstSummary := "verified summary compact_1"
	secondSummary := "verified summary compact_2"
	events := []cloudDSHCheckpointEvent{
		{Sequence: 0, Type: "system/message", SurfaceOp: "append", Data: map[string]any{"message": map[string]any{"role": "system"}}},
		checkpointTurnStart(1, "turn/start", map[string]any{"turn": 1}),
		{Sequence: 2, Type: "user/message", SurfaceOp: "append", Data: map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "older request"}}}},
		checkpointAssistantText(3, "older answer"),
		{Sequence: 4, Type: "user/message", SurfaceOp: "append", Data: map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "recent request"}}}},
		{Sequence: 5, Type: "compaction/start", Data: map[string]any{"compactionId": "compact_1", "turn": 1}},
		compactionEvent(6, "compact_1", 2, 3, []int64{2, 3}),
		replacement(7, 2, 3, "compact_1", firstSummary, []int64{5, 6, 2, 3}),
		{Sequence: 8, Type: "compaction/end", Data: map[string]any{"compactionId": "compact_1", "turn": 1}},
		{Sequence: 9, Type: "user/message", SurfaceOp: "append", Data: map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "follow-up request"}}}},
		checkpointAssistantText(10, "follow-up answer"),
		{Sequence: 11, Type: "compaction/start", Data: map[string]any{"compactionId": "compact_2", "turn": 1}},
		compactionEvent(12, "compact_2", 7, 4, []int64{7, 4}),
		replacement(13, 7, 4, "compact_2", secondSummary, []int64{11, 12, 7, 4}),
		{Sequence: 14, Type: "compaction/end", Data: map[string]any{"compactionId": "compact_2", "turn": 1}},
	}
	history := newCloudDSHCheckpointHistory("session_1")
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0, events...), 0, 14); err != nil {
		t.Fatalf("repeated compaction with a reversed numeric endpoint pair was rejected: %v", err)
	}
	if got := history.surfaceOrder; len(got) != 4 || got[0] != 0 || got[1] != 13 || got[2] != 9 || got[3] != 10 {
		t.Fatalf("active DSH surface after repeated compaction = %v; want [0 13 9 10]", got)
	}
	if _, active := history.surfaceNodes[7]; active {
		t.Fatal("first compacted replacement remained active after the second compaction")
	}
	if proof := history.compactions["compact_2"]; proof == nil || !proof.ended || proof.start != 7 || proof.end != 4 {
		t.Fatalf("second compaction proof = %+v; want verified range 7..4", proof)
	}
}

func TestCloudDSHCheckpointHistoryAcceptsDirectUserMessagePayload(t *testing.T) {
	history := newCloudDSHCheckpointHistory("session_1")
	err := history.append(cloudDSHCheckpointPayload("session_1", 0,
		cloudDSHCheckpointEvent{Sequence: 0, Type: "user/message", SurfaceOp: "append", Data: map[string]any{
			"role": "user", "content": []any{map[string]any{"type": "text", "text": "message"}},
		}}), 0, 0)
	if err != nil {
		t.Fatalf("DSH user message payload was rejected: %v", err)
	}
	if _, active := history.surfaceNodes[0]; !active {
		t.Fatal("direct user message was not added to the active surface")
	}
}

func TestCloudDSHCheckpointHistoryRejectsUnprovenCompactionReplacement(t *testing.T) {
	compactionID := "compact_2"
	events := []cloudDSHCheckpointEvent{
		{Sequence: 0, Type: "system/message", SurfaceOp: "append", Data: map[string]any{"message": map[string]any{"role": "system"}}},
		checkpointTurnStart(1, "turn/start", map[string]any{"turn": 1}),
		{Sequence: 2, Type: "user/message", SurfaceOp: "append", Data: map[string]any{"message": map[string]any{"role": "user"}}},
		checkpointAssistantText(3, "older assistant answer"),
		{Sequence: 4, Type: "compaction/start", Data: map[string]any{"compactionId": compactionID, "turn": 1}},
		{Sequence: 5, Type: "compaction/summary", Data: map[string]any{
			"compactionId": compactionID, "summary": []any{map[string]any{"type": "text", "text": "verified summary"}},
			"shadowedRange": map[string]any{"start": 2, "end": 3}, "shadowedSeqs": []int64{2, 3}, "shadowedTokenCount": 100,
			"provider": "morde", "model": "test-model", "maxTokens": 256,
		}},
		{Sequence: 6, Type: "user/message", SurfaceOp: map[string]any{"op": "replace", "startSeq": 2, "endSeq": 3},
			SourceEventSeqs: []int64{4, 5, 3, 2}, Data: map[string]any{"message": map[string]any{
				"role": "user", "source": map[string]any{"kind": "compact-checkpoint", "compactionId": compactionID},
				"content": []any{map[string]any{"type": "text", "text": cloudDSHCompactionPreamble + "\n\n<compacted-summary>"},
					map[string]any{"type": "text", "text": "verified summary"}, map[string]any{"type": "text", "text": "</compacted-summary>"}},
			}}},
	}
	history := newCloudDSHCheckpointHistory("session_1")
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0, events...), 0, 6); err == nil {
		t.Fatal("compaction with a reordered source proof was accepted")
	}
}

func TestCloudDSHCheckpointHistoryAcceptsVerifiedToolResultPrune(t *testing.T) {
	callID := "call_prune_1"
	original := strings.Repeat("tool output ", 900)
	pruned := cloudDSHPrunedToolResultText(original)
	replacement := checkpointToolResult(5, callID, pruned, 3)
	replacement.SurfaceOp = map[string]any{"op": "replace", "startSeq": 3, "endSeq": 3}
	replacement.SourceEventSeqs = []int64{3}
	events := []cloudDSHCheckpointEvent{
		checkpointTurnStart(0, "turn/start", map[string]any{"turn": 1}),
		checkpointAssistantCall(1, callID),
		checkpointToolCall(2, callID),
		checkpointToolResult(3, callID, original, 2),
		{Sequence: 4, Type: "compaction/prune", Data: map[string]any{
			"shadowedRange": map[string]any{"start": 3, "end": 3}, "shadowedSeqs": []int64{3}, "shadowedTokenCount": 10_000,
		}},
		replacement,
		checkpointAssistantText(6, "done"),
		{Sequence: 7, Type: "step/end", Data: map[string]any{"turn": 1, "step": 1}},
		checkpointTurnEnd(8, "turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}),
	}
	history := newCloudDSHCheckpointHistory("session_1")
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0, events...), 0, 8); err != nil {
		t.Fatalf("verified DSH tool-result prune was rejected: %v", err)
	}
	if _, active := history.surfaceNodes[3]; active {
		t.Fatal("full tool-result source remained active after the verified replacement")
	}
	if _, active := history.surfaceNodes[5]; !active || history.calls[callID].surfaceResultSeq != 5 || history.calls[callID].resultText != original {
		t.Fatal("pruned projection replaced the durable tool result or was not activated")
	}
	if err := history.noteToolResult(callID, "file_write", []byte(checkpointToolArguments), app.AgentToolSucceeded, original, nil); err != nil {
		t.Fatalf("original tool result no longer reconciles against the operation ledger: %v", err)
	}
	if len(history.inputs) != 2 || history.inputs[1].Output != original {
		t.Fatal("completion inputs did not retain the full reconciled tool output")
	}
}

func TestCloudDSHCheckpointHistoryRejectsUnprovenToolResultPrune(t *testing.T) {
	callID := "call_prune_2"
	original := strings.Repeat("tool output ", 900)
	for _, test := range []struct {
		name        string
		replacement func() cloudDSHCheckpointEvent
		intervene   bool
	}{
		{name: "wrong source", replacement: func() cloudDSHCheckpointEvent {
			event := checkpointToolResult(5, callID, cloudDSHPrunedToolResultText(original), 3)
			event.SurfaceOp = map[string]any{"op": "replace", "startSeq": 3, "endSeq": 3}
			event.SourceEventSeqs = []int64{2}
			return event
		}},
		{name: "unexpected content", replacement: func() cloudDSHCheckpointEvent {
			event := checkpointToolResult(5, callID, strings.Repeat("x", 7000), 3)
			event.SurfaceOp = map[string]any{"op": "replace", "startSeq": 3, "endSeq": 3}
			event.SourceEventSeqs = []int64{3}
			return event
		}},
		{name: "nonadjacent replacement", intervene: true, replacement: func() cloudDSHCheckpointEvent {
			event := checkpointToolResult(6, callID, cloudDSHPrunedToolResultText(original), 3)
			event.SurfaceOp = map[string]any{"op": "replace", "startSeq": 3, "endSeq": 3}
			event.SourceEventSeqs = []int64{3}
			return event
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := []cloudDSHCheckpointEvent{
				checkpointTurnStart(0, "turn/start", map[string]any{"turn": 1}),
				checkpointAssistantCall(1, callID), checkpointToolCall(2, callID), checkpointToolResult(3, callID, original, 2),
				{Sequence: 4, Type: "compaction/prune", Data: map[string]any{
					"shadowedRange": map[string]any{"start": 3, "end": 3}, "shadowedSeqs": []int64{3}, "shadowedTokenCount": 10_000,
				}},
			}
			if test.intervene {
				events = append(events, checkpointAssistantText(5, "unrelated event"))
			}
			events = append(events, test.replacement())
			last := events[len(events)-1].Sequence
			if err := newCloudDSHCheckpointHistory("session_1").append(cloudDSHCheckpointPayload("session_1", 0, events...), 0, last); err == nil {
				t.Fatal("invalid DSH tool-result prune was accepted")
			}
		})
	}
}

func TestCloudDSHCheckpointHistoryKeepsToolResultAppendRequirement(t *testing.T) {
	for _, op := range []any{json.RawMessage(`null`), "replace", map[string]any{"op": "replace", "startSeq": 0, "endSeq": 0}} {
		history := newCloudDSHCheckpointHistory("session_1")
		result := checkpointToolResult(2, "call_write_1", checkpointToolOutput, 1)
		result.SurfaceOp = op
		if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
			checkpointAssistantCall(0, "call_write_1"), checkpointToolCall(1, "call_write_1"), result), 0, 2); err == nil {
			t.Fatalf("tool result with surfaceOp %v was accepted", op)
		}
	}
	history := newCloudDSHCheckpointHistory("session_1")
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
		cloudDSHCheckpointEvent{Sequence: 0, Type: "user/message", SurfaceOp: json.RawMessage(`null`), Data: map[string]any{}}), 0, 0); err != nil {
		t.Fatalf("existing non-tool null behavior changed: %v", err)
	}
}

func TestCloudDSHCheckpointHistoryPersistsUnknownSupersededToolResultWithoutReconciling(t *testing.T) {
	const callID = "call_unknown_1"
	event := cloudDSHCheckpointEvent{Sequence: 4, Type: "tool/result", SurfaceOp: "append", SourceEventSeqs: []int64{3}, Data: map[string]any{
		"turn": 1, "step": 1, "message": map[string]any{"toolCallId": callID, "isError": true,
			"source": map[string]any{"kind": "tool", "callId": callID}, "content": []any{
				map[string]any{"type": "text", "text": cloudDSHUnknownToolResultText}}},
		"error": map[string]any{"name": cloudDSHUnknownToolResultErrorName, "code": cloudDSHUnknownToolResultErrorCode},
	}}
	history := newCloudDSHCheckpointHistory("session_1")
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
		checkpointTurnStart(0, "turn/start", map[string]any{"turn": 1}),
		checkpointTurnStart(1, "step/start", map[string]any{"turn": 1, "step": 1}),
		checkpointAssistantCall(2, callID), checkpointToolCall(3, callID), event), 0, 4); err != nil {
		t.Fatalf("explicit unknown tool outcome was not persisted: %v", err)
	}
	call := history.calls[callID]
	if !call.resultSeen || !call.resultUnknown || call.reconciled || history.allCallsReconciled() {
		t.Fatalf("unknown tool outcome was treated as reconciled: %+v", call)
	}

	for name, mutate := range map[string]func(cloudDSHCheckpointEvent) cloudDSHCheckpointEvent{
		"wrong marker": func(value cloudDSHCheckpointEvent) cloudDSHCheckpointEvent {
			value.Data.(map[string]any)["error"] = map[string]any{"name": "ToolOutcomeUnknownError", "code": "OTHER"}
			return value
		},
		"wrong message": func(value cloudDSHCheckpointEvent) cloudDSHCheckpointEvent {
			value.Data.(map[string]any)["message"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] = "Operation completed."
			return value
		},
	} {
		t.Run(name, func(t *testing.T) {
			value := event
			encoded, _ := json.Marshal(event.Data)
			if err := json.Unmarshal(encoded, &value.Data); err != nil {
				t.Fatal(err)
			}
			value = mutate(value)
			history := newCloudDSHCheckpointHistory("session_1")
			if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
				checkpointTurnStart(0, "turn/start", map[string]any{"turn": 1}),
				checkpointTurnStart(1, "step/start", map[string]any{"turn": 1, "step": 1}),
				checkpointAssistantCall(2, callID), checkpointToolCall(3, callID), value), 0, 4); err == nil {
				t.Fatal("invalid unknown tool outcome was accepted")
			}
		})
	}
}

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

func TestCloudDSHCheckpointHistoryKeepsOnlyAnswerBoundEvidenceClaims(t *testing.T) {
	for _, test := range []struct {
		name       string
		finalText  string
		wantClaims bool
		invalidEnd bool
	}{
		{name: "exact answer", finalText: "已完成", wantClaims: true},
		{name: "answer changed", finalText: "已完成，另有结果", wantClaims: false},
		{name: "invalid range", finalText: "已完成", invalidEnd: true, wantClaims: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			answer := "已完成"
			endByte := int32(len(answer))
			if test.invalidEnd {
				endByte++
			}
			args, err := json.Marshal(map[string]any{"claims": []app.TaskResultClaim{{
				ID: "claim-1", StartByte: 0, EndByte: endByte, EvidenceRefs: []app.TaskValidationEvidenceRef{
					{Kind: app.TaskValidationEvidenceSource, ID: "current"},
					{Kind: app.TaskValidationEvidenceAction, CallID: "call_action_1"},
				},
			}}})
			if err != nil {
				t.Fatal(err)
			}
			history := newCloudDSHCheckpointHistory("session_1")
			if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
				checkpointTurnStart(0, "turn/start", map[string]any{"turn": 1}),
				checkpointTurnStart(1, "step/start", map[string]any{"turn": 1, "step": 1}),
				checkpointAssistantTextAndTool(2, answer, "call_evidence_1", cloudDSHResultEvidenceToolName, string(args)),
				checkpointNamedToolCall(3, "call_evidence_1", cloudDSHResultEvidenceToolName, string(args)),
			), 0, 3); err != nil {
				t.Fatal(err)
			}
			ack := "Evidence map received as an untrusted claim."
			if err := history.noteToolResult("call_evidence_1", cloudDSHResultEvidenceToolName, args,
				app.AgentToolSucceeded, ack, nil); err != nil {
				t.Fatal(err)
			}
			if err := history.append(cloudDSHCheckpointPayload("session_1", 4,
				checkpointNamedToolResult(4, "call_evidence_1", ack, 3),
				checkpointAssistantText(5, test.finalText),
				checkpointTurnEnd(6, "step/end", map[string]any{"turn": 1, "step": 1}),
				checkpointTurnEnd(7, "turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}),
			), 4, 7); err != nil {
				t.Fatal(err)
			}
			completion, err := history.completion(cloudDSHFence("session_1"), 7, make([]byte, 32))
			if err != nil {
				t.Fatal(err)
			}
			if got := len(completion.ValidationClaims) > 0; got != test.wantClaims {
				t.Fatalf("completion ValidationClaims present = %v, want %v", got, test.wantClaims)
			}
		})
	}
}

func TestCloudDSHCheckpointHistoryDoesNotReuseEvidenceAcrossTurns(t *testing.T) {
	answer := "已完成"
	args, err := json.Marshal(map[string]any{"claims": []app.TaskResultClaim{{
		ID: "claim-1", StartByte: 0, EndByte: int32(len(answer)), EvidenceRefs: []app.TaskValidationEvidenceRef{
			{Kind: app.TaskValidationEvidenceSource, ID: "current"},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	history := newCloudDSHCheckpointHistory("session_1")
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
		checkpointTurnStart(0, "turn/start", map[string]any{"turn": 1}),
		checkpointTurnStart(1, "step/start", map[string]any{"turn": 1, "step": 1}),
		checkpointAssistantTextAndTool(2, answer, "call_evidence_1", cloudDSHResultEvidenceToolName, string(args)),
		checkpointNamedToolCall(3, "call_evidence_1", cloudDSHResultEvidenceToolName, string(args)),
	), 0, 3); err != nil {
		t.Fatal(err)
	}
	ack := "Evidence map received as an untrusted claim."
	if err := history.noteToolResult("call_evidence_1", cloudDSHResultEvidenceToolName, args, app.AgentToolSucceeded, ack, nil); err != nil {
		t.Fatal(err)
	}
	if err := history.append(cloudDSHCheckpointPayload("session_1", 4,
		checkpointNamedToolResult(4, "call_evidence_1", ack, 3), checkpointAssistantText(5, answer),
		checkpointTurnEnd(6, "step/end", map[string]any{"turn": 1, "step": 1}),
		checkpointTurnEnd(7, "turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}),
	), 4, 7); err != nil {
		t.Fatal(err)
	}
	if err := history.append(cloudDSHCheckpointPayload("session_1", 8,
		checkpointTurnStart(8, "turn/start", map[string]any{"turn": 2}),
		checkpointTurnStart(9, "step/start", map[string]any{"turn": 2, "step": 1}),
		cloudDSHCheckpointEvent{Sequence: 10, Type: "assistant/message", Data: map[string]any{"turn": 2, "step": 1,
			"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": answer}}}}},
		checkpointTurnEnd(11, "step/end", map[string]any{"turn": 2, "step": 1}),
		checkpointTurnEnd(12, "turn/end", map[string]any{"turn": 2, "reason": map[string]any{"kind": "completed"}}),
	), 8, 12); err != nil {
		t.Fatal(err)
	}
	completion, err := history.completion(cloudDSHFence("session_1"), 12, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if len(completion.ValidationClaims) != 0 {
		t.Fatalf("stale prior-turn claims survived: %#v", completion.ValidationClaims)
	}
}

func TestCloudDSHPerformanceComparisonRequiresMatchingReconciledMeasurements(t *testing.T) {
	comparison := &app.TaskResultPerformanceComparison{
		Metric: "latency", Unit: "ms", Before: 120, After: 90, Environment: "same host",
		BaselineCallID: "call_baseline_1", AfterCallID: "call_after_1",
	}
	measurement := func(value string) *cloudDSHToolCall {
		return &cloudDSHToolCall{callSeen: true, resultSeen: true, reconciled: true, status: app.AgentToolSucceeded,
			output: value}
	}
	baseOutput := `{"type":"morde.performance_measurement.v1","metric":"latency","unit":"ms","value":120,"environment":"same host"}`
	afterOutput := `{"type":"morde.performance_measurement.v1","metric":"latency","unit":"ms","value":90,"environment":"same host"}`
	for _, test := range []struct {
		name      string
		baseline  *cloudDSHToolCall
		after     *cloudDSHToolCall
		wantMatch bool
	}{
		{name: "matching outputs", baseline: measurement(baseOutput), after: measurement(afterOutput), wantMatch: true},
		{name: "claimed value differs", baseline: measurement(baseOutput), after: measurement(strings.Replace(afterOutput, `"value":90`, `"value":91`, 1))},
		{name: "different metric", baseline: measurement(baseOutput), after: measurement(strings.Replace(afterOutput, `"metric":"latency"`, `"metric":"throughput"`, 1))},
		{name: "not reconciled", baseline: measurement(baseOutput), after: func() *cloudDSHToolCall {
			call := measurement(afterOutput)
			call.reconciled = false
			return call
		}()},
		{name: "failed tool", baseline: measurement(baseOutput), after: func() *cloudDSHToolCall {
			call := measurement(afterOutput)
			call.status = app.AgentToolFailed
			return call
		}()},
		{name: "missing value", baseline: measurement(`{"type":"morde.performance_measurement.v1","metric":"latency","unit":"ms","environment":"same host"}`), after: measurement(afterOutput)},
		{name: "duplicate key", baseline: measurement(`{"type":"morde.performance_measurement.v1","metric":"latency","unit":"ms","value":120,"value":120,"environment":"same host"}`), after: measurement(afterOutput)},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := []app.TaskResultClaim{{PerformanceComparison: comparison}}
			got := cloudDSHPerformanceComparisonsMatch(claims, map[string]*cloudDSHToolCall{
				"call_baseline_1": test.baseline, "call_after_1": test.after,
			})
			if got != test.wantMatch {
				t.Fatalf("comparison matched = %v, want %v", got, test.wantMatch)
			}
		})
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

func TestCloudDSHCheckpointHistoryRejectsUnfinishedTurnForCompletion(t *testing.T) {
	history := newCloudDSHCheckpointHistory("session_1")
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
		checkpointAssistantText(0, "Earlier completed result."),
		checkpointTurnEnd(1, "step/end", map[string]any{"turn": 1, "step": 1}),
		checkpointTurnEnd(2, "turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}),
		checkpointTurnStart(3, "turn/start", map[string]any{"turn": 2}),
	), 0, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := history.completion(cloudDSHFence("session_1"), 3, make([]byte, 32)); err == nil {
		t.Fatal("an unfinished turn reused the previous assistant result")
	}
}

func TestCloudDSHCheckpointHistoryRejectsCompletedTurnWithoutNewAssistantOutput(t *testing.T) {
	history := newCloudDSHCheckpointHistory("session_1")
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
		checkpointAssistantText(0, "Earlier completed result."),
		checkpointTurnEnd(1, "step/end", map[string]any{"turn": 1, "step": 1}),
		checkpointTurnEnd(2, "turn/end", map[string]any{"turn": 1, "reason": map[string]any{"kind": "completed"}}),
		checkpointTurnStart(3, "turn/start", map[string]any{"turn": 2}),
		checkpointTurnStart(4, "step/start", map[string]any{"turn": 2, "step": 1}),
		checkpointTurnEnd(5, "step/end", map[string]any{"turn": 2, "step": 1}),
		checkpointTurnEnd(6, "turn/end", map[string]any{"turn": 2, "reason": map[string]any{"kind": "completed"}}),
	), 0, 6); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(history.result) != "" {
		t.Fatal("the previous turn's result remained current after a new turn started")
	}
	if _, err := history.completion(cloudDSHFence("session_1"), 6, make([]byte, 32)); err == nil {
		t.Fatal("a completed turn without new assistant output reused the previous result")
	}
}

func TestCloudDSHCheckpointHistoryRejectsAssistantResultFromAnotherTurn(t *testing.T) {
	history := newCloudDSHCheckpointHistory("session_1")
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
		checkpointAssistantText(0, "Earlier result."),
		checkpointTurnEnd(1, "turn/end", map[string]any{"turn": 2, "reason": map[string]any{"kind": "completed"}}),
	), 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := history.completion(cloudDSHFence("session_1"), 1, make([]byte, 32)); err == nil {
		t.Fatal("a completed marker for a different turn accepted the old assistant result")
	}
}

func TestCloudDSHCheckpointHistoryRejectsRecoveredScreenshotWithoutImageBytes(t *testing.T) {
	const callID = "call_screenshot_1"
	const output = `{"mime_type":"image/jpeg","width":1,"height":1}`
	imageID := "morde_img_" + strings.Repeat("a", 64)
	history := newCloudDSHCheckpointHistory("session_1")
	history.beginRestore()
	if err := history.append(cloudDSHCheckpointPayload("session_1", 0,
		checkpointTurnStart(0, "turn/start", map[string]any{"turn": 1}),
		checkpointTurnStart(1, "step/start", map[string]any{"turn": 1, "step": 1}),
		cloudDSHCheckpointEvent{Sequence: 2, Type: "assistant/message", Data: map[string]any{
			"turn": 1, "step": 1, "message": map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool-call", "id": callID, "name": "desktop_screenshot", "arguments": `{}`},
			}},
		}},
		cloudDSHCheckpointEvent{Sequence: 3, Type: "tool/call", Data: map[string]any{
			"turn": 1, "step": 1, "callId": callID, "name": "desktop_screenshot", "arguments": `{}`,
		}},
		cloudDSHCheckpointEvent{Sequence: 4, Type: "tool/result", SurfaceOp: "append", SourceEventSeqs: []int64{3}, Data: map[string]any{
			"turn": 1, "step": 1, "message": map[string]any{"toolCallId": callID, "isError": false,
				"source": map[string]any{"kind": "tool", "callId": callID}, "content": []any{
					map[string]any{"type": "text", "text": output},
					map[string]any{"type": "image", "attachment": map[string]any{
						"attachmentId": imageID, "mediaType": "image/jpeg", "bytes": 4, "width": 1, "height": 1,
					}},
				}},
		}},
	), 0, 4); err != nil {
		t.Fatal(err)
	}
	history.finishRestore()
	if err := history.noteToolResult(callID, "desktop_screenshot", []byte(`{}`), app.AgentToolSucceeded, output, nil); err == nil {
		t.Fatal("recovered screenshot without its authenticated image bytes was accepted")
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

func checkpointAssistantTextAndTool(seq int64, text, callID, name, arguments string) cloudDSHCheckpointEvent {
	return cloudDSHCheckpointEvent{Sequence: seq, Type: "assistant/message", Data: map[string]any{
		"turn": 1, "step": 1,
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": text},
			map[string]any{"type": "tool-call", "id": callID, "name": name, "arguments": arguments},
		}},
	}}
}

func checkpointToolCall(seq int64, callID string) cloudDSHCheckpointEvent {
	return cloudDSHCheckpointEvent{Sequence: seq, Type: "tool/call", Data: map[string]any{
		"turn": 1, "step": 1, "callId": callID, "name": "file_write", "arguments": checkpointToolArguments,
	}}
}

func checkpointNamedToolCall(seq int64, callID, name, arguments string) cloudDSHCheckpointEvent {
	return cloudDSHCheckpointEvent{Sequence: seq, Type: "tool/call", Data: map[string]any{
		"turn": 1, "step": 1, "callId": callID, "name": name, "arguments": arguments,
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

func checkpointNamedToolResult(seq int64, callID, output string, sourceSeq int64) cloudDSHCheckpointEvent {
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
	SurfaceOp       any     `json:"surfaceOp,omitempty"`
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
