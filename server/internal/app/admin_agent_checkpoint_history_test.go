package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/domain"
)

type adminCheckpointHistoryTestStore struct {
	*checkpointStoreStub
	page     StoredAgentCheckpointAdminPage
	sessions []AgentCheckpointAdminSession
}

func (s *adminCheckpointHistoryTestStore) ReadAdminAgentCheckpointPage(_ context.Context, taskID, ownerID, sessionID string,
	_ int64) (StoredAgentCheckpointAdminPage, error) {
	s.page.TaskID, s.page.OwnerID, s.page.SessionID = taskID, ownerID, sessionID
	return s.page, nil
}

func (s *adminCheckpointHistoryTestStore) ListAdminAgentCheckpointSessions(context.Context, string, string) ([]AgentCheckpointAdminSession, error) {
	return s.sessions, nil
}

func TestAgentCheckpointAdminPageDecodesAndReportsCoverage(t *testing.T) {
	base := &checkpointStoreStub{}
	store := &adminCheckpointHistoryTestStore{checkpointStoreStub: base}
	sealer, err := NewSealer(bytes.Repeat([]byte{0x3a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	command := checkpointTestCommand(`{"header":{"id":"session_1"},"from_seq":0,"events":[{"seq":0,"text":"private"},{"seq":1,"text":"thought"}]}`)
	command.Fence.AgentSessionID = "session_1"
	command.Fence.AgentInstanceID = "agent_1"
	if _, err := service.Save(context.Background(), command); err != nil {
		t.Fatalf("save encrypted checkpoint: %v", err)
	}
	createdAt := time.Date(2026, 11, 27, 1, 2, 3, 0, time.UTC)
	store.page = StoredAgentCheckpointAdminPage{
		TaskID: command.Fence.TaskID, OwnerID: command.Fence.OwnerID, SessionID: command.Fence.AgentSessionID,
		RootSessionID: command.Fence.AgentSessionID, Watermark: command.Through,
		Checkpoint: &base.value, CreatedAt: &createdAt,
	}
	page, err := service.ReadAdminPage(context.Background(), command.Fence.TaskID, command.Fence.OwnerID, command.Fence.AgentSessionID, 0)
	if err != nil || page.Coverage != AgentCheckpointCoverageComplete || page.From != 0 || page.Through != 1 ||
		page.Watermark != 1 || page.NextFrom != 2 || page.Digest == "" || string(page.Payload) != string(command.Payload) || page.CreatedAt == nil {
		t.Fatalf("read admin page: page=%+v err=%v", page, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(page.Payload, &decoded); err != nil || decoded["header"] == nil {
		t.Fatalf("admin payload is not raw checkpoint JSON: %s err=%v", page.Payload, err)
	}
	interior, err := service.ReadAdminPage(context.Background(), command.Fence.TaskID, command.Fence.OwnerID, command.Fence.AgentSessionID, 1)
	if err != nil || interior.From != 1 || interior.Through != 1 || interior.NextFrom != 2 || string(interior.Payload) != string(command.Payload) {
		t.Fatalf("read from inside a checkpoint segment: page=%+v err=%v", interior, err)
	}

	store.page.Checkpoint = nil
	store.page.Reason = "checkpoint_gap"
	gapAt := int64(0)
	observed := int64(2)
	store.page.GapAt, store.page.ObservedFrom = &gapAt, &observed
	partial, err := service.ReadAdminPage(context.Background(), command.Fence.TaskID, command.Fence.OwnerID, command.Fence.AgentSessionID, 0)
	if err != nil || partial.Coverage != AgentCheckpointCoveragePartial || partial.Reason != "checkpoint_gap" ||
		partial.GapAt == nil || *partial.GapAt != 0 || partial.ObservedFrom == nil || *partial.ObservedFrom != 2 {
		t.Fatalf("report checkpoint gap: page=%+v err=%v", partial, err)
	}
	store.page.Checkpoint = &base.value
	store.page.Reason, store.page.GapAt, store.page.ObservedFrom = "", nil, nil
	corrupted := *store.page.Checkpoint
	wrongDigest := sha256.Sum256([]byte("different"))
	corrupted.Digest = wrongDigest[:]
	store.page.Checkpoint = &corrupted
	if _, err := service.ReadAdminPage(context.Background(), command.Fence.TaskID, command.Fence.OwnerID, command.Fence.AgentSessionID, 0); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("corrupt checkpoint digest returned %v", err)
	}
}

func TestAgentCheckpointAdminSessionsAreBoundedAndValidated(t *testing.T) {
	store := &adminCheckpointHistoryTestStore{checkpointStoreStub: &checkpointStoreStub{}, sessions: []AgentCheckpointAdminSession{{
		SessionID: "session_1", RootSessionID: "session_1", CheckpointThrough: -1,
	}}}
	service, err := NewAgentCheckpointService(store, mustTestSealer(t))
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := service.ListAdminSessions(context.Background(), "q1234567", "u1234567")
	if err != nil || len(sessions) != 1 || sessions[0].SessionID != "session_1" {
		t.Fatalf("list admin sessions = %+v, %v", sessions, err)
	}
	store.sessions = make([]AgentCheckpointAdminSession, MaxAdminAgentCheckpointSessions+1)
	if _, err := service.ListAdminSessions(context.Background(), "q1234567", "u1234567"); domain.CodeOf(err) != domain.CodeResultTooLarge {
		t.Fatalf("oversized session tree returned %v", err)
	}
	store.sessions = []AgentCheckpointAdminSession{{SessionID: "bad id", RootSessionID: "session_1", CheckpointThrough: -1}}
	if _, err := service.ListAdminSessions(context.Background(), "q1234567", "u1234567"); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("invalid session identity returned %v", err)
	}
}

func mustTestSealer(t *testing.T) *Sealer {
	t.Helper()
	sealer, err := NewSealer(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return sealer
}
