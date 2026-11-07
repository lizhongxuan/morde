package app

import (
	"context"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

type ConversationMessageRetryStore interface {
	RetryConversationRoute(context.Context, identity.Principal, string, string, string, string, string) error
}

func (s *TaskService) RetryConversationMessageProcessing(ctx context.Context, principal identity.Principal, projectID, conversationID, messageID, instanceID, key string) error {
	if err := validateConversationPrincipal(principal, identity.ScopeTaskSubmit); err != nil {
		return err
	}
	if (projectID != "" && !project.ValidID(projectID)) || !domain.ValidConversationID(conversationID) || !domain.ValidMessageID(messageID) {
		return domain.NewError(domain.CodeNotFound, "conversation message not found")
	}
	if !domain.ValidServiceInstanceID(instanceID) || !idempotencyKeyPattern.MatchString(key) {
		return domain.InvalidArgument("retry", "message retry identity is invalid")
	}
	store, ok := s.store.(ConversationMessageRetryStore)
	if !ok {
		return domain.NewError(domain.CodeUnsupported, "message processing retry is unavailable")
	}
	return store.RetryConversationRoute(ctx, principal, projectID, conversationID, messageID, instanceID, key)
}
