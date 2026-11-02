package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"

	"github.com/jackc/pgx/v5"
)

func (s *Store) ListPersonalGoals(ctx context.Context, principal identity.Principal, page app.PageRequest) (app.GoalPage, error) {
	if len(page.Cursor) > 1024 {
		return app.GoalPage{}, domain.InvalidArgument("cursor", "page cursor is invalid")
	}
	limit, cursorAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.GoalPage{}, err
	}
	if err := validatePersonalSyncStoreRequest(principal, limit); err != nil {
		return app.GoalPage{}, err
	}

	result := app.GoalPage{Items: make([]personal.Object, 0)}
	err = s.withPersonalSnapshot(ctx, func(tx pgx.Tx) error {
		if err := authorizePersonalMutationInTx(ctx, tx, principal); err != nil {
			return err
		}
		instanceID, err := serviceInstanceIDInTx(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT v.object_id, v.revision, p.body, p.created_at
FROM account_object_versions v
JOIN personal_objects p ON p.service_instance_id=v.service_instance_id AND p.owner_id=v.owner_id
  AND p.object_type=v.object_type AND p.object_id=v.object_id
WHERE v.service_instance_id=$1::uuid AND v.owner_id=$2 AND v.object_type=$3
  AND NOT v.is_deleted AND p.body IS NOT NULL
  AND ($4::timestamptz IS NULL OR (p.created_at,v.object_id)<($4::timestamptz,$5::text))
ORDER BY p.created_at DESC, v.object_id DESC
LIMIT $6`, instanceID, principal.UserID, personal.ObjectTypeGoal, cursorAt, cursorID, limit+1)
		if err != nil {
			return fmt.Errorf("query personal goals: %w", err)
		}
		defer rows.Close()

		type goalRow struct {
			object    personal.Object
			createdAt time.Time
		}
		listed := make([]goalRow, 0, limit+1)
		for rows.Next() {
			var row goalRow
			var body []byte
			if err := rows.Scan(&row.object.ObjectID, &row.object.Revision, &body, &row.createdAt); err != nil {
				return fmt.Errorf("scan personal goal: %w", err)
			}
			if row.object.Revision < 1 || len(body) == 0 || !json.Valid(body) ||
				personal.ValidateObjectIdentifier(personal.ObjectTypeGoal, row.object.ObjectID) != nil ||
				personal.ValidateObjectBody(personal.ObjectTypeGoal, row.object.ObjectID, body) != nil {
				return domain.NewError(domain.CodeInternal, "stored personal goal is invalid")
			}
			row.object.ObjectType = personal.ObjectTypeGoal
			row.object.Body = append(json.RawMessage(nil), body...)
			listed = append(listed, row)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate personal goals: %w", err)
		}
		hasMore := int32(len(listed)) > limit
		if hasMore {
			listed = listed[:limit]
		}
		result.Items = make([]personal.Object, 0, len(listed))
		for _, row := range listed {
			result.Items = append(result.Items, row.object)
		}
		if hasMore && len(listed) > 0 {
			last := listed[len(listed)-1]
			result.NextCursor = encodePageCursor(last.createdAt, last.object.ObjectID)
		}
		return nil
	})
	if err != nil {
		return app.GoalPage{}, err
	}
	return result, nil
}
