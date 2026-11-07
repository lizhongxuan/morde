package postgres

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestConversationRouteClaimExcludesConcurrentAndStaleWorkers(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 1000, 2048)
	value := createMainConversationForTest(t, store, owner.ID)
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("one routing request"))
	message, _, err := store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: owner.ID,
			ConversationID: value.ID, Role: conversation.RoleUser, Content: "one routing request"},
		ServiceInstanceID: instanceID, ClientID: "route-request-001", RequestHash: hash[:],
		Authorization: task.Authorization{Kind: "session", Version: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		claim app.ConversationRouteClaim
		found bool
		err   error
	}
	const workers = 8
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, found, err := store.ClaimConversationRoute(ctx, "route-worker", time.Minute)
			results <- result{claim, found, err}
		}()
	}
	wg.Wait()
	close(results)
	var first app.ConversationRouteClaim
	count := 0
	for item := range results {
		if item.err != nil {
			t.Fatalf("claim route: %v", item.err)
		}
		if item.found {
			first = item.claim
			count++
		}
	}
	if count != 1 || first.Message.ID != message.ID || first.LeaseVersion != 1 || first.Message.RoutingStatus != conversation.RoutingProcessing {
		t.Fatalf("concurrent claims = %d, first=%+v", count, first)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_message_routes SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE message_id=$1`, message.ID); err != nil {
		t.Fatal(err)
	}
	second, found, err := store.ClaimConversationRoute(ctx, "route-worker-new", time.Minute)
	if err != nil || !found || second.Message.ID != message.ID || second.LeaseVersion != 2 {
		t.Fatalf("recover expired claim = %+v found=%t err=%v", second, found, err)
	}
	if err := store.ReleaseConversationRoute(ctx, first); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale worker released current route: %v", err)
	}
	if err := store.ReleaseConversationRoute(ctx, second); err != nil {
		t.Fatalf("current worker could not release route: %v", err)
	}
	third, found, err := store.ClaimConversationRoute(ctx, "route-worker-third", time.Minute)
	if err != nil || !found || third.Message.ID != message.ID || third.LeaseVersion != 3 {
		t.Fatalf("released route could not be resumed: %+v found=%t err=%v", third, found, err)
	}
	if err := store.DeferConversationRoute(ctx, first, time.Second); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("stale worker deferred current route: %v", err)
	}
	if err := store.DeferConversationRoute(ctx, third, time.Minute); err != nil {
		t.Fatalf("defer missing predecessor: %v", err)
	}
	other, _, err := store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: owner.ID,
			ConversationID: value.ID, Role: conversation.RoleUser, Content: "independent message"},
		ServiceInstanceID: instanceID, ClientID: "route-request-002", RequestHash: hash[:],
		Authorization: task.Authorization{Kind: "session", Version: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	independent, found, err := store.ClaimConversationRoute(ctx, "route-worker-independent", time.Minute)
	if err != nil || !found || independent.Message.ID != other.ID {
		t.Fatalf("deferred predecessor blocked another message: %+v found=%t err=%v", independent, found, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_message_routes SET available_after=clock_timestamp()-interval '1 second' WHERE message_id=$1`, message.ID); err != nil {
		t.Fatal(err)
	}
	resumed, found, err := store.ClaimConversationRoute(ctx, "route-worker-resumed", time.Minute)
	if err != nil || !found || resumed.Message.ID != message.ID || resumed.LeaseVersion != 4 {
		t.Fatalf("deferred route was not recoverable: %+v found=%t err=%v", resumed, found, err)
	}
}
