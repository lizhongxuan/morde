package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
)

type cloudDSHPersonalReceiptTestTools struct {
	*cloudDSHTestTools
	version app.CloudDSHMemoryVersion
	output  string
	err     error
}

func (tools *cloudDSHPersonalReceiptTestTools) ConfirmPersonalContextWrite(context.Context, task.Task, app.AgentInstanceFence,
	int64, []byte, app.ModelToolCall) (app.CloudDSHMemoryVersion, string, error) {
	return tools.version, tools.output, tools.err
}

func TestCloudDSHRecoveryUsesOnlyMatchingPersonalWriteReceipt(t *testing.T) {
	call := &cloudDSHToolCall{name: "morde_personal_object_update", callSeen: true, historical: true,
		arguments: []byte(`{"object_type":"memory","object_id":"global","base_revision":1,"body":{"content":"updated"}}`)}
	history := &cloudDSHCheckpointHistory{sessionID: "root_session", calls: map[string]*cloudDSHToolCall{"call_1": call}}
	tools := &cloudDSHPersonalReceiptTestTools{cloudDSHTestTools: &cloudDSHTestTools{},
		version: app.CloudDSHMemoryVersion{ObjectType: personal.ObjectTypeMemory, ID: "global", Revision: 2},
		output:  `{"object_type":"memory","object_id":"global","revision":2}`}
	executor := &CloudDSHExecutor{options: CloudDSHExecutorOptions{Tools: tools}}
	confirmed, err := executor.confirmCloudDSHPersonalWrites(context.Background(), task.Task{}, app.AgentInstanceFence{}, 1, make([]byte, 32), history)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]*cloudDSHSessionState{"root": {history: history}}
	versions, err := cloudDSHMemoryWritesWithReceipts(states, true, confirmed)
	if err != nil || len(versions) != 1 || versions[0] != tools.version {
		t.Fatalf("confirmed write was not restored: versions=%+v err=%v", versions, err)
	}
	call.resultSeen, call.resultText = true, tools.output
	if _, err := cloudDSHMemoryWritesWithReceipts(states, false, confirmed); err != nil {
		t.Fatalf("matching checkpointed output was rejected: %v", err)
	}
	call.resultText = `{"object_type":"memory","object_id":"global","revision":3}`
	if _, err := cloudDSHMemoryWritesWithReceipts(states, false, confirmed); err == nil {
		t.Fatal("mismatched checkpoint output was trusted")
	}
	call.resultSeen = false
	call.arguments = []byte(`{"object_type":"memory","object_id":"global","base_revision":2}`)
	if _, err := cloudDSHMemoryWritesWithReceipts(states, false, confirmed); err == nil {
		t.Fatal("different checkpointed arguments reused a receipt")
	}
	call.arguments = confirmed[history.sessionID]["call_1"].arguments
	tools.err = errors.New("receipt missing")
	if _, err := executor.confirmCloudDSHPersonalWrites(context.Background(), task.Task{}, app.AgentInstanceFence{}, 1, make([]byte, 32), history); err == nil {
		t.Fatal("missing durable receipt allowed recovery")
	}
	call.name = "morde_goal_update"
	tools.err = nil
	confirmed, err = executor.confirmCloudDSHPersonalWrites(context.Background(), task.Task{}, app.AgentInstanceFence{}, 1, make([]byte, 32), history)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cloudDSHMemoryWritesWithReceipts(states, true, confirmed); err == nil {
		t.Fatal("a personal object receipt was accepted as a goal receipt")
	}
}

func TestCloudDSHRecoveryReconcilesGoalCallsByExactArgumentsAndResult(t *testing.T) {
	cases := []struct {
		name      string
		arguments string
		output    string
		revision  int64
	}{
		{"morde_goal_create", `{"title":"synthetic","status":"active"}`, `{"id":"goal_1","revision":1,"status":"active"}`, 1},
		{"morde_goal_update", `{"goal_id":"goal_1","expected_revision":1}`, `{"id":"goal_1","revision":2,"status":"paused"}`, 2},
		{"morde_goal_delete", `{"goal_id":"goal_1","expected_revision":2}`, `{"id":"goal_1","revision":3,"deleted":true}`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := &cloudDSHToolCall{name: tc.name, callSeen: true, historical: true, arguments: []byte(tc.arguments)}
			history := &cloudDSHCheckpointHistory{sessionID: "root_session", calls: map[string]*cloudDSHToolCall{"goal_call": call}}
			tools := &cloudDSHPersonalReceiptTestTools{cloudDSHTestTools: &cloudDSHTestTools{},
				version: app.CloudDSHMemoryVersion{ObjectType: personal.ObjectTypeGoal, ID: "goal_1", Revision: tc.revision}, output: tc.output}
			executor := &CloudDSHExecutor{options: CloudDSHExecutorOptions{Tools: tools}}
			confirmed, err := executor.confirmCloudDSHPersonalWrites(context.Background(), task.Task{}, app.AgentInstanceFence{}, 1, make([]byte, 32), history)
			if err != nil {
				t.Fatal(err)
			}
			states := map[string]*cloudDSHSessionState{"root": {history: history}}
			versions, err := cloudDSHMemoryWritesWithReceipts(states, true, confirmed)
			if err != nil || len(versions) != 1 || versions[0] != tools.version {
				t.Fatalf("goal receipt was not reconciled: versions=%+v err=%v", versions, err)
			}
			call.resultSeen, call.resultText = true, `{"id":"goal_1","revision":99}`
			if _, err := cloudDSHMemoryWritesWithReceipts(states, true, confirmed); err == nil {
				t.Fatal("changed checkpoint output reused the goal receipt")
			}
			call.resultSeen = false
			call.arguments = append(call.arguments, ' ')
			if _, err := cloudDSHMemoryWritesWithReceipts(states, true, confirmed); err == nil {
				t.Fatal("changed checkpoint arguments reused the goal receipt")
			}
		})
	}
}

func TestCloudDSHPersonalProtectionRejectsMissingContext(t *testing.T) {
	executor, _, instances, _, models, value := cloudDSHFixture(t)
	tools := &cloudDSHMemoryTestTools{cloudDSHTestTools: &cloudDSHTestTools{}, instances: instances}
	executor.options.Tools = tools
	value.ConversationContext = nil
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, errCloudDSHPersonalMemoryChanged) ||
		len(models.calls) != 0 || tools.pauseCalls != 1 {
		t.Fatalf("missing personal context bypassed protection: err=%v calls=%d pauses=%d", err, len(models.calls), tools.pauseCalls)
	}
}

type cloudDSHScheduleMemoryTestTools struct{ *cloudDSHMemoryTestTools }

func (*cloudDSHScheduleMemoryTestTools) InitialContext(context.Context, task.Task) (string, error) {
	return "", nil
}

func TestCloudDSHScheduledTaskDoesNotRequirePersonalProof(t *testing.T) {
	executor, _, instances, _, models, value := cloudDSHFixture(t)
	base := &cloudDSHMemoryTestTools{cloudDSHTestTools: &cloudDSHTestTools{}, instances: instances}
	executor.options.Tools = &cloudDSHScheduleMemoryTestTools{cloudDSHMemoryTestTools: base}
	value.Authorization.Kind = string(identity.KindScheduleTask)
	value.ConversationContext = nil
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) ||
		len(models.calls) != 1 || base.pauseCalls != 0 {
		t.Fatalf("schedule was blocked by personal proof: err=%v calls=%d pauses=%d", err, len(models.calls), base.pauseCalls)
	}
}

func TestCloudDSHRecoveryStopsWhenPersonalProofProviderIsUnavailable(t *testing.T) {
	executor, supervisor, _, checkpoints, models, value := cloudDSHFixture(t)
	value.Authorization.Kind = string(identity.KindSession)
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, ErrCloudDSHCompletionPending) {
		t.Fatal(err)
	}
	previous := supervisor.launch.Fence
	tail := checkpoints.commands[len(checkpoints.commands)-1]
	value.AgentInstanceID, value.AgentGeneration, value.AgentSessionID = previous.AgentInstanceID, previous.AgentGeneration, previous.AgentSessionID
	value.AgentEngineVersion, value.AgentProfileVersion = previous.EngineVersion, previous.ProfileVersion
	value.CheckpointThroughSequence, value.CheckpointDigest = tail.Through, append([]byte(nil), tail.Digest...)
	value.Lease.Version++
	expires := time.Now().Add(time.Minute)
	value.Lease.ExpiresAt = &expires
	if err := executor.ExecuteTask(context.Background(), value); !errors.Is(err, errCloudDSHPersonalMemoryChanged) || len(models.calls) != 1 {
		t.Fatalf("unavailable personal proof allowed recovery: err=%v calls=%d", err, len(models.calls))
	}
}

func TestCloudDSHPersonalVersionsRequireConfirmedReceipts(t *testing.T) {
	state := &cloudDSHSessionState{history: &cloudDSHCheckpointHistory{calls: map[string]*cloudDSHToolCall{
		"pending": {name: "morde_personal_object_update", arguments: []byte(`{"object_type":"memory","object_id":"global","base_revision":1}`),
			resultSeen: true, resultText: `{"object_type":"memory","object_id":"global","revision":2}`},
		"profile": {name: "morde_personal_object_update", arguments: []byte(`{"object_type":"profile","object_id":"default","base_revision":1}`),
			status: app.AgentToolSucceeded, output: `{"object_type":"profile","object_id":"default","revision":2}`},
		"goal": {name: "morde_goal_update", arguments: []byte(`{"goal_id":"goal_1","expected_revision":3}`),
			status: app.AgentToolSucceeded, output: `{"id":"goal_1","revision":4}`},
		"failed": {name: "morde_personal_object_update", arguments: []byte(`{"object_type":"memory","object_id":"user","base_revision":1}`),
			status: app.AgentToolFailed, output: `{"object_type":"memory","object_id":"user","revision":2}`},
	}}}
	if _, err := cloudDSHMemoryWrites(map[string]*cloudDSHSessionState{"root": state}, true); err == nil {
		t.Fatal("a checkpointed result without a confirmed receipt must pause recovery")
	}
	delete(state.history.calls, "pending")
	versions, err := cloudDSHMemoryWrites(map[string]*cloudDSHSessionState{"root": state}, true)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]app.CloudDSHMemoryVersion)
	for _, version := range versions {
		got[version.ObjectType+":"+version.ID] = version
	}
	if len(got) != 2 || got[personal.ObjectTypeProfile+":default"].Revision != 2 || got[personal.ObjectTypeGoal+":goal_1"].Revision != 4 {
		t.Fatalf("personal versions trusted unconfirmed output or missed confirmed writes: %+v", got)
	}
	state.history.calls["profile"].output = `{"object_type":"profile","object_id":"default","revision":3}`
	if _, err := cloudDSHMemoryWrites(map[string]*cloudDSHSessionState{"root": state}, true); err == nil {
		t.Fatal("a mismatched object receipt was accepted")
	}
}
