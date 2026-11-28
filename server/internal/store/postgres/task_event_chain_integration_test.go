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

// TestPostgresTaskEventChainRejectsUnboundValidationReviewAndReceipt ensures
// a review row alone cannot provide verification facts or justify delivery.
func TestPostgresTaskEventChainRejectsUnboundValidationReviewAndReceipt(t *testing.T) {
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
(task_id,owner_id,task_revision,result_event_sequence,result_sha256,state,reviewer_session_id,
 execution_model_id,execution_connection_version,execution_endpoint,execution_configured_model,execution_reported_model,
 execution_model_family,execution_catalog_version,reviewer_model_id,reviewer_connection_version,reviewer_endpoint,
 reviewer_configured_model,reviewer_reported_model,reviewer_model_family,reviewer_catalog_version,review_report,
 reviewed_at,execution_model_call_id,validation_state)
VALUES ($1,$2,0,5,$3,'reviewed',$4,
 'p1234567',clock_timestamp(),'https://example.test/v1','fixture-execution','fixture-execution',
 'fixture-execution-family','fixture-catalog','p2234567',clock_timestamp(),'https://example.test/v1',
 'fixture-reviewer','fixture-reviewer','fixture-reviewer-family','fixture-catalog','{"summary":"fixture"}'::jsonb,
 clock_timestamp(),$5,'PASS')`, taskID, owner.ID, resultDigest[:], "review_"+taskID, "execution_"+taskID); err != nil {
		t.Fatalf("insert validation review: %v", err)
	}
	facts, err = store.TaskEventChainFacts(ctx, owner.ID, taskID)
	if err != nil {
		t.Fatalf("chain facts after review: %v", err)
	}
	if facts != (app.TaskEventChainFacts{}) {
		t.Fatalf("unbound PASS review produced chain facts = %+v; want no validation or delivery facts", facts)
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
	if facts != (app.TaskEventChainFacts{Delivered: true}) {
		t.Fatalf("unverified receipt chain facts = %+v, want delivery fact without a validation verdict", facts)
	}
	service, err := app.NewTaskEventChainService(store)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := service.Replay(ctx, identity.Principal{UserID: owner.ID}, taskID)
	if err != nil {
		t.Fatalf("replay chain: %v", err)
	}
	if chain.Valid {
		t.Fatal("published result without bound validation evidence produced a valid event chain")
	}
	wantKinds := []task.EventKind{task.EventKindCreated, task.EventKindSplit, task.EventKindDispatched, task.EventKindExecution}
	if len(chain.Entries) != len(wantKinds) {
		t.Fatalf("chain entries = %d, want %d: %+v", len(chain.Entries), len(wantKinds), chain.Entries)
	}
	for index, want := range wantKinds {
		if chain.Entries[index].Kind != want {
			t.Fatalf("chain entry %d kind = %q, want %q", index, chain.Entries[index].Kind, want)
		}
	}
	for _, entry := range chain.Entries {
		if entry.Kind == task.EventKindVerification || entry.Kind == task.EventKindDelivery {
			t.Fatalf("unbound PASS review supplied synthetic chain entry: %+v", entry)
		}
	}
	foundUnverifiedDelivery := false
	for _, reason := range chain.InvalidReasons {
		if reason == "delivered without passing verification" {
			foundUnverifiedDelivery = true
			break
		}
	}
	if !foundUnverifiedDelivery {
		t.Fatalf("unverified delivery invalid reason missing: %v", chain.InvalidReasons)
	}
	if len(chain.UnmappedSequences) != 1 || chain.UnmappedSequences[0] != 5 {
		t.Fatalf("unmapped sequences = %v, want [5]", chain.UnmappedSequences)
	}
	if _, err := store.TaskEventChainFacts(ctx, owner.ID, "q0000000"); err == nil {
		t.Fatalf("chain facts for a missing task must fail")
	}
}
