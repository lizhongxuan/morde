package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

type conversationUploadRow struct {
	TotalBytes  int
	TotalChunks int
	ContentHash []byte
	RequestHash []byte
	Metadata    []byte
	State       string
	MessageID   string
	Expired     bool
}

func loadConversationUploadInTx(ctx context.Context, tx pgx.Tx, receipt app.ConversationMessageReceipt) (conversationUploadRow, error) {
	var row conversationUploadRow
	err := tx.QueryRow(ctx, `SELECT total_bytes,total_chunks,content_sha256,request_hash,request_metadata,state,COALESCE(message_id,''),
(expires_at IS NOT NULL AND expires_at <= clock_timestamp())
FROM conversation_message_uploads
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4 FOR UPDATE`,
		receipt.ServiceInstanceID, receipt.Message.ActorID, receipt.Message.ConversationID, receipt.ClientID).
		Scan(&row.TotalBytes, &row.TotalChunks, &row.ContentHash, &row.RequestHash, &row.Metadata, &row.State, &row.MessageID, &row.Expired)
	if errors.Is(err, pgx.ErrNoRows) {
		return conversationUploadRow{}, domain.NewError(domain.CodeNotFound, "message upload not found")
	}
	if err != nil {
		return conversationUploadRow{}, fmt.Errorf("load conversation message upload: %w", err)
	}
	return row, nil
}

func conversationUploadViewInTx(ctx context.Context, tx pgx.Tx, receipt app.ConversationMessageReceipt, row conversationUploadRow) (app.ConversationMessageUpload, error) {
	value := app.ConversationMessageUpload{ClientMessageID: receipt.ClientID, State: row.State, TotalBytes: row.TotalBytes,
		TotalChunks: row.TotalChunks, ReceivedSequences: make([]int, 0)}
	rows, err := tx.Query(ctx, `SELECT sequence FROM conversation_message_upload_chunks
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4 ORDER BY sequence`,
		receipt.ServiceInstanceID, receipt.Message.ActorID, receipt.Message.ConversationID, receipt.ClientID)
	if err != nil {
		return app.ConversationMessageUpload{}, fmt.Errorf("list conversation upload chunks: %w", err)
	}
	for rows.Next() {
		var sequence int
		if err := rows.Scan(&sequence); err != nil {
			rows.Close()
			return app.ConversationMessageUpload{}, fmt.Errorf("scan conversation upload chunk: %w", err)
		}
		value.ReceivedSequences = append(value.ReceivedSequences, sequence)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return app.ConversationMessageUpload{}, fmt.Errorf("read conversation upload chunks: %w", err)
	}
	rows.Close()
	if row.State == "committed" {
		message, _, err := loadConversationReceipt(ctx, tx, receipt.Message.ActorID, receipt.Message.ConversationID, receipt.ClientID)
		if err != nil || message.ID != row.MessageID {
			return app.ConversationMessageUpload{}, domain.NewError(domain.CodeInvalidState, "committed message receipt is unavailable")
		}
		value.Message = &message
	}
	return value, nil
}

func (s *Store) InitializeConversationMessageUpload(ctx context.Context, init app.ConversationMessageUploadInit) (app.ConversationMessageUpload, error) {
	if init.TotalBytes < 1 || init.TotalBytes > app.MaxConversationMessageBytes || init.TotalChunks != (init.TotalBytes+app.ConversationMessageChunkBytes-1)/app.ConversationMessageChunkBytes ||
		len(init.ContentHash) != sha256.Size || len(init.RequestHash) != sha256.Size {
		return app.ConversationMessageUpload{}, domain.InvalidArgument("upload", "message upload is invalid")
	}
	metadata, err := json.Marshal(init.Receipt.Input)
	if err != nil {
		return app.ConversationMessageUpload{}, fmt.Errorf("encode message upload metadata: %w", err)
	}
	authorization, err := json.Marshal(init.Receipt.Authorization)
	if err != nil {
		return app.ConversationMessageUpload{}, fmt.Errorf("encode message upload authorization: %w", err)
	}
	var result app.ConversationMessageUpload
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockConversationReceiptScopeInTx(ctx, tx, init.Receipt); err != nil {
			return err
		}
		row, err := loadConversationUploadInTx(ctx, tx, init.Receipt)
		if err == nil {
			if !bytes.Equal(row.RequestHash, init.RequestHash) {
				return domain.NewError(domain.CodeIdempotencyConflict, "client message id was used for a different upload")
			}
			if !row.Expired {
				result, err = conversationUploadViewInTx(ctx, tx, init.Receipt, row)
				return err
			}
			if row.State != "uploading" {
				return domain.NewError(domain.CodeInvalidState, "committed message upload cannot expire")
			}
			if _, err := tx.Exec(ctx, `DELETE FROM conversation_message_upload_chunks
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4`,
				init.Receipt.ServiceInstanceID, init.Receipt.Message.ActorID, init.Receipt.Message.ConversationID, init.Receipt.ClientID); err != nil {
				return fmt.Errorf("reset expired conversation upload chunks: %w", err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM conversation_message_uploads
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4 AND state='uploading'`,
				init.Receipt.ServiceInstanceID, init.Receipt.Message.ActorID, init.Receipt.Message.ConversationID, init.Receipt.ClientID); err != nil {
				return fmt.Errorf("reset expired conversation upload: %w", err)
			}
			err = domain.NewError(domain.CodeNotFound, "expired message upload was reset")
		}
		if domain.CodeOf(err) != domain.CodeNotFound {
			return err
		}
		if _, _, err := loadConversationReceipt(ctx, tx, init.Receipt.Message.ActorID, init.Receipt.Message.ConversationID, init.Receipt.ClientID); err == nil {
			return domain.NewError(domain.CodeIdempotencyConflict, "client message id is already accepted")
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO conversation_message_uploads
(service_instance_id,owner_id,actor_id,conversation_id,client_message_id,total_bytes,total_chunks,
 content_sha256,request_hash,request_metadata,authorization_snapshot,expires_at)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11::jsonb,clock_timestamp()+interval '24 hours')`, init.Receipt.ServiceInstanceID,
			init.Receipt.Message.OwnerID, init.Receipt.Message.ActorID, init.Receipt.Message.ConversationID, init.Receipt.ClientID,
			init.TotalBytes, init.TotalChunks, init.ContentHash, init.RequestHash, metadata, authorization)
		if err != nil {
			return fmt.Errorf("initialize conversation message upload: %w", err)
		}
		result = app.ConversationMessageUpload{ClientMessageID: init.Receipt.ClientID, State: "uploading", TotalBytes: init.TotalBytes,
			TotalChunks: init.TotalChunks, ReceivedSequences: []int{}}
		return nil
	})
	return result, err
}

func (s *Store) PutConversationMessageChunk(ctx context.Context, receipt app.ConversationMessageReceipt, sequence int, content, digest []byte) error {
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockConversationReceiptScopeInTx(ctx, tx, receipt); err != nil {
			return err
		}
		row, err := loadConversationUploadInTx(ctx, tx, receipt)
		if err != nil {
			return err
		}
		if row.Expired {
			return domain.NewError(domain.CodeNotFound, "message upload expired")
		}
		if sequence < 0 || sequence >= row.TotalChunks {
			return domain.InvalidArgument("sequence", "message chunk sequence is out of range")
		}
		expectedBytes := min(app.ConversationMessageChunkBytes, row.TotalBytes-sequence*app.ConversationMessageChunkBytes)
		actual := sha256.Sum256(content)
		if len(content) != expectedBytes || !bytes.Equal(digest, actual[:]) {
			return domain.InvalidArgument("chunk", "message chunk size or digest does not match")
		}
		var priorContent, priorDigest []byte
		err = tx.QueryRow(ctx, `SELECT content,content_sha256 FROM conversation_message_upload_chunks
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4 AND sequence=$5`,
			receipt.ServiceInstanceID, receipt.Message.ActorID, receipt.Message.ConversationID, receipt.ClientID, sequence).
			Scan(&priorContent, &priorDigest)
		if err == nil {
			if !bytes.Equal(priorDigest, digest) || (priorContent != nil && !bytes.Equal(priorContent, content)) {
				return domain.NewError(domain.CodeIdempotencyConflict, "message chunk was already uploaded with different content")
			}
			if row.State == "uploading" {
				if _, err := tx.Exec(ctx, `UPDATE conversation_message_uploads
SET expires_at=clock_timestamp()+interval '24 hours',updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4 AND state='uploading'`,
					receipt.ServiceInstanceID, receipt.Message.ActorID, receipt.Message.ConversationID, receipt.ClientID); err != nil {
					return fmt.Errorf("refresh conversation message upload expiry: %w", err)
				}
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read existing message chunk: %w", err)
		}
		if row.State != "uploading" {
			return domain.NewError(domain.CodeConflict, "message upload is already committed")
		}
		_, err = tx.Exec(ctx, `INSERT INTO conversation_message_upload_chunks
(service_instance_id,actor_id,conversation_id,client_message_id,sequence,content,content_sha256)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7)`, receipt.ServiceInstanceID, receipt.Message.ActorID,
			receipt.Message.ConversationID, receipt.ClientID, sequence, content, digest)
		if err != nil {
			return fmt.Errorf("store conversation message chunk: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE conversation_message_uploads
SET expires_at=clock_timestamp()+interval '24 hours',updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4 AND state='uploading'`,
			receipt.ServiceInstanceID, receipt.Message.ActorID, receipt.Message.ConversationID, receipt.ClientID); err != nil {
			return fmt.Errorf("refresh conversation message upload expiry: %w", err)
		}
		return nil
	})
}

func (s *Store) GetConversationMessageUpload(ctx context.Context, receipt app.ConversationMessageReceipt) (app.ConversationMessageUpload, error) {
	var value app.ConversationMessageUpload
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockConversationReceiptScopeInTx(ctx, tx, receipt); err != nil {
			return err
		}
		row, err := loadConversationUploadInTx(ctx, tx, receipt)
		if err != nil {
			return err
		}
		if row.Expired {
			return domain.NewError(domain.CodeNotFound, "message upload expired")
		}
		value, err = conversationUploadViewInTx(ctx, tx, receipt, row)
		return err
	})
	return value, err
}

func (s *Store) CommitConversationMessageUpload(ctx context.Context, receipt app.ConversationMessageReceipt) (conversation.Message, error) {
	var result conversation.Message
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockConversationReceiptScopeInTx(ctx, tx, receipt); err != nil {
			return err
		}
		row, err := loadConversationUploadInTx(ctx, tx, receipt)
		if err != nil {
			return err
		}
		if row.Expired {
			return domain.NewError(domain.CodeNotFound, "message upload expired")
		}
		if row.State == "committed" {
			result, _, err = loadConversationReceipt(ctx, tx, receipt.Message.ActorID, receipt.Message.ConversationID, receipt.ClientID)
			if err == nil && result.ID != row.MessageID {
				return domain.NewError(domain.CodeInvalidState, "committed message receipt does not match")
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT sequence,content,content_sha256 FROM conversation_message_upload_chunks
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4 ORDER BY sequence`,
			receipt.ServiceInstanceID, receipt.Message.ActorID, receipt.Message.ConversationID, receipt.ClientID)
		if err != nil {
			return fmt.Errorf("read message upload for commit: %w", err)
		}
		content := make([]byte, 0, row.TotalBytes)
		sequence := 0
		for rows.Next() {
			var storedSequence int
			var chunk, digest []byte
			if err := rows.Scan(&storedSequence, &chunk, &digest); err != nil {
				rows.Close()
				return fmt.Errorf("scan message upload for commit: %w", err)
			}
			actual := sha256.Sum256(chunk)
			if storedSequence != sequence || !bytes.Equal(digest, actual[:]) {
				rows.Close()
				return domain.NewError(domain.CodeConflict, "message upload has a missing or invalid chunk")
			}
			content = append(content, chunk...)
			sequence++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read message upload chunks: %w", err)
		}
		rows.Close()
		actual := sha256.Sum256(content)
		if sequence != row.TotalChunks || len(content) != row.TotalBytes || !bytes.Equal(actual[:], row.ContentHash) ||
			!utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
			return domain.NewError(domain.CodeConflict, "message upload is incomplete or its content digest is invalid")
		}
		var input app.ConversationRoutingInput
		if err := json.Unmarshal(row.Metadata, &input); err != nil {
			return domain.NewError(domain.CodeInvalidState, "message upload metadata is invalid")
		}
		if strings.TrimSpace(string(content)) == "" && len(input.ClientAttachmentIDs) == 0 {
			return domain.InvalidArgument("content", "message text or an attachment is required")
		}
		messageID, err := domain.NewMessageID()
		if err != nil {
			return err
		}
		receipt.Message.ID = messageID
		receipt.Message.Content = string(content)
		receipt.Input = input
		receipt.RequestHash, err = app.ConversationReceiptHash(receipt.Message.ConversationID, receipt.Message.Content, input)
		if err != nil {
			return err
		}
		authorization, err := json.Marshal(receipt.Authorization)
		if err != nil {
			return fmt.Errorf("encode message commit authorization: %w", err)
		}
		var replayed bool
		if err := s.acceptConversationMessageInTx(ctx, tx, receipt, row.Metadata, authorization, &result, &replayed); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE conversation_message_uploads SET state='committed',message_id=$5,expires_at=NULL,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4`,
			receipt.ServiceInstanceID, receipt.Message.ActorID, receipt.Message.ConversationID, receipt.ClientID, result.ID)
		if err != nil {
			return fmt.Errorf("commit conversation message upload: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE conversation_message_upload_chunks SET content=NULL
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4`,
			receipt.ServiceInstanceID, receipt.Message.ActorID, receipt.Message.ConversationID, receipt.ClientID); err != nil {
			return fmt.Errorf("discard committed conversation message upload content: %w", err)
		}
		return nil
	})
	return result, err
}

func (s *Store) CleanupExpiredConversationMessageUploads(ctx context.Context, now time.Time, limit int32) (int32, error) {
	if now.IsZero() || limit < 1 || limit > 1000 {
		return 0, domain.InvalidArgument("limit", "conversation message upload cleanup request is invalid")
	}
	var removed int32
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT service_instance_id::text,actor_id,conversation_id,client_message_id
FROM conversation_message_uploads
WHERE state='uploading' AND expires_at <= $1
ORDER BY expires_at,service_instance_id,actor_id,conversation_id,client_message_id
LIMIT $2 FOR UPDATE SKIP LOCKED`, now.UTC(), limit)
		if err != nil {
			return fmt.Errorf("select expired conversation message uploads: %w", err)
		}
		type uploadKey struct {
			serviceInstanceID string
			actorID           string
			conversationID    string
			clientMessageID   string
		}
		candidates := make([]uploadKey, 0, limit)
		for rows.Next() {
			var key uploadKey
			if err := rows.Scan(&key.serviceInstanceID, &key.actorID, &key.conversationID, &key.clientMessageID); err != nil {
				rows.Close()
				return fmt.Errorf("scan expired conversation message upload: %w", err)
			}
			candidates = append(candidates, key)
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			return fmt.Errorf("read expired conversation message uploads: %w", rowsErr)
		}
		for _, key := range candidates {
			if _, err := tx.Exec(ctx, `DELETE FROM conversation_message_upload_chunks
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4`,
				key.serviceInstanceID, key.actorID, key.conversationID, key.clientMessageID); err != nil {
				return fmt.Errorf("delete expired conversation message upload chunks: %w", err)
			}
			result, err := tx.Exec(ctx, `DELETE FROM conversation_message_uploads
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4
  AND state='uploading' AND expires_at <= $5`, key.serviceInstanceID, key.actorID, key.conversationID, key.clientMessageID, now.UTC())
			if err != nil {
				return fmt.Errorf("delete expired conversation message upload: %w", err)
			}
			removed += int32(result.RowsAffected())
		}
		return nil
	})
	return removed, err
}
