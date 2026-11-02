package app

import (
	"bytes"
	"context"
	"sync"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/hub"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type cloudDSHHubTaskStore struct {
	*cloudDSHBrokerTaskStore
	mu         sync.Mutex
	operations map[string]TaskHubOperation
}

func (s *cloudDSHHubTaskStore) PrepareTaskHubOperation(_ context.Context, value TaskHubOperation, _ string) (TaskHubOperation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.operations[value.Key]; ok {
		if existing.OwnerID != value.OwnerID || existing.TaskID != value.TaskID || existing.ToolName != value.ToolName ||
			existing.TargetInstallationID != value.TargetInstallationID || existing.Action != value.Action ||
			existing.TaskAuthorizationVersion != value.TaskAuthorizationVersion || existing.TargetGrantVersion != value.TargetGrantVersion ||
			!bytes.Equal(existing.RequestHash, value.RequestHash) {
			return TaskHubOperation{}, false, domain.NewError(domain.CodeIdempotencyConflict, "task Hub operation changed")
		}
		return existing, true, nil
	}
	value.RequestHash = append([]byte(nil), value.RequestHash...)
	s.operations[value.Key] = value
	return value, false, nil
}

func (s *cloudDSHHubTaskStore) MarkTaskHubOperationDispatched(_ context.Context, value TaskHubOperation, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.operations[value.Key]
	if !ok || stored.Status != task.OperationPrepared {
		return domain.NewError(domain.CodeConflict, "task Hub operation is not prepared")
	}
	stored.Status = task.OperationDispatched
	s.operations[value.Key] = stored
	return nil
}

func (s *cloudDSHHubTaskStore) CompleteTaskHubOperation(_ context.Context, value TaskHubOperation, _ string, status task.OperationStatus, resultHash []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.operations[value.Key]
	if !ok || (stored.Status != task.OperationPrepared && stored.Status != task.OperationDispatched) ||
		(status != task.OperationSucceeded && status != task.OperationFailed) {
		return domain.NewError(domain.CodeConflict, "task Hub operation is not completable")
	}
	stored.Status, stored.ResultHash = status, append([]byte(nil), resultHash...)
	s.operations[value.Key] = stored
	return nil
}

func (s *cloudDSHHubTaskStore) MarkTaskHubOperationUnknown(_ context.Context, value TaskHubOperation, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.operations[value.Key]
	if !ok || stored.Status != task.OperationDispatched {
		return domain.NewError(domain.CodeConflict, "task Hub operation is not dispatched")
	}
	stored.Status = task.OperationUnknown
	s.operations[value.Key] = stored
	return nil
}

func (s *cloudDSHHubTaskStore) GetTaskHubOperation(_ context.Context, ownerID, taskID, key string) (TaskHubOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.operations[key]
	if !ok || value.OwnerID != ownerID || value.TaskID != taskID {
		return TaskHubOperation{}, domain.NewError(domain.CodeNotFound, "task Hub operation not found")
	}
	return value, nil
}

type cloudDSHHubTaskAuthorizer struct{ authorization task.Authorization }

func (a *cloudDSHHubTaskAuthorizer) AuthorizeTaskAction(context.Context, string, identity.Kind, string, string, []identity.Scope, identity.Scope) error {
	return nil
}

func (a *cloudDSHHubTaskAuthorizer) AuthorizeTaskExecution(_ context.Context, principal identity.Principal, _ string, scope identity.Scope) (task.Authorization, error) {
	if !principal.HasScope(scope) {
		return task.Authorization{}, domain.NewError(domain.CodeForbidden, "task scope is unavailable")
	}
	return a.authorization, nil
}

func hubTaskInfoResult(actions []string, targetGrantVersion any) map[string]any {
	body := map[string]any{
		"name": "Desktop", "platform": "desktop", "os": "macOS", "version": "1",
		"actions": actions,
	}
	if targetGrantVersion != nil {
		body["target_grant_version"] = targetGrantVersion
	}
	return body
}

func answerHubTaskInfo(t *testing.T, target *hubTestPeer, actions []string, targetGrantVersion any) {
	t.Helper()
	call := target.next(t, "call")
	if call["action"] != hub.ActionInfo {
		t.Fatalf("expected Hub info probe, got %#v", call)
	}
	target.push(t, map[string]any{"type": "result", "id": call["id"], "ok": true, "body": hubTaskInfoResult(actions, targetGrantVersion)})
}

func newCloudDSHHubBroker(t *testing.T, fixture *runtimeToolExecutorFixture, hubService *HubService) (*CloudDSHToolBroker, *cloudDSHHubTaskStore, task.Task) {
	t.Helper()
	value := fixture.store.details.Task
	value.HostID, value.ExecutionLocation, value.Status = "", "server", task.StatusRunning
	value.Authorization = task.Authorization{
		Kind: string(identity.KindSession), TokenID: "t1234567", Version: 1,
		Scopes: []string{string(identity.ScopeComputerObserve), string(identity.ScopeComputerOperate), string(identity.ScopeTaskRead)},
	}
	expires := time.Now().Add(time.Hour).UTC()
	value.Authorization.ExpiresAt = &expires
	fixture.store.details.Task = value
	baseStore := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	store := &cloudDSHHubTaskStore{cloudDSHBrokerTaskStore: baseStore, operations: make(map[string]TaskHubOperation)}
	fixture.executor.store = store
	authorizer := &cloudDSHHubTaskAuthorizer{authorization: value.Authorization}
	fixture.executor.authorizer = authorizer
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil, hubService)
	if err != nil {
		t.Fatal(err)
	}
	return broker, store, value
}

func TestCloudDSHHubToolsStayHiddenUntilTargetAdvertisesGrant(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	hubService, _ := newHubFixture(t)
	target := connectHubTestPeer(t, hubService, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	broker, _, claimed := newCloudDSHHubBroker(t, fixture, hubService)
	definitions := make(chan []AgentToolDefinition, 1)
	errors := make(chan error, 1)
	go func() {
		value, err := broker.Definitions(context.Background(), claimed)
		definitions <- value
		errors <- err
	}()
	answerHubTaskInfo(t, target, []string{hub.ActionInfo, hub.ActionOpen, hub.ActionNotify}, nil)
	value := <-definitions
	if err := <-errors; err != nil {
		t.Fatalf("Definitions() error = %v", err)
	}
	for _, definition := range value {
		if _, _, _, ok := parseHubTaskToolName(definition.Name); ok {
			t.Fatalf("ungranted Hub tool was exposed: %q", definition.Name)
		}
	}
}

func TestCloudDSHHubActionUsesTaskFenceAndReplaysOnlyCheckpointedResult(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	hubService, _ := newHubFixture(t)
	target := connectHubTestPeer(t, hubService, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	broker, store, claimed := newCloudDSHHubBroker(t, fixture, hubService)
	definitions := make(chan []AgentToolDefinition, 1)
	definitionErrors := make(chan error, 1)
	go func() {
		value, err := broker.Definitions(context.Background(), claimed)
		definitions <- value
		definitionErrors <- err
	}()
	answerHubTaskInfo(t, target, []string{hub.ActionInfo, hub.ActionFiles}, int64(5))
	available := <-definitions
	if err := <-definitionErrors; err != nil {
		t.Fatalf("Definitions() error = %v", err)
	}
	toolName := hubTaskToolName("morde_hub_files", "i7654321", 5)
	found := false
	for _, definition := range available {
		found = found || definition.Name == toolName
	}
	if !found {
		t.Fatalf("target files tool %q was not exposed: %#v", toolName, available)
	}
	call := ModelToolCall{CallID: "call_hub_files_1", Name: toolName, Arguments: `{"path":"workspace"}`}
	resultCh := make(chan AgentToolResult, 1)
	errorCh := make(chan error, 1)
	go func() {
		value, err := broker.Execute(context.Background(), claimed, call)
		resultCh <- value
		errorCh <- err
	}()
	answerHubTaskInfo(t, target, []string{hub.ActionInfo, hub.ActionFiles}, int64(5))
	action := target.next(t, "call")
	from, ok := action["from"].(map[string]any)
	if !ok || from["kind"] != "task" || from["task_id"] != claimed.ID ||
		from["authorization_version"] != float64(claimed.Authorization.Version) ||
		from["target_grant_version"] != float64(5) || action["action"] != hub.ActionFiles {
		t.Fatalf("task action caller context = %#v", action)
	}
	args, ok := action["args"].(map[string]any)
	if !ok || args["path"] != "workspace" {
		t.Fatalf("files path argument = %#v", action["args"])
	}
	target.push(t, map[string]any{"type": "result", "id": action["id"], "ok": true,
		"body": map[string]any{"entries": []any{}, "target_grant_version": 5}})
	first := <-resultCh
	if err := <-errorCh; err != nil || first.Status != AgentToolSucceeded {
		t.Fatalf("Hub action result = %#v, err=%v", first, err)
	}
	key := agentOperationKey(call.CallID)
	operation, err := store.GetTaskHubOperation(context.Background(), claimed.OwnerID, claimed.ID, key)
	if err != nil || operation.Status != task.OperationSucceeded || len(operation.ResultHash) != 32 ||
		!bytes.Equal(operation.ResultHash, sha256Bytes([]byte(first.Output))) {
		t.Fatalf("stored Hub operation = %#v, err=%v", operation, err)
	}

	hubService.CloseInstallation("i7654321")
	replayResult := make(chan AgentToolResult, 1)
	replayError := make(chan error, 1)
	go func() {
		value, err := broker.Reconcile(context.Background(), claimed, call, &first.Output)
		replayResult <- value
		replayError <- err
	}()
	replayed := <-replayResult
	if err := <-replayError; err != nil || replayed.Status != AgentToolSucceeded || replayed.Output != first.Output {
		t.Fatalf("replayed Hub result = %#v, err=%v", replayed, err)
	}
	if hubService.InstallationOnline("u1234567", "i7654321") {
		t.Fatal("test target remained online during completed-operation recovery")
	}
	if len(store.operations) != 1 || store.operations[key].Status != task.OperationSucceeded {
		t.Fatalf("completed Hub operation changed during recovery: %#v", store.operations)
	}
}

func TestCloudDSHHubInterruptedActionIsMarkedUnknownAndNotReportedSuccessful(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	hubService, _ := newHubFixture(t)
	target := connectHubTestPeer(t, hubService, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	broker, store, claimed := newCloudDSHHubBroker(t, fixture, hubService)
	call := ModelToolCall{
		CallID: "call_hub_unknown_1", Name: hubTaskToolName("morde_hub_files", "i7654321", 5), Arguments: `{}`,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan AgentToolResult, 1)
	errorCh := make(chan error, 1)
	go func() {
		value, err := broker.Execute(ctx, claimed, call)
		resultCh <- value
		errorCh <- err
	}()
	answerHubTaskInfo(t, target, []string{hub.ActionInfo, hub.ActionFiles}, int64(5))
	if action := target.next(t, "call"); action["action"] != hub.ActionFiles {
		t.Fatalf("Hub action call = %#v", action)
	}
	cancel()
	if cancelFrame := target.next(t, "cancel"); cancelFrame["type"] != "cancel" {
		t.Fatalf("target cancel = %#v", cancelFrame)
	}
	result := <-resultCh
	if err := <-errorCh; err != nil || result.Status != AgentToolUnknown {
		t.Fatalf("interrupted Hub action = %#v, err=%v", result, err)
	}
	operation, err := store.GetTaskHubOperation(context.Background(), claimed.OwnerID, claimed.ID, agentOperationKey(call.CallID))
	if err != nil || operation.Status != task.OperationUnknown || fixture.store.details.Task.Status != task.StatusWaitingUser {
		t.Fatalf("unknown Hub operation/task state = %#v / %s, err=%v", operation, fixture.store.details.Task.Status, err)
	}
}
