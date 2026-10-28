package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type AgentChildSession struct {
	TaskID            string
	SessionID         string
	ParentSessionID   string
	SpawnKey          string
	Step              task.Step
	CheckpointThrough int64
	CheckpointDigest  []byte
	AllowedTools      []string
	Background        bool
	CancelRequested   bool
}

type AgentChildSessionCreate struct {
	Fence           AgentInstanceFence
	ParentSessionID string
	SpawnKey        string
	RequestHash     []byte
	StepID          string
	SessionID       string
	Title           string
	Summary         string
	AllowedTools    []string
	Background      bool
}

type AgentChildSessionStore interface {
	CreateAgentChildSession(context.Context, AgentChildSessionCreate) (AgentChildSession, bool, error)
	GetAgentChildSession(context.Context, AgentInstanceFence, string, string) (AgentChildSession, error)
	ListBackgroundAgentChildSessions(context.Context, AgentInstanceFence) ([]AgentChildSession, error)
	RequestAgentChildSessionCancel(context.Context, AgentInstanceFence, string) (AgentChildSession, bool, error)
	SettleAgentChildSession(context.Context, AgentInstanceFence, string, string, task.StepStatus, int64, []byte, string) (AgentChildSession, bool, error)
}

type AgentChildSessionCommand struct {
	Fence           AgentInstanceFence
	ParentSessionID string
	SpawnKey        string
	Arguments       []byte
	Title           string
	Summary         string
	AllowedTools    []string
	Background      bool
}

type AgentChildSessionCancelCommand struct {
	Fence          AgentInstanceFence
	ActorSessionID string
	SessionID      string
}

type AgentChildSessionSettleCommand struct {
	Fence             AgentInstanceFence
	ActorSessionID    string
	SessionID         string
	Status            task.StepStatus
	CheckpointThrough int64
	CheckpointDigest  []byte
	Summary           string
}

type AgentChildSessionService struct {
	store AgentChildSessionStore
}

func NewAgentChildSessionService(store AgentChildSessionStore) (*AgentChildSessionService, error) {
	if store == nil {
		return nil, domain.InvalidArgument("dependencies", "child session store is required")
	}
	return &AgentChildSessionService{store: store}, nil
}

func (s *AgentChildSessionService) Create(ctx context.Context, command AgentChildSessionCommand) (AgentChildSession, bool, error) {
	var arguments map[string]json.RawMessage
	argumentsValid := len(command.Arguments) > 0 && len(command.Arguments) <= 64*1024 &&
		json.Unmarshal(command.Arguments, &arguments) == nil && arguments != nil
	if err := command.Fence.Validate(); err != nil ||
		!agentCheckpointIDPattern.MatchString(command.ParentSessionID) ||
		!agentCheckpointIDPattern.MatchString(command.SpawnKey) ||
		len(command.Arguments) == 0 || len(command.Arguments) > 64*1024 || !argumentsValid ||
		invalidCheckpointLabel(strings.TrimSpace(command.Title), 120) || len(command.Summary) > 2048 || !utf8.ValidString(command.Summary) ||
		!validAgentChildToolNames(command.AllowedTools) {
		return AgentChildSession{}, false, domain.InvalidArgument("child_session", "child session fields are invalid")
	}
	allowedTools := append([]string{}, command.AllowedTools...)
	sort.Strings(allowedTools)
	stepID, err := domain.NewStepID()
	if err != nil {
		return AgentChildSession{}, false, fmt.Errorf("generate child TaskStep id: %w", err)
	}
	requestHash := sha256.Sum256(command.Arguments)
	return s.store.CreateAgentChildSession(ctx, AgentChildSessionCreate{
		Fence: command.Fence, ParentSessionID: command.ParentSessionID, SpawnKey: command.SpawnKey,
		RequestHash: requestHash[:], StepID: stepID, SessionID: "child_" + stepID,
		Title: strings.TrimSpace(command.Title), Summary: strings.TrimSpace(command.Summary), AllowedTools: allowedTools,
		Background: command.Background,
	})
}

func (s *AgentChildSessionService) List(ctx context.Context, fence AgentInstanceFence, actorSessionID string) ([]AgentChildSession, error) {
	if err := fence.Validate(); err != nil || actorSessionID != fence.AgentSessionID ||
		!agentCheckpointIDPattern.MatchString(actorSessionID) {
		return nil, domain.InvalidArgument("child_session", "background job actor is invalid")
	}
	return s.store.ListBackgroundAgentChildSessions(ctx, fence)
}

func (s *AgentChildSessionService) RequestCancel(ctx context.Context, command AgentChildSessionCancelCommand) (AgentChildSession, bool, error) {
	if err := command.Fence.Validate(); err != nil || command.ActorSessionID != command.Fence.AgentSessionID ||
		!agentCheckpointIDPattern.MatchString(command.ActorSessionID) || !agentCheckpointIDPattern.MatchString(command.SessionID) ||
		command.SessionID == command.ActorSessionID {
		return AgentChildSession{}, false, domain.InvalidArgument("child_session", "background job cancellation is invalid")
	}
	return s.store.RequestAgentChildSessionCancel(ctx, command.Fence, command.SessionID)
}

func validAgentChildToolNames(names []string) bool {
	if len(names) > 64 {
		return false
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if !agentCheckpointIDPattern.MatchString(name) {
			return false
		}
		if _, exists := seen[name]; exists {
			return false
		}
		seen[name] = struct{}{}
	}
	return true
}

func (s *AgentChildSessionService) Get(ctx context.Context, fence AgentInstanceFence, actorSessionID, sessionID string) (AgentChildSession, error) {
	if err := fence.Validate(); err != nil || !agentCheckpointIDPattern.MatchString(actorSessionID) ||
		!agentCheckpointIDPattern.MatchString(sessionID) || sessionID == fence.AgentSessionID || actorSessionID == sessionID {
		return AgentChildSession{}, domain.InvalidArgument("child_session", "child session identity is invalid")
	}
	return s.store.GetAgentChildSession(ctx, fence, actorSessionID, sessionID)
}

func (s *AgentChildSessionService) Settle(ctx context.Context, command AgentChildSessionSettleCommand) (AgentChildSession, bool, error) {
	if err := command.Fence.Validate(); err != nil || !agentCheckpointIDPattern.MatchString(command.ActorSessionID) ||
		!agentCheckpointIDPattern.MatchString(command.SessionID) || command.SessionID == command.Fence.AgentSessionID ||
		command.ActorSessionID == command.SessionID || command.CheckpointThrough < -1 || command.CheckpointThrough > 1<<53-1 ||
		(command.CheckpointThrough == -1 && len(command.CheckpointDigest) != 0) ||
		(command.CheckpointThrough >= 0 && len(command.CheckpointDigest) != sha256.Size) ||
		(command.Status == task.StepSucceeded && command.CheckpointThrough < 0) ||
		(command.Status != task.StepSucceeded && command.Status != task.StepFailed && command.Status != task.StepUnknown) ||
		len(command.Summary) > 2048 || !utf8.ValidString(command.Summary) {
		return AgentChildSession{}, false, domain.InvalidArgument("child_session", "child session settlement is invalid")
	}
	return s.store.SettleAgentChildSession(ctx, command.Fence, command.ActorSessionID, command.SessionID,
		command.Status, command.CheckpointThrough, command.CheckpointDigest, strings.TrimSpace(command.Summary))
}
