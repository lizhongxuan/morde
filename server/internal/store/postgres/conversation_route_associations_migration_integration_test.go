package postgres

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestConversationRouteAssociationMigrationPreservesLegacyRowsAndEvidence(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner := createTestUser(t, store, 1, 1000, 2048)
	other := createTestUser(t, store, 1, 1000, 2048)
	conversationID := createMainConversationForTest(t, store, owner.ID).ID
	otherConversationID := createMainConversationForTest(t, store, owner.ID).ID
	otherActorConversationID := createMainConversationForTest(t, store, other.ID).ID
	accept := func(ownerID, id, clientID, targetConversationID string) conversation.Message {
		t.Helper()
		hash := sha256.Sum256([]byte(clientID))
		message, _, err := store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
			Message: conversation.Message{ID: id, OwnerID: ownerID, ActorID: ownerID,
				ConversationID: targetConversationID, Role: conversation.RoleUser, Content: "same request"},
			ServiceInstanceID: instanceID, ClientID: clientID, RequestHash: hash[:],
			Authorization: task.Authorization{Kind: "session", Version: 1},
		})
		if err != nil {
			t.Fatalf("accept legacy message %s: %v", clientID, err)
		}
		return message
	}
	root := accept(owner.ID, mustID(t, domain.NewMessageID), "route-association-root-001", conversationID)
	otherConversation := accept(owner.ID, mustID(t, domain.NewMessageID), "route-association-other-001", otherConversationID)
	otherActor := accept(other.ID, mustID(t, domain.NewMessageID), "route-association-actor-001", otherActorConversationID)
	modelID := mustID(t, domain.NewModelConnectionID)
	callID := "route-migration-call-001"
	callDigest := sha256.Sum256([]byte("legacy route call"))
	if _, err := pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ($1,$2,'route migration','https://route-migration.test','test-model',$3,true,clock_timestamp())`,
		modelID, owner.ID, make([]byte, 29)); err != nil {
		t.Fatalf("insert legacy route model: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_call_ledger
(owner_id,source,call_id,request_digest,model_id,provider_model,reserved_tokens,state)
VALUES ($1,'route',$2,$3,$4,'test-model',1,'dispatched')`, owner.ID, callID, callDigest[:], modelID); err != nil {
		t.Fatalf("insert legacy route model ledger: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversation_route_model_calls
(message_id,service_instance_id,owner_id,actor_id,conversation_id,source,call_id,request_digest,model_id,model_version,state,phase)
VALUES ($1,$2::uuid,$3,$3,$4,'route',$5,$6,$7,$8,'reserved','normal')`,
		root.ID, instanceID, owner.ID, conversationID, callID, callDigest[:], modelID, time.Now().UTC()); err != nil {
		t.Fatalf("insert legacy route model call: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_message_routes SET route_attempt=0,auto_retry_count=0`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_route_model_calls SET attempt=0 WHERE message_id=$1`, root.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 192); err != nil {
		t.Fatalf("roll back empty association migration: %v", err)
	}

	if _, err := provider.UpTo(ctx, 193); err != nil {
		t.Fatalf("apply association migration over legacy rows: %v", err)
	}
	var attempt int64
	var retryCount int16
	var duplicateID *string
	if err := pool.QueryRow(ctx, `SELECT route_attempt,auto_retry_count,duplicate_of_message_id
FROM conversation_message_routes WHERE message_id=$1`, root.ID).Scan(&attempt, &retryCount, &duplicateID); err != nil {
		t.Fatal(err)
	}
	if attempt != 0 || retryCount != 0 || duplicateID != nil {
		t.Fatalf("legacy route migrated with unexpected evidence: attempt=%d retries=%d duplicate=%v", attempt, retryCount, duplicateID)
	}
	var migratedCallAttempt int64
	if err := pool.QueryRow(ctx, `SELECT attempt FROM conversation_route_model_calls WHERE message_id=$1 AND phase='normal'`, root.ID).Scan(&migratedCallAttempt); err != nil || migratedCallAttempt != 0 {
		t.Fatalf("legacy model call attempt = %d, err=%v", migratedCallAttempt, err)
	}
	for _, target := range []string{root.ID, otherConversation.ID, otherActor.ID} {
		if _, err := pool.Exec(ctx, `UPDATE conversation_message_routes SET duplicate_of_message_id=$2 WHERE message_id=$1`, root.ID, target); err == nil {
			t.Fatalf("association crossed target scope or accepted self-reference: %s", target)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_message_routes SET route_attempt=1 WHERE message_id=$1`, root.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 192); err == nil || !strings.Contains(err.Error(), "cannot discard conversation route association or retry evidence") {
		t.Fatalf("rollback discarded retry evidence: %v", err)
	}
	var associationColumn bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
WHERE table_schema=current_schema() AND table_name='conversation_message_routes' AND column_name='duplicate_of_message_id')`).Scan(&associationColumn); err != nil || !associationColumn {
		t.Fatalf("evidence rollback removed association schema: present=%t err=%v", associationColumn, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversation_message_routes SET route_attempt=0 WHERE message_id=$1`, root.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 192); err != nil {
		t.Fatalf("roll back unused association fields: %v", err)
	}
	if _, err := provider.UpTo(ctx, 193); err != nil {
		t.Fatalf("reapply association migration: %v", err)
	}
	var scopedUnique, unresolvedUnique bool
	if err := pool.QueryRow(ctx, `SELECT
  EXISTS (SELECT 1 FROM pg_constraint WHERE conname='conversation_message_routes_scoped_message_unique'),
  to_regclass('conversation_route_model_calls_one_unresolved') IS NOT NULL`).Scan(&scopedUnique, &unresolvedUnique); err != nil || !scopedUnique || !unresolvedUnique {
		t.Fatalf("migration constraints missing: scoped=%t unresolved=%t err=%v", scopedUnique, unresolvedUnique, err)
	}
	newMessage := accept(owner.ID, mustID(t, domain.NewMessageID), "route-association-new-001", conversationID)
	var newAttempt int64
	if err := pool.QueryRow(ctx, `SELECT route_attempt FROM conversation_message_routes WHERE message_id=$1`, newMessage.ID).Scan(&newAttempt); err != nil || newAttempt != 1 {
		t.Fatalf("new route did not start at attempt 1: attempt=%d err=%v", newAttempt, err)
	}
}
