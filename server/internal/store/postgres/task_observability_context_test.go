package postgres

import (
	"context"
	"testing"

	"morde/server/internal/app"
)

func TestTaskObservabilityContextRejectsInvalidIdentitiesBeforeDatabase(t *testing.T) {
	store := &Store{}
	if _, found, err := store.GetTaskObservabilityContext(context.Background(), "invalid"); err == nil || found {
		t.Fatalf("invalid task identity result: found=%t err=%v", found, err)
	}

	origin := app.TaskObservabilityContext{OriginTraceID: "11111111111111111111111111111111",
		OriginSpanID: "2222222222222222", RequestID: "request_123", MessageID: "m1234567"}
	for _, test := range []struct {
		name    string
		taskID  string
		attempt int64
		origin  app.TaskObservabilityContext
		traceID string
		spanID  string
	}{
		{name: "invalid task", taskID: "invalid", attempt: 1, origin: origin,
			traceID: "33333333333333333333333333333333", spanID: "4444444444444444"},
		{name: "invalid attempt", taskID: "q1234567", attempt: 0, origin: origin,
			traceID: "33333333333333333333333333333333", spanID: "4444444444444444"},
		{name: "invalid source trace", taskID: "q1234567", attempt: 1,
			origin: app.TaskObservabilityContext{OriginTraceID: "00000000000000000000000000000000", OriginSpanID: origin.OriginSpanID,
				RequestID: origin.RequestID, MessageID: origin.MessageID},
			traceID: "33333333333333333333333333333333", spanID: "4444444444444444"},
		{name: "invalid attempt trace", taskID: "q1234567", attempt: 1, origin: origin,
			traceID: "00000000000000000000000000000000", spanID: "4444444444444444"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := store.LinkTaskAttemptTrace(context.Background(), test.taskID, test.attempt,
				test.origin, test.traceID, test.spanID); err == nil {
				t.Fatal("invalid trace link reached database or was accepted")
			}
		})
	}
}
