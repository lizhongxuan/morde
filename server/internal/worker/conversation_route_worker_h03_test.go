package worker

import (
	"bytes"
	"context"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func safeSteerWorkerContext() app.ConversationRouteContext {
	digest := bytes.Repeat([]byte{1}, 32)
	return app.ConversationRouteContext{
		ActorID: "u1234567", Content: "补充：也请把成本写进报告。", CurrentRequestModelID: "p1234567", TimeZone: "Asia/Shanghai",
		ControlScopeDigest: digest,
		Candidates:         []app.ConversationRouteCandidate{{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 3}},
		RequestCandidates: []app.ConversationRouteRequestCandidate{{
			MessageID: "m1234567", Sequence: 1, Content: "整理财报", RoutingStatus: conversation.RoutingRouted,
			TaskID: "q1234567", TaskStatus: task.StatusRunning, SameControlScope: true, SameAuthorization: true,
			ControlScopeDigest: digest, RequestInput: app.ConversationRoutingInput{ModelID: "p1234567", TimeZone: "Asia/Shanghai"},
		}},
	}
}

func TestConversationRouteWorkerAppliesValidatedSteer(t *testing.T) {
	worker, stub := newRouteWorkerTest(t)
	stub.snapshot.Context = safeSteerWorkerContext()
	stub.decision = app.ConversationRouteDecision{Action: app.ConversationRouteSteer, TaskID: "q1234567", ExpectedRevision: 3}
	found, err := worker.RunOnce(context.Background())
	if err != nil || !found || len(stub.applied) != 1 || stub.applied[0].Action != app.ConversationRouteSteer ||
		stub.applied[0].TaskID != "q1234567" || stub.applied[0].Acknowledge {
		t.Fatalf("validated steer decision was not applied: decisions=%+v found=%t err=%v", stub.applied, found, err)
	}
}

func TestConversationRouteWorkerRejectsUnsafeSteerDecisions(t *testing.T) {
	for _, test := range []struct {
		name      string
		mutateCtx func(*app.ConversationRouteContext)
		decision  app.ConversationRouteDecision
	}{
		{name: "revision changed", decision: app.ConversationRouteDecision{Action: app.ConversationRouteSteer, TaskID: "q1234567", ExpectedRevision: 2}},
		{name: "acknowledged", decision: app.ConversationRouteDecision{Action: app.ConversationRouteSteer, TaskID: "q1234567", ExpectedRevision: 3, Acknowledge: true}},
		{name: "attachments", mutateCtx: func(value *app.ConversationRouteContext) { value.CurrentMessageHasAttachments = true },
			decision: app.ConversationRouteDecision{Action: app.ConversationRouteSteer, TaskID: "q1234567", ExpectedRevision: 3}},
		{name: "authorization changed", mutateCtx: func(value *app.ConversationRouteContext) { value.RequestCandidates[0].SameAuthorization = false },
			decision: app.ConversationRouteDecision{Action: app.ConversationRouteSteer, TaskID: "q1234567", ExpectedRevision: 3}},
		{name: "task no longer running", mutateCtx: func(value *app.ConversationRouteContext) { value.Candidates[0].Status = task.StatusPaused },
			decision: app.ConversationRouteDecision{Action: app.ConversationRouteSteer, TaskID: "q1234567", ExpectedRevision: 3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := safeSteerWorkerContext()
			if test.mutateCtx != nil {
				test.mutateCtx(&value)
			}
			decision := safeConversationRouteDecision(value, test.decision)
			if decision.Action != app.ConversationRouteClarify || decision.TaskID != "" {
				t.Fatalf("unsafe steer survived worker validation: %+v", decision)
			}
		})
	}
}
