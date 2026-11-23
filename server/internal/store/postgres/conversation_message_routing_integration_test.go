package postgres

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestConversationMessageReceiptIsAtomicAndScoped(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner := createTestUser(t, store, 1, 1000, 2048)
	other := createTestUser(t, store, 1, 1000, 2048)
	first := createMainConversationForTest(t, store, owner.ID)
	second := createMainConversationForTest(t, store, other.ID)
	third := createMainConversationForTest(t, store, owner.ID)
	receipt := func(ownerID, conversationID, content string) app.ConversationMessageReceipt {
		hash := sha256.Sum256([]byte(content))
		return app.ConversationMessageReceipt{
			Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: ownerID, ActorID: ownerID,
				ConversationID: conversationID, Role: conversation.RoleUser, Content: content},
			ServiceInstanceID: instanceID, ClientID: "client-message-001", RequestHash: hash[:],
			Authorization: task.Authorization{Kind: "session", Version: 1},
		}
	}

	const callers = 8
	requests := make([]app.ConversationMessageReceipt, callers)
	for i := range requests {
		requests[i] = receipt(owner.ID, first.ID, "first message")
	}
	type outcome struct {
		message  conversation.Message
		replayed bool
		err      error
	}
	results := make(chan outcome, callers)
	var wg sync.WaitGroup
	for _, request := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			message, replayed, err := store.AcceptConversationMessage(ctx, request)
			results <- outcome{message, replayed, err}
		}()
	}
	wg.Wait()
	close(results)
	var acceptedID string
	newCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent receipt: %v", result.err)
		}
		if acceptedID == "" {
			acceptedID = result.message.ID
		}
		if result.message.ID != acceptedID || result.message.TaskID != "" || result.message.Sequence != 1 ||
			result.message.RoutingStatus != conversation.RoutingPending {
			t.Fatalf("replayed receipt changed identity or state: %+v", result.message)
		}
		if !result.replayed {
			newCount++
		}
	}
	if newCount != 1 {
		t.Fatalf("created %d messages for one client id", newCount)
	}
	matched, found, err := store.FindConversationMessageByClientID(ctx, owner.ID, "", first.ID, "client-message-001", instanceID)
	if err != nil || !found || matched.ID != acceptedID || matched.RoutingStatus != conversation.RoutingPending {
		t.Fatalf("reconcile accepted message = %+v, found=%t, err=%v", matched, found, err)
	}
	if _, found, err := store.FindConversationMessageByClientID(ctx, other.ID, "", first.ID, "client-message-001", instanceID); err != nil || found {
		t.Fatalf("another account reconciled the message: found=%t, err=%v", found, err)
	}
	wrongInstance := receipt(owner.ID, first.ID, "first message")
	wrongInstance.ServiceInstanceID = "11111111-1111-4111-8111-111111111111"
	if _, _, err := store.AcceptConversationMessage(ctx, wrongInstance); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("receipt crossed service instances: %v", err)
	}
	if _, _, err := store.FindConversationMessageByClientID(ctx, owner.ID, "", first.ID, "client-message-001", wrongInstance.ServiceInstanceID); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("reconciliation crossed service instances: %v", err)
	}
	page, err := store.ListMessages(ctx, owner.ID, first.ID, app.PageRequest{Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].ClientMessageID != "client-message-001" ||
		page.Items[0].RoutingStatus != conversation.RoutingPending {
		t.Fatalf("conversation page lost routing state: %+v, err=%v", page, err)
	}
	if _, _, err := store.AcceptConversationMessage(ctx, receipt(owner.ID, first.ID, "changed message")); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("conflicting retry = %v", err)
	}
	otherMessage, replayed, err := store.AcceptConversationMessage(ctx, receipt(other.ID, second.ID, "other account"))
	if err != nil || replayed || otherMessage.ID == acceptedID || otherMessage.OwnerID != other.ID {
		t.Fatalf("client id crossed account scope: %+v, replayed=%t, err=%v", otherMessage, replayed, err)
	}
	otherConversation, replayed, err := store.AcceptConversationMessage(ctx, receipt(owner.ID, third.ID, "other conversation"))
	if err != nil || replayed || otherConversation.ID == acceptedID || otherConversation.ConversationID != third.ID {
		t.Fatalf("client id crossed conversation scope: %+v, replayed=%t, err=%v", otherConversation, replayed, err)
	}
	if _, err := store.GetConversation(ctx, first.ID, other.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("another account read the conversation: %v", err)
	}
	if _, err := store.ListMessages(ctx, other.ID, first.ID, app.PageRequest{Limit: 10}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("another account read conversation messages: %v", err)
	}
	ownerConversations, err := store.ListConversations(ctx, owner.ID, nil, app.PageRequest{Limit: 10})
	if err != nil {
		t.Fatalf("list owner conversations: %v", err)
	}
	otherConversations, err := store.ListConversations(ctx, other.ID, nil, app.PageRequest{Limit: 10})
	if err != nil {
		t.Fatalf("list other account conversations: %v", err)
	}
	if len(ownerConversations.Items) != 2 || len(otherConversations.Items) != 1 ||
		ownerConversations.Items[0].OwnerID != owner.ID || ownerConversations.Items[1].OwnerID != owner.ID ||
		otherConversations.Items[0].OwnerID != other.ID || otherConversations.Items[0].ID == first.ID {
		t.Fatalf("conversation lists crossed account boundaries: owner=%+v other=%+v", ownerConversations.Items, otherConversations.Items)
	}
	var messages, routes, tasks int
	if err := pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM conversation_messages WHERE conversation_id=$1),
(SELECT count(*) FROM conversation_message_routes WHERE conversation_id=$1),
(SELECT count(*) FROM tasks WHERE conversation_id=$1)`, first.ID).Scan(&messages, &routes, &tasks); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || routes != 1 || tasks != 0 {
		t.Fatalf("receipt committed messages=%d routes=%d tasks=%d", messages, routes, tasks)
	}
}

func TestConversationMessageReceiptRollsBackWhenRouteReceiptFails(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner := createTestUser(t, store, 1, 1000, 2048)
	conv := createMainConversationForTest(t, store, owner.ID)
	hash := sha256.Sum256([]byte("atomic receipt failure"))
	receipt := app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: owner.ID,
			ConversationID: conv.ID, Role: conversation.RoleUser, Content: "atomic receipt failure"},
		ServiceInstanceID: instanceID, ClientID: "client-message-failure", RequestHash: hash[:],
		Authorization: task.Authorization{Kind: "session", Version: 1},
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION reject_test_conversation_route() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF NEW.client_message_id = 'client-message-failure' THEN
		RAISE EXCEPTION 'injected route receipt failure';
	END IF;
	RETURN NEW;
END
$$`); err != nil {
		t.Fatalf("create route failure trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER reject_test_conversation_route BEFORE INSERT ON conversation_message_routes
FOR EACH ROW EXECUTE FUNCTION reject_test_conversation_route()`); err != nil {
		t.Fatalf("create route failure trigger: %v", err)
	}
	if _, _, err := store.AcceptConversationMessage(ctx, receipt); err == nil {
		t.Fatal("receipt unexpectedly succeeded while its route insert was rejected")
	}
	var messages, routes, sequence int
	if err := pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM conversation_messages WHERE conversation_id=$1),
(SELECT count(*) FROM conversation_message_routes WHERE conversation_id=$1),
(SELECT message_seq FROM conversations WHERE id=$1)`, conv.ID).Scan(&messages, &routes, &sequence); err != nil {
		t.Fatal(err)
	}
	if messages != 0 || routes != 0 || sequence != 0 {
		t.Fatalf("failed receipt left partial state: messages=%d routes=%d sequence=%d", messages, routes, sequence)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER reject_test_conversation_route ON conversation_message_routes`); err != nil {
		t.Fatalf("drop route failure trigger: %v", err)
	}
	if _, err := pool.Exec(ctx, `DROP FUNCTION reject_test_conversation_route()`); err != nil {
		t.Fatalf("drop route failure trigger function: %v", err)
	}
	accepted, replayed, err := store.AcceptConversationMessage(ctx, receipt)
	if err != nil || replayed || accepted.Sequence != 1 || accepted.ClientMessageID != receipt.ClientID {
		t.Fatalf("retry after rolled back receipt = %+v, replayed=%t, err=%v", accepted, replayed, err)
	}
}
