package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func TestAdminObservabilityTraceSessionsExplainTasklessMessage(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 1000, 2048)
	conversation := createMainConversationForTest(t, store, owner.ID)
	messageID := mustID(t, domain.NewMessageID)
	if _, err := pool.Exec(ctx, `INSERT INTO conversation_messages
(id,conversation_id,owner_id,actor_id,role,content,task_id,sequence)
VALUES ($1,$2,$3,$3,'user','synthetic direct conversation',NULL,1)`, messageID, conversation.ID, owner.ID); err != nil {
		t.Fatalf("insert synthetic Task-less conversation message: %v", err)
	}
	const traceID = "cccccccccccccccccccccccccccccccc"
	const spanID = "dddddddddddddddd"
	if err := store.LinkAcceptedMessageTrace(ctx, traceID, spanID, "observability-taskless-request", messageID); err != nil {
		t.Fatalf("link synthetic direct conversation trace: %v", err)
	}

	sessions, err := store.GetAdminObservabilityTraceSessions(ctx, traceID)
	if err != nil || sessions.TaskID != nil || len(sessions.Sessions) != 0 || sessions.Source.Status != "partial" ||
		sessions.Source.Reason == nil || *sessions.Source.Reason != "trace is linked to a conversation message, but no DSH checkpoint Task is available" {
		t.Fatalf("Task-less conversation coverage = %+v, err=%v", sessions, err)
	}
}

func TestAdminObservabilityTraceSessionsDetectSpansBeyondCheckpoint(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 3, 4000, 8192)
	created, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "observability-uncheckpointed-span", "synthetic checkpoint coverage"))
	if err != nil {
		t.Fatalf("submit synthetic task: %v", err)
	}
	const (
		originTraceID  = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		originSpanID   = "eeeeeeeeeeeeeeee"
		attemptTraceID = "ffffffffffffffffffffffffffffffff"
		attemptSpanID  = "ffffffffffffffff"
		sessionID      = "recorded-session"
	)
	if err := store.LinkAcceptedMessageTrace(ctx, originTraceID, originSpanID, "observability-checkpoint-request", created.Message.ID); err != nil {
		t.Fatalf("link accepted message to source trace: %v", err)
	}
	origin, found, err := store.GetTaskObservabilityContext(ctx, created.Task.ID)
	if err != nil || !found {
		t.Fatalf("task observability context = %+v found=%t err=%v", origin, found, err)
	}
	if err := store.LinkTaskAttemptTrace(ctx, created.Task.ID, 1, origin, attemptTraceID, attemptSpanID); err != nil {
		t.Fatalf("link task attempt trace: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest)
VALUES ($1,$2,$3,1,$4)`, created.Task.ID, owner.ID, sessionID, make([]byte, 32)); err != nil {
		t.Fatalf("insert synthetic DSH session checkpoint: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO observability_spans
(trace_id,span_id,name,span_kind,start_time,end_time,status,attributes)
VALUES ($1,$2,'morde.tool.call','internal',clock_timestamp(),clock_timestamp(),'ok',$3::jsonb)`,
		attemptTraceID, attemptSpanID, `{"morde.task_id":"`+created.Task.ID+`","morde.agent_session_id":"`+sessionID+`","morde.event_sequence":2}`); err != nil {
		t.Fatalf("insert synthetic span beyond checkpoint: %v", err)
	}

	sessions, err := store.GetAdminObservabilityTraceSessions(ctx, originTraceID)
	if err != nil || len(sessions.Sessions) != 1 || sessions.Source.Status != "partial" ||
		sessions.Source.Reason == nil || *sessions.Source.Reason != "one or more DSH spans record event sequences beyond the last committed checkpoint" {
		t.Fatalf("uncheckpointed span coverage = %+v, err=%v", sessions, err)
	}
}

func TestAdminObservabilityTraceIncludesLinkedTaskAttempt(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 2, 4000, 8192)
	submission := newTestTaskSubmission(t, owner.ID, "", "observability-attempt-link", "synthetic observability fixture")
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatalf("submit synthetic conversation task: %v", err)
	}
	const (
		originTraceID  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		originSpanID   = "aaaaaaaaaaaaaaaa"
		attemptTraceID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		attemptSpanID  = "bbbbbbbbbbbbbbbb"
		requestID      = "observability-request-1"
	)
	if err := store.LinkAcceptedMessageTrace(ctx, originTraceID, originSpanID, requestID, created.Message.ID); err != nil {
		t.Fatalf("link accepted message to source trace: %v", err)
	}
	origin, found, err := store.GetTaskObservabilityContext(ctx, created.Task.ID)
	if err != nil || !found || origin.OriginTraceID != originTraceID || origin.OriginSpanID != originSpanID ||
		origin.RequestID != requestID || origin.MessageID != created.Message.ID {
		t.Fatalf("task observability context = %+v found=%t err=%v", origin, found, err)
	}
	if err := store.LinkTaskAttemptTrace(ctx, created.Task.ID, 1, origin, attemptTraceID, attemptSpanID); err != nil {
		t.Fatalf("link task attempt trace: %v", err)
	}
	if err := store.LinkTaskAttemptTrace(ctx, created.Task.ID, 1, origin, attemptTraceID, attemptSpanID); err != nil {
		t.Fatalf("replay task attempt trace link: %v", err)
	}
	base := time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC)
	for _, span := range []struct {
		traceID string
		spanID  string
		links   string
	}{{originTraceID, originSpanID, "[]"},
		{attemptTraceID, attemptSpanID, `[{"trace_id":"` + originTraceID + `","span_id":"` + originSpanID + `","attributes":{"morde.link.kind":"task_input"}}]`}} {
		if _, err := pool.Exec(ctx, `INSERT INTO observability_spans
(trace_id,span_id,name,span_kind,start_time,end_time,status,links)
VALUES ($1,$2,'synthetic span','internal',$3,$3,'ok',$4::jsonb)`, span.traceID, span.spanID, base, span.links); err != nil {
			t.Fatalf("insert synthetic span: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO observability_logs
(trace_id,span_id,request_id,task_id,service,severity,message,attributes,occurred_at)
VALUES ($1,$2,$3,$4,'morde-cloud-runtime','info','synthetic attempt log',$5::jsonb,$6)`,
		attemptTraceID, attemptSpanID, requestID, created.Task.ID, json.RawMessage(`{"operation":"task_attempt"}`), base); err != nil {
		t.Fatalf("insert synthetic attempt log: %v", err)
	}

	detail, err := store.GetAdminObservabilityTrace(ctx, originTraceID)
	if err != nil || detail.Source.Status != "available" || len(detail.Spans) != 2 {
		t.Fatalf("linked trace detail = %+v err=%v", detail, err)
	}
	var links []struct {
		TraceID string `json:"trace_id"`
		SpanID  string `json:"span_id"`
	}
	if err := json.Unmarshal(detail.Spans[1].Links, &links); err != nil || len(links) != 1 ||
		links[0].TraceID != originTraceID || links[0].SpanID != originSpanID ||
		detail.Spans[0].TraceID != originTraceID || detail.Spans[1].TraceID != attemptTraceID {
		t.Fatalf("linked span identities or link = %+v", detail.Spans)
	}
	page, err := store.ListAdminObservabilityLogs(ctx, app.AdminObservabilityLogQuery{TraceID: originTraceID, RequestID: requestID,
		TaskID: created.Task.ID, Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].TraceID != attemptTraceID || page.Items[0].Message != "synthetic attempt log" {
		t.Fatalf("linked attempt logs = %+v err=%v", page, err)
	}
	traces, err := store.ListAdminObservabilityTraces(ctx, app.AdminObservabilityTraceQuery{TraceID: originTraceID, Limit: 10})
	if err != nil || len(traces.Items) != 1 || traces.Items[0].SpanCount != 1 || traces.Items[0].TaskID == nil || *traces.Items[0].TaskID != created.Task.ID {
		t.Fatalf("source trace listing = %+v err=%v", traces, err)
	}
	sessions, err := store.GetAdminObservabilityTraceSessions(ctx, originTraceID)
	if err != nil || sessions.TaskID == nil || *sessions.TaskID != created.Task.ID || sessions.Source.Status != "partial" {
		t.Fatalf("source trace Task sessions = %+v err=%v", sessions, err)
	}
}
