package worker

import (
	"encoding/json"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestCloudDSHReadOnlyRecoveryRequiresExactRootCheckpointAndToolset(t *testing.T) {
	revision := int64(1)
	value := task.Task{ID: "q1234567", OwnerID: "u1234567", InitiatorID: "u1234567", AgentInstanceID: "agent_123",
		AgentGeneration: 2, AgentSessionID: "session_123", AgentEngineVersion: "engine", AgentProfileVersion: "profile",
		CurrentRevision: &revision, DesiredRevision: &revision,
		Lease: task.Lease{Owner: "worker-1", Version: 4}, Authorization: task.Authorization{Version: 4}}
	fence := &app.ModelTaskFence{TaskID: value.ID, OwnerID: value.OwnerID, InitiatorID: value.OwnerID,
		LeaseOwner: value.Lease.Owner, LeaseVersion: value.Lease.Version, AgentInstanceID: value.AgentInstanceID,
		AgentGeneration: value.AgentGeneration, AgentSessionID: value.AgentSessionID, AgentRootSessionID: value.AgentSessionID,
		AuthorizationVersion: value.Authorization.Version, ThroughSequence: 5, EngineVersion: value.AgentEngineVersion,
		ProfileVersion: value.AgentProfileVersion}
	call := app.ModelCall{Source: app.ModelCallTask, Task: fence}
	definitions := []app.AgentToolDefinition{recoveryTestTool("morde_web_search")}
	state := &cloudDSHSessionState{identity: cloudDSHIdentity{TaskID: value.ID, OwnerID: value.OwnerID, AgentID: value.AgentInstanceID,
		Generation: value.AgentGeneration, SessionID: value.AgentSessionID, AuthorizationVersion: value.Authorization.Version},
		definitions: definitions, through: 5, checkpointDigest: make([]byte, 32), restoreDigest: make([]byte, 32),
		restoreThrough: 5, readThrough: 5, resume: true, history: newCloudDSHCheckpointHistory(value.AgentSessionID),
		toolSelectionVerified: true}
	state.history.nextSeq = 6

	candidate, ok := cloudDSHReadOnlyRecoveryCheckpointFor(value, state, call, false)
	if !ok || candidate.ThroughSequence != 5 || len(candidate.Digest) != 32 || len(candidate.ToolsetDigest) != 32 {
		t.Fatalf("valid read-only checkpoint = %+v, %t", candidate, ok)
	}

	cases := []struct {
		name   string
		change func(*cloudDSHSessionState, *app.ModelCall)
	}{
		{name: "write tool", change: func(state *cloudDSHSessionState, _ *app.ModelCall) {
			state.definitions = append(state.definitions, recoveryTestTool("morde_workspace_write"))
		}},
		{name: "delegation enabled", change: func(state *cloudDSHSessionState, _ *app.ModelCall) { state.canDelegate = true }},
		{name: "child session", change: func(_ *cloudDSHSessionState, _ *app.ModelCall) {}},
		{name: "unverified tool history", change: func(state *cloudDSHSessionState, _ *app.ModelCall) {
			state.history.calls["call_1"] = &cloudDSHToolCall{}
		}},
		{name: "sequence mismatch", change: func(_ *cloudDSHSessionState, modelCall *app.ModelCall) {
			modelCall.Task.ThroughSequence++
		}},
		{name: "session mismatch", change: func(state *cloudDSHSessionState, _ *app.ModelCall) {
			state.parentSessionID = "parent_123"
		}},
		{name: "tool selection unverified", change: func(state *cloudDSHSessionState, _ *app.ModelCall) {
			state.toolSelectionVerified = false
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			candidateState := *state
			candidateState.definitions = append([]app.AgentToolDefinition(nil), state.definitions...)
			candidateState.history = state.history.clone()
			candidateCall := call
			if call.Task != nil {
				fenceCopy := *call.Task
				candidateCall.Task = &fenceCopy
			}
			test.change(&candidateState, &candidateCall)
			_, accepted := cloudDSHReadOnlyRecoveryCheckpointFor(value, &candidateState, candidateCall, test.name == "child session")
			if accepted {
				t.Fatal("unsafe or stale recovery candidate was accepted")
			}
		})
	}
}

func recoveryTestTool(name string) app.AgentToolDefinition {
	return app.AgentToolDefinition{ModelTool: app.ModelTool{Name: name, Description: "read-only test tool",
		Parameters: json.RawMessage(`{"type":"object","properties":{},"required":[],"additionalProperties":false}`)}}
}

func TestCloudDSHReadOnlyRecoveryToolsetUsesClosedAllowlist(t *testing.T) {
	for _, name := range []string{"morde_web_search", "morde_web_read"} {
		if !cloudDSHReadOnlyRecoveryToolset([]app.AgentToolDefinition{recoveryTestTool(name)}) {
			t.Errorf("read-only tool %q was rejected", name)
		}
	}
	for _, name := range []string{"ask_user", "dsh_delegate", "morde_workspace_read", "morde_workspace_write", "morde_mcp_call",
		"morde_current_conversation_history_search", "morde_task_plan_update", "unknown_tool"} {
		if cloudDSHReadOnlyRecoveryToolset([]app.AgentToolDefinition{recoveryTestTool(name)}) {
			t.Errorf("tool %q passed the read-only allowlist", name)
		}
	}
}

func TestCloudDSHModelRecoveryStartRequiresExactAttemptAndConfirmedResponse(t *testing.T) {
	const failedCallID = "mdl_failed_123"
	const recoveryCallID = "mrc_recovery_123"
	for _, test := range []struct {
		name                  string
		state                 task.ModelRecoveryState
		callID                string
		started, succeeded    bool
		wantBegin, wantReplay bool
		wantAllowed           bool
	}{
		{name: "scheduled exact frame begins once", state: task.ModelRecoveryScheduled, callID: failedCallID, wantBegin: true, wantAllowed: true},
		{name: "scheduled new call cannot bypass begin", state: task.ModelRecoveryScheduled, callID: "mdl_next_123"},
		{name: "scheduled duplicate before response is denied", state: task.ModelRecoveryScheduled, callID: failedCallID, started: true},
		{name: "scheduled continuation after response", state: task.ModelRecoveryScheduled, callID: "mdl_next_123", started: true, succeeded: true, wantAllowed: true},
		{name: "attempting exact frame only replays", state: task.ModelRecoveryAttempting, callID: failedCallID, wantReplay: true, wantAllowed: true},
		{name: "attempting new steer call is denied", state: task.ModelRecoveryAttempting, callID: "mdl_steered_123"},
		{name: "attempting replay duplicate before response is denied", state: task.ModelRecoveryAttempting, callID: failedCallID, started: true},
		{name: "replayed response permits continuation", state: task.ModelRecoveryAttempting, callID: "mdl_next_123", started: true, succeeded: true, wantAllowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			begin, replay, allowed := cloudDSHModelRecoveryStartAllowed(test.state, test.callID, failedCallID, recoveryCallID,
				test.started, test.succeeded)
			if begin != test.wantBegin || replay != test.wantReplay || allowed != test.wantAllowed {
				t.Fatalf("start decision = begin:%t replay:%t allowed:%t", begin, replay, allowed)
			}
		})
	}
}
