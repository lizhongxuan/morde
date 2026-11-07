package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
}

func loadConversationUploadInTx(ctx context.Context, tx pgx.Tx, receipt app.ConversationMessageReceipt) (conversationUploadRow, error) {
	var row conversationUploadRow
	err := tx.QueryRow(ctx, `SELECT total_bytes,total_chunks,content_sha256,request_hash,request_metadata,state,COALESCE(message_id,'')
FROM conversation_message_uploads
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4 FOR UPDATE`,
		receipt.ServiceInstanceID, receipt.Message.ActorID, receipt.Message.ConversationID, receipt.ClientID).
		Scan(&row.TotalBytes, &row.TotalChunks, &row.ContentHash, &row.RequestHash, &row.Metadata, &row.State, &row.MessageID)
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
			result, err = conversationUploadViewInTx(ctx, tx, init.Receipt, row)
			return err
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
 content_sha256,request_hash,request_metadata,authorization_snapshot)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11::jsonb)`, init.Receipt.ServiceInstanceID,
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
			if !bytes.Equal(priorContent, content) || !bytes.Equal(priorDigest, digest) {
				return domain.NewError(domain.CodeIdempotencyConflict, "message chunk was already uploaded with different content")
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
		_, err = tx.Exec(ctx, `UPDATE conversation_message_uploads SET state='committed',message_id=$5,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND client_message_id=$4`,
			receipt.ServiceInstanceID, receipt.Message.ActorID, receipt.Message.ConversationID, receipt.ClientID, result.ID)
		if err != nil {
			return fmt.Errorf("commit conversation message upload: %w", err)
		}
		return nil
	})
	return result, err
}
