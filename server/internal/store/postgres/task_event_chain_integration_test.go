package postgres

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

// TestPostgresTaskEventChainFactsAndReplay drives one complete fix task
// through its event chain: raw events project to the nine semantic kinds,
// the reviewed validation state and the published receipt supply the
// verification and delivery entries, and the replayed chain runs from
// head to tail.
func TestPostgresTaskEventChainFactsAndReplay(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	conversationValue := createMainConversationForTest(t, store, owner.ID)
	submission := newTestTaskSubmission(t, owner.ID, "", "e08-chain-task", "fix the bug")
	submission.Task.ConversationID = conversationValue.ID
	submission.Message.ConversationID = conversationValue.ID
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	taskID := created.Task.ID
	resultContent := "fixed the bug"
	resultDigest := sha256.Sum256([]byte(resultContent))
	insertEvent := func(sequence int64, eventType, payload string, resultSHA []byte) {
		t.Helper()
		if _, err := store.pool.Exec(ctx, `INSERT INTO task_events(task_id,sequence,event_type,payload,task_revision,result_content_sha256)
VALUES ($1,$2,$3,$4::jsonb,0,$5)`, taskID, sequence, eventType, payload, resultSHA); err != nil {
			t.Fatalf("insert task event %d %s: %v", sequence, eventType, err)
		}
	}
	insertEvent(2, "task.plan", `{}`, nil)
	insertEvent(3, "task.status", `{"status":"running"}`, nil)
	insertEvent(4, "task.step", `{"status":"succeeded"}`, nil)
	insertEvent(5, "task.result", `{"status":"completed"}`, resultDigest[:])
	if _, err := store.pool.Exec(ctx, `UPDATE tasks SET status='completed',current_revision=0,desired_revision=0,result=$2 WHERE id=$1`,
		taskID, resultContent); err != nil {
		t.Fatalf("complete task: %v", err)
	}
	facts, err := store.TaskEventChainFacts(ctx, owner.ID, taskID)
	if err != nil {
		t.Fatalf("chain facts before review: %v", err)
	}
	if facts != (app.TaskEventChainFacts{}) {
		t.Fatalf("chain facts before review = %+v, want empty", facts)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO task_validation_reviews
(task_id,owner_id,task_revision,result_event_sequence,result_sha256,state,validation_state)
VALUES ($1,$2,0,5,$3,'reviewed','PASS')`, taskID, owner.ID, resultDigest[:]); err != nil {
		t.Fatalf("insert validation review: %v", err)
	}
	facts, err = store.TaskEventChainFacts(ctx, owner.ID, taskID)
	if err != nil {
		t.Fatalf("chain facts after review: %v", err)
	}
	if facts != (app.TaskEventChainFacts{ValidationState: app.TaskValidationPass}) {
		t.Fatalf("chain facts after review = %+v, want {PASS false}", facts)
	}
	var serviceInstanceID string
	if err := store.pool.QueryRow(ctx, `SELECT instance_id::text FROM service_identity WHERE singleton=true`).Scan(&serviceInstanceID); err != nil {
		t.Fatal(err)
	}
	batchID := mustID(t, domain.NewMessageID)
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		return insertMessage(ctx, tx, conversation.Message{ID: batchID, OwnerID: owner.ID, ActorID: owner.ID,
			ConversationID: conversationValue.ID, Role: conversation.RoleAssistant, Content: resultContent, SourceKind: "conversation"})
	}); err != nil {
		t.Fatalf("insert receipt message: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversation_result_batches
(message_id,service_instance_id,owner_id,actor_id,conversation_id,state,published_message_id,created_at,deadline)
VALUES ($1,$2,$3,$3,$4,'published',$1,now(),now()+interval '1 second')`,
		batchID, serviceInstanceID, owner.ID, conversationValue.ID); err != nil {
		t.Fatalf("insert published batch: %v", err)
	}
	turnID := mustID(t, domain.NewStepID)
	if _, err := store.pool.Exec(ctx, `INSERT INTO conversation_result_sources
(batch_message_id,owner_id,actor_id,conversation_id,user_message_id,task_id,task_revision,event_sequence,turn_id,content,source_order,state,payload_sha256,authorization_version)
VALUES ($1,$2,$2,$3,$4,$5,0,5,$6,$7,1,'published',$8,1)`, batchID, owner.ID, conversationValue.ID,
		created.Message.ID, taskID, turnID, resultContent, resultDigest[:]); err != nil {
		t.Fatalf("insert published source: %v", err)
	}
	facts, err = store.TaskEventChainFacts(ctx, owner.ID, taskID)
	if err != nil {
		t.Fatalf("chain facts after publish: %v", err)
	}
	if facts != (app.TaskEventChainFacts{ValidationState: app.TaskValidationPass, Delivered: true}) {
		t.Fatalf("chain facts after publish = %+v, want {PASS true}", facts)
	}
	service, err := app.NewTaskEventChainService(store)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := service.Replay(ctx, identity.Principal{UserID: owner.ID}, taskID)
	if err != nil {
		t.Fatalf("replay chain: %v", err)
	}
	if !chain.Valid {
		t.Fatalf("complete fix chain must be valid, reasons: %v", chain.InvalidReasons)
	}
	wantKinds := []task.EventKind{task.EventKindCreated, task.EventKindSplit, task.EventKindDispatched,
		task.EventKindExecution, task.EventKindVerification, task.EventKindDelivery}
	if len(chain.Entries) != len(wantKinds) {
		t.Fatalf("chain entries = %d, want %d: %+v", len(chain.Entries), len(wantKinds), chain.Entries)
	}
	for index, want := range wantKinds {
		if chain.Entries[index].Kind != want {
			t.Fatalf("chain entry %d kind = %q, want %q", index, chain.Entries[index].Kind, want)
		}
	}
	if len(chain.UnmappedSequences) != 1 || chain.UnmappedSequences[0] != 5 {
		t.Fatalf("unmapped sequences = %v, want [5]", chain.UnmappedSequences)
	}
	if _, err := store.TaskEventChainFacts(ctx, owner.ID, "q0000000"); err == nil {
		t.Fatalf("chain facts for a missing task must fail")
	}
}
