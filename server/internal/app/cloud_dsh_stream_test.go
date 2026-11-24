package app

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
)

func TestCloudDSHStreamPersonalProofRequiresConfirmedVersions(t *testing.T) {
	proof := cloudDSHMemoryProof{Version: 2, Enabled: false, Revision: 0,
		Objects: []CloudDSHMemoryVersion{{ObjectType: personal.ObjectTypeProfile, ID: personal.ProfileObjectID, Revision: 2}}}
	encode := func(value cloudDSHMemoryProof) string {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(body)
	}
	parsed, err := DecodeCloudDSHStreamPersonalProof(encode(proof), []CloudDSHMemoryVersion{
		{ObjectType: personal.ObjectTypeProfile, ID: personal.ProfileObjectID, Revision: 3},
	})
	if err != nil || len(parsed.Objects) != 1 || parsed.Objects[0].Revision != 3 || parsed.SettingRevision != 0 || parsed.Enabled {
		t.Fatalf("confirmed personal proof = %+v err=%v", parsed, err)
	}
	proof.Items = []CloudDSHMemoryVersion{{ID: "memory_1", Revision: 1}}
	if _, err := DecodeCloudDSHStreamPersonalProof(encode(proof), nil); err == nil {
		t.Fatal("disabled memory was accepted in a stream proof")
	}
	proof.Items = nil
	proof.Objects = nil
	if _, err := DecodeCloudDSHStreamPersonalProof(encode(proof), nil); err == nil {
		t.Fatal("missing personal object proof was accepted")
	}
}

func TestCloudDSHStreamPersonalProofBindsApprovedRuleToItsMemoryRevision(t *testing.T) {
	proof := cloudDSHMemoryProof{Version: 2, Enabled: true, Revision: 1,
		Items: []CloudDSHMemoryVersion{{ID: "global", Revision: 4}},
		Objects: []CloudDSHMemoryVersion{{ObjectType: personal.ObjectTypeRuleCandidate, ID: "r_abcdef01", Revision: 2,
			SourceMemoryID: "global", SourceMemoryRevision: 4}}}
	body, err := json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := DecodeCloudDSHStreamPersonalProof(base64.RawURLEncoding.EncodeToString(body), nil)
	if err != nil || len(parsed.Objects) != 2 {
		t.Fatalf("approved rule proof = %+v, %v", parsed, err)
	}
	proof.Objects[0].SourceMemoryRevision = 3
	body, err = json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCloudDSHStreamPersonalProof(base64.RawURLEncoding.EncodeToString(body), nil); err == nil {
		t.Fatal("rule proof with a mismatched source memory revision was accepted")
	}
}

func TestCloudDSHStreamResultReusesOnlyMatchingRevisionAndPrefix(t *testing.T) {
	current := int64(2)
	snapshotRevision := int64(2)
	details := TaskDetails{Task: task.Task{CurrentRevision: &current}, AssistantMessage: &AssistantMessageSnapshot{
		Message: conversation.Message{ID: "m1234567", Content: "Hello "}, Status: AssistantMessageStreaming, TaskRevision: &snapshotRevision,
	}}
	completion := CloudDSHTaskCompletion{Result: "Hello world", StreamMessageID: "m1234567", StreamTurnID: "s1234567"}
	step := task.Step{ID: "s7654321"}
	useCloudDSHStreamResultStep(&step, details, completion)
	if step.ID != completion.StreamTurnID {
		t.Fatalf("matching stream did not retain its turn: %s", step.ID)
	}
	for _, test := range []struct {
		name   string
		change func(*TaskDetails, *CloudDSHTaskCompletion)
	}{
		{"different message", func(_ *TaskDetails, c *CloudDSHTaskCompletion) { c.StreamMessageID = "m7654321" }},
		{"different result", func(_ *TaskDetails, c *CloudDSHTaskCompletion) { c.Result = "Another answer" }},
		{"different revision", func(d *TaskDetails, _ *CloudDSHTaskCompletion) { d.Task.CurrentRevision = new(int64) }},
		{"completed snapshot", func(d *TaskDetails, _ *CloudDSHTaskCompletion) { d.AssistantMessage.Status = AssistantMessageCompleted }},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := details
			snapshot := *details.AssistantMessage
			d.AssistantMessage = &snapshot
			c := completion
			test.change(&d, &c)
			candidate := task.Step{ID: "s7654321"}
			useCloudDSHStreamResultStep(&candidate, d, c)
			if candidate.ID != "s7654321" {
				t.Fatal("unmatched stream identity was reused")
			}
		})
	}
}
