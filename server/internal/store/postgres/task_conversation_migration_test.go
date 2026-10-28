package postgres

import (
	"context"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func TestFusedConversationMigrationPreservesHistoryAndRetries(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 53); err != nil {
		t.Fatalf("prepare old schema: %v", err)
	}
	owner := createTestUser(t, store, 1, 1000, 2048)
	conversationID := mustID(t, domain.NewConversationID)
	exchangeID := mustID(t, domain.NewMessageID)
	userMessageID := mustID(t, domain.NewMessageID)
	assistantMessageID := mustID(t, domain.NewMessageID)
	if _, err := pool.Exec(ctx, `INSERT INTO instant_chat_conversations (id, owner_id, model_id, title)
VALUES ($1, $2, 'p1234567', 'old title')`, conversationID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO instant_chat_exchanges
(id, conversation_id, owner_id, request_key, user_message_id, assistant_message_id, user_content, assistant_content)
VALUES ($1,$2,$3,'legacy-key',$4,$5,'old question','old answer')`, exchangeID, conversationID, owner.ID, userMessageID, assistantMessageID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 54); err != nil {
		t.Fatalf("migrate old conversation: %v", err)
	}
	if _, err := provider.UpTo(ctx, 54); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	var archivedID, archivedConversationID, archivedOwnerID, archivedKey string
	var archivedUserID, archivedAssistantID, archivedUserContent, archivedAssistantContent string
	if err := pool.QueryRow(ctx, `SELECT id, conversation_id, owner_id, request_key, user_message_id,
user_content, assistant_message_id, assistant_content FROM retired_instant_chat_exchanges WHERE owner_id=$1 AND request_key=$2`,
		owner.ID, "legacy-key").Scan(&archivedID, &archivedConversationID, &archivedOwnerID, &archivedKey,
		&archivedUserID, &archivedUserContent, &archivedAssistantID, &archivedAssistantContent); err != nil ||
		archivedID != exchangeID || archivedConversationID != conversationID || archivedOwnerID != owner.ID ||
		archivedKey != "legacy-key" || archivedUserID != userMessageID || archivedAssistantID != assistantMessageID ||
		archivedUserContent != "old question" || archivedAssistantContent != "old answer" {
		t.Fatalf("migrated legacy exchange = %q/%q/%q/%q %q/%q %q/%q, err=%v", archivedID, archivedConversationID,
			archivedOwnerID, archivedKey, archivedUserID, archivedUserContent, archivedAssistantID, archivedAssistantContent, err)
	}
	chat, err := store.GetConversation(ctx, conversationID, owner.ID)
	if err != nil || chat.ID != conversationID || chat.ModelID != "p1234567" || chat.Title != "old title" {
		t.Fatalf("migrated canonical conversation = %#v, err=%v", chat, err)
	}
	firstPage, err := store.ListMessages(ctx, owner.ID, conversationID, app.PageRequest{Limit: 1})
	if err != nil || len(firstPage.Items) != 1 || firstPage.Items[0].ID != assistantMessageID || firstPage.NextCursor == "" {
		t.Fatalf("first message page = %#v, err=%v", firstPage, err)
	}
	secondPage, err := store.ListMessages(ctx, owner.ID, conversationID, app.PageRequest{Limit: 1, Cursor: firstPage.NextCursor})
	if err != nil || len(secondPage.Items) != 1 || secondPage.Items[0].ID != userMessageID || secondPage.NextCursor != "" {
		t.Fatalf("second message page = %#v, err=%v", secondPage, err)
	}
	var messages, archived, tasks int
	var oldTableMissing bool
	if err := pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM conversation_messages WHERE conversation_id=$1),
(SELECT count(*) FROM retired_instant_chat_exchanges WHERE conversation_id=$1),
(SELECT count(*) FROM tasks),
to_regclass('instant_chat_exchanges') IS NULL`, conversationID).Scan(&messages, &archived, &tasks, &oldTableMissing); err != nil {
		t.Fatal(err)
	}
	if messages != 2 || archived != 1 || tasks != 0 || !oldTableMissing {
		t.Fatalf("unexpected migration state: messages=%d archived=%d tasks=%d old table missing=%t", messages, archived, tasks, oldTableMissing)
	}
	if _, err := pool.Exec(ctx, `UPDATE conversations SET title='edited after upgrade' WHERE id=$1`, conversationID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 53); err == nil {
		t.Fatal("rollback silently discarded an edited conversation title")
	}
	if _, err := pool.Exec(ctx, `UPDATE conversations SET title='old title' WHERE id=$1`, conversationID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversations (id, owner_id, title, source_kind) VALUES ($1,$2,'new','instant')`,
		mustID(t, domain.NewConversationID), owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 53); err == nil {
		t.Fatal("rollback silently accepted a new post-migration conversation")
	}
}

func TestFusedConversationMigrationRejectsConflictingIDs(t *testing.T) {
	for _, sameOwner := range []bool{true, false} {
		t.Run(map[bool]string{true: "same owner", false: "different owner"}[sameOwner], func(t *testing.T) {
			store, pool := newTemporaryStore(t)
			ctx := context.Background()
			provider, closeProvider, err := newMigrationProvider(pool)
			if err != nil {
				t.Fatal(err)
			}
			defer closeProvider()
			if _, err := provider.DownTo(ctx, 53); err != nil {
				t.Fatal(err)
			}
			owner := createTestUser(t, store, 2, 10000, 20000)
			other := owner
			if !sameOwner {
				other = createTestUser(t, store, 2, 10000, 20000)
			}
			hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
			conversationID := mustID(t, domain.NewConversationID)
			if _, err := pool.Exec(ctx, `INSERT INTO conversations (id, owner_id, host_id, title) VALUES ($1,$2,$3,'task history')`,
				conversationID, owner.ID, hostValue.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO instant_chat_conversations (id, owner_id, model_id, title) VALUES ($1,$2,'p1234567','different history')`,
				conversationID, other.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := provider.UpTo(ctx, 54); err == nil {
				t.Fatal("migration silently merged conflicting conversations")
			}
			var taskTitle, chatTitle string
			if err := pool.QueryRow(ctx, `SELECT c.title, i.title FROM conversations c JOIN instant_chat_conversations i ON i.id=c.id WHERE c.id=$1`, conversationID).
				Scan(&taskTitle, &chatTitle); err != nil || taskTitle != "task history" || chatTitle != "different history" {
				t.Fatalf("conflicting histories changed: task=%q chat=%q err=%v", taskTitle, chatTitle, err)
			}
		})
	}
}

func TestFusedConversationMigrationRejectsConflictingMessageIDs(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 53); err != nil {
		t.Fatal(err)
	}
	owner := createTestUser(t, store, 2, 10000, 20000)
	other := createTestUser(t, store, 2, 10000, 20000)
	hostValue := createRunningControlHost(t, ctx, store, owner.ID, "ai", 0)
	taskConversation := mustID(t, domain.NewConversationID)
	taskID := mustID(t, domain.NewTaskID)
	messageID := mustID(t, domain.NewMessageID)
	instantConversation := mustID(t, domain.NewConversationID)
	if _, err := pool.Exec(ctx, `INSERT INTO conversations (id, owner_id, host_id, title) VALUES ($1,$2,$3,'task')`,
		taskConversation, owner.ID, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tasks
(id,owner_id,conversation_id,host_id,model_id,objective,status,auth_kind,budget_active_seconds,budget_tool_calls,budget_model_tokens)
VALUES ($1,$2,$3,$4,'model','objective','queued','session',1,1,1)`, taskID, owner.ID, taskConversation, hostValue.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO conversation_messages (id,conversation_id,owner_id,role,content,task_id)
VALUES ($1,$2,$3,'user','existing task message',$4)`, messageID, taskConversation, owner.ID, taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO instant_chat_conversations (id,owner_id,model_id,title) VALUES ($1,$2,'p1234567','chat')`,
		instantConversation, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO instant_chat_exchanges
(id,conversation_id,owner_id,request_key,user_message_id,assistant_message_id,user_content,assistant_content)
VALUES ($1,$2,$3,'old-chat-key',$4,$5,'different message','answer')`,
		mustID(t, domain.NewMessageID), instantConversation, other.ID, messageID, mustID(t, domain.NewMessageID)); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 54); err == nil {
		t.Fatal("migration silently merged conflicting message IDs")
	}
	var original, legacy string
	if err := pool.QueryRow(ctx, `SELECT m.content, i.user_content FROM conversation_messages m
JOIN instant_chat_exchanges i ON i.user_message_id=m.id WHERE m.id=$1`, messageID).Scan(&original, &legacy); err != nil ||
		original != "existing task message" || legacy != "different message" {
		t.Fatalf("conflicting messages changed: original=%q legacy=%q err=%v", original, legacy, err)
	}
}
