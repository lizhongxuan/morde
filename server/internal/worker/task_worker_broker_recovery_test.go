package worker

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

// t55BrokerTaskStore is a deliberately small Task ledger for proving that the
// real CloudDSHToolBroker is called by the real Worker executor. Unexpected
// TaskStore methods remain nil through the embedded interface.
type t55BrokerTaskStore struct {
	app.TaskStore
	mu                    sync.Mutex
	details               app.TaskDetails
	progress              []app.TaskProgress
	commitErrorAfterApply error
}

func (s *t55BrokerTaskStore) GetTask(_ context.Context, id, ownerID string) (app.TaskDetails, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.details.Task.ID || ownerID != s.details.Task.OwnerID {
		return app.TaskDetails{}, domain.NewError(domain.CodeNotFound, "Task not found")
	}
	return cloneT55TaskDetails(s.details), nil
}

func (s *t55BrokerTaskStore) GetCloudDSHTaskPlanReceipt(_ context.Context, taskID, ownerID, sessionID, callID string) (task.Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if taskID != s.details.Task.ID || ownerID != s.details.Task.OwnerID || sessionID == "" || callID == "" {
		return task.Plan{}, domain.NewError(domain.CodeNotFound, "task plan receipt not found")
	}
	for _, progress := range s.progress {
		if commit := progress.CloudDSHTaskPlan; commit != nil && commit.AgentSessionID == sessionID && commit.CallID == callID {
			return commit.Plan, nil
		}
	}
	return task.Plan{}, domain.NewError(domain.CodeNotFound, "task plan receipt not found")
}

func (s *t55BrokerTaskStore) CommitTaskProgress(_ context.Context, progress app.TaskProgress) (task.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if progress.TaskID != s.details.Task.ID || progress.LeaseOwner != s.details.Task.Lease.Owner ||
		progress.LeaseVersion != s.details.Task.Lease.Version {
		return task.Event{}, domain.NewError(domain.CodeInvalidState, "Task lease changed")
	}
	s.details.Task.Status = progress.Status
	s.details.Task.CurrentStep = progress.CurrentStep
	s.details.Task.Result = progress.Result
	s.details.Task.Usage = progress.Usage
	s.details.Task.WaitingReason = progress.WaitingReason
	for _, step := range progress.Steps {
		replaced := false
		for index := range s.details.Steps {
			if s.details.Steps[index].ID == step.ID {
				s.details.Steps[index] = step
				replaced = true
				break
			}
		}
		if !replaced {
			s.details.Steps = append(s.details.Steps, step)
		}
	}
	if progress.CloudDSHTaskPlan != nil {
		plan := progress.CloudDSHTaskPlan.Plan
		s.details.Plan = &plan
	}
	s.progress = append(s.progress, progress)
	if s.commitErrorAfterApply != nil {
		err := s.commitErrorAfterApply
		s.commitErrorAfterApply = nil
		return task.Event{}, err
	}
	return progress.Event, nil
}

func (s *t55BrokerTaskStore) AdmitAgentToolCall(_ context.Context, call app.ModelToolCall, _ bool) (app.AgentToolCallAdmission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fence := call.TaskFence
	if fence == nil || fence.TaskID != s.details.Task.ID || fence.OwnerID != s.details.Task.OwnerID ||
		fence.AgentSessionID == "" || fence.AgentSessionID != s.details.Task.AgentSessionID ||
		fence.AuthorizationVersion != s.details.Task.Authorization.Version {
		return app.AgentToolCallAdmission{}, domain.NewError(domain.CodeInvalidState, "tool admission fence mismatch")
	}
	return app.AgentToolCallAdmission{Status: app.AgentToolCallAdmitted}, nil
}

func (s *t55BrokerTaskStore) syncClaimedFence(claimed task.Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if claimed.ID != s.details.Task.ID || claimed.OwnerID != s.details.Task.OwnerID {
		return
	}
	s.details.Task.AgentInstanceID = claimed.AgentInstanceID
	s.details.Task.AgentGeneration = claimed.AgentGeneration
	s.details.Task.AgentSessionID = claimed.AgentSessionID
	s.details.Task.Lease = claimed.Lease
	s.details.Task.Authorization = claimed.Authorization
	s.details.Task.CurrentRevision = cloneT55Int64(claimed.CurrentRevision)
}

func (s *t55BrokerTaskStore) snapshot() (app.TaskDetails, []app.TaskProgress) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneT55TaskDetails(s.details), append([]app.TaskProgress(nil), s.progress...)
}

func cloneT55TaskDetails(value app.TaskDetails) app.TaskDetails {
	value.Steps = append([]task.Step(nil), value.Steps...)
	if value.Plan != nil {
		plan := *value.Plan
		value.Plan = &plan
	}
	value.Task.CurrentRevision = cloneT55Int64(value.Task.CurrentRevision)
	if value.Task.Lease.ExpiresAt != nil {
		expires := *value.Task.Lease.ExpiresAt
		value.Task.Lease.ExpiresAt = &expires
	}
	if value.Task.Authorization.ExpiresAt != nil {
		expires := *value.Task.Authorization.ExpiresAt
		value.Task.Authorization.ExpiresAt = &expires
	}
	return value
}

func cloneT55Int64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

type t55CloudDSHToolBroker struct {
	*app.CloudDSHToolBroker
	store *t55BrokerTaskStore
	calls []app.ModelToolCall
}

func (b *t55CloudDSHToolBroker) Definitions(ctx context.Context, claimed task.Task) ([]app.AgentToolDefinition, error) {
	b.store.syncClaimedFence(claimed)
	return b.CloudDSHToolBroker.Definitions(ctx, claimed)
}

func (b *t55CloudDSHToolBroker) Execute(ctx context.Context, claimed task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	b.store.syncClaimedFence(claimed)
	b.calls = append(b.calls, call)
	return b.CloudDSHToolBroker.Execute(ctx, claimed, call)
}

func (b *t55CloudDSHToolBroker) Reconcile(ctx context.Context, claimed task.Task, call app.ModelToolCall,
	checkpointOutput *string) (app.AgentToolResult, error) {
	b.store.syncClaimedFence(claimed)
	return b.CloudDSHToolBroker.Reconcile(ctx, claimed, call, checkpointOutput)
}

func (b *t55CloudDSHToolBroker) PauseRecovery(ctx context.Context, claimed task.Task) error {
	b.store.syncClaimedFence(claimed)
	return b.CloudDSHToolBroker.PauseRecovery(ctx, claimed)
}

func TestCloudDSHWorkerJoinsBrokerPreDispatchRepairInSameSession(t *testing.T) {
	executor, supervisor, instances, checkpoints, models, value := cloudDSHFixture(t)
	broker, store, value := t55NewBroker(t, value, nil)
	executor.options.Tools = broker
	completer := &cloudDSHTestCompleter{instances: instances}
	executor.options.Completion = completer
	models.responses = []app.ModelResponse{
		{ToolCalls: []app.ModelToolCall{{CallID: "call_incomplete_plan_1", Name: "morde_task_plan_update", Arguments: t55IncompletePlanArguments()}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2}},
		{ToolCalls: []app.ModelToolCall{{CallID: "call_corrected_plan_1", Name: "morde_task_plan_update", Arguments: t55ValidPlanArguments()}},
			StopReason: "tool_calls", Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}},
		{Text: "我补全了计划，并保留了第一次提交缺少范围与验证方式的事实。", StopReason: "stop",
			Usage: app.ModelUsage{Known: true, InputTokens: 10, OutputTokens: 12}},
	}

	if err := executor.ExecuteTask(context.Background(), value); err != nil {
		t.Fatalf("ExecuteTask() error = %v", err)
	}
	details, progress := store.snapshot()
	if len(models.calls) != 3 || len(broker.calls) != 2 || completer.calls != 1 ||
		len(progress) != 2 || len(checkpoints.commands) == 0 {
		t.Fatalf("models=%d broker calls=%d completions=%d progress=%d checkpoints=%d; want 3/2/1/2/>0",
			len(models.calls), len(broker.calls), completer.calls, len(progress), len(checkpoints.commands))
	}
	for index, modelCall := range models.calls {
		if modelCall.Task == nil || supervisor.launch.Fence.AgentSessionID == "" ||
			modelCall.Task.AgentSessionID != supervisor.launch.Fence.AgentSessionID {
			t.Fatalf("model turn %d left DSH session %q: %+v", index+1, supervisor.launch.Fence.AgentSessionID, modelCall.Task)
		}
	}
	if broker.calls[0].CallID != "call_incomplete_plan_1" || broker.calls[0].Name != "morde_task_plan_update" ||
		broker.calls[1].CallID != "call_corrected_plan_1" || broker.calls[1].Name != "morde_task_plan_update" {
		t.Fatalf("actual Broker calls = %#v", broker.calls)
	}
	if len(details.Steps) != 2 || details.Steps[0].Status != task.StepFailed ||
		details.Steps[0].Phase != "prepare" || details.Steps[0].Retryable ||
		details.Steps[0].AgentSessionID != supervisor.launch.Fence.AgentSessionID ||
		details.Steps[1].Status != task.StepSucceeded || details.Plan == nil ||
		details.Plan.Goal != "Continue safely after an incomplete plan" {
		t.Fatalf("Broker rejection/correction was not retained: steps=%+v plan=%+v", details.Steps, details.Plan)
	}
	assertT55ToolResult(t, models.calls[1].RawRequest, "call_incomplete_plan_1",
		"Tool failed: The task plan needs a complete brief: fill in the scope, verification method and step acceptance criteria from context, then resubmit.")
	assertT55ToolResult(t, models.calls[2].RawRequest, "call_incomplete_plan_1",
		"Tool failed: The task plan needs a complete brief: fill in the scope, verification method and step acceptance criteria from context, then resubmit.")
	planOutput, err := json.Marshal(*details.Plan)
	if err != nil {
		t.Fatal(err)
	}
	assertT55ToolResult(t, models.calls[2].RawRequest, "call_corrected_plan_1", string(planOutput))
}

func TestCloudDSHWorkerDoesNotRetryBrokerResultAfterAmbiguousProgressCommit(t *testing.T) {
	executor, _, _, _, models, value := cloudDSHFixture(t)
	broker, store, value := t55NewBroker(t, value, domain.NewError(domain.CodeInternal, "progress commit response was lost"))
	executor.options.Tools = broker
	models.responses = []app.ModelResponse{{ToolCalls: []app.ModelToolCall{{CallID: "call_ambiguous_plan_1",
		Name: "morde_task_plan_update", Arguments: t55IncompletePlanArguments()}}, StopReason: "tool_calls",
		Usage: app.ModelUsage{Known: true, InputTokens: 4, OutputTokens: 2}}}

	err := executor.ExecuteTask(context.Background(), value)
	details, progress := store.snapshot()
	if err == nil || len(models.calls) != 1 || len(broker.calls) != 1 || details.Plan != nil || len(progress) > 2 {
		t.Fatalf("ambiguous Broker result was retried: err=%v models=%d broker calls=%d plan=%+v steps=%+v progress=%d",
			err, len(models.calls), len(broker.calls), details.Plan, details.Steps, len(progress))
	}
	if len(details.Steps) == 0 || details.Steps[0].ToolCallKey != "call_ambiguous_plan_1" ||
		details.Steps[0].Status != task.StepFailed {
		t.Fatalf("original ambiguous failure is not retained: %+v", details.Steps)
	}
}

func t55NewBroker(t *testing.T, initial task.Task, commitError error) (*t55CloudDSHToolBroker, *t55BrokerTaskStore, task.Task) {
	t.Helper()
	initial = t55AuthorizedTask(initial)
	store := &t55BrokerTaskStore{details: app.TaskDetails{Task: initial}, commitErrorAfterApply: commitError}
	broker, err := app.NewCloudDSHToolBroker(store, &app.RuntimeToolExecutor{}, nil, nil)
	if err != nil {
		t.Fatalf("NewCloudDSHToolBroker() error = %v", err)
	}
	return &t55CloudDSHToolBroker{CloudDSHToolBroker: broker, store: store}, store, initial
}

func t55AuthorizedTask(initial task.Task) task.Task {
	revision := int64(1)
	if initial.CurrentRevision == nil {
		initial.CurrentRevision = &revision
	}
	expires := time.Now().Add(10 * time.Minute)
	initial.ExecutionLocation = "server"
	initial.Authorization = task.Authorization{Kind: string(identity.KindSession), TokenID: "t1234567", Version: 1,
		ExpiresAt: &expires, Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}}
	initial.FileDeliveryIntent = task.FileDeliveryNone
	initial.Budget.ToolCalls = 3
	return initial
}

func t55IncompletePlanArguments() string {
	return `{"goal":"Continue safely after an incomplete plan","steps":[{"id":"record-next-step","title":"Record the safe next step","description":"Preserve the rejected plan and summarize the permitted follow-up.","completion_criteria":["The previous rejection remains recorded","The next step is explicit"],"status":"pending"}]}`
}

func t55ValidPlanArguments() string {
	return `{"goal":"Continue safely after an incomplete plan","scope":"Record a safe next step without touching external resources.","verification_method":"Inspect the persisted plan and failed plan step.","steps":[{"id":"record-next-step","title":"Record the safe next step","description":"Preserve the rejected plan and summarize the permitted follow-up.","completion_criteria":["The previous rejection remains recorded","The next step is explicit"],"status":"pending"}],"requirement_manifest":{"version":1,"obligations":[]}}`
}

func assertT55ToolResult(t *testing.T, rawRequest json.RawMessage, callID, want string) {
	t.Helper()
	var request struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"toolCallId"`
			Content    []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rawRequest, &request); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, message := range request.Messages {
		if message.Role == "tool" && message.ToolCallID == callID &&
			len(message.Content) == 1 && message.Content[0].Text == want {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("model request has %d exact tool results for %s; want one %q: %s", found, callID, want, rawRequest)
	}
}
