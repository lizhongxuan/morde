package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

type deferredBrokerTestStore struct {
	*cloudDSHBrokerTaskStore
	target         DeferredTaskTarget
	resolvedTarget DeferredTaskTarget
	resolveErr     error
	resolveCalls   int
	loadCalls      int
	bindCalls      int
	bindErr        error
}

type deferredWorkspaceBrokerTestStore struct {
	*deferredBrokerTestStore
	workspaceStore *cloudDSHT08WorkspaceStore
	workspaceReads int
	workspaceBinds int
}

type projectDeferredWorkspaceBrokerTestStore struct {
	*deferredWorkspaceBrokerTestStore
	inSelection, denySelection      bool
	selectionCalls, unguardedAccess int
}

func (s *projectDeferredWorkspaceBrokerTestStore) ValidateProjectTaskWorkspaceAccess(context.Context, task.Task, string, bool) (string, error) {
	return s.workspaceStore.workspace.OwnerID, nil
}

func (s *projectDeferredWorkspaceBrokerTestStore) WithProjectTaskWorkspaceSelection(ctx context.Context, _ task.Task,
	_ DeferredTaskTarget, _ bool, operation func(context.Context, string) error) error {
	s.selectionCalls++
	if s.denySelection {
		return domain.NewError(domain.CodeForbidden, "synthetic project Workspace grant was revoked")
	}
	s.inSelection = true
	defer func() { s.inSelection = false }()
	return operation(ctx, s.workspaceStore.workspace.OwnerID)
}

func (s *projectDeferredWorkspaceBrokerTestStore) GetWorkspace(ctx context.Context, id string) (workspace.Workspace, error) {
	if !s.inSelection {
		s.unguardedAccess++
	}
	return s.deferredWorkspaceBrokerTestStore.GetWorkspace(ctx, id)
}

func (s *projectDeferredWorkspaceBrokerTestStore) GetWorkspaceManifestPageForOwner(ctx context.Context, ownerID, workspaceID string,
	revision int64, prefix, after string, limit int32) (workspace.ManifestPage, error) {
	if !s.inSelection {
		s.unguardedAccess++
	}
	return s.deferredWorkspaceBrokerTestStore.GetWorkspaceManifestPageForOwner(ctx, ownerID, workspaceID, revision, prefix, after, limit)
}

func (s *projectDeferredWorkspaceBrokerTestStore) BindDeferredTaskWorkspace(ctx context.Context, value task.Task,
	id string, revision int64, digest []byte) (task.Task, error) {
	if !s.inSelection {
		s.unguardedAccess++
	}
	bound, err := s.deferredWorkspaceBrokerTestStore.BindDeferredTaskWorkspace(ctx, value, id, revision, digest)
	if err == nil {
		version := int64(1)
		bound.WorkspaceOwnerID, bound.WorkspaceProjectResourceVersion = s.workspaceStore.workspace.OwnerID, &version
		s.mu.Lock()
		s.details.Task = bound
		s.mu.Unlock()
	}
	return bound, err
}

func (s *deferredWorkspaceBrokerTestStore) GetWorkspace(ctx context.Context, id string) (workspace.Workspace, error) {
	s.workspaceReads++
	return s.workspaceStore.GetWorkspace(ctx, id)
}

func (s *deferredWorkspaceBrokerTestStore) GetWorkspaceManifestPageForOwner(ctx context.Context, ownerID, workspaceID string,
	revision int64, prefix, after string, limit int32) (workspace.ManifestPage, error) {
	return s.workspaceStore.GetWorkspaceManifestPageForOwner(ctx, ownerID, workspaceID, revision, prefix, after, limit)
}

func (s *deferredWorkspaceBrokerTestStore) BindDeferredTaskWorkspace(_ context.Context, _ task.Task, id string, revision int64, digest []byte) (task.Task, error) {
	s.workspaceBinds++
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.details.Task
	current.WorkspaceID, current.InputRevision, current.WorkspaceCheckpointRevision, current.ContextDigest = id, &revision, &revision, digest
	s.details.Task = current
	return current, nil
}

func (s *deferredBrokerTestStore) LoadDeferredTaskTarget(context.Context, task.Task) (DeferredTaskTarget, bool, error) {
	s.loadCalls++
	return s.target, true, nil
}

func (s *deferredBrokerTestStore) ResolveDeferredTaskWorkspace(_ context.Context, _ task.Task,
	target DeferredTaskTarget) (DeferredTaskTarget, bool, error) {
	s.resolveCalls++
	if s.resolveErr != nil {
		return DeferredTaskTarget{}, false, s.resolveErr
	}
	resolved := s.resolvedTarget
	if resolved.HostID == "" {
		resolved.HostID = target.HostID
	}
	return resolved, resolved.WorkspaceID != "", nil
}

func (s *deferredBrokerTestStore) BindDeferredTaskTarget(_ context.Context, _ task.Task, selected DeferredTaskTarget, revision *int64, digest []byte) (task.Task, error) {
	s.bindCalls++
	if s.bindErr != nil {
		return task.Task{}, s.bindErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.details.Task
	current.HostID, current.ApplicationID = selected.HostID, selected.ApplicationID
	if selected.WorkspaceID != "" {
		current.WorkspaceID, current.InputRevision, current.WorkspaceCheckpointRevision, current.ContextDigest = selected.WorkspaceID, revision, revision, digest
	}
	s.details.Task = current
	return current, nil
}

func TestDeferredComputerIsCheckedOnlyWhenToolIsCalled(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	base := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	store := &deferredBrokerTestStore{cloudDSHBrokerTaskStore: base, target: DeferredTaskTarget{HostID: "h1234567"}}
	fixture.executor.store = store
	store.details.Task.HostID = ""
	store.details.Task.WorkspaceID = ""
	store.details.Task.InputRevision = nil
	store.details.Task.WorkspaceCheckpointRevision = nil
	store.details.Task.ExecutionLocation = "server"
	store.details.Task.AgentInstanceID = "agent-deferred"
	store.details.Task.AgentGeneration = 1
	store.details.Task.AgentSessionID = "session-deferred"
	claimed := store.details.Task
	fixture.fences.fence.AIAllowed = false
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil || !containsAgentTool(definitions, runtimeToolWindowList) || store.bindCalls != 0 {
		t.Fatalf("offline computer blocked normal model startup: definitions=%+v bindings=%d err=%v", definitions, store.bindCalls, err)
	}
	result, err := broker.Execute(context.Background(), claimed, ModelToolCall{CallID: "deferred-window-list", Name: runtimeToolWindowList, Arguments: `{}`})
	if err != nil || result.Status != AgentToolNeedsUser || result.WaitingReason != "computer_unavailable" ||
		store.details.Task.WaitingReason != "computer_unavailable" || store.bindCalls != 0 || len(store.events) != 1 {
		t.Fatalf("offline tool call was not safely paused: result=%+v bindings=%d task=%+v err=%v", result, store.bindCalls, store.details.Task, err)
	}
}

func TestUnavailableDeferredTargetDoesNotBlockOrdinaryConversation(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	base := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	store := &deferredBrokerTestStore{cloudDSHBrokerTaskStore: base, target: DeferredTaskTarget{Unavailable: true}}
	fixture.executor.store = store
	store.details.Task.HostID = ""
	store.details.Task.WorkspaceID = ""
	store.details.Task.InputRevision = nil
	store.details.Task.WorkspaceCheckpointRevision = nil
	store.details.Task.ExecutionLocation = "server"
	store.details.Task.AgentInstanceID = "agent-deferred"
	store.details.Task.AgentGeneration = 1
	store.details.Task.AgentSessionID = "session-deferred"
	claimed := store.details.Task
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil || !containsAgentTool(definitions, runtimeToolWindowList) || store.bindCalls != 0 {
		t.Fatalf("unavailable target blocked ordinary model startup: definitions=%+v bindings=%d err=%v", definitions, store.bindCalls, err)
	}
	result, err := broker.Execute(context.Background(), claimed, ModelToolCall{CallID: "stale-target-window-list", Name: runtimeToolWindowList, Arguments: `{}`})
	if err != nil || result.Status != AgentToolNeedsUser || result.WaitingReason != "computer_unavailable" || store.bindCalls != 0 {
		t.Fatalf("stale target was not deferred until actual tool use: result=%+v bindings=%d err=%v", result, store.bindCalls, err)
	}
}

func TestDeferredComputerNeverSwitchesTargetOrReplaysUnknownResult(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	base := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	store := &deferredBrokerTestStore{cloudDSHBrokerTaskStore: base, target: DeferredTaskTarget{HostID: "h1234567"}}
	fixture.executor.store = store
	store.details.Task.HostID = ""
	store.details.Task.WorkspaceID = ""
	store.details.Task.InputRevision = nil
	store.details.Task.WorkspaceCheckpointRevision = nil
	store.details.Task.ExecutionLocation = "server"
	store.details.Task.AgentInstanceID = "agent-deferred"
	store.details.Task.AgentGeneration = 1
	store.details.Task.AgentSessionID = "session-deferred"
	claimed := store.details.Task
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := "previous tool output"
	result, err := broker.Reconcile(context.Background(), claimed,
		ModelToolCall{CallID: "deferred-unknown", Name: runtimeToolWindowList, Arguments: `{}`}, &checkpoint)
	if err != nil || result.Status != AgentToolNeedsUser || result.WaitingReason != "target_result_unknown" || store.bindCalls != 0 {
		t.Fatalf("unknown prior tool result was replayed: result=%+v bindings=%d err=%v", result, store.bindCalls, err)
	}
	store.details.Task.Status = task.StatusRunning
	store.details.Task.HostID = "h7654321"
	if _, _, _, err := broker.refreshDeferredTask(context.Background(), claimed); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("changed computer target was accepted: %v", err)
	}
}

func TestDeferredComputerBindsAndRunsOneAuthorizedToolCall(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{capability: "window.list", action: "list", result: `{"windows":[]}`}})
	base := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	store := &deferredBrokerTestStore{cloudDSHBrokerTaskStore: base, target: DeferredTaskTarget{HostID: "h1234567"}}
	fixture.executor.store = store
	session, err := fixture.executor.connect(context.Background(), store.details.Task, fixture.fence)
	if err != nil {
		t.Fatal(err)
	}
	fixture.fences.fence = agentFenceFromRuntime(session.Fence)
	session.Close()
	store.details.Task.HostID = ""
	store.details.Task.WorkspaceID = ""
	store.details.Task.InputRevision = nil
	store.details.Task.WorkspaceCheckpointRevision = nil
	store.details.Task.ExecutionLocation = "server"
	store.details.Task.AgentInstanceID = "agent-deferred"
	store.details.Task.AgentGeneration = 1
	store.details.Task.AgentSessionID = "session-deferred"
	claimed := store.details.Task
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelToolCall{CallID: "deferred-list-once", Name: runtimeToolWindowList, Arguments: `{}`}
	result, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || result.Status != AgentToolSucceeded || store.bindCalls != 1 || store.resolveCalls != 0 || fixture.handlers["window.list"].calls.Load() != 1 {
		t.Fatalf("selected computer did not execute: result=%+v bindings=%d err=%v", result, store.bindCalls, err)
	}
	replayed, err := broker.Reconcile(context.Background(), claimed, call, &result.Output)
	if err != nil || replayed.Status != AgentToolSucceeded || fixture.handlers["window.list"].calls.Load() != 1 {
		t.Fatalf("computer call was replayed: result=%+v err=%v calls=%d", replayed, err, fixture.handlers["window.list"].calls.Load())
	}
}

func TestDeferredFileToolResolvesWorkspaceAndInjectsServerTarget(t *testing.T) {
	fixture, base, broker, workspaceStore, original := newCloudDSHT08WorkspaceBroker(t)
	deferred := &deferredWorkspaceBrokerTestStore{deferredBrokerTestStore: &deferredBrokerTestStore{
		cloudDSHBrokerTaskStore: base, target: DeferredTaskTarget{HostID: "h1234567"},
		resolvedTarget: DeferredTaskTarget{HostID: "h1234567", WorkspaceID: original.WorkspaceID, WorkspaceBindingVersion: 7},
	}, workspaceStore: workspaceStore}
	fixture.executor.store, broker.store = deferred, deferred
	base.details.Task.HostID, base.details.Task.WorkspaceID = "", ""
	base.details.Task.Authorization.Kind = string(identity.KindSession)
	base.details.Task.Authorization.Kind = string(identity.KindSession)
	base.details.Task.Authorization.Scopes = []string{string(identity.ScopeComputerObserve)}
	base.details.Task.InputRevision, base.details.Task.WorkspaceCheckpointRevision = nil, nil
	claimed := base.details.Task

	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil || deferred.resolveCalls != 0 || deferred.workspaceReads != 0 {
		t.Fatalf("Workspace was resolved before a file tool call: definitions=%+v resolve=%d reads=%d err=%v",
			definitions, deferred.resolveCalls, deferred.workspaceReads, err)
	}
	var fileRead *AgentToolDefinition
	for index := range definitions {
		if definitions[index].Name == runtimeToolFileRead {
			fileRead = &definitions[index]
			break
		}
	}
	if fileRead == nil {
		t.Fatal("authorized file_read tool was not offered")
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(fileRead.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	if _, exists := schema.Properties["workspace_id"]; exists || containsString(schema.Required, "workspace_id") {
		t.Fatalf("file_read still lets the model choose a Workspace: %s", fileRead.Parameters)
	}

	mapping, ok := runtimeToolMappingByName(runtimeToolFileRead)
	if !ok {
		t.Fatal("file_read mapping is missing")
	}
	call := ModelToolCall{CallID: "deferred-file-read", Name: runtimeToolFileRead,
		Arguments: `{"workspace_id":"w7654321","path":"README.md"}`}
	bound, resolvedCall, reason, err := broker.bindDeferredRuntimeTarget(context.Background(), claimed, deferred.target, mapping, call)
	if err != nil || reason != "" || bound.HostID != deferred.target.HostID || bound.WorkspaceID != original.WorkspaceID ||
		deferred.resolveCalls != 1 || deferred.workspaceReads == 0 || deferred.bindCalls != 1 {
		t.Fatalf("deferred Workspace binding failed: task=%+v call=%+v reason=%q resolve=%d reads=%d binds=%d err=%v",
			bound, resolvedCall, reason, deferred.resolveCalls, deferred.workspaceReads, deferred.bindCalls, err)
	}
	var arguments map[string]json.RawMessage
	if err := json.Unmarshal([]byte(resolvedCall.Arguments), &arguments); err != nil {
		t.Fatal(err)
	}
	var injectedWorkspaceID string
	if err := json.Unmarshal(arguments["workspace_id"], &injectedWorkspaceID); err != nil || injectedWorkspaceID != original.WorkspaceID {
		t.Fatalf("server did not replace model Workspace ID: arguments=%s decoded=%q err=%v", resolvedCall.Arguments, injectedWorkspaceID, err)
	}
	refreshed, _, found, err := broker.refreshDeferredTask(context.Background(), claimed)
	if err != nil || !found || refreshed.HostID != deferred.target.HostID || refreshed.WorkspaceID != original.WorkspaceID {
		t.Fatalf("server-resolved target was rejected on the next turn: task=%+v found=%t err=%v", refreshed, found, err)
	}
}

func TestDeferredProjectFileToolRequiresExplicitWorkspace(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	base := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	deferred := &deferredBrokerTestStore{cloudDSHBrokerTaskStore: base, target: DeferredTaskTarget{HostID: "h1234567"}}
	fixture.executor.store = deferred
	deferred.details.Task.ProjectID = "j1234567"
	deferred.details.Task.HostID, deferred.details.Task.WorkspaceID = "", ""
	deferred.details.Task.InputRevision, deferred.details.Task.WorkspaceCheckpointRevision = nil, nil
	claimed := deferred.details.Task
	broker, err := NewCloudDSHToolBroker(deferred, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	mapping, _ := runtimeToolMappingByName(runtimeToolFileRead)
	bound, _, reason, err := broker.bindDeferredRuntimeTarget(context.Background(), claimed, deferred.target, mapping,
		ModelToolCall{CallID: "project-file-read", Name: runtimeToolFileRead, Arguments: `{"path":"README.md"}`})
	if err != nil || reason != "workspace_unavailable" || deferred.resolveCalls != 0 || deferred.bindCalls != 0 || bound.ID != "" {
		t.Fatalf("project Workspace was derived from Host: task=%+v reason=%q resolver_calls=%d binds=%d err=%v",
			bound, reason, deferred.resolveCalls, deferred.bindCalls, err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestDeferredComputerExplainsChangedBoundTarget(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	base := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	store := &deferredBrokerTestStore{cloudDSHBrokerTaskStore: base, target: DeferredTaskTarget{HostID: "h7654321"}}
	fixture.executor.store = store
	store.details.Task.WorkspaceID = ""
	store.details.Task.InputRevision = nil
	store.details.Task.WorkspaceCheckpointRevision = nil
	store.details.Task.ExecutionLocation = "server"
	store.details.Task.AgentInstanceID = "agent-deferred"
	store.details.Task.AgentGeneration = 1
	store.details.Task.AgentSessionID = "session-deferred"
	claimed := store.details.Task
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := broker.Execute(context.Background(), claimed, ModelToolCall{CallID: "changed-target", Name: runtimeToolWindowList, Arguments: `{}`})
	if err != nil || result.Status != AgentToolNeedsUser || result.WaitingReason != "computer_target_changed" || store.bindCalls != 0 {
		t.Fatalf("changed target was used without review: result=%+v bindings=%d err=%v", result, store.bindCalls, err)
	}
}

func TestDeferredWorkspaceWorksWithoutAComputerAndKeepsItsOriginalRevision(t *testing.T) {
	fixture, base, broker, workspaceStore, original := newCloudDSHT08WorkspaceBroker(t)
	deferred := &deferredWorkspaceBrokerTestStore{deferredBrokerTestStore: &deferredBrokerTestStore{
		cloudDSHBrokerTaskStore: base, target: DeferredTaskTarget{HostID: "h1234567", WorkspaceID: original.WorkspaceID},
	}, workspaceStore: workspaceStore}
	fixture.executor.store, broker.store = deferred, deferred
	base.details.Task.Authorization.Kind = string(identity.KindSession)
	base.details.Task.WorkspaceID = ""
	base.details.Task.InputRevision, base.details.Task.WorkspaceCheckpointRevision, base.details.Task.ContextDigest = nil, nil, nil
	claimed := base.details.Task
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil || !containsAgentTool(definitions, cloudDSHWorkspaceListToolName) || deferred.workspaceReads != 0 || deferred.workspaceBinds != 0 {
		t.Fatalf("Workspace was consulted before use: definitions=%+v reads=%d binds=%d err=%v", definitions, deferred.workspaceReads, deferred.workspaceBinds, err)
	}
	call := ModelToolCall{CallID: "deferred-workspace-list", Name: cloudDSHWorkspaceListToolName, Arguments: `{"expected_revision":-1}`}
	result, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || result.Status != AgentToolSucceeded || !strings.Contains(result.Output, `"revision":0`) ||
		deferred.workspaceBinds != 1 || base.details.Task.HostID != "" {
		t.Fatalf("hostless Workspace call failed: result=%+v task=%+v binds=%d err=%v", result, base.details.Task, deferred.workspaceBinds, err)
	}
	advanced := int64(2)
	base.details.Task.WorkspaceCheckpointRevision = &advanced
	resolvedTask, resolved, reason, err := broker.prepareDeferredWorkspaceCall(context.Background(), base.details.Task, deferred.target, call, true, &result.Output)
	if err != nil || reason != "" || resolvedTask.InputRevision == nil || *resolvedTask.InputRevision != 0 ||
		!strings.Contains(resolved.Arguments, `"expected_revision":0`) {
		t.Fatalf("retry changed the original Workspace revision: call=%+v reason=%q err=%v", resolved, reason, err)
	}
}

func TestProjectDeferredWorkspaceReadsAndBindsInsideSelectionGuard(t *testing.T) {
	for _, deny := range []bool{false, true} {
		name := "authorized"
		if deny {
			name = "revoked"
		}
		t.Run(name, func(t *testing.T) {
			fixture, base, broker, workspaceStore, original := newCloudDSHT08WorkspaceBroker(t)
			workspaceStore.workspace.OwnerID = "u7654321"
			deferred := &projectDeferredWorkspaceBrokerTestStore{deferredWorkspaceBrokerTestStore: &deferredWorkspaceBrokerTestStore{
				deferredBrokerTestStore: &deferredBrokerTestStore{cloudDSHBrokerTaskStore: base,
					target: DeferredTaskTarget{WorkspaceID: original.WorkspaceID}}, workspaceStore: workspaceStore},
				denySelection: deny}
			fixture.executor.store, broker.store = deferred, deferred
			base.details.Task.ProjectID = "j1234567"
			base.details.Task.Authorization.Kind = string(identity.KindSession)
			base.details.Task.WorkspaceID = ""
			base.details.Task.InputRevision, base.details.Task.WorkspaceCheckpointRevision, base.details.Task.ContextDigest = nil, nil, nil
			claimed := base.details.Task
			call := ModelToolCall{CallID: "project-deferred-workspace-list", Name: cloudDSHWorkspaceListToolName,
				Arguments: `{"expected_revision":-1}`}
			bound, _, reason, err := broker.prepareDeferredWorkspaceCall(context.Background(), claimed, deferred.target, call, false, nil)
			if deferred.selectionCalls != 1 || deferred.unguardedAccess != 0 {
				t.Fatalf("project Workspace was accessed outside its selection guard: selections=%d unguarded=%d",
					deferred.selectionCalls, deferred.unguardedAccess)
			}
			if deny {
				if err != nil || reason != "workspace_unavailable" || deferred.workspaceReads != 0 || deferred.workspaceBinds != 0 {
					t.Fatalf("revoked selection read or bound the Workspace: reads=%d binds=%d reason=%q err=%v",
						deferred.workspaceReads, deferred.workspaceBinds, reason, err)
				}
				return
			}
			if err != nil || reason != "" || bound.WorkspaceOwnerID != workspaceStore.workspace.OwnerID ||
				bound.WorkspaceProjectResourceVersion == nil || deferred.workspaceBinds != 1 {
				t.Fatalf("authorized project Workspace was not bound: bound=%+v reason=%q binds=%d err=%v",
					bound, reason, deferred.workspaceBinds, err)
			}
		})
	}
}

func TestDeferredWorkspaceMissingTargetPausesOnlyWhenUsed(t *testing.T) {
	fixture, base, broker, workspaceStore, _ := newCloudDSHT08WorkspaceBroker(t)
	deferred := &deferredWorkspaceBrokerTestStore{deferredBrokerTestStore: &deferredBrokerTestStore{
		cloudDSHBrokerTaskStore: base,
	}, workspaceStore: workspaceStore}
	fixture.executor.store, broker.store = deferred, deferred
	base.details.Task.Authorization.Kind = string(identity.KindSession)
	base.details.Task.WorkspaceID = ""
	base.details.Task.InputRevision, base.details.Task.WorkspaceCheckpointRevision, base.details.Task.ContextDigest = nil, nil, nil
	claimed := base.details.Task
	if _, err := broker.Definitions(context.Background(), claimed); err != nil || deferred.workspaceReads != 0 {
		t.Fatalf("unselected Workspace blocked model startup: reads=%d err=%v", deferred.workspaceReads, err)
	}
	result, err := broker.Execute(context.Background(), claimed, ModelToolCall{CallID: "missing-workspace", Name: cloudDSHWorkspaceListToolName,
		Arguments: `{"expected_revision":-1}`})
	if err != nil || result.Status != AgentToolNeedsUser || result.WaitingReason != "workspace_unavailable" || deferred.workspaceBinds != 0 {
		t.Fatalf("missing Workspace did not pause only the related task: result=%+v binds=%d err=%v", result, deferred.workspaceBinds, err)
	}
}

func TestDeferredWorkspaceRequiresToolScopeBeforeReading(t *testing.T) {
	fixture, base, broker, workspaceStore, original := newCloudDSHT08WorkspaceBroker(t)
	deferred := &deferredWorkspaceBrokerTestStore{deferredBrokerTestStore: &deferredBrokerTestStore{
		cloudDSHBrokerTaskStore: base, target: DeferredTaskTarget{WorkspaceID: original.WorkspaceID},
	}, workspaceStore: workspaceStore}
	fixture.executor.store, broker.store = deferred, deferred
	base.details.Task.WorkspaceID = ""
	base.details.Task.InputRevision, base.details.Task.WorkspaceCheckpointRevision, base.details.Task.ContextDigest = nil, nil, nil
	base.details.Task.Authorization.Kind = string(identity.KindClient)
	base.details.Task.Authorization.Scopes = []string{string(identity.ScopeTaskSubmit)}
	claimed := base.details.Task
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil || containsAgentTool(definitions, cloudDSHWorkspaceListToolName) {
		t.Fatalf("unauthorized Workspace read tool was offered: definitions=%+v err=%v", definitions, err)
	}
	_, err = broker.Execute(context.Background(), claimed, ModelToolCall{CallID: "unscoped-workspace", Name: cloudDSHWorkspaceListToolName,
		Arguments: `{"expected_revision":-1}`})
	if domain.CodeOf(err) != domain.CodeForbidden || deferred.workspaceReads != 0 || deferred.workspaceBinds != 0 {
		t.Fatalf("unauthorized Workspace was consulted: reads=%d binds=%d err=%v", deferred.workspaceReads, deferred.workspaceBinds, err)
	}
}
