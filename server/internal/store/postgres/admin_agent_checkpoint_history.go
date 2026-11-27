package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

const adminAgentCheckpointSessionLimit = app.MaxAdminAgentCheckpointSessions

var adminCheckpointSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (s *Store) ReadAdminAgentCheckpointPage(ctx context.Context, taskID, ownerID, sessionID string,
	from int64) (app.StoredAgentCheckpointAdminPage, error) {
	if !domain.ValidTaskID(taskID) || !domain.ValidUserID(ownerID) || !adminCheckpointSessionID.MatchString(sessionID) || from < 0 {
		return app.StoredAgentCheckpointAdminPage{}, domain.InvalidArgument("checkpoint_history", "checkpoint history request is invalid")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return app.StoredAgentCheckpointAdminPage{}, fmt.Errorf("begin admin checkpoint history snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	page, err := loadAdminAgentCheckpointPage(ctx, tx, taskID, ownerID, sessionID, from)
	if err != nil {
		return app.StoredAgentCheckpointAdminPage{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return app.StoredAgentCheckpointAdminPage{}, fmt.Errorf("commit admin checkpoint history snapshot: %w", err)
	}
	return page, nil
}

func loadAdminAgentCheckpointPage(ctx context.Context, tx pgx.Tx, taskID, ownerID, sessionID string,
	from int64) (app.StoredAgentCheckpointAdminPage, error) {
	var page app.StoredAgentCheckpointAdminPage
	var initiatorID string
	var taskRootSessionID string
	var taskWatermark int64
	var taskHeadDigest []byte
	var watermark int64
	var headDigest []byte
	err := tx.QueryRow(ctx, `
WITH RECURSIVE ancestors(session_id,parent_session_id,path) AS (
  SELECT session_id,parent_session_id,ARRAY[session_id]::text[]
  FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2 AND session_id=$3
  UNION ALL
  SELECT parent.session_id,parent.parent_session_id,ancestors.path || parent.session_id
  FROM task_agent_sessions parent
  JOIN ancestors ON ancestors.parent_session_id=parent.session_id
  WHERE parent.task_id=$1 AND parent.owner_id=$2 AND cardinality(ancestors.path)<128
    AND NOT parent.session_id=ANY(ancestors.path)
)
SELECT task.id,task.owner_id,task.initiator_id,task.agent_session_id,task.checkpoint_through_seq,task.checkpoint_digest,
       session.session_id,session.checkpoint_through_seq,session.checkpoint_digest,
       COALESCE((SELECT session_id FROM ancestors WHERE parent_session_id IS NULL LIMIT 1),'')
FROM tasks task
JOIN task_agent_sessions session ON session.task_id=task.id AND session.owner_id=task.owner_id
WHERE task.id=$1 AND task.owner_id=$2 AND session.session_id=$3`, taskID, ownerID, sessionID).
		Scan(&page.TaskID, &page.OwnerID, &initiatorID, &taskRootSessionID, &taskWatermark, &taskHeadDigest, &page.SessionID,
			&watermark, &headDigest, &page.RootSessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.StoredAgentCheckpointAdminPage{}, domain.NewError(domain.CodeNotFound, "agent checkpoint session not found")
	}
	if err != nil {
		return app.StoredAgentCheckpointAdminPage{}, fmt.Errorf("read admin checkpoint session head: %w", err)
	}
	if page.RootSessionID == "" || watermark < -1 ||
		(watermark == -1 && len(headDigest) != 0) || (watermark >= 0 && len(headDigest) != 32) {
		return app.StoredAgentCheckpointAdminPage{}, domain.NewError(domain.CodeInvalidState, "agent checkpoint session head is invalid")
	}
	if page.RootSessionID == sessionID && (taskRootSessionID != page.RootSessionID || taskWatermark != watermark ||
		!bytes.Equal(taskHeadDigest, headDigest)) {
		return app.StoredAgentCheckpointAdminPage{}, domain.NewError(domain.CodeInvalidState, "root checkpoint head is inconsistent")
	}
	page.Watermark = watermark
	if watermark < math.MaxInt64 && from > watermark+1 {
		return app.StoredAgentCheckpointAdminPage{}, domain.InvalidArgument("from_seq", "checkpoint history cursor is beyond the committed range")
	}
	if watermark < math.MaxInt64 && from == watermark+1 {
		return page, nil
	}

	stored, createdAt, found, err := readAdminAgentCheckpointSegment(ctx, tx, taskID, ownerID, initiatorID,
		sessionID, page.RootSessionID, from)
	if err != nil {
		return app.StoredAgentCheckpointAdminPage{}, err
	}
	if !found {
		observedFrom, err := nextAdminAgentCheckpointStart(ctx, tx, taskID, ownerID, sessionID, from)
		if err != nil {
			return app.StoredAgentCheckpointAdminPage{}, err
		}
		gapAt := from
		page.Reason, page.GapAt, page.ObservedFrom = "checkpoint_gap", &gapAt, observedFrom
		return page, nil
	}
	if stored.Through > watermark || stored.Fence.AgentRootSessionID != page.RootSessionID {
		return app.StoredAgentCheckpointAdminPage{}, domain.NewError(domain.CodeInvalidState, "agent checkpoint segment is outside the committed session")
	}
	if stored.Through == watermark && !bytes.Equal(stored.Digest, headDigest) {
		return app.StoredAgentCheckpointAdminPage{}, domain.NewError(domain.CodeInvalidState, "agent checkpoint digest does not match the committed session head")
	}
	page.Checkpoint, page.CreatedAt = &stored, &createdAt
	if stored.Through < watermark {
		expected := stored.Through + 1
		next, _, hasNext, err := readAdminAgentCheckpointSegment(ctx, tx, taskID, ownerID, initiatorID,
			sessionID, page.RootSessionID, expected)
		if err != nil {
			return app.StoredAgentCheckpointAdminPage{}, err
		}
		if hasNext && next.Through > watermark {
			return app.StoredAgentCheckpointAdminPage{}, domain.NewError(domain.CodeInvalidState, "agent checkpoint exceeds the committed watermark")
		}
		if !hasNext {
			observedFrom, err := nextAdminAgentCheckpointStart(ctx, tx, taskID, ownerID, sessionID, expected)
			if err != nil {
				return app.StoredAgentCheckpointAdminPage{}, err
			}
			page.Reason, page.GapAt, page.ObservedFrom = "checkpoint_gap", &expected, observedFrom
		}
	}
	return page, nil
}

func readAdminAgentCheckpointSegment(ctx context.Context, tx pgx.Tx, taskID, ownerID, initiatorID, sessionID, rootSessionID string,
	from int64) (app.StoredAgentCheckpoint, time.Time, bool, error) {
	var stored app.StoredAgentCheckpoint
	var checkpointRootSessionID string
	var leaseOwner, instanceSessionID, instanceEngine, instanceProfile pgtype.Text
	var instanceAuthVersion, instanceLeaseVersion pgtype.Int8
	var ciphertextBytes int64
	var matchingSegments int64
	var createdAt time.Time
	err := tx.QueryRow(ctx, `
SELECT count(*) OVER(),checkpoint.agent_instance_id,checkpoint.agent_generation,checkpoint.agent_session_id,checkpoint.agent_root_session_id,
       checkpoint.from_seq,checkpoint.through_seq,checkpoint.digest,octet_length(checkpoint.ciphertext),
       CASE WHEN octet_length(checkpoint.ciphertext)<=$5 THEN checkpoint.ciphertext ELSE NULL END,
       checkpoint.engine_version,checkpoint.profile_version,checkpoint.auth_version,checkpoint.lease_version,checkpoint.created_at,
       instance.lease_owner,instance.agent_session_id,instance.engine_version,instance.profile_version,instance.auth_version,instance.lease_version
FROM task_agent_checkpoints checkpoint
LEFT JOIN task_agent_instances instance
  ON instance.task_id=checkpoint.task_id AND instance.owner_id=checkpoint.owner_id
 AND instance.agent_generation=checkpoint.agent_generation AND instance.agent_instance_id=checkpoint.agent_instance_id
WHERE checkpoint.task_id=$1 AND checkpoint.owner_id=$2 AND checkpoint.agent_session_id=$3
  AND checkpoint.from_seq<=$4 AND checkpoint.through_seq>=$4
ORDER BY checkpoint.from_seq DESC LIMIT 1`,
		taskID, ownerID, sessionID, from, app.MaxAdminAgentCheckpointPageBytes+64).Scan(
		&matchingSegments, &stored.Fence.AgentInstanceID, &stored.Fence.AgentGeneration, &stored.Fence.AgentSessionID, &checkpointRootSessionID,
		&stored.From, &stored.Through, &stored.Digest, &ciphertextBytes, &stored.Ciphertext,
		&stored.Fence.EngineVersion, &stored.Fence.ProfileVersion, &stored.Fence.AuthorizationVersion, &stored.Fence.LeaseVersion,
		&createdAt, &leaseOwner, &instanceSessionID, &instanceEngine, &instanceProfile, &instanceAuthVersion, &instanceLeaseVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.StoredAgentCheckpoint{}, time.Time{}, false, nil
	}
	if err != nil {
		return app.StoredAgentCheckpoint{}, time.Time{}, false, fmt.Errorf("read admin checkpoint segment: %w", err)
	}
	if matchingSegments != 1 {
		return app.StoredAgentCheckpoint{}, time.Time{}, false, domain.NewError(domain.CodeInvalidState, "agent checkpoint segments overlap")
	}
	if !leaseOwner.Valid || !instanceSessionID.Valid || !instanceEngine.Valid || !instanceProfile.Valid ||
		!instanceAuthVersion.Valid || !instanceLeaseVersion.Valid || instanceSessionID.String != rootSessionID ||
		instanceEngine.String != stored.Fence.EngineVersion || instanceProfile.String != stored.Fence.ProfileVersion ||
		instanceAuthVersion.Int64 != stored.Fence.AuthorizationVersion || instanceLeaseVersion.Int64 != stored.Fence.LeaseVersion ||
		ciphertextBytes <= 0 || ciphertextBytes != int64(len(stored.Ciphertext)) ||
		len(stored.Ciphertext) == 0 || len(stored.Ciphertext) > app.MaxAdminAgentCheckpointPageBytes+64 ||
		stored.Through < stored.From || len(stored.Digest) != 32 || leaseOwner.String == "" {
		return app.StoredAgentCheckpoint{}, time.Time{}, false, domain.NewError(domain.CodeInvalidState, "stored agent checkpoint identity is invalid")
	}
	stored.Fence.TaskID, stored.Fence.OwnerID, stored.Fence.InitiatorID = taskID, ownerID, initiatorID
	stored.Fence.AgentRootSessionID = checkpointRootSessionID
	stored.Fence.LeaseOwner = leaseOwner.String
	if stored.Fence.AgentRootSessionID != rootSessionID || !adminCheckpointSessionID.MatchString(stored.Fence.AgentInstanceID) ||
		stored.Fence.AgentGeneration < 1 || stored.Fence.AuthorizationVersion < 1 || stored.Fence.LeaseVersion < 1 ||
		stored.Fence.EngineVersion == "" || len(stored.Fence.EngineVersion) > 128 || stored.Fence.ProfileVersion == "" || len(stored.Fence.ProfileVersion) > 128 {
		return app.StoredAgentCheckpoint{}, time.Time{}, false, domain.NewError(domain.CodeInvalidState, "stored agent checkpoint identity is invalid")
	}
	return stored, createdAt.UTC(), true, nil
}

func nextAdminAgentCheckpointStart(ctx context.Context, tx pgx.Tx, taskID, ownerID, sessionID string, from int64) (*int64, error) {
	var next int64
	err := tx.QueryRow(ctx, `SELECT from_seq FROM task_agent_checkpoints
WHERE task_id=$1 AND owner_id=$2 AND agent_session_id=$3 AND from_seq>$4
ORDER BY from_seq LIMIT 1`, taskID, ownerID, sessionID, from).Scan(&next)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read next admin checkpoint start: %w", err)
	}
	return &next, nil
}

func (s *Store) ListAdminAgentCheckpointSessions(ctx context.Context, taskID, ownerID string) ([]app.AgentCheckpointAdminSession, error) {
	if !domain.ValidTaskID(taskID) || !domain.ValidUserID(ownerID) {
		return nil, domain.InvalidArgument("task", "task identity is invalid")
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin admin agent session snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var taskExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND owner_id=$2)`, taskID, ownerID).Scan(&taskExists); err != nil {
		return nil, fmt.Errorf("verify admin task: %w", err)
	}
	if !taskExists {
		return nil, domain.NewError(domain.CodeNotFound, "task not found")
	}
	var total int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2`, taskID, ownerID).Scan(&total); err != nil {
		return nil, fmt.Errorf("count admin agent sessions: %w", err)
	}
	if total > adminAgentCheckpointSessionLimit {
		return nil, domain.NewError(domain.CodeResultTooLarge, "agent session history exceeds the page limit")
	}
	rows, err := tx.Query(ctx, `
WITH RECURSIVE lineage(session_id,parent_session_id,root_session_id,depth,path) AS (
  SELECT session_id,parent_session_id,session_id,0,ARRAY[session_id]::text[]
  FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2 AND parent_session_id IS NULL
  UNION ALL
  SELECT child.session_id,child.parent_session_id,lineage.root_session_id,lineage.depth+1,lineage.path || child.session_id
  FROM task_agent_sessions child JOIN lineage ON child.parent_session_id=lineage.session_id
  WHERE child.task_id=$1 AND child.owner_id=$2 AND lineage.depth<127 AND NOT child.session_id=ANY(lineage.path)
)
SELECT session.session_id,COALESCE(session.parent_session_id,''),lineage.root_session_id,COALESCE(session.step_id,''),
       session.checkpoint_through_seq,session.created_at
FROM task_agent_sessions session JOIN lineage ON lineage.session_id=session.session_id
WHERE session.task_id=$1 AND session.owner_id=$2
ORDER BY session.created_at,session.session_id LIMIT $3`, taskID, ownerID, adminAgentCheckpointSessionLimit+1)
	if err != nil {
		return nil, fmt.Errorf("list admin agent sessions: %w", err)
	}
	sessions := make([]app.AgentCheckpointAdminSession, 0, adminAgentCheckpointSessionLimit+1)
	for rows.Next() {
		var session app.AgentCheckpointAdminSession
		if err := rows.Scan(&session.SessionID, &session.ParentSessionID, &session.RootSessionID,
			&session.StepID, &session.CheckpointThrough, &session.CreatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan admin agent session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate admin agent sessions: %w", err)
	}
	rows.Close()
	if len(sessions) > adminAgentCheckpointSessionLimit {
		return nil, domain.NewError(domain.CodeResultTooLarge, "agent session history exceeds the page limit")
	}
	if len(sessions) != total {
		return nil, domain.NewError(domain.CodeInvalidState, "agent session tree is incomplete")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit admin agent session snapshot: %w", err)
	}
	return sessions, nil
}
