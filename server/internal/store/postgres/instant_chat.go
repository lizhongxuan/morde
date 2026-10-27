package postgres

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

func (s *Store) ListInstantChatConversations(ctx context.Context, ownerID string) ([]app.InstantChatConversation, error) {
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.owner_id, c.model_id, c.title, c.created_at, c.updated_at
		FROM conversations c WHERE c.owner_id = $1 ORDER BY c.updated_at DESC, c.id DESC`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list instant chat conversations: %w", err)
	}
	defer rows.Close()
	items := make([]app.InstantChatConversation, 0)
	for rows.Next() {
		var item app.InstantChatConversation
		if err := rows.Scan(&item.ID, &item.OwnerID, &item.ModelID, &item.Title, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan instant chat conversation: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetInstantChatConversation(ctx context.Context, ownerID, id string) (app.InstantChatConversation, error) {
	var item app.InstantChatConversation
	err := s.pool.QueryRow(ctx, `SELECT id, owner_id, model_id, title, created_at, updated_at
		FROM conversations WHERE owner_id = $1 AND id = $2`, ownerID, id).
		Scan(&item.ID, &item.OwnerID, &item.ModelID, &item.Title, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		return app.InstantChatConversation{}, storeError("get instant chat conversation", "conversation", err)
	}
	return item, nil
}

func (s *Store) ListInstantChatMessages(ctx context.Context, ownerID, conversationID string) ([]app.InstantChatMessage, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, conversation_id, role, content, created_at
		FROM conversation_messages WHERE owner_id=$1 AND conversation_id=$2 ORDER BY sequence`, ownerID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("list instant chat messages: %w", err)
	}
	defer rows.Close()
	items := make([]app.InstantChatMessage, 0)
	for rows.Next() {
		var item app.InstantChatMessage
		if err := rows.Scan(&item.ID, &item.ConversationID, &item.Role, &item.Content, &item.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan instant chat message: %w", err)
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) GetInstantChatExchangeByKey(ctx context.Context, ownerID, key string) (app.InstantChatConversation, app.InstantChatExchange, error) {
	var conversation app.InstantChatConversation
	var exchange app.InstantChatExchange
	err := s.pool.QueryRow(ctx, `SELECT c.id, c.owner_id, c.model_id, c.title, c.created_at, c.updated_at,
		u.source_exchange_id, u.conversation_id, u.requested_conversation_id, u.owner_id, k.key, u.id, a.id,
		u.content, a.content, u.created_at, u.model_id
		FROM idempotency_keys k
		JOIN conversation_messages u ON u.id=k.resource_id AND u.owner_id=k.owner_id AND u.role='user'
		JOIN conversation_messages a ON a.id=k.related_id AND a.owner_id=k.owner_id AND a.conversation_id=u.conversation_id AND a.role='assistant'
		JOIN conversations c ON c.id=u.conversation_id AND c.owner_id=u.owner_id
		WHERE k.operation='instant_chat.send' AND k.owner_id=$1 AND k.key=$2`, ownerID, key).
		Scan(&conversation.ID, &conversation.OwnerID, &conversation.ModelID, &conversation.Title, &conversation.CreatedAt, &conversation.UpdatedAt,
			&exchange.ID, &exchange.ConversationID, &exchange.RequestedConversationID, &exchange.OwnerID, &exchange.RequestKey, &exchange.UserMessageID,
			&exchange.AssistantMessageID, &exchange.UserContent, &exchange.AssistantContent, &exchange.CreatedAt, &exchange.ModelID)
	if err != nil {
		return app.InstantChatConversation{}, app.InstantChatExchange{}, storeError("get instant chat exchange", "exchange", err)
	}
	return conversation, exchange, nil
}

func (s *Store) SaveInstantChatExchange(ctx context.Context, chat app.InstantChatConversation, exchange app.InstantChatExchange) (app.InstantChatConversation, app.InstantChatExchange, error) {
	if !domain.ValidConversationID(chat.ID) || !domain.ValidUserID(chat.OwnerID) || chat.ID != exchange.ConversationID ||
		chat.OwnerID != exchange.OwnerID || !domain.ValidMessageID(exchange.ID) || !domain.ValidMessageID(exchange.UserMessageID) ||
		!domain.ValidMessageID(exchange.AssistantMessageID) || exchange.UserMessageID == exchange.AssistantMessageID ||
		(exchange.RequestedConversationID != "" && exchange.RequestedConversationID != chat.ID) ||
		exchange.RequestKey == "" || exchange.ModelID == "" {
		return app.InstantChatConversation{}, app.InstantChatExchange{}, domain.InvalidArgument("exchange", "instant chat exchange fields are invalid")
	}
	// Conversation creation and both messages are committed together, so a failed model response leaves no partial chat turn.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return app.InstantChatConversation{}, app.InstantChatExchange{}, fmt.Errorf("begin instant chat exchange: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	hash := sha256.Sum256([]byte(exchange.UserContent + "\n" + exchange.ModelID + "\n" + exchange.RequestedConversationID))
	_, replayed, err := reserveIdempotency(ctx, tx, app.IdempotencyRecord{OwnerID: chat.OwnerID, Operation: "instant_chat.send",
		Key: exchange.RequestKey, RequestHash: hash[:]}, exchange.UserMessageID, exchange.AssistantMessageID)
	if err != nil {
		return app.InstantChatConversation{}, app.InstantChatExchange{}, err
	}
	if replayed {
		return app.InstantChatConversation{}, app.InstantChatExchange{}, domain.NewError(domain.CodeIdempotencyConflict, "duplicate instant chat request")
	}
	if exchange.RequestedConversationID == "" {
		_, err = tx.Exec(ctx, `INSERT INTO conversations (id, owner_id, host_id, model_id, title, source_kind)
			VALUES ($1, $2, NULL, $3, $4, 'instant')`, chat.ID, chat.OwnerID, exchange.ModelID, chat.Title)
		if err != nil {
			return app.InstantChatConversation{}, app.InstantChatExchange{}, fmt.Errorf("create instant chat conversation: %w", err)
		}
	}
	err = tx.QueryRow(ctx, `SELECT id, owner_id, model_id, title, created_at, updated_at
		FROM conversations WHERE id = $1 AND owner_id = $2 FOR UPDATE`, chat.ID, chat.OwnerID).
		Scan(&chat.ID, &chat.OwnerID, &chat.ModelID, &chat.Title, &chat.CreatedAt, &chat.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.InstantChatConversation{}, app.InstantChatExchange{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	if err != nil {
		return app.InstantChatConversation{}, app.InstantChatExchange{}, fmt.Errorf("lock instant chat conversation: %w", err)
	}
	for _, item := range []conversation.Message{
		{ID: exchange.UserMessageID, Role: conversation.RoleUser, Content: exchange.UserContent},
		{ID: exchange.AssistantMessageID, Role: conversation.RoleAssistant, Content: exchange.AssistantContent},
	} {
		item.ConversationID, item.OwnerID, item.ModelID = chat.ID, chat.OwnerID, exchange.ModelID
		item.SourceKind, item.SourceExchangeID, item.RequestedConversationID = "instant", exchange.ID, exchange.RequestedConversationID
		if err := validateMessage(item); err != nil {
			return app.InstantChatConversation{}, app.InstantChatExchange{}, err
		}
		if err := insertMessage(ctx, tx, item); err != nil {
			return app.InstantChatConversation{}, app.InstantChatExchange{}, fmt.Errorf("save instant chat message: %w", err)
		}
	}
	err = tx.QueryRow(ctx, `UPDATE conversations SET model_id=$3, updated_at=clock_timestamp()
		WHERE id=$1 AND owner_id=$2 RETURNING updated_at`, chat.ID, chat.OwnerID, exchange.ModelID).Scan(&chat.UpdatedAt)
	if err != nil {
		return app.InstantChatConversation{}, app.InstantChatExchange{}, fmt.Errorf("update instant chat conversation: %w", err)
	}
	chat.ModelID = exchange.ModelID
	err = tx.QueryRow(ctx, `SELECT created_at FROM conversation_messages WHERE id=$1 AND owner_id=$2`, exchange.UserMessageID, chat.OwnerID).Scan(&exchange.CreatedAt)
	if err != nil {
		return app.InstantChatConversation{}, app.InstantChatExchange{}, fmt.Errorf("read instant chat exchange: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return app.InstantChatConversation{}, app.InstantChatExchange{}, fmt.Errorf("commit instant chat exchange: %w", err)
	}
	return chat, exchange, nil
}

var _ app.InstantChatStore = (*Store)(nil)
