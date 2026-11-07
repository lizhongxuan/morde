package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

func TestPersonalTaskRecoveryUsesOriginalConversationSnapshot(t *testing.T) {
	for _, tamper := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "changed original"}[tamper], func(t *testing.T) {
			f := newRouteActionsTestFixture(t)
			ctx := context.Background()
			priorID := mustID(t, domain.NewMessageID)
			if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
				return insertMessage(ctx, tx, conversation.Message{ID: priorID, ConversationID: f.conversation.ID,
					OwnerID: f.ownerID, ActorID: f.ownerID, Role: conversation.RoleUser, Content: "Original authorized history"})
			}); err != nil {
				t.Fatal(err)
			}
			initial := f.receive(t, "personal-context-recovery-initial", "Start task", app.ConversationRoutingInput{})
			snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, initial)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.Context.ModelID = "p1234567"
			if err := f.store.ApplyConversationRoute(ctx, initial, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
				t.Fatal(err)
			}
			first, found, err := f.store.ClaimNextTask(ctx, "context-first-worker", time.Minute)
			if err != nil || !found || first.ConversationContext == nil || len(first.ConversationContext.Messages) != 1 {
				t.Fatalf("initial context: found=%t context=%+v err=%v", found, first.ConversationContext, err)
			}
			if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET checkpoint_through_seq=0,checkpoint_digest=decode(repeat('01',32),'hex'),
lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, first.ID); err != nil {
				t.Fatal(err)
			}
			if tamper {
				if _, err := f.store.pool.Exec(ctx, `UPDATE conversation_messages SET content='changed private history' WHERE id=$1`, priorID); err != nil {
					t.Fatal(err)
				}
			} else {
				f.receive(t, "personal-context-recovery-later", "New message outside the original snapshot", app.ConversationRoutingInput{})
			}
			resumed, found, err := f.store.ClaimNextTask(ctx, "context-recovery-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if tamper {
				if found {
					t.Fatal("changed original history was accepted during recovery")
				}
				return
			}
			if !found || resumed.ID != first.ID || resumed.ConversationContext == nil ||
				len(resumed.ConversationContext.Messages) != 1 || resumed.ConversationContext.Messages[0].ID != priorID ||
				resumed.ConversationContext.Messages[0].Content != "Original authorized history" {
				t.Fatalf("recovery enlarged or lost the original snapshot: found=%t context=%+v", found, resumed.ConversationContext)
			}
		})
	}
}
