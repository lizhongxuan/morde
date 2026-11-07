package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func TestProjectAttachmentRouteAndCheckpointRespectInitiator(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 2000, 4096)
	member := createPreRetirementTestUser(t, store, 4, 2000, 4096)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Attachments", "Project files", "checkpoint-project-attachment")
	otherProject := createProjectStoreFixture(t, ctx, store, owner.ID, "Other", "Other files", "checkpoint-other-attachment")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1,
		"checkpoint-attachment-invite", "checkpoint-attachment-accept"); err != nil {
		t.Fatal(err)
	}
	projects, err := app.NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	authorization := projectConversationTestAuthorization{}
	tasks, err := app.NewTaskService(store, authorization, authorization, projects)
	if err != nil {
		t.Fatal(err)
	}
	conversations, err := app.NewProjectConversationService(store, projects, tasks)
	if err != nil {
		t.Fatal(err)
	}
	ownerPrincipal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(owner.ID)}
	shared, _, err := conversations.Create(ctx, ownerPrincipal, projectValue.ID,
		app.CreateProjectConversationCommand{Title: "Member files", IdempotencyKey: "checkpoint-attachment-conversation"})
	if err != nil {
		t.Fatal(err)
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_connections(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',NULL,'project test','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	token, err := store.GetTokenByID(ctx, testSessionTokenID(member.ID))
	if err != nil {
		t.Fatal(err)
	}
	const clientID = "checkpoint-project-message"
	const clientSlot = "checkpoint-project-file"
	const attachmentID = "pca_0123456789abcdef0123456789abcdef"
	hash := sha256.Sum256([]byte(clientID))
	message, _, err := store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
		Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: member.ID,
			ConversationID: shared.ID, Role: conversation.RoleUser, Content: "Inspect the project file"},
		ServiceInstanceID: instanceID, ClientID: clientID, RequestHash: hash[:],
		Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
		Input: app.ConversationRoutingInput{ProjectID: projectValue.ID, ModelID: "p1234567", ClientAttachmentIDs: []string{clientSlot},
			Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}},
	})
	if err != nil {
		t.Fatal(err)
	}
	claim := func(want bool) app.ConversationRouteClaim {
		t.Helper()
		value, found, err := store.ClaimConversationRoute(ctx, "project-attachment-router", time.Minute)
		if err != nil || found != want {
			t.Fatalf("project route claim: found=%t want=%t err=%v", found, want, err)
		}
		return value
	}
	claim(false)
	if _, err := pool.Exec(ctx, `INSERT INTO project_conversation_attachments
(service_instance_id,project_id,owner_id,actor_id,conversation_id,message_id,client_attachment_id,attachment_id,name,media_type,size_bytes,sha256,state,expires_at)
VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,'report.txt','text/plain',0,$9,'uploading',clock_timestamp()+interval '1 hour')`,
		instanceID, otherProject.ID, owner.ID, member.ID, shared.ID, message.ID, clientSlot, attachmentID, hex.EncodeToString(make([]byte, 32))); err != nil {
		t.Fatal(err)
	}
	claim(false)
	if _, err := pool.Exec(ctx, `UPDATE project_conversation_attachments SET state='committed' WHERE message_id=$1`, message.ID); err != nil {
		t.Fatal(err)
	}
	claim(false) // A committed slot from another project is not the declared file.
	if _, err := pool.Exec(ctx, `UPDATE project_conversation_attachments SET project_id=$2 WHERE message_id=$1`, message.ID, projectValue.ID); err != nil {
		t.Fatal(err)
	}
	route := claim(true)
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, route)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := store.ApplyConversationRoute(ctx, route, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
		t.Fatal(err)
	}
	claimed, found, err := store.ClaimNextTask(ctx, "project-attachment-worker", time.Minute)
	if err != nil || !found || claimed.OwnerID != owner.ID || claimed.InitiatorID != member.ID || claimed.ProjectID != projectValue.ID {
		t.Fatalf("project attachment task claim: found=%t task=%+v err=%v", found, claimed, err)
	}
	key := make([]byte, 32)
	key[0] = 1
	stopAuth, err := app.NewAgentStopAuthenticator("project-attachment-supervisor", key)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := app.NewAgentInstanceService(store, stopAuth)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 32)
	nonce[0] = 1
	agent, _, err := instances.Start(ctx, app.AgentInstanceStart{Fence: app.AgentInstanceFence{
		TaskID: claimed.ID, OwnerID: owner.ID, InitiatorID: member.ID, AgentInstanceID: "project_attachment_agent",
		AgentSessionID: "project_attachment_session", SupervisorID: "project-attachment-supervisor", LeaseOwner: claimed.Lease.Owner,
		LeaseVersion: claimed.Lease.Version, AuthorizationVersion: claimed.Authorization.Version, EngineVersion: "0.2.0-rc.2", ProfileVersion: "v1",
	}, SupervisorNonce: nonce, LeaseDuration: app.MaxAgentLease})
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := app.NewSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	checkpoints, err := app.NewAgentCheckpointService(store, sealer)
	if err != nil {
		t.Fatal(err)
	}
	fence := app.AgentCheckpointFence{TaskID: claimed.ID, OwnerID: owner.ID, InitiatorID: member.ID,
		AgentInstanceID: agent.Fence.AgentInstanceID, AgentGeneration: agent.Fence.AgentGeneration,
		AgentSessionID: agent.Fence.AgentSessionID, AuthorizationVersion: agent.Fence.AuthorizationVersion,
		LeaseOwner: agent.Fence.LeaseOwner, LeaseVersion: agent.Fence.LeaseVersion, EngineVersion: agent.Fence.EngineVersion, ProfileVersion: agent.Fence.ProfileVersion}
	pending, err := checkpoints.PendingMessages(ctx, fence)
	if err != nil || len(pending) != 1 || pending[0].ID != message.ID || len(pending[0].AttachmentIDs) != 1 || pending[0].AttachmentIDs[0] != attachmentID {
		t.Fatalf("project task did not receive exactly the member's committed attachment: pending=%+v err=%v", pending, err)
	}
	wrongActor := fence
	wrongActor.InitiatorID = owner.ID
	if _, err := checkpoints.PendingMessages(ctx, wrongActor); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("project owner could use a different member's task fence: %v", err)
	}
	payload, err := json.Marshal(map[string]any{"header": map[string]any{"id": fence.AgentSessionID}, "from_seq": 0, "events": []any{
		map[string]any{"seq": 0, "type": "user/message", "data": map[string]any{"id": message.ID, "role": "user", "content": []any{
			map[string]string{"type": "text", "text": message.Content},
			map[string]string{"type": "text", "text": "[MORDE_TASK_ATTACHMENT_IDS_V1:" + attachmentID + "]"},
		}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: fence, From: 0, Through: 0, Digest: digest[:], Payload: payload}); err != nil {
		t.Fatalf("committed project attachment checkpoint: %v", err)
	}
	var delivery string
	if err := pool.QueryRow(ctx, `SELECT delivery_status FROM conversation_messages WHERE id=$1`, message.ID).Scan(&delivery); err != nil || delivery != "applied" {
		t.Fatalf("project message delivery = %q err=%v", delivery, err)
	}
	changed := bytes.Replace(payload, []byte(attachmentID), []byte("pca_1123456789abcdef0123456789abcdef"), 1)
	var body map[string]any
	if err := json.Unmarshal(changed, &body); err != nil {
		t.Fatal(err)
	}
	body["from_seq"] = 1
	body["events"].([]any)[0].(map[string]any)["seq"] = 1
	changed, err = json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	changedDigest := sha256.Sum256(changed)
	if _, err := checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: fence, From: 1, Through: 1, Digest: changedDigest[:], Payload: changed}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("unbound project attachment entered the checkpoint: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM project_members WHERE project_id=$1 AND user_id=$2`, projectValue.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := checkpoints.PendingMessages(ctx, fence); domain.CodeOf(err) != domain.CodeUnauthenticated {
		t.Fatalf("removed project member retained attachment access: %v", err)
	}
}
