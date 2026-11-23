package postgres

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/worker"
)

func TestConversationRouteRetryAfterBeyondDurableHorizonFailsClosedInPostgres(t *testing.T) {
	for _, scenario := range []struct {
		name string
	}{
		{name: "future HTTP date beyond 24 hours"},
		{name: "integer too large to represent"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// internal/model tests exercise the real Retry-After headers and assert
			// that both forms produce RetryAfterExceedsDurableHorizon. Here that
			// parsed provider error crosses the gateway, Worker, and PostgreSQL
			// boundaries to verify it cannot become a short retry.
			client := &routeModelClientTest{err: &app.ModelClientError{
				Kind: app.ModelErrorRateLimited, Stage: "provider_status",
				RetryAfterExceedsDurableHorizon: true,
			}}
			store, gateway, _, initialClaim, _ := routeModelFixture(t, false, client)
			ctx := context.Background()
			if err := store.ReleaseConversationRoute(ctx, initialClaim); err != nil {
				t.Fatalf("release initial route claim: %v", err)
			}

			routeWorker, err := worker.NewConversationRouteWorker(worker.ConversationRouteWorkerOptions{
				Queue: store, Actions: store, Models: gateway,
				WorkerID: "route-retry-after-horizon-worker", LeaseDuration: time.Minute,
				PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop(),
			})
			if err != nil {
				t.Fatalf("create route Worker: %v", err)
			}
			if found, err := routeWorker.RunOnce(ctx); err != nil || !found {
				t.Fatalf("process over-horizon rate limit: found=%t err=%v", found, err)
			}

			var routeState, failureReason, routeCallState, ledgerState string
			var routeAttempt int64
			if err := store.pool.QueryRow(ctx, `SELECT r.state,r.route_attempt,r.failure_reason,c.state,l.state
FROM conversation_message_routes r
JOIN conversation_route_model_calls c ON c.message_id=r.message_id AND c.attempt=$2 AND c.phase='normal'
JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
WHERE r.message_id=$1`, initialClaim.Message.ID, initialClaim.RouteAttempt).
				Scan(&routeState, &routeAttempt, &failureReason, &routeCallState, &ledgerState); err != nil {
				t.Fatalf("read persisted over-horizon route state: %v", err)
			}
			if routeState != "failed" || routeAttempt != initialClaim.RouteAttempt || failureReason == "" ||
				routeCallState != "rejected" || ledgerState != "rejected" {
				t.Fatalf("over-horizon route state=%q attempt=%d failure=%q call=%q ledger=%q; want failed without a new attempt and rejected billing evidence",
					routeState, routeAttempt, failureReason, routeCallState, ledgerState)
			}

			newWorker, err := worker.NewConversationRouteWorker(worker.ConversationRouteWorkerOptions{
				Queue: store, Actions: store, Models: gateway,
				WorkerID: "route-retry-after-horizon-recovery-worker", LeaseDuration: time.Minute,
				PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop(),
			})
			if err != nil {
				t.Fatalf("create recovery Worker: %v", err)
			}
			if found, err := newWorker.RunOnce(ctx); err != nil || found || client.calls.Load() != 1 {
				t.Fatalf("recovery Worker reclaimed over-horizon route: found=%t providerCalls=%d err=%v",
					found, client.calls.Load(), err)
			}
		})
	}
}
