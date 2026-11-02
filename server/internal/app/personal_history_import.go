package app

import (
	"context"
	"errors"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
)

type PersonalHistoryImportStore interface {
	ImportPersonalHistory(context.Context, identity.Principal, conversation.HistoryImport) (conversation.HistoryImportResult, error)
}

type PersonalHistoryImportService struct {
	store PersonalHistoryImportStore
}

func NewPersonalHistoryImportService(store PersonalHistoryImportStore) (*PersonalHistoryImportService, error) {
	if store == nil {
		return nil, errors.New("personal history import store is required")
	}
	return &PersonalHistoryImportService{store: store}, nil
}

func (s *PersonalHistoryImportService) Import(ctx context.Context, principal identity.Principal, input conversation.HistoryImport) (conversation.HistoryImportResult, error) {
	if err := validatePersonalHistoryImportPrincipal(principal); err != nil {
		return conversation.HistoryImportResult{}, err
	}
	if err := conversation.ValidateHistoryImport(input); err != nil {
		return conversation.HistoryImportResult{}, err
	}
	result, err := s.store.ImportPersonalHistory(ctx, principal, input)
	if err != nil {
		return conversation.HistoryImportResult{}, err
	}
	if !domain.ValidConversationID(result.Conversation.ID) || result.Conversation.OwnerID != principal.UserID ||
		result.Conversation.HostID != "" ||
		len(result.Messages) != len(input.Messages) {
		return conversation.HistoryImportResult{}, domain.NewError(domain.CodeInternal, "history import receipt is invalid")
	}
	for i, imported := range result.Messages {
		message := imported.Message
		if imported.SourceMessageID != input.Messages[i].SourceMessageID || !domain.ValidMessageID(message.ID) ||
			message.ConversationID != result.Conversation.ID || message.OwnerID != principal.UserID ||
			message.Role != input.Messages[i].Role || message.Content != input.Messages[i].Content || message.TaskID != "" ||
			message.SourceKind != conversation.HistoryImportSourceKind || message.Sequence < 1 {
			return conversation.HistoryImportResult{}, domain.NewError(domain.CodeInternal, "history import message receipt is invalid")
		}
		if len(imported.AttachmentIDs) != len(input.Messages[i].AttachmentIDs) {
			return conversation.HistoryImportResult{}, domain.NewError(domain.CodeInternal, "history import attachment receipt is invalid")
		}
		for attachmentIndex, attachmentID := range imported.AttachmentIDs {
			if attachmentID != input.Messages[i].AttachmentIDs[attachmentIndex] {
				return conversation.HistoryImportResult{}, domain.NewError(domain.CodeInternal, "history import attachment receipt is invalid")
			}
		}
	}
	return result, nil
}

func validatePersonalHistoryImportPrincipal(principal identity.Principal) error {
	if principal.Kind != identity.KindSession && principal.Kind != identity.KindClient {
		return domain.NewError(domain.CodeForbidden, "portal session or client credential required")
	}
	if err := validatePrincipal(principal); err != nil {
		return err
	}
	if !domain.ValidTokenID(principal.TokenID) || !principal.HasScope(identity.ScopeTaskSubmit) {
		return domain.NewError(domain.CodeForbidden, "history import is not authorized")
	}
	return nil
}
