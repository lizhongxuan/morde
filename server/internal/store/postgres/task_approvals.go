package postgres

import (
	"context"
	"fmt"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
)

func (s *Store) ListTaskApprovals(ctx context.Context, actorID string, hostIDs []string, conversationID string, page app.PageRequest) (app.TaskApprovalPage, error) {
	if !domain.ValidUserID(actorID) || (conversationID != "" && !domain.ValidConversationID(conversationID)) {
		return app.TaskApprovalPage{}, domain.InvalidArgument("conversation_id", "task approval query is invalid")
	}
	for _, hostID := range hostIDs {
		if !domain.ValidHostID(hostID) {
			return app.TaskApprovalPage{}, domain.InvalidArgument("host_ids", "host id is invalid")
		}
	}
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.TaskApprovalPage{}, err
	}
	var rows []sqlcgen.ListTaskApprovalsRow
	pendingCountsByHostID := make(map[string]int64)
	err = s.withTaskSnapshot(ctx, func(tx pgx.Tx) error {
		queries := sqlcgen.New(tx)
		rows, err = queries.ListTaskApprovals(ctx, sqlcgen.ListTaskApprovalsParams{
			ActorID: actorID, HostIds: hostIDs, ConversationID: conversationID,
			CursorAt: cursorAt, CursorID: cursorID, PageLimit: limit + 1,
		})
		if err != nil {
			return err
		}
		countRows, err := queries.CountPendingTaskApprovalHosts(ctx, sqlcgen.CountPendingTaskApprovalHostsParams{
			ActorID: actorID, HostIds: hostIDs, ConversationID: conversationID,
		})
		if err != nil {
			return err
		}
		for _, row := range countRows {
			if (row.HostID != "" && !domain.ValidHostID(row.HostID)) || row.TaskCount < 0 {
				return domain.NewError(domain.CodeInternal, "task approval count is invalid")
			}
			pendingCountsByHostID[row.HostID] = row.TaskCount
		}
		return nil
	})
	if err != nil {
		return app.TaskApprovalPage{}, fmt.Errorf("list task approvals: %w", err)
	}
	items := make([]app.TaskApproval, 0, len(rows))
	for _, row := range rows {
		var expiresAt, respondedAt *time.Time
		if row.ExpiresAt.Valid {
			value := row.ExpiresAt.Time
			expiresAt = &value
		}
		if row.RespondedAt.Valid {
			value := row.RespondedAt.Time
			respondedAt = &value
		}
		if !row.CreatedAt.Valid || !row.TaskUpdatedAt.Valid {
			return app.TaskApprovalPage{}, domain.NewError(domain.CodeInternal, "task approval timestamps are invalid")
		}
		items = append(items, app.TaskApproval{
			TaskID: row.TaskID, ConversationID: row.ConversationID, ProjectID: row.ProjectID, HostID: row.HostID,
			TaskStatus: task.Status(row.TaskStatus), TaskEventSeq: row.TaskEventSeq, TaskUpdatedAt: row.TaskUpdatedAt.Time,
			Request: task.PendingRequest{
				ID: row.ID, TaskID: row.TaskID, Version: row.Version, Kind: row.Kind, Title: row.Title, Object: row.Object,
				Content: row.Content, Impact: row.Impact, MediaPreviewOperationID: row.MediaPreviewOperationID,
				Status: task.PendingRequestStatus(row.Status), Decision: task.PendingDecision(row.Decision),
				CreatedAt: row.CreatedAt.Time, ExpiresAt: expiresAt, RespondedAt: respondedAt,
			},
		})
	}
	result := app.TaskApprovalPage{Items: items, PendingCountsByHostID: pendingCountsByHostID}
	result.NextCursor = nextPageCursor(items, limit, func(value app.TaskApproval) (string, time.Time) {
		return value.Request.ID, value.Request.CreatedAt
	})
	if int32(len(result.Items)) > limit {
		result.Items = result.Items[:limit]
	}
	return result, nil
}

var _ app.TaskApprovalQueryStore = (*Store)(nil)
