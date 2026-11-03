package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/schedule"
)

func (s *Store) SaveAgentCheckpoint(ctx context.Context, value app.StoredAgentCheckpoint) (bool, error) {
	fence := value.Fence
	rootSessionID := fence.AgentRootSessionID
	if rootSessionID == "" {
		rootSessionID = fence.AgentSessionID
		fence.AgentRootSessionID = rootSessionID
		value.Fence = fence
	}
	if !domain.ValidTaskID(fence.TaskID) || !domain.ValidUserID(fence.OwnerID) || !domain.ValidUserID(fence.InitiatorID) || fence.AgentInstanceID == "" ||
		fence.AgentSessionID == "" || fence.AgentRootSessionID == "" || fence.AgentGeneration < 1 || fence.AuthorizationVersion < 1 ||
		fence.LeaseOwner == "" || fence.LeaseVersion < 1 || fence.EngineVersion == "" || fence.ProfileVersion == "" ||
		value.From < 0 || value.Through < value.From || len(value.Digest) != 32 || len(value.Ciphertext) == 0 || len(value.Ciphertext) > (1<<20)+64 {
		return false, domain.InvalidArgument("checkpoint", "checkpoint fields are invalid")
	}
	var replayed bool
	var deliveryErr error
	// Lock authorization and the task with the checkpoint row in one transaction.
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, err := lockAuthorizedAgentTask(ctx, tx, agentCheckpointInstanceFence(fence, rootSessionID))
		if err != nil {
			return err
		}
		if current.InstanceID != fence.AgentInstanceID || current.Generation != fence.AgentGeneration || current.SessionID != rootSessionID ||
			current.EngineVersion != fence.EngineVersion || current.ProfileVersion != fence.ProfileVersion {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint authorization is unavailable")
		}
		taskThrough, taskDigest := current.CheckpointThrough, current.CheckpointDigest
		if err := lockActiveAgentLease(ctx, tx, app.AgentInstanceFence{
			TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID, AgentInstanceID: fence.AgentInstanceID,
			AgentGeneration: fence.AgentGeneration, AgentSessionID: rootSessionID,
			LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion,
			AuthorizationVersion: fence.AuthorizationVersion, EngineVersion: fence.EngineVersion,
			ProfileVersion: fence.ProfileVersion,
		}); err != nil {
			return err
		}
		var parentSessionID string
		var sessionThrough int64
		var sessionDigest []byte
		err = tx.QueryRow(ctx, `SELECT COALESCE(parent_session_id,''),checkpoint_through_seq,checkpoint_digest
FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 FOR UPDATE`,
			fence.TaskID, fence.OwnerID, fence.AgentSessionID).Scan(&parentSessionID, &sessionThrough, &sessionDigest)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint session is unavailable")
		}
		if err != nil {
			return fmt.Errorf("lock checkpoint session: %w", err)
		}
		if fence.AgentSessionID == rootSessionID {
			if parentSessionID != "" || sessionThrough != taskThrough || !bytes.Equal(sessionDigest, taskDigest) {
				return domain.NewError(domain.CodeInvalidState, "root checkpoint head changed")
			}
		} else {
			underRoot, err := agentSessionDescendsFrom(ctx, tx, fence.TaskID, fence.OwnerID, fence.AgentSessionID, rootSessionID)
			if err != nil {
				return fmt.Errorf("verify checkpoint session ancestry: %w", err)
			}
			if parentSessionID == "" || !underRoot {
				return domain.NewError(domain.CodeUnauthenticated, "checkpoint session is outside the active agent tree")
			}
		}
		if fence.AgentSessionID != rootSessionID {
			var stepStatus string
			if err := tx.QueryRow(ctx, `SELECT st.status FROM task_agent_sessions s JOIN task_steps st
ON st.task_id=s.task_id AND st.id=s.step_id WHERE s.task_id=$1 AND s.owner_id=$2 AND s.session_id=$3 FOR UPDATE OF st`,
				fence.TaskID, fence.OwnerID, fence.AgentSessionID).Scan(&stepStatus); err != nil {
				return fmt.Errorf("read child session TaskStep: %w", err)
			}
			if stepStatus != "running" {
				return domain.NewError(domain.CodeInvalidState, "child session is not running")
			}
		}
		var previousFrom, previousThrough int64
		var previousDigest []byte
		var previousEngine, previousProfile, previousRootSession string
		err = tx.QueryRow(ctx, `SELECT from_seq,through_seq,digest,engine_version,profile_version,agent_root_session_id FROM task_agent_checkpoints
WHERE task_id=$1 AND agent_session_id=$2 AND (from_seq=$3 OR through_seq=$4)`,
			fence.TaskID, fence.AgentSessionID, value.From, value.Through).Scan(&previousFrom, &previousThrough, &previousDigest,
			&previousEngine, &previousProfile, &previousRootSession)
		if err == nil {
			if previousFrom != value.From || previousThrough != value.Through || !bytes.Equal(previousDigest, value.Digest) ||
				previousEngine != fence.EngineVersion || previousProfile != fence.ProfileVersion || previousRootSession != rootSessionID {
				return domain.NewError(domain.CodeIdempotencyConflict, "checkpoint sequence was reused with different content")
			}
			replayed = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check existing checkpoint: %w", err)
		}
		if sessionThrough == int64(^uint64(0)>>1) || value.From != sessionThrough+1 {
			return domain.NewError(domain.CodeConflict, "checkpoint event sequence is not contiguous")
		}
		if fence.AgentSessionID == rootSessionID && value.ConversationMessagesInvalid {
			if err := markPendingConversationMessagesUnknown(ctx, tx, fence.TaskID, fence.OwnerID); err != nil {
				return err
			}
			deliveryErr = domain.NewError(domain.CodeInvalidState, "checkpoint user message could not be matched safely")
			return nil
		}
		pendingMessageIDs := make([]string, 0, len(value.ConversationMessages))
		if fence.AgentSessionID == rootSessionID {
			for _, message := range value.ConversationMessages {
				var role, content, deliveryStatus string
				err := tx.QueryRow(ctx, `SELECT message.role,message.content,message.delivery_status FROM conversation_messages AS message JOIN tasks AS task
ON task.id=message.task_id AND task.owner_id=message.owner_id AND task.conversation_id=message.conversation_id
WHERE message.id=$1 AND message.owner_id=$2 AND message.task_id=$3
FOR UPDATE OF message`, message.ID, fence.OwnerID, fence.TaskID).Scan(&role, &content, &deliveryStatus)
				if errors.Is(err, pgx.ErrNoRows) {
					if err := markPendingConversationMessagesUnknown(ctx, tx, fence.TaskID, fence.OwnerID); err != nil {
						return err
					}
					deliveryErr = domain.NewError(domain.CodeInvalidState, "checkpoint user message could not be matched safely")
					return nil
				}
				if err != nil {
					return fmt.Errorf("lock checkpoint user message: %w", err)
				}
				if role != "user" || content != message.Content {
					if err := markPendingConversationMessagesUnknown(ctx, tx, fence.TaskID, fence.OwnerID); err != nil {
						return err
					}
					deliveryErr = domain.NewError(domain.CodeInvalidState, "checkpoint user message could not be matched safely")
					return nil
				}
				if deliveryStatus == "pending" {
					pendingMessageIDs = append(pendingMessageIDs, message.ID)
				}
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO task_agent_checkpoints
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,agent_root_session_id,from_seq,through_seq,digest,ciphertext,
 engine_version,profile_version,auth_version,lease_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			fence.TaskID, fence.OwnerID, fence.AgentInstanceID, fence.AgentGeneration, fence.AgentSessionID, rootSessionID,
			value.From, value.Through, value.Digest, value.Ciphertext, fence.EngineVersion, fence.ProfileVersion,
			fence.AuthorizationVersion, fence.LeaseVersion)
		if err != nil {
			return fmt.Errorf("save agent checkpoint: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE task_agent_sessions SET checkpoint_through_seq=$4,checkpoint_digest=$5
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3`, fence.TaskID, fence.OwnerID, fence.AgentSessionID, value.Through, value.Digest); err != nil {
			return fmt.Errorf("advance agent session checkpoint: %w", err)
		}
		if fence.AgentSessionID == rootSessionID {
			if _, err := tx.Exec(ctx, `UPDATE tasks SET checkpoint_through_seq=$2,checkpoint_digest=$3,updated_at=clock_timestamp()
WHERE id=$1`, fence.TaskID, value.Through, value.Digest); err != nil {
				return fmt.Errorf("advance task checkpoint: %w", err)
			}
			for _, messageID := range pendingMessageIDs {
				result, err := tx.Exec(ctx, `UPDATE conversation_messages SET delivery_status='applied'
WHERE id=$1 AND owner_id=$2 AND task_id=$3 AND role='user' AND delivery_status='pending'`,
					messageID, fence.OwnerID, fence.TaskID)
				if err != nil {
					return fmt.Errorf("apply checkpointed user message: %w", err)
				}
				if result.RowsAffected() != 1 {
					return domain.NewError(domain.CodeInvalidState, "checkpoint user message changed before commit")
				}
			}
		}
		return nil
	})
	if err == nil && deliveryErr != nil {
		return false, deliveryErr
	}
	return replayed, err
}

func (s *Store) PendingMessages(ctx context.Context, fence app.AgentCheckpointFence) ([]app.AgentConversationMessage, error) {
	rootSessionID := fence.AgentRootSessionID
	if rootSessionID == "" {
		rootSessionID = fence.AgentSessionID
		fence.AgentRootSessionID = rootSessionID
	}
	if !domain.ValidTaskID(fence.TaskID) || !domain.ValidUserID(fence.OwnerID) || !domain.ValidUserID(fence.InitiatorID) || fence.AgentInstanceID == "" ||
		fence.AgentSessionID == "" || fence.AgentSessionID != rootSessionID || fence.AgentGeneration < 1 || fence.AuthorizationVersion < 1 ||
		fence.LeaseOwner == "" || fence.LeaseVersion < 1 || fence.EngineVersion == "" || fence.ProfileVersion == "" {
		return nil, domain.InvalidArgument("checkpoint", "pending message identity is invalid")
	}
	messages := make([]app.AgentConversationMessage, 0, 128)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, err := lockAuthorizedAgentTask(ctx, tx, agentCheckpointInstanceFence(fence, rootSessionID))
		if err != nil {
			return err
		}
		if current.InstanceID != fence.AgentInstanceID || current.Generation != fence.AgentGeneration || current.SessionID != rootSessionID ||
			current.EngineVersion != fence.EngineVersion || current.ProfileVersion != fence.ProfileVersion {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint authorization is unavailable")
		}
		taskThrough, taskDigest := current.CheckpointThrough, current.CheckpointDigest
		if err := lockActiveAgentLease(ctx, tx, agentCheckpointInstanceFence(fence, rootSessionID)); err != nil {
			return err
		}
		var parentSessionID string
		var sessionThrough int64
		var sessionDigest []byte
		if err := tx.QueryRow(ctx, `SELECT COALESCE(parent_session_id,''),checkpoint_through_seq,checkpoint_digest
FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 FOR UPDATE`,
			fence.TaskID, fence.OwnerID, rootSessionID).Scan(&parentSessionID, &sessionThrough, &sessionDigest); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint session is unavailable")
		} else if err != nil {
			return fmt.Errorf("lock pending message checkpoint session: %w", err)
		}
		if parentSessionID != "" || sessionThrough != taskThrough || !bytes.Equal(sessionDigest, taskDigest) {
			return domain.NewError(domain.CodeInvalidState, "root checkpoint head changed")
		}
		rows, err := tx.Query(ctx, `SELECT id,content FROM conversation_messages
WHERE task_id=$1 AND owner_id=$2 AND conversation_id=(SELECT conversation_id FROM tasks WHERE id=$1 AND owner_id=$2)
  AND role='user' AND delivery_status='pending'
ORDER BY sequence LIMIT 128`, fence.TaskID, fence.OwnerID)
		if err != nil {
			return fmt.Errorf("list pending task messages: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var message app.AgentConversationMessage
			if err := rows.Scan(&message.ID, &message.Content); err != nil {
				return fmt.Errorf("scan pending task message: %w", err)
			}
			messages = append(messages, message)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate pending task messages: %w", err)
		}
		return nil
	})
	return messages, err
}

func agentCheckpointInstanceFence(fence app.AgentCheckpointFence, rootSessionID string) app.AgentInstanceFence {
	return app.AgentInstanceFence{
		TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID, AgentInstanceID: fence.AgentInstanceID,
		AgentGeneration: fence.AgentGeneration, AgentSessionID: rootSessionID,
		LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion,
		AuthorizationVersion: fence.AuthorizationVersion, EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion,
	}
}

func markPendingConversationMessagesUnknown(ctx context.Context, tx pgx.Tx, taskID, ownerID string) error {
	if _, err := tx.Exec(ctx, `UPDATE conversation_messages AS message SET delivery_status='unknown'
FROM tasks task
WHERE task.id=$1 AND task.owner_id=$2 AND message.task_id=task.id AND message.owner_id=task.owner_id
  AND message.conversation_id=task.conversation_id AND message.role='user' AND message.delivery_status='pending'`, taskID, ownerID); err != nil {
		return fmt.Errorf("mark unmatched pending task messages unknown: %w", err)
	}
	return nil
}

func (s *Store) ReadAgentCheckpointPage(ctx context.Context, request app.AgentCheckpointPageRequest) (app.StoredAgentCheckpoint, error) {
	fence := request.Fence
	if request.SessionID == "" {
		request.SessionID = fence.AgentSessionID
	}
	if err := fence.Validate(); err != nil || request.SessionID == "" || request.From < 0 || request.Through < request.From || len(request.ThroughDigest) != 32 {
		return app.StoredAgentCheckpoint{}, domain.InvalidArgument("checkpoint_page", "checkpoint page request is invalid")
	}
	var result app.StoredAgentCheckpoint
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, err := lockAuthorizedAgentTask(ctx, tx, fence)
		if err != nil {
			return err
		}
		if current.InstanceID != fence.AgentInstanceID || current.Generation != fence.AgentGeneration || current.SessionID != fence.AgentSessionID ||
			current.EngineVersion != fence.EngineVersion || current.ProfileVersion != fence.ProfileVersion {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint page authorization is unavailable")
		}
		instance, err := lockAgentInstance(ctx, tx, fence.TaskID, fence.OwnerID, fence.AgentGeneration)
		if err != nil {
			return err
		}
		if instance.Fence != fence || instance.State != "active" {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint page authorization is unavailable")
		}
		if err := lockActiveAgentLease(ctx, tx, fence); err != nil {
			return err
		}
		var parentSessionID string
		var sessionThrough int64
		var sessionDigest []byte
		if err := tx.QueryRow(ctx, `SELECT COALESCE(parent_session_id,''),checkpoint_through_seq,checkpoint_digest
FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 FOR UPDATE`,
			fence.TaskID, fence.OwnerID, request.SessionID).Scan(&parentSessionID, &sessionThrough, &sessionDigest); errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint session is unavailable")
		} else if err != nil {
			return fmt.Errorf("lock checkpoint session for read: %w", err)
		}
		if request.SessionID == fence.AgentSessionID {
			if parentSessionID != "" || sessionThrough != current.CheckpointThrough || !bytes.Equal(sessionDigest, current.CheckpointDigest) {
				return domain.NewError(domain.CodeInvalidState, "root checkpoint head changed")
			}
		} else {
			underRoot, err := agentSessionDescendsFrom(ctx, tx, fence.TaskID, fence.OwnerID, request.SessionID, fence.AgentSessionID)
			if err != nil {
				return fmt.Errorf("verify checkpoint session ancestry: %w", err)
			}
			if parentSessionID == "" || !underRoot {
				return domain.NewError(domain.CodeUnauthenticated, "checkpoint session is outside the active agent tree")
			}
		}
		if request.Through > sessionThrough || request.Through == sessionThrough && !bytes.Equal(sessionDigest, request.ThroughDigest) {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint page is outside the pinned session history")
		}
		var pinned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM task_agent_checkpoints c
 JOIN task_agent_instances i ON i.task_id=c.task_id AND i.owner_id=c.owner_id
  AND i.agent_generation=c.agent_generation AND i.agent_instance_id=c.agent_instance_id
  AND i.agent_session_id=c.agent_root_session_id AND i.engine_version=c.engine_version
  AND i.profile_version=c.profile_version AND i.auth_version=c.auth_version AND i.lease_version=c.lease_version
	 JOIN task_agent_sessions s ON s.task_id=c.task_id AND s.owner_id=c.owner_id AND s.session_id=c.agent_session_id
 WHERE c.task_id=$1 AND c.owner_id=$2 AND c.agent_session_id=$3 AND c.agent_root_session_id=$4
  AND c.through_seq=$5 AND c.digest=$6 AND c.auth_version=$7 AND c.engine_version=$8 AND c.profile_version=$9
  AND c.agent_generation<$10 AND i.state='stopped'
)`, fence.TaskID, fence.OwnerID, request.SessionID, fence.AgentSessionID, request.Through, request.ThroughDigest,
			fence.AuthorizationVersion, fence.EngineVersion, fence.ProfileVersion, fence.AgentGeneration).Scan(&pinned); err != nil {
			return fmt.Errorf("verify pinned checkpoint history: %w", err)
		}
		if !pinned {
			return domain.NewError(domain.CodeInvalidState, "pinned checkpoint history is unavailable")
		}
		err = tx.QueryRow(ctx, `SELECT c.agent_instance_id,c.agent_generation,c.agent_session_id,c.agent_root_session_id,c.from_seq,c.through_seq,c.digest,c.ciphertext,
 c.engine_version,c.profile_version,c.auth_version,c.lease_version,i.lease_owner
FROM task_agent_checkpoints c
JOIN task_agent_instances i ON i.task_id=c.task_id AND i.owner_id=c.owner_id
 AND i.agent_generation=c.agent_generation AND i.agent_instance_id=c.agent_instance_id
 AND i.agent_session_id=c.agent_root_session_id AND i.engine_version=c.engine_version
 AND i.profile_version=c.profile_version AND i.auth_version=c.auth_version AND i.lease_version=c.lease_version
WHERE c.task_id=$1 AND c.owner_id=$2 AND c.agent_session_id=$3 AND c.agent_root_session_id=$4 AND c.from_seq=$5
 AND c.through_seq<=$6 AND c.auth_version=$7 AND c.engine_version=$8 AND c.profile_version=$9
 AND c.agent_generation<$10 AND i.state='stopped'`, fence.TaskID, fence.OwnerID, request.SessionID, fence.AgentSessionID,
			request.From, request.Through, fence.AuthorizationVersion, fence.EngineVersion, fence.ProfileVersion, fence.AgentGeneration).
			Scan(&result.Fence.AgentInstanceID, &result.Fence.AgentGeneration, &result.Fence.AgentSessionID, &result.Fence.AgentRootSessionID,
				&result.From, &result.Through, &result.Digest, &result.Ciphertext,
				&result.Fence.EngineVersion, &result.Fence.ProfileVersion, &result.Fence.AuthorizationVersion,
				&result.Fence.LeaseVersion, &result.Fence.LeaseOwner)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeInvalidState, "checkpoint page is unavailable")
		}
		if err != nil {
			return fmt.Errorf("read checkpoint page: %w", err)
		}
		result.Fence.TaskID, result.Fence.OwnerID, result.Fence.InitiatorID = fence.TaskID, fence.OwnerID, fence.InitiatorID
		return nil
	})
	return result, err
}

func agentSessionDescendsFrom(ctx context.Context, tx pgx.Tx, taskID, ownerID, sessionID, rootSessionID string) (bool, error) {
	var belongs bool
	err := tx.QueryRow(ctx, `WITH RECURSIVE lineage(session_id,parent_session_id,path) AS (
 SELECT session_id,parent_session_id,ARRAY[session_id] FROM task_agent_sessions
 WHERE task_id=$1 AND owner_id=$2 AND session_id=$3
 UNION ALL
 SELECT p.session_id,p.parent_session_id,l.path || p.session_id
 FROM task_agent_sessions p JOIN lineage l ON p.session_id=l.parent_session_id
 WHERE p.task_id=$1 AND p.owner_id=$2 AND NOT p.session_id=ANY(l.path)
)
SELECT EXISTS(SELECT 1 FROM lineage WHERE session_id=$4 AND parent_session_id IS NULL)`,
		taskID, ownerID, sessionID, rootSessionID).Scan(&belongs)
	if err != nil {
		return false, fmt.Errorf("read Agent Session ancestry: %w", err)
	}
	return belongs, nil
}

func checkAgentCheckpointAuthorization(ctx context.Context, tx pgx.Tx, ownerID, taskID, kind, tokenID string, scopes []string, installationID string) error {
	if kind == string(identity.KindScheduleTask) {
		if !scheduleTaskScopeStringsValid(scopes) || !schedule.ValidRunID(tokenID) || installationID != "" {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint authorization is unavailable")
		}
		var scheduleHostID, taskHostID pgtype.Text
		var taskScopes []string
		var taskAuthKind, taskAuthTokenID string
		err := tx.QueryRow(ctx, `SELECT s.host_id,t.host_id,t.auth_scopes,t.auth_kind,t.auth_token_id
FROM schedule_runs r
JOIN schedules s ON s.id=r.schedule_id AND s.owner_id=r.owner_id
JOIN tasks t ON t.id=r.task_id AND t.owner_id=r.owner_id AND t.initiator_id=r.owner_id
WHERE r.id=$1 AND r.owner_id=$2 AND r.task_id=$3 AND t.execution_location='server'`, tokenID, ownerID, taskID).
			Scan(&scheduleHostID, &taskHostID, &taskScopes, &taskAuthKind, &taskAuthTokenID)
		if err != nil || scheduleHostID.Valid != taskHostID.Valid ||
			scheduleHostID.Valid && scheduleHostID.String != taskHostID.String ||
			!scheduleTaskScopeStringsValidForHost(taskScopes, taskHostID.Valid) || !sameStringSlices(taskScopes, scopes) ||
			taskAuthKind != string(identity.KindScheduleTask) || taskAuthTokenID != tokenID {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint authorization is unavailable")
		}
		return nil
	}
	if kind == string(identity.KindClient) {
		if !domain.ValidClientInstallationID(installationID) {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint installation is unavailable")
		}
	} else if kind == string(identity.KindSession) || kind == string(identity.KindAPIKey) {
		var active bool
		query := `SELECT revoked_at IS NULL AND kind=$3 FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`
		if kind == string(identity.KindAPIKey) {
			query = `SELECT revoked_at IS NULL AND expires_at>clock_timestamp() AND kind=$3 FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`
		}
		err := tx.QueryRow(ctx, query, tokenID, ownerID, kind).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint credential is unavailable")
		}
		if err != nil {
			return fmt.Errorf("lock checkpoint credential: %w", err)
		}
	} else {
		return domain.NewError(domain.CodeUnauthenticated, "checkpoint authorization is unavailable")
	}
	if installationID != "" {
		var active bool
		err := tx.QueryRow(ctx, `SELECT revoked_at IS NULL AND expires_at>clock_timestamp()
FROM client_installations WHERE id=$1 AND owner_id=$2 FOR SHARE`, installationID, ownerID).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !active {
			return domain.NewError(domain.CodeUnauthenticated, "checkpoint installation is unavailable")
		}
		if err != nil {
			return fmt.Errorf("lock checkpoint installation: %w", err)
		}
	}
	return nil
}
