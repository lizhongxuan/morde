package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskTerminalDeliveryClaimSerializesActiveLeaseAndReplay(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	sequence := commitTaskTerminalDeliveryFailure(t, f)

	type result struct {
		claims []TaskTerminalDeliveryClaim
		err    error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var group sync.WaitGroup
	for _, workerID := range []string{"terminal-claim-a", "terminal-claim-b"} {
		workerID := workerID
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			claims, err := f.store.ClaimTaskTerminalDeliveryOutbox(ctx, workerID, 30*time.Second, 4)
			results <- result{claims: claims, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(results)

	var claimed []TaskTerminalDeliveryClaim
	for outcome := range results {
		if outcome.err != nil {
			t.Fatalf("concurrent terminal delivery claim: %v", outcome.err)
		}
		claimed = append(claimed, outcome.claims...)
	}
	if len(claimed) != 1 {
		t.Fatalf("concurrent claims returned %d rows, want exactly one", len(claimed))
	}
	first := claimed[0]
	if first.State != TaskTerminalDeliveryStateClaimed || first.ClaimGeneration != 1 ||
		first.SourceEventSequence != sequence || first.ModelCallID == "" || first.LeaseOwner == "" {
		t.Fatalf("unexpected first claim: %+v", first)
	}

	replay, err := f.store.ClaimTaskTerminalDeliveryOutbox(ctx, "terminal-claim-replay", 30*time.Second, 4)
	if err != nil || len(replay) != 0 {
		t.Fatalf("active claim was stolen on replay: claims=%+v err=%v", replay, err)
	}
	var state, leaseOwner, modelCallID string
	var generation int64
	var sourceCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT state,lease_owner,model_call_id,claim_generation
FROM task_terminal_delivery_outbox WHERE task_id=$1 AND source_event_sequence=$2 AND source_event_type='task.result'`,
		f.request.Fence.TaskID, sequence).Scan(&state, &leaseOwner, &modelCallID, &generation); err != nil {
		t.Fatal(err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_terminal_delivery_outbox
WHERE task_id=$1 AND source_event_sequence=$2 AND source_event_type='task.result'`,
		f.request.Fence.TaskID, sequence).Scan(&sourceCount); err != nil {
		t.Fatal(err)
	}
	if state != TaskTerminalDeliveryStateClaimed || leaseOwner != first.LeaseOwner ||
		modelCallID != first.ModelCallID || generation != 1 || sourceCount != 1 {
		t.Fatalf("replay changed source/claim: state=%q lease=%q call=%q generation=%d rows=%d",
			state, leaseOwner, modelCallID, generation, sourceCount)
	}
}

func TestPostgresTaskTerminalDeliveryClaimReclaimsExpiredLeaseWithNewGeneration(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	commitTaskTerminalDeliveryFailure(t, f)

	firstClaims, err := f.store.ClaimTaskTerminalDeliveryOutbox(ctx, "terminal-expired-first", 30*time.Second, 1)
	if err != nil || len(firstClaims) != 1 {
		t.Fatalf("first terminal delivery claim: claims=%+v err=%v", firstClaims, err)
	}
	first := firstClaims[0]
	if _, err := f.store.pool.Exec(ctx, `UPDATE task_terminal_delivery_outbox
SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	reclaimedClaims, err := f.store.ClaimTaskTerminalDeliveryOutbox(ctx, "terminal-expired-second", 30*time.Second, 1)
	if err != nil || len(reclaimedClaims) != 1 {
		t.Fatalf("reclaim expired terminal delivery: claims=%+v err=%v", reclaimedClaims, err)
	}
	reclaimed := reclaimedClaims[0]
	if reclaimed.ClaimGeneration != first.ClaimGeneration+1 || reclaimed.LeaseOwner != "terminal-expired-second" ||
		reclaimed.ModelCallID != first.ModelCallID || !reclaimed.LeaseExpiresAt.After(time.Now()) {
		t.Fatalf("expired lease reclaim changed identity or generation: first=%+v reclaimed=%+v", first, reclaimed)
	}
	if changed, err := f.store.MarkTaskTerminalDeliveryUnknown(ctx, first); err == nil || changed {
		t.Fatalf("stale claim transitioned after reclaim: changed=%t err=%v", changed, err)
	}
	if changed, err := f.store.MarkTaskTerminalDeliveryUnknown(ctx, reclaimed); err != nil || !changed {
		t.Fatalf("current claim did not enter unknown: changed=%t err=%v", changed, err)
	}
	nextClaims, err := f.store.ClaimTaskTerminalDeliveryOutbox(ctx, "terminal-expired-third", 30*time.Second, 1)
	if err != nil || len(nextClaims) != 1 {
		t.Fatalf("reconcile unknown source: claims=%+v err=%v", nextClaims, err)
	}
	next := nextClaims[0]
	if next.ClaimGeneration != reclaimed.ClaimGeneration+1 || next.ModelCallID != first.ModelCallID {
		t.Fatalf("unknown replay changed stable call identity: first=%+v next=%+v", first, next)
	}
}

func TestPostgresTaskTerminalDeliveryClaimInvalidatesStaleRevisionAuthorizationAndSource(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(context.Context, *Store, int64, string) error
	}{
		{name: "revision", mutate: func(ctx context.Context, store *Store, _ int64, taskID string) error {
			_, err := store.pool.Exec(ctx, `UPDATE tasks
SET current_revision=current_revision+1,desired_revision=desired_revision+1 WHERE id=$1`, taskID)
			return err
		}},
		{name: "authorization", mutate: func(ctx context.Context, store *Store, _ int64, taskID string) error {
			_, err := store.pool.Exec(ctx, `UPDATE tasks SET auth_version=auth_version+1 WHERE id=$1`, taskID)
			return err
		}},
		{name: "source event digest", mutate: func(ctx context.Context, store *Store, sequence int64, taskID string) error {
			_, err := store.pool.Exec(ctx, `UPDATE task_events SET payload=payload||'{"changed":true}'::jsonb
WHERE task_id=$1 AND sequence=$2`, taskID, sequence)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newAgentLifecycleFixture(t)
			ctx := context.Background()
			sequence := commitTaskTerminalDeliveryFailure(t, f)
			if err := test.mutate(ctx, f.store, sequence, f.request.Fence.TaskID); err != nil {
				t.Fatalf("make source stale: %v", err)
			}
			claims, err := f.store.ClaimTaskTerminalDeliveryOutbox(ctx, "terminal-stale-worker", 30*time.Second, 1)
			if err != nil || len(claims) != 0 {
				t.Fatalf("stale source was claimable: claims=%+v err=%v", claims, err)
			}
			var state string
			if err := f.store.pool.QueryRow(ctx, `SELECT state FROM task_terminal_delivery_outbox WHERE task_id=$1`,
				f.request.Fence.TaskID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state != TaskTerminalDeliveryStateInvalidated {
				t.Fatalf("stale source state=%q, want invalidated", state)
			}
		})
	}
}

func TestPostgresTaskTerminalDeliveryReplyReadyReplayIsIdempotent(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	commitTaskTerminalDeliveryFailure(t, f)
	claims, err := f.store.ClaimTaskTerminalDeliveryOutbox(ctx, "terminal-reply-worker", 30*time.Second, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim terminal delivery: claims=%+v err=%v", claims, err)
	}
	ciphertext := []byte("encrypted reply candidate")
	ready, changed, err := f.store.MarkTaskTerminalDeliveryReplyReady(ctx, claims[0], ciphertext)
	if err != nil || !changed || ready.State != TaskTerminalDeliveryStateReplyReady {
		t.Fatalf("persist reply-ready candidate: ready=%+v changed=%t err=%v", ready, changed, err)
	}
	wantDigest := sha256.Sum256(ciphertext)
	if !bytes.Equal(ready.ResponseSHA256, wantDigest[:]) || ready.LeaseOwner != "" || !ready.LeaseExpiresAt.IsZero() {
		t.Fatalf("reply-ready candidate is not durable/fenced: %+v", ready)
	}

	recovered, err := f.store.ListTaskTerminalDeliveryRepliesReady(ctx, 1)
	if err != nil || len(recovered) != 1 || recovered[0].ID != ready.ID ||
		!bytes.Equal(recovered[0].ResponseCiphertext, ciphertext) ||
		!bytes.Equal(recovered[0].ResponseSHA256, wantDigest[:]) {
		t.Fatalf("recover reply-ready candidate: rows=%+v err=%v", recovered, err)
	}
	messageID := mustID(t, domain.NewMessageID)
	var published bool
	if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		published, err = f.store.MarkTaskTerminalDeliveryPublishedInTx(ctx, tx, recovered[0], messageID)
		return err
	}); err != nil || !published {
		t.Fatalf("publish reply-ready source: published=%t err=%v", published, err)
	}
	var replayPublished bool
	if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		var err error
		replayPublished, err = f.store.MarkTaskTerminalDeliveryPublishedInTx(ctx, tx, recovered[0], messageID)
		return err
	}); err != nil || replayPublished {
		t.Fatalf("published replay duplicated transition: published=%t err=%v", replayPublished, err)
	}
	var state, storedMessageID string
	if err := f.store.pool.QueryRow(ctx, `SELECT state,published_message_id FROM task_terminal_delivery_outbox WHERE id=$1`,
		ready.ID).Scan(&state, &storedMessageID); err != nil {
		t.Fatal(err)
	}
	if state != TaskTerminalDeliveryStatePublished || storedMessageID != messageID {
		t.Fatalf("published source state=%q message=%q", state, storedMessageID)
	}
}

func commitTaskTerminalDeliveryFailure(t *testing.T, f agentLifecycleFixture) int64 {
	t.Helper()
	stepID := mustID(t, domain.NewStepID)
	committed, err := f.store.CommitTaskProgress(context.Background(), app.TaskProgress{
		TaskID: f.request.Fence.TaskID, LeaseOwner: f.request.Fence.LeaseOwner, LeaseVersion: f.request.Fence.LeaseVersion,
		Status: task.StatusFailed, CurrentStep: stepID, Result: "Task stopped after a model error.",
		Steps: []task.Step{{ID: stepID, TaskID: f.request.Fence.TaskID, Sequence: 1, Kind: "model", Status: task.StepFailed,
			Title: "Run model", Summary: "The model call failed."}},
		Event: task.Event{Type: "task.result", Payload: []byte(`{"status":"failed","reason":"provider_unavailable"}`)},
	})
	if err != nil {
		t.Fatalf("commit failed Task: %v", err)
	}
	return committed.Sequence
}
