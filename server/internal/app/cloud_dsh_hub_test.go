package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
	mu           sync.Mutex
	operations   map[string]TaskHubOperation
	fetches      map[string]TaskFileFetch
	ticketHashes map[string][]byte
	fileContents map[string][]byte
	mediaTypes   map[string]string
	receiptErr   error
}

func (s *cloudDSHHubTaskStore) PrepareTaskHubOperation(_ context.Context, value TaskHubOperation, _ string) (TaskHubOperation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.operations[value.Key]; ok {
		if existing.OwnerID != value.OwnerID || existing.TaskID != value.TaskID || existing.ToolName != value.ToolName ||
			existing.TargetInstallationID != value.TargetInstallationID || existing.Action != value.Action ||
			existing.TaskAuthorizationVersion != value.TaskAuthorizationVersion || existing.TargetGrantVersion != value.TargetGrantVersion ||
			existing.TaskRevision != value.TaskRevision ||
			!bytes.Equal(existing.RequestHash, value.RequestHash) {
			return TaskHubOperation{}, false, domain.NewError(domain.CodeIdempotencyConflict, "task Hub operation changed")
		}
		if existing.Status == task.OperationUnknown && existing.Action == hub.ActionFileFetch {
			if fetch, exists := s.fetches[value.Key]; exists && (fetch.Status == task.FileFetchQueued ||
				fetch.Status == task.FileFetchTransferring || fetch.Status == task.FileFetchVerifying) {
				existing.Status = task.OperationPrepared
				existing.LeaseVersion = value.LeaseVersion
				s.operations[value.Key] = existing
			}
		}
		return existing, true, nil
	}
	value.RequestHash = append([]byte(nil), value.RequestHash...)
	s.operations[value.Key] = value
	return value, false, nil
}

func (s *cloudDSHHubTaskStore) CreateTaskFileFetch(_ context.Context, command TaskFileFetchCreate, _ string) (TaskFileFetch, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value := command.Fetch
	if existing, ok := s.fetches[value.OperationKey]; ok {
		if existing.OwnerID != value.OwnerID || existing.TaskID != value.TaskID || existing.SourceInstallationID != value.SourceInstallationID ||
			existing.TargetGrantVersion != value.TargetGrantVersion || existing.TaskRevision != value.TaskRevision || existing.RelativePath != value.RelativePath {
			return TaskFileFetch{}, false, domain.NewError(domain.CodeIdempotencyConflict, "Task file fetch changed")
		}
		return existing, true, nil
	}
	now := time.Now().UTC()
	value.CreatedAt, value.UpdatedAt = now, now
	value.ContentState = task.FileFetchContentNone
	value.ParseStatus = task.FileFetchParsePending
	if value.SourceDeviceName == "" {
		value.SourceDeviceName = "Synthetic PC"
	}
	s.fetches[value.OperationKey] = value
	return value, false, nil
}

func (s *cloudDSHHubTaskStore) GetTaskFileFetchByOperation(_ context.Context, ownerID, taskID, operationKey string) (TaskFileFetch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.fetches[operationKey]
	if !ok || value.OwnerID != ownerID || value.TaskID != taskID {
		return TaskFileFetch{}, domain.NewError(domain.CodeNotFound, "Task file fetch not found")
	}
	return value, nil
}

func (s *cloudDSHHubTaskStore) DispatchTaskFileFetch(_ context.Context, operation TaskHubOperation, fetchID string, ticketHash []byte,
	ticketExpiresAt *time.Time, targetOnline bool, _ string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.fetches[operation.Key]
	storedOperation, operationOK := s.operations[operation.Key]
	if !ok || value.ID != fetchID || !operationOK || storedOperation.Status != task.OperationPrepared {
		return false, domain.NewError(domain.CodeConflict, "Task file fetch cannot be dispatched")
	}
	storedOperation.Status = task.OperationDispatched
	s.operations[operation.Key] = storedOperation
	if targetOnline {
		s.ticketHashes[fetchID] = append([]byte(nil), ticketHash...)
	} else {
		value.Status, value.WaitingReason = task.FileFetchWaitingDevice, "device_offline"
	}
	value.UpdatedAt = time.Now().UTC()
	s.fetches[operation.Key] = value
	return true, nil
}

func (s *cloudDSHHubTaskStore) MarkTaskFileFetchWaiting(_ context.Context, operation TaskHubOperation, fetchID, reason string) (TaskFileFetch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.fetches[operation.Key]
	if !ok || value.ID != fetchID || reason != "device_offline" {
		return TaskFileFetch{}, domain.NewError(domain.CodeNotFound, "Task file fetch not found")
	}
	value.Status, value.WaitingReason, value.UpdatedAt = task.FileFetchWaitingDevice, reason, time.Now().UTC()
	s.fetches[operation.Key] = value
	return value, nil
}

func (s *cloudDSHHubTaskStore) FailTaskFileFetch(_ context.Context, operation TaskHubOperation, fetchID, failureReason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.fetches[operation.Key]
	if !ok || value.ID != fetchID {
		return domain.NewError(domain.CodeNotFound, "Task file fetch not found")
	}
	value.Status, value.WaitingReason, value.FailureReason, value.UpdatedAt = task.FileFetchFailed, "", failureReason, time.Now().UTC()
	s.fetches[operation.Key] = value
	return nil
}

func (s *cloudDSHHubTaskStore) GetTaskFileFetch(_ context.Context, ownerID, taskID, fetchID string) (TaskFileFetch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range s.fetches {
		if value.ID == fetchID && value.OwnerID == ownerID && value.TaskID == taskID {
			return value, nil
		}
	}
	return TaskFileFetch{}, domain.NewError(domain.CodeNotFound, "Task file fetch not found")
}

func (s *cloudDSHHubTaskStore) SetTaskFileFetchParseResult(_ context.Context, ownerID, taskID, fetchID string,
	status task.FileFetchParseStatus, parseError string) (TaskFileFetch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, value := range s.fetches {
		if value.ID != fetchID || value.OwnerID != ownerID || value.TaskID != taskID {
			continue
		}
		if value.ParseStatus != task.FileFetchParsePending && (value.ParseStatus != status || value.ParseError != parseError) {
			return TaskFileFetch{}, domain.NewError(domain.CodeConflict, "Task file fetch parse result is already final")
		}
		value.ParseStatus, value.ParseError = status, parseError
		s.fetches[key] = value
		return value, nil
	}
	return TaskFileFetch{}, domain.NewError(domain.CodeNotFound, "Task file fetch not found")
}

func (s *cloudDSHHubTaskStore) OpenTaskFileFetchContent(_ context.Context, ownerID, taskID, fetchID string) (io.ReadCloser, TaskFileFetch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range s.fetches {
		if value.ID == fetchID && value.OwnerID == ownerID && value.TaskID == taskID {
			content, ok := s.fileContents[fetchID]
			if !ok {
				return nil, TaskFileFetch{}, domain.NewError(domain.CodeNotFound, "Task file fetch content not found")
			}
			return io.NopCloser(bytes.NewReader(content)), value, nil
		}
	}
	return nil, TaskFileFetch{}, domain.NewError(domain.CodeNotFound, "Task file fetch not found")
}

func (s *cloudDSHHubTaskStore) completeFileFetch(fetchID string, receipt hub.FileFetchReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, value := range s.fetches {
		if value.ID == fetchID {
			value.Status, value.SourceVersion, value.SourceSHA256 = task.FileFetchCompleted, receipt.SourceVersion, receipt.SourceSHA256
			value.SizeBytes, value.ContentID, value.ExpiresAt, value.UpdatedAt = receipt.SizeBytes, receipt.ContentID, &receipt.ExpiresAt, time.Now().UTC()
			value.ContentState, value.MediaType = task.FileFetchContentRetained, s.mediaTypes[fetchID]
			if value.MediaType == "" {
				value.MediaType = "application/pdf"
			}
			s.fetches[key] = value
			return nil
		}
	}
	return domain.NewError(domain.CodeNotFound, "Task file fetch not found")
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
	if !ok || (stored.Status != task.OperationPrepared && stored.Status != task.OperationDispatched &&
		stored.Status != task.OperationUnknown) ||
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

func (s *cloudDSHHubTaskStore) RecordTaskHubOperationLocalReceipt(_ context.Context, value TaskHubOperation, _ string, receiptHash []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.receiptErr != nil {
		return s.receiptErr
	}
	stored, ok := s.operations[value.Key]
	if !ok || stored.Action != hub.ActionFilePut || stored.Status != task.OperationDispatched || stored.SyncStage != TaskHubWriteStagePending ||
		stored.WorkspaceID != value.WorkspaceID || stored.WorkspacePath != value.WorkspacePath ||
		stored.WorkspaceSizeBytes != value.WorkspaceSizeBytes || stored.WorkspaceSHA256 != value.WorkspaceSHA256 ||
		stored.SourceCheckpointRevision != value.SourceCheckpointRevision || len(receiptHash) != 32 {
		return domain.NewError(domain.CodeConflict, "file.put operation cannot record its local receipt")
	}
	stored.LocalReceiptHash = append([]byte(nil), receiptHash...)
	stored.SyncStage = TaskHubWriteStageLocal
	s.operations[value.Key] = stored
	return nil
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

type cloudDSHRevokingTaskAuthorizer struct {
	authorization task.Authorization
	validCalls    int
	calls         int
}

func (*cloudDSHRevokingTaskAuthorizer) AuthorizeTaskAction(context.Context, string, identity.Kind, string, string, []identity.Scope, identity.Scope) error {
	return nil
}

func (a *cloudDSHRevokingTaskAuthorizer) AuthorizeTaskExecution(_ context.Context, principal identity.Principal, _ string, scope identity.Scope) (task.Authorization, error) {
	a.calls++
	if a.calls > a.validCalls {
		return task.Authorization{}, domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
	}
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
	store := &cloudDSHHubTaskStore{cloudDSHBrokerTaskStore: baseStore, operations: make(map[string]TaskHubOperation),
		fetches: make(map[string]TaskFileFetch), ticketHashes: make(map[string][]byte)}
	fixture.executor.store = store
	authorizer := &cloudDSHHubTaskAuthorizer{authorization: value.Authorization}
	fixture.executor.authorizer = authorizer
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil, hubService)
	if err != nil {
		t.Fatal(err)
	}
	return broker, store, value
}

func TestCloudDSHHubExpiredTaskAuthorizationHidesToolsAndBlocksDispatch(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	hubService, _ := newHubFixture(t)
	broker, store, claimed := newCloudDSHHubBroker(t, fixture, hubService)
	expired := time.Now().Add(-time.Minute).UTC()
	claimed.Authorization.ExpiresAt = &expired
	fixture.store.details.Task.Authorization = claimed.Authorization

	definitions, err := broker.hubTaskDefinitions(context.Background(), claimed)
	if err != nil || len(definitions) != 0 {
		t.Fatalf("expired Task authorization definitions = %#v, err=%v; want no Hub tools", definitions, err)
	}
	call := ModelToolCall{CallID: "call-expired-auth", Name: hubTaskToolName("morde_hub_files", "i7654321", 5), Arguments: `{}`}
	_, err = broker.Execute(context.Background(), claimed, call)
	if domain.CodeOf(err) != domain.CodeForbidden || len(store.operations) != 0 {
		t.Fatalf("expired Task authorization dispatch err=%v, operations=%#v; want forbidden before operation creation", err, store.operations)
	}
}

func TestCloudDSHHubExpiredTargetInstallationHidesToolsAndBlocksDispatch(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	hubService, hubStore := newHubFixture(t)
	target := connectHubTestPeer(t, hubService, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	hubStore.mu.Lock()
	installation := hubStore.installations["i7654321"]
	installation.ExpiresAt = time.Now().Add(-time.Minute)
	hubStore.installations["i7654321"] = installation
	hubStore.mu.Unlock()
	broker, store, claimed := newCloudDSHHubBroker(t, fixture, hubService)

	definitions, err := broker.hubTaskDefinitions(context.Background(), claimed)
	if err != nil || len(definitions) != 0 {
		t.Fatalf("expired target installation definitions = %#v, err=%v; want no Hub tools", definitions, err)
	}
	call := ModelToolCall{CallID: "call-expired-target", Name: hubTaskToolName("morde_hub_files", "i7654321", 5), Arguments: `{"path":"workspace"}`}
	result, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || result.Status != AgentToolNeedsUser || result.WaitingReason != "hub_target_authorization_changed" || len(store.operations) != 0 {
		t.Fatalf("expired target installation dispatch = %#v, operations=%#v, err=%v; want no operation", result, store.operations, err)
	}
	if frame := target.maybeNext(); frame != nil {
		t.Fatalf("expired target installation received a Hub action: %#v", frame)
	}
}

func TestCloudDSHHubActionScopeHidesAndBlocksCommand(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	hubService, _ := newHubFixture(t)
	target := connectHubTestPeer(t, hubService, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	broker, store, claimed := newCloudDSHHubBroker(t, fixture, hubService)
	claimed.Authorization.Kind = string(identity.KindClient)
	claimed.Authorization.ClientInstallationID = "i1234567"
	claimed.Authorization.Scopes = []string{string(identity.ScopeComputerObserve), string(identity.ScopeTaskRead)}
	fixture.store.details.Task.Authorization = claimed.Authorization
	fixture.executor.authorizer.(*cloudDSHHubTaskAuthorizer).authorization = claimed.Authorization

	definitions := make(chan []AgentToolDefinition, 1)
	errors := make(chan error, 1)
	go func() {
		value, err := broker.hubTaskDefinitions(context.Background(), claimed)
		definitions <- value
		errors <- err
	}()
	answerHubTaskInfo(t, target, []string{hub.ActionInfo, hub.ActionScreenshot, hub.ActionCommand}, int64(5))
	available := <-definitions
	if err := <-errors; err != nil {
		t.Fatalf("definitions without computer.operate scope: %v", err)
	}
	commandName := hubTaskToolName("morde_hub_command", "i7654321", 5)
	for _, definition := range available {
		if definition.Name == commandName {
			t.Fatal("command tool was exposed without computer.operate scope")
		}
	}
	call := ModelToolCall{CallID: "call-missing-action-scope", Name: commandName, Arguments: `{"command":"pwd","timeout_ms":1000}`}
	_, err := broker.Execute(context.Background(), claimed, call)
	if domain.CodeOf(err) != domain.CodeForbidden || len(store.operations) != 0 {
		t.Fatalf("command without computer.operate scope err=%v, operations=%#v; want forbidden before operation creation", err, store.operations)
	}
	if frame := target.maybeNext(); frame != nil {
		t.Fatalf("target received action without computer.operate scope: %#v", frame)
	}
}

func TestCloudDSHHubAuthorizedCommandUsesTaskFence(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	hubService, _ := newHubFixture(t)
	target := connectHubTestPeer(t, hubService, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	broker, store, claimed := newCloudDSHHubBroker(t, fixture, hubService)
	claimed.Authorization.Kind = string(identity.KindClient)
	claimed.Authorization.ClientInstallationID = "i1234567"
	claimed.Authorization.Scopes = []string{
		string(identity.ScopeTaskRead), string(identity.ScopeComputerObserve), string(identity.ScopeComputerOperate),
	}
	fixture.store.details.Task.Authorization = claimed.Authorization
	fixture.executor.authorizer.(*cloudDSHHubTaskAuthorizer).authorization = claimed.Authorization

	definitions := make(chan []AgentToolDefinition, 1)
	definitionErrors := make(chan error, 1)
	go func() {
		value, err := broker.hubTaskDefinitions(context.Background(), claimed)
		definitions <- value
		definitionErrors <- err
	}()
	answerHubTaskInfo(t, target, []string{hub.ActionInfo, hub.ActionCommand}, int64(5))
	available := <-definitions
	if err := <-definitionErrors; err != nil {
		t.Fatalf("definitions with computer.operate scope: %v", err)
	}
	toolName := hubTaskToolName("morde_hub_command", "i7654321", 5)
	found := false
	for _, definition := range available {
		found = found || definition.Name == toolName
	}
	if !found {
		t.Fatalf("authorized command tool %q was not exposed: %#v", toolName, available)
	}

	call := ModelToolCall{CallID: "call-authorized-command", Name: toolName, Arguments: `{"command":"printf ok","timeout_ms":1000}`}
	resultCh := make(chan AgentToolResult, 1)
	errorCh := make(chan error, 1)
	go func() {
		value, err := broker.Execute(context.Background(), claimed, call)
		resultCh <- value
		errorCh <- err
	}()
	answerHubTaskInfo(t, target, []string{hub.ActionInfo, hub.ActionCommand}, int64(5))
	action := target.next(t, "call")
	from, ok := action["from"].(map[string]any)
	if !ok || from["kind"] != "task" || from["task_id"] != claimed.ID ||
		from["authorization_version"] != float64(claimed.Authorization.Version) ||
		from["target_grant_version"] != float64(5) || action["action"] != hub.ActionCommand {
		t.Fatalf("Task command context = %#v", action)
	}
	args, ok := action["args"].(map[string]any)
	if !ok || args["command"] != "printf ok" || args["timeout_ms"] != float64(1000) {
		t.Fatalf("command arguments = %#v", action["args"])
	}
	target.push(t, map[string]any{"type": "result", "id": action["id"], "ok": true,
		"body": map[string]any{"exit_code": 0, "stdout": "ok", "stderr": "", "timed_out": false, "stopped": false, "target_grant_version": 5}})
	result := <-resultCh
	if err := <-errorCh; err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("authorized command result = %#v, err=%v", result, err)
	}
	operation, err := store.GetTaskHubOperation(context.Background(), claimed.OwnerID, claimed.ID, agentOperationKey(call.CallID))
	if err != nil || operation.Status != task.OperationSucceeded || len(operation.ResultHash) != 32 {
		t.Fatalf("authorized command operation = %#v, err=%v", operation, err)
	}
}

func TestCloudDSHHubTaskAuthorizationRevocationFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name                    string
		validAuthorizationCalls int
		dispatched              bool
		wantResult              AgentToolResultStatus
		wantOperation           task.OperationStatus
	}{
		{name: "before dispatch", validAuthorizationCalls: 1, wantResult: AgentToolFailed, wantOperation: task.OperationFailed},
		{name: "while in flight", validAuthorizationCalls: 2, dispatched: true, wantResult: AgentToolUnknown, wantOperation: task.OperationUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			hubService, _ := newHubFixture(t)
			target := connectHubTestPeer(t, hubService, "u1234567", "i7654321")
			_ = target.next(t, "devices")
			broker, store, claimed := newCloudDSHHubBroker(t, fixture, hubService)
			fixture.executor.authorizer = &cloudDSHRevokingTaskAuthorizer{
				authorization: claimed.Authorization, validCalls: test.validAuthorizationCalls,
			}
			call := ModelToolCall{
				CallID: "call-revoked-task-auth", Name: hubTaskToolName("morde_hub_files", "i7654321", 5), Arguments: `{"path":"workspace"}`,
			}
			resultCh := make(chan AgentToolResult, 1)
			errorCh := make(chan error, 1)
			go func() {
				value, err := broker.Execute(context.Background(), claimed, call)
				resultCh <- value
				errorCh <- err
			}()
			answerHubTaskInfo(t, target, []string{hub.ActionInfo, hub.ActionFiles}, int64(5))
			if test.dispatched {
				action := target.next(t, "call")
				target.push(t, map[string]any{"type": "result", "id": action["id"], "ok": true,
					"body": map[string]any{"entries": []any{}, "target_grant_version": 5}})
			}
			result := <-resultCh
			if err := <-errorCh; err != nil || result.Status != test.wantResult {
				t.Fatalf("revoked Task authorization result = %#v, err=%v; want %s", result, err, test.wantResult)
			}
			operation, err := store.GetTaskHubOperation(context.Background(), claimed.OwnerID, claimed.ID, agentOperationKey(call.CallID))
			if err != nil || operation.Status != test.wantOperation {
				t.Fatalf("revoked Task authorization operation = %#v, err=%v; want %s", operation, err, test.wantOperation)
			}
			if !test.dispatched && target.maybeNext() != nil {
				t.Fatal("Hub action was dispatched after Task authorization was revoked")
			}
			if test.dispatched && fixture.store.details.Task.Status != task.StatusWaitingUser {
				t.Fatalf("revoked in-flight Task status = %s; want waiting_user", fixture.store.details.Task.Status)
			}
		})
	}
}

func TestCloudDSHHubToolsStayHiddenUntilTargetAdvertisesGrant(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	hubService, _ := newHubFixture(t)
	target := connectHubTestPeer(t, hubService, "u1234567", "i7654321")
	_ = target.next(t, "devices")
	broker, store, claimed := newCloudDSHHubBroker(t, fixture, hubService)
	definitions := make(chan []AgentToolDefinition, 1)
	errors := make(chan error, 1)
	go func() {
		value, err := broker.Definitions(context.Background(), claimed)
		definitions <- value
		errors <- err
	}()
	answerHubTaskInfo(t, target, []string{hub.ActionInfo, hub.ActionScreenshot, hub.ActionFiles}, nil)
	value := <-definitions
	if err := <-errors; err != nil {
		t.Fatalf("Definitions() error = %v", err)
	}
	for _, definition := range value {
		if _, _, _, ok := parseHubTaskToolName(definition.Name); ok {
			t.Fatalf("ungranted Hub tool was exposed: %q", definition.Name)
		}
	}
	if _, _, _, ok := parseHubTaskToolName("morde_hub_file_put_i7654321_v1"); ok {
		t.Fatal("file.put remained callable without an advertised server tool")
	}
	_, err := broker.Execute(context.Background(), claimed, ModelToolCall{
		CallID: "call-file-put-unpublished", Name: "morde_hub_file_put_i7654321_v1",
		Arguments: `{"workspace_id":"w1234567","path":"notes.txt","size_bytes":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
	})
	if domain.CodeOf(err) != domain.CodeForbidden || len(store.operations) != 0 {
		t.Fatalf("unpublished file.put dispatch err=%v, operations=%#v; want forbidden before operation creation", err, store.operations)
	}
	if frame := target.maybeNext(); frame != nil {
		t.Fatalf("target received unpublished file.put action: %#v", frame)
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

func TestCloudDSHHubFilePutWaitsForWorkspaceSyncAndRehydratesReceipt(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	broker, store, claimed := newCloudDSHHubBroker(t, fixture, nil)
	claimed.WorkspaceID = "w1234567"
	checkpoint := int64(3)
	claimed.WorkspaceCheckpointRevision = &checkpoint
	fixture.store.details.Task.WorkspaceID = claimed.WorkspaceID
	fixture.store.details.Task.WorkspaceCheckpointRevision = &checkpoint
	operation := TaskHubOperation{
		OwnerID: claimed.OwnerID, TaskID: claimed.ID, Key: "agent:0123456789abcdef", ToolName: "morde_hub_file_put_i7654321_v5",
		TargetInstallationID: "i7654321", Action: hub.ActionFilePut, TaskAuthorizationVersion: claimed.Authorization.Version,
		TargetGrantVersion: 5, LeaseVersion: claimed.Lease.Version, RequestHash: sha256Bytes([]byte("file.put request")),
		Status: task.OperationDispatched, WorkspaceID: claimed.WorkspaceID, WorkspacePath: "reports/summary.txt",
		WorkspaceSizeBytes: 12, WorkspaceSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		SourceCheckpointRevision: checkpoint, LocalReceiptHash: bytes.Repeat([]byte{1}, 32),
		SyncStage: TaskHubWriteStageSyncPending, SyncSessionID: "p1234567",
	}
	call := ModelToolCall{CallID: "call_hub_file_put_1", Name: operation.ToolName, Arguments: `{}`}
	waiting, handled, err := broker.reconcileHubTaskOperation(context.Background(), claimed, call, operation, operation, true, nil)
	if err != nil || !handled || waiting.Status != AgentToolNeedsUser || waiting.WaitingReason != "task_hub_workspace_sync" ||
		fixture.store.details.Task.Status != task.StatusWaitingUser || fixture.store.details.Task.WaitingReason != "task_hub_workspace_sync" {
		t.Fatalf("file.put sync wait = %#v, handled=%t, task=%#v, err=%v", waiting, handled, fixture.store.details.Task, err)
	}
	var eventPayload map[string]json.RawMessage
	if len(store.events) == 0 || json.Unmarshal(store.events[len(store.events)-1].Payload, &eventPayload) != nil {
		t.Fatalf("file.put wait event missing: %#v", store.events)
	}
	var detail struct {
		OperationKey string `json:"operation_key"`
	}
	if json.Unmarshal(eventPayload["detail"], &detail) != nil || detail.OperationKey != operation.Key {
		t.Fatalf("file.put wait event operation key = %q, want %q", detail.OperationKey, operation.Key)
	}

	settledFixture := newRuntimeToolExecutorFixture(t, nil)
	settledBroker, _, settledTask := newCloudDSHHubBroker(t, settledFixture, nil)
	settledTask.WorkspaceID = operation.WorkspaceID
	currentCheckpoint := int64(7)
	settledTask.WorkspaceCheckpointRevision = &currentCheckpoint
	settledFixture.store.details.Task.WorkspaceID = settledTask.WorkspaceID
	settledFixture.store.details.Task.WorkspaceCheckpointRevision = &currentCheckpoint
	settled := operation
	settled.OwnerID, settled.TaskID, settled.LeaseVersion = settledTask.OwnerID, settledTask.ID, settledTask.Lease.Version
	settled.SyncStage, settled.SyncSessionID, settled.SyncRevision, settled.Status =
		TaskHubWriteStageCloudSynced, "p1234567", currentCheckpoint, task.OperationSucceeded
	resultHash, err := TaskHubFilePutCloudReceiptHash(settled)
	if err != nil {
		t.Fatalf("hash settled file.put receipt: %v", err)
	}
	settled.ResultHash = resultHash[:]
	settledCall := call
	completed, handled, err := settledBroker.reconcileHubTaskOperation(context.Background(), settledTask, settledCall, settled, settled, true, nil)
	if err != nil || !handled || completed.Status != AgentToolSucceeded || !bytes.Contains([]byte(completed.Output), []byte("reports/summary.txt")) {
		t.Fatalf("rehydrated file.put result = %#v, handled=%t, err=%v", completed, handled, err)
	}

	tamperedFixture := newRuntimeToolExecutorFixture(t, nil)
	tamperedBroker, _, tamperedTask := newCloudDSHHubBroker(t, tamperedFixture, nil)
	tamperedTask.WorkspaceID = settled.WorkspaceID
	tamperedTask.WorkspaceCheckpointRevision = &currentCheckpoint
	tamperedFixture.store.details.Task.WorkspaceID = settled.WorkspaceID
	tamperedFixture.store.details.Task.WorkspaceCheckpointRevision = &currentCheckpoint
	tampered := settled
	tampered.OwnerID, tampered.TaskID, tampered.LeaseVersion = tamperedTask.OwnerID, tamperedTask.ID, tamperedTask.Lease.Version
	tamperedHash, err := TaskHubFilePutCloudReceiptHash(tampered)
	if err != nil {
		t.Fatalf("hash tampered-test file.put receipt: %v", err)
	}
	tampered.ResultHash = sha256Bytes([]byte("tampered receipt"))
	if bytes.Equal(tampered.ResultHash, tamperedHash[:]) {
		t.Fatal("tampered result unexpectedly matches the Task file.put receipt")
	}
	unknown, handled, err := tamperedBroker.reconcileHubTaskOperation(context.Background(), tamperedTask, call, tampered, tampered, true, nil)
	if err != nil || !handled || unknown.Status != AgentToolUnknown {
		t.Fatalf("tampered file.put receipt = %#v, handled=%t, err=%v; want durable unknown", unknown, handled, err)
	}
}

func TestCloudDSHHubFilePutReceiptIsPersistedBeforeSyncWait(t *testing.T) {
	newPendingWrite := func(t *testing.T, receiptErr error) (*CloudDSHToolBroker, *cloudDSHHubTaskStore, task.Task, TaskHubOperation, hub.WorkspaceWriteArgs, hub.TaskContext) {
		t.Helper()
		fixture := newRuntimeToolExecutorFixture(t, nil)
		broker, store, claimed := newCloudDSHHubBroker(t, fixture, nil)
		claimed.WorkspaceID = "w1234567"
		checkpoint := int64(4)
		claimed.WorkspaceCheckpointRevision = &checkpoint
		fixture.store.details.Task.WorkspaceID = claimed.WorkspaceID
		fixture.store.details.Task.WorkspaceCheckpointRevision = &checkpoint
		store.receiptErr = receiptErr
		request := hub.WorkspaceWriteArgs{
			WorkspaceID: claimed.WorkspaceID, Path: "reports/summary.txt", SizeBytes: 12,
			SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		}
		call := ModelToolCall{CallID: "call_hub_file_put_receipt", Name: "morde_hub_file_put_i7654321_v5", Arguments: `{}`}
		operationKey := agentOperationKey(call.CallID)
		operation := TaskHubOperation{
			OwnerID: claimed.OwnerID, TaskID: claimed.ID, Key: operationKey, ToolName: call.Name,
			TargetInstallationID: "i7654321", Action: hub.ActionFilePut,
			TaskAuthorizationVersion: claimed.Authorization.Version, TargetGrantVersion: 5,
			LeaseVersion: claimed.Lease.Version, RequestHash: sha256Bytes([]byte("file.put request")),
			Status: task.OperationPrepared, WorkspaceID: request.WorkspaceID, WorkspacePath: request.Path,
			WorkspaceSizeBytes: request.SizeBytes, WorkspaceSHA256: request.SHA256,
			SourceCheckpointRevision: checkpoint, SyncStage: TaskHubWriteStagePending,
		}
		operation, replayed, err := store.PrepareTaskHubOperation(context.Background(), operation, claimed.Lease.Owner)
		if err != nil || replayed {
			t.Fatalf("prepare file.put operation = %#v, replayed=%t, err=%v", operation, replayed, err)
		}
		if err := store.MarkTaskHubOperationDispatched(context.Background(), operation, claimed.Lease.Owner); err != nil {
			t.Fatalf("mark file.put operation dispatched: %v", err)
		}
		operation.Status = task.OperationDispatched
		taskContext := hub.TaskContext{
			TaskID: claimed.ID, AuthorizationVersion: claimed.Authorization.Version,
			TargetGrantVersion: 5, OperationID: operationKey,
		}
		return broker, store, claimed, operation, request, taskContext
	}
	writeReceipt := func(t *testing.T, request hub.WorkspaceWriteArgs, taskContext hub.TaskContext) json.RawMessage {
		t.Helper()
		value, err := json.Marshal(hub.WorkspaceWriteReceipt{
			OperationID: taskContext.OperationID, WorkspaceID: request.WorkspaceID, Path: request.Path,
			SizeBytes: request.SizeBytes, SHA256: request.SHA256,
			TargetGrantVersion: taskContext.TargetGrantVersion, Stage: "local_applied",
		})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}

	t.Run("valid local receipt", func(t *testing.T) {
		broker, store, claimed, operation, request, taskContext := newPendingWrite(t, nil)
		call := ModelToolCall{CallID: "call_hub_file_put_receipt", Name: operation.ToolName, Arguments: `{}`}
		result, err := broker.recordHubTaskFilePutReceipt(context.Background(), claimed, call, store, operation,
			request, taskContext, HubTaskCallResult{OK: true, Body: writeReceipt(t, request, taskContext)})
		if err != nil || result.Status != AgentToolNeedsUser || result.WaitingReason != "task_hub_workspace_sync" ||
			claimed.ID != store.operations[operation.Key].TaskID || store.operations[operation.Key].SyncStage != TaskHubWriteStageLocal ||
			len(store.operations[operation.Key].LocalReceiptHash) != 32 || store.operations[operation.Key].Status != task.OperationDispatched {
			t.Fatalf("local receipt result/state = %#v / %#v, err=%v", result, store.operations[operation.Key], err)
		}
		if fixtureTask := store.details.Task; fixtureTask.Status != task.StatusWaitingUser || fixtureTask.WaitingReason != "task_hub_workspace_sync" {
			t.Fatalf("Task state after local receipt = %#v", fixtureTask)
		}
		var eventPayload map[string]json.RawMessage
		if json.Unmarshal(store.events[len(store.events)-1].Payload, &eventPayload) != nil {
			t.Fatalf("file.put wait event is invalid: %#v", store.events)
		}
		var detail struct {
			OperationKey string `json:"operation_key"`
		}
		if json.Unmarshal(eventPayload["detail"], &detail) != nil || detail.OperationKey != operation.Key {
			t.Fatalf("file.put wait event operation key = %q, want %q", detail.OperationKey, operation.Key)
		}
	})

	for name, receiptErr := range map[string]error{
		"receipt mismatch":    nil,
		"receipt persistence": domain.NewError(domain.CodeInternal, "test receipt persistence failure"),
	} {
		t.Run(name, func(t *testing.T) {
			broker, store, claimed, operation, request, taskContext := newPendingWrite(t, receiptErr)
			call := ModelToolCall{CallID: "call_hub_file_put_receipt", Name: operation.ToolName, Arguments: `{}`}
			body := writeReceipt(t, request, taskContext)
			if name == "receipt mismatch" {
				mismatchBody, marshalErr := json.Marshal(hub.WorkspaceWriteReceipt{
					OperationID: taskContext.OperationID, WorkspaceID: request.WorkspaceID, Path: "reports/other.txt",
					SizeBytes: request.SizeBytes, SHA256: request.SHA256,
					TargetGrantVersion: taskContext.TargetGrantVersion, Stage: "local_applied",
				})
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				body = mismatchBody
			}
			result, err := broker.recordHubTaskFilePutReceipt(context.Background(), claimed, call, store, operation,
				request, taskContext, HubTaskCallResult{OK: true, Body: body})
			if err != nil || result.Status != AgentToolUnknown || store.operations[operation.Key].Status != task.OperationUnknown ||
				store.details.Task.WaitingReason != "hub_action_unknown" {
				t.Fatalf("unknown local receipt result/state = %#v / %#v, err=%v", result, store.operations[operation.Key], err)
			}
		})
	}
}
