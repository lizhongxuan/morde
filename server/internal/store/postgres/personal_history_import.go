package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

func (s *Store) ImportPersonalHistory(ctx context.Context, principal identity.Principal, input conversation.HistoryImport) (conversation.HistoryImportResult, error) {
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return conversation.HistoryImportResult{}, domain.NewError(domain.CodeForbidden, "portal session or client credential required")
	}
	if !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) || !principal.HasScope(identity.ScopeTaskSubmit) ||
		(principal.Kind == identity.KindSession && principal.ClientInstallationID != "") ||
		(principal.Kind == identity.KindClient && (!domain.ValidClientInstallationID(principal.ClientInstallationID) || principal.Role != identity.RoleUser)) {
		return conversation.HistoryImportResult{}, domain.NewError(domain.CodeForbidden, "history import is not authorized")
	}
	if err := conversation.ValidateHistoryImport(input); err != nil {
		return conversation.HistoryImportResult{}, err
	}
	var result conversation.HistoryImportResult
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		// Auth is rechecked before mapping lookup, including replay, so revocation
		// cannot be bypassed by a previously imported source key.
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := lockPersonalHistoryImportSource(ctx, tx, instanceID, principal.UserID, input); err != nil {
			return err
		}

		requestedTarget := input.TargetConversationID
		conversationID, markerExists, err := loadHistoryImportMarker(ctx, tx, instanceID, principal.UserID, input)
		if err != nil {
			return err
		}
		createdConversation := false
		if markerExists {
			var storedTarget string
			if err := tx.QueryRow(ctx, `SELECT requested_conversation_id
FROM personal_history_import_mappings
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND source_id=$3
  AND device_account_id=$4 AND source_conversation_id=$5 AND source_message_id=''
FOR UPDATE`, instanceID, principal.UserID, input.SourceID, input.DeviceAccountID, input.SourceConversationID).Scan(&storedTarget); err != nil {
				return fmt.Errorf("read history import marker target: %w", err)
			}
			if storedTarget != requestedTarget {
				return domain.NewError(domain.CodeIdempotencyConflict, "history import source conversation was mapped to a different target")
			}
			if err := lockPrivateConversationForImport(ctx, tx, conversationID, principal.UserID); err != nil {
				return err
			}
		} else {
			if requestedTarget != "" {
				conversationID = requestedTarget
				if err := lockPrivateConversationForImport(ctx, tx, conversationID, principal.UserID); err != nil {
					return err
				}
			} else {
				conversationID, err = domain.NewConversationID()
				if err != nil {
					return fmt.Errorf("generate history import conversation id: %w", err)
				}
				if _, err := tx.Exec(ctx, `INSERT INTO conversations (id,owner_id,host_id,title,model_id,source_kind)
VALUES ($1,$2,NULL,$3,'',$4)`, conversationID, principal.UserID, strings.TrimSpace(input.Title), conversation.HistoryImportSourceKind); err != nil {
					return fmt.Errorf("create imported private conversation: %w", err)
				}
				createdConversation = true
			}
			if _, err := tx.Exec(ctx, `INSERT INTO personal_history_import_mappings
(service_instance_id,owner_id,source_id,device_account_id,source_conversation_id,source_message_id,
 requested_conversation_id,canonical_conversation_id,canonical_message_id,content_hash)
VALUES ($1::uuid,$2,$3,$4,$5,'',$6,$7,NULL,NULL)`, instanceID, principal.UserID, input.SourceID,
				input.DeviceAccountID, input.SourceConversationID, requestedTarget, conversationID); err != nil {
				return fmt.Errorf("create history import conversation mapping: %w", err)
			}
		}

		result.Conversation, err = loadHistoryImportConversation(ctx, tx, conversationID, principal.UserID)
		if err != nil {
			return err
		}
		result.Messages = make([]conversation.ImportedHistoryMessage, 0, len(input.Messages))
		changes := make([]accountObjectChange, 0, len(input.Messages)+1)
		if createdConversation {
			changes = append(changes, accountObjectChange{OwnerID: principal.UserID, ObjectType: "conversation", ObjectID: conversationID, Operation: "upsert"})
		}
		newMessages := 0
		for _, sourceMessage := range input.Messages {
			digest, err := conversation.HashHistoryMessage(sourceMessage)
			if err != nil {
				return fmt.Errorf("hash imported history message: %w", err)
			}
			imported, exists, err := loadHistoryImportMessageMapping(ctx, tx, instanceID, principal.UserID, input, sourceMessage.SourceMessageID)
			if err != nil {
				return err
			}
			if exists {
				if imported.ConversationID != conversationID || !bytes.Equal(imported.ContentHash, digest[:]) {
					return domain.NewError(domain.CodeIdempotencyConflict, "history import message source key was reused with different content")
				}
				if _, err := lockAndValidateHistoryAttachments(ctx, tx, instanceID, principal.UserID, sourceMessage.AttachmentIDs); err != nil {
					return err
				}
				importedMessage, err := loadMappedHistoryMessage(ctx, tx, imported.CanonicalMessageID, conversationID, principal.UserID)
				if err != nil {
					return err
				}
				storedAttachmentIDs, err := loadHistoryMessageAttachmentIDs(ctx, tx, instanceID, principal.UserID, conversationID, importedMessage.ID)
				if err != nil {
					return err
				}
				if !sameStringSet(storedAttachmentIDs, sourceMessage.AttachmentIDs) {
					return domain.NewError(domain.CodeInternal, "history import attachment mapping is inconsistent")
				}
				importedMessage.AttachmentIDs = append([]string(nil), sourceMessage.AttachmentIDs...)
				result.Messages = append(result.Messages, conversation.ImportedHistoryMessage{
					SourceMessageID: sourceMessage.SourceMessageID, Message: importedMessage,
					AttachmentIDs: append([]string(nil), sourceMessage.AttachmentIDs...), Replay: true,
				})
				continue
			}

			uploadIDs, err := lockAndValidateHistoryAttachments(ctx, tx, instanceID, principal.UserID, sourceMessage.AttachmentIDs)
			if err != nil {
				return err
			}
			messageID, err := domain.NewMessageID()
			if err != nil {
				return fmt.Errorf("generate imported message id: %w", err)
			}
			message := conversation.Message{
				ID: messageID, ConversationID: conversationID, OwnerID: principal.UserID,
				Role: sourceMessage.Role, Content: sourceMessage.Content,
				SourceKind:    conversation.HistoryImportSourceKind,
				AttachmentIDs: append([]string(nil), sourceMessage.AttachmentIDs...),
			}
			if err := insertMessage(ctx, tx, message); err != nil {
				return err
			}
			if err := insertHistoryMessageAttachments(ctx, tx, instanceID, principal.UserID, conversationID, messageID, sourceMessage.AttachmentIDs, uploadIDs); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO personal_history_import_mappings
(service_instance_id,owner_id,source_id,device_account_id,source_conversation_id,source_message_id,
 requested_conversation_id,canonical_conversation_id,canonical_message_id,content_hash)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, instanceID, principal.UserID, input.SourceID,
				input.DeviceAccountID, input.SourceConversationID, sourceMessage.SourceMessageID, requestedTarget,
				conversationID, messageID, digest[:]); err != nil {
				return fmt.Errorf("create history import message mapping: %w", err)
			}
			message, err = loadMappedHistoryMessage(ctx, tx, messageID, conversationID, principal.UserID)
			if err != nil {
				return err
			}
			message.AttachmentIDs = append([]string(nil), sourceMessage.AttachmentIDs...)
			result.Messages = append(result.Messages, conversation.ImportedHistoryMessage{
				SourceMessageID: sourceMessage.SourceMessageID, Message: message,
				AttachmentIDs: append([]string(nil), sourceMessage.AttachmentIDs...),
			})
			newMessages++
			changes = append(changes, accountObjectChange{OwnerID: principal.UserID, ObjectType: "message", ObjectID: messageID, Operation: "upsert"})
		}

		if newMessages > 0 {
			if !createdConversation {
				changes = append([]accountObjectChange{{OwnerID: principal.UserID, ObjectType: "conversation", ObjectID: conversationID, Operation: "upsert"}}, changes...)
			}
			if _, err := tx.Exec(ctx, `UPDATE conversations SET updated_at=clock_timestamp() WHERE id=$1 AND owner_id=$2`, conversationID, principal.UserID); err != nil {
				return fmt.Errorf("touch imported conversation: %w", err)
			}
		}
		if err := recordAccountObjectChanges(ctx, tx, changes...); err != nil {
			return err
		}
		result.Conversation, err = loadHistoryImportConversation(ctx, tx, conversationID, principal.UserID)
		if err != nil {
			return err
		}
		result.Replay = !createdConversation && newMessages == 0
		return nil
	})
	if err != nil {
		return conversation.HistoryImportResult{}, err
	}
	return result, nil
}

type historyImportMapping struct {
	ConversationID     string
	CanonicalMessageID string
	ContentHash        []byte
}

func lockPersonalHistoryImportSource(ctx context.Context, tx pgx.Tx, instanceID, ownerID string, input conversation.HistoryImport) error {
	// Source IDs reject ASCII control bytes, so this separator cannot collide with a field value.
	key := strings.Join([]string{instanceID, ownerID, input.SourceID, input.DeviceAccountID, input.SourceConversationID}, "\x1f")
	var locked int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM (SELECT pg_advisory_xact_lock(hashtextextended($1,0))) AS history_import_lock`, key).Scan(&locked); err != nil {
		return fmt.Errorf("lock history import source mapping: %w", err)
	}
	return nil
}

func loadHistoryImportMarker(ctx context.Context, tx pgx.Tx, instanceID, ownerID string, input conversation.HistoryImport) (string, bool, error) {
	var conversationID string
	err := tx.QueryRow(ctx, `SELECT canonical_conversation_id
FROM personal_history_import_mappings
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND source_id=$3
  AND device_account_id=$4 AND source_conversation_id=$5 AND source_message_id=''`,
		instanceID, ownerID, input.SourceID, input.DeviceAccountID, input.SourceConversationID).Scan(&conversationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("load history import conversation mapping: %w", err)
	}
	return conversationID, true, nil
}

func lockPrivateConversationForImport(ctx context.Context, tx pgx.Tx, conversationID, ownerID string) error {
	var lockedID string
	err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id=$1 AND owner_id=$2 AND host_id IS NULL FOR UPDATE`, conversationID, ownerID).Scan(&lockedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "private conversation not found")
	}
	if err != nil {
		return fmt.Errorf("lock target private conversation: %w", err)
	}
	return nil
}

func loadHistoryImportConversation(ctx context.Context, tx pgx.Tx, conversationID, ownerID string) (conversation.Conversation, error) {
	row, err := sqlcgen.New(tx).GetConversation(ctx, sqlcgen.GetConversationParams{ID: conversationID, OwnerID: ownerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return conversation.Conversation{}, domain.NewError(domain.CodeNotFound, "private conversation not found")
	}
	if err != nil {
		return conversation.Conversation{}, fmt.Errorf("load imported conversation: %w", err)
	}
	return conversationDomain(row), nil
}

func loadHistoryImportMessageMapping(ctx context.Context, tx pgx.Tx, instanceID, ownerID string, input conversation.HistoryImport, sourceMessageID string) (historyImportMapping, bool, error) {
	var mapping historyImportMapping
	err := tx.QueryRow(ctx, `SELECT canonical_conversation_id,canonical_message_id,content_hash
FROM personal_history_import_mappings
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND source_id=$3
  AND device_account_id=$4 AND source_conversation_id=$5 AND source_message_id=$6`,
		instanceID, ownerID, input.SourceID, input.DeviceAccountID, input.SourceConversationID, sourceMessageID).Scan(
		&mapping.ConversationID, &mapping.CanonicalMessageID, &mapping.ContentHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return historyImportMapping{}, false, nil
	}
	if err != nil {
		return historyImportMapping{}, false, fmt.Errorf("load history import message mapping: %w", err)
	}
	if mapping.CanonicalMessageID == "" || len(mapping.ContentHash) != 32 {
		return historyImportMapping{}, false, domain.NewError(domain.CodeInternal, "history import message mapping is incomplete")
	}
	return mapping, true, nil
}

func loadMappedHistoryMessage(ctx context.Context, tx pgx.Tx, messageID, conversationID, ownerID string) (conversation.Message, error) {
	var result conversation.Message
	var role string
	err := tx.QueryRow(ctx, `SELECT id,conversation_id,owner_id,role,content,COALESCE(task_id,''),model_id,sequence,
source_kind,source_exchange_id,requested_conversation_id,created_at
FROM conversation_messages WHERE id=$1 AND conversation_id=$2 AND owner_id=$3`, messageID, conversationID, ownerID).Scan(
		&result.ID, &result.ConversationID, &result.OwnerID, &role, &result.Content, &result.TaskID, &result.ModelID,
		&result.Sequence, &result.SourceKind, &result.SourceExchangeID, &result.RequestedConversationID, &result.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return conversation.Message{}, domain.NewError(domain.CodeInternal, "mapped history message is missing")
	}
	if err != nil {
		return conversation.Message{}, fmt.Errorf("load mapped history message: %w", err)
	}
	result.Role = conversation.Role(role)
	if result.TaskID != "" || result.Sequence < 1 || result.SourceKind != conversation.HistoryImportSourceKind {
		return conversation.Message{}, domain.NewError(domain.CodeInternal, "mapped history message is inconsistent")
	}
	return result, nil
}

func lockAndValidateHistoryAttachments(ctx context.Context, tx pgx.Tx, instanceID, ownerID string, attachmentIDs []string) (map[string]string, error) {
	ids := append([]string(nil), attachmentIDs...)
	sort.Strings(ids)
	uploadIDs := make(map[string]string, len(ids))
	for _, attachmentID := range ids {
		if !personal.ValidPersonalAttachmentID(attachmentID) {
			return nil, domain.InvalidArgument("attachment_ids", "personal attachment id is invalid")
		}
		rows, err := tx.Query(ctx, `SELECT upload_id FROM personal_attachment_uploads
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND attachment_id=$3 AND state='committed'
ORDER BY upload_id FOR UPDATE`, instanceID, ownerID, attachmentID)
		if err != nil {
			return nil, fmt.Errorf("lock imported personal attachment: %w", err)
		}
		var found []string
		for rows.Next() {
			var uploadID string
			if err := rows.Scan(&uploadID); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan imported personal attachment: %w", err)
			}
			found = append(found, uploadID)
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			return nil, fmt.Errorf("read imported personal attachment: %w", rowsErr)
		}
		if len(found) == 0 {
			return nil, domain.NewError(domain.CodeNotFound, "personal attachment not found")
		}
		if len(found) != 1 || !personal.ValidPersonalAttachmentUploadID(found[0]) {
			return nil, domain.NewError(domain.CodeInternal, "personal attachment mapping is inconsistent")
		}
		var readable bool
		if err := tx.QueryRow(ctx, `SELECT (
  EXISTS (SELECT 1 FROM personal_objects p
    WHERE p.service_instance_id=$1::uuid AND p.owner_id=$2 AND p.object_type='library_item'
      AND p.body IS NOT NULL AND p.body->>'attachment_id'=$3)
  OR EXISTS (SELECT 1 FROM conversation_message_attachments r
    JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.conversation_id=r.conversation_id
    JOIN conversations c ON c.id=m.conversation_id AND c.owner_id=m.owner_id
    WHERE r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.attachment_id=$3)
)`, instanceID, ownerID, attachmentID).Scan(&readable); err != nil {
			return nil, fmt.Errorf("check imported attachment readability: %w", err)
		}
		if !readable {
			return nil, domain.NewError(domain.CodeForbidden, "personal attachment has no readable owner reference")
		}
		uploadIDs[attachmentID] = found[0]
	}
	return uploadIDs, nil
}

func insertHistoryMessageAttachments(ctx context.Context, tx pgx.Tx, instanceID, ownerID, conversationID, messageID string, attachmentIDs []string, uploadIDs map[string]string) error {
	for _, attachmentID := range attachmentIDs {
		uploadID, ok := uploadIDs[attachmentID]
		if !ok {
			return domain.NewError(domain.CodeInternal, "personal attachment upload mapping is missing")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO conversation_message_attachments
(service_instance_id,owner_id,conversation_id,message_id,upload_id,attachment_id)
VALUES ($1::uuid,$2,$3,$4,$5,$6)`, instanceID, ownerID, conversationID, messageID, uploadID, attachmentID); err != nil {
			return fmt.Errorf("attach personal history message: %w", err)
		}
	}
	return nil
}

func loadHistoryMessageAttachmentIDs(ctx context.Context, tx pgx.Tx, instanceID, ownerID, conversationID, messageID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT attachment_id FROM conversation_message_attachments
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND conversation_id=$3 AND message_id=$4
ORDER BY attachment_id`, instanceID, ownerID, conversationID, messageID)
	if err != nil {
		return nil, fmt.Errorf("load imported message attachments: %w", err)
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var attachmentID string
		if err := rows.Scan(&attachmentID); err != nil {
			return nil, fmt.Errorf("scan imported message attachment: %w", err)
		}
		result = append(result, attachmentID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list imported message attachments: %w", err)
	}
	return result, nil
}

func sameStringSet(left, right []string) bool {
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
