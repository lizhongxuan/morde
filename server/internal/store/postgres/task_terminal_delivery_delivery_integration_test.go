package postgres

import (
	"bytes"
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"

	"github.com/jackc/pgx/v5"
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
	reply := "系统状态说明：任务执行状态为失败。原因：" + app.TaskTerminalDeliveryFailureExplanation("provider_unavailable")
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
		Text: reply,
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
	if _, _, err := f.store.PublishTaskTerminalDeliveryReply(ctx, ready, reply); err == nil {
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

	published, messageID, err := f.store.PublishTaskTerminalDeliveryReply(ctx, ready, reply)
	if err != nil || !published || !domain.ValidMessageID(messageID) {
		t.Fatalf("publish terminal delivery: published=%t message=%q err=%v", published, messageID, err)
	}
	replayed, replayMessageID, err := f.store.PublishTaskTerminalDeliveryReply(ctx, ready, reply)
	if err != nil || replayed || replayMessageID != messageID {
		t.Fatalf("replayed terminal delivery: published=%t message=%q err=%v", replayed, replayMessageID, err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages
WHERE id=$1 AND task_id=$2 AND role='assistant' AND content=$3`, messageID, ready.TaskID,
		reply).Scan(&assistantMessages); err != nil {
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

func TestPostgresTaskTerminalDeliveryWaitingReplyPublishesOnceAfterTokenExpiry(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET model_id='p1234567' WHERE id=$1`, f.request.Fence.TaskID); err != nil {
		t.Fatalf("set Task model identity for delivery facts: %v", err)
	}
	sequence := commitTaskTerminalDeliveryWaiting(t, f, "dsh_recovery_unresolved")
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET auth_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`,
		f.request.Fence.TaskID); err != nil {
		t.Fatalf("expire the Task authorization snapshot after queuing: %v", err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tokens SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1 AND owner_id=$2`,
		f.principal.TokenID, f.principal.UserID); err != nil {
		t.Fatalf("expire the original authorization token after queuing: %v", err)
	}

	claims, err := f.store.ClaimTaskTerminalDeliverySources(ctx, "terminal-expired-token-publisher", 30*time.Second, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim waiting delivery after token expiry: claims=%+v err=%v", claims, err)
	}
	claim := claims[0]
	if claim.SourceKind != app.TaskTerminalDeliverySourceWaiting || claim.SourceEventSequence != sequence ||
		claim.TaskID != f.request.Fence.TaskID || claim.OwnerID != f.principal.UserID || claim.InitiatorID != f.principal.UserID {
		t.Fatalf("waiting delivery identity changed after token expiry: %+v", claim)
	}
	facts, found, err := f.store.ReadTaskTerminalDeliverySourceFacts(ctx, claim)
	if err != nil || !found || !app.ValidTaskTerminalDeliverySourceFacts(claim, facts) {
		t.Fatalf("read waiting delivery facts after token expiry: found=%t err=%v facts=%+v", found, err, facts)
	}
	if facts.WaitingReason != "reconciliation_required" {
		t.Fatalf("waiting reason was not kept within the safe public vocabulary: %q", facts.WaitingReason)
	}

	const replyText = "这次任务没有形成可核实的答复。现有记录无法确定具体的中断原因，也无法确认上一步操作是否完成。执行进度保留在任务详情中。"
	sealer, err := app.NewSealer(bytes.Repeat([]byte{0x3b}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := app.SealTaskTerminalDeliveryReply(sealer, claim, app.TaskTerminalDeliveryReply{
		Kind: app.TaskTerminalDeliveryReplySystemState,
		Text: replyText,
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, changed, err := f.store.PersistTaskTerminalDeliveryReplyReady(ctx, claim, ciphertext)
	if err != nil || !changed || ready.State != TaskTerminalDeliveryStateReplyReady {
		t.Fatalf("persist waiting explanation after token expiry: ready=%+v changed=%t err=%v", ready, changed, err)
	}
	published, messageID, err := f.store.PublishTaskTerminalDeliveryReply(ctx, ready, replyText)
	if err != nil || !published || !domain.ValidMessageID(messageID) {
		t.Fatalf("publish waiting explanation after token expiry: published=%t message=%q err=%v", published, messageID, err)
	}
	replayed, replayMessageID, err := f.store.PublishTaskTerminalDeliveryReply(ctx, ready, replyText)
	if err != nil || replayed || replayMessageID != messageID {
		t.Fatalf("replay waiting explanation after token expiry: published=%t message=%q err=%v", replayed, replayMessageID, err)
	}

	var messageCount int
	if err := f.store.pool.QueryRow(ctx, `SELECT count(*) FROM conversation_messages
WHERE id=$1 AND task_id=$2 AND owner_id=$3 AND actor_id=$4 AND conversation_id=$5
  AND role='assistant' AND source_kind='task' AND delivery_status='applied' AND content=$6`,
		messageID, claim.TaskID, claim.OwnerID, claim.InitiatorID, claim.ConversationID, replyText).Scan(&messageCount); err != nil {
		t.Fatal(err)
	}
	if messageCount != 1 {
		t.Fatalf("waiting explanation messages=%d, want exactly one bound to the original conversation", messageCount)
	}
	var state, storedMessageID string
	if err := f.store.pool.QueryRow(ctx, `SELECT state,published_message_id FROM task_terminal_delivery_outbox WHERE id=$1`,
		claim.DeliveryID).Scan(&state, &storedMessageID); err != nil {
		t.Fatal(err)
	}
	if state != TaskTerminalDeliveryStatePublished || storedMessageID != messageID {
		t.Fatalf("waiting delivery state=%q message=%q, want published message %q", state, storedMessageID, messageID)
	}
}

func TestPostgresTaskTerminalDeliveryRecoveryDiagnosisReachesModelFacts(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET model_id='p1234567' WHERE id=$1`, f.request.Fence.TaskID); err != nil {
		t.Fatalf("set Task model identity for recovery diagnosis: %v", err)
	}
	sequence := commitTaskTerminalDeliveryWaiting(t, f, "dsh_recovery_unresolved", []byte(`{"reason":"dsh_recovery_unresolved","detail":{"phase":"recovery","failure_stage":"model_response_validation","failure_category":"unauthorized_tool","failure_reason":"tool_not_in_task_allowlist","tool_name":"morde_mcp_private","tool_call_id":"call_private","tool_dispatched":false}}`))
	claims, err := f.store.ClaimTaskTerminalDeliverySources(ctx, "terminal-diagnosis-reader", 30*time.Second, 1)
	if err != nil || len(claims) != 1 || claims[0].SourceEventSequence != sequence {
		t.Fatalf("claim recovery diagnosis source: claims=%+v err=%v", claims, err)
	}
	facts, found, err := f.store.ReadTaskTerminalDeliverySourceFacts(ctx, claims[0])
	if err != nil || !found || !app.ValidTaskTerminalDeliverySourceFacts(claims[0], facts) {
		t.Fatalf("read verified recovery diagnosis facts: found=%t facts=%+v err=%v", found, facts, err)
	}
	const expected = "模型提出了当前任务未授权的工具操作，系统在派发前已拦截。"
	if facts.RecoveryDiagnosis != expected {
		t.Fatalf("recovery diagnosis facts = %q, want %q", facts.RecoveryDiagnosis, expected)
	}
	request, err := app.BuildTaskTerminalDeliveryDSHModelRequest(facts.ModelID, facts)
	if err != nil {
		t.Fatalf("build safe recovery explanation request: %v", err)
	}
	if !bytes.Contains(request, []byte(expected)) {
		t.Fatalf("terminal explanation request omitted the diagnosis: %s", request)
	}
	for _, internalValue := range []string{"unauthorized_tool", "tool_not_in_task_allowlist", "morde_mcp_private", "call_private"} {
		if bytes.Contains(request, []byte(internalValue)) {
			t.Fatalf("terminal explanation request exposed internal value %q: %s", internalValue, request)
		}
	}
}

func TestPostgresTaskTerminalDeliveryStillInvalidatesAfterInstallationRevocation(t *testing.T) {
	f := newAgentLifecycleFixture(t)
	ctx := context.Background()
	commitTaskTerminalDeliveryWaiting(t, f, "dsh_recovery_unresolved")
	claims, err := f.store.ClaimTaskTerminalDeliverySources(ctx, "terminal-revoked-installation", 30*time.Second, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim waiting delivery before revocation: claims=%+v err=%v", claims, err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE client_installations SET revoked_at=clock_timestamp()
WHERE id=$1 AND owner_id=$2`, f.principal.ClientInstallationID, f.principal.UserID); err != nil {
		t.Fatalf("revoke the task's client installation: %v", err)
	}
	if _, found, err := f.store.ReadTaskTerminalDeliverySourceFacts(ctx, claims[0]); err != nil || found {
		t.Fatalf("delivery remained authorized after installation revocation: found=%t err=%v", found, err)
	}
	var state string
	if err := f.store.pool.QueryRow(ctx, `SELECT state FROM task_terminal_delivery_outbox WHERE id=$1`, claims[0].DeliveryID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != TaskTerminalDeliveryStateInvalidated {
		t.Fatalf("delivery state after installation revocation=%q, want invalidated", state)
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
