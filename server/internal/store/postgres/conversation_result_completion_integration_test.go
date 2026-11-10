package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/task"
)

func TestRoutedTaskCompletionPublishesOneCombinedConversationMessage(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	var messageIDs []string
	for index, content := range []string{"first question", "second question"} {
		clientID := []string{"result-message-first", "result-message-second"}[index]
		claim := f.receive(t, clientID, content, app.ConversationRoutingInput{})
		snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Context.ModelID = "p1234567"
		if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
			t.Fatal(err)
		}
		messageIDs = append(messageIDs, claim.Message.ID)
	}
	claimed := make([]task.Task, 0, 2)
	for index := 0; index < 2; index++ {
		value, found, err := f.store.ClaimNextTask(ctx, "result-worker", time.Minute)
		if err != nil || !found {
			t.Fatalf("claim task %d: found=%t err=%v", index, found, err)
		}
		claimed = append(claimed, value)
	}
	taskMessageIDs := make(map[string]string, len(messageIDs))
	for _, messageID := range messageIDs {
		var taskID string
		if err := f.store.pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, messageID).Scan(&taskID); err != nil {
			t.Fatal(err)
		}
		taskMessageIDs[taskID] = messageID
	}
	for index, value := range claimed {
		stepID := mustID(t, domain.NewStepID)
		answer := []string{"first answer", "second answer"}[index]
		_, err := f.store.CommitTaskProgress(ctx, app.TaskProgress{TaskID: value.ID, LeaseOwner: value.Lease.Owner,
			LeaseVersion: value.Lease.Version, Status: task.StatusCompleted, CurrentStep: stepID,
			Result: answer, PublishResultMessage: true, Usage: value.Usage,
			Steps: []task.Step{{ID: stepID, TaskID: value.ID, Sequence: 1, Kind: "result", Status: task.StepSucceeded}},
			Event: task.Event{Type: "task.result", Payload: []byte(`{}`)}})
		if err != nil {
			t.Fatalf("complete task %d: %v", index, err)
		}
		if index == 0 {
			details, err := f.store.GetTask(ctx, value.ID, value.OwnerID)
			if err != nil || !details.ResultMessagePending || details.ResultMessagePublished {
				t.Fatalf("first result did not wait: task=%+v err=%v", details, err)
			}
		}
	}
	(resultAggregationFixture{store: f.store, ownerID: f.ownerID, conversation: f.conversation}).runNextReview(t)
	page, err := f.store.ListMessages(ctx, f.ownerID, f.conversation.ID, app.PageRequest{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	var replies []conversation.Message
	for _, message := range page.Items {
		if message.Role == conversation.RoleAssistant {
			replies = append(replies, message)
		}
	}
	if len(replies) != 1 || replies[0].Content != resultReviewTestReply || len(replies[0].Sources) != 2 ||
		replies[0].Sources[0].UserMessageID != taskMessageIDs[claimed[0].ID] || replies[0].Sources[1].UserMessageID != taskMessageIDs[claimed[1].ID] ||
		replies[0].Sources[0].TaskID != claimed[0].ID || replies[0].Sources[1].TaskID != claimed[1].ID ||
		replies[0].Sources[0].Content != "first answer" || replies[0].Sources[1].Content != "second answer" {
		t.Fatalf("combined reply is missing or duplicated: %+v", replies)
	}
	for _, value := range claimed {
		details, err := f.store.GetTask(ctx, value.ID, value.OwnerID)
		if err != nil || details.ResultMessagePending || !details.ResultMessagePublished || details.AssistantMessage != nil ||
			details.ResultReview == nil || details.ResultReview.State != "published" || details.ResultReview.SourceTaskID != value.ID ||
			details.ResultReview.ReplyMessageID == nil || *details.ResultReview.ReplyMessageID != replies[0].ID {
			t.Fatalf("published result projection for %s: %+v err=%v", value.ID, details, err)
		}
	}
}
