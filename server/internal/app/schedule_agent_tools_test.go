package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

func TestScheduleAgentCreateRechecksApprovalRequestHashBeforeWriting(t *testing.T) {
	runtime := newRuntimeToolExecutorFixture(t, nil)
	store := &scheduleStoreStub{}
	schedules, err := NewScheduleService(store, &scheduleAuthorizerStub{}, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewScheduleAgentToolExecutor(runtime.executor, schedules)
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]any{
		"host_id": "h1234567", "model_id": "p1234567", "objective": "check project changes",
		"timing":      map[string]any{"kind": "once", "run_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339), "time_zone": "UTC"},
		"observation": map[string]any{"url": "https://example.test/status", "selector": "#status", "field": "text"},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := AgentToolRequest{
		Task: runtime.store.details.Task, Call: ModelToolCall{CallID: "schedule-call", Name: agentScheduleCreateTool, Arguments: string(arguments)},
		Arguments: arguments, OperationKey: "agent:schedule-test", RequiredScope: identity.ScopeScheduleManage, Fence: runtime.fence,
	}
	hash, err := agentOperationHash(request.Call.Name, arguments)
	if err != nil {
		t.Fatal(err)
	}
	request.Approval = &AgentToolApprovalDecision{PendingID: "r1234567", Version: 1, Decision: task.PendingDecisionApprove, RequestHash: append([]byte(nil), hash[:]...)}
	preparation, err := executor.Prepare(context.Background(), request)
	if err != nil || preparation.Status != AgentToolReady {
		t.Fatalf("Prepare() = %#v, %v", preparation, err)
	}
	request.Fence = preparation.Fence
	request.OperationID = "o1234567"
	runtime.store.operation = task.Operation{
		ID: request.OperationID, TaskID: request.Task.ID, Key: request.OperationKey, RequestHash: append([]byte(nil), hash[:]...),
		HostID: request.Task.HostID, RuntimeGeneration: preparation.Fence.RuntimeGeneration,
		RuntimeInstanceID: preparation.Fence.RuntimeInstanceID, ControlVersion: preparation.Fence.ControlVersion,
		LeaseVersion: request.Task.Lease.Version, RuntimeLeaseVersion: request.Task.Lease.Version, Status: task.OperationDispatched,
	}
	request.Approval.RequestHash = []byte("different approved request")
	if _, err := executor.Execute(context.Background(), request, preparation); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("Execute() with a changed approval hash error = %v", err)
	}
	if store.createCalls != 0 {
		t.Fatalf("schedule was created %d times after approval mismatch", store.createCalls)
	}
}

func TestScheduleAgentUpdateRequiresApprovalAndResetsChangedObservation(t *testing.T) {
	runtime := newRuntimeToolExecutorFixture(t, nil)
	old := &schedule.ObservationSpec{URL: "https://example.test/status", Selector: "#status", Field: schedule.ObservationText}
	store := &scheduleStoreStub{stored: schedule.Schedule{
		ID: "j1234567", OwnerID: runtime.store.details.Task.OwnerID, Version: 3, Status: schedule.StatusEnabled,
		HostID: runtime.store.details.Task.HostID, ModelID: "model-a", Objective: "check project changes",
		Timing: schedule.Timing{Kind: schedule.TimingOnce, RunAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), TimeZone: "UTC"},
		Budget: task.Budget{ActiveSeconds: 60, ToolCalls: 2, ModelTokens: 1000}, Authorization: task.Authorization{Kind: string(identity.KindSession)},
		MisfirePolicy: schedule.MisfireSkip, NotificationEvents: []schedule.NotificationEvent{schedule.NotifyCompleted},
		Observation: old, ObservationConfigHash: old.ConfigHash(), BaselineFingerprint: strings.Repeat("a", 64),
	}}
	schedules, err := NewScheduleService(store, &scheduleAuthorizerStub{}, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewScheduleAgentToolExecutor(runtime.executor, schedules)
	if err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(map[string]any{"schedule_id": "j1234567", "expected_version": 3,
		"observation": map[string]any{"url": "https://example.test/status", "selector": ".status", "field": "value"}})
	if err != nil {
		t.Fatal(err)
	}
	request := AgentToolRequest{
		Task: runtime.store.details.Task, Call: ModelToolCall{CallID: "schedule-update-call", Name: agentScheduleUpdateTool, Arguments: string(arguments)},
		Arguments: arguments, OperationKey: "agent:schedule-update-test", RequiredScope: identity.ScopeScheduleManage, Fence: runtime.fence,
	}
	preparation, err := executor.Prepare(context.Background(), request)
	if err != nil || preparation.Status != AgentToolApprovalRequired || preparation.Approval.Object == "" {
		t.Fatalf("Prepare() = %#v, %v; expected user approval", preparation, err)
	}
	hash, err := agentOperationHash(request.Call.Name, arguments)
	if err != nil {
		t.Fatal(err)
	}
	request.Fence = preparation.Fence
	request.Approval = &AgentToolApprovalDecision{PendingID: "r1234567", Version: 1, Decision: task.PendingDecisionApprove, RequestHash: append([]byte(nil), hash[:]...)}
	preparation, err = executor.Prepare(context.Background(), request)
	if err != nil || preparation.Status != AgentToolReady {
		t.Fatalf("approved Prepare() = %#v, %v", preparation, err)
	}
	request.OperationID = "o1234567"
	runtime.store.operation = task.Operation{
		ID: request.OperationID, TaskID: request.Task.ID, Key: request.OperationKey, RequestHash: append([]byte(nil), hash[:]...),
		HostID: request.Task.HostID, RuntimeGeneration: preparation.Fence.RuntimeGeneration,
		RuntimeInstanceID: preparation.Fence.RuntimeInstanceID, ControlVersion: preparation.Fence.ControlVersion,
		LeaseVersion: request.Task.Lease.Version, RuntimeLeaseVersion: request.Task.Lease.Version, Status: task.OperationDispatched,
	}
	result, err := executor.Execute(context.Background(), request, preparation)
	if err != nil || result.Status != AgentToolSucceeded || store.updateCalls != 1 || store.updated.BaselineFingerprint != "" || store.updated.Observation == nil || store.updated.Observation.Field != schedule.ObservationValue {
		t.Fatalf("Execute() = %#v, %v; update calls=%d stored=%#v", result, err, store.updateCalls, store.updated)
	}
}
