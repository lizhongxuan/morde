package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type taskServiceStub struct {
	mu              sync.Mutex
	submissions     map[string]app.TaskSubmissionResult
	requests        map[string]string
	submitCalls     int
	lastSubmit      app.SubmitTaskCommand
	lastPrincipal   identity.Principal
	listCalls       int
	lastList        app.TaskListRequest
	page            app.TaskPage
	details         app.TaskDetails
	getCalls        int
	actionCalls     int
	lastAction      task.Action
	lastExpected    task.Status
	lastActionKey   string
	actionErr       error
	responseCalls   int
	lastDecision    task.PendingDecision
	lastResponseKey string
	clarifyCalls    int
	respondErr      error
}

func newTaskServiceStub() *taskServiceStub {
	return &taskServiceStub{submissions: make(map[string]app.TaskSubmissionResult), requests: make(map[string]string)}
}

func (s *taskServiceStub) Submit(_ context.Context, principal identity.Principal, command app.SubmitTaskCommand) (app.TaskSubmissionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.submitCalls++
	s.lastPrincipal, s.lastSubmit = principal, command
	encoded, err := json.Marshal(command)
	if err != nil {
		return app.TaskSubmissionResult{}, err
	}
	if existing, ok := s.submissions[command.IdempotencyKey]; ok {
		if s.requests[command.IdempotencyKey] != string(encoded) {
			return app.TaskSubmissionResult{}, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was reused")
		}
		existing.Replayed = true
		return existing, nil
	}
	now := time.Now().UTC()
	value := app.TaskSubmissionResult{
		Task: task.Task{
			ID: "q1234567", OwnerID: principal.UserID, HostID: command.HostID, ConversationID: "c1234567",
			ApplicationID: command.ApplicationID, ModelID: command.ModelID, Objective: command.Objective, Status: task.StatusQueued,
			Budget: command.Budget, Authorization: task.Authorization{Kind: "api_key", TokenID: "secret-token-id"}, CreatedAt: now, UpdatedAt: now,
		},
		Conversation: appConversation(command, principal.UserID, now),
	}
	s.submissions[command.IdempotencyKey] = value
	s.requests[command.IdempotencyKey] = string(encoded)
	return value, nil
}

func appConversation(command app.SubmitTaskCommand, owner string, now time.Time) conversation.Conversation {
	conversationID := command.ConversationID
	if conversationID == "" {
		conversationID = "c1234567"
	}
	return conversation.Conversation{ID: conversationID, OwnerID: owner, HostID: command.HostID, CreatedAt: now, UpdatedAt: now}
}

func (s *taskServiceStub) List(_ context.Context, principal identity.Principal, request app.TaskListRequest) (app.TaskPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	s.lastPrincipal, s.lastList = principal, request
	return s.page, nil
}

func (s *taskServiceStub) Get(_ context.Context, principal identity.Principal, _ string) (app.TaskDetails, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getCalls++
	s.lastPrincipal = principal
	return s.details, nil
}

func (s *taskServiceStub) RequestAction(_ context.Context, principal identity.Principal, taskID string, action task.Action, expected task.Status, key string) (app.TaskActionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actionCalls++
	s.lastPrincipal, s.lastAction, s.lastExpected, s.lastActionKey = principal, action, expected, key
	if s.actionErr != nil {
		return app.TaskActionResult{}, s.actionErr
	}
	value := s.details.Task
	value.ID, value.OwnerID = taskID, principal.UserID
	return app.TaskActionResult{Task: value, Action: action}, nil
}

func (s *taskServiceStub) RespondToPendingRequest(_ context.Context, principal identity.Principal, _, _ string, _ int64, decision task.PendingDecision, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responseCalls++
	s.lastPrincipal = principal
	s.lastDecision, s.lastResponseKey = decision, key
	return s.respondErr
}

func (s *taskServiceStub) AnswerClarificationRequest(_ context.Context, principal identity.Principal, _, _ string, _ int64, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clarifyCalls++
	s.lastPrincipal = principal
	return s.respondErr
}

type mutableTaskKeyVerifier struct {
	mu         sync.RWMutex
	principals map[string]identity.Principal
}

func (v *mutableTaskKeyVerifier) set(key string, principal identity.Principal) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.principals[key] = principal
}

func (v *mutableTaskKeyVerifier) revoke(key string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.principals, key)
}

func (v *mutableTaskKeyVerifier) VerifyAPIKey(_ context.Context, key string) (identity.Principal, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	principal, ok := v.principals[key]
	if !ok {
		return identity.Principal{}, identity.ErrInvalidCredential
	}
	return principal, nil
}

func (*mutableTaskKeyVerifier) VerifySession(context.Context, string) (identity.Principal, error) {
	return identity.Principal{}, identity.ErrInvalidCredential
}

func (*mutableTaskKeyVerifier) ConsumeTicket(context.Context, string) (app.Ticket, error) {
	return app.Ticket{}, identity.ErrInvalidCredential
}

func taskPrincipal(scopes ...identity.Scope) identity.Principal {
	return identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567",
		HostIDs: []string{"h1234567"}, Scopes: scopes,
	}
}

func newTaskMCPServer(t *testing.T, verifier app.Verifier, tasks taskService, maxOutput int) *httptest.Server {
	t.Helper()
	handler, err := New(Deps{
		PreviewPortMin: 1024, Config: config.MCP{Enabled: true, MaxOutputBytes: maxOutput, ScreenshotMaxWidth: 1280},
		Domains:      config.Domains{Scheme: "https", Portal: "portal.example.test", HostSuffix: "hosts.example.test"},
		Hosts:        &hostServiceStub{value: host.Host{ID: "h1234567", State: host.StateRunning}},
		Applications: newApplicationStatusServiceStub(), RuntimeTools: &runtimeComputerToolsStub{}, Tasks: tasks, Schedules: scheduleServiceFake{}, Auth: verifier, Audit: &auditStub{},
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func connectTaskMCP(t *testing.T, endpoint string, token string) *sdk.ClientSession {
	t.Helper()
	transport := &rotatingBearerTransport{base: http.DefaultTransport, token: token}
	client := sdk.NewClient(&sdk.Implementation{Name: "task-tool-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &sdk.StreamableClientTransport{
		Endpoint: endpoint, DisableStandaloneSSE: true, HTTPClient: &http.Client{Transport: transport},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestTaskToolsRegisterAndMapToSharedTaskService(t *testing.T) {
	principal := taskPrincipal(identity.ScopeTaskRead, identity.ScopeTaskSubmit, identity.ScopeTaskControl)
	verifier := &mutableTaskKeyVerifier{principals: map[string]identity.Principal{"chk_tasks": principal}}
	now := time.Now().UTC()
	service := &taskServiceStub{
		submissions: make(map[string]app.TaskSubmissionResult), requests: make(map[string]string),
		page: app.TaskPage{Items: []task.Task{{
			ID: "q1234567", OwnerID: principal.UserID, HostID: "h1234567", ConversationID: "c1234567", ModelID: "model-a",
			Objective: "summarize", Status: task.StatusQueued, CreatedAt: now, UpdatedAt: now,
		}}},
		details: app.TaskDetails{Task: task.Task{
			ID: "q1234567", OwnerID: principal.UserID, HostID: "h1234567", ConversationID: "c1234567", ModelID: "model-a",
			Objective: "summarize", Status: task.StatusWaitingUser, WaitingReason: "clarification_required", CreatedAt: now, UpdatedAt: now,
		}, ClarificationRequests: []task.ClarificationRequest{{ID: "r1234567", TaskID: "q1234567", Version: 3, Question: "Which date range?", Status: task.ClarificationPending}}},
	}
	server := newTaskMCPServer(t, verifier, service, 65536)
	session := connectTaskMCP(t, server.URL, "chk_tasks")
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(listed.Tools))
	for _, value := range listed.Tools {
		got[value.Name] = true
	}
	for _, name := range []string{"task_submit", "task_list", "task_get", "task_pause", "task_resume", "task_cancel", "task_respond"} {
		if !got[name] {
			t.Errorf("task tool %q is not registered", name)
		}
	}
	if len(listed.Tools) != 22+len(app.RuntimeComputerTools()) {
		t.Fatalf("registered tool count = %d, want %d", len(listed.Tools), 22+len(app.RuntimeComputerTools()))
	}

	submitted, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "task_submit", Arguments: map[string]any{
		"host_id": "h1234567", "application_id": "wechat", "model_id": "model-a", "objective": "summarize this folder", "idempotency_key": "submit-key-001",
		"budget": map[string]any{"active_seconds": json.Number("90"), "tool_calls": json.Number("12"), "model_tokens": json.Number("4000")},
	}})
	if err != nil || submitted.IsError {
		t.Fatalf("task_submit result=%+v err=%v", submitted, err)
	}
	if service.lastSubmit.HostID != "h1234567" || service.lastSubmit.ApplicationID != "wechat" || service.lastSubmit.IdempotencyKey != "submit-key-001" ||
		service.lastSubmit.Budget != (task.Budget{ActiveSeconds: 90, ToolCalls: 12, ModelTokens: 4000}) || service.lastPrincipal.TokenID != principal.TokenID {
		t.Fatalf("TaskService submission mapping = %+v principal=%+v", service.lastSubmit, service.lastPrincipal)
	}
	if !strings.Contains(textResult(submitted), "https://portal.example.test/tasks/q1234567") {
		t.Fatalf("task_submit omitted login-gated task URL: %s", textResult(submitted))
	}
	if !strings.Contains(textResult(submitted), `"application_id":"wechat"`) {
		t.Fatalf("task_submit omitted application target: %s", textResult(submitted))
	}

	listedTasks, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "task_list", Arguments: map[string]any{"status": "queued", "limit": json.Number("5")}})
	if err != nil || listedTasks.IsError || service.lastList.Status != task.StatusQueued || service.lastList.Page.Limit != 5 {
		t.Fatalf("task_list mapping result=%+v err=%v request=%+v", listedTasks, err, service.lastList)
	}
	gotTask, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "task_get", Arguments: map[string]any{"task_id": "q1234567"}})
	if err != nil || gotTask.IsError || !strings.Contains(textResult(gotTask), "Which date range?") {
		t.Fatalf("task_get result=%+v err=%v", gotTask, err)
	}
	responded, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "task_respond", Arguments: map[string]any{
		"task_id": "q1234567", "clarification_id": "r1234567", "version": json.Number("3"), "answer": "Last week", "idempotency_key": "respond-key-001",
	}})
	if err != nil || responded.IsError || service.clarifyCalls != 1 {
		t.Fatalf("task_respond result=%+v err=%v calls=%d", responded, err, service.clarifyCalls)
	}
	paused, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "task_pause", Arguments: map[string]any{
		"task_id": "q1234567", "expected_status": "running", "idempotency_key": "pause-key-001",
	}})
	if err != nil || paused.IsError || service.lastAction != task.ActionPause || service.lastExpected != task.StatusRunning || service.lastActionKey != "pause-key-001" {
		t.Fatalf("task_pause result=%+v err=%v action=%q expected=%q key=%q", paused, err, service.lastAction, service.lastExpected, service.lastActionKey)
	}
	for _, test := range []struct {
		name           string
		expectedStatus task.Status
		wantAction     task.Action
	}{
		{name: "task_resume", expectedStatus: task.StatusPaused, wantAction: task.ActionResume},
		{name: "task_cancel", expectedStatus: task.StatusWaitingUser, wantAction: task.ActionCancel},
	} {
		result, callErr := session.CallTool(context.Background(), &sdk.CallToolParams{Name: test.name, Arguments: map[string]any{
			"task_id": "q1234567", "expected_status": string(test.expectedStatus), "idempotency_key": test.name + "-key-001",
		}})
		if callErr != nil || result.IsError || service.lastAction != test.wantAction || service.lastExpected != test.expectedStatus {
			t.Errorf("%s result=%+v err=%v action=%q expected=%q", test.name, result, callErr, service.lastAction, service.lastExpected)
		}
	}
	approved, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "task_respond", Arguments: map[string]any{
		"task_id": "q1234567", "pending_id": "r1234567", "decision": "approve", "version": json.Number("1"), "idempotency_key": "approve-key-001",
	}})
	if err != nil || approved.IsError || service.responseCalls != 1 || service.lastDecision != task.PendingDecisionApprove || service.lastResponseKey != "approve-key-001" {
		t.Fatalf("pending approval response=%+v err=%v calls=%d decision=%q key=%q", approved, err, service.responseCalls, service.lastDecision, service.lastResponseKey)
	}
}

func TestTaskSubmitDeduplicatesAcrossTwoMCPClients(t *testing.T) {
	principal := taskPrincipal(identity.ScopeTaskSubmit)
	verifier := &mutableTaskKeyVerifier{principals: map[string]identity.Principal{"chk_tasks": principal}}
	service := newTaskServiceStub()
	server := newTaskMCPServer(t, verifier, service, 65536)
	first, second := connectTaskMCP(t, server.URL, "chk_tasks"), connectTaskMCP(t, server.URL, "chk_tasks")
	params := &sdk.CallToolParams{Name: "task_submit", Arguments: map[string]any{
		"host_id": "h1234567", "model_id": "model-a", "objective": "same request", "idempotency_key": "same-submit-key",
	}}
	results := make([]*sdk.CallToolResult, 2)
	errs := make([]error, 2)
	var wait sync.WaitGroup
	wait.Add(2)
	go func() { defer wait.Done(); results[0], errs[0] = first.CallTool(context.Background(), params) }()
	go func() { defer wait.Done(); results[1], errs[1] = second.CallTool(context.Background(), params) }()
	wait.Wait()
	for index := range results {
		if errs[index] != nil || results[index] == nil || results[index].IsError {
			t.Fatalf("client %d submit = %+v, err=%v", index, results[index], errs[index])
		}
	}
	if service.submitCalls != 2 || len(service.submissions) != 1 {
		t.Fatalf("MCP calls=%d durable task count=%d; expected two calls under one idempotency key", service.submitCalls, len(service.submissions))
	}
	if !strings.Contains(textResult(results[0]), "q1234567") || !strings.Contains(textResult(results[1]), "q1234567") {
		t.Fatalf("clients did not observe the same task: %q / %q", textResult(results[0]), textResult(results[1]))
	}
	var replayStates []bool
	for _, result := range results {
		var output struct {
			Replayed bool `json:"replayed"`
		}
		if err := json.Unmarshal([]byte(textResult(result)), &output); err != nil {
			t.Fatalf("decode task_submit output: %v", err)
		}
		replayStates = append(replayStates, output.Replayed)
	}
	if replayStates[0] == replayStates[1] {
		t.Fatalf("same-key submissions should return one new task and one replay, got %v", replayStates)
	}
	changed, err := first.CallTool(context.Background(), &sdk.CallToolParams{Name: "task_submit", Arguments: map[string]any{
		"host_id": "h1234567", "model_id": "model-a", "objective": "different request", "idempotency_key": "same-submit-key",
	}})
	if err != nil || changed == nil || !changed.IsError || !strings.Contains(textResult(changed), string(domain.CodeIdempotencyConflict)) {
		t.Fatalf("changed request reused an idempotency key: result=%+v err=%v", changed, err)
	}
}

func TestTaskToolsRecheckRevokedAndExpiredAuthorizationOnSessionReuse(t *testing.T) {
	principal := taskPrincipal(identity.ScopeTaskRead, identity.ScopeTaskControl)
	verifier := &mutableTaskKeyVerifier{principals: map[string]identity.Principal{"chk_tasks": principal}}
	service := newTaskServiceStub()
	service.details = app.TaskDetails{Task: task.Task{
		ID: "q1234567", OwnerID: principal.UserID, HostID: "h1234567", Status: task.StatusWaitingUser,
	}}
	server := newTaskMCPServer(t, verifier, service, 65536)
	session := connectTaskMCP(t, server.URL, "chk_tasks")
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "task_get", Arguments: map[string]any{"task_id": "q1234567"}})
	if err != nil || result.IsError {
		t.Fatalf("first task_get result=%+v err=%v", result, err)
	}
	verifier.set("chk_tasks", taskPrincipal(identity.ScopeTaskControl))
	result, err = session.CallTool(context.Background(), &sdk.CallToolParams{Name: "task_get", Arguments: map[string]any{"task_id": "q1234567"}})
	if err != nil || result == nil || !result.IsError || !strings.Contains(textResult(result), string(domain.CodeForbidden)) {
		t.Fatalf("removed task:read scope result=%+v err=%v", result, err)
	}
	if service.getCalls != 1 {
		t.Fatalf("task service calls after current scope removal = %d, want 1", service.getCalls)
	}
	verifier.revoke("chk_tasks")
	result, _ = session.CallTool(context.Background(), &sdk.CallToolParams{Name: "task_respond", Arguments: map[string]any{
		"task_id": "q1234567", "pending_id": "r1234567", "decision": "approve", "version": json.Number("1"), "idempotency_key": "respond-key-001",
	}})
	if result != nil && !result.IsError {
		t.Fatal("revoked API Key continued using its established MCP session")
	}
	if service.responseCalls != 0 {
		t.Fatalf("revoked API Key reached TaskService %d times", service.responseCalls)
	}
}

func TestTaskToolsFailClosedForHostAllowListAndTakeover(t *testing.T) {
	allowed := taskPrincipal(identity.ScopeTaskSubmit, identity.ScopeTaskControl)
	if err := authorizeTool(allowed, "task_submit", map[string]any{"host_id": "h1234567"}); err != nil {
		t.Fatalf("authorized task_submit: %v", err)
	}
	unscopedHost := taskPrincipal(identity.ScopeTaskSubmit)
	unscopedHost.HostIDs = nil
	if code := domain.CodeOf(authorizeTool(unscopedHost, "task_submit", map[string]any{"host_id": "h1234567"})); code != domain.CodeNotFound {
		t.Fatalf("task_submit without Host grant code=%q, want not_found", code)
	}
	if code := domain.CodeOf(authorizeTool(taskPrincipal(identity.ScopeTaskRead), "task_cancel", map[string]any{"task_id": "q1234567"})); code != domain.CodeForbidden {
		t.Fatalf("task_cancel without task:control code=%q, want forbidden", code)
	}

	principal := taskPrincipal(identity.ScopeTaskControl)
	verifier := &mutableTaskKeyVerifier{principals: map[string]identity.Principal{"chk_tasks": principal}}
	service := newTaskServiceStub()
	service.details = app.TaskDetails{Task: task.Task{
		ID: "q1234567", OwnerID: principal.UserID, HostID: "h1234567", Status: task.StatusRunning,
	}}
	service.actionErr = domain.NewError(domain.CodeInvalidState, "host is controlled by the user")
	server := newTaskMCPServer(t, verifier, service, 65536)
	session := connectTaskMCP(t, server.URL, "chk_tasks")
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "task_pause", Arguments: map[string]any{
		"task_id": "q1234567", "expected_status": "running", "idempotency_key": "pause-key-001",
	}})
	if err != nil || result == nil || !result.IsError || !strings.Contains(textResult(result), string(domain.CodeInvalidState)) {
		t.Fatalf("takeover-fenced task_pause result=%+v err=%v", result, err)
	}
	if service.actionCalls != 1 {
		t.Fatalf("takeover attempt did not go through TaskService: calls=%d", service.actionCalls)
	}
}

func TestTaskOutputsAndErrorsAreBoundedAndSanitized(t *testing.T) {
	principal := taskPrincipal(identity.ScopeTaskRead, identity.ScopeTaskControl)
	verifier := &mutableTaskKeyVerifier{principals: map[string]identity.Principal{"chk_tasks": principal}}
	service := newTaskServiceStub()
	service.details = app.TaskDetails{Task: task.Task{
		ID: "q1234567", OwnerID: principal.UserID, HostID: "h1234567", ModelID: "model-a", Status: task.StatusCompleted,
		Result: strings.Repeat("result ", 3000), Authorization: task.Authorization{Kind: "api_key", TokenID: "must-not-leak"},
	}}
	server := newTaskMCPServer(t, verifier, service, 512)
	session := connectTaskMCP(t, server.URL, "chk_tasks")
	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: "task_get", Arguments: map[string]any{"task_id": "q1234567"}})
	if err != nil || result == nil || result.IsError {
		t.Fatalf("large task_get result=%+v err=%v", result, err)
	}
	if text := textResult(result); len(text) > 600 || strings.Contains(text, "must-not-leak") {
		t.Fatalf("task output exceeded the configured limit or leaked internal authorization: length=%d text=%q", len(text), text)
	}
	if got := boundedText("Authorization: Bearer secret-value API_KEY=private-value chk_t1234567_keysecret", 256); strings.Contains(got, "secret-value") || strings.Contains(got, "private-value") || strings.Contains(got, "keysecret") {
		t.Fatalf("task text credentials were not redacted: %q", got)
	}
	service.actionErr = errors.New("provider credential secret-value")
	result, err = session.CallTool(context.Background(), &sdk.CallToolParams{Name: "task_cancel", Arguments: map[string]any{
		"task_id": "q1234567", "expected_status": "running", "idempotency_key": "cancel-key-001",
	}})
	if err != nil || result == nil || !result.IsError || strings.Contains(textResult(result), "secret-value") ||
		!strings.Contains(textResult(result), string(domain.CodeInternal)) {
		t.Fatalf("task error was not sanitized: result=%+v err=%v", result, err)
	}
}

func TestValidateTaskToolArguments(t *testing.T) {
	tests := []struct {
		name    string
		spec    toolSpec
		json    string
		wantErr bool
	}{
		{name: "submit with budget", spec: toolByName("task_submit"), json: `{"host_id":"h1234567","model_id":"model-a","objective":"run","idempotency_key":"submit-key-001","budget":{"active_seconds":30,"tool_calls":5,"model_tokens":1000}}`},
		{name: "unknown target application", spec: toolByName("task_submit"), json: `{"host_id":"h1234567","application_id":"unknown","model_id":"model-a","objective":"run","idempotency_key":"submit-key-001"}`, wantErr: true},
		{name: "partial budget", spec: toolByName("task_submit"), json: `{"host_id":"h1234567","model_id":"model-a","objective":"run","idempotency_key":"submit-key-001","budget":{"active_seconds":30}}`, wantErr: true},
		{name: "task list over limit", spec: toolByName("task_list"), json: `{"limit":101}`, wantErr: true},
		{name: "task respond mixed request types", spec: toolByName("task_respond"), json: `{"task_id":"q1234567","version":1,"idempotency_key":"respond-key-001","pending_id":"r1234567","decision":"approve","clarification_id":"r2345678","answer":"yes"}`, wantErr: true},
		{name: "task respond missing decision", spec: toolByName("task_respond"), json: `{"task_id":"q1234567","version":1,"idempotency_key":"respond-key-001","pending_id":"r1234567"}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateToolArguments(test.spec, json.RawMessage(test.json))
			if (err != nil) != test.wantErr {
				t.Fatalf("validateToolArguments() error=%v wantErr=%t", err, test.wantErr)
			}
		})
	}
}

var _ taskService = (*taskServiceStub)(nil)
var _ app.Verifier = (*mutableTaskKeyVerifier)(nil)
