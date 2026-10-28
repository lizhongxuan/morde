package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type cloudDSHBrokerTaskStore struct {
	*runtimeToolTaskStore
	events []task.Event
}

func (s *cloudDSHBrokerTaskStore) CommitTaskProgress(ctx context.Context, progress TaskProgress) (task.Event, error) {
	if err := ctx.Err(); err != nil {
		return task.Event{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := &s.details.Task
	if current.ID != progress.TaskID || current.Lease.Owner != progress.LeaseOwner || current.Lease.Version != progress.LeaseVersion {
		return task.Event{}, errors.New("stale task progress")
	}
	if read := progress.CloudDSHSkillRead; read != nil {
		if current.ID != read.TaskID || current.OwnerID != read.OwnerID || current.AgentSessionID != read.AgentSessionID ||
			current.Authorization.Version != read.AuthorizationVersion || read.FingerprintSHA256 != cloudDSHSkillReadFingerprint(*read) {
			return task.Event{}, errors.New("stale skill read")
		}
		for _, event := range s.events {
			var payload struct {
				Detail map[string]json.RawMessage `json:"detail"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || string(payload.Detail["skill_call_id"]) != `"`+read.CallID+`"` {
				continue
			}
			var stored struct {
				SessionID         string `json:"agent_session_id"`
				SkillID           string `json:"skill_id"`
				SkillVersion      string `json:"skill_version"`
				BundleVersion     string `json:"bundle_version"`
				SourceRef         string `json:"source_ref"`
				ContentSHA256     string `json:"content_sha256"`
				OutputSHA256      string `json:"output_sha256"`
				FingerprintSHA256 string `json:"fingerprint_sha256"`
			}
			if json.Unmarshal(payload.Detail["agent_session_id"], &stored.SessionID) != nil ||
				json.Unmarshal(payload.Detail["skill_id"], &stored.SkillID) != nil ||
				json.Unmarshal(payload.Detail["skill_version"], &stored.SkillVersion) != nil ||
				json.Unmarshal(payload.Detail["bundle_version"], &stored.BundleVersion) != nil ||
				json.Unmarshal(payload.Detail["source_ref"], &stored.SourceRef) != nil ||
				json.Unmarshal(payload.Detail["content_sha256"], &stored.ContentSHA256) != nil ||
				json.Unmarshal(payload.Detail["output_sha256"], &stored.OutputSHA256) != nil ||
				json.Unmarshal(payload.Detail["fingerprint_sha256"], &stored.FingerprintSHA256) != nil ||
				stored.SessionID != read.AgentSessionID || stored.SkillID != read.SkillID || stored.SkillVersion != read.SkillVersion ||
				stored.BundleVersion != read.BundleVersion || stored.SourceRef != read.SourceRef ||
				stored.ContentSHA256 != read.ContentSHA256 || stored.OutputSHA256 != read.OutputSHA256 ||
				stored.FingerprintSHA256 != read.FingerprintSHA256 {
				return task.Event{}, errors.New("skill replay fingerprint changed")
			}
			return event, nil
		}
	}
	current.Status, current.CurrentStep, current.Result = progress.Status, progress.CurrentStep, progress.Result
	current.WaitingReason, current.Usage = progress.WaitingReason, progress.Usage
	current.EventSequence++
	event := progress.Event
	event.TaskID, event.Sequence = progress.TaskID, current.EventSequence
	for _, step := range progress.Steps {
		found := false
		for index := range s.details.Steps {
			if s.details.Steps[index].ID == step.ID {
				s.details.Steps[index], found = step, true
				break
			}
		}
		if !found {
			s.details.Steps = append(s.details.Steps, step)
		}
	}
	if progress.PendingRequest != nil {
		s.details.PendingRequest = progress.PendingRequest
	}
	if progress.ClarificationRequest != nil {
		s.details.ClarificationRequests = append(s.details.ClarificationRequests, *progress.ClarificationRequest)
	}
	if progress.Status != task.StatusRunning {
		current.Lease = task.Lease{}
	}
	s.events = append(s.events, event)
	return event, nil
}

func TestCloudDSHToolBrokerReadsFixedNanoMuseSkillAndReplaysWithoutRecharging(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.ExecutionLocation = "server"
	store.details.Task.AgentInstanceID = "agent_test"
	store.details.Task.AgentGeneration = 1
	store.details.Task.AgentSessionID = "session_test"
	store.details.Task.Authorization.TokenID = "t1234567"
	store.details.Task.Authorization.Version = 1
	authorizationExpiry := time.Now().Add(time.Hour).UTC()
	store.details.Task.Authorization.ExpiresAt = &authorizationExpiry
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, definition := range definitions {
		if definition.Name == cloudDSHSkillReadToolName {
			found = true
			if definition.RequiredScope != identity.ScopeTaskRead || !validAgentJSONObject(definition.Parameters, maxAgentToolSchemaBytes) {
				t.Fatalf("skill tool definition is invalid: %#v", definition)
			}
		}
	}
	if !found {
		t.Fatal("fixed nanoMuse skill was not exposed to a running server task")
	}
	call := ModelToolCall{CallID: "call_skill_1", Name: cloudDSHSkillReadToolName, Arguments: `{"skill_id":"compare-options"}`}
	first, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || first.Status != AgentToolSucceeded {
		t.Fatalf("read nanoMuse skill = %#v, err=%v", first, err)
	}
	var output cloudDSHSkillReadOutput
	if err := json.Unmarshal([]byte(first.Output), &output); err != nil || output.SkillID != "compare-options" ||
		output.Version != "1" || output.BundleVersion != cloudDSHSkillBundleVersion ||
		output.SourceRef != "project/nanoMuse/nanomuse/skills/builtin/compare-options/SKILL.md" ||
		output.ExecutionNotes == "" || output.Content != string(mustReadEmbeddedSkill(t)) || len(first.Output) > maxAgentToolOutputBytes {
		t.Fatalf("skill result metadata/content/size is invalid: %#v, err=%v", output, err)
	}
	contentHash := sha256.Sum256([]byte(output.Content))
	if output.ContentSHA256 != hex.EncodeToString(contentHash[:]) || store.details.Task.Usage.ToolCalls != 1 || len(store.events) != 1 {
		t.Fatalf("skill result hash or first-call budget/event is invalid: hash=%s usage=%+v events=%d",
			output.ContentSHA256, store.details.Task.Usage, len(store.events))
	}
	if strings.Contains(string(store.events[0].Payload), output.Content) {
		t.Fatal("skill body was duplicated into the client-visible Task event")
	}
	var eventPayload struct {
		Detail map[string]json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(store.events[0].Payload, &eventPayload) != nil ||
		string(eventPayload.Detail["agent_session_id"]) != `"session_test"` ||
		string(eventPayload.Detail["skill_id"]) != `"compare-options"` ||
		string(eventPayload.Detail["content_sha256"]) != `"`+output.ContentSHA256+`"` {
		t.Fatalf("skill source metadata missing from Task event: %s", store.events[0].Payload)
	}
	replayed, err := broker.Reconcile(context.Background(), claimed, call, &first.Output)
	if err != nil || replayed.Output != first.Output || replayed.Status != AgentToolSucceeded ||
		store.details.Task.Usage.ToolCalls != 1 || len(store.events) != 1 {
		t.Fatalf("skill replay = %#v, err=%v, usage=%+v events=%d", replayed, err, store.details.Task.Usage, len(store.events))
	}
	wrongOutput := first.Output + " "
	if _, err := broker.Reconcile(context.Background(), claimed, call, &wrongOutput); err == nil ||
		store.details.Task.Usage.ToolCalls != 1 || len(store.events) != 1 {
		t.Fatalf("mismatched checkpoint output was accepted or charged: err=%v usage=%+v events=%d", err,
			store.details.Task.Usage, len(store.events))
	}
	unknown := ModelToolCall{CallID: "call_skill_bad", Name: cloudDSHSkillReadToolName, Arguments: `{"skill_id":"../compare-options"}`}
	if _, err := broker.Execute(context.Background(), claimed, unknown); domain.CodeOf(err) != domain.CodeForbidden ||
		store.details.Task.Usage.ToolCalls != 1 || len(store.events) != 1 {
		t.Fatalf("unknown skill was not rejected: err=%v usage=%+v events=%d", err, store.details.Task.Usage, len(store.events))
	}
}

func TestCloudDSHToolBrokerLoadsFixedSkillForHostlessServerTask(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.HostID = ""
	store.details.Task.ExecutionLocation = "server"
	store.details.Task.AgentInstanceID = "agent_hostless"
	store.details.Task.AgentGeneration = 1
	store.details.Task.AgentSessionID = "session_hostless"
	store.details.Task.Authorization.TokenID = "t1234567"
	store.details.Task.Authorization.Version = 1
	expires := time.Now().Add(time.Hour).UTC()
	store.details.Task.Authorization.ExpiresAt = &expires
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 2 || definitions[0].Name != cloudDSHSkillReadToolName || definitions[1].Name != agentAskUserToolName {
		t.Fatalf("hostless server task definitions = %#v; want the fixed skill and ask_user", definitions)
	}
	result, err := broker.Execute(context.Background(), claimed, ModelToolCall{
		CallID: "call_hostless_skill", Name: cloudDSHSkillReadToolName, Arguments: `{"skill_id":"compare-options"}`,
	})
	if err != nil || result.Status != AgentToolSucceeded || !strings.Contains(result.Output, `"skill_id":"compare-options"`) ||
		store.details.Task.Usage.ToolCalls != 1 || len(store.events) != 1 {
		t.Fatalf("hostless server skill read = %#v, err=%v, usage=%+v events=%d", result, err,
			store.details.Task.Usage, len(store.events))
	}
}

func TestCloudDSHSkillCatalogEmbedsEveryBuiltinWithFixedMetadata(t *testing.T) {
	definition := cloudDSHSkillReadDefinition()
	if len(cloudDSHSkillCatalog) != 11 || len(definition.Description) > 2048 || !strings.Contains(definition.Description, "weekly-review:") ||
		!strings.Contains(definition.Description, "amap:") {
		t.Fatalf("skill catalog does not expose all built-ins: %d, %s", len(cloudDSHSkillCatalog), definition.Description)
	}
	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	var toolIDs []string
	for _, skill := range cloudDSHSkillCatalog {
		metadata, ok := CloudDSHSkillMetadataForID(skill.id)
		if !ok || metadata.ID != skill.id || metadata.Version != skill.version ||
			metadata.SourceRef != "project/nanoMuse/nanomuse/skills/builtin/"+skill.id+"/SKILL.md" {
			t.Fatalf("skill metadata is not fixed for %q: %#v", skill.id, metadata)
		}
		content, err := cloudDSHSkillBundle.ReadFile("cloud_dsh_skills/" + skill.id + ".md")
		if err != nil || len(content) == 0 || !utf8.Valid(content) {
			t.Fatalf("embedded skill %q is unavailable: %v", skill.id, err)
		}
		toolIDs = append(toolIDs, skill.id)
	}
	if len(schema.Properties["skill_id"].Enum) != len(toolIDs) {
		t.Fatalf("skill tool allowlist differs from the embedded catalog: %#v", schema.Properties)
	}
	for index, id := range toolIDs {
		if schema.Properties["skill_id"].Enum[index] != id {
			t.Fatalf("skill tool allowlist differs from the embedded catalog: %#v", schema.Properties)
		}
	}
}

func mustReadEmbeddedSkill(t *testing.T) []byte {
	t.Helper()
	skill, ok := cloudDSHSkillForID("compare-options")
	if !ok {
		t.Fatal("compare-options skill is absent from the fixed allowlist")
	}
	content, err := cloudDSHSkillBundle.ReadFile(skill.resource)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func TestCloudDSHToolBrokerPersistsAndReconcilesClarification(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.HostID = ""
	store.details.Task.Usage.ToolCalls = 3
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := broker.Definitions(context.Background(), store.details.Task)
	if err != nil || len(definitions) != 1 || definitions[0].Name != agentAskUserToolName {
		t.Fatalf("hostless definitions = %#v, err=%v; want only ask_user", definitions, err)
	}
	claimed := store.details.Task
	call := ModelToolCall{CallID: "call_dsh_clarify", Name: agentAskUserToolName,
		Arguments: `{"question":"Which environment should I use?"}`}
	first, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || first.Status != AgentToolNeedsUser {
		t.Fatalf("clarification result = %#v, err=%v; want waiting user", first, err)
	}
	details, err := store.GetTask(context.Background(), claimed.ID, claimed.OwnerID)
	if err != nil || details.Task.Status != task.StatusWaitingUser || details.Task.WaitingReason != "clarification_required" ||
		details.Task.Lease.Owner != "" || details.Task.Usage.ToolCalls != 4 || len(details.ClarificationRequests) != 1 || len(details.Steps) != 2 {
		t.Fatalf("persisted clarification state = %#v, err=%v", details, err)
	}
	request := details.ClarificationRequests[0]
	if request.TaskID != claimed.ID || request.CallID != call.CallID || request.Question != "Which environment should I use?" ||
		request.Status != task.ClarificationPending || request.ModelStepID != details.Steps[0].ID ||
		details.Steps[0].Kind != "model" || details.Steps[1].Kind != "clarification" || details.Steps[1].Status != task.StepWaitingUser ||
		len(store.events) != 1 || store.events[0].Type != "task.waiting_user" {
		t.Fatalf("clarification request/steps/event = %#v / %#v / %#v", request, details.Steps, store.events)
	}
	pending, err := broker.Reconcile(context.Background(), claimed, call, nil)
	if err != nil || pending.Status != AgentToolNeedsUser || len(store.operations) != 0 {
		t.Fatalf("pending clarification reconcile = %#v, err=%v; operations=%#v", pending, err, store.operations)
	}
	store.mu.Lock()
	responded := time.Now().UTC()
	store.details.ClarificationRequests[0].Status = task.ClarificationAnswered
	store.details.ClarificationRequests[0].Answer = "Use staging."
	store.details.ClarificationRequests[0].RespondedAt = &responded
	expires := time.Now().Add(time.Minute).UTC()
	store.details.Task.Status = task.StatusRunning
	store.details.Task.WaitingReason = ""
	store.details.Task.Lease = task.Lease{Owner: "worker-2", Version: claimed.Lease.Version + 1, ExpiresAt: &expires}
	resumed := store.details.Task
	store.mu.Unlock()
	answer, err := broker.Reconcile(context.Background(), resumed, call, nil)
	if err != nil || answer.Status != AgentToolSucceeded || answer.Output != "Use staging." ||
		store.details.Task.Usage.ToolCalls != 4 || len(store.operations) != 0 {
		t.Fatalf("answered clarification reconcile = %#v, err=%v; usage=%d operations=%#v",
			answer, err, store.details.Task.Usage.ToolCalls, store.operations)
	}
}

func TestCloudDSHToolBrokerSensitiveApprovalResumesExactCallOnce(t *testing.T) {
	fixture, store, broker, call := newCloudDSHApprovalFixture(t)
	first, err := broker.Execute(context.Background(), store.details.Task, call)
	if err != nil || first.Status != AgentToolNeedsUser {
		t.Fatalf("initial sensitive call = %#v, %v; want approval pause", first, err)
	}
	if store.details.Task.Usage.ToolCalls != 1 || len(fixture.store.operations) != 0 || fixture.handlers["shell.exec"].calls.Load() != 0 {
		t.Fatalf("initial approval state: usage=%+v operations=%d dispatches=%d", store.details.Task.Usage,
			len(fixture.store.operations), fixture.handlers["shell.exec"].calls.Load())
	}
	pending := store.details.PendingRequest
	if pending == nil || pending.Status != task.PendingRequestPending || pending.CallID != call.CallID || pending.ToolName != call.Name {
		t.Fatalf("sensitive call was not bound to a pending approval: %#v", pending)
	}
	if strings.Contains(pending.Object+pending.Content+pending.Impact, "echo approved") {
		t.Fatalf("approval text contains raw tool arguments: %#v", pending)
	}
	resumeCloudDSHApproval(fixture, store, task.PendingRequestApproved, task.PendingDecisionApprove)
	result, err := broker.Reconcile(context.Background(), store.details.Task, call, nil)
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("approved exact call = %#v, %v; operations=%#v dispatches=%d task=%+v; want success", result, err,
			fixture.store.operations, fixture.handlers["shell.exec"].calls.Load(), store.details.Task)
	}
	if store.details.Task.Usage.ToolCalls != 1 || fixture.handlers["shell.exec"].calls.Load() != 1 {
		t.Fatalf("approved resume was not once-only: usage=%+v dispatches=%d", store.details.Task.Usage,
			fixture.handlers["shell.exec"].calls.Load())
	}
}

func TestCloudDSHToolBrokerRejectedApprovalReturnsWithoutDispatch(t *testing.T) {
	fixture, store, broker, call := newCloudDSHApprovalFixture(t)
	first, err := broker.Execute(context.Background(), store.details.Task, call)
	if err != nil || first.Status != AgentToolNeedsUser {
		t.Fatalf("initial sensitive call = %#v, %v; want approval pause", first, err)
	}
	resumeCloudDSHApproval(fixture, store, task.PendingRequestRejected, task.PendingDecisionReject)
	result, err := broker.Reconcile(context.Background(), store.details.Task, call, nil)
	if err != nil || result.Status != AgentToolFailed || result.Output != "The user rejected this exact action. It was not executed." {
		t.Fatalf("rejected exact call = %#v, %v; want rejection tool result", result, err)
	}
	if store.details.Task.Usage.ToolCalls != 1 || fixture.handlers["shell.exec"].calls.Load() != 0 || len(fixture.store.operations) != 0 {
		t.Fatalf("rejected action crossed dispatch or counted twice: usage=%+v dispatches=%d operations=%d", store.details.Task.Usage,
			fixture.handlers["shell.exec"].calls.Load(), len(fixture.store.operations))
	}
}

func TestCloudDSHToolBrokerReapprovalOnChangedApprovalFenceDoesNotRecount(t *testing.T) {
	fixture, store, broker, call := newCloudDSHApprovalFixture(t)
	first, err := broker.Execute(context.Background(), store.details.Task, call)
	if err != nil || first.Status != AgentToolNeedsUser {
		t.Fatalf("initial sensitive call = %#v, %v; want approval pause", first, err)
	}
	resumeCloudDSHApproval(fixture, store, task.PendingRequestApproved, task.PendingDecisionApprove)
	store.mu.Lock()
	store.details.PendingRequest.RuntimeGeneration++ // Simulate an approval bound to the previous Host generation.
	store.mu.Unlock()
	result, err := broker.Reconcile(context.Background(), store.details.Task, call, nil)
	if err != nil || result.Status != AgentToolNeedsUser {
		t.Fatalf("stale approval resume = %#v, %v; want a fresh approval pause", result, err)
	}
	pending := store.details.PendingRequest
	if pending == nil || pending.Status != task.PendingRequestPending || pending.Version != 2 ||
		store.details.Task.Usage.ToolCalls != 1 || fixture.handlers["shell.exec"].calls.Load() != 0 || len(fixture.store.operations) != 0 {
		t.Fatalf("changed fence did not reapprove without recount/dispatch: pending=%#v usage=%+v dispatches=%d operations=%d",
			pending, store.details.Task.Usage, fixture.handlers["shell.exec"].calls.Load(), len(fixture.store.operations))
	}
}

func newCloudDSHApprovalFixture(t *testing.T) (*runtimeToolExecutorFixture, *cloudDSHBrokerTaskStore, *CloudDSHToolBroker, ModelToolCall) {
	t.Helper()
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "shell.exec", action: "run",
		result: `{"exit_code":0,"stdout":"ok","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`,
	}})
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelToolCall{CallID: "call_dsh_sensitive", Name: runtimeToolShellExec,
		Arguments: `{"workspace_id":"w1234567","command":"echo approved"}`}
	return fixture, store, broker, call
}

func resumeCloudDSHApproval(fixture *runtimeToolExecutorFixture, store *cloudDSHBrokerTaskStore, status task.PendingRequestStatus, decision task.PendingDecision) {
	store.mu.Lock()
	defer store.mu.Unlock()
	expires := time.Now().Add(time.Minute).UTC()
	store.details.Task.Status = task.StatusRunning
	store.details.Task.WaitingReason = ""
	store.details.Task.Lease = task.Lease{Owner: "worker-2", Version: 2, ExpiresAt: &expires}
	store.details.PendingRequest.Status = status
	store.details.PendingRequest.Decision = decision
	fixture.fences.fence = AgentHostFence{
		RuntimeGeneration: store.details.PendingRequest.RuntimeGeneration,
		RuntimeInstanceID: store.details.PendingRequest.RuntimeInstanceID,
		ControlVersion:    store.details.PendingRequest.ControlVersion,
		AIAllowed:         true,
	}
}

func TestCloudDSHToolBrokerPausesClarificationAtToolCallBudget(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.HostID = ""
	store.details.Task.Budget.ToolCalls = 1
	store.details.Task.Usage.ToolCalls = 1
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := broker.Execute(context.Background(), store.details.Task, ModelToolCall{
		CallID: "call_dsh_budget", Name: agentAskUserToolName,
		Arguments: `{"question":"Which environment should I use?"}`,
	})
	if err != nil || result.Status != AgentToolNeedsUser || store.details.Task.Status != task.StatusWaitingUser ||
		store.details.Task.WaitingReason != "tool_call_budget" || store.details.Task.Usage.ToolCalls != 1 ||
		store.details.Task.Lease.Owner != "" || len(store.details.ClarificationRequests) != 0 ||
		len(store.details.Steps) != 1 || store.details.Steps[0].Kind != "budget" || len(store.events) != 1 ||
		store.events[0].Type != "task.waiting_user" || len(store.operations) != 0 {
		t.Fatalf("budget-paused clarification = %#v, err=%v; task=%#v requests=%#v steps=%#v operations=%#v",
			result, err, store.details.Task, store.details.ClarificationRequests, store.details.Steps, store.operations)
	}
}

func TestCloudDSHToolBrokerPausesForWorkspaceInputChange(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "file.read", action: "read", result: `{"path":"README.md","text":"hello"}`,
	}})
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	setRuntimeToolWorkspaceRevision(t, fixture, 1)
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := broker.Execute(context.Background(), store.details.Task,
		ModelToolCall{CallID: "call_workspace_changed", Name: runtimeToolFileRead, Arguments: `{"workspace_id":"w1234567","path":"README.md"}`})
	if err != nil || result.Status != AgentToolNeedsUser || result.WaitingReason != "workspace_input_changed" ||
		store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "workspace_input_changed" ||
		len(store.operations) != 0 || fixture.handlers["file.read"].calls.Load() != 0 {
		t.Fatalf("workspace conflict result=%#v err=%v task=%#v operations=%d dispatches=%d; want pause before operation admission",
			result, err, store.details.Task, len(store.operations), fixture.handlers["file.read"].calls.Load())
	}
}

func TestCloudDSHToolBrokerDoesNotReuseStaleOrMismatchedClarificationAnswer(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		callID    string
		question  string
		status    task.ClarificationStatus
		answer    string
		duplicate bool
	}{
		{name: "wrong call id", callID: "call_dsh_other", question: "Which environment?", status: task.ClarificationAnswered, answer: "Use staging."},
		{name: "changed question", callID: "call_dsh_clarify", question: "Which other environment?", status: task.ClarificationAnswered, answer: "Use staging."},
		{name: "cancelled request", callID: "call_dsh_clarify", question: "Which environment?", status: task.ClarificationCancelled},
		{name: "duplicate matching requests", callID: "call_dsh_clarify", question: "Which environment?", status: task.ClarificationAnswered, answer: "Use staging.", duplicate: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			expires := time.Now().Add(time.Minute).UTC()
			responded := time.Now().UTC()
			store.details.Task.Status = task.StatusRunning
			store.details.Task.Lease = task.Lease{Owner: "worker-2", Version: 2, ExpiresAt: &expires}
			request := task.ClarificationRequest{
				ID: "r1234567", TaskID: store.details.Task.ID, Version: 1, ModelStepID: "s1234567",
				CallID: "call_dsh_clarify", Question: "Which environment?", Status: scenario.status,
				Answer: scenario.answer,
			}
			if scenario.status != task.ClarificationPending {
				request.RespondedAt = &responded
			}
			store.details.ClarificationRequests = []task.ClarificationRequest{request}
			if scenario.duplicate {
				duplicate := request
				duplicate.ID, duplicate.Version = "r2234567", 2
				store.details.ClarificationRequests = append(store.details.ClarificationRequests, duplicate)
			}
			call := ModelToolCall{CallID: scenario.callID, Name: agentAskUserToolName,
				Arguments: `{"question":"` + scenario.question + `"}`}
			result, err := broker.Reconcile(context.Background(), store.details.Task, call, nil)
			wantClarifications := 1
			if scenario.duplicate {
				wantClarifications = 2
			}
			if err != nil || result.Status != AgentToolUnknown || result.Output != "" || len(store.details.ClarificationRequests) != wantClarifications || len(store.operations) != 0 {
				t.Fatalf("stale clarification result = %#v, err=%v; clarifications=%#v operations=%#v",
					result, err, store.details.ClarificationRequests, store.operations)
			}
		})
	}
}

func TestCloudDSHToolBrokerClarificationDoesNotRunHostTool(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "window.list", action: "list", result: `{"windows":[]}`,
	}})
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	session, err := fixture.executor.connect(context.Background(), store.details.Task, fixture.fence)
	if err != nil {
		t.Fatal(err)
	}
	fixture.fences.fence = agentFenceFromRuntime(session.Fence)
	session.Close()
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := broker.Definitions(context.Background(), store.details.Task)
	if err != nil {
		t.Fatal(err)
	}
	foundClarification := false
	for _, definition := range definitions {
		foundClarification = foundClarification || definition.Name == agentAskUserToolName
	}
	if !foundClarification {
		t.Fatal("Host task did not expose ask_user")
	}
	result, err := broker.Execute(context.Background(), store.details.Task, ModelToolCall{
		CallID: "call_dsh_host_clarify", Name: agentAskUserToolName,
		Arguments: `{"question":"Which environment should I use?"}`,
	})
	if err != nil || result.Status != AgentToolNeedsUser || fixture.handlers["window.list"].calls.Load() != 0 || len(store.operations) != 0 {
		t.Fatalf("Host clarification = %#v, err=%v; host calls=%d operations=%#v",
			result, err, fixture.handlers["window.list"].calls.Load(), store.operations)
	}
}

func TestCloudDSHToolBrokerUsesAuthorizedHostOperationLedger(t *testing.T) {
	var screenshot bytes.Buffer
	if err := jpeg.Encode(&screenshot, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	screenshotResult := `{"mime_type":"image/jpeg","width":2,"height":2,"data_base64":"` + base64.StdEncoding.EncodeToString(screenshot.Bytes()) + `"}`
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{
		{capability: "file.read", action: "read", result: `{"path":"README.md","text":"from Host"}`},
		{capability: "desktop.observe", action: "screenshot", result: screenshotResult},
		{capability: "window.list", action: "list", result: `{"windows":[{"id":"0x1","title":"Editor","x":0,"y":0,"width":100,"height":100,"active":true}]}`},
	})
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	session, err := fixture.executor.connect(context.Background(), store.details.Task, fixture.fence)
	if err != nil {
		t.Fatal(err)
	}
	fixture.fences.fence = agentFenceFromRuntime(session.Fence)
	session.Close()
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := broker.Definitions(context.Background(), fixture.store.details.Task)
	if err != nil {
		t.Fatal(err)
	}
	fileRead := false
	windowList := false
	desktopScreenshot := false
	clarification := false
	for _, definition := range definitions {
		if definition.Name == agentAskUserToolName {
			clarification = true
		}
		if definition.Name == runtimeToolFileRead {
			fileRead = true
		}
		if definition.Name == runtimeToolWindowList {
			windowList = true
		}
		if definition.Name == runtimeToolDesktopScreenshot {
			desktopScreenshot = true
		}
	}
	if !fileRead || !windowList || !desktopScreenshot || !clarification {
		t.Fatal("authorized Host tools or the Agent clarification tool were not exposed")
	}
	screenshotResultFromBroker, err := broker.Execute(context.Background(), fixture.store.details.Task, ModelToolCall{
		CallID: "call_dsh_screenshot_1", Name: runtimeToolDesktopScreenshot, Arguments: `{}`,
	})
	if err != nil || screenshotResultFromBroker.Status != AgentToolSucceeded || len(screenshotResultFromBroker.Images) != 1 ||
		!bytes.Equal(screenshotResultFromBroker.Images[0].Data, screenshot.Bytes()) {
		t.Fatalf("desktop screenshot was not preserved over the tool broker: %#v, err=%v", screenshotResultFromBroker, err)
	}
	result, err := broker.Execute(context.Background(), fixture.store.details.Task, ModelToolCall{
		CallID: "call_dsh_window_1", Name: runtimeToolWindowList, Arguments: `{}`,
	})
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("tool result = %#v, err = %v, runtime calls = %d, transitions = %#v", result, err,
			fixture.handlers["window.list"].calls.Load(), store.operationTransitions)
	}
	replayed, err := broker.Execute(context.Background(), fixture.store.details.Task, ModelToolCall{
		CallID: "call_dsh_window_1", Name: runtimeToolWindowList, Arguments: `{}`,
	})
	if err != nil || replayed.Status != AgentToolSucceeded || replayed.Output != result.Output || fixture.handlers["window.list"].calls.Load() != 1 {
		t.Fatalf("replayed tool result = %#v, err = %v, runtime calls = %d", replayed, err, fixture.handlers["window.list"].calls.Load())
	}
	operation, err := store.GetOperation(context.Background(), fixture.store.details.Task.OwnerID, fixture.store.details.Task.ID, agentOperationKey("call_dsh_window_1"))
	if err != nil || operation.Status != task.OperationSucceeded || operation.Result != result.Output {
		t.Fatalf("persisted operation = %#v, err = %v", operation, err)
	}
	details, err := store.GetTask(context.Background(), fixture.store.details.Task.ID, fixture.store.details.Task.OwnerID)
	if err != nil || details.Task.Status != task.StatusRunning || details.Task.Usage.ToolCalls != 3 ||
		len(details.Steps) != 3 || details.Steps[0].Status != task.StepSucceeded || details.Steps[1].Status != task.StepSucceeded ||
		details.Steps[2].Status != task.StepSucceeded {
		t.Fatalf("task progress = %#v, err = %v", details, err)
	}
	var output map[string]any
	if err := json.Unmarshal([]byte(result.Output), &output); err != nil || output["windows"] == nil || fixture.handlers["window.list"].calls.Load() != 1 {
		t.Fatalf("tool output was not retained: %q", result.Output)
	}
}

func TestCloudDSHToolBrokerReconcilesCheckpointedResultWithoutMoreToolBudget(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "window.list", action: "list",
		result: `{"windows":[{"id":"0x1","title":"Editor","x":0,"y":0,"width":100,"height":100,"active":true}]}`,
	}})
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	session, err := fixture.executor.connect(context.Background(), store.details.Task, fixture.fence)
	if err != nil {
		t.Fatal(err)
	}
	fixture.fences.fence = agentFenceFromRuntime(session.Fence)
	session.Close()
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	call := ModelToolCall{CallID: "call_dsh_reconcile_1", Name: runtimeToolWindowList, Arguments: `{}`}
	first, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || first.Status != AgentToolSucceeded {
		t.Fatalf("initial result = %#v, err = %v", first, err)
	}
	store.mu.Lock()
	store.details.Task.Budget.ToolCalls = 1
	store.details.Task.Usage.ToolCalls = 1
	claimed = store.details.Task
	store.mu.Unlock()
	expected := first.Output
	recovered, err := broker.Reconcile(context.Background(), claimed, call, &expected)
	if err != nil || recovered.Status != AgentToolSucceeded || recovered.Output != first.Output {
		t.Fatalf("reconciled result = %#v, err = %v", recovered, err)
	}
	if fixture.handlers["window.list"].calls.Load() != 1 || store.details.Task.Usage.ToolCalls != 1 || store.details.Task.Status != task.StatusRunning {
		t.Fatalf("reconciliation repeated work or charged budget: calls=%d usage=%d status=%s",
			fixture.handlers["window.list"].calls.Load(), store.details.Task.Usage.ToolCalls, store.details.Task.Status)
	}

	wrong := `{"different":true}`
	paused, err := broker.Reconcile(context.Background(), claimed, call, &wrong)
	if err != nil || paused.Status != AgentToolUnknown || fixture.handlers["window.list"].calls.Load() != 1 ||
		store.details.Task.Status != task.StatusWaitingUser || store.details.Task.Usage.ToolCalls != 1 {
		t.Fatalf("mismatched checkpoint result = %#v, err=%v, calls=%d, task=%#v",
			paused, err, fixture.handlers["window.list"].calls.Load(), store.details.Task)
	}
}

func TestCloudDSHToolBrokerQueriesUnknownOperationAtExhaustedBudget(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "window.list", action: "list",
		result: `{"windows":[{"id":"0x1","title":"Editor","x":0,"y":0,"width":100,"height":100,"active":true}]}`,
	}})
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	session, err := fixture.executor.connect(context.Background(), store.details.Task, fixture.fence)
	if err != nil {
		t.Fatal(err)
	}
	fixture.fences.fence = agentFenceFromRuntime(session.Fence)
	session.Close()
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	call := ModelToolCall{CallID: "call_dsh_unknown_1", Name: runtimeToolWindowList, Arguments: `{}`}
	first, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || first.Status != AgentToolSucceeded {
		t.Fatalf("initial result = %#v, err = %v", first, err)
	}
	store.mu.Lock()
	store.details.Task.Budget.ToolCalls = 1
	store.details.Task.Usage.ToolCalls = 1
	claimed = store.details.Task
	operation := store.operation
	operation.Status = task.OperationUnknown
	store.operation = operation
	store.operations[operation.Key] = operation
	store.mu.Unlock()

	expected := first.Output
	recovered, err := broker.Reconcile(context.Background(), claimed, call, &expected)
	if err != nil || recovered.Status != AgentToolSucceeded || recovered.Output != expected {
		t.Fatalf("reconciled unknown operation = %#v, err = %v", recovered, err)
	}
	if fixture.handlers["window.list"].calls.Load() != 1 || store.details.Task.Usage.ToolCalls != 1 ||
		store.details.Task.Status != task.StatusRunning {
		t.Fatalf("unknown reconciliation replayed or charged work: calls=%d usage=%d status=%s",
			fixture.handlers["window.list"].calls.Load(), store.details.Task.Usage.ToolCalls, store.details.Task.Status)
	}
}

func TestCloudDSHToolBrokerPausesUnresolvedRecoveryDurably(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.PauseRecovery(context.Background(), store.details.Task); err != nil {
		t.Fatalf("PauseRecovery() error = %v", err)
	}
	if store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "dsh_recovery_unresolved" ||
		store.details.Task.Usage.ToolCalls != 0 || len(store.details.Steps) != 1 || store.details.Steps[0].Status != task.StepUnknown {
		t.Fatalf("paused recovery task = %#v, steps=%#v", store.details.Task, store.details.Steps)
	}
}

func TestCloudDSHToolBrokerCompletesHostlessTaskWithMatchingCheckpoint(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.HostID = ""
	store.details.Task.Objective = "Explain the answer in chat."
	setCloudDSHCompletionFence(&store.details.Task)
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	inputs := []ModelInput{
		{Type: "function_call", CallID: "mcp-call-1", Name: cloudDSHMCPToolPrefix + "connector_tool"},
		{Type: "function_call_output", CallID: "mcp-call-1", Output: `{"text":"connector result"}`},
	}
	if err := broker.Complete(context.Background(), claimed, cloudDSHCompletionFor(claimed, "Cloud task finished.", inputs)); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if store.details.Task.Status != task.StatusCompleted || store.details.Task.Result != "Cloud task finished." ||
		len(store.details.Steps) != 1 || store.details.Steps[0].Status != task.StepSucceeded {
		t.Fatalf("completed task = %#v, steps = %#v", store.details.Task, store.details.Steps)
	}
}

func TestCloudDSHToolBrokerRejectsHostToolHistoryForHostlessTask(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.HostID = ""
	store.details.Task.Objective = "Answer the question in chat."
	setCloudDSHCompletionFence(&store.details.Task)
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	completion := cloudDSHCompletionFor(claimed, "Done.", []ModelInput{{
		Type: "function_call", CallID: "host-call-1", Name: runtimeToolFileRead,
	}})
	if err := broker.Complete(context.Background(), claimed, completion); err == nil {
		t.Fatal("hostless task accepted a computer tool call")
	}
	if store.details.Task.Status != task.StatusRunning {
		t.Fatalf("rejected hostless completion changed task status to %q", store.details.Task.Status)
	}
}

func TestCloudDSHToolBrokerCompletesHostTaskWithoutFileOutput(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.Objective = "Answer the question in chat."
	setCloudDSHCompletionFence(&store.details.Task)
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	if err := broker.Complete(context.Background(), claimed, cloudDSHCompletionFor(claimed, "No file proof.", nil)); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	if store.details.Task.Status != task.StatusCompleted || store.details.Task.Result != "No file proof." ||
		len(store.details.Steps) != 1 || store.details.Steps[0].Status != task.StepSucceeded {
		t.Fatalf("completed no-file task = %#v, steps = %#v", store.details.Task, store.details.Steps)
	}
}

func TestCloudDSHFileDeliveryIntentUsesTaskTextConservatively(t *testing.T) {
	for _, test := range []struct {
		name         string
		objective    string
		hasWorkspace bool
		want         task.FileDeliveryIntent
	}{
		{name: "explicit file", objective: "Generate a PDF report and save it to the Workspace.", hasWorkspace: true, want: task.FileDeliveryRequired},
		{name: "explicit Chinese file", objective: "生成 PDF 报告文件并保存到工作区。", hasWorkspace: true, want: task.FileDeliveryRequired},
		{name: "explicit Workspace destination", objective: "Write and verify a note in the attached Workspace.", hasWorkspace: true, want: task.FileDeliveryRequired},
		{name: "explicit Chinese Workspace destination", objective: "在工作区写一份任务总结。", hasWorkspace: true, want: task.FileDeliveryRequired},
		{name: "workspace code change", objective: "Fix the bug in this project.", hasWorkspace: true, want: task.FileDeliveryRequired},
		{name: "ambiguous report", objective: "Write a report about the project.", want: task.FileDeliveryUnclear},
		{name: "ambiguous note about Workspace", objective: "Write a note about the Workspace.", hasWorkspace: true, want: task.FileDeliveryUnclear},
		{name: "ambiguous Chinese report", objective: "写一份项目报告。", want: task.FileDeliveryUnclear},
		{name: "ordinary explanation", objective: "Explain what caused the error.", want: task.FileDeliveryNone},
		{name: "ordinary Chinese explanation", objective: "解释一下错误原因。", want: task.FileDeliveryNone},
		{name: "explicit Chinese chat only", objective: "不要生成文件，直接用文字回答。", want: task.FileDeliveryNone},
		{name: "explicit chat only", objective: "Create a PDF report, but no file is needed; explain it in chat.", want: task.FileDeliveryUnclear},
		{name: "conflicting Chinese code edit", objective: "不要生成文件，直接修改工作区代码。", hasWorkspace: true, want: task.FileDeliveryUnclear},
		{name: "conflicting file delivery", objective: "Do not create a file, but save a PDF report in the Workspace.", hasWorkspace: true, want: task.FileDeliveryUnclear},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyFileDeliveryIntent(test.objective, test.hasWorkspace); got != test.want {
				t.Fatalf("ClassifyFileDeliveryIntent(%q, %t) = %q, want %q", test.objective, test.hasWorkspace, got, test.want)
			}
		})
	}
}

func TestCloudDSHToolBrokerPausesWhenTaskTextRequiresMissingFile(t *testing.T) {
	for _, test := range []struct {
		name      string
		objective string
		hostless  bool
		reason    string
	}{
		{name: "required file", objective: "Create and save a PDF report.", reason: "file_delivery_required"},
		{name: "unclear report", objective: "Write a report about the project.", reason: "file_delivery_unclear"},
		{name: "hostless file", objective: "Create and save a PDF report.", hostless: true, reason: "file_delivery_required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			store.details.Task.Objective = test.objective
			if test.hostless {
				store.details.Task.HostID = ""
			}
			setCloudDSHCompletionFence(&store.details.Task)
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			claimed := store.details.Task
			if err := broker.Complete(context.Background(), claimed, cloudDSHCompletionFor(claimed, "Done.", nil)); err != nil {
				t.Fatalf("Complete() error = %v", err)
			}
			if store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != test.reason ||
				store.details.Task.Result != "" || len(store.details.Steps) != 1 || store.details.Steps[0].Status != task.StepUnknown {
				t.Fatalf("missing file did not pause task: task=%#v steps=%#v", store.details.Task, store.details.Steps)
			}
		})
	}
}

func TestCloudDSHToolBrokerDoesNotBypassFileWriteVerification(t *testing.T) {
	validCall := ModelInput{Type: "function_call", CallID: "write-1", Name: runtimeToolFileWrite,
		Arguments: `{"workspace_id":"w1234567","path":"notes/today.txt","content":"hello"}`}
	validOutput := ModelInput{Type: "function_call_output", CallID: "write-1",
		Output: `{"path":"notes/today.txt","bytes_written":5}`}
	cases := []struct {
		name   string
		inputs []ModelInput
	}{
		{name: "pending write", inputs: []ModelInput{validCall}},
		{name: "unknown write result", inputs: []ModelInput{validCall, {Type: "function_call_output", CallID: "write-1", Output: `{"error":"unknown"}`}}},
		{name: "malformed write arguments", inputs: []ModelInput{{Type: "function_call", CallID: "write-1", Name: runtimeToolFileWrite,
			Arguments: `{"path":"../escape.txt","content":"hello"}`}, validOutput}},
		{name: "malformed write result", inputs: []ModelInput{validCall, {Type: "function_call_output", CallID: "write-1", Output: `not-json`}}},
		{name: "unresolved write alongside a successful write", inputs: []ModelInput{
			validCall, validOutput,
			{Type: "function_call", CallID: "write-2", Name: runtimeToolFileWrite,
				Arguments: `{"workspace_id":"w1234567","path":"notes/other.txt","content":"pending"}`},
		}},
		{name: "successful write without durable operation", inputs: []ModelInput{validCall, validOutput}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			setCloudDSHCompletionFence(&store.details.Task)
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			claimed := store.details.Task
			if err := broker.Complete(context.Background(), claimed, cloudDSHCompletionFor(claimed, "Attempted file output.", test.inputs)); err != nil {
				t.Fatalf("Complete() error = %v", err)
			}
			if store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "output_verification_pending" ||
				store.details.Task.Result != "" || len(store.details.Steps) != 1 || store.details.Steps[0].Status != task.StepUnknown {
				t.Fatalf("unverified file-write task = %#v, steps = %#v", store.details.Task, store.details.Steps)
			}
		})
	}
}

func TestCloudDSHFileWriteEvidenceCoversEveryWriteAttempt(t *testing.T) {
	goodCall := ModelInput{Type: "function_call", CallID: "write-1", Name: runtimeToolFileWrite,
		Arguments: `{"workspace_id":"w1234567","path":"notes/today.txt","content":"hello"}`}
	goodOutput := ModelInput{Type: "function_call_output", CallID: "write-1",
		Output: `{"path":"notes/today.txt","bytes_written":5}`}
	evidence, err := agentFileWriteEvidence([]ModelInput{goodCall, goodOutput}, nil)
	if err != nil || cloudDSHHasUnverifiedFileWrite([]ModelInput{goodCall, goodOutput}, evidence) {
		t.Fatalf("matching write evidence = %#v, %v; want all writes accounted for", evidence, err)
	}
	if cloudDSHHasUnverifiedFileWrite(nil, nil) {
		t.Fatal("empty tool history must not require file verification")
	}
	inputs := []ModelInput{
		goodCall, goodOutput,
		{Type: "function_call", CallID: "write-2", Name: runtimeToolFileWrite,
			Arguments: `{"workspace_id":"w1234567","path":"notes/other.txt","content":"pending"}`},
	}
	evidence, err = agentFileWriteEvidence(inputs, nil)
	if err != nil || !cloudDSHHasUnverifiedFileWrite(inputs, evidence) {
		t.Fatalf("partially accounted write history = %#v, %v; want unresolved second write", evidence, err)
	}
}

func setCloudDSHCompletionFence(value *task.Task) {
	value.ExecutionLocation = "server"
	value.AgentInstanceID = "agent_test"
	value.AgentGeneration = 1
	value.Authorization.Version = 1
	value.AgentSessionID = "session_test"
	value.CheckpointThroughSequence = 2
	value.CheckpointDigest = bytes.Repeat([]byte{7}, 32)
}

func cloudDSHCompletionFor(value task.Task, result string, inputs []ModelInput) CloudDSHTaskCompletion {
	return CloudDSHTaskCompletion{
		AgentInstanceID: value.AgentInstanceID, AgentGeneration: value.AgentGeneration,
		AuthorizationVersion: value.Authorization.Version, AgentSessionID: value.AgentSessionID,
		CheckpointThroughSequence: value.CheckpointThroughSequence, CheckpointDigest: append([]byte(nil), value.CheckpointDigest...),
		Result: result, Inputs: inputs,
	}
}
