package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/record"
	"morde/server/internal/domain/task"
)

type cloudDSHBrokerTaskStore struct {
	*runtimeToolTaskStore
	events                  []task.Event
	toolCalls               []TaskToolCallProgress
	commitTaskProgressError error
	committedTaskProgress   []TaskProgress
	planReadEvents          []string
	planReadAdmissions      []cloudDSHPlanReadRecord
	planReadSettlements     []cloudDSHPlanReadRecord
	planReadAdmissionError  error
	planReadSettlementError error
	planReadSettledDigests  map[string][]byte
}

type cloudDSHPlanReadRecord struct {
	PlanRevision int64
	StepID       string
	ToolCall     TaskToolCallProgress
	ResultDigest []byte
}

func (s *cloudDSHBrokerTaskStore) AdmitCloudDSHTaskPlanStepRead(ctx context.Context, current task.Task, planRevision int64,
	stepID string, toolCall TaskToolCallProgress) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.planReadEvents = append(s.planReadEvents, "admit")
	s.planReadAdmissions = append(s.planReadAdmissions, cloudDSHPlanReadRecord{PlanRevision: planRevision, StepID: stepID, ToolCall: toolCall})
	if s.planReadAdmissionError != nil {
		return s.planReadAdmissionError
	}
	if current.ID != s.details.Task.ID || current.OwnerID != s.details.Task.OwnerID {
		return errors.New("stale direct read admission")
	}
	return nil
}

func (s *cloudDSHBrokerTaskStore) SettleCloudDSHTaskPlanStepRead(ctx context.Context, current task.Task, planRevision int64,
	stepID string, toolCall TaskToolCallProgress, resultDigest []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.planReadSettlementError != nil {
		return s.planReadSettlementError
	}
	if len(resultDigest) != sha256.Size || current.ID != s.details.Task.ID || current.OwnerID != s.details.Task.OwnerID {
		return errors.New("invalid direct read settlement")
	}
	foundSuccessfulStep := false
	for _, step := range s.details.Steps {
		if step.AgentSessionID == toolCall.AgentSessionID && step.TaskRevision != nil && *step.TaskRevision == toolCall.TaskRevision &&
			step.ToolCallKey == toolCall.CallKey && step.ToolName == toolCall.ToolName &&
			bytes.Equal(step.RequestDigest, toolCall.RequestDigest) && step.Status == task.StepSucceeded {
			foundSuccessfulStep = true
			break
		}
	}
	if !foundSuccessfulStep {
		return errors.New("direct read TaskStep is not durably successful")
	}
	key := current.ID + "\x00" + toolCall.AgentSessionID + "\x00" + toolCall.CallKey
	if s.planReadSettledDigests == nil {
		s.planReadSettledDigests = make(map[string][]byte)
	}
	if previous, found := s.planReadSettledDigests[key]; found {
		if !bytes.Equal(previous, resultDigest) {
			return domain.NewError(domain.CodeConflict, "direct read replay result digest changed")
		}
		return nil
	}
	s.planReadEvents = append(s.planReadEvents, "settle")
	s.planReadSettledDigests[key] = append([]byte(nil), resultDigest...)
	s.planReadSettlements = append(s.planReadSettlements, cloudDSHPlanReadRecord{PlanRevision: planRevision, StepID: stepID,
		ToolCall: toolCall, ResultDigest: append([]byte(nil), resultDigest...)})
	return nil
}

func TestCloudDSHPlanStepArgumentsAreSeparatedFromBusinessArguments(t *testing.T) {
	parsed, err := extractCloudDSHPlanStepArguments(`{"workspace_id":"w1234567","path":"README.md","morde_plan_step_id":"sources","morde_plan_revision":7}`)
	if err != nil || !parsed.HasStepID || parsed.StepID != "sources" || parsed.PlanRevision != 7 {
		t.Fatalf("extractCloudDSHPlanStepArguments() = %#v, %v", parsed, err)
	}
	var business map[string]json.RawMessage
	if err := json.Unmarshal(parsed.BusinessArgs, &business); err != nil || len(business) != 2 ||
		string(business["path"]) != `"README.md"` || string(business["workspace_id"]) != `"w1234567"` {
		t.Fatalf("business arguments = %s, err=%v; plan metadata leaked or business fields changed", parsed.BusinessArgs, err)
	}
	if hasCloudDSHPlanStepMetadata(string(parsed.BusinessArgs)) {
		t.Fatal("reserved plan metadata remained in business arguments")
	}
	plain, err := extractCloudDSHPlanStepArguments(`{"path":"README.md"}`)
	if err != nil || plain.HasStepID || string(plain.BusinessArgs) != `{"path":"README.md"}` {
		t.Fatalf("metadata-free arguments changed: %#v, %v", plain, err)
	}
	for name, raw := range map[string]string{
		"missing revision":    `{"morde_plan_step_id":"sources"}`,
		"missing step":        `{"morde_plan_revision":7}`,
		"invalid step":        `{"morde_plan_step_id":"../sources","morde_plan_revision":7}`,
		"string revision":     `{"morde_plan_step_id":"sources","morde_plan_revision":"7"}`,
		"fractional revision": `{"morde_plan_step_id":"sources","morde_plan_revision":7.5}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := extractCloudDSHPlanStepArguments(raw); err == nil {
				t.Fatalf("invalid plan metadata %s was accepted", raw)
			}
		})
	}
}

func TestCloudDSHPlanMetadataIsExposedOnSideEffectToolSchemas(t *testing.T) {
	definitions := []AgentToolDefinition{
		{ModelTool: ModelTool{Name: runtimeToolFileRead, Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)}},
		{ModelTool: ModelTool{Name: cloudDSHWebSearchToolName, Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}},
		{ModelTool: ModelTool{Name: cloudDSHMCPCallToolName, Parameters: json.RawMessage(`{"type":"object","properties":{"connector_id":{"type":"string"}},"required":["connector_id"],"additionalProperties":false}`)}},
		{ModelTool: ModelTool{Name: cloudDSHMCPDiscoverToolName, Parameters: json.RawMessage(`{"type":"object","properties":{"connector_id":{"type":"string"}},"additionalProperties":false}`)}},
	}
	got, err := addPlanStepArgumentToHostTools(definitions, 9)
	if err != nil {
		t.Fatalf("addPlanStepArgumentToHostTools() error = %v", err)
	}
	got, err = addPlanStepArgumentToMCPTools(got, 9)
	if err != nil {
		t.Fatalf("addPlanStepArgumentToMCPTools() error = %v", err)
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	var revisionSchema struct {
		Enum []int64 `json:"enum"`
	}
	for _, index := range []int{0, 1, 2} {
		if err := json.Unmarshal(got[index].Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		if len(schema.Properties[cloudDSHPlanStepIDArgumentName]) == 0 || len(schema.Properties[cloudDSHPlanRevisionArgumentName]) == 0 ||
			!containsString(schema.Required, cloudDSHPlanStepIDArgumentName) || !containsString(schema.Required, cloudDSHPlanRevisionArgumentName) {
			t.Fatalf("side-effect schema %q omits required plan metadata: %s", got[index].Name, got[index].Parameters)
		}
		if err := json.Unmarshal(schema.Properties[cloudDSHPlanRevisionArgumentName], &revisionSchema); err != nil ||
			len(revisionSchema.Enum) != 1 || revisionSchema.Enum[0] != 9 {
			t.Fatalf("plan revision schema = %s, err=%v; want current revision enum", schema.Properties[cloudDSHPlanRevisionArgumentName], err)
		}
	}
	var discoverySchema map[string]json.RawMessage
	if err := json.Unmarshal(got[3].Parameters, &discoverySchema); err != nil {
		t.Fatal(err)
	}
	var discoveryProperties map[string]json.RawMessage
	if err := json.Unmarshal(discoverySchema["properties"], &discoveryProperties); err != nil ||
		len(discoveryProperties[cloudDSHPlanStepIDArgumentName]) != 0 || len(discoveryProperties[cloudDSHPlanRevisionArgumentName]) != 0 {
		t.Fatalf("MCP discovery schema received plan metadata: %s, err=%v", got[3].Parameters, err)
	}
}

func TestCloudDSHPlanStepSelectionRequiresCurrentPersistedStep(t *testing.T) {
	currentRevision := int64(4)
	details := TaskDetails{Task: task.Task{CurrentRevision: &currentRevision}, Plan: &task.Plan{
		TaskRevision: 4, Revision: 9, Steps: []task.PlanStep{{ID: "sources"}},
	}}
	for name, tc := range map[string]struct {
		details  TaskDetails
		metadata cloudDSHPlanStepArguments
		wantErr  bool
	}{
		"valid current step": {details: details, metadata: cloudDSHPlanStepArguments{HasStepID: true, StepID: "sources", PlanRevision: 9}},
		"missing metadata":   {details: details, wantErr: true},
		"unknown step":       {details: details, metadata: cloudDSHPlanStepArguments{HasStepID: true, StepID: "model-invented", PlanRevision: 9}, wantErr: true},
		"stale revision":     {details: details, metadata: cloudDSHPlanStepArguments{HasStepID: true, StepID: "sources", PlanRevision: 8}, wantErr: true},
		"no plan":            {details: TaskDetails{Task: task.Task{CurrentRevision: &currentRevision}}},
		"metadata without plan": {details: TaskDetails{Task: task.Task{CurrentRevision: &currentRevision}},
			metadata: cloudDSHPlanStepArguments{HasStepID: true, StepID: "sources", PlanRevision: 9}, wantErr: true},
		"stale task revision": {details: TaskDetails{Task: task.Task{CurrentRevision: &currentRevision}, Plan: &task.Plan{
			TaskRevision: 3, Revision: 9, Steps: []task.PlanStep{{ID: "sources"}},
		}}, metadata: cloudDSHPlanStepArguments{HasStepID: true, StepID: "sources", PlanRevision: 9}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := validateCloudDSHPlanStepSelection(tc.details, tc.metadata, "MCP operation")
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateCloudDSHPlanStepSelection() err=%v, wantErr=%t", err, tc.wantErr)
			}
		})
	}
}

func TestCloudDSHPlanMetadataIsExposedOnlyOnDirectReadTools(t *testing.T) {
	hubReadNames := []string{
		hubTaskToolName("morde_hub_screenshot", "i7654321", 5),
		hubTaskToolName("morde_hub_files", "i7654321", 5),
		hubTaskToolName("morde_hub_file_get", "i7654321", 5),
		hubTaskToolName("morde_hub_file_fetch", "i7654321", 5),
	}
	readNames := []string{
		cloudDSHSkillReadToolName, cloudDSHTaskAttachmentReadToolName,
		cloudDSHConversationHistorySearchToolName, cloudDSHRecentHistorySummaryToolName,
		cloudDSHCurrentHistorySearchToolName, cloudDSHCurrentMessageReadToolName, cloudDSHCurrentMessageSearchToolName,
		cloudDSHWorkspaceListToolName, cloudDSHWorkspaceReadToolName, cloudDSHWorkspaceSearchToolName,
		cloudDSHImpactMapToolName, cloudDSHGoalListToolName, cloudDSHScheduleListToolName, cloudDSHScheduleRunsToolName,
	}
	readNames = append(readNames, hubReadNames...)
	nonReadNames := []string{
		cloudDSHGoalCreateToolName, cloudDSHScheduleCreateToolName, runtimeToolFileRead,
		cloudDSHWebSearchToolName, cloudDSHWebReadToolName, cloudDSHMCPCallToolName, cloudDSHMCPDiscoverToolName,
		hubTaskToolName("morde_hub_command", "i7654321", 5),
	}
	definitions := make([]AgentToolDefinition, 0, len(readNames)+len(nonReadNames))
	for _, name := range append(append([]string(nil), readNames...), nonReadNames...) {
		definitions = append(definitions, AgentToolDefinition{ModelTool: ModelTool{Name: name,
			Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}})
	}
	got, err := addPlanStepArgumentToDirectReadTools(definitions, 7)
	if err != nil {
		t.Fatalf("addPlanStepArgumentToDirectReadTools() error = %v", err)
	}
	for index, name := range append(append([]string(nil), readNames...), nonReadNames...) {
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(got[index].Parameters, &schema); err != nil {
			t.Fatalf("decode %q schema: %v", name, err)
		}
		if index < len(readNames) {
			var revision struct {
				Enum []int64 `json:"enum"`
			}
			if len(schema.Properties[cloudDSHPlanStepIDArgumentName]) == 0 || len(schema.Properties[cloudDSHPlanRevisionArgumentName]) == 0 ||
				!containsString(schema.Required, cloudDSHPlanStepIDArgumentName) || !containsString(schema.Required, cloudDSHPlanRevisionArgumentName) ||
				json.Unmarshal(schema.Properties[cloudDSHPlanRevisionArgumentName], &revision) != nil || len(revision.Enum) != 1 || revision.Enum[0] != 7 {
				t.Fatalf("direct read tool %q schema lacks exact required plan metadata: %s", name, got[index].Parameters)
			}
		} else if len(schema.Properties[cloudDSHPlanStepIDArgumentName]) != 0 || len(schema.Properties[cloudDSHPlanRevisionArgumentName]) != 0 ||
			containsString(schema.Required, cloudDSHPlanStepIDArgumentName) || containsString(schema.Required, cloudDSHPlanRevisionArgumentName) {
			t.Fatalf("non-read tool %q received direct-read plan metadata: %s", name, got[index].Parameters)
		}
	}
}

func TestCloudDSHPlanMetadataIsExposedForHubCommand(t *testing.T) {
	commandName := hubTaskToolName("morde_hub_command", "i7654321", 5)
	readName := hubTaskToolName("morde_hub_files", "i7654321", 5)
	definitions := []AgentToolDefinition{
		{ModelTool: ModelTool{Name: commandName,
			Parameters: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"timeout_ms":{"type":"integer"}},"required":["command","timeout_ms"],"additionalProperties":false}`)}},
		{ModelTool: ModelTool{Name: readName,
			Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}`)}},
	}
	got, err := addPlanStepArgumentToHubOperationTools(definitions, 7)
	if err != nil {
		t.Fatalf("addPlanStepArgumentToHubOperationTools() error = %v", err)
	}
	var commandSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(got[0].Parameters, &commandSchema); err != nil {
		t.Fatal(err)
	}
	var revision struct {
		Enum []int64 `json:"enum"`
	}
	if len(commandSchema.Properties[cloudDSHPlanStepIDArgumentName]) == 0 ||
		len(commandSchema.Properties[cloudDSHPlanRevisionArgumentName]) == 0 ||
		!containsString(commandSchema.Required, cloudDSHPlanStepIDArgumentName) ||
		!containsString(commandSchema.Required, cloudDSHPlanRevisionArgumentName) ||
		json.Unmarshal(commandSchema.Properties[cloudDSHPlanRevisionArgumentName], &revision) != nil ||
		len(revision.Enum) != 1 || revision.Enum[0] != 7 {
		t.Fatalf("Hub command schema lacks exact required plan metadata: %s", got[0].Parameters)
	}
	var readSchema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(got[1].Parameters, &readSchema); err != nil {
		t.Fatal(err)
	}
	if len(readSchema.Properties[cloudDSHPlanStepIDArgumentName]) != 0 ||
		len(readSchema.Properties[cloudDSHPlanRevisionArgumentName]) != 0 {
		t.Fatalf("Hub read tool received operation metadata: %s", got[1].Parameters)
	}
}

func TestCloudDSHPlanStepReadAdmissionAndSettlement(t *testing.T) {
	t.Run("missing or stale selector is rejected before read", func(t *testing.T) {
		for name, metadata := range map[string]cloudDSHPlanStepArguments{
			"missing": {BusinessArgs: json.RawMessage(`{}`)},
			"stale":   {HasStepID: true, StepID: "sources", PlanRevision: 1, BusinessArgs: json.RawMessage(`{}`)},
		} {
			t.Run(name, func(t *testing.T) {
				broker, store, current, ctx, call := newCloudDSHPlanStepReadFixture(t)
				readCalled := false
				_, err := broker.executePlanStepRead(ctx, current, call, metadata, func() (AgentToolResult, error) {
					readCalled = true
					return AgentToolResult{Status: AgentToolSucceeded, Output: "should not be read"}, nil
				})
				if domain.CodeOf(err) != domain.CodeConflict || readCalled || len(store.planReadAdmissions) != 0 {
					t.Fatalf("executePlanStepRead() error=%v read=%t admissions=%d; want pre-read conflict", err, readCalled, len(store.planReadAdmissions))
				}
			})
		}
	})

	t.Run("successful read settles after durable step", func(t *testing.T) {
		broker, store, current, ctx, call, metadata := newCloudDSHPlanStepReadFixtureWithMetadata(t)
		result, err := broker.executePlanStepRead(ctx, current, call, metadata, func() (AgentToolResult, error) {
			store.mu.Lock()
			store.planReadEvents = append(store.planReadEvents, "read")
			store.mu.Unlock()
			step, stepErr := newAgentStep(current.ID, 1, "tool", task.StepSucceeded, "Read source", "Read completed.")
			if stepErr != nil {
				return AgentToolResult{}, stepErr
			}
			if commitErr := broker.commitProgress(ctx, current, step, current.Usage, task.StatusRunning, "", "task.step", map[string]any{"tool": call.Name}); commitErr != nil {
				return AgentToolResult{}, commitErr
			}
			return AgentToolResult{Status: AgentToolSucceeded, Output: "source contents"}, nil
		})
		if err != nil || result.Status != AgentToolSucceeded {
			t.Fatalf("executePlanStepRead() = %#v, %v", result, err)
		}
		if got := strings.Join(store.planReadEvents, ","); got != "admit,read,settle" {
			t.Fatalf("read lifecycle order = %q; want admission before read and settlement afterward", got)
		}
		if len(store.planReadAdmissions) != 1 || len(store.planReadSettlements) != 1 {
			t.Fatalf("admissions=%d settlements=%d; want exactly one each", len(store.planReadAdmissions), len(store.planReadSettlements))
		}
		callIdentity := store.planReadAdmissions[0].ToolCall
		if callIdentity.CallKey != call.CallID || callIdentity.ToolName != call.Name || callIdentity.TaskRevision != *current.CurrentRevision ||
			callIdentity.AgentSessionID != current.AgentSessionID || len(callIdentity.RequestDigest) != sha256.Size ||
			store.planReadSettlements[0].PlanRevision != metadata.PlanRevision || store.planReadSettlements[0].StepID != metadata.StepID ||
			len(store.planReadSettlements[0].ResultDigest) != sha256.Size {
			t.Fatalf("direct read call/result binding = admission:%+v settlement:%+v", store.planReadAdmissions[0], store.planReadSettlements[0])
		}
	})

	t.Run("failed read does not settle", func(t *testing.T) {
		broker, store, current, ctx, call, metadata := newCloudDSHPlanStepReadFixtureWithMetadata(t)
		result, err := broker.executePlanStepRead(ctx, current, call, metadata, func() (AgentToolResult, error) {
			store.mu.Lock()
			store.planReadEvents = append(store.planReadEvents, "read")
			store.mu.Unlock()
			return AgentToolResult{Status: AgentToolFailed, Output: "read failed"}, nil
		})
		if err != nil || result.Status != AgentToolFailed || len(store.planReadAdmissions) != 1 || len(store.planReadSettlements) != 0 {
			t.Fatalf("failed read = %#v, %v admissions=%d settlements=%d", result, err, len(store.planReadAdmissions), len(store.planReadSettlements))
		}
	})

	t.Run("settled replay mismatch becomes unknown", func(t *testing.T) {
		broker, store, current, ctx, call, metadata := newCloudDSHPlanStepReadFixtureWithMetadata(t)
		read := func(output string) func() (AgentToolResult, error) {
			return func() (AgentToolResult, error) {
				step, stepErr := newAgentStep(current.ID, 1, "tool", task.StepSucceeded, "Read source", "Read completed.")
				if stepErr != nil {
					return AgentToolResult{}, stepErr
				}
				if commitErr := broker.commitProgress(ctx, current, step, current.Usage, task.StatusRunning, "", "task.step", map[string]any{"tool": call.Name}); commitErr != nil {
					return AgentToolResult{}, commitErr
				}
				return AgentToolResult{Status: AgentToolSucceeded, Output: output}, nil
			}
		}
		first, firstErr := broker.executePlanStepRead(ctx, current, call, metadata, read("first result"))
		if firstErr != nil || first.Status != AgentToolSucceeded {
			t.Fatalf("initial read = %#v, %v", first, firstErr)
		}
		replayed, replayErr := broker.executePlanStepRead(ctx, current, call, metadata, read("changed result"))
		if domain.CodeOf(replayErr) != domain.CodeConflict || replayed.Status != AgentToolUnknown || len(store.planReadSettlements) != 1 {
			t.Fatalf("changed replay = %#v, %v settlements=%d; want unknown/conflict without replacing original receipt", replayed, replayErr, len(store.planReadSettlements))
		}
	})
}

func TestCloudDSHPlanStepReadResultDigestIncludesImages(t *testing.T) {
	base, err := cloudDSHPlanStepReadResultDigest(AgentToolResult{Status: AgentToolSucceeded, Output: "same output"})
	if err != nil {
		t.Fatal(err)
	}
	withImage, err := cloudDSHPlanStepReadResultDigest(AgentToolResult{Status: AgentToolSucceeded, Output: "same output",
		Images: []ModelImage{{MediaType: "image/png", Data: []byte{1, 2, 3}}}})
	if err != nil {
		t.Fatal(err)
	}
	withChangedData, err := cloudDSHPlanStepReadResultDigest(AgentToolResult{Status: AgentToolSucceeded, Output: "same output",
		Images: []ModelImage{{MediaType: "image/png", Data: []byte{1, 2, 4}}}})
	if err != nil {
		t.Fatal(err)
	}
	withChangedType, err := cloudDSHPlanStepReadResultDigest(AgentToolResult{Status: AgentToolSucceeded, Output: "same output",
		Images: []ModelImage{{MediaType: "image/jpeg", Data: []byte{1, 2, 3}}}})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(base, withImage) || bytes.Equal(withImage, withChangedData) || bytes.Equal(withImage, withChangedType) {
		t.Fatalf("image result changes were not reflected in result digest: base=%x image=%x data=%x type=%x", base, withImage, withChangedData, withChangedType)
	}
}

func newCloudDSHPlanStepReadFixture(t *testing.T) (*CloudDSHToolBroker, *cloudDSHBrokerTaskStore, task.Task, context.Context, ModelToolCall) {
	t.Helper()
	broker, store, current, ctx, call, _ := newCloudDSHPlanStepReadFixtureWithMetadata(t)
	return broker, store, current, ctx, call
}

func newCloudDSHPlanStepReadFixtureWithMetadata(t *testing.T) (*CloudDSHToolBroker, *cloudDSHBrokerTaskStore, task.Task,
	context.Context, ModelToolCall, cloudDSHPlanStepArguments) {
	t.Helper()
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	current := configureInvalidRuntimeToolTask(&store.details.Task)
	current.ExecutionLocation = "server"
	current.Status = task.StatusRunning
	store.details.Task = current
	store.details.Plan = &task.Plan{TaskRevision: *current.CurrentRevision, Revision: 2,
		Goal: "Read a source", Scope: "Inspect the selected source", VerificationMethod: "Check the returned content",
		CostEstimateState: "unknown", Steps: []task.PlanStep{{ID: "sources", Title: "Read source",
			Status: task.PlanStepPending, CompletionCriteria: []string{"the source is read"}}}}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	revision := store.details.Plan.Revision
	original := invalidRuntimeToolCall(current, "call-direct-plan-read", cloudDSHCurrentHistorySearchToolName,
		`{"query":"history","morde_plan_step_id":"sources","morde_plan_revision":2}`)
	ctx := withCloudDSHToolActivity(context.Background(), current, original, nil)
	metadata, err := extractCloudDSHPlanStepArguments(original.Arguments)
	if err != nil || !metadata.HasStepID || metadata.PlanRevision != revision {
		t.Fatalf("extract direct read plan metadata = %#v, %v", metadata, err)
	}
	call := original
	call.Arguments = string(metadata.BusinessArgs)
	return broker, store, current, ctx, call, metadata
}

func setCloudDSHPlanStepMetadataForTest(t *testing.T, call ModelToolCall, stepID string, revision *int64) ModelToolCall {
	t.Helper()
	var arguments map[string]json.RawMessage
	if err := json.Unmarshal([]byte(call.Arguments), &arguments); err != nil {
		t.Fatalf("decode DSH tool arguments: %v", err)
	}
	delete(arguments, cloudDSHPlanStepIDArgumentName)
	delete(arguments, cloudDSHPlanRevisionArgumentName)
	if stepID != "" {
		encoded, err := json.Marshal(stepID)
		if err != nil {
			t.Fatalf("encode step ID: %v", err)
		}
		arguments[cloudDSHPlanStepIDArgumentName] = encoded
	}
	if revision != nil {
		encoded, err := json.Marshal(*revision)
		if err != nil {
			t.Fatalf("encode plan revision: %v", err)
		}
		arguments[cloudDSHPlanRevisionArgumentName] = encoded
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("encode DSH tool arguments: %v", err)
	}
	call.Arguments = string(encoded)
	return call
}

func TestCloudDSHToolBrokerStripsPlanMetadataBeforeHostDispatch(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "window.list", action: "list", result: `{"windows":[]}`,
	}})
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	current := configureInvalidRuntimeToolTask(&store.details.Task)
	current.ExecutionLocation = "server"
	store.details.Task = current
	store.details.Plan = &task.Plan{TaskRevision: *current.CurrentRevision, Revision: 2, Goal: "Read the project file",
		Scope: "Read the requested file", VerificationMethod: "Inspect the returned content", CostEstimateState: "unknown",
		Steps: []task.PlanStep{{ID: "sources", Title: "List windows", Status: task.PlanStepPending,
			CompletionCriteria: []string{"window list is returned"}}}}
	session, err := fixture.executor.connect(context.Background(), current, fixture.fence)
	if err != nil {
		t.Fatal(err)
	}
	fixture.fences.fence = agentFenceFromRuntime(session.Fence)
	session.Close()
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	definitions, err := broker.Definitions(context.Background(), current)
	if err != nil {
		t.Fatalf("Definitions() error = %v", err)
	}
	var fileReadDefinition AgentToolDefinition
	for _, definition := range definitions {
		if definition.Name == runtimeToolWindowList {
			fileReadDefinition = definition
			break
		}
	}
	if len(fileReadDefinition.Parameters) == 0 || !strings.Contains(string(fileReadDefinition.Parameters), `"morde_plan_revision"`) ||
		!strings.Contains(string(fileReadDefinition.Parameters), `"morde_plan_step_id"`) {
		t.Fatalf("current plan metadata was not exposed to the Host tool schema: %s", fileReadDefinition.Parameters)
	}

	call := invalidRuntimeToolCall(current, "call_plan_metadata_read", runtimeToolWindowList,
		`{"morde_plan_step_id":"sources","morde_plan_revision":2}`)
	for name, arguments := range map[string]string{
		"missing metadata":   `{}`,
		"unknown model step": `{"morde_plan_step_id":"model-invented","morde_plan_revision":2}`,
		"stale revision":     `{"morde_plan_step_id":"sources","morde_plan_revision":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			invalid := invalidRuntimeToolCall(current, "call_invalid_plan_metadata_"+strings.ReplaceAll(name, " ", "_"), runtimeToolWindowList, arguments)
			if _, err := broker.Execute(context.Background(), current, invalid); domain.CodeOf(err) != domain.CodeConflict {
				t.Fatalf("invalid plan metadata dispatch error=%v; want conflict", err)
			}
			if fixture.handlers["window.list"].calls.Load() != 0 || store.operation.ID != "" {
				t.Fatalf("invalid metadata reached Host admission: calls=%d operation=%+v", fixture.handlers["window.list"].calls.Load(), store.operation)
			}
		})
	}
	var runtimeArguments json.RawMessage
	fixture.wrap = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodPost && request.URL.Path == "/runtime/v1/operations" {
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Errorf("read Runtime request body: %v", err)
				}
				request.Body = io.NopCloser(bytes.NewReader(body))
				var payload struct {
					Arguments json.RawMessage `json:"arguments"`
				}
				if err := json.Unmarshal(body, &payload); err == nil {
					runtimeArguments = append(json.RawMessage(nil), payload.Arguments...)
				}
			}
			next.ServeHTTP(writer, request)
		})
	}
	result, err := broker.Execute(context.Background(), current, call)
	if err != nil || result.Status != AgentToolSucceeded || fixture.handlers["window.list"].calls.Load() != 1 {
		t.Fatalf("Execute() = %#v, %v; Host calls=%d operation=%+v steps=%+v progress=%+v runtime_args=%s",
			result, err, fixture.handlers["window.list"].calls.Load(), store.operation, store.details.Steps,
			store.committedTaskProgress, runtimeArguments)
	}
	var business map[string]json.RawMessage
	if err := json.Unmarshal(runtimeArguments, &business); err != nil || len(business) != 0 ||
		len(business[cloudDSHPlanStepIDArgumentName]) != 0 || len(business[cloudDSHPlanRevisionArgumentName]) != 0 {
		t.Fatalf("runtime received plan metadata or invalid business args: %s, err=%v", runtimeArguments, err)
	}
	expectedHash, err := agentOperationHash(call.Name, json.RawMessage(call.Arguments))
	if err != nil || !bytes.Equal(store.operation.RequestHash, expectedHash[:]) {
		t.Fatalf("stored operation hash does not bind the original DSH call: %x, err=%v", store.operation.RequestHash, err)
	}
	if len(store.toolCalls) == 0 || !bytes.Equal(store.toolCalls[0].RequestDigest, expectedHash[:]) {
		t.Fatalf("persisted Task tool-call digest does not retain original metadata: %+v", store.toolCalls)
	}
}

// ListTaskEvents returns the events the fake recorded, in order. The broker's
// E02 coverage check reads the recorded impact map from them.
func (s *cloudDSHBrokerTaskStore) ListTaskEvents(_ context.Context, _, _ string, _ int64, _ int32) ([]task.Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]task.Event(nil), s.events...), nil
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

func TestCloudDSHToolBrokerReturnsArgumentSchemaFailureToAgentBeforeOperationCreation(t *testing.T) {
	for name, arguments := range map[string]string{
		"missing workspace id":   `{"path":"README.md"}`,
		"malformed workspace id": `{"workspace_id":123,"path":"README.md"}`,
		"invalid workspace id":   `{"workspace_id":"bad-id","path":"README.md"}`,
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
				capability: "file.read", action: "read", result: `{"path":"README.md","text":"unused"}`,
			}})
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			current := configureInvalidRuntimeToolTask(&store.details.Task)
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}

			call := invalidRuntimeToolCall(current, "call_invalid_workspace_args", runtimeToolFileRead, arguments)
			result, err := broker.Execute(context.Background(), current, call)
			if err != nil || result.Status != AgentToolFailed || result.Output != "The tool call was not executed because its arguments do not match the declared input format. Correct the arguments and try again." {
				t.Fatalf("Execute() = %#v, %v; want sanitized argument failure for the Agent", result, err)
			}
			if strings.Contains(result.Output, "bad-id") || strings.Contains(result.Output, "private") {
				t.Fatalf("raw argument detail escaped to the Agent: %q", result.Output)
			}
			if len(store.operations) != 0 || fixture.handlers["file.read"].calls.Load() != 0 {
				t.Fatalf("invalid arguments created an operation or dispatched: operations=%d calls=%d", len(store.operations), fixture.handlers["file.read"].calls.Load())
			}
			if len(store.details.Steps) != 1 || store.details.Steps[0].Status != task.StepFailed ||
				store.details.Steps[0].Summary != "The tool arguments were invalid; the action was not executed." ||
				store.details.Task.Usage.ToolCalls != 1 || store.details.Steps[0].AgentSessionID != call.TaskFence.AgentSessionID ||
				store.details.Steps[0].ToolCallKey != call.CallID || store.details.Steps[0].FailureReason != "" ||
				store.details.Steps[0].Retryable ||
				!bytes.Equal(store.details.Steps[0].RequestDigest, cloudDSHInvalidRuntimeToolArgumentsDigest(call.Name, call.Arguments)) {
				t.Fatalf("invalid argument step was not durably recorded: task=%#v steps=%#v", store.details.Task, store.details.Steps)
			}
		})
	}
}

func TestCloudDSHToolBrokerReturnsInvalidRuntimeArgumentsIdempotently(t *testing.T) {
	for name, arguments := range map[string]string{
		"malformed object":      `{"workspace_id":"w1234567","path":"private-secret"`,
		"array":                 `[]`,
		"null":                  `null`,
		"schema-invalid object": `{"workspace_id":"w1234567"}`,
	} {
		t.Run(name, func(t *testing.T) {
			broker, store, fixture, call := newInvalidRuntimeToolBroker(t, runtimeToolFileRead, arguments)

			first, err := broker.Execute(context.Background(), store.details.Task, call)
			if err != nil || first.Status != AgentToolFailed || first.Output != invalidRuntimeToolArgumentsResult {
				t.Fatalf("Execute() = %#v, %v; want a sanitized pre-dispatch result for the Agent", first, err)
			}
			if len(store.operations) != 0 || fixture.handlers["file.read"].calls.Load() != 0 ||
				store.details.Task.Usage.ToolCalls != 1 || len(store.details.Steps) != 1 {
				t.Fatalf("invalid tool call changed execution/accounting: operations=%d handler_calls=%d usage=%+v steps=%+v",
					len(store.operations), fixture.handlers["file.read"].calls.Load(), store.details.Task.Usage, store.details.Steps)
			}
			step := store.details.Steps[0]
			if step.Status != task.StepFailed || step.Phase != "prepare" || step.FailureReason != "" ||
				step.Retryable || step.AgentSessionID != call.TaskFence.AgentSessionID || step.ToolCallKey != call.CallID ||
				!bytes.Equal(step.RequestDigest, cloudDSHInvalidRuntimeToolArgumentsDigest(call.Name, call.Arguments)) {
				t.Fatalf("invalid argument step lacks exact non-retryable identity: %+v", step)
			}
			if operationHash, hashErr := agentOperationHash(call.Name, json.RawMessage(call.Arguments)); hashErr == nil &&
				bytes.Equal(step.RequestDigest, operationHash[:]) {
				t.Fatal("invalid-argument identity reused an executable operation hash")
			}
			if len(store.toolCalls) != 2 || store.toolCalls[1].FailureReason != "" || store.toolCalls[1].Retryable {
				t.Fatalf("invalid argument progress violates Store failure evidence contract: %+v", store.toolCalls)
			}
			for _, event := range store.events {
				if strings.Contains(string(event.Payload), "private-secret") || strings.Contains(string(event.Payload), "workspace_id") {
					t.Fatalf("malformed raw arguments escaped into event payload: %s", event.Payload)
				}
			}
			if !strings.Contains(string(store.events[len(store.events)-1].Payload), `"failure_reason":"invalid_tool_arguments"`) {
				t.Fatalf("sanitized Task event omitted the argument failure classification: %s", store.events[len(store.events)-1].Payload)
			}

			checkpointOutput := dshToolResultText(first)
			for _, checkpoint := range []*string{nil, &checkpointOutput} {
				replayed, replayErr := broker.Reconcile(context.Background(), store.details.Task, call, checkpoint)
				if replayErr != nil || replayed.Status != AgentToolFailed || replayed.Output != first.Output {
					t.Fatalf("Reconcile(%v) = %#v, %v; want the same recorded pre-dispatch result", checkpoint != nil, replayed, replayErr)
				}
				if len(store.details.Steps) != 1 || store.details.Task.Usage.ToolCalls != 1 || len(store.toolCalls) != 2 {
					t.Fatalf("replay duplicated step/accounting: steps=%d usage=%+v tool-call progress=%d",
						len(store.details.Steps), store.details.Task.Usage, len(store.toolCalls))
				}
			}

			wrongArguments := call
			wrongArguments.Arguments = `[] `
			if _, replayErr := broker.Reconcile(context.Background(), store.details.Task, wrongArguments, nil); domain.CodeOf(replayErr) != domain.CodeIdempotencyConflict {
				t.Fatalf("Reconcile(reused CallID with changed raw input) error = %v; want idempotency conflict", replayErr)
			}
		})
	}
}

func TestCloudDSHToolBrokerFailsClosedForInvalidRuntimeArguments(t *testing.T) {
	t.Run("authorization denied", func(t *testing.T) {
		broker, store, fixture, call := newInvalidRuntimeToolBroker(t, runtimeToolFileRead, `{"workspace_id":"w1234567"}`)
		fixture.authorizer.errors[identity.ScopeComputerObserve] = domain.NewError(domain.CodeForbidden, "private authorization detail")

		result, err := broker.Execute(context.Background(), store.details.Task, call)
		if err == nil || result.Status == AgentToolFailed || domain.CodeOf(err) != domain.CodeForbidden {
			t.Fatalf("Execute(authorization denied) = %#v, %v; want ordinary forbidden error, not Agent retry result", result, err)
		}
		if len(store.details.Steps) != 0 || store.details.Task.Usage.ToolCalls != 0 || len(store.operations) != 0 ||
			fixture.handlers["file.read"].calls.Load() != 0 {
			t.Fatalf("authorization failure was recorded or dispatched: steps=%+v usage=%+v operations=%d handler_calls=%d",
				store.details.Steps, store.details.Task.Usage, len(store.operations), fixture.handlers["file.read"].calls.Load())
		}
	})

	t.Run("scope not declared", func(t *testing.T) {
		broker, store, fixture, call := newInvalidRuntimeToolBroker(t, runtimeToolShellExec, `{"cwd":123}`)
		fixture.authorizer.errors[identity.ScopeComputerOperate] = domain.NewError(domain.CodeForbidden, "operate scope denied")

		result, err := broker.Execute(context.Background(), store.details.Task, call)
		if err == nil || result.Status == AgentToolFailed || domain.CodeOf(err) != domain.CodeForbidden {
			t.Fatalf("Execute(scope denied) = %#v, %v; want ordinary forbidden error, not Agent retry result", result, err)
		}
		if len(store.details.Steps) != 0 || store.details.Task.Usage.ToolCalls != 0 || len(store.operations) != 0 ||
			fixture.handlers["shell.exec"].calls.Load() != 0 {
			t.Fatalf("scope failure was recorded or dispatched: steps=%+v usage=%+v operations=%d handler_calls=%d",
				store.details.Steps, store.details.Task.Usage, len(store.operations), fixture.handlers["shell.exec"].calls.Load())
		}
	})

	t.Run("existing operation", func(t *testing.T) {
		broker, store, fixture, call := newInvalidRuntimeToolBroker(t, runtimeToolFileRead, `{"workspace_id":"w1234567"}`)
		operationID, err := domain.NewOperationID()
		if err != nil {
			t.Fatal(err)
		}
		store.operations[agentOperationKey(call.CallID)] = task.Operation{ID: operationID, TaskID: store.details.Task.ID,
			Key: agentOperationKey(call.CallID), HostID: store.details.Task.HostID}

		result, err := broker.Execute(context.Background(), store.details.Task, call)
		if err != nil || result.Status != AgentToolUnknown {
			t.Fatalf("Execute(existing operation) = %#v, %v; want unknown result", result, err)
		}
		if len(store.details.Steps) != 1 || store.details.Steps[0].Status != task.StepUnknown ||
			store.details.Task.Usage.ToolCalls != 0 || fixture.handlers["file.read"].calls.Load() != 0 {
			t.Fatalf("existing operation was retried/accounted as a new failure: steps=%+v usage=%+v handler_calls=%d",
				store.details.Steps, store.details.Task.Usage, fixture.handlers["file.read"].calls.Load())
		}
	})

	t.Run("operation lookup error", func(t *testing.T) {
		fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
			capability: "file.read", action: "read", result: `{"path":"README.md","text":"unused"}`,
		}})
		base := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
		store := &cloudDSHOperationLookupErrorStore{cloudDSHBrokerTaskStore: base, lookupErr: errors.New("operation journal unavailable")}
		fixture.executor.store = store
		current := configureInvalidRuntimeToolTask(&base.details.Task)
		broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		call := invalidRuntimeToolCall(current, "invalid-lookup-error", runtimeToolFileRead, `{"workspace_id":"w1234567"}`)

		result, err := broker.Execute(context.Background(), current, call)
		if err != nil || result.Status != AgentToolUnknown {
			t.Fatalf("Execute(operation lookup error) = %#v, %v; want unknown result", result, err)
		}
		if len(base.details.Steps) != 1 || base.details.Steps[0].Status != task.StepUnknown ||
			base.details.Task.Usage.ToolCalls != 0 || fixture.handlers["file.read"].calls.Load() != 0 {
			t.Fatalf("operation lookup error was treated as retryable: steps=%+v usage=%+v handler_calls=%d",
				base.details.Steps, base.details.Task.Usage, fixture.handlers["file.read"].calls.Load())
		}
	})

	t.Run("checkpoint mismatch", func(t *testing.T) {
		broker, store, fixture, call := newInvalidRuntimeToolBroker(t, runtimeToolFileRead, `{"workspace_id":"w1234567"}`)
		first, err := broker.Execute(context.Background(), store.details.Task, call)
		if err != nil || first.Status != AgentToolFailed {
			t.Fatalf("initial Execute() = %#v, %v", first, err)
		}
		wrongOutput := dshToolResultText(AgentToolResult{Status: AgentToolFailed, Output: "different result"})

		result, err := broker.Reconcile(context.Background(), store.details.Task, call, &wrongOutput)
		if err != nil || result.Status != AgentToolUnknown || store.details.Steps[0].Status != task.StepUnknown ||
			fixture.handlers["file.read"].calls.Load() != 0 || store.details.Task.Usage.ToolCalls != 1 {
			t.Fatalf("Reconcile(mismatched checkpoint) = %#v, %v task=%+v steps=%+v", result, err, store.details.Task, store.details.Steps)
		}
	})

	t.Run("missing task fence", func(t *testing.T) {
		broker, store, fixture, call := newInvalidRuntimeToolBroker(t, runtimeToolFileRead, `{"workspace_id":"w1234567"}`)
		call.TaskFence = nil

		result, err := broker.Execute(context.Background(), store.details.Task, call)
		if err == nil || result.Status == AgentToolFailed || len(store.details.Steps) != 0 || store.details.Task.Usage.ToolCalls != 0 ||
			fixture.handlers["file.read"].calls.Load() != 0 {
			t.Fatalf("Execute(missing fence) = %#v, %v steps=%+v usage=%+v", result, err, store.details.Steps, store.details.Task.Usage)
		}
	})
}

func newInvalidRuntimeToolBroker(t *testing.T, toolName, arguments string) (*CloudDSHToolBroker, *cloudDSHBrokerTaskStore,
	*runtimeToolExecutorFixture, ModelToolCall) {
	t.Helper()
	capability, action := "file.read", "read"
	if toolName == runtimeToolShellExec {
		capability, action = "shell.exec", "run"
	}
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: capability, action: action, result: `{"path":"README.md","text":"unused"}`,
	}})
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	current := configureInvalidRuntimeToolTask(&store.details.Task)
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := invalidRuntimeToolCall(current, "invalid-"+toolName, toolName, arguments)
	return broker, store, fixture, call
}

func configureInvalidRuntimeToolTask(current *task.Task) task.Task {
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-invalid-args", 1, "session-invalid-args"
	current.Authorization.Version = 1
	return *current
}

func invalidRuntimeToolCall(current task.Task, callID, name, arguments string) ModelToolCall {
	return ModelToolCall{CallID: callID, Name: name, Arguments: arguments,
		TaskFence: &ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
			LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, AgentInstanceID: current.AgentInstanceID,
			AgentGeneration: current.AgentGeneration, AgentSessionID: current.AgentSessionID,
			AuthorizationVersion: current.Authorization.Version}}
}

type cloudDSHOperationLookupErrorStore struct {
	*cloudDSHBrokerTaskStore
	lookupErr error
}

func (s *cloudDSHOperationLookupErrorStore) GetOperation(context.Context, string, string, string) (task.Operation, error) {
	return task.Operation{}, s.lookupErr
}

func TestCloudDSHToolBrokerKeepsAuthorizationFailureOnOriginalPreparePath(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "file.read", action: "read", result: `{"path":"README.md","text":"unused"}`,
	}})
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	fixture.executor.authorizer = &cloudDSHFailScopeCallAuthorizer{
		scope: identity.ScopeComputerObserve, failOn: 2,
		err: domain.NewError(domain.CodeForbidden, "private authorization detail"),
	}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	result, err := broker.Execute(context.Background(), store.details.Task, ModelToolCall{
		CallID: "call_read_denied", Name: runtimeToolFileRead, Arguments: `{"workspace_id":"w1234567","path":"README.md"}`,
	})
	if err != nil || result.Status != AgentToolFailed ||
		result.Output != "The Host tool request was not executed because authorization or Host readiness could not be confirmed." {
		t.Fatalf("authorization failure = %#v, %v; want existing generic pre-dispatch failure", result, err)
	}
	if strings.Contains(result.Output, "private authorization detail") || len(store.operations) != 0 ||
		fixture.handlers["file.read"].calls.Load() != 0 || len(store.details.Steps) != 1 ||
		store.details.Steps[0].Summary != "The Host tool request could not be prepared." {
		t.Fatalf("authorization failure changed classification or dispatched: result=%#v operations=%d calls=%d steps=%#v",
			result, len(store.operations), fixture.handlers["file.read"].calls.Load(), store.details.Steps)
	}
}

type cloudDSHFailScopeCallAuthorizer struct {
	scope  identity.Scope
	failOn int
	calls  int
	err    error
}

func (a *cloudDSHFailScopeCallAuthorizer) AuthorizeTaskAction(_ context.Context, _ string, _ identity.Kind, _ string, _ string, _ []identity.Scope, scope identity.Scope) error {
	if scope != a.scope {
		return nil
	}
	a.calls++
	if a.calls == a.failOn {
		return a.err
	}
	return nil
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

func TestSelectedMCPDefinitionsDoNotDisplaceCurrentMessageSearch(t *testing.T) {
	definitions := []AgentToolDefinition{
		{ModelTool: ModelTool{Name: cloudDSHCurrentHistorySearchToolName}},
		{ModelTool: ModelTool{Name: cloudDSHCurrentMessageReadToolName}},
		{ModelTool: ModelTool{Name: cloudDSHCurrentMessageSearchToolName}},
	}
	for index := 0; len(definitions) < maxCloudDSHToolDefinitions-1; index++ {
		definitions = append(definitions, AgentToolDefinition{ModelTool: ModelTool{Name: fmt.Sprintf("optional_%02d", index)}})
	}
	selectedMCP := []AgentToolDefinition{
		{ModelTool: ModelTool{Name: cloudDSHMCPDiscoverToolName}},
		{ModelTool: ModelTool{Name: cloudDSHMCPCallToolName}},
	}
	got := appendSelectedMCPDefinitionsWithinLimit(definitions, selectedMCP)
	if len(got) != len(definitions) || !containsAgentTool(got, cloudDSHCurrentHistorySearchToolName) ||
		!containsAgentTool(got, cloudDSHCurrentMessageReadToolName) || !containsAgentTool(got, cloudDSHCurrentMessageSearchToolName) ||
		containsAgentTool(got, cloudDSHMCPDiscoverToolName) || containsAgentTool(got, cloudDSHMCPCallToolName) {
		t.Fatalf("optional MCP definitions displaced required history tools at capacity: got=%d history=%t read=%t search=%t MCP=%t/%t",
			len(got), containsAgentTool(got, cloudDSHCurrentHistorySearchToolName), containsAgentTool(got, cloudDSHCurrentMessageReadToolName),
			containsAgentTool(got, cloudDSHCurrentMessageSearchToolName),
			containsAgentTool(got, cloudDSHMCPDiscoverToolName), containsAgentTool(got, cloudDSHMCPCallToolName))
	}
}

func (s *cloudDSHBrokerTaskStore) CommitTaskProgress(ctx context.Context, progress TaskProgress) (task.Event, error) {
	if err := ctx.Err(); err != nil {
		return task.Event{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.committedTaskProgress = append(s.committedTaskProgress, progress)
	if s.commitTaskProgressError != nil {
		return task.Event{}, s.commitTaskProgressError
	}
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
				SessionID             string `json:"agent_session_id"`
				SkillID               string `json:"skill_id"`
				SkillVersion          string `json:"skill_version"`
				BundleVersion         string `json:"bundle_version"`
				PersonalSkillRevision int64  `json:"personal_skill_revision"`
				SourceRef             string `json:"source_ref"`
				ContentSHA256         string `json:"content_sha256"`
				OutputSHA256          string `json:"output_sha256"`
				FingerprintSHA256     string `json:"fingerprint_sha256"`
			}
			if json.Unmarshal(payload.Detail["agent_session_id"], &stored.SessionID) != nil ||
				json.Unmarshal(payload.Detail["skill_id"], &stored.SkillID) != nil ||
				json.Unmarshal(payload.Detail["skill_version"], &stored.SkillVersion) != nil ||
				json.Unmarshal(payload.Detail["bundle_version"], &stored.BundleVersion) != nil ||
				json.Unmarshal(payload.Detail["personal_skill_revision"], &stored.PersonalSkillRevision) != nil ||
				json.Unmarshal(payload.Detail["source_ref"], &stored.SourceRef) != nil ||
				json.Unmarshal(payload.Detail["content_sha256"], &stored.ContentSHA256) != nil ||
				json.Unmarshal(payload.Detail["output_sha256"], &stored.OutputSHA256) != nil ||
				json.Unmarshal(payload.Detail["fingerprint_sha256"], &stored.FingerprintSHA256) != nil ||
				stored.SessionID != read.AgentSessionID || stored.SkillID != read.SkillID || stored.SkillVersion != read.SkillVersion ||
				stored.BundleVersion != read.BundleVersion || stored.PersonalSkillRevision != read.PersonalSkillRevision || stored.SourceRef != read.SourceRef ||
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
	steps := append([]task.Step(nil), progress.Steps...)
	if call := progress.ToolCall; call != nil && len(steps) == 1 {
		revision := call.TaskRevision
		steps[0].AgentSessionID, steps[0].TaskRevision = call.AgentSessionID, &revision
		steps[0].ToolCallKey, steps[0].ToolName = call.CallKey, call.ToolName
		steps[0].RequestDigest, steps[0].Phase, steps[0].DetailState = append([]byte(nil), call.RequestDigest...), call.Phase, call.DetailState
		steps[0].FailureReason, steps[0].Retryable = call.FailureReason, call.Retryable
		if steps[0].Attempt < 1 {
			steps[0].Attempt = 1
		}
	}
	for _, step := range steps {
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
	if progress.ToolCall != nil {
		toolCall := *progress.ToolCall
		toolCall.DetailCiphertext = append([]byte(nil), progress.ToolCall.DetailCiphertext...)
		s.toolCalls = append(s.toolCalls, toolCall)
	}
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

func TestCloudDSHToolBrokerPersistsSafeDefinitiveFailureReason(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	current := store.details.Task
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-failure", 1, "session-failure"
	current.Authorization.Version = 1
	store.details.Task = current
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelToolCall{CallID: "failure-call-1", Name: cloudDSHWorkspaceReadToolName,
		Arguments: `{"path":"private-input-value"}`,
		TaskFence: &ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
			LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, AgentInstanceID: current.AgentInstanceID,
			AgentGeneration: current.AgentGeneration, AgentSessionID: current.AgentSessionID,
			AuthorizationVersion: current.Authorization.Version}}
	ctx := withCloudDSHToolActivity(context.Background(), current, call, nil)
	step, err := newAgentStep(current.ID, 1, "tool", task.StepRunning, "Read file", "Reading the selected file.")
	if err != nil {
		t.Fatal(err)
	}
	result, err := broker.commitResult(ctx, current, step, current.Usage, call.Name,
		AgentToolResult{Status: AgentToolFailed, Output: "private-error-detail and credential-value"})
	if err != nil || result.Status != AgentToolFailed || len(store.toolCalls) != 1 {
		t.Fatalf("commit definitive Host failure = %+v, err=%v tool calls=%d", result, err, len(store.toolCalls))
	}
	progress := store.toolCalls[0]
	if progress.FailureReason != taskToolExecutionFailureReason || progress.Retryable || progress.Phase != "execute" ||
		len(store.details.Steps) != 1 || store.details.Steps[0].FailureReason != taskToolExecutionFailureReason || store.details.Steps[0].Retryable {
		t.Fatalf("persisted Host failure evidence = progress:%+v step:%+v", progress, store.details.Steps)
	}
	var event struct {
		Detail map[string]json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(store.events[0].Payload, &event); err != nil {
		t.Fatal(err)
	}
	var reason string
	if json.Unmarshal(event.Detail["failure_reason"], &reason) != nil || reason != taskToolExecutionFailureReason ||
		bytes.Contains(store.events[0].Payload, []byte("private-input-value")) ||
		bytes.Contains(store.events[0].Payload, []byte("private-error-detail")) ||
		bytes.Contains(store.events[0].Payload, []byte("credential-value")) {
		t.Fatalf("Host failure event exposed unsafe data or omitted its reason: %s", store.events[0].Payload)
	}
}

func TestCloudDSHToolBrokerRedactsDefinitiveShellFailureAndReplaysExactOutput(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "shell.exec", action: "run",
		result: `{"exit_code":7,"stdout":"https://internal.example/path?token=private-token-92","stderr":"api_key=private-key-61","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`,
	}})
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelToolCall{CallID: "call_dsh_failed_shell", Name: runtimeToolShellExec,
		Arguments: `{"workspace_id":"w1234567","command":"run failed check"}`}
	if result, err := broker.Execute(context.Background(), store.details.Task, call); err != nil || result.Status != AgentToolNeedsUser {
		t.Fatalf("initial sensitive call = %#v, %v; want approval pause", result, err)
	}
	resumeCloudDSHApproval(fixture, store, task.PendingRequestApproved, task.PendingDecisionApprove)
	first, err := broker.Reconcile(context.Background(), store.details.Task, call, nil)
	if err != nil || first.Status != AgentToolFailed || fixture.handlers["shell.exec"].calls.Load() != 1 {
		t.Fatalf("first failed shell result = %#v, %v; dispatches=%d", first, err, fixture.handlers["shell.exec"].calls.Load())
	}
	operation := store.operations[agentOperationKey(call.CallID)]
	want := `{"exit_code":7,"timed_out":false,"stdout":"","stderr":"","stdout_truncated":true,"stderr_truncated":true}`
	if operation.Status != task.OperationFailed || operation.Result != first.Output || first.Output != want {
		t.Fatalf("first result/ledger output differs: result=%q ledger=%+v; want %q", first.Output, operation, want)
	}
	for _, secret := range []string{"internal.example", "private-token-92", "private-key-61", "api_key"} {
		if strings.Contains(first.Output, secret) || strings.Contains(operation.Result, secret) {
			t.Fatalf("sensitive shell failure detail %q escaped: output=%q ledger=%q", secret, first.Output, operation.Result)
		}
	}
	checkpoint := dshToolResultText(first)
	replayed, err := broker.Reconcile(context.Background(), store.details.Task, call, &checkpoint)
	if err != nil || replayed.Status != AgentToolFailed || replayed.Output != first.Output || fixture.handlers["shell.exec"].calls.Load() != 1 {
		t.Fatalf("replayed failed shell result = %#v, %v; first=%q dispatches=%d", replayed, err, first.Output, fixture.handlers["shell.exec"].calls.Load())
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
			var schema struct {
				Properties map[string]struct {
					Pattern string `json:"pattern"`
				} `json:"properties"`
			}
			if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
				t.Fatalf("decode skill tool schema: %v", err)
			}
			skillIDPattern, err := regexp.Compile(schema.Properties["skill_id"].Pattern)
			if err != nil || !skillIDPattern.MatchString("sk_0123abcd") || skillIDPattern.MatchString("sk_0123ABCD") ||
				!strings.Contains(definition.Description, "active personal skill") || !strings.Contains(definition.Description, "permissions") {
				t.Fatalf("skill tool does not admit controlled personal Skills: %+v", definition)
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

func TestCloudDSHToolBrokerReadsOnlyAnActiveAuthorizedPersonalSkill(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.ExecutionLocation = "server"
	store.details.Task.AgentInstanceID = "agent_personal_skill"
	store.details.Task.AgentGeneration = 1
	store.details.Task.AgentSessionID = "session_personal_skill"
	store.details.Task.ConversationID = "c1234567"
	store.details.Task.Authorization = task.Authorization{
		Kind: string(identity.KindSession), TokenID: "t1234567", Version: 1,
		Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)},
	}
	expires := time.Now().Add(time.Hour).UTC()
	store.details.Task.Authorization.ExpiresAt = &expires
	store.details.Task.ConversationContext = &task.ConversationContext{
		OwnerID: store.details.Task.OwnerID, InitiatorID: store.details.Task.OwnerID,
		ConversationID: store.details.Task.ConversationID, Scope: "personal",
	}
	const skillID = "sk_0123abcd"
	const skillBody = `{"scope":"personal","status":"active","name":"Weekly review","description":"Review goals","content":"Read the current goals and draft next steps."}`
	mutationStore := &personalMutationStoreStub{object: personal.Object{
		ObjectType: personal.ObjectTypeSkillCandidate, ObjectID: skillID, Revision: 4, Body: json.RawMessage(skillBody),
	}}
	personalData, err := NewPersonalMutationService(mutationStore)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	broker.personalData = personalData
	claimed := store.details.Task
	call := ModelToolCall{CallID: "call_personal_skill_1", Name: cloudDSHSkillReadToolName,
		Arguments: `{"skill_id":"` + skillID + `"}`}
	result, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("read active personal skill: result=%+v err=%v", result, err)
	}
	var output cloudDSHSkillReadOutput
	if json.Unmarshal([]byte(result.Output), &output) != nil || output.SkillID != skillID || output.Version != "4" ||
		output.BundleVersion != cloudDSHPersonalSkillBundleVersion || output.SourceRef != "personal://"+skillID+"/revision/4" ||
		output.Content != "Read the current goals and draft next steps." {
		t.Fatalf("personal Skill result metadata/content invalid: %+v", output)
	}
	if mutationStore.objectType != personal.ObjectTypeSkillCandidate || mutationStore.objectID != skillID ||
		mutationStore.readPrincipal.UserID != claimed.OwnerID || len(store.committedTaskProgress) != 1 ||
		store.committedTaskProgress[0].CloudDSHSkillRead.PersonalSkillRevision != 4 ||
		strings.Contains(string(store.events[0].Payload), output.Content) {
		t.Fatalf("personal Skill escaped owner scoping or durable read ledger: read=%+v progress=%+v event=%s",
			mutationStore, store.committedTaskProgress, store.events[0].Payload)
	}
	var pending personal.SkillCandidateBody
	if err := json.Unmarshal(mutationStore.object.Body, &pending); err != nil {
		t.Fatal(err)
	}
	pending.Status = personal.SkillCandidateValidated
	mutationStore.object.Body, _ = json.Marshal(pending)
	call.CallID = "call_personal_skill_revoked"
	if _, err := broker.Execute(context.Background(), claimed, call); domain.CodeOf(err) != domain.CodeForbidden ||
		store.details.Task.Usage.ToolCalls != 1 || len(store.events) != 1 {
		t.Fatalf("revoked personal Skill was loaded or charged: err=%v usage=%+v events=%d", err, store.details.Task.Usage, len(store.events))
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
		t.Fatalf("skill catalog does not expose all built-ins: count=%d descriptionBytes=%d hasSkillByID=%v hasLegacy=%v hasNanoMuse=%v",
			len(cloudDSHSkillCatalog), len(definition.Description), strings.Contains(definition.Description, "Morde skill by ID"),
			strings.Contains(definition.Description, "Legacy Muse"), strings.Contains(definition.Description, "nanoMuse skill"))
	}
	for _, id := range []string{"weekly-review-planning", "markdown-docs", "translation"} {
		if !strings.Contains(definition.Description, id) {
			t.Errorf("skill overview omits %q", id)
		}
	}
	var schema struct {
		Properties map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	skillIDPattern, err := regexp.Compile(schema.Properties["skill_id"].Pattern)
	if err != nil {
		t.Fatalf("compile skill ID pattern: %v", err)
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
	for _, id := range toolIDs {
		if !skillIDPattern.MatchString(id) {
			t.Fatalf("skill tool pattern does not allow built-in %q: %#v", id, schema.Properties)
		}
	}
	if !skillIDPattern.MatchString("sk_0123abcd") || skillIDPattern.MatchString("sk_0123ABCD") || skillIDPattern.MatchString("../compare-options") {
		t.Fatalf("skill tool pattern does not constrain personal IDs: %#v", schema.Properties)
	}
	for _, id := range []string{"office", "office-docx", "office-pptx", "office-xlsx", "office-render", "office-recalculate", "office-export-pdf"} {
		if !skillIDPattern.MatchString(id) {
			t.Errorf("Host skill ID %q is absent from the skill pattern", id)
		}
	}
	hostless := cloudDSHSkillReadDefinition(false)
	var hostlessSchema struct {
		Properties map[string]struct {
			Pattern string `json:"pattern"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(hostless.Parameters, &hostlessSchema); err != nil {
		t.Fatal(err)
	}
	hostlessPattern, err := regexp.Compile(hostlessSchema.Properties["skill_id"].Pattern)
	if err != nil {
		t.Fatalf("compile hostless skill ID pattern: %v", err)
	}
	if hostlessPattern.MatchString("office") || hostlessPattern.MatchString("office-xlsx") {
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
	lockedDefinition := CloudDSHAskUserToolDefinition()
	if definitions[0].Description != lockedDefinition.Description || !bytes.Equal(definitions[0].Parameters, lockedDefinition.Parameters) || !definitions[0].Strict {
		t.Fatalf("clarification tool was not locked to the structured Morde definition: got=%#v want=%#v", definitions[0], lockedDefinition)
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

func TestCloudDSHToolBrokerPersistsAndReconcilesStructuredClarification(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.HostID = ""
	store.details.Task.Usage.ToolCalls = 3
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelToolCall{CallID: "call_dsh_group", Name: agentAskUserToolName, Arguments: `{"question":null,"questions":[{"id":"environment","prompt":"Which environment should I use?","type":"single_choice","required":true,"allow_skip":false,"options":[{"id":"staging","label":"Staging"},{"id":"production","label":"Production"}]},{"id":"notes","prompt":"Any other constraints?","type":"free_text","required":false,"allow_skip":true,"options":[]}]}`}
	claimed := store.details.Task
	first, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || first.Status != AgentToolNeedsUser {
		t.Fatalf("structured clarification result = %#v, err=%v; want waiting user", first, err)
	}
	details, err := store.GetTask(context.Background(), claimed.ID, claimed.OwnerID)
	if err != nil || details.Task.Status != task.StatusWaitingUser || details.Task.WaitingReason != "clarification_required" ||
		details.Task.Lease.Owner != "" || details.Task.Usage.ToolCalls != 4 || len(details.ClarificationRequests) != 1 {
		t.Fatalf("persisted structured clarification state = %#v, err=%v", details, err)
	}
	request := details.ClarificationRequests[0]
	if request.TaskID != claimed.ID || request.CallID != call.CallID || request.Version != 1 ||
		request.Question != "Which environment should I use?" || request.AnswerFormat != task.ClarificationAnswerStructured ||
		request.Status != task.ClarificationPending || len(request.Questions) != 2 || request.Questions[0].ID != "environment" ||
		request.Questions[1].ID != "notes" || request.Questions[1].Options == nil || len(details.ClarificationRequests) != 1 {
		t.Fatalf("structured clarification request = %#v", request)
	}
	pending, err := broker.Reconcile(context.Background(), claimed, call, nil)
	if err != nil || pending.Status != AgentToolNeedsUser || store.details.Task.Usage.ToolCalls != 4 || len(store.operations) != 0 {
		t.Fatalf("pending structured clarification reconcile = %#v, err=%v; task=%#v operations=%#v",
			pending, err, store.details.Task, store.operations)
	}
	answers := []task.ClarificationAnswer{
		{QuestionID: "environment", OptionIDs: []string{"staging"}, Text: ""},
		{QuestionID: "notes", OptionIDs: []string{}, Text: "Keep the current defaults."},
	}
	answerBytes, err := json.Marshal(struct {
		QuestionGroupID string                     `json:"question_group_id"`
		Version         int64                      `json:"version"`
		Answers         []task.ClarificationAnswer `json:"answers"`
	}{QuestionGroupID: request.ID, Version: request.Version, Answers: answers})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	responded := time.Now().UTC()
	store.details.ClarificationRequests[0].Status = task.ClarificationAnswered
	store.details.ClarificationRequests[0].Answer = string(answerBytes)
	store.details.ClarificationRequests[0].AnswerFormat = task.ClarificationAnswerStructured
	store.details.ClarificationRequests[0].RespondedAt = &responded
	expires := time.Now().Add(time.Minute).UTC()
	store.details.Task.Status = task.StatusRunning
	store.details.Task.WaitingReason = ""
	store.details.Task.Lease = task.Lease{Owner: "worker-2", Version: claimed.Lease.Version + 1, ExpiresAt: &expires}
	resumed := store.details.Task
	store.mu.Unlock()
	for range 2 {
		answerResult, err := broker.Reconcile(context.Background(), resumed, call, nil)
		if err != nil || answerResult.Status != AgentToolSucceeded || answerResult.Output != string(answerBytes) {
			t.Fatalf("answered structured clarification reconcile = %#v, err=%v; want persisted group answer", answerResult, err)
		}
	}
	if store.details.Task.Usage.ToolCalls != 4 || len(store.details.ClarificationRequests) != 1 || len(store.operations) != 0 {
		t.Fatalf("structured clarification replay changed Task state: task=%#v requests=%#v operations=%#v",
			store.details.Task, store.details.ClarificationRequests, store.operations)
	}
}

func TestCloudDSHToolBrokerRejectsChildAndUpstreamClarificationTools(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.HostID = ""
	store.details.Task.AgentSessionID = "session_root"
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	childCall := ModelToolCall{CallID: "call_child_ask", Name: agentAskUserToolName,
		Arguments: `{"question":"Which environment?","questions":null}`,
		TaskFence: &ModelTaskFence{AgentSessionID: "session_child", AgentRootSessionID: "session_root"}}
	if _, err := broker.Execute(context.Background(), store.details.Task, childCall); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("child clarification error = %v; want forbidden", err)
	}
	if _, err := broker.Execute(context.Background(), store.details.Task, ModelToolCall{
		CallID: "call_upstream_ask", Name: agentAskUserQuestionToolName, Arguments: `{}`,
	}); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("upstream clarification extension error = %v; want forbidden", err)
	}
	if len(store.details.ClarificationRequests) != 0 || len(store.events) != 0 || store.details.Task.Usage.ToolCalls != 0 {
		t.Fatalf("rejected clarification tools changed Task state: task=%#v requests=%#v events=%#v",
			store.details.Task, store.details.ClarificationRequests, store.events)
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
	if pending == nil || pending.Status != task.PendingRequestPending || pending.CallID != call.CallID || pending.ToolName != call.Name ||
		pending.TaskRevision == nil || store.details.Task.CurrentRevision == nil || *pending.TaskRevision != *store.details.Task.CurrentRevision {
		t.Fatalf("sensitive call was not bound to a pending approval: %#v", pending)
	}
	if !strings.Contains(pending.Content, `"command":"echo approved"`) || strings.Contains(pending.Content, `"workspace_id"`) {
		t.Fatalf("approval text does not show the bounded exact-action preview: %#v", pending)
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
	operation, ok := fixture.store.operations[agentOperationKey(call.CallID)]
	if !ok || operation.TaskRevision == nil || *operation.TaskRevision != *store.details.Task.CurrentRevision ||
		operation.CallID != call.CallID || operation.ToolName != call.Name || operation.ApprovalID != pending.ID ||
		operation.ApprovalVersion == nil || *operation.ApprovalVersion != pending.Version ||
		!bytes.Equal(operation.RequestHash, pending.RequestHash) || !bytes.Equal(operation.ApprovalRequestHash, pending.RequestHash) {
		t.Fatalf("dispatched operation lost exact Task/approval identity: operation=%#v pending=%#v", operation, pending)
	}
}

func TestCloudDSHToolBrokerReconcilesApprovedActionAfterLostRuntimeReceipt(t *testing.T) {
	fixture, store, broker, call := newCloudDSHApprovalFixture(t)
	var dropped, operationPosts int
	fixture.wrap = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodPost && request.URL.Path == "/runtime/v1/operations" {
				operationPosts++
			}
			if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/runtime/v1/operations/") && dropped == 0 {
				dropped++
				recorded := httptest.NewRecorder()
				next.ServeHTTP(recorded, request)
				http.Error(writer, "simulated lost runtime receipt", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(writer, request)
		})
	}
	first, err := broker.Execute(context.Background(), store.details.Task, call)
	if err != nil || first.Status != AgentToolNeedsUser || operationPosts != 0 {
		t.Fatalf("initial approval = %#v, %v; posts=%d; want pause before dispatch", first, err, operationPosts)
	}
	resumeCloudDSHApproval(fixture, store, task.PendingRequestApproved, task.PendingDecisionApprove)
	unknown, err := broker.Reconcile(context.Background(), store.details.Task, call, nil)
	if err != nil || unknown.Status != AgentToolUnknown || store.details.Task.Status != task.StatusWaitingUser ||
		store.operation.Status != task.OperationUnknown || fixture.handlers["shell.exec"].calls.Load() != 1 || operationPosts != 1 {
		t.Fatalf("lost receipt result = %#v, %v; task=%#v operation=%#v calls=%d posts=%d; want one executed action and unknown receipt",
			unknown, err, store.details.Task, store.operation, fixture.handlers["shell.exec"].calls.Load(), operationPosts)
	}

	// ClaimTask advances unknown operation lease versions while retaining the original Runtime lease.
	expires := time.Now().Add(time.Minute).UTC()
	store.mu.Lock()
	store.details.Task.Status = task.StatusRunning
	store.details.Task.WaitingReason = ""
	store.details.Task.Lease = task.Lease{Owner: "worker-3", Version: 3, ExpiresAt: &expires}
	claimed := store.details.Task
	operation := store.operations[agentOperationKey(call.CallID)]
	operation.LeaseVersion = claimed.Lease.Version
	store.operations[operation.Key] = operation
	store.operation = operation
	store.mu.Unlock()

	recovered, err := broker.Reconcile(context.Background(), claimed, call, nil)
	if err != nil || recovered.Status != AgentToolSucceeded || store.operation.Status != task.OperationSucceeded ||
		store.operation.RuntimeLeaseVersion != 2 || fixture.handlers["shell.exec"].calls.Load() != 1 || operationPosts != 1 {
		t.Fatalf("reconciled approved action = %#v, %v; operation=%#v calls=%d posts=%d; want journal result without redispatch",
			recovered, err, store.operation, fixture.handlers["shell.exec"].calls.Load(), operationPosts)
	}
}

func TestCloudDSHToolBrokerKeepsFencedActivityAcrossLostReceiptAndRecovery(t *testing.T) {
	fixture, store, broker, call := newCloudDSHApprovalFixture(t)
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	broker.stepDetailSealer = sealer
	current := &store.details.Task
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-activity", 1, "session-root"
	revision := int64(4)
	current.CurrentRevision, current.Authorization.Version = &revision, 2
	call.TaskFence = &ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
		LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, AgentInstanceID: current.AgentInstanceID,
		AgentGeneration: current.AgentGeneration, AgentSessionID: current.AgentSessionID,
		AuthorizationVersion: current.Authorization.Version}
	var dropped, operationPosts int
	fixture.wrap = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodPost && request.URL.Path == "/runtime/v1/operations" {
				operationPosts++
			}
			if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/runtime/v1/operations/") && dropped == 0 {
				dropped++
				recorded := httptest.NewRecorder()
				next.ServeHTTP(recorded, request)
				http.Error(writer, "simulated lost runtime receipt", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(writer, request)
		})
	}

	first, err := broker.Execute(context.Background(), *current, call)
	if err != nil || first.Status != AgentToolNeedsUser || len(store.details.Steps) != 1 ||
		store.details.Steps[0].Status != task.StepWaitingUser {
		t.Fatalf("approval activity = %#v, err=%v, steps=%+v", first, err, store.details.Steps)
	}
	original := store.details.Steps[0]
	if original.AgentSessionID != current.AgentSessionID || original.ToolCallKey != call.CallID ||
		original.TaskRevision == nil || *original.TaskRevision != revision {
		t.Fatalf("approval activity is not bound to the original call: %+v", original)
	}
	pendingDetail := store.toolCalls[len(store.toolCalls)-1]
	plain, err := sealer.OpenWithAAD(pendingDetail.DetailCiphertext, taskStepDetailAAD(current.OwnerID, current.ID, original.ID, revision))
	if err != nil {
		t.Fatal(err)
	}
	var pendingDocument TaskStepDetailDocument
	if err := json.Unmarshal(plain, &pendingDocument); err != nil ||
		(pendingDocument.Execution != nil && pendingDocument.Execution.DurationMS != nil) {
		t.Fatalf("pending approval unexpectedly recorded an execution duration: err=%v", err)
	}
	resumeCloudDSHApproval(fixture, store, task.PendingRequestApproved, task.PendingDecisionApprove)
	call.TaskFence.LeaseOwner, call.TaskFence.LeaseVersion = store.details.Task.Lease.Owner, store.details.Task.Lease.Version
	unknown, err := broker.Reconcile(context.Background(), store.details.Task, call, nil)
	if err != nil || unknown.Status != AgentToolUnknown || len(store.details.Steps) != 1 ||
		store.details.Steps[0].ID != original.ID || store.details.Steps[0].Status != task.StepUnknown ||
		store.operation.Status != task.OperationUnknown || fixture.handlers["shell.exec"].calls.Load() != 1 || operationPosts != 1 {
		t.Fatalf("lost receipt did not preserve the original unknown activity: result=%+v err=%v steps=%+v operation=%+v calls=%d posts=%d",
			unknown, err, store.details.Steps, store.operation, fixture.handlers["shell.exec"].calls.Load(), operationPosts)
	}

	expires := time.Now().Add(time.Minute).UTC()
	store.mu.Lock()
	store.details.Task.Status, store.details.Task.WaitingReason = task.StatusRunning, ""
	store.details.Task.Lease = task.Lease{Owner: "worker-3", Version: 3, ExpiresAt: &expires}
	claimed := store.details.Task
	operation := store.operations[agentOperationKey(call.CallID)]
	operation.LeaseVersion = claimed.Lease.Version
	store.operations[operation.Key], store.operation = operation, operation
	store.mu.Unlock()
	call.TaskFence.LeaseOwner, call.TaskFence.LeaseVersion = claimed.Lease.Owner, claimed.Lease.Version
	recovered, err := broker.Reconcile(context.Background(), claimed, call, nil)
	if err != nil || recovered.Status != AgentToolSucceeded || len(store.details.Steps) != 1 ||
		store.details.Steps[0].ID != original.ID || store.details.Steps[0].Sequence != original.Sequence ||
		store.details.Steps[0].Status != task.StepSucceeded || store.details.Steps[0].ToolCallKey != call.CallID ||
		store.operation.Status != task.OperationSucceeded || fixture.handlers["shell.exec"].calls.Load() != 1 || operationPosts != 1 ||
		store.details.Task.Usage.ToolCalls != 1 {
		t.Fatalf("recovery did not settle the original activity exactly once: result=%+v err=%v steps=%+v operation=%+v calls=%d posts=%d usage=%d",
			recovered, err, store.details.Steps, store.operation, fixture.handlers["shell.exec"].calls.Load(), operationPosts,
			store.details.Task.Usage.ToolCalls)
	}
}

func TestCloudDSHToolBrokerRecordsActualDeferredHostExecution(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "window.list", action: "list", result: `{"windows":[]}`,
	}})
	base := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	store := &deferredBrokerTestStore{cloudDSHBrokerTaskStore: base, target: DeferredTaskTarget{HostID: "h1234567"}}
	fixture.executor.store = store
	session, err := fixture.executor.connect(context.Background(), store.details.Task, fixture.fence)
	if err != nil {
		t.Fatal(err)
	}
	fixture.fences.fence = agentFenceFromRuntime(session.Fence)
	session.Close()
	current := &store.details.Task
	current.HostID, current.WorkspaceID = "", ""
	current.InputRevision, current.WorkspaceCheckpointRevision = nil, nil
	current.ExecutionLocation, current.Authorization.Version = "server", 1
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-deferred", 1, "session-deferred"
	claimed := *current
	call := ModelToolCall{CallID: "deferred-activity", Name: runtimeToolWindowList, Arguments: `{}`,
		TaskFence: &ModelTaskFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID, InitiatorID: claimed.InitiatorID,
			LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version, AgentInstanceID: claimed.AgentInstanceID,
			AgentGeneration: claimed.AgentGeneration, AgentSessionID: claimed.AgentSessionID,
			AuthorizationVersion: claimed.Authorization.Version}}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	broker.stepDetailSealer = sealer

	activityCtx := withCloudDSHToolActivity(context.Background(), claimed, call, sealer)
	for _, change := range []func(*task.Task){
		func(value *task.Task) { value.OwnerID = "u7654321" },
		func(value *task.Task) { value.Authorization.Version++ },
		func(value *task.Task) { value.Lease.Version++ },
		func(value *task.Task) { value.AgentSessionID = "different-session" },
	} {
		wrong := claimed
		wrong.HostID = store.target.HostID
		change(&wrong)
		activity, ok := cloudDSHToolActivityFromContext(withBoundTaskToolActivity(activityCtx, wrong))
		if !ok || activity.current.HostID != "" {
			t.Fatal("mismatched deferred target changed the original activity identity")
		}
	}

	result, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || result.Status != AgentToolSucceeded || fixture.handlers["window.list"].calls.Load() != 1 {
		t.Fatalf("deferred Host execution failed: result=%+v err=%v", result, err)
	}
	progress := store.committedTaskProgress[len(store.committedTaskProgress)-1]
	if progress.ToolCall == nil || len(progress.Steps) != 1 || progress.ToolCall.DetailState != "available" {
		t.Fatalf("completed execution detail was not saved")
	}
	plain, err := sealer.OpenWithAAD(progress.ToolCall.DetailCiphertext,
		taskStepDetailAAD(claimed.OwnerID, claimed.ID, progress.Steps[0].ID, *claimed.CurrentRevision))
	if err != nil {
		t.Fatal(err)
	}
	var detail TaskStepDetailDocument
	if err := json.Unmarshal(plain, &detail); err != nil || detail.Execution == nil || detail.Execution.Location == nil ||
		*detail.Execution.Location != "云端电脑" || detail.Execution.DurationMS == nil || *detail.Execution.DurationMS < 0 {
		t.Fatalf("deferred Host execution lacks a measured duration or actual location: err=%v detail=%+v", err, detail.Execution)
	}
}

func TestCloudDSHToolBrokerRecordsDeferredWorkspaceLocationWithoutHostDuration(t *testing.T) {
	fixture, base, broker, workspaceStore, original := newCloudDSHT08WorkspaceBroker(t)
	store := &deferredWorkspaceBrokerTestStore{deferredBrokerTestStore: &deferredBrokerTestStore{
		cloudDSHBrokerTaskStore: base, target: DeferredTaskTarget{WorkspaceID: original.WorkspaceID},
	}, workspaceStore: workspaceStore}
	fixture.executor.store, broker.store = store, store
	current := &base.details.Task
	current.Authorization.Kind, current.Authorization.Version = string(identity.KindSession), 1
	current.WorkspaceID = ""
	current.InputRevision, current.WorkspaceCheckpointRevision, current.ContextDigest = nil, nil, nil
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-workspace", 1, "session-workspace"
	claimed := *current
	call := ModelToolCall{CallID: "deferred-workspace-activity", Name: cloudDSHWorkspaceListToolName,
		Arguments: `{"expected_revision":-1}`, TaskFence: &ModelTaskFence{TaskID: claimed.ID, OwnerID: claimed.OwnerID,
			InitiatorID: claimed.InitiatorID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
			AgentInstanceID: claimed.AgentInstanceID, AgentGeneration: claimed.AgentGeneration,
			AgentSessionID: claimed.AgentSessionID, AuthorizationVersion: claimed.Authorization.Version}}
	sealer, err := NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	broker.stepDetailSealer = sealer
	result, err := broker.Execute(context.Background(), claimed, call)
	if err != nil || result.Status != AgentToolSucceeded || store.workspaceBinds != 1 {
		t.Fatalf("deferred Workspace execution failed: result=%+v err=%v", result, err)
	}
	progress := base.committedTaskProgress[len(base.committedTaskProgress)-1]
	if progress.ToolCall == nil || len(progress.Steps) != 1 || progress.ToolCall.DetailState != "available" {
		t.Fatal("completed Workspace detail was not saved")
	}
	plain, err := sealer.OpenWithAAD(progress.ToolCall.DetailCiphertext,
		taskStepDetailAAD(claimed.OwnerID, claimed.ID, progress.Steps[0].ID, *claimed.CurrentRevision))
	if err != nil {
		t.Fatal(err)
	}
	var detail TaskStepDetailDocument
	if err := json.Unmarshal(plain, &detail); err != nil || detail.Execution == nil || detail.Execution.Location == nil ||
		*detail.Execution.Location != "云端工作区" || detail.ExecutionWorkspaceID != original.WorkspaceID || detail.Execution.DurationMS != nil {
		t.Fatalf("deferred Workspace location or Host duration is incorrect: err=%v detail=%+v", err, detail.Execution)
	}
}

func TestCloudDSHToolBrokerKeepsUnknownWhenRuntimeOperationQueriesStayUnavailable(t *testing.T) {
	fixture, store, broker, call := newCloudDSHApprovalFixture(t)
	var operationPosts, operationGets int
	fixture.wrap = func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.Method == http.MethodPost && request.URL.Path == "/runtime/v1/operations" {
				operationPosts++
			}
			if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/runtime/v1/operations/") {
				operationGets++
				// Keep the runtime journal result while dropping every read response.
				recorded := httptest.NewRecorder()
				next.ServeHTTP(recorded, request)
				http.Error(writer, "simulated unavailable operation query", http.StatusServiceUnavailable)
				return
			}
			next.ServeHTTP(writer, request)
		})
	}

	first, err := broker.Execute(context.Background(), store.details.Task, call)
	if err != nil || first.Status != AgentToolNeedsUser || operationPosts != 0 {
		t.Fatalf("initial approval = %#v, %v; posts=%d; want pause before dispatch", first, err, operationPosts)
	}
	resumeCloudDSHApproval(fixture, store, task.PendingRequestApproved, task.PendingDecisionApprove)
	unknown, err := broker.Reconcile(context.Background(), store.details.Task, call, nil)
	if err != nil || unknown.Status != AgentToolUnknown || store.details.Task.Status != task.StatusWaitingUser ||
		store.operation.Status != task.OperationUnknown || fixture.handlers["shell.exec"].calls.Load() != 1 || operationPosts != 1 {
		t.Fatalf("unavailable query result = %#v, %v; task=%#v operation=%#v calls=%d posts=%d",
			unknown, err, store.details.Task, store.operation, fixture.handlers["shell.exec"].calls.Load(), operationPosts)
	}
	reservedToolCalls := store.details.Task.Usage.ToolCalls
	if reservedToolCalls < 1 {
		t.Fatalf("unknown operation released its tool-call reservation: usage=%+v", store.details.Task.Usage)
	}

	expires := time.Now().Add(time.Minute).UTC()
	store.mu.Lock()
	store.details.Task.Status = task.StatusRunning
	store.details.Task.WaitingReason = ""
	store.details.Task.Lease = task.Lease{Owner: "worker-3", Version: 3, ExpiresAt: &expires}
	claimed := store.details.Task
	operation := store.operations[agentOperationKey(call.CallID)]
	operation.LeaseVersion = claimed.Lease.Version
	store.operations[operation.Key] = operation
	store.operation = operation
	store.mu.Unlock()

	recovered, err := broker.Reconcile(context.Background(), claimed, call, nil)
	if err != nil || recovered.Status != AgentToolUnknown || store.details.Task.Status != task.StatusWaitingUser ||
		store.operation.Status != task.OperationUnknown || operationGets < 2 || fixture.handlers["shell.exec"].calls.Load() != 1 ||
		operationPosts != 1 || store.details.Task.Usage.ToolCalls != reservedToolCalls {
		t.Fatalf("still-unavailable query recovery = %#v, %v; task=%#v operation=%#v queries=%d calls=%d posts=%d reserved=%d",
			recovered, err, store.details.Task, store.operation, operationGets, fixture.handlers["shell.exec"].calls.Load(), operationPosts,
			store.details.Task.Usage.ToolCalls)
	}
}

func TestCloudDSHToolBrokerChangedApprovedPayloadDoesNotDispatch(t *testing.T) {
	fixture, store, broker, call := newCloudDSHApprovalFixture(t)
	first, err := broker.Execute(context.Background(), store.details.Task, call)
	if err != nil || first.Status != AgentToolNeedsUser {
		t.Fatalf("initial sensitive call = %#v, %v; want approval pause", first, err)
	}
	approvedHash := append([]byte(nil), store.details.PendingRequest.RequestHash...)
	resumeCloudDSHApproval(fixture, store, task.PendingRequestApproved, task.PendingDecisionApprove)
	call.Arguments = `{"workspace_id":"w1234567","command":"rm -rf ~/workspace/project"}`
	result, err := broker.Reconcile(context.Background(), store.details.Task, call, nil)
	if err != nil || result.Status != AgentToolFailed || !strings.Contains(result.Output, "no longer matches this exact action") {
		t.Fatalf("changed approved payload = %#v, %v; want an exact-action mismatch", result, err)
	}
	if store.details.Task.Usage.ToolCalls != 1 || fixture.handlers["shell.exec"].calls.Load() != 0 || len(fixture.store.operations) != 0 ||
		!bytes.Equal(store.details.PendingRequest.RequestHash, approvedHash) {
		t.Fatalf("changed approved payload crossed dispatch or changed the approval: usage=%+v dispatches=%d operations=%d pending=%#v",
			store.details.Task.Usage, fixture.handlers["shell.exec"].calls.Load(), len(fixture.store.operations), store.details.PendingRequest)
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

func TestCloudDSHToolBrokerExhaustedBudgetDoesNotDispatchHostAction(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, []runtimeToolHandlerSpec{{
		capability: "shell.exec", action: "run",
		result: `{"exit_code":0,"stdout":"should not run","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`,
	}})
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	store.details.Task.Budget.ToolCalls = 1
	store.details.Task.Usage.ToolCalls = 1
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := broker.Execute(context.Background(), store.details.Task, ModelToolCall{
		CallID: "call_dsh_no_budget", Name: runtimeToolShellExec,
		Arguments: `{"workspace_id":"w1234567","command":"echo should not run"}`,
	})
	if err != nil || result.Status != AgentToolNeedsUser || store.details.Task.Status != task.StatusWaitingUser ||
		store.details.Task.WaitingReason != "tool_call_budget" || store.details.Task.Usage.ToolCalls != 1 ||
		len(fixture.store.operations) != 0 || fixture.handlers["shell.exec"].calls.Load() != 0 {
		t.Fatalf("exhausted-budget Host action = %#v, err=%v; task=%#v operations=%d dispatches=%d",
			result, err, store.details.Task, len(fixture.store.operations), fixture.handlers["shell.exec"].calls.Load())
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

func TestCloudDSHToolBrokerDoesNotMergeDifferentAuthorizedCallsWithSameArguments(t *testing.T) {
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
	for _, callID := range []string{"call_dsh_window_a", "call_dsh_window_b"} {
		result, err := broker.Execute(context.Background(), store.details.Task, ModelToolCall{
			CallID: callID, Name: runtimeToolWindowList, Arguments: `{}`,
		})
		if err != nil || result.Status != AgentToolSucceeded {
			t.Fatalf("authorized call %s = %#v, err=%v", callID, result, err)
		}
	}
	if fixture.handlers["window.list"].calls.Load() != 2 || len(fixture.store.operations) != 2 {
		t.Fatalf("different authorized call IDs were merged: dispatches=%d operations=%d",
			fixture.handlers["window.list"].calls.Load(), len(fixture.store.operations))
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

type failingCloudDSHOperationStore struct {
	*cloudDSHBrokerTaskStore
	err   error
	calls int
}

func (s *failingCloudDSHOperationStore) CreateOperation(context.Context, task.Operation, string) (task.Operation, bool, error) {
	s.calls++
	return task.Operation{}, false, s.err
}

func TestCloudDSHToolBrokerDoesNotDispatchWhenOperationIntentCannotPersist(t *testing.T) {
	fixture, baseStore, _, call := newCloudDSHApprovalFixture(t)
	store := &failingCloudDSHOperationStore{cloudDSHBrokerTaskStore: baseStore, err: errors.New("simulated operation store failure")}
	fixture.executor.store = store
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := broker.Execute(context.Background(), baseStore.details.Task, call)
	if err != nil || first.Status != AgentToolNeedsUser {
		t.Fatalf("initial sensitive call = %#v, %v; want approval pause", first, err)
	}
	resumeCloudDSHApproval(fixture, baseStore, task.PendingRequestApproved, task.PendingDecisionApprove)
	result, err := broker.Reconcile(context.Background(), baseStore.details.Task, call, nil)
	if err != nil || result.Status != AgentToolFailed || store.calls != 1 || fixture.handlers["shell.exec"].calls.Load() != 0 ||
		len(fixture.store.operations) != 0 {
		t.Fatalf("operation intent persistence failure = %#v, err=%v, persistence attempts=%d dispatches=%d operations=%d",
			result, err, store.calls, fixture.handlers["shell.exec"].calls.Load(), len(fixture.store.operations))
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
	if summary := store.details.Steps[0].Summary; strings.Contains(strings.ToLower(summary), "checkpoint") ||
		!strings.Contains(summary, "could not be safely resumed") {
		t.Fatalf("generic recovery summary asserts an unobserved checkpoint failure: %q", summary)
	}

	fixture = newRuntimeToolExecutorFixture(t, nil)
	store = &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	broker, err = NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.PauseRecoveryDiagnostic(context.Background(), store.details.Task, AgentRecoveryDiagnostic{
		Stage: "private stage containing credentials", ReasonCode: "private raw protocol error",
	}); err != nil {
		t.Fatalf("PauseRecoveryDiagnostic() with unknown values error = %v", err)
	}
	progress := store.committedTaskProgress[0]
	var event struct {
		Summary string            `json:"summary"`
		Detail  map[string]string `json:"detail"`
	}
	if err := json.Unmarshal(progress.Event.Payload, &event); err != nil {
		t.Fatalf("decode recovery event: %v", err)
	}
	if strings.Contains(string(progress.Event.Payload), "private") || strings.Contains(event.Summary, "checkpoint") ||
		event.Detail["recovery_stage"] != "" || event.Detail["recovery_reason"] != "" {
		t.Fatalf("unknown recovery diagnostics were not filtered: summary=%q detail=%#v payload=%s", event.Summary, event.Detail, progress.Event.Payload)
	}
}

func TestCloudDSHToolBrokerModelRecoveryStoresSafeFailureCategory(t *testing.T) {
	for _, category := range []string{"timeout", "empty_response", "truncated_response", "invalid_response", "unauthorized_tool"} {
		t.Run(category, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := broker.PauseModelRecovery(context.Background(), store.details.Task, category); err != nil {
				t.Fatalf("PauseModelRecovery() error = %v", err)
			}
			if store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != "dsh_recovery_unresolved" ||
				len(store.details.Steps) != 1 || !strings.Contains(store.details.Steps[0].Summary, "category: "+category) {
				t.Fatalf("model recovery did not persist safe category %q: task=%#v steps=%#v", category, store.details.Task, store.details.Steps)
			}
		})
	}

	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
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

func TestCloudDSHToolBrokerLadderExhaustionPausesWithTerminalText(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.PauseModelRecoveryAfterLadderExhaustion(context.Background(), store.details.Task, FailoverTimeout, ModelRetryMaxRetries); err != nil {
		t.Fatal(err)
	}
	if len(store.details.Steps) != 1 || !strings.Contains(store.details.Steps[0].Summary, "已重试 3 次") ||
		strings.Contains(store.details.Steps[0].Summary, "timeout") || store.details.Task.Status != task.StatusWaitingUser ||
		store.details.Task.WaitingReason != "model_retry_ladder_exhausted" {
		t.Fatalf("missing ladder-exhausted terminal: task=%+v steps=%+v", store.details.Task, store.details.Steps)
	}
	var terminalEvent struct {
		Detail map[string]any `json:"detail"`
	}
	if err := json.Unmarshal(store.events[0].Payload, &terminalEvent); err != nil || terminalEvent.Detail["ladder_exhausted"] != true ||
		terminalEvent.Detail["retry_attempts"] != float64(ModelRetryMaxRetries) || terminalEvent.Detail["failover_reason"] != string(FailoverTimeout) {
		t.Fatalf("ladder-exhausted event detail = %+v, err=%v", terminalEvent, err)
	}
	// A policy refusal ends the ladder with the §4.3 case-3 text, and an
	// unknown reason cannot end a ladder at all.
	if err := broker.PauseModelRecoveryAfterLadderExhaustion(context.Background(), store.details.Task, FailoverReason("not_a_reason"), 1); err == nil {
		t.Fatal("unknown failover reason was accepted as a ladder terminal")
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

func TestCloudDSHToolBrokerDoesNotPublishStaleCompletionWhenSteerChangesCheckpoint(t *testing.T) {
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
	completion := cloudDSHCompletionFor(claimed, "Stale result from before the steer.", nil)
	pending := []AgentConversationMessage{{ID: "m1234567", Content: "Use the updated requirement.", Sequence: 9, Mode: AgentMessageDeliverySteer}}
	store.mu.Lock()
	store.details.Task.CheckpointThroughSequence = 4
	store.details.Task.CheckpointDigest = bytes.Repeat([]byte{8}, 32)
	store.commitTaskProgressError = NewTaskInputPendingError(pending)
	store.mu.Unlock()

	err = broker.Complete(context.Background(), claimed, completion)
	var pendingError *TaskInputPendingError
	if !errors.As(err, &pendingError) || !errors.Is(err, ErrTaskInputPending) || len(pendingError.PendingMessages) != 1 ||
		pendingError.PendingMessages[0].ID != pending[0].ID {
		t.Fatalf("Complete() checkpoint mismatch error = %v pending=%+v; want typed steer snapshot", err, pendingError)
	}
	if len(store.committedTaskProgress) != 1 {
		t.Fatalf("stale completion progress attempts = %d; want exactly one paused attempt", len(store.committedTaskProgress))
	}
	progress := store.committedTaskProgress[0]
	if progress.Status != task.StatusWaitingUser || progress.Result != "" || progress.AgentCompletionFence != nil ||
		progress.WaitingReason != "output_verification_pending" {
		t.Fatalf("stale completion was not converted to a pending state: %+v", progress)
	}
	if store.details.Task.Status != task.StatusRunning || store.details.Task.Result != "" {
		t.Fatalf("stale completion changed task state: status=%s result=%q", store.details.Task.Status, store.details.Task.Result)
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

func TestCloudDSHCompletionRejectsInvalidPersistedFileIntent(t *testing.T) {
	for _, test := range []struct {
		name      string
		intent    task.FileDeliveryIntent
		objective string
		stale     bool
	}{
		{name: "missing intent", objective: "Answer in chat."},
		{name: "unknown intent", intent: "invalid", objective: "Save a report."},
		{name: "stale completion", intent: "invalid", objective: "Answer in chat.", stale: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			store.details.Task.Objective = test.objective
			store.details.Task.FileDeliveryIntent = test.intent
			setCloudDSHCompletionFence(&store.details.Task)
			broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			claimed := store.details.Task
			completion := cloudDSHCompletionFor(claimed, "Done.", nil)
			if test.stale {
				completion.CheckpointThroughSequence++
			}
			err = broker.Complete(context.Background(), claimed, completion)
			if domain.CodeOf(err) != domain.CodeInvalidState || store.details.Task.Status != task.StatusRunning ||
				store.details.Task.Result != "" || len(store.details.Steps) != 0 {
				t.Fatalf("invalid intent completed or changed task: error=%v task=%+v steps=%+v", err, store.details.Task, store.details.Steps)
			}
		})
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

func TestCloudDSHToolBrokerUsesStructuredFileDeliveryIntent(t *testing.T) {
	for _, test := range []struct {
		name              string
		objective         string
		intent            task.FileDeliveryIntent
		hostless          bool
		reason            string
		wantClarification bool
	}{
		{name: "required despite ordinary wording", objective: "Explain what caused the error.", intent: task.FileDeliveryRequired, reason: "file_delivery_required"},
		{name: "unclear requests clarification", objective: "Write a report about the project.", intent: task.FileDeliveryUnclear, reason: "clarification_required", wantClarification: true},
		{name: "hostless required still waits", objective: "Explain what caused the error.", intent: task.FileDeliveryRequired, hostless: true, reason: "file_delivery_required"},
		{name: "none despite file wording", objective: "Create and save a PDF report.", intent: task.FileDeliveryNone},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeToolExecutorFixture(t, nil)
			store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
			fixture.executor.store = store
			store.details.Task.Objective = test.objective
			store.details.Task.FileDeliveryIntent = test.intent
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
			if test.reason == "" {
				if store.details.Task.Status != task.StatusCompleted || store.details.Task.Result != "Done." {
					t.Fatalf("none intent was overridden by Objective text: task=%#v", store.details.Task)
				}
				return
			}
			if store.details.Task.Status != task.StatusWaitingUser || store.details.Task.WaitingReason != test.reason || store.details.Task.Result != "" {
				t.Fatalf("structured file intent did not pause task: task=%#v steps=%#v", store.details.Task, store.details.Steps)
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
