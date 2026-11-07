package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
)

func newConversationReceiptForTest(t *testing.T, ctx context.Context, store *Store, ownerID, conversationID, referenceID, key, content string) app.ConversationMessageReceipt {
	t.Helper()
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.GetTokenByID(ctx, testSessionTokenID(ownerID))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(key))
	return app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: ownerID, ActorID: ownerID,
			ConversationID: conversationID, Role: conversation.RoleUser, Content: content},
		ServiceInstanceID: instanceID, ClientID: key, RequestHash: digest[:],
		Input:         app.ConversationRoutingInput{ReferenceTaskID: referenceID},
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
	}
}

func receivePendingRouteMessageForTest(t *testing.T, ctx context.Context, store *Store, ownerID, conversationID, referenceID, key, content string) conversation.Message {
	t.Helper()
	receipt := newConversationReceiptForTest(t, ctx, store, ownerID, conversationID, referenceID, key, content)
	message, replay, err := store.AcceptConversationMessage(ctx, receipt)
	if err != nil || replay {
		t.Fatalf("accept conversation message: replay=%t err=%v", replay, err)
	}
	return message
}

// seedLegacyPendingTaskMessageForTest represents already-persisted pre-routing
// inbox rows so checkpoint and terminal-delivery compatibility remain tested.
func seedLegacyPendingTaskMessageForTest(t *testing.T, ctx context.Context, store *Store, ownerID, conversationID, taskID, content string,
	attachments map[string]string) conversation.Message {
	t.Helper()
	message := conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: ownerID, ActorID: ownerID,
		ConversationID: conversationID, TaskID: taskID, Role: conversation.RoleUser, Content: content, DeliveryStatus: conversation.DeliveryPending}
	err := store.WithTx(ctx, func(tx pgx.Tx) error {
		if err := insertMessage(ctx, tx, message); err != nil {
			return err
		}
		if len(attachments) == 0 {
			return nil
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(attachments))
		for id := range attachments {
			ids = append(ids, id)
		}
		return insertHistoryMessageAttachments(ctx, tx, instanceID, ownerID, conversationID, message.ID, ids, attachments)
	})
	if err != nil {
		t.Fatalf("seed historical pending message: %v", err)
	}
	return message
}

func TestPostgresConversationReceiptsAcrossTaskStates(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 2, 2000, 4096)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "delivery-state-task", "initial"))
	if err != nil {
		t.Fatalf("submit task: %v", err)
	}

	setStatus := func(status task.Status) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE tasks SET status=$2 WHERE id=$1`, created.Task.ID, string(status)); err != nil {
			t.Fatalf("set task status %s: %v", status, err)
		}
	}
	var beforeEvents int64
	if err := pool.QueryRow(ctx, `SELECT event_seq FROM tasks WHERE id=$1`, created.Task.ID).Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	for _, status := range []task.Status{task.StatusQueued, task.StatusWaitingUser, task.StatusPaused, task.StatusPausing, task.StatusCancelling} {
		setStatus(status)
		message := receivePendingRouteMessageForTest(t, ctx, store, owner.ID, created.Conversation.ID, created.Task.ID,
			"delivery-"+string(status), string(status)+" message")
		if message.DeliveryStatus != conversation.DeliveryPending || message.TaskID != "" {
			t.Fatalf("receipt while %s was prematurely attached: %+v", status, message)
		}
		if status == task.StatusPaused {
			independent := newTestTaskSubmission(t, owner.ID, "", "delivery-paused-independent", "independent task while paused")
			independent.Task.ConversationID, independent.Message.ConversationID = created.Conversation.ID, created.Conversation.ID
			if _, err := store.SubmitTask(ctx, independent); err != nil {
				t.Fatalf("independent Task should not depend on a paused sibling: %v", err)
			}
		}
	}
	var afterEvents int64
	if err := pool.QueryRow(ctx, `SELECT event_seq FROM tasks WHERE id=$1`, created.Task.ID).Scan(&afterEvents); err != nil || afterEvents != beforeEvents {
		t.Fatalf("receipts changed the referenced Task: before=%d after=%d err=%v", beforeEvents, afterEvents, err)
	}

	page, err := store.ListMessages(ctx, owner.ID, created.Conversation.ID, app.PageRequest{Limit: 20})
	if err != nil || len(page.Items) != 7 {
		t.Fatalf("list messages after state receipts: count=%d err=%v", len(page.Items), err)
	}
	statuses := make(map[string]conversation.DeliveryStatus, len(page.Items))
	for _, message := range page.Items {
		statuses[message.Content] = message.DeliveryStatus
	}
	for content, status := range map[string]conversation.DeliveryStatus{
		"initial":                       conversation.DeliveryApplied,
		"queued message":                conversation.DeliveryPending,
		"waiting_user message":          conversation.DeliveryPending,
		"paused message":                conversation.DeliveryPending,
		"pausing message":               conversation.DeliveryPending,
		"cancelling message":            conversation.DeliveryPending,
		"independent task while paused": conversation.DeliveryApplied,
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
	stale := receivePendingRouteMessageForTest(t, ctx, store, owner.ID, created.Conversation.ID, "q7654321",
		"delivery-completion-stale-task", "stale task input")
	if stale.TaskID != "" || stale.DeliveryStatus != conversation.DeliveryPending {
		t.Fatalf("unresolved reference should be received before routing: %+v", stale)
	}
	appended := seedLegacyPendingTaskMessageForTest(t, ctx, store, owner.ID, created.Conversation.ID, created.Task.ID, "pending user input", nil)
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
WHERE t.id=$1 AND m.id=$2`, created.Task.ID, appended.ID).Scan(&status, &deliveryStatus); err != nil {
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
WHERE t.id=$1 AND m.id=$2`, created.Task.ID, appended.ID).Scan(&status, &deliveryStatus); err != nil {
		t.Fatal(err)
	}
	if status != string(task.StatusFailed) || deliveryStatus != string(conversation.DeliveryUnknown) {
		t.Fatalf("terminal failure did not close pending delivery: Task=%q message=%q", status, deliveryStatus)
	}
}

func TestPostgresAgentCheckpointAppliesOnlyMatchingRootMessages(t *testing.T) {
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	if !requirePersonalAttachmentSchema(t, ctx, store.pool) {
		return
	}
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
	createPersonalLibraryItemForAttachmentTest(t, store, principal, "library-item_1")
	pdf := []byte("%PDF-1.4\n% Morde queued attachment test\n%%EOF\n")
	pdfDigest := sha256.Sum256(pdf)
	attachmentService, err := app.NewPersonalAttachmentService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	attachmentUpload, err := attachmentService.CreatePersonalAttachmentUpload(ctx, principal, app.PersonalAttachmentUploadCommand{
		IdempotencyKey: "queue-pdf-create", ObjectID: "library-item_1", BaseRevision: 1,
		SizeBytes: int64(len(pdf)), SHA256: hex.EncodeToString(pdfDigest[:]), MediaType: "application/pdf",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachmentService.PutPersonalAttachmentChunk(ctx, principal, app.PersonalAttachmentChunkCommand{
		UploadID: attachmentUpload.UploadID, Sequence: 0, SizeBytes: int64(len(pdf)),
		SHA256: hex.EncodeToString(pdfDigest[:]), Body: bytes.NewReader(pdf),
	}); err != nil {
		t.Fatal(err)
	}
	committedPDF, err := attachmentService.CommitPersonalAttachment(ctx, principal, attachmentUpload.UploadID, "queue-pdf-commit")
	if err != nil || !personal.ValidPersonalAttachmentID(committedPDF.AttachmentID) {
		t.Fatalf("commit queued PDF = %+v, %v", committedPDF, err)
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
	first := seedLegacyPendingTaskMessageForTest(t, ctx, store, owner.ID, created.Conversation.ID, created.Task.ID, "first pending",
		map[string]string{committedPDF.AttachmentID: attachmentUpload.UploadID})
	pending, err := checkpoints.PendingMessages(ctx, fence)
	if err != nil || len(pending) != 1 || pending[0].ID != first.ID || pending[0].Content != first.Content ||
		len(pending[0].AttachmentIDs) != 1 || pending[0].AttachmentIDs[0] != committedPDF.AttachmentID {
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
	makeEvent := func(sequence int64, id, content string, attachmentIDs ...string) map[string]any {
		var checkpointContent any = content
		if len(attachmentIDs) != 0 {
			checkpointContent = []map[string]string{
				{"type": "text", "text": content},
				{"type": "text", "text": "[MORDE_TASK_ATTACHMENT_IDS_V1:" + strings.Join(attachmentIDs, ",") + "]"},
			}
		}
		return map[string]any{"seq": sequence, "type": "user/message", "data": map[string]any{"id": id, "role": "user", "content": checkpointContent}}
	}
	makeCheckpoint := func(currentFence app.AgentCheckpointFence, from, through int64, payload []byte) app.AgentCheckpointCommand {
		digest := sha256.Sum256(payload)
		return app.AgentCheckpointCommand{Fence: currentFence, From: from, Through: through, Digest: digest[:], Payload: payload}
	}
	firstPayload := makePayload(0,
		makeEvent(0, "2d47aa7b-f4db-4f11-b606-b7102c2c2df2", "initial prompt"),
		makeEvent(1, created.Message.ID, created.Message.Content),
		makeEvent(2, first.ID, first.Content, committedPDF.AttachmentID))
	if _, err := checkpoints.Save(ctx, makeCheckpoint(fence, 0, 2, firstPayload)); err != nil {
		t.Fatalf("save matching root checkpoint: %v", err)
	}
	var firstStatus string
	if err := store.pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, first.ID).Scan(&firstStatus); err != nil || firstStatus != string(conversation.DeliveryApplied) {
		t.Fatalf("matched root message status=%q err=%v", firstStatus, err)
	}

	second := seedLegacyPendingTaskMessageForTest(t, ctx, store, owner.ID, created.Conversation.ID, created.Task.ID, "second pending", nil)
	wrongPayload := makePayload(3, makeEvent(3, second.ID, "changed content"))
	if _, err := checkpoints.Save(ctx, makeCheckpoint(fence, 3, 3, wrongPayload)); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("mismatched root message checkpoint accepted: %v", err)
	}
	var secondStatus string
	if err := store.pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, second.ID).Scan(&secondStatus); err != nil || secondStatus != string(conversation.DeliveryUnknown) {
		t.Fatalf("mismatched root message status=%q err=%v", secondStatus, err)
	}
	var checkpointCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_checkpoints WHERE task_id=$1 AND agent_session_id=$2`,
		created.Task.ID, fence.AgentSessionID).Scan(&checkpointCount); err != nil || checkpointCount != 1 {
		t.Fatalf("checkpoint count after mismatch=%d err=%v", checkpointCount, err)
	}

	third := seedLegacyPendingTaskMessageForTest(t, ctx, store, owner.ID, created.Conversation.ID, created.Task.ID, "third pending", nil)
	unknownIDPayload := makePayload(3, makeEvent(3, "m9999999", "not a message for this Task"))
	if _, err := checkpoints.Save(ctx, makeCheckpoint(fence, 3, 3, unknownIDPayload)); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("unmapped root message checkpoint accepted: %v", err)
	}
	var thirdStatus string
	if err := store.pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, third.ID).Scan(&thirdStatus); err != nil || thirdStatus != string(conversation.DeliveryUnknown) {
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
