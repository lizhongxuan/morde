package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestAdminObservabilityLogsPaginationAndFilters(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	const traceID = "0123456789abcdef0123456789abcdef"
	base := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	for index := 0; index < 3; index++ {
		if _, err := pool.Exec(ctx, `INSERT INTO observability_logs
(trace_id,span_id,request_id,task_id,service,severity,message,attributes,occurred_at)
VALUES ($1,$2,$3,$4,'morde-portal','info',$5,$6::jsonb,$7)`, traceID,
			[]string{"0000000000000001", "0000000000000002", "0000000000000003"}[index], "req-1", "task-1",
			"message-"+string(rune('1'+index)), json.RawMessage(`{"stage":"route"}`), base.Add(time.Duration(index)*time.Second)); err != nil {
			t.Fatalf("insert observability log: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO observability_logs
(trace_id,service,severity,message,occurred_at) VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','morde-portal','info','other trace',$1)`, base); err != nil {
		t.Fatalf("insert unrelated observability log: %v", err)
	}
	first, err := store.ListAdminObservabilityLogs(ctx, app.AdminObservabilityLogQuery{TraceID: traceID, RequestID: "req-1", TaskID: "task-1", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.NextCursor == nil || first.Coverage.Status != "available" {
		t.Fatalf("first log page = %+v", first)
	}
	second, err := store.ListAdminObservabilityLogs(ctx, app.AdminObservabilityLogQuery{TraceID: traceID,
		RequestID: "req-1", TaskID: "task-1", Limit: 2, Cursor: *first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.NextCursor != nil || second.Items[0].Message != "message-1" {
		t.Fatalf("second log page = %+v", second)
	}
	missing, err := store.ListAdminObservabilityLogs(ctx, app.AdminObservabilityLogQuery{TraceID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing.Items) != 0 || missing.Coverage.Status != "partial" || missing.Coverage.Reason == nil {
		t.Fatalf("missing log coverage = %+v", missing)
	}
}
