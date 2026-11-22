package worker

import (
	"encoding/json"
	"testing"
)

func TestCloudDSHCoordinatorFrameTerminalContracts(t *testing.T) {
	base := map[string]any{
		"context_role": "coordinator", "conversation_id": "c1234567", "message_id": "m1234567",
		"owner_id": "u1234567", "actor_id": "u1234567", "agent_id": "coord_12345678",
		"generation": 1, "authorization_version": 3, "session_id": "coord_session_12345678",
		"purpose": "conversation", "operation_id": "",
	}
	tests := []struct {
		name    string
		fields  map[string]any
		wantErr bool
	}{
		{name: "settled", fields: map[string]any{"type": "coordinator_settled", "state": "completed", "through_seq": 2, "result": "{}"}},
		{name: "settled without completion state", fields: map[string]any{"type": "coordinator_settled", "through_seq": 2, "result": "{}"}, wantErr: true},
		{name: "failed", fields: map[string]any{"type": "coordinator_failed", "code": "runtime_unavailable"}},
		{name: "failed after process exit", fields: map[string]any{"type": "coordinator_failed", "code": "runtime_unavailable", "reason": "child_exited_without_result", "process_exited": true}},
		{name: "terminated", fields: map[string]any{"type": "coordinator_terminated", "reason": "cancelled", "state": "cancelled", "process_exited": true}},
		{name: "terminated without process proof", fields: map[string]any{"type": "coordinator_terminated", "reason": "cancelled", "state": "cancelled", "process_exited": false}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame := make(map[string]any, len(base)+len(test.fields))
			for key, value := range base {
				frame[key] = value
			}
			for key, value := range test.fields {
				frame[key] = value
			}
			encoded, err := json.Marshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			_, err = decodeCloudDSHCoordinatorFrame(encoded)
			if (err != nil) != test.wantErr {
				t.Fatalf("decode coordinator terminal error = %v, want error %t", err, test.wantErr)
			}
		})
	}
}

func TestCloudDSHCoordinatorRuntimeFrameRequiresExactFields(t *testing.T) {
	frame := map[string]json.RawMessage{}
	encoded, err := json.Marshal(map[string]any{
		"type": "coordinator_process_ready", "server_instance_id": "00000000-0000-4000-8000-000000000001",
		"owner_id": "u1234567", "runtime_id": "runtime_123", "runtime_generation": 1,
		"runtime_sequence": 1, "container_pid": 1234,
		"context_role": "coordinator", "conversation_id": "c1234567", "message_id": "m1234567",
		"actor_id": "u1234567", "agent_id": "coord_12345678", "generation": 1,
		"authorization_version": 3, "session_id": "coord_session_12345678",
		"purpose": "conversation", "operation_id": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &frame); err != nil {
		t.Fatal(err)
	}
	if !validCloudDSHCoordinatorRuntimeFrame("coordinator_process_ready", frame) {
		t.Fatal("valid process-ready frame was rejected")
	}
	frame["unexpected"] = json.RawMessage(`true`)
	if validCloudDSHCoordinatorRuntimeFrame("coordinator_process_ready", frame) {
		t.Fatal("process-ready frame accepted an unrecognized field")
	}
	for _, kind := range []string{"coordinator_started", "checkpoint", "checkpoint_read", "model", "model_cancel",
		"coordinator_settled", "coordinator_failed", "coordinator_terminated"} {
		value := map[string]any{"type": kind, "server_instance_id": "00000000-0000-4000-8000-000000000001",
			"owner_id": "u1234567", "runtime_id": "runtime_123", "runtime_generation": 1, "runtime_sequence": 1,
			"context_role": "coordinator", "conversation_id": "c1234567", "message_id": "m1234567", "actor_id": "u1234567",
			"agent_id": "coord_12345678", "generation": 1, "authorization_version": 3,
			"session_id": "coord_session_12345678", "purpose": "conversation", "operation_id": ""}
		switch kind {
		case "checkpoint":
			value["id"], value["from_seq"], value["through_seq"], value["payload"], value["sha256"] = "c-1", 0, 0, "{}", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		case "checkpoint_read":
			value["id"], value["from_seq"] = "p-1", 0
		case "model":
			value["id"], value["through_seq"], value["request"], value["sha256"] = "model-1", 0, map[string]any{}, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		case "model_cancel":
			value["id"] = "model-1"
		case "coordinator_settled":
			value["state"], value["through_seq"], value["result"] = "completed", 0, "{}"
		case "coordinator_failed":
			value["code"] = "runtime_unavailable"
		case "coordinator_terminated":
			value["reason"], value["state"], value["process_exited"] = "cancelled", "cancelled", true
		}
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		decoded := make(map[string]json.RawMessage)
		if err := json.Unmarshal(raw, &decoded); err != nil || !validCloudDSHCoordinatorRuntimeFrame(kind, decoded) {
			t.Fatalf("valid %s frame with sequence was rejected: %v", kind, err)
		}
		delete(decoded, "runtime_sequence")
		if validCloudDSHCoordinatorRuntimeFrame(kind, decoded) {
			t.Fatalf("%s frame without runtime sequence was accepted", kind)
		}
	}
}
