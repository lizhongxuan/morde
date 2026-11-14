package app

import (
	"context"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type ConversationMessageVariantAction string

const (
	ConversationMessageVariantEditResend     ConversationMessageVariantAction = "edit_resend"
	ConversationMessageVariantRegenerateText ConversationMessageVariantAction = "regenerate_text"
	ConversationMessageVariantContinue       ConversationMessageVariantAction = "continue_generation"
)

type CreateConversationMessageVariantCommand struct {
	ProjectID         string
	ConversationID    string
	SourceMessageID   string
	ServiceInstanceID string
	Action            ConversationMessageVariantAction
	ExpectedVersion   int64
	Content           string
	IdempotencyKey    string
	OperationID       string
	UserMessageID     string
	RequestHash       []byte
	Authorization     task.Authorization
}

type ConversationMessageVariantOperation struct {
	OperationID        string
	VariantGroupID     string
	VariantVersion     int64
	Action             ConversationMessageVariantAction
	State              string
	TaskID             string
	UserMessageID      string
	AssistantMessageID string
	Replayed           bool
}

type ConversationMessageVariantStore interface {
	CreateConversationMessageVariant(context.Context, identity.Principal, CreateConversationMessageVariantCommand) (ConversationMessageVariantOperation, error)
	SetConversationMessageVariantState(context.Context, identity.Principal, string, string) error
}

func (s *TaskService) CreateConversationMessageVariant(ctx context.Context, principal identity.Principal, projectID, conversationID, sourceMessageID, serviceInstanceID string,
	action ConversationMessageVariantAction, expectedVersion int64, content, idempotencyKey string) (ConversationMessageVariantOperation, error) {
	if err := validateConversationPrincipal(principal, identity.ScopeTaskSubmit); err != nil {
		return ConversationMessageVariantOperation{}, err
	}
	if (projectID != "" && !project.ValidID(projectID)) || !domain.ValidConversationID(conversationID) || !domain.ValidMessageID(sourceMessageID) {
		return ConversationMessageVariantOperation{}, domain.NewError(domain.CodeNotFound, "conversation message not found")
	}
	if !domain.ValidServiceInstanceID(serviceInstanceID) || expectedVersion < 1 || validateIdempotencyKey(idempotencyKey) != nil {
		return ConversationMessageVariantOperation{}, domain.InvalidArgument("variant", "conversation message variant identity is invalid")
	}
	switch action {
	case ConversationMessageVariantEditResend:
		if !utf8.ValidString(content) || strings.ContainsRune(content, '\x00') || utf8.RuneCountInString(content) > 32768 || strings.TrimSpace(content) == "" {
			return ConversationMessageVariantOperation{}, domain.InvalidArgument("content", "replacement message text is required")
		}
	case ConversationMessageVariantRegenerateText, ConversationMessageVariantContinue:
		if content != "" {
			return ConversationMessageVariantOperation{}, domain.InvalidArgument("content", "content is only valid for edit_resend")
		}
	default:
		return ConversationMessageVariantOperation{}, domain.InvalidArgument("action", "conversation message variant action is invalid")
	}
	if projectID != "" {
		if s.projectAccess == nil {
			return ConversationMessageVariantOperation{}, domain.NewError(domain.CodeUnsupported, "project conversation access is unavailable")
		}
		if _, err := s.projectAccess.RequireAccess(ctx, principal, projectID, project.RoleMember); err != nil {
			return ConversationMessageVariantOperation{}, err
		}
		if _, err := s.store.GetProjectConversation(ctx, principal.UserID, projectID, conversationID); err != nil {
			return ConversationMessageVariantOperation{}, err
		}
	} else if _, err := s.store.GetConversation(ctx, conversationID, principal.UserID); err != nil {
		return ConversationMessageVariantOperation{}, err
	}
	store, ok := s.store.(ConversationMessageVariantStore)
	if !ok {
		return ConversationMessageVariantOperation{}, domain.NewError(domain.CodeUnsupported, "conversation message variants are unavailable")
	}
	if action == ConversationMessageVariantContinue && s.actions == nil {
		return ConversationMessageVariantOperation{}, domain.NewError(domain.CodeUnsupported, "task actions are unavailable")
	}
	operationID, err := domain.NewOperationID()
	if err != nil {
		return ConversationMessageVariantOperation{}, err
	}
	userMessageID := ""
	if action == ConversationMessageVariantEditResend || action == ConversationMessageVariantRegenerateText {
		userMessageID, err = domain.NewMessageID()
		if err != nil {
			return ConversationMessageVariantOperation{}, err
		}
	}
	authorization, err := s.authorizeTask(ctx, principal, "", identity.ScopeTaskSubmit)
	if err != nil {
		return ConversationMessageVariantOperation{}, err
	}
	requestHash, err := hashIdempotencyRequest(struct {
		ProjectID       string                           `json:"project_id,omitempty"`
		ConversationID  string                           `json:"conversation_id"`
		SourceMessageID string                           `json:"source_message_id"`
		Action          ConversationMessageVariantAction `json:"action"`
		ExpectedVersion int64                            `json:"expected_version"`
		Content         string                           `json:"content,omitempty"`
	}{projectID, conversationID, sourceMessageID, action, expectedVersion, content})
	if err != nil {
		return ConversationMessageVariantOperation{}, err
	}
	operation, err := store.CreateConversationMessageVariant(ctx, principal, CreateConversationMessageVariantCommand{
		ProjectID: projectID, ConversationID: conversationID, SourceMessageID: sourceMessageID,
		ServiceInstanceID: serviceInstanceID, Action: action, ExpectedVersion: expectedVersion,
		Content: content, IdempotencyKey: idempotencyKey, OperationID: operationID,
		UserMessageID: userMessageID, RequestHash: requestHash, Authorization: authorization,
	})
	if err != nil {
		return ConversationMessageVariantOperation{}, err
	}
	if action == ConversationMessageVariantContinue && operation.State != "completed" && operation.State != "failed" {
		markResumeUnknown := func() (ConversationMessageVariantOperation, error) {
			operation.State = "unknown"
			// The variant row is already durable. A failed or ambiguous Task
			// resume must be returned as an accepted operation so the caller keeps
			// its idempotency key and can reconcile this exact operation.
			_ = store.SetConversationMessageVariantState(ctx, principal, operation.OperationID, "unknown")
			return operation, nil
		}
		if operation.TaskID == "" {
			return markResumeUnknown()
		}
		details, getErr := s.store.GetTask(ctx, operation.TaskID, principal.UserID)
		if getErr != nil || details.Task.CurrentRevision == nil || details.Task.DesiredRevision == nil {
			return markResumeUnknown()
		}
		fence := &TaskActionRevisionFence{CurrentRevision: *details.Task.CurrentRevision, DesiredRevision: *details.Task.DesiredRevision}
		resumeKey := "variant-resume-" + operation.OperationID
		if _, resumeErr := s.RequestActionWithRevisionFence(ctx, principal, operation.TaskID, task.ActionResume,
			task.StatusPaused, fence, resumeKey); resumeErr != nil {
			return markResumeUnknown()
		}
	}
	return operation, nil
}
