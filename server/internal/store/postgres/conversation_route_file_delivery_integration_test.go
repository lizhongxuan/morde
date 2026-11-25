package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestConversationRouteNewTaskUsesStructuredFileDeliveryIntent(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		intent  task.FileDeliveryIntent
	}{
		{name: "model says none despite file wording", content: "请生成 PDF 文件并提供下载。", intent: task.FileDeliveryNone},
		{name: "model requires file without keyword", content: "请分析这个问题并给出结论。", intent: task.FileDeliveryRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &routeModelClientTest{reply: `{"action":"new","task_id":"","file_delivery_intent":"` + string(test.intent) + `"}`}
			store, gateway, _, claim, _ := routeModelFixtureWithMemberAndContent(t, false, false, client, test.content)
			ctx := context.Background()
			if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, claim.Message.OwnerID); err != nil {
				t.Fatal(err)
			}
			snapshot, decision := classifyFileDeliveryRoute(t, store, gateway, claim)
			if decision.InvalidOutput || decision.Action != app.ConversationRouteNew || decision.FileDeliveryIntent != test.intent {
				t.Fatalf("model route decision = %+v", decision)
			}
			if err := store.ApplyConversationRoute(ctx, claim, snapshot, decision); err != nil {
				t.Fatal(err)
			}
			var got string
			if err := store.pool.QueryRow(ctx, `SELECT t.file_delivery_intent FROM tasks t JOIN conversation_messages m ON m.task_id=t.id WHERE m.id=$1`, claim.Message.ID).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != string(test.intent) {
				t.Fatalf("created task file_delivery_intent = %q, want %q", got, test.intent)
			}
		})
	}
}

func TestConversationRouteRevisionReadsStructuredFileDeliveryDecision(t *testing.T) {
	for _, test := range []struct {
		name             string
		content          string
		revisionIntent   string
		want             task.FileDeliveryIntent
		mismatchRevision bool
	}{
		{name: "empty means inherit despite file wording", content: "请生成 PDF 文件并提供下载。", want: task.FileDeliveryNone},
		{name: "explicit none overrides file wording", content: "请生成 PDF 文件并提供下载。", revisionIntent: "none", want: task.FileDeliveryNone},
		{name: "explicit required overrides negation wording", content: "不要生成文件，只用文字说明。", revisionIntent: "required", want: task.FileDeliveryRequired},
		{name: "wrong stored revision is deferred", content: "请生成 PDF 文件并提供下载。", revisionIntent: "required", want: task.FileDeliveryNone, mismatchRevision: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			newReply := `{"action":"new","task_id":"","file_delivery_intent":"none"}`
			client := &routeModelClientTest{reply: newReply}
			store, gateway, _, initialClaim, _ := routeModelFixtureWithMemberAndContent(t, false, false, client, "请分析这段内容。")
			ctx := context.Background()
			if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, initialClaim.Message.OwnerID); err != nil {
				t.Fatal(err)
			}
			initialSnapshot, initialDecision := classifyFileDeliveryRoute(t, store, gateway, initialClaim)
			if err := store.ApplyConversationRoute(ctx, initialClaim, initialSnapshot, initialDecision); err != nil {
				t.Fatal(err)
			}
			var taskID string
			if err := store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, initialClaim.Message.ID).Scan(&taskID); err != nil {
				t.Fatal(err)
			}

			revisionMessage := receiveCausalMessage(t, store, initialClaim, "fd-revision-001", initialClaim.Message.ClientMessageID, test.content)
			revisionClaim := claimCausalMessage(t, store, revisionMessage.ID)
			client.secondReply = `{"action":"update","task_id":"` + taskID + `"}`
			if test.revisionIntent != "" {
				client.secondReply = `{"action":"update","task_id":"` + taskID + `","file_delivery_intent":"` + test.revisionIntent + `"}`
			}
			revisionSnapshot, revisionDecision := classifyFileDeliveryRoute(t, store, gateway, revisionClaim)
			if revisionDecision.InvalidOutput || revisionDecision.Action != app.ConversationRouteUpdate {
				t.Fatalf("revision route decision = %+v", revisionDecision)
			}
			if err := store.ApplyConversationRoute(ctx, revisionClaim, revisionSnapshot, revisionDecision); err != nil {
				t.Fatal(err)
			}
			if test.mismatchRevision {
				result, err := store.pool.Exec(ctx, `UPDATE conversation_route_model_calls SET decision=jsonb_set(decision,'{ExpectedRevision}','1'::jsonb,true)
WHERE message_id=$1 AND phase='normal' AND attempt=(SELECT route_attempt FROM conversation_message_routes WHERE message_id=$1)`, revisionMessage.ID)
				if err != nil || result.RowsAffected() != 1 {
					t.Fatalf("tamper synthetic stored revision fence: rows=%d err=%v", result.RowsAffected(), err)
				}
			}

			if _, err := store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE task_id=$1 AND status='pending'`, taskID); err != nil {
				t.Fatal(err)
			}
			stop, found, err := store.ClaimNextTaskStop(ctx, "file-delivery-revision-stop", time.Minute)
			if err != nil || !found || stop.Task.ID != taskID {
				t.Fatalf("claim revision stop: found=%t err=%v", found, err)
			}
			if _, err := store.CompleteTaskStop(ctx, taskID, stop.Request.ID, stop.Request.LeaseOwner, stop.Request.LeaseVersion); err != nil {
				t.Fatal(err)
			}
			applied, err := store.ApplyNextConversationTaskRevision(ctx)
			if test.mismatchRevision {
				if err != nil || applied {
					t.Fatalf("mismatched durable revision was applied: applied=%t err=%v", applied, err)
				}
				var status, intent string
				if err := store.pool.QueryRow(ctx, `SELECT status,file_delivery_intent FROM tasks WHERE id=$1`, taskID).Scan(&status, &intent); err != nil ||
					status != string(task.StatusPaused) || intent != string(task.FileDeliveryNone) {
					t.Fatalf("mismatched durable revision changed task: status=%s intent=%s err=%v", status, intent, err)
				}
				return
			}
			if err != nil || !applied {
				t.Fatalf("apply structured revision: applied=%t err=%v", applied, err)
			}
			var got string
			if err := store.pool.QueryRow(ctx, `SELECT file_delivery_intent FROM tasks WHERE id=$1`, taskID).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != string(test.want) {
				t.Fatalf("revised file_delivery_intent = %q, want %q", got, test.want)
			}
		})
	}
}

func classifyFileDeliveryRoute(t *testing.T, store *Store, gateway *app.ModelGatewayService,
	claim app.ConversationRouteClaim) (app.ConversationRouteSnapshot, app.ConversationRouteDecision) {
	t.Helper()
	ctx := context.Background()
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	modelID, err := gateway.ResolveConversationRouteModel(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = modelID
	decision, err := gateway.ClassifyConversationRoute(ctx, claim, snapshot.Context)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, decision
}
