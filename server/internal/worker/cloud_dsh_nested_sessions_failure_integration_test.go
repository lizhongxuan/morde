package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

// A failing nested worker must be persisted as failed and its actual outcome
// must reach the coordinator and the root along with the successful sibling.
func TestCloudDSHNestedWorkerFailureReachesCoordinator(t *testing.T) {
	for _, scenario := range []struct {
		name, category, summary, alternative string
		failClosed                           bool
	}{
		{name: "confirmed context limit", category: "provider_context_length_exceeded", summary: "CONTEXT_WINDOW_EXCEEDED", alternative: "Worker model context limit was reached."},
		{name: "confirmed provider error", category: "provider_server_error", summary: "MODEL_PROVIDER_ERROR", alternative: "Worker model provider returned an error."},
		{name: "unclassified error remains uncertain", category: "unclassified_test_error", failClosed: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			runNestedWorkerFailureScenario(t, scenario.category, scenario.summary, scenario.alternative, scenario.failClosed)
		})
	}
}

func runNestedWorkerFailureScenario(t *testing.T, category, expectedSummary, alternativeSummary string, failClosed bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, pool := newF01TemporaryStore(t)
	owner := createF01User(t, ctx, store)
	if _, err := pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 5, LoginWindow: time.Minute,
	}, f01DefaultHosts{})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifySession(ctx, createF01Session(t, ctx, store, owner.ID))
	if err != nil || principal.Kind != identity.KindSession {
		t.Fatalf("verify session: %v", err)
	}
	tasks, err := app.NewTaskService(store, credentials, f01ConfiguredModels{})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := tasks.Submit(ctx, principal, app.SubmitTaskCommand{
		ModelID: "p1234567", Objective: "Report one successful and one failed worker.",
		Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 32, ModelTokens: 50000}, IdempotencyKey: "nested-failure-e2e",
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "nested-failure-worker", 2*time.Minute)
	if err != nil || !found || claimed.ID != submitted.Task.ID {
		t.Fatalf("claim Task: found=%t err=%v", found, err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	node, script := f01NodeRuntimePaths(t)
	const supervisorID = "nested-failure-supervisor"
	instances, runtimes, _ := newF01ConversationRuntime(t, store, node, script, supervisorID, bytes.Repeat([]byte{0x72}, 32))
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	children, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	model := &nestedFailureModel{pool: pool, turns: make(map[string]int), category: category}
	executor, err := NewCloudDSHExecutor(CloudDSHExecutorOptions{
		SupervisorID: supervisorID, AccountRuntimes: runtimes, Instances: instances, Checkpoints: checkpoints,
		Models: model, ToolCapabilities: cloudDSHTestToolCapability{allowed: true}, Tools: &nestedE2ETools{},
		Children: children, Completion: &parentSiblingCancelCompletion{store: store},
	})
	if err != nil {
		t.Fatal(err)
	}
	runErr := executor.ExecuteTask(ctx, claimed)
	if failClosed {
		if !errors.Is(runErr, errCloudDSHModelResultUnavailable) {
			t.Fatalf("unclassified failure was treated as certain: %v; calls=%v", runErr, model.snapshot())
		}
		details, err := store.GetTask(ctx, claimed.ID, owner.ID)
		if err != nil || details.Task.Status == task.StatusCompleted {
			t.Fatalf("unclassified failure was reported as completed: %#v err=%v", details.Task, err)
		}
		return
	}
	if runErr != nil {
		t.Fatalf("Task failed before reporting child failure: %v; calls=%v", runErr, model.snapshot())
	}
	details, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusCompleted || details.Task.Result != model.rootResult() {
		t.Fatalf("root did not preserve worker failure: status=%s result=%q err=%v", details.Task.Status, details.Task.Result, err)
	}
	coordinators, err := f07ReadPersistedChildren(ctx, pool, claimed.ID, owner.ID, details.Task.AgentSessionID)
	if err != nil || len(coordinators) != 1 || coordinators[0].Step.Status != task.StepSucceeded ||
		!strings.Contains(coordinators[0].Step.Summary, "alpha-result") || !strings.Contains(coordinators[0].Step.Summary, "beta failed") {
		t.Fatalf("coordinator did not summarize success and failure: %#v err=%v", coordinators, err)
	}
	workers, err := f07ReadPersistedChildren(ctx, pool, claimed.ID, owner.ID, coordinators[0].SessionID)
	if err != nil || len(workers) != 2 {
		t.Fatalf("worker records: %#v err=%v", workers, err)
	}
	statuses := map[string]task.StepStatus{}
	for _, worker := range workers {
		statuses[worker.Step.Title] = worker.Step.Status
	}
	if statuses["failure-alpha"] != task.StepSucceeded || statuses["failure-beta"] != task.StepFailed {
		t.Fatalf("actual worker status was not persisted: %#v", workers)
	}
	for _, worker := range workers {
		if worker.Step.Title == "failure-beta" && worker.Step.Summary != model.failureSummary() {
			t.Fatalf("failed summary was changed: %#v", worker)
		}
	}
	if !model.failureChecked() || !strings.Contains(model.failureSummary(), expectedSummary) && !strings.Contains(model.failureSummary(), alternativeSummary) {
		t.Fatalf("coordinator did not receive the classified worker failure: %q", model.failureSummary())
	}
}

type nestedFailureModel struct {
	pool           *pgxpool.Pool
	mu             sync.Mutex
	turns          map[string]int
	calls          []string
	checkedFailure bool
	summary        string
	category       string
}

func (m *nestedFailureModel) snapshot() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

func (m *nestedFailureModel) failureChecked() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkedFailure
}
func (m *nestedFailureModel) failureSummary() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.summary
}
func (m *nestedFailureModel) rootResult() string {
	return "Alpha completed: alpha-result. Beta failed: " + m.failureSummary()
}

func (m *nestedFailureModel) Complete(ctx context.Context, call app.ModelCall) (_ app.ModelResponse, resultErr error) {
	defer func() {
		if resultErr != nil {
			m.mu.Lock()
			m.calls = append(m.calls, "model error: "+resultErr.Error())
			m.mu.Unlock()
		}
	}()
	if call.Task == nil {
		return app.ModelResponse{}, errors.New("model Task is missing")
	}
	var parent, title string
	if err := m.pool.QueryRow(ctx, `SELECT COALESCE(s.parent_session_id,''),COALESCE(st.title,'') FROM task_agent_sessions s
LEFT JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.session_id=$3`, call.Task.TaskID, call.Task.OwnerID, call.Task.AgentSessionID).Scan(&parent, &title); err != nil {
		return app.ModelResponse{}, err
	}
	m.mu.Lock()
	m.turns[call.Task.AgentSessionID]++
	turn := m.turns[call.Task.AgentSessionID]
	m.calls = append(m.calls, fmt.Sprintf("session=%s parent=%s title=%s turn=%d", call.Task.AgentSessionID, parent, title, turn))
	m.mu.Unlock()
	usage := app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}
	response := func(calls ...app.ModelToolCall) app.ModelResponse {
		return app.ModelResponse{ToolCalls: calls, StopReason: "tool_calls", Usage: usage}
	}
	if parent == "" {
		if turn == 1 {
			return response(app.ModelToolCall{CallID: "failure-coordinator", Name: cloudDSHDelegateToolName,
				Arguments: `{"title":"failure-coordinator","objective":"Report actual outcomes.","tool_names":["nested_allowed_read"]}`}), nil
		}
		output, ok := f01ToolResult(call.RawRequest, "failure-coordinator")
		if turn != 2 || !ok || !strings.Contains(output, "alpha-result") || !strings.Contains(output, "beta failed") {
			return app.ModelResponse{}, fmt.Errorf("root received invalid coordinator outcome: %q turn=%d", output, turn)
		}
		return app.ModelResponse{Text: m.rootResult(), StopReason: "stop", Usage: usage}, nil
	}
	if parent == call.Task.AgentRootSessionID {
		switch turn {
		case 1:
			return response(
				app.ModelToolCall{CallID: "failure-alpha", Name: cloudDSHDelegateToolName, Arguments: `{"title":"failure-alpha","objective":"Return alpha-result.","tool_names":["nested_allowed_read"],"run_in_background":true}`},
				app.ModelToolCall{CallID: "failure-beta", Name: cloudDSHDelegateToolName, Arguments: `{"title":"failure-beta","objective":"Fail the model call.","tool_names":["nested_allowed_read"],"run_in_background":true}`},
			), nil
		case 2:
			alpha, err := nestedBackgroundHandle(call.RawRequest, "failure-alpha")
			if err != nil {
				return app.ModelResponse{}, err
			}
			beta, err := nestedBackgroundHandle(call.RawRequest, "failure-beta")
			if err != nil {
				return app.ModelResponse{}, err
			}
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				var count int
				if err := m.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id WHERE s.task_id=$1 AND s.owner_id=$2 AND s.parent_session_id=$3 AND st.status IN ('succeeded','failed')`, call.Task.TaskID, call.Task.OwnerID, call.Task.AgentSessionID).Scan(&count); err != nil {
					return app.ModelResponse{}, err
				}
				if count == 2 {
					break
				}
				select {
				case <-ticker.C:
				case <-ctx.Done():
					return app.ModelResponse{}, ctx.Err()
				}
			}
			return response(
				app.ModelToolCall{CallID: "failure-alpha-output", Name: cloudDSHJobOutputToolName, Arguments: fmt.Sprintf(`{"job_id":%q,"after_seq":-1}`, alpha)},
				app.ModelToolCall{CallID: "failure-beta-output", Name: cloudDSHJobOutputToolName, Arguments: fmt.Sprintf(`{"job_id":%q,"after_seq":-1}`, beta)},
				app.ModelToolCall{CallID: "failure-beta-get", Name: cloudDSHJobGetToolName, Arguments: fmt.Sprintf(`{"job_id":%q}`, beta)},
			), nil
		case 3:
			alpha, okA := f01ToolResult(call.RawRequest, "failure-alpha-output")
			beta, okB := f01ToolResult(call.RawRequest, "failure-beta-output")
			betaDetails, okDetails := f01ToolResult(call.RawRequest, "failure-beta-get")
			var a, b struct {
				Status string `json:"status"`
				Output string `json:"output"`
			}
			var details struct {
				Status  string `json:"status"`
				Summary string `json:"summary"`
			}
			if !okA || !okB || !okDetails || json.Unmarshal([]byte(alpha), &a) != nil || json.Unmarshal([]byte(beta), &b) != nil || json.Unmarshal([]byte(betaDetails), &details) != nil ||
				a.Status != "succeeded" || a.Output != "alpha-result" || b.Status != "failed" || details.Status != "failed" || details.Summary == "" {
				return app.ModelResponse{}, fmt.Errorf("coordinator received invalid worker outcomes: alpha=%q beta=%q details=%q", alpha, beta, betaDetails)
			}
			m.mu.Lock()
			m.checkedFailure = true
			m.summary = details.Summary
			m.mu.Unlock()
			return app.ModelResponse{Text: "alpha-result; beta failed: " + details.Summary, StopReason: "stop", Usage: usage}, nil
		case 4:
			handoff, err := nestedModelHandoff(call.RawRequest)
			_, body, found := strings.Cut(handoff, "\n")
			var jobs []struct {
				Status string `json:"status"`
				Output string `json:"output"`
			}
			if err != nil || !found || json.Unmarshal([]byte(body), &jobs) != nil || len(jobs) != 2 {
				return app.ModelResponse{}, fmt.Errorf("coordinator review contained invalid handoff: %q err=%v", handoff, err)
			}
			seenSuccess, seenFailure := false, false
			for _, job := range jobs {
				seenSuccess = seenSuccess || job.Status == "succeeded" && job.Output == "alpha-result"
				seenFailure = seenFailure || job.Status == "failed" && job.Output == m.failureSummary()
			}
			if !seenSuccess || !seenFailure {
				return app.ModelResponse{}, fmt.Errorf("coordinator review omitted actual worker outcomes: %q", handoff)
			}
			return app.ModelResponse{Text: "alpha-result; beta failed: " + m.failureSummary(), StopReason: "stop", Usage: usage}, nil
		}
		return app.ModelResponse{}, fmt.Errorf("unexpected coordinator turn %d", turn)
	}
	if turn != 1 {
		return app.ModelResponse{}, fmt.Errorf("unexpected worker turn %d", turn)
	}
	if title == "failure-alpha" {
		return app.ModelResponse{Text: "alpha-result", StopReason: "stop", Usage: usage}, nil
	}
	if title == "failure-beta" {
		return app.ModelResponse{}, nestedFailureError{category: m.category}
	}
	return app.ModelResponse{}, fmt.Errorf("unexpected worker title %q", title)
}

// The fake provider distinguishes classified failures from unknown errors.
type nestedFailureError struct{ category string }

func (e nestedFailureError) Error() string                { return "synthetic model provider failure" }
func (e nestedFailureError) ModelFailureCategory() string { return e.category }
