package worker

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/app"
	"morde/server/internal/domain/conversation"
)

type longReplyRouteActionsStub struct {
	*routeWorkerStub
	cancelChecks int
	cancelOn     int
}

func (s *longReplyRouteActionsStub) ConversationRouteCancellationRequested(context.Context, app.ConversationRouteClaim) (bool, error) {
	s.cancelChecks++
	return s.cancelOn > 0 && s.cancelChecks >= s.cancelOn, nil
}

type longReplyRouteGeneratorStub struct {
	*routeWorkerStub
	decision app.ConversationRouteDecision
	wait     bool
	calls    int
}

func (s *longReplyRouteGeneratorStub) GenerateConversationLongReply(ctx context.Context, _ app.ConversationRouteClaim,
	_ app.ConversationRouteContext) (app.ConversationRouteDecision, error) {
	s.calls++
	if s.wait {
		<-ctx.Done()
		return app.ConversationRouteDecision{}, ctx.Err()
	}
	return s.decision, nil
}

func TestConversationRouteWorkerLongReplyPathAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name       string
		cancelOn   int
		wait       bool
		wantCancel bool
		decision   app.ConversationRouteDecision
	}{
		{name: "applies direct long reply without classifier", decision: app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: "A complete answer.", LongReply: true}},
		{name: "cancellation stops provider call", cancelOn: 2, wait: true, wantCancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := &routeWorkerStub{
				claim: app.ConversationRouteClaim{Message: conversation.Message{ID: "m1234567", ActorID: "u1234567", OwnerID: "u1234567",
					ConversationID: "c1234567", Content: "Explain this carefully", Role: conversation.RoleUser, Sequence: 1},
					Input: app.ConversationRoutingInput{ResponseMode: app.ConversationResponseLongForm}, LeaseExpiresAt: time.Now().Add(5 * time.Minute)},
				contextWindow: 32_768, capacityKnown: true,
				snapshot: app.ConversationRouteSnapshot{Context: app.ConversationRouteContext{ActorID: "u1234567", Content: "Explain this carefully"}},
			}
			actions := &longReplyRouteActionsStub{routeWorkerStub: base, cancelOn: test.cancelOn}
			models := &longReplyRouteGeneratorStub{routeWorkerStub: base, decision: test.decision, wait: test.wait}
			worker, err := NewConversationRouteWorker(ConversationRouteWorkerOptions{Queue: base, Actions: actions, Models: models,
				WorkerID: "long-reply-test", LeaseDuration: time.Minute, PollInterval: time.Millisecond, MaxConcurrent: 1, Logger: zap.NewNop()})
			if err != nil {
				t.Fatal(err)
			}
			found, err := worker.RunOnce(context.Background())
			if err != nil || !found || models.calls != 1 || base.classifies != 0 || len(base.applied) != 1 {
				t.Fatalf("long-form route result found=%t err=%v generator_calls=%d classifier_calls=%d applied=%d",
					found, err, models.calls, base.classifies, len(base.applied))
			}
			if base.applied[0].Cancelled != test.wantCancel {
				t.Fatalf("cancelled decision = %t, want %t: %+v", base.applied[0].Cancelled, test.wantCancel, base.applied[0])
			}
			if !test.wantCancel && base.applied[0].Reply != test.decision.Reply {
				t.Fatalf("long-form reply = %q, want %q", base.applied[0].Reply, test.decision.Reply)
			}
		})
	}
}
