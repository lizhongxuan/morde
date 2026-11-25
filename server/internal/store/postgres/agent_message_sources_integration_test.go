package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"morde/server/internal/app"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type c01PostgresAgentHostFenceReader struct{}

func (c01PostgresAgentHostFenceReader) ReadTaskFence(context.Context, string, string) (app.AgentHostFence, error) {
	return app.AgentHostFence{}, nil
}

type c01PostgresSandboxRouter struct{}

func (c01PostgresSandboxRouter) For(string) (app.SandboxClient, error) { return nil, nil }
func (c01PostgresSandboxRouter) Profiles() []string                    { return []string{"default"} }

type c01PostgresWorkspaceBlocks struct{}

func (c01PostgresWorkspaceBlocks) Open(string, string) (io.ReadCloser, error) { return nil, nil }
func (c01PostgresWorkspaceBlocks) PutBlock(string, string, io.Reader) (int64, error) {
	return 0, nil
}
func (c01PostgresWorkspaceBlocks) ReservationBytes(size int64) (int64, error) { return size, nil }
func (c01PostgresWorkspaceBlocks) BlockAllocatedBytes(string, string) (int64, error) {
	return 0, nil
}

func TestPostgresAgentMessageDeliveryReceiptsAndBoundedSourcePages(t *testing.T) {
	store, credentials, _, owner := clientLoginFixture(t)
	ctx := context.Background()
	tokenID := testSessionTokenID(owner.ID)
	secretHash := sha256.Sum256([]byte("integration-session:" + tokenID))
	expiresAt := time.Now().UTC().Add(time.Hour)
	if _, err := store.CreateToken(ctx, identity.Token{ID: tokenID, OwnerID: owner.ID, Kind: identity.KindSession,
		SecretHash: secretHash[:], ExpiresAt: expiresAt}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, owner.ID); err != nil {
		t.Fatal(err)
	}
	submission := newTestTaskSubmission(t, owner.ID, "", "r21-receipt-task", "initial source message")
	submission.Task.ExecutionLocation = "server"
	submission.Task.Authorization = task.Authorization{Kind: string(identity.KindSession), TokenID: tokenID,
		Version: 1, ExpiresAt: &expiresAt}
	created, err := store.SubmitTask(ctx, submission)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimNextTask(ctx, "r21-receipt-worker", 20*time.Second)
	if err != nil || !ok || claimed.ID != created.Task.ID {
		t.Fatalf("claim Task: claimed=%t id=%q err=%v", ok, claimed.ID, err)
	}
	supervisorKey := make([]byte, 32)
	supervisorKey[0] = 1
	authenticator, err := app.NewAgentStopAuthenticator("r21-receipt-supervisor", supervisorKey)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 1
	started, replayed, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: owner.ID, InitiatorID: claimed.InitiatorID, AgentInstanceID: "agent-r21-receipt",
		AgentSessionID: "session-r21-receipt", SupervisorID: "r21-receipt-supervisor", LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, AuthorizationVersion: 1, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1",
	}, SupervisorNonce: nonce, LeaseDuration: 10 * time.Second})
	if err != nil || replayed {
		t.Fatalf("start Agent: replayed=%t err=%v", replayed, err)
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
		LeaseVersion: started.Fence.LeaseVersion, EngineVersion: started.Fence.EngineVersion, ProfileVersion: started.Fence.ProfileVersion}

	followup := seedLegacyPendingTaskMessageForTest(t, ctx, store, owner.ID, created.Conversation.ID, created.Task.ID, "followup source message", nil)
	payload, err := json.Marshal(map[string]any{"header": map[string]string{"id": fence.AgentSessionID}, "from_seq": 0,
		"events": []any{
			map[string]any{"seq": 0, "type": "user/message", "data": map[string]any{"id": created.Message.ID, "role": "user", "content": created.Message.Content}},
			map[string]any{"seq": 1, "type": "user/message", "data": map[string]any{"id": followup.ID, "role": "user", "content": followup.Content}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	payloadDigest := sha256.Sum256(payload)
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: fence, From: 0, Through: 1, Digest: payloadDigest[:], Payload: payload}); err != nil {
		t.Fatalf("save initial and followup checkpoint receipts: %v", err)
	}
	for _, testCase := range []struct{ id, content string }{
		{created.Message.ID, created.Message.Content}, {followup.ID, followup.Content},
	} {
		receipt, err := checkpoints.ReadMessageSourceReference(ctx, fence, testCase.id)
		if err != nil || receipt.RepresentationKind != "inline" || receipt.AppliedCheckpointThrough != 1 ||
			receipt.Envelope.TaskID != claimed.ID || receipt.Envelope.MessageID != testCase.id || receipt.Envelope.ContentBytes != int64(len(testCase.content)) {
			t.Fatalf("read inline receipt for %q: receipt=%+v err=%v", testCase.id, receipt, err)
		}
		sourceDigest, err := hex.DecodeString(receipt.Envelope.SourceSHA256)
		if err != nil {
			t.Fatal(err)
		}
		page, err := checkpoints.ReadMessageSourcePage(ctx, fence, testCase.id, sourceDigest, 0)
		if err != nil || page.Content != testCase.content || page.NextRuneOffset != receipt.Envelope.TotalRunes {
			t.Fatalf("read inline source page for %q: page=%+v err=%v", testCase.id, page, err)
		}
		if _, err := checkpoints.ReadMessageSourcePage(ctx, fence, testCase.id, make([]byte, sha256.Size), 0); err == nil {
			t.Fatalf("source page for %q accepted a different source digest", testCase.id)
		}
	}

	longContent := "HEAD-ANCHOR-0001" + strings.Repeat("用户资料🙂", 1<<15-2) +
		"MID-ANCHOR-00001" + strings.Repeat("用户资料🙂", 1<<15-1) + "TAIL-ANCHOR-0001!"
	if len(longContent) != (1<<20)+1 {
		t.Fatalf("long source fixture bytes=%d, want %d", len(longContent), (1<<20)+1)
	}
	longMessage := seedLegacyPendingTaskMessageForTest(t, ctx, store, owner.ID, created.Conversation.ID, created.Task.ID, longContent, nil)
	pending, err := checkpoints.PendingMessages(ctx, fence)
	if err != nil || len(pending) != 1 || pending[0].ID != longMessage.ID || pending[0].Reference == nil || pending[0].RepresentationKind != "reference" {
		t.Fatalf("read long pending message reference: pending=%+v err=%v", pending, err)
	}
	marker, err := app.FormatAgentMessageReferenceEnvelope(*pending[0].Reference)
	if err != nil {
		t.Fatal(err)
	}
	pending[0].Content = marker
	pending[0].Sequence = 2
	checkpointPayload := []byte("durable R21 test checkpoint")
	checkpointDigest := sha256.Sum256(checkpointPayload)
	if _, err := store.SaveAgentCheckpoint(ctx, app.StoredAgentCheckpoint{Fence: fence, From: 2, Through: 2,
		Digest: checkpointDigest[:], Ciphertext: []byte("sealed checkpoint"), ConversationMessages: pending}); err != nil {
		t.Fatalf("save source reference receipt: %v", err)
	}
	reference, err := checkpoints.ReadMessageSourceReference(ctx, fence, longMessage.ID)
	if err != nil || reference.RepresentationKind != "reference" || reference.AppliedCheckpointThrough != 2 ||
		reference.Envelope.RepresentationSHA256 != pending[0].Reference.RepresentationSHA256 {
		t.Fatalf("read source reference receipt: receipt=%+v err=%v", reference, err)
	}
	sourceDigest, err := hex.DecodeString(reference.Envelope.SourceSHA256)
	if err != nil {
		t.Fatal(err)
	}
	var reconstructed strings.Builder
	offset := int64(0)
	for offset < reference.Envelope.TotalRunes {
		page, err := checkpoints.ReadMessageSourcePage(ctx, fence, longMessage.ID, sourceDigest, offset)
		if err != nil || page.RuneOffset != offset || page.NextRuneOffset <= offset || page.PageRuneOffset != offset {
			t.Fatalf("read long source page at %d: page=%+v err=%v", offset, page, err)
		}
		reconstructed.WriteString(page.Content)
		offset = page.NextRuneOffset
	}
	if reconstructed.String() != longContent {
		t.Fatal("bounded source pages did not reconstruct the exact original message")
	}

	// Exercise the public Broker path against the same PostgreSQL-backed receipt
	// and page reader. The model-facing read tool must advance in canonical rune
	// order, validate each chunk hash, and reconstruct the exact long message.
	runtimeControl, err := app.NewRuntimeControl(store, c01PostgresSandboxRouter{}, sealer)
	if err != nil {
		t.Fatal(err)
	}
	workspaceSync, err := app.NewWorkspaceSyncService(store, c01PostgresWorkspaceBlocks{}, 1<<20, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	runtimeTools, err := app.NewRuntimeToolExecutor(store, credentials, c01PostgresAgentHostFenceReader{}, runtimeControl, workspaceSync)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := app.NewCloudDSHToolBroker(store, runtimeTools, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := broker.SetAgentMessageSourceReader(checkpoints); err != nil {
		t.Fatal(err)
	}
	toolDetails, err := store.GetTask(ctx, claimed.ID, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	toolTask := toolDetails.Task
	toolTask.ConversationContext = claimed.ConversationContext
	modelFence := &app.ModelTaskFence{TaskID: toolTask.ID, OwnerID: toolTask.OwnerID, InitiatorID: toolTask.InitiatorID,
		LeaseOwner: toolTask.Lease.Owner, LeaseVersion: toolTask.Lease.Version,
		AgentInstanceID: toolTask.AgentInstanceID, AgentGeneration: toolTask.AgentGeneration,
		AgentSessionID: toolTask.AgentSessionID, AgentRootSessionID: toolTask.AgentSessionID,
		AuthorizationVersion: toolTask.Authorization.Version, ThroughSequence: 2,
		EngineVersion: toolTask.AgentEngineVersion, ProfileVersion: toolTask.AgentProfileVersion}
	var brokerReconstructed strings.Builder
	offset = 0
	toolCalls := 0
	for offset < reference.Envelope.TotalRunes {
		arguments, err := json.Marshal(map[string]any{"message_id": longMessage.ID,
			"sha256": reference.Envelope.SourceSHA256, "rune_offset": offset})
		if err != nil {
			t.Fatal(err)
		}
		result, err := broker.Execute(ctx, toolTask, app.ModelToolCall{CallID: "r21_long_read_" + strconv.Itoa(toolCalls),
			Name: "morde_read_current_conversation_message", Arguments: string(arguments), TaskFence: modelFence})
		if err != nil || result.Status != app.AgentToolSucceeded {
			t.Fatalf("Broker read long source at rune %d: result=%+v err=%v", offset, result, err)
		}
		var chunk struct {
			MessageID      string `json:"message_id"`
			Sequence       int64  `json:"sequence"`
			Page           int    `json:"page"`
			PageCount      int    `json:"page_count"`
			RuneOffset     int64  `json:"rune_offset"`
			NextRuneOffset int64  `json:"next_rune_offset"`
			TotalRunes     int64  `json:"total_runes"`
			SHA256         string `json:"sha256"`
			ChunkSHA256    string `json:"chunk_sha256"`
			Content        string `json:"content"`
			Complete       bool   `json:"complete"`
		}
		if err := json.Unmarshal([]byte(result.Output), &chunk); err != nil {
			t.Fatalf("decode Broker source chunk at rune %d: %v", offset, err)
		}
		chunkDigest := sha256.Sum256([]byte(chunk.Content))
		if chunk.MessageID != longMessage.ID || chunk.Sequence != reference.Envelope.MessageSequence ||
			chunk.Page < 1 || chunk.Page > len(reference.Envelope.Pages) || chunk.PageCount != len(reference.Envelope.Pages) ||
			chunk.RuneOffset != offset || chunk.NextRuneOffset <= offset || chunk.TotalRunes != reference.Envelope.TotalRunes ||
			chunk.NextRuneOffset-offset != int64(utf8.RuneCountInString(chunk.Content)) || !utf8.ValidString(chunk.Content) ||
			chunk.SHA256 != reference.Envelope.SourceSHA256 || chunk.ChunkSHA256 != hex.EncodeToString(chunkDigest[:]) ||
			chunk.Complete != (chunk.NextRuneOffset == reference.Envelope.TotalRunes) || !json.Valid([]byte(result.Output)) {
			t.Fatalf("Broker returned an invalid bounded source chunk at rune %d: %+v", offset, chunk)
		}
		brokerReconstructed.WriteString(chunk.Content)
		offset = chunk.NextRuneOffset
		toolCalls++
		if toolCalls > 100 {
			t.Fatalf("Broker needed more than the Task tool-call budget to read the source: %d", toolCalls)
		}
	}
	if brokerReconstructed.String() != longContent {
		t.Fatal("Broker source reads did not reconstruct the exact original PostgreSQL message")
	}
	for _, anchor := range []string{"HEAD-ANCHOR-0001", "MID-ANCHOR-00001", "TAIL-ANCHOR-0001"} {
		if !strings.Contains(brokerReconstructed.String(), anchor) {
			t.Fatalf("Broker reconstruction omitted a %q message anchor", anchor)
		}
	}

	staleFence := fence
	staleFence.AgentGeneration++
	if _, err := checkpoints.ReadMessageSourceReference(ctx, staleFence, longMessage.ID); err == nil {
		t.Fatal("stale Agent generation could read a source reference")
	}
}
