package postgres

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

// GetTaskObservabilityContext resolves the earliest accepted user message for
// this task. It deliberately returns correlation metadata only.
func (s *Store) GetTaskObservabilityContext(ctx context.Context, taskID string) (app.TaskObservabilityContext, bool, error) {
	if !domain.ValidTaskID(taskID) {
		return app.TaskObservabilityContext{}, false, domain.InvalidArgument("task_id", "task identity is invalid")
	}
	var foundTask string
	var traceID, spanID, requestID, messageID pgtype.Text
	err := s.pool.QueryRow(ctx, `SELECT task.id,source.trace_id,source.span_id,source.request_id,source.message_id
FROM tasks task
LEFT JOIN LATERAL (
 SELECT link.trace_id,link.span_id,link.request_id,message.id AS message_id
 FROM conversation_messages message
 JOIN observability_trace_links link ON link.message_id=message.id
 WHERE message.task_id=task.id AND message.owner_id=task.owner_id AND message.role='user'
 ORDER BY message.created_at ASC,message.id ASC,link.created_at ASC,link.span_id ASC
 LIMIT 1
) source ON TRUE
WHERE task.id=$1`, taskID).Scan(&foundTask, &traceID, &spanID, &requestID, &messageID)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.TaskObservabilityContext{}, false, nil
	}
	if err != nil {
		return app.TaskObservabilityContext{}, false, fmt.Errorf("read task observability context: %w", err)
	}
	if foundTask != taskID {
		return app.TaskObservabilityContext{}, false, nil
	}
	if !traceID.Valid {
		return app.TaskObservabilityContext{}, false, nil
	}
	value := app.TaskObservabilityContext{OriginTraceID: traceID.String, OriginSpanID: spanID.String,
		RequestID: requestID.String, MessageID: messageID.String}
	if !validLowerHexIdentifier(value.OriginTraceID, 32) || !validLowerHexIdentifier(value.OriginSpanID, 16) ||
		value.RequestID == "" || len(value.RequestID) > 255 || !utf8.ValidString(value.RequestID) || !domain.ValidMessageID(value.MessageID) {
		return app.TaskObservabilityContext{}, false, errors.New("stored task observability context is invalid")
	}
	return value, true, nil
}

// LinkTaskAttemptTrace records a durable, idempotent relation from an exported
// task-attempt span to the original accepted message. The source link and task
// ownership are checked in the INSERT itself to prevent cross-task reuse.
func (s *Store) LinkTaskAttemptTrace(ctx context.Context, taskID string, attempt int64,
	origin app.TaskObservabilityContext, taskTraceID, taskSpanID string) error {
	if !domain.ValidTaskID(taskID) || attempt < 1 || !validLowerHexIdentifier(origin.OriginTraceID, 32) ||
		!validLowerHexIdentifier(origin.OriginSpanID, 16) || origin.RequestID == "" || len(origin.RequestID) > 255 ||
		!utf8.ValidString(origin.RequestID) || !domain.ValidMessageID(origin.MessageID) ||
		!validLowerHexIdentifier(taskTraceID, 32) || !validLowerHexIdentifier(taskSpanID, 16) {
		return domain.InvalidArgument("task_attempt_trace", "task attempt trace link is invalid")
	}
	var inserted int
	err := s.pool.QueryRow(ctx, `INSERT INTO observability_task_attempt_links
(task_id,attempt,origin_trace_id,origin_span_id,task_trace_id,task_span_id,request_id,message_id)
SELECT task.id,$2,$3,$4,$5,$6,$7,message.id
FROM tasks task
JOIN conversation_messages message ON message.task_id=task.id AND message.owner_id=task.owner_id AND message.role='user'
JOIN observability_trace_links source ON source.message_id=message.id
WHERE task.id=$1 AND message.id=$8 AND source.trace_id=$3 AND source.span_id=$4 AND source.request_id=$7
ON CONFLICT (task_id,attempt) DO NOTHING
RETURNING 1`, taskID, attempt, origin.OriginTraceID, origin.OriginSpanID, taskTraceID, taskSpanID,
		origin.RequestID, origin.MessageID).Scan(&inserted)
	if err == nil && inserted == 1 {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("write task attempt trace link: %w", err)
	}
	var same bool
	err = s.pool.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM observability_task_attempt_links
 WHERE task_id=$1 AND attempt=$2 AND origin_trace_id=$3 AND origin_span_id=$4 AND
  task_trace_id=$5 AND task_span_id=$6 AND request_id=$7 AND message_id=$8
)`, taskID, attempt, origin.OriginTraceID, origin.OriginSpanID, taskTraceID, taskSpanID,
		origin.RequestID, origin.MessageID).Scan(&same)
	if err != nil {
		return fmt.Errorf("verify task attempt trace link: %w", err)
	}
	if same {
		return nil
	}
	return errors.New("task attempt trace link has no matching accepted source or conflicts with a saved attempt")
}
