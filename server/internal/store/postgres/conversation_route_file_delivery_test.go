package postgres

import (
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestStoredRouteFileDeliveryIntentIsLimitedToNewAndCurrentUpdate(t *testing.T) {
	context := app.ConversationRouteContext{ActorID: "u1234567", Content: "请求",
		Candidates: []app.ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 4}}}
	for _, test := range []struct {
		name     string
		decision app.ConversationRouteDecision
		want     bool
	}{
		{name: "new required", decision: app.ConversationRouteDecision{Action: app.ConversationRouteNew, FileDeliveryIntent: task.FileDeliveryRequired}, want: true},
		{name: "new legacy empty", decision: app.ConversationRouteDecision{Action: app.ConversationRouteNew}, want: true},
		{name: "update none", decision: app.ConversationRouteDecision{Action: app.ConversationRouteUpdate, TaskID: "q1234567", ExpectedRevision: 4, FileDeliveryIntent: task.FileDeliveryNone}, want: true},
		{name: "update unclear", decision: app.ConversationRouteDecision{Action: app.ConversationRouteUpdate, TaskID: "q1234567", ExpectedRevision: 4, FileDeliveryIntent: task.FileDeliveryUnclear}, want: true},
		{name: "invalid enum", decision: app.ConversationRouteDecision{Action: app.ConversationRouteNew, FileDeliveryIntent: task.FileDeliveryIntent("maybe")}},
		{name: "reply cannot set intent", decision: app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "Done.", FileDeliveryIntent: task.FileDeliveryRequired}},
		{name: "update must match revision", decision: app.ConversationRouteDecision{Action: app.ConversationRouteUpdate, TaskID: "q1234567", ExpectedRevision: 3, FileDeliveryIntent: task.FileDeliveryRequired}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validStoredRouteDecision(test.decision, context); got != test.want {
				t.Fatalf("validStoredRouteDecision() = %t, want %t for %+v", got, test.want, test.decision)
			}
		})
	}
}
