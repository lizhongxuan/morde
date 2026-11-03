package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskMessageDeliveryAcrossActiveStates(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 2, 2000, 4096)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "delivery-state-task", "initial"))
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}

	append := func(key, content string) app.TaskSubmissionResult {
		t.Helper()
		result, err := store.AppendMessage(ctx, app.TaskSubmission{
			Task: task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, ModelID: created.Task.ModelID},
			Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID,
				ConversationID: created.Conversation.ID, Role: conversation.RoleUser, Content: content},
			Idempotency: testIdempotency(owner.ID, "append-message", key),
		})
		if err != nil {
			t.Fatalf("append %s: %v", key, err)
		}
		return result
	}
	setStatus := func(status task.Status) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE tasks SET status=$2 WHERE id=$1`, created.Task.ID, string(status)); err != nil {
			t.Fatalf("set task status %s: %v", status, err)
		}
	}
	assertAppend := func(status task.Status, delivery conversation.DeliveryStatus, key string) {
		t.Helper()
		result := append(key, key)
		if result.Task.Status != status || result.Message.DeliveryStatus != delivery {
			t.Fatalf("append %s returned Task status %q and delivery status %q", key, result.Task.Status, result.Message.DeliveryStatus)
		}
	}

	assertAppend(task.StatusQueued, conversation.DeliveryPending, "delivery-queued")
	setStatus(task.StatusWaitingUser)
	assertAppend(task.StatusWaitingUser, conversation.DeliveryPending, "delivery-waiting")

	setStatus(task.StatusPaused)
	var beforePausedEvents int64
	if err := pool.QueryRow(ctx, `SELECT event_seq FROM tasks WHERE id=$1`, created.Task.ID).Scan(&beforePausedEvents); err != nil {
		t.Fatal(err)
	}
	assertAppend(task.StatusPaused, conversation.DeliveryPending, "delivery-paused")
	var afterPausedEvents int64
	if err := pool.QueryRow(ctx, `SELECT event_seq FROM tasks WHERE id=$1`, created.Task.ID).Scan(&afterPausedEvents); err != nil || afterPausedEvents != beforePausedEvents {
		t.Fatalf("paused append advanced Task event sequence: before=%d after=%d err=%v", beforePausedEvents, afterPausedEvents, err)
	}
	newTask := newTestTaskSubmission(t, owner.ID, "", "delivery-paused-conflict", "must not bypass pause")
	newTask.Task.ConversationID = created.Conversation.ID
	newTask.Message.ConversationID = created.Conversation.ID
	if _, err := store.SubmitTask(ctx, newTask); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("new Task bypassed paused conversation: %v", err)
	}

	for _, status := range []task.Status{task.StatusPausing, task.StatusCancelling} {
		setStatus(status)
		_, err := store.AppendMessage(ctx, app.TaskSubmission{
			Task: task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, ModelID: created.Task.ModelID},
			Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID,
				ConversationID: created.Conversation.ID, Role: conversation.RoleUser, Content: string(status) + " message"},
			Idempotency: testIdempotency(owner.ID, "append-message", "delivery-"+string(status)),
		})
		if domain.CodeOf(err) != domain.CodeInvalidState {
			t.Fatalf("append while %s error = %v, want invalid state", status, err)
		}
	}

	page, err := store.ListMessages(ctx, owner.ID, created.Conversation.ID, app.PageRequest{Limit: 20})
	if err != nil || len(page.Items) != 4 {
		t.Fatalf("list messages after state appends: count=%d err=%v", len(page.Items), err)
	}
	statuses := make(map[string]conversation.DeliveryStatus, len(page.Items))
	for _, message := range page.Items {
		statuses[message.Content] = message.DeliveryStatus
	}
	for content, status := range map[string]conversation.DeliveryStatus{
		"initial":          conversation.DeliveryApplied,
		"delivery-queued":  conversation.DeliveryPending,
		"delivery-waiting": conversation.DeliveryPending,
		"delivery-paused":  conversation.DeliveryPending,
	} {
		if statuses[content] != status {
			t.Errorf("delivery status for %q = %q, want %q", content, statuses[content], status)
		}
	}
}

func TestPostgresPendingConversationMessagesBlockTaskCompletion(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 2, 2000, 4096)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "delivery-completion-task", "initial"))
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, "delivery-completion-worker", time.Minute)
	if err != nil || !ok || claimed.ID != created.Task.ID {
		t.Fatalf("claim task: claimed=%t task=%q err=%v", ok, claimed.ID, err)
	}
	_, err = store.AppendMessage(ctx, app.TaskSubmission{
		ExpectedTaskID: "q7654321",
		Task:           task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, ModelID: created.Task.ModelID},
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID,
			ConversationID: created.Conversation.ID, Role: conversation.RoleUser, Content: "stale task input"},
		Idempotency: testIdempotency(owner.ID, "append-message", "delivery-completion-stale-task"),
	})
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale expected Task was accepted: %v", err)
	}
	appended, err := store.AppendMessage(ctx, app.TaskSubmission{
		ExpectedTaskID: created.Task.ID,
		Task:           task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, ModelID: created.Task.ModelID},
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID,
			ConversationID: created.Conversation.ID, Role: conversation.RoleUser, Content: "pending user input"},
		Idempotency: testIdempotency(owner.ID, "append-message", "delivery-completion-pending"),
	})
	if err != nil || appended.Message.DeliveryStatus != conversation.DeliveryPending {
		t.Fatalf("append pending message: result=%#v err=%v", appended, err)
	}
	_, err = store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusCompleted, Result: "done",
		Event: task.Event{Type: "task.result", Payload: []byte(`{"result":"done"}`)},
	})
	if domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("completed Task accepted pending user input: %v", err)
	}
	var status, deliveryStatus string
	if err := pool.QueryRow(ctx, `SELECT t.status,m.delivery_status FROM tasks t JOIN conversation_messages m ON m.task_id=t.id
WHERE t.id=$1 AND m.id=$2`, created.Task.ID, appended.Message.ID).Scan(&status, &deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if status != string(task.StatusRunning) || deliveryStatus != string(conversation.DeliveryPending) {
		t.Fatalf("completion conflict changed persisted state: Task=%q message=%q", status, deliveryStatus)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: created.Task.ID, LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		Status: task.StatusFailed,
		Event:  task.Event{Type: "task.status", Payload: []byte(`{"status":"failed"}`)},
	}); err != nil {
		t.Fatalf("fail Task with undelivered message: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT t.status,m.delivery_status FROM tasks t JOIN conversation_messages m ON m.task_id=t.id
WHERE t.id=$1 AND m.id=$2`, created.Task.ID, appended.Message.ID).Scan(&status, &deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if status != string(task.StatusFailed) || deliveryStatus != string(conversation.DeliveryUnknown) {
		t.Fatalf("terminal failure did not close pending delivery: Task=%q message=%q", status, deliveryStatus)
	}
}

func TestPostgresAgentCheckpointAppliesOnlyMatchingRootMessages(t *testing.T) {
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	login, err := clientLoginForTest(ctx, clients, owner.Name, "client-password-123", "ios", "phone", "", "delivery-checkpoint-login", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifyClient(ctx, login.ClientCredential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	submission := newTestTaskSubmission(t, owner.ID, "", "delivery-checkpoint-task", "initial prompt")
	submission.Task.ExecutionLocation = "server"
	submission.Task.Authorization = task.Authorization{Kind: string(identity.KindClient), TokenID: principal.TokenID,
		ClientInstallationID: principal.ClientInstallationID, Version: 1, ExpiresAt: &expiresAt}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, "delivery-checkpoint-worker", 20*time.Second)
	if err != nil || !ok || claimed.ID != created.Task.ID {
		t.Fatalf("claim Task: claimed=%t id=%q err=%v", ok, claimed.ID, err)
	}
	supervisorKey := make([]byte, 32)
	supervisorKey[0] = 1
	authenticator, err := app.NewAgentStopAuthenticator("supervisor-delivery", supervisorKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 1
	started, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: owner.ID, InitiatorID: claimed.InitiatorID, AgentInstanceID: "agent-delivery", AgentSessionID: "session-delivery",
		SupervisorID: "supervisor-delivery", LeaseOwner: claimed.Lease.Owner, LeaseVersion: claimed.Lease.Version,
		AuthorizationVersion: 1, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1",
	}, SupervisorNonce: nonce, LeaseDuration: 10 * time.Second})
	if err != nil || replayed {
		t.Fatalf("start Agent: started=%#v replayed=%t err=%v", started, replayed, err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	fence := app.AgentCheckpointFence{TaskID: claimed.ID, OwnerID: owner.ID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: started.Fence.AgentInstanceID, AgentGeneration: started.Fence.AgentGeneration,
		AgentSessionID: started.Fence.AgentSessionID, AgentRootSessionID: started.Fence.AgentSessionID,
		AuthorizationVersion: started.Fence.AuthorizationVersion, LeaseOwner: started.Fence.LeaseOwner,
		LeaseVersion: started.Fence.LeaseVersion, EngineVersion: started.Fence.EngineVersion, ProfileVersion: started.Fence.ProfileVersion}
	appendMessage := func(key, content string) app.TaskSubmissionResult {
		t.Helper()
		result, err := store.AppendMessage(ctx, app.TaskSubmission{
			Task: task.Task{OwnerID: owner.ID, ConversationID: created.Conversation.ID, ModelID: created.Task.ModelID},
			Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID,
				ConversationID: created.Conversation.ID, Role: conversation.RoleUser, Content: content},
			Idempotency: testIdempotency(owner.ID, "append-message", key),
		})
		if err != nil {
			t.Fatalf("append %s: %v", key, err)
		}
		return result
	}
	first := appendMessage("delivery-checkpoint-first", "first pending")
	pending, err := checkpoints.PendingMessages(ctx, fence)
	if err != nil || len(pending) != 1 || pending[0].ID != first.Message.ID || pending[0].Content != first.Message.Content {
		t.Fatalf("read pending messages: %#v err=%v", pending, err)
	}
	makePayload := func(from int64, events ...any) []byte {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"header": map[string]string{"id": fence.AgentSessionID}, "from_seq": from, "events": events,
		})
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	makeEvent := func(sequence int64, id, content string) map[string]any {
		return map[string]any{"seq": sequence, "type": "user/message", "data": map[string]string{"id": id, "role": "user", "content": content}}
	}
	makeCheckpoint := func(currentFence app.AgentCheckpointFence, from, through int64, payload []byte) app.AgentCheckpointCommand {
		digest := sha256.Sum256(payload)
		return app.AgentCheckpointCommand{Fence: currentFence, From: from, Through: through, Digest: digest[:], Payload: payload}
	}
	firstPayload := makePayload(0,
		makeEvent(0, "2d47aa7b-f4db-4f11-b606-b7102c2c2df2", "initial prompt"),
		makeEvent(1, created.Message.ID, created.Message.Content),
		makeEvent(2, first.Message.ID, first.Message.Content))
	if _, err := checkpoints.Save(ctx, makeCheckpoint(fence, 0, 2, firstPayload)); err != nil {
		t.Fatalf("save matching root checkpoint: %v", err)
	}
	var firstStatus string
	if err := store.pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, first.Message.ID).Scan(&firstStatus); err != nil || firstStatus != string(conversation.DeliveryApplied) {
		t.Fatalf("matched root message status=%q err=%v", firstStatus, err)
	}

	second := appendMessage("delivery-checkpoint-second", "second pending")
	wrongPayload := makePayload(3, makeEvent(3, second.Message.ID, "changed content"))
	if _, err := checkpoints.Save(ctx, makeCheckpoint(fence, 3, 3, wrongPayload)); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("mismatched root message checkpoint accepted: %v", err)
	}
	var secondStatus string
	if err := store.pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, second.Message.ID).Scan(&secondStatus); err != nil || secondStatus != string(conversation.DeliveryUnknown) {
		t.Fatalf("mismatched root message status=%q err=%v", secondStatus, err)
	}
	var checkpointCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_checkpoints WHERE task_id=$1 AND agent_session_id=$2`,
		created.Task.ID, fence.AgentSessionID).Scan(&checkpointCount); err != nil || checkpointCount != 1 {
		t.Fatalf("checkpoint count after mismatch=%d err=%v", checkpointCount, err)
	}

	third := appendMessage("delivery-checkpoint-third", "third pending")
	unknownIDPayload := makePayload(3, makeEvent(3, "m9999999", "not a message for this Task"))
	if _, err := checkpoints.Save(ctx, makeCheckpoint(fence, 3, 3, unknownIDPayload)); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("unmapped root message checkpoint accepted: %v", err)
	}
	var thirdStatus string
	if err := store.pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, third.Message.ID).Scan(&thirdStatus); err != nil || thirdStatus != string(conversation.DeliveryUnknown) {
		t.Fatalf("unmapped root message left delivery status=%q err=%v", thirdStatus, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_checkpoints WHERE task_id=$1 AND agent_session_id=$2`,
		created.Task.ID, fence.AgentSessionID).Scan(&checkpointCount); err != nil || checkpointCount != 1 {
		t.Fatalf("checkpoint count after unmapped id=%d err=%v", checkpointCount, err)
	}

	children, err := app.NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	child, replayed, err := children.Create(ctx, app.AgentChildSessionCommand{Fence: started.Fence, ParentSessionID: fence.AgentSessionID,
		SpawnKey: "delivery-child", Arguments: []byte(`{"objective":"inspect"}`), Title: "Inspect", Summary: "inspect safely",
		AllowedTools: []string{"workspace_read"}})
	if err != nil || replayed {
		t.Fatalf("create child session: child=%#v replayed=%t err=%v", child, replayed, err)
	}
	childFence := fence
	childFence.AgentSessionID, childFence.AgentRootSessionID = child.SessionID, fence.AgentSessionID
	childPayload, err := json.Marshal(map[string]any{"header": map[string]string{"id": child.SessionID}, "from_seq": 0,
		"events": []any{makeEvent(0, "ef2186dc-7069-4787-b580-b31d1e7e77a0", "child objective")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoints.Save(ctx, makeCheckpoint(childFence, 0, 0, childPayload)); err != nil {
		t.Fatalf("child UUID user message should not enter root delivery mapping: %v", err)
	}
}
