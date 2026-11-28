package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type agentMessageReceiptFixture struct {
	store           *Store
	ownerID         string
	conversationID  string
	taskID          string
	service         *app.AgentCheckpointService
	fence           app.AgentCheckpointFence
	instances       *app.AgentInstanceService
	agentLease      app.AgentInstanceLease
	supervisorKey   []byte
	supervisorNonce []byte
}

func newAgentMessageReceiptFixture(t *testing.T) agentMessageReceiptFixture {
	t.Helper()
	store, credentials, clients, owner := clientLoginFixture(t)
	ctx := context.Background()
	login, err := clientLoginForTest(ctx, clients, owner.Name, "client-password-123", "ios", "phone", "", "r21-receipt-login", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := credentials.VerifyClient(ctx, login.ClientCredential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Hour)
	submission := newTestTaskSubmission(t, owner.ID, "", "r21-receipt-task", "do work")
	submission.Task.ExecutionLocation = "server"
	submission.Task.Authorization = task.Authorization{Kind: string(identity.KindClient), TokenID: principal.TokenID,
		ClientInstallationID: principal.ClientInstallationID, Version: 1, ExpiresAt: &expiresAt}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "r21-receipt-worker", time.Minute)
	if err != nil || !found || claimed.ID != created.Task.ID {
		t.Fatalf("claim receipt task: found=%t task=%q err=%v", found, claimed.ID, err)
	}
	key := make([]byte, 32)
	key[0] = 7
	authenticator, err := app.NewAgentStopAuthenticator("r21-receipt-supervisor", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 9
	started, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: owner.ID, InitiatorID: claimed.InitiatorID, AgentInstanceID: "r21_receipt_agent",
		AgentSessionID: "r21_receipt_session", SupervisorID: "r21-receipt-supervisor", LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, AuthorizationVersion: claimed.Authorization.Version,
		EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1",
	}, SupervisorNonce: nonce, LeaseDuration: app.MaxAgentLease})
	if err != nil || replayed {
		t.Fatalf("start receipt Agent: replayed=%t err=%v", replayed, err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	fence := app.AgentCheckpointFence{TaskID: claimed.ID, OwnerID: owner.ID, InitiatorID: claimed.InitiatorID,
		AgentInstanceID: started.Fence.AgentInstanceID, AgentGeneration: started.Fence.AgentGeneration,
		AgentSessionID: started.Fence.AgentSessionID, AgentRootSessionID: started.Fence.AgentSessionID,
		AuthorizationVersion: started.Fence.AuthorizationVersion, LeaseOwner: started.Fence.LeaseOwner,
		LeaseVersion: started.Fence.LeaseVersion, EngineVersion: started.Fence.EngineVersion,
		ProfileVersion: started.Fence.ProfileVersion}
	return agentMessageReceiptFixture{store: store, ownerID: owner.ID, conversationID: created.Conversation.ID,
		taskID: created.Task.ID, service: checkpoints, fence: fence, instances: instances, agentLease: started,
		supervisorKey: key, supervisorNonce: nonce}
}

func TestPostgresAgentMessageDeliveryReceiptAtomicApplyReplayAndPageRead(t *testing.T) {
	fixture := newAgentMessageReceiptFixture(t)
	ctx := context.Background()
	content := strings.Repeat("a", 1<<20)
	message := seedLegacyPendingTaskMessageForTest(t, ctx, fixture.store, fixture.ownerID, fixture.conversationID, fixture.taskID, content, nil)
	pending, err := fixture.service.PendingMessages(ctx, fixture.fence)
	if err != nil || len(pending) != 1 || pending[0].ID != message.ID || pending[0].Reference == nil ||
		pending[0].Content != content || pending[0].Reference.AgentGeneration != fixture.fence.AgentGeneration {
		t.Fatalf("pending long message reference = %+v err=%v", pending, err)
	}
	var beforeSource []byte
	var beforeApplied pgtype.Int8
	if err := fixture.store.pool.QueryRow(ctx, `SELECT source_sha256,applied_checkpoint_through_seq
FROM task_agent_message_deliveries WHERE message_id=$1`, message.ID).Scan(&beforeSource, &beforeApplied); err != nil {
		t.Fatal(err)
	}
	if beforeSource != nil || beforeApplied.Valid {
		t.Fatalf("receipt was applied before a root checkpoint: source=%x applied=%v", beforeSource, beforeApplied)
	}
	marker, err := app.FormatAgentMessageReferenceEnvelope(*pending[0].Reference)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"header": map[string]string{"id": fixture.fence.AgentSessionID}, "from_seq": 0,
		"events": []any{map[string]any{"seq": 0, "type": "user/message", "data": map[string]any{
			"id": message.ID, "role": "user", "content": marker,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	command := app.AgentCheckpointCommand{Fence: fixture.fence, From: 0, Through: 0, Digest: digest[:], Payload: payload}
	if replayed, err := fixture.service.Save(ctx, command); err != nil || replayed {
		t.Fatalf("apply source-reference checkpoint: replayed=%t err=%v", replayed, err)
	}
	var status, kind string
	var sourceDigest, manifestDigest, representationDigest []byte
	var appliedThrough, appliedGeneration int64
	if err := fixture.store.pool.QueryRow(ctx, `SELECT message.delivery_status,delivery.representation_kind,delivery.source_sha256,
 delivery.manifest_sha256,delivery.representation_sha256,delivery.applied_checkpoint_through_seq,delivery.applied_agent_generation
FROM conversation_messages message JOIN task_agent_message_deliveries delivery ON delivery.message_id=message.id
WHERE message.id=$1`, message.ID).Scan(&status, &kind, &sourceDigest, &manifestDigest, &representationDigest,
		&appliedThrough, &appliedGeneration); err != nil {
		t.Fatal(err)
	}
	expectedSource := sha256.Sum256([]byte(content))
	if status != "applied" || kind != "reference" || !bytes.Equal(sourceDigest, expectedSource[:]) ||
		len(manifestDigest) != sha256.Size || len(representationDigest) != sha256.Size || appliedThrough != 0 ||
		appliedGeneration != fixture.fence.AgentGeneration {
		t.Fatalf("checkpoint receipt was not applied atomically: status=%s kind=%s source=%x applied=%d generation=%d",
			status, kind, sourceDigest, appliedThrough, appliedGeneration)
	}
	if replayed, err := fixture.service.Save(ctx, command); err != nil || !replayed {
		t.Fatalf("replay applied receipt checkpoint: replayed=%t err=%v", replayed, err)
	}
	var replayApplied int64
	if err := fixture.store.pool.QueryRow(ctx, `SELECT applied_checkpoint_through_seq FROM task_agent_message_deliveries WHERE message_id=$1`, message.ID).Scan(&replayApplied); err != nil || replayApplied != appliedThrough {
		t.Fatalf("checkpoint replay changed applied receipt: through=%d err=%v", replayApplied, err)
	}
	reference, err := fixture.service.ReadMessageSourceReference(ctx, fixture.fence, message.ID)
	if err != nil || reference.AppliedCheckpointThrough != appliedThrough || reference.AuthorizedAgentGeneration != fixture.fence.AgentGeneration ||
		reference.Envelope.MessageID != message.ID || reference.Envelope.SourceSHA256 != hex.EncodeToString(expectedSource[:]) {
		t.Fatalf("read applied source reference: %+v err=%v", reference, err)
	}
	page, err := fixture.service.ReadMessageSourcePage(ctx, fixture.fence, message.ID, expectedSource[:], 123)
	if err != nil || page.RuneOffset != 123 || page.PageRuneOffset != 0 || page.PageNextRuneOffset <= 123 ||
		page.NextRuneOffset != page.PageNextRuneOffset || page.Content != content[123:int(page.NextRuneOffset)] {
		t.Fatalf("read verified source page fragment: %+v err=%v", page, err)
	}
	if _, err := fixture.instances.ConfirmStopped(ctx, signAgentLifecycleReceipt(t, fixture.supervisorKey,
		fixture.agentLease.Fence, fixture.supervisorNonce)); err != nil {
		t.Fatalf("confirm receipt Agent stopped: %v", err)
	}
	if _, err := fixture.store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, fixture.taskID); err != nil {
		t.Fatal(err)
	}
	recoveredTask, found, err := fixture.store.ClaimNextTask(ctx, "r21-receipt-recovery-worker", time.Minute)
	if err != nil || !found || recoveredTask.ID != fixture.taskID {
		t.Fatalf("claim receipt task for recovery: found=%t task=%q err=%v", found, recoveredTask.ID, err)
	}
	recoveredNonce := make([]byte, 32)
	recoveredNonce[0] = 11
	recoveredLease, replayed, err := fixture.instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: recoveredTask.ID, OwnerID: fixture.ownerID, InitiatorID: recoveredTask.InitiatorID,
		AgentInstanceID: "r21_receipt_agent_recovered", AgentSessionID: fixture.fence.AgentSessionID,
		SupervisorID: "r21-receipt-supervisor", LeaseOwner: recoveredTask.Lease.Owner, LeaseVersion: recoveredTask.Lease.Version,
		AuthorizationVersion: recoveredTask.Authorization.Version, EngineVersion: fixture.fence.EngineVersion,
		ProfileVersion: fixture.fence.ProfileVersion,
	}, SupervisorNonce: recoveredNonce, LeaseDuration: app.MaxAgentLease})
	if err != nil || replayed || recoveredLease.Fence.AgentGeneration != fixture.fence.AgentGeneration+1 {
		t.Fatalf("recover receipt Agent: lease=%+v replayed=%t err=%v", recoveredLease, replayed, err)
	}
	recoveredFence := app.AgentCheckpointFence{TaskID: fixture.taskID, OwnerID: fixture.ownerID, InitiatorID: recoveredTask.InitiatorID,
		AgentInstanceID: recoveredLease.Fence.AgentInstanceID, AgentGeneration: recoveredLease.Fence.AgentGeneration,
		AgentSessionID: recoveredLease.Fence.AgentSessionID, AgentRootSessionID: recoveredLease.Fence.AgentSessionID,
		AuthorizationVersion: recoveredLease.Fence.AuthorizationVersion, LeaseOwner: recoveredLease.Fence.LeaseOwner,
		LeaseVersion: recoveredLease.Fence.LeaseVersion, EngineVersion: recoveredLease.Fence.EngineVersion,
		ProfileVersion: recoveredLease.Fence.ProfileVersion}
	recoveredReference, err := fixture.service.ReadMessageSourceReference(ctx, recoveredFence, message.ID)
	if err != nil || recoveredReference.Envelope.AgentGeneration != fixture.fence.AgentGeneration ||
		recoveredReference.AuthorizedAgentGeneration != recoveredLease.Fence.AgentGeneration {
		t.Fatalf("recovered source receipt did not preserve accepted generation: %+v err=%v", recoveredReference, err)
	}
	var reboundGeneration, receiptGeneration int64
	if err := fixture.store.pool.QueryRow(ctx, `SELECT agent_generation,receipt_agent_generation,applied_agent_generation
FROM task_agent_message_deliveries WHERE message_id=$1`, message.ID).Scan(&reboundGeneration, &receiptGeneration, &appliedGeneration); err != nil {
		t.Fatal(err)
	}
	if reboundGeneration != recoveredLease.Fence.AgentGeneration || receiptGeneration != fixture.fence.AgentGeneration ||
		appliedGeneration != fixture.fence.AgentGeneration {
		t.Fatalf("generation rebind lost original receipt proof: current=%d receipt=%d applied=%d", reboundGeneration,
			receiptGeneration, appliedGeneration)
	}
}

func TestPostgresAgentMessageDeliveryReceiptRejectsMismatchedReference(t *testing.T) {
	fixture := newAgentMessageReceiptFixture(t)
	ctx := context.Background()
	content := strings.Repeat("b", app.MaxConversationAgentInputBytes+64)
	message := seedLegacyPendingTaskMessageForTest(t, ctx, fixture.store, fixture.ownerID, fixture.conversationID, fixture.taskID, content, nil)
	pending, err := fixture.service.PendingMessages(ctx, fixture.fence)
	if err != nil || len(pending) != 1 || pending[0].Reference == nil {
		t.Fatalf("pending long message = %+v err=%v", pending, err)
	}
	marker, err := app.FormatAgentMessageReferenceEnvelope(*pending[0].Reference)
	if err != nil {
		t.Fatal(err)
	}
	marker = strings.Replace(marker, "source_sha256="+pending[0].Reference.SourceSHA256,
		"source_sha256="+strings.Repeat("0", 64), 1)
	payload, err := json.Marshal(map[string]any{
		"header": map[string]string{"id": fixture.fence.AgentSessionID}, "from_seq": 0,
		"events": []any{map[string]any{"seq": 0, "type": "user/message", "data": map[string]any{
			"id": message.ID, "role": "user", "content": marker,
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	command := app.AgentCheckpointCommand{Fence: fixture.fence, From: 0, Through: 0, Digest: digest[:], Payload: payload}
	if _, err := fixture.service.Save(ctx, command); err == nil {
		t.Fatal("mismatched reference header was accepted")
	}
	var status string
	var sourceDigest []byte
	var appliedCheckpoint pgtype.Int8
	var checkpointCount int
	if err := fixture.store.pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, message.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.pool.QueryRow(ctx, `SELECT source_sha256,applied_checkpoint_through_seq FROM task_agent_message_deliveries WHERE message_id=$1`, message.ID).
		Scan(&sourceDigest, &appliedCheckpoint); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_agent_checkpoints WHERE task_id=$1 AND agent_session_id=$2`,
		fixture.taskID, fixture.fence.AgentSessionID).Scan(&checkpointCount); err != nil {
		t.Fatal(err)
	}
	if status != "unknown" || sourceDigest != nil || appliedCheckpoint.Valid || checkpointCount != 0 {
		t.Fatalf("mismatch left receipt/checkpoint evidence: status=%s source=%x applied=%v checkpoints=%d",
			status, sourceDigest, appliedCheckpoint, checkpointCount)
	}
}

func TestPostgresAgentMessageDeliveryReceiptSurvivesSpliceClaim(t *testing.T) {
	fixture := newAgentMessageReceiptFixture(t)
	ctx := context.Background()
	content := strings.Repeat("follow-up source ", app.MaxConversationAgentInputBytes/len("follow-up source ")+2)
	message := seedLegacyPendingTaskMessageForTest(t, ctx, fixture.store, fixture.ownerID, fixture.conversationID, fixture.taskID, content, nil)
	pending, err := fixture.service.PendingMessages(ctx, fixture.fence)
	if err != nil || len(pending) != 1 || pending[0].ID != message.ID || pending[0].Reference == nil {
		t.Fatalf("pending follow-up source = %+v err=%v", pending, err)
	}
	marker, err := app.FormatAgentMessageReferenceEnvelope(*pending[0].Reference)
	if err != nil {
		t.Fatal(err)
	}
	makeCheckpoint := func(from, through int64, event any) app.AgentCheckpointCommand {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"header": map[string]string{"id": fixture.fence.AgentSessionID}, "from_seq": from,
			"events": []any{event},
		})
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		return app.AgentCheckpointCommand{Fence: fixture.fence, From: from, Through: through, Digest: digest[:], Payload: payload}
	}
	splice := map[string]any{"seq": int64(0), "type": "agent/inbox/spliced", "data": map[string]any{
		"target": "next-turn", "start": 0, "inserted": []any{map[string]any{
			"id": message.ID, "role": "user", "content": marker, "source": map[string]string{"kind": "user"},
		}},
	}}
	if replayed, err := fixture.service.Save(ctx, makeCheckpoint(0, 0, splice)); err != nil || replayed {
		t.Fatalf("save follow-up enqueue checkpoint: replayed=%t err=%v", replayed, err)
	}
	queued, err := fixture.service.ReadMessageSourceReference(ctx, fixture.fence, message.ID)
	if err != nil || queued.AppliedCheckpointThrough != 0 {
		t.Fatalf("read enqueued follow-up receipt: receipt=%+v err=%v", queued, err)
	}
	claimed := map[string]any{"seq": int64(1), "type": "user/message", "data": map[string]any{
		"id": message.ID, "role": "user", "content": marker,
	}}
	if replayed, err := fixture.service.Save(ctx, makeCheckpoint(1, 1, claimed)); err != nil || replayed {
		t.Fatalf("save follow-up claim checkpoint: replayed=%t err=%v", replayed, err)
	}
	var status string
	var enqueuedCheckpoint, claimedCheckpoint, appliedCheckpoint int64
	if err := fixture.store.pool.QueryRow(ctx, `SELECT message.delivery_status,delivery.enqueued_checkpoint_through_seq,
 delivery.claimed_checkpoint_through_seq,delivery.applied_checkpoint_through_seq
FROM conversation_messages message JOIN task_agent_message_deliveries delivery ON delivery.message_id=message.id
WHERE message.id=$1`, message.ID).Scan(&status, &enqueuedCheckpoint, &claimedCheckpoint, &appliedCheckpoint); err != nil {
		t.Fatal(err)
	}
	if status != "applied" || enqueuedCheckpoint != 0 || claimedCheckpoint != 1 || appliedCheckpoint != 0 {
		t.Fatalf("follow-up receipt proof = status %q enqueue %d claim %d applied %d", status,
			enqueuedCheckpoint, claimedCheckpoint, appliedCheckpoint)
	}
	readAfterClaim, err := fixture.service.ReadMessageSourceReference(ctx, fixture.fence, message.ID)
	if err != nil || readAfterClaim.AppliedCheckpointThrough != enqueuedCheckpoint ||
		readAfterClaim.Envelope.RepresentationSHA256 != pending[0].Reference.RepresentationSHA256 {
		t.Fatalf("read follow-up receipt after claim: receipt=%+v err=%v", readAfterClaim, err)
	}
}
