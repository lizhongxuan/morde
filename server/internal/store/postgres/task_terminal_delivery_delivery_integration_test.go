package postgres

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

func TestPostgresTaskTerminalDeliveryFactsAreAuthorizedSummaries(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET model_id='p1234567' WHERE id=$1`, f.request.Fence.TaskID); err != nil {
		t.Fatalf("set test Task model identity: %v", err)
	}
	commitTaskTerminalDeliveryFailure(t, f)
	claims, err := f.store.ClaimTaskTerminalDeliverySources(ctx, "terminal-facts-reader", 30*time.Second, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim terminal delivery source: claims=%+v err=%v", claims, err)
	}
	claim := claims[0]
	facts, found, err := f.store.ReadTaskTerminalDeliverySourceFacts(ctx, claim)
	if err != nil || !found || !app.ValidTaskTerminalDeliverySourceFacts(claim, facts) {
		t.Fatalf("read verified Task terminal delivery facts: facts=%+v found=%t err=%v", facts, found, err)
	}
	if facts.TaskStatus != "failed" || facts.FailureCode != "provider_unavailable" || facts.ModelID == "" ||
		facts.StepCounts.Total != 1 || facts.StepCounts.Failed != 1 || facts.StepCounts.Succeeded != 0 ||
		bytes.Contains([]byte(facts.ObjectiveSummary), []byte("Task stopped after a model error.")) {
		t.Fatalf("source facts included an unexpected value: %+v", facts)
	}
	modelRequest, err := app.BuildTaskTerminalDeliveryDSHModelRequest(facts.ModelID, facts)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateIdentity := range []string{claim.OwnerID, claim.ConversationID, claim.TaskID, "source_sha256", "objective_sha256"} {
		if bytes.Contains(modelRequest, []byte(privateIdentity)) {
			t.Fatalf("model input contains internal identity %q: %s", privateIdentity, modelRequest)
		}
	}
	digest, err := app.TaskTerminalDeliveryFactsDigest(facts)
	if err != nil || !bytes.Equal(digest, facts.FactsSHA256) {
		t.Fatalf("facts summary digest mismatch: digest=%x stored=%x err=%v", digest, facts.FactsSHA256, err)
	}

	if _, err := f.store.pool.Exec(ctx, `UPDATE task_events SET payload=payload||'{"reason":"changed"}'::jsonb
WHERE task_id=$1 AND sequence=$2`, claim.TaskID, claim.SourceEventSequence); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.store.ReadTaskTerminalDeliverySourceFacts(ctx, claim); err != nil || found {
		t.Fatalf("tampered source event remained readable: found=%t err=%v", found, err)
	}
	var state string
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM task_terminal_delivery_outbox WHERE id=$1`, claim.DeliveryID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != TaskTerminalDeliveryStateInvalidated {
		t.Fatalf("tampered source state=%q, want invalidated", state)
	}
}

func TestPostgresTaskTerminalDeliveryMessageAndPublishedMarkerAreAtomicAndIdempotent(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	commitTaskTerminalDeliveryFailure(t, f)
	claims, err := f.store.ClaimTaskTerminalDeliverySources(ctx, "terminal-publisher", 30*time.Second, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim terminal delivery source: claims=%+v err=%v", claims, err)
	}
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x2a}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := app.SealTaskTerminalDeliveryReply(sealer, claims[0], app.TaskTerminalDeliveryReply{
		Kind: app.TaskTerminalDeliveryReplySystemState,
		Text: "系统状态说明：任务执行失败，当前无法生成 Agent 整理说明。",
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, changed, err := f.store.PersistTaskTerminalDeliveryReplyReady(ctx, claims[0], ciphertext)
	if err != nil || !changed || ready.State != app.TaskTerminalDeliveryStateReplyReady {
		t.Fatalf("persist fallback reply-ready: ready=%+v changed=%t err=%v", ready, changed, err)
	}
	if _, err := f.store.pool.Exec(ctx, `CREATE FUNCTION test_reject_terminal_delivery_publish() RETURNS trigger
LANGUAGE plpgsql AS $$ BEGIN
  IF OLD.state='reply_ready' AND NEW.state='published' THEN
    RAISE EXCEPTION 'forced terminal delivery publish marker failure';
  END IF;
  RETURN NEW;
END $$`); err != nil {
		t.Fatalf("create terminal delivery publish trigger function: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `CREATE TRIGGER test_reject_terminal_delivery_publish
BEFORE UPDATE ON task_terminal_delivery_outbox FOR EACH ROW EXECUTE FUNCTION test_reject_terminal_delivery_publish()`); err != nil {
		t.Fatalf("create terminal delivery publish trigger: %v", err)
	}
	if _, _, err := f.store.PublishTaskTerminalDeliveryReply(ctx, ready, "系统状态说明：任务执行失败，当前无法生成 Agent 整理说明。"); err == nil {
		t.Fatal("message publication committed without the source marker")
	}
	if _, err := f.store.pool.Exec(ctx, `DROP TRIGGER test_reject_terminal_delivery_publish ON task_terminal_delivery_outbox`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `DROP FUNCTION test_reject_terminal_delivery_publish()`); err != nil {
		t.Fatal(err)
	}
	var assistantMessages int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE task_id=$1 AND role='assistant'`,
		ready.TaskID).Scan(&assistantMessages); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM task_terminal_delivery_outbox WHERE id=$1`, ready.DeliveryID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if assistantMessages != 0 || state != TaskTerminalDeliveryStateReplyReady {
		t.Fatalf("failed transaction was partially committed: assistant_messages=%d state=%q", assistantMessages, state)
	}

	published, messageID, err := f.store.PublishTaskTerminalDeliveryReply(ctx, ready,
		"系统状态说明：任务执行失败，当前无法生成 Agent 整理说明。")
	if err != nil || !published || !domain.ValidMessageID(messageID) {
		t.Fatalf("publish terminal delivery: published=%t message=%q err=%v", published, messageID, err)
	}
	replayed, replayMessageID, err := f.store.PublishTaskTerminalDeliveryReply(ctx, ready,
		"系统状态说明：任务执行失败，当前无法生成 Agent 整理说明。")
	if err != nil || replayed || replayMessageID != messageID {
		t.Fatalf("replayed terminal delivery: published=%t message=%q err=%v", replayed, replayMessageID, err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages
WHERE id=$1 AND task_id=$2 AND role='assistant' AND content=$3`, messageID, ready.TaskID,
		"系统状态说明：任务执行失败，当前无法生成 Agent 整理说明。").Scan(&assistantMessages); err != nil {
		t.Fatal(err)
	}
	if assistantMessages != 1 {
		t.Fatalf("published assistant messages=%d, want one", assistantMessages)
	}
	var storedState, storedMessageID string
	if err := f.store.pool.QueryRow(ctx, `SELECT state,published_message_id FROM task_terminal_delivery_outbox WHERE id=$1`,
		ready.DeliveryID).Scan(&storedState, &storedMessageID); err != nil {
		t.Fatal(err)
	}
	if storedState != TaskTerminalDeliveryStatePublished || storedMessageID != messageID {
		t.Fatalf("published source identity changed: state=%q message=%q", storedState, storedMessageID)
	}
}

func TestPostgresTaskTerminalDeliveryInvalidatesAfterLaterConversationInput(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	commitTaskTerminalDeliveryFailure(t, f)
	claims, err := f.store.ClaimTaskTerminalDeliverySources(ctx, "terminal-steer-fence", 30*time.Second, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim terminal delivery source: claims=%+v err=%v", claims, err)
	}
	ready, changed, err := f.store.PersistTaskTerminalDeliveryReplyReady(ctx, claims[0], []byte("synthetic encrypted reply"))
	if err != nil || !changed {
		t.Fatalf("persist terminal reply-ready: changed=%t err=%v", changed, err)
	}
	messageID := mustID(t, domain.NewMessageID)
	if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
		return insertMessage(ctx, tx, conversation.Message{ID: messageID, OwnerID: ready.OwnerID,
			ActorID: ready.InitiatorID, ConversationID: ready.ConversationID, Role: conversation.RoleUser,
			Content: "Please change the task direction.", SourceKind: "conversation",
			DeliveryStatus: conversation.DeliveryPending})
	}); err != nil {
		t.Fatalf("insert later pending user input: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO conversation_message_routes
(service_instance_id,owner_id,actor_id,conversation_id,message_id,client_message_id,request_hash,request_metadata,authorization_snapshot)
SELECT instance_id,$1,$2,$3,$4,'later-input-0001',$5,'{}'::jsonb,'{}'::jsonb FROM service_identity WHERE singleton=true`,
		ready.OwnerID, ready.InitiatorID, ready.ConversationID, messageID, bytes.Repeat([]byte{0x44}, 32)); err != nil {
		t.Fatalf("insert pending conversation route: %v", err)
	}
	published, publishedID, err := f.store.PublishTaskTerminalDeliveryReply(ctx, ready,
		"An old terminal reply that must not be published.")
	if err != nil || published || publishedID != "" {
		t.Fatalf("stale reply published before later route was applied: published=%t message=%q err=%v", published, publishedID, err)
	}
	var state string
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM task_terminal_delivery_outbox WHERE id=$1`, ready.DeliveryID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != TaskTerminalDeliveryStateInvalidated {
		t.Fatalf("source after later input state=%q, want invalidated", state)
	}
}
