package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

func TestConversationSummaryInputPagesKeepLongSourcesAndExcludeSuppressedDescendants(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	routeAsNew := func(clientID, content string) app.ConversationRouteClaim {
		claim := f.receive(t, clientID, content, app.ConversationRoutingInput{})
		snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Context.ModelID = "p1234567"
		if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{
			Action: app.ConversationRouteReply, Reply: "历史回应：" + clientID,
		}); err != nil {
			t.Fatal(err)
		}
		return claim
	}
	allowed := routeAsNew("summary-input-old-1", "保留这条历史消息。")
	suppressed := routeAsNew("summary-input-old-2", "这条已抑制消息不得进入摘要。")
	longText := strings.Repeat("中文历史内容。", 1500)
	longSource := routeAsNew("summary-input-old-3", longText)
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO personal_history_suppressions(service_instance_id,owner_id,message_id)
VALUES ($1::uuid,$2,$3)`, f.instanceID, f.ownerID, suppressed.Message.ID); err != nil {
		t.Fatal(err)
	}
	results := resultAggregationFixture{store: f.store, ownerID: f.ownerID, conversation: f.conversation}
	firstResult, secondResult := results.createTask(t, f.ownerID, "first summary task"), results.createTask(t, f.ownerID, "second summary task")
	if !results.complete(t, &firstResult, "first confirmed task result") || !results.complete(t, &secondResult, "second confirmed task result") {
		t.Fatal("confirmed task results were not queued")
	}
	results.runNextReview(t)

	childClientID, currentClientID := "summary-input-child", "summary-input-current"
	childHash := sha256.Sum256([]byte(childClientID))
	child, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.ownerID, ActorID: f.ownerID,
			ConversationID: f.conversation.ID, Role: conversation.RoleUser, Content: "因果后代不能仅按较小序号纳入摘要。"},
		ServiceInstanceID: f.instanceID, ClientID: childClientID, RequestHash: childHash[:],
		Input: app.ConversationRoutingInput{PreviousClientMessageID: currentClientID}, Authorization: f.authorization,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.store.ClaimConversationRoute(ctx, "summary-child-held", time.Minute); err != nil || !found {
		t.Fatalf("claim early-sequence descendant: found=%t err=%v", found, err)
	}
	current := f.receive(t, currentClientID, "继续处理。", app.ConversationRoutingInput{})

	var cursor *app.ConversationSummaryInputCursor
	parts := make(map[string][]string)
	taskResultIDs := make(map[string]bool)
	assistantReplies := 0
	for pageIndex := 0; pageIndex < 16; pageIndex++ {
		page, err := f.store.LoadConversationSummaryInputPage(ctx, app.ConversationSummaryInputPageRequest{
			Claim: current, ContextWindowTokens: 4096, Cursor: cursor,
		})
		if err != nil {
			t.Fatal(err)
		}
		if page.Identity.ConversationID != current.Message.ConversationID || page.BoundarySequence != current.Message.Sequence-1 || page.Head.Version != 0 {
			t.Fatalf("summary page fence = %+v", page)
		}
		for _, source := range page.Sources {
			parts[source.Source.ID] = append(parts[source.Source.ID], source.Content)
			if source.Role == string(conversation.RoleAssistant) && strings.HasPrefix(source.Content, "历史回应：") {
				assistantReplies++
			}
			if source.Source.Kind == app.ConversationSummarySourceTask {
				taskResultIDs[source.Source.ID] = true
			}
			if source.Source.ID == longSource.Message.ID {
				want := sha256.Sum256([]byte(longText))
				if !bytes.Equal(source.Source.SHA256, want[:]) {
					t.Fatalf("long source proof changed: %x", source.Source.SHA256)
				}
			}
		}
		if page.Next == nil {
			break
		}
		cursor = page.Next
		if pageIndex == 15 {
			t.Fatal("summary input pagination did not terminate")
		}
	}
	if strings.Join(parts[longSource.Message.ID], "") != longText {
		t.Fatal("long conversation message was truncated or reordered across summary pages")
	}
	if len(parts[allowed.Message.ID]) == 0 || len(parts[suppressed.Message.ID]) != 0 || len(parts[child.ID]) != 0 {
		t.Fatalf("summary source visibility: allowed=%t suppressed=%t descendant=%t", len(parts[allowed.Message.ID]) > 0,
			len(parts[suppressed.Message.ID]) > 0, len(parts[child.ID]) > 0)
	}
	if !taskResultIDs[firstResult.ID] || !taskResultIDs[secondResult.ID] {
		t.Fatalf("published task result sources were omitted: %v", taskResultIDs)
	}
	if assistantReplies < 3 {
		t.Fatalf("ordinary assistant route replies were omitted: included=%d", assistantReplies)
	}
}

func TestProjectConversationSummaryInputSharesOnlyProvenMemberHistory(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	results := resultAggregationFixture{store: f.store, ownerID: f.owner.UserID,
		conversation: conversation.Conversation{ID: f.conversationID, OwnerID: f.owner.UserID, ProjectID: f.projectID}}
	first, second := results.createTask(t, f.owner.UserID, "first shared summary task"),
		results.createTask(t, f.owner.UserID, "second shared summary task")
	if !results.complete(t, &first, "first shared confirmed result") || !results.complete(t, &second, "second shared confirmed result") {
		t.Fatal("shared task results were not queued")
	}
	results.runNextReview(t)
	pendingTask := results.createTask(t, f.owner.UserID, "unpublished owner task")
	claim := projectHistoryClaim(t, f, "project-summary-input-current")
	page, err := f.store.LoadConversationSummaryInputPage(ctx, app.ConversationSummaryInputPageRequest{
		Claim: claim, ContextWindowTokens: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.Identity.ScopeKind != app.ConversationSummaryProjectScope || page.Identity.ActorID != f.member.UserID {
		t.Fatalf("project summary scope = %+v", page.Identity)
	}
	sharedResults := make(map[string]bool)
	for _, source := range page.Sources {
		if source.Source.Kind == app.ConversationSummarySourceTask {
			sharedResults[source.Source.ID] = true
		}
		if source.Source.ID == pendingTask.UserMessageID {
			t.Fatal("unpublished project task history entered shared summary input")
		}
	}
	if !sharedResults[first.ID] || !sharedResults[second.ID] {
		t.Fatalf("proven project task results were omitted: %v", sharedResults)
	}
}
