package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type taskServiceStub struct {
	submitCommand          app.SubmitTaskCommand
	submitCalls            int
	submitReplayed         bool
	acceptedConversationID string
	acceptCommand          app.AcceptConversationMessageCommand
	acceptCalls            int
	acceptReplayed         bool
	listRequest            app.TaskListRequest
	listCalls              int
	listPage               app.TaskPage
	conversationTasks      app.TaskPage
	conversationTaskID     string
	getID                  string
	getDetails             *app.TaskDetails
	actionTaskID           string
	action                 task.Action
	actionExpected         task.Status
	actionKey              string
	actionCalls            int
	retryTaskID            string
	retryItemID            string
	retryKey               string
	retryCalls             int
	responseTaskID         string
	responseID             string
	responseVersion        int64
	responseDecision       task.PendingDecision
	responseKey            string
	responseCalls          int
	clarificationTaskID    string
	clarificationID        string
	clarificationVersion   int64
	clarificationAnswer    string
	clarificationKey       string
	clarificationCalls     int
	events                 []task.Event
	eventStatus            task.Status
	eventCurrentSequence   int64
	eventCalls             int
	eventAfter             int64
	eventErrorAfterCall    int
	revokeTaskID           string
	revokeConnectorID      string
	revokeVersion          int64
	revokeCalls            int
	err                    error
}

type conversationRecoveryTaskServiceStub struct {
	*taskServiceStub
	activePage     app.TaskPage
	resumeEvents   map[string]task.Event
	resumedTaskIDs []string
	resumeAfter    []int64
}

func (s *conversationRecoveryTaskServiceStub) ListConversationActiveTasks(_ context.Context, _ identity.Principal, _, id string, _ app.PageRequest) (app.TaskPage, error) {
	s.conversationTaskID = id
	return s.activePage, nil
}

func (s *conversationRecoveryTaskServiceStub) ListEvents(_ context.Context, _ identity.Principal, taskID string, after int64, _ int32) (app.TaskEventPage, error) {
	s.resumedTaskIDs = append(s.resumedTaskIDs, taskID)
	s.resumeAfter = append(s.resumeAfter, after)
	event, ok := s.resumeEvents[taskID]
	if !ok {
		return app.TaskEventPage{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	page := app.TaskEventPage{Status: task.StatusQueued, CurrentSequence: event.Sequence}
	if event.Sequence > after {
		page.Items = []task.Event{event}
	}
	return page, nil
}

type observationTicketTaskServiceStub struct {
	*taskServiceStub
	principal identity.Principal
	taskID    string
	calls     int
	value     app.TaskWorkbenchObservationTicket
}

func (s *observationTicketTaskServiceStub) IssueWorkbenchObservationTicket(_ context.Context, principal identity.Principal, taskID string) (app.TaskWorkbenchObservationTicket, error) {
	s.calls++
	s.principal, s.taskID = principal, taskID
	return s.value, s.err
}

func (s *taskServiceStub) Submit(_ context.Context, _ identity.Principal, command app.SubmitTaskCommand) (app.TaskSubmissionResult, error) {
	s.submitCalls++
	s.submitCommand = command
	value := apiTestTask()
	value.HostID, value.ExecutionLocation, value.ApplicationID = command.HostID, "server", command.ApplicationID
	value.WorkspaceID, value.InputRevision = command.WorkspaceID, command.InputRevision
	value.WorkspaceCheckpointRevision = command.InputRevision
	if command.ConversationID != "" {
		value.ConversationID = command.ConversationID
	}
	if command.ModelID != "" {
		value.ModelID = command.ModelID
	}
	if command.Objective != "" {
		value.Objective = command.Objective
	}
	if command.ClientMessageID == "" {
		return app.TaskSubmissionResult{Task: value}, s.err
	}
	message := conversation.Message{
		ID: "m1234567", ConversationID: value.ConversationID, Role: conversation.RoleUser,
		Content: command.Objective, TaskID: value.ID, ClientMessageID: command.ClientMessageID,
		AttachmentIDs: append([]string(nil), command.AttachmentIDs...), Sequence: 1,
		DeliveryStatus: conversation.DeliveryApplied, CreatedAt: value.CreatedAt,
	}
	return app.TaskSubmissionResult{Task: value, Message: message, Replayed: s.submitReplayed}, s.err
}

func (s *taskServiceStub) AcceptConversationMessage(_ context.Context, _ identity.Principal, id string, command app.AcceptConversationMessageCommand) (conversation.Message, bool, error) {
	s.acceptCalls++
	s.acceptCommand, s.acceptedConversationID = command, id
	return conversation.Message{ID: "m1234567", ConversationID: id, Sequence: 1, Role: conversation.RoleUser,
		Content: command.Content, ClientMessageID: command.ClientMessageID, RoutingStatus: conversation.RoutingPending,
		DeliveryStatus: conversation.DeliveryPending, CreatedAt: time.Now().UTC()}, s.acceptReplayed, s.err
}

func (s *taskServiceStub) InitializeConversationMessageUpload(_ context.Context, _ identity.Principal, _ string, command app.InitializeConversationMessageUploadCommand) (app.ConversationMessageUpload, error) {
	return app.ConversationMessageUpload{ClientMessageID: command.ClientMessageID, State: "uploading", TotalBytes: command.TotalBytes, TotalChunks: command.TotalChunks}, s.err
}

func (s *taskServiceStub) PutConversationMessageChunk(_ context.Context, _ identity.Principal, _, _, _, _ string, _ int, _ []byte, _ string) error {
	return s.err
}

func (s *taskServiceStub) GetConversationMessageUpload(_ context.Context, _ identity.Principal, _, _, clientID, _ string) (app.ConversationMessageUpload, error) {
	return app.ConversationMessageUpload{ClientMessageID: clientID, State: "uploading"}, s.err
}

func (s *taskServiceStub) CommitConversationMessageUpload(_ context.Context, _ identity.Principal, _, conversationID, clientID, _, _ string) (conversation.Message, error) {
	return conversation.Message{ID: "m1234567", ConversationID: conversationID, Sequence: 1, Role: conversation.RoleUser,
		Content: "uploaded", ClientMessageID: clientID, RoutingStatus: conversation.RoutingPending,
		DeliveryStatus: conversation.DeliveryPending, CreatedAt: time.Now().UTC()}, s.err
}

func (s *taskServiceStub) AssociateConversationMessageAttachment(context.Context, identity.Principal, string, string, string, string, string) error {
	return s.err
}

func (s *taskServiceStub) RetryConversationMessageProcessing(context.Context, identity.Principal, string, string, string, string, string) error {
	return s.err
}

func (s *taskServiceStub) List(_ context.Context, _ identity.Principal, request app.TaskListRequest) (app.TaskPage, error) {
	s.listCalls++
	s.listRequest = request
	return s.listPage, s.err
}

func (s *taskServiceStub) ListConversationActiveTasks(_ context.Context, _ identity.Principal, _ string, id string, _ app.PageRequest) (app.TaskPage, error) {
	s.conversationTaskID = id
	return s.conversationTasks, s.err
}

func (s *taskServiceStub) Get(_ context.Context, _ identity.Principal, id string) (app.TaskDetails, error) {
	s.getID = id
	if s.getDetails != nil {
		return *s.getDetails, s.err
	}
	return app.TaskDetails{Task: apiTestTask()}, s.err
}

func (s *taskServiceStub) RequestAction(_ context.Context, _ identity.Principal, taskID string, action task.Action, expected task.Status, key string) (app.TaskActionResult, error) {
	s.actionCalls++
	s.actionTaskID, s.action, s.actionExpected, s.actionKey = taskID, action, expected, key
	return app.TaskActionResult{Task: apiTestTask(), Action: action, UnsettledOperationIDs: []string{"o1234567"}}, s.err
}

func (s *taskServiceStub) RetryBatchItem(_ context.Context, _ identity.Principal, taskID, batchItemID, idempotencyKey string) error {
	s.retryCalls++
	s.retryTaskID, s.retryItemID, s.retryKey = taskID, batchItemID, idempotencyKey
	return s.err
}

func (s *taskServiceStub) RespondToPendingRequest(_ context.Context, _ identity.Principal, taskID, pendingID string, version int64, decision task.PendingDecision, idempotencyKey string) error {
	s.responseCalls++
	s.responseTaskID, s.responseID, s.responseVersion, s.responseDecision, s.responseKey = taskID, pendingID, version, decision, idempotencyKey
	return s.err
}

func (s *taskServiceStub) AnswerClarificationRequest(_ context.Context, _ identity.Principal, taskID, clarificationID string, version int64, answer, idempotencyKey string) error {
	s.clarificationCalls++
	s.clarificationTaskID, s.clarificationID, s.clarificationVersion = taskID, clarificationID, version
	s.clarificationAnswer, s.clarificationKey = answer, idempotencyKey
	return s.err
}

func (s *taskServiceStub) ListEvents(_ context.Context, _ identity.Principal, _ string, after int64, _ int32) (app.TaskEventPage, error) {
	s.eventCalls++
	s.eventAfter = after
	status := s.eventStatus
	if status == "" {
		status = task.StatusQueued
	}
	current := s.eventCurrentSequence
	if current == 0 && len(s.events) > 0 {
		current = s.events[len(s.events)-1].Sequence
	}
	err := s.err
	if s.eventErrorAfterCall > 0 && s.eventCalls < s.eventErrorAfterCall {
		err = nil
	}
	return app.TaskEventPage{HostID: "h1234567", Status: status, CurrentSequence: current, Items: s.events}, err
}

func (s *taskServiceStub) RevokeMCPConnectorGrant(_ context.Context, _ identity.Principal, taskID, connectorID string, expectedVersion int64) (app.TaskMCPConnectorGrant, error) {
	s.revokeCalls++
	s.revokeTaskID, s.revokeConnectorID, s.revokeVersion = taskID, connectorID, expectedVersion
	return app.TaskMCPConnectorGrant{}, s.err
}

func TestTaskSubmitHandlerPassesRequestAndReturnsStableShape(t *testing.T) {
	service := &taskServiceStub{}
	engine := taskAPIRouter(service, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"host_id":"h1234567","workspace_id":"w1234567","input_revision":7,"mcp_connector_ids":["kbbbbbbb","kaaaaaaa"],"application_id":"wechat","model_id":"model-a","objective":"summarize files","batch_items":[]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "retry-key-007")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || service.submitCalls != 1 || service.submitCommand.IdempotencyKey != "retry-key-007" || service.submitCommand.HostID != "h1234567" ||
		service.submitCommand.WorkspaceID != "w1234567" || service.submitCommand.InputRevision == nil || *service.submitCommand.InputRevision != 7 ||
		service.submitCommand.ApplicationID != "wechat" || service.submitCommand.ModelID != "model-a" || len(service.submitCommand.MCPConnectorIDs) != 2 ||
		service.submitCommand.MCPConnectorIDs[0] != "kbbbbbbb" || service.submitCommand.MCPConnectorIDs[1] != "kaaaaaaa" {
		t.Fatalf("status=%d calls=%d command=%#v body=%s", response.Code, service.submitCalls, service.submitCommand, response.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if string(body["application_id"]) != `"wechat"` {
		t.Fatalf("application_id = %s, want wechat", body["application_id"])
	}
	if string(body["workspace_id"]) != `"w1234567"` || string(body["input_revision"]) != `7` {
		t.Fatalf("workspace input snapshot = %s/%s", body["workspace_id"], body["input_revision"])
	}
	if string(body["workspace_checkpoint_revision"]) != `7` {
		t.Fatalf("workspace checkpoint = %s, want 7", body["workspace_checkpoint_revision"])
	}
	for _, field := range []string{"steps", "batch_items", "artifacts"} {
		if string(body[field]) != "[]" {
			t.Errorf("%s = %s, want []", field, body[field])
		}
	}
}

func TestTaskSubmitRejectsUnknownFieldsBeforeCallingService(t *testing.T) {
	service := &taskServiceStub{}
	engine := taskAPIRouter(service, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"host_id":"h1234567","model_id":"model-a","objective":"summarize","unexpected":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "retry-key-008")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || service.submitCalls != 0 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, service.submitCalls, response.Body.String())
	}
}

func TestStandaloneTaskSubmitRejectsAttachmentIDs(t *testing.T) {
	service := &taskServiceStub{}
	engine := taskAPIRouter(service, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"model_id":"model-a","objective":"summarize","attachment_ids":["pa_0123456789abcdef0123456789abcdef"]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "standalone-attachment-01")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || service.submitCalls != 0 {
		t.Fatalf("status=%d submit=%d body=%s; want rejection before Task service", response.Code, service.submitCalls, response.Body.String())
	}
}

func TestCloudTaskResponseDoesNotRequireAHost(t *testing.T) {
	service := &taskServiceStub{}
	engine := taskAPIRouter(service, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"model_id":"model-a","objective":"continue in the cloud"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "cloud-task-001")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusAccepted || service.submitCommand.HostID != "" || string(body["execution_location"]) != `"server"` || body["host_id"] != nil {
		t.Fatalf("cloud task response = %d %s; command=%#v", response.Code, response.Body, service.submitCommand)
	}
}

func TestTaskActionRetriesOneBatchItem(t *testing.T) {
	service := &taskServiceStub{}
	engine := taskAPIRouter(service, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/tasks/q1234567/actions", strings.NewReader(`{"action":"retry_batch_item","batch_item_id":"b1234567"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "retry-key-101")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || service.retryCalls != 1 || service.retryTaskID != "q1234567" || service.retryItemID != "b1234567" || service.retryKey != "retry-key-101" {
		t.Fatalf("status=%d calls=%d task=%s item=%s key=%s body=%s", response.Code, service.retryCalls, service.retryTaskID, service.retryItemID, service.retryKey, response.Body.String())
	}
}

func TestTaskActionRespondsToExactPendingRequest(t *testing.T) {
	service := &taskServiceStub{}
	engine := taskAPIRouter(service, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/tasks/q1234567/approvals/r1234567/responses", strings.NewReader(`{"version":2,"decision":"approve"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "approval-key-001")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || service.responseCalls != 1 || service.responseTaskID != "q1234567" ||
		service.responseID != "r1234567" || service.responseVersion != 2 || service.responseDecision != task.PendingDecisionApprove || service.responseKey != "approval-key-001" {
		t.Fatalf("status=%d calls=%d task=%s pending=%s version=%d decision=%s key=%s body=%s", response.Code, service.responseCalls,
			service.responseTaskID, service.responseID, service.responseVersion, service.responseDecision, service.responseKey, response.Body.String())
	}
}

func TestTaskActionAnswersExactClarification(t *testing.T) {
	service := &taskServiceStub{}
	engine := taskAPIRouter(service, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/tasks/q1234567/inputs", strings.NewReader(`{"clarification_id":"r1234567","clarification_version":2,"answer":"Use staging"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "clarify-key-001")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	var body taskActionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusAccepted || service.clarificationCalls != 1 || service.clarificationTaskID != "q1234567" ||
		service.clarificationID != "r1234567" || service.clarificationVersion != 2 || service.clarificationAnswer != "Use staging" ||
		service.clarificationKey != "clarify-key-001" || body.TaskID != "q1234567" || body.Action != "answer_clarification" || body.Status != task.StatusQueued {
		t.Fatalf("status=%d calls=%d request=%#v response=%#v body=%s", response.Code, service.clarificationCalls, service, body, response.Body.String())
	}
}

func TestTaskActionRequestsPauseWithExpectedStatusAndReturnsUnsettledOperations(t *testing.T) {
	service := &taskServiceStub{}
	engine := taskAPIRouter(service, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/tasks/q1234567/actions", strings.NewReader(`{"action":"pause","expected_status":"queued"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "pause-key-001")
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	var body taskActionResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusAccepted || service.actionCalls != 1 || service.actionTaskID != "q1234567" ||
		service.action != task.ActionPause || service.actionExpected != task.StatusQueued || service.actionKey != "pause-key-001" ||
		body.TaskID != "q1234567" || body.Action != string(task.ActionPause) || len(body.UnsettledOperationIDs) != 1 || body.UnsettledOperationIDs[0] != "o1234567" {
		t.Fatalf("status=%d calls=%d request=%#v response=%#v body=%s", response.Code, service.actionCalls, service, body, response.Body.String())
	}
}

func TestTaskActionRejectsUnsupportedOrExtendedRequests(t *testing.T) {
	for _, body := range []string{
		`{"action":"cancel","batch_item_id":"b1234567"}`,
		`{"action":"pause"}`,
		`{"action":"resume","expected_status":"running"}`,
		`{"action":"cancel","expected_status":"completed"}`,
		`{"action":"pause","expected_status":"running","batch_item_id":"b1234567"}`,
		`{"action":"retry_batch_item","batch_item_id":"b1234567","force":true}`,
		`{"action":"retry_batch_item"}`,
		`{"action":"respond","pending_id":"r1234567","pending_version":1,"decision":"maybe"}`,
		`{"action":"respond","pending_id":"bad","pending_version":1,"decision":"approve"}`,
		`{"action":"respond","pending_id":"r1234567","pending_version":0,"decision":"approve"}`,
		`{"action":"respond","batch_item_id":"b1234567","pending_id":"r1234567","pending_version":1,"decision":"approve"}`,
		`{"action":"answer_clarification","clarification_id":"r1234567","clarification_version":1,"answer":"ok","pending_id":"r7654321"}`,
		`{"action":"answer_clarification","clarification_id":"r1234567","clarification_version":0,"answer":"ok"}`,
		`{"action":"answer_clarification","clarification_id":"r1234567","clarification_version":1,"answer":"  "}`,
		`{"action":"answer_clarification","clarification_id":"r1234567","clarification_version":1,"answer":"a\u0000b"}`,
	} {
		service := &taskServiceStub{}
		engine := taskAPIRouter(service, nil)
		request := httptest.NewRequest(http.MethodPost, "/api/tasks/q1234567/actions", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "retry-key-102")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || service.retryCalls != 0 || service.responseCalls != 0 || service.clarificationCalls != 0 {
			t.Errorf("body=%s status=%d retry_calls=%d response_calls=%d clarification_calls=%d", body, response.Code, service.retryCalls, service.responseCalls, service.clarificationCalls)
		}
	}
}

func TestTaskResponseIncludesBatchAttemptAndArtifactAssociation(t *testing.T) {
	now := time.Now().UTC()
	view, err := toTaskResponse(app.TaskDetails{
		Task:       apiTestTask(),
		BatchItems: []task.BatchItem{{ID: "b1234567", TaskID: "q1234567", Key: "report", Status: task.BatchItemSucceeded, Attempt: 2}},
		Artifacts: []task.Artifact{{
			ID: "a1234567", TaskID: "q1234567", StepID: "s1234567", Label: "Report", Path: "reports/summary.txt",
			MediaType: "text/plain", BatchItemID: "b1234567", BatchAttempt: 1, CreatedAt: now,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.BatchItems) != 1 || view.BatchItems[0].Attempt != 2 || len(view.Artifacts) != 1 ||
		view.Artifacts[0].BatchItemID == nil || *view.Artifacts[0].BatchItemID != "b1234567" || view.Artifacts[0].BatchAttempt != 1 ||
		view.Artifacts[0].Verified || view.Artifacts[0].WorkspaceID != nil || view.Artifacts[0].WorkspaceRevision != nil ||
		view.Artifacts[0].SizeBytes != nil || view.Artifacts[0].SHA256 != nil {
		t.Fatalf("batch response metadata = %#v", view)
	}
}

func TestTaskResponseIncludesVerifiedArtifactWorkspaceProof(t *testing.T) {
	now := time.Now().UTC()
	current := apiTestTask()
	current.WorkspaceID = "w1234567"
	revision := int64(3)
	current.InputRevision, current.WorkspaceCheckpointRevision = &revision, &revision
	digest := strings.Repeat("a", 64)
	value := app.TaskDetails{Task: current, Artifacts: []task.Artifact{{
		ID: "a1234567", TaskID: current.ID, StepID: "s1234567", Label: "Report", Path: "reports/summary.txt",
		MediaType: "text/plain", WorkspaceID: current.WorkspaceID, Revision: 4, SizeBytes: 12, SHA256: digest, CreatedAt: now,
	}}}
	view, err := toTaskResponse(value)
	if err != nil || len(view.Artifacts) != 1 {
		t.Fatalf("verified artifact response = %#v, err=%v", view.Artifacts, err)
	}
	artifact := view.Artifacts[0]
	if !artifact.Verified || artifact.WorkspaceID == nil || *artifact.WorkspaceID != current.WorkspaceID ||
		artifact.WorkspaceRevision == nil || *artifact.WorkspaceRevision != 4 || artifact.SizeBytes == nil || *artifact.SizeBytes != 12 ||
		artifact.SHA256 == nil || *artifact.SHA256 != digest {
		t.Fatalf("artifact proof response = %#v", artifact)
	}

	value.Artifacts[0].SHA256 = ""
	if _, err := toTaskResponse(value); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("partial artifact proof returned %v", err)
	}
	value.Artifacts[0].SHA256 = digest
	value.Artifacts[0].WorkspaceID = "w7654321"
	if _, err := toTaskResponse(value); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("artifact proof from another Workspace returned %v", err)
	}
}

func TestTaskResponseIncludesValidatedAssistantMessageSnapshot(t *testing.T) {
	current := apiTestTask()
	current.Status = task.StatusRunning
	current.EventSequence = 8
	message := conversation.Message{
		ID: "m1234567", ConversationID: current.ConversationID, Sequence: 1, OwnerID: current.OwnerID,
		Role: conversation.RoleAssistant, Content: "Partial answer", TaskID: current.ID, CreatedAt: time.Now().UTC(),
	}
	value := app.TaskDetails{
		Task: current,
		AssistantMessage: &app.AssistantMessageSnapshot{
			Message: message, Status: app.AssistantMessageStreaming,
		},
	}
	view, err := toTaskResponse(value)
	if err != nil || view.AssistantMessage == nil || view.AssistantMessage.ID != message.ID ||
		view.AssistantMessage.Content != message.Content || view.AssistantMessage.Status != "streaming" ||
		view.EventSequence != current.EventSequence {
		t.Fatalf("assistant message snapshot response = %#v, err=%v", view.AssistantMessage, err)
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		ID       string `json:"id"`
		Sequence int64  `json:"sequence"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(body["assistant_message"], &snapshot); err != nil ||
		snapshot.ID != message.ID || snapshot.Sequence != message.Sequence || snapshot.Status != "streaming" {
		t.Fatalf("assistant_message JSON = %s, err=%v", body["assistant_message"], err)
	}

	invalid := value
	invalidMessage := message
	invalidMessage.TaskID = "q8765432"
	invalid.AssistantMessage = &app.AssistantMessageSnapshot{Message: invalidMessage, Status: app.AssistantMessageStreaming}
	if _, err := toTaskResponse(invalid); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("mismatched assistant Task error = %v, want internal", err)
	}
	invalid = value
	invalidMessage = message
	invalidMessage.Sequence = 0
	invalid.AssistantMessage = &app.AssistantMessageSnapshot{Message: invalidMessage, Status: app.AssistantMessageStreaming}
	if _, err := toTaskResponse(invalid); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("invalid assistant message sequence error = %v, want internal", err)
	}
	invalid = value
	invalid.AssistantMessage = &app.AssistantMessageSnapshot{Message: message, Status: app.AssistantMessageStatus("unknown")}
	if _, err := toTaskResponse(invalid); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("unknown assistant snapshot status error = %v, want internal", err)
	}
}

func TestTaskResponseIncludesResultRevisionEvidence(t *testing.T) {
	current := apiTestTask()
	current.Status = task.StatusCompleted
	currentRevision, desiredRevision, eventSequence := int64(2), int64(2), int64(7)
	started := false
	current.CurrentRevision, current.DesiredRevision = &currentRevision, &desiredRevision
	message := conversation.Message{ID: "m1234567", ConversationID: current.ConversationID, Sequence: 1,
		OwnerID: current.OwnerID, Role: conversation.RoleAssistant, Content: "Answer", TaskID: current.ID, CreatedAt: time.Now().UTC()}
	value := app.TaskDetails{Task: current, CurrentRevisionStarted: &started, ResultMessagePublished: true, AssistantMessage: &app.AssistantMessageSnapshot{
		Message: message, Status: app.AssistantMessageCompleted, TaskRevision: &currentRevision, ResultEventSequence: &eventSequence,
	}}
	view, err := toTaskResponse(value)
	if err != nil || view.CurrentRevision == nil || *view.CurrentRevision != 2 || view.DesiredRevision == nil || *view.DesiredRevision != 2 ||
		view.CurrentRevisionStarted == nil || *view.CurrentRevisionStarted || !view.ResultMessagePublished || view.AssistantMessage == nil || view.AssistantMessage.TaskRevision == nil ||
		*view.AssistantMessage.TaskRevision != 2 || view.AssistantMessage.ResultEventSequence == nil || *view.AssistantMessage.ResultEventSequence != 7 {
		t.Fatalf("Task revision response = %+v, err=%v", view, err)
	}
	encoded, err := json.Marshal(view)
	if err != nil || !strings.Contains(string(encoded), `"current_revision_started":false`) {
		t.Fatalf("false Task start evidence was omitted: %s err=%v", encoded, err)
	}
	value.Task.CurrentRevision, value.Task.DesiredRevision = nil, nil
	if _, err := toTaskResponse(value); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("Task start evidence without revision was accepted: %v", err)
	}
	value.Task.CurrentRevision, value.Task.DesiredRevision = &currentRevision, &desiredRevision
	value.Task.DesiredRevision = nil
	if _, err := toTaskResponse(value); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("incomplete Task revision projection was accepted: %v", err)
	}
	value.Task.DesiredRevision = &desiredRevision
	value.AssistantMessage.TaskRevision = nil
	if _, err := toTaskResponse(value); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("result event without Task revision was accepted: %v", err)
	}
}

func TestTaskResponseIncludesOnlyActivePendingApproval(t *testing.T) {
	now := time.Now().UTC()
	value := app.TaskDetails{
		Task: func() task.Task { result := apiTestTask(); result.Status = task.StatusWaitingUser; return result }(),
		PendingRequest: &task.PendingRequest{
			ID: "r1234567", TaskID: "q1234567", Version: 2, Kind: "approval", Title: "Approve file upload",
			Object: "report.txt", Content: "Upload this report?", Impact: "The file will leave this Host.",
			MediaPreviewOperationID: "o1234567",
			Status:                  task.PendingRequestPending, CreatedAt: now,
		},
	}
	view, err := toTaskResponse(value)
	if err != nil || view.PendingRequest == nil || view.PendingRequest.ID != "r1234567" || view.PendingRequest.Version != 2 ||
		view.PendingRequest.Impact != "The file will leave this Host." || view.PendingRequest.MediaPreviewOperationID != "o1234567" {
		t.Fatalf("active pending request response = %#v, %v", view.PendingRequest, err)
	}
	encoded, err := json.Marshal(view.PendingRequest)
	if err != nil || !strings.Contains(string(encoded), `"media_preview_operation_id":"o1234567"`) {
		t.Fatalf("pending media preview JSON = %s, err=%v", encoded, err)
	}
	value.PendingRequest.MediaPreviewOperationID = "invalid"
	if _, err := toTaskResponse(value); domain.CodeOf(err) != domain.CodeInternal {
		t.Fatalf("invalid media preview operation ID error = %v, want internal", err)
	}
	value.PendingRequest.MediaPreviewOperationID = "o1234567"
	value.PendingRequest.Status = task.PendingRequestApproved
	view, err = toTaskResponse(value)
	if err != nil || view.PendingRequest != nil {
		t.Fatalf("resolved pending request must not be offered again: %#v, %v", view.PendingRequest, err)
	}
}

func TestTaskResponseIncludesOnlyPendingClarificationForClarificationWait(t *testing.T) {
	current := apiTestTask()
	current.Status, current.WaitingReason = task.StatusWaitingUser, "clarification_required"
	value := app.TaskDetails{Task: current, ClarificationRequests: []task.ClarificationRequest{{
		ID: "r1234567", TaskID: current.ID, Version: 1, ModelStepID: "s1234567", CallID: "call-1", Question: "Which environment?", Status: task.ClarificationPending,
	}}}
	view, err := toTaskResponse(value)
	if err != nil || view.ClarificationRequest == nil || view.ClarificationRequest.ID != "r1234567" ||
		view.ClarificationRequest.Version != 1 || view.ClarificationRequest.Question != "Which environment?" {
		t.Fatalf("active clarification response = %#v, %v", view.ClarificationRequest, err)
	}
	value.ClarificationRequests[0].Status = task.ClarificationAnswered
	value.ClarificationRequests[0].Answer = "staging"
	view, err = toTaskResponse(value)
	if err != nil || view.ClarificationRequest != nil {
		t.Fatalf("answered clarification should not be offered again: %#v, %v", view.ClarificationRequest, err)
	}
}

func TestGetTaskReturnsChildAndClarificationSteps(t *testing.T) {
	now := time.Now().UTC()
	modelStepID := "s1234567"
	clarificationStepID := "s1234568"
	childStepID := "s1234569"
	progressStepID := "s1234570"
	clarificationID := "r1234567"
	taskValue := apiTestTask()
	taskValue.Status = task.StatusWaitingUser
	taskValue.WaitingReason = "clarification_required"
	service := &taskServiceStub{getDetails: &app.TaskDetails{
		Task: taskValue,
		Steps: []task.Step{
			{ID: modelStepID, TaskID: taskValue.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded, Title: "Ask model", Summary: "Need a required detail.", CreatedAt: now},
			{ID: clarificationStepID, TaskID: taskValue.ID, Sequence: 2, Kind: "clarification", Status: task.StepWaitingUser, Title: "Clarification", Summary: "Which environment?", CreatedAt: now},
			{ID: childStepID, TaskID: taskValue.ID, Sequence: 3, Kind: "subagent", Status: task.StepRunning, Title: "Child Agent", Summary: "Working on a child task.", CreatedAt: now},
			{ID: progressStepID, TaskID: taskValue.ID, Sequence: 4, Kind: "progress", Status: task.StepSucceeded, Title: "开始处理", Summary: "正在处理你的请求。", CreatedAt: now},
		},
		ClarificationRequests: []task.ClarificationRequest{{
			ID: clarificationID, TaskID: taskValue.ID, Version: 1, ModelStepID: modelStepID,
			CallID: "call-clarify", Question: "Which environment?", Status: task.ClarificationPending,
		}},
	}}
	engine := taskAPIRouter(service, nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/"+taskValue.ID, nil))
	if response.Code != http.StatusOK || service.getID != taskValue.ID {
		t.Fatalf("GET task status=%d id=%q body=%s", response.Code, service.getID, response.Body.String())
	}
	var body struct {
		Steps []struct {
			Kind string `json:"kind"`
		} `json:"steps"`
		ClarificationRequest *struct {
			ID       string `json:"id"`
			Question string `json:"question"`
		} `json:"clarification_request"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, step := range body.Steps {
		kinds[step.Kind] = true
	}
	if !kinds["subagent"] || !kinds["clarification"] || !kinds["progress"] || body.ClarificationRequest == nil ||
		body.ClarificationRequest.ID != clarificationID || body.ClarificationRequest.Question != "Which environment?" {
		t.Fatalf("GET task omitted child/clarification contract: %#v", body)
	}
}

func TestGetTaskCountsStepsExcludingBudget(t *testing.T) {
	now := time.Now().UTC()
	current := apiTestTask()
	view, err := toTaskResponse(app.TaskDetails{
		Task: current,
		Steps: []task.Step{
			{ID: "s1234567", TaskID: current.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded, Title: "Model", CreatedAt: now},
			{ID: "s1234568", TaskID: current.ID, Sequence: 2, Kind: "budget", Status: task.StepSucceeded, Title: "Budget", CreatedAt: now},
			{ID: "s1234569", TaskID: current.ID, Sequence: 3, Kind: "tool", Status: task.StepFailed, Title: "Tool", CreatedAt: now},
			{ID: "s1234570", TaskID: current.ID, Sequence: 4, Kind: "progress", Status: task.StepRunning, Title: "Progress", CreatedAt: now},
		},
	})
	if err != nil || view.StepCount != 3 || view.CompletedStepCount != 1 {
		t.Fatalf("task step counts = (%d, %d), err=%v; want (3, 1)", view.StepCount, view.CompletedStepCount, err)
	}
}

func TestListTasksReturnsStepCounts(t *testing.T) {
	value := apiTestTask()
	value.StepCount = 3
	value.CompletedStepCount = 1
	service := &taskServiceStub{listPage: app.TaskPage{Items: []task.Task{value}}}
	engine := taskAPIRouter(service, nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks?limit=20", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Items []struct {
			StepCount          int64 `json:"step_count"`
			CompletedStepCount int64 `json:"completed_step_count"`
		} `json:"items"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Items) != 1 || body.Items[0].StepCount != 3 || body.Items[0].CompletedStepCount != 1 {
		t.Fatalf("task summary counts = %#v; want total 3, completed 1", body.Items)
	}
}

func TestGetTaskIncludesSnapshotEventSequence(t *testing.T) {
	taskValue := apiTestTask()
	taskValue.EventSequence = 42
	service := &taskServiceStub{getDetails: &app.TaskDetails{Task: taskValue}}
	engine := taskAPIRouter(service, nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/"+taskValue.ID, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET task status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		EventSequence int64 `json:"event_sequence"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.EventSequence != taskValue.EventSequence {
		t.Fatalf("event_sequence=%d, want %d", body.EventSequence, taskValue.EventSequence)
	}

	taskValue.EventSequence = -1
	service.getDetails.Task = taskValue
	response = httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/"+taskValue.ID, nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("negative event_sequence status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTaskResponseIncludesPendingStopAndUnsettledOperations(t *testing.T) {
	now := time.Now().UTC()
	current := apiTestTask()
	current.Status = task.StatusCancelling
	view, err := toTaskResponse(app.TaskDetails{
		Task: current,
		StopRequest: &task.StopRequest{
			ID: "x1234567", TaskID: current.ID, Action: task.ActionCancel,
			Status: task.StopRequestPending, SafeAfter: now,
		},
		StopOperationIDs: []string{"o1234567"},
	})
	if err != nil || view.StopRequest == nil || view.StopRequest.ID != "x1234567" ||
		view.StopRequest.Status != task.StopRequestPending || len(view.StopRequest.UnsettledOperationIDs) != 1 || view.StopRequest.UnsettledOperationIDs[0] != "o1234567" {
		t.Fatalf("pending stop response = %#v, err=%v", view.StopRequest, err)
	}
	view, err = toTaskResponse(app.TaskDetails{
		Task: current,
		StopRequest: &task.StopRequest{
			ID: "x1234567", TaskID: current.ID, Action: task.ActionCancel,
			Status: task.StopRequestCompleted, SafeAfter: now,
		},
		StopOperationIDs: []string{"o1234567"},
	})
	if err == nil || view.StopRequest != nil {
		t.Fatalf("completed stop must not include unrelated unsettled operations: %#v err=%v", view.StopRequest, err)
	}
}

func TestTaskListRejectsInvalidAndDuplicatePageLimits(t *testing.T) {
	for _, target := range []string{"/api/tasks?limit=101", "/api/tasks?limit=10&limit=20"} {
		service := &taskServiceStub{}
		engine := taskAPIRouter(service, nil)
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, target, nil))
		if response.Code != http.StatusBadRequest || service.listCalls != 0 {
			t.Errorf("%s status=%d list calls=%d body=%s", target, response.Code, service.listCalls, response.Body.String())
		}
	}
}

func TestTaskListReturnsNullCursorAndEmptyItems(t *testing.T) {
	engine := taskAPIRouter(&taskServiceStub{}, nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks", nil))
	var body struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor json.RawMessage   `json:"next_cursor"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || body.Items == nil || string(body.NextCursor) != "null" {
		t.Fatalf("status=%d items=%#v next_cursor=%s", response.Code, body.Items, body.NextCursor)
	}
}

func TestConversationMessageRequiresMatchingIdempotencyKey(t *testing.T) {
	service := &taskServiceStub{}
	request := httptest.NewRequest(http.MethodPost, "/api/conversations/c1234567/messages", strings.NewReader(`{"client_message_id":"message-0001","content":"继续"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "different-key-0001")
	request.Header.Set("X-Morde-Service-Instance-ID", "11111111-1111-4111-8111-111111111111")
	request.Header.Set("X-Morde-Actor-ID", "u1234567")
	response := httptest.NewRecorder()
	taskAPIRouter(service, nil).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || service.submitCalls != 0 || service.acceptCalls != 0 {
		t.Fatalf("mismatched idempotency key = %d submit=%d accept=%d: %s", response.Code, service.submitCalls, service.acceptCalls, response.Body.String())
	}
}

func TestConversationMessageIsAcceptedBeforeRouting(t *testing.T) {
	service := &taskServiceStub{acceptReplayed: true}
	request := httptest.NewRequest(http.MethodPost, "/api/conversations/c1234567/messages", strings.NewReader(`{"client_message_id":"message-0001","previous_client_message_id":"message-0000","reference_task_id":"q1234567","host_id":"h1234567","workspace_id":"w1234567","mcp_connector_ids":["kbbbbbbb"],"client_attachment_ids":["client-file-0001"],"application_id":"wechat","model_id":"model-a","content":"hello"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "message-0001")
	request.Header.Set("X-Morde-Service-Instance-ID", "11111111-1111-4111-8111-111111111111")
	request.Header.Set("X-Morde-Actor-ID", "u1234567")
	response := httptest.NewRecorder()
	taskAPIRouter(service, nil).ServeHTTP(response, request)
	var body acceptedConversationMessageResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusAccepted ||
		service.acceptCalls != 1 || service.submitCalls != 0 || service.acceptedConversationID != "c1234567" ||
		service.acceptCommand.ServiceInstanceID != "11111111-1111-4111-8111-111111111111" ||
		service.acceptCommand.IdempotencyKey != "message-0001" || service.acceptCommand.ClientMessageID != "message-0001" ||
		service.acceptCommand.PreviousClientMessageID != "message-0000" || service.acceptCommand.ReferenceTaskID != "q1234567" ||
		service.acceptCommand.HostID != "h1234567" || service.acceptCommand.WorkspaceID != "w1234567" ||
		len(service.acceptCommand.MCPConnectorIDs) != 1 || service.acceptCommand.MCPConnectorIDs[0] != "kbbbbbbb" ||
		len(service.acceptCommand.ClientAttachmentIDs) != 1 || service.acceptCommand.ClientAttachmentIDs[0] != "client-file-0001" ||
		service.acceptCommand.ApplicationID != "wechat" || service.acceptCommand.ModelID != "model-a" || service.acceptCommand.Content != "hello" ||
		body.Message.ID != "m1234567" || body.Message.ClientMessageID != "message-0001" || body.Message.TaskID != "" ||
		body.Message.RoutingStatus != conversation.RoutingPending || body.Message.DeliveryStatus != conversation.DeliveryPending || !body.Replayed {
		t.Fatalf("accept conversation message: status=%d body=%s response=%#v service=%#v err=%v", response.Code, response.Body.String(), body, service, err)
	}
}

func TestConversationActiveTasksIncludesCursor(t *testing.T) {
	value := apiTestTask()
	value.EventSequence = 7
	service := &taskServiceStub{conversationTasks: app.TaskPage{Items: []task.Task{value}, NextCursor: "next"}}
	response := httptest.NewRecorder()
	taskAPIRouter(service, nil).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/conversations/c1234567/tasks?limit=1", nil))
	var body struct {
		Items []struct {
			ID            string `json:"id"`
			EventSequence int64  `json:"event_sequence"`
		} `json:"items"`
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK ||
		service.conversationTaskID != "c1234567" || len(body.Items) != 1 || body.Items[0].ID != value.ID ||
		body.Items[0].EventSequence != 7 || body.NextCursor != "next" {
		t.Fatalf("active tasks response=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
}

func TestConversationActiveTasksResumeEachTaskFromItsOwnEventCursor(t *testing.T) {
	conversationID := "c1234567"
	taskIDs := []string{"q1234567", "q7654321"}
	messageIDs := []string{"m1234567", "m7654321"}
	turnIDs := []string{"s1234567", "s7654321"}
	cursors := []int64{7, 12}
	now := time.Now().UTC()
	tasks := make([]task.Task, len(taskIDs))
	resumeEvents := make(map[string]task.Event, len(taskIDs))
	for i, taskID := range taskIDs {
		value := apiTestTask()
		value.ID, value.ConversationID, value.EventSequence = taskID, conversationID, cursors[i]
		tasks[i] = value
		payload, err := json.Marshal(map[string]string{
			"role": "assistant", "message_id": messageIDs[i], "turn_id": turnIDs[i], "delta": "resumed answer",
		})
		if err != nil {
			t.Fatal(err)
		}
		resumeEvents[taskID] = task.Event{TaskID: taskID, Sequence: cursors[i] + 1, Type: "task.message", Payload: payload, CreatedAt: now}
	}
	service := &conversationRecoveryTaskServiceStub{
		taskServiceStub: &taskServiceStub{},
		activePage:      app.TaskPage{Items: tasks}, resumeEvents: resumeEvents,
	}
	engine := taskAPIRouter(service, nil)
	discovery := httptest.NewRecorder()
	engine.ServeHTTP(discovery, httptest.NewRequest(http.MethodGet, "/api/conversations/"+conversationID+"/tasks", nil))
	var recovered struct {
		Items []struct {
			ID            string `json:"id"`
			EventSequence int64  `json:"event_sequence"`
		} `json:"items"`
	}
	if err := json.Unmarshal(discovery.Body.Bytes(), &recovered); err != nil || discovery.Code != http.StatusOK || len(recovered.Items) != len(taskIDs) {
		t.Fatalf("discover active conversation tasks: status=%d body=%s err=%v", discovery.Code, discovery.Body.String(), err)
	}

	for i, item := range recovered.Items {
		if item.ID != taskIDs[i] || item.EventSequence != cursors[i] {
			t.Fatalf("discovered task %d=%+v, want id=%s cursor=%d", i, item, taskIDs[i], cursors[i])
		}
		request := httptest.NewRequest(http.MethodGet, "/api/tasks/"+item.ID+"/events", nil)
		request.Header.Set("Last-Event-ID", strconv.FormatInt(item.EventSequence, 10))
		requestContext, cancel := context.WithCancel(request.Context())
		request = request.WithContext(requestContext)
		timer := time.AfterFunc(30*time.Millisecond, cancel)
		response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
		engine.ServeHTTP(response, request)
		timer.Stop()
		cancel()
		resumed := resumeEvents[item.ID]
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "id: "+strconv.FormatInt(resumed.Sequence, 10)) ||
			!strings.Contains(response.Body.String(), `"delta":"resumed answer"`) ||
			service.resumedTaskIDs[i] != item.ID || service.resumeAfter[i] != item.EventSequence {
			t.Fatalf("resume task %s after %d: status=%d target=%v cursor=%v body=%s", item.ID, item.EventSequence,
				response.Code, service.resumedTaskIDs, service.resumeAfter, response.Body.String())
		}
	}
}

func TestConversationMessageRejectsLegacyFieldsAndInvalidRequest(t *testing.T) {
	service := &taskServiceStub{}
	engine := taskAPIRouter(service, nil)
	for _, body := range []string{
		`{"client_message_id":"different-key","content":"hello"}`,
		`{"client_message_id":"retry-key-009","mode":"new","content":"hello"}`,
		`{"client_message_id":"retry-key-009","expected_task_id":"q1234567","content":"hello"}`,
		`{"client_message_id":"retry-key-009","input_revision":3,"content":"hello"}`,
		`{"client_message_id":"retry-key-009","attachment_ids":["f1234567"],"content":"hello"}`,
	} {
		request := httptest.NewRequest(http.MethodPost, "/api/conversations/c1234567/messages", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "retry-key-009")
		request.Header.Set("X-Morde-Service-Instance-ID", "11111111-1111-4111-8111-111111111111")
		request.Header.Set("X-Morde-Actor-ID", "u1234567")
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || service.submitCalls != 0 || service.acceptCalls != 0 {
			t.Fatalf("invalid message body %s: status=%d submit_calls=%d accept_calls=%d", body, response.Code, service.submitCalls, service.acceptCalls)
		}
	}
}

func TestTaskMCPConnectorGrantRevokeUsesExpectedGrantVersion(t *testing.T) {
	service := &taskServiceStub{}
	engine := taskAPIRouter(service, nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/api/tasks/q1234567/mcp-connectors/kaaaaaaa?expected_grant_version=3", nil))
	if response.Code != http.StatusNoContent || service.revokeCalls != 1 || service.revokeTaskID != "q1234567" ||
		service.revokeConnectorID != "kaaaaaaa" || service.revokeVersion != 3 {
		t.Fatalf("status=%d calls=%d revoke=(%s,%s,%d) body=%s", response.Code, service.revokeCalls,
			service.revokeTaskID, service.revokeConnectorID, service.revokeVersion, response.Body.String())
	}
	for _, query := range []string{"", "?expected_grant_version=0", "?expected_grant_version=1&expected_grant_version=2"} {
		invalid := httptest.NewRecorder()
		engine.ServeHTTP(invalid, httptest.NewRequest(http.MethodDelete, "/api/tasks/q1234567/mcp-connectors/kaaaaaaa"+query, nil))
		if invalid.Code != http.StatusBadRequest || service.revokeCalls != 1 {
			t.Errorf("query %q status=%d calls=%d body=%s", query, invalid.Code, service.revokeCalls, invalid.Body.String())
		}
	}
}

func TestTaskEventCursorParsingAndHeaderResume(t *testing.T) {
	for _, value := range []string{"", "-1", "+1", "1x", "9223372036854775808"} {
		if _, err := parseEventCursor(value); err == nil {
			t.Errorf("parseEventCursor(%q) succeeded", value)
		}
	}
	gin.SetMode(gin.TestMode)
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/events?after=4", nil)
	context.Request.Header.Set("Last-Event-ID", "7")
	got, ok := eventCursor(context)
	if !ok || got != 7 {
		t.Fatalf("eventCursor() = %d, %t; want 7, true", got, ok)
	}
	context.Request = httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/events?after=9", nil)
	context.Request.Header.Set("Last-Event-ID", "7")
	got, ok = eventCursor(context)
	if !ok || got != 9 {
		t.Fatalf("eventCursor() = %d, %t; want 9, true", got, ok)
	}
}

func TestTaskEventsStreamOrderedSSEAndClearsWriteDeadline(t *testing.T) {
	service := &taskServiceStub{events: []task.Event{{
		TaskID: "q1234567", Sequence: 1, Type: "task.status", Payload: []byte(`{"status":"queued"}`), CreatedAt: time.Now().UTC(),
	}}}
	engine := taskAPIRouter(service, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/events?after=0", nil)
	requestContext, cancel := context.WithCancel(request.Context())
	request = request.WithContext(requestContext)
	timer := time.AfterFunc(25*time.Millisecond, cancel)
	defer func() {
		timer.Stop()
		cancel()
	}()
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.deadlineCalls != 1 {
		t.Fatalf("status=%d deadline calls=%d body=%s", response.Code, response.deadlineCalls, response.Body.String())
	}
	for _, expected := range []string{"id: 1", "event: task.status", `"sequence":1`, `"payload":{"status":"queued"}`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("SSE response missing %q: %s", expected, response.Body.String())
		}
	}
}

func TestTaskEventsResumeAfterLastEventIDAndStreamsAssistantDelta(t *testing.T) {
	service := &taskServiceStub{events: []task.Event{{
		TaskID: "q1234567", Sequence: 8, Type: "task.message",
		Payload:   []byte(`{"role":"assistant","message_id":"m1234567","turn_id":"s1234567","delta":"Hello "}`),
		CreatedAt: time.Now().UTC(),
	}}}
	engine := taskAPIRouter(service, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/events", nil)
	request.Header.Set("Last-Event-ID", "7")
	requestContext, cancel := context.WithCancel(request.Context())
	request = request.WithContext(requestContext)
	timer := time.AfterFunc(25*time.Millisecond, cancel)
	defer func() {
		timer.Stop()
		cancel()
	}()
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK || service.eventAfter != 7 {
		t.Fatalf("status=%d after=%d body=%s", response.Code, service.eventAfter, response.Body.String())
	}
	for _, expected := range []string{"id: 8", "event: task.message", `"message_id":"m1234567"`, `"delta":"Hello "`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("resumed SSE response missing %q: %s", expected, response.Body.String())
		}
	}
}

func TestTaskEventsClosesAfterReplayingTerminalCursor(t *testing.T) {
	service := &taskServiceStub{eventStatus: task.StatusCompleted, eventCurrentSequence: 1, events: []task.Event{{
		TaskID: "q1234567", Sequence: 1, Type: "task.status", Payload: []byte(`{"status":"completed"}`), CreatedAt: time.Now().UTC(),
	}}}
	engine := taskAPIRouter(service, nil)
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/events?after=0", nil))
	if response.Code != http.StatusOK || service.eventCalls != 1 || !strings.Contains(response.Body.String(), "id: 1\nevent: task.status") {
		t.Fatalf("terminal replay status=%d calls=%d body=%s", response.Code, service.eventCalls, response.Body.String())
	}
}

func TestTaskEventsStopsEventSourceWhenTerminalCursorIsCurrent(t *testing.T) {
	service := &taskServiceStub{eventStatus: task.StatusCancelled, eventCurrentSequence: 7}
	engine := taskAPIRouter(service, nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/events?after=7", nil))
	if response.Code != http.StatusNoContent || service.eventCalls != 1 || strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("terminal cursor status=%d calls=%d headers=%#v body=%s", response.Code, service.eventCalls, response.Header(), response.Body.String())
	}
}

func TestTaskEventsEmitsSnapshotWhenTerminalHistoryIsMissing(t *testing.T) {
	service := &taskServiceStub{eventStatus: task.StatusCompleted, eventCurrentSequence: 7}
	engine := taskAPIRouter(service, nil)
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/events?after=0", nil))
	if response.Code != http.StatusOK || response.deadlineCalls != 1 || service.eventCalls != 1 || !strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stale terminal cursor status=%d calls=%d headers=%#v body=%s", response.Code, service.eventCalls, response.Header(), response.Body.String())
	}
	for _, expected := range []string{"id: 7", "event: task.snapshot_required", `data: {"sequence":7}`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("snapshot recovery response missing %q: %s", expected, response.Body.String())
		}
	}
}

func TestTaskEventsEmitsSnapshotCursorWhenHistoryExpired(t *testing.T) {
	service := &taskServiceStub{
		eventCurrentSequence: 8,
		err:                  domain.NewError(domain.CodeEventCursorExpired, "event cursor has expired"),
	}
	engine := taskAPIRouter(service, nil)
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/events?after=3", nil))
	if response.Code != http.StatusOK || response.deadlineCalls != 1 || service.eventAfter != 3 ||
		!strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d deadline calls=%d after=%d headers=%#v body=%s", response.Code, response.deadlineCalls, service.eventAfter, response.Header(), response.Body.String())
	}
	for _, expected := range []string{"id: 8", "event: task.snapshot_required", `data: {"sequence":8}`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("snapshot recovery response missing %q: %s", expected, response.Body.String())
		}
	}
}

func TestTaskEventsEmitsSnapshotWhenCursorExpiresDuringStream(t *testing.T) {
	service := &taskServiceStub{
		eventCurrentSequence: 8,
		eventErrorAfterCall:  2,
		err:                  domain.NewError(domain.CodeEventCursorExpired, "event cursor has expired"),
	}
	engine := taskAPIRouter(service, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/events?after=3", nil)
	requestContext, cancel := context.WithCancel(request.Context())
	request = request.WithContext(requestContext)
	timer := time.AfterFunc(1500*time.Millisecond, cancel)
	defer func() {
		timer.Stop()
		cancel()
	}()
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.deadlineCalls != 1 || service.eventCalls != 2 {
		t.Fatalf("status=%d deadline calls=%d event calls=%d body=%s", response.Code, response.deadlineCalls, service.eventCalls, response.Body.String())
	}
	for _, expected := range []string{"id: 8", "event: task.snapshot_required", `data: {"sequence":8}`} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Errorf("snapshot recovery response missing %q: %s", expected, response.Body.String())
		}
	}
}

func TestTaskEventsDoNotExposeModelToolArgumentsOrDuplicateText(t *testing.T) {
	service := &taskServiceStub{events: []task.Event{{
		TaskID: "q1234567", Sequence: 1, Type: "task.step",
		Payload:   []byte(`{"step_id":"s1234567","detail":{"text":"assistant private text","tool_calls":[{"name":"lookup","arguments":"secret"}],"output":"private tool output","usage_known":true}}`),
		CreatedAt: time.Now().UTC(),
	}}}
	engine := taskAPIRouter(service, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/events?after=0", nil)
	requestContext, cancel := context.WithCancel(request.Context())
	request = request.WithContext(requestContext)
	timer := time.AfterFunc(25*time.Millisecond, cancel)
	defer func() {
		timer.Stop()
		cancel()
	}()
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	engine.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || strings.Contains(body, "secret") || strings.Contains(body, "assistant private text") ||
		strings.Contains(body, "tool_calls") || strings.Contains(body, "private tool output") || strings.Contains(body, `"output"`) {
		t.Fatalf("status=%d exposed internal model detail: %s", response.Code, body)
	}
	if !strings.Contains(body, `"usage_known":true`) {
		t.Fatalf("safe progress detail was removed: %s", body)
	}
}

func TestTaskStepEventSanitizationFailsClosedOnUnexpectedShapes(t *testing.T) {
	tests := []struct {
		name           string
		payload        string
		expectedPublic string
	}{
		{
			name:           "scalar detail",
			payload:        `{"step_id":"s1234567","output":"private root output","detail":"private scalar detail"}`,
			expectedPublic: `"detail":{}`,
		},
		{
			name:           "array detail",
			payload:        `{"step_id":"s1234567","detail":["private array detail"]}`,
			expectedPublic: `"detail":{}`,
		},
		{
			name:           "non-object root",
			payload:        `["private root payload"]`,
			expectedPublic: `"payload":{}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &taskServiceStub{eventStatus: task.StatusRunning, events: []task.Event{{
				TaskID: "q1234567", Sequence: 1, Type: "task.step", Payload: []byte(test.payload), CreatedAt: time.Now().UTC(),
			}}}
			engine := taskAPIRouter(service, nil)
			request := httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/events?after=0", nil)
			requestContext, cancel := context.WithCancel(request.Context())
			request = request.WithContext(requestContext)
			timer := time.AfterFunc(25*time.Millisecond, cancel)
			defer func() {
				timer.Stop()
				cancel()
			}()
			response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			engine.ServeHTTP(response, request)
			body := response.Body.String()
			if response.Code != http.StatusOK || !strings.Contains(body, "id: 1\nevent: task.step") ||
				!strings.Contains(body, test.expectedPublic) || strings.Contains(body, "private") {
				t.Fatalf("status=%d malformed Task event was leaked or dropped: %s", response.Code, body)
			}
		})
	}
}

func TestTaskEventsRejectInvalidBatchBeforeStartingSSE(t *testing.T) {
	now := time.Now().UTC()
	service := &taskServiceStub{events: []task.Event{
		{TaskID: "q1234567", Sequence: 1, Type: "task.status", Payload: []byte(`{"status":"queued"}`), CreatedAt: now},
		{TaskID: "q1234567", Sequence: 3, Type: "task.status", Payload: []byte(`{"status":"running"}`), CreatedAt: now},
	}}
	engine := taskAPIRouter(service, nil)
	response := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/tasks/q1234567/events?after=0", nil))
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "id: 1") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestIssueWorkbenchObservationTicketRequiresPortalSessionAndDoesNotCache(t *testing.T) {
	service := &observationTicketTaskServiceStub{
		taskServiceStub: &taskServiceStub{},
		value:           app.TaskWorkbenchObservationTicket{Ticket: "opaque-ticket", TaskID: "q1234567", HostID: "h1234567", ExpiresAt: time.Now().UTC().Add(time.Minute)},
	}
	engine := taskAPIRouter(service, nil)
	request := httptest.NewRequest(http.MethodPost, "/api/tasks/q1234567/workbench-observation-ticket", nil)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("ticket response = %d headers=%#v body=%s", response.Code, response.Header(), response.Body)
	}
	if service.calls != 1 || service.taskID != "q1234567" || service.principal.Kind != identity.KindSession || service.principal.UserID != "u1234567" {
		t.Fatalf("ticket service call = %#v, calls=%d", service.principal, service.calls)
	}
	var decoded app.TaskWorkbenchObservationTicket
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || decoded.Ticket != service.value.Ticket || decoded.HostID != service.value.HostID {
		t.Fatalf("ticket response decode = %#v err=%v", decoded, err)
	}
	for _, target := range []string{
		"/api/tasks/q1234567/workbench-observation-ticket?unexpected=1",
	} {
		bad := httptest.NewRecorder()
		engine.ServeHTTP(bad, httptest.NewRequest(http.MethodPost, target, nil))
		if bad.Code != http.StatusBadRequest {
			t.Errorf("ticket request %s = %d, want 400", target, bad.Code)
		}
	}
	badBody := httptest.NewRecorder()
	engine.ServeHTTP(badBody, httptest.NewRequest(http.MethodPost, "/api/tasks/q1234567/workbench-observation-ticket", strings.NewReader("{}")))
	if badBody.Code != http.StatusBadRequest || service.calls != 1 {
		t.Fatalf("ticket body request = %d calls=%d, want rejected before service", badBody.Code, service.calls)
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlineCalls int
}

func (w *deadlineRecorder) SetWriteDeadline(time.Time) error {
	w.deadlineCalls++
	return nil
}

func taskAPIRouter(tasks TaskService, conversations ConversationService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Set(principalKey, identity.Principal{UserID: "u1234567", Role: identity.RoleUser, Kind: identity.KindSession, TokenID: "t1234567"})
		c.Next()
	})
	group := engine.Group("/api")
	handler := &api{}
	handler.registerTasks(group, tasks)
	if ticketService, ok := tasks.(TaskWorkbenchObservationTicketService); ok {
		handler.registerTaskWorkbenchObservationTickets(group, ticketService)
	}
	handler.registerConversations(group, conversations, tasks)
	return engine
}

func apiTestTask() task.Task {
	now := time.Now().UTC()
	return task.Task{
		ID: "q1234567", OwnerID: "u1234567", HostID: "h1234567", ExecutionLocation: "server", ConversationID: "c1234567",
		ModelID: "model-a", Objective: "summarize files", Status: task.StatusQueued,
		Budget:    task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000},
		CreatedAt: now, UpdatedAt: now,
	}
}
