package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
)

func TestConversationAttachmentAssociationGatesOnlyItsMessage(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 2000, 4096)
	other := createTestUser(t, store, 1, 2000, 4096)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)
	otherPrincipal := createPersonalReadSessionPrincipal(t, store, other.ID)
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	convo := createMainConversationForTest(t, store, owner.ID)
	createPersonalLibraryItemForAttachmentTest(t, store, principal, "library-association_1")
	content := []byte("synthetic attachment")
	upload := newPersonalAttachmentUploadForTest(t, owner.ID, "library-association_1", 1, content)
	created, err := store.CreatePersonalAttachmentUpload(ctx, principal, upload, "association-upload-001")
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := store.ReservePersonalAttachment(ctx, principal, created.UploadID, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ActivatePersonalAttachmentUpload(ctx, principal, created.UploadID, reservation); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	chunk := personal.AttachmentChunk{UploadID: created.UploadID, Sequence: 0, SizeBytes: int64(len(content)), SHA256: hex.EncodeToString(digest[:])}
	if _, err := store.PutPersonalAttachmentChunk(ctx, principal, chunk, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	committed, err := store.CommitPersonalAttachment(ctx, principal, created.UploadID, "association-commit-001")
	if err != nil {
		t.Fatal(err)
	}
	receive := func(clientID string, slots []string) conversation.Message {
		t.Helper()
		hash := sha256.Sum256([]byte(clientID))
		message, _, err := store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
			Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: owner.ID,
				ConversationID: convo.ID, Role: conversation.RoleUser, Content: clientID},
			ServiceInstanceID: instanceID, ClientID: clientID, RequestHash: hash[:],
			Input: app.ConversationRoutingInput{ClientAttachmentIDs: slots}, Authorization: task.Authorization{Kind: "session", Version: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		return message
	}
	withAttachment := receive("attachment-message-001", []string{"client-attachment-001"})
	independent := receive("independent-message-001", nil)
	claim, found, err := store.ClaimConversationRoute(ctx, "route-worker", time.Minute)
	if err != nil || !found || claim.Message.ID != independent.ID {
		t.Fatalf("independent message should route first: %+v found=%v err=%v", claim, found, err)
	}
	if _, found, err := store.ClaimConversationRoute(ctx, "other-worker", time.Minute); err != nil || found {
		t.Fatalf("unready attachment claimed: found=%v err=%v", found, err)
	}
	associate := func(clientID, attachmentID, serviceID string) error {
		return store.AssociateConversationMessageAttachment(ctx, principal, convo.ID, withAttachment.ID, clientID, attachmentID, serviceID)
	}
	if err := store.AssociateConversationMessageAttachment(ctx, otherPrincipal, convo.ID, withAttachment.ID, "client-attachment-001", committed.AttachmentID, instanceID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("other account associated attachment: %v", err)
	}
	if err := associate("unknown-attachment-001", committed.AttachmentID, instanceID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("undeclared attachment slot accepted: %v", err)
	}
	const workers = 5
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- associate("client-attachment-001", committed.AttachmentID, instanceID)
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("idempotent concurrent association: %v", err)
		}
	}
	if err := associate("client-attachment-001", "pa_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", instanceID); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("attachment slot accepted different file: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversation_attachment_slots WHERE message_id=$1`, withAttachment.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("slot count=%d err=%v", count, err)
	}
	lookedUp, found, err := store.FindConversationMessageByClientID(ctx, owner.ID, "", convo.ID, "attachment-message-001", instanceID)
	if err != nil || !found || len(lookedUp.AttachmentIDs) != 1 || lookedUp.AttachmentIDs[0] != committed.AttachmentID {
		t.Fatalf("reconciled attachment = %+v found=%v err=%v", lookedUp.AttachmentIDs, found, err)
	}
	claim, found, err = store.ClaimConversationRoute(ctx, "ready-worker", time.Minute)
	if err != nil || !found || claim.Message.ID != withAttachment.ID {
		t.Fatalf("ready attachment should route: %+v found=%v err=%v", claim, found, err)
	}
	if err := associate("client-attachment-001", committed.AttachmentID, instanceID); err != nil {
		t.Fatalf("association replay after routing began: %v", err)
	}
}
