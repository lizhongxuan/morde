package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func TestTaskStepDetailHTTPReadsAuthorizedEncryptedPostgresRecord(t *testing.T) {
	fixture := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := fixture.service.Start(ctx, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	current, err := fixture.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || current.Task.CurrentRevision == nil {
		t.Fatalf("load task revision: task=%+v err=%v", current.Task, err)
	}
	stepID, err := domain.NewStepID()
	if err != nil {
		t.Fatal(err)
	}
	step := task.Step{ID: stepID, TaskID: current.Task.ID, Sequence: 1, Kind: "tool", Status: task.StepSucceeded,
		Title: "Search the web", Summary: "Search completed.", CreatedAt: time.Now().UTC()}
	document := app.TaskStepDetailDocument{Object: "morde_web_search", Input: "Search query: AI news (time range: week).", Result: "2 public sources found."}
	plain, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := sealer.SealWithAAD(plain, aadForTaskStepDetail(current.Task.OwnerID, current.Task.ID, stepID, *current.Task.CurrentRevision))
	if err != nil {
		t.Fatal(err)
	}
	detailDigest := sha256.Sum256(plain)
	requestDigest := sha256.Sum256([]byte("morde_web_search:{\"query\":\"AI news\",\"time_range\":\"week\"}"))
	usage := current.Task.Usage
	usage.ToolCalls++
	payload, err := json.Marshal(map[string]any{"step_id": step.ID, "sequence": step.Sequence, "kind": step.Kind,
		"status": step.Status, "title": step.Title, "summary": step.Summary})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		Status: task.StatusRunning, CurrentStep: step.ID, Usage: usage, Steps: []task.Step{step},
		Event: task.Event{TaskID: current.Task.ID, Type: "task.step", Payload: payload},
		ToolCall: &app.TaskToolCallProgress{AgentSessionID: lease.Fence.AgentSessionID, AgentInstanceID: lease.Fence.AgentInstanceID,
			AgentGeneration: lease.Fence.AgentGeneration, AuthorizationVersion: lease.Fence.AuthorizationVersion,
			TaskRevision: *current.Task.CurrentRevision, CallKey: "http-search-call", ToolName: "morde_web_search",
			RequestDigest: requestDigest[:], Phase: string(task.StepSucceeded), DetailState: "available",
			DetailCiphertext: ciphertext, DetailSHA256: detailDigest[:], DetailSizeBytes: int32(len(plain))},
	}); err != nil {
		t.Fatalf("persist task activity: %v", err)
	}

	tasks, err := app.NewTaskService(fixture.store, k4FixtureAuthorization{}, k4FixtureModelCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	router, err := api.New(api.Deps{
		Hosts: k4FixtureHostService{}, TLSAuthorizer: k4FixtureHostService{},
		Verifier: k4FixtureVerifier{ownerID: current.Task.OwnerID}, Auth: k4FixtureAuth{}, Tasks: tasks,
	}, api.Options{Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 1 << 20,
		MCPMaxBodyBytes: 1 << 20, SessionTTL: time.Hour, HostScheme: "http", PortalDomain: "morde.test",
		HostSuffix: "hosts.invalid", PreviewPortMin: 1024})
	if err != nil {
		t.Fatalf("create task detail API: %v", err)
	}
	credential, _, err := identity.NewCredential(identity.KindSession, testSessionTokenID(current.Task.OwnerID))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/tasks/"+current.Task.ID+"/steps/"+stepID, nil)
	request.AddCookie(&http.Cookie{Name: "ch_portal", Value: credential})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("Pragma") != "no-cache" || response.Header().Get("Referrer-Policy") != "no-referrer" ||
		!json.Valid(response.Body.Bytes()) {
		t.Fatalf("task detail response=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var got app.TaskStepDetail
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil || got.TaskID != current.Task.ID || got.StepID != stepID ||
		got.DetailState != "available" || got.Object == nil || *got.Object != document.Object ||
		got.Input == nil || *got.Input != document.Input || got.Result == nil || *got.Result != document.Result || got.EventSequence < 1 {
		t.Fatalf("authorized HTTP task detail=%+v err=%v body=%s", got, err, response.Body.String())
	}

	stranger := createPreRetirementTestUser(t, fixture.store, 8, 20_000, 40_000)
	foreignRouter, err := api.New(api.Deps{
		Hosts: k4FixtureHostService{}, TLSAuthorizer: k4FixtureHostService{},
		Verifier: k4FixtureVerifier{ownerID: stranger.ID}, Auth: k4FixtureAuth{}, Tasks: tasks,
	}, api.Options{Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 1 << 20,
		MCPMaxBodyBytes: 1 << 20, SessionTTL: time.Hour, HostScheme: "http", PortalDomain: "morde.test",
		HostSuffix: "hosts.invalid", PreviewPortMin: 1024})
	if err != nil {
		t.Fatalf("create foreign-principal task detail API: %v", err)
	}
	foreignCredential, _, err := identity.NewCredential(identity.KindSession, testSessionTokenID(stranger.ID))
	if err != nil {
		t.Fatal(err)
	}
	foreignRequest := httptest.NewRequest(http.MethodGet, "/api/tasks/"+current.Task.ID+"/steps/"+stepID, nil)
	foreignRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: foreignCredential})
	foreignResponse := httptest.NewRecorder()
	foreignRouter.ServeHTTP(foreignResponse, foreignRequest)
	if foreignResponse.Code != http.StatusNotFound || strings.Contains(foreignResponse.Body.String(), document.Input) ||
		strings.Contains(foreignResponse.Body.String(), document.Result) {
		t.Fatalf("foreign owner task detail response=%d body=%s; want not found without private detail", foreignResponse.Code, foreignResponse.Body.String())
	}
	foreignEventsRequest := httptest.NewRequest(http.MethodGet, "/api/tasks/"+current.Task.ID+"/events?after=0", nil)
	foreignEventsRequest.AddCookie(&http.Cookie{Name: "ch_portal", Value: foreignCredential})
	foreignEventsResponse := httptest.NewRecorder()
	foreignRouter.ServeHTTP(foreignEventsResponse, foreignEventsRequest)
	if foreignEventsResponse.Code != http.StatusNotFound || strings.Contains(foreignEventsResponse.Body.String(), document.Input) ||
		strings.Contains(foreignEventsResponse.Body.String(), document.Result) {
		t.Fatalf("foreign owner task event stream response=%d body=%s; want not found without private detail", foreignEventsResponse.Code, foreignEventsResponse.Body.String())
	}
}

func TestPostgresTaskStepDetailStoresFullCapturedOutputAboveLegacyLimit(t *testing.T) {
	fixture := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := fixture.service.Start(ctx, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	current, err := fixture.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || current.Task.CurrentRevision == nil {
		t.Fatalf("load task revision: task=%+v err=%v", current.Task, err)
	}
	stepID, err := domain.NewStepID()
	if err != nil {
		t.Fatal(err)
	}
	capturedOutput := strings.Repeat("captured-output-", 4<<10)
	document := app.TaskStepDetailDocument{Object: "shell_exec", Input: "Run an authorized command.", Result: "Shell completed.",
		ShellExecution: &app.TaskStepShellExecutionEvidence{StdoutBytes: len(capturedOutput)},
		Execution:      &app.TaskStepExecution{Stdout: &capturedOutput}, ExecutionWorkspaceID: "w1234567"}
	plain, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := sealer.SealWithAAD(plain, aadForTaskStepDetail(current.Task.OwnerID, current.Task.ID, stepID,
		*current.Task.CurrentRevision))
	if err != nil {
		t.Fatal(err)
	}
	detailDigest := sha256.Sum256(plain)
	size := int32(len(plain))
	if len(ciphertext) <= 65564 || size <= 65536 {
		t.Fatalf("test detail did not exceed legacy storage bounds: ciphertext=%d plaintext=%d", len(ciphertext), size)
	}
	step := task.Step{ID: stepID, TaskID: current.Task.ID, Sequence: 1, Kind: "tool", Status: task.StepSucceeded,
		Title: "Run command", Summary: "Shell completed.", CreatedAt: time.Now().UTC()}
	requestDigest := sha256.Sum256([]byte("shell.exec:{\"command\":\"printf captured\"}"))
	usage := current.Task.Usage
	usage.ToolCalls++
	payload, err := json.Marshal(map[string]any{"step_id": step.ID, "sequence": step.Sequence, "kind": step.Kind,
		"status": step.Status, "title": step.Title, "summary": step.Summary})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		Status: task.StatusRunning, CurrentStep: step.ID, Usage: usage, Steps: []task.Step{step},
		Event: task.Event{TaskID: current.Task.ID, Type: "task.step", Payload: payload},
		ToolCall: &app.TaskToolCallProgress{AgentSessionID: lease.Fence.AgentSessionID, AgentInstanceID: lease.Fence.AgentInstanceID,
			AgentGeneration: lease.Fence.AgentGeneration, AuthorizationVersion: lease.Fence.AuthorizationVersion,
			TaskRevision: *current.Task.CurrentRevision, CallKey: "http-shell-large-output", ToolName: "shell_exec",
			RequestDigest: requestDigest[:], Phase: string(task.StepSucceeded), DetailState: "available",
			DetailCiphertext: ciphertext, DetailSHA256: detailDigest[:], DetailSizeBytes: size},
	}); err != nil {
		t.Fatalf("persist large encrypted Task step detail: %v", err)
	}
	record, err := fixture.store.GetTaskStepDetail(ctx, current.Task.OwnerID, current.Task.ID, stepID)
	if err != nil || record.SizeBytes != size || len(record.Ciphertext) != len(ciphertext) || len(record.Ciphertext) <= 65564 {
		t.Fatalf("read large encrypted Task step detail: record bytes=%d size=%d err=%v", len(record.Ciphertext), record.SizeBytes, err)
	}
	opened, err := sealer.OpenWithAAD(record.Ciphertext, aadForTaskStepDetail(current.Task.OwnerID, current.Task.ID, stepID,
		*current.Task.CurrentRevision))
	if err != nil {
		t.Fatalf("open persisted large detail: %v", err)
	}
	var got app.TaskStepDetailDocument
	if err := json.Unmarshal(opened, &got); err != nil || got.Execution == nil || got.Execution.Stdout == nil || *got.Execution.Stdout != capturedOutput {
		t.Fatalf("persisted captured output length=%d err=%v", func() int {
			if got.Execution == nil || got.Execution.Stdout == nil {
				return 0
			}
			return len(*got.Execution.Stdout)
		}(), err)
	}
}
