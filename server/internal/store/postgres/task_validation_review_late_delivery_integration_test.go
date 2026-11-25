package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskValidationLateConclusionOutboxIsTransactional(t *testing.T) {
	for _, test := range []struct {
		name          string
		direct        bool
		pendingResult bool
		publishFirst  bool
	}{{"published result batch", false, false, false}, {"completed assistant result", true, false, false},
		{"first result pending", false, true, false}, {"publication wins validation race", false, true, true}} {
		t.Run(test.name, func(t *testing.T) {
			testPostgresTaskValidationLateConclusionOutbox(t, test.direct, test.pendingResult, test.publishFirst)
		})
	}
}

func testPostgresTaskValidationLateConclusionOutbox(t *testing.T, directResult, pendingResult, publishFirst bool) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	lease, _, err := f.service.Start(ctx, f.request)
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.store.GetTask(ctx, lease.Fence.TaskID, lease.Fence.OwnerID)
	if err != nil || current.Task.CurrentRevision == nil {
		t.Fatalf("load running Task: current=%+v err=%v", current.Task, err)
	}
	for _, model := range []struct{ id, name, endpoint, model string }{
		{"p1234567", "review execution", "https://api.openai.com/v1", "gpt-6-luna"},
		{"p2234567", "review reviewer", "https://api.deepseek.com", "deepseek-flash"},
	} {
		if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,checked_at,capability_text)
VALUES ($1,NULL,$2,$3,$4,decode(repeat('00',29),'hex'),clock_timestamp(),true)`, model.id, model.name, model.endpoint, model.model); err != nil {
			t.Fatal(err)
		}
	}
	executionModel := seedTaskValidationExecutionModelResponse(t, f.store, lease.Fence)
	var sourceMessageID string
	if err := f.store.pool.QueryRow(ctx, `SELECT id FROM conversation_messages
WHERE task_id=$1 AND role='user' AND delivery_status='applied' ORDER BY sequence LIMIT 1`, current.Task.ID).Scan(&sourceMessageID); err != nil {
		t.Fatal(err)
	}
	result := "The requested change is complete and verified."
	resultStepID := mustID(t, domain.NewStepID)
	event, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: current.Task.ID, LeaseOwner: lease.Fence.LeaseOwner, LeaseVersion: lease.Fence.LeaseVersion,
		Status: task.StatusCompleted, CurrentStep: resultStepID, Result: result, Usage: current.Task.Usage,
		Steps: []task.Step{{ID: resultStepID, TaskID: current.Task.ID, Sequence: 1, Kind: "model", Status: task.StepSucceeded}},
		Event: task.Event{TaskID: current.Task.ID, Type: "task.result", Payload: []byte(`{"status":"completed"}`)},
		ValidationClaims: []app.TaskResultClaim{{ID: "claim-1", StartByte: 0, EndByte: int32(len(result)), EvidenceRefs: []app.TaskValidationEvidenceRef{
			{Kind: app.TaskValidationEvidenceSource, ID: sourceMessageID},
		}}},
	})
	if err != nil {
		t.Fatalf("complete Task with evidence: %v", err)
	}
	if directResult {
		seedDeliveredTaskAssistantResultForLateValidation(t, f.store, current.Task.ID, resultStepID, event.Sequence, result)
	} else {
		seedTaskResultBatchForLateValidation(t, f.store, current.Task.ID, sourceMessageID, resultStepID, event.Sequence, result, !pendingResult)
	}
	claims, err := f.store.ClaimTaskValidationReviews(ctx, "late-review-worker", 30*time.Second, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim validation review: claims=%+v err=%v", claims, err)
	}
	current, err = f.store.GetTask(ctx, current.Task.ID, current.Task.OwnerID)
	if err != nil || current.ValidationEvidence == nil {
		t.Fatalf("load validation evidence: evidence=%+v err=%v", current.ValidationEvidence, err)
	}
	completion := taskValidationReviewCompletionFixture(t, f.store, claims[0], current.ValidationEvidence)
	completion.ExecutionModel = executionModel
	completion.Report.Findings[0].Assessment = app.TaskValidationReviewContradicts
	completion.Report.Recommendation = app.TaskValidationFail
	completion.ReviewerModel.SessionID = completion.SessionID
	completion.ReviewerModel.ThroughSequence = 0
	completion.ReviewerModel, completion.ResponseText = settleTaskValidationReviewModelResponse(t, f.store, claims[0], completion.ReviewerModel, completion.Report)
	if publishFirst {
		receipt := publishTaskResultReviewBeforeValidationCompletion(t, f.store, completion)
		if !bytes.Contains([]byte(receipt), []byte("pending")) {
			t.Fatalf("first receipt should preserve the pre-review pending state, got %q", receipt)
		}
		var outboxCount int
		if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_terminal_delivery_outbox
WHERE task_id=$1 AND source_kind='late_validation'`, current.Task.ID).Scan(&outboxCount); err != nil || outboxCount != 1 {
			t.Fatalf("concurrent late validation created %d outbox rows err=%v; want one", outboxCount, err)
		}
		return
	}
	if pendingResult {
		if updated, err := f.store.CompleteTaskValidationReview(ctx, completion); err != nil || !updated {
			t.Fatalf("complete review before first result publication: updated=%t err=%v", updated, err)
		}
		var outboxCount int
		if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_terminal_delivery_outbox
WHERE task_id=$1 AND source_kind='late_validation'`, current.Task.ID).Scan(&outboxCount); err != nil || outboxCount != 0 {
			t.Fatalf("unpublished result created %d late-validation outbox rows err=%v", outboxCount, err)
		}
		var validationState app.TaskValidationState
		if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
			var err error
			validationState, err = conversationTaskResultValidationStateInTx(ctx, tx, current.Task.OwnerID,
				app.ConversationTaskResultSource{TaskID: current.Task.ID, TaskRevision: *current.Task.CurrentRevision,
					EventSequence: event.Sequence, Content: result})
			return err
		}); err != nil || validationState != app.TaskValidationFail {
			t.Fatalf("first result validation state=%q err=%v; want persisted FAIL", validationState, err)
		}
		receipt := publishTaskResultReview(t, f.store, current.Task.ID)
		if !bytes.Contains([]byte(receipt), []byte("FAIL")) || !bytes.Contains([]byte(receipt), []byte("验证未通过")) {
			t.Fatalf("first result receipt did not include the persisted FAIL state: %q", receipt)
		}
		return
	}

	if _, err := f.store.pool.Exec(ctx, `CREATE FUNCTION test_reject_late_validation_delivery() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced late validation delivery insert failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `CREATE TRIGGER test_reject_late_validation_delivery
BEFORE INSERT ON task_terminal_delivery_outbox FOR EACH ROW
WHEN (NEW.source_kind='late_validation') EXECUTE FUNCTION test_reject_late_validation_delivery()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.store.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_reject_late_validation_delivery ON task_terminal_delivery_outbox`)
		_, _ = f.store.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_reject_late_validation_delivery()`)
	})
	if updated, err := f.store.CompleteTaskValidationReview(ctx, completion); err == nil || updated {
		t.Fatalf("review completed despite failed late-delivery insert: updated=%t err=%v", updated, err)
	}
	var reviewState string
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM task_validation_reviews WHERE task_id=$1`, current.Task.ID).Scan(&reviewState); err != nil || reviewState != "processing" {
		t.Fatalf("failed late-delivery insert changed review state to %q err=%v; want processing", reviewState, err)
	}
	var outboxCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_terminal_delivery_outbox
WHERE task_id=$1 AND source_kind='late_validation'`, current.Task.ID).Scan(&outboxCount); err != nil || outboxCount != 0 {
		t.Fatalf("failed late-delivery transaction left %d outbox rows err=%v", outboxCount, err)
	}
	if _, err := f.store.pool.Exec(ctx, `DROP TRIGGER test_reject_late_validation_delivery ON task_terminal_delivery_outbox`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `DROP FUNCTION test_reject_late_validation_delivery()`); err != nil {
		t.Fatal(err)
	}

	if updated, err := f.store.CompleteTaskValidationReview(ctx, completion); err != nil || !updated {
		t.Fatalf("complete late-failing review: updated=%t err=%v", updated, err)
	}
	var ownerID, conversationID, initiatorID, kind, eventType, state, eventPayload string
	var taskRevision, authorizationVersion, eventSequence, claimGeneration int64
	var sourceDigest, resultDigest, verdictDigest []byte
	if err := f.store.pool.QueryRow(ctx, `SELECT delivery.owner_id,delivery.conversation_id,delivery.initiator_id,
delivery.task_revision,delivery.authorization_version,delivery.source_kind,delivery.source_event_sequence,
delivery.source_event_type,delivery.source_sha256,delivery.result_sha256,delivery.validation_claim_generation,
delivery.verdict_sha256,delivery.state,event.payload::text
FROM task_terminal_delivery_outbox delivery
JOIN task_events event ON event.task_id=delivery.task_id AND event.sequence=delivery.source_event_sequence
WHERE delivery.task_id=$1 AND delivery.source_kind='late_validation'`, current.Task.ID).Scan(
		&ownerID, &conversationID, &initiatorID, &taskRevision, &authorizationVersion, &kind, &eventSequence,
		&eventType, &sourceDigest, &resultDigest, &claimGeneration, &verdictDigest, &state, &eventPayload); err != nil {
		t.Fatalf("read late validation source: %v", err)
	}
	eventDigest := sha256.Sum256([]byte(eventPayload))
	wantResultDigest := sha256.Sum256([]byte(result))
	if ownerID != current.Task.OwnerID || conversationID != current.Task.ConversationID || initiatorID != current.Task.InitiatorID ||
		taskRevision != *current.Task.CurrentRevision || authorizationVersion != lease.Fence.AuthorizationVersion ||
		kind != "late_validation" || eventSequence != event.Sequence || eventType != "task.result" || state != "pending" ||
		claimGeneration != claims[0].ClaimGeneration || !bytes.Equal(sourceDigest, eventDigest[:]) ||
		!bytes.Equal(resultDigest, wantResultDigest[:]) || len(verdictDigest) != sha256.Size {
		t.Fatalf("late validation source identity mismatch: owner=%q conversation=%q initiator=%q revision=%d auth=%d kind=%q event=%d/%q claim=%d state=%q",
			ownerID, conversationID, initiatorID, taskRevision, authorizationVersion, kind, eventSequence, eventType, claimGeneration, state)
	}
	if updated, err := f.store.CompleteTaskValidationReview(ctx, completion); err != nil || updated {
		t.Fatalf("duplicate review completion changed persisted result: updated=%t err=%v", updated, err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_terminal_delivery_outbox
WHERE task_id=$1 AND source_kind='late_validation'`, current.Task.ID).Scan(&outboxCount); err != nil || outboxCount != 1 {
		t.Fatalf("duplicate review completion created %d late outbox rows err=%v", outboxCount, err)
	}
}

func publishTaskResultReview(t *testing.T, store *Store, taskID string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := store.pool.Exec(ctx, `WITH now_value AS (SELECT clock_timestamp() AS value)
UPDATE conversation_result_batches batch
SET created_at=now_value.value-interval '3 seconds',deadline=now_value.value-interval '1 second'
FROM now_value
WHERE batch.message_id=(SELECT source.batch_message_id FROM conversation_result_sources source
WHERE source.task_id=$1 AND source.state='pending' LIMIT 1) AND batch.state='pending'`, taskID); err != nil {
		t.Fatalf("make Task result review ready: %v", err)
	}
	if queued, err := store.QueueReadyConversationResultReviews(ctx, 10); err != nil || queued != 1 {
		t.Fatalf("queue first Task result review: queued=%d err=%v", queued, err)
	}
	claim, found, err := store.ClaimConversationRoute(ctx, "late-result-publisher", time.Minute)
	if err != nil || !found || claim.Input.ResultReviewID == "" {
		t.Fatalf("claim first Task result review route: found=%t claim=%+v err=%v", found, claim, err)
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		return publishConversationResultReviewInTx(ctx, tx, claim, resultReviewTestReply)
	}); err != nil {
		t.Fatalf("publish first Task result receipt: %v", err)
	}
	return queryPublishedTaskResultReceipt(t, store, claim.Input.ResultReviewID)
}

func publishTaskResultReviewBeforeValidationCompletion(t *testing.T, store *Store,
	completion app.TaskValidationReviewCompletion) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := store.pool.Exec(ctx, `WITH now_value AS (SELECT clock_timestamp() AS value)
UPDATE conversation_result_batches batch
SET created_at=now_value.value-interval '3 seconds',deadline=now_value.value-interval '1 second'
FROM now_value
WHERE batch.message_id=(SELECT source.batch_message_id FROM conversation_result_sources source
WHERE source.task_id=$1 AND source.state='pending' LIMIT 1) AND batch.state='pending'`, completion.Claim.TaskID); err != nil {
		t.Fatalf("make concurrent Task result review ready: %v", err)
	}
	if queued, err := store.QueueReadyConversationResultReviews(ctx, 10); err != nil || queued != 1 {
		t.Fatalf("queue concurrent Task result review: queued=%d err=%v", queued, err)
	}
	claim, found, err := store.ClaimConversationRoute(ctx, "late-result-race-publisher", time.Minute)
	if err != nil || !found || claim.Input.ResultReviewID == "" {
		t.Fatalf("claim concurrent Task result review route: found=%t claim=%+v err=%v", found, claim, err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin concurrent first result publication: %v", err)
	}
	txOpen := true
	defer func() {
		if txOpen {
			_ = tx.Rollback(context.Background())
		}
	}()
	if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
		t.Fatalf("lock concurrent first result review route: %v", err)
	}
	if err := publishConversationResultReviewInTx(ctx, tx, claim, resultReviewTestReply); err != nil {
		t.Fatalf("stage concurrent first result receipt: %v", err)
	}
	var publisherPID int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&publisherPID); err != nil {
		t.Fatalf("read concurrent first result publisher PID: %v", err)
	}
	completionDone := make(chan struct {
		updated bool
		err     error
	}, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		updated, err := store.CompleteTaskValidationReview(ctx, completion)
		completionDone <- struct {
			updated bool
			err     error
		}{updated, err}
	}()
	<-started
	waitCtx, waitCancel := context.WithTimeout(ctx, 3*time.Second)
	defer waitCancel()
	if err := waitForTaskValidationResultLock(waitCtx, store, publisherPID); err != nil {
		_ = tx.Rollback(context.Background())
		txOpen = false
		<-completionDone
		t.Fatalf("validation completion did not block behind the first result publication: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		txOpen = false
		t.Fatalf("commit first result publication in race: %v", err)
	}
	txOpen = false
	result := <-completionDone
	if result.err != nil || !result.updated {
		t.Fatalf("complete late validation after first publication: updated=%t err=%v", result.updated, result.err)
	}
	return queryPublishedTaskResultReceipt(t, store, claim.Input.ResultReviewID)
}

func waitForTaskValidationResultLock(ctx context.Context, store *Store, publisherPID int32) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := store.pool.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM pg_stat_activity
  WHERE datname=current_database() AND pid<>pg_backend_pid() AND pid<>$1
    AND wait_event_type='Lock' AND query ILIKE '%FOR UPDATE OF task%'
)`, publisherPID).Scan(&blocked)
		if err != nil {
			return fmt.Errorf("inspect validation lock wait: %w", err)
		}
		if blocked {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func queryPublishedTaskResultReceipt(t *testing.T, store *Store, batchID string) string {
	t.Helper()
	var receipt string
	if err := store.pool.QueryRow(context.Background(), `SELECT message.content
FROM conversation_result_batches batch JOIN conversation_messages message ON message.id=batch.published_message_id
WHERE batch.message_id=$1 AND batch.state='published'`, batchID).Scan(&receipt); err != nil {
		t.Fatalf("read published Task result receipt: %v", err)
	}
	return receipt
}

func seedDeliveredTaskAssistantResultForLateValidation(t *testing.T, store *Store, taskID, turnID string,
	eventSequence int64, result string) {
	t.Helper()
	ctx := context.Background()
	var ownerID, actorID, conversationID string
	var revision int64
	if err := store.pool.QueryRow(ctx, `SELECT owner_id,initiator_id,conversation_id,current_revision
FROM tasks WHERE id=$1`, taskID).Scan(&ownerID, &actorID, &conversationID, &revision); err != nil {
		t.Fatal(err)
	}
	messageID := mustID(t, domain.NewMessageID)
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		if err := insertMessage(ctx, tx, conversation.Message{ID: messageID, OwnerID: ownerID, ActorID: actorID,
			ConversationID: conversationID, TaskID: taskID, Role: conversation.RoleAssistant, Content: result}); err != nil {
			return err
		}
		updated, err := tx.Exec(ctx, `UPDATE conversation_messages SET output_status='completed',output_task_revision=$2,
output_result_event_sequence=$3 WHERE id=$1 AND task_id=$4 AND role='assistant'`, messageID, revision, eventSequence, taskID)
		if err != nil {
			return err
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "could not persist completed assistant result fixture")
		}
		return nil
	}); err != nil {
		t.Fatalf("seed delivered assistant result: %v", err)
	}
}

func seedTaskResultBatchForLateValidation(t *testing.T, store *Store, taskID, sourceMessageID, turnID string,
	eventSequence int64, result string, published bool) {
	t.Helper()
	ctx := context.Background()
	var ownerID, actorID, conversationID string
	var revision, authorizationVersion int64
	if err := store.pool.QueryRow(ctx, `SELECT owner_id,initiator_id,conversation_id,current_revision,auth_version
FROM tasks WHERE id=$1`, taskID).Scan(&ownerID, &actorID, &conversationID, &revision, &authorizationVersion); err != nil {
		t.Fatal(err)
	}
	messageID := mustID(t, domain.NewMessageID)
	digest := sha256.Sum256([]byte(result))
	state, publishedMessageID := "pending", any(nil)
	if published {
		state, publishedMessageID = "published", messageID
	}
	if err := store.WithTx(ctx, func(tx pgx.Tx) error {
		if published {
			if err := insertMessage(ctx, tx, conversation.Message{ID: messageID, OwnerID: ownerID, ActorID: actorID,
				ConversationID: conversationID, Role: conversation.RoleAssistant, Content: "The first result receipt.",
				SourceKind: "conversation", DeliveryStatus: conversation.DeliveryApplied}); err != nil {
				return err
			}
		}
		var serviceInstanceID string
		if err := tx.QueryRow(ctx, `SELECT instance_id::text FROM service_identity WHERE singleton=true`).Scan(&serviceInstanceID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO conversation_result_batches
(message_id,service_instance_id,owner_id,actor_id,conversation_id,authorization_version,state,published_message_id,created_at,deadline)
VALUES ($1,$2::uuid,$3,$4,$5,$6,$7,$8,clock_timestamp(),clock_timestamp()+interval '1 second')`,
			messageID, serviceInstanceID, ownerID, actorID, conversationID, authorizationVersion, state, publishedMessageID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO conversation_result_sources
(batch_message_id,owner_id,actor_id,conversation_id,user_message_id,task_id,task_revision,event_sequence,turn_id,content,
 payload_sha256,authorization_version,source_order,state)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,1,$13)`, messageID, ownerID, actorID, conversationID,
			sourceMessageID, taskID, revision, eventSequence, turnID, result, digest[:], authorizationVersion, state)
		return err
	}); err != nil {
		t.Fatalf("seed published Task result receipt: %v", err)
	}
}
