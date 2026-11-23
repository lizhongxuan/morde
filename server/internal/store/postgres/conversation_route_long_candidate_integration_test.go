package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
)

func TestStoredLongRouteDecisionDoesNotTreatArticleTextAsRestart(t *testing.T) {
	decision := app.ConversationRouteDecision{Action: app.ConversationRouteNew, ExplicitNew: true}
	value := app.ConversationRouteContext{ContentTruncated: true, Content: "请总结这篇文章：\n正文写着重新开始新任务。"}
	if validStoredRouteDecision(decision, value) {
		t.Fatal("stored decision accepted restart text from the document")
	}
	value.Content = "重新开始新任务，请总结这篇文章：\n正文。"
	if !validStoredRouteDecision(decision, value) {
		t.Fatal("stored decision rejected the explicit restart request")
	}
}

func TestLongArticleFallbackKeepsInterruptedDuplicateAndRoutesIndependentRequest(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	content := "好现在你帮我看下这篇文章说了什么：\n# 长文章\n" + strings.Repeat("文章正文。", 5000)
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
	decision, ok := app.ConversationRouteLongMessageFallback(preview)
	if !ok || decision.Action != app.ConversationRouteClarify || !strings.Contains(decision.Question, "相同") {
		t.Fatalf("unresolved duplicate decision = %+v, ok=%t", decision, ok)
	}
	if err := f.store.ApplyConversationRoute(ctx, duplicate, snapshot, decision); err != nil {
		t.Fatalf("apply duplicate recovery guidance: %v", err)
	}
	var taskCount int
	var state, oldStatus string
	if err := f.store.pool.QueryRow(ctx, `SELECT r.state,t.status,
 (SELECT count(*) FROM tasks WHERE conversation_id=r.conversation_id)
 FROM conversation_message_routes r JOIN tasks t ON t.id=$2 WHERE r.message_id=$1`, duplicate.Message.ID, claimed.ID).
		Scan(&state, &oldStatus, &taskCount); err != nil || state != "waiting_user" || oldStatus != "waiting_user" || taskCount != 1 {
		t.Fatalf("duplicate changed task state or created work: route=%s old=%s count=%d err=%v", state, oldStatus, taskCount, err)
	}

	independentContent := "请总结这篇文章：\n# 另一篇文章\n" + strings.Repeat("新文章正文。", 5000)
	independent := f.receive(t, "long-candidate-other-001", independentContent, app.ConversationRoutingInput{
		PreviousClientMessageID: root.Message.ClientMessageID,
	})
	snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, independent)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	preview = snapshot.Context
	app.ProjectConversationRouteMessage(&preview, app.ConversationRouteCurrentMessagePreviewBytes)
	decision, ok = app.ConversationRouteLongMessageFallback(preview)
	if !ok || decision.Action != app.ConversationRouteNew {
		t.Fatalf("independent article decision = %+v, ok=%t", decision, ok)
	}
	if err := f.store.ApplyConversationRoute(ctx, independent, snapshot, decision); err != nil {
		t.Fatalf("route independent article: %v", err)
	}
	var newTaskID, stored string
	if err := f.store.pool.QueryRow(ctx, `SELECT task_id,content FROM conversation_messages WHERE id=$1`, independent.Message.ID).
		Scan(&newTaskID, &stored); err != nil || newTaskID == "" || newTaskID == claimed.ID || stored != independentContent {
		t.Fatalf("independent article source or task changed: new_task=%s err=%v", newTaskID, err)
	}

	explicitContent := "重新开始新任务，请总结这篇文章：\n# 新文章\n" + strings.Repeat("正文。", 3000)
	var previousClientID, explicitTaskID string
	previousClientID = root.Message.ClientMessageID
	for _, clientID := range []string{"long-candidate-explicit-001", "long-candidate-explicit-002"} {
		retry := f.receive(t, clientID, explicitContent, app.ConversationRoutingInput{PreviousClientMessageID: previousClientID})
		snapshot, err = f.store.LoadConversationRouteSnapshot(ctx, retry)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Context.ModelID = "p1234567"
		preview = snapshot.Context
		app.ProjectConversationRouteMessage(&preview, app.ConversationRouteCurrentMessagePreviewBytes)
		decision, ok = app.ConversationRouteLongMessageFallback(preview)
		if !ok || decision.Action != app.ConversationRouteNew || !decision.ExplicitNew {
			t.Fatalf("explicit restart decision = %+v, ok=%t", decision, ok)
		}
		if err := f.store.ApplyConversationRoute(ctx, retry, snapshot, decision); err != nil {
			t.Fatalf("apply explicit restart: %v", err)
		}
		var taskID string
		if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, retry.Message.ID).
			Scan(&taskID); err != nil || taskID == "" || taskID == explicitTaskID {
			t.Fatalf("explicit restart did not create the requested new task: task=%s err=%v", taskID, err)
		}
		explicitTaskID, previousClientID = taskID, retry.Message.ClientMessageID
	}
}
