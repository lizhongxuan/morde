package postgres

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func (s *Store) ListAdminObservabilityLogs(ctx context.Context, query app.AdminObservabilityLogQuery) (app.AdminObservabilityLogPage, error) {
	if !observabilityTraceIDPattern.MatchString(query.TraceID) || query.Limit < 1 || query.Limit > 100 ||
		len(query.RequestID) > 255 || len(query.TaskID) > 128 || len(query.Cursor) > 512 {
		return app.AdminObservabilityLogPage{}, domain.InvalidArgument("observability_log_query", "observability log query is invalid")
	}
	var cursorTime *time.Time
	var cursorID int64
	if query.Cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(query.Cursor)
		if err != nil {
			return app.AdminObservabilityLogPage{}, domain.InvalidArgument("cursor", "observability log cursor is invalid")
		}
		parts := strings.SplitN(string(decoded), "\n", 2)
		if len(parts) != 2 {
			return app.AdminObservabilityLogPage{}, domain.InvalidArgument("cursor", "observability log cursor is invalid")
		}
		value, err := time.Parse(time.RFC3339Nano, parts[0])
		if err != nil {
			return app.AdminObservabilityLogPage{}, domain.InvalidArgument("cursor", "observability log cursor is invalid")
		}
		cursorID, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil || cursorID < 1 {
			return app.AdminObservabilityLogPage{}, domain.InvalidArgument("cursor", "observability log cursor is invalid")
		}
		cursorTime = &value
	}
	rows, err := s.pool.Query(ctx, `SELECT id,occurred_at,severity,service,message,trace_id,span_id,request_id,task_id,attributes
FROM observability_logs
WHERE (trace_id=$1 OR trace_id IN (
 SELECT task_trace_id FROM observability_task_attempt_links WHERE origin_trace_id=$1
 UNION
 SELECT origin_trace_id FROM observability_task_attempt_links WHERE task_trace_id=$1
)) AND ($2='' OR request_id=$2) AND ($3='' OR task_id=$3)
 AND ($4::timestamptz IS NULL OR occurred_at<$4 OR (occurred_at=$4 AND id<$5))
ORDER BY occurred_at DESC,id DESC LIMIT $6`, query.TraceID, query.RequestID, query.TaskID, cursorTime, cursorID, query.Limit+1)
	if err != nil {
		return app.AdminObservabilityLogPage{}, fmt.Errorf("list correlated observability logs: %w", err)
	}
	defer rows.Close()
	items := make([]app.AdminObservabilityLog, 0, query.Limit+1)
	for rows.Next() {
		var item app.AdminObservabilityLog
		var spanID, requestID, taskID string
		if err := rows.Scan(&item.ID, &item.Time, &item.Severity, &item.Service, &item.Message, &item.TraceID, &spanID,
			&requestID, &taskID, &item.Attributes); err != nil {
			return app.AdminObservabilityLogPage{}, fmt.Errorf("scan correlated observability log: %w", err)
		}
		item.Time = item.Time.UTC()
		if spanID != "" {
			item.SpanID = &spanID
		}
		if requestID != "" {
			item.RequestID = &requestID
		}
		if taskID != "" {
			item.TaskID = &taskID
		}
		if len(item.Attributes) == 0 {
			item.Attributes = []byte(`{}`)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return app.AdminObservabilityLogPage{}, fmt.Errorf("iterate correlated observability logs: %w", err)
	}
	page := app.AdminObservabilityLogPage{Items: items, Coverage: app.AdminObservabilitySource{Status: "available"}}
	if len(items) == 0 {
		reason := "no trace-correlated structured logs were captured"
		page.Coverage = app.AdminObservabilitySource{Status: "partial", Reason: &reason}
		return page, nil
	}
	if len(items) > query.Limit {
		last := items[query.Limit-1]
		cursor := base64.RawURLEncoding.EncodeToString([]byte(last.Time.UTC().Format(time.RFC3339Nano) + "\n" + strconv.FormatInt(last.ID, 10)))
		page.NextCursor = &cursor
		page.Items = items[:query.Limit]
	}
	updated := page.Items[0].Time
	page.Coverage.UpdatedAt = &updated
	return page, nil
}

var _ app.AdminObservabilityLogReader = (*Store)(nil)
