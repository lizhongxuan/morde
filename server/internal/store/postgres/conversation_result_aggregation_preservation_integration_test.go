package postgres

import (
	"context"
	"crypto/sha256"
	"testing"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func TestConversationResultAggregationPreservesInterruptedPreviousRevision(t *testing.T) {
	f := newResultAggregationFixture(t)
	ctx := context.Background()
	value := f.createTask(t, f.ownerID, "original request")
	oldMessageID := mustID(t, domain.NewMessageID)
	oldTurnID := mustID(t, domain.NewStepID)
	if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		if err := insertMessage(ctx, tx, conversation.Message{ID: oldMessageID, OwnerID: f.ownerID, ActorID: f.ownerID,
			ConversationID: f.conversation.ID, Role: conversation.RoleAssistant, TaskID: value.ID, Content: "visible older fragment"}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE conversation_messages SET output_status='streaming',output_task_revision=0 WHERE id=$1`, oldMessageID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO task_assistant_message_snapshots
(task_id,message_id,turn_id,content,status,task_revision) VALUES ($1,$2,$3,'visible older fragment','streaming',0)`,
			value.ID, oldMessageID, oldTurnID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var oldSequence int64
	if err := f.store.pool.QueryRow(ctx, `SELECT sequence FROM conversation_messages WHERE id=$1`, oldMessageID).Scan(&oldSequence); err != nil {
		t.Fatal(err)
	}
	var beforeChanges int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM account_change_log WHERE owner_id=$1 AND object_type='message' AND object_id=$2`,
		f.ownerID, oldMessageID).Scan(&beforeChanges); err != nil {
		t.Fatal(err)
	}
	instanceID, err := f.store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	token, err := f.store.GetTokenByID(ctx, testSessionTokenID(f.ownerID))
	if err != nil {
		t.Fatal(err)
	}
	modifiedID := mustID(t, domain.NewMessageID)
	digest := sha256.Sum256([]byte("new revision"))
	message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: modifiedID, OwnerID: f.ownerID, ActorID: f.ownerID,
			ConversationID: f.conversation.ID, Role: conversation.RoleUser, Content: "new revision"},
		ServiceInstanceID: instanceID, ClientID: "preserve-old-stream-revision", RequestHash: digest[:],
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_revision_intents
(message_id,task_id,owner_id,initiator_id,conversation_id,message_sequence,expected_revision,state)
VALUES ($1,$2,$3,$3,$4,$5,0,'applied')`, modifiedID, value.ID, f.ownerID, f.conversation.ID, message.Sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET current_revision=1,desired_revision=1 WHERE id=$1`, value.ID); err != nil {
		t.Fatal(err)
	}
	if !f.complete(t, &value, "new revision result") {
		t.Fatal("new revision result was not published")
	}
	var newMessageID, content, snapshotID, snapshotText, snapshotStatus string
	var revision, snapshotRevision int64
	err = f.store.pool.QueryRow(ctx, `SELECT m.id,m.content,s.task_revision,snapshot.message_id,snapshot.content,snapshot.status,snapshot.task_revision
FROM conversation_result_sources s JOIN conversation_messages m ON m.id=s.batch_message_id
JOIN task_assistant_message_snapshots snapshot ON snapshot.task_id=s.task_id
WHERE s.task_id=$1 AND s.state='published'`, value.ID).
		Scan(&newMessageID, &content, &revision, &snapshotID, &snapshotText, &snapshotStatus, &snapshotRevision)
	if err != nil || newMessageID == oldMessageID || content != "new revision result" || revision != 1 ||
		snapshotID != oldMessageID || snapshotText != "visible older fragment" || snapshotStatus != "interrupted" || snapshotRevision != 0 {
		t.Fatalf("new result or previous snapshot was lost: new=%s revision=%d snapshot=%s snapshot_revision=%d status=%s err=%v",
			newMessageID, revision, snapshotID, snapshotRevision, snapshotStatus, err)
	}
	var oldContent, oldStatus string
	var actualSequence int64
	if err := f.store.pool.QueryRow(ctx, `SELECT content,output_status,sequence FROM conversation_messages WHERE id=$1`, oldMessageID).
		Scan(&oldContent, &oldStatus, &actualSequence); err != nil || oldContent != "visible older fragment" || oldStatus != "interrupted" || actualSequence != oldSequence {
		t.Fatalf("persisted older message was not preserved: status=%s sequence=%d err=%v", oldStatus, actualSequence, err)
	}
	var afterChanges int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM account_change_log WHERE owner_id=$1 AND object_type='message' AND object_id=$2`,
		f.ownerID, oldMessageID).Scan(&afterChanges); err != nil || afterChanges != beforeChanges+1 {
		t.Fatalf("interrupted message sync change missing: before=%d after=%d err=%v", beforeChanges, afterChanges, err)
	}
	restarted := New(f.store.pool)
	details, err := restarted.GetTask(ctx, value.ID, f.ownerID)
	if err != nil || !details.ResultMessagePublished || details.AssistantMessage == nil || details.AssistantMessage.Message.ID != oldMessageID ||
		details.AssistantMessage.TaskRevision == nil || *details.AssistantMessage.TaskRevision != 0 || details.AssistantMessage.ResultEventSequence != nil {
		t.Fatalf("previous snapshot is not recoverable after restart: %#v err=%v", details.AssistantMessage, err)
	}
	sources, err := restarted.ListConversationResultSources(ctx, f.ownerID, f.conversation.ID, []string{newMessageID})
	if err != nil || len(sources[newMessageID]) != 1 || sources[newMessageID][0].TaskRevision != 1 || sources[newMessageID][0].EventSequence != value.EventSequence {
		t.Fatalf("new result source not independently recoverable: %+v err=%v", sources, err)
	}
}

func TestConversationResultAggregationPreservesDifferentVisibleTurn(t *testing.T) {
	f := newResultAggregationFixture(t)
	value := f.createTask(t, f.ownerID, "one task")
	oldMessageID := mustID(t, domain.NewMessageID)
	oldTurnID := mustID(t, domain.NewStepID)
	if _, err := f.store.pool.Exec(context.Background(), `INSERT INTO task_assistant_message_snapshots
(task_id,message_id,turn_id,content,status,task_revision) VALUES ($1,$2,$3,'visible previous turn','streaming',0)`,
		value.ID, oldMessageID, oldTurnID); err != nil {
		t.Fatal(err)
	}
	if !f.complete(t, &value, "new independent result") {
		t.Fatal("new result was not published")
	}
	var snapshotID, content, status string
	if err := f.store.pool.QueryRow(context.Background(), `SELECT message_id,content,status FROM task_assistant_message_snapshots WHERE task_id=$1`, value.ID).
		Scan(&snapshotID, &content, &status); err != nil || snapshotID != oldMessageID || content != "visible previous turn" || status != "interrupted" {
		t.Fatalf("previous visible turn was overwritten: id=%s status=%s err=%v", snapshotID, status, err)
	}
}
