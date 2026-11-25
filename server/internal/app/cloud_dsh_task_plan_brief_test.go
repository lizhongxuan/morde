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

func TestTaskPlanSubmissionRequiresCompleteBrief(t *testing.T) {
	store, broker, current := newBriefPlanBroker(t)
	steps := `[{"id":"repro","title":"Reproduce","status":"pending","completion_criteria":["crash reproduces"]}]`
	missingScope := `{"goal":"Fix the login crash","steps":` + steps + `}`

	if _, err := broker.Execute(context.Background(), current, briefPlanCall(current, "plan-brief-1", missingScope)); err == nil ||
		domain.CodeOf(err) != domain.CodeInvalidArgument || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("plan without scope was not rejected with the missing field: err=%v", err)
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
	if _, err := broker.Execute(context.Background(), current, briefPlanCall(current, "plan-brief-3", missingScope)); err == nil ||
		domain.CodeOf(err) != domain.CodeInvalidArgument || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("revised plan without scope was not rejected: err=%v", err)
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
	if _, err := broker.Execute(context.Background(), current, call); err == nil ||
		domain.CodeOf(err) != domain.CodeInvalidArgument || !strings.Contains(err.Error(), "brief") {
		t.Fatalf("host file write with an incomplete brief was not rejected: err=%v", err)
	}
}
