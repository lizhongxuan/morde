package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
)

type InstantChatConversation struct {
	ID        string
	OwnerID   string
	ModelID   string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type InstantChatMessage struct {
	ID             string
	ConversationID string
	Role           string
	Content        string
	CreatedAt      time.Time
}

type InstantChatExchange struct {
	ID                      string
	ConversationID          string
	RequestedConversationID string
	OwnerID                 string
	ModelID                 string
	RequestKey              string
	UserMessageID           string
	AssistantMessageID      string
	UserContent             string
	AssistantContent        string
	CreatedAt               time.Time
}

type InstantChatStore interface {
	ListInstantChatConversations(context.Context, string) ([]InstantChatConversation, error)
	GetInstantChatConversation(context.Context, string, string) (InstantChatConversation, error)
	ListInstantChatMessages(context.Context, string, string) ([]InstantChatMessage, error)
	GetInstantChatExchangeByKey(context.Context, string, string) (InstantChatConversation, InstantChatExchange, error)
	SaveInstantChatExchange(context.Context, InstantChatConversation, InstantChatExchange) (InstantChatConversation, InstantChatExchange, error)
}

type InstantChatModelGateway interface {
	Complete(context.Context, ModelCall) (ModelResponse, error)
}

type InstantChatService struct {
	store  InstantChatStore
	models InstantChatModelGateway
}

func NewInstantChatService(store InstantChatStore, models InstantChatModelGateway) (*InstantChatService, error) {
	if store == nil || models == nil {
		return nil, fmt.Errorf("instant chat dependencies are required")
	}
	return &InstantChatService{store: store, models: models}, nil
}

func (s *InstantChatService) List(ctx context.Context, principal identity.Principal) ([]InstantChatConversation, error) {
	if err := validateInstantChatPrincipal(principal); err != nil {
		return nil, err
	}
	return s.store.ListInstantChatConversations(ctx, principal.UserID)
}

func (s *InstantChatService) Messages(ctx context.Context, principal identity.Principal, conversationID string) ([]InstantChatMessage, error) {
	if err := validateInstantChatPrincipal(principal); err != nil {
		return nil, err
	}
	if !domain.ValidConversationID(conversationID) {
		return nil, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	if _, err := s.store.GetInstantChatConversation(ctx, principal.UserID, conversationID); err != nil {
		return nil, err
	}
	return s.store.ListInstantChatMessages(ctx, principal.UserID, conversationID)
}

type InstantChatSend struct {
	ConversationID string
	ModelID        string
	Content        string
	RequestKey     string
}

type InstantChatReply struct {
	Conversation InstantChatConversation
	Message      InstantChatMessage
	Reply        InstantChatMessage
}

func (s *InstantChatService) Send(ctx context.Context, principal identity.Principal, command InstantChatSend) (InstantChatReply, error) {
	if err := validateInstantChatPrincipal(principal); err != nil {
		return InstantChatReply{}, err
	}
	content := strings.TrimSpace(command.Content)
	if content == "" || utf8.RuneCountInString(content) > 32768 {
		return InstantChatReply{}, domain.InvalidArgument("content", "content must be 1 to 32768 characters")
	}
	if !domain.ValidModelConnectionID(command.ModelID) {
		return InstantChatReply{}, domain.InvalidArgument("model_id", "invalid model connection")
	}
	if command.ConversationID != "" && !domain.ValidConversationID(command.ConversationID) {
		return InstantChatReply{}, domain.InvalidArgument("conversation_id", "invalid conversation")
	}
	if len(command.RequestKey) < 8 || len(command.RequestKey) > 128 || !validInstantChatKey(command.RequestKey) {
		return InstantChatReply{}, domain.InvalidArgument("Idempotency-Key", "invalid idempotency key")
	}

	// shortcut: concurrent retries may call the model twice before one exchange commits; add an in-flight lease if duplicate billing becomes material.
	priorConversation, priorExchange, err := s.store.GetInstantChatExchangeByKey(ctx, principal.UserID, command.RequestKey)
	if err == nil {
		return matchInstantChatExchange(priorConversation, priorExchange, command, content)
	}
	if domain.CodeOf(err) != domain.CodeNotFound {
		return InstantChatReply{}, err
	}

	conversation := InstantChatConversation{ID: command.ConversationID, OwnerID: principal.UserID, ModelID: command.ModelID}
	var history []InstantChatMessage
	if conversation.ID != "" {
		conversation, err = s.store.GetInstantChatConversation(ctx, principal.UserID, conversation.ID)
		if err != nil {
			return InstantChatReply{}, err
		}
		history, err = s.store.ListInstantChatMessages(ctx, principal.UserID, conversation.ID)
		if err != nil {
			return InstantChatReply{}, err
		}
	} else {
		conversation.ID, err = domain.NewConversationID()
		if err != nil {
			return InstantChatReply{}, fmt.Errorf("generate instant chat conversation id: %w", err)
		}
		title := []rune(content)
		if len(title) > 40 {
			title = title[:40]
		}
		conversation.Title = string(title)
	}
	input := make([]ModelInput, 0, 41)
	if len(history) > 40 {
		history = history[len(history)-40:]
	}
	for _, message := range history {
		input = append(input, ModelInput{Type: "message", Role: message.Role, Text: message.Content})
	}
	input = append(input, ModelInput{Type: "message", Role: "user", Text: content})
	response, err := s.models.Complete(ctx, ModelCall{Source: ModelCallChat, Principal: principal, ModelID: command.ModelID,
		CallID: instantChatModelCallID(principal.UserID, command.RequestKey), Request: ModelRequest{
			Instructions: "You are Morde's helpful chat assistant. Answer the user directly. You do not have access to their cloud computer or files.",
			Input:        input, MaxOutputTokens: 2048,
		}})
	if err != nil {
		return InstantChatReply{}, err
	}
	answer := strings.TrimSpace(response.Text)
	if answer == "" || len(response.ToolCalls) != 0 {
		return InstantChatReply{}, domain.NewError(domain.CodeUpstreamError, "model returned no chat response")
	}
	conversation.ModelID = command.ModelID
	exchange := InstantChatExchange{ConversationID: conversation.ID, RequestedConversationID: command.ConversationID, OwnerID: principal.UserID, ModelID: command.ModelID, RequestKey: command.RequestKey,
		UserContent: content, AssistantContent: answer}
	exchange.ID, err = domain.NewMessageID()
	if err != nil {
		return InstantChatReply{}, fmt.Errorf("generate instant chat exchange id: %w", err)
	}
	exchange.UserMessageID, err = domain.NewMessageID()
	if err != nil {
		return InstantChatReply{}, fmt.Errorf("generate instant chat user message id: %w", err)
	}
	exchange.AssistantMessageID, err = domain.NewMessageID()
	if err != nil {
		return InstantChatReply{}, fmt.Errorf("generate instant chat assistant message id: %w", err)
	}
	conversation, exchange, err = s.store.SaveInstantChatExchange(ctx, conversation, exchange)
	if err != nil {
		if domain.CodeOf(err) != domain.CodeIdempotencyConflict {
			return InstantChatReply{}, err
		}
		priorConversation, priorExchange, lookupErr := s.store.GetInstantChatExchangeByKey(ctx, principal.UserID, command.RequestKey)
		if lookupErr != nil {
			return InstantChatReply{}, lookupErr
		}
		return matchInstantChatExchange(priorConversation, priorExchange, command, content)
	}
	messages := exchangeMessages(exchange)
	return InstantChatReply{Conversation: conversation, Message: messages[0], Reply: messages[1]}, nil
}

func instantChatModelCallID(ownerID, requestKey string) string {
	digest := sha256.Sum256([]byte(ownerID + "\n" + requestKey))
	return "chat_" + hex.EncodeToString(digest[:])
}

func matchInstantChatExchange(conversation InstantChatConversation, exchange InstantChatExchange, command InstantChatSend, content string) (InstantChatReply, error) {
	if exchange.UserContent != content || exchange.ModelID != command.ModelID || exchange.RequestedConversationID != command.ConversationID {
		return InstantChatReply{}, domain.NewError(domain.CodeIdempotencyConflict, "idempotency key reused with different request")
	}
	messages := exchangeMessages(exchange)
	return InstantChatReply{Conversation: conversation, Message: messages[0], Reply: messages[1]}, nil
}

func exchangeMessages(exchange InstantChatExchange) []InstantChatMessage {
	return []InstantChatMessage{
		{ID: exchange.UserMessageID, ConversationID: exchange.ConversationID, Role: "user", Content: exchange.UserContent, CreatedAt: exchange.CreatedAt},
		{ID: exchange.AssistantMessageID, ConversationID: exchange.ConversationID, Role: "assistant", Content: exchange.AssistantContent, CreatedAt: exchange.CreatedAt},
	}
}

func validateInstantChatPrincipal(principal identity.Principal) error {
	if principal.Kind != identity.KindSession || !domain.ValidUserID(principal.UserID) ||
		(principal.Role != identity.RoleUser && !principal.Role.IsAdmin()) || principal.HostScope != "" ||
		len(principal.HostIDs) != 0 || len(principal.Scopes) != 0 {
		return domain.NewError(domain.CodeForbidden, "portal session required")
	}
	return nil
}

func validInstantChatKey(key string) bool {
	for _, char := range key {
		if (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '.' && char != '_' && char != '~' && char != '-' {
			return false
		}
	}
	return true
}
