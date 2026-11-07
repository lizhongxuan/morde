package postgres

import (
	"context"
	"crypto/sha256"
	"strings"
	"sync"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestConversationMessageUploadPreservesLongTextAndIdempotency(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 1000, 2048)
	other := createTestUser(t, store, 1, 1000, 2048)
	conversationValue := createMainConversationForTest(t, store, owner.ID)
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte(strings.Repeat("中文a", 18000) + "结束")
	contentHash := sha256.Sum256(content)
	requestHash := sha256.Sum256([]byte("stable upload metadata"))
	receipt := app.ConversationMessageReceipt{
		Message:           conversation.Message{ConversationID: conversationValue.ID, OwnerID: owner.ID, ActorID: owner.ID, Role: conversation.RoleUser},
		ServiceInstanceID: instanceID, ClientID: "long-message-001", Authorization: task.Authorization{Kind: "session", Version: 1},
	}
	init := app.ConversationMessageUploadInit{Receipt: receipt, TotalBytes: len(content),
		TotalChunks: (len(content) + app.ConversationMessageChunkBytes - 1) / app.ConversationMessageChunkBytes,
		ContentHash: contentHash[:], RequestHash: requestHash[:]}
	upload, err := store.InitializeConversationMessageUpload(ctx, init)
	if err != nil || upload.State != "uploading" || upload.TotalChunks != init.TotalChunks || len(upload.ReceivedSequences) != 0 {
		t.Fatalf("initialize upload = %+v err=%v", upload, err)
	}
	if _, err := store.CommitConversationMessageUpload(ctx, receipt); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("incomplete upload was committed: %v", err)
	}
	for sequence := init.TotalChunks - 1; sequence >= 0; sequence-- {
		start := sequence * app.ConversationMessageChunkBytes
		chunk := content[start:min(start+app.ConversationMessageChunkBytes, len(content))]
		digest := sha256.Sum256(chunk)
		if err := store.PutConversationMessageChunk(ctx, receipt, sequence, chunk, digest[:]); err != nil {
			t.Fatalf("write chunk %d: %v", sequence, err)
		}
		if err := store.PutConversationMessageChunk(ctx, receipt, sequence, chunk, digest[:]); err != nil {
			t.Fatalf("replay chunk %d: %v", sequence, err)
		}
	}
	changed := append([]byte(nil), content[:app.ConversationMessageChunkBytes]...)
	changed[0] ^= 1
	changedHash := sha256.Sum256(changed)
	if err := store.PutConversationMessageChunk(ctx, receipt, 0, changed, changedHash[:]); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("conflicting chunk replay = %v", err)
	}
	foreign := receipt
	foreign.Message.OwnerID, foreign.Message.ActorID = other.ID, other.ID
	if _, err := store.GetConversationMessageUpload(ctx, foreign); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("another account read upload: %v", err)
	}

	const callers = 6
	type result struct {
		message conversation.Message
		err     error
	}
	results := make(chan result, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			message, err := store.CommitConversationMessageUpload(ctx, receipt)
			results <- result{message, err}
		}()
	}
	wg.Wait()
	close(results)
	var messageID string
	for value := range results {
		if value.err != nil || value.message.Content != string(content) || value.message.TaskID != "" ||
			value.message.RoutingStatus != conversation.RoutingPending {
			t.Fatalf("commit result content bytes=%d task=%q state=%q err=%v", len(value.message.Content), value.message.TaskID, value.message.RoutingStatus, value.err)
		}
		if messageID == "" {
			messageID = value.message.ID
		}
		if value.message.ID != messageID {
			t.Fatalf("concurrent commit created different messages: %s and %s", messageID, value.message.ID)
		}
	}
	upload, err = store.GetConversationMessageUpload(ctx, receipt)
	if err != nil || upload.State != "committed" || upload.Message == nil || upload.Message.ID != messageID || len(upload.ReceivedSequences) != init.TotalChunks {
		t.Fatalf("recover committed upload = %+v err=%v", upload, err)
	}
	var messages, routes int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM conversation_messages WHERE conversation_id=$1),
(SELECT count(*) FROM conversation_message_routes WHERE conversation_id=$1)`, conversationValue.ID).Scan(&messages, &routes); err != nil {
		t.Fatal(err)
	}
	if messages != 1 || routes != 1 {
		t.Fatalf("long upload created messages=%d routes=%d", messages, routes)
	}
}
