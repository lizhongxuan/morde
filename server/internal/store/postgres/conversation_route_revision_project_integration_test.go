package postgres

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func TestProjectRevisionSettlementRequiresOriginalInitiatorMembership(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	owner := createPreRetirementTestUser(t, store, 4, 2000, 4096)
	member := createPreRetirementTestUser(t, store, 4, 2000, 4096)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Revision project", "Shared work", "revision-project-001")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1,
		"revision-project-invite", "revision-project-accept"); err != nil {
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
		app.CreateProjectConversationCommand{Title: "Revision isolation", IdempotencyKey: "revision-project-conversation"})
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
	receive := func(actor, clientID, content, reference string) app.ConversationRouteClaim {
		t.Helper()
		token, err := store.GetTokenByID(ctx, testSessionTokenID(actor))
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256([]byte(clientID))
		message, _, err := store.AcceptConversationMessage(ctx, app.ConversationMessageReceipt{
			Message: conversation.Message{ID: mustID(t, domain.NewMessageID), OwnerID: owner.ID, ActorID: actor,
				ConversationID: shared.ID, Role: conversation.RoleUser, Content: content},
			ServiceInstanceID: instanceID, ClientID: clientID, RequestHash: hash[:],
			Authorization: task.Authorization{Kind: string(identity.KindSession), TokenID: token.ID, Version: 1, ExpiresAt: &token.ExpiresAt},
			Input: app.ConversationRoutingInput{ProjectID: projectValue.ID, ModelID: "p1234567", ReferenceTaskID: reference,
				Budget: task.Budget{ActiveSeconds: 120, ToolCalls: 5, ModelTokens: 1000}},
		})
		if err != nil {
			t.Fatal(err)
		}
		claim, found, err := store.ClaimConversationRoute(ctx, "project-revision-router", time.Minute)
		if err != nil || !found || claim.Message.ID != message.ID {
			t.Fatalf("claim project route: found=%t err=%v", found, err)
		}
		return claim
	}
	createTask := func(claim app.ConversationRouteClaim) string {
		t.Helper()
		snapshot, err := store.LoadConversationRouteSnapshot(ctx, claim)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Context.ModelID = "p1234567"
		if err := store.ApplyConversationRoute(ctx, claim, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteNew}); err != nil {
			t.Fatal(err)
		}
		var id string
		if err := pool.QueryRow(ctx, `SELECT task_id FROM conversation_messages WHERE id=$1`, claim.Message.ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	memberInitial := receive(member.ID, "project-revision-member-001", "Member task", "")
	memberTask := createTask(memberInitial)
	claimed, found, err := store.ClaimNextTask(ctx, "project-revision-member-worker", time.Minute)
	if err != nil || !found || claimed.ID != memberTask || claimed.OwnerID != owner.ID || claimed.InitiatorID != member.ID {
		t.Fatalf("project Task identities: found=%t task=%+v err=%v", found, claimed, err)
	}
	model := &revisionBlockingModel{started: make(chan struct{})}
	executor := revisionTestExecutor(t, store, model)
	result := make(chan error, 1)
	go func() { result <- executor.ExecuteTask(ctx, claimed) }()
	select {
	case <-model.started:
	case err := <-result:
		t.Fatalf("member Agent stopped before model start: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("member model did not start")
	}
	ownerTask := createTask(receive(owner.ID, "project-revision-owner-001", "Owner task", ""))
	malicious := receive(member.ID, "project-revision-member-002", "Modify the other Task", ownerTask)
	snapshot, err := store.LoadConversationRouteSnapshot(ctx, malicious)
	if err != nil || len(snapshot.Context.Candidates) != 1 || snapshot.Context.Candidates[0].TaskID != memberTask {
		t.Fatalf("another member's active task was exposed: %+v err=%v", snapshot, err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := store.ApplyConversationRoute(ctx, malicious, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteUpdate,
		TaskID: ownerTask, ExpectedRevision: 0}); err != nil {
		t.Fatal(err)
	}
	update := receive(member.ID, "project-revision-member-003", "Change my Task", memberTask)
	snapshot, err = store.LoadConversationRouteSnapshot(ctx, update)
	if err != nil || len(snapshot.Context.Candidates) != 1 {
		t.Fatalf("member snapshot: %+v err=%v", snapshot, err)
	}
	snapshot.Context.ModelID = "p1234567"
	if err := store.ApplyConversationRoute(ctx, update, snapshot, app.ConversationRouteDecision{Action: app.ConversationRouteUpdate,
		TaskID: memberTask, ExpectedRevision: snapshot.Context.Candidates[0].Revision}); err != nil {
		t.Fatal(err)
	}
	if proven, err := store.RevisionStopProven(ctx, claimed); err != nil || !proven {
		t.Fatalf("authorized member revision proof: proven=%t err=%v", proven, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM project_members WHERE project_id=$1 AND user_id=$2`, projectValue.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	if proven, err := store.RevisionStopProven(ctx, claimed); err != nil || proven {
		t.Fatalf("removed member retained revision proof: proven=%t err=%v", proven, err)
	}
	var ownerStatus, maliciousState string
	if err := pool.QueryRow(ctx, `SELECT (SELECT status FROM tasks WHERE id=$1), (SELECT state FROM conversation_message_routes WHERE message_id=$2)`,
		ownerTask, malicious.Message.ID).Scan(&ownerStatus, &maliciousState); err != nil || ownerStatus != "queued" || maliciousState != "waiting_user" {
		t.Fatalf("member action affected owner task: owner=%s route=%s err=%v", ownerStatus, maliciousState, err)
	}
	cancel()
	select {
	case <-result:
	case <-time.After(10 * time.Second):
		t.Fatal("member Agent did not stop after cancellation")
	}
}
