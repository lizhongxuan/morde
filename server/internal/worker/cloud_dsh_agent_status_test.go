package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCloudDSHAgentStatusFrameRequiresExactFields(t *testing.T) {
	valid := `{"type":"agent_status","task_id":"q1234567","owner_id":"u1234567","agent_id":"agent_1","generation":2,"authorization_version":1,"session_id":"session_1","task_revision":4,"status":"running","observation_sequence":1}`
	frame, err := decodeCloudDSHFrame([]byte(valid))
	if err != nil || frame.Status != "running" || frame.TaskRevision == nil || *frame.TaskRevision != 4 || frame.ObservationSequence == nil || *frame.ObservationSequence != 1 {
		t.Fatalf("valid status frame = %#v, %v", frame, err)
	}
	for _, raw := range []string{
		`{"type":"agent_status","task_id":"q1234567","owner_id":"u1234567","agent_id":"agent_1","generation":2,"authorization_version":1,"session_id":"session_1","status":"running","observation_sequence":1}`,
		`{"type":"agent_status","task_id":"q1234567","owner_id":"u1234567","agent_id":"agent_1","generation":2,"authorization_version":1,"session_id":"session_1","task_revision":4,"status":"running","observation_sequence":1,"extra":true}`,
		`{"type":"agent_status","task_id":"q1234567","owner_id":"u1234567","agent_id":"agent_1","generation":2,"authorization_version":1,"session_id":"session_1","task_revision":4,"status":"running","observation_sequence":1.5}`,
		`{"type":"agent_status","task_id":"q1234567","owner_id":"u1234567","agent_id":"agent_1","generation":2,"authorization_version":1,"session_id":"session_1","task_revision":-1,"status":"running","observation_sequence":1}`,
		`{"type":"agent_status","task_id":"q1234567","owner_id":"u1234567","agent_id":"agent_1","generation":2,"authorization_version":1,"session_id":"session_1","task_revision":4,"status":"unknown","observation_sequence":1}`,
		`{"type":"ready","version":1,"engine":"dsh","engine_version":"0.2.0-rc.2","observation_sequence":1}`,
	} {
		if _, err := decodeCloudDSHFrame([]byte(raw)); err == nil {
			t.Fatalf("invalid status frame accepted: %s", raw)
		}
	}
}

func TestCloudDSHAgentActivityFramesRequireExactFields(t *testing.T) {
	identity := `"task_id":"q1234567","owner_id":"u1234567","agent_id":"agent_1","generation":2,"authorization_version":1,"session_id":"session_1"`
	validActivity := `{"type":"agent_activity",` + identity + `,"task_revision":4,"observation_sequence":9,"phase":"tool","state":"waiting","activity_id":"call_123"}`
	frame, err := decodeCloudDSHFrame([]byte(validActivity))
	if err != nil || frame.TaskRevision == nil || *frame.TaskRevision != 4 || frame.State != "waiting" || frame.Phase != "tool" || frame.ActivityID != "call_123" {
		t.Fatalf("valid activity frame = %#v, %v", frame, err)
	}
	validHeartbeat := `{"type":"agent_heartbeat",` + identity + `,"task_revision":4,"observation_sequence":10,"phase":"model","activity_id":"mdl_123"}`
	if frame, err = decodeCloudDSHFrame([]byte(validHeartbeat)); err != nil || frame.Type != "agent_heartbeat" || frame.TaskRevision == nil {
		t.Fatalf("valid heartbeat frame = %#v, %v", frame, err)
	}
	for _, raw := range []string{
		`{"type":"agent_activity",` + identity + `,"observation_sequence":9,"phase":"tool","state":"started","activity_id":"call_123"}`,
		`{"type":"agent_activity",` + identity + `,"task_revision":4,"observation_sequence":9,"phase":"unknown","state":"started","activity_id":"call_123"}`,
		`{"type":"agent_activity",` + identity + `,"task_revision":4,"observation_sequence":9,"phase":"tool","state":"unknown","activity_id":"call_123"}`,
		`{"type":"agent_activity",` + identity + `,"task_revision":4,"observation_sequence":9,"phase":"tool","state":"started","activity_id":"call_123","extra":true}`,
		`{"type":"agent_activity",` + identity + `,"task_revision":4,"observation_sequence":9.5,"phase":"tool","state":"started","activity_id":"call_123"}`,
		`{"type":"agent_heartbeat",` + identity + `,"task_revision":4,"observation_sequence":10,"phase":"model"}`,
		`{"type":"agent_heartbeat",` + identity + `,"task_revision":4,"observation_sequence":10,"phase":"unknown","activity_id":"mdl_123"}`,
		`{"type":"agent_heartbeat",` + identity + `,"task_revision":4,"observation_sequence":10,"phase":"","activity_id":"mdl_123"}`,
		`{"type":"agent_heartbeat",` + identity + `,"task_revision":4,"observation_sequence":10,"phase":"model","activity_id":""}`,
		`{"type":"agent_heartbeat",` + identity + `,"task_revision":4,"observation_sequence":10,"phase":null,"activity_id":null}`,
		`{"type":"agent_activity",` + identity + `,"task_revision":4,"observation_sequence":9,"phase":null,"state":"started","activity_id":"call_123"}`,
	} {
		if _, err := decodeCloudDSHFrame([]byte(raw)); err == nil {
			t.Fatalf("invalid activity frame accepted: %s", raw)
		}
	}
}

func TestCloudDSHExecutorRejectsInvalidAgentStatus(t *testing.T) {
	cases := []struct {
		name     string
		frames   string
		accepted []int64
	}{
		{name: "invalid status", frames: `[{status:'completed',observation_sequence:1}]`},
		{name: "zero sequence", frames: `[{status:'idle',observation_sequence:0}]`},
		{name: "null sequence", frames: `[{status:'idle',observation_sequence:null}]`},
		{name: "unsafe sequence", frames: `[{status:'idle',observation_sequence:9007199254740992}]`},
		{name: "child session", frames: `[{status:'idle',observation_sequence:1,session_id:'child_1'}]`},
		{name: "wrong task revision", frames: `[{status:'idle',observation_sequence:1,task_revision:1}]`},
		{name: "duplicate and out of order", frames: `[{status:'idle',observation_sequence:1},
{status:'running',observation_sequence:3},{status:'idle',observation_sequence:2},
{status:'idle',observation_sequence:3},{status:'unknown',observation_sequence:4}]`, accepted: []int64{1, 3}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			executor, supervisor, _, _, _, value := cloudDSHFixture(t)
			recorder := &cloudDSHTestActivityRecorder{applied: true}
			executor.options.AgentActivity = recorder
			script := filepath.Join(t.TempDir(), "agent-status.mjs")
			source := fmt.Sprintf(`console.log(JSON.stringify({type:'ready',version:1,engine:'dsh',engine_version:'0.2.0-rc.2'}));
process.stdin.once('data', data => { const f = JSON.parse(data.toString());
const identity = {task_id:f.task_id,owner_id:f.owner_id,agent_id:f.agent_id,generation:f.generation,
authorization_version:f.authorization_version,session_id:f.session_id,task_revision:f.task_revision};
for (const fields of %s) console.log(JSON.stringify({type:'agent_status',...identity,...fields})); });
setInterval(()=>{},1000);`, test.frames)
			if err := os.WriteFile(script, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			cloudDSHUseTestTaskScript(t, supervisor, script)
			err := executor.ExecuteTask(context.Background(), value)
			if err == nil || errors.Is(err, ErrCloudDSHCompletionPending) {
				t.Fatalf("invalid status accepted as candidate completion: %v", err)
			}
			if len(recorder.observations) != len(test.accepted) {
				t.Fatalf("accepted observations = %#v, want %v", recorder.observations, test.accepted)
			}
			for index, sequence := range test.accepted {
				if recorder.observations[index].ObservationSequence != sequence {
					t.Fatalf("accepted observations = %#v, want %v", recorder.observations, test.accepted)
				}
			}
		})
	}
}
