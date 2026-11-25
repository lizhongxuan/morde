package task

import "testing"

func TestEventKindValid(t *testing.T) {
	for _, kind := range []EventKind{EventKindCreated, EventKindSplit, EventKindDispatched, EventKindApproval,
		EventKindExecution, EventKindVerification, EventKindDelivery, EventKindFailure, EventKindRecovery} {
		if !kind.Valid() {
			t.Fatalf("event kind %q must be valid", kind)
		}
	}
	for _, kind := range []EventKind{"", "queued", "completed", "unknown"} {
		if kind.Valid() {
			t.Fatalf("event kind %q must be invalid", kind)
		}
	}
}

func TestProjectTaskEventKind(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		payload   string
		want      EventKind
		wantOK    bool
	}{
		{name: "status queued is created", eventType: "task.status", payload: `{"status":"queued"}`, want: EventKindCreated, wantOK: true},
		{name: "status running is dispatched", eventType: "task.status", payload: `{"status":"running"}`, want: EventKindDispatched, wantOK: true},
		{name: "status waiting user is approval", eventType: "task.status", payload: `{"status":"waiting_user"}`, want: EventKindApproval, wantOK: true},
		{name: "status failed is failure", eventType: "task.status", payload: `{"status":"failed"}`, want: EventKindFailure, wantOK: true},
		{name: "status cancelled is failure", eventType: "task.status", payload: `{"status":"cancelled"}`, want: EventKindFailure, wantOK: true},
		{name: "status completed is not delivery", eventType: "task.status", payload: `{"status":"completed"}`},
		{name: "status pausing has no kind", eventType: "task.status", payload: `{"status":"pausing"}`},
		{name: "status paused has no kind", eventType: "task.status", payload: `{"status":"paused"}`},
		{name: "status cancelling has no kind", eventType: "task.status", payload: `{"status":"cancelling"}`},
		{name: "status missing has no kind", eventType: "task.status", payload: `{}`},
		{name: "status malformed has no kind", eventType: "task.status", payload: `{bad`},
		{name: "status unknown value has no kind", eventType: "task.status", payload: `{"status":"bogus"}`},
		{name: "plan is split", eventType: "task.plan", payload: `{}`, want: EventKindSplit, wantOK: true},
		{name: "step succeeded is execution", eventType: "task.step", payload: `{"status":"succeeded"}`, want: EventKindExecution, wantOK: true},
		{name: "step running is execution", eventType: "task.step", payload: `{"status":"running"}`, want: EventKindExecution, wantOK: true},
		{name: "step failed is failure", eventType: "task.step", payload: `{"status":"failed"}`, want: EventKindFailure, wantOK: true},
		{name: "step missing status has no kind", eventType: "task.step", payload: `{}`},
		{name: "step malformed has no kind", eventType: "task.step", payload: `{bad`},
		{name: "operation dispatched is execution", eventType: "task.operation", payload: `{"status":"dispatched"}`, want: EventKindExecution, wantOK: true},
		{name: "operation failed is failure", eventType: "task.operation", payload: `{"status":"failed"}`, want: EventKindFailure, wantOK: true},
		{name: "operation missing status has no kind", eventType: "task.operation", payload: `{}`},
		{name: "batch item running is execution", eventType: "task.batch_item", payload: `{"status":"running"}`, want: EventKindExecution, wantOK: true},
		{name: "batch item failed is failure", eventType: "task.batch_item", payload: `{"status":"failed"}`, want: EventKindFailure, wantOK: true},
		{name: "batch item missing status has no kind", eventType: "task.batch_item", payload: `{}`},
		{name: "waiting user is approval", eventType: "task.waiting_user", payload: `{}`, want: EventKindApproval, wantOK: true},
		{name: "result has no kind", eventType: "task.result", payload: `{"status":"completed"}`},
		{name: "message has no kind", eventType: "task.message", payload: `{}`},
		{name: "response has no kind", eventType: "task.response", payload: `{}`},
		{name: "artifact has no kind", eventType: "task.artifact", payload: `{}`},
		{name: "priority has no kind", eventType: "task.priority", payload: `{}`},
		{name: "unknown type has no kind", eventType: "task.bogus", payload: `{}`},
		{name: "empty type has no kind", eventType: "", payload: `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ProjectTaskEventKind(tc.eventType, []byte(tc.payload))
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("ProjectTaskEventKind(%q, %s) = (%q, %v), want (%q, %v)", tc.eventType, tc.payload, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
