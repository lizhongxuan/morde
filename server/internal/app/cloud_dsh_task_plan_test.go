package app

import (
	"encoding/json"
	"testing"

	"morde/server/internal/domain/task"
)

func TestCloudDSHTaskPlanDefinitionAndEventFence(t *testing.T) {
	if _, err := validateAgentTools([]AgentToolDefinition{cloudDSHTaskPlanDefinition()}); err != nil {
		t.Fatalf("task plan tool definition is invalid: %v", err)
	}
	plan := task.Plan{TaskRevision: 2, Revision: 1, Goal: "Prepare report", CostEstimateState: "unknown", Steps: []task.PlanStep{
		{ID: "sources", Title: "Collect sources", Description: "", DependsOn: []string{}, Outputs: []string{"source notes"}, CompletionCriteria: []string{"sources are cited"}, Status: task.PlanStepPending},
	}}
	commit := CloudDSHTaskPlanCommit{TaskID: "q1234567", OwnerID: "u1234567", AgentSessionID: "session-plan",
		AuthorizationVersion: 3, CallID: "call-plan", Plan: plan}
	commit.FingerprintSHA256 = cloudDSHTaskPlanFingerprint(commit)
	event := cloudDSHTaskPlanEvent{StepID: "s1234567", Sequence: 4, TaskRevision: plan.TaskRevision, PlanRevision: plan.Revision, Plan: plan}
	event.Detail.Tool, event.Detail.CallID = cloudDSHTaskPlanToolName, commit.CallID
	event.Detail.AgentSessionID, event.Detail.AuthorizationVersion, event.Detail.FingerprintSHA256 = commit.AgentSessionID, commit.AuthorizationVersion, commit.FingerprintSHA256
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCloudDSHTaskPlanEvent(payload, commit); err != nil {
		t.Fatalf("valid plan event rejected: %v", err)
	}
	plan.Goal = "Different goal"
	commit.Plan = plan
	commit.FingerprintSHA256 = cloudDSHTaskPlanFingerprint(commit)
	if err := validateCloudDSHTaskPlanEvent(payload, commit); err == nil {
		t.Fatal("plan event accepted a changed plan fingerprint")
	}
}
