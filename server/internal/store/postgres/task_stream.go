package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
)

// AppendCloudDSHTextDelta starts streaming only if the conversation has no
// competing accepted message, active task or unpublished result. Once the
// first delta is visible, its exact message/turn remains the stream identity.
func (s *Store) AppendCloudDSHTextDelta(ctx context.Context, delta app.CloudDSHTextDelta) (bool, error) {
	fence := delta.Fence
	root := fence.AgentRootSessionID
	if root == "" {
		root = fence.AgentSessionID
	}
	if ctx == nil || !domain.ValidTaskID(fence.TaskID) || !domain.ValidUserID(fence.OwnerID) || !domain.ValidUserID(fence.InitiatorID) ||
		fence.AgentInstanceID == "" || fence.AgentGeneration < 1 || fence.AgentSessionID == "" || root != fence.AgentSessionID ||
		fence.AuthorizationVersion < 1 || fence.LeaseOwner == "" || fence.LeaseVersion < 1 || fence.ThroughSequence < 0 ||
		fence.EngineVersion == "" || fence.ProfileVersion == "" || !domain.ValidMessageID(delta.MessageID) || !domain.ValidStepID(delta.TurnID) ||
		delta.Text == "" || len(delta.Text) > app.MaxAssistantMessageBytes || !utf8.ValidString(delta.Text) || strings.ContainsRune(delta.Text, '\x00') {
		return false, domain.InvalidArgument("text_delta", "cloud dsh text delta is invalid")
	}
	callID, err := app.DSHModelCallID(fence.TaskID, fence.AgentSessionID, fence.ThroughSequence)
	if err != nil || delta.CallID != callID {
		return false, domain.InvalidArgument("text_delta", "cloud dsh model call identity is invalid")
	}
	var appended bool
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, fence.InitiatorID); err != nil {
			return err
		}
		var dispatchModelID string
		var dispatchRevision int64
		err := tx.QueryRow(ctx, `SELECT d.model_id,d.task_revision FROM model_text_streaming_dispatches d
JOIN model_call_ledger ledger ON ledger.owner_id=d.owner_id AND ledger.source=d.source AND ledger.call_id=d.call_id
JOIN model_connections model ON model.id=d.model_id AND model.updated_at=d.connection_version
WHERE d.owner_id=$1 AND d.initiator_id=$1 AND d.task_owner_id=$14 AND d.source='task' AND d.call_id=$2 AND d.task_id=$3
 AND d.agent_instance_id=$4 AND d.agent_generation=$5 AND d.agent_session_id=$6 AND d.agent_root_session_id=$7
 AND d.lease_owner=$8 AND d.lease_version=$9 AND d.authorization_version=$10 AND d.through_sequence=$11
 AND d.engine_version=$12 AND d.profile_version=$13 AND ledger.state='dispatched' AND ledger.task_id=d.task_id
 AND ledger.model_id=d.model_id AND ledger.provider_model=model.model_name AND model.checked_at IS NOT NULL
 AND model.capability_text AND (model.owner_id IS NULL OR model.owner_id=$1)
FOR SHARE OF d,ledger,model`, fence.InitiatorID, delta.CallID, fence.TaskID, fence.AgentInstanceID, fence.AgentGeneration,
			fence.AgentSessionID, root, fence.LeaseOwner, fence.LeaseVersion, fence.AuthorizationVersion, fence.ThroughSequence,
			fence.EngineVersion, fence.ProfileVersion, fence.OwnerID).Scan(&dispatchModelID, &dispatchRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "stream model dispatch is unavailable")
		}
		if err != nil {
			return fmt.Errorf("lock stream model dispatch: %w", err)
		}
		var conversationID, projectID, instanceID string
		err = tx.QueryRow(ctx, `SELECT t.conversation_id,COALESCE(t.project_id,''),i.instance_id::text
FROM tasks t CROSS JOIN service_identity i WHERE t.id=$1 AND t.owner_id=$2 AND t.initiator_id=$3 AND i.singleton=true`,
			fence.TaskID, fence.OwnerID, fence.InitiatorID).Scan(&conversationID, &projectID, &instanceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "stream task is unavailable")
		}
		if err != nil {
			return fmt.Errorf("read stream task scope: %w", err)
		}
		receipt := app.ConversationMessageReceipt{ServiceInstanceID: instanceID,
			Message: conversation.Message{OwnerID: fence.OwnerID, ActorID: fence.InitiatorID, ConversationID: conversationID},
			Input:   app.ConversationRoutingInput{ProjectID: projectID}}
		if err := lockConversationReceiptScopeInTx(ctx, tx, receipt); err != nil {
			return err
		}
		projectWorkspaceFence, err := lockProjectTaskWorkspaceFence(ctx, tx, fence.TaskID, false)
		if err != nil {
			return err
		}
		hostTarget := agentHostTarget{TaskActorID: fence.InitiatorID}
		if projectWorkspaceFence.hostID != "" {
			var observedHostOwner string
			if err := tx.QueryRow(ctx, `SELECT owner_id FROM hosts WHERE id=$1 AND deleted_at IS NULL`,
				projectWorkspaceFence.hostID).Scan(&observedHostOwner); err != nil {
				return domain.NewError(domain.CodeUnauthenticated, "stream Host authorization is unavailable")
			}
			if projectWorkspaceFence.projectID != "" && projectWorkspaceFence.workspaceID != "" &&
				observedHostOwner != projectWorkspaceFence.workspaceOwnerID {
				return domain.NewError(domain.CodeUnauthenticated, "stream Host authorization is unavailable")
			}
			if err := lockTaskHostOwnerGates(ctx, tx, fence.InitiatorID, observedHostOwner); err != nil {
				return err
			}
			hostTarget, err = lockAgentHostTargetIdentity(ctx, tx, fence.TaskID, fence.OwnerID,
				projectWorkspaceFence.hostID, fence.InitiatorID)
			if err != nil || hostTarget.OwnerID != observedHostOwner {
				return domain.NewError(domain.CodeUnauthenticated, "stream Host authorization is unavailable")
			}
		}
		agentFence := app.AgentInstanceFence{TaskID: fence.TaskID, OwnerID: fence.OwnerID, InitiatorID: fence.InitiatorID,
			AgentInstanceID: fence.AgentInstanceID, AgentGeneration: fence.AgentGeneration, AgentSessionID: root,
			LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion, AuthorizationVersion: fence.AuthorizationVersion,
			EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion}
		current, err := lockAuthorizedAgentTask(ctx, tx, agentFence)
		if err != nil {
			return err
		}
		if err := verifyProjectTaskWorkspaceFence(ctx, tx, fence.TaskID, projectWorkspaceFence); err != nil {
			return err
		}
		if current.InstanceID != fence.AgentInstanceID || current.Generation != fence.AgentGeneration || current.SessionID != root ||
			current.EngineVersion != fence.EngineVersion || current.ProfileVersion != fence.ProfileVersion ||
			current.ConversationID != conversationID || current.ProjectID != projectID ||
			current.HostID != projectWorkspaceFence.hostID || current.CheckpointThrough != fence.ThroughSequence {
			return domain.NewError(domain.CodeUnauthenticated, "stream agent fence changed")
		}
		if err := lockActiveAgentLease(ctx, tx, agentFence); err != nil {
			return err
		}
		if err := verifyTaskModelHostAuthorization(ctx, tx, fence, hostTarget, current.HostID); err != nil {
			return err
		}
		var currentRevision, desiredRevision int64
		var taskModelID string
		if err := tx.QueryRow(ctx, `SELECT current_revision,desired_revision,model_id FROM tasks WHERE id=$1`, fence.TaskID).Scan(&currentRevision, &desiredRevision, &taskModelID); err != nil {
			return fmt.Errorf("read stream task revision: %w", err)
		}
		if currentRevision < 0 || currentRevision != desiredRevision || currentRevision != dispatchRevision || taskModelID != dispatchModelID {
			return domain.NewError(domain.CodeInvalidState, "stream task revision changed")
		}
		var continuingVariant bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversation_message_variants
WHERE task_id=$1 AND action='continue_generation' AND state IN ('accepted','running','unknown'))`, fence.TaskID).Scan(&continuingVariant); err != nil {
			return fmt.Errorf("read continuation variant stream policy: %w", err)
		}
		if delta.PersonalProof != nil {
			if projectID != "" || fence.InitiatorID != fence.OwnerID {
				return domain.NewError(domain.CodeUnauthenticated, "personal stream proof is outside its scope")
			}
			if err := requireCloudDSHStreamPersonalProof(ctx, tx, instanceID, fence.OwnerID, *delta.PersonalProof); err != nil {
				return err
			}
		}
		var previousID, previousTurn, previousContent, status string
		var previousRevision pgtype.Int8
		err = tx.QueryRow(ctx, `SELECT message_id,turn_id,content,status,task_revision FROM task_assistant_message_snapshots
WHERE task_id=$1 FOR UPDATE`, fence.TaskID).Scan(&previousID, &previousTurn, &previousContent, &status, &previousRevision)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read stream snapshot: %w", err)
		}
		hasSnapshot := err == nil
		sameRevision := hasSnapshot && (previousRevision.Valid && previousRevision.Int64 == currentRevision ||
			!previousRevision.Valid && currentRevision == 0)
		continuation := sameRevision && previousID == delta.MessageID && previousTurn == delta.TurnID && status == string(app.AssistantMessageStreaming)
		if hasSnapshot && !continuation {
			if sameRevision && !continuingVariant {
				return nil
			}
			if status != string(app.AssistantMessageCompleted) {
				// Only replace the current snapshot after its exact visible content
				// has been retained as an interrupted conversation message.
				var archived bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversation_messages
WHERE id=$1 AND task_id=$2 AND conversation_id=$3 AND owner_id=$4 AND actor_id=$5
 AND content=$6 AND output_status='interrupted' AND output_task_revision=$7)`,
					previousID, fence.TaskID, conversationID, fence.OwnerID, fence.InitiatorID, previousContent, previousRevision).Scan(&archived); err != nil {
					return fmt.Errorf("check archived assistant stream: %w", err)
				}
				if status != string(app.AssistantMessageInterrupted) || !archived {
					return nil
				}
			}
		}
		if !continuation {
			var competing, routed bool
			if err := tx.QueryRow(ctx, `SELECT
EXISTS (SELECT 1 FROM tasks WHERE owner_id=$1 AND initiator_id=$2 AND conversation_id=$3 AND id<>$4
 AND status IN ('queued','running','pausing')) OR
EXISTS (SELECT 1 FROM conversation_message_routes r JOIN conversation_messages m ON m.id=r.message_id
 WHERE r.owner_id=$1 AND r.actor_id=$2 AND r.conversation_id=$3 AND r.state IN ('pending','processing','waiting_adjustment')
 AND COALESCE(m.task_id,'')<>$4) OR
EXISTS (SELECT 1 FROM conversation_result_batches b WHERE b.service_instance_id=$5::uuid AND b.owner_id=$1
 AND b.actor_id=$2 AND b.conversation_id=$3 AND b.state='pending'),
EXISTS (SELECT 1 FROM conversation_messages m JOIN conversation_message_routes r ON r.message_id=m.id
 WHERE m.task_id=$4 AND m.owner_id=$1 AND m.actor_id=$2 AND m.conversation_id=$3
 AND r.service_instance_id=$5::uuid AND r.owner_id=$1 AND r.actor_id=$2 AND r.state='routed')`, fence.OwnerID, fence.InitiatorID,
				conversationID, fence.TaskID, instanceID).Scan(&competing, &routed); err != nil {
				return fmt.Errorf("check competing conversation work: %w", err)
			}
			if competing || !routed {
				return nil
			}
		}
		if !continuation {
			message := conversation.Message{ID: delta.MessageID, ConversationID: conversationID, OwnerID: fence.OwnerID,
				ActorID: fence.InitiatorID, Role: conversation.RoleAssistant, Content: delta.Text, TaskID: fence.TaskID}
			if err := insertMessage(ctx, tx, message); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE conversation_message_variants SET assistant_message_id=$2,updated_at=clock_timestamp()
WHERE task_id=$1 AND state IN ('accepted','running','unknown') AND (assistant_message_id IS NULL OR assistant_message_id=$2)`,
				fence.TaskID, delta.MessageID); err != nil {
				return fmt.Errorf("link streamed assistant message to conversation variant: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE conversation_messages SET output_status='streaming',output_task_revision=$2
WHERE id=$1 AND task_id=$3`, delta.MessageID, currentRevision, fence.TaskID); err != nil {
				return fmt.Errorf("initialize assistant stream message: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE conversations SET updated_at=clock_timestamp() WHERE id=$1 AND owner_id=$2`, conversationID, fence.OwnerID); err != nil {
				return fmt.Errorf("touch streamed conversation: %w", err)
			}
		} else {
			updated, err := tx.Exec(ctx, `UPDATE conversation_messages SET content=content || $1
WHERE id=$2 AND task_id=$3 AND conversation_id=$4 AND owner_id=$5 AND actor_id=$6
 AND output_status='streaming' AND output_task_revision=$7 AND content=$8`, delta.Text, delta.MessageID,
				fence.TaskID, conversationID, fence.OwnerID, fence.InitiatorID, currentRevision, previousContent)
			if err != nil {
				return fmt.Errorf("append assistant stream message: %w", err)
			}
			if updated.RowsAffected() != 1 {
				return domain.NewError(domain.CodeInvalidState, "assistant stream message changed")
			}
		}
		payload, err := json.Marshal(map[string]any{"role": "assistant", "message_id": delta.MessageID, "turn_id": delta.TurnID, "delta": delta.Text, "task_revision": currentRevision})
		if err != nil {
			return fmt.Errorf("encode cloud dsh text delta: %w", err)
		}
		sequence, err := incrementTaskEventSequence(ctx, tx, fence.TaskID)
		if err != nil {
			return err
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: fence.TaskID, Sequence: sequence, Type: "task.message", Payload: payload}); err != nil {
			return err
		}
		if err := appendAssistantMessageSnapshot(ctx, tx, app.AssistantTextDelta{TaskID: fence.TaskID, LeaseOwner: fence.LeaseOwner,
			LeaseVersion: fence.LeaseVersion, MessageID: delta.MessageID, TurnID: delta.TurnID, Delta: delta.Text}); err != nil {
			return err
		}
		if err := recordAccountObjectChanges(ctx, tx,
			accountObjectChange{OwnerID: fence.OwnerID, ObjectType: "conversation", ObjectID: conversationID, Operation: "upsert"},
			accountObjectChange{OwnerID: fence.OwnerID, ObjectType: "message", ObjectID: delta.MessageID, Operation: "upsert"},
		); err != nil {
			return err
		}
		appended = true
		return nil
	})
	return appended, err
}

func requireCloudDSHStreamPersonalProof(ctx context.Context, tx pgx.Tx, instanceID, ownerID string, proof app.CloudDSHStreamPersonalProof) error {
	invalid := func() error {
		return domain.NewError(domain.CodeInvalidState, "personal context changed before streaming")
	}
	if proof.SettingRevision < 0 || len(proof.Objects) > 184 {
		return invalid()
	}
	// Personal mutations take this counter before touching an object, so both
	// a saved preference and the default-on missing preference stay stable until this commit.
	if _, err := lockAccountChangeCounter(ctx, tx, instanceID, ownerID); err != nil {
		return err
	}
	var settingRevision int64
	var deleted bool
	var body []byte
	err := tx.QueryRow(ctx, `SELECT v.revision,v.is_deleted,p.body FROM account_object_versions v
LEFT JOIN personal_objects p ON p.service_instance_id=v.service_instance_id AND p.owner_id=v.owner_id
 AND p.object_type=v.object_type AND p.object_id=v.object_id
WHERE v.service_instance_id=$1::uuid AND v.owner_id=$2 AND v.object_type=$3 AND v.object_id='builtin_memory'`,
		instanceID, ownerID, personal.ObjectTypeSkillSetting).Scan(&settingRevision, &deleted, &body)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && deleted {
		if !proof.Enabled || proof.SettingRevision != 0 {
			return invalid()
		}
	} else if err != nil {
		return fmt.Errorf("read personal stream consent: %w", err)
	} else {
		var setting struct {
			Enabled bool `json:"enabled"`
		}
		if settingRevision != proof.SettingRevision || len(body) == 0 ||
			personal.ValidateObjectBody(personal.ObjectTypeSkillSetting, "builtin_memory", body) != nil ||
			json.Unmarshal(body, &setting) != nil || setting.Enabled != proof.Enabled {
			return invalid()
		}
	}
	seen := make(map[string]struct{}, len(proof.Objects))
	for _, object := range proof.Objects {
		switch object.ObjectType {
		case personal.ObjectTypeMemory, personal.ObjectTypeProfile, personal.ObjectTypeSoul, personal.ObjectTypeGoal:
		default:
			return invalid()
		}
		key := object.ObjectType + "\x00" + object.ID
		if _, exists := seen[key]; exists || object.Revision < 1 || personal.ValidateObjectIdentifier(object.ObjectType, object.ID) != nil ||
			object.ObjectType == personal.ObjectTypeMemory && !proof.Enabled {
			return invalid()
		}
		seen[key] = struct{}{}
		var revision int64
		var isDeleted, hasBody bool
		err := tx.QueryRow(ctx, `SELECT v.revision,v.is_deleted,(p.body IS NOT NULL) FROM account_object_versions v
LEFT JOIN personal_objects p ON p.service_instance_id=v.service_instance_id AND p.owner_id=v.owner_id
 AND p.object_type=v.object_type AND p.object_id=v.object_id
WHERE v.service_instance_id=$1::uuid AND v.owner_id=$2 AND v.object_type=$3 AND v.object_id=$4`,
			instanceID, ownerID, object.ObjectType, object.ID).Scan(&revision, &isDeleted, &hasBody)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && (isDeleted || !hasBody || revision != object.Revision) {
			return invalid()
		}
		if err != nil {
			return fmt.Errorf("read personal stream object: %w", err)
		}
	}
	return nil
}
