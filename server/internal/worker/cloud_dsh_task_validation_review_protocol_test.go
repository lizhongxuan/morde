package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/app"
)

func taskValidationReviewProtocolFence() app.TaskValidationReviewModelFence {
	digest := sha256.Sum256([]byte("verified result"))
	claim := app.TaskValidationReviewClaim{OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567",
		TaskID: "q1234567", TaskRevision: 4, ResultEventSequence: 9, ResultSHA256: digest[:], ClaimGeneration: 2,
		LeaseOwner: "worker-a"}
	return app.TaskValidationReviewModelFence{Claim: claim, InitiatorID: claim.InitiatorID, SessionID: "review-session-1", ThroughSequence: 3}
}

func taskValidationReviewProtocolIdentity(identity cloudDSHTaskValidationReviewIdentityWire) map[string]any {
	return map[string]any{
		"context_role": identity.ContextRole, "owner_id": identity.OwnerID, "actor_id": identity.ActorID,
		"task_id": identity.TaskID, "task_revision": identity.TaskRevision,
		"result_event_sequence": identity.ResultEventSequence, "result_sha256": identity.ResultSHA256,
		"claim_generation": identity.ClaimGeneration, "lease_owner": identity.LeaseOwner,
		"session_id": identity.SessionID, "purpose": identity.Purpose,
	}
}

func taskValidationReviewProtocolStart(fence app.TaskValidationReviewModelFence) map[string]any {
	start := taskValidationReviewProtocolIdentity(cloudDSHTaskValidationReviewIdentityForFence(fence))
	start["type"] = "task_validation_review_start"
	start["version"] = 1
	start["model"] = "p1234567"
	start["tools"] = []any{}
	start["system_prompt"] = "Review the claimed task result."
	start["prompt"] = "Check the result against the evidence."
	return start
}

func taskValidationReviewProtocolFrame(kind string, fence app.TaskValidationReviewModelFence, body map[string]any) map[string]any {
	frame := taskValidationReviewProtocolIdentity(cloudDSHTaskValidationReviewIdentityForFence(fence))
	frame["type"] = kind
	for key, value := range body {
		frame[key] = value
	}
	return frame
}

func taskValidationReviewRawFrame(t *testing.T, frame map[string]any) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestCloudDSHTaskValidationReviewStartFenceAndTools(t *testing.T) {
	fence := taskValidationReviewProtocolFence()
	if !validCloudDSHTaskValidationReviewModelFence(fence) {
		t.Fatal("expected fixture fence to be valid")
	}
	encoded, err := json.Marshal(taskValidationReviewProtocolStart(fence))
	if err != nil || !validCloudDSHTaskValidationReviewStart(encoded, fence) {
		t.Fatalf("valid tool-free start rejected: %s, %v", encoded, err)
	}

	stale := fence
	stale.Claim.ClaimGeneration++
	if !validCloudDSHTaskValidationReviewModelFence(stale) || cloudDSHTaskValidationReviewIdentityMatches(
		cloudDSHTaskValidationReviewIdentityForFence(fence), stale) {
		t.Fatal("stale claim generation matched the active review fence")
	}

	mismatchedInitiator := fence
	mismatchedInitiator.InitiatorID = "u7654321"
	if validCloudDSHTaskValidationReviewModelFence(mismatchedInitiator) {
		t.Fatal("review fence accepted an initiator different from the claim")
	}

	changed := taskValidationReviewProtocolStart(fence)
	changed["task_revision"] = fence.Claim.TaskRevision + 1
	encoded, _ = json.Marshal(changed)
	if validCloudDSHTaskValidationReviewStart(encoded, fence) {
		t.Fatal("start accepted a mismatched revision")
	}

	changed = taskValidationReviewProtocolStart(fence)
	changed["conversation_id"] = fence.Claim.ConversationID
	encoded, _ = json.Marshal(changed)
	if validCloudDSHTaskValidationReviewStart(encoded, fence) {
		t.Fatal("review DSH identity must not include conversation_id")
	}

	changed = taskValidationReviewProtocolStart(fence)
	changed["tools"] = []any{map[string]any{"name": "shell"}}
	encoded, _ = json.Marshal(changed)
	if validCloudDSHTaskValidationReviewStart(encoded, fence) {
		t.Fatal("review start accepted tools")
	}
}

func TestCloudDSHTaskValidationReviewRuntimeFramesAreBoundedAndExact(t *testing.T) {
	fence := taskValidationReviewProtocolFence()
	request := json.RawMessage(`{"tools":[],"messages":[{"role":"user","content":"review"}]}`)
	requestDigest := sha256.Sum256(request)
	payload := "checkpoint contents"
	payloadDigest := sha256.Sum256([]byte(payload))
	account := map[string]any{"server_instance_id": "server-1", "runtime_id": "runtime-1", "runtime_generation": int64(2), "runtime_sequence": int64(1)}

	frameCases := []struct {
		name string
		kind string
		body map[string]any
	}{
		{name: "process ready", kind: "task_validation_review_process_ready", body: map[string]any{"container_pid": 1234}},
		{name: "started", kind: "task_validation_review_started", body: nil},
		{name: "checkpoint", kind: "checkpoint", body: map[string]any{"id": "checkpoint-1", "from_seq": int64(0), "through_seq": int64(1), "payload": payload, "sha256": hex.EncodeToString(payloadDigest[:])}},
		{name: "checkpoint read", kind: "checkpoint_read", body: map[string]any{"id": "read-1", "from_seq": int64(0)}},
		{name: "tool-free model request", kind: "model", body: map[string]any{"id": "model-1", "through_seq": int64(1), "request": request, "sha256": hex.EncodeToString(requestDigest[:])}},
		{name: "model cancel", kind: "model_cancel", body: map[string]any{"id": "model-1"}},
		{name: "settled", kind: "task_validation_review_settled", body: map[string]any{"through_seq": int64(1), "result": "review result", "state": "completed"}},
		{name: "failed", kind: "task_validation_review_failed", body: map[string]any{"code": "runtime_unavailable", "reason": "child_exited_without_result", "process_exited": true}},
		{name: "terminated", kind: "task_validation_review_terminated", body: map[string]any{"reason": "cancelled", "state": "cancelled", "process_exited": true}},
	}
	for _, test := range frameCases {
		t.Run(test.name, func(t *testing.T) {
			frame := taskValidationReviewProtocolFrame(test.kind, fence, test.body)
			for key, value := range account {
				frame[key] = value
			}
			if !validCloudDSHTaskValidationReviewRuntimeFrame(test.kind, taskValidationReviewRawFrame(t, frame)) {
				t.Fatalf("valid runtime frame %q rejected", test.kind)
			}
		})
	}

	badModel := taskValidationReviewProtocolFrame("model", fence, map[string]any{"id": "model-2", "through_seq": int64(1),
		"request": json.RawMessage(`{"tools":[{"name":"shell"}]}`), "sha256": strings.Repeat("a", 64)})
	if validCloudDSHTaskValidationReviewEventFrame("model", taskValidationReviewRawFrame(t, badModel)) {
		t.Fatal("model request with tools was accepted")
	}

	badResult := taskValidationReviewProtocolFrame("task_validation_review_settled", fence, map[string]any{"through_seq": int64(1),
		"result": strings.Repeat("x", app.MaxTaskValidationReviewReportBytes+1), "state": "completed"})
	if validCloudDSHTaskValidationReviewEventFrame("task_validation_review_settled", taskValidationReviewRawFrame(t, badResult)) {
		t.Fatal("oversized settled result was accepted")
	}

	badIdentity := taskValidationReviewProtocolFrame("checkpoint_read", fence, map[string]any{"id": "read-2", "from_seq": int64(0)})
	badIdentity["owner_id"] = "u8765432"
	decodedIdentity, err := decodeCloudDSHTaskValidationReviewIdentity(taskValidationReviewRawFrame(t, badIdentity))
	if err != nil || cloudDSHTaskValidationReviewIdentityMatches(decodedIdentity, fence) {
		t.Fatal("frame with another owner's identity matched the active fence")
	}

	unknown := taskValidationReviewProtocolFrame("checkpoint_read", fence, map[string]any{"id": "read-3", "from_seq": int64(0), "extra": true})
	if validCloudDSHTaskValidationReviewEventFrame("checkpoint_read", taskValidationReviewRawFrame(t, unknown)) {
		t.Fatal("frame with unknown fields was accepted")
	}
}

func TestCloudDSHTaskValidationReviewResponsesMatchFenceAndShape(t *testing.T) {
	fence := taskValidationReviewProtocolFence()
	identity := taskValidationReviewProtocolIdentity(cloudDSHTaskValidationReviewIdentityForFence(fence))
	identity["type"] = "model_chunk"
	identity["id"] = "model-1"
	identity["index"] = int64(0)
	identity["chunk"] = map[string]any{"type": "text-delta", "delta": "review"}
	if !validCloudDSHTaskValidationReviewResponseFrame("model_chunk", taskValidationReviewRawFrame(t, identity), fence) {
		t.Fatal("valid model response chunk rejected")
	}

	identity["owner_id"] = "u8765432"
	if validCloudDSHTaskValidationReviewResponseFrame("model_chunk", taskValidationReviewRawFrame(t, identity), fence) {
		t.Fatal("response with mismatched owner accepted")
	}

	cancel := taskValidationReviewProtocolFrame("task_validation_review_cancel", fence, nil)
	if !validCloudDSHTaskValidationReviewResponseFrame("task_validation_review_cancel", taskValidationReviewRawFrame(t, cancel), fence) {
		t.Fatal("valid cancel frame rejected")
	}
}

func TestCloudDSHTaskValidationReviewRouteRejectsStaleRuntimeSequence(t *testing.T) {
	fence := taskValidationReviewProtocolFence()
	identity := cloudDSHTaskValidationReviewIdentityForFence(fence)
	review := &cloudDSHTaskValidationReview{fence: fence, identity: identity, sequence: 2}
	session := &cloudDSHAccountRuntimeSession{reviews: map[string]*cloudDSHTaskValidationReview{fence.SessionID: review}}
	frame := taskValidationReviewProtocolFrame("task_validation_review_process_ready", fence, map[string]any{"container_pid": 1234})
	frame["server_instance_id"] = "server-1"
	frame["runtime_id"] = "runtime-1"
	frame["runtime_generation"] = int64(2)
	frame["runtime_sequence"] = int64(1)
	if err := session.routeTaskValidationReviewFrame("task_validation_review_process_ready", taskValidationReviewRawFrame(t, frame)); err == nil {
		t.Fatal("process-ready frame with stale runtime sequence was accepted")
	}

	frame["runtime_sequence"] = int64(2)
	frame["claim_generation"] = fence.Claim.ClaimGeneration + 1
	if err := session.routeTaskValidationReviewFrame("task_validation_review_process_ready", taskValidationReviewRawFrame(t, frame)); err == nil {
		t.Fatal("process-ready frame with a stale claim fence was accepted")
	}
}
