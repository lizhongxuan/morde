package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"time"

	"morde/server/internal/domain"
)

const (
	MaxAdminAgentCheckpointPageBytes = maxAgentCheckpointBytes
	MaxAdminAgentCheckpointSessions  = 128

	AgentCheckpointCoverageComplete = "complete"
	AgentCheckpointCoveragePartial  = "partial"
	AgentCheckpointCoverageEmpty    = "empty"
)

var adminAgentCheckpointSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type AgentCheckpointAdminPage struct {
	TaskID        string          `json:"task_id"`
	OwnerID       string          `json:"owner_id"`
	SessionID     string          `json:"session_id"`
	RootSessionID string          `json:"root_session_id"`
	From          int64           `json:"from_seq"`
	Through       int64           `json:"through_seq"`
	Watermark     int64           `json:"watermark"`
	NextFrom      int64           `json:"next_from_seq"`
	Coverage      string          `json:"coverage"`
	Reason        string          `json:"reason,omitempty"`
	GapAt         *int64          `json:"gap_at,omitempty"`
	ObservedFrom  *int64          `json:"observed_from,omitempty"`
	Digest        string          `json:"digest,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
	CreatedAt     *time.Time      `json:"created_at,omitempty"`
}

type AgentCheckpointAdminSession struct {
	SessionID         string    `json:"session_id"`
	ParentSessionID   string    `json:"parent_session_id,omitempty"`
	RootSessionID     string    `json:"root_session_id"`
	StepID            string    `json:"step_id,omitempty"`
	CheckpointThrough int64     `json:"checkpoint_through_seq"`
	CreatedAt         time.Time `json:"created_at"`
}

type StoredAgentCheckpointAdminPage struct {
	TaskID        string
	OwnerID       string
	SessionID     string
	RootSessionID string
	Watermark     int64
	Checkpoint    *StoredAgentCheckpoint
	CreatedAt     *time.Time
	Reason        string
	GapAt         *int64
	ObservedFrom  *int64
}

// AdminAgentCheckpointHistoryStore reads committed checkpoint history without
// requiring an active agent lease. Implementations must bind every page to the
// Task, owner, Agent session tree, and stored checkpoint identity.
type AdminAgentCheckpointHistoryStore interface {
	ReadAdminAgentCheckpointPage(context.Context, string, string, string, int64) (StoredAgentCheckpointAdminPage, error)
	ListAdminAgentCheckpointSessions(context.Context, string, string) ([]AgentCheckpointAdminSession, error)
}

// ReadAdminPage is for an admin-authorized observability handler. It returns
// one bounded, committed checkpoint segment and never mutates checkpoint data.
func (s *AgentCheckpointService) ReadAdminPage(ctx context.Context, taskID, ownerID, sessionID string,
	from int64) (AgentCheckpointAdminPage, error) {
	if !domain.ValidTaskID(taskID) || !domain.ValidUserID(ownerID) || !adminAgentCheckpointSessionID.MatchString(sessionID) || from < 0 {
		return AgentCheckpointAdminPage{}, domain.InvalidArgument("checkpoint_history", "checkpoint history request is invalid")
	}
	reader, ok := s.store.(AdminAgentCheckpointHistoryStore)
	if !ok {
		return AgentCheckpointAdminPage{}, domain.NewError(domain.CodeUnsupported, "checkpoint history is unavailable")
	}
	stored, err := reader.ReadAdminAgentCheckpointPage(ctx, taskID, ownerID, sessionID, from)
	if err != nil {
		return AgentCheckpointAdminPage{}, err
	}
	page := AgentCheckpointAdminPage{
		TaskID: taskID, OwnerID: ownerID, SessionID: sessionID, RootSessionID: stored.RootSessionID,
		From: from, Through: from - 1, NextFrom: from, Watermark: stored.Watermark,
		Coverage: AgentCheckpointCoverageComplete,
		Reason:   stored.Reason, GapAt: stored.GapAt, ObservedFrom: stored.ObservedFrom,
	}
	if stored.TaskID != taskID || stored.OwnerID != ownerID || stored.SessionID != sessionID ||
		!adminAgentCheckpointSessionID.MatchString(stored.RootSessionID) || stored.Watermark < -1 {
		return AgentCheckpointAdminPage{}, domain.NewError(domain.CodeInvalidState, "checkpoint history identity is invalid")
	}
	if stored.Watermark < math.MaxInt64 && from > stored.Watermark+1 {
		return AgentCheckpointAdminPage{}, domain.InvalidArgument("from_seq", "checkpoint history cursor is beyond the committed range")
	}
	if stored.Checkpoint == nil {
		if stored.Watermark == -1 && stored.Reason == "" {
			page.Coverage = AgentCheckpointCoverageEmpty
			page.Reason = "no_committed_checkpoint"
			return page, nil
		}
		if stored.Reason == "checkpoint_gap" && stored.GapAt != nil && *stored.GapAt == from &&
			(stored.ObservedFrom == nil || *stored.ObservedFrom > from) {
			page.Coverage = AgentCheckpointCoveragePartial
			return page, nil
		}
		return AgentCheckpointAdminPage{}, domain.NewError(domain.CodeInvalidState, "checkpoint history page result is inconsistent")
	}
	checkpoint := *stored.Checkpoint
	if checkpoint.Fence.TaskID != taskID || checkpoint.Fence.OwnerID != ownerID || checkpoint.Fence.AgentSessionID != sessionID ||
		checkpoint.Fence.AgentRootSessionID != stored.RootSessionID || checkpoint.From > from || checkpoint.Through < from ||
		checkpoint.Through == math.MaxInt64 || checkpoint.Through > stored.Watermark || len(checkpoint.Ciphertext) == 0 ||
		len(checkpoint.Ciphertext) > MaxAdminAgentCheckpointPageBytes+64 || len(checkpoint.Digest) != sha256.Size || stored.CreatedAt == nil {
		return AgentCheckpointAdminPage{}, domain.NewError(domain.CodeInvalidState, "checkpoint history segment is invalid")
	}
	payload, err := s.Decode(checkpoint)
	if err != nil {
		return AgentCheckpointAdminPage{}, domain.NewError(domain.CodeInvalidState, "stored checkpoint integrity validation failed")
	}
	if len(payload) == 0 || len(payload) > MaxAdminAgentCheckpointPageBytes || !json.Valid(payload) {
		return AgentCheckpointAdminPage{}, domain.NewError(domain.CodeInvalidState, "stored checkpoint payload is invalid")
	}
	page.Through = checkpoint.Through
	page.NextFrom = checkpoint.Through + 1
	page.Digest = hex.EncodeToString(checkpoint.Digest)
	page.Payload = append(json.RawMessage(nil), payload...)
	createdAt := stored.CreatedAt.UTC()
	page.CreatedAt = &createdAt
	if stored.Reason != "" {
		if stored.Reason != "checkpoint_gap" || stored.GapAt == nil || *stored.GapAt != page.NextFrom ||
			(stored.ObservedFrom != nil && *stored.ObservedFrom <= page.NextFrom) {
			return AgentCheckpointAdminPage{}, domain.NewError(domain.CodeInvalidState, "checkpoint history gap metadata is invalid")
		}
		page.Coverage = AgentCheckpointCoveragePartial
	}
	return page, nil
}

// ListAdminSessions returns the bounded Agent session tree for one Task. It is
// intended for an admin-authorized handler that separately enforces admin ACLs.
func (s *AgentCheckpointService) ListAdminSessions(ctx context.Context, taskID, ownerID string) ([]AgentCheckpointAdminSession, error) {
	if !domain.ValidTaskID(taskID) || !domain.ValidUserID(ownerID) {
		return nil, domain.InvalidArgument("task", "task identity is invalid")
	}
	reader, ok := s.store.(AdminAgentCheckpointHistoryStore)
	if !ok {
		return nil, domain.NewError(domain.CodeUnsupported, "checkpoint history is unavailable")
	}
	sessions, err := reader.ListAdminAgentCheckpointSessions(ctx, taskID, ownerID)
	if err != nil {
		return nil, err
	}
	if len(sessions) > MaxAdminAgentCheckpointSessions {
		return nil, domain.NewError(domain.CodeResultTooLarge, "agent session history exceeds the page limit")
	}
	for _, session := range sessions {
		if !adminAgentCheckpointSessionID.MatchString(session.SessionID) ||
			!adminAgentCheckpointSessionID.MatchString(session.RootSessionID) || session.CheckpointThrough < -1 ||
			(session.ParentSessionID != "" && !adminAgentCheckpointSessionID.MatchString(session.ParentSessionID)) {
			return nil, domain.NewError(domain.CodeInvalidState, "agent session history is invalid")
		}
	}
	return sessions, nil
}
