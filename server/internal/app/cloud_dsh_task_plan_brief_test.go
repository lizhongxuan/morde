package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func newBriefPlanBroker(t *testing.T) (*cloudDSHBrokerTaskStore, *CloudDSHToolBroker, task.Task) {
	t.Helper()
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	current := store.details.Task
	current.ExecutionLocation = "server"
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-brief", 1, "session-brief"
	current.Authorization.TokenID = "t1234567"
	current.Authorization.Version = 1
	current.Authorization.Scopes = []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}
	expires := time.Now().Add(time.Hour).UTC()
	current.Authorization.ExpiresAt = &expires
	store.details.Task = current
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store, broker, current
}

func briefPlanCall(current task.Task, callID, arguments string) ModelToolCall {
	return ModelToolCall{CallID: callID, Name: cloudDSHTaskPlanToolName, Arguments: arguments,
		TaskFence: &ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
			LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, AgentInstanceID: current.AgentInstanceID,
			AgentGeneration: current.AgentGeneration, AgentSessionID: current.AgentSessionID,
			AuthorizationVersion: current.Authorization.Version}}
}

func (s *cloudDSHBrokerTaskStore) GetCloudDSHTaskPlanReceipt(_ context.Context, _, _, sessionID, callID string) (task.Plan, error) {
	for _, progress := range s.committedTaskProgress {
		if commit := progress.CloudDSHTaskPlan; commit != nil && commit.AgentSessionID == sessionID && commit.CallID == callID {
			return commit.Plan, nil
		}
	}
	return task.Plan{}, domain.NewError(domain.CodeNotFound, "task plan receipt not found")
}

func TestTaskPlanSubmissionRequiresCompleteBrief(t *testing.T) {
	store, broker, current := newBriefPlanBroker(t)
	steps := `[{"id":"repro","title":"Reproduce","status":"pending","completion_criteria":["crash reproduces"]}]`
	missingScope := `{"goal":"Fix the login crash","steps":` + steps + `}`

	if result, err := broker.Execute(context.Background(), current, briefPlanCall(current, "plan-brief-1", missingScope)); err != nil ||
		result.Status != AgentToolFailed || !strings.Contains(result.Output, "scope") {
		t.Fatalf("plan without scope was not returned to the Agent: result=%#v, err=%v", result, err)
	}
	for _, progress := range store.committedTaskProgress {
		if progress.CloudDSHTaskPlan != nil {
			t.Fatal("rejected plan was persisted")
		}
	}

	complete := `{"goal":"Fix the login crash","scope":"auth package only","verification_method":"run the failing test","steps":` + steps + `}`
	result, err := broker.Execute(context.Background(), current, briefPlanCall(current, "plan-brief-2", complete))
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("plan with a complete brief was rejected: result=%#v, err=%v", result, err)
	}
	var submitted task.Plan
	if err := json.Unmarshal([]byte(result.Output), &submitted); err != nil || submitted.Revision != 1 ||
		submitted.Scope != "auth package only" || submitted.VerificationMethod != "run the failing test" {
		t.Fatalf("submitted plan output = %s, err=%v", result.Output, err)
	}
	committed := false
	for _, progress := range store.committedTaskProgress {
		if progress.CloudDSHTaskPlan != nil {
			committed = true
		}
	}
	if !committed {
		t.Fatal("accepted plan was not persisted")
	}

	// The fake store does not replay the stored plan into GetTask, so record
	// the committed revision the way the PostgreSQL store would return it.
	store.details.Plan = &submitted
	current = store.details.Task
	if result, err := broker.Execute(context.Background(), current, briefPlanCall(current, "plan-brief-3", missingScope)); err != nil ||
		result.Status != AgentToolFailed || !strings.Contains(result.Output, "scope") {
		t.Fatalf("revised plan without scope was not returned to the Agent: result=%#v, err=%v", result, err)
	}
	revised := `{"goal":"Fix the login crash","scope":"auth package only","verification_method":"run the failing test and the auth suite","steps":` + steps + `}`
	result, err = broker.Execute(context.Background(), current, briefPlanCall(current, "plan-brief-4", revised))
	if err != nil || result.Status != AgentToolSucceeded {
		t.Fatalf("revised plan with a complete brief was rejected: result=%#v, err=%v", result, err)
	}
	var resubmitted task.Plan
	if err := json.Unmarshal([]byte(result.Output), &resubmitted); err != nil || resubmitted.Revision != 2 {
		t.Fatalf("revised plan output = %s, err=%v; want revision 2", result.Output, err)
	}
}

func TestAdvisoryPlanRejectionReplaysWithoutExecuting(t *testing.T) {
	store, broker, current := newBriefPlanBroker(t)
	call := briefPlanCall(current, "plan-brief-replay", `{"goal":"Fix the bug","steps":[{"id":"a","title":"Reproduce","status":"pending"}]}`)
	first, err := broker.Execute(context.Background(), current, call)
	if err != nil || first.Status != AgentToolFailed || len(store.details.Steps) != 1 || store.details.Steps[0].Retryable {
		t.Fatalf("first rejection = %#v, %v; steps=%#v", first, err, store.details.Steps)
	}
	checkpoint := dshToolResultText(first)
	usage := store.details.Task.Usage.ToolCalls
	store.details.Plan = briefPlan(1, true)
	replayed, err := broker.Reconcile(context.Background(), store.details.Task, call, &checkpoint)
	if err != nil || replayed.Status != first.Status || replayed.Output != first.Output || store.details.Task.Usage.ToolCalls != usage || len(store.details.Steps) != 1 {
		t.Fatalf("replayed rejection = %#v, %v; first=%#v usage=%d", replayed, err, first, store.details.Task.Usage.ToolCalls)
	}
	changed := "Tool failed: a different result"
	if _, err := broker.Reconcile(context.Background(), store.details.Task, call, &changed); err == nil {
		t.Fatal("changed checkpoint was accepted")
	}
}

func TestAdvisoryWorkspaceWriteRejectionDoesNotPublish(t *testing.T) {
	_, store, broker, workspaceStore, current := newCloudDSHT08WorkspaceBroker(t)
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-brief", 1, "session-brief"
	expires := time.Now().Add(time.Hour).UTC()
	current.Authorization = task.Authorization{Kind: string(identity.KindSession), TokenID: "t1234567", Version: 1, ExpiresAt: &expires,
		Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}}
	store.details.Task, store.details.Plan = current, briefPlan(1, false)
	call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "workspace-brief-replay", cloudDSHWorkspaceWriteArguments{
		ExpectedRevision: 0, Path: "reports/summary.txt", Content: "synthetic",
	})
	call.TaskFence = briefPlanCall(current, call.CallID, call.Arguments).TaskFence
	first, err := broker.Execute(context.Background(), current, call)
	if err != nil || first.Status != AgentToolFailed || workspaceStore.commits != 0 || store.details.PendingRequest != nil {
		t.Fatalf("Workspace rejection = %#v, %v; commits=%d", first, err, workspaceStore.commits)
	}
	checkpoint := dshToolResultText(first)
	usage := store.details.Task.Usage.ToolCalls
	store.details.Plan = briefPlan(1, true)
	replayed, err := broker.Reconcile(context.Background(), store.details.Task, call, &checkpoint)
	if err != nil || replayed.Status != first.Status || replayed.Output != first.Output || store.details.Task.Usage.ToolCalls != usage || workspaceStore.commits != 0 {
		t.Fatalf("Workspace rejection replay = %#v, %v; first=%#v commits=%d", replayed, err, first, workspaceStore.commits)
	}
}

func TestAdvisoryHostWriteRejectsRevokedAuthorization(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	current := store.details.Task
	current.ExecutionLocation = "server"
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-brief", 1, "session-brief"
	store.details.Task, store.details.Plan = current, briefPlan(1, false)
	fixture.authorizer.errors[identity.ScopeComputerOperate] = domain.NewError(domain.CodeForbidden, "operate scope revoked")
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := minimalDiffHostWriteCall(current, "revoked-advisory-write", "auth/session.go")
	call = setCloudDSHPlanStepMetadataForTest(t, call, "repro", &store.details.Plan.Revision)
	if result, err := broker.Execute(context.Background(), current, call); domain.CodeOf(err) != domain.CodeForbidden || result.Status != "" || len(store.details.Steps) != 0 {
		t.Fatalf("revoked authorization returned a retryable gate result: result=%#v err=%v", result, err)
	}
}

func briefPlan(revision int64, complete bool) *task.Plan {
	plan := &task.Plan{TaskRevision: 0, Revision: revision, Goal: "Fix the login crash", CostEstimateState: "unknown",
		Steps: []task.PlanStep{{ID: "repro", Title: "Reproduce", Status: task.PlanStepPending,
			CompletionCriteria: []string{"crash reproduces"}}}}
	if complete {
		plan.Scope = "auth package only"
		plan.VerificationMethod = "run the failing test"
	}
	return plan
}

func TestWorkspaceWriteGateRequiresCompleteBrief(t *testing.T) {
	for _, test := range []struct {
		name    string
		plan    *task.Plan
		wantErr bool
	}{
		{name: "no plan is unaffected", plan: nil},
		{name: "incomplete brief is rejected", plan: briefPlan(1, false), wantErr: true},
		{name: "complete brief passes", plan: briefPlan(1, true)},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, store, broker, _, current := newCloudDSHT08WorkspaceBroker(t)
			store.details.Plan = test.plan
			call := workspaceToolCall(t, cloudDSHWorkspaceWriteToolName, "write-brief-1", cloudDSHWorkspaceWriteArguments{
				ExpectedRevision: 0, Path: "reports/summary.txt", Content: "draft saved only after confirmation",
			})
			result, err := broker.Execute(context.Background(), current, call)
			if test.wantErr {
				if err == nil || domain.CodeOf(err) != domain.CodeInvalidArgument || !strings.Contains(err.Error(), "brief") {
					t.Fatalf("write with an incomplete brief was not rejected: result=%#v, err=%v", result, err)
				}
				if store.details.PendingRequest != nil {
					t.Fatal("rejected write created an approval request")
				}
				return
			}
			if err != nil || result.Status != AgentToolNeedsUser || store.details.PendingRequest == nil {
				t.Fatalf("write was blocked without an incomplete brief: result=%#v, err=%v pending=%#v", result, err, store.details.PendingRequest)
			}
		})
	}
}

func TestHostFileWriteGateRequiresCompleteBrief(t *testing.T) {
	fixture := newRuntimeToolExecutorFixture(t, nil)
	store := &cloudDSHBrokerTaskStore{runtimeToolTaskStore: fixture.store}
	fixture.executor.store = store
	current := store.details.Task
	current.ExecutionLocation = "server"
	current.AgentInstanceID, current.AgentGeneration, current.AgentSessionID = "agent-brief", 1, "session-brief"
	store.details.Task = current
	store.details.Plan = briefPlan(1, false)
	broker, err := NewCloudDSHToolBroker(store, fixture.executor, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	call := ModelToolCall{CallID: "file-write-brief-1", Name: runtimeToolFileWrite,
		Arguments: `{"workspace_id":"w1234567","path":"reports/result.txt","content":"synthetic"}`,
		TaskFence: &ModelTaskFence{TaskID: current.ID, OwnerID: current.OwnerID, InitiatorID: current.InitiatorID,
			LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version, AgentInstanceID: current.AgentInstanceID,
			AgentGeneration: current.AgentGeneration, AgentSessionID: current.AgentSessionID,
			AuthorizationVersion: current.Authorization.Version}}
	call = setCloudDSHPlanStepMetadataForTest(t, call, "repro", &store.details.Plan.Revision)
	if result, err := broker.Execute(context.Background(), current, call); err != nil ||
		result.Status != AgentToolFailed || !strings.Contains(result.Output, "brief") {
		t.Fatalf("host file write rejection was not returned to the Agent: result=%#v, err=%v", result, err)
	}
}
