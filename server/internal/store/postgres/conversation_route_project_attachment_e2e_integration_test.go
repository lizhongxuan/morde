package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
	"morde/server/internal/worker"
)

func TestProjectAttachmentRealAgentInitialDeliverySurvivesRestart(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_connections(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',NULL,'project attachment model','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	token, err := f.store.GetTokenByID(ctx, f.member.TokenID)
	if err != nil {
		t.Fatal(err)
	}
	const clientID, clientSlot = "project-first-message-001", "project-first-file-001"
	content := "Please inspect the project file: " + strings.Repeat("x", 40<<10)
	hash := sha256.Sum256([]byte(clientID))
	message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.owner.UserID, ActorID: f.member.UserID,
			ConversationID: f.conversationID, Role: conversation.RoleUser, Content: content},
		ServiceInstanceID: f.instanceID, ClientID: clientID, RequestHash: hash[:],
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
		Input: app.ConversationRoutingInput{ProjectID: f.projectID, ModelID: "p1234567", ClientAttachmentIDs: []string{clientSlot},
			Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := app.ProjectConversationAttachmentScope{ProjectID: f.projectID, ConversationID: f.conversationID,
		MessageID: message.ID, ClientAttachmentID: clientSlot, ServiceInstanceID: f.instanceID}
	data := []byte("synthetic local project attachment")
	if _, err := f.service.Initialize(ctx, f.member, scope, syntheticProjectAttachmentMetadata(data)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.store.ClaimConversationRoute(ctx, "project-first-router", time.Minute); err != nil || found {
		t.Fatalf("uncommitted project attachment entered routing: found=%t err=%v", found, err)
	}
	digest := sha256.Sum256(data)
	if _, err := f.service.PutChunk(ctx, f.member, scope, 0, hex.EncodeToString(digest[:]), bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	attachment, err := f.service.Commit(ctx, f.member, scope)
	if err != nil {
		t.Fatal(err)
	}
	claim, found, err := f.store.ClaimConversationRoute(ctx, "project-first-router", time.Minute)
	if err != nil || !found || claim.Message.ID != message.ID {
		t.Fatalf("committed project attachment route: found=%t err=%v", found, err)
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
		t.Fatalf("project message before startup: delivery=%q err=%v", delivery, err)
	}
	model := &initialAttachmentModel{}
	var failedCheckpoint *failFirstInitialCheckpoint
	executor := revisionTestExecutorWithCheckpoints(t, f.store, model, func(checkpoints worker.CloudDSHCheckpoints) worker.CloudDSHCheckpoints {
		failedCheckpoint = &failFirstInitialCheckpoint{CloudDSHCheckpoints: checkpoints}
		return failedCheckpoint
	}, initialAttachmentReader{})
	first, found, err := f.store.ClaimNextTask(ctx, "project-first-worker-1", time.Minute)
	if err != nil || !found || first.ID != taskID || first.InitiatorID != f.member.UserID {
		t.Fatalf("first project task claim: found=%t err=%v", found, err)
	}
	if err := executor.ExecuteTask(ctx, first); err == nil || failedCheckpoint == nil || !failedCheckpoint.failed {
		t.Fatalf("expected first project checkpoint to fail before the model: %v", err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, message.ID).Scan(&delivery); err != nil || delivery != "pending" || len(model.calls) != 0 {
		t.Fatalf("failed project startup changed delivery=%q model_calls=%d err=%v", delivery, len(model.calls), err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, taskID); err != nil {
		t.Fatal(err)
	}
	second, found, err := f.store.ClaimNextTask(ctx, "project-first-worker-2", time.Minute)
	if err != nil || !found || second.ID != taskID {
		t.Fatalf("recover project task: found=%t err=%v", found, err)
	}
	if err := executor.ExecuteTask(ctx, second); !errors.Is(err, worker.ErrCloudDSHCompletionPending) {
		t.Fatalf("recovered project runtime: %v", err)
	}
	if err := f.store.pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, message.ID).Scan(&delivery); err != nil || delivery != "applied" || len(model.calls) != 1 {
		t.Fatalf("recovered project delivery=%q model_calls=%d err=%v", delivery, len(model.calls), err)
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
	var messageCount, textCount, attachmentCount int
	for _, item := range request.Messages {
		if item.ID == message.ID {
			messageCount++
		}
		for _, block := range item.Content {
			if block.Text == content {
				textCount++
			}
			if block.Text == "[MORDE_TASK_ATTACHMENT_IDS_V1:"+attachment.AttachmentID+"]" {
				attachmentCount++
			}
		}
	}
	if messageCount != 1 || textCount != 1 || attachmentCount != 1 {
		t.Fatalf("project first input was lost or duplicated: message=%d text=%d attachment=%d", messageCount, textCount, attachmentCount)
	}
}
