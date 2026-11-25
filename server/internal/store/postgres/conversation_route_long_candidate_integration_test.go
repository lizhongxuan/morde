package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestStoredExplicitNewDecisionIsNotReinterpretedFromArticleText(t *testing.T) {
	decision := app.ConversationRouteDecision{Action: app.ConversationRouteNew, ExplicitNew: true}
	for _, content := range []string{
		"帮我检查一下接口的输入校验，并补上测试。",
		"请总结这篇文章：\n正文写着重新开始新任务。",
	} {
		value := app.ConversationRouteContext{ContentTruncated: true, Content: content}
		if !validStoredRouteDecision(decision, value) {
			t.Fatalf("stored structured explicit-new decision was rejected for content %q", content)
		}
	}
}

func TestLongRouteUsesStructuredDecisionAndPreservesDuplicateIdempotency(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	content := "请总结这篇文章：\n# 长文章\n正文引用“重新开始新任务”这句话。\n" + strings.Repeat("文章正文。", 5000)
	root := f.receive(t, "long-candidate-root-001", content, app.ConversationRoutingInput{})
	rootSnapshot, err := f.store.LoadConversationRouteSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	rootSnapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, root, rootSnapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "long-candidate-worker", time.Minute)
	if err != nil || !found {
		t.Fatalf("claim initial task: found=%t err=%v", found, err)
	}
	if _, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: claimed.ID, LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, Status: task.StatusWaitingUser, WaitingReason: "dsh_recovery_unresolved",
		Event: task.Event{Type: "task.waiting_user", Payload: []byte(`{"reason":"dsh_recovery_unresolved"}`)}}); err != nil {
		t.Fatalf("record interrupted task: %v", err)
	}

	duplicate := f.receive(t, "long-candidate-copy-001", content, app.ConversationRoutingInput{
		PreviousClientMessageID: root.Message.ClientMessageID,
	})
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, duplicate)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	preview := snapshot.Context
	app.ProjectConversationRouteMessage(&preview, app.ConversationRouteCurrentMessagePreviewBytes)
	decision := app.ParseConversationRouteDecisionForContext(`{"action":"new","explicit_new":false,"dependency_task_ids":[]}`, preview)
	if decision.Action != app.ConversationRouteNew || decision.ExplicitNew || decision.InvalidOutput {
		t.Fatalf("article wording changed the structured non-explicit New decision: %+v", decision)
	}
	if err := f.store.ApplyConversationRoute(ctx, duplicate, snapshot, decision); err != nil {
		t.Fatalf("apply duplicate idempotency decision: %v", err)
	}
	var taskCount int
	var state, oldStatus, duplicateTaskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT r.state,t.status,COALESCE(root.task_id,''),
 (SELECT count(*) FROM tasks WHERE conversation_id=duplicate.conversation_id)
	FROM conversation_message_routes r
	JOIN conversation_messages duplicate ON duplicate.id=r.message_id
	JOIN conversation_messages root ON root.id=r.duplicate_of_message_id
	JOIN tasks t ON t.id=root.task_id AND t.conversation_id=root.conversation_id
	WHERE r.message_id=$1 AND root.id=$3 AND t.id=$2`, duplicate.Message.ID, claimed.ID, root.Message.ID).
		Scan(&state, &oldStatus, &duplicateTaskID, &taskCount); err != nil || state != "routed" || oldStatus != "waiting_user" || duplicateTaskID != claimed.ID || taskCount != 1 {
		t.Fatalf("exact duplicate did not link to the existing Task: route=%s old=%s task=%s count=%d err=%v", state, oldStatus, duplicateTaskID, taskCount, err)
	}

	clarified := f.receive(t, "long-candidate-clarify-001", content, app.ConversationRoutingInput{
		PreviousClientMessageID: duplicate.Message.ClientMessageID,
	})
	snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, clarified)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	preview = snapshot.Context
	app.ProjectConversationRouteMessage(&preview, app.ConversationRouteCurrentMessagePreviewBytes)
	decision = app.ParseConversationRouteDecisionForContext(`{"action":"clarify","question":"请补充希望完成的目标。"}`, preview)
	if decision.Action != app.ConversationRouteClarify {
		t.Fatalf("body keywords changed the structured Clarify decision: %+v", decision)
	}
	if err := f.store.ApplyConversationRoute(ctx, clarified, snapshot, decision); err != nil {
		t.Fatalf("apply structured clarification: %v", err)
	}
	var clarifiedTaskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT COALESCE(task_id,'') FROM conversation_messages WHERE id=$1`, clarified.Message.ID).
		Scan(&clarifiedTaskID); err != nil || clarifiedTaskID != "" {
		t.Fatalf("model clarification was reinterpreted as a Task: task=%s err=%v", clarifiedTaskID, err)
	}

	explicitContent := "请总结这篇文章：\n# 再次处理\n" + strings.Repeat("文章正文。", 3000)
	explicit := f.receive(t, "long-candidate-explicit-001", explicitContent, app.ConversationRoutingInput{
		PreviousClientMessageID: clarified.Message.ClientMessageID,
	})
	snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, explicit)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	preview = snapshot.Context
	app.ProjectConversationRouteMessage(&preview, app.ConversationRouteCurrentMessagePreviewBytes)
	decision = app.ParseConversationRouteDecisionForContext(`{"action":"new","explicit_new":true,"dependency_task_ids":[]}`, preview)
	if decision.Action != app.ConversationRouteNew || !decision.ExplicitNew {
		t.Fatalf("structured ExplicitNew decision was changed by article wording: %+v", decision)
	}
	if err := f.store.ApplyConversationRoute(ctx, explicit, snapshot, decision); err != nil {
		t.Fatalf("apply structured ExplicitNew decision: %v", err)
	}
	var explicitTaskID string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, explicit.Message.ID).
		Scan(&explicitTaskID); err != nil || explicitTaskID == "" || explicitTaskID == claimed.ID {
		t.Fatalf("model ExplicitNew decision did not create a new Task: task=%s err=%v", explicitTaskID, err)
	}
}
