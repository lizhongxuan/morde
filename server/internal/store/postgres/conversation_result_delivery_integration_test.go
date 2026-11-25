package postgres

import (
	"context"
	"crypto/sha256"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
)

// TestPostgresTaskResultDeliveryRefusesFailedValidation is the E11 gate at
// the store level: a batch whose only source currently fails validation is
// refused whole. Nothing is published (atomic refusal), the route fails
// with routing_action_unavailable, and the batch is marked failed.
func TestPostgresTaskResultDeliveryRefusesFailedValidation(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	value := f.createTask(t, f.ownerID, "unverified fix attempt")
	if !f.complete(t, &value, "fix without passing validation") {
		t.Fatal("task result was not queued")
	}
	digest := sha256.Sum256([]byte(value.Content))
	seedConversationResultValidationReview(t, f, value, value.TaskRevision, value.EventSequence, digest[:], app.TaskValidationFail)
	f.runNextReview(t)
	var batchID, batchState, triggerID string
	if err := f.store.pool.QueryRow(ctx, `SELECT message_id,state,trigger_message_id
FROM conversation_result_batches WHERE owner_id=$1`, f.ownerID).Scan(&batchID, &batchState, &triggerID); err != nil {
		t.Fatalf("read refused result batch: %v", err)
	}
	if batchState != "failed" {
		t.Fatalf("batch state = %q, want failed", batchState)
	}
	var routeState, failureReason string
	if err := f.store.pool.QueryRow(ctx, `SELECT state,failure_reason FROM conversation_message_routes
WHERE message_id=$1`, triggerID).Scan(&routeState, &failureReason); err != nil {
		t.Fatalf("read failed result route: %v", err)
	}
	if routeState != "failed" || failureReason != "routing_action_unavailable" {
		t.Fatalf("route state = %q reason = %q, want failed/routing_action_unavailable", routeState, failureReason)
	}
	var pendingSources int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_result_sources
WHERE batch_message_id=$1 AND state='pending'`, batchID).Scan(&pendingSources); err != nil || pendingSources != 1 {
		t.Fatalf("pending sources = %d err=%v, want 1: refusal must leave the source unpublished", pendingSources, err)
	}
	var publishedMessages int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE id=$1`, batchID).Scan(&publishedMessages); err != nil || publishedMessages != 0 {
		t.Fatalf("published messages = %d err=%v, want 0: no receipt may be published for a failed validation", publishedMessages, err)
	}
}

// TestPostgresTaskResultDeliveryRetryPublishesAfterReReviewPasses covers the
// retry half of E11: after the gate refuses a FAIL batch, an authorized
// retry re-runs the route once a re-review passes, and the gate re-checks
// the current verdict at publish time. The validation_state UPDATE below
// stands in for a completed re-review; the review worker's own completion
// path is covered by the E09 tests.
func TestPostgresTaskResultDeliveryRetryPublishesAfterReReviewPasses(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	value := f.createTask(t, f.ownerID, "fix that fails review first")
	if !f.complete(t, &value, "fix reviewed twice") {
		t.Fatal("task result was not queued")
	}
	digest := sha256.Sum256([]byte(value.Content))
	seedConversationResultValidationReview(t, f, value, value.TaskRevision, value.EventSequence, digest[:], app.TaskValidationFail)
	f.runNextReview(t)
	var batchID, triggerID string
	if err := f.store.pool.QueryRow(ctx, `SELECT batch_message_id FROM conversation_result_sources WHERE task_id=$1`, value.ID).Scan(&batchID); err != nil {
		t.Fatalf("read result batch: %v", err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT trigger_message_id FROM conversation_result_batches WHERE message_id=$1`, batchID).Scan(&triggerID); err != nil {
		t.Fatalf("read result trigger: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_validation_reviews SET validation_state='PASS',updated_at=clock_timestamp()
WHERE task_id=$1 AND task_revision=$2`, value.ID, value.TaskRevision); err != nil {
		t.Fatalf("simulate passing re-review: %v", err)
	}
	instanceID, err := f.store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: f.ownerID, Kind: identity.KindSession, TokenID: testSessionTokenID(f.ownerID)}
	if err := f.store.RetryConversationRoute(ctx, principal, "", f.conversation.ID, triggerID, instanceID, "delivery-retry-key-001"); err != nil {
		t.Fatalf("retry refused result review: %v", err)
	}
	f.runNextReview(t)
	var batchState string
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM conversation_result_batches WHERE message_id=$1`, batchID).Scan(&batchState); err != nil || batchState != "published" {
		t.Fatalf("retried batch state = %q err=%v, want published", batchState, err)
	}
}

// TestPostgresTaskResultDeliveryPublishesPassingValidation is the gate's
// positive control: a PASS verdict publishes, and pending verdicts do not
// block delivery (the decided pending rule).
func TestPostgresTaskResultDeliveryPublishesPassingValidation(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	passed := f.createTask(t, f.ownerID, "passing fix")
	pending := f.createTask(t, f.ownerID, "fix without review yet")
	if !f.complete(t, &passed, "verified fix") || !f.complete(t, &pending, "unreviewed fix") {
		t.Fatal("task results were not queued")
	}
	digest := sha256.Sum256([]byte(passed.Content))
	seedConversationResultValidationReview(t, f, passed, passed.TaskRevision, passed.EventSequence, digest[:], app.TaskValidationPass)
	f.runNextReview(t)
	var published int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_result_batches
WHERE owner_id=$1 AND state='published'`, f.ownerID).Scan(&published); err != nil || published != 1 {
		t.Fatalf("published batches = %d err=%v, want 1", published, err)
	}
}
