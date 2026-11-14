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
	"image/png"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/record"
	"morde/server/internal/domain/task"
)

type cloudDSHBrokerTaskStore struct {
	*runtimeToolTaskStore
	events []task.Event
}

func TestValidCloudDSHToolImagesAllowsBoundedTaskAttachmentImages(t *testing.T) {
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	imageData := encoded.Bytes()
	images := make([]ModelImage, maxCloudDSHTaskAttachmentImages)
	for index := range images {
		images[index] = ModelImage{MediaType: "image/png", Data: imageData}
	}
	valid := AgentToolResult{Status: AgentToolSucceeded, Images: images}
	if !validCloudDSHToolImages(cloudDSHTaskAttachmentReadToolName, valid) {
		t.Fatal("Task Reader rejected four fully decoded bounded images")
	}
	if !validCloudDSHToolImages("morde_hub_file_fetch_i1234567_v5", valid) {
		t.Fatal("Task file.fetch rejected bounded images extracted from a presentation")
	}

	tooMany := valid
	tooMany.Images = append(append([]ModelImage(nil), images...), images[0])
	badMIME := AgentToolResult{Status: AgentToolSucceeded, Images: []ModelImage{{MediaType: "image/gif", Data: imageData}}}
	badImage := AgentToolResult{Status: AgentToolSucceeded, Images: []ModelImage{{MediaType: "image/png", Data: []byte("bad image")}}}
	oversized := AgentToolResult{Status: AgentToolSucceeded, Images: []ModelImage{{MediaType: "image/png",
		Data: bytes.Repeat([]byte("x"), maxCloudDSHTaskAttachmentImageBytes+1)}}}
	for name, result := range map[string]AgentToolResult{
		"too many images": tooMany,
		"unknown MIME":    badMIME,
		"corrupt image":   badImage,
		"oversized image": oversized,
	} {
		t.Run(name, func(t *testing.T) {
			if validCloudDSHToolImages(cloudDSHTaskAttachmentReadToolName, result) {
				t.Fatal("invalid Task Reader image result was accepted")
			}
		})
	}
	if validCloudDSHToolImages(cloudDSHMCPToolPrefix+"search", valid) || validCloudDSHToolImages(runtimeToolDesktopScreenshot, valid) {
		t.Fatal("Task Reader multi-image limits leaked into MCP or desktop tools")
	}
}

func TestTextRegenerationExposesNoDSHToolDefinitions(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "shell.exec", action: "run",
		result: `{"exit_code":0,"stdout":"should not run","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`,
	}})
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	claimed.ToolsDisabled = true
	definitions, err := broker.Definitions(context.Background(), claimed)
	if err != nil {
		t.Fatalf("text-only Definitions() error = %v", err)
	}
	if len(definitions) != 0 {
		t.Fatalf("text-only DSH definitions = %#v, want no tools including built-ins", definitions)
	}
}

type cloudDSHRecordCaptureStore struct {
	taskStore       *cloudDSHBrokerTaskStore
	calls           int
	principal       identity.Principal
	input           record.CaptureInput
	statusAtEnqueue task.Status
	err             error
}

func TestGoalFollowupReadOnlyToolsUseExplicitAllowlist(t *testing.T) {
	allowed := []string{
		cloudDSHSkillReadToolName, cloudDSHTaskAttachmentReadToolName,
		cloudDSHWorkspaceListToolName, cloudDSHWorkspaceReadToolName, cloudDSHWorkspaceSearchToolName,
	}
	for _, name := range allowed {
		if !goalFollowupReadOnlyToolAllowed(name) {
			t.Errorf("read-only tool %q was rejected", name)
		}
	}
	blocked := []string{
		cloudDSHWebSearchToolName, cloudDSHWebReadToolName, cloudDSHPersonalUpdateToolName,
		cloudDSHGoalUpdateToolName, cloudDSHScheduleCreateToolName, cloudDSHMCPDiscoverToolName,
		agentAskUserToolName, runtimeToolShellExec, "morde_hub_send_message",
	}
	for _, name := range blocked {
		if goalFollowupReadOnlyToolAllowed(name) {
			t.Errorf("side-effecting or external tool %q was allowed", name)
		}
	}
	definitions := []AgentToolDefinition{
		{ModelTool: ModelTool{Name: cloudDSHSkillReadToolName}},
		{ModelTool: ModelTool{Name: cloudDSHPersonalUpdateToolName}},
		{ModelTool: ModelTool{Name: cloudDSHWebSearchToolName}},
	}
	filtered := filterGoalFollowupReadOnlyTools(definitions)
	if len(filtered) != 1 || filtered[0].Name != cloudDSHSkillReadToolName {
		t.Fatalf("read-only definitions = %#v", filtered)
	}
}

func TestGoalFollowupReadOnlyRejectsDisallowedCallsBeforeDispatch(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.HostID = ""
	store.details.Task.ExecutionLocation = "server"
	store.details.Task.WorkspaceID = ""
	store.details.Task.WorkspaceOwnerID = ""
	store.details.Task.InputRevision = nil
	store.details.Task.WorkspaceCheckpointRevision = nil
	store.details.Task.ContextDigest = nil
	store.details.Task.Authorization = task.Authorization{
		Kind: string(identity.KindScheduleTask), TokenID: "y1234567", Version: 1,
		Scopes: []string{
			string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit), string(identity.ScopeTaskControl),
			string(identity.ScopeGoalFollowupReadOnly),
		},
	}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := broker.Definitions(context.Background(), store.details.Task)
	if err != nil {
		t.Fatalf("read-only Goal follow-up definitions: %v", err)
	}
	for _, definition := range definitions {
		if !goalFollowupReadOnlyToolAllowed(definition.Name) {
			t.Fatalf("read-only Goal follow-up exposed disallowed tool %q", definition.Name)
		}
	}
	if len(definitions) != 0 {
		t.Fatalf("Goal follow-up without user attachments or Workspace should expose no tools: %#v", definitions)
	}
	_, err = broker.Execute(context.Background(), store.details.Task, ModelToolCall{
		CallID: "blocked-update", Name: cloudDSHPersonalUpdateToolName, Arguments: `{}`,
	})
	if domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("mutating Goal follow-up call error = %v, want forbidden", err)
	}
	_, err = broker.Reconcile(context.Background(), store.details.Task, ModelToolCall{
		CallID: "recovered-blocked-update", Name: cloudDSHPersonalUpdateToolName, Arguments: `{}`,
	}, nil)
	if domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("recovered mutating Goal follow-up call error = %v, want forbidden", err)
	}
}

type cloudDSHSkillTaskAuthorizationRecorder struct {
	userID  string
	tokenID string
	hostID  string
	scope   identity.Scope
}

func (r *cloudDSHSkillTaskAuthorizationRecorder) AuthorizeTaskAction(_ context.Context, userID string, _ identity.Kind,
	tokenID, hostID string, _ []identity.Scope, scope identity.Scope) error {
	r.userID, r.tokenID, r.hostID, r.scope = userID, tokenID, hostID, scope
	return nil
}

func (s *cloudDSHRecordCaptureStore) EnqueueRecordCapture(_ context.Context, principal identity.Principal, input record.CaptureInput) (record.QueueItem, error) {
	s.calls++
	s.principal = principal
	s.input = input
	s.taskStore.mu.RLock()
	s.statusAtEnqueue = s.taskStore.details.Task.Status
	s.taskStore.mu.RUnlock()
	return record.QueueItem{ID: "rq1234567", OwnerID: principal.UserID, Purpose: input.Purpose, Kind: input.Kind, State: record.QueueQueued}, s.err
}

func (*cloudDSHRecordCaptureStore) ClaimRecordCaptures(context.Context, string, time.Time, time.Duration, int32) ([]record.QueueItem, error) {
	return nil, nil
}

func (*cloudDSHRecordCaptureStore) CommitRecordCapture(context.Context, string, string, int64) (record.RecordMetadata, error) {
	return record.RecordMetadata{}, nil
}

func (*cloudDSHRecordCaptureStore) FailRecordCapture(context.Context, string, string, int64, string) error {
	return nil
}

func TestCloudDSHToolBrokerRejectsChangedTaskIdentity(t *testing.T) {
	fields := []struct {
		name   string
		mutate func(*task.Task)
	}{
		{name: "initiator", mutate: func(value *task.Task) { value.InitiatorID = "u7654321" }},
		{name: "project", mutate: func(value *task.Task) { value.ProjectID = "project_changed" }},
		{name: "conversation", mutate: func(value *task.Task) { value.ConversationID = "conversation_changed" }},
		{name: "execution location", mutate: func(value *task.Task) { value.ExecutionLocation = "server" }},
		{name: "workspace", mutate: func(value *task.Task) { value.WorkspaceID = "w7654321" }},
	}
	for _, field := range fields {
		t.Run(field.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			claimed := store.details.Task
			store.mu.Lock()
			field.mutate(&store.details.Task)
			store.mu.Unlock()
			if _, err := broker.currentTaskDetails(context.Background(), claimed); domain.CodeOf(err) != domain.CodeInvalidState {
				t.Fatalf("changed task identity error = %v, want invalid state", err)
			}
		})
	}
}

func TestCloudDSHToolBrokerAllowsTaskProgressAfterClaim(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	claimed := store.details.Task
	checkpointRevision := int64(2)
	leaseExpiry := time.Now().Add(time.Minute).UTC()
	store.mu.Lock()
	store.details.Task.WorkspaceCheckpointRevision = &checkpointRevision
	store.details.Task.EventSequence++
	store.details.Task.Usage.ToolCalls++
	store.details.Task.Lease.ExpiresAt = &leaseExpiry
	store.mu.Unlock()
	if _, err := broker.currentTaskDetails(context.Background(), claimed); err != nil {
		t.Fatalf("currentTaskDetails() rejected legitimate task progress: %v", err)
	}
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

func TestCloudDSHToolActivityPersistsPreparePhaseAndReusesStepOnReplay(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	current := store.details.Task
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-1", 1, "session-root"
	revision := int64(3)
	current.CurrentRevision = &revision
	current.Authorization.Version = 1
	store.details.Task = current
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelToolCall{CallID: "read-call-1", Name: cloudDSHWorkspaceReadToolName, Arguments: `{"path":"README.md"}`,
		TaskFence: &ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
			LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, AgentInstanceID: current.AgentInstanceID,
			AgentGeneration: current.AgentGeneration, AgentSessionID: current.AgentSessionID,
			AuthorizationVersion: current.Authorization.Version}}
	ctx := withCloudDSHToolActivity(context.Background(), current, call, nil)
	first, err := newAgentStep(current.ID, 1, "tool", task.StepRunning, "Read README", "Checking the Workspace.")
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.commitProgress(ctx, current, first, current.Usage, task.StatusRunning, "", "task.step", map[string]any{"phase": "prepare"}); err != nil {
		t.Fatal(err)
	}
	if len(store.details.Steps) != 1 || store.details.Steps[0].ID != first.ID || store.details.Steps[0].Phase != "prepare" {
		t.Fatalf("prepared activity step = %+v", store.details.Steps)
	}
	var firstEvent struct {
		StepID string         `json:"step_id"`
		Detail map[string]any `json:"detail"`
	}
	if err := json.Unmarshal(store.events[0].Payload, &firstEvent); err != nil || firstEvent.StepID != first.ID || firstEvent.Detail["phase"] != "prepare" {
		t.Fatalf("prepared activity event = %+v, err=%v", firstEvent, err)
	}

	replayed := task.Step{ID: "s7654321", TaskID: current.ID, Sequence: 2, Kind: "tool", Status: task.StepSucceeded,
		Title: "Read README", Summary: "Read completed."}
	if err := broker.commitProgress(ctx, current, replayed, current.Usage, task.StatusRunning, "", "task.step", nil); err != nil {
		t.Fatal(err)
	}
	if len(store.details.Steps) != 1 || store.details.Steps[0].ID != first.ID || store.details.Steps[0].Status != task.StepSucceeded ||
		store.details.Steps[0].Phase != string(task.StepSucceeded) || len(store.events) != 2 {
		t.Fatalf("replayed activity step=%+v events=%d", store.details.Steps, len(store.events))
	}
	var replayEvent struct {
		StepID string `json:"step_id"`
	}
	if err := json.Unmarshal(store.events[1].Payload, &replayEvent); err != nil || replayEvent.StepID != first.ID {
		t.Fatalf("replayed activity event step=%q, err=%v; want original %q", replayEvent.StepID, err, first.ID)
	}
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
			for _, id := range []string{"inbox-triage", "meeting-prep", "weekly-review"} {
				if strings.Contains(definition.Description, id+":") {
					t.Errorf("deprecated skill %q was exposed to new Tasks", id)
				}
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
		output.SourceRef != "clients/nanoMuse/nanomuse/skills/builtin/compare-options/SKILL.md" ||
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
	legacy := ModelToolCall{CallID: "call_legacy_skill", Name: cloudDSHSkillReadToolName, Arguments: `{"skill_id":"weekly-review"}`}
	if _, err := broker.Execute(context.Background(), claimed, legacy); domain.CodeOf(err) != domain.CodeForbidden ||
		store.details.Task.Usage.ToolCalls != 1 || len(store.events) != 1 {
		t.Fatalf("deprecated skill was available to a new Task: err=%v usage=%+v events=%d", err,
			store.details.Task.Usage, len(store.events))
	}
	legacyReplay, err := broker.Reconcile(context.Background(), claimed, legacy, nil)
	if err != nil || legacyReplay.Status != AgentToolSucceeded || !strings.Contains(legacyReplay.Output, `"skill_id":"weekly-review"`) ||
		store.details.Task.Usage.ToolCalls != 2 || len(store.events) != 2 {
		t.Fatalf("deprecated skill could not recover an existing checkpoint: result=%#v err=%v usage=%+v events=%d", legacyReplay, err,
			store.details.Task.Usage, len(store.events))
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
	definitionNames := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		definitionNames[definition.Name] = struct{}{}
	}
	for _, name := range []string{cloudDSHSkillReadToolName, cloudDSHWorkspaceListToolName, cloudDSHWorkspaceReadToolName,
		cloudDSHWorkspaceWriteToolName, agentAskUserToolName} {
		if _, exists := definitionNames[name]; !exists {
			t.Fatalf("hostless server task definitions omit %q: %#v", name, definitions)
		}
	}
	result, err := broker.Execute(context.Background(), claimed, ModelToolCall{
		CallID: "call_hostless_skill", Name: cloudDSHSkillReadToolName, Arguments: `{"skill_id":"compare-options"}`,
	})
	if err != nil || result.Status != AgentToolSucceeded || !strings.Contains(result.Output, `"skill_id":"compare-options"`) ||
		store.details.Task.Usage.ToolCalls != 1 || len(store.events) != 1 {
		t.Fatalf("hostless server skill read = %#v, err=%v, usage=%+v events=%d", result, err,
			store.details.Task.Usage, len(store.events))
	}
	result, err = broker.Execute(context.Background(), claimed, ModelToolCall{
		CallID: "call_hostless_markdown_skill", Name: cloudDSHSkillReadToolName, Arguments: `{"skill_id":"markdown-docs"}`,
	})
	if err != nil || result.Status != AgentToolSucceeded || !strings.Contains(result.Output, "morde_workspace_write") ||
		strings.Contains(result.Output, "artifact.create_file") || !strings.Contains(result.Output, "downloadable artifact") ||
		store.details.Task.Usage.ToolCalls != 2 || len(store.events) != 2 {
		t.Fatalf("hostless Markdown skill read = %#v, err=%v, usage=%+v events=%d", result, err,
			store.details.Task.Usage, len(store.events))
	}
	for index, skillID := range []string{"translation", "meeting-follow-up", "app-creation"} {
		result, err = broker.Execute(context.Background(), claimed, ModelToolCall{
			CallID: "call_hostless_followup_skill_" + skillID, Name: cloudDSHSkillReadToolName,
			Arguments: `{"skill_id":"` + skillID + `"}`,
		})
		if err != nil || result.Status != AgentToolSucceeded || !strings.Contains(result.Output, `"skill_id":"`+skillID+`"`) ||
			!strings.Contains(result.Output, cloudDSHWorkspaceWriteToolName) ||
			store.details.Task.Usage.ToolCalls != int64(3+index) || len(store.events) != 3+index {
			t.Fatalf("hostless %s skill read = %#v, err=%v, usage=%+v events=%d", skillID, result, err,
				store.details.Task.Usage, len(store.events))
		}
	}
	if result, err = broker.Execute(context.Background(), claimed, ModelToolCall{
		CallID: "call_hostless_office_skill", Name: cloudDSHSkillReadToolName, Arguments: `{"skill_id":"office"}`,
	}); domain.CodeOf(err) != domain.CodeForbidden || result.Status == AgentToolSucceeded ||
		store.details.Task.Usage.ToolCalls != 5 || len(store.events) != 5 {
		t.Fatalf("hostless task loaded an Office skill: %#v, err=%v, usage=%+v events=%d", result, err,
			store.details.Task.Usage, len(store.events))
	}
}

func TestCloudDSHProjectSkillUsesTaskInitiatorForHostAuthorization(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	authorizer := &cloudDSHSkillTaskAuthorizationRecorder{}
	fixture.executor.authorizer = authorizer
	current := store.details.Task
	current.OwnerID, current.InitiatorID, current.ProjectID = "u1234567", "u2234567", "j1234567"
	expiresAt := time.Now().Add(time.Hour)
	current.Authorization = task.Authorization{
		Kind: string(identity.KindSession), TokenID: "t1234567", Version: 1,
		Scopes:    []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)},
		ExpiresAt: &expiresAt,
	}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.authorizeSkillTask(context.Background(), current); err != nil {
		t.Fatalf("authorize project skill task: %v", err)
	}
	if authorizer.userID != current.InitiatorID || authorizer.tokenID != current.Authorization.TokenID ||
		authorizer.hostID != current.HostID || authorizer.scope != identity.ScopeTaskRead {
		t.Fatalf("skill Host authorization used the wrong Task actor: %#v", authorizer)
	}
}

func TestCloudDSHSkillCatalogEmbedsEveryBuiltinWithFixedMetadata(t *testing.T) {
	definition := cloudDSHSkillReadDefinition(true)
	if len(cloudDSHSkillCatalog) != 29 || len(definition.Description) > 2048 ||
		!strings.Contains(definition.Description, "Morde skill by ID") ||
		strings.Contains(definition.Description, "Legacy Muse") || strings.Contains(definition.Description, "nanoMuse skill") {
		t.Fatalf("skill catalog does not expose all built-ins: %d, %s", len(cloudDSHSkillCatalog), definition.Description)
	}
	for _, id := range []string{"weekly-review-planning", "markdown-docs", "translation"} {
		if !strings.Contains(definition.Description, id) {
			t.Errorf("skill overview omits %q", id)
		}
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
		wantSourceRef := "clients/nanoMuse/nanomuse/skills/builtin/" + skill.id + "/SKILL.md"
		if skill.sourceRef != "" {
			wantSourceRef = skill.sourceRef
		}
		if !ok || metadata.ID != skill.id || metadata.Version != skill.version ||
			metadata.SourceRef != wantSourceRef {
			t.Fatalf("skill metadata is not fixed for %q: %#v", skill.id, metadata)
		}
		content, err := cloudDSHSkillBundle.ReadFile("cloud_dsh_skills/" + skill.id + ".md")
		if err != nil || len(content) == 0 || !utf8.Valid(content) || !strings.Contains(string(content), "author: Morde") ||
			strings.Contains(string(content), "author: nanoMuse") {
			t.Fatalf("embedded skill %q is unavailable: %v", skill.id, err)
		}
		if !skill.deprecated {
			toolIDs = append(toolIDs, skill.id)
		}
	}
	if len(schema.Properties["skill_id"].Enum) != len(toolIDs) {
		t.Fatalf("skill tool allowlist differs from the embedded catalog: %#v", schema.Properties)
	}
	for index, id := range toolIDs {
		if schema.Properties["skill_id"].Enum[index] != id {
			t.Fatalf("skill tool allowlist differs from the embedded catalog: %#v", schema.Properties)
		}
	}
	for _, id := range []string{"office", "office-docx", "office-pptx", "office-xlsx", "office-render", "office-recalculate", "office-export-pdf"} {
		found := false
		for _, allowedID := range schema.Properties["skill_id"].Enum {
			found = found || allowedID == id
		}
		if !found {
			t.Errorf("Host skill ID %q is absent from the skill enum", id)
		}
	}
	hostless := cloudDSHSkillReadDefinition(false)
	if strings.Contains(string(hostless.Parameters), `"office"`) || strings.Contains(string(hostless.Parameters), `"office-xlsx"`) {
		t.Fatalf("hostless skill tool exposes Office skills: %s", hostless.Parameters)
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

func TestCloudDSHToolBrokerExpiredApprovalReturnsWithoutDispatch(t *testing.T) {
	fixture, store, broker, call := newCloudDSHApprovalFixture(t)
	first, err := broker.Execute(context.Background(), store.details.Task, call)
	if err != nil || first.Status != AgentToolNeedsUser {
		t.Fatalf("initial sensitive call = %#v, %v; want approval pause", first, err)
	}
	resumeCloudDSHApproval(fixture, store, task.PendingRequestExpired, task.PendingDecisionReject)
	store.mu.Lock()
	expiresAt := time.Now().Add(-time.Minute).UTC()
	respondedAt := time.Now().UTC()
	store.details.PendingRequest.ExpiresAt = &expiresAt
	store.details.PendingRequest.RespondedAt = &respondedAt
	store.mu.Unlock()
	result, err := broker.Reconcile(context.Background(), store.details.Task, call, nil)
	if err != nil || result.Status != AgentToolFailed || result.Output != "The approval expired. This exact action was not executed." {
		t.Fatalf("expired exact call = %#v, %v; want expiry failure", result, err)
	}
	if store.details.Task.Usage.ToolCalls != 1 || fixture.handlers["shell.exec"].calls.Load() != 0 || len(fixture.store.operations) != 0 {
		t.Fatalf("expired action crossed dispatch or counted twice: usage=%+v dispatches=%d operations=%d", store.details.Task.Usage,
			fixture.handlers["shell.exec"].calls.Load(), len(fixture.store.operations))
	}
}

func TestCloudDSHToolRejectedApprovalUpdatesSameFencedActivityStep(t *testing.T) {
	fixture, store, broker, call := newCloudDSHApprovalFixture(t)
	current := &store.details.Task
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-approval", 1, "session-root"
	revision := int64(4)
	current.CurrentRevision = &revision
	current.Authorization.Version = 2
	call.TaskFence = &ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
		LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, AgentInstanceID: current.AgentInstanceID,
		AgentGeneration: current.AgentGeneration, AgentSessionID: current.AgentSessionID,
		AuthorizationVersion: current.Authorization.Version}

	first, err := broker.Execute(context.Background(), *current, call)
	if err != nil || first.Status != AgentToolNeedsUser || len(store.details.Steps) != 1 {
		t.Fatalf("approval activity = %#v, err=%v, steps=%+v", first, err, store.details.Steps)
	}
	original := store.details.Steps[0]
	if original.AgentSessionID != current.AgentSessionID || original.ToolCallKey != call.CallID ||
		original.ToolName != call.Name || original.TaskRevision == nil || *original.TaskRevision != revision ||
		original.Status != task.StepWaitingUser || original.Phase != string(task.StepWaitingUser) {
		t.Fatalf("prepared approval activity step = %+v", original)
	}

	resumeCloudDSHApproval(fixture, store, task.PendingRequestRejected, task.PendingDecisionReject)
	call.TaskFence.LeaseOwner, call.TaskFence.LeaseVersion = store.details.Task.Lease.Owner, store.details.Task.Lease.Version
	result, err := broker.Reconcile(context.Background(), store.details.Task, call, nil)
	if err != nil || result.Status != AgentToolFailed || len(store.details.Steps) != 1 {
		t.Fatalf("rejected approval activity = %#v, err=%v, steps=%+v", result, err, store.details.Steps)
	}
	settled := store.details.Steps[0]
	if settled.ID != original.ID || settled.Sequence != original.Sequence || settled.AgentSessionID != original.AgentSessionID ||
		settled.ToolCallKey != original.ToolCallKey || settled.Status != task.StepFailed || settled.Phase != string(task.StepFailed) ||
		!strings.Contains(settled.Summary, "not dispatched") || fixture.handlers["shell.exec"].calls.Load() != 0 {
		t.Fatalf("rejected approval did not settle original activity without dispatch: %+v calls=%d",
			settled, fixture.handlers["shell.exec"].calls.Load())
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
	screenshotResult := `{"mime_type":"image/jpeg","width":2,"height":2,"screen_width":100,"screen_height":100,"screenshot_token":"` + strings.Repeat("A", 43) + `","data_base64":"` + base64.StdEncoding.EncodeToString(screenshot.Bytes()) + `"}`
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
	if !strings.Contains(screenshotResultFromBroker.Output, `"screen_width":100`) || !strings.Contains(screenshotResultFromBroker.Output, `"screen_height":100`) {
		t.Fatalf("desktop screenshot geometry not preserved over broker: %s", screenshotResultFromBroker.Output)
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
	for _, event := range store.events {
		if bytes.Contains(event.Payload, []byte(`"output"`)) || bytes.Contains(event.Payload, []byte("Editor")) ||
			bytes.Contains(event.Payload, []byte("data_base64")) {
			t.Fatalf("Host tool output was duplicated into a client-visible Task event: %s", event.Payload)
		}
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

func TestCloudDSHToolBrokerReconcilesOnlyTerminalWorkspaceOperationsAfterCheckpointAdvance(t *testing.T) {
	for _, status := range []task.OperationStatus{task.OperationSucceeded, task.OperationUnknown} {
		t.Run(string(status), func(t *testing.T) {
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
			claimed := store.details.Task
			call := ModelToolCall{CallID: "call_dsh_workspace_reconcile_1", Name: runtimeToolWindowList, Arguments: `{}`}
			first, err := broker.Execute(context.Background(), claimed, call)
			if err != nil || first.Status != AgentToolSucceeded {
				t.Fatalf("initial operation = %#v, err=%v", first, err)
			}

			store.mu.Lock()
			advancedCheckpoint := int64(1)
			store.details.Task.WorkspaceCheckpointRevision = &advancedCheckpoint
			claimed = store.details.Task
			operation := store.operations[agentOperationKey(call.CallID)]
			if status == task.OperationUnknown {
				operation.Status = status
				store.operations[operation.Key] = operation
				store.operation = operation
			}
			store.mu.Unlock()

			output := first.Output
			recovered, err := broker.Reconcile(context.Background(), claimed, call, &output)
			if err != nil || fixture.handlers["window.list"].calls.Load() != 1 {
				t.Fatalf("recovered operation = %#v, err=%v, Host calls=%d", recovered, err, fixture.handlers["window.list"].calls.Load())
			}
			if status == task.OperationSucceeded {
				if recovered.Status != AgentToolSucceeded || recovered.Output != first.Output || store.details.Task.Status != task.StatusRunning {
					t.Fatalf("terminal result was not safely reused: result=%#v task=%#v", recovered, store.details.Task)
				}
				return
			}
			if recovered.Status != AgentToolUnknown || store.details.Task.Status != task.StatusWaitingUser {
				t.Fatalf("unresolved operation crossed a changed Workspace checkpoint: result=%#v task=%#v", recovered, store.details.Task)
			}
		})
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

func TestCloudDSHToolBrokerModelRecoveryStoresSafeFailureCategory(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.PauseModelRecovery(context.Background(), store.details.Task, "timeout"); err != nil {
		t.Fatalf("PauseModelRecovery() error = %v", err)
	}
	if len(store.details.Steps) != 1 || !strings.Contains(store.details.Steps[0].Summary, "category: timeout") {
		t.Fatalf("model recovery summary did not include safe category: %#v", store.details.Steps)
	}

	fixture = newRuntimeToolExecutorFixture(t, nil)
	store = &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	broker, err = NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.PauseModelRecovery(context.Background(), store.details.Task, "private response with API key"); err != nil {
		t.Fatalf("PauseModelRecovery() with unknown category error = %v", err)
	}
	if len(store.details.Steps) != 1 || !strings.Contains(store.details.Steps[0].Summary, "category: unknown") ||
		strings.Contains(store.details.Steps[0].Summary, "private response") {
		t.Fatalf("model recovery summary leaked untrusted category: %#v", store.details.Steps)
	}
}

func TestCloudDSHToolBrokerExplainsModelContextLimit(t *testing.T) {
	for _, category := range []string{"context_length_exceeded", "provider_context_length_exceeded"} {
		t.Run(category, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.PauseModelRecovery(context.Background(), store.details.Task, category); err != nil {
				t.Fatal(err)
			}
			if len(store.details.Steps) != 1 || !strings.Contains(store.details.Steps[0].Summary, "原文已保留") ||
				!strings.Contains(store.details.Steps[0].Summary, "缩短后重新发送") || store.details.Task.Status != task.StatusWaitingUser ||
				store.details.Task.WaitingReason != "model_context_limit" {
				t.Fatalf("missing actionable oversized-input explanation: task=%+v steps=%+v", store.details.Task, store.details.Steps)
			}
		})
	}
}

func TestCloudDSHToolBrokerExplainsUnavailableAttachmentReader(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.PauseModelRecovery(context.Background(), store.details.Task, "attachment_reader_unavailable"); err != nil {
		t.Fatal(err)
	}
	if len(store.details.Steps) != 1 || !strings.Contains(store.details.Steps[0].Summary, "当前模型无法读取附件") ||
		!strings.Contains(store.details.Steps[0].Summary, "粘贴") || store.details.Task.Status != task.StatusWaitingUser ||
		store.details.Task.WaitingReason != "model_attachment_unavailable" {
		t.Fatalf("missing attachment-reader explanation: task=%+v steps=%+v", store.details.Task, store.details.Steps)
	}
}

func TestCloudDSHToolBrokerExplainsInterruptedStream(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.PauseModelRecovery(context.Background(), store.details.Task, "stream_result_mismatch"); err != nil {
		t.Fatal(err)
	}
	if len(store.details.Steps) != 1 || !strings.Contains(store.details.Steps[0].Summary, "部分回复") ||
		store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "model_stream_interrupted" {
		t.Fatalf("missing interrupted-stream explanation: task=%+v steps=%+v", store.details.Task, store.details.Steps)
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

func TestCloudDSHToolBrokerCapturesOnlyCompletedUserTurn(t *testing.T) {
	for _, test := range []struct {
		name              string
		authorizationKind string
		captureErr        error
		wantCapture       bool
	}{
		{name: "queued after completion", wantCapture: true},
		{name: "queue error does not change completed task", captureErr: errors.New("queue unavailable"), wantCapture: true},
		{name: "unsupported authorization kind is not rewritten", authorizationKind: string(identity.KindScheduleTask)},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			store.details.Task.HostID = ""
			store.details.Task.Objective = "Explain the answer in chat."
			if test.authorizationKind != "" {
				store.details.Task.Authorization.Kind = test.authorizationKind
			}
			store.details.Task.Authorization.TokenID = "t1234567"
			setCloudDSHCompletionFence(&store.details.Task)

			captureStore := &cloudDSHRecordCaptureStore{taskStore: store, err: test.captureErr}
			capture, err := NewRecordCaptureService(captureStore)
			if err != nil {
				t.Fatal(err)
			}
			broker, err := NewCloudDSHToolBrokerWithRecordCapture(store, fixture.executor, nil, nil, capture)
			if err != nil {
				t.Fatal(err)
			}

			claimed := store.details.Task
			const toolOnlySentinel = "tool-data-must-not-be-recorded"
			inputs := []ModelInput{
				{Type: "function_call", CallID: "mcp-call-1", Name: cloudDSHMCPToolPrefix + "connector_tool", Arguments: `{"query":"` + toolOnlySentinel + `"}`},
				{Type: "function_call_output", CallID: "mcp-call-1", Output: toolOnlySentinel},
			}
			if err := broker.Complete(context.Background(), claimed, cloudDSHCompletionFor(claimed, "Cloud task finished.", inputs)); err != nil {
				t.Fatalf("Complete() error = %v", err)
			}
			if store.details.Task.Status != task.StatusCompleted || store.details.Task.Result != "Cloud task finished." {
				t.Fatalf("capture changed task completion: status=%q result=%q", store.details.Task.Status, store.details.Task.Result)
			}
			if !test.wantCapture {
				if captureStore.calls != 0 {
					t.Fatalf("unsupported authorization kind triggered capture: %#v", captureStore.principal)
				}
				return
			}
			if captureStore.calls != 1 || captureStore.statusAtEnqueue != task.StatusCompleted {
				t.Fatalf("capture calls=%d, task status at enqueue=%q", captureStore.calls, captureStore.statusAtEnqueue)
			}
			if captureStore.principal.UserID != claimed.OwnerID || captureStore.principal.Kind != identity.KindSession ||
				captureStore.principal.TokenID != claimed.Authorization.TokenID || captureStore.principal.Role != "" {
				t.Fatalf("capture principal was not derived from the persisted authorization snapshot: %#v", captureStore.principal)
			}
			if captureStore.input.Purpose != record.PurposeFullRecord || captureStore.input.Kind != record.ItemTurn || captureStore.input.TaskID != claimed.ID ||
				len(captureStore.input.Attachments) != 0 || strings.Contains(string(captureStore.input.Payload), toolOnlySentinel) {
				t.Fatalf("capture included unsupported data: %#v", captureStore.input)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(captureStore.input.Payload, &payload); err != nil || len(payload) != 2 ||
				string(payload["objective"]) != `"Explain the answer in chat."` ||
				string(payload["result"]) != `"Cloud task finished."` {
				t.Fatalf("captured payload must contain only the objective and final result: %s (err=%v)", captureStore.input.Payload, err)
			}
		})
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

func TestCloudDSHToolBrokerDoesNotPublishUnverifiedFileClaim(t *testing.T) {
	for _, test := range []struct {
		name, result string
		blocked      bool
	}{
		{"affirmative", "我已经将文件保存到工作区。", true},
		{"negative", "我没有保存文件，当前无法使用工具。", false},
		{"discussion", "请解释“文件已保存”是什么意思。", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			store.details.Task.Objective = "Answer the question in chat."
			store.details.Task.FileDeliveryIntent = task.FileDeliveryNone
			setCloudDSHCompletionFence(&store.details.Task)
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			claimed := store.details.Task
			if err := broker.Complete(context.Background(), claimed, cloudDSHCompletionFor(claimed, test.result, nil)); err != nil {
				t.Fatal(err)
			}
			if test.blocked {
				if store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "output_verification_pending" || store.details.Task.Result == test.result {
					t.Fatalf("unsupported claim was published: %+v", store.details.Task)
				}
			} else if store.details.Task.Status != task.StatusCompleted || store.details.Task.Result != test.result {
				t.Fatalf("ordinary answer was interrupted: %+v", store.details.Task)
			}
		})
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
		{name: "colloquial Chinese report", objective: "帮我做一份项目报告。", want: task.FileDeliveryUnclear},
		{name: "colloquial Chinese spreadsheet", objective: "做个 Excel 表格。", want: task.FileDeliveryRequired},
		{name: "ordinary explanation", objective: "Explain what caused the error.", want: task.FileDeliveryNone},
		{name: "ordinary Chinese explanation", objective: "解释一下错误原因。", want: task.FileDeliveryNone},
		{name: "explicit Chinese chat only", objective: "不要生成文件，直接用文字回答。", want: task.FileDeliveryNone},
		{name: "Chinese no file write", objective: "不要写文件，只要在聊天里回答。", want: task.FileDeliveryNone},
		{name: "Chinese no Workspace write", objective: "不要在工作区写文件，只在聊天说明。", hasWorkspace: true, want: task.FileDeliveryNone},
		{name: "Chinese no file save", objective: "不需要保存文件，解释结论即可。", want: task.FileDeliveryNone},
		{name: "English no file create", objective: "Don't create any files; just answer in chat.", want: task.FileDeliveryNone},
		{name: "English no Workspace write", objective: "Do not write the file to the Workspace; answer in chat.", hasWorkspace: true, want: task.FileDeliveryNone},
		{name: "explicit chat only", objective: "Create a PDF report, but no file is needed; explain it in chat.", want: task.FileDeliveryUnclear},
		{name: "conflicting Chinese code edit", objective: "不要生成文件，直接修改工作区代码。", hasWorkspace: true, want: task.FileDeliveryUnclear},
		{name: "conflicting file delivery", objective: "Do not create a file, but save a PDF report in the Workspace.", hasWorkspace: true, want: task.FileDeliveryUnclear},
		{name: "conflicting file write", objective: "Do not write a file, but save the PDF report in the Workspace.", hasWorkspace: true, want: task.FileDeliveryUnclear},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyFileDeliveryIntent(test.objective, test.hasWorkspace); got != test.want {
				t.Fatalf("ClassifyFileDeliveryIntent(%q, %t) = %q, want %q", test.objective, test.hasWorkspace, got, test.want)
			}
		})
	}
}

func TestCloudDSHMarkdownFileDeliveryIntent(t *testing.T) {
	for _, test := range []struct {
		name      string
		objective string
		want      task.FileDeliveryIntent
	}{
		{name: "screenshot request", objective: "我要生产一个md文档", want: task.FileDeliveryRequired},
		{name: "spaced markdown", objective: "为五子棋生成一份 Markdown 文档", want: task.FileDeliveryRequired},
		{name: "extension", objective: "生成 .MD 文件并提供下载", want: task.FileDeliveryRequired},
		{name: "md shorthand", objective: "生成md", want: task.FileDeliveryRequired},
		{name: "save as markdown", objective: "保存为 Markdown 文件", want: task.FileDeliveryRequired},
		{name: "design plan download request", objective: "为五子棋应用写一份设计方案，保存为 Markdown 文件并提供下载", want: task.FileDeliveryRequired},
		{name: "downloadable design plan", objective: "帮助我写一个设计方案文档，并提供下载", want: task.FileDeliveryRequired},
		{name: "chat only", objective: "只在聊天回答怎么生成 Markdown 文档", want: task.FileDeliveryNone},
		{name: "negative", objective: "不要生产md文档，只用文字回答", want: task.FileDeliveryNone},
		{name: "conflict", objective: "不要生成 .md 文件，但保存为 Markdown 文档", want: task.FileDeliveryUnclear},
		{name: "how-to question", objective: "请告诉我如何生成一个 .md 文档", want: task.FileDeliveryNone},
		{name: "tutorial file", objective: "帮我写一份如何生成 Markdown 文档的教程，保存为.md", want: task.FileDeliveryRequired},
		{name: "tutorial filename", objective: "请生成一份关于如何生成 Markdown 的教程.md", want: task.FileDeliveryRequired},
		{name: "summarize input file in chat", objective: "帮我分析文件并写一段总结", want: task.FileDeliveryNone},
		{name: "output existing file contents", objective: "输出文件内容", want: task.FileDeliveryNone},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyFileDeliveryIntent(test.objective, false); got != test.want {
				t.Fatalf("ClassifyFileDeliveryIntent(%q) = %q, want %q", test.objective, got, test.want)
			}
		})
	}
}

func TestCloudDSHToolBrokerPausesWhenTaskTextRequiresMissingFile(t *testing.T) {
	for _, test := range []struct {
		name              string
		objective         string
		hostless          bool
		reason            string
		wantClarification bool
	}{
		{name: "required file", objective: "Create and save a PDF report.", reason: "file_delivery_required"},
		{name: "unclear report", objective: "Write a report about the project.", reason: "clarification_required", wantClarification: true},
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
			if test.wantClarification {
				store.details.ClarificationRequests = []task.ClarificationRequest{{
					ID: "r1234567", TaskID: store.details.Task.ID, Version: 1, CallID: "call-previous",
					Question: "Which environment?", Answer: "staging", Status: task.ClarificationAnswered,
				}}
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
			if store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != test.reason || store.details.Task.Result != "" {
				t.Fatalf("missing file did not pause task: task=%#v steps=%#v", store.details.Task, store.details.Steps)
			}
			if test.wantClarification {
				if len(store.details.Steps) != 2 || len(store.details.ClarificationRequests) != 2 ||
					store.details.ClarificationRequests[1].CallID != FileDeliveryIntentClarificationCallID ||
					store.details.ClarificationRequests[1].Version != 2 ||
					store.details.ClarificationRequests[1].Status != task.ClarificationPending {
					t.Fatalf("unclear file intent did not create an answerable request: %#v", store.details)
				}
			} else if len(store.details.Steps) != 1 || store.details.Steps[0].Status != task.StepUnknown {
				t.Fatalf("required file pause did not preserve its verification step: %#v", store.details.Steps)
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
	evidence, err := agentFileWriteEvidence([]ModelInput{goodCall, goodOutput})
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
	evidence, err = agentFileWriteEvidence(inputs)
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
