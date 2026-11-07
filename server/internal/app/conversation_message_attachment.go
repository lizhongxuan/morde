package app

import (
	"context"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
)

type ConversationMessageAttachmentStore interface {
	AssociateConversationMessageAttachment(context.Context, identity.Principal, string, string, string, string, string) error
}

func (s *TaskService) AssociateConversationMessageAttachment(ctx context.Context, principal identity.Principal, conversationID, messageID, clientAttachmentID, attachmentID, instanceID string) error {
	if err := validateConversationPrincipal(principal, identity.ScopeTaskSubmit); err != nil {
		return err
	}
	if !domain.ValidConversationID(conversationID) || !domain.ValidMessageID(messageID) {
		return domain.NewError(domain.CodeNotFound, "conversation message not found")
	}
	if !idempotencyKeyPattern.MatchString(clientAttachmentID) || !personal.ValidPersonalAttachmentID(attachmentID) || !domain.ValidServiceInstanceID(instanceID) {
		return domain.InvalidArgument("attachment", "conversation attachment identity is invalid")
	}
	store, ok := s.store.(ConversationMessageAttachmentStore)
	if !ok {
		return domain.NewError(domain.CodeUnsupported, "conversation attachment association is unavailable")
	}
	return store.AssociateConversationMessageAttachment(ctx, principal, conversationID, messageID, clientAttachmentID, attachmentID, instanceID)
}
