package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/control"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresTaskSubmissionAppendAndReplay(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)

	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "create-task", "first work")
	if submission.Task.ConversationID != "" || submission.Conversation != nil {
		t.Fatal("test requires a task with no conversation so the store creates one atomically")
	}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit task without conversation: %v", err)
	}
	if created.Task.ConversationID == "" || created.Conversation.ID != created.Task.ConversationID || created.Conversation.HostID != hostValue.ID ||
		created.Message.TaskID != created.Task.ID || created.Message.ConversationID != created.Conversation.ID {
		t.Fatalf("created associations = %#v", created)
	}

	replayed, err := store.SubmitTask(ctx, submission)
	if err != nil || !replayed.Replayed || replayed.Task.ID != created.Task.ID || replayed.Message.ID != created.Message.ID || replayed.Conversation.ID != created.Conversation.ID {
		t.Fatalf("idempotent replay = %#v, err=%v", replayed, err)
	}

	different := submission
	different.Idempotency.RequestHash = hashBytes("different request")
	if _, err := store.SubmitTask(ctx, different); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("different request under same key error = %v", err)
	}

	appendRequest := app.TaskSubmission{
		Task:        task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, HostID: hostValue.ID, ModelID: "model-test"},
		Message:     conversation.Message{ID: mustID(t, domain.NewMessageID), ConversationID: created.Conversation.ID, OwnerID: owner.ID, Role: conversation.RoleUser, Content: "continue"},
		Idempotency: testIdempotency(owner.ID, "append-message", "append-task-message"),
	}
	appended, err := store.AppendMessage(ctx, appendRequest)
	if err != nil || appended.Task.ID != created.Task.ID || appended.Message.TaskID != created.Task.ID || appended.Task.Status != task.StatusQueued {
		t.Fatalf("append to active task = %#v, err=%v", appended, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, created.Task.ID, 1, 10)
	if err != nil || len(events) != 1 || events[0].Sequence != 2 || events[0].Type != "task.message" {
		t.Fatalf("append event = %#v, err=%v", events, err)
	}
	eventPage, err := store.GetTaskEventPage(ctx, owner.ID, created.Task.ID, 1, 10)
	if err != nil || eventPage.HostID != hostValue.ID || eventPage.Status != task.StatusQueued || eventPage.CurrentSequence != 2 ||
		len(eventPage.Items) != 1 || eventPage.Items[0].Sequence != 2 {
		t.Fatalf("event page = %#v, err=%v", eventPage, err)
	}

	mismatch := appendRequest
	mismatch.Task.ModelID = "different-model"
	mismatch.Message.ID = mustID(t, domain.NewMessageID)
	mismatch.Message.Content = "must not be persisted"
	mismatch.Idempotency = testIdempotency(owner.ID, "append-message", "append-mismatch")
	if _, err := store.AppendMessage(ctx, mismatch); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("mismatched model error = %v", err)
	}

	otherHost := createTestHost(t, ctx, store, owner.ID, app.Capacity{CPUMilli: 100_000, MemMiB: 200_000}, nil)
	mismatchHost := appendRequest
	mismatchHost.Task.HostID = otherHost.ID
	mismatchHost.Message.ID = mustID(t, domain.NewMessageID)
	mismatchHost.Message.Content = "must not cross hosts"
	mismatchHost.Idempotency = testIdempotency(owner.ID, "append-message", "append-host-mismatch")
	if _, err := store.AppendMessage(ctx, mismatchHost); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("mismatched Host error = %v", err)
	}
	messages, err := store.ListMessages(ctx, owner.ID, created.Conversation.ID, app.PageRequest{Limit: 20})
	if err != nil || len(messages.Items) != 2 {
		t.Fatalf("messages after rejected append = %d, err=%v", len(messages.Items), err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='pausing' WHERE id=$1`, created.Task.ID); err != nil {
		t.Fatal(err)
	}
	pausingMessage := appendRequest
	pausingMessage.Message.ID = mustID(t, domain.NewMessageID)
	pausingMessage.Idempotency = testIdempotency(owner.ID, "append-message", "append-during-pause")
	if _, err := store.AppendMessage(ctx, pausingMessage); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("append during pause error = %v", err)
	}
	messages, err = store.ListMessages(ctx, owner.ID, created.Conversation.ID, app.PageRequest{Limit: 20})
	if err != nil || len(messages.Items) != 2 {
		t.Fatalf("messages after rejected append during pause = %d, err=%v", len(messages.Items), err)
	}

	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='completed', finished_at=clock_timestamp() WHERE id=$1`, created.Task.ID); err != nil {
		t.Fatal(err)
	}
	newTask := newTestTaskSubmission(t, owner.ID, hostValue.ID, "append-after-complete", "new task after completion")
	newTask.Task.ConversationID = created.Conversation.ID
	newTask.Message.ConversationID = created.Conversation.ID
	newTask.Conversation = nil
	newTask.Idempotency.RequestHash = hashBytes("completed conversation new request")
	newTask.Idempotency.Key = "append-after-complete"
	newTask.Idempotency.Operation = "append-message"
	result, err := store.AppendMessage(ctx, newTask)
	if err != nil || result.Task.ID != newTask.Task.ID || result.Task.ID == created.Task.ID {
		t.Fatalf("append after completion should create a new task: %#v, err=%v", result, err)
	}
}

func TestPostgresClientTaskAuthorizationSnapshotSurvivesReloadAndRevocation(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	installationID := createTestClientInstallation(t, store.pool, owner.ID, nil)
	clientToken := clientTestToken(t, owner.ID, installationID, identity.KindClient, nil)
	if _, err := store.CreateToken(ctx, clientToken); err != nil {
		t.Fatalf("create client token: %v", err)
	}

	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "client-task-snapshot", "run from a client")
	submission.Task.Authorization = task.Authorization{
		Kind: string(identity.KindClient), TokenID: clientToken.ID, ClientInstallationID: installationID,
	}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit client task: %v", err)
	}
	if created.Task.Authorization.ClientInstallationID != installationID {
		t.Fatalf("submitted task installation = %q, want %q", created.Task.Authorization.ClientInstallationID, installationID)
	}

	loaded, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || loaded.Task.Authorization.ClientInstallationID != installationID {
		t.Fatalf("reloaded task authorization = %#v, err=%v", loaded.Task.Authorization, err)
	}

	appendRequest := app.TaskSubmission{
		Task: task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, HostID: hostValue.ID, ModelID: submission.Task.ModelID},
		Message: conversation.Message{
			ID: mustID(t, domain.NewMessageID), ConversationID: created.Conversation.ID, OwnerID: owner.ID,
			Role: conversation.RoleUser, Content: "continue from the same client",
		},
		Idempotency: testIdempotency(owner.ID, "append-message", "client-task-append"),
	}
	appended, err := store.AppendMessage(ctx, appendRequest)
	if err != nil || appended.Task.ID != created.Task.ID || appended.Task.Authorization.ClientInstallationID != installationID {
		t.Fatalf("append preserved client authorization = %#v, err=%v", appended.Task.Authorization, err)
	}

	claimed, found, err := store.ClaimNextTask(ctx, "client-task-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID || claimed.Authorization.ClientInstallationID != installationID {
		t.Fatalf("claimed client task = %#v, found=%t, err=%v", claimed, found, err)
	}
	audit, err := app.NewAuditService(store)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := app.NewCredentialService(store, store, store, audit, app.CredentialOptions{
		SessionTTL: time.Hour, TicketTTL: time.Minute, LoginLimit: 2, LoginWindow: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := credentials.AuthorizeTaskAction(ctx, owner.ID, identity.Kind(claimed.Authorization.Kind),
		claimed.Authorization.TokenID, hostValue.ID, nil, identity.ScopeTaskSubmit); err != nil {
		t.Fatalf("authorize task before installation revocation: %v", err)
	}
	if err := store.RevokeClientInstallation(ctx, installationID, owner.ID, "127.0.0.1"); err != nil {
		t.Fatalf("revoke installation: %v", err)
	}
	if err := credentials.AuthorizeTaskAction(ctx, owner.ID, identity.Kind(claimed.Authorization.Kind),
		claimed.Authorization.TokenID, hostValue.ID, nil, identity.ScopeTaskSubmit); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("authorize task after installation revocation = %v, want unauthenticated", err)
	}
}

func TestPostgresTaskApplicationTargetRequiresCurrentLaunchAndSurvivesClaim(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	if err := store.SetRuntimeInstanceID(ctx, hostValue.ID, hostValue.RuntimeGeneration, hostValue.ControlVersion, "", "runtime-task-app"); err != nil {
		t.Fatal(err)
	}
	hostValue.RuntimeInstanceID = "runtime-task-app"
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "app-target", "find a message")
	submission.Task.ApplicationID = app.WechatApplicationID
	if _, err := store.SubmitTask(ctx, submission); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("submit without a running managed application error = %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO host_application_launches
		(host_id, owner_id, launch_id, application_id, runtime_generation, runtime_instance_id, control_version, status)
		VALUES ($1,$2,'launch_tasktarget01','wechat',$3,$4,$5,'running')`,
		hostValue.ID, owner.ID, hostValue.RuntimeGeneration, hostValue.RuntimeInstanceID, hostValue.ControlVersion); err != nil {
		t.Fatal(err)
	}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil || created.Task.ApplicationID != app.WechatApplicationID {
		t.Fatalf("submit application task = %#v, err=%v", created, err)
	}
	loaded, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || loaded.Task.ApplicationID != app.WechatApplicationID {
		t.Fatalf("load application task = %#v, err=%v", loaded.Task, err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "application-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID || claimed.ApplicationID != app.WechatApplicationID {
		t.Fatalf("claim application task = %#v, found=%t, err=%v", claimed, found, err)
	}
	appendRequest := app.TaskSubmission{
		Task: task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, HostID: hostValue.ID,
			ModelID: submission.Task.ModelID},
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), ConversationID: created.Conversation.ID,
			OwnerID: owner.ID, Role: conversation.RoleUser, Content: "continue"},
		Idempotency: testIdempotency(owner.ID, "append-message", "app-target-mismatch"),
	}
	if _, err := store.AppendMessage(ctx, appendRequest); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("append without active task application target error = %v", err)
	}
	appendRequest.Task.ApplicationID = app.WechatApplicationID
	appendRequest.Idempotency = testIdempotency(owner.ID, "append-message", "app-target-match")
	appended, err := store.AppendMessage(ctx, appendRequest)
	if err != nil || appended.Task.ID != created.Task.ID || appended.Task.ApplicationID != app.WechatApplicationID {
		t.Fatalf("append to application task = %#v, err=%v", appended, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE host_application_launches SET status='stopped' WHERE host_id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	newSubmission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "app-target-stopped", "another message")
	newSubmission.Task.ApplicationID = app.WechatApplicationID
	if _, err := store.SubmitTask(ctx, newSubmission); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("submit after application stop error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE host_application_launches SET status='running', control_version=control_version+1 WHERE host_id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitTask(ctx, newSubmission); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("submit with stale application Host fence error = %v", err)
	}
}

func TestPostgresTaskEventPageDetectsAllEventsExpired(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "create-task", "event retention")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM task_events WHERE task_id=$1`, created.Task.ID); err != nil {
		t.Fatalf("remove events to simulate retention: %v", err)
	}
	page, err := store.GetTaskEventPage(ctx, owner.ID, created.Task.ID, 0, 10)
	if domain.CodeOf(err) != domain.CodeEventCursorExpired {
		t.Fatalf("event page with missing history error = %v, want expired cursor", err)
	}
	if page.HostID != hostValue.ID || page.Status != task.StatusQueued || page.CurrentSequence < 1 {
		t.Fatalf("expired event page lost authorized snapshot cursor: %#v", page)
	}
}

func TestPostgresAssistantTextDeltasAreOrderedLeasedAndFinalizedAtomically(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "streaming-assistant", "write a summary"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "streaming-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("ClaimNextTask() = %#v, found=%t, err=%v", claimed, found, err)
	}
	messageID := mustID(t, domain.NewMessageID)
	turnID := mustID(t, domain.NewStepID)
	for _, delta := range []string{"Hello ", "world."} {
		if _, err := store.AppendAssistantTextDelta(ctx, app.AssistantTextDelta{
			TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
			MessageID: messageID, TurnID: turnID, Delta: delta,
		}); err != nil {
			t.Fatalf("AppendAssistantTextDelta(%q): %v", delta, err)
		}
	}
	step := task.Step{
		ID: turnID, TaskID: claimed.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded,
		Title: "Ask the model", Summary: "Received a complete response.",
	}
	finalMessage := conversation.Message{
		ID: messageID, ConversationID: created.Conversation.ID, OwnerID: owner.ID,
		Role: conversation.RoleAssistant, Content: "Hello world.", TaskID: claimed.ID,
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusRunning, CurrentStep: turnID, Steps: []task.Step{step},
		Event:            task.Event{TaskID: claimed.ID, Type: "task.step", Payload: []byte(`{"step_id":"` + turnID + `","status":"succeeded"}`)},
		AssistantMessage: &finalMessage,
	}); err != nil {
		t.Fatalf("commit completed provider response: %v", err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, claimed.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 6 {
		t.Fatalf("event history = %#v; want submit, claim, two deltas, step and final message", events)
	}
	for index, event := range events {
		if event.Sequence != int64(index+1) {
			t.Fatalf("event[%d] sequence = %d, want %d", index, event.Sequence, index+1)
		}
	}
	for index, want := range []string{"Hello ", "world."} {
		if events[index+2].Type != "task.message" {
			t.Fatalf("delta event[%d] type = %q", index, events[index+2].Type)
		}
		var payload map[string]any
		if err := json.Unmarshal(events[index+2].Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["message_id"] != messageID || payload["turn_id"] != turnID || payload["delta"] != want || payload["done"] != nil {
			t.Fatalf("delta payload = %#v", payload)
		}
	}
	if events[4].Type != "task.step" || events[5].Type != "task.message" {
		t.Fatalf("terminal event order = %q, %q", events[4].Type, events[5].Type)
	}
	var finalPayload map[string]any
	if err := json.Unmarshal(events[5].Payload, &finalPayload); err != nil {
		t.Fatal(err)
	}
	if finalPayload["message_id"] != messageID || finalPayload["turn_id"] != turnID || finalPayload["content"] != finalMessage.Content || finalPayload["done"] != true {
		t.Fatalf("final assistant event payload = %#v", finalPayload)
	}
	messages, err := store.ListMessages(ctx, owner.ID, created.Conversation.ID, app.PageRequest{Limit: 20})
	var storedAssistant *conversation.Message
	for index := range messages.Items {
		if messages.Items[index].Role == conversation.RoleAssistant {
			storedAssistant = &messages.Items[index]
		}
	}
	if err != nil || len(messages.Items) != 2 || storedAssistant == nil || storedAssistant.ID != messageID || storedAssistant.Content != finalMessage.Content {
		t.Fatalf("stored messages = %#v, err=%v", messages.Items, err)
	}
	if _, err := store.AppendAssistantTextDelta(ctx, app.AssistantTextDelta{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version + 1,
		MessageID: messageID, TurnID: turnID, Delta: "stale",
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale lease delta error = %v, want invalid state", err)
	}
}

func TestPostgresPendingApprovalResponseIsVersionedAndAtomic(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "pending-submit-key", "approve an external action")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "pending-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("ClaimNextTask() = %+v, %t, %v", claimed, found, err)
	}
	stepID := mustID(t, domain.NewStepID)
	pendingID := mustID(t, domain.NewPendingRequestID)
	request := &task.PendingRequest{
		ID: pendingID, TaskID: claimed.ID, Version: 4, Kind: "approval", Title: "Approve export",
		Object: "report.txt", Content: "Export this file?", Impact: "The file will leave this Host.",
		CallID: "call-export", ToolName: "file_export", RequestHash: hashBytes("exact-export-call"),
		RuntimeGeneration: 2, RuntimeInstanceID: "runtime-instance-2", ControlVersion: 3, Status: task.PendingRequestPending,
	}
	usage := task.Usage{ActiveSeconds: 3, ToolCalls: 1}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "approval_required", Usage: usage,
		Steps: []task.Step{{ID: stepID, TaskID: claimed.ID, Sequence: 1, Kind: "tool", Status: task.StepWaitingUser, Title: request.Title, Summary: request.Content}},
		Event: task.Event{TaskID: claimed.ID, Type: "task.waiting_user", Payload: []byte(`{"reason":"approval_required"}`)}, PendingRequest: request,
	}); err != nil {
		t.Fatal(err)
	}
	details, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.PendingRequest == nil || details.PendingRequest.ID != pendingID || details.PendingRequest.Version != 4 || details.PendingRequest.Status != task.PendingRequestPending {
		t.Fatalf("GetTask() pending = %#v, err=%v", details.PendingRequest, err)
	}
	record := app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "task.pending.respond", Key: "pending-response-key",
		RequestHash: hashBytes("approve:" + pendingID + ":4"), ResourceID: claimed.ID, RelatedID: pendingID,
	}
	if err := store.RespondToPendingRequest(ctx, record, claimed.ID, pendingID, 4, task.PendingDecisionApprove); err != nil {
		t.Fatal(err)
	}
	if err := store.RespondToPendingRequest(ctx, record, claimed.ID, pendingID, 4, task.PendingDecisionApprove); err != nil {
		t.Fatalf("idempotent response replay: %v", err)
	}
	details, err = store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusQueued || details.PendingRequest == nil ||
		details.PendingRequest.Status != task.PendingRequestApproved || details.PendingRequest.Decision != task.PendingDecisionApprove {
		t.Fatalf("task after response = status %q pending %#v err=%v", details.Task.Status, details.PendingRequest, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, claimed.ID, 0, 20)
	if err != nil || len(events) != 4 || events[3].Type != "task.response" || events[3].Sequence != 4 {
		t.Fatalf("response event history = %#v err=%v", events, err)
	}
	stale := app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "task.pending.respond", Key: "pending-response-stale",
		RequestHash: hashBytes("stale"), ResourceID: claimed.ID, RelatedID: pendingID,
	}
	if err := store.RespondToPendingRequest(ctx, stale, claimed.ID, pendingID, 3, task.PendingDecisionReject); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale response error = %v, want conflict", err)
	}
}

func TestPostgresClarificationAnswerIsVersionedIdempotentAndRestorable(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "clarification-submit", "deploy the project"))
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "clarification-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("ClaimNextTask() = %#v, found=%t, err=%v", claimed, found, err)
	}
	modelStepID := mustID(t, domain.NewStepID)
	stepID := mustID(t, domain.NewStepID)
	requestID := mustID(t, domain.NewPendingRequestID)
	request := &task.ClarificationRequest{
		ID: requestID, TaskID: claimed.ID, Version: 1, ModelStepID: modelStepID, CallID: "call-ask-user",
		Question: "Which environment should I use?", Status: task.ClarificationPending,
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: stepID, WaitingReason: "clarification_required", Usage: task.Usage{ActiveSeconds: 2, ToolCalls: 1},
		Steps: []task.Step{
			{ID: modelStepID, TaskID: claimed.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded, Title: "Ask the model", Summary: "Model asked the user."},
			{ID: stepID, TaskID: claimed.ID, Sequence: 2, Kind: "clarification", Status: task.StepWaitingUser, Title: "The Agent needs one detail", Summary: request.Question},
		},
		Event: task.Event{TaskID: claimed.ID, Type: "task.waiting_user", Payload: []byte(`{"reason":"clarification_required"}`)}, ClarificationRequest: request,
	}); err != nil {
		t.Fatal(err)
	}
	details, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusWaitingUser || details.Task.WaitingReason != "clarification_required" ||
		len(details.ClarificationRequests) != 1 || details.ClarificationRequests[0].ID != requestID || details.ClarificationRequests[0].Status != task.ClarificationPending {
		t.Fatalf("GetTask() clarification = task %#v requests %#v err=%v", details.Task, details.ClarificationRequests, err)
	}
	answer := "Use staging."
	record := app.IdempotencyRecord{
		OwnerID: owner.ID, Operation: "task.clarification.answer", Key: "clarification-answer-key",
		RequestHash: hashBytes("answer:" + answer), ResourceID: claimed.ID, RelatedID: requestID,
	}
	if err := store.AnswerClarificationRequest(ctx, record, claimed.ID, requestID, 1, answer); err != nil {
		t.Fatal(err)
	}
	if err := store.AnswerClarificationRequest(ctx, record, claimed.ID, requestID, 1, answer); err != nil {
		t.Fatalf("idempotent answer replay: %v", err)
	}
	details, err = store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil || details.Task.Status != task.StatusQueued || details.Task.WaitingReason != "" || details.Task.Lease.Version != claimed.Lease.Version+1 ||
		len(details.ClarificationRequests) != 1 || details.ClarificationRequests[0].Status != task.ClarificationAnswered || details.ClarificationRequests[0].Answer != answer {
		t.Fatalf("task after clarification answer = task %#v requests %#v err=%v", details.Task, details.ClarificationRequests, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, claimed.ID, 0, 20)
	if err != nil || len(events) == 0 || events[len(events)-1].Type != "task.response" || !json.Valid(events[len(events)-1].Payload) {
		t.Fatalf("clarification response event = %#v err=%v", events, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[len(events)-1].Payload, &payload); err != nil || payload["clarification_id"] != requestID || payload["answer"] != answer {
		t.Fatalf("response event payload = %#v err=%v", payload, err)
	}
	stale := record
	stale.Key = "clarification-answer-stale"
	stale.RequestHash = hashBytes("stale")
	if err := store.AnswerClarificationRequest(ctx, stale, claimed.ID, requestID, 2, "other answer"); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale clarification version error = %v", err)
	}
}

func TestPostgresTaskSubmissionRollbackAndHostScopedLists(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "create-task", "initial task"))
	if err != nil {
		t.Fatal(err)
	}

	conversationID := mustID(t, domain.NewConversationID)
	failed := newTestTaskSubmission(t, owner.ID, hostValue.ID, "rollback", "must roll back")
	failed.Task.ConversationID = conversationID
	failed.Conversation = &conversation.Conversation{ID: conversationID, OwnerID: owner.ID, HostID: hostValue.ID, Title: "rollback"}
	failed.Message.ConversationID = conversationID
	failed.Message.ID = created.Message.ID
	_, err = store.SubmitTask(ctx, failed)
	if err == nil {
		t.Fatal("expected duplicate message id to abort submission")
	}
	if _, err := store.GetConversation(ctx, conversationID, owner.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("conversation survived failed transaction: %v", err)
	}
	if _, err := store.GetTask(ctx, failed.Task.ID, owner.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("task survived failed transaction: %v", err)
	}
	var reserved int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE owner_id=$1 AND key=$2`, owner.ID, failed.Idempotency.Key).Scan(&reserved); err != nil || reserved != 0 {
		t.Fatalf("idempotency reservation survived failed transaction: count=%d err=%v", reserved, err)
	}

	all, err := store.ListConversations(ctx, owner.ID, nil, app.PageRequest{Limit: 20})
	if err != nil || len(all.Items) != 1 {
		t.Fatalf("owner conversation list = %#v, err=%v", all, err)
	}
	otherOwner := createTestUser(t, store, 8, 20_000, 40_000)
	if _, err := store.GetConversation(ctx, created.Conversation.ID, otherOwner.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner conversation read error = %v", err)
	}
	if _, err := store.GetTask(ctx, created.Task.ID, otherOwner.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner task read error = %v", err)
	}
	if _, err := store.ListTaskEvents(ctx, otherOwner.ID, created.Task.ID, 0, 10); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner event read error = %v", err)
	}
	none, err := store.ListConversations(ctx, owner.ID, []string{}, app.PageRequest{Limit: 20})
	if err != nil || len(none.Items) != 0 {
		t.Fatalf("empty Host scope conversation list = %#v, err=%v", none, err)
	}
	tasks, err := store.ListTasks(ctx, owner.ID, app.TaskListRequest{HostIDs: []string{hostValue.ID}, Status: task.StatusQueued, Page: app.PageRequest{Limit: 20}})
	if err != nil || len(tasks.Items) != 1 || tasks.Items[0].ID != created.Task.ID {
		t.Fatalf("Host scoped task list = %#v, err=%v", tasks, err)
	}
	noTasks, err := store.ListTasks(ctx, owner.ID, app.TaskListRequest{HostIDs: []string{}, Page: app.PageRequest{Limit: 20}})
	if err != nil || len(noTasks.Items) != 0 {
		t.Fatalf("empty Host scope task list = %#v, err=%v", noTasks, err)
	}
}

func TestPostgresAITaskWritesRequireRunningAIHostAndPreserveReplays(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)

	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "fenced-submit", "initial work")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit while Host is AI-controlled: %v", err)
	}
	appendRequest := app.TaskSubmission{
		Task: task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, HostID: hostValue.ID, ModelID: "model-test"},
		Message: conversation.Message{
			ID: mustID(t, domain.NewMessageID), ConversationID: created.Conversation.ID, OwnerID: owner.ID,
			Role: conversation.RoleUser, Content: "continue",
		},
		Idempotency: testIdempotency(owner.ID, "append-message", "fenced-append"),
	}
	appended, err := store.AppendMessage(ctx, appendRequest)
	if err != nil || appended.Task.ID != created.Task.ID {
		t.Fatalf("append while Host is AI-controlled = %#v, err=%v", appended, err)
	}

	transition, err := store.BeginControlTransition(ctx, control.BeginRequest{
		HostID: hostValue.ID, OwnerID: owner.ID, RequestID: "task-write-fence",
		Action: control.ActionTakeOver, ExpectedControlVersion: 0,
	})
	if err != nil || transition.Phase != control.PhaseBlocking {
		t.Fatalf("begin control takeover = %#v, err=%v", transition, err)
	}

	// A retry with the same idempotency key must still return the committed
	// result after the Host fence changes to taking_over.
	replayedSubmit, err := store.SubmitTask(ctx, submission)
	if err != nil || !replayedSubmit.Replayed || replayedSubmit.Task.ID != created.Task.ID {
		t.Fatalf("submit replay after takeover = %#v, err=%v", replayedSubmit, err)
	}
	replayedAppend, err := store.AppendMessage(ctx, appendRequest)
	if err != nil || !replayedAppend.Replayed || replayedAppend.Message.ID != appendRequest.Message.ID {
		t.Fatalf("append replay after takeover = %#v, err=%v", replayedAppend, err)
	}

	blockedSubmit := newTestTaskSubmission(t, owner.ID, hostValue.ID, "blocked-submit", "must not start")
	if _, err := store.SubmitTask(ctx, blockedSubmit); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("new task submission during takeover error = %v", err)
	}
	blockedAppend := appendRequest
	blockedAppend.Message.ID = mustID(t, domain.NewMessageID)
	blockedAppend.Message.Content = "must not be appended"
	blockedAppend.Idempotency = testIdempotency(owner.ID, "append-message", "blocked-append")
	if _, err := store.AppendMessage(ctx, blockedAppend); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("new message append during takeover error = %v", err)
	}
	if claimedTask, claimed, err := store.ClaimNextTask(ctx, "worker-fenced", time.Minute); err != nil || claimed {
		t.Fatalf("claim during takeover = %#v, claimed=%t, err=%v", claimedTask, claimed, err)
	}
	messages, err := store.ListMessages(ctx, owner.ID, created.Conversation.ID, app.PageRequest{Limit: 20})
	if err != nil || len(messages.Items) != 2 {
		t.Fatalf("messages after rejected append = %d, err=%v", len(messages.Items), err)
	}
}

func TestPostgresAITaskWritesRequireRunningHost(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	if _, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "running-host-seed", "queued work")); err != nil {
		t.Fatalf("submit initial queued task: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET state='failed' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "failed-host-submit", "must not start")); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("task submission on a non-running Host error = %v", err)
	}
	if claimedTask, claimed, err := store.ClaimNextTask(ctx, "worker-failed-host", time.Minute); err != nil || claimed {
		t.Fatalf("claim on a non-running Host = %#v, claimed=%t, err=%v", claimedTask, claimed, err)
	}
}

func TestPostgresAITaskFenceSerializesConcurrentSubmissionAndClaim(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	queued, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "fence-race-existing", "queued work"))
	if err != nil {
		t.Fatal(err)
	}

	// Hold the same exclusive Host lock BeginControlTransition uses, then race
	// both task creation and claiming against the fence installation.
	fenceTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fenceTx.Rollback(context.Background()) }()
	if err := fenceTx.QueryRow(ctx, `SELECT id FROM hosts WHERE id=$1 FOR UPDATE`, hostValue.ID).Scan(new(string)); err != nil {
		t.Fatalf("lock Host control fence: %v", err)
	}

	type submitResult struct {
		value app.TaskSubmissionResult
		err   error
	}
	type claimResult struct {
		value   task.Task
		claimed bool
		err     error
	}
	started := make(chan struct{}, 2)
	submitted := make(chan submitResult, 1)
	claimed := make(chan claimResult, 1)
	newSubmission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "fence-race-new", "blocked by takeover")
	go func() {
		started <- struct{}{}
		value, err := store.SubmitTask(ctx, newSubmission)
		submitted <- submitResult{value: value, err: err}
	}()
	go func() {
		started <- struct{}{}
		value, didClaim, err := store.ClaimNextTask(ctx, "worker-fence-race", time.Minute)
		claimed <- claimResult{value: value, claimed: didClaim, err: err}
	}()
	<-started
	<-started
	waitForPostgresLockWait(t, ctx, pool, "%FROM hosts WHERE id=$1 AND owner_id=$2 FOR SHARE%", 2)

	if _, err := fenceTx.Exec(ctx, `UPDATE hosts SET control_state='taking_over', control_version=1 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if err := fenceTx.Commit(ctx); err != nil {
		t.Fatalf("commit Host control fence: %v", err)
	}
	select {
	case result := <-submitted:
		if domain.CodeOf(result.err) != domain.CodeInvalidState {
			t.Fatalf("racing task submission error = %v", result.err)
		}
	case <-ctx.Done():
		t.Fatalf("task submission remained blocked after fence commit: %v", ctx.Err())
	}
	select {
	case result := <-claimed:
		if result.err != nil || result.claimed {
			t.Fatalf("racing task claim = %#v, claimed=%t, err=%v", result.value, result.claimed, result.err)
		}
	case <-ctx.Done():
		t.Fatalf("task claim remained blocked after fence commit: %v", ctx.Err())
	}
	var taskCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE host_id=$1`, hostValue.ID).Scan(&taskCount); err != nil || taskCount != 1 {
		t.Fatalf("tasks persisted after fenced race = %d, err=%v", taskCount, err)
	}
	stored, err := store.GetTask(ctx, queued.Task.ID, owner.ID)
	if err != nil || stored.Task.Status != task.StatusQueued || stored.Task.Lease.Version != 0 {
		t.Fatalf("queued task after fenced race = %#v, err=%v", stored.Task, err)
	}
}

func waitForPostgresLockWait(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queryPattern string, want int) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `
SELECT count(*) FROM pg_stat_activity
WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1`, queryPattern).Scan(&waiting); err != nil {
			t.Fatalf("inspect PostgreSQL lock waiters: %v", err)
		}
		if waiting >= want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %d PostgreSQL lock waiters; got %d", want, waiting)
		case <-ticker.C:
		}
	}
}

func TestPostgresTaskLeaseFencesOperationAndRecoversUnknown(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id='ri_test' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "lease-task", "run this"))
	if err != nil {
		t.Fatal(err)
	}
	first, claimed, err := store.ClaimNextTask(ctx, "worker-one", time.Minute)
	if err != nil || !claimed || first.ID != submitted.Task.ID || first.Lease.Version != 1 {
		t.Fatalf("first task claim = %#v, claimed=%t, err=%v", first, claimed, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET control_state='user', control_version=1 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	blockedOperation := task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: first.ID, Key: "blocked-during-takeover", RequestHash: hashBytes("blocked"),
		HostID: hostValue.ID, RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test",
		ControlVersion: 0, LeaseVersion: first.Lease.Version, RuntimeLeaseVersion: first.Lease.Version, Status: task.OperationPrepared,
	}
	if _, _, err := store.CreateOperation(ctx, blockedOperation, "worker-one"); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("create operation during user control error = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET control_state='ai', control_version=0 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	operationID := mustID(t, domain.NewOperationID)
	operation, replayed, err := store.CreateOperation(ctx, task.Operation{
		ID: operationID, TaskID: first.ID, Key: "browser.click", RequestHash: hashBytes("click"),
		HostID: hostValue.ID, WorkspaceID: "w1234567", BindingVersion: 3,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test",
		ControlVersion: 0, LeaseVersion: first.Lease.Version, RuntimeLeaseVersion: first.Lease.Version, Status: task.OperationPrepared,
	}, "worker-one")
	if err != nil || replayed || operation.Status != task.OperationPrepared || operation.WorkspaceID != "w1234567" || operation.BindingVersion != 3 {
		t.Fatalf("create prepared operation = %#v, replayed=%t, err=%v", operation, replayed, err)
	}
	loadedOperation, err := store.GetOperation(ctx, owner.ID, first.ID, operation.Key)
	if err != nil || loadedOperation.ID != operation.ID || loadedOperation.WorkspaceID != operation.WorkspaceID || loadedOperation.BindingVersion != operation.BindingVersion {
		t.Fatalf("get owned operation = %#v, err=%v", loadedOperation, err)
	}
	changedBinding := operation
	changedBinding.BindingVersion++
	changedBinding.ID = mustID(t, domain.NewOperationID)
	if _, _, err := store.CreateOperation(ctx, changedBinding, "worker-one"); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("replay with changed Workspace binding version error = %v", err)
	}
	otherOwner := createTestUser(t, store, 8, 20_000, 40_000)
	if _, err := store.GetOperation(ctx, otherOwner.ID, first.ID, operation.Key); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner operation lookup error = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET control_state='user', control_version=1 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: first.ID, OperationKey: operation.Key, LeaseOwner: "worker-one", LeaseVersion: first.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: first.Lease.Version, Status: task.OperationDispatched,
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("dispatch operation during user control error = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET control_state='ai', control_version=0 WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	operation, err = store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: first.ID, OperationKey: operation.Key, LeaseOwner: "worker-one", LeaseVersion: first.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: first.Lease.Version, Status: task.OperationDispatched,
	})
	if err != nil || operation.Status != task.OperationDispatched {
		t.Fatalf("dispatch operation = %#v, err=%v", operation, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	second, claimed, err := store.ClaimNextTask(ctx, "worker-two", time.Minute)
	if err != nil || !claimed || second.Lease.Version != 2 {
		t.Fatalf("recovered task claim = %#v, claimed=%t, err=%v", second, claimed, err)
	}
	if _, err := store.RenewTaskLease(ctx, first.ID, "worker-one", first.Lease.Version, time.Minute); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale lease renewal error = %v", err)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: first.ID, LeaseOwner: "worker-one", LeaseVersion: first.Lease.Version, Status: task.StatusCompleted,
		Event: task.Event{Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale progress commit error = %v", err)
	}
	unknownOperations, err := store.ListUnknownOperations(ctx, owner.ID, first.ID)
	if err != nil || len(unknownOperations) != 1 || unknownOperations[0].Key != operation.Key || unknownOperations[0].LeaseVersion != second.Lease.Version ||
		unknownOperations[0].WorkspaceID != "w1234567" || unknownOperations[0].BindingVersion != 3 ||
		unknownOperations[0].RuntimeLeaseVersion != first.Lease.Version || unknownOperations[0].RuntimeInstanceID != "ri_test" {
		t.Fatalf("unknown operations requiring reconciliation = %#v, err=%v", unknownOperations, err)
	}
	unknownOperation, err := store.GetOperation(ctx, owner.ID, first.ID, operation.Key)
	if err != nil || unknownOperation.Status != task.OperationUnknown || unknownOperation.RuntimeLeaseVersion != first.Lease.Version || unknownOperation.LeaseVersion != second.Lease.Version ||
		unknownOperation.WorkspaceID != "w1234567" || unknownOperation.BindingVersion != 3 {
		t.Fatalf("get recovered operation = %#v, err=%v", unknownOperation, err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: first.ID, OperationKey: operation.Key, LeaseOwner: "worker-one", LeaseVersion: first.Lease.Version,
		Status: task.OperationSucceeded, ExternalID: "external",
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale worker update error = %v", err)
	}
	operation, err = store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: second.ID, OperationKey: operation.Key, LeaseOwner: "worker-two", LeaseVersion: second.Lease.Version,
		Status: task.OperationSucceeded, ExternalID: "external", Result: "verified",
	})
	if err != nil || operation.Status != task.OperationSucceeded {
		t.Fatalf("reconciled operation update = %#v, err=%v", operation, err)
	}
	unknownOperations, err = store.ListUnknownOperations(ctx, owner.ID, first.ID)
	if err != nil || len(unknownOperations) != 0 {
		t.Fatalf("resolved unknown operations = %#v, err=%v", unknownOperations, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, first.ID, 0, 20)
	if err != nil || len(events) != 7 {
		t.Fatalf("operation recovery events = %#v, err=%v", events, err)
	}
	for index, event := range events {
		if event.Sequence != int64(index+1) {
			t.Fatalf("task event sequence at index %d = %d", index, event.Sequence)
		}
	}
	var recovery map[string]any
	if err := json.Unmarshal(events[5].Payload, &recovery); err != nil || recovery["requires_reconciliation"] != true || events[5].Type != "task.operation" {
		t.Fatalf("recovery event = %#v, decoded=%#v err=%v", events[5], recovery, err)
	}
	prepared, _, err := store.CreateOperation(ctx, task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: second.ID, Key: "agent:confirmed-not-found",
		RequestHash: hashBytes("confirmed-not-found"), HostID: hostValue.ID,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		LeaseVersion: second.Lease.Version, RuntimeLeaseVersion: second.Lease.Version, Status: task.OperationPrepared,
	}, "worker-two")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: second.ID, OperationKey: prepared.Key, LeaseOwner: "worker-two", LeaseVersion: second.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: second.Lease.Version, Status: task.OperationDispatched,
	})
	if err != nil || prepared.Status != task.OperationDispatched {
		t.Fatalf("dispatch operation for not-found recovery = %#v, err=%v", prepared, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	third, claimed, err := store.ClaimNextTask(ctx, "worker-three", time.Minute)
	if err != nil || !claimed || third.Lease.Version != 3 {
		t.Fatalf("third task claim = %#v, claimed=%t, err=%v", third, claimed, err)
	}
	unknownNotFound, err := store.GetOperation(ctx, owner.ID, third.ID, prepared.Key)
	if err != nil || unknownNotFound.Status != task.OperationUnknown || unknownNotFound.LeaseVersion != third.Lease.Version || unknownNotFound.RuntimeLeaseVersion != second.Lease.Version {
		t.Fatalf("operation before confirmed-not-found rearm = %#v, err=%v", unknownNotFound, err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: third.ID, OperationKey: prepared.Key, LeaseOwner: "worker-two", LeaseVersion: second.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: third.Lease.Version, RearmAfterNotFound: true, Status: task.OperationPrepared,
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale worker rearm error = %v", err)
	}
	rearmed, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: third.ID, OperationKey: prepared.Key, LeaseOwner: "worker-three", LeaseVersion: third.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: third.Lease.Version, RearmAfterNotFound: true, Status: task.OperationPrepared,
	})
	if err != nil || rearmed.ID != prepared.ID || rearmed.Status != task.OperationPrepared || rearmed.RuntimeLeaseVersion != third.Lease.Version {
		t.Fatalf("rearm same operation after confirmed not-found = %#v, err=%v", rearmed, err)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: third.ID, LeaseOwner: "worker-three", LeaseVersion: third.Lease.Version, Status: task.StatusCompleted,
		Usage: task.Usage{ActiveSeconds: 2, ToolCalls: 1}, Event: task.Event{Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
	}); err != nil {
		t.Fatalf("commit task progress: %v", err)
	}
}

func TestPostgresAdoptsPreparedOperationAfterLeaseReclaim(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	if _, err := store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id='ri_test' WHERE id=$1`, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "prepared-recovery", "run once"))
	if err != nil {
		t.Fatal(err)
	}
	first, claimed, err := store.ClaimNextTask(ctx, "worker-one", time.Minute)
	if err != nil || !claimed || first.ID != submitted.Task.ID || first.Lease.Version != 1 {
		t.Fatalf("first claim = %#v, claimed=%t, err=%v", first, claimed, err)
	}
	prepared, replayed, err := store.CreateOperation(ctx, task.Operation{
		ID: mustID(t, domain.NewOperationID), TaskID: first.ID, Key: "agent:prepared-before-dispatch",
		RequestHash: hashBytes("prepared"), HostID: hostValue.ID, RuntimeGeneration: hostValue.RuntimeGeneration,
		RuntimeInstanceID: "ri_test", ControlVersion: 0, LeaseVersion: first.Lease.Version,
		RuntimeLeaseVersion: first.Lease.Version, Status: task.OperationPrepared,
	}, first.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("create prepared operation = %#v, replayed=%t, err=%v", prepared, replayed, err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	second, claimed, err := store.ClaimNextTask(ctx, "worker-two", time.Minute)
	if err != nil || !claimed || second.Lease.Version != 2 {
		t.Fatalf("second claim = %#v, claimed=%t, err=%v", second, claimed, err)
	}
	if _, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: second.ID, OperationKey: prepared.Key, LeaseOwner: first.Lease.Owner, LeaseVersion: first.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: first.Lease.Version, Status: task.OperationDispatched,
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale worker dispatch error = %v", err)
	}
	adopted, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: second.ID, OperationKey: prepared.Key, LeaseOwner: second.Lease.Owner, LeaseVersion: second.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: second.Lease.Version, AdoptPrepared: true, Status: task.OperationPrepared,
	})
	if err != nil || adopted.ID != prepared.ID || adopted.Status != task.OperationPrepared ||
		adopted.LeaseVersion != second.Lease.Version || adopted.RuntimeLeaseVersion != second.Lease.Version {
		t.Fatalf("adopted prepared operation = %#v, err=%v", adopted, err)
	}
	dispatched, err := store.UpdateOperation(ctx, app.OperationUpdate{
		TaskID: second.ID, OperationKey: prepared.Key, LeaseOwner: second.Lease.Owner, LeaseVersion: second.Lease.Version,
		RuntimeGeneration: hostValue.RuntimeGeneration, RuntimeInstanceID: "ri_test", ControlVersion: 0,
		RuntimeLeaseVersion: second.Lease.Version, Status: task.OperationDispatched,
	})
	if err != nil || dispatched.ID != prepared.ID || dispatched.Status != task.OperationDispatched {
		t.Fatalf("dispatch adopted operation = %#v, err=%v", dispatched, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, second.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var sawAdoption bool
	for _, event := range events {
		var payload map[string]any
		if json.Unmarshal(event.Payload, &payload) == nil && payload["adoption_reason"] == "task_lease_reclaimed_before_dispatch" {
			sawAdoption = true
		}
	}
	if !sawAdoption {
		t.Fatal("prepared operation adoption was not persisted in the task event log")
	}
}

func TestPostgresClaimNextTaskIsExclusiveAcrossStores(t *testing.T) {
	store, pool := newTemporaryStore(t)
	otherStore := New(pool)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submitted, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, hostValue.ID, "claim-race", "race workers"))
	if err != nil {
		t.Fatal(err)
	}

	type claimResult struct {
		task    task.Task
		claimed bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan claimResult, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	for _, candidate := range []struct {
		store  *Store
		worker string
	}{{store, "worker-one"}, {otherStore, "worker-two"}} {
		candidate := candidate
		go func() {
			defer workers.Done()
			<-start
			claimedTask, claimed, err := candidate.store.ClaimNextTask(ctx, candidate.worker, time.Minute)
			results <- claimResult{task: claimedTask, claimed: claimed, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	claims := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("claim task: %v", result.err)
		}
		if result.claimed {
			claims++
			if result.task.Lease.Version != 1 || result.task.Lease.Owner == "" {
				t.Fatalf("claimed task lease = %#v", result.task.Lease)
			}
		}
	}
	if claims != 1 {
		t.Fatalf("concurrent workers claimed task %d times, want exactly once", claims)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, submitted.Task.ID, 0, 10)
	if err != nil || len(events) != 2 || events[1].Sequence != 2 || events[1].Type != "task.status" {
		t.Fatalf("claim status event = %#v, err=%v", events, err)
	}
	var claimPayload map[string]any
	if err := json.Unmarshal(events[1].Payload, &claimPayload); err != nil || claimPayload["status"] != string(task.StatusRunning) {
		t.Fatalf("claim status payload = %#v err=%v", claimPayload, err)
	}
}

func TestPostgresTaskBatchItemAndValidatedArtifactPersistence(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "durable-primitives", "generate a report")
	itemID := mustID(t, domain.NewBatchItemID)
	submission.BatchItems = []task.BatchItem{{ID: itemID, Key: "report", Input: "write report"}}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit task with batch item: %v", err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, "worker-one", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim task: claimed=%t err=%v", ok, err)
	}
	stepID := mustID(t, domain.NewStepID)
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: "worker-one", LeaseVersion: claimed.Lease.Version,
		Status: task.StatusRunning, CurrentStep: "Write report",
		Steps: []task.Step{{ID: stepID, TaskID: created.Task.ID, Sequence: 1, Kind: "tool", Status: task.StepRunning, Title: "Write report"}},
		Event: task.Event{Type: "task.step", Payload: []byte(`{"step_id":"` + stepID + `"}`)},
	}); err != nil {
		t.Fatalf("persist artifact source step: %v", err)
	}
	details, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || len(details.BatchItems) != 1 {
		t.Fatalf("get submitted batch item: %#v err=%v", details.BatchItems, err)
	}
	batchItem := details.BatchItems[0]
	if batchItem.Attempt != 1 {
		t.Fatalf("new batch item attempt = %d, want 1", batchItem.Attempt)
	}
	otherOwner := createTestUser(t, store, 8, 20_000, 40_000)
	if _, err := store.UpdateBatchItem(ctx, otherOwner.ID, batchItem, "worker-one", claimed.Lease.Version); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner batch item update error = %v", err)
	}
	batchItem.Status = task.BatchItemRunning
	updated, err := store.UpdateBatchItem(ctx, owner.ID, batchItem, "worker-one", claimed.Lease.Version)
	if err != nil || updated.Status != task.BatchItemRunning {
		t.Fatalf("mark batch item running: %#v err=%v", updated, err)
	}
	batchItem = updated
	batchItem.Status = task.BatchItemSucceeded
	batchItem.Result = "report.pdf"
	updated, err = store.UpdateBatchItem(ctx, owner.ID, batchItem, "worker-one", claimed.Lease.Version)
	if err != nil || updated.Status != task.BatchItemSucceeded || updated.Result != "report.pdf" {
		t.Fatalf("complete batch item: %#v err=%v", updated, err)
	}
	if _, err := store.UpdateBatchItem(ctx, owner.ID, batchItem, "stale-worker", claimed.Lease.Version); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("stale batch item update error = %v", err)
	}

	artifact := task.Artifact{
		ID: mustID(t, domain.NewArtifactID), TaskID: created.Task.ID, StepID: stepID,
		BatchItemID: batchItem.ID, BatchAttempt: batchItem.Attempt,
		Label: "Report", Path: "artifacts/report.pdf", MediaType: "application/pdf",
	}
	if _, err := store.RecordValidatedArtifact(ctx, otherOwner.ID, artifact, "worker-one", claimed.Lease.Version); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("cross-owner artifact record error = %v", err)
	}
	stored, err := store.RecordValidatedArtifact(ctx, owner.ID, artifact, "worker-one", claimed.Lease.Version)
	if err != nil || stored.ID != artifact.ID || stored.StepID != stepID || stored.Path != artifact.Path || stored.CreatedAt.IsZero() {
		t.Fatalf("record validated artifact metadata: %#v err=%v", stored, err)
	}
	replayed, err := store.RecordValidatedArtifact(ctx, owner.ID, artifact, "worker-one", claimed.Lease.Version)
	if err != nil || replayed.ID != stored.ID || !replayed.CreatedAt.Equal(stored.CreatedAt) {
		t.Fatalf("idempotent artifact metadata retry: %#v err=%v", replayed, err)
	}
	newArtifactID := artifact
	newArtifactID.ID = mustID(t, domain.NewArtifactID)
	replayed, err = store.RecordValidatedArtifact(ctx, owner.ID, newArtifactID, "worker-one", claimed.Lease.Version)
	if err != nil || replayed.ID != stored.ID {
		t.Fatalf("artifact path replay after crash: %#v err=%v; want existing artifact %q", replayed, err, stored.ID)
	}
	changedMetadata := newArtifactID
	changedMetadata.Label = "Different report"
	if _, err := store.RecordValidatedArtifact(ctx, owner.ID, changedMetadata, "worker-one", claimed.Lease.Version); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("duplicate artifact path with changed metadata error = %v", err)
	}
	ordinaryArtifact := task.Artifact{
		ID: mustID(t, domain.NewArtifactID), TaskID: created.Task.ID, StepID: stepID,
		Label: "Summary", Path: "artifacts/summary.txt", MediaType: "text/plain",
	}
	ordinaryStored, err := store.RecordValidatedArtifact(ctx, owner.ID, ordinaryArtifact, "worker-one", claimed.Lease.Version)
	if err != nil {
		t.Fatalf("record ordinary task artifact: %v", err)
	}
	ordinaryReplay := ordinaryArtifact
	ordinaryReplay.ID = mustID(t, domain.NewArtifactID)
	ordinaryReplay, err = store.RecordValidatedArtifact(ctx, owner.ID, ordinaryReplay, "worker-one", claimed.Lease.Version)
	if err != nil || ordinaryReplay.ID != ordinaryStored.ID || ordinaryReplay.BatchItemID != "" || ordinaryReplay.BatchAttempt != 0 {
		t.Fatalf("ordinary artifact path replay: %#v err=%v", ordinaryReplay, err)
	}
	badPath := artifact
	badPath.ID = mustID(t, domain.NewArtifactID)
	badPath.Path = "../outside.txt"
	if _, err := store.RecordValidatedArtifact(ctx, owner.ID, badPath, "worker-one", claimed.Lease.Version); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("traversing artifact path error = %v", err)
	}
	missingStep := artifact
	missingStep.ID = mustID(t, domain.NewArtifactID)
	missingStep.StepID = mustID(t, domain.NewStepID)
	if _, err := store.RecordValidatedArtifact(ctx, owner.ID, missingStep, "worker-one", claimed.Lease.Version); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("artifact with missing source step error = %v", err)
	}

	otherSubmission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "other-artifact-task", "another task")
	otherItemID := mustID(t, domain.NewBatchItemID)
	otherSubmission.BatchItems = []task.BatchItem{{ID: otherItemID, Key: "other", Input: "other item"}}
	_, err = store.SubmitTask(ctx, otherSubmission)
	if err != nil {
		t.Fatalf("submit second task for artifact binding check: %v", err)
	}
	crossTask := artifact
	crossTask.ID = mustID(t, domain.NewArtifactID)
	crossTask.BatchItemID = otherItemID
	if _, err := store.RecordValidatedArtifact(ctx, owner.ID, crossTask, "worker-one", claimed.Lease.Version); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("artifact referencing another task's batch item error = %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_artifacts (id, task_id, step_id, label, path, media_type, batch_item_id, batch_attempt)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, mustID(t, domain.NewArtifactID), created.Task.ID, stepID,
		"Cross task", "artifacts/cross-task.pdf", "application/pdf", crossTask.BatchItemID, crossTask.BatchAttempt); err == nil {
		t.Fatal("database accepted an artifact whose batch item belongs to another task")
	}

	details, err = store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || len(details.Artifacts) != 2 || details.Artifacts[0].ID != artifact.ID || len(details.BatchItems) != 1 || details.BatchItems[0].Status != task.BatchItemSucceeded {
		t.Fatalf("durable task details = %#v err=%v", details, err)
	}
	events, err := store.ListTaskEvents(ctx, owner.ID, created.Task.ID, 1, 10)
	if err != nil || len(events) != 6 || events[0].Type != "task.status" || events[1].Type != "task.step" || events[2].Type != "task.batch_item" || events[3].Type != "task.batch_item" || events[4].Type != "task.artifact" || events[5].Type != "task.artifact" {
		t.Fatalf("batch/artifact events = %#v err=%v", events, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(events[4].Payload, &payload); err != nil || payload["artifact_id"] != artifact.ID || payload["step_id"] != stepID || payload["path"] != artifact.Path || payload["media_type"] != artifact.MediaType || payload["batch_item_id"] != batchItem.ID || payload["batch_attempt"] != float64(1) {
		t.Fatalf("artifact event payload = %#v err=%v", payload, err)
	}
}

func TestPostgresRetryFailedBatchItemPersistsAttemptAndIsIdempotent(t *testing.T) {
	fixture := newFailedBatchRetryFixture(t)
	before, err := fixture.store.GetTask(fixture.ctx, fixture.taskID, fixture.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := fixture.store.ListTaskEvents(fixture.ctx, fixture.ownerID, fixture.taskID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	key := testIdempotency(fixture.ownerID, "retry-batch-item", "retry-item-once")
	if err := fixture.store.RetryFailedBatchItem(fixture.ctx, key, fixture.taskID, fixture.itemID); err != nil {
		t.Fatalf("retry failed batch item: %v", err)
	}
	after, err := fixture.store.GetTask(fixture.ctx, fixture.taskID, fixture.ownerID)
	if err != nil {
		t.Fatal(err)
	}
	item := batchItemByID(t, after.BatchItems, fixture.itemID)
	if after.Task.Status != task.StatusQueued || after.Task.Lease.Owner != "" || after.Task.Lease.ExpiresAt != nil ||
		after.Task.Lease.Version != before.Task.Lease.Version+1 || after.Task.CurrentStep != "" || after.Task.Result != "" ||
		after.Task.WaitingReason != "" || after.Task.FinishedAt != nil || after.Task.Usage.ActiveSeconds != 17 ||
		after.Task.Usage.ToolCalls != 3 || after.Task.Usage.ModelTokens == nil || *after.Task.Usage.ModelTokens != 29 {
		t.Fatalf("task retry state = %#v; want queued with cleared result/lease and retained usage", after.Task)
	}
	if item.Status != task.BatchItemQueued || item.Attempt != 2 || item.Result != "" || item.Error != "" {
		t.Fatalf("retried batch item = %#v; want clean queued attempt 2", item)
	}
	afterEvents, err := fixture.store.ListTaskEvents(fixture.ctx, fixture.ownerID, fixture.taskID, 0, 100)
	if err != nil || len(afterEvents) != len(beforeEvents)+2 {
		t.Fatalf("retry events = %d (err=%v), before=%d; want exactly two appended events", len(afterEvents), err, len(beforeEvents))
	}
	statusEvent, itemEvent := afterEvents[len(afterEvents)-2], afterEvents[len(afterEvents)-1]
	if statusEvent.Sequence+1 != itemEvent.Sequence || statusEvent.Type != "task.status" || itemEvent.Type != "task.batch_item" {
		t.Fatalf("retry event order = %#v then %#v", statusEvent, itemEvent)
	}
	var retryPayload map[string]any
	if err := json.Unmarshal(itemEvent.Payload, &retryPayload); err != nil || retryPayload["batch_item_id"] != fixture.itemID || retryPayload["status"] != string(task.BatchItemQueued) || retryPayload["attempt"] != float64(2) {
		t.Fatalf("retry item event payload = %#v, err=%v", retryPayload, err)
	}
	if err := fixture.store.RetryFailedBatchItem(fixture.ctx, key, fixture.taskID, fixture.itemID); err != nil {
		t.Fatalf("replay failed batch item retry: %v", err)
	}
	replayed, err := fixture.store.GetTask(fixture.ctx, fixture.taskID, fixture.ownerID)
	if err != nil || batchItemByID(t, replayed.BatchItems, fixture.itemID).Attempt != 2 {
		t.Fatalf("same-key retry replay changed attempt: details=%#v err=%v", replayed, err)
	}
	replayEvents, err := fixture.store.ListTaskEvents(fixture.ctx, fixture.ownerID, fixture.taskID, 0, 100)
	if err != nil || len(replayEvents) != len(afterEvents) {
		t.Fatalf("same-key retry replay appended events: count=%d err=%v want=%d", len(replayEvents), err, len(afterEvents))
	}
	differentBody := key
	differentBody.RequestHash = hashBytes("different retry body")
	if err := fixture.store.RetryFailedBatchItem(fixture.ctx, differentBody, fixture.taskID, fixture.itemID); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("same key with changed request error = %v", err)
	}
}

func TestPostgresRetryFailedBatchItemRejectsUnknownOperation(t *testing.T) {
	fixture := newFailedBatchRetryFixture(t)
	operationID := mustID(t, domain.NewOperationID)
	if _, err := fixture.store.pool.Exec(fixture.ctx, `INSERT INTO task_operations
(id, task_id, operation_key, request_hash, host_id, runtime_generation, runtime_instance_id, control_version, lease_version, runtime_lease_version, status)
VALUES ($1,$2,'unknown-operation',$3,$4,0,'ri_test',0,$5,$5,'unknown')`, operationID, fixture.taskID,
		hashBytes("unknown operation"), fixture.hostID, fixture.leaseVersion); err != nil {
		t.Fatalf("seed unknown operation: %v", err)
	}
	key := testIdempotency(fixture.ownerID, "retry-batch-item", "retry-with-unknown-operation")
	if err := fixture.store.RetryFailedBatchItem(fixture.ctx, key, fixture.taskID, fixture.itemID); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("retry with an unknown operation error = %v", err)
	}
	var reservations int
	if err := fixture.store.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM idempotency_keys WHERE owner_id=$1 AND operation=$2 AND key=$3`, key.OwnerID, key.Operation, key.Key).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatalf("failed retry left an idempotency reservation: count=%d err=%v", reservations, err)
	}
	details, err := fixture.store.GetTask(fixture.ctx, fixture.taskID, fixture.ownerID)
	if err != nil || batchItemByID(t, details.BatchItems, fixture.itemID).Attempt != 1 || details.Task.Status != task.StatusFailed {
		t.Fatalf("unknown operation changed batch state: details=%#v err=%v", details, err)
	}
}

func TestPostgresRetryFailedBatchItemConcurrentDifferentKeys(t *testing.T) {
	fixture := newFailedBatchRetryFixture(t)
	keys := []app.IdempotencyRecord{
		testIdempotency(fixture.ownerID, "retry-batch-item", "retry-concurrent-a"),
		testIdempotency(fixture.ownerID, "retry-batch-item", "retry-concurrent-b"),
	}
	results := make(chan error, len(keys))
	var ready sync.WaitGroup
	ready.Add(len(keys))
	start := make(chan struct{})
	for _, key := range keys {
		key := key
		go func() {
			ready.Done()
			<-start
			results <- fixture.store.RetryFailedBatchItem(fixture.ctx, key, fixture.taskID, fixture.itemID)
		}()
	}
	ready.Wait()
	close(start)
	var successes, conflicts int
	for range keys {
		err := <-results
		if err == nil {
			successes++
		} else if domain.CodeOf(err) == domain.CodeInvalidState {
			conflicts++
		} else {
			t.Fatalf("concurrent retry returned unexpected error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent retries: successes=%d state conflicts=%d; want exactly one of each", successes, conflicts)
	}
	details, err := fixture.store.GetTask(fixture.ctx, fixture.taskID, fixture.ownerID)
	if err != nil || details.Task.Status != task.StatusQueued || batchItemByID(t, details.BatchItems, fixture.itemID).Attempt != 2 {
		t.Fatalf("concurrent retry final state = %#v err=%v", details, err)
	}
}

type failedBatchRetryFixture struct {
	store        *Store
	ctx          context.Context
	ownerID      string
	hostID       string
	taskID       string
	itemID       string
	leaseVersion int64
}

func newFailedBatchRetryFixture(t *testing.T) failedBatchRetryFixture {
	t.Helper()
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 8, 20_000, 40_000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	submission := newTestTaskSubmission(t, owner.ID, hostValue.ID, "failed-batch", "process the batch")
	failedID := mustID(t, domain.NewBatchItemID)
	succeededID := mustID(t, domain.NewBatchItemID)
	submission.BatchItems = []task.BatchItem{
		{ID: failedID, Key: "failed", Input: "retry this item"},
		{ID: succeededID, Key: "succeeded", Input: "keep this result"},
	}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("create retryable batch task: %v", err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, "retry-worker", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim retryable task: claimed=%t err=%v", ok, err)
	}
	details, err := store.GetTask(ctx, created.Task.ID, owner.ID)
	if err != nil || len(details.BatchItems) != 2 {
		t.Fatalf("load retryable batch items: %#v err=%v", details.BatchItems, err)
	}
	failed := batchItemByID(t, details.BatchItems, failedID)
	failed.Status = task.BatchItemRunning
	failed, err = store.UpdateBatchItem(ctx, owner.ID, failed, "retry-worker", claimed.Lease.Version)
	if err != nil {
		t.Fatalf("mark retryable item running: %v", err)
	}
	failed.Status = task.BatchItemFailed
	failed.Error = "temporary failure"
	if _, err := store.UpdateBatchItem(ctx, owner.ID, failed, "retry-worker", claimed.Lease.Version); err != nil {
		t.Fatalf("mark retryable item failed: %v", err)
	}
	succeeded := batchItemByID(t, details.BatchItems, succeededID)
	succeeded.Status = task.BatchItemSucceeded
	succeeded.Result = "preserved success"
	if _, err := store.UpdateBatchItem(ctx, owner.ID, succeeded, "retry-worker", claimed.Lease.Version); err != nil {
		t.Fatalf("persist completed peer item: %v", err)
	}
	stepID := mustID(t, domain.NewStepID)
	tokens := int64(29)
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: "retry-worker", LeaseVersion: claimed.Lease.Version,
		Status: task.StatusFailed, CurrentStep: stepID, Result: "prior task result", WaitingReason: "prior wait",
		Usage: task.Usage{ActiveSeconds: 17, ToolCalls: 3, ModelTokens: &tokens},
		Steps: []task.Step{{ID: stepID, TaskID: created.Task.ID, Sequence: 1, Kind: "batch_result", Status: task.StepFailed, Title: "Batch failed"}},
		Event: task.Event{Type: "task.result", Payload: []byte(`{"status":"failed"}`)},
	}); err != nil {
		t.Fatalf("mark retryable task failed: %v", err)
	}
	return failedBatchRetryFixture{
		store: store, ctx: ctx, ownerID: owner.ID, hostID: hostValue.ID,
		taskID: created.Task.ID, itemID: failedID, leaseVersion: claimed.Lease.Version,
	}
}

func batchItemByID(t *testing.T, items []task.BatchItem, id string) task.BatchItem {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("batch item %q not found in %#v", id, items)
	return task.BatchItem{}
}

func newTestTaskSubmission(t *testing.T, ownerID, hostID, idemKey, objective string) app.TaskSubmission {
	t.Helper()
	taskID := mustID(t, domain.NewTaskID)
	messageID := mustID(t, domain.NewMessageID)
	return app.TaskSubmission{
		Task: task.Task{
			ID: taskID, OwnerID: ownerID, HostID: hostID, ModelID: "model-test", Objective: objective,
			Status: task.StatusQueued, Authorization: task.Authorization{Kind: "session"},
			Budget: task.Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000},
		},
		Message:      conversation.Message{ID: messageID, OwnerID: ownerID, Role: conversation.RoleUser, Content: objective},
		InitialEvent: task.Event{Type: "task.status", Payload: []byte(`{"status":"queued"}`)},
		Idempotency:  testIdempotency(ownerID, "submit-task", idemKey),
	}
}

func testIdempotency(ownerID, operation, key string) app.IdempotencyRecord {
	return app.IdempotencyRecord{OwnerID: ownerID, Operation: operation, Key: key, RequestHash: hashBytes(operation + ":" + key)}
}

func hashBytes(value string) []byte {
	hash := sha256.Sum256([]byte(value))
	return hash[:]
}

func mustID(t *testing.T, generate func() (string, error)) string {
	t.Helper()
	value, err := generate()
	if err != nil {
		t.Fatal(err)
	}
	return value
}
