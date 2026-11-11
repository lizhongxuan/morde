package app

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
	"time"
)

func taskModelResponseFixture(t *testing.T) (*Sealer, ModelCallReservation) {
	t.Helper()
	fence := &ModelTaskFence{TaskID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", LeaseOwner: "worker-1", LeaseVersion: 1,
		AgentInstanceID: "agent-1", AgentGeneration: 1, AgentSessionID: "session-1", AgentRootSessionID: "root-session-1",
		AuthorizationVersion: 1, ThroughSequence: 0, EngineVersion: "dsh-1", ProfileVersion: "v1"}
	callID, err := DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("request"))
	reservation := ModelCallReservation{Source: ModelCallTask, Task: fence, OwnerID: fence.InitiatorID, ModelID: "p1234567",
		ProviderModel: "test-model", ConnectionVersion: time.Unix(1, 0), CallID: callID, RequestDigest: digest[:]}
	sealer, err := NewSealer(bytes.Repeat([]byte{0x5a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return sealer, reservation
}

func TestTaskModelResponseSealOpenAndRejectsChangedFence(t *testing.T) {
	sealer, reservation := taskModelResponseFixture(t)
	want := ModelResponse{Reasoning: "private reasoning", ToolCalls: []ModelToolCall{{CallID: "tool-1", Name: "lookup", Arguments: `{"query":"x"}`}},
		Usage: ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2}, StopReason: "tool_calls"}
	record, err := SealTaskModelResponse(sealer, reservation, want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenTaskModelResponse(sealer, record, reservation)
	if err != nil || got.Reasoning != want.Reasoning || got.Text != want.Text || got.StopReason != want.StopReason ||
		got.Usage != want.Usage || len(got.ToolCalls) != 1 || got.ToolCalls[0] != want.ToolCalls[0] {
		t.Fatalf("response round trip = %#v, err=%v", got, err)
	}

	changed := reservation
	changed.Task = normalizedModelTaskFence(reservation.Task)
	changed.Task.ThroughSequence++
	changed.CallID, err = DSHModelCallID(changed.Task.TaskID, changed.Task.AgentSessionID, changed.Task.ThroughSequence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTaskModelResponse(sealer, record, changed); err == nil {
		t.Fatal("response opened for a different checkpoint")
	}
	wrongSource := reservation
	wrongSource.Source = ModelCallClient
	if _, err := SealTaskModelResponse(sealer, wrongSource, want); err == nil {
		t.Fatal("Task response was sealed for a non-Task source")
	}
	oversized := ModelResponse{Text: strings.Repeat("x", MaxDSHModelResponseTextBytes+1), StopReason: "stop",
		Usage: ModelUsage{Known: true, InputTokens: 1, OutputTokens: 1}}
	if _, err := SealTaskModelResponse(sealer, reservation, oversized); err == nil {
		t.Fatal("oversized response was sealed")
	}

	tampered := record
	tampered.Ciphertext = append([]byte(nil), record.Ciphertext...)
	tampered.Ciphertext[len(tampered.Ciphertext)-1] ^= 1
	if _, err := OpenTaskModelResponse(sealer, tampered, reservation); err == nil {
		t.Fatal("tampered response opened")
	}

	changedGeneration := reservation
	changedGeneration.Task = normalizedModelTaskFence(reservation.Task)
	changedGeneration.Task.AgentInstanceID = "agent-2"
	changedGeneration.Task.AgentGeneration = 2
	if _, err := OpenTaskModelResponse(sealer, record, changedGeneration); err != nil {
		t.Fatalf("valid successor generation could not open prior response: %v", err)
	}
}
