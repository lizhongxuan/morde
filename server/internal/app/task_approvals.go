package app

import (
	"context"
	"math"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

// ListTaskApprovals returns only requests the current principal may inspect.
// PendingCount is computed from the complete query scope, not the current page.
func (s *TaskService) ListTaskApprovals(ctx context.Context, principal identity.Principal, conversationID string, page PageRequest) (TaskApprovalPage, error) {
	if err := validateTaskPrincipal(principal); err != nil {
		return TaskApprovalPage{}, err
	}
	if conversationID != "" && !domain.ValidConversationID(conversationID) {
		return TaskApprovalPage{}, domain.InvalidArgument("conversation_id", "conversation id is invalid")
	}
	if err := validatePageRequest(page); err != nil {
		return TaskApprovalPage{}, err
	}
	if page.Limit == 0 {
		page.Limit = defaultPageSize
	}
	store, ok := s.store.(TaskApprovalQueryStore)
	if !ok {
		return TaskApprovalPage{}, domain.NewError(domain.CodeInternal, "task approval query storage is unavailable")
	}
	hostIDs, err := authorizedTaskApprovalHostIDs(principal)
	if err != nil {
		return TaskApprovalPage{}, err
	}
	result, err := store.ListTaskApprovals(ctx, principal.UserID, hostIDs, conversationID, page)
	if err != nil {
		return TaskApprovalPage{}, err
	}
	if result.Items == nil || len(result.Items) > int(page.Limit) || len(result.NextCursor) > 1024 {
		return TaskApprovalPage{}, domain.NewError(domain.CodeInternal, "task approval store returned an invalid page")
	}
	visible := make([]TaskApproval, 0, len(result.Items))
	for _, item := range result.Items {
		if err := validateTaskApproval(item); err != nil {
			return TaskApprovalPage{}, err
		}
		scope := taskControlScope
		if item.Request.Status != task.PendingRequestPending ||
			(item.Request.ExpiresAt != nil && !item.Request.ExpiresAt.After(time.Now())) {
			scope = taskReadScope
		}
		if _, err := s.authorizeTask(ctx, principal, item.HostID, scope); err != nil {
			switch domain.CodeOf(err) {
			case domain.CodeNotFound, domain.CodeForbidden:
				continue
			default:
				return TaskApprovalPage{}, err
			}
		}
		visible = append(visible, item)
	}
	result.Items = visible
	result.PendingCount = 0
	for hostID, count := range result.PendingCountsByHostID {
		if (hostID != "" && !domain.ValidHostID(hostID)) || count < 0 || result.PendingCount > math.MaxInt64-count {
			return TaskApprovalPage{}, domain.NewError(domain.CodeInternal, "task approval count is invalid")
		}
		if _, err := s.authorizeTask(ctx, principal, hostID, taskControlScope); err != nil {
			switch domain.CodeOf(err) {
			case domain.CodeNotFound, domain.CodeForbidden:
				continue
			default:
				return TaskApprovalPage{}, err
			}
		}
		result.PendingCount += count
	}
	result.PendingCountsByHostID = nil
	return result, nil
}

func authorizedTaskApprovalHostIDs(principal identity.Principal) ([]string, error) {
	if principal.Kind == identity.KindSession {
		// The service authorizes each returned Host by the approval's current state.
		return nil, nil
	}
	if !principal.HasScope(taskReadScope) && !principal.HasScope(taskControlScope) {
		return nil, domain.NewError(domain.CodeForbidden, "task read or control scope is required")
	}
	if principal.Kind == identity.KindClient {
		// Client Tasks are scoped by user and the service authorizes each returned Host.
		return nil, nil
	}
	if len(principal.HostIDs) == 0 {
		return nil, nil
	}
	hostIDs := make([]string, 0, len(principal.HostIDs))
	seen := make(map[string]struct{})
	for _, id := range principal.HostIDs {
		if !domain.ValidHostID(id) {
			return nil, domain.NewError(domain.CodeUnauthenticated, "authentication required")
		}
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			hostIDs = append(hostIDs, id)
		}
	}
	return hostIDs, nil
}

func validateTaskApproval(value TaskApproval) error {
	pending := value.Request
	validStatus := pending.Status == task.PendingRequestPending || pending.Status == task.PendingRequestApproved ||
		pending.Status == task.PendingRequestRejected || pending.Status == task.PendingRequestCancelled || pending.Status == task.PendingRequestExpired
	validDecision := (pending.Status == task.PendingRequestPending && pending.Decision == "" && pending.RespondedAt == nil) ||
		(pending.Status == task.PendingRequestApproved && pending.Decision == task.PendingDecisionApprove && pending.RespondedAt != nil) ||
		((pending.Status == task.PendingRequestRejected || pending.Status == task.PendingRequestExpired) && pending.Decision == task.PendingDecisionReject && pending.RespondedAt != nil) ||
		(pending.Status == task.PendingRequestCancelled && pending.Decision == "" && pending.RespondedAt != nil)
	if !domain.ValidTaskID(value.TaskID) || !domain.ValidConversationID(value.ConversationID) ||
		(value.ProjectID != "" && !project.ValidID(value.ProjectID)) ||
		(value.HostID != "" && !domain.ValidHostID(value.HostID)) || value.TaskEventSeq < 0 || value.TaskUpdatedAt.IsZero() ||
		!domain.ValidPendingRequestID(pending.ID) || pending.TaskID != value.TaskID || pending.Version < 1 || pending.Kind != "approval" ||
		!validStatus || !validDecision || pending.CreatedAt.IsZero() || (pending.ExpiresAt != nil && pending.ExpiresAt.IsZero()) ||
		!validBoundedText(pending.Title, 512) || !validBoundedText(pending.Object, 512) || !validBoundedText(pending.Content, 512) || !validBoundedText(pending.Impact, 512) ||
		strings.ContainsRune(pending.Title+pending.Object+pending.Content+pending.Impact, '\x00') {
		return domain.NewError(domain.CodeInternal, "task approval store returned an invalid request")
	}
	if pending.RespondedAt != nil && pending.RespondedAt.IsZero() {
		return domain.NewError(domain.CodeInternal, "task approval store returned an invalid response time")
	}
	return nil
}
