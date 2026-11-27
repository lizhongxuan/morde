package postgres

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

const maxAdminObservabilitySpans = 500

var observabilityTraceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (s *Store) GetAdminObservabilityOverview(ctx context.Context) (app.AdminObservabilityOverview, error) {
	var result app.AdminObservabilityOverview
	result.SpansSource = app.AdminObservabilitySource{Status: "unavailable", Reason: observabilityReason("span source query failed")}
	result.EventsSource = app.AdminObservabilitySource{Status: "unavailable", Reason: observabilityReason("DSH checkpoint source query failed")}
	var latestSpan, latestEvent pgtype.Timestamptz
	var requestCount, errorCount int64
	var p95 float64
	spanCtx, cancelSpans := context.WithTimeout(ctx, 1200*time.Millisecond)
	err := s.pool.QueryRow(spanCtx, `SELECT count(*) FILTER (WHERE span_kind='server'),
 count(*) FILTER (WHERE span_kind='server' AND status='error'),
 COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM end_time-start_time)*1000)
   FILTER (WHERE span_kind='server'),0)::float8,
 max(created_at) FILTER (WHERE span_kind='server')
FROM observability_spans WHERE start_time >= clock_timestamp()-interval '24 hours'`).
		Scan(&requestCount, &errorCount, &p95, &latestSpan)
	cancelSpans()
	if err == nil {
		result.RequestCount, result.ErrorCount, result.P95DurationMS = requestCount, errorCount, p95
		result.SpansSource = observabilitySource(latestSpan, requestCount > 0, "no HTTP span has been exported yet")
	}
	var eventCount int64
	eventCtx, cancelEvents := context.WithTimeout(ctx, 1200*time.Millisecond)
	eventErr := s.pool.QueryRow(eventCtx, `SELECT count(*),max(created_at) FROM task_agent_checkpoints`).Scan(&eventCount, &latestEvent)
	cancelEvents()
	if eventErr == nil {
		result.EventsSource = observabilitySource(latestEvent, eventCount > 0, "no committed DSH checkpoint is available yet")
	}
	return result, nil
}

func observabilityReason(value string) *string { return &value }

func observabilitySource(updated pgtype.Timestamptz, available bool, emptyReason string) app.AdminObservabilitySource {
	source := app.AdminObservabilitySource{Status: "available"}
	if updated.Valid {
		value := updated.Time.UTC()
		source.UpdatedAt = &value
	}
	if !available {
		reason := emptyReason
		source.Status, source.Reason = "partial", &reason
	}
	return source
}

func (s *Store) ListAdminObservabilityTraces(ctx context.Context, query app.AdminObservabilityTraceQuery) (app.AdminObservabilityTracePage, error) {
	if query.Limit < 1 || query.Limit > 100 || len(query.Query) > 256 {
		return app.AdminObservabilityTracePage{}, domain.InvalidArgument("observability_query", "observability query is invalid")
	}
	var cursorTime *time.Time
	var cursorTraceID string
	if query.Cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(query.Cursor)
		if err != nil {
			return app.AdminObservabilityTracePage{}, domain.InvalidArgument("cursor", "observability cursor is invalid")
		}
		parts := strings.SplitN(string(decoded), "\n", 2)
		if len(parts) != 2 || !observabilityTraceIDPattern.MatchString(parts[1]) {
			return app.AdminObservabilityTracePage{}, domain.InvalidArgument("cursor", "observability cursor is invalid")
		}
		value, err := time.Parse(time.RFC3339Nano, parts[0])
		if err != nil {
			return app.AdminObservabilityTracePage{}, domain.InvalidArgument("cursor", "observability cursor is invalid")
		}
		cursorTime, cursorTraceID = &value, parts[1]
	}
	if query.TraceID != "" && !observabilityTraceIDPattern.MatchString(query.TraceID) {
		return app.AdminObservabilityTracePage{}, domain.InvalidArgument("trace_id", "trace ID is invalid")
	}
	query.Query = strings.TrimSpace(query.Query)
	if query.Query != "" {
		query.Query = strings.ReplaceAll(query.Query, "!", "!!")
		query.Query = strings.ReplaceAll(query.Query, "%", "!%")
		query.Query = strings.ReplaceAll(query.Query, "_", "!_")
		query.Query = "%" + query.Query + "%"
	}
 	rows, err := s.pool.Query(ctx, `WITH linked AS (
 SELECT link.trace_id,max(link.request_id) AS request_id,max(message.task_id) AS task_id,
  min(link.created_at) AS first_at,max(link.created_at) AS last_at
 FROM observability_trace_links link LEFT JOIN conversation_messages message ON message.id=link.message_id
 GROUP BY link.trace_id
), expanded AS (
 SELECT span.trace_id,span.start_time,span.end_time,span.status,
  COALESCE(NULLIF(span.attributes->>'morde.request_id',''),linked.request_id) AS request_id,
  COALESCE(NULLIF(span.attributes->>'morde.task_id',''),message.task_id,linked.task_id) AS task_id,
  span.name,COALESCE(span.attributes->>'http.route','') AS route,true AS is_span
 FROM observability_spans span
 LEFT JOIN conversation_messages message ON message.id=span.attributes->>'morde.message_id'
 LEFT JOIN linked ON linked.trace_id=span.trace_id
 UNION ALL
 SELECT linked.trace_id,linked.first_at,linked.last_at,'unset',linked.request_id,linked.task_id,'conversation link','',false
 FROM linked WHERE NOT EXISTS (SELECT 1 FROM observability_spans span WHERE span.trace_id=linked.trace_id)
), grouped AS (
 SELECT trace_id,max(request_id) AS request_id,max(task_id) AS task_id,min(start_time) AS started_at,max(end_time) AS ended_at,
  count(*) FILTER (WHERE is_span)::bigint AS span_count,count(*) FILTER (WHERE is_span AND status='error')::bigint AS error_count,
  max(name) AS span_name,max(route) AS route
 FROM expanded GROUP BY trace_id
)
SELECT trace_id,request_id,task_id,started_at,ended_at,span_count,error_count
FROM grouped
WHERE ($1='' OR trace_id=$1)
 AND ($2='' OR request_id=$2)
 AND ($3='' OR task_id=$3)
 AND ($4='' OR trace_id ILIKE $4 ESCAPE '!' OR COALESCE(request_id,'') ILIKE $4 ESCAPE '!'
   OR COALESCE(task_id,'') ILIKE $4 ESCAPE '!' OR span_name ILIKE $4 ESCAPE '!' OR route ILIKE $4 ESCAPE '!')
 AND ($5::timestamptz IS NULL OR ended_at<$5 OR (ended_at=$5 AND trace_id<$6))
ORDER BY ended_at DESC,trace_id DESC LIMIT $7`, query.TraceID, query.RequestID, query.TaskID, query.Query,
		cursorTime, cursorTraceID, query.Limit+1)
	if err != nil {
		return app.AdminObservabilityTracePage{}, fmt.Errorf("list observability traces: %w", err)
	}
	defer rows.Close()
	items := make([]app.AdminObservabilityTraceSummary, 0, query.Limit+1)
	for rows.Next() {
		var item app.AdminObservabilityTraceSummary
		var requestID, taskID pgtype.Text
		if err := rows.Scan(&item.TraceID, &requestID, &taskID, &item.StartedAt, &item.EndedAt, &item.SpanCount, &item.ErrorCount); err != nil {
			return app.AdminObservabilityTracePage{}, fmt.Errorf("scan observability trace: %w", err)
		}
		if requestID.Valid {
			item.RequestID = &requestID.String
		}
		if taskID.Valid {
			item.TaskID = &taskID.String
		}
		item.StartedAt = item.StartedAt.UTC()
		item.EndedAt = timePtrUTC(item.EndedAt)
		item.Status = "ok"
		if item.ErrorCount > 0 {
			item.Status = "error"
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return app.AdminObservabilityTracePage{}, fmt.Errorf("iterate observability traces: %w", err)
	}
	page := app.AdminObservabilityTracePage{Items: items}
	if len(items) > query.Limit {
		last := items[query.Limit-1]
		cursor := base64.RawURLEncoding.EncodeToString([]byte(last.EndedAt.UTC().Format(time.RFC3339Nano) + "\n" + last.TraceID))
		page.NextCursor = &cursor
		page.Items = items[:query.Limit]
	}
	return page, nil
}

func (s *Store) GetAdminObservabilityTrace(ctx context.Context, traceID string) (app.AdminObservabilityTraceDetail, error) {
	if !observabilityTraceIDPattern.MatchString(traceID) {
		return app.AdminObservabilityTraceDetail{}, domain.InvalidArgument("trace_id", "trace ID is invalid")
	}
	rows, err := s.pool.Query(ctx, `SELECT span_id,parent_span_id,name,span_kind,status,start_time,end_time,attributes,events,
 dropped_attributes+dropped_links+dropped_events
FROM observability_spans WHERE trace_id=$1 ORDER BY start_time,span_id LIMIT $2`, traceID, maxAdminObservabilitySpans+1)
	if err != nil {
		return app.AdminObservabilityTraceDetail{}, fmt.Errorf("read observability trace spans: %w", err)
	}
	defer rows.Close()
	spans := make([]app.AdminObservabilitySpan, 0, maxAdminObservabilitySpans)
	dropped := false
	truncated := false
	for rows.Next() {
		var span app.AdminObservabilitySpan
		if err := rows.Scan(&span.SpanID, &span.ParentSpanID, &span.Name, &span.Kind, &span.Status, &span.StartAt, &span.EndAt,
			&span.Attributes, &span.Events, &span.DroppedCount); err != nil {
			return app.AdminObservabilityTraceDetail{}, fmt.Errorf("scan observability span: %w", err)
		}
		if len(spans) == maxAdminObservabilitySpans {
			truncated = true
			break
		}
		if span.DroppedCount > 0 {
			dropped = true
		}
		span.StartAt, span.EndAt = span.StartAt.UTC(), span.EndAt.UTC()
		if len(span.Attributes) == 0 {
			span.Attributes = []byte(`{}`)
		}
		if len(span.Events) == 0 {
			span.Events = []byte(`[]`)
		}
		spans = append(spans, span)
	}
	if err := rows.Err(); err != nil {
		return app.AdminObservabilityTraceDetail{}, fmt.Errorf("iterate observability span details: %w", err)
	}
	if len(spans) == 0 {
		var linked bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM observability_trace_links WHERE trace_id=$1)`, traceID).Scan(&linked); err != nil {
			return app.AdminObservabilityTraceDetail{}, fmt.Errorf("check observability trace link: %w", err)
		}
		if !linked {
			return app.AdminObservabilityTraceDetail{}, domain.NewError(domain.CodeNotFound, "trace not found")
		}
		reason := "request is linked to a conversation but span data is not available"
		return app.AdminObservabilityTraceDetail{TraceID: traceID, Spans: []app.AdminObservabilitySpan{},
			Source: app.AdminObservabilitySource{Status: "partial", Reason: &reason}}, nil
	}
	source := app.AdminObservabilitySource{Status: "available"}
	if dropped || truncated {
		reason := "some span attributes, links, events, or span rows were dropped by configured limits"
		source.Status, source.Reason = "partial", &reason
	}
	return app.AdminObservabilityTraceDetail{TraceID: traceID, Spans: spans, Source: source}, nil
}

func (s *Store) GetAdminObservabilityTraceSessions(ctx context.Context, traceID string) (app.AdminObservabilityTraceSessions, error) {
	if !observabilityTraceIDPattern.MatchString(traceID) {
		return app.AdminObservabilityTraceSessions{}, domain.InvalidArgument("trace_id", "trace ID is invalid")
	}
	var taskID, ownerID string
	err := s.pool.QueryRow(ctx, `WITH linked_tasks AS (
 SELECT COALESCE(NULLIF(span.attributes->>'morde.task_id',''),message.task_id) AS task_id,span.start_time AS linked_at
 FROM observability_spans span
 LEFT JOIN conversation_messages message ON message.id=span.attributes->>'morde.message_id'
 WHERE span.trace_id=$1
 UNION ALL
 SELECT message.task_id,link.created_at
 FROM observability_trace_links link
 JOIN conversation_messages message ON message.id=link.message_id
 WHERE link.trace_id=$1
)
SELECT task.id,task.owner_id FROM linked_tasks
JOIN tasks task ON task.id=linked_tasks.task_id
ORDER BY linked_tasks.linked_at DESC LIMIT 1`, traceID).Scan(&taskID, &ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		reason := "trace is not linked to a conversation message or Task"
		return app.AdminObservabilityTraceSessions{Sessions: []app.AdminObservabilityAgentSession{}, Source: app.AdminObservabilitySource{Status: "unavailable", Reason: &reason}}, nil
	}
	if err != nil {
		return app.AdminObservabilityTraceSessions{}, fmt.Errorf("resolve observability trace Task: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT session_id,COALESCE(parent_session_id,''),COALESCE(step_id,''),checkpoint_through_seq
FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2 ORDER BY created_at,session_id LIMIT 128`, taskID, ownerID)
	if err != nil {
		return app.AdminObservabilityTraceSessions{}, fmt.Errorf("list observability Agent sessions: %w", err)
	}
	defer rows.Close()
	sessions := make([]app.AdminObservabilityAgentSession, 0)
	var hasUncheckpointed bool
	for rows.Next() {
		var session app.AdminObservabilityAgentSession
		var parentID, stepID string
		if err := rows.Scan(&session.SessionID, &parentID, &stepID, &session.ThroughSequence); err != nil {
			return app.AdminObservabilityTraceSessions{}, fmt.Errorf("scan observability Agent session: %w", err)
		}
		if parentID != "" {
			session.ParentSessionID = &parentID
			session.Role = "subagent"
		} else {
			session.Role = "root"
		}
		if stepID != "" {
			session.StepID = &stepID
		}
		if session.ThroughSequence < 0 {
			hasUncheckpointed = true
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return app.AdminObservabilityTraceSessions{}, fmt.Errorf("iterate observability Agent sessions: %w", err)
	}
	taskValue, ownerValue := taskID, ownerID
	source := app.AdminObservabilitySource{Status: "available"}
	if len(sessions) == 0 {
		reason := "Task has no DSH session yet"
		source.Status, source.Reason = "partial", &reason
	} else if hasUncheckpointed {
		reason := "one or more DSH sessions have no committed checkpoint yet"
		source.Status, source.Reason = "partial", &reason
	}
	return app.AdminObservabilityTraceSessions{TaskID: &taskValue, OwnerID: &ownerValue, Sessions: sessions, Source: source}, nil
}

func timePtrUTC(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	utc := value.UTC()
	return &utc
}
