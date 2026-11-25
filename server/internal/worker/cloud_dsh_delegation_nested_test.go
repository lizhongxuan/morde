package worker

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func TestCloudDSHChildDelegateExceedsDepthLimit(t *testing.T) {
	// E12: the default subagent depth is 1 layer (direct children of the
	// root only). A child agent asking for its own child is rejected with
	// an explicit depth error and no child is created, even though the
	// child's stored state still carries canDelegate from before the limit.
	const arguments = `{"title":"Inspect","objective":"Read the project","tool_names":["file_read"]}`
	fence, root, frame, value := cloudDSHChildCreateFixture(t, arguments, false)
	identity := root.identity
	identity.SessionID = "child_parent"
	parent := newCloudDSHSessionState(fence, identity, fence.AgentSessionID, "root_spawn",
		[]app.AgentToolDefinition{cloudDSHReadTool()}, true, false, root.through, root.checkpointDigest)
	parent.rootCheckpointThrough = 1
	parent.history.calls[frame.CallID] = root.history.calls[frame.CallID]
	frame.cloudDSHIdentity = identity
	children := &cloudDSHTestChildSessions{}
	tools := &cloudDSHTestTools{admissionStatus: app.AgentToolCallAdmitted}
	executor := &CloudDSHExecutor{options: CloudDSHExecutorOptions{Tools: tools, Children: children}}
	states := map[string]*cloudDSHSessionState{identity.SessionID: parent}
	writer := &cloudDSHFrameBuffer{}

	superseded, err := executor.createCloudDSHChildSession(context.Background(), value, fence, frame, states,
		[]app.AgentToolDefinition{cloudDSHReadTool()}, writer, nil, nil)
	if err == nil || superseded || !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("nested delegate = superseded %t, err %v, want an explicit depth-limit error", superseded, err)
	}
	if children.creates != 0 {
		t.Fatalf("depth-limited child was created: %d", children.creates)
	}
	if len(tools.admissionCalls) != 0 {
		t.Fatalf("depth-limited delegation reached admission: %+v", tools.admissionCalls)
	}

	// The limit is configurable: with MaxSubagentDepth 2 the same nested
	// delegation is admitted again, and the grandchild itself cannot
	// delegate further.
	executor = &CloudDSHExecutor{options: CloudDSHExecutorOptions{Tools: tools, Children: children, MaxSubagentDepth: 2}}
	superseded, err = executor.createCloudDSHChildSession(context.Background(), value, fence, frame, states,
		[]app.AgentToolDefinition{cloudDSHReadTool()}, writer, nil, nil)
	if err != nil || superseded {
		t.Fatalf("nested delegate at depth 2 = superseded %t, err %v", superseded, err)
	}
	if children.createCommand.ParentSessionID != identity.SessionID || children.createCommand.SpawnKey != frame.CallID ||
		children.createCommand.RootCheckpointThroughSequence != 1 {
		t.Fatalf("nested delegation lost its parent or root baseline: %+v", children.createCommand)
	}
	if len(tools.admissionCalls) != 1 || tools.admissionCalls[0].CallID != cloudDSHNamespacedToolCallID(value.ID, identity.SessionID, frame.CallID) {
		t.Fatalf("nested delegation admission was not namespaced: %+v", tools.admissionCalls)
	}
	child := states[children.child.SessionID]
	if child == nil || child.parentSessionID != identity.SessionID || child.canDelegate ||
		len(child.definitions) != 1 || child.definitions[0].Name != "file_read" {
		t.Fatalf("grandchild inherited unexpected capabilities: %+v", child)
	}
}

func TestCloudDSHNestedDelegateRejectsSiblingToolsAndThirdLevel(t *testing.T) {
	const arguments = `{"title":"Inspect","objective":"Read the project","tool_names":["file_read"]}`
	fence, root, frame, value := cloudDSHChildCreateFixture(t, arguments, false)
	identity := root.identity
	identity.SessionID = "child_parent"
	parent := newCloudDSHSessionState(fence, identity, fence.AgentSessionID, "root_spawn", nil,
		true, false, root.through, root.checkpointDigest)
	parent.history.calls[frame.CallID] = root.history.calls[frame.CallID]
	frame.cloudDSHIdentity = identity
	children := &cloudDSHTestChildSessions{}
	executor := &CloudDSHExecutor{options: CloudDSHExecutorOptions{Tools: &cloudDSHTestTools{}, Children: children}}
	states := map[string]*cloudDSHSessionState{identity.SessionID: parent}

	if _, err := executor.createCloudDSHChildSession(context.Background(), value, fence, frame, states,
		[]app.AgentToolDefinition{cloudDSHReadTool()}, &cloudDSHFrameBuffer{}, nil, nil); err == nil {
		t.Fatal("nested delegate received a tool available only to the root")
	}
	if children.creates != 0 {
		t.Fatalf("unauthorized child was created: %d", children.creates)
	}
	parent.parentSessionID = "another_child"
	parent.definitions = []app.AgentToolDefinition{cloudDSHReadTool()}
	if _, err := executor.createCloudDSHChildSession(context.Background(), value, fence, frame, states,
		parent.definitions, &cloudDSHFrameBuffer{}, nil, nil); err == nil {
		t.Fatal("third-level delegation was accepted")
	}
}

func TestCloudDSHChildDelegationToolsDoNotExpandStoredSelection(t *testing.T) {
	ordinary := []app.AgentToolDefinition{cloudDSHReadTool()}
	definitions, err := cloudDSHChildDelegationTools(ordinary, true)
	if err != nil || len(definitions) != 6 || !cloudDSHToolAllowed(definitions, cloudDSHDelegateToolName) {
		t.Fatalf("coordinator tools = %v, err %v", definitions, err)
	}
	for _, name := range []string{cloudDSHDelegateToolName, cloudDSHJobListToolName, cloudDSHResultEvidenceToolName} {
		arguments, err := json.Marshal(map[string]any{"title": "x", "objective": "y", "tool_names": []string{name}})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := validateCloudDSHDelegateArguments(arguments, definitions); err == nil {
			t.Fatalf("reserved control tool %q was delegated as an ordinary tool", name)
		}
	}
	if len(ordinary) != 1 || ordinary[0].Name != "file_read" {
		t.Fatal("adding coordinator controls mutated the parent tool selection")
	}
	leaf, err := cloudDSHChildDelegationTools(ordinary, false)
	if err != nil || len(leaf) != 1 || cloudDSHToolAllowed(leaf, cloudDSHDelegateToolName) {
		t.Fatalf("grandchild received unexpected definitions: %v, %v", leaf, err)
	}
}

func TestCloudDSHChildModelFailuresAreClassifiedWithoutProviderText(t *testing.T) {
	rateLimit := domain.NewError(domain.CodeUpstreamError, "provider response contained private text")
	rateLimit.Details["stage"] = "provider_status"
	rateLimit.Details["provider_code"] = "rate_limit_exceeded"
	if code, ok := cloudDSHChildModelFailureCode(rateLimit); !ok || code != "MODEL_PROVIDER_RATE_LIMITED" {
		t.Fatalf("provider rate limit classification = %q, %t", code, ok)
	}

	budget := domain.NewError(domain.CodeQuotaExceeded, "model token budget exceeded")
	budget.Details["reason"] = "task_model_budget_exhausted"
	if code, ok := cloudDSHChildModelFailureCode(budget); !ok || code != "MODEL_TASK_BUDGET_EXHAUSTED" {
		t.Fatalf("task budget classification = %q, %t", code, ok)
	}

	if code, ok := cloudDSHChildModelFailureCode(domain.NewError(domain.CodeForbidden, "authorization fence changed")); ok || code != "" {
		t.Fatalf("authorization failure was incorrectly isolated to a child: %q, %t", code, ok)
	}
	if summary := cloudDSHModelFailureSummary("MODEL_PROVIDER_RATE_LIMITED"); summary == "" ||
		strings.Contains(summary, "private text") || !strings.Contains(summary, "rate limited") {
		t.Fatalf("failure summary was not sanitized or actionable: %q", summary)
	}
}
