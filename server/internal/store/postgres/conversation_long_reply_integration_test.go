package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

func TestLongFormRouteClaimsExtendedLeaseAndCancellationIsDurable(t *testing.T) {
	f := newRouteActionsTestFixture(t)
	ctx := context.Background()
	claim := f.receive(t, "long-route-cancel-001", "Explain this carefully", app.ConversationRoutingInput{
		ResponseMode: app.ConversationResponseLongForm,
	})
	page, err := f.store.ListMessages(ctx, f.ownerID, claim.Message.ConversationID, app.PageRequest{Limit: 20})
	if err != nil || len(page.Items) != 1 || page.Items[0].ResponseMode != app.ConversationResponseLongForm {
		t.Fatalf("long-form mode was not included in message history: items=%+v err=%v", page.Items, err)
	}
	reconciled, found, err := f.store.FindConversationMessageByClientID(ctx, f.ownerID, "", claim.Message.ConversationID,
		claim.Message.ClientMessageID, f.instanceID)
	if err != nil || !found || reconciled.ResponseMode != app.ConversationResponseLongForm {
		t.Fatalf("long-form mode was not included in outbox reconciliation: message=%+v found=%t err=%v", reconciled, found, err)
	}
	remaining := time.Until(claim.LeaseExpiresAt)
	if remaining < 4*time.Minute+45*time.Second || remaining > 5*time.Minute+10*time.Second {
		t.Fatalf("long-form lease remaining = %v, want about five minutes", remaining)
	}
	if _, requested, err := f.store.CancelConversationRoute(ctx, f.ownerID, "", claim.Message.ConversationID,
		claim.Message.ID, f.instanceID); err != nil || !requested {
		t.Fatalf("first in-flight cancellation = changed:%t err:%v", requested, err)
	}
	message, changed, err := f.store.CancelConversationRoute(ctx, f.ownerID, "", claim.Message.ConversationID,
		claim.Message.ID, f.instanceID)
	if err != nil || changed || message.RoutingStatus != conversation.RoutingCancelRequested || message.ResponseMode != app.ConversationResponseLongForm {
		t.Fatalf("replayed cancellation = changed:%t status:%s mode:%s err:%v", changed, message.RoutingStatus, message.ResponseMode, err)
	}
	if err := f.store.ApplyConversationRoute(ctx, claim, app.ConversationRouteSnapshot{}, app.ConversationRouteDecision{
		Action: app.ConversationRouteReply, Reply: "This answer arrived after cancellation.", LongReply: true,
	}); err != nil {
		t.Fatalf("finalize cancellation without publishing late answer: %v", err)
	}
	var state, delivery string
	var assistants int
	if err := f.store.pool.QueryRow(ctx, `SELECT r.state,m.delivery_status,
(SELECT count(*) FROM conversation_messages a WHERE a.conversation_id=m.conversation_id AND a.role='assistant')
FROM conversation_message_routes r JOIN conversation_messages m ON m.id=r.message_id WHERE r.message_id=$1`,
		claim.Message.ID).Scan(&state, &delivery, &assistants); err != nil {
		t.Fatal(err)
	}
	if state != "cancelled" || delivery != string(conversation.DeliveryApplied) || assistants != 0 {
		t.Fatalf("cancelled route published output or remained unsettled: state=%s delivery=%s assistants=%d", state, delivery, assistants)
	}
}

func TestLongFormPendingCancellationAndPartialReply(t *testing.T) {
	t.Run("pending cancellation", func(t *testing.T) {
		f := newRouteActionsTestFixture(t)
		ctx := context.Background()
		claim := f.receive(t, "long-route-cancel-002", "Explain this carefully", app.ConversationRoutingInput{
			ResponseMode: app.ConversationResponseLongForm,
		})
		if err := f.store.ReleaseConversationRoute(ctx, claim); err != nil {
			t.Fatal(err)
		}
		message, changed, err := f.store.CancelConversationRoute(ctx, f.ownerID, "", claim.Message.ConversationID,
			claim.Message.ID, f.instanceID)
		if err != nil || !changed || message.RoutingStatus != conversation.RoutingCancelled || message.DeliveryStatus != conversation.DeliveryApplied {
			t.Fatalf("pending cancellation = changed:%t status:%s delivery:%s err:%v", changed, message.RoutingStatus, message.DeliveryStatus, err)
		}
	})

	t.Run("partial output is committed once", func(t *testing.T) {
		f := newRouteActionsTestFixture(t)
		ctx := context.Background()
		claim := f.receive(t, "long-route-partial-001", "Explain this carefully", app.ConversationRoutingInput{
			ResponseMode: app.ConversationResponseLongForm,
		})
		snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Context.ModelID = claim.Input.ModelID
		partial := strings.Repeat("partial answer ", 500)
		decision := app.ConversationRouteDecision{Action: app.ConversationRouteReply, Reply: partial, LongReply: true, Partial: true}
		if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, decision); err != nil {
			t.Fatal(err)
		}
		var state string
		var assistantCount int
		var stored string
		if err := f.store.pool.QueryRow(ctx, `SELECT r.state,
(SELECT count(*) FROM conversation_messages a WHERE a.conversation_id=m.conversation_id AND a.role='assistant'),
(SELECT a.content FROM conversation_messages a WHERE a.conversation_id=m.conversation_id AND a.role='assistant' ORDER BY a.sequence DESC LIMIT 1)
FROM conversation_message_routes r JOIN conversation_messages m ON m.id=r.message_id WHERE r.message_id=$1`,
			claim.Message.ID).Scan(&state, &assistantCount, &stored); err != nil {
			t.Fatal(err)
		}
		if state != "partial" || assistantCount != 1 || stored != strings.TrimSpace(partial) {
			t.Fatalf("partial route result = state:%s count:%d content_length:%d", state, assistantCount, len(stored))
		}
	})

	t.Run("short route cannot use cancellation", func(t *testing.T) {
		f := newRouteActionsTestFixture(t)
		claim := f.receive(t, "short-route-cancel-001", "Please answer briefly", app.ConversationRoutingInput{})
		if _, _, err := f.store.CancelConversationRoute(context.Background(), f.ownerID, "", claim.Message.ConversationID,
			claim.Message.ID, f.instanceID); domain.CodeOf(err) != domain.CodeConflict {
			t.Fatalf("short route cancellation error = %v", err)
		}
	})
}
