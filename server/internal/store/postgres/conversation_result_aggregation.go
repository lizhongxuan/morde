package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/project"
)

const maxConversationResultWindow = 2 * time.Second
const maxConversationResultSources = 64

type conversationResultBatch struct {
	MessageID         string
	ServiceInstanceID string
	OwnerID           string
	ActorID           string
	ConversationID    string
	ProjectID         string
	Deadline          time.Time
}

type conversationResultSource struct {
	conversation.MessageSource
	TurnID string
}

// queueConversationResultInTx must run in the Task completion transaction,
// after the conversation and Task are locked and task.result is inserted.
// It returns false for an already visible stream, which must keep its existing
// single-message path. A true result owns publication, including on replay.
func queueConversationResultInTx(ctx context.Context, tx pgx.Tx, taskID string, revision, eventSequence int64, content string, window time.Duration) (bool, error) {
	if ctx == nil || tx == nil || !domain.ValidTaskID(taskID) || revision < 0 || eventSequence < 1 ||
		window <= 0 || window > maxConversationResultWindow || strings.TrimSpace(content) == "" ||
		len(content) > app.MaxAssistantMessageBytes || !utf8.ValidString(content) || strings.ContainsRune(content, '\x00') {
		return false, domain.InvalidArgument("conversation_result", "task result identity or content is invalid")
	}
	var priorEvent int64
	var priorContent string
	err := tx.QueryRow(ctx, `SELECT event_sequence,content FROM conversation_result_sources WHERE task_id=$1 AND task_revision=$2`,
		taskID, revision).Scan(&priorEvent, &priorContent)
	if err == nil {
		if priorEvent != eventSequence || priorContent != content {
			return false, domain.NewError(domain.CodeIdempotencyConflict, "task revision already has a different result")
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("find prior conversation result: %w", err)
	}
	var batch conversationResultBatch
	var taskRevision, desiredRevision, taskEventSequence int64
	var storedContent, status, location, turnID, eventType string
	var resultAt time.Time
	err = tx.QueryRow(ctx, `SELECT t.owner_id,t.initiator_id,t.conversation_id,COALESCE(t.project_id,''),t.current_revision,
t.desired_revision,t.event_seq,t.result,t.status,t.execution_location,COALESCE(t.current_step,''),e.event_type,clock_timestamp()
FROM tasks t JOIN task_events e ON e.task_id=t.id AND e.sequence=$2 WHERE t.id=$1 FOR UPDATE OF t`, taskID, eventSequence).
		Scan(&batch.OwnerID, &batch.ActorID, &batch.ConversationID, &batch.ProjectID, &taskRevision, &desiredRevision,
			&taskEventSequence, &storedContent, &status, &location, &turnID, &eventType, &resultAt)
	if err != nil || status != "completed" || location != "server" || taskRevision != revision || desiredRevision != revision ||
		taskEventSequence != eventSequence || eventType != "task.result" || storedContent != content || !domain.ValidStepID(turnID) {
		return false, domain.NewError(domain.CodeConflict, "task result revision or event is no longer current")
	}
	var streamContent, streamTurn, streamStatus string
	var streamRevision *int64
	err = tx.QueryRow(ctx, `SELECT content,task_revision,turn_id,status FROM task_assistant_message_snapshots WHERE task_id=$1 FOR UPDATE`, taskID).
		Scan(&streamContent, &streamRevision, &streamTurn, &streamStatus)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("check task result streaming snapshot: %w", err)
	}
	sameRevision := streamRevision != nil && *streamRevision == revision || streamRevision == nil && revision == 0
	visibleStream := (streamStatus == "streaming" || streamStatus == "interrupted") && strings.HasPrefix(content, streamContent)
	alreadyCompleted := streamStatus == "completed" && content == streamContent
	if err == nil && streamContent != "" && streamTurn == turnID && sameRevision && (visibleStream || alreadyCompleted) {
		return false, nil
	}
	userMessageID, err := conversationResultUserMessageInTx(ctx, tx, taskID, revision, batch)
	if err != nil {
		return false, err
	}
	if err := tx.QueryRow(ctx, `SELECT instance_id::text FROM service_identity WHERE singleton=true FOR SHARE`).Scan(&batch.ServiceInstanceID); err != nil {
		return false, fmt.Errorf("lock conversation result service instance: %w", err)
	}
	var existingID string
	var deadline time.Time
	err = tx.QueryRow(ctx, `SELECT message_id,deadline FROM conversation_result_batches
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND conversation_id=$3 AND actor_id=$4 AND state='pending' FOR UPDATE`,
		batch.ServiceInstanceID, batch.OwnerID, batch.ConversationID, batch.ActorID).Scan(&existingID, &deadline)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("find pending conversation result batch: %w", err)
	}
	if err == nil {
		var count, bytes int
		if err := tx.QueryRow(ctx, `SELECT count(*),COALESCE(sum(octet_length(content)),0) FROM conversation_result_sources
WHERE batch_message_id=$1 AND state='pending'`, existingID).Scan(&count, &bytes); err != nil {
			return false, fmt.Errorf("measure pending conversation results: %w", err)
		}
		if !deadline.After(resultAt) || count >= maxConversationResultSources || bytes+len(content)+2*count > app.MaxAssistantMessageBytes {
			if _, err := publishConversationResultBatchInTx(ctx, tx, existingID); err != nil {
				return false, err
			}
			existingID = ""
		}
	}
	if existingID == "" {
		batch.MessageID, err = domain.NewMessageID()
		if err != nil {
			return false, err
		}
		batch.Deadline = resultAt.Add(window)
		if _, err := tx.Exec(ctx, `INSERT INTO conversation_result_batches
(message_id,service_instance_id,owner_id,actor_id,conversation_id,created_at,deadline)
VALUES ($1,$2::uuid,$3,$4,$5,$6,$7)`, batch.MessageID, batch.ServiceInstanceID, batch.OwnerID, batch.ActorID,
			batch.ConversationID, resultAt, batch.Deadline); err != nil {
			return false, fmt.Errorf("create conversation result batch: %w", err)
		}
	} else {
		batch.MessageID, batch.Deadline = existingID, deadline
	}
	if _, err := tx.Exec(ctx, `INSERT INTO conversation_result_sources
(batch_message_id,owner_id,actor_id,conversation_id,user_message_id,task_id,task_revision,event_sequence,turn_id,content,source_order)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,
 (SELECT COALESCE(max(source_order),0)+1 FROM conversation_result_sources WHERE batch_message_id=$1))`,
		batch.MessageID, batch.OwnerID, batch.ActorID, batch.ConversationID, userMessageID, taskID, revision, eventSequence, turnID, content); err != nil {
		return false, fmt.Errorf("persist conversation result source: %w", err)
	}
	var otherActive, deadlineExpired bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE owner_id=$1 AND conversation_id=$2 AND initiator_id=$3 AND id<>$4
 AND status IN ('queued','running')), $5::timestamptz<=clock_timestamp()`,
		batch.OwnerID, batch.ConversationID, batch.ActorID, taskID, batch.Deadline).Scan(&otherActive, &deadlineExpired); err != nil {
		return false, fmt.Errorf("check active conversation tasks: %w", err)
	}
	if !otherActive || deadlineExpired {
		if _, err := publishConversationResultBatchInTx(ctx, tx, batch.MessageID); err != nil {
			return false, err
		}
	}
	return true, nil
}

func conversationResultUserMessageInTx(ctx context.Context, tx pgx.Tx, taskID string, revision int64, batch conversationResultBatch) (string, error) {
	query := `SELECT m.id FROM conversation_messages m WHERE m.task_id=$1 AND m.role='user'
AND m.owner_id=$2 AND m.conversation_id=$3 AND m.actor_id=$4
AND NOT EXISTS (SELECT 1 FROM task_revision_intents i WHERE i.message_id=m.id AND i.task_id=m.task_id)
ORDER BY m.sequence LIMIT 2`
	args := []any{taskID, batch.OwnerID, batch.ConversationID, batch.ActorID}
	if revision > 0 {
		query = `SELECT m.id FROM task_revision_intents i JOIN conversation_messages m ON m.id=i.message_id
WHERE i.task_id=$1 AND i.owner_id=$2 AND i.conversation_id=$3 AND i.initiator_id=$4
 AND i.expected_revision=$5 AND i.state='applied' AND m.owner_id=$2 AND m.conversation_id=$3 AND m.actor_id=$4 AND m.role='user'
ORDER BY i.message_sequence LIMIT 2`
		args = append(args, revision-1)
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return "", fmt.Errorf("find task revision source message: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", fmt.Errorf("read task revision source message: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("iterate task revision source messages: %w", err)
	}
	if len(ids) == 0 || (revision > 0 && len(ids) != 1) {
		return "", domain.NewError(domain.CodeInvalidState, "task revision source message is unavailable")
	}
	return ids[0], nil
}

// The conversation lock serializes publishing with message inserts and Task
// completion. The same transaction revalidates every source and records the
// message, per-Task snapshots, source states and account changes together.
func publishConversationResultBatchInTx(ctx context.Context, tx pgx.Tx, messageID string) (bool, error) {
	var batch conversationResultBatch
	var state string
	err := tx.QueryRow(ctx, `SELECT message_id,owner_id,actor_id,conversation_id,state FROM conversation_result_batches
WHERE message_id=$1 FOR UPDATE`, messageID).Scan(&batch.MessageID, &batch.OwnerID, &batch.ActorID, &batch.ConversationID, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock conversation result batch: %w", err)
	}
	if state != "pending" {
		return false, nil
	}
	rows, err := tx.Query(ctx, `SELECT user_message_id,task_id,task_revision,event_sequence,turn_id,content
FROM conversation_result_sources WHERE batch_message_id=$1 AND state='pending' ORDER BY source_order FOR UPDATE`, messageID)
	if err != nil {
		return false, fmt.Errorf("lock conversation result sources: %w", err)
	}
	var pending []conversationResultSource
	for rows.Next() {
		var source conversationResultSource
		if err := rows.Scan(&source.UserMessageID, &source.TaskID, &source.TaskRevision, &source.EventSequence, &source.TurnID, &source.Content); err != nil {
			rows.Close()
			return false, fmt.Errorf("read conversation result source: %w", err)
		}
		pending = append(pending, source)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, fmt.Errorf("iterate conversation result sources: %w", err)
	}
	if len(pending) == 0 || len(pending) > maxConversationResultSources {
		return false, domain.NewError(domain.CodeInvalidState, "conversation result sources are unavailable")
	}
	valid := make([]conversationResultSource, 0, len(pending))
	for _, source := range pending {
		var currentRevision, desiredRevision int64
		var taskStatus, result, turnID string
		err := tx.QueryRow(ctx, `SELECT current_revision,desired_revision,status,result,COALESCE(current_step,'') FROM tasks
WHERE id=$1 AND owner_id=$2 AND conversation_id=$3 AND initiator_id=$4 FOR UPDATE`,
			source.TaskID, batch.OwnerID, batch.ConversationID, batch.ActorID).
			Scan(&currentRevision, &desiredRevision, &taskStatus, &result, &turnID)
		if err != nil {
			return false, fmt.Errorf("lock conversation result task: %w", err)
		}
		if currentRevision != source.TaskRevision || desiredRevision != source.TaskRevision || taskStatus != "completed" ||
			result != source.Content || turnID != source.TurnID {
			result, err := tx.Exec(ctx, `UPDATE conversation_result_sources SET state='superseded'
WHERE task_id=$1 AND task_revision=$2 AND state='pending'`, source.TaskID, source.TaskRevision)
			if err != nil {
				return false, fmt.Errorf("suppress superseded conversation result: %w", err)
			}
			if result.RowsAffected() != 1 {
				return false, domain.NewError(domain.CodeConflict, "conversation result source changed")
			}
			continue
		}
		valid = append(valid, source)
	}
	if len(valid) == 0 {
		result, err := tx.Exec(ctx, `UPDATE conversation_result_batches SET state='superseded',updated_at=clock_timestamp()
WHERE message_id=$1 AND state='pending'`, messageID)
		if err != nil {
			return false, fmt.Errorf("finish superseded conversation result batch: %w", err)
		}
		if result.RowsAffected() != 1 {
			return false, domain.NewError(domain.CodeConflict, "conversation result batch changed")
		}
		return false, nil
	}
	texts := make([]string, 0, len(valid))
	for _, source := range valid {
		texts = append(texts, source.Content)
	}
	content := strings.Join(texts, "\n\n")
	if len(content) > app.MaxAssistantMessageBytes {
		return false, domain.NewError(domain.CodeInvalidState, "conversation result batch exceeds its bound")
	}
	taskID := ""
	if len(valid) == 1 {
		taskID = valid[0].TaskID
	}
	message := conversation.Message{ID: messageID, OwnerID: batch.OwnerID, ActorID: batch.ActorID,
		ConversationID: batch.ConversationID, Role: conversation.RoleAssistant, Content: content, TaskID: taskID}
	if err := insertMessage(ctx, tx, message); err != nil {
		return false, err
	}
	var messageSequence int64
	var createdAt time.Time
	if err := tx.QueryRow(ctx, `SELECT sequence,created_at FROM conversation_messages WHERE id=$1 AND owner_id=$2 AND conversation_id=$3`,
		messageID, batch.OwnerID, batch.ConversationID).Scan(&messageSequence, &createdAt); err != nil {
		return false, fmt.Errorf("read published conversation result: %w", err)
	}
	for _, source := range valid {
		// A previous stream may already have been displayed even when a later
		// revision or turn completes as a separate conversation message. Keep
		// its only recoverable snapshot instead of overwriting visible text.
		var interruptedMessageID string
		err := tx.QueryRow(ctx, `UPDATE conversation_messages message SET output_status='interrupted'
FROM task_assistant_message_snapshots snapshot
WHERE snapshot.task_id=$1 AND snapshot.status='streaming' AND snapshot.content<>''
 AND message.id=snapshot.message_id AND message.task_id=snapshot.task_id AND message.owner_id=$2
 AND message.actor_id=$3 AND message.conversation_id=$4 AND message.output_status='streaming'
 AND message.output_task_revision=COALESCE(snapshot.task_revision,0) AND message.content=snapshot.content RETURNING message.id`,
			source.TaskID, batch.OwnerID, batch.ActorID, batch.ConversationID).Scan(&interruptedMessageID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("preserve interrupted conversation message: %w", err)
		}
		if interruptedMessageID != "" {
			if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: batch.OwnerID, ObjectType: "message",
				ObjectID: interruptedMessageID, Operation: "upsert"}); err != nil {
				return false, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE task_assistant_message_snapshots SET status='interrupted',updated_at=clock_timestamp()
WHERE task_id=$1 AND status='streaming' AND content<>''`, source.TaskID); err != nil {
			return false, fmt.Errorf("preserve interrupted conversation stream: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_assistant_message_snapshots
(task_id,message_id,turn_id,content,status,message_sequence,created_at,updated_at,task_revision,result_event_sequence)
VALUES ($1,$2,$3,$4,'completed',$5,$6,clock_timestamp(),$7,$8)
ON CONFLICT (task_id) DO UPDATE SET message_id=EXCLUDED.message_id,turn_id=EXCLUDED.turn_id,content=EXCLUDED.content,
 status='completed',message_sequence=EXCLUDED.message_sequence,created_at=EXCLUDED.created_at,updated_at=clock_timestamp(),
 task_revision=EXCLUDED.task_revision,result_event_sequence=EXCLUDED.result_event_sequence
WHERE task_assistant_message_snapshots.status NOT IN ('streaming','interrupted') OR task_assistant_message_snapshots.content=''`,
			source.TaskID, messageID, source.TurnID, source.Content, messageSequence, createdAt, source.TaskRevision, source.EventSequence); err != nil {
			return false, fmt.Errorf("record conversation result task snapshot: %w", err)
		}
		result, err := tx.Exec(ctx, `UPDATE conversation_result_sources SET state='published'
WHERE task_id=$1 AND task_revision=$2 AND event_sequence=$3 AND state='pending'`, source.TaskID, source.TaskRevision, source.EventSequence)
		if err != nil {
			return false, fmt.Errorf("publish conversation result source: %w", err)
		}
		if result.RowsAffected() != 1 {
			return false, domain.NewError(domain.CodeConflict, "conversation result source changed")
		}
	}
	result, err := tx.Exec(ctx, `UPDATE conversation_result_batches SET state='published',published_message_id=message_id,updated_at=clock_timestamp()
WHERE message_id=$1 AND state='pending'`, messageID)
	if err != nil {
		return false, fmt.Errorf("publish conversation result batch: %w", err)
	}
	if result.RowsAffected() != 1 {
		return false, domain.NewError(domain.CodeConflict, "conversation result batch changed")
	}
	if err := recordAccountObjectChanges(ctx, tx,
		accountObjectChange{OwnerID: batch.OwnerID, ObjectType: "conversation", ObjectID: batch.ConversationID, Operation: "upsert"},
		accountObjectChange{OwnerID: batch.OwnerID, ObjectType: "message", ObjectID: messageID, Operation: "upsert"}); err != nil {
		return false, err
	}
	return true, nil
}

// PublishReadyConversationResults independently processes due batches; it
// obtains project and conversation locks before batch and Task locks.
func (s *Store) PublishReadyConversationResults(ctx context.Context, limit int) (int, error) {
	if ctx == nil || limit < 1 || limit > 100 {
		return 0, domain.InvalidArgument("conversation_result", "result batch limit is invalid")
	}
	rows, err := s.pool.Query(ctx, `SELECT b.message_id,b.service_instance_id::text,b.owner_id,b.actor_id,b.conversation_id,COALESCE(c.project_id,'')
FROM conversation_result_batches b JOIN conversations c ON c.id=b.conversation_id AND c.owner_id=b.owner_id
JOIN service_identity i ON i.singleton=true AND i.instance_id=b.service_instance_id
WHERE b.state='pending' AND (b.deadline<=clock_timestamp() OR NOT EXISTS (
 SELECT 1 FROM tasks t WHERE t.owner_id=b.owner_id AND t.conversation_id=b.conversation_id AND t.initiator_id=b.actor_id
  AND t.status IN ('queued','running')))
 AND (c.project_id IS NULL OR EXISTS(SELECT 1 FROM project_members pm JOIN users u ON u.id=pm.user_id AND u.status='approved'
   WHERE pm.project_id=c.project_id AND pm.user_id=b.actor_id))
ORDER BY b.deadline,b.message_id LIMIT $1`, limit)
	if err != nil {
		return 0, fmt.Errorf("list ready conversation result batches: %w", err)
	}
	var batches []conversationResultBatch
	for rows.Next() {
		var batch conversationResultBatch
		if err := rows.Scan(&batch.MessageID, &batch.ServiceInstanceID, &batch.OwnerID, &batch.ActorID, &batch.ConversationID, &batch.ProjectID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("read ready conversation result batch: %w", err)
		}
		batches = append(batches, batch)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("iterate ready conversation result batches: %w", err)
	}
	published := 0
	for _, batch := range batches {
		var didPublish bool
		err := s.WithTx(ctx, func(tx pgx.Tx) error {
			if err := requireConversationServiceInstance(ctx, tx, batch.ServiceInstanceID); err != nil {
				return err
			}
			if batch.ProjectID != "" {
				// Share locks fence membership revocation while remaining compatible
				// with a concurrent Task's authorization read.
				if _, _, err := lockProjectForActorInTx(ctx, tx, batch.ActorID, batch.ProjectID, project.RoleMember, false); err != nil {
					return err
				}
				value, err := loadProjectConversationForActorInTx(ctx, tx, batch.ActorID, batch.ProjectID, batch.ConversationID, true)
				if err != nil {
					return err
				}
				if value.OwnerID != batch.OwnerID {
					return domain.NewError(domain.CodeNotFound, "conversation not found")
				}
			} else {
				var locked string
				if batch.ActorID != batch.OwnerID {
					return domain.NewError(domain.CodeNotFound, "conversation not found")
				}
				err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id=$1 AND owner_id=$2 AND project_id IS NULL FOR UPDATE`,
					batch.ConversationID, batch.OwnerID).Scan(&locked)
				if errors.Is(err, pgx.ErrNoRows) {
					return domain.NewError(domain.CodeNotFound, "conversation not found")
				}
				if err != nil {
					return fmt.Errorf("lock conversation for result batch: %w", err)
				}
			}
			var due bool
			err := tx.QueryRow(ctx, `SELECT b.state='pending' AND (b.deadline<=clock_timestamp() OR NOT EXISTS (
 SELECT 1 FROM tasks t WHERE t.owner_id=b.owner_id AND t.conversation_id=b.conversation_id AND t.initiator_id=b.actor_id
  AND t.status IN ('queued','running'))) FROM conversation_result_batches b
WHERE b.message_id=$1 AND b.service_instance_id=$2::uuid AND b.owner_id=$3 AND b.actor_id=$4 AND b.conversation_id=$5 FOR UPDATE OF b`,
				batch.MessageID, batch.ServiceInstanceID, batch.OwnerID, batch.ActorID, batch.ConversationID).Scan(&due)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && !due {
				return nil
			}
			if err != nil {
				return fmt.Errorf("recheck ready conversation result batch: %w", err)
			}
			didPublish, err = publishConversationResultBatchInTx(ctx, tx, batch.MessageID)
			return err
		})
		if err != nil {
			return published, err
		}
		if didPublish {
			published++
		}
	}
	return published, nil
}

func (s *Store) ListConversationResultSources(ctx context.Context, actorID, conversationID string, messageIDs []string) (map[string][]conversation.MessageSource, error) {
	if ctx == nil || !domain.ValidUserID(actorID) || !domain.ValidConversationID(conversationID) || len(messageIDs) > 101 {
		return nil, domain.InvalidArgument("conversation_result", "result source query is invalid")
	}
	for _, messageID := range messageIDs {
		if !domain.ValidMessageID(messageID) {
			return nil, domain.InvalidArgument("conversation_result", "result message identity is invalid")
		}
	}
	result := make(map[string][]conversation.MessageSource)
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var ownerID, projectID string
		err := tx.QueryRow(ctx, `SELECT owner_id,COALESCE(project_id,'') FROM conversations WHERE id=$1`, conversationID).Scan(&ownerID, &projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "conversation not found")
		}
		if err != nil {
			return fmt.Errorf("read conversation for result sources: %w", err)
		}
		if projectID == "" && ownerID != actorID {
			return domain.NewError(domain.CodeNotFound, "conversation not found")
		}
		if projectID != "" {
			if _, _, err := lockProjectForActorInTx(ctx, tx, actorID, projectID, project.RoleViewer, false); err != nil {
				return err
			}
			if _, err := loadProjectConversationForActorInTx(ctx, tx, actorID, projectID, conversationID, false); err != nil {
				return err
			}
		}
		var lockedID string
		if err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id=$1 AND owner_id=$2 AND COALESCE(project_id,'')=$3 FOR SHARE`,
			conversationID, ownerID, projectID).Scan(&lockedID); err != nil {
			return domain.NewError(domain.CodeNotFound, "conversation not found")
		}
		rows, err := tx.Query(ctx, `SELECT s.batch_message_id,s.user_message_id,s.task_id,s.task_revision,s.event_sequence,s.content
FROM conversation_result_sources s JOIN conversation_result_batches b ON b.message_id=s.batch_message_id
JOIN service_identity i ON i.singleton=true AND i.instance_id=b.service_instance_id
WHERE b.owner_id=$1 AND b.conversation_id=$2 AND b.state='published' AND s.state='published' AND b.message_id=ANY($3::text[])
ORDER BY s.batch_message_id,s.source_order`, ownerID, conversationID, messageIDs)
		if err != nil {
			return fmt.Errorf("list conversation result sources: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var messageID string
			var source conversation.MessageSource
			if err := rows.Scan(&messageID, &source.UserMessageID, &source.TaskID, &source.TaskRevision, &source.EventSequence, &source.Content); err != nil {
				return fmt.Errorf("read published conversation result source: %w", err)
			}
			result[messageID] = append(result[messageID], source)
		}
		return rows.Err()
	})
	return result, err
}

var _ app.ConversationResultPublisher = (*Store)(nil)
