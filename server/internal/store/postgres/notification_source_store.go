package postgres

import (
	"context"
	"errors"
	"fmt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/notification"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Store) ListUnprocessedNotificationSourceEvents(ctx context.Context, limit int32) ([]notification.SourceEvent, error) {
	if limit < 1 || limit > 100 {
		return nil, domain.InvalidArgument("limit", "notification event batch size is out of range")
	}
	rows, err := s.pool.Query(ctx, `SELECT id,recipient_id,source_kind,source_id,source_version,
COALESCE(project_id,''),event_kind,created_at
FROM notification_source_events WHERE processed_at IS NULL ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("query unprocessed notification source events: %w", err)
	}
	defer rows.Close()
	events := make([]notification.SourceEvent, 0)
	for rows.Next() {
		var event notification.SourceEvent
		var kind string
		if err := rows.Scan(&event.ID, &event.RecipientID, &event.SourceKind, &event.SourceID,
			&event.SourceVersion, &event.ProjectID, &kind, &event.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan notification source event: %w", err)
		}
		event.Kind = notification.Kind(kind)
		if err := notification.ValidateSourceEvent(event); err != nil {
			return nil, fmt.Errorf("stored notification source event is invalid: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate notification source events: %w", err)
	}
	return events, nil
}

func (s *Store) ConsumeNotificationSourceEvent(ctx context.Context, event notification.SourceEvent, value notification.Notification) (bool, error) {
	if notification.ValidateSourceEvent(event) != nil || notification.Validate(value) != nil ||
		value.SourceEventID != event.ID || value.OwnerID != event.RecipientID || value.SourceKind != event.SourceKind ||
		value.SourceID != event.SourceID || value.SourceVersion != event.SourceVersion || value.ProjectID != event.ProjectID ||
		value.Kind != event.Kind || !value.CreatedAt.Equal(event.CreatedAt) {
		return false, domain.InvalidArgument("event", "notification source event identity is invalid")
	}
	consumed := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var current notification.SourceEvent
		var kind string
		var projectID pgtype.Text
		var processedAt pgtype.Timestamptz
		err := tx.QueryRow(ctx, `SELECT id,recipient_id,source_kind,source_id,source_version,project_id,event_kind,created_at,processed_at
FROM notification_source_events WHERE id=$1 FOR UPDATE`, event.ID).Scan(
			&current.ID, &current.RecipientID, &current.SourceKind, &current.SourceID, &current.SourceVersion,
			&projectID, &kind, &current.CreatedAt, &processedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "notification source event not found")
		}
		if err != nil {
			return fmt.Errorf("lock notification source event: %w", err)
		}
		if projectID.Valid {
			current.ProjectID = projectID.String
		}
		current.Kind = notification.Kind(kind)
		if !sameNotificationSourceEvent(current, event) {
			return domain.NewError(domain.CodeConflict, "notification source event changed")
		}
		if processedAt.Valid {
			return nil
		}
		if event.SourceKind == notification.SourceProjectWorkItem {
			var memberID string
			err := tx.QueryRow(ctx, `SELECT user_id FROM project_members WHERE project_id=$1 AND user_id=$2 FOR SHARE`, event.ProjectID, event.RecipientID).Scan(&memberID)
			if errors.Is(err, pgx.ErrNoRows) {
				if _, err := tx.Exec(ctx, `UPDATE notification_source_events SET processed_at=clock_timestamp() WHERE id=$1`, event.ID); err != nil {
					return fmt.Errorf("discard revoked project notification: %w", err)
				}
				consumed = true
				return nil
			}
			if err != nil {
				return fmt.Errorf("recheck project notification recipient: %w", err)
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO notifications
(id,owner_id,source_event_id,source_kind,source_id,source_version,project_id,kind,summary)
VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9)
ON CONFLICT (source_event_id) DO NOTHING`, value.ID, value.OwnerID, value.SourceEventID,
			value.SourceKind, value.SourceID, value.SourceVersion, value.ProjectID, string(value.Kind), value.Summary)
		if err != nil {
			return fmt.Errorf("create source notification: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE notification_source_events SET processed_at=clock_timestamp() WHERE id=$1`, event.ID); err != nil {
			return fmt.Errorf("mark notification source event processed: %w", err)
		}
		consumed = true
		return nil
	})
	return consumed, err
}

func sameNotificationSourceEvent(left, right notification.SourceEvent) bool {
	return left.ID == right.ID && left.RecipientID == right.RecipientID && left.SourceKind == right.SourceKind &&
		left.SourceID == right.SourceID && left.SourceVersion == right.SourceVersion && left.ProjectID == right.ProjectID &&
		left.Kind == right.Kind && left.CreatedAt.Equal(right.CreatedAt)
}
