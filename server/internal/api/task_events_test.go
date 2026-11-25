package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

// P3-5: the E08 nine-kind chain must be readable through the HTTP
// interface. The fixtures below build chains with the real projection
// (app.ProjectTaskEventChain), so the tests prove the interface returns
// what the projection computed — including the no-delivery case, where a
// FAIL verdict yields a verification node and the interface must not
// invent a delivery node.

func taskEventChainTestEvent(sequence int64, eventType, payload string) task.Event {
	return task.Event{TaskID: "q1234567", Sequence: sequence, Type: eventType,
		Payload: []byte(payload), CreatedAt: time.Unix(1000+sequence, 0).UTC()}
}

func taskEventChainCompleteEvents() []task.Event {
	return []task.Event{
		taskEventChainTestEvent(1, "task.status", `{"status":"queued"}`),
		taskEventChainTestEvent(2, "task.plan", `{"step_id":"s0000001","sequence":1,"task_revision":0,"plan_revision":1}`),
		taskEventChainTestEvent(3, "task.message", `{"role":"user","message_id":"m0000001","turn_id":"s0000001","done":true}`),
		taskEventChainTestEvent(4, "task.operation", `{"operation_id":"op000001","status":"dispatched"}`),
		taskEventChainTestEvent(5, "task.step", `{"step_id":"s0000002","status":"running","kind":"tool","tool":"go_test"}`),
		taskEventChainTestEvent(6, "task.step", `{"step_id":"s0000002","status":"failed","kind":"tool","tool":"go_test"}`),
		taskEventChainTestEvent(7, "task.operation", `{"operation_id":"op000001","status":"unknown","requires_reconciliation":true}`),
		taskEventChainTestEvent(8, "task.step", `{"step_id":"s0000003","status":"succeeded","kind":"tool","tool":"go_test"}`),
		taskEventChainTestEvent(9, "task.waiting_user", `{"status":"waiting_user","reason":"approval_required"}`),
		taskEventChainTestEvent(10, "task.response", `{"pending_id":"p0000001","decision":"approve","status":"approved"}`),
		taskEventChainTestEvent(11, "task.artifact", `{"artifact_id":"a0000001","step_id":"s0000003"}`),
		taskEventChainTestEvent(12, "task.priority", `{"priority":"pinned","previous_priority":"normal"}`),
		taskEventChainTestEvent(13, "task.status", `{"status":"completed"}`),
		taskEventChainTestEvent(14, "task.result", `{"status":"completed"}`),
	}
}

func taskEventChainFixture(t *testing.T, facts app.TaskEventChainFacts) app.TaskEventChain {
	t.Helper()
	chain, err := app.ProjectTaskEventChain(taskEventChainCompleteEvents(), facts)
	if err != nil {
		t.Fatal(err)
	}
	return chain
}

func taskEventChainKinds(body taskEventChainResponse) []string {
	kinds := make([]string, 0, len(body.Nodes))
	for _, node := range body.Nodes {
		kinds = append(kinds, node.Kind)
	}
	return kinds
}

func TestGetTaskEventChainReturnsCompleteChain(t *testing.T) {
	chain := taskEventChainFixture(t, app.TaskEventChainFacts{ValidationState: app.TaskValidationPass,
		ResultEventSequence: 14, ReceiptPublished: true, ReceiptEventSequence: 14})
	service := &taskServiceStub{chain: chain}
	engine := taskAPIRouter(service, nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/event-chain", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET event chain status=%d body=%s", response.Code, response.Body.String())
	}
	var body taskEventChainResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	wantKinds := []string{"created", "split", "dispatched", "execution", "failure", "recovery",
		"execution", "approval", "approval", "verification", "delivery"}
	if body.TaskID != "q1234567" || len(body.Nodes) != len(wantKinds) {
		t.Fatalf("event chain = %+v, want %d nodes for task q1234567", body, len(wantKinds))
	}
	for index, kind := range wantKinds {
		if body.Nodes[index].Kind != kind {
			t.Fatalf("chain node %d kind = %q, want %q (kinds %v)", index, body.Nodes[index].Kind, kind, taskEventChainKinds(body))
		}
	}
	if body.Nodes[0].SourceSequence != 1 || body.Nodes[0].SourceType != "task.status" {
		t.Fatalf("chain head = %+v, want created from task.status at sequence 1", body.Nodes[0])
	}
	delivery := body.Nodes[len(body.Nodes)-1]
	if delivery.Kind != "delivery" || delivery.SourceSequence != 14 || delivery.SourceType != "" || delivery.Detail != "receipt published" {
		t.Fatalf("delivery node = %+v, want fact-derived delivery at sequence 14", delivery)
	}
	// Collections are arrays, never null — the raw body proves it.
	if !strings.Contains(response.Body.String(), `"unmapped_sequences":[3,11,12,13,14]`) {
		t.Fatalf("event chain unmapped sequences are not an array: %s", response.Body.String())
	}
	if body.UnmappedSequences == nil || len(body.UnmappedSequences) != 5 {
		t.Fatalf("unmapped = %v, want [3 11 12 13 14]", body.UnmappedSequences)
	}
}

func TestGetTaskEventChainOmitsDeliveryOnFailedVerdict(t *testing.T) {
	// The counterexample through the interface: a FAIL verdict with a
	// published receipt projects a verification node and no delivery node,
	// and the handler must return exactly that — it must not invent the
	// delivery the completed status event seems to promise.
	chain := taskEventChainFixture(t, app.TaskEventChainFacts{ValidationState: app.TaskValidationFail,
		ResultEventSequence: 14, ReceiptPublished: true, ReceiptEventSequence: 14})
	service := &taskServiceStub{chain: chain}
	engine := taskAPIRouter(service, nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/event-chain", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET event chain status=%d body=%s", response.Code, response.Body.String())
	}
	var body taskEventChainResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	kinds := taskEventChainKinds(body)
	for _, kind := range kinds {
		if kind == "delivery" {
			t.Fatalf("FAIL verdict chain gained a delivery node through the interface: kinds %v", kinds)
		}
	}
	foundVerification := false
	for _, node := range body.Nodes {
		if node.Kind == "verification" {
			foundVerification = true
			if node.Detail != "FAIL" || node.SourceSequence != 14 {
				t.Fatalf("verification node = %+v, want FAIL at sequence 14", node)
			}
		}
	}
	if !foundVerification {
		t.Fatalf("FAIL verdict chain lost its verification node: kinds %v", kinds)
	}
}

func TestGetTaskEventChainRejectsBadRequests(t *testing.T) {
	missing := &taskServiceStub{chainErr: domain.NewError(domain.CodeNotFound, "task not found")}
	engine := taskAPIRouter(missing, nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/event-chain", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing task event chain status=%d, want 404: %s", response.Code, response.Body.String())
	}
	service := &taskServiceStub{chain: taskEventChainFixture(t, app.TaskEventChainFacts{})}
	engine = taskAPIRouter(service, nil)
	for _, path := range []string{"/api/tasks/bad!/event-chain", "/api/tasks/q1234567/event-chain?after=1"} {
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("GET %s status=%d, want 400: %s", path, response.Code, response.Body.String())
		}
	}
	if service.chainCalls != 0 {
		t.Fatalf("rejected event chain requests reached the service (%d calls)", service.chainCalls)
	}
}

func TestStreamTaskEventChainSendsSnapshotAndStopsOnTerminal(t *testing.T) {
	chain := taskEventChainFixture(t, app.TaskEventChainFacts{ValidationState: app.TaskValidationPass,
		ResultEventSequence: 14, ReceiptPublished: true, ReceiptEventSequence: 14})
	details := apiTestTask()
	details.Status = task.StatusCompleted
	service := &taskServiceStub{chain: chain, getDetails: &app.TaskDetails{Task: details}}
	engine := taskAPIRouter(service, nil)
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/event-chain/stream", nil))
	if response.Code != http.StatusOK || response.deadlineCalls != 1 || service.chainCalls != 1 {
		t.Fatalf("chain stream status=%d deadline calls=%d chain calls=%d body=%s",
			response.Code, response.deadlineCalls, service.chainCalls, response.Body.String())
	}
	if !strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("chain stream content type = %q, want text/event-stream", response.Header().Get("Content-Type"))
	}
	body := response.Body.String()
	for _, expected := range []string{"event: task.event_chain", `"kind":"delivery"`, `"kind":"created"`, `"unmapped_sequences":[3,11,12,13,14]`} {
		if !strings.Contains(body, expected) {
			t.Errorf("chain stream missing %q: %s", expected, body)
		}
	}
	if strings.Count(body, "event: task.event_chain") != 1 {
		t.Fatalf("terminal chain stream sent %d snapshots, want exactly 1: %s", strings.Count(body, "event: task.event_chain"), body)
	}
}

func TestStreamTaskEventChainSendsChangedSnapshotOnPoll(t *testing.T) {
	withoutReceipt := taskEventChainFixture(t, app.TaskEventChainFacts{ValidationState: app.TaskValidationPass,
		ResultEventSequence: 14, ReceiptPublished: false})
	withReceipt := taskEventChainFixture(t, app.TaskEventChainFacts{ValidationState: app.TaskValidationPass,
		ResultEventSequence: 14, ReceiptPublished: true, ReceiptEventSequence: 14})
	service := &taskServiceStub{
		chains:            []app.TaskEventChain{withoutReceipt, withReceipt},
		chainErr:          errors.New("event chain store failed"),
		chainErrAfterCall: 3,
	}
	engine := taskAPIRouter(service, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/event-chain/stream", nil)
	requestContext, cancel := context.WithCancel(request.Context())
	defer cancel()
	request = request.WithContext(requestContext)
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	engine.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || service.chainCalls != 3 {
		t.Fatalf("chain stream status=%d chain calls=%d body=%s", response.Code, service.chainCalls, body)
	}
	frames := strings.Count(body, "event: task.event_chain")
	if frames != 2 {
		t.Fatalf("chain stream sent %d snapshots, want 2 (initial + delivery landing): %s", frames, body)
	}
	// The first snapshot has no delivery; the second one does — the delivery
	// node landed on an old sequence (14) and still reached the client.
	first, second := strings.SplitN(body, "event: task.event_chain", 3)[1], strings.SplitN(body, "event: task.event_chain", 3)[2]
	if strings.Contains(first, `"kind":"delivery"`) {
		t.Fatalf("first chain snapshot already has a delivery node: %s", body)
	}
	if !strings.Contains(second, `"kind":"delivery"`) {
		t.Fatalf("second chain snapshot is missing the delivery node: %s", body)
	}
}

func TestTaskEventChainOpenAPIContract(t *testing.T) {
	openAPI, err := os.ReadFile(filepath.Join("..", "..", "openapi", "portal.yaml"))
	if err != nil {
		t.Fatalf("read Portal OpenAPI schema: %v", err)
	}
	contract := string(openAPI)
	for _, required := range []string{
		"operationId: getTaskEventChain",
		"operationId: streamTaskEventChain",
		"/api/tasks/{id}/event-chain:",
		"/api/tasks/{id}/event-chain/stream:",
		"TaskEventChain:",
		"TaskEventChainNode:",
		"enum: [created, split, dispatched, approval, execution, verification, delivery, failure, recovery]",
		"unmapped_sequences: { type: array, items: { type: integer, format: int64 } }",
		"nodes: { type: array, items: { $ref: '#/components/schemas/TaskEventChainNode' } }",
	} {
		if !strings.Contains(contract, required) {
			t.Fatalf("Portal OpenAPI event chain contract omits %q", required)
		}
	}
	// The write-side TaskEvent enum is untouched: still eleven types.
	if !strings.Contains(contract, "TaskEvent:") {
		t.Fatal("Portal OpenAPI TaskEvent schema is missing")
	}
}
