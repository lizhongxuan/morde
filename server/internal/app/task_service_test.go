package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type taskStoreStub struct {
	TaskStore
	conversations        map[string]conversation.Conversation
	submissions          []TaskSubmission
	appends              []TaskSubmission
	created              []conversation.Conversation
	listHostIDs          []string
	listTasksPage        TaskPage
	listConvPage         ConversationPage
	messagePage          MessagePage
	details              TaskDetails
	retryRecord          IdempotencyRecord
	retryTaskID          string
	retryItemID          string
	retryCalls           int
	retryRecords         []IdempotencyRecord
	pendingRecord        IdempotencyRecord
	pendingTaskID        string
	pendingID            string
	pendingVersion       int64
	pendingDecision      task.PendingDecision
	pendingCalls         int
	clarificationRecord  IdempotencyRecord
	clarificationTaskID  string
	clarificationID      string
	clarificationVersion int64
	clarificationAnswer  string
	clarificationCalls   int
	actionResult         TaskActionResult
	actionRecord         IdempotencyRecord
	actionTaskID         string
	action               task.Action
	actionExpected       task.Status
	actionRequestID      string
	actionCalls          int
	events               []task.Event
	eventPage            *TaskEventPage
	storeErr             error
}

func (s *taskStoreStub) CreateConversation(_ context.Context, value conversation.Conversation, _ IdempotencyRecord) (conversation.Conversation, bool, error) {
	s.created = append(s.created, value)
	return value, false, s.storeErr
}

func (s *taskStoreStub) GetConversation(_ context.Context, id, ownerID string) (conversation.Conversation, error) {
	value, ok := s.conversations[id]
	if !ok || value.OwnerID != ownerID {
		return conversation.Conversation{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	return value, nil
}

func (s *taskStoreStub) ListConversations(_ context.Context, _ string, hostIDs []string, _ PageRequest) (ConversationPage, error) {
	s.listHostIDs = hostIDs
	return s.listConvPage, s.storeErr
}

func (s *taskStoreStub) ListMessages(context.Context, string, string, PageRequest) (MessagePage, error) {
	return s.messagePage, s.storeErr
}

func (s *taskStoreStub) SubmitTask(_ context.Context, submission TaskSubmission) (TaskSubmissionResult, error) {
	s.submissions = append(s.submissions, submission)
	if s.storeErr != nil {
		return TaskSubmissionResult{}, s.storeErr
	}
	return TaskSubmissionResult{
		Conversation: conversation.Conversation{ID: submission.Task.ConversationID, OwnerID: submission.Task.OwnerID, HostID: submission.Task.HostID},
		Message:      submission.Message, Task: submission.Task,
	}, nil
}

func (s *taskStoreStub) AppendMessage(_ context.Context, submission TaskSubmission) (TaskSubmissionResult, error) {
	s.appends = append(s.appends, submission)
	if s.storeErr != nil {
		return TaskSubmissionResult{}, s.storeErr
	}
	return TaskSubmissionResult{Task: submission.Task, Message: submission.Message}, nil
}

func (s *taskStoreStub) GetTask(context.Context, string, string) (TaskDetails, error) {
	return s.details, s.storeErr
}

func (s *taskStoreStub) ListTasks(_ context.Context, _ string, filter TaskListRequest) (TaskPage, error) {
	s.listHostIDs = filter.HostIDs
	return s.listTasksPage, s.storeErr
}

func (s *taskStoreStub) ListTaskEvents(context.Context, string, string, int64, int32) ([]task.Event, error) {
	return s.events, s.storeErr
}

func (s *taskStoreStub) GetTaskEventPage(_ context.Context, _ string, _ string, _ int64, _ int32) (TaskEventPage, error) {
	if s.eventPage != nil {
		return *s.eventPage, s.storeErr
	}
	status := s.details.Task.Status
	if status == "" {
		status = task.StatusQueued
	}
	var current int64
	if len(s.events) > 0 {
		current = s.events[len(s.events)-1].Sequence
	}
	return TaskEventPage{HostID: s.details.Task.HostID, Status: status, CurrentSequence: current, Items: s.events}, s.storeErr
}

func (s *taskStoreStub) RetryFailedBatchItem(_ context.Context, record IdempotencyRecord, taskID, batchItemID string) error {
	s.retryCalls++
	s.retryRecord, s.retryTaskID, s.retryItemID = record, taskID, batchItemID
	s.retryRecords = append(s.retryRecords, record)
	return s.storeErr
}

func (s *taskStoreStub) RespondToPendingRequest(_ context.Context, record IdempotencyRecord, taskID, pendingID string, version int64, decision task.PendingDecision) error {
	s.pendingCalls++
	s.pendingRecord, s.pendingTaskID, s.pendingID, s.pendingVersion, s.pendingDecision = record, taskID, pendingID, version, decision
	return s.storeErr
}

func (s *taskStoreStub) AnswerClarificationRequest(_ context.Context, record IdempotencyRecord, taskID, clarificationID string, version int64, answer string) error {
	s.clarificationCalls++
	s.clarificationRecord, s.clarificationTaskID, s.clarificationID, s.clarificationVersion, s.clarificationAnswer = record, taskID, clarificationID, version, answer
	return s.storeErr
}

func (s *taskStoreStub) RequestTaskAction(_ context.Context, record IdempotencyRecord, taskID string, action task.Action, expected task.Status, requestID string) (TaskActionResult, error) {
	s.actionCalls++
	s.actionRecord, s.actionTaskID, s.action, s.actionExpected, s.actionRequestID = record, taskID, action, expected, requestID
	if s.storeErr != nil {
		return TaskActionResult{}, s.storeErr
	}
	return s.actionResult, nil
}

type taskAuthorizerStub struct {
	err        error
	hostErrors map[string]error
	calls      int
	lastHost   string
	lastScope  identity.Scope
}

func (s *taskAuthorizerStub) AuthorizeTaskAction(_ context.Context, _ string, _ identity.Kind, _ string, hostID string, _ []identity.Scope, scope identity.Scope) error {
	s.calls++
	s.lastHost, s.lastScope = hostID, scope
	if err, ok := s.hostErrors[hostID]; ok {
		return err
	}
	return s.err
}

type modelCatalogStub struct {
	configured bool
	err        error
	ownerID    string
	modelID    string
}

func (s *modelCatalogStub) IsConfiguredModel(_ context.Context, ownerID, modelID string) (bool, error) {
	s.ownerID, s.modelID = ownerID, modelID
	return s.configured, s.err
}

func TestTaskServiceSubmitCreatesHostBoundConversationAndIdempotencyHash(t *testing.T) {
	store := &taskStoreStub{}
	authorizer := &taskAuthorizerStub{}
	models := &modelCatalogStub{configured: true}
	service, err := NewTaskService(store, authorizer, models)
	if err != nil {
		t.Fatal(err)
	}
	principal := taskTestPrincipal()
	command := SubmitTaskCommand{
		HostID: "h1234567", ModelID: " model-a ", Objective: "review this project",
		IdempotencyKey: "retry-key-001", BatchItems: []BatchItemInput{},
	}
	first, err := service.Submit(context.Background(), principal, command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Submit(context.Background(), principal, command)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.submissions) != 2 {
		t.Fatalf("SubmitTask calls = %d, want 2", len(store.submissions))
	}
	a, b := store.submissions[0], store.submissions[1]
	if a.Conversation == nil || a.Task.ConversationID != a.Conversation.ID || a.Conversation.HostID != a.Task.HostID || a.Message.ConversationID != a.Task.ConversationID {
		t.Fatalf("new conversation is not Host-bound: %#v", a)
	}
	if a.Task.ModelID != "model-a" || a.Task.Status != task.StatusQueued || a.Task.Budget != (task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000}) {
		t.Fatalf("task normalization = %#v", a.Task)
	}
	if a.InitialEvent.Type != "task.status" || !json.Valid(a.InitialEvent.Payload) || a.Idempotency.Operation != "task.submit" || !bytes.Equal(a.Idempotency.RequestHash, b.Idempotency.RequestHash) {
		t.Fatalf("submission event/hash is invalid: %#v", a)
	}
	if first.Task.ID == "" || second.Task.ID == "" || models.ownerID != principal.UserID || models.modelID != "model-a" || authorizer.lastScope != identity.ScopeTaskSubmit {
		t.Fatalf("missing owner/model/authorization validation: first=%#v second=%#v model=%#v auth=%#v", first, second, models, authorizer)
	}
	if a.Task.Authorization.Kind != string(identity.KindSession) || a.Task.Authorization.TokenID != principal.TokenID {
		t.Fatalf("authorization snapshot = %#v", a.Task.Authorization)
	}
}

func TestTaskServiceSubmitCapturesClientInstallationAuthorization(t *testing.T) {
	store := &taskStoreStub{}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := taskTestPrincipal()
	principal.Kind = identity.KindClient
	principal.TokenID = "t7654321"
	principal.ClientInstallationID = "i1234567"
	if _, err := service.Submit(context.Background(), principal, SubmitTaskCommand{
		HostID: "h1234567", ModelID: "model-a", Objective: "review this project", IdempotencyKey: "client-submit-001",
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.submissions) != 1 {
		t.Fatalf("SubmitTask calls = %d, want 1", len(store.submissions))
	}
	authorization := store.submissions[0].Task.Authorization
	if authorization.Kind != string(identity.KindClient) || authorization.TokenID != principal.TokenID || authorization.ClientInstallationID != principal.ClientInstallationID {
		t.Fatalf("client authorization snapshot = %#v", authorization)
	}
}

func TestTaskServiceApplicationTargetIsValidatedAndBoundToTaskHash(t *testing.T) {
	store := &taskStoreStub{}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	command := SubmitTaskCommand{HostID: "h1234567", ModelID: "model-a", Objective: "find a message", IdempotencyKey: "app-target-001"}
	if _, err := service.Submit(context.Background(), taskTestPrincipal(), command); err != nil {
		t.Fatal(err)
	}
	command.ApplicationID = WechatApplicationID
	if _, err := service.Submit(context.Background(), taskTestPrincipal(), command); err != nil {
		t.Fatal(err)
	}
	if store.submissions[1].Task.ApplicationID != WechatApplicationID ||
		bytes.Equal(store.submissions[0].Idempotency.RequestHash, store.submissions[1].Idempotency.RequestHash) {
		t.Fatalf("application target was not persisted or hashed: %#v", store.submissions)
	}
	command.ApplicationID = "unknown"
	if _, err := service.Submit(context.Background(), taskTestPrincipal(), command); domain.CodeOf(err) != domain.CodeInvalidArgument || len(store.submissions) != 2 {
		t.Fatalf("unknown application error=%v, submissions=%d", err, len(store.submissions))
	}
}

func TestTaskServiceAppendMessageBindsApplicationTarget(t *testing.T) {
	store := &taskStoreStub{conversations: map[string]conversation.Conversation{
		"c1234567": {ID: "c1234567", OwnerID: taskTestPrincipal().UserID, HostID: "h1234567"},
	}}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	command := AppendMessageCommand{HostID: "h1234567", ApplicationID: WechatApplicationID, ModelID: "model-a", Content: "continue", IdempotencyKey: "app-append-001"}
	if _, err := service.AppendMessage(context.Background(), taskTestPrincipal(), "c1234567", command); err != nil {
		t.Fatal(err)
	}
	if len(store.appends) != 1 || store.appends[0].Task.ApplicationID != WechatApplicationID {
		t.Fatalf("append application target = %#v", store.appends)
	}
	command.ApplicationID = "unknown"
	if _, err := service.AppendMessage(context.Background(), taskTestPrincipal(), "c1234567", command); domain.CodeOf(err) != domain.CodeInvalidArgument || len(store.appends) != 1 {
		t.Fatalf("unknown application error=%v, appends=%d", err, len(store.appends))
	}
}

func TestInitialAgentInputsKeepApplicationTargetAcrossBatchAndRecovery(t *testing.T) {
	current := task.Task{ID: "q1234567", HostID: "h1234567", ApplicationID: WechatApplicationID, Objective: "draft a reply"}
	plain := initialAgentInputs(current, nil)
	batch := initialAgentInputs(current, &agentBatchScope{Item: task.BatchItem{Key: "first", Input: "look up contact"}})
	for _, inputs := range [][]ModelInput{plain, batch} {
		if len(inputs) != 1 || !strings.Contains(inputs[0].Text, "WeChat (wechat) on Host h1234567") || !strings.Contains(inputs[0].Text, "draft a reply") {
			t.Fatalf("task target is missing from model input: %#v", inputs)
		}
	}
	untargeted := current
	untargeted.ApplicationID = ""
	if got := initialAgentInputs(untargeted, nil); len(got) != 1 || got[0].Text != current.Objective {
		t.Fatalf("untargeted task input changed: %#v", got)
	}
}

func TestTaskServiceRejectsUnconfiguredModelBeforeSubmitting(t *testing.T) {
	store := &taskStoreStub{}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Submit(context.Background(), taskTestPrincipal(), SubmitTaskCommand{
		HostID: "h1234567", ModelID: "unknown", Objective: "do work", IdempotencyKey: "retry-key-002",
	})
	if domain.CodeOf(err) != domain.CodeInvalidArgument || len(store.submissions) != 0 {
		t.Fatalf("unconfigured model error=%v submissions=%d", err, len(store.submissions))
	}
}

func TestTaskServiceRejectsUnscopedAPIKeyBeforeStoreWrite(t *testing.T) {
	store := &taskStoreStub{}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567",
		HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeTaskRead},
	}
	_, err = service.Submit(context.Background(), principal, SubmitTaskCommand{
		HostID: "h1234567", ModelID: "model-a", Objective: "do work", IdempotencyKey: "retry-key-003",
	})
	if domain.CodeOf(err) != domain.CodeForbidden || len(store.submissions) != 0 {
		t.Fatalf("unscoped API key error=%v submissions=%d", err, len(store.submissions))
	}
}

func TestTaskServiceAppendMessageRejectsConversationHostMismatch(t *testing.T) {
	store := &taskStoreStub{conversations: map[string]conversation.Conversation{
		"c1234567": {ID: "c1234567", OwnerID: "u1234567", HostID: "h7654321"},
	}}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.AppendMessage(context.Background(), taskTestPrincipal(), "c1234567", AppendMessageCommand{
		HostID: "h1234567", ModelID: "model-a", Content: "continue", IdempotencyKey: "retry-key-004",
	})
	if domain.CodeOf(err) != domain.CodeInvalidState || len(store.appends) != 0 {
		t.Fatalf("host mismatch error=%v appends=%d", err, len(store.appends))
	}
}

func TestTaskServiceAppendHidesConversationBoundToUnauthorizedHost(t *testing.T) {
	store := &taskStoreStub{conversations: map[string]conversation.Conversation{
		"c1234567": {ID: "c1234567", OwnerID: "u1234567", HostID: "h7654321"},
	}}
	authorizer := &taskAuthorizerStub{hostErrors: map[string]error{
		"h7654321": domain.NewError(domain.CodeNotFound, "host not found"),
	}}
	service, err := NewTaskService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.AppendMessage(context.Background(), taskTestPrincipal(), "c1234567", AppendMessageCommand{
		HostID: "h1234567", ModelID: "model-a", Content: "continue", IdempotencyKey: "retry-key-012",
	})
	if domain.CodeOf(err) != domain.CodeNotFound || len(store.appends) != 0 || authorizer.lastHost != "h7654321" {
		t.Fatalf("unauthorized conversation host error=%v appends=%d authorized_host=%s", err, len(store.appends), authorizer.lastHost)
	}
}

func TestTaskServiceListPassesEmptyAPIKeyHostScopeAsEmptySlice(t *testing.T) {
	store := &taskStoreStub{}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567",
		Scopes: []identity.Scope{identity.ScopeTaskRead},
	}
	if _, err := service.List(context.Background(), principal, TaskListRequest{}); err != nil {
		t.Fatal(err)
	}
	if store.listHostIDs == nil || len(store.listHostIDs) != 0 {
		t.Fatalf("empty key Host allow-list became unrestricted: %#v", store.listHostIDs)
	}
}

func TestTaskServiceListEventsRejectsInvalidStoredSequence(t *testing.T) {
	store := &taskStoreStub{
		details: TaskDetails{Task: task.Task{ID: "q1234567", OwnerID: "u1234567", HostID: "h1234567"}},
		events:  []task.Event{{TaskID: "q1234567", Sequence: 4, Type: "task.status", Payload: []byte(`{}`), CreatedAt: time.Now()}},
	}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.ListEvents(context.Background(), taskTestPrincipal(), "q1234567", 0, 50)
	if domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("event sequence gap returned %v", err)
	}
	store.events = []task.Event{{TaskID: "q1234567", Sequence: 1, Type: "task.unknown", Payload: []byte(`{}`), CreatedAt: time.Now()}}
	_, err = service.ListEvents(context.Background(), taskTestPrincipal(), "q1234567", 0, 50)
	if domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("unknown event type returned %v", err)
	}
}

func TestTaskServiceListEventsPreservesAndAuthorizesExpiredCursorSnapshot(t *testing.T) {
	store := &taskStoreStub{
		details:   TaskDetails{Task: task.Task{ID: "q1234567", OwnerID: "u1234567", HostID: "h1234567"}},
		eventPage: &TaskEventPage{HostID: "h1234567", Status: task.StatusRunning, CurrentSequence: 9},
		storeErr:  domain.NewError(domain.CodeEventCursorExpired, "event cursor has expired"),
	}
	authorizer := &taskAuthorizerStub{}
	service, err := NewTaskService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}

	page, err := service.ListEvents(context.Background(), taskTestPrincipal(), "q1234567", 3, 50)
	if domain.CodeOf(err) != domain.CodeEventCursorExpired || page.HostID != "h1234567" || page.CurrentSequence != 9 ||
		authorizer.calls != 1 || authorizer.lastHost != "h1234567" || authorizer.lastScope != taskReadScope {
		t.Fatalf("page=%#v err=%v auth=%#v", page, err, authorizer)
	}

	authorizer.err = domain.NewError(domain.CodeForbidden, "task scope is required")
	page, err = service.ListEvents(context.Background(), taskTestPrincipal(), "q1234567", 3, 50)
	if domain.CodeOf(err) != domain.CodeNotFound || page.CurrentSequence != 0 || authorizer.calls != 2 {
		t.Fatalf("unauthorized expired cursor page=%#v err=%v auth=%#v", page, err, authorizer)
	}
}

func TestTaskServiceRetryBatchItemRequiresControlScopeAndUsesStableIdempotency(t *testing.T) {
	store := &taskStoreStub{details: TaskDetails{Task: task.Task{ID: "q1234567", OwnerID: "u1234567", HostID: "h1234567"}}}
	authorizer := &taskAuthorizerStub{}
	service, err := NewTaskService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := taskTestPrincipal()
	for range 2 {
		if err := service.RetryBatchItem(context.Background(), principal, "q1234567", "b1234567", "retry-key-123"); err != nil {
			t.Fatal(err)
		}
	}
	if store.retryCalls != 2 || store.retryTaskID != "q1234567" || store.retryItemID != "b1234567" ||
		store.retryRecord.OwnerID != principal.UserID || store.retryRecord.Operation != "task.batch_item.retry" ||
		store.retryRecord.ResourceID != "q1234567" || store.retryRecord.RelatedID != "b1234567" ||
		authorizer.lastHost != "h1234567" || authorizer.lastScope != identity.ScopeTaskControl {
		t.Fatalf("retry authorization/record mismatch: calls=%d record=%#v auth=%#v", store.retryCalls, store.retryRecord, authorizer)
	}
	if !bytes.Equal(store.retryRecords[0].RequestHash, store.retryRecords[1].RequestHash) {
		t.Fatal("repeating an identical action changed its request hash")
	}
	if err := service.RetryBatchItem(context.Background(), principal, "q1234567", "b8765432", "retry-key-123"); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(store.retryRecords[0].RequestHash, store.retryRecord.RequestHash) {
		t.Fatal("reusing a key for a different batch item produced the same request hash")
	}
}

func TestTaskServiceRetryBatchItemRejectsUnscopedKeyAndHidesUnauthorizedHost(t *testing.T) {
	store := &taskStoreStub{details: TaskDetails{Task: task.Task{ID: "q1234567", OwnerID: "u1234567", HostID: "h1234567"}}}
	authorizer := &taskAuthorizerStub{}
	service, err := NewTaskService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567",
		HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeTaskRead},
	}
	if err := service.RetryBatchItem(context.Background(), principal, "q1234567", "b1234567", "retry-key-124"); domain.CodeOf(err) != domain.CodeNotFound || store.retryCalls != 0 {
		t.Fatalf("unscoped retry err=%v writes=%d", err, store.retryCalls)
	}
	principal = taskTestPrincipal()
	authorizer.hostErrors = map[string]error{"h1234567": domain.NewError(domain.CodeNotFound, "host not found")}
	if err := service.RetryBatchItem(context.Background(), principal, "q1234567", "b1234567", "retry-key-124"); domain.CodeOf(err) != domain.CodeNotFound || store.retryCalls != 0 {
		t.Fatalf("unauthorized retry err=%v writes=%d", err, store.retryCalls)
	}
}

func TestTaskServiceRespondToPendingRequiresControlAndUsesVersionedIdempotency(t *testing.T) {
	store := &taskStoreStub{details: TaskDetails{Task: task.Task{ID: "q1234567", OwnerID: "u1234567", HostID: "h1234567"}}}
	authorizer := &taskAuthorizerStub{}
	service, err := NewTaskService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := taskTestPrincipal()
	for range 2 {
		if err := service.RespondToPendingRequest(context.Background(), principal, "q1234567", "r1234567", 3, task.PendingDecisionApprove, "approve-key-001"); err != nil {
			t.Fatal(err)
		}
	}
	if store.pendingCalls != 2 || store.pendingTaskID != "q1234567" || store.pendingID != "r1234567" || store.pendingVersion != 3 ||
		store.pendingDecision != task.PendingDecisionApprove || store.pendingRecord.Operation != "task.pending.respond" ||
		store.pendingRecord.OwnerID != principal.UserID || store.pendingRecord.ResourceID != "q1234567" || store.pendingRecord.RelatedID != "r1234567" ||
		authorizer.lastHost != "h1234567" || authorizer.lastScope != identity.ScopeTaskControl {
		t.Fatalf("pending response authorization/record mismatch: calls=%d record=%#v auth=%#v", store.pendingCalls, store.pendingRecord, authorizer)
	}
	principal = identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567", HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeTaskRead}}
	if err := service.RespondToPendingRequest(context.Background(), principal, "q1234567", "r1234567", 3, task.PendingDecisionApprove, "approve-key-002"); domain.CodeOf(err) != domain.CodeNotFound || store.pendingCalls != 2 {
		t.Fatalf("unscoped response err=%v writes=%d", err, store.pendingCalls)
	}
	if err := service.RespondToPendingRequest(context.Background(), taskTestPrincipal(), "q1234567", "r1234567", 0, task.PendingDecisionApprove, "approve-key-003"); domain.CodeOf(err) != domain.CodeInvalidArgument || store.pendingCalls != 2 {
		t.Fatalf("invalid version err=%v writes=%d", err, store.pendingCalls)
	}
}

func TestTaskServiceAnswerClarificationRequiresControlAndBindsAnswerToVersion(t *testing.T) {
	store := &taskStoreStub{details: TaskDetails{Task: task.Task{ID: "q1234567", OwnerID: "u1234567", HostID: "h1234567"}}}
	authorizer := &taskAuthorizerStub{}
	service, err := NewTaskService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := taskTestPrincipal()
	answer := "Use the staging environment."
	for range 2 {
		if err := service.AnswerClarificationRequest(context.Background(), principal, "q1234567", "r1234567", 4, answer, "clarify-key-001"); err != nil {
			t.Fatal(err)
		}
	}
	if store.clarificationCalls != 2 || store.clarificationTaskID != "q1234567" || store.clarificationID != "r1234567" ||
		store.clarificationVersion != 4 || store.clarificationAnswer != answer || store.clarificationRecord.Operation != "task.clarification.answer" ||
		store.clarificationRecord.OwnerID != principal.UserID || store.clarificationRecord.ResourceID != "q1234567" ||
		store.clarificationRecord.RelatedID != "r1234567" || authorizer.lastHost != "h1234567" || authorizer.lastScope != identity.ScopeTaskControl {
		t.Fatalf("clarification answer authorization/record mismatch: calls=%d record=%#v auth=%#v", store.clarificationCalls, store.clarificationRecord, authorizer)
	}
	principal = identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567",
		HostIDs: []string{"h1234567"}, Scopes: []identity.Scope{identity.ScopeTaskRead},
	}
	if err := service.AnswerClarificationRequest(context.Background(), principal, "q1234567", "r1234567", 4, answer, "clarify-key-002"); domain.CodeOf(err) != domain.CodeNotFound || store.clarificationCalls != 2 {
		t.Fatalf("unscoped answer err=%v writes=%d", err, store.clarificationCalls)
	}
	if err := service.AnswerClarificationRequest(context.Background(), taskTestPrincipal(), "q1234567", "r1234567", 0, answer, "clarify-key-003"); domain.CodeOf(err) != domain.CodeInvalidArgument || store.clarificationCalls != 2 {
		t.Fatalf("invalid version err=%v writes=%d", err, store.clarificationCalls)
	}
	if err := service.AnswerClarificationRequest(context.Background(), taskTestPrincipal(), "q1234567", "r1234567", 4, "a\x00b", "clarify-key-004"); domain.CodeOf(err) != domain.CodeInvalidArgument || store.clarificationCalls != 2 {
		t.Fatalf("NUL answer err=%v writes=%d", err, store.clarificationCalls)
	}
}

func TestTaskServiceRequestActionRequiresControlAndUsesExpectedStatusInIdempotency(t *testing.T) {
	taskValue := task.Task{ID: "q1234567", OwnerID: "u1234567", HostID: "h1234567", Status: task.StatusRunning}
	store := &taskStoreStub{
		details:      TaskDetails{Task: taskValue},
		actionResult: TaskActionResult{Task: taskValue, Action: task.ActionPause, UnsettledOperationIDs: []string{"o1234567"}},
	}
	authorizer := &taskAuthorizerStub{}
	service, err := NewTaskService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := taskTestPrincipal()
	for range 2 {
		if _, err := service.RequestAction(context.Background(), principal, taskValue.ID, task.ActionPause, task.StatusRunning, "pause-key-001"); err != nil {
			t.Fatal(err)
		}
	}
	if store.actionCalls != 2 || store.actionTaskID != taskValue.ID || store.action != task.ActionPause ||
		store.actionExpected != task.StatusRunning || store.actionRecord.Operation != "task.action.pause" ||
		store.actionRecord.OwnerID != principal.UserID || store.actionRecord.RelatedID != taskValue.ID ||
		!domain.ValidTaskActionID(store.actionRequestID) || authorizer.lastHost != taskValue.HostID || authorizer.lastScope != identity.ScopeTaskControl {
		t.Fatalf("task action authorization/record mismatch: calls=%d request=%#v auth=%#v", store.actionCalls, store.actionRecord, authorizer)
	}
	firstHash := append([]byte(nil), store.actionRecord.RequestHash...)
	if _, err := service.RequestAction(context.Background(), principal, taskValue.ID, task.ActionPause, task.StatusQueued, "pause-key-001"); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstHash, store.actionRecord.RequestHash) {
		t.Fatal("expected_status did not affect the idempotency request hash")
	}
	principal = identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567", HostIDs: []string{taskValue.HostID}, Scopes: []identity.Scope{identity.ScopeTaskRead}}
	if _, err := service.RequestAction(context.Background(), principal, taskValue.ID, task.ActionCancel, task.StatusRunning, "cancel-key-001"); domain.CodeOf(err) != domain.CodeNotFound || store.actionCalls != 3 {
		t.Fatalf("unscoped cancel err=%v writes=%d", err, store.actionCalls)
	}
	if _, err := service.RequestAction(context.Background(), taskTestPrincipal(), taskValue.ID, task.ActionResume, task.StatusRunning, "resume-key-001"); domain.CodeOf(err) != domain.CodeInvalidArgument || store.actionCalls != 3 {
		t.Fatalf("invalid expected status err=%v writes=%d", err, store.actionCalls)
	}
}

func TestTaskServiceListFiltersKeyHostsThroughCurrentAuthorization(t *testing.T) {
	store := &taskStoreStub{}
	authorizer := &taskAuthorizerStub{}
	service, err := NewTaskService(store, authorizer, &modelCatalogStub{configured: true})
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{
		UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindAPIKey, TokenID: "t1234567",
		HostIDs: []string{"h7654321", "h1234567", "h1234567"}, Scopes: []identity.Scope{identity.ScopeTaskRead},
	}
	authorizer.err = domain.NewError(domain.CodeNotFound, "host not found")
	if _, err := service.List(context.Background(), principal, TaskListRequest{}); err != nil {
		t.Fatal(err)
	}
	if store.listHostIDs == nil || len(store.listHostIDs) != 0 {
		t.Fatalf("revoked Host remained visible: %#v", store.listHostIDs)
	}
	if authorizer.calls != 2 {
		t.Fatalf("duplicate Host was checked; calls=%d", authorizer.calls)
	}
}

func TestTaskServiceRejectsModelCatalogFailure(t *testing.T) {
	store := &taskStoreStub{}
	service, err := NewTaskService(store, &taskAuthorizerStub{}, &modelCatalogStub{err: errors.New("catalog unavailable")})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Submit(context.Background(), taskTestPrincipal(), SubmitTaskCommand{
		HostID: "h1234567", ModelID: "model-a", Objective: "do work", IdempotencyKey: "retry-key-005",
	})
	if err == nil || len(store.submissions) != 0 {
		t.Fatalf("catalog error=%v submissions=%d", err, len(store.submissions))
	}
}

func taskTestPrincipal() identity.Principal {
	return identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"}
}
