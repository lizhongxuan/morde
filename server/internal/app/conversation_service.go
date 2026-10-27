package app

import (
	"context"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
)

// CreateConversationCommand creates a conversation independently of any later task.
type CreateConversationCommand struct {
	HostID         string
	Title          string
	IdempotencyKey string
}

type ConversationService struct {
	store      TaskStore
	authorizer TaskAuthorizer
}

func NewConversationService(store TaskStore, authorizer TaskAuthorizer) (*ConversationService, error) {
	if store == nil || authorizer == nil {
		return nil, domain.NewError(domain.CodeInvalidArgument, "conversation dependencies are required")
	}
	return &ConversationService{store: store, authorizer: authorizer}, nil
}

func (s *ConversationService) Create(ctx context.Context, principal identity.Principal, command CreateConversationCommand) (conversation.Conversation, bool, error) {
	if err := validateConversationPrincipal(principal, identity.ScopeTaskSubmit); err != nil {
		return conversation.Conversation{}, false, err
	}
	command.Title = strings.TrimSpace(command.Title)
	if !utf8.ValidString(command.Title) || utf8.RuneCountInString(command.Title) > 120 {
		return conversation.Conversation{}, false, domain.InvalidArgument("title", "title is invalid")
	}
	if err := validateIdempotencyKey(command.IdempotencyKey); err != nil {
		return conversation.Conversation{}, false, err
	}
	if command.HostID != "" {
		if _, err := authorizeTaskHost(ctx, s.authorizer, principal, command.HostID, identity.ScopeTaskSubmit); err != nil {
			return conversation.Conversation{}, false, err
		}
	}
	id, err := domain.NewConversationID()
	if err != nil {
		return conversation.Conversation{}, false, err
	}
	record, err := idempotencyRecord(principal.UserID, "conversation.create", command.IdempotencyKey, struct {
		HostID string `json:"host_id"`
		Title  string `json:"title"`
	}{HostID: command.HostID, Title: command.Title}, id, "")
	if err != nil {
		return conversation.Conversation{}, false, err
	}
	return s.store.CreateConversation(ctx, conversation.Conversation{
		ID: id, OwnerID: principal.UserID, HostID: command.HostID, Title: command.Title,
	}, record)
}

func (s *ConversationService) List(ctx context.Context, principal identity.Principal, page PageRequest) (ConversationPage, error) {
	if err := validateConversationPrincipal(principal, identity.ScopeTaskRead); err != nil {
		return ConversationPage{}, err
	}
	if err := validatePageRequest(page); err != nil {
		return ConversationPage{}, err
	}
	if page.Limit == 0 {
		page.Limit = defaultPageSize
	}
	return s.store.ListConversations(ctx, principal.UserID, nil, page)
}

func (s *ConversationService) Get(ctx context.Context, principal identity.Principal, id string) (conversation.Conversation, error) {
	if err := validateConversationPrincipal(principal, identity.ScopeTaskRead); err != nil {
		return conversation.Conversation{}, err
	}
	if !domain.ValidConversationID(id) {
		return conversation.Conversation{}, domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	return s.store.GetConversation(ctx, id, principal.UserID)
}

func (s *ConversationService) ListMessages(ctx context.Context, principal identity.Principal, conversationID string, page PageRequest) (MessagePage, error) {
	if err := validatePageRequest(page); err != nil {
		return MessagePage{}, err
	}
	if _, err := s.Get(ctx, principal, conversationID); err != nil {
		return MessagePage{}, err
	}
	if page.Limit == 0 {
		page.Limit = defaultPageSize
	}
	result, err := s.store.ListMessages(ctx, principal.UserID, conversationID, page)
	if err != nil {
		return MessagePage{}, err
	}
	for left, right := 0, len(result.Items)-1; left < right; left, right = left+1, right-1 {
		result.Items[left], result.Items[right] = result.Items[right], result.Items[left]
	}
	return result, nil
}

func validateConversationPrincipal(principal identity.Principal, scope identity.Scope) error {
	if err := validateTaskPrincipal(principal); err != nil {
		return err
	}
	if !domain.ValidTokenID(principal.TokenID) || !principal.HasScope(scope) {
		return domain.NewError(domain.CodeForbidden, "conversation access is not authorized")
	}
	return nil
}
