package app

import (
	"context"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
)

// ConversationMessageRetryEligibility is a read-only preflight for the
// same-message retry action. It is advisory because the POST must recheck the
// same facts in its write transaction.
type ConversationMessageRetryEligibility struct {
	Eligible bool   `json:"retry_eligible"`
	Reason   string `json:"retry_reason"`
	Warning  string `json:"retry_warning,omitempty"`
}

const (
	ConversationMessageRetryReasonAvailable               = "retry_available"
	ConversationMessageRetryReasonNotFailed               = "route_not_failed"
	ConversationMessageRetryReasonTaskAttached            = "task_already_created"
	ConversationMessageRetryReasonMessageTooLong          = "message_too_long"
	ConversationMessageRetryReasonReconciliationRequired  = "reconciliation_required"
	ConversationMessageRetryReasonProcessing              = "route_processing"
	ConversationMessageRetryReasonReviewNotRetryable      = "result_review_not_retryable"
	ConversationMessageRetryReasonReviewOutcomeUnknown    = "result_review_outcome_unknown"
	ConversationMessageRetryReasonLimitReached            = "retry_limit_reached"
	ConversationMessageRetryWarningPersonalOutcomeUnknown = "previous_personal_model_call_outcome_unknown"
)

type ConversationMessageRetryStore interface {
	RetryConversationRoute(context.Context, identity.Principal, string, string, string, string, string) error
}

type ConversationMessageRetryEligibilityStore interface {
	GetConversationMessageRetryEligibility(context.Context, identity.Principal, string, string, string, string) (ConversationMessageRetryEligibility, error)
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

func (s *TaskService) GetConversationMessageRetryEligibility(ctx context.Context, principal identity.Principal,
	projectID, conversationID, messageID, instanceID string) (ConversationMessageRetryEligibility, error) {
	if err := validateConversationPrincipal(principal, identity.ScopeTaskSubmit); err != nil {
		return ConversationMessageRetryEligibility{}, err
	}
	if (projectID != "" && !project.ValidID(projectID)) || !domain.ValidConversationID(conversationID) || !domain.ValidMessageID(messageID) {
		return ConversationMessageRetryEligibility{}, domain.NewError(domain.CodeNotFound, "conversation message not found")
	}
	if !domain.ValidServiceInstanceID(instanceID) {
		return ConversationMessageRetryEligibility{}, domain.InvalidArgument("retry", "message retry identity is invalid")
	}
	store, ok := s.store.(ConversationMessageRetryEligibilityStore)
	if !ok {
		return ConversationMessageRetryEligibility{}, domain.NewError(domain.CodeUnsupported, "message processing retry eligibility is unavailable")
	}
	return store.GetConversationMessageRetryEligibility(ctx, principal, projectID, conversationID, messageID, instanceID)
}
