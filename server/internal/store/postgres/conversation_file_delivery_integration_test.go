package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestConversationFileDeliveryFollowsAppliedRevisions(t *testing.T) {
	type revisionDecision struct {
		content string
		intent  task.FileDeliveryIntent
	}
	for _, test := range []struct {
		name          string
		initial       string
		initialIntent task.FileDeliveryIntent
		revisions     []revisionDecision
		want          task.FileDeliveryIntent
	}{
		{name: "ordinary question", initial: "分析文件并写一段总结", initialIntent: task.FileDeliveryNone, want: task.FileDeliveryNone},
		{name: "preserve requirement", initial: "生成 PDF 报告文件", initialIntent: task.FileDeliveryRequired,
			revisions: []revisionDecision{{content: "标题改成中文"}}, want: task.FileDeliveryRequired},
		{name: "explicit cancellation", initial: "生成 PDF 报告文件", initialIntent: task.FileDeliveryRequired,
			revisions: []revisionDecision{{content: "不要生成文件，只要文字", intent: task.FileDeliveryNone}}, want: task.FileDeliveryNone},
		{name: "conflicting revision", initial: "生成 PDF 报告文件", initialIntent: task.FileDeliveryRequired,
			revisions: []revisionDecision{{content: "不要生成文件，但请创建 PDF 文件", intent: task.FileDeliveryUnclear}}, want: task.FileDeliveryUnclear},
		{name: "later file request", initial: "只要文字回答", initialIntent: task.FileDeliveryNone,
			revisions: []revisionDecision{{content: "请生成 PDF 文件", intent: task.FileDeliveryRequired}}, want: task.FileDeliveryRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &routeModelClientTest{reply: fmt.Sprintf(`{"action":"new","task_id":"","file_delivery_intent":"%s"}`, test.initialIntent)}
			store, gateway, _, initial, _ := routeModelFixtureWithMemberAndContent(t, false, false, client, test.initial)
			ctx := context.Background()
			if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, initial.Message.OwnerID); err != nil {
				t.Fatal(err)
			}
			snapshot, decision := classifyFileDeliveryRoute(t, store, gateway, initial)
			if decision.InvalidOutput || decision.Action != app.ConversationRouteNew || decision.FileDeliveryIntent != test.initialIntent {
				t.Fatalf("initial model route = %+v", decision)
			}
			if err := store.ApplyConversationRoute(ctx, initial, snapshot, decision); err != nil {
				t.Fatal(err)
			}
			var taskID string
			if err := store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, initial.Message.ID).Scan(&taskID); err != nil {
				t.Fatal(err)
			}
			for index, revision := range test.revisions {
				message := receiveCausalMessage(t, store, initial, fmt.Sprintf("file-intent-revision-%d", index), initial.Message.ClientMessageID,
					revision.content, taskID)
				claim := claimCausalMessage(t, store, message.ID)
				client.secondReply = fmt.Sprintf(`{"action":"update","task_id":"%s","file_delivery_intent":"%s"}`, taskID, revision.intent)
				if revision.intent == "" {
					client.secondReply = fmt.Sprintf(`{"action":"update","task_id":"%s"}`, taskID)
				}
				updated, revisionRoute := classifyFileDeliveryRoute(t, store, gateway, claim)
				if revisionRoute.InvalidOutput || revisionRoute.Action != app.ConversationRouteUpdate || revisionRoute.TaskID != taskID ||
					revisionRoute.FileDeliveryIntent != revision.intent {
					t.Fatalf("revision model route = %+v", revisionRoute)
				}
				if err := store.ApplyConversationRoute(ctx, claim, updated, revisionRoute); err != nil {
					t.Fatal(err)
				}
			}
			if len(test.revisions) != 0 {
				if _, err := store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE task_id=$1`, taskID); err != nil {
					t.Fatal(err)
				}
				stop, found, err := store.ClaimNextTaskStop(ctx, "file-intent-stop-worker", time.Minute)
				if err != nil || !found || stop.Task.ID != taskID {
					t.Fatalf("stop: found=%t err=%v", found, err)
				}
				if _, err := store.CompleteTaskStop(ctx, taskID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); err != nil {
					t.Fatal(err)
				}
				if applied, err := store.ApplyNextConversationTaskRevision(ctx); err != nil || !applied {
					t.Fatalf("apply revision: applied=%t err=%v", applied, err)
				}
			}
			var actual string
			if err := store.pool.QueryRow(ctx, `SELECT file_delivery_intent FROM tasks WHERE id=$1`, taskID).Scan(&actual); err != nil || task.FileDeliveryIntent(actual) != test.want {
				t.Fatalf("file intent=%q want=%q err=%v", actual, test.want, err)
			}
		})
	}
}
