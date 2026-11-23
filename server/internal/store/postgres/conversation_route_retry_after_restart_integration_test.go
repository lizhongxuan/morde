package postgres

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/worker"
)

func TestConversationRouteRetryAfterWaitSurvivesWorkerRestart(t *testing.T) {
	client := &routeModelClientTest{
		err:   &app.ModelClientError{Kind: app.ModelErrorRateLimited, Stage: "provider_status", RetryAfter: 5 * time.Minute},
		reply: `{"action":"reply","task_id":"","question":"","reply":"recovered after worker restart"}`,
	}
	store, gateway, principal, initialClaim, _ := routeModelFixture(t, false, client)
	ctx := context.Background()
	if err := store.ReleaseConversationRoute(ctx, initialClaim); err != nil {
		t.Fatal(err)
	}

	newWorker := func(t *testing.T, workerID string) *worker.ConversationRouteWorker {
		t.Helper()
		result, err := worker.NewConversationRouteWorker(worker.ConversationRouteWorkerOptions{
			Queue: store, Actions: store, Models: gateway, WorkerID: workerID,
			LeaseDuration: time.Minute, PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	func() {
		workerA := newWorker(t, "route-retry-after-worker-a")
		if found, err := workerA.RunOnce(ctx); err != nil || !found {
			t.Fatalf("Worker A rate-limited route: found=%t err=%v", found, err)
		}
	}()

	var scheduledState string
	var scheduledAttempt int64
	var availableAfter time.Time
	if err := store.pool.QueryRow(ctx, `SELECT state,route_attempt,available_after
FROM conversation_message_routes WHERE message_id=$1`, initialClaim.Message.ID).
		Scan(&scheduledState, &scheduledAttempt, &availableAfter); err != nil || scheduledState != "pending" ||
		scheduledAttempt != initialClaim.RouteAttempt+1 || !availableAfter.After(time.Now()) {
		t.Fatalf("Worker A did not persist a future retry: state=%q attempt=%d available_after=%s err=%v",
			scheduledState, scheduledAttempt, availableAfter, err)
	}

	var firstCallID, firstRouteCallState, firstLedgerState string
	if err := store.pool.QueryRow(ctx, `SELECT c.call_id,c.state,l.state FROM conversation_route_model_calls c
JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
WHERE c.message_id=$1 AND c.attempt=$2 AND c.phase='normal'`, initialClaim.Message.ID, initialClaim.RouteAttempt).
		Scan(&firstCallID, &firstRouteCallState, &firstLedgerState); err != nil ||
		firstRouteCallState != "rejected" || firstLedgerState != "rejected" {
		t.Fatalf("Worker A rejection ledger: call=%q route=%q ledger=%q err=%v",
			firstCallID, firstRouteCallState, firstLedgerState, err)
	}

	// Worker A has left scope. Worker B must rely on the persisted due time, not
	// on any in-memory timer or retry state owned by Worker A.
	workerB := newWorker(t, "route-retry-after-worker-b")
	if found, err := workerB.RunOnce(ctx); err != nil || found || client.calls.Load() != 1 {
		t.Fatalf("Worker B claimed route before available_after: found=%t providerCalls=%d err=%v",
			found, client.calls.Load(), err)
	}
	var stateBeforeDue string
	var attemptBeforeDue int64
	var dueBeforeDue bool
	if err := store.pool.QueryRow(ctx, `SELECT state,route_attempt,available_after<=clock_timestamp()
FROM conversation_message_routes WHERE message_id=$1`, initialClaim.Message.ID).
		Scan(&stateBeforeDue, &attemptBeforeDue, &dueBeforeDue); err != nil || stateBeforeDue != "pending" ||
		attemptBeforeDue != scheduledAttempt || dueBeforeDue {
		t.Fatalf("route changed before durable due time: state=%q attempt=%d due=%t err=%v",
			stateBeforeDue, attemptBeforeDue, dueBeforeDue, err)
	}

	if _, err := store.pool.Exec(ctx, `UPDATE conversation_message_routes
SET available_after=clock_timestamp()-interval '1 second' WHERE message_id=$1 AND state='pending'`, initialClaim.Message.ID); err != nil {
		t.Fatalf("advance persisted retry due time: %v", err)
	}
	client.err = nil
	if found, err := workerB.RunOnce(ctx); err != nil || !found {
		t.Fatalf("Worker B did not claim due route: found=%t err=%v", found, err)
	}
	if found, err := workerB.RunOnce(ctx); err != nil || found || client.calls.Load() != 2 {
		t.Fatalf("route was recovered more than once: found=%t providerCalls=%d err=%v",
			found, client.calls.Load(), err)
	}

	var finalState, finalFailureReason, persistedClientID, persistedContent, persistedActorID string
	var finalAttempt int64
	var userMessageCount int64
	if err := store.pool.QueryRow(ctx, `SELECT r.state,r.route_attempt,r.failure_reason,r.client_message_id,m.content,m.actor_id,
(SELECT count(*) FROM conversation_messages sibling WHERE sibling.conversation_id=m.conversation_id AND sibling.role='user')
FROM conversation_message_routes r JOIN conversation_messages m ON m.id=r.message_id
WHERE r.message_id=$1`, initialClaim.Message.ID).
		Scan(&finalState, &finalAttempt, &finalFailureReason, &persistedClientID, &persistedContent, &persistedActorID, &userMessageCount); err != nil ||
		finalState != "routed" || finalAttempt != scheduledAttempt || finalFailureReason != "" ||
		persistedClientID != initialClaim.Message.ClientMessageID || persistedContent != initialClaim.Message.Content ||
		persistedActorID != principal.UserID || userMessageCount != 1 {
		t.Fatalf("recovered route did not retain the original message: state=%q attempt=%d reason=%q client_message_id=%q content=%q actor=%q user_messages=%d err=%v",
			finalState, finalAttempt, finalFailureReason, persistedClientID, persistedContent, persistedActorID, userMessageCount, err)
	}

	rows, err := store.pool.Query(ctx, `SELECT c.attempt,c.call_id,c.state,l.state
FROM conversation_route_model_calls c JOIN model_call_ledger l
 ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
WHERE c.message_id=$1 AND c.phase='normal' ORDER BY c.attempt`, initialClaim.Message.ID)
	if err != nil {
		t.Fatalf("read route call identities after recovery: %v", err)
	}
	defer rows.Close()
	type persistedCall struct {
		attempt           int64
		id, route, ledger string
	}
	var calls []persistedCall
	for rows.Next() {
		var call persistedCall
		if err := rows.Scan(&call.attempt, &call.id, &call.route, &call.ledger); err != nil {
			t.Fatalf("scan route call identity: %v", err)
		}
		calls = append(calls, call)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate route call identities: %v", err)
	}
	if len(calls) != 2 || calls[0].attempt != initialClaim.RouteAttempt || calls[0].id != firstCallID ||
		calls[0].route != "rejected" || calls[0].ledger != "rejected" ||
		calls[1].attempt != scheduledAttempt || calls[1].id == firstCallID ||
		calls[1].route != "settled" || calls[1].ledger != "settled" {
		t.Fatalf("recovered route call identities = %+v; want original rejected call and one distinct settled call", calls)
	}
}
