package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func (s *Store) AdmitAgentToolCall(ctx context.Context, call app.ModelToolCall, recovering bool) (app.AgentToolCallAdmission, error) {
	if call.TaskFence == nil {
		return app.AgentToolCallAdmission{}, domain.InvalidArgument("tool_call_admission", "tool-call admission fence is missing")
	}
	value := *call.TaskFence
	operationKey := app.AgentOperationKey(call.CallID)
	rootSessionID := value.AgentRootSessionID
	if rootSessionID == "" {
		rootSessionID = value.AgentSessionID
	}
	if ctx == nil || call.CallID == "" || len(call.CallID) > 256 || call.Name == "" || len(call.Name) > 128 ||
		!domain.ValidTaskID(value.TaskID) || !domain.ValidUserID(value.OwnerID) ||
		!domain.ValidUserID(value.InitiatorID) || !agentToolAdmissionIdentifier(value.AgentInstanceID) ||
		!agentToolAdmissionIdentifier(value.AgentSessionID) || !agentToolAdmissionIdentifier(rootSessionID) ||
		value.AgentGeneration < 1 || value.AuthorizationVersion < 1 || value.LeaseVersion < 1 || value.LeaseOwner == "" ||
		len(value.LeaseOwner) > 255 || value.ThroughSequence < 0 || value.EngineVersion == "" || len(value.EngineVersion) > 128 ||
		value.ProfileVersion == "" || len(value.ProfileVersion) > 128 || !validAgentToolOperationKey(operationKey) {
		return app.AgentToolCallAdmission{}, domain.InvalidArgument("tool_call_admission", "tool-call admission fence is invalid")
	}
	fence := app.AgentInstanceFence{TaskID: value.TaskID, OwnerID: value.OwnerID, InitiatorID: value.InitiatorID,
		AgentInstanceID: value.AgentInstanceID, AgentGeneration: value.AgentGeneration, AgentSessionID: rootSessionID,
		LeaseOwner: value.LeaseOwner, LeaseVersion: value.LeaseVersion, AuthorizationVersion: value.AuthorizationVersion,
		EngineVersion: value.EngineVersion, ProfileVersion: value.ProfileVersion}
	var admission app.AgentToolCallAdmission
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, err := lockAuthorizedAgentTask(ctx, tx, fence)
		if err != nil {
			return err
		}
		if current.InstanceID != value.AgentInstanceID || current.Generation != value.AgentGeneration || current.SessionID != rootSessionID ||
			current.InitiatorID != value.InitiatorID || current.EngineVersion != value.EngineVersion || current.ProfileVersion != value.ProfileVersion {
			return domain.NewError(domain.CodeUnauthenticated, "tool-call admission fence changed")
		}
		if err := lockActiveAgentLease(ctx, tx, fence); err != nil {
			return err
		}
		if value.AgentSessionID != rootSessionID {
			if err := lockAgentChildSessionActor(ctx, tx, fence, value.AgentSessionID); err != nil {
				return err
			}
			cancelling, err := agentSessionCancellationPending(ctx, tx, value.TaskID, value.OwnerID, value.AgentSessionID)
			if err != nil {
				return err
			}
			if cancelling {
				_, durable, err := agentToolCallDurableState(ctx, tx, value, rootSessionID, operationKey, call)
				if err != nil {
					return err
				}
				if !recovering || !durable {
					return domain.NewError(domain.CodeInvalidState, "Agent Session or ancestor cancellation is pending")
				}
			}
		}
		rootThroughSequence, rootBaselineKnown, err := toolCallRootCheckpointThroughSequence(ctx, tx, value, rootSessionID)
		if err != nil {
			return err
		}

		storedMessages, hasStoredSupersession, err := readTaskAgentToolSupersession(ctx, tx, value, operationKey)
		if err != nil {
			return err
		}
		pending, err := pendingTaskAgentSteers(ctx, tx, value.TaskID, value.OwnerID, value.InitiatorID, current.ProjectID, current.ConversationID)
		if err != nil {
			return err
		}
		appliedFence := value
		appliedFence.ThroughSequence = rootThroughSequence
		applied, err := appliedTaskAgentSteersAfterCheckpoint(ctx, tx, appliedFence, rootSessionID, current.CheckpointThrough)
		if err != nil {
			return err
		}
		pending = mergeAgentToolSupersedingMessages(pending, applied)
		if hasStoredSupersession {
			admission = app.AgentToolCallAdmission{Status: app.AgentToolCallSuperseded,
				PendingMessages: mergeAgentToolSupersedingMessages(storedMessages, pending)}
			return nil
		}
		if len(pending) == 0 {
			if err := persistTaskAgentToolCallAdmission(ctx, tx, value, operationKey, rootSessionID, rootThroughSequence); err != nil {
				return err
			}
			admission = app.AgentToolCallAdmission{Status: app.AgentToolCallAdmitted}
			return nil
		}

		prepared, dispatchedOrTerminal, err := agentToolCallDurableState(ctx, tx, value, rootSessionID, operationKey, call)
		if err != nil {
			return err
		}
		if dispatchedOrTerminal {
			if err := persistTaskAgentToolCallAdmission(ctx, tx, value, operationKey, rootSessionID, rootThroughSequence); err != nil {
				return err
			}
			admission = app.AgentToolCallAdmission{Status: app.AgentToolCallAdmitted}
			return nil
		}
		if value.AgentSessionID != rootSessionID && !rootBaselineKnown && len(applied) != 0 {
			// Historical child sessions have no trustworthy mapping from their
			// local checkpoint sequence to the root DSH checkpoint. Persist the
			// conservative fence so any operation final gate also sees the applied
			// steer, then stop this child once instead of repeatedly superseding
			// fresh call IDs forever.
			if err := persistTaskAgentToolCallAdmission(ctx, tx, value, operationKey, rootSessionID, rootThroughSequence); err != nil {
				return err
			}
			admission = app.AgentToolCallAdmission{Status: app.AgentToolCallBaselineUnverifiable,
				Reason: "This delegated Agent session predates checkpoint fencing and cannot safely continue after newer user input."}
			return nil
		}
		if err := persistTaskAgentToolSupersession(ctx, tx, value.TaskID, value.OwnerID, operationKey, pending); err != nil {
			return err
		}
		if prepared {
			if err := failPreparedAgentToolOperations(ctx, tx, value.TaskID, value.OwnerID, operationKey); err != nil {
				return err
			}
		}
		admission = app.AgentToolCallAdmission{Status: app.AgentToolCallSuperseded, PendingMessages: pending}
		return nil
	})
	return admission, err
}

func agentSessionCancellationPending(ctx context.Context, tx pgx.Tx, taskID, ownerID, sessionID string) (bool, error) {
	var cancelling bool
	err := tx.QueryRow(ctx, `WITH RECURSIVE lineage(session_id,parent_session_id,cancel_requested_at,path) AS (
 SELECT session_id,parent_session_id,cancel_requested_at,ARRAY[session_id] FROM task_agent_sessions
 WHERE task_id=$1 AND owner_id=$2 AND session_id=$3
 UNION ALL
 SELECT p.session_id,p.parent_session_id,p.cancel_requested_at,l.path || p.session_id
 FROM task_agent_sessions p JOIN lineage l ON p.session_id=l.parent_session_id
 WHERE p.task_id=$1 AND p.owner_id=$2 AND NOT p.session_id=ANY(l.path)
)
SELECT EXISTS(SELECT 1 FROM lineage WHERE cancel_requested_at IS NOT NULL)`, taskID, ownerID, sessionID).Scan(&cancelling)
	if err != nil {
		return false, fmt.Errorf("read Agent Session cancellation ancestry: %w", err)
	}
	return cancelling, nil
}

func agentToolCallDurableState(ctx context.Context, tx pgx.Tx, value app.ModelTaskFence, rootSessionID, operationKey string,
	call app.ModelToolCall) (bool, bool, error) {
	prepared, durable, err := agentToolOperationLedgerState(ctx, tx, value.TaskID, value.OwnerID, operationKey)
	if err != nil {
		return false, false, err
	}
	if call.Name == "dsh_delegate" {
		rows, err := tx.Query(ctx, `SELECT spawn_key,request_hash FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND parent_session_id=$3`, value.TaskID, value.OwnerID, value.AgentSessionID)
		if err != nil {
			return false, false, fmt.Errorf("read durable delegated Agent session: %w", err)
		}
		for rows.Next() {
			var spawnKey string
			var requestHash []byte
			if err := rows.Scan(&spawnKey, &requestHash); err != nil {
				rows.Close()
				return false, false, fmt.Errorf("scan durable delegated Agent session: %w", err)
			}
			if value.AgentSessionID != rootSessionID {
				spawnKey = app.AgentSessionToolCallID(value.TaskID, value.AgentSessionID, spawnKey)
			}
			if spawnKey != call.CallID {
				continue
			}
			digest := sha256.Sum256([]byte(call.Arguments))
			if !bytes.Equal(digest[:], requestHash) {
				rows.Close()
				return false, false, domain.NewError(domain.CodeIdempotencyConflict, "delegated Agent call changed its arguments")
			}
			durable = true
			break
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return false, false, fmt.Errorf("iterate durable delegated Agent sessions: %w", err)
		}
		rows.Close()
	}
	if call.Name == "dsh_job_cancel" {
		var arguments struct {
			JobID string `json:"job_id"`
		}
		if json.Unmarshal([]byte(call.Arguments), &arguments) == nil && arguments.JobID != "" {
			var durableSessionID string
			lookupErr := tx.QueryRow(ctx, `SELECT session_id FROM task_agent_child_cancel_operations
WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3`, value.TaskID, value.OwnerID, operationKey).Scan(&durableSessionID)
			if lookupErr != nil && !errors.Is(lookupErr, pgx.ErrNoRows) {
				return false, false, fmt.Errorf("read durable delegated Agent cancellation: %w", lookupErr)
			}
			if lookupErr == nil {
				if durableSessionID != arguments.JobID {
					return false, false, domain.NewError(domain.CodeIdempotencyConflict, "background job cancel call was reused for a different job")
				}
				durable = true
			}
		}
	}
	return prepared, durable, nil
}

func agentToolAdmissionIdentifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func validAgentToolOperationKey(value string) bool {
	if len(value) != len("agent:")+sha256.Size*2 || !strings.HasPrefix(value, "agent:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "agent:"))
	return err == nil && len(decoded) == sha256.Size
}

func pendingTaskAgentSteers(ctx context.Context, tx pgx.Tx, taskID, ownerID, initiatorID, projectID, conversationID string) ([]app.AgentConversationMessage, error) {
	return pendingTaskAgentMessages(ctx, tx, taskID, ownerID, initiatorID, projectID, conversationID, true)
}

func pendingTaskAgentInputMessages(ctx context.Context, tx pgx.Tx, taskID, ownerID, initiatorID, projectID, conversationID string) ([]app.AgentConversationMessage, error) {
	return pendingTaskAgentMessages(ctx, tx, taskID, ownerID, initiatorID, projectID, conversationID, false)
}

func pendingTaskAgentMessages(ctx context.Context, tx pgx.Tx, taskID, ownerID, initiatorID, projectID, conversationID string, steersOnly bool) ([]app.AgentConversationMessage, error) {
	modePredicate := "delivery_mode IN ('steer','followup')"
	if steersOnly {
		modePredicate = "delivery_mode='steer'"
	}
	rows, err := tx.Query(ctx, `SELECT id,content,sequence,delivery_mode FROM conversation_messages
WHERE task_id=$1 AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND role='user'
	 AND source_kind<>'task_result_review' AND delivery_status='pending' AND `+modePredicate+`
ORDER BY sequence`, taskID, ownerID, initiatorID, conversationID)
	if err != nil {
		return nil, fmt.Errorf("read pending Task inputs for tool admission: %w", err)
	}
	messageValues := make([]app.AgentConversationMessage, 0, 4)
	for rows.Next() {
		var message app.AgentConversationMessage
		var mode string
		if err := rows.Scan(&message.ID, &message.Content, &message.Sequence, &mode); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan pending Task input for tool admission: %w", err)
		}
		message.Mode = app.AgentMessageDeliveryMode(mode)
		messageValues = append(messageValues, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read pending Task inputs for tool admission: %w", err)
	}
	rows.Close()
	serviceInstanceID, err := serviceInstanceIDInTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	for index := range messageValues {
		message := &messageValues[index]
		if projectID != "" {
			message.AttachmentIDs, err = loadProjectCheckpointAttachmentIDs(ctx, tx, serviceInstanceID, projectID,
				ownerID, initiatorID, conversationID, message.ID)
		} else {
			message.AttachmentIDs, err = loadConversationMessageAttachmentIDs(ctx, tx, serviceInstanceID, ownerID, conversationID, message.ID)
		}
		if err != nil {
			return nil, fmt.Errorf("load pending Task input attachments for tool admission: %w", err)
		}
	}
	return messageValues, nil
}

func pendingTaskAgentToolSteers(ctx context.Context, tx pgx.Tx, value app.ModelTaskFence, projectID, conversationID string) ([]app.AgentConversationMessage, error) {
	return pendingTaskAgentSteers(ctx, tx, value.TaskID, value.OwnerID, value.InitiatorID, projectID, conversationID)
}

func pendingTaskAgentSteersForLockedTask(ctx context.Context, tx pgx.Tx, taskID, ownerID string) ([]app.AgentConversationMessage, error) {
	var initiatorID, projectID, conversationID string
	if err := tx.QueryRow(ctx, `SELECT initiator_id,COALESCE(project_id,''),conversation_id FROM tasks
WHERE id=$1 AND owner_id=$2`, taskID, ownerID).Scan(&initiatorID, &projectID, &conversationID); err != nil {
		return nil, fmt.Errorf("read Task scope for pending steer gate: %w", err)
	}
	return pendingTaskAgentSteers(ctx, tx, taskID, ownerID, initiatorID, projectID, conversationID)
}

func pendingTaskAgentToolMessagesForOperation(ctx context.Context, tx pgx.Tx, taskID, ownerID, operationKey string) ([]app.AgentConversationMessage, error) {
	pending, err := pendingTaskAgentSteersForLockedTask(ctx, tx, taskID, ownerID)
	if err != nil {
		return nil, err
	}
	var initiatorID, taskRootSessionID, admissionRootSessionID, callSessionID string
	var throughSequence, rootThroughSequence int64
	err = tx.QueryRow(ctx, `SELECT task.initiator_id,task.agent_session_id,admission.root_session_id,
admission.call_session_id,admission.through_sequence,admission.root_through_sequence
FROM task_agent_tool_call_admissions admission
JOIN tasks task ON task.id=admission.task_id AND task.owner_id=admission.owner_id
WHERE admission.task_id=$1 AND admission.owner_id=$2 AND admission.operation_key=$3`,
		taskID, ownerID, operationKey).Scan(&initiatorID, &taskRootSessionID, &admissionRootSessionID,
		&callSessionID, &throughSequence, &rootThroughSequence)
	if errors.Is(err, pgx.ErrNoRows) {
		return pending, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Task tool-call admission fence before dispatch: %w", err)
	}
	if taskRootSessionID == "" || admissionRootSessionID != taskRootSessionID || callSessionID == "" ||
		throughSequence < 0 || rootThroughSequence < -1 || callSessionID == admissionRootSessionID && throughSequence != rootThroughSequence {
		return nil, domain.NewError(domain.CodeUnauthenticated, "tool-call admission root fence changed")
	}
	if callSessionID != admissionRootSessionID {
		cancelling, err := agentSessionCancellationPending(ctx, tx, taskID, ownerID, callSessionID)
		if err != nil {
			return nil, err
		}
		if cancelling {
			return nil, domain.NewError(domain.CodeInvalidState, "Agent Session or ancestor cancellation is pending")
		}
		var persistedRootThrough int64
		if err := tx.QueryRow(ctx, `SELECT root_checkpoint_through_seq FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 AND parent_session_id IS NOT NULL`, taskID, ownerID, callSessionID).
			Scan(&persistedRootThrough); err != nil {
			return nil, domain.NewError(domain.CodeUnauthenticated, "tool-call child session root fence is unavailable")
		}
		if persistedRootThrough != rootThroughSequence {
			return nil, domain.NewError(domain.CodeUnauthenticated, "tool-call child session root fence changed")
		}
	}
	fence := app.ModelTaskFence{TaskID: taskID, OwnerID: ownerID, InitiatorID: initiatorID,
		AgentSessionID: admissionRootSessionID, AgentRootSessionID: admissionRootSessionID, ThroughSequence: rootThroughSequence}
	var currentThrough int64
	if err := tx.QueryRow(ctx, `SELECT checkpoint_through_seq FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, ownerID).Scan(&currentThrough); err != nil {
		return nil, fmt.Errorf("read current Task checkpoint before tool dispatch: %w", err)
	}
	applied, err := appliedTaskAgentSteersAfterCheckpoint(ctx, tx, fence, admissionRootSessionID, currentThrough)
	if err != nil {
		return nil, err
	}
	return mergeAgentToolSupersedingMessages(pending, applied), nil
}

func appliedTaskAgentSteersAfterCheckpoint(ctx context.Context, tx pgx.Tx, value app.ModelTaskFence,
	rootSessionID string, currentCheckpointThrough int64) ([]app.AgentConversationMessage, error) {
	if value.ThroughSequence < -1 || currentCheckpointThrough <= value.ThroughSequence || rootSessionID == "" {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT message.id FROM conversation_messages message
JOIN task_agent_message_deliveries delivery ON delivery.message_id=message.id
WHERE message.task_id=$1 AND message.owner_id=$2 AND message.actor_id=$3 AND message.role='user'
  AND message.source_kind<>'task_result_review' AND message.delivery_status='applied' AND message.delivery_mode='steer'
  AND delivery.task_id=$1 AND delivery.owner_id=$2 AND delivery.initiator_id=$3
  AND delivery.session_id=$4 AND delivery.mode='steer'
  AND delivery.claimed_checkpoint_through_seq>$5
ORDER BY message.sequence,message.id`, value.TaskID, value.OwnerID, value.InitiatorID, rootSessionID, value.ThroughSequence)
	if err != nil {
		return nil, fmt.Errorf("read applied Task steers after tool-call checkpoint: %w", err)
	}
	ids := make([]string, 0, 4)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan applied Task steer after tool-call checkpoint: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read applied Task steers after tool-call checkpoint: %w", err)
	}
	rows.Close()
	if len(ids) == 0 {
		return nil, nil
	}
	return loadTaskAgentToolMessagesByIDs(ctx, tx, value, ids, true)
}

func toolCallRootCheckpointThroughSequence(ctx context.Context, tx pgx.Tx, value app.ModelTaskFence, rootSessionID string) (int64, bool, error) {
	if value.AgentSessionID == rootSessionID {
		return value.ThroughSequence, true, nil
	}
	var rootThroughSequence int64
	var baselineKnown bool
	if err := tx.QueryRow(ctx, `SELECT root_checkpoint_through_seq,root_checkpoint_baseline_known FROM task_agent_sessions
WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 AND parent_session_id IS NOT NULL`,
		value.TaskID, value.OwnerID, value.AgentSessionID).Scan(&rootThroughSequence, &baselineKnown); err != nil {
		return 0, false, fmt.Errorf("read child Agent root checkpoint baseline: %w", err)
	}
	if rootThroughSequence < -1 {
		return 0, false, domain.NewError(domain.CodeInvalidState, "child Agent root checkpoint baseline is invalid")
	}
	return rootThroughSequence, baselineKnown, nil
}

func persistTaskAgentToolCallAdmission(ctx context.Context, tx pgx.Tx, value app.ModelTaskFence,
	operationKey, rootSessionID string, rootThroughSequence int64) error {
	_, err := tx.Exec(ctx, `INSERT INTO task_agent_tool_call_admissions
	(task_id,owner_id,operation_key,agent_instance_id,agent_generation,root_session_id,call_session_id,through_sequence,root_through_sequence)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT (task_id,operation_key) DO NOTHING`,
		value.TaskID, value.OwnerID, operationKey, value.AgentInstanceID, value.AgentGeneration,
		rootSessionID, value.AgentSessionID, value.ThroughSequence, rootThroughSequence)
	if err != nil {
		return fmt.Errorf("persist Task tool-call admission fence: %w", err)
	}
	var storedInstance, storedRoot, storedSession string
	var storedGeneration, storedThrough int64
	var storedRootThroughSequence int64
	if err := tx.QueryRow(ctx, `SELECT agent_instance_id,agent_generation,root_session_id,call_session_id,through_sequence,root_through_sequence
FROM task_agent_tool_call_admissions WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3 FOR UPDATE`,
		value.TaskID, value.OwnerID, operationKey).Scan(&storedInstance, &storedGeneration, &storedRoot, &storedSession, &storedThrough, &storedRootThroughSequence); err != nil {
		return fmt.Errorf("read persisted Task tool-call admission fence: %w", err)
	}
	if storedRoot != rootSessionID || storedSession != value.AgentSessionID || storedThrough != value.ThroughSequence ||
		storedRootThroughSequence != rootThroughSequence || storedGeneration > value.AgentGeneration ||
		storedGeneration == value.AgentGeneration && storedInstance != value.AgentInstanceID {
		return domain.NewError(domain.CodeIdempotencyConflict, "tool-call admission key was reused with a different session fence")
	}
	if storedGeneration < value.AgentGeneration {
		if _, err := tx.Exec(ctx, `UPDATE task_agent_tool_call_admissions SET agent_instance_id=$4,agent_generation=$5,updated_at=clock_timestamp()
WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3 AND agent_generation<=$5`,
			value.TaskID, value.OwnerID, operationKey, value.AgentInstanceID, value.AgentGeneration); err != nil {
			return fmt.Errorf("rebind Task tool-call admission fence: %w", err)
		}
	}
	return nil
}

func readTaskAgentToolSupersession(ctx context.Context, tx pgx.Tx, value app.ModelTaskFence, operationKey string) ([]app.AgentConversationMessage, bool, error) {
	rows, err := tx.Query(ctx, `SELECT message_id FROM task_agent_tool_supersessions
WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3 ORDER BY position`, value.TaskID, value.OwnerID, operationKey)
	if err != nil {
		return nil, false, fmt.Errorf("read superseded Task tool call messages: %w", err)
	}
	ids := make([]string, 0, 4)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, false, fmt.Errorf("scan superseded Task tool call message: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, false, fmt.Errorf("read superseded Task tool call messages: %w", err)
	}
	rows.Close()
	if len(ids) == 0 {
		return nil, false, nil
	}
	messages, err := loadTaskAgentToolMessagesByIDs(ctx, tx, value, ids, true)
	if err != nil {
		return nil, false, err
	}
	return messages, true, nil
}

func loadTaskAgentToolMessagesByIDs(ctx context.Context, tx pgx.Tx, value app.ModelTaskFence, ids []string, allowApplied bool) ([]app.AgentConversationMessage, error) {
	var projectID, conversationID string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(project_id,''),conversation_id FROM tasks WHERE id=$1 AND owner_id=$2`,
		value.TaskID, value.OwnerID).Scan(&projectID, &conversationID); err != nil {
		return nil, fmt.Errorf("read Task scope for superseding messages: %w", err)
	}
	serviceInstanceID, err := serviceInstanceIDInTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	statusPredicate := "delivery_status='pending'"
	if allowApplied {
		statusPredicate = "delivery_status IN ('pending','applied')"
	}
	messages := make([]app.AgentConversationMessage, 0, len(ids))
	for _, id := range ids {
		var message app.AgentConversationMessage
		var actorID, mode string
		query := `SELECT id,content,sequence,actor_id,delivery_mode FROM conversation_messages
WHERE id=$1 AND task_id=$2 AND owner_id=$3 AND conversation_id=$4 AND role='user'
 AND source_kind<>'task_result_review' AND ` + statusPredicate + ` AND delivery_mode='steer'`
		if err := tx.QueryRow(ctx, query, id, value.TaskID, value.OwnerID, conversationID).
			Scan(&message.ID, &message.Content, &message.Sequence, &actorID, &mode); errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NewError(domain.CodeConflict, "superseding Task message is no longer available")
		} else if err != nil {
			return nil, fmt.Errorf("read superseding Task message: %w", err)
		}
		if actorID != value.InitiatorID || mode != string(app.AgentMessageDeliverySteer) {
			return nil, domain.NewError(domain.CodeUnauthenticated, "superseding Task message is outside the actor scope")
		}
		message.Mode = app.AgentMessageDeliverySteer
		if projectID != "" {
			message.AttachmentIDs, err = loadProjectCheckpointAttachmentIDs(ctx, tx, serviceInstanceID, projectID,
				value.OwnerID, value.InitiatorID, conversationID, id)
		} else {
			message.AttachmentIDs, err = loadConversationMessageAttachmentIDs(ctx, tx, serviceInstanceID, value.OwnerID, conversationID, id)
		}
		if err != nil {
			return nil, fmt.Errorf("load superseding Task message attachments: %w", err)
		}
		messages = append(messages, message)
	}
	return messages, nil
}

func mergeAgentToolSupersedingMessages(left, right []app.AgentConversationMessage) []app.AgentConversationMessage {
	byID := make(map[string]app.AgentConversationMessage, len(left)+len(right))
	for _, message := range left {
		byID[message.ID] = message
	}
	for _, message := range right {
		byID[message.ID] = message
	}
	result := make([]app.AgentConversationMessage, 0, len(byID))
	for _, message := range byID {
		result = append(result, message)
	}
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].Sequence < result[i].Sequence {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	return result
}

func persistTaskAgentToolSupersession(ctx context.Context, tx pgx.Tx, taskID, ownerID, operationKey string,
	messages []app.AgentConversationMessage) error {
	if len(messages) == 0 {
		return domain.NewError(domain.CodeInvalidState, "superseding Task message snapshot is empty")
	}
	for position, message := range messages {
		if message.Mode != app.AgentMessageDeliverySteer || message.ID == "" || message.Sequence < 1 {
			return domain.NewError(domain.CodeInvalidState, "superseding Task message snapshot is invalid")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_agent_tool_supersessions(task_id,owner_id,operation_key,message_id,position)
VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, taskID, ownerID, operationKey, message.ID, position); err != nil {
			return fmt.Errorf("persist superseded Task tool call message: %w", err)
		}
	}
	var storedIDs []string
	rows, err := tx.Query(ctx, `SELECT message_id FROM task_agent_tool_supersessions
WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3 ORDER BY position`, taskID, ownerID, operationKey)
	if err != nil {
		return fmt.Errorf("verify superseded Task tool call messages: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan superseded Task tool call message: %w", err)
		}
		storedIDs = append(storedIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("verify superseded Task tool call messages: %w", err)
	}
	rows.Close()
	if len(storedIDs) != len(messages) {
		return domain.NewError(domain.CodeIdempotencyConflict, "tool-call supersession key was reused for different messages")
	}
	for index := range messages {
		if storedIDs[index] != messages[index].ID {
			return domain.NewError(domain.CodeIdempotencyConflict, "tool-call supersession key was reused for different messages")
		}
	}
	return nil
}

func agentToolOperationLedgerState(ctx context.Context, tx pgx.Tx, taskID, ownerID, operationKey string) (prepared, dispatchedOrTerminal bool, err error) {
	if err := tx.QueryRow(ctx, `SELECT
EXISTS(SELECT 1 FROM task_operations WHERE task_id=$1 AND operation_key=$3 AND status='prepared') OR
EXISTS(SELECT 1 FROM task_mcp_connector_operations WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3 AND status='prepared') OR
EXISTS(SELECT 1 FROM task_hub_operations WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3 AND status='prepared') OR
EXISTS(SELECT 1 FROM task_workspace_publish_receipts WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3 AND state='pending'),
EXISTS(SELECT 1 FROM task_operations WHERE task_id=$1 AND operation_key=$3 AND status<>'prepared') OR
EXISTS(SELECT 1 FROM task_mcp_connector_operations WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3 AND status<>'prepared') OR
EXISTS(SELECT 1 FROM task_hub_operations WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3 AND status<>'prepared') OR
EXISTS(SELECT 1 FROM task_workspace_publish_receipts WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3 AND state<>'pending')`,
		taskID, ownerID, operationKey).Scan(&prepared, &dispatchedOrTerminal); err != nil {
		return false, false, fmt.Errorf("read durable tool-call ledger state: %w", err)
	}
	return prepared, dispatchedOrTerminal, nil
}

func failPreparedAgentToolOperations(ctx context.Context, tx pgx.Tx, taskID, ownerID, operationKey string) error {
	var operationID string
	updatedErr := tx.QueryRow(ctx, `UPDATE task_operations SET status='failed',external_id='',result=$3,updated_at=clock_timestamp()
WHERE task_id=$1 AND operation_key=$2 AND status='prepared' RETURNING id`, taskID, operationKey,
		app.TaskAgentToolSupersededOperationResult).Scan(&operationID)
	if updatedErr != nil && !errors.Is(updatedErr, pgx.ErrNoRows) {
		return fmt.Errorf("mark prepared Task operation superseded: %w", updatedErr)
	}
	if updatedErr == nil {
		payload, err := json.Marshal(map[string]any{"operation_id": operationID, "operation_key": operationKey,
			"status": string(task.OperationFailed), "reason": "task_input_superseded"})
		if err != nil {
			return fmt.Errorf("encode superseded Task operation event: %w", err)
		}
		sequence, err := incrementTaskEventSequence(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if err := insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: sequence, Type: "task.operation", Payload: payload}); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations SET status='failed',result=$4,updated_at=clock_timestamp()
WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3 AND status='prepared'`, taskID, ownerID, operationKey,
		app.TaskAgentToolSupersededOperationResult); err != nil {
		return fmt.Errorf("mark prepared MCP operation superseded: %w", err)
	}
	markerHash := sha256.Sum256([]byte(app.TaskAgentToolSupersededOperationResult))
	if _, err := tx.Exec(ctx, `UPDATE task_hub_operations SET status='failed',result_hash=$4,updated_at=clock_timestamp()
WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3 AND status='prepared'`, taskID, ownerID, operationKey, markerHash[:]); err != nil {
		return fmt.Errorf("mark prepared Hub operation superseded: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM task_file_fetches
WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3 AND status='queued'`, taskID, ownerID, operationKey); err != nil {
		return fmt.Errorf("remove undispatched Task file fetch: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE task_workspace_publish_receipts SET state='superseded',updated_at=clock_timestamp()
WHERE task_id=$1 AND owner_id=$2 AND operation_key=$3 AND state='pending'`, taskID, ownerID, operationKey); err != nil {
		return fmt.Errorf("mark pending Task Workspace publish superseded: %w", err)
	}
	return nil
}
