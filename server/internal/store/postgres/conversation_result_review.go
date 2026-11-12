package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type conversationReviewSource struct {
	app.ConversationTaskResultSource
	TurnID        string
	UserMessageID string
	PayloadSHA256 []byte
}

// enqueueConversationResultReviewInTx turns a durable Task result batch into a
// hidden route event. The existing route worker and DSH coordinator own reply generation.
func enqueueConversationResultReviewInTx(ctx context.Context, tx pgx.Tx, messageID string) (bool, error) {
	var batch conversationResultBatch
	var state, triggerMessageID string
	err := tx.QueryRow(ctx, `SELECT b.message_id,b.service_instance_id::text,b.owner_id,b.actor_id,b.conversation_id,
COALESCE(c.project_id,''),b.authorization_version,b.state,COALESCE(b.trigger_message_id,'')
FROM conversation_result_batches b JOIN conversations c ON c.id=b.conversation_id AND c.owner_id=b.owner_id
WHERE b.message_id=$1 FOR UPDATE OF b`, messageID).Scan(&batch.MessageID, &batch.ServiceInstanceID,
		&batch.OwnerID, &batch.ActorID, &batch.ConversationID, &batch.ProjectID, &batch.AuthorizationVersion, &state, &triggerMessageID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && (state != "pending" || triggerMessageID != "") {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock conversation result review batch: %w", err)
	}
	rows, err := tx.Query(ctx, `SELECT user_message_id,task_id,task_revision,event_sequence,turn_id,content,payload_sha256,authorization_version
FROM conversation_result_sources WHERE batch_message_id=$1 AND state='pending' ORDER BY source_order FOR UPDATE`, batch.MessageID)
	if err != nil {
		return false, fmt.Errorf("lock conversation result review sources: %w", err)
	}
	sources := make([]conversationReviewSource, 0)
	for rows.Next() {
		var source conversationReviewSource
		if err := rows.Scan(&source.UserMessageID, &source.TaskID, &source.TaskRevision, &source.EventSequence,
			&source.TurnID, &source.Content, &source.PayloadSHA256, &source.AuthorizationVersion); err != nil {
			rows.Close()
			return false, fmt.Errorf("read conversation result review source: %w", err)
		}
		sources = append(sources, source)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, fmt.Errorf("iterate conversation result review sources: %w", err)
	}
	if len(sources) == 0 || len(sources) > maxConversationResultSources {
		return false, domain.NewError(domain.CodeInvalidState, "conversation result review sources are unavailable")
	}
	requestSources := make([]app.ConversationTaskResultSource, 0, len(sources))
	var inputBytes int
	for _, source := range sources {
		var currentRevision, desiredRevision, authVersion int64
		var status, result, turnID, objective string
		var dependenciesSatisfied bool
		err := tx.QueryRow(ctx, `SELECT current_revision,desired_revision,auth_version,status,result,
	COALESCE(current_step,''),objective,NOT EXISTS (SELECT 1 FROM task_dependencies dependency JOIN tasks prerequisite
  ON prerequisite.id=dependency.dependency_task_id AND prerequisite.owner_id=dependency.owner_id
    AND prerequisite.conversation_id=dependency.conversation_id
  WHERE dependency.task_id=tasks.id AND (prerequisite.initiator_id<>tasks.initiator_id OR prerequisite.project_id IS DISTINCT FROM tasks.project_id
    OR prerequisite.status<>'completed'
    OR prerequisite.current_revision<>dependency.required_revision OR prerequisite.desired_revision<>dependency.required_revision))
FROM tasks WHERE id=$1 AND owner_id=$2 AND initiator_id=$3 AND conversation_id=$4 FOR UPDATE`,
			source.TaskID, batch.OwnerID, batch.ActorID, batch.ConversationID).Scan(
			&currentRevision, &desiredRevision, &authVersion, &status, &result, &turnID, &objective, &dependenciesSatisfied)
		if err != nil {
			return false, fmt.Errorf("lock conversation result review task: %w", err)
		}
		digest := sha256.Sum256([]byte(source.Content))
		if currentRevision != source.TaskRevision || desiredRevision != source.TaskRevision || status != string(task.StatusCompleted) ||
			result != source.Content || turnID != source.TurnID || authVersion != source.AuthorizationVersion ||
			batch.AuthorizationVersion != source.AuthorizationVersion || !dependenciesSatisfied || len(source.PayloadSHA256) != sha256.Size || !bytes.Equal(source.PayloadSHA256, digest[:]) {
			if _, err := tx.Exec(ctx, `UPDATE conversation_result_sources SET state='superseded'
WHERE task_id=$1 AND task_revision=$2 AND state='pending'`, source.TaskID, source.TaskRevision); err != nil {
				return false, fmt.Errorf("supersede stale conversation result review source: %w", err)
			}
			continue
		}
		inputBytes += len(objective) + len(source.Content)
		if inputBytes > app.MaxAssistantMessageBytes {
			if _, err := tx.Exec(ctx, `UPDATE conversation_result_batches SET state='failed',updated_at=clock_timestamp()
WHERE message_id=$1 AND state='pending'`, batch.MessageID); err != nil {
				return false, fmt.Errorf("fail oversized conversation result review: %w", err)
			}
			return false, nil
		}
		source.Objective = objective
		requestSources = append(requestSources, source.ConversationTaskResultSource)
	}
	if len(requestSources) == 0 {
		if _, err := tx.Exec(ctx, `UPDATE conversation_result_batches SET state='superseded',updated_at=clock_timestamp()
WHERE message_id=$1 AND state='pending'`, batch.MessageID); err != nil {
			return false, fmt.Errorf("finish superseded conversation result review: %w", err)
		}
		return false, nil
	}
	triggerID, err := domain.NewMessageID()
	if err != nil {
		return false, err
	}
	clientMessageID := "task-result-" + strings.TrimPrefix(batch.MessageID, "m")
	input := app.ConversationRoutingInput{ProjectID: batch.ProjectID, ResultReviewID: batch.MessageID}
	authorization := task.Authorization{Kind: string(identity.KindSession), Scopes: []string{string(identity.ScopeTaskSubmit)},
		Version: batch.AuthorizationVersion}
	metadata, err := json.Marshal(input)
	if err != nil {
		return false, fmt.Errorf("encode task result review route: %w", err)
	}
	authorizationJSON, err := json.Marshal(authorization)
	if err != nil {
		return false, fmt.Errorf("encode task result review authorization: %w", err)
	}
	requestHash, err := json.Marshal(struct {
		MessageID string                             `json:"message_id"`
		Sources   []app.ConversationTaskResultSource `json:"sources"`
	}{batch.MessageID, requestSources})
	if err != nil {
		return false, fmt.Errorf("encode task result review identity: %w", err)
	}
	requestDigest := sha256.Sum256(requestHash)
	trigger := conversation.Message{ID: triggerID, OwnerID: batch.OwnerID, ActorID: batch.ActorID,
		ConversationID: batch.ConversationID, Role: conversation.RoleUser,
		Content: "Completed Task results are ready for review.", SourceKind: "task_result_review", DeliveryStatus: conversation.DeliveryPending}
	if err := insertMessage(ctx, tx, trigger); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO conversation_message_routes
(service_instance_id,owner_id,actor_id,conversation_id,message_id,client_message_id,request_hash,request_metadata,authorization_snapshot)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8::jsonb,$9::jsonb)`, batch.ServiceInstanceID, batch.OwnerID, batch.ActorID,
		batch.ConversationID, triggerID, clientMessageID, requestDigest[:], string(metadata), string(authorizationJSON)); err != nil {
		return false, fmt.Errorf("persist task result review route: %w", err)
	}
	result, err := tx.Exec(ctx, `UPDATE conversation_result_batches SET state='processing',trigger_message_id=$2,updated_at=clock_timestamp()
WHERE message_id=$1 AND state='pending' AND trigger_message_id IS NULL`, batch.MessageID, triggerID)
	if err != nil {
		return false, fmt.Errorf("queue conversation result review: %w", err)
	}
	if result.RowsAffected() != 1 {
		return false, domain.NewError(domain.CodeConflict, "conversation result review changed while queuing")
	}
	return true, nil
}

func loadConversationTaskResultReviewInTx(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) (*app.ConversationTaskResultReview, error) {
	rows, err := tx.Query(ctx, `SELECT batch.retry_attempt,source.user_message_id,source.task_id,source.task_revision,source.event_sequence,
source.authorization_version,source.turn_id,source.content,source.payload_sha256,task.objective,task.current_revision,
task.desired_revision,task.status,task.result,COALESCE(task.current_step,''),task.auth_version,
NOT EXISTS (SELECT 1 FROM task_dependencies dependency JOIN tasks prerequisite
  ON prerequisite.id=dependency.dependency_task_id AND prerequisite.owner_id=dependency.owner_id
    AND prerequisite.conversation_id=dependency.conversation_id
  WHERE dependency.task_id=task.id AND (prerequisite.initiator_id<>task.initiator_id OR prerequisite.project_id IS DISTINCT FROM task.project_id
    OR prerequisite.status<>'completed'
    OR prerequisite.current_revision<>dependency.required_revision OR prerequisite.desired_revision<>dependency.required_revision))
FROM conversation_result_batches batch JOIN conversation_result_sources source ON source.batch_message_id=batch.message_id
JOIN tasks task ON task.id=source.task_id AND task.owner_id=source.owner_id AND task.conversation_id=source.conversation_id
WHERE batch.message_id=$1 AND batch.trigger_message_id=$2 AND batch.state='processing' AND source.state='pending'
ORDER BY source.source_order FOR SHARE OF batch,source,task`, claim.Input.ResultReviewID, claim.Message.ID)
	if err != nil {
		return nil, fmt.Errorf("load task result review sources: %w", err)
	}
	value := &app.ConversationTaskResultReview{Sources: make([]app.ConversationTaskResultSource, 0)}
	stale := false
	for rows.Next() {
		var source app.ConversationTaskResultSource
		var userMessageID, turnID, status, result, currentStep string
		var payloadDigest []byte
		var currentRevision, desiredRevision, authVersion int64
		var dependenciesSatisfied bool
		if err := rows.Scan(&value.RetryAttempt, &userMessageID, &source.TaskID, &source.TaskRevision, &source.EventSequence,
			&source.AuthorizationVersion, &turnID, &source.Content, &payloadDigest, &source.Objective,
			&currentRevision, &desiredRevision, &status, &result, &currentStep, &authVersion, &dependenciesSatisfied); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read task result review source: %w", err)
		}
		digest := sha256.Sum256([]byte(source.Content))
		if !domain.ValidMessageID(userMessageID) || !domain.ValidStepID(turnID) || currentRevision != source.TaskRevision ||
			desiredRevision != source.TaskRevision || status != string(task.StatusCompleted) || result != source.Content ||
			currentStep != turnID || authVersion != source.AuthorizationVersion || source.AuthorizationVersion != claim.Authorization.Version ||
			!dependenciesSatisfied || !bytes.Equal(payloadDigest, digest[:]) {
			stale = true
			continue
		}
		value.Sources = append(value.Sources, source)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("iterate task result review sources: %w", err)
	}
	if stale || len(value.Sources) == 0 {
		if err := supersedeConversationTaskResultReviewInTx(ctx, tx, claim); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if len(value.Sources) == 0 || len(value.Sources) > maxConversationResultSources {
		return nil, domain.NewError(domain.CodeConflict, "task result review source is unavailable")
	}
	return value, nil
}

func supersedeConversationTaskResultReviewInTx(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) error {
	taskIDs, err := conversationResultReviewTaskIDsInTx(ctx, tx, claim.Input.ResultReviewID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE conversation_result_sources SET state='superseded'
WHERE batch_message_id=$1 AND state='pending'`, claim.Input.ResultReviewID); err != nil {
		return fmt.Errorf("supersede stale task result review sources: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE conversation_result_batches SET state='superseded',updated_at=clock_timestamp()
WHERE message_id=$1 AND state='processing'`, claim.Input.ResultReviewID); err != nil {
		return fmt.Errorf("supersede stale task result review batch: %w", err)
	}
	changes := make([]accountObjectChange, 0, len(taskIDs)+1)
	for _, taskID := range taskIDs {
		changes = append(changes, accountObjectChange{OwnerID: claim.Message.OwnerID, ObjectType: "task", ObjectID: taskID, Operation: "upsert"})
	}
	changes = append(changes, accountObjectChange{OwnerID: claim.Message.OwnerID, ObjectType: "conversation",
		ObjectID: claim.Message.ConversationID, Operation: "upsert"})
	return recordAccountObjectChanges(ctx, tx, changes...)
}

func conversationResultReviewTaskIDsInTx(ctx context.Context, tx pgx.Tx, batchMessageID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT task_id FROM conversation_result_sources WHERE batch_message_id=$1 ORDER BY task_id`, batchMessageID)
	if err != nil {
		return nil, fmt.Errorf("list task result review sources: %w", err)
	}
	defer rows.Close()
	var taskIDs []string
	for rows.Next() {
		var taskID string
		if err := rows.Scan(&taskID); err != nil {
			return nil, fmt.Errorf("read task result review source: %w", err)
		}
		taskIDs = append(taskIDs, taskID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate task result review sources: %w", err)
	}
	return taskIDs, nil
}

func lockTaskResultReviewRouteClaim(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim, sourceKind string) error {
	if sourceKind != "task_result_review" || !domain.ValidMessageID(claim.Input.ResultReviewID) ||
		claim.Authorization.Kind != string(identity.KindSession) || claim.Authorization.Version < 1 ||
		len(claim.Authorization.Scopes) != 1 || claim.Authorization.Scopes[0] != string(identity.ScopeTaskSubmit) {
		return domain.NewError(domain.CodeConflict, "task result review route identity is invalid")
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT batch.service_instance_id=$1::uuid AND batch.owner_id=$2 AND batch.actor_id=$3
AND batch.conversation_id=$4 AND batch.authorization_version=$5 AND batch.state='processing'
AND batch.trigger_message_id=$6 AND message.source_kind='task_result_review'
AND message.role='user' AND message.content='Completed Task results are ready for review.'
FROM conversation_result_batches batch JOIN conversation_messages message ON message.id=batch.trigger_message_id
WHERE batch.message_id=$7 FOR SHARE OF batch,message`, claim.ServiceInstanceID, claim.Message.OwnerID, claim.Message.ActorID,
		claim.Message.ConversationID, claim.Authorization.Version, claim.Message.ID, claim.Input.ResultReviewID).Scan(&valid)
	if err != nil || !valid {
		return domain.NewError(domain.CodeConflict, "task result review route is no longer current")
	}
	_, err = loadConversationTaskResultReviewInTx(ctx, tx, claim)
	return err
}

func publishConversationResultReviewInTx(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim, reply string) error {
	if claim.Input.ResultReviewID == "" || !app.ValidConversationRouteReply(reply) {
		return domain.InvalidArgument("result_review", "task result review reply is invalid")
	}
	review, err := loadConversationTaskResultReviewInTx(ctx, tx, claim)
	if err != nil {
		return err
	}
	if review == nil {
		return domain.NewError(domain.CodeConflict, "task result review sources are no longer current")
	}
	var replyMessageID, state string
	if err := tx.QueryRow(ctx, `SELECT message_id,state FROM conversation_result_batches
WHERE message_id=$1 AND trigger_message_id=$2 FOR UPDATE`, claim.Input.ResultReviewID, claim.Message.ID).
		Scan(&replyMessageID, &state); err != nil || state != "processing" {
		return domain.NewError(domain.CodeConflict, "task result review is no longer pending")
	}
	validSources := make(map[string]struct{}, len(review.Sources))
	for _, source := range review.Sources {
		validSources[source.TaskID] = struct{}{}
	}
	rows, err := tx.Query(ctx, `SELECT task_id,task_revision FROM conversation_result_sources
WHERE batch_message_id=$1 AND state='pending' ORDER BY source_order FOR UPDATE`, replyMessageID)
	if err != nil {
		return fmt.Errorf("lock sources before publishing task result review: %w", err)
	}
	var sourceIDs []string
	for rows.Next() {
		var taskID string
		var revision int64
		if err := rows.Scan(&taskID, &revision); err != nil {
			rows.Close()
			return fmt.Errorf("read source before publishing task result review: %w", err)
		}
		if _, ok := validSources[taskID]; ok {
			sourceIDs = append(sourceIDs, taskID)
		} else if _, err := tx.Exec(ctx, `UPDATE conversation_result_sources SET state='superseded'
WHERE batch_message_id=$1 AND task_id=$2 AND task_revision=$3 AND state='pending'`, replyMessageID, taskID, revision); err != nil {
			rows.Close()
			return fmt.Errorf("supersede stale task result review source: %w", err)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("iterate task result review sources before publish: %w", err)
	}
	if len(sourceIDs) == 0 {
		if _, err := tx.Exec(ctx, `UPDATE conversation_result_batches SET state='superseded',updated_at=clock_timestamp()
WHERE message_id=$1 AND state='processing'`, replyMessageID); err != nil {
			return fmt.Errorf("finish superseded task result review: %w", err)
		}
		return finishConversationRoute(ctx, tx, claim, "routed", "", "", true)
	}
	for _, source := range review.Sources {
		var interruptedMessageID string
		err := tx.QueryRow(ctx, `UPDATE conversation_messages message SET output_status='interrupted'
FROM task_assistant_message_snapshots snapshot
WHERE snapshot.task_id=$1 AND snapshot.status='streaming' AND snapshot.content<>''
 AND message.id=snapshot.message_id AND message.task_id=snapshot.task_id AND message.owner_id=$2
 AND message.actor_id=$3 AND message.conversation_id=$4 AND message.output_status='streaming'
 AND message.output_task_revision=COALESCE(snapshot.task_revision,0) AND message.content=snapshot.content
RETURNING message.id`, source.TaskID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID).Scan(&interruptedMessageID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("preserve interrupted conversation message: %w", err)
		}
		if interruptedMessageID != "" {
			if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: claim.Message.OwnerID,
				ObjectType: "message", ObjectID: interruptedMessageID, Operation: "upsert"}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE task_assistant_message_snapshots SET status='interrupted',updated_at=clock_timestamp()
WHERE task_id=$1 AND status='streaming' AND content<>''`, source.TaskID); err != nil {
			return fmt.Errorf("preserve interrupted conversation stream: %w", err)
		}
	}
	message := conversation.Message{ID: replyMessageID, OwnerID: claim.Message.OwnerID, ActorID: claim.Message.ActorID,
		ConversationID: claim.Message.ConversationID, Role: conversation.RoleAssistant, Content: strings.TrimSpace(reply),
		SourceKind: "conversation", DeliveryStatus: conversation.DeliveryApplied}
	if err := insertMessage(ctx, tx, message); err != nil {
		return err
	}
	for _, taskID := range sourceIDs {
		result, err := tx.Exec(ctx, `UPDATE conversation_result_sources SET state='published'
WHERE batch_message_id=$1 AND task_id=$2 AND state='pending'`, replyMessageID, taskID)
		if err != nil || result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "task result review source changed before publish")
		}
	}
	result, err := tx.Exec(ctx, `UPDATE conversation_result_batches SET state='published',published_message_id=message_id,updated_at=clock_timestamp()
WHERE message_id=$1 AND trigger_message_id=$2 AND state='processing'`, replyMessageID, claim.Message.ID)
	if err != nil || result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "task result review changed before publish")
	}
	if err := recordAccountObjectChanges(ctx, tx,
		accountObjectChange{OwnerID: claim.Message.OwnerID, ObjectType: "message", ObjectID: replyMessageID, Operation: "upsert"},
		accountObjectChange{OwnerID: claim.Message.OwnerID, ObjectType: "conversation", ObjectID: claim.Message.ConversationID, Operation: "upsert"}); err != nil {
		return err
	}
	return finishConversationRoute(ctx, tx, claim, "routed", "", "", true)
}
