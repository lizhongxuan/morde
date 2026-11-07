package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

func TestProjectConversationAttachmentTaskReadRequiresCurrentAgentAndMembership(t *testing.T) {
	f := newProjectAttachmentFixture(t)
	ctx := context.Background()
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',NULL,'synthetic project model','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE owner_execution_controls SET state='enabled' WHERE owner_id=$1`, f.member.UserID); err != nil {
		t.Fatal(err)
	}
	token, err := f.store.GetTokenByID(ctx, f.member.TokenID)
	if err != nil {
		t.Fatal(err)
	}
	clientID := "project-task-attachment-message"
	digest := sha256.Sum256([]byte(clientID))
	message, _, err := f.store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: f.owner.UserID, ActorID: f.member.UserID,
			ConversationID: f.conversationID, Role: conversation.RoleUser, Content: "Read the synthetic file"},
		ServiceInstanceID: f.instanceID, ClientID: clientID, RequestHash: digest[:],
		Input: app.ConversationRoutingInput{ProjectID: f.projectID, ModelID: "p1234567",
			ClientAttachmentIDs: []string{"task-attachment-001", "task-pdf-attachment-001"}, Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}},
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := app.ProjectConversationAttachmentScope{ProjectID: f.projectID, ConversationID: f.conversationID,
		MessageID: message.ID, ClientAttachmentID: "task-attachment-001", ServiceInstanceID: f.instanceID}
	content := []byte("synthetic project task attachment")
	initialized, err := f.service.Initialize(ctx, f.member, scope, syntheticProjectAttachmentMetadata(content))
	if err != nil {
		t.Fatal(err)
	}
	contentDigest := sha256.Sum256(content)
	if _, err := f.service.PutChunk(ctx, f.member, scope, 0, hex.EncodeToString(contentDigest[:]), bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Commit(ctx, f.member, scope); err != nil {
		t.Fatal(err)
	}
	pdfScope := scope
	pdfScope.ClientAttachmentID = "task-pdf-attachment-001"
	pdf := []byte("synthetic pdf")
	pdfMeta := syntheticProjectAttachmentMetadata(pdf)
	pdfMeta.Name, pdfMeta.MediaType, pdfMeta.DerivativeMode = "synthetic.pdf", "application/pdf", "pdf"
	pdfReceipt, err := f.service.Initialize(ctx, f.member, pdfScope, pdfMeta)
	if err != nil {
		t.Fatal(err)
	}
	pdfDigest := sha256.Sum256(pdf)
	if _, err := f.service.PutChunk(ctx, f.member, pdfScope, 0, hex.EncodeToString(pdfDigest[:]), bytes.NewReader(pdf), int64(len(pdf))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Commit(ctx, f.member, pdfScope); err != nil {
		t.Fatal(err)
	}
	pdfText := []byte("synthetic extracted text")
	textDigest := sha256.Sum256(pdfText)
	if _, err := f.service.PutDerivatives(ctx, f.member, pdfScope, app.ProjectConversationDerivatives{Outcome: "completed",
		Files: []app.ProjectConversationDerivativeFile{{Kind: "extracted_text", Page: 0, Name: "extracted.txt",
			MediaType: "text/plain", SHA256: hex.EncodeToString(textDigest[:]), Content: pdfText}}}); err != nil {
		t.Fatal(err)
	}
	claim, found, err := f.store.ClaimConversationRoute(ctx, "project-attachment-router", time.Minute)
	if err != nil || !found || claim.Message.ID != message.ID {
		t.Fatalf("claim attachment message: found=%t err=%v", found, err)
	}
	snapshot, err := f.store.LoadConversationRouteSnapshot(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := f.store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := f.store.ClaimNextTask(ctx, "project-attachment-task-worker", time.Minute)
	if err != nil || !found || claimed.ProjectID != f.projectID || claimed.InitiatorID != f.member.UserID {
		t.Fatalf("claim attachment task: found=%t err=%v", found, err)
	}
	checkpoint := sha256.Sum256([]byte("synthetic project agent checkpoint"))
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET agent_instance_id='agent-attachment',agent_generation=1,
agent_session_id='session-attachment',agent_engine_version='engine',agent_profile_version='profile',checkpoint_through_seq=0,checkpoint_digest=$2
WHERE id=$1`, claimed.ID, checkpoint[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_agent_sessions(task_id,owner_id,session_id,checkpoint_through_seq,checkpoint_digest)
VALUES ($1,$2,'session-attachment',0,$3)`, claimed.ID, f.owner.UserID, checkpoint[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `INSERT INTO task_agent_instances
(task_id,owner_id,agent_instance_id,agent_generation,agent_session_id,engine_version,profile_version,supervisor_id,
 stop_nonce_digest,lease_owner,lease_version,auth_version,initial_lease_ms,lease_expires_at,state)
VALUES ($1,$2,'agent-attachment',1,'session-attachment','engine','profile','attachment-supervisor',$3,$4,$5,1,30000,
 clock_timestamp()+interval '30 seconds','active')`, claimed.ID, f.owner.UserID, make([]byte, 32), claimed.Lease.Owner, claimed.Lease.Version); err != nil {
		t.Fatal(err)
	}
	claimed.AgentInstanceID, claimed.AgentGeneration, claimed.AgentSessionID = "agent-attachment", 1, "session-attachment"
	claimed.AgentEngineVersion, claimed.AgentProfileVersion = "engine", "profile"
	attachments, err := f.service.ListTaskAttachments(ctx, claimed)
	if err != nil || len(attachments) != 3 || attachments[0].AttachmentID != initialized.AttachmentID {
		t.Fatalf("list authorized task attachments: %+v err=%v", attachments, err)
	}
	var derivativeID string
	for _, attachment := range attachments {
		if attachment.ParentAttachmentID == pdfReceipt.AttachmentID && attachment.DerivativeKind == "extracted_text" {
			derivativeID = attachment.AttachmentID
		}
	}
	if derivativeID == "" {
		t.Fatalf("PDF text was not associated with its original: %+v", attachments)
	}
	derivedReader, derivedMetadata, err := f.service.OpenTaskAttachment(ctx, claimed, derivativeID)
	if err != nil {
		t.Fatal(err)
	}
	derivedBytes, err := io.ReadAll(derivedReader)
	_ = derivedReader.Close()
	if err != nil || derivedMetadata.ParentAttachmentID != pdfReceipt.AttachmentID || !bytes.Equal(derivedBytes, pdfText) {
		t.Fatalf("read authorized PDF text: size=%d err=%v", len(derivedBytes), err)
	}
	reader, metadata, err := f.service.OpenTaskAttachment(ctx, claimed, initialized.AttachmentID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || metadata.AttachmentID != initialized.AttachmentID || !bytes.Equal(got, content) {
		t.Fatalf("read authorized task attachment: size=%d err=%v", len(got), err)
	}
	wrongInitiator := claimed
	wrongInitiator.InitiatorID = f.owner.UserID
	if _, _, err := f.service.OpenTaskAttachment(ctx, wrongInitiator, initialized.AttachmentID); err == nil {
		t.Fatal("a different task initiator read the attachment")
	}
	if _, _, err := f.service.OpenTaskAttachment(ctx, wrongInitiator, derivativeID); err == nil {
		t.Fatal("a different task initiator read the derived text")
	}
	staleLease := claimed
	staleLease.Lease.Version++
	if _, err := f.service.ListTaskAttachments(ctx, staleLease); err == nil {
		t.Fatal("a stale task lease listed attachments")
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=current_revision+1 WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.service.OpenTaskAttachment(ctx, claimed, initialized.AttachmentID); err == nil {
		t.Fatal("an older task revision read an attachment")
	}
	if _, err := f.store.pool.Exec(ctx, `UPDATE tasks SET desired_revision=current_revision WHERE id=$1`, claimed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.pool.Exec(ctx, `DELETE FROM project_members WHERE project_id=$1 AND user_id=$2`, f.projectID, f.member.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.ListTaskAttachments(ctx, claimed); err == nil {
		t.Fatal("removed member's task listed attachments")
	}
}
