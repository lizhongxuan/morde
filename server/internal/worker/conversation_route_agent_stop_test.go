package worker

import (
	"context"
	"errors"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestConversationRouteAgentControlsStopIntent(t *testing.T) {
	value := app.ConversationRouteContext{ActorID: "u1234567", Content: "别继续跑了，把我现在所有正在进行的工作都结束吧"}
	decision := app.ParseConversationRouteDecisionForContext(`{"action":"stop_all"}`, value)
	if got := safeConversationRouteDecision(value, decision); got.Action != app.ConversationRouteStopAll {
		t.Fatalf("Agent stop-all decision was rejected: %+v", got)
	}

	value.Content = "请解释‘停止全部任务’的含义"
	decision = app.ParseConversationRouteDecisionForContext(`{"action":"reply","reply":"这表示结束所有任务。"}`, value)
	if got := safeConversationRouteDecision(value, decision); got.Action == app.ConversationRouteStopAll || got.Action == app.ConversationRouteStop {
		t.Fatalf("a question became a stop action: %+v", got)
	}
}

func TestConversationRouteDoesNotStopWithoutMatchingAgentDecision(t *testing.T) {
	value := app.ConversationRouteContext{ActorID: "u1234567", Content: "停止全部任务", Candidates: []app.ConversationRouteCandidate{
		{TaskID: "q1234567", InitiatorID: "u1234567", Status: task.StatusRunning, Revision: 1},
	}}
	for _, output := range []string{"", `{"action":"reply","reply":"好的。"}`, `{"action":"stop","task_id":"q1234567"}`} {
		decision := app.ConversationRouteDecision{Action: app.ConversationRouteStopAll, ModelOutput: output}
		if got := safeConversationRouteDecision(value, decision); got.Action == app.ConversationRouteStopAll {
			t.Errorf("stop-all accepted without a matching Agent decision %q: %+v", output, got)
		}
	}

	worker, stub := newRouteWorkerTest(t)
	stub.snapshot.Context = value
	stub.classifyErr = errors.New("synthetic classifier unavailable")
	if _, err := worker.RunOnce(context.Background()); err != nil || len(stub.applied) != 1 ||
		stub.applied[0].Action == app.ConversationRouteStopAll || stub.applied[0].Action == app.ConversationRouteStop {
		t.Fatalf("classifier failure caused a stop: applied=%+v err=%v", stub.applied, err)
	}
}
