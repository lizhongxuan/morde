package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

func TestRouteModelHistoryRechecksCompletedStreamResultIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, status, eventType string
		included, tampered      bool
	}{
		{name: "verified completed result", status: "completed", eventType: "task.result", included: true},
		{name: "changed result content", status: "completed", eventType: "task.result", tampered: true},
		{name: "different event type", status: "completed", eventType: "task.status"},
		{name: "interrupted stream", status: "interrupted"},
		{name: "unfinished stream", status: "streaming"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResultAggregationFixture(t)
			ctx := context.Background()
			if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',NULL,'synthetic route model','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`); err != nil {
				t.Fatal(err)
			}
			value := f.createTask(t, f.ownerID, "stream source")
			messageID := mustID(t, domain.NewMessageID)
			if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `UPDATE tasks SET status='completed',result='final answer' WHERE id=$1`, value.ID); err != nil {
					return err
				}
				if tc.eventType != "" {
					content := "visible stream content"
					if tc.tampered {
						content = "different stream content"
					}
					if _, err := tx.Exec(ctx, `INSERT INTO task_events(task_id,sequence,event_type,payload,task_revision,result_content_sha256)
VALUES ($1,1,$2,'{}'::jsonb,0,sha256(convert_to($3,'UTF8')))`, value.ID, tc.eventType, content); err != nil {
						return err
					}
				}
				if err := insertMessage(ctx, tx, conversation.Message{ID: messageID, OwnerID: f.ownerID, ActorID: f.ownerID,
					ConversationID: f.conversation.ID, Role: conversation.RoleAssistant, TaskID: value.ID, Content: "visible stream content"}); err != nil {
					return err
				}
				var eventSequence any
				if tc.eventType != "" {
					eventSequence = int64(1)
				}
				_, err := tx.Exec(ctx, `UPDATE conversation_messages SET output_status=$2,output_task_revision=0,output_result_event_sequence=$3 WHERE id=$1`,
					messageID, tc.status, eventSequence)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			history := f.routeHistoryForActor(t, f.ownerID)
			found := false
			for _, item := range history {
				found = found || item.MessageID == messageID
			}
			if found != tc.included {
				t.Fatalf("stream history inclusion=%t want=%t", found, tc.included)
			}
		})
	}
}
