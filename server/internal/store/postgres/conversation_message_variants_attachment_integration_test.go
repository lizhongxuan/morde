package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

func TestConversationVariantsReuseOnlySourceAuthorizedAttachments(t *testing.T) {
	for _, action := range []app.ConversationMessageVariantAction{
		app.ConversationMessageVariantEditResend,
		app.ConversationMessageVariantRegenerateText,
	} {
		t.Run(string(action), func(t *testing.T) {
			f := newRouteActionsTestFixture(t)
			ctx := context.Background()
			principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser,
				Kind: identity.KindSession, TokenID: f.authorization.TokenID}
			createPersonalLibraryItemForAttachmentTest(t, f.store, principal, "variant-attachment-library")
			data := []byte("source-authorized attachment")
			upload, err := f.store.CreatePersonalAttachmentUpload(ctx, principal,
				newPersonalAttachmentUploadForTest(t, f.ownerID, "variant-attachment-library", 1, data), "variant-upload-001")
			if err != nil {
				t.Fatal(err)
			}
			reservation, err := f.store.ReservePersonalAttachment(ctx, principal, upload.UploadID, int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.ActivatePersonalAttachmentUpload(ctx, principal, upload.UploadID, reservation); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(data)
			chunk := personal.AttachmentChunk{UploadID: upload.UploadID, Sequence: 0, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}
			if _, err := f.store.PutPersonalAttachmentChunk(ctx, principal, chunk, bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
			attachment, err := f.store.CommitPersonalAttachment(ctx, principal, upload.UploadID, "variant-commit-001")
			if err != nil {
				t.Fatal(err)
			}

			const slotID = "source-attachment-001"
			input := app.ConversationRoutingInput{ModelID: "p1234567", ClientAttachmentIDs: []string{slotID}}
			source, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
				Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.ownerID, ActorID: f.ownerID,
					ConversationID: f.conversation.ID, Role: conversation.RoleUser, Content: "Review this attachment"},
				ServiceInstanceID: f.instanceID, ClientID: "variant-source-001", RequestHash: variantAttachmentHash("variant-source-001"),
				Input: input, Authorization: f.authorization,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.AssociateConversationMessageAttachment(ctx, principal, f.conversation.ID, source.ID,
				slotID, attachment.AttachmentID, f.instanceID); err != nil {
				t.Fatal(err)
			}

			first := createAttachmentVariantForTest(t, f, principal, source.ID, 1,
				app.ConversationMessageVariantEditResend, "variant-edit-001", "Edited request")
			variantSourceMessageID := first.UserMessageID
			if action == app.ConversationMessageVariantRegenerateText {
				assistantID := mustID(t, domain.NewMessageID)
				if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
					if err := insertMessage(ctx, tx, conversation.Message{ID: assistantID, ConversationID: f.conversation.ID,
						OwnerID: f.ownerID, ActorID: f.ownerID, Role: conversation.RoleAssistant, Content: "Earlier answer",
						SourceKind: "conversation", VariantGroupID: source.ID, VariantVersion: 2,
						VariantAction: string(app.ConversationMessageVariantEditResend), VariantOperationID: first.OperationID}); err != nil {
						return err
					}
					_, err := tx.Exec(ctx, `UPDATE conversation_message_variants SET state='completed',assistant_message_id=$2
WHERE operation_id=$1`, first.OperationID, assistantID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				second := createAttachmentVariantForTest(t, f, principal, assistantID, 2,
					app.ConversationMessageVariantRegenerateText, "variant-regen-001", "")
				variantSourceMessageID = second.UserMessageID
			} else {
				second := createAttachmentVariantForTest(t, f, principal, first.UserMessageID, 2,
					app.ConversationMessageVariantEditResend, "variant-edit-002", "Edited again")
				variantSourceMessageID = second.UserMessageID
			}

			var metadata []byte
			if err := f.store.pool.QueryRow(ctx, `SELECT request_metadata FROM conversation_message_routes WHERE message_id=$1`,
				variantSourceMessageID).Scan(&metadata); err != nil {
				t.Fatal(err)
			}
			var storedInput app.ConversationRoutingInput
			if err := json.Unmarshal(metadata, &storedInput); err != nil {
				t.Fatal(err)
			}
			if storedInput.VariantSourceMessageID != source.ID || len(storedInput.ClientAttachmentIDs) != 1 ||
				storedInput.ClientAttachmentIDs[0] != slotID {
				t.Fatalf("variant attachment lineage = %+v; want only source message %s and slot %s",
					storedInput, source.ID, slotID)
			}
			var copiedGrants int
			if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_message_attachments WHERE message_id=$1`,
				variantSourceMessageID).Scan(&copiedGrants); err != nil || copiedGrants != 0 {
				t.Fatalf("variant copied %d attachment grants; err=%v", copiedGrants, err)
			}
			claim := app.ConversationRouteClaim{ServiceInstanceID: f.instanceID,
				Message: conversation.Message{ID: variantSourceMessageID, OwnerID: f.ownerID, ActorID: f.ownerID,
					ConversationID: f.conversation.ID}, Input: storedInput}
			if err := f.store.WithTx(ctx, func(tx pgx.Tx) error { return validateInitialRouteAttachments(ctx, tx, claim) }); err != nil {
				t.Fatalf("variant lost source-authorized attachment: %v", err)
			}
			claim.Input.ClientAttachmentIDs = append(append([]string(nil), storedInput.ClientAttachmentIDs...), "unrelated-attachment-002")
			if err := f.store.WithTx(ctx, func(tx pgx.Tx) error { return validateInitialRouteAttachments(ctx, tx, claim) }); domain.CodeOf(err) != domain.CodeInvalidState {
				t.Fatalf("variant accepted an attachment absent from its source message: %v", err)
			}
		})
	}
}

func TestConversationVariantIdempotencyIsScopedToServiceInstanceAndAccount(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser,
		Kind: identity.KindSession, TokenID: f.authorization.TokenID}
	source := f.receive(t, "variant-scope-source", "Original request", app.ConversationRoutingInput{})
	command := app.CreateConversationMessageVariantCommand{
		ConversationID: f.conversation.ID, SourceMessageID: source.Message.ID, ServiceInstanceID: f.instanceID,
		Action: app.ConversationMessageVariantEditResend, ExpectedVersion: 1, Content: "Updated request",
		IdempotencyKey: "variant-scope-key-001", OperationID: mustID(t, domain.NewOperationID),
		UserMessageID: mustID(t, domain.NewMessageID), RequestHash: variantAttachmentHash("variant-scope-hash"),
		Authorization: f.authorization,
	}
	first, err := f.store.CreateConversationMessageVariant(ctx, principal, command)
	if err != nil {
		t.Fatal(err)
	}
	var persistedInstanceID string
	if err := f.store.pool.QueryRow(ctx, `SELECT service_instance_id::text FROM conversation_message_variants WHERE operation_id=$1`,
		first.OperationID).Scan(&persistedInstanceID); err != nil || persistedInstanceID != f.instanceID {
		t.Fatalf("variant operation service instance = %q; want %q (err=%v)", persistedInstanceID, f.instanceID, err)
	}

	command.OperationID = mustID(t, domain.NewOperationID)
	command.UserMessageID = mustID(t, domain.NewMessageID)
	replay, err := f.store.CreateConversationMessageVariant(ctx, principal, command)
	if err != nil || !replay.Replayed || replay.OperationID != first.OperationID || replay.VariantVersion != first.VariantVersion {
		t.Fatalf("same service/account idempotency key did not replay persisted operation: first=%+v replay=%+v err=%v",
			first, replay, err)
	}
	var scopedUniqueConstraint bool
	if err := f.store.pool.QueryRow(ctx, `SELECT EXISTS (
SELECT 1 FROM pg_constraint WHERE conrelid='conversation_message_variants'::regclass AND contype='u'
 AND pg_get_constraintdef(oid)='UNIQUE (service_instance_id, actor_id, conversation_id, idempotency_key)')`).Scan(&scopedUniqueConstraint); err != nil || !scopedUniqueConstraint {
		t.Fatalf("service/account idempotency uniqueness constraint was not installed: exists=%t err=%v", scopedUniqueConstraint, err)
	}
}

func createAttachmentVariantForTest(t *testing.T, f routeActionsTestFixture, principal identity.Principal,
	sourceMessageID string, expectedVersion int64, action app.ConversationMessageVariantAction, key, content string) app.ConversationMessageVariantOperation {
	t.Helper()
	operationID := mustID(t, domain.NewOperationID)
	userMessageID := mustID(t, domain.NewMessageID)
	requestHash := variantAttachmentHash(key)
	operation, err := f.store.CreateConversationMessageVariant(context.Background(), principal, app.CreateConversationMessageVariantCommand{
		ConversationID: f.conversation.ID, SourceMessageID: sourceMessageID, ServiceInstanceID: f.instanceID,
		Action: action, ExpectedVersion: expectedVersion, Content: content, IdempotencyKey: key,
		OperationID: operationID, UserMessageID: userMessageID, RequestHash: requestHash, Authorization: f.authorization,
	})
	if err != nil {
		t.Fatal(err)
	}
	if operation.UserMessageID != "" && operation.UserMessageID != userMessageID {
		t.Fatalf("variant user message id=%q; want %q", operation.UserMessageID, userMessageID)
	}
	operation.UserMessageID = userMessageID
	return operation
}

func variantAttachmentHash(value string) []byte {
	digest := sha256.Sum256([]byte(value))
	return digest[:]
}
