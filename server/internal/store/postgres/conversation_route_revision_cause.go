package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

// RevisionStopProven may annotate a lost worker lease as a revision stop only
// when an Agent using exactly that lease has durable revision-stop evidence.
// Missing or ambiguous evidence stays an ordinary cancellation.
func (s *Store) RevisionStopProven(ctx context.Context, value task.Task) (bool, error) {
	if ctx == nil || !domain.ValidTaskID(value.ID) || !domain.ValidUserID(value.OwnerID) || !domain.ValidUserID(value.InitiatorID) ||
		value.Lease.Owner == "" || value.Lease.Version < 1 {
		return false, nil
	}
	fence := app.ModelTaskFence{TaskID: value.ID, OwnerID: value.OwnerID, InitiatorID: value.InitiatorID,
		LeaseOwner: value.Lease.Owner, LeaseVersion: value.Lease.Version}
	err := s.pool.QueryRow(ctx, `SELECT agent_instance_id,agent_generation,agent_session_id,auth_version,engine_version,profile_version
FROM task_agent_instances WHERE task_id=$1 AND owner_id=$2 AND lease_owner=$3 AND lease_version=$4
ORDER BY agent_generation DESC LIMIT 1`, value.ID, value.OwnerID, value.Lease.Owner, value.Lease.Version).
		Scan(&fence.AgentInstanceID, &fence.AgentGeneration, &fence.AgentRootSessionID, &fence.AuthorizationVersion, &fence.EngineVersion, &fence.ProfileVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read interrupted task Agent: %w", err)
	}
	if value.Authorization.Version != fence.AuthorizationVersion || value.AgentInstanceID != "" &&
		(value.AgentInstanceID != fence.AgentInstanceID || value.AgentGeneration != fence.AgentGeneration || value.AgentSessionID != fence.AgentRootSessionID ||
			value.AgentEngineVersion != fence.EngineVersion || value.AgentProfileVersion != fence.ProfileVersion) {
		return false, nil
	}
	fence.AgentSessionID = fence.AgentRootSessionID
	return s.CheckRevisionSettlement(ctx, fence)
}

// CheckRevisionSettlement is a read-only, fail-closed proof used after the
// old provider call has already been dispatched. It does not authorize a new
// model call, tool action or publication of the old result.
func (s *Store) CheckRevisionSettlement(ctx context.Context, fence app.ModelTaskFence) (bool, error) {
	if ctx == nil || !domain.ValidTaskID(fence.TaskID) || !domain.ValidUserID(fence.OwnerID) || !domain.ValidUserID(fence.InitiatorID) ||
		fence.LeaseOwner == "" || fence.LeaseVersion < 1 || fence.AgentInstanceID == "" || fence.AgentGeneration < 1 ||
		fence.AgentSessionID == "" || fence.AuthorizationVersion < 1 || fence.EngineVersion == "" || fence.ProfileVersion == "" {
		return false, nil
	}
	rootSessionID := fence.AgentRootSessionID
	if rootSessionID == "" {
		rootSessionID = fence.AgentSessionID
	}
	var proven bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var conversationID, projectID, instanceID string
		err := tx.QueryRow(ctx, `SELECT t.conversation_id,COALESCE(t.project_id,''),r.service_instance_id::text
FROM tasks t JOIN task_revision_intents i ON i.task_id=t.id AND i.expected_revision=t.current_revision AND i.state='stopping'
JOIN conversation_message_routes r ON r.message_id=i.message_id
WHERE t.id=$1 AND t.owner_id=$2 AND t.initiator_id=$3 LIMIT 1`, fence.TaskID, fence.OwnerID, fence.InitiatorID).
			Scan(&conversationID, &projectID, &instanceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("find revision settlement scope: %w", err)
		}
		receipt := app.ConversationMessageReceipt{ServiceInstanceID: instanceID,
			Message: conversation.Message{OwnerID: fence.OwnerID, ActorID: fence.InitiatorID, ConversationID: conversationID},
			Input:   app.ConversationRoutingInput{ProjectID: projectID}}
		if err := lockConversationReceiptScopeInTx(ctx, tx, receipt); err != nil {
			if domain.CodeOf(err) == domain.CodeNotFound || domain.CodeOf(err) == domain.CodeForbidden || domain.CodeOf(err) == domain.CodeUnauthenticated || domain.CodeOf(err) == domain.CodeConflict {
				return nil
			}
			return err
		}
		workspaceFence, err := lockProjectTaskWorkspaceFence(ctx, tx, fence.TaskID, false)
		if err != nil {
			if domain.CodeOf(err) == domain.CodeNotFound || domain.CodeOf(err) == domain.CodeForbidden ||
				domain.CodeOf(err) == domain.CodeUnauthenticated || domain.CodeOf(err) == domain.CodeConflict {
				return nil
			}
			return err
		}
		if workspaceFence.projectID != projectID || workspaceFence.actorID != fence.InitiatorID {
			return nil
		}
		hostTarget, err := lockTaskModelHostTarget(ctx, tx, fence, workspaceFence)
		if err != nil {
			if domain.CodeOf(err) == domain.CodeInvalidState || domain.CodeOf(err) == domain.CodeNotFound || domain.CodeOf(err) == domain.CodeUnauthenticated {
				return nil
			}
			return err
		}
		var messageID, hostID string
		var metadata, authorization []byte
		err = tx.QueryRow(ctx, `SELECT i.message_id,r.request_metadata,r.authorization_snapshot,COALESCE(t.host_id,'')
FROM tasks t JOIN task_revision_intents i ON i.task_id=t.id AND i.owner_id=t.owner_id AND i.initiator_id=t.initiator_id
 AND i.conversation_id=t.conversation_id AND i.expected_revision=t.current_revision AND i.state='stopping'
JOIN conversation_message_routes r ON r.message_id=i.message_id AND r.owner_id=i.owner_id AND r.actor_id=i.initiator_id
 AND r.conversation_id=i.conversation_id AND r.service_instance_id=$4::uuid AND r.state='waiting_adjustment'
JOIN task_stop_requests stop ON stop.id=i.stop_request_id AND stop.task_id=t.id AND stop.action='pause' AND stop.status IN ('pending','completed')
JOIN task_agent_instances agent ON agent.task_id=t.id AND agent.owner_id=t.owner_id AND agent.agent_instance_id=i.interrupted_agent_instance_id
 AND agent.agent_generation=i.interrupted_agent_generation AND agent.lease_owner=i.interrupted_lease_owner AND agent.lease_version=i.interrupted_lease_version
WHERE t.id=$1 AND t.owner_id=$2 AND t.initiator_id=$3 AND t.conversation_id=$5 AND t.project_id IS NOT DISTINCT FROM NULLIF($6,'')
 AND t.status IN ('pausing','paused') AND t.desired_revision>t.current_revision AND t.lease_version=$8+1
 AND i.interrupted_lease_owner=$7 AND i.interrupted_lease_version=$8 AND i.interrupted_agent_instance_id=$9 AND i.interrupted_agent_generation=$10
 AND t.agent_instance_id=$9 AND t.agent_generation=$10 AND t.agent_session_id=$11 AND t.auth_version=$12
 AND agent.agent_session_id=$11 AND agent.auth_version=$12 AND agent.engine_version=$13 AND agent.profile_version=$14
 AND t.auth_kind IN ('session','client') AND EXISTS(SELECT 1 FROM tokens original
   WHERE original.id=t.auth_token_id AND original.owner_id=t.initiator_id AND original.kind=t.auth_kind
     AND original.revoked_at IS NULL AND original.expires_at>clock_timestamp())
 AND EXISTS(SELECT 1 FROM owner_execution_controls gate WHERE gate.owner_id=t.initiator_id AND gate.state='enabled' AND gate.epoch=t.execution_epoch)
 AND EXISTS(SELECT 1 FROM task_agent_sessions session WHERE session.task_id=t.id AND session.owner_id=t.owner_id AND session.session_id=$15)
FOR SHARE OF t,i,r,stop,agent`, fence.TaskID, fence.OwnerID, fence.InitiatorID, instanceID, conversationID, projectID,
			fence.LeaseOwner, fence.LeaseVersion, fence.AgentInstanceID, fence.AgentGeneration, rootSessionID, fence.AuthorizationVersion,
			fence.EngineVersion, fence.ProfileVersion, fence.AgentSessionID).Scan(&messageID, &metadata, &authorization, &hostID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("verify revision interruption evidence: %w", err)
		}
		if projectID != "" {
			if err := verifyProjectTaskWorkspaceFence(ctx, tx, fence.TaskID, workspaceFence); err != nil {
				if domain.CodeOf(err) == domain.CodeConflict {
					return nil
				}
				return err
			}
		}
		if err := verifyTaskModelHostAuthorization(ctx, tx, fence, hostTarget, hostID); err != nil {
			if domain.CodeOf(err) == domain.CodeUnauthenticated {
				return nil
			}
			return err
		}
		claim := app.ConversationRouteClaim{ServiceInstanceID: instanceID,
			Message: conversation.Message{ID: messageID, OwnerID: fence.OwnerID, ActorID: fence.InitiatorID, ConversationID: conversationID}}
		if json.Unmarshal(metadata, &claim.Input) != nil || json.Unmarshal(authorization, &claim.Authorization) != nil || claim.Input.ProjectID != projectID {
			return nil
		}
		stored, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: fence.TaskID, OwnerID: fence.OwnerID})
		if err != nil {
			return fmt.Errorf("read revision task authorization: %w", err)
		}
		allowed, err := requireCurrentTaskAuthorization(ctx, tx, stored)
		if err != nil || !allowed {
			return err
		}
		allowed, err = revisionAuthorizationCurrent(ctx, tx, claim)
		if err != nil || !allowed {
			return err
		}
		proven = true
		return nil
	})
	return proven, err
}
