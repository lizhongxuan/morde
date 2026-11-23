package worker

import (
	"context"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

type internalRouteToolStoreStub struct {
	call           app.ConversationRouteInternalToolCall
	details        app.TaskDetails
	progress       *app.TaskProgress
	pendingReplies bool
}

func (s *internalRouteToolStoreStub) GetConversationRouteInternalToolCall(context.Context, string, string) (app.ConversationRouteInternalToolCall, error) {
	return s.call, nil
}

func (s *internalRouteToolStoreStub) PublishPendingConversationRouteInternalToolReplies(context.Context) (bool, error) {
	return s.pendingReplies, nil
}

func (s *internalRouteToolStoreStub) GetTask(context.Context, string, string) (app.TaskDetails, error) {
	return s.details, nil
}

func (s *internalRouteToolStoreStub) CommitTaskProgress(_ context.Context, progress app.TaskProgress) (task.Event, error) {
	s.progress = &progress
	return progress.Event, nil
}

type internalRouteToolBrokerStub struct {
	store  *internalRouteToolStoreStub
	result app.AgentToolResult
	call   app.ModelToolCall
	called int
}

func (b *internalRouteToolBrokerStub) Execute(_ context.Context, _ task.Task, call app.ModelToolCall) (app.AgentToolResult, error) {
	b.called++
	b.call = call
	if b.result.Status == app.AgentToolUnknown {
		b.store.details.Task.Status = task.StatusWaitingUser
		b.store.details.Task.WaitingReason = "mcp_result_unknown"
	}
	return b.result, nil
}

func TestConversationRouteInternalToolExecutorCommitsBrokerResult(t *testing.T) {
	store := &internalRouteToolStoreStub{
		call: app.ConversationRouteInternalToolCall{TaskID: "q1234567", OwnerID: "u1234567", MessageID: "m1234567",
			CallID: "morde_route_tool_m1234567", ToolName: "morde_web_search", Arguments: `{"query":"public topic"}`},
		details: app.TaskDetails{Task: task.Task{ID: "q1234567", OwnerID: "u1234567", Status: task.StatusRunning}},
	}
	broker := &internalRouteToolBrokerStub{store: store, result: app.AgentToolResult{Status: app.AgentToolSucceeded,
		Output: `{"text":["Located two authoritative results."]}`}}
	executor, err := NewConversationRouteInternalToolExecutor(store, broker)
	if err != nil {
		t.Fatal(err)
	}
	claim := task.Task{ID: "q1234567", OwnerID: "u1234567", Status: task.StatusRunning, InternalTool: true,
		Lease: task.Lease{Owner: "worker-1", Version: 1}}
	if err := executor.ExecuteInternalToolTask(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if broker.called != 1 || broker.call.CallID != store.call.CallID || broker.call.Name != store.call.ToolName || broker.call.Arguments != store.call.Arguments {
		t.Fatalf("Broker did not receive the persisted one-call payload: calls=%d call=%+v", broker.called, broker.call)
	}
	if store.progress == nil || store.progress.Status != task.StatusCompleted || !store.progress.PublishResultMessage ||
		store.progress.Event.Type != "task.result" || !strings.Contains(store.progress.Result, "Located two authoritative results") ||
		len(store.progress.Steps) != 1 || store.progress.Steps[0].Status != task.StepSucceeded {
		t.Fatalf("Broker result was not committed as a user-visible successful reply: %+v", store.progress)
	}
}

func TestConversationRouteInternalToolExecutorPublishesUnknownResultWithoutCompleting(t *testing.T) {
	store := &internalRouteToolStoreStub{
		call: app.ConversationRouteInternalToolCall{TaskID: "q1234567", OwnerID: "u1234567", MessageID: "m1234567",
			CallID: "morde_route_tool_m1234567", ToolName: "morde_web_search", Arguments: `{"query":"public topic"}`},
		details: app.TaskDetails{Task: task.Task{ID: "q1234567", OwnerID: "u1234567", Status: task.StatusRunning}}, pendingReplies: true,
	}
	broker := &internalRouteToolBrokerStub{store: store, result: app.AgentToolResult{Status: app.AgentToolUnknown}}
	executor, err := NewConversationRouteInternalToolExecutor(store, broker)
	if err != nil {
		t.Fatal(err)
	}
	claim := task.Task{ID: "q1234567", OwnerID: "u1234567", Status: task.StatusRunning, InternalTool: true,
		Lease: task.Lease{Owner: "worker-1", Version: 1}}
	if err := executor.ExecuteInternalToolTask(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if store.progress != nil {
		t.Fatalf("unknown Broker result was falsely completed: %+v", store.progress)
	}
	published, err := executor.PublishPendingReplies(context.Background())
	if err != nil || !published || broker.called != 1 {
		t.Fatalf("unknown Broker result was not published from the durable waiting state: published=%t calls=%d err=%v", published, broker.called, err)
	}
}
