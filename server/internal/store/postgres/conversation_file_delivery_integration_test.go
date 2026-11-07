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
	for _, test := range []struct {
		name, initial string
		revisions     []string
		want          task.FileDeliveryIntent
		workspace     bool
	}{
		{"ordinary question", "分析文件并写一段总结", nil, task.FileDeliveryNone, false},
		{"preserve requirement", "生成 PDF 报告文件", []string{"标题改成中文"}, task.FileDeliveryRequired, false},
		{"explicit cancellation", "生成 PDF 报告文件", []string{"不要生成文件，只要文字"}, task.FileDeliveryNone, false},
		{"conflicting revision", "生成 PDF 报告文件", []string{"不要生成文件，但请创建 PDF 文件"}, task.FileDeliveryUnclear, false},
		{"later file request", "只要文字回答", []string{"请生成 PDF 文件"}, task.FileDeliveryRequired, false},
		{"inherited workspace target", "看看代码", []string{"修复代码"}, task.FileDeliveryRequired, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRouteActionsTestFixture(t)
			ctx := context.Background()
			input := app.ConversationRoutingInput{}
			if test.workspace {
				input.WorkspaceID = "w1234567"
			}
			initial := f.receive(t, "file-intent-initial", test.initial, input)
			snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, initial)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.Context.ModelID = "p1234567"
			if err := f.store.ApplyConversationRoute(ctx, initial, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
				t.Fatal(err)
			}
			var taskID string
			if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, initial.Message.ID).Scan(&taskID); err != nil {
				t.Fatal(err)
			}
			for index, content := range test.revisions {
				message := f.receive(t, fmt.Sprintf("file-intent-revision-%d", index), content, app.ConversationRoutingInput{ReferenceTaskID: taskID})
				updated, err := f.store.LoadConversationRouteSnapshot(ctx, message)
				if err != nil || len(updated.Context.Candidates) != 1 {
					t.Fatalf("revision candidates=%+v err=%v", updated.Context.Candidates, err)
				}
				updated.Context.ModelID = "p1234567"
				if err := f.store.ApplyConversationRoute(ctx, message, updated, app.ConversationRouteDecision{Action: app.ConversationRouteUpdate,
					TaskID: taskID, ExpectedRevision: updated.Context.Candidates[0].Revision}); err != nil {
					t.Fatal(err)
				}
			}
			if len(test.revisions) != 0 {
				if _, err := f.store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE task_id=$1`, taskID); err != nil {
					t.Fatal(err)
				}
				stop, found, err := f.store.ClaimNextTaskStop(ctx, "file-intent-stop-worker", time.Minute)
				if err != nil || !found || stop.Task.ID != taskID {
					t.Fatalf("stop: found=%t err=%v", found, err)
				}
				if _, err := f.store.CompleteTaskStop(ctx, taskID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); err != nil {
					t.Fatal(err)
				}
				if applied, err := f.store.ApplyNextConversationTaskRevision(ctx); err != nil || !applied {
					t.Fatalf("apply revision: applied=%t err=%v", applied, err)
				}
			}
			var actual string
			if err := f.store.pool.QueryRow(ctx, `SELECT file_delivery_intent FROM tasks WHERE id=$1`, taskID).Scan(&actual); err != nil || task.FileDeliveryIntent(actual) != test.want {
				t.Fatalf("file intent=%q want=%q err=%v", actual, test.want, err)
			}
		})
	}
}
