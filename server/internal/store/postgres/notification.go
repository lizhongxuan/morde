package postgres

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/notification"
	"morde/server/internal/domain/task"

	"github.com/jackc/pgx/v5"
)

func (s *Store) ListUnprocessedNotificationEvents(ctx context.Context, limit int32) ([]notification.TaskEvent, error) {
	if limit < 1 || limit > 100 {
		return nil, domain.InvalidArgument("limit", "notification event batch size is out of range")
	}
	rows, err := s.pool.Query(ctx, `
SELECT t.owner_id, e.task_id, e.sequence, e.event_type, e.payload, t.status,
CASE
  WHEN NOT EXISTS (SELECT 1 FROM schedule_runs sr WHERE sr.task_id=e.task_id) THEN true
  WHEN e.event_type='task.waiting_user' THEN EXISTS (
    SELECT 1 FROM schedule_runs sr WHERE sr.task_id=e.task_id AND 'waiting_user'=ANY(sr.notification_events)
  )
  WHEN e.event_type='task.result' AND t.status='completed' THEN EXISTS (
    SELECT 1 FROM schedule_runs sr WHERE sr.task_id=e.task_id AND 'completed'=ANY(sr.notification_events)
      AND sr.observation_outcome<>'no_change'
  )
  WHEN e.event_type='task.result' AND t.status='failed' THEN EXISTS (
    SELECT 1 FROM schedule_runs sr WHERE sr.task_id=e.task_id AND 'failed'=ANY(sr.notification_events)
  )
  ELSE true
END AS notification_enabled, e.created_at
FROM task_events e JOIN tasks t ON t.id=e.task_id
WHERE (
  e.event_type IN ('task.result', 'task.waiting_user', 'task.response') OR
  (e.event_type='task.status' AND e.payload->>'status' IN ('cancelled', 'completed', 'failed'))
) AND NOT EXISTS (
  SELECT 1 FROM notification_event_receipts r WHERE r.task_id=e.task_id AND r.event_sequence=e.sequence
)
ORDER BY e.task_id, e.sequence
LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("query unprocessed task notification events: %w", err)
	}
	defer rows.Close()
	events := make([]notification.TaskEvent, 0)
	for rows.Next() {
		var value notification.TaskEvent
		var status string
		if err := rows.Scan(&value.OwnerID, &value.TaskID, &value.Sequence, &value.Type, &value.Payload,
			&status, &value.NotificationEnabled, &value.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan unprocessed task notification event: %w", err)
		}
		value.TaskStatus = task.Status(status)
		value.Payload = append([]byte(nil), value.Payload...)
		events = append(events, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unprocessed task notification events: %w", err)
	}
	return events, nil
}

// ConsumeTaskNotificationEvent locks the source task, preserves per-task event order,
// and commits the receipt with its notice/handled update as one transaction.
func (s *Store) ConsumeTaskNotificationEvent(ctx context.Context, event notification.TaskEvent, value *notification.Notification, closePending bool) (bool, error) {
	if !validNotificationSource(event) {
		return false, domain.InvalidArgument("event", "task notification event is invalid")
	}
	if value != nil {
		if err := notification.Validate(*value); err != nil || value.OwnerID != event.OwnerID || value.TaskID != event.TaskID || value.EventSequence != event.Sequence {
			return false, domain.InvalidArgument("notification", "notification event identity is invalid")
		}
	}
	consumed := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var currentStatus string
		err := tx.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1 AND owner_id=$2 FOR UPDATE`, event.TaskID, event.OwnerID).Scan(&currentStatus)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("lock notification source task: %w", err)
		}
		var eventType string
		var payload []byte
		err = tx.QueryRow(ctx, `SELECT event_type, payload FROM task_events WHERE task_id=$1 AND sequence=$2`, event.TaskID, event.Sequence).
			Scan(&eventType, &payload)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "task event not found")
		}
		if err != nil {
			return fmt.Errorf("verify notification source task event: %w", err)
		}
		if eventType != event.Type || !bytes.Equal(payload, event.Payload) {
			return domain.NewError(domain.CodeConflict, "task notification event changed")
		}
		var earlierUnprocessed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
  SELECT 1 FROM task_events e
  WHERE e.task_id=$1 AND e.sequence<$2 AND (
    e.event_type IN ('task.result', 'task.waiting_user', 'task.response') OR
    (e.event_type='task.status' AND e.payload->>'status' IN ('cancelled', 'completed', 'failed'))
  ) AND NOT EXISTS (
    SELECT 1 FROM notification_event_receipts r WHERE r.task_id=e.task_id AND r.event_sequence=e.sequence
  )
)`, event.TaskID, event.Sequence).Scan(&earlierUnprocessed); err != nil {
			return fmt.Errorf("check earlier task notification events: %w", err)
		}
		if earlierUnprocessed {
			return nil
		}
		result, err := tx.Exec(ctx, `INSERT INTO notification_event_receipts (task_id, event_sequence)
VALUES ($1,$2) ON CONFLICT (task_id,event_sequence) DO NOTHING`, event.TaskID, event.Sequence)
		if err != nil {
			return fmt.Errorf("record consumed task notification event: %w", err)
		}
		if result.RowsAffected() == 0 {
			return nil
		}
		if closePending {
			if _, err := tx.Exec(ctx, `UPDATE notifications SET actionable=false, handled_at=COALESCE(handled_at, clock_timestamp())
WHERE task_id=$1 AND actionable`, event.TaskID); err != nil {
				return fmt.Errorf("close handled task notifications: %w", err)
			}
		}
		if value != nil {
			active := false
			if value.Actionable && task.Status(currentStatus) == task.StatusWaitingUser {
				active, err = activeNotificationRequest(ctx, tx, *value)
				if err != nil {
					return fmt.Errorf("check active task notification request: %w", err)
				}
			}
			value.Actionable = value.Actionable && task.Status(currentStatus) == task.StatusWaitingUser && active
			markHandled := !value.Actionable && (value.Kind == notification.KindApprovalRequired || value.Kind == notification.KindClarification)
			deliverySequence, err := nextNotificationDeliverySequence(ctx, tx, value.OwnerID)
			if err != nil {
				return fmt.Errorf("reserve notification delivery sequence: %w", err)
			}
			inserted, err := tx.Exec(ctx, `INSERT INTO notifications
(id, owner_id, task_id, event_sequence, kind, summary, request_id, request_version, actionable, handled_at, delivery_sequence)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,CASE WHEN $10::boolean THEN clock_timestamp() ELSE NULL END,$11)
ON CONFLICT (task_id,event_sequence) DO NOTHING`, value.ID, value.OwnerID, value.TaskID, value.EventSequence,
				string(value.Kind), value.Summary, value.RequestID, value.RequestVersion, value.Actionable,
				markHandled, deliverySequence)
			if err != nil {
				return fmt.Errorf("create task notification: %w", err)
			}
			if inserted.RowsAffected() == 1 {
				if err := commitNotificationDeliverySequence(ctx, tx, value.OwnerID, deliverySequence); err != nil {
					return fmt.Errorf("commit notification delivery sequence: %w", err)
				}
			}
		}
		consumed = true
		return nil
	})
	return consumed, err
}

func activeNotificationRequest(ctx context.Context, tx pgx.Tx, value notification.Notification) (bool, error) {
	if value.RequestID == "" || value.RequestVersion < 1 {
		return false, nil
	}
	var exists bool
	query := `SELECT EXISTS (SELECT 1 FROM task_pending_requests WHERE id=$1 AND task_id=$2 AND version=$3 AND status='pending')`
	if value.Kind == notification.KindClarification {
		query = `SELECT EXISTS (SELECT 1 FROM task_clarification_requests WHERE id=$1 AND task_id=$2 AND version=$3 AND status='pending')`
	}
	if err := tx.QueryRow(ctx, query, value.RequestID, value.TaskID, value.RequestVersion).Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

func validNotificationSource(event notification.TaskEvent) bool {
	if !domain.ValidUserID(event.OwnerID) || !domain.ValidTaskID(event.TaskID) || event.Sequence < 1 ||
		!json.Valid(event.Payload) || event.CreatedAt.IsZero() {
		return false
	}
	switch event.Type {
	case "task.result", "task.waiting_user", "task.response", "task.status":
		return true
	default:
		return false
	}
}

func (s *Store) ListNotificationUpdates(ctx context.Context, ownerID string, page app.PageRequest) (app.NotificationUpdatePage, error) {
	if !domain.ValidUserID(ownerID) {
		return app.NotificationUpdatePage{}, domain.InvalidArgument("owner_id", "notification owner is invalid")
	}
	if page.Limit < 1 || page.Limit > 100 || len(page.Cursor) > 1024 {
		return app.NotificationUpdatePage{}, domain.InvalidArgument("page", "notification update request is out of range")
	}
	result := app.NotificationUpdatePage{Items: []notification.Notification{}}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		lastSequence, secret, err := notificationDeliveryState(ctx, tx, ownerID, page.Cursor == "")
		if err != nil {
			return fmt.Errorf("read notification delivery state: %w", err)
		}
		if page.Cursor == "" {
			result.Cursor = encodeNotificationUpdateCursor(ownerID, lastSequence, secret, time.Now().UTC())
			return nil
		}
		after, err := decodeNotificationUpdateCursor(page.Cursor, ownerID, secret, time.Now().UTC())
		if err != nil {
			return err
		}
		if after > lastSequence {
			return invalidNotificationUpdateCursor()
		}
		rows, err := tx.Query(ctx, `
SELECT n.delivery_sequence,
       (n.project_id IS NULL OR EXISTS (
          SELECT 1 FROM project_members pm WHERE pm.project_id=n.project_id AND pm.user_id=n.owner_id
       )) AS currently_visible,
       n.id, n.owner_id, COALESCE(n.task_id,''), COALESCE(n.event_sequence,0), COALESCE(n.source_event_id,0),
       COALESCE(n.source_kind,''), COALESCE(n.source_id,''), COALESCE(n.source_version,0), COALESCE(n.project_id,''),
       n.kind, n.summary, n.request_id, n.request_version,
       COALESCE(n.actionable AND t.status='waiting_user' AND current_request.is_pending,false) AS actionable,
       n.read_at,
       COALESCE(n.handled_at, CASE WHEN n.actionable AND NOT (t.status='waiting_user' AND current_request.is_pending) THEN t.updated_at END) AS handled_at,
       n.created_at
FROM notifications n
LEFT JOIN tasks t ON t.id=n.task_id AND t.owner_id=n.owner_id
CROSS JOIN LATERAL (SELECT CASE n.kind
  WHEN 'approval_required' THEN EXISTS (
    SELECT 1 FROM task_pending_requests r WHERE r.id=n.request_id AND r.task_id=n.task_id AND r.version=n.request_version AND r.status='pending'
  )
  WHEN 'clarification_required' THEN EXISTS (
    SELECT 1 FROM task_clarification_requests r WHERE r.id=n.request_id AND r.task_id=n.task_id AND r.version=n.request_version AND r.status='pending'
  )
  ELSE false
END AS is_pending) current_request
WHERE n.owner_id=$1 AND n.delivery_sequence>$2
ORDER BY n.delivery_sequence ASC
LIMIT $3`, ownerID, after, page.Limit+1)
		if err != nil {
			return fmt.Errorf("query notification updates: %w", err)
		}
		defer rows.Close()
		lastScanned := after
		scanned := 0
		for rows.Next() {
			deliverySequence, visible, value, err := scanNotificationUpdateRow(rows)
			if err != nil {
				return err
			}
			if int32(scanned) < page.Limit {
				lastScanned = deliverySequence
			}
			if int32(scanned) < page.Limit && visible {
				result.Items = append(result.Items, value)
			}
			scanned++
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate notification updates: %w", err)
		}
		result.HasMore = int32(scanned) > page.Limit
		result.Cursor = encodeNotificationUpdateCursor(ownerID, lastScanned, secret, time.Now().UTC())
		return nil
	})
	if err != nil {
		return app.NotificationUpdatePage{}, err
	}
	return result, nil
}

const notificationUpdateCursorTTL = 30 * 24 * time.Hour

type notificationUpdateCursor struct {
	Version  int    `json:"v"`
	OwnerID  string `json:"owner_id"`
	Sequence int64  `json:"sequence"`
	IssuedAt int64  `json:"issued_at"`
}

func notificationDeliveryState(ctx context.Context, tx pgx.Tx, ownerID string, lock bool) (int64, []byte, error) {
	query := `SELECT last_sequence,cursor_secret FROM notification_account_sequences WHERE owner_id=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	var lastSequence int64
	var storedSecret []byte
	err := tx.QueryRow(ctx, query, ownerID).Scan(&lastSequence, &storedSecret)
	if errors.Is(err, pgx.ErrNoRows) {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return 0, nil, fmt.Errorf("generate notification cursor secret: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO notification_account_sequences(owner_id,cursor_secret)
VALUES ($1,$2) ON CONFLICT (owner_id) DO NOTHING`, ownerID, secret); err != nil {
			return 0, nil, fmt.Errorf("initialize notification delivery state: %w", err)
		}
		err = tx.QueryRow(ctx, query, ownerID).Scan(&lastSequence, &storedSecret)
	}
	if err != nil {
		return 0, nil, fmt.Errorf("load notification delivery state: %w", err)
	}
	if lastSequence < 0 || len(storedSecret) != 32 {
		return 0, nil, errors.New("notification delivery state is invalid")
	}
	return lastSequence, storedSecret, nil
}

// nextNotificationDeliverySequence holds the account row lock through transaction commit.
// The caller inserts the notification before advancing the locked counter, so its sequence
// cannot be visible unless that notification is committed in the same transaction.
func nextNotificationDeliverySequence(ctx context.Context, tx pgx.Tx, ownerID string) (int64, error) {
	lastSequence, _, err := notificationDeliveryState(ctx, tx, ownerID, true)
	if err != nil {
		return 0, err
	}
	if lastSequence == int64(^uint64(0)>>1) {
		return 0, errors.New("notification delivery sequence is exhausted")
	}
	return lastSequence + 1, nil
}

func commitNotificationDeliverySequence(ctx context.Context, tx pgx.Tx, ownerID string, sequence int64) error {
	result, err := tx.Exec(ctx, `UPDATE notification_account_sequences
SET last_sequence=$2 WHERE owner_id=$1 AND last_sequence=$2-1`, ownerID, sequence)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "notification delivery sequence changed")
	}
	return nil
}

func encodeNotificationUpdateCursor(ownerID string, sequence int64, secret []byte, now time.Time) string {
	value := notificationUpdateCursor{Version: 1, OwnerID: ownerID, Sequence: sequence, IssuedAt: now.UTC().Unix()}
	payload, _ := json.Marshal(value)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func decodeNotificationUpdateCursor(value, ownerID string, secret []byte, now time.Time) (int64, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return 0, invalidNotificationUpdateCursor()
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return 0, invalidNotificationUpdateCursor()
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0, invalidNotificationUpdateCursor()
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return 0, invalidNotificationUpdateCursor()
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var cursor notificationUpdateCursor
	if err := decoder.Decode(&cursor); err != nil {
		return 0, invalidNotificationUpdateCursor()
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return 0, invalidNotificationUpdateCursor()
	}
	issuedAt := time.Unix(cursor.IssuedAt, 0).UTC()
	if cursor.Version != 1 || cursor.OwnerID != ownerID || cursor.Sequence < 0 || cursor.IssuedAt <= 0 || issuedAt.After(now.UTC().Add(5*time.Minute)) {
		return 0, invalidNotificationUpdateCursor()
	}
	if now.UTC().After(issuedAt.Add(notificationUpdateCursorTTL)) {
		return 0, domain.NewError(domain.CodeEventCursorExpired, "notification cursor expired; reconcile from the notification list before establishing a new baseline")
	}
	return cursor.Sequence, nil
}

func invalidNotificationUpdateCursor() error {
	return domain.InvalidArgument("cursor", "notification cursor is invalid")
}

func scanNotificationUpdateRow(rows pgx.Rows) (int64, bool, notification.Notification, error) {
	var sequence int64
	var visible bool
	var value notification.Notification
	var kind string
	err := rows.Scan(&sequence, &visible, &value.ID, &value.OwnerID, &value.TaskID, &value.EventSequence,
		&value.SourceEventID, &value.SourceKind, &value.SourceID, &value.SourceVersion, &value.ProjectID,
		&kind, &value.Summary, &value.RequestID, &value.RequestVersion, &value.Actionable,
		&value.ReadAt, &value.HandledAt, &value.CreatedAt)
	if err != nil {
		return 0, false, notification.Notification{}, fmt.Errorf("scan notification update: %w", err)
	}
	value.Kind = notification.Kind(kind)
	if err := notification.Validate(value); err != nil {
		return 0, false, notification.Notification{}, fmt.Errorf("stored notification update is invalid: %w", err)
	}
	return sequence, visible, value, nil
}

func (s *Store) ListNotifications(ctx context.Context, ownerID string, unreadOnly bool, page app.PageRequest) (app.NotificationPage, error) {
	if !domain.ValidUserID(ownerID) {
		return app.NotificationPage{}, domain.InvalidArgument("owner_id", "notification owner is invalid")
	}
	limit, createdAt, cursorID, err := pagination(page.Limit, page.Cursor)
	if err != nil {
		return app.NotificationPage{}, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT n.id, n.owner_id, COALESCE(n.task_id,''), COALESCE(n.event_sequence,0), COALESCE(n.source_event_id,0),
COALESCE(n.source_kind,''), COALESCE(n.source_id,''), COALESCE(n.source_version,0), COALESCE(n.project_id,''),
n.kind, n.summary, n.request_id, n.request_version,
COALESCE(n.actionable AND t.status='waiting_user' AND current_request.is_pending,false) AS actionable,
n.read_at,
COALESCE(n.handled_at, CASE WHEN n.actionable AND NOT (t.status='waiting_user' AND current_request.is_pending) THEN t.updated_at END) AS handled_at,
n.created_at
FROM notifications n
LEFT JOIN tasks t ON t.id=n.task_id AND t.owner_id=n.owner_id
CROSS JOIN LATERAL (SELECT CASE n.kind
  WHEN 'approval_required' THEN EXISTS (
    SELECT 1 FROM task_pending_requests r WHERE r.id=n.request_id AND r.task_id=n.task_id AND r.version=n.request_version AND r.status='pending'
  )
  WHEN 'clarification_required' THEN EXISTS (
    SELECT 1 FROM task_clarification_requests r WHERE r.id=n.request_id AND r.task_id=n.task_id AND r.version=n.request_version AND r.status='pending'
  )
  ELSE false
END AS is_pending) current_request
WHERE n.owner_id=$1 AND (n.project_id IS NULL OR EXISTS (
  SELECT 1 FROM project_members pm WHERE pm.project_id=n.project_id AND pm.user_id=n.owner_id
)) AND (NOT $2::boolean OR (n.created_at,n.id)<($3,$4))
AND (NOT $5::boolean OR n.read_at IS NULL)
ORDER BY n.created_at DESC, n.id DESC LIMIT $6`, ownerID, createdAt.Valid, createdAt, cursorID, unreadOnly, limit+1)
	if err != nil {
		return app.NotificationPage{}, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()
	values := make([]notification.Notification, 0)
	for rows.Next() {
		value, err := scanNotification(rows)
		if err != nil {
			return app.NotificationPage{}, err
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return app.NotificationPage{}, fmt.Errorf("iterate notifications: %w", err)
	}
	var next string
	if int32(len(values)) > limit {
		last := values[limit-1]
		next = encodePageCursor(last.CreatedAt, last.ID)
		values = values[:limit]
	}
	if values == nil {
		values = []notification.Notification{}
	}
	return app.NotificationPage{Items: values, NextCursor: next}, nil
}

func (s *Store) MarkNotificationRead(ctx context.Context, ownerID, id string) (notification.Notification, error) {
	if !domain.ValidUserID(ownerID) || !notification.ValidID(id) {
		return notification.Notification{}, domain.NewError(domain.CodeNotFound, "notification not found")
	}
	row := s.pool.QueryRow(ctx, `UPDATE notifications SET read_at=COALESCE(read_at, clock_timestamp())
WHERE id=$1 AND owner_id=$2 AND (project_id IS NULL OR EXISTS (
  SELECT 1 FROM project_members pm WHERE pm.project_id=notifications.project_id AND pm.user_id=notifications.owner_id
))
RETURNING id, owner_id, COALESCE(task_id,''), COALESCE(event_sequence,0), COALESCE(source_event_id,0),
COALESCE(source_kind,''), COALESCE(source_id,''), COALESCE(source_version,0), COALESCE(project_id,''), kind, summary, request_id, request_version,
actionable, read_at, handled_at, created_at`, id, ownerID)
	value, err := scanNotificationRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return notification.Notification{}, domain.NewError(domain.CodeNotFound, "notification not found")
	}
	if err != nil {
		return notification.Notification{}, fmt.Errorf("mark notification read: %w", err)
	}
	return value, nil
}

type notificationRowScanner interface {
	Scan(...any) error
}

func scanNotification(rows pgx.Rows) (notification.Notification, error) {
	return scanNotificationRow(rows)
}

func scanNotificationRow(row notificationRowScanner) (notification.Notification, error) {
	var value notification.Notification
	var kind string
	err := row.Scan(&value.ID, &value.OwnerID, &value.TaskID, &value.EventSequence, &value.SourceEventID,
		&value.SourceKind, &value.SourceID, &value.SourceVersion, &value.ProjectID, &kind, &value.Summary,
		&value.RequestID, &value.RequestVersion, &value.Actionable, &value.ReadAt, &value.HandledAt, &value.CreatedAt)
	if err != nil {
		return notification.Notification{}, err
	}
	value.Kind = notification.Kind(kind)
	if err := notification.Validate(value); err != nil {
		return notification.Notification{}, fmt.Errorf("stored notification is invalid: %w", err)
	}
	return value, nil
}
