package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"

	"morde/server/internal/api"
	"morde/server/internal/app"
	"morde/server/internal/config"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/schedule"
	"morde/server/internal/domain/task"
)

type cloudDSHTaskServiceProbe struct {
	api.TaskService
	submits, appends, lists, retries, answers int
	actions                                   []task.Action
	decisions                                 []task.PendingDecision
}

type projectWorkItemTaskServiceProbe struct {
	api.TaskService
	details app.TaskDetails
	err     error
	userID  string
	taskID  string
	calls   int
}

func (s *projectWorkItemTaskServiceProbe) Get(_ context.Context, principal identity.Principal, taskID string) (app.TaskDetails, error) {
	s.userID, s.taskID, s.calls = principal.UserID, taskID, s.calls+1
	return s.details, s.err
}

type projectWorkItemWorkspaceReadProbe struct {
	page        app.ProjectWorkspaceFilePage
	err         error
	projectID   string
	workspaceID string
	revision    int64
	principalID string
	calls       int
}

func (s *projectWorkItemWorkspaceReadProbe) GetWorkspaceManifest(_ context.Context, principal identity.Principal, projectID, workspaceID string,
	revision int64, _, _ string, _ int32) (app.ProjectWorkspaceFilePage, error) {
	s.projectID, s.workspaceID, s.revision, s.principalID = projectID, workspaceID, revision, principal.UserID
	s.calls++
	return s.page, s.err
}

type projectWorkItemSavedProofProbe struct {
	err         error
	projectID   string
	evidence    project.WorkItemDelivery
	principalID string
	calls       int
}

type projectMCPConnectorPortalStoreProbe struct {
	item      app.ProjectMCPConnector
	actorID   string
	projectID string
}

func (s *projectMCPConnectorPortalStoreProbe) ListProjectMCPConnectors(_ context.Context, actorID, projectID string) ([]app.ProjectMCPConnector, error) {
	s.actorID, s.projectID = actorID, projectID
	return []app.ProjectMCPConnector{s.item}, nil
}

func (s *projectMCPConnectorPortalStoreProbe) GrantProjectMCPConnector(context.Context, string, string, string, int64) (app.ProjectMCPConnector, error) {
	return s.item, nil
}

func (s *projectMCPConnectorPortalStoreProbe) RevokeProjectMCPConnector(context.Context, string, string, string, int64) (app.ProjectMCPConnector, error) {
	return s.item, nil
}

func (s *projectMCPConnectorPortalStoreProbe) AuthorizeProjectMCPConnector(context.Context, string, string, string, string, int64) error {
	return nil
}

type projectMCPConnectorPortalAccessProbe struct {
	role        project.Role
	minimumRole project.Role
	calls       int
}

func (s *projectMCPConnectorPortalAccessProbe) RequireAccess(_ context.Context, principal identity.Principal, projectID string, minimum project.Role) (project.Member, error) {
	s.calls, s.minimumRole = s.calls+1, minimum
	return project.Member{ProjectID: projectID, UserID: principal.UserID, Role: s.role, Version: 1}, nil
}

type projectMCPConnectorPortalHostProbe struct{ app.HostService }

func (projectMCPConnectorPortalHostProbe) AuthorizeTLS(context.Context, string) bool { return false }

type projectMCPConnectorPortalVerifierProbe struct {
	app.Verifier
	principal identity.Principal
}

func (s projectMCPConnectorPortalVerifierProbe) VerifySession(context.Context, string) (identity.Principal, error) {
	return s.principal, nil
}

func TestProjectMCPConnectorServiceIsAvailableThroughPortalAPI(t *testing.T) {
	now := time.Now().UTC()
	principal := identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
	store := &projectMCPConnectorPortalStoreProbe{item: app.ProjectMCPConnector{
		Grant: project.MCPConnectorGrant{ProjectID: "j7654321", ConnectorOwnerID: principal.UserID, ConnectorID: "k1234567",
			Version: 1, AuthorizedBy: principal.UserID, Active: true, CreatedAt: now, UpdatedAt: now},
		Name: "Calendar", Enabled: true, ConnectorRevision: 2,
	}}
	access := &projectMCPConnectorPortalAccessProbe{role: project.RoleViewer}
	service, err := app.NewProjectMCPConnectorService(store, access)
	if err != nil {
		t.Fatal(err)
	}
	portalCredential, _, err := identity.NewCredential(identity.KindSession, principal.TokenID)
	if err != nil {
		t.Fatal(err)
	}
	hosts := projectMCPConnectorPortalHostProbe{}
	router, err := api.New(api.Deps{
		Hosts: hosts, TLSAuthorizer: hosts, Verifier: projectMCPConnectorPortalVerifierProbe{principal: principal},
		Auth: authAdapter{}, ProjectMCPConnectors: service,
	}, api.Options{
		Logger: zap.NewNop(), Metrics: prometheus.NewRegistry(), APIMaxBodyBytes: 1 << 20, MCPMaxBodyBytes: 1 << 20,
		SessionTTL: time.Hour, HostScheme: "https", PortalDomain: "portal.example.test", HostSuffix: "example-hosts.test",
		PreviewPortMin: 1024,
	})
	if err != nil {
		t.Fatalf("initialize Portal API with project MCP connectors: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/projects/j7654321/mcp-connectors", nil)
	request.AddCookie(&http.Cookie{Name: "ch_portal", Value: portalCredential})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"Calendar"`) ||
		strings.Contains(response.Body.String(), "credential") || store.actorID != principal.UserID || store.projectID != "j7654321" ||
		access.calls != 1 || access.minimumRole != project.RoleViewer {
		t.Fatalf("project connector API route = %d %s; store=%#v access=%#v", response.Code, response.Body, store, access)
	}
}

func (s *projectWorkItemSavedProofProbe) AuthorizeSavedProjectDelivery(_ context.Context, principal identity.Principal, projectID string,
	evidence project.WorkItemDelivery) error {
	s.projectID, s.evidence, s.principalID = projectID, evidence, principal.UserID
	s.calls++
	return s.err
}

func TestProjectWorkItemTaskLinkAuthorizerChecksTaskAccessAndProject(t *testing.T) {
	ctx := context.Background()
	principal := identity.Principal{UserID: "u1234567"}
	authorizer := projectWorkItemTaskLinkAuthorizer{tasks: &projectWorkItemTaskServiceProbe{
		details: app.TaskDetails{Task: task.Task{ID: "q1234567", ProjectID: "j7654321"}},
	}}
	if err := authorizer.AuthorizeProjectTaskLink(ctx, principal, "j7654321", "q1234567"); err != nil {
		t.Fatalf("authorized project Task link rejected: %v", err)
	}
	if err := authorizer.AuthorizeProjectTaskLink(ctx, principal, "j0000000", "q1234567"); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("cross-project Task link = %v, want forbidden", err)
	}
	underlying := errors.New("Task access revoked")
	if err := (projectWorkItemTaskLinkAuthorizer{tasks: &projectWorkItemTaskServiceProbe{err: underlying}}).
		AuthorizeProjectTaskLink(ctx, principal, "j7654321", "q1234567"); !errors.Is(err, underlying) {
		t.Fatalf("Task authorization error = %v, want original error", err)
	}
}

func TestProjectWorkItemDeliveryAuthorizerRequiresTaskAndWorkspaceReadAccess(t *testing.T) {
	ctx := context.Background()
	principal := identity.Principal{UserID: "u1234567"}
	revision := int64(7)
	evidence := project.WorkItemDelivery{TaskID: "q1234567", WorkspaceID: "w1234567", Revision: revision}
	taskProbe := &projectWorkItemTaskServiceProbe{details: app.TaskDetails{Task: task.Task{
		ID: evidence.TaskID, ProjectID: "j7654321", WorkspaceID: evidence.WorkspaceID, WorkspaceCheckpointRevision: &revision,
	}}}
	workspaceProbe := &projectWorkItemWorkspaceReadProbe{page: app.ProjectWorkspaceFilePage{
		ProjectID: "j7654321", WorkspaceID: evidence.WorkspaceID, Revision: evidence.Revision,
	}}
	savedProbe := &projectWorkItemSavedProofProbe{}
	authorizer := projectWorkItemDeliveryAuthorizer{tasks: taskProbe, projects: workspaceProbe, savedProof: savedProbe}

	if err := authorizer.AuthorizeSavedProjectDelivery(ctx, principal, "j7654321", evidence); err != nil {
		t.Fatalf("authorized delivery rejected: %v", err)
	}
	if taskProbe.calls != 1 || taskProbe.userID != principal.UserID || taskProbe.taskID != evidence.TaskID ||
		workspaceProbe.calls != 1 || workspaceProbe.principalID != principal.UserID || workspaceProbe.projectID != "j7654321" ||
		workspaceProbe.workspaceID != evidence.WorkspaceID || workspaceProbe.revision != evidence.Revision ||
		savedProbe.calls != 1 || savedProbe.principalID != principal.UserID || savedProbe.projectID != "j7654321" || savedProbe.evidence != evidence {
		t.Fatalf("delivery checks were not bound to caller/evidence: task=%#v workspace=%#v saved=%#v", taskProbe, workspaceProbe, savedProbe)
	}

	for _, tamper := range []func(*task.Task){
		func(value *task.Task) { value.ID = "q7654321" },
		func(value *task.Task) { value.ProjectID = "j0000000" },
		func(value *task.Task) { value.WorkspaceID = "w7654321" },
		func(value *task.Task) { value.WorkspaceCheckpointRevision = nil },
		func(value *task.Task) { other := revision + 1; value.WorkspaceCheckpointRevision = &other },
	} {
		original := taskProbe.details.Task
		tamper(&taskProbe.details.Task)
		if err := authorizer.AuthorizeSavedProjectDelivery(ctx, principal, "j7654321", evidence); domain.CodeOf(err) != domain.CodeWorkspaceConflict ||
			workspaceProbe.calls != 1 || savedProbe.calls != 1 {
			t.Fatalf("mismatched Task evidence error=%v; workspace=%d saved=%d", err, workspaceProbe.calls, savedProbe.calls)
		}
		taskProbe.details.Task = original
	}
	workspaceProbe.page.Revision++
	if err := authorizer.AuthorizeSavedProjectDelivery(ctx, principal, "j7654321", evidence); domain.CodeOf(err) != domain.CodeWorkspaceConflict || savedProbe.calls != 1 {
		t.Fatalf("unreadable Workspace revision error=%v; saved=%d", err, savedProbe.calls)
	}

	denied := errors.New("Task read permission revoked")
	taskProbe.err = denied
	workspaceProbe.page.Revision = revision
	if err := authorizer.AuthorizeSavedProjectDelivery(ctx, principal, "j7654321", evidence); !errors.Is(err, denied) || workspaceProbe.calls != 2 || savedProbe.calls != 1 {
		t.Fatalf("Task read denial error=%v; workspace=%d saved=%d", err, workspaceProbe.calls, savedProbe.calls)
	}
	taskProbe.err = nil
	denied = errors.New("Workspace read permission revoked")
	workspaceProbe.err = denied
	if err := authorizer.AuthorizeSavedProjectDelivery(ctx, principal, "j7654321", evidence); !errors.Is(err, denied) || savedProbe.calls != 1 {
		t.Fatalf("Workspace read denial error=%v; saved=%d", err, savedProbe.calls)
	}
}

func (s *cloudDSHTaskServiceProbe) Submit(context.Context, identity.Principal, app.SubmitTaskCommand) (app.TaskSubmissionResult, error) {
	s.submits++
	return app.TaskSubmissionResult{}, nil
}

func (s *cloudDSHTaskServiceProbe) AppendMessage(context.Context, identity.Principal, string, app.AppendMessageCommand) (app.TaskSubmissionResult, error) {
	s.appends++
	return app.TaskSubmissionResult{}, nil
}

func (s *cloudDSHTaskServiceProbe) List(context.Context, identity.Principal, app.TaskListRequest) (app.TaskPage, error) {
	s.lists++
	return app.TaskPage{}, nil
}

func (s *cloudDSHTaskServiceProbe) RequestAction(_ context.Context, _ identity.Principal, _ string, action task.Action, _ task.Status, _ string) (app.TaskActionResult, error) {
	s.actions = append(s.actions, action)
	return app.TaskActionResult{}, nil
}

func (s *cloudDSHTaskServiceProbe) RetryBatchItem(context.Context, identity.Principal, string, string, string) error {
	s.retries++
	return nil
}

func (s *cloudDSHTaskServiceProbe) RespondToPendingRequest(_ context.Context, _ identity.Principal, _, _ string, _ int64, decision task.PendingDecision, _ string) error {
	s.decisions = append(s.decisions, decision)
	return nil
}

func (s *cloudDSHTaskServiceProbe) AnswerClarificationRequest(context.Context, identity.Principal, string, string, int64, string, string) error {
	s.answers++
	return nil
}

type cloudDSHScheduleServiceProbe struct {
	api.ScheduleService
	creates, lists, actions, processed int
	lastAction                         schedule.Action
}

func (s *cloudDSHScheduleServiceProbe) Create(context.Context, identity.Principal, app.CreateScheduleCommand, string) (schedule.Schedule, error) {
	s.creates++
	return schedule.Schedule{}, nil
}

func (s *cloudDSHScheduleServiceProbe) List(context.Context, identity.Principal, schedule.Status, app.PageRequest) (app.SchedulePage, error) {
	s.lists++
	return app.SchedulePage{}, nil
}

func (s *cloudDSHScheduleServiceProbe) Action(_ context.Context, _ identity.Principal, _ string, command app.ScheduleActionCommand, _ string) (app.ScheduleActionResult, error) {
	s.actions++
	s.lastAction = command.Action
	return app.ScheduleActionResult{}, nil
}

func (s *cloudDSHScheduleServiceProbe) ProcessDue(context.Context, time.Time, int32) (int, error) {
	s.processed++
	return 0, nil
}

type cloudDSHInstantChatServiceProbe struct {
	api.InstantChatService
	lists, messages, sends int
}

func (s *cloudDSHInstantChatServiceProbe) List(context.Context, identity.Principal) ([]app.InstantChatConversation, error) {
	s.lists++
	return nil, nil
}

func (s *cloudDSHInstantChatServiceProbe) Messages(context.Context, identity.Principal, string) ([]app.InstantChatMessage, error) {
	s.messages++
	return nil, nil
}

func (s *cloudDSHInstantChatServiceProbe) Send(context.Context, identity.Principal, app.InstantChatSend) (app.InstantChatReply, error) {
	s.sends++
	return app.InstantChatReply{}, nil
}

func TestCloudDSHDisabledKeepsReadsAndRejectsNewExecution(t *testing.T) {
	tasks := &cloudDSHTaskServiceProbe{}
	schedules := &cloudDSHScheduleServiceProbe{}
	instantChat := &cloudDSHInstantChatServiceProbe{}
	services := cloudDSHAdaptersForRuntime(false, tasks, schedules, instantChat, schedules)
	if services.scheduleProcessor != nil {
		t.Fatal("disabled CloudDSH must not start schedule processing")
	}

	principal := identity.Principal{}
	if _, err := services.tasks.List(context.Background(), principal, app.TaskListRequest{}); err != nil || tasks.lists != 1 {
		t.Fatalf("task history read = %v, list calls = %d", err, tasks.lists)
	}
	if _, err := services.tasks.Submit(context.Background(), principal, app.SubmitTaskCommand{}); domain.CodeOf(err) != domain.CodeHostNotReady || tasks.submits != 0 {
		t.Fatalf("task submit = %v, underlying calls = %d", err, tasks.submits)
	}
	if _, err := services.tasks.AppendMessage(context.Background(), principal, "c1234567", app.AppendMessageCommand{}); domain.CodeOf(err) != domain.CodeHostNotReady || tasks.appends != 0 {
		t.Fatalf("task message append = %v, underlying calls = %d", err, tasks.appends)
	}
	if _, err := services.tasks.RequestAction(context.Background(), principal, "t1234567", task.ActionResume, task.StatusPaused, "resume-key"); domain.CodeOf(err) != domain.CodeHostNotReady || len(tasks.actions) != 0 {
		t.Fatalf("task resume = %v, underlying actions = %v", err, tasks.actions)
	}
	if _, err := services.tasks.RequestAction(context.Background(), principal, "t1234567", task.ActionCancel, task.StatusQueued, "cancel-key"); err != nil || len(tasks.actions) != 1 || tasks.actions[0] != task.ActionCancel {
		t.Fatalf("task cancel = %v, underlying actions = %v", err, tasks.actions)
	}
	if err := services.tasks.RetryBatchItem(context.Background(), principal, "t1234567", "b1234567", "retry-key"); domain.CodeOf(err) != domain.CodeHostNotReady || tasks.retries != 0 {
		t.Fatalf("batch retry = %v, underlying calls = %d", err, tasks.retries)
	}
	if err := services.tasks.RespondToPendingRequest(context.Background(), principal, "t1234567", "p1234567", 1, task.PendingDecisionApprove, "approve-key"); domain.CodeOf(err) != domain.CodeHostNotReady || len(tasks.decisions) != 0 {
		t.Fatalf("pending approval = %v, underlying decisions = %v", err, tasks.decisions)
	}
	if err := services.tasks.RespondToPendingRequest(context.Background(), principal, "t1234567", "p1234567", 1, task.PendingDecisionReject, "reject-key"); err != nil || len(tasks.decisions) != 1 || tasks.decisions[0] != task.PendingDecisionReject {
		t.Fatalf("pending rejection = %v, underlying decisions = %v", err, tasks.decisions)
	}
	if err := services.tasks.AnswerClarificationRequest(context.Background(), principal, "t1234567", "p1234567", 1, "answer", "answer-key"); domain.CodeOf(err) != domain.CodeHostNotReady || tasks.answers != 0 {
		t.Fatalf("clarification answer = %v, underlying calls = %d", err, tasks.answers)
	}

	if _, err := services.schedules.List(context.Background(), principal, "", app.PageRequest{}); err != nil || schedules.lists != 1 {
		t.Fatalf("schedule history read = %v, list calls = %d", err, schedules.lists)
	}
	if _, err := services.schedules.Create(context.Background(), principal, app.CreateScheduleCommand{}, "schedule-key"); domain.CodeOf(err) != domain.CodeHostNotReady || schedules.creates != 0 {
		t.Fatalf("schedule create = %v, underlying calls = %d", err, schedules.creates)
	}
	if _, err := services.schedules.Action(context.Background(), principal, "s1234567", app.ScheduleActionCommand{Action: schedule.ActionEnable}, "enable-key"); domain.CodeOf(err) != domain.CodeHostNotReady || schedules.actions != 0 {
		t.Fatalf("schedule enable = %v, underlying calls = %d", err, schedules.actions)
	}
	if _, err := services.schedules.Action(context.Background(), principal, "s1234567", app.ScheduleActionCommand{Action: schedule.ActionDisable}, "disable-key"); err != nil || schedules.actions != 1 || schedules.lastAction != schedule.ActionDisable {
		t.Fatalf("schedule disable = %v, underlying calls/action = %d/%s", err, schedules.actions, schedules.lastAction)
	}

	if _, err := services.instantChat.List(context.Background(), principal); err != nil || instantChat.lists != 1 {
		t.Fatalf("instant chat history read = %v, list calls = %d", err, instantChat.lists)
	}
	if _, err := services.instantChat.Messages(context.Background(), principal, "c1234567"); err != nil || instantChat.messages != 1 {
		t.Fatalf("instant chat messages read = %v, calls = %d", err, instantChat.messages)
	}
	if _, err := services.instantChat.Send(context.Background(), principal, app.InstantChatSend{}); domain.CodeOf(err) != domain.CodeHostNotReady || instantChat.sends != 0 {
		t.Fatalf("instant chat send = %v, underlying calls = %d", err, instantChat.sends)
	}
}

func TestCloudDSHEnabledKeepsExecutionAdapters(t *testing.T) {
	tasks := &cloudDSHTaskServiceProbe{}
	schedules := &cloudDSHScheduleServiceProbe{}
	instantChat := &cloudDSHInstantChatServiceProbe{}
	services := cloudDSHAdaptersForRuntime(true, tasks, schedules, instantChat, schedules)
	if services.tasks != tasks || services.schedules != schedules || services.instantChat != instantChat || services.scheduleProcessor != schedules {
		t.Fatal("enabled CloudDSH must keep the configured task and schedule services")
	}
	if _, err := services.tasks.Submit(context.Background(), identity.Principal{}, app.SubmitTaskCommand{}); err != nil || tasks.submits != 1 {
		t.Fatalf("enabled task submit = %v, underlying calls = %d", err, tasks.submits)
	}
}

func TestRequireLoopbackMetrics(t *testing.T) {
	if err := requireLoopbackMetrics("127.0.0.1:7082"); err != nil {
		t.Fatalf("loopback metrics address rejected: %v", err)
	}
	for _, address := range []string{"0.0.0.0:7082", "localhost:7082", "[::1]:7082", "bad"} {
		if err := requireLoopbackMetrics(address); err == nil {
			t.Errorf("requireLoopbackMetrics(%q) accepted a non-127.0.0.1 address", address)
		}
	}
}

func TestReadProtectedKeyRequiresPrivateRegularFile(t *testing.T) {
	directory := t.TempDir()
	keyPath := filepath.Join(directory, "supervisor.key")
	key := bytes.Repeat([]byte{0x42}, 32)
	if err := os.WriteFile(keyPath, key, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readProtectedKey(keyPath)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatalf("readProtectedKey() = %x, %v", got, err)
	}

	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readProtectedKey(keyPath); err == nil || !strings.Contains(err.Error(), "no group or other permissions") {
		t.Fatalf("readProtectedKey(permissive file) error = %v", err)
	}

	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(directory, "key-link")
	if err := os.Symlink(keyPath, symlinkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := readProtectedKey(symlinkPath); err == nil {
		t.Fatal("readProtectedKey accepted a symlink")
	}
}

func TestCheckNetworkPoliciesPropagatesGuardFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "network-policy.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := checkNetworkPolicies(context.Background(), map[string]config.Profile{
		"container": {BaseURL: "http://127.0.0.1:18080", NetworkGuardPath: path},
	})
	if !errors.Is(err, app.ErrSandboxUnavailable) || !strings.Contains(err.Error(), "container") {
		t.Fatalf("checkNetworkPolicies() = %v, want container guard failure", err)
	}
}

func TestMetricsHandlerOnlyExposesMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	counter := prometheus.NewCounter(prometheus.CounterOpts{Name: "portal_test_counter", Help: "test"})
	if err := registry.Register(counter); err != nil {
		t.Fatal(err)
	}
	handler := metricsHandler(registry)
	for _, test := range []struct {
		path string
		code int
	}{{path: "/metrics", code: http.StatusOK}, {path: "/", code: http.StatusNotFound}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.code {
			t.Errorf("GET %s = %d, want %d", test.path, response.Code, test.code)
		}
	}
}

func TestPortalCommandUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"migrate"}, {"user"}, {"user", "add"}} {
		var stdout, stderr bytes.Buffer
		if err := run(context.Background(), args, strings.NewReader("pw\n"), &stdout, &stderr); err == nil {
			t.Errorf("run(%q) returned nil error", args)
		}
	}
}

func TestWorkspaceVerifyRejectsUnexpectedArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"workspace", "verify", "unexpected"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments: unexpected") {
		t.Fatalf("workspace verify with unexpected argument error = %v", err)
	}
}

func TestUserAddCommandIsRemoved(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"user", "add"}, strings.NewReader(""), &stdout, &stderr); err == nil {
		t.Fatal("removed user add command was accepted")
	}
}

func TestParseWorkspaceImportFlags(t *testing.T) {
	for _, test := range []struct {
		name        string
		args        []string
		wantID      string
		wantConfig  string
		wantErrPart string
	}{
		{name: "valid", args: []string{"--workspace", "w1234567", "--config", "/tmp/portal.yaml"}, wantID: "w1234567", wantConfig: "/tmp/portal.yaml"},
		{name: "missing workspace", args: []string{"--config", "/tmp/portal.yaml"}, wantErrPart: "--workspace"},
		{name: "invalid workspace", args: []string{"--workspace", "../other"}, wantErrPart: "valid workspace ID"},
		{name: "unexpected argument", args: []string{"--workspace", "w1234567", "extra"}, wantErrPart: "unexpected arguments"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workspaceID, configPath, err := parseWorkspaceImportFlags(test.args, &bytes.Buffer{})
			if test.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrPart) {
					t.Fatalf("parseWorkspaceImportFlags() error = %v, want substring %q", err, test.wantErrPart)
				}
				return
			}
			if err != nil || workspaceID != test.wantID || configPath != test.wantConfig {
				t.Fatalf("parseWorkspaceImportFlags() = (%q, %q, %v), want (%q, %q, nil)", workspaceID, configPath, err, test.wantID, test.wantConfig)
			}
		})
	}
}
