package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"testing"

	"morde/server/internal/domain/task"
)

type agentChildSessionStoreStub struct {
	request       AgentChildSessionCreate
	settleStatus  task.StepStatus
	settleText    string
	settleThrough int64
	settleDigest  []byte
	cancelCalls   int
	listCalls     int
	calls         int
	getCalls      int
	settleCalls   int
}

func (s *agentChildSessionStoreStub) CreateAgentChildSession(_ context.Context, request AgentChildSessionCreate) (AgentChildSession, bool, error) {
	s.request = request
	s.calls++
	return AgentChildSession{TaskID: request.Fence.TaskID, SessionID: request.SessionID, SpawnKey: request.SpawnKey,
		ParentSessionID: request.ParentSessionID, AllowedTools: append([]string(nil), request.AllowedTools...),
		Background: request.Background,
		Step:       task.Step{ID: request.StepID, TaskID: request.Fence.TaskID}}, false, nil
}

func (s *agentChildSessionStoreStub) GetAgentChildSession(_ context.Context, fence AgentInstanceFence, actorSessionID, sessionID string) (AgentChildSession, error) {
	s.getCalls++
	return AgentChildSession{TaskID: fence.TaskID, SessionID: sessionID, ParentSessionID: actorSessionID, SpawnKey: "call_1"}, nil
}

func (s *agentChildSessionStoreStub) ListBackgroundAgentChildSessions(_ context.Context, fence AgentInstanceFence) ([]AgentChildSession, error) {
	s.listCalls++
	return []AgentChildSession{{TaskID: fence.TaskID, SessionID: "child_1", ParentSessionID: fence.AgentSessionID,
		SpawnKey: "call_1", Background: true}}, nil
}

func (s *agentChildSessionStoreStub) RequestAgentChildSessionCancel(_ context.Context, fence AgentInstanceFence, sessionID string) (AgentChildSession, bool, error) {
	s.cancelCalls++
	return AgentChildSession{TaskID: fence.TaskID, SessionID: sessionID, ParentSessionID: fence.AgentSessionID, SpawnKey: "call_1",
		Background: true, CancelRequested: true, Step: task.Step{ID: "step_1", TaskID: fence.TaskID, Status: task.StepRunning}}, s.cancelCalls > 1, nil
}

func (s *agentChildSessionStoreStub) SettleAgentChildSession(_ context.Context, fence AgentInstanceFence, actorSessionID, sessionID string,
	status task.StepStatus, through int64, digest []byte, summary string) (AgentChildSession, bool, error) {
	s.settleCalls++
	s.settleStatus, s.settleText = status, summary
	s.settleThrough, s.settleDigest = through, append([]byte(nil), digest...)
	return AgentChildSession{TaskID: fence.TaskID, SessionID: sessionID, SpawnKey: "call_1",
		ParentSessionID: actorSessionID, CheckpointThrough: through, CheckpointDigest: append([]byte(nil), digest...),
		Step: task.Step{ID: "step_1", TaskID: fence.TaskID, Status: status, Summary: summary}}, false, nil
}

func TestAgentChildSessionServiceHashesArgumentsAndRejectsNonObject(t *testing.T) {
	store := &agentChildSessionStoreStub{}
	service, err := NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	command := AgentChildSessionCommand{
		Fence: AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", AgentInstanceID: "agent_1", AgentGeneration: 1,
			AgentSessionID: "session_root", SupervisorID: "supervisor_1", LeaseOwner: "worker_1", LeaseVersion: 1,
			AuthorizationVersion: 1, EngineVersion: "engine", ProfileVersion: "profile"},
		ParentSessionID: "session_root", SpawnKey: "call_1", Arguments: []byte(`{"instruction":"read"}`),
		Title: " Read file ", Summary: "safe child task", AllowedTools: []string{"file_read", "workspace_search"}, Background: true,
	}
	created, replayed, err := service.Create(context.Background(), command)
	if err != nil || replayed || store.calls != 1 {
		t.Fatalf("create child session = %#v replayed=%t calls=%d err=%v", created, replayed, store.calls, err)
	}
	digest := sha256.Sum256(command.Arguments)
	if !bytes.Equal(store.request.RequestHash, digest[:]) || store.request.SessionID != "child_"+store.request.StepID ||
		store.request.Title != "Read file" || store.request.Summary != command.Summary || !store.request.Background ||
		len(store.request.AllowedTools) != 2 || store.request.AllowedTools[0] != "file_read" || store.request.AllowedTools[1] != "workspace_search" {
		t.Fatalf("stored child request = %#v", store.request)
	}
	child, err := service.Get(context.Background(), command.Fence, command.Fence.AgentSessionID, "child_1")
	if err != nil || store.getCalls != 1 || child.ParentSessionID != command.Fence.AgentSessionID {
		t.Fatalf("get child session = %#v calls=%d err=%v", child, store.getCalls, err)
	}
	digest = sha256.Sum256([]byte("checkpoint"))
	settled, replayed, err := service.Settle(context.Background(), AgentChildSessionSettleCommand{
		Fence: command.Fence, ActorSessionID: command.Fence.AgentSessionID, SessionID: "child_1",
		Status: task.StepSucceeded, CheckpointThrough: 0, CheckpointDigest: digest[:], Summary: " done ",
	})
	if err != nil || replayed || settled.Step.Status != task.StepSucceeded || settled.Step.Summary != "done" ||
		store.settleCalls != 1 || store.settleStatus != task.StepSucceeded || store.settleText != "done" ||
		store.settleThrough != 0 || !bytes.Equal(store.settleDigest, digest[:]) {
		t.Fatalf("settle child session = %#v replayed=%t calls=%d err=%v", settled, replayed, store.settleCalls, err)
	}
	if _, _, err := service.Settle(context.Background(), AgentChildSessionSettleCommand{
		Fence: command.Fence, ActorSessionID: command.Fence.AgentSessionID, SessionID: "child_1", Status: task.StepQueued,
	}); err == nil || store.settleCalls != 1 {
		t.Fatalf("invalid child settlement reached store: calls=%d err=%v", store.settleCalls, err)
	}
	if _, _, err := service.Settle(context.Background(), AgentChildSessionSettleCommand{
		Fence: command.Fence, ActorSessionID: command.Fence.AgentSessionID, SessionID: "child_1",
		Status: task.StepSucceeded, CheckpointThrough: -1,
	}); err == nil || store.settleCalls != 1 {
		t.Fatalf("child succeeded without a committed checkpoint: calls=%d err=%v", store.settleCalls, err)
	}
	command.Arguments = []byte(`null`)
	if _, _, err := service.Create(context.Background(), command); err == nil || store.calls != 1 {
		t.Fatalf("non-object child arguments reached store: calls=%d err=%v", store.calls, err)
	}
}

func TestAgentChildSessionBackgroundListingAndCancelFenceRoot(t *testing.T) {
	store := &agentChildSessionStoreStub{}
	service, err := NewAgentChildSessionService(store)
	if err != nil {
		t.Fatal(err)
	}
	fence := AgentInstanceFence{TaskID: "q1234567", OwnerID: "u1234567", AgentInstanceID: "agent_1", AgentGeneration: 1,
		AgentSessionID: "session_root", SupervisorID: "supervisor_1", LeaseOwner: "worker_1", LeaseVersion: 1,
		AuthorizationVersion: 1, EngineVersion: "engine", ProfileVersion: "profile"}
	jobs, err := service.List(context.Background(), fence, fence.AgentSessionID)
	if err != nil || store.listCalls != 1 || len(jobs) != 1 || !jobs[0].Background {
		t.Fatalf("list background jobs = %#v calls=%d err=%v", jobs, store.listCalls, err)
	}
	if _, err := service.List(context.Background(), fence, "child_1"); err == nil || store.listCalls != 1 {
		t.Fatalf("child session listed jobs outside root: calls=%d err=%v", store.listCalls, err)
	}
	job, replayed, err := service.RequestCancel(context.Background(), AgentChildSessionCancelCommand{
		Fence: fence, ActorSessionID: fence.AgentSessionID, SessionID: "child_1",
	})
	if err != nil || replayed || !job.Background || !job.CancelRequested || job.Step.Status != task.StepRunning || store.cancelCalls != 1 {
		t.Fatalf("request cancel = %#v replayed=%t calls=%d err=%v", job, replayed, store.cancelCalls, err)
	}
	if _, _, err := service.RequestCancel(context.Background(), AgentChildSessionCancelCommand{
		Fence: fence, ActorSessionID: "child_1", SessionID: "child_2",
	}); err == nil || store.cancelCalls != 1 {
		t.Fatalf("non-root cancel reached store: calls=%d err=%v", store.cancelCalls, err)
	}
}
