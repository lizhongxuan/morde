package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

const maxAgentChildSessionsPerTask = 16

func (s *Store) CreateAgentChildSession(ctx context.Context, request app.AgentChildSessionCreate) (app.AgentChildSession, bool, error) {
	fence := request.Fence
	if err := fence.Validate(); err != nil || request.ParentSessionID == "" ||
		request.SessionID == "" || request.SessionID == request.ParentSessionID || request.StepID == "" ||
		request.SpawnKey == "" || len(request.RequestHash) != 32 || request.Title == "" || !validStoredAgentToolNames(request.AllowedTools) {
		return app.AgentChildSession{}, false, domain.InvalidArgument("child_session", "child session fields are invalid")
	}
	toolNames := append([]string{}, request.AllowedTools...)
	allowedTools, err := json.Marshal(toolNames)
	if err != nil {
		return app.AgentChildSession{}, false, fmt.Errorf("encode child tool allowlist: %w", err)
	}
	var result app.AgentChildSession
	var replayed bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockCurrentAgentChildSessionTask(ctx, tx, fence); err != nil {
			return err
		}
		var parentOfParent, parentStepID string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(parent_session_id,''),COALESCE(step_id,'') FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 FOR UPDATE`, fence.TaskID, fence.OwnerID, request.ParentSessionID).
			Scan(&parentOfParent, &parentStepID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "parent Agent Session is unavailable")
		} else if err != nil {
			return fmt.Errorf("lock parent Agent Session: %w", err)
		}
		if request.ParentSessionID == fence.AgentSessionID {
			if parentOfParent != "" {
				return domain.NewError(domain.CodeUnauthenticated, "root Agent Session is unavailable")
			}
		} else {
			underRoot, err := agentSessionDescendsFrom(ctx, tx, fence.TaskID, fence.OwnerID, request.ParentSessionID, fence.AgentSessionID)
			if err != nil {
				return fmt.Errorf("verify child parent ancestry: %w", err)
			}
			if parentOfParent == "" || !underRoot {
				return domain.NewError(domain.CodeUnauthenticated, "parent Agent Session is outside the active agent tree")
			}
			var parentStepStatus string
			if err := tx.QueryRow(ctx, `SELECT status FROM task_steps WHERE task_id=$1 AND id=$2 FOR UPDATE`,
				fence.TaskID, parentStepID).Scan(&parentStepStatus); err != nil {
				return fmt.Errorf("lock parent Agent TaskStep: %w", err)
			}
			if parentStepStatus != string(task.StepRunning) {
				return domain.NewError(domain.CodeInvalidState, "parent Agent Session is not running")
			}
		}
		var existingStepID string
		var existingRequestHash, existingAllowedTools []byte
		if err := tx.QueryRow(ctx, `SELECT s.task_id,s.session_id,s.parent_session_id,s.spawn_key,s.step_id,s.checkpoint_through_seq,s.checkpoint_digest,s.allowed_tools,
s.is_background,s.cancel_requested_at IS NOT NULL,
st.sequence,st.kind,st.status,st.title,st.summary,st.created_at
FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.parent_session_id=$3 AND s.spawn_key=$4
			FOR UPDATE OF s,st`, fence.TaskID, fence.OwnerID, request.ParentSessionID, request.SpawnKey).Scan(
			&result.TaskID, &result.SessionID, &result.ParentSessionID, &result.SpawnKey, &existingStepID, &result.CheckpointThrough, &result.CheckpointDigest, &existingAllowedTools,
			&result.Background, &result.CancelRequested,
			&result.Step.Sequence, &result.Step.Kind, &result.Step.Status, &result.Step.Title, &result.Step.Summary,
			&result.Step.CreatedAt); err == nil {
			if err := tx.QueryRow(ctx, `SELECT request_hash FROM task_agent_sessions WHERE task_id=$1 AND session_id=$2`,
				fence.TaskID, result.SessionID).Scan(&existingRequestHash); err != nil {
				return fmt.Errorf("read existing child request hash: %w", err)
			}
			if !bytes.Equal(existingRequestHash, request.RequestHash) {
				return domain.NewError(domain.CodeIdempotencyConflict, "child spawn key was reused with different arguments")
			}
			if err := json.Unmarshal(existingAllowedTools, &result.AllowedTools); err != nil || !validStoredAgentToolNames(result.AllowedTools) {
				return errors.New("stored child tool allowlist is invalid")
			}
			if !sameAgentToolNames(result.AllowedTools, toolNames) {
				return domain.NewError(domain.CodeIdempotencyConflict, "child spawn key changed its tool allowlist")
			}
			if result.Background != request.Background {
				return domain.NewError(domain.CodeIdempotencyConflict, "child spawn key changed its execution mode")
			}
			result.Step.ID, result.Step.TaskID = existingStepID, fence.TaskID
			replayed = true
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read existing child Agent Session: %w", err)
		}
		var childCount int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_agent_sessions WHERE task_id=$1 AND parent_session_id IS NOT NULL`,
			fence.TaskID).Scan(&childCount); err != nil {
			return fmt.Errorf("count child Agent Sessions: %w", err)
		}
		if childCount >= maxAgentChildSessionsPerTask {
			return domain.NewError(domain.CodeQuotaExceeded, "Task child Agent limit exceeded")
		}

		var lastSequence int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0) FROM task_steps WHERE task_id=$1`, fence.TaskID).Scan(&lastSequence); err != nil {
			return fmt.Errorf("read last TaskStep sequence: %w", err)
		}
		if lastSequence == math.MaxInt64 {
			return domain.NewError(domain.CodeInvalidState, "TaskStep sequence is exhausted")
		}
		sequence := lastSequence + 1
		if err := tx.QueryRow(ctx, `INSERT INTO task_steps(id,task_id,sequence,kind,status,title,summary)
VALUES ($1,$2,$3,'subagent','running',$4,$5) RETURNING created_at`, request.StepID, fence.TaskID, sequence, request.Title, request.Summary).Scan(&result.Step.CreatedAt); err != nil {
			return fmt.Errorf("create child TaskStep: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,parent_session_id,step_id,spawn_key,request_hash,allowed_tools,is_background)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9)`, fence.TaskID, fence.OwnerID, request.SessionID, request.ParentSessionID,
			request.StepID, request.SpawnKey, request.RequestHash, string(allowedTools), request.Background); err != nil {
			return fmt.Errorf("create child Agent Session: %w", err)
		}
		sequenceNumber, err := incrementTaskEventSequence(ctx, tx, fence.TaskID)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]string{"phase": "subagent_started", "step_id": request.StepID,
			"session_id": request.SessionID, "parent_session_id": request.ParentSessionID})
		if err != nil {
			return fmt.Errorf("encode child session event: %w", err)
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: fence.TaskID, Sequence: sequenceNumber,
			Type: "task.step", Payload: payload}); err != nil {
			return err
		}
		result = app.AgentChildSession{TaskID: fence.TaskID, SessionID: request.SessionID, ParentSessionID: request.ParentSessionID, SpawnKey: request.SpawnKey,
			Step: task.Step{ID: request.StepID, TaskID: fence.TaskID, Sequence: sequence, Kind: "subagent", Status: task.StepRunning,
				Title: request.Title, Summary: request.Summary, CreatedAt: result.Step.CreatedAt},
			CheckpointThrough: -1, AllowedTools: toolNames, Background: request.Background}
		return nil
	})
	return result, replayed, err
}

func (s *Store) GetAgentChildSession(ctx context.Context, fence app.AgentInstanceFence, actorSessionID, sessionID string) (app.AgentChildSession, error) {
	if err := fence.Validate(); err != nil || sessionID == "" || sessionID == fence.AgentSessionID {
		return app.AgentChildSession{}, domain.InvalidArgument("child_session", "child session identity is invalid")
	}
	var result app.AgentChildSession
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockCurrentAgentChildSessionTask(ctx, tx, fence); err != nil {
			return err
		}
		if err := lockAgentChildSessionActor(ctx, tx, fence, actorSessionID); err != nil {
			return err
		}
		var parentSessionID string
		var stepStatus string
		var allowedTools []byte
		if err := tx.QueryRow(ctx, `SELECT s.task_id,s.session_id,s.parent_session_id,s.spawn_key,s.checkpoint_through_seq,s.checkpoint_digest,s.allowed_tools,
s.is_background,s.cancel_requested_at IS NOT NULL,
st.id,st.sequence,st.kind,st.status,st.title,st.summary,st.created_at
FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.session_id=$3 AND s.parent_session_id=$4
FOR UPDATE OF s,st`, fence.TaskID, fence.OwnerID, sessionID, actorSessionID).Scan(
			&result.TaskID, &result.SessionID, &parentSessionID, &result.SpawnKey, &result.CheckpointThrough, &result.CheckpointDigest, &allowedTools,
			&result.Background, &result.CancelRequested,
			&result.Step.ID, &result.Step.Sequence, &result.Step.Kind, &stepStatus, &result.Step.Title, &result.Step.Summary,
			&result.Step.CreatedAt); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "child Agent Session is unavailable")
		} else if err != nil {
			return fmt.Errorf("read child Agent Session: %w", err)
		}
		underRoot, err := agentSessionDescendsFrom(ctx, tx, fence.TaskID, fence.OwnerID, sessionID, fence.AgentSessionID)
		if err != nil {
			return fmt.Errorf("verify child Agent Session ancestry: %w", err)
		}
		if !underRoot {
			return domain.NewError(domain.CodeUnauthenticated, "child Agent Session is outside the active agent tree")
		}
		result.ParentSessionID, result.Step.TaskID = parentSessionID, result.TaskID
		result.Step.Status = task.StepStatus(stepStatus)
		if err := json.Unmarshal(allowedTools, &result.AllowedTools); err != nil || !validStoredAgentToolNames(result.AllowedTools) {
			return errors.New("stored child tool allowlist is invalid")
		}
		return nil
	})
	return result, err
}

func (s *Store) ListBackgroundAgentChildSessions(ctx context.Context, fence app.AgentInstanceFence) ([]app.AgentChildSession, error) {
	if err := fence.Validate(); err != nil {
		return nil, domain.InvalidArgument("child_session", "background job fence is invalid")
	}
	result := make([]app.AgentChildSession, 0)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockCurrentAgentChildSessionTask(ctx, tx, fence); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT s.task_id,s.session_id,s.parent_session_id,s.spawn_key,s.checkpoint_through_seq,s.checkpoint_digest,s.allowed_tools,
s.is_background,s.cancel_requested_at IS NOT NULL,
st.id,st.sequence,st.kind,st.status,st.title,st.summary,st.created_at
FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.parent_session_id=$3 AND s.is_background
ORDER BY st.sequence FOR UPDATE OF s,st`, fence.TaskID, fence.OwnerID, fence.AgentSessionID)
		if err != nil {
			return fmt.Errorf("list background Agent Sessions: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var child app.AgentChildSession
			var status string
			var allowedTools []byte
			if err := rows.Scan(&child.TaskID, &child.SessionID, &child.ParentSessionID, &child.SpawnKey, &child.CheckpointThrough, &child.CheckpointDigest,
				&allowedTools, &child.Background, &child.CancelRequested, &child.Step.ID, &child.Step.Sequence, &child.Step.Kind,
				&status, &child.Step.Title, &child.Step.Summary, &child.Step.CreatedAt); err != nil {
				return fmt.Errorf("scan background Agent Session: %w", err)
			}
			child.Step.TaskID, child.Step.Status = child.TaskID, task.StepStatus(status)
			if err := json.Unmarshal(allowedTools, &child.AllowedTools); err != nil || !validStoredAgentToolNames(child.AllowedTools) {
				return errors.New("stored background Agent tool allowlist is invalid")
			}
			result = append(result, child)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate background Agent Sessions: %w", err)
		}
		return nil
	})
	return result, err
}

func (s *Store) RequestAgentChildSessionCancel(ctx context.Context, fence app.AgentInstanceFence, sessionID string) (app.AgentChildSession, bool, error) {
	if err := fence.Validate(); err != nil || sessionID == "" || sessionID == fence.AgentSessionID {
		return app.AgentChildSession{}, false, domain.InvalidArgument("child_session", "background job cancellation is invalid")
	}
	var result app.AgentChildSession
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockCurrentAgentChildSessionTask(ctx, tx, fence); err != nil {
			return err
		}
		var parentSessionID, stepStatus string
		var allowedTools []byte
		if err := tx.QueryRow(ctx, `SELECT s.task_id,s.session_id,s.parent_session_id,s.spawn_key,s.checkpoint_through_seq,s.checkpoint_digest,s.allowed_tools,
s.is_background,s.cancel_requested_at IS NOT NULL,
st.id,st.sequence,st.kind,st.status,st.title,st.summary,st.created_at
FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.session_id=$3 AND s.parent_session_id=$4
FOR UPDATE OF s,st`, fence.TaskID, fence.OwnerID, sessionID, fence.AgentSessionID).Scan(
			&result.TaskID, &result.SessionID, &parentSessionID, &result.SpawnKey, &result.CheckpointThrough, &result.CheckpointDigest, &allowedTools,
			&result.Background, &result.CancelRequested, &result.Step.ID, &result.Step.Sequence, &result.Step.Kind, &stepStatus,
			&result.Step.Title, &result.Step.Summary, &result.Step.CreatedAt); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "background Agent Session is unavailable")
		} else if err != nil {
			return fmt.Errorf("lock background Agent Session for cancellation: %w", err)
		}
		result.ParentSessionID, result.Step.TaskID, result.Step.Status = parentSessionID, result.TaskID, task.StepStatus(stepStatus)
		if err := json.Unmarshal(allowedTools, &result.AllowedTools); err != nil || !validStoredAgentToolNames(result.AllowedTools) {
			return errors.New("stored background Agent tool allowlist is invalid")
		}
		if !result.Background {
			return domain.NewError(domain.CodeInvalidState, "Agent Session is not a background job")
		}
		if result.Step.Status != task.StepRunning {
			if result.CancelRequested {
				replayed = true
				return nil
			}
			return domain.NewError(domain.CodeInvalidState, "background Agent Session is terminal")
		}
		if result.CancelRequested {
			replayed = true
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE task_agent_sessions SET cancel_requested_at=clock_timestamp()
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 AND cancel_requested_at IS NULL`, fence.TaskID, fence.OwnerID, sessionID); err != nil {
			return fmt.Errorf("request background Agent cancellation: %w", err)
		}
		result.CancelRequested = true
		sequenceNumber, err := incrementTaskEventSequence(ctx, tx, fence.TaskID)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]string{"phase": "subagent_cancel_requested", "step_id": result.Step.ID,
			"session_id": sessionID})
		if err != nil {
			return fmt.Errorf("encode child cancellation event: %w", err)
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: fence.TaskID, Sequence: sequenceNumber,
			Type: "task.step", Payload: payload}); err != nil {
			return err
		}
		return nil
	})
	return result, replayed, err
}

func validStoredAgentToolNames(names []string) bool {
	if len(names) > 64 {
		return false
	}
	previous := ""
	for _, name := range names {
		if len(name) == 0 || len(name) > 128 || name <= previous {
			return false
		}
		for _, char := range name {
			if char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '_' || char == '-' {
				continue
			}
			return false
		}
		previous = name
	}
	return true
}

func sameAgentToolNames(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (s *Store) SettleAgentChildSession(ctx context.Context, fence app.AgentInstanceFence, actorSessionID, sessionID string,
	status task.StepStatus, checkpointThrough int64, checkpointDigest []byte, summary string) (app.AgentChildSession, bool, error) {
	if err := fence.Validate(); err != nil || sessionID == "" || sessionID == fence.AgentSessionID ||
		(status != task.StepSucceeded && status != task.StepFailed && status != task.StepUnknown) || len(summary) > 2048 {
		return app.AgentChildSession{}, false, domain.InvalidArgument("child_session", "child session settlement is invalid")
	}
	if actorSessionID == "" || actorSessionID == sessionID || checkpointThrough < -1 || checkpointThrough > 1<<53-1 ||
		(checkpointThrough == -1 && len(checkpointDigest) != 0) || (checkpointThrough >= 0 && len(checkpointDigest) != sha256.Size) ||
		(status == task.StepSucceeded && checkpointThrough < 0) || !utf8.ValidString(summary) {
		return app.AgentChildSession{}, false, domain.InvalidArgument("child_session", "child session settlement is invalid")
	}
	var result app.AgentChildSession
	var replayed bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockCurrentAgentChildSessionTask(ctx, tx, fence); err != nil {
			return err
		}
		if err := lockAgentChildSessionActor(ctx, tx, fence, actorSessionID); err != nil {
			return err
		}
		var parentSessionID, stepStatus string
		var allowedTools []byte
		if err := tx.QueryRow(ctx, `SELECT s.task_id,s.session_id,s.parent_session_id,s.spawn_key,s.checkpoint_through_seq,s.checkpoint_digest,s.allowed_tools,
s.is_background,s.cancel_requested_at IS NOT NULL,
st.id,st.sequence,st.kind,st.status,st.title,st.summary,st.created_at
FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.session_id=$3 AND s.parent_session_id=$4
FOR UPDATE OF s,st`, fence.TaskID, fence.OwnerID, sessionID, actorSessionID).Scan(
			&result.TaskID, &result.SessionID, &parentSessionID, &result.SpawnKey, &result.CheckpointThrough, &result.CheckpointDigest, &allowedTools,
			&result.Background, &result.CancelRequested,
			&result.Step.ID, &result.Step.Sequence, &result.Step.Kind, &stepStatus, &result.Step.Title, &result.Step.Summary,
			&result.Step.CreatedAt); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "child Agent Session is unavailable")
		} else if err != nil {
			return fmt.Errorf("lock child Agent Session for settlement: %w", err)
		}
		underRoot, err := agentSessionDescendsFrom(ctx, tx, fence.TaskID, fence.OwnerID, sessionID, fence.AgentSessionID)
		if err != nil {
			return fmt.Errorf("verify child Agent Session settlement ancestry: %w", err)
		}
		if !underRoot {
			return domain.NewError(domain.CodeUnauthenticated, "child Agent Session is outside the active agent tree")
		}
		result.ParentSessionID, result.Step.TaskID = parentSessionID, result.TaskID
		result.Step.Status = task.StepStatus(stepStatus)
		if err := json.Unmarshal(allowedTools, &result.AllowedTools); err != nil || !validStoredAgentToolNames(result.AllowedTools) {
			return errors.New("stored child tool allowlist is invalid")
		}
		if result.CheckpointThrough != checkpointThrough || !bytes.Equal(result.CheckpointDigest, checkpointDigest) {
			return domain.NewError(domain.CodeInvalidState, "child Agent Session checkpoint head changed before settlement")
		}
		if result.Background && result.CancelRequested && status == task.StepSucceeded {
			return domain.NewError(domain.CodeInvalidState, "cancelled background Agent Session cannot settle as succeeded")
		}
		if result.Step.Status == status {
			if result.Step.Summary != summary {
				return domain.NewError(domain.CodeIdempotencyConflict, "child Agent Session settlement changed")
			}
			replayed = true
			return nil
		}
		if result.Step.Status != task.StepRunning {
			return domain.NewError(domain.CodeInvalidState, "child Agent Session is not running")
		}
		updated, err := tx.Exec(ctx, `UPDATE task_steps SET status=$3,summary=$4 WHERE task_id=$1 AND id=$2 AND status='running'`,
			fence.TaskID, result.Step.ID, string(status), summary)
		if err != nil {
			return fmt.Errorf("settle child Agent TaskStep: %w", err)
		}
		if updated.RowsAffected() != 1 {
			return domain.NewError(domain.CodeInvalidState, "child Agent TaskStep changed before settlement")
		}
		result.Step.Status, result.Step.Summary = status, summary
		sequenceNumber, err := incrementTaskEventSequence(ctx, tx, fence.TaskID)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]string{"phase": "subagent_settled", "step_id": result.Step.ID,
			"session_id": sessionID, "status": string(status)})
		if err != nil {
			return fmt.Errorf("encode child session settlement event: %w", err)
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: fence.TaskID, Sequence: sequenceNumber,
			Type: "task.step", Payload: payload}); err != nil {
			return err
		}
		return nil
	})
	return result, replayed, err
}

func lockAgentChildSessionActor(ctx context.Context, tx pgx.Tx, fence app.AgentInstanceFence, actorSessionID string) error {
	if actorSessionID == "" {
		return domain.NewError(domain.CodeUnauthenticated, "child Agent Session actor is unavailable")
	}
	if actorSessionID == fence.AgentSessionID {
		return nil
	}
	var parentSessionID, stepID, status string
	if err := tx.QueryRow(ctx, `SELECT s.parent_session_id,st.id,st.status
FROM task_agent_sessions s JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 AND s.session_id=$3
FOR UPDATE OF s,st`, fence.TaskID, fence.OwnerID, actorSessionID).Scan(&parentSessionID, &stepID, &status); errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeUnauthenticated, "child Agent Session actor is unavailable")
	} else if err != nil {
		return fmt.Errorf("lock child Agent Session actor: %w", err)
	}
	if parentSessionID == "" || status != string(task.StepRunning) {
		return domain.NewError(domain.CodeUnauthenticated, "child Agent Session actor is outside the active execution tree")
	}
	underRoot, err := agentSessionDescendsFrom(ctx, tx, fence.TaskID, fence.OwnerID, actorSessionID, fence.AgentSessionID)
	if err != nil {
		return fmt.Errorf("verify child Agent Session actor ancestry: %w", err)
	}
	if !underRoot {
		return domain.NewError(domain.CodeUnauthenticated, "child Agent Session actor is outside the active execution tree")
	}
	return nil
}

func lockCurrentAgentChildSessionTask(ctx context.Context, tx pgx.Tx, fence app.AgentInstanceFence) error {
	current, err := lockAuthorizedAgentTask(ctx, tx, fence)
	if err != nil {
		return err
	}
	if current.InstanceID != fence.AgentInstanceID || current.Generation != fence.AgentGeneration || current.SessionID != fence.AgentSessionID ||
		current.EngineVersion != fence.EngineVersion || current.ProfileVersion != fence.ProfileVersion {
		return domain.NewError(domain.CodeUnauthenticated, "child session authorization is unavailable")
	}
	instance, err := lockAgentInstance(ctx, tx, fence.TaskID, fence.OwnerID, fence.AgentGeneration)
	if err != nil {
		return err
	}
	if instance.Fence != fence || instance.State != "active" {
		return domain.NewError(domain.CodeUnauthenticated, "child session authorization is unavailable")
	}
	return lockActiveAgentLease(ctx, tx, fence)
}
