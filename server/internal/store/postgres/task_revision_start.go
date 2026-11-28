package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

// The caller has already locked and authorized the current Task and Agent,
// and commits the root checkpoint in the same transaction as this event.
func recordTaskRevisionStartedInTx(ctx context.Context, tx pgx.Tx, taskID string) error {
	var current, desired int64
	if err := tx.QueryRow(ctx, `SELECT current_revision,desired_revision FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&current, &desired); err != nil {
		return fmt.Errorf("read task revision before start: %w", err)
	}
	if current < 0 || current != desired {
		return domain.NewError(domain.CodeInvalidState, "task revision changed before start")
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM task_revision_starts WHERE task_id=$1 AND revision=$2)`, taskID, current).Scan(&exists); err != nil {
		return fmt.Errorf("check task revision start: %w", err)
	}
	if exists {
		return acknowledgeStartedTaskMessagesInTx(ctx, tx, taskID, current)
	}
	acknowledgementMessageID, err := taskAcknowledgementMessageInTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	var previous int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0) FROM task_steps WHERE task_id=$1`, taskID).Scan(&previous); err != nil {
		return fmt.Errorf("read task step sequence: %w", err)
	}
	if previous == math.MaxInt64 {
		return domain.NewError(domain.CodeInvalidState, "task step sequence is exhausted")
	}
	stepID, err := domain.NewStepID()
	if err != nil {
		return fmt.Errorf("create task start step identity: %w", err)
	}
	const title, summary = "开始处理", "正在处理你的请求。"
	if _, err := tx.Exec(ctx, `INSERT INTO task_steps (id,task_id,sequence,kind,status,title,summary)
VALUES ($1,$2,$3,'progress','succeeded',$4,$5)`, stepID, taskID, previous+1, title, summary); err != nil {
		return fmt.Errorf("insert task revision start step: %w", err)
	}
	eventSequence, err := incrementTaskEventSequence(ctx, tx, taskID)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{"phase": "started", "step_id": stepID, "title": title, "summary": summary, "revision": current})
	if err != nil {
		return fmt.Errorf("encode task revision start event: %w", err)
	}
	if err := insertTaskEvent(ctx, tx, task.Event{TaskID: taskID, Sequence: eventSequence, Type: "task.step", Payload: payload}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_revision_starts
(task_id,revision,step_id,event_sequence,acknowledgement_message_id) VALUES ($1,$2,$3,$4,NULLIF($5,''))`,
		taskID, current, stepID, eventSequence, acknowledgementMessageID); err != nil {
		return fmt.Errorf("record task revision start: %w", err)
	}
	return acknowledgeStartedTaskMessagesInTx(ctx, tx, taskID, current)
}

func taskAcknowledgementMessageInTx(ctx context.Context, tx pgx.Tx, taskID string) (string, error) {
	var messageID string
	err := tx.QueryRow(ctx, `SELECT COALESCE(
 (SELECT message.id FROM conversation_messages message
  WHERE message.task_id=$1 AND message.role='user'
  ORDER BY message.sequence,message.id LIMIT 1),
 (SELECT message.id FROM task_revision_intents intent
  JOIN conversation_messages message ON message.id=intent.message_id AND message.role='user'
  WHERE intent.task_id=$1
  ORDER BY intent.message_sequence,intent.message_id LIMIT 1),
 '')`, taskID).Scan(&messageID)
	if err != nil {
		return "", fmt.Errorf("select Task revision acknowledgement message: %w", err)
	}
	if messageID != "" && !domain.ValidMessageID(messageID) {
		return "", domain.NewError(domain.CodeInvalidState, "Task revision acknowledgement identity is invalid")
	}
	return messageID, nil
}

func acknowledgeStartedTaskMessagesInTx(ctx context.Context, tx pgx.Tx, taskID string, revision int64) error {
	rows, err := tx.Query(ctx, `UPDATE conversation_message_routes r
SET acknowledgement_requested=false,agent_acknowledged=true,updated_at=clock_timestamp()
FROM conversation_messages m
WHERE r.message_id=m.id AND m.role='user' AND r.acknowledgement_requested AND NOT r.agent_acknowledged
 AND EXISTS (SELECT 1 FROM task_revision_starts anchor WHERE anchor.task_id=$1 AND anchor.revision=$2
  AND anchor.acknowledgement_message_id=m.id)
 AND EXISTS (SELECT 1 FROM tasks t WHERE t.id=$1 AND t.status='running' AND t.lease_expires_at>clock_timestamp()
  AND t.current_revision=$2 AND t.desired_revision=$2
  AND EXISTS (SELECT 1 FROM task_revision_starts started WHERE started.task_id=t.id AND started.revision=t.current_revision))
RETURNING m.id,m.owner_id,r.client_message_id`, taskID, revision)
	if err != nil {
		return fmt.Errorf("confirm accepted messages when Task starts: %w", err)
	}
	type acknowledgedMessage struct{ id, ownerID, clientMessageID string }
	var acknowledged []acknowledgedMessage
	for rows.Next() {
		var message acknowledgedMessage
		if err := rows.Scan(&message.id, &message.ownerID, &message.clientMessageID); err != nil {
			rows.Close()
			return fmt.Errorf("read Task-start acknowledgements: %w", err)
		}
		acknowledged = append(acknowledged, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate Task-start acknowledgements: %w", err)
	}
	rows.Close()
	for _, message := range acknowledged {
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: message.ownerID,
			ObjectType: "message", ObjectID: message.id, Operation: "upsert", MutationID: message.clientMessageID}); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE conversation_message_routes r
SET acknowledgement_requested=false,updated_at=clock_timestamp()
FROM conversation_messages m
WHERE r.message_id=m.id AND m.role='user' AND r.acknowledgement_requested AND NOT r.agent_acknowledged
 AND EXISTS (SELECT 1 FROM tasks t WHERE t.id=$1 AND t.current_revision=$2 AND t.desired_revision=$2)
 AND (
  m.task_id=$1
  OR EXISTS (SELECT 1 FROM task_revision_intents intent WHERE intent.message_id=m.id
    AND intent.task_id=$1 AND intent.expected_revision+1=$2)
  OR EXISTS (SELECT 1 FROM conversation_message_routes duplicate_route
    JOIN conversation_messages original ON original.id=duplicate_route.duplicate_of_message_id
      AND original.owner_id=m.owner_id AND original.conversation_id=m.conversation_id
    WHERE duplicate_route.message_id=m.id AND original.task_id=$1)
 )
 AND NOT EXISTS (SELECT 1 FROM task_revision_starts anchor WHERE anchor.task_id=$1 AND anchor.revision=$2
  AND anchor.acknowledgement_message_id=m.id)`, taskID, revision); err != nil {
		return fmt.Errorf("clear non-anchor Task acknowledgement requests: %w", err)
	}
	return nil
}
