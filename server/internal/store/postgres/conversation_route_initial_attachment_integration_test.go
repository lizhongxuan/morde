package postgres

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/task"
	"morde/server/internal/worker"
)

type failFirstInitialCheckpoint struct {
	worker.CloudDSHCheckpoints
	mu     sync.Mutex
	failed bool
}

func (c *failFirstInitialCheckpoint) Save(ctx context.Context, value app.AgentCheckpointCommand) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.failed {
		c.failed = true
		return false, errors.New("synthetic local checkpoint failure")
	}
	return c.CloudDSHCheckpoints.Save(ctx, value)
}

type initialAttachmentModel struct {
	mu    sync.Mutex
	calls []app.ModelCall
}

func (m *initialAttachmentModel) Complete(_ context.Context, value app.ModelCall) (app.ModelResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, value)
	return app.ModelResponse{Text: "Attachment understood.", StopReason: "stop", Usage: app.ModelUsage{Known: true, InputTokens: 8, OutputTokens: 3}}, nil
}

func (*initialAttachmentModel) TaskToolCapability(context.Context, task.Task) (bool, error) {
	return true, nil
}

type initialAttachmentReader struct{}

func (initialAttachmentReader) Definitions(context.Context, task.Task) ([]app.AgentToolDefinition, error) {
	return []app.AgentToolDefinition{{ModelTool: app.ModelTool{
		Name: "morde_read_task_attachment", Description: "Read only files attached to the current personal Task. Synthetic test reader.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"attachment_id":{"type":"string","pattern":"^(?:pa|pca)_[a-f0-9]{32}$"}},"required":["attachment_id"],"additionalProperties":false}`),
	}, RequiredScope: identity.ScopeTaskRead}}, nil
}

func (initialAttachmentReader) Execute(context.Context, task.Task, app.ModelToolCall) (app.AgentToolResult, error) {
	return app.AgentToolResult{}, errors.New("synthetic reader is not called by this test")
}

func TestConversationRouteInitialAttachmentIsDeliveredOnceAfterStartupRetry(t *testing.T) {
	for _, test := range []struct{ name, content string }{
		{"long text and attachment", "Please inspect: " + strings.Repeat("a", 40<<10)},
		{"attachment only", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRouteActionsTestFixture(t)
			ctx := context.Background()
			principal := identity.Principal{UserID: f.ownerID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: f.authorization.TokenID}
			createPersonalLibraryItemForAttachmentTest(t, f.store, principal, "initial-attachment-item")
			data := []byte("synthetic local attachment")
			upload := newPersonalAttachmentUploadForTest(t, f.ownerID, "initial-attachment-item", 1, data)
			created, err := f.store.CreatePersonalAttachmentUpload(ctx, principal, upload, "initial-upload-001")
			if err != nil {
				t.Fatal(err)
			}
			reservation, err := f.store.ReservePersonalAttachment(ctx, principal, created.UploadID, int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.ActivatePersonalAttachmentUpload(ctx, principal, created.UploadID, reservation); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(data)
			chunk := personal.AttachmentChunk{UploadID: created.UploadID, Sequence: 0, SizeBytes: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}
			if _, err := f.store.PutPersonalAttachmentChunk(ctx, principal, chunk, bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
			attachment, err := f.store.CommitPersonalAttachment(ctx, principal, created.UploadID, "initial-commit-001")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.WithTx(ctx, func(tx pgx.Tx) error {
				return insertMessage(ctx, tx, conversation.Message{ID: mustID(t, domain.NewMessageID), ConversationID: f.conversation.ID,
					OwnerID: f.ownerID, ActorID: f.ownerID, Role: conversation.RoleAssistant, SourceKind: "history_import", Content: "The earlier report is for the board."})
			}); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte("initial-message-001"))
			message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
				Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.ownerID, ActorID: f.ownerID,
					ConversationID: f.conversation.ID, Role: conversation.RoleUser, Content: test.content},
				ServiceInstanceID: f.instanceID, ClientID: "initial-message-001", RequestHash: hash[:], Authorization: f.authorization,
				Input: app.ConversationRoutingInput{ModelID: "p1234567", Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000},
					ClientAttachmentIDs: []string{"initial-attachment-001"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.store.AssociateConversationMessageAttachment(ctx, principal, f.conversation.ID, message.ID,
				"initial-attachment-001", attachment.AttachmentID, f.instanceID); err != nil {
				t.Fatal(err)
			}
			claim, found, err := f.store.ClaimConversationRoute(ctx, "initial-route-worker", time.Minute)
			if err != nil || !found || claim.Message.ID != message.ID {
				t.Fatalf("route claim: found=%t err=%v", found, err)
			}
			snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.Context.ModelID = "p1234567"
			if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
				t.Fatal(err)
			}
			var taskID, delivery string
			if err := f.store.pool.QueryRow(ctx, `SELECT task_id,delivery_status FROM conversation_messages WHERE id=$1`, message.ID).Scan(&taskID, &delivery); err != nil || delivery != "pending" {
				t.Fatalf("initial message should remain pending: delivery=%q err=%v", delivery, err)
			}

			node, err := exec.LookPath("node")
			if err != nil {
				t.Skip("local Node is unavailable")
			}
			node, err = filepath.Abs(node)
			if err != nil {
				t.Fatal(err)
			}
			script, err := filepath.Abs(filepath.Join("..", "..", "..", "..", "runtime", "morde-cloud-runtime", "bin-account.mjs"))
			if err != nil {
				t.Fatal(err)
			}
			publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			stopAuth, err := app.NewAgentStopAuthenticator("initial-test-supervisor", bytes.Repeat([]byte{9}, 32))
			if err != nil {
				t.Fatal(err)
			}
			agentAuth, err := app.NewAgentGuardianStopAuthenticator("initial-test-supervisor", publicKey)
			if err != nil {
				t.Fatal(err)
			}
			taskAuth, err := app.NewAccountAgentTaskGuardianStopAuthenticator(publicKey)
			if err != nil {
				t.Fatal(err)
			}
			instances, err := app.NewAgentInstanceServiceWithGuardianStops(f.store, stopAuth, agentAuth, taskAuth)
			if err != nil {
				t.Fatal(err)
			}
			sealer, err := app.NewSealer(bytes.Repeat([]byte{3}, 32))
			if err != nil {
				t.Fatal(err)
			}
			checkpointService, err := app.NewAgentCheckpointService(f.store, sealer)
			if err != nil {
				t.Fatal(err)
			}
			checkpoints := &failFirstInitialCheckpoint{CloudDSHCheckpoints: checkpointService}
			control, err := app.NewAccountAgentRuntimeService(f.store)
			if err != nil {
				t.Fatal(err)
			}
			verifier, err := app.NewAccountAgentRuntimeGuardianStopAuthenticator(publicKey)
			if err != nil {
				t.Fatal(err)
			}
			supervisor := &recordCloudDSHE2ESupervisor{node: node, script: script, key: privateKey}
			t.Cleanup(func() {
				supervisor.mu.Lock()
				defer supervisor.mu.Unlock()
				if supervisor.cmd != nil && supervisor.cmd.Process != nil {
					_ = supervisor.cmd.Process.Kill()
					_ = supervisor.cmd.Wait()
				}
			})
			manager, err := worker.NewCloudDSHAccountRuntimeManager(worker.CloudDSHAccountRuntimeManagerOptions{Control: control,
				Supervisor: supervisor, Guardian: &recordCloudDSHE2EGuardian{key: privateKey}, StopVerifier: verifier,
				TaskStopper: instances, LeaseOwner: "initial-test-supervisor", MaxTasks: 2})
			if err != nil {
				t.Fatal(err)
			}
			model := &initialAttachmentModel{}
			executor, err := worker.NewCloudDSHExecutor(worker.CloudDSHExecutorOptions{SupervisorID: "initial-test-supervisor",
				AccountRuntimes: manager, Instances: instances, Checkpoints: checkpoints, Models: model, Tools: initialAttachmentReader{}})
			if err != nil {
				t.Fatal(err)
			}
			first, found, err := f.store.ClaimNextTask(ctx, "initial-task-worker-1", time.Minute)
			if err != nil || !found || first.ID != taskID {
				t.Fatalf("first task claim: found=%t err=%v", found, err)
			}
			if err := executor.ExecuteTask(ctx, first); err == nil {
				t.Fatal("checkpoint failure should prevent model dispatch")
			}
			if err := f.store.pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, message.ID).Scan(&delivery); err != nil || delivery != "pending" {
				t.Fatalf("failed startup changed delivery=%q err=%v", delivery, err)
			}
			if len(model.calls) != 0 {
				t.Fatalf("failed startup made %d model calls", len(model.calls))
			}
			if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, taskID); err != nil {
				t.Fatal(err)
			}
			second, found, err := f.store.ClaimNextTask(ctx, "initial-task-worker-2", time.Minute)
			if err != nil || !found || second.ID != taskID {
				t.Fatalf("recovery task claim: found=%t err=%v", found, err)
			}
			if err := executor.ExecuteTask(ctx, second); !errors.Is(err, worker.ErrCloudDSHCompletionPending) {
				t.Fatalf("recovered local runtime: %v", err)
			}
			if err := f.store.pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, message.ID).Scan(&delivery); err != nil || delivery != "applied" {
				t.Fatalf("recovered delivery=%q err=%v", delivery, err)
			}
			if len(model.calls) != 1 {
				t.Fatalf("recovered startup made %d model calls", len(model.calls))
			}
			var request struct {
				Messages []struct {
					ID      string `json:"id"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(model.calls[0].RawRequest, &request); err != nil {
				t.Fatal(err)
			}
			var messageCount, textCount, attachmentCount, contextCount int
			for _, item := range request.Messages {
				if item.ID == message.ID {
					messageCount++
				}
				for _, block := range item.Content {
					if test.content != "" && block.Text == test.content {
						textCount++
					}
					if block.Text == "[MORDE_TASK_ATTACHMENT_IDS_V1:"+attachment.AttachmentID+"]" {
						attachmentCount++
					}
					if strings.Contains(block.Text, "The earlier report is for the board.") {
						contextCount++
					}
				}
			}
			expectedText := 1
			if test.content == "" {
				expectedText = 0
			}
			if messageCount != 1 || textCount != expectedText || attachmentCount != 1 || contextCount != 1 {
				t.Fatalf("first model request lost or duplicated input: message=%d text=%d attachment=%d context=%d", messageCount, textCount, attachmentCount, contextCount)
			}
		})
	}
}
