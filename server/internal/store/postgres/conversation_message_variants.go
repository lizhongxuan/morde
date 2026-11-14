package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type conversationVariantSource struct {
	ID                        string
	OwnerID                   string
	ActorID                   string
	Role                      string
	Content                   string
	TaskID                    string
	Sequence                  int64
	ModelID                   string
	VariantGroupID            string
	VariantVersion            int64
	VariantAction             string
	OutputStatus              string
	OutputRevision            *int64
	SourceUserMessageID       string
	SourceUserContent         string
	SourceAttachmentMessageID string
	SourceUserMetadata        []byte
}

func (s *Store) CreateConversationMessageVariant(ctx context.Context, principal identity.Principal, command app.CreateConversationMessageVariantCommand) (app.ConversationMessageVariantOperation, error) {
	if !domain.ValidUserID(principal.UserID) || !domain.ValidConversationID(command.ConversationID) || !domain.ValidMessageID(command.SourceMessageID) ||
		!domain.ValidOperationID(command.OperationID) || !domain.ValidServiceInstanceID(command.ServiceInstanceID) || len(command.RequestHash) != 32 ||
		!validConversationVariantAction(command.Action) || command.ExpectedVersion < 1 {
		return app.ConversationMessageVariantOperation{}, domain.InvalidArgument("variant", "conversation message variant is invalid")
	}
	if command.Action != app.ConversationMessageVariantContinue && !domain.ValidMessageID(command.UserMessageID) {
		return app.ConversationMessageVariantOperation{}, domain.InvalidArgument("variant", "variant input message identity is invalid")
	}
	var result app.ConversationMessageVariantOperation
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireConversationServiceInstance(ctx, tx, command.ServiceInstanceID); err != nil {
			return err
		}
		ownerID, err := lockVariantConversationScope(ctx, tx, principal, command.ProjectID, command.ConversationID)
		if err != nil {
			return err
		}
		var existing variantOperationRow
		var existingHash []byte
		err = tx.QueryRow(ctx, `SELECT operation_id,service_instance_id::text,owner_id,actor_id,COALESCE(project_id,''),conversation_id,
source_message_id,variant_group_id,variant_version,action,state,COALESCE(task_id,''),COALESCE(user_message_id,''),
COALESCE(assistant_message_id,''),request_hash
FROM conversation_message_variants
			WHERE service_instance_id=$1::uuid AND actor_id=$2 AND conversation_id=$3 AND idempotency_key=$4 FOR UPDATE`,
			command.ServiceInstanceID, principal.UserID, command.ConversationID, command.IdempotencyKey).
			Scan(&existing.OperationID, &existing.ServiceInstanceID, &existing.OwnerID, &existing.ActorID, &existing.ProjectID, &existing.ConversationID,
				&existing.SourceMessageID, &existing.VariantGroupID, &existing.VariantVersion, &existing.Action, &existing.State,
				&existing.TaskID, &existing.UserMessageID, &existing.AssistantMessageID, &existingHash)
		if err == nil {
			if !reflect.DeepEqual(existingHash, command.RequestHash) {
				return domain.NewError(domain.CodeIdempotencyConflict, "idempotency key was used for a different conversation variant")
			}
			result = existing.operation(true)
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read conversation variant idempotency record: %w", err)
		}

		source, err := loadConversationVariantSource(ctx, tx, command.ProjectID, ownerID, principal.UserID,
			command.ConversationID, command.SourceMessageID)
		if err != nil {
			return err
		}
		if source.ActorID != principal.UserID {
			return domain.NewError(domain.CodeForbidden, "only the message author may create a variant")
		}
		rootID := source.VariantGroupID
		if rootID == "" {
			rootID = source.SourceUserMessageID
		}
		if !domain.ValidMessageID(rootID) {
			return domain.NewError(domain.CodeInvalidState, "conversation variant group is unavailable")
		}
		var highest int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(variant_version),1) FROM conversation_message_variants
WHERE variant_group_id=$1`, rootID).Scan(&highest); err != nil {
			return fmt.Errorf("read conversation variant version: %w", err)
		}
		if highest != command.ExpectedVersion {
			return domain.NewError(domain.CodeConflict, "conversation variant version changed")
		}
		if highest >= int64(^uint64(0)>>1) {
			return domain.NewError(domain.CodeInvalidState, "conversation variant version is exhausted")
		}
		version := highest + 1
		if err := validateVariantSourceForAction(source, command.Action); err != nil {
			return err
		}
		inputSourceID := source.SourceUserMessageID
		if !domain.ValidMessageID(inputSourceID) {
			return domain.NewError(domain.CodeInvalidState, "conversation source message is unavailable")
		}
		attachmentSourceID := source.SourceAttachmentMessageID
		if !domain.ValidMessageID(attachmentSourceID) {
			attachmentSourceID = inputSourceID
		}
		var taskID string
		if command.Action == app.ConversationMessageVariantContinue {
			taskID, err = verifyConversationVariantResume(ctx, tx, source)
			if err != nil {
				return err
			}
			attachmentSourceID = ""
		}
		var input app.ConversationRoutingInput
		if command.Action != app.ConversationMessageVariantContinue {
			if len(source.SourceUserMetadata) == 0 || json.Unmarshal(source.SourceUserMetadata, &input) != nil || input.ProjectID != command.ProjectID {
				return domain.NewError(domain.CodeInvalidState, "source message routing context is unavailable")
			}
			input.ProjectID = command.ProjectID
			input.VariantGroupID, input.VariantVersion = rootID, version
			input.VariantAction, input.VariantOperationID = string(command.Action), command.OperationID
			input.VariantSourceMessageID = attachmentSourceID
			input.PreviousClientMessageID = ""
			input.ReferenceTaskID = ""
			if command.Action == app.ConversationMessageVariantRegenerateText {
				input.HostID, input.WorkspaceID, input.ApplicationID = "", "", ""
				input.ChangeTaskTarget, input.MCPConnectorIDs = false, nil
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO conversation_message_variants
				(operation_id,service_instance_id,owner_id,actor_id,project_id,conversation_id,source_message_id,input_source_message_id,
attachment_source_message_id,variant_group_id,expected_version,variant_version,action,state,idempotency_key,request_hash,task_id)
				VALUES ($1,$2::uuid,$3,$4,NULLIF($5,''),$6,$7,$8,NULLIF($9,''),$10,$11,$12,$13,'accepted',$14,$15,NULLIF($16,''))`,
			command.OperationID, command.ServiceInstanceID, ownerID, principal.UserID, command.ProjectID, command.ConversationID, command.SourceMessageID,
			inputSourceID, attachmentSourceID, rootID, command.ExpectedVersion, version, string(command.Action),
			command.IdempotencyKey, command.RequestHash, taskID); err != nil {
			return fmt.Errorf("insert conversation variant operation: %w", err)
		}

		userMessageID := ""
		if command.Action != app.ConversationMessageVariantContinue {
			userMessageID = command.UserMessageID
			content, sourceKind := command.Content, "conversation"
			if command.Action == app.ConversationMessageVariantRegenerateText {
				content, sourceKind = source.SourceUserContent, "conversation_variant_input"
			}
			message := conversation.Message{ID: userMessageID, ConversationID: command.ConversationID, OwnerID: ownerID,
				ActorID: principal.UserID, Role: conversation.RoleUser, Content: content, ModelID: input.ModelID,
				SourceKind: sourceKind, DeliveryStatus: conversation.DeliveryPending, VariantGroupID: rootID,
				VariantVersion: version, VariantAction: string(command.Action), VariantOperationID: command.OperationID}
			if err := insertMessage(ctx, tx, message); err != nil {
				return err
			}
			clientID := "variant-" + command.OperationID
			requestHash, err := app.ConversationReceiptHash(command.ConversationID, content, input)
			if err != nil {
				return err
			}
			metadata, err := json.Marshal(input)
			if err != nil {
				return fmt.Errorf("encode conversation variant route input: %w", err)
			}
			authorization, err := json.Marshal(command.Authorization)
			if err != nil {
				return fmt.Errorf("encode conversation variant authorization: %w", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO conversation_message_routes
(service_instance_id,owner_id,actor_id,conversation_id,message_id,client_message_id,request_hash,request_metadata,authorization_snapshot)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb)`, command.ServiceInstanceID, ownerID, principal.UserID,
				command.ConversationID, userMessageID, clientID, requestHash, metadata, authorization); err != nil {
				return fmt.Errorf("insert conversation variant route: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE conversation_message_variants SET user_message_id=$2,updated_at=clock_timestamp()
WHERE operation_id=$1`, command.OperationID, userMessageID); err != nil {
				return fmt.Errorf("link conversation variant input: %w", err)
			}
			if err := recordAccountObjectChanges(ctx, tx,
				accountObjectChange{OwnerID: ownerID, ObjectType: "conversation", ObjectID: command.ConversationID, Operation: "upsert", MutationID: command.IdempotencyKey},
				accountObjectChange{OwnerID: ownerID, ObjectType: "message", ObjectID: userMessageID, Operation: "upsert", MutationID: command.IdempotencyKey}); err != nil {
				return err
			}
		}
		result = app.ConversationMessageVariantOperation{OperationID: command.OperationID, VariantGroupID: rootID, VariantVersion: version,
			Action: command.Action, State: "accepted", TaskID: taskID, UserMessageID: func() string {
				if command.Action == app.ConversationMessageVariantEditResend {
					return userMessageID
				}
				return ""
			}()}
		return nil
	})
	return result, err
}

type variantOperationRow struct {
	OperationID, ServiceInstanceID, OwnerID, ActorID, ProjectID, ConversationID string
	SourceMessageID, VariantGroupID                                             string
	VariantVersion                                                              int64
	Action, State, TaskID, UserMessageID, AssistantMessageID                    string
}

func (row variantOperationRow) operation(replayed bool) app.ConversationMessageVariantOperation {
	return app.ConversationMessageVariantOperation{OperationID: row.OperationID, VariantGroupID: row.VariantGroupID,
		VariantVersion: row.VariantVersion, Action: app.ConversationMessageVariantAction(row.Action), State: row.State,
		TaskID: row.TaskID, UserMessageID: row.UserMessageID, AssistantMessageID: row.AssistantMessageID, Replayed: replayed}
}

func validConversationVariantAction(action app.ConversationMessageVariantAction) bool {
	return action == app.ConversationMessageVariantEditResend || action == app.ConversationMessageVariantRegenerateText ||
		action == app.ConversationMessageVariantContinue
}

func lockVariantConversationScope(ctx context.Context, tx pgx.Tx, principal identity.Principal, projectID, conversationID string) (string, error) {
	if projectID != "" {
		if !project.ValidID(projectID) {
			return "", domain.NewError(domain.CodeNotFound, "project not found")
		}
		if _, _, err := lockProjectForActorInTx(ctx, tx, principal.UserID, projectID, project.RoleMember, true); err != nil {
			return "", err
		}
		value, err := loadProjectConversationForActorInTx(ctx, tx, principal.UserID, projectID, conversationID, true)
		if err != nil {
			return "", err
		}
		return value.OwnerID, nil
	}
	var ownerID string
	err := tx.QueryRow(ctx, `SELECT owner_id FROM conversations WHERE id=$1 AND owner_id=$2 AND project_id IS NULL AND archived_at IS NULL FOR UPDATE`,
		conversationID, principal.UserID).Scan(&ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.NewError(domain.CodeNotFound, "conversation not found")
	}
	if err != nil {
		return "", fmt.Errorf("lock variant conversation: %w", err)
	}
	return ownerID, nil
}

func loadConversationVariantSource(ctx context.Context, tx pgx.Tx, projectID, ownerID, actorID, conversationID, messageID string) (conversationVariantSource, error) {
	var source conversationVariantSource
	var groupID pgtype.Text
	var version pgtype.Int8
	var outputRevision pgtype.Int8
	var metadata []byte
	err := tx.QueryRow(ctx, `SELECT m.id,m.owner_id,m.actor_id,m.role,m.content,COALESCE(m.task_id,''),m.sequence,m.model_id,
m.variant_group_id,m.variant_version,m.variant_action,
CASE WHEN m.output_status='streaming' AND NOT EXISTS (
 SELECT 1 FROM tasks live WHERE live.id=m.task_id AND live.status='running' AND live.lease_expires_at>clock_timestamp()
  AND live.current_revision=live.desired_revision AND live.current_revision=m.output_task_revision
) THEN 'interrupted' ELSE COALESCE(m.output_status,'') END,m.output_task_revision,
COALESCE(input.id,''),COALESCE(input.content,''),COALESCE(input.attachment_source_message_id,''),
COALESCE(input_route.request_metadata,'{}'::jsonb)
FROM conversation_messages m JOIN conversations c ON c.id=m.conversation_id AND c.owner_id=m.owner_id
LEFT JOIN LATERAL (
 SELECT user_message.id,user_message.content,COALESCE((
  SELECT variant.attachment_source_message_id FROM conversation_message_variants variant
  WHERE variant.user_message_id=user_message.id AND variant.actor_id=m.actor_id
   AND variant.attachment_source_message_id IS NOT NULL ORDER BY variant.variant_version DESC LIMIT 1
 ),user_message.id) AS attachment_source_message_id FROM conversation_messages user_message
 WHERE user_message.conversation_id=m.conversation_id AND user_message.owner_id=m.owner_id AND user_message.actor_id=m.actor_id
  AND user_message.role='user' AND user_message.source_kind<>'conversation_variant_input'
  AND ((m.role='user' AND user_message.id=m.id) OR
       (m.role='assistant' AND ((m.task_id<>'' AND user_message.task_id=m.task_id) OR (m.task_id='' AND user_message.sequence<m.sequence))))
 ORDER BY CASE WHEN m.task_id<>'' THEN user_message.sequence END ASC,
          CASE WHEN m.task_id='' THEN user_message.sequence END DESC LIMIT 1
) input ON true
LEFT JOIN conversation_message_routes input_route ON input_route.message_id=input.id AND input_route.owner_id=m.owner_id
WHERE m.id=$1 AND m.conversation_id=$2 AND m.owner_id=$3 AND c.project_id IS NOT DISTINCT FROM NULLIF($4,'')
 AND m.source_kind<>'conversation_variant_input'`, messageID, conversationID, ownerID, projectID).Scan(
		&source.ID, &source.OwnerID, &source.ActorID, &source.Role, &source.Content, &source.TaskID, &source.Sequence, &source.ModelID,
		&groupID, &version, &source.VariantAction, &source.OutputStatus, &outputRevision,
		&source.SourceUserMessageID, &source.SourceUserContent, &source.SourceAttachmentMessageID, &metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return conversationVariantSource{}, domain.NewError(domain.CodeNotFound, "conversation message not found")
	}
	if err != nil {
		return conversationVariantSource{}, fmt.Errorf("read conversation variant source: %w", err)
	}
	source.VariantGroupID = groupID.String
	if version.Valid {
		source.VariantVersion = version.Int64
	}
	if outputRevision.Valid {
		revision := outputRevision.Int64
		source.OutputRevision = &revision
	}
	source.SourceUserMetadata = metadata
	return source, nil
}

func validateVariantSourceForAction(source conversationVariantSource, action app.ConversationMessageVariantAction) error {
	switch action {
	case app.ConversationMessageVariantEditResend:
		if source.Role != string(conversation.RoleUser) || source.SourceUserMessageID != source.ID {
			return domain.NewError(domain.CodeInvalidArgument, "edit_resend requires a user message")
		}
	case app.ConversationMessageVariantRegenerateText:
		if source.Role != string(conversation.RoleAssistant) || source.SourceUserMessageID == "" {
			return domain.NewError(domain.CodeInvalidArgument, "regenerate_text requires an assistant message with a source request")
		}
	case app.ConversationMessageVariantContinue:
		if source.Role != string(conversation.RoleAssistant) || source.SourceUserMessageID == "" || source.TaskID == "" ||
			source.OutputStatus != string(conversation.OutputInterrupted) || source.OutputRevision == nil {
			return domain.NewError(domain.CodeConflict, "assistant output is not safely resumable")
		}
	default:
		return domain.InvalidArgument("action", "conversation message variant action is invalid")
	}
	return nil
}

func (s *Store) SetConversationMessageVariantState(ctx context.Context, principal identity.Principal, operationID, state string) error {
	if !domain.ValidOperationID(operationID) || (state != "failed" && state != "unknown") {
		return domain.InvalidArgument("variant", "conversation variant state is invalid")
	}
	result, err := s.pool.Exec(ctx, `UPDATE conversation_message_variants SET state=$3,updated_at=clock_timestamp()
WHERE operation_id=$1 AND actor_id=$2 AND state IN ('accepted','running')`, operationID, principal.UserID, state)
	if err != nil {
		return fmt.Errorf("update conversation variant state: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeNotFound, "conversation variant operation not found")
	}
	return nil
}

// Verify the checkpoint, revision fence and operation ledger before accepting a
// continuation. The actual resume still goes through the existing Task action
// path so lease and cancellation fencing remain authoritative.
func verifyConversationVariantResume(ctx context.Context, tx pgx.Tx, source conversationVariantSource) (string, error) {
	var status, sessionID string
	var currentRevision, desiredRevision int64
	var checkpointThrough int64
	var checkpointDigest []byte
	var sessionThrough int64
	var sessionDigest []byte
	var outputRevision int64
	if err := tx.QueryRow(ctx, `SELECT t.status,t.agent_session_id,t.current_revision,t.desired_revision,t.checkpoint_through_seq,t.checkpoint_digest,
 s.checkpoint_through_seq,s.checkpoint_digest,m.output_task_revision
FROM tasks t JOIN conversation_messages m ON m.task_id=t.id AND m.id=$2
JOIN task_agent_sessions s ON s.task_id=t.id AND s.session_id=t.agent_session_id AND s.parent_session_id IS NULL
JOIN task_assistant_message_snapshots snapshot ON snapshot.task_id=t.id AND snapshot.message_id=m.id
 AND snapshot.status='interrupted' AND snapshot.task_revision=m.output_task_revision AND snapshot.content=m.content
WHERE t.id=$1 AND t.owner_id=$3 AND t.initiator_id=$4 FOR SHARE OF t,s,m,snapshot`, source.TaskID, source.ID, source.OwnerID, source.ActorID).
		Scan(&status, &sessionID, &currentRevision, &desiredRevision, &checkpointThrough, &checkpointDigest,
			&sessionThrough, &sessionDigest, &outputRevision); err != nil {
		return "", domain.NewError(domain.CodeConflict, "interrupted output checkpoint is unavailable")
	}
	if status != string(task.StatusPaused) || sessionID == "" || currentRevision != desiredRevision || outputRevision != currentRevision ||
		checkpointThrough < 0 || sessionThrough != checkpointThrough || len(checkpointDigest) != 32 || !reflect.DeepEqual(checkpointDigest, sessionDigest) {
		return "", domain.NewError(domain.CodeConflict, "interrupted output checkpoint is not verified")
	}
	var activeVariant bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversation_message_variants
WHERE task_id=$1 AND state IN ('accepted','running','unknown'))`, source.TaskID).Scan(&activeVariant); err != nil {
		return "", fmt.Errorf("check active continuation operation: %w", err)
	}
	if activeVariant {
		return "", domain.NewError(domain.CodeConflict, "interrupted output already has an unresolved continuation")
	}
	var unresolved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM task_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown')
 UNION ALL SELECT 1 FROM task_hub_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown')
 UNION ALL SELECT 1 FROM task_mcp_connector_operations WHERE task_id=$1 AND status IN ('prepared','dispatched','unknown')
 UNION ALL SELECT 1 FROM conversation_messages WHERE task_id=$1 AND role='user' AND delivery_status IN ('pending','unknown')
)`, source.TaskID).Scan(&unresolved); err != nil {
		return "", fmt.Errorf("check unresolved continuation side effects: %w", err)
	}
	if unresolved {
		return "", domain.NewError(domain.CodeConflict, "interrupted output has unresolved side effects")
	}
	return source.TaskID, nil
}
