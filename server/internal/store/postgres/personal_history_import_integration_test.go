package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

func TestPostgresPersonalHistoryImportIsAtomicIdempotentAndTaskFree(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	var schemaReady bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.personal_history_import_mappings') IS NOT NULL
  AND to_regclass('public.conversation_message_attachments') IS NOT NULL`).Scan(&schemaReady); err != nil {
		t.Fatal(err)
	}
	if !schemaReady {
		t.Skip("personal history import mapping/relation migration is not installed")
	}
	owner := createTestUser(t, store, 2, 2000, 4096)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	createPersonalLibraryItemForAttachmentTest(t, store, principal, "library-item_1")
	content := []byte("history import attachment")
	digest := sha256.Sum256(content)
	service, err := app.NewPersonalAttachmentService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	createdUpload, err := service.CreatePersonalAttachmentUpload(ctx, principal, app.PersonalAttachmentUploadCommand{
		IdempotencyKey: "history-attachment-create", ObjectID: "library-item_1", BaseRevision: 1,
		SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:]), MediaType: "application/octet-stream",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutPersonalAttachmentChunk(ctx, principal, app.PersonalAttachmentChunkCommand{
		UploadID: createdUpload.UploadID, Sequence: 0, SizeBytes: int64(len(content)),
		SHA256: hex.EncodeToString(digest[:]), Body: bytes.NewReader(content),
	}); err != nil {
		t.Fatal(err)
	}
	receipt, err := service.CommitPersonalAttachment(ctx, principal, createdUpload.UploadID, "history-attachment-commit")
	if err != nil {
		t.Fatal(err)
	}
	attachmentID := receipt.AttachmentID
	input := conversation.HistoryImport{
		SourceID: "test-export", DeviceAccountID: "device-account-1", SourceConversationID: "source-chat-1", Title: "Imported",
		Messages: []conversation.HistoryMessage{
			{SourceMessageID: "source-message-1", Role: conversation.RoleUser, Content: "question", AttachmentIDs: []string{attachmentID}},
			{SourceMessageID: "source-message-2", Role: conversation.RoleAssistant, Content: "answer"},
		},
	}
	first, err := store.ImportPersonalHistory(ctx, principal, input)
	if err != nil || first.Replay || len(first.Messages) != 2 || first.Conversation.ID == "" || first.Conversation.OwnerID != owner.ID || first.Conversation.HostID != "" {
		t.Fatalf("first import = %+v, %v", first, err)
	}
	if first.Messages[0].Message.Sequence != 1 || first.Messages[1].Message.Sequence != 2 ||
		first.Messages[0].Message.TaskID != "" || first.Messages[1].Message.TaskID != "" {
		t.Fatalf("imported message task/sequence state = %+v", first.Messages)
	}
	if len(first.Messages[0].AttachmentIDs) != 1 || first.Messages[0].AttachmentIDs[0] != attachmentID {
		t.Fatalf("imported attachment mapping = %+v", first.Messages[0].AttachmentIDs)
	}
	for _, imported := range first.Messages {
		if imported.Message.SourceKind != conversation.HistoryImportSourceKind {
			t.Errorf("source kind = %q, want %q", imported.Message.SourceKind, conversation.HistoryImportSourceKind)
		}
	}

	var messageCount, taskCount, mappingCount, changeCount, attachmentCount int
	if err := pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND owner_id=$2 AND task_id IS NULL),
(SELECT count(*) FROM tasks WHERE conversation_id=$1 AND owner_id=$2),
(SELECT count(*) FROM personal_history_import_mappings WHERE owner_id=$2 AND source_id=$3 AND device_account_id=$4 AND source_conversation_id=$5),
(SELECT count(*) FROM conversation_message_attachments WHERE owner_id=$2 AND attachment_id=$7),
(SELECT count(*) FROM account_change_log WHERE owner_id=$2 AND object_id=ANY($6::text[]))`,
		first.Conversation.ID, owner.ID, input.SourceID, input.DeviceAccountID, input.SourceConversationID,
		[]string{first.Conversation.ID, first.Messages[0].Message.ID, first.Messages[1].Message.ID}, attachmentID).Scan(
		&messageCount, &taskCount, &mappingCount, &attachmentCount, &changeCount); err != nil {
		t.Fatal(err)
	}
	if messageCount != 2 || taskCount != 0 || mappingCount != 3 || attachmentCount != 1 || changeCount != 3 {
		t.Fatalf("persisted import state: messages=%d tasks=%d mappings=%d attachments=%d changes=%d", messageCount, taskCount, mappingCount, attachmentCount, changeCount)
	}

	replayed, err := store.ImportPersonalHistory(ctx, principal, input)
	if err != nil || !replayed.Replay || replayed.Conversation.ID != first.Conversation.ID || len(replayed.Messages) != len(first.Messages) {
		t.Fatalf("exact replay = %+v, %v", replayed, err)
	}
	for i := range first.Messages {
		if !replayed.Messages[i].Replay || replayed.Messages[i].Message.ID != first.Messages[i].Message.ID ||
			replayed.Messages[i].Message.Sequence != first.Messages[i].Message.Sequence {
			t.Fatalf("replay mapping[%d] = %+v; initial=%+v", i, replayed.Messages[i], first.Messages[i])
		}
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE conversation_id=$1 AND owner_id=$2 AND task_id IS NULL`,
		first.Conversation.ID, owner.ID).Scan(&messageCount); err != nil || messageCount != 2 {
		t.Fatalf("replay changed message count to %d, err=%v", messageCount, err)
	}

	changed := input
	changed.Messages = append([]conversation.HistoryMessage(nil), input.Messages...)
	changed.Messages[0].Content = "different content"
	if _, err := store.ImportPersonalHistory(ctx, principal, changed); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("changed replay error = %v, want idempotency_conflict", err)
	}
	if err := store.RevokeToken(ctx, principal.TokenID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportPersonalHistory(ctx, principal, input); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked credential replay error = %v, want unauthenticated", err)
	}
}
