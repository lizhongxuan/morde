package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

type projectConversationTestAuthorization struct{}

func (projectConversationTestAuthorization) AuthorizeTaskAction(context.Context, string, identity.Kind, string, string, []identity.Scope, identity.Scope) error {
	return nil
}

func (projectConversationTestAuthorization) AuthorizeTaskExecution(_ context.Context, principal identity.Principal, _ string, _ identity.Scope) (task.Authorization, error) {
	expiresAt := time.Now().Add(time.Hour)
	return task.Authorization{Kind: string(principal.Kind), TokenID: principal.TokenID, Version: 1, ExpiresAt: &expiresAt,
		Scopes: []string{string(identity.ScopeTaskRead), string(identity.ScopeTaskSubmit)}}, nil
}

func (projectConversationTestAuthorization) IsConfiguredModel(context.Context, string, string) (bool, error) {
	return true, nil
}

func TestProjectConversationMembersShareHistoryAndTasksKeepInitiator(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	member := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	outsider := createTestUser(t, store, 4, 20_000, 40_000)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Shared chat", "One project conversation", "project-conversation-project")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1,
		"project-conversation-invite", "project-conversation-accept"); err != nil {
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
	service, err := app.NewProjectConversationService(store, projects, tasks)
	if err != nil {
		t.Fatal(err)
	}
	principal := func(userID string) identity.Principal {
		return identity.Principal{UserID: userID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(userID)}
	}
	created, replay, err := service.Create(ctx, principal(owner.ID), projectValue.ID, app.CreateProjectConversationCommand{
		Title: "Shared", IdempotencyKey: "project-conversation-create",
	})
	if err != nil || replay || created.ProjectID != projectValue.ID || created.OwnerID != owner.ID {
		t.Fatalf("create project conversation = %#v replay=%t err=%v", created, replay, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',NULL,'synthetic','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`); err != nil {
		t.Fatal(err)
	}

	append := func(actorID, key, content string) app.TaskSubmissionResult {
		t.Helper()
		result := routeProjectMessageForTest(t, ctx, store, service, principal(actorID), projectValue.ID, created.ID, key, content)
		if result.Task.OwnerID != owner.ID || result.Task.InitiatorID != actorID || result.Message.OwnerID != owner.ID || result.Message.ActorID != actorID {
			t.Fatalf("project Task/message attribution = task(%s,%s) message(%s,%s), actor=%s", result.Task.OwnerID,
				result.Task.InitiatorID, result.Message.OwnerID, result.Message.ActorID, actorID)
		}
		return result
	}
	ownerTask := append(owner.ID, "project-conversation-owner-message", "from owner")
	aggregation := resultAggregationFixture{store: store, ownerID: owner.ID, conversation: created}
	completed := resultAggregationTask{ID: ownerTask.Task.ID, ActorID: owner.ID, UserMessageID: ownerTask.Message.ID, TurnID: mustID(t, domain.NewStepID)}
	if !aggregation.complete(t, &completed, "owner's confirmed answer") {
		t.Fatal("owner's Task result was not published")
	}
	memberTask := append(member.ID, "project-conversation-member-message", "from member")
	for _, actorID := range []string{owner.ID, member.ID} {
		page, err := service.ListTasks(ctx, principal(actorID), projectValue.ID, created.ID, app.PageRequest{Limit: 20})
		if err != nil || len(page.Items) != 1 || page.Items[0].ID != memberTask.Task.ID || page.Items[0].EventSequence < 1 {
			t.Fatalf("active project tasks for %s = %#v err=%v", actorID, page, err)
		}
	}
	if _, err := service.ListTasks(ctx, principal(outsider.ID), projectValue.ID, created.ID, app.PageRequest{Limit: 20}); err == nil {
		t.Fatal("outsider read active project tasks")
	}
	for _, actorID := range []string{owner.ID, member.ID} {
		page, err := service.ListMessages(ctx, principal(actorID), projectValue.ID, created.ID, app.PageRequest{Limit: 20})
		if err != nil || len(page.Items) != 3 || page.Items[0].ActorID != owner.ID || page.Items[1].ActorID != owner.ID ||
			page.Items[2].ActorID != member.ID || page.Items[0].Content != "from owner" ||
			page.Items[1].Content != "owner's confirmed answer" || page.Items[2].Content != "from member" {
			t.Fatalf("shared history for %s = %#v err=%v", actorID, page, err)
		}
	}
	instanceID, err := store.ServiceInstanceID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cross, replay, err := service.AcceptMessage(ctx, principal(owner.ID), projectValue.ID, created.ID, app.AcceptConversationMessageCommand{
		ServiceInstanceID: instanceID, ClientMessageID: "project-cross-actor-message", IdempotencyKey: "project-cross-actor-message",
		ReferenceTaskID: memberTask.Task.ID, Content: "change another member's task", ModelID: "p1234567",
	})
	if err != nil || replay {
		t.Fatalf("accept cross-actor message: replay=%t err=%v", replay, err)
	}
	crossClaim, found, err := store.ClaimConversationRoute(ctx, "project-cross-actor-router", time.Minute)
	if err != nil || !found || crossClaim.Message.ID != cross.ID {
		t.Fatalf("claim cross-actor message: found=%t err=%v", found, err)
	}
	crossSnapshot, err := store.LoadConversationRouteSnapshot(ctx, crossClaim)
	if err != nil || len(crossSnapshot.Context.Candidates) != 0 {
		t.Fatalf("other actor's Task entered control candidates: %+v err=%v", crossSnapshot.Context.Candidates, err)
	}
	if err := store.ApplyConversationRoute(ctx, crossClaim, crossSnapshot, app.ConversationRouteDecision{
		Action: app.ConversationRouteUpdate, TaskID: memberTask.Task.ID, ExpectedRevision: 0,
	}); err != nil {
		t.Fatal(err)
	}
	var routeState, targetStatus string
	if err := pool.QueryRow(ctx, `SELECT r.state,t.status FROM conversation_message_routes r JOIN tasks t ON t.id=$2 WHERE r.message_id=$1`,
		cross.ID, memberTask.Task.ID).Scan(&routeState, &targetStatus); err != nil || routeState != "waiting_user" || targetStatus != "queued" {
		t.Fatalf("cross-actor Task control was applied: route=%s task=%s err=%v", routeState, targetStatus, err)
	}
	if _, err := store.GetProjectConversation(ctx, outsider.ID, projectValue.ID, created.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("outsider project conversation read = %v, want not found", err)
	}
	if _, err := store.GetConversation(ctx, created.ID, owner.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("project conversation entered personal scope = %v, want not found", err)
	}

	personalID, err := domain.NewConversationID()
	if err != nil {
		t.Fatal(err)
	}
	personalConversation, _, err := store.CreateConversation(ctx, conversation.Conversation{ID: personalID, OwnerID: member.ID},
		testIdempotency(member.ID, "conversation.create", "project-conversation-personal"))
	if err != nil {
		t.Fatalf("create member's personal conversation: %v", err)
	}
	personalHistory, err := store.ListMessages(ctx, member.ID, personalConversation.ID, app.PageRequest{Limit: 20})
	if err != nil || len(personalHistory.Items) != 0 {
		t.Fatalf("member personal history mixed with project messages: %#v err=%v", personalHistory, err)
	}

	claimed, found, err := store.ClaimNextTask(ctx, "project-conversation-worker", time.Minute)
	if err != nil || !found || claimed.ID != memberTask.Task.ID {
		t.Fatalf("claim member project Task = task=%q found=%t err=%v; want %s", claimed.ID, found, err, memberTask.Task.ID)
	}
	for _, actorID := range []string{owner.ID, member.ID} {
		details, err := store.GetTask(ctx, memberTask.Task.ID, actorID)
		if err != nil || details.Task.OwnerID != owner.ID || details.Task.InitiatorID != member.ID {
			t.Fatalf("project Task read by member %s = %#v, %v", actorID, details.Task, err)
		}
		events, err := store.GetTaskEventPage(ctx, actorID, memberTask.Task.ID, 0, 20)
		if err != nil || len(events.Items) == 0 {
			t.Fatalf("project Task events for member %s = %#v, %v", actorID, events, err)
		}
	}
	if _, err := store.GetTask(ctx, memberTask.Task.ID, outsider.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("outsider project Task read = %v, want not found", err)
	}
	listed, err := store.ListTasks(ctx, member.ID, app.TaskListRequest{Page: app.PageRequest{Limit: 20}})
	if err != nil || len(listed.Items) != 2 {
		t.Fatalf("member project Task list = %#v, %v", listed, err)
	}
	listedIDs := map[string]bool{}
	for _, item := range listed.Items {
		listedIDs[item.ID] = true
	}
	if !listedIDs[memberTask.Task.ID] || !listedIDs[ownerTask.Task.ID] {
		t.Fatalf("project Task list omitted shared tasks: %#v", listed.Items)
	}
	if _, err := store.ListTaskEvents(ctx, outsider.ID, memberTask.Task.ID, 0, 20); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("outsider project Task events = %v, want not found", err)
	}
	contextValue := claimed.ConversationContext
	if contextValue == nil || contextValue.Scope != "project" || contextValue.ProjectID != projectValue.ID ||
		contextValue.OwnerID != owner.ID || contextValue.InitiatorID != member.ID || contextValue.ThroughSequence != 2 || len(contextValue.Messages) != 2 ||
		contextValue.Messages[0].Content != "from owner" || contextValue.Messages[1].Content != "owner's confirmed answer" {
		t.Fatalf("member project Task context = %#v", contextValue)
	}
	paused, err := tasks.RequestAction(ctx, principal(member.ID), memberTask.Task.ID, task.ActionPause, task.StatusRunning, "project-task-pause")
	if err != nil || paused.Task.OwnerID != owner.ID || paused.Task.InitiatorID != member.ID || paused.Task.Status != task.StatusPausing {
		t.Fatalf("project Task action by initiator = %#v, %v", paused, err)
	}
	otherMemberActionID := mustID(t, domain.NewTaskActionID)
	otherMemberAction := taskActionIdempotency(owner.ID, task.ActionCancel, "project-task-owner-cancel", memberTask.Task.ID,
		task.StatusRunning, otherMemberActionID)
	if _, err := store.RequestTaskAction(ctx, otherMemberAction, memberTask.Task.ID, task.ActionCancel,
		task.StatusRunning, otherMemberActionID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("conversation owner controlled another member's Task = %v, want not found", err)
	}

	currentProject, err := store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveProjectMember(ctx, owner.ID, projectValue.ID, member.ID, currentProject.Version, "project-conversation-revoke"); err != nil {
		t.Fatalf("revoke project member: %v", err)
	}
	if _, err := service.ListMessages(ctx, principal(member.ID), projectValue.ID, created.ID, app.PageRequest{Limit: 20}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked member retained project history access = %v, want not found", err)
	}
	if _, err := store.ListProjectConversationMessages(ctx, member.ID, projectValue.ID, created.ID, app.PageRequest{Limit: 20}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked member retained Store history access = %v, want not found", err)
	}
	if _, err := store.GetTask(ctx, memberTask.Task.ID, member.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked member retained Task access = %v, want not found", err)
	}
	if _, err := store.GetTask(ctx, memberTask.Task.ID, owner.ID); err != nil {
		t.Fatalf("remaining project owner lost access to Task history: %v", err)
	}
}

func TestProjectConversationTaskListPaginatesAndRevokedMemberCannotRead(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	member := createPreRetirementTestUser(t, store, 8, 20_000, 40_000)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Paged chat", "Project task pagination", "project-conversation-pages")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1,
		"project-pages-invite", "project-pages-accept"); err != nil {
		t.Fatal(err)
	}
	projects, err := app.NewProjectService(store)
	if err != nil {
		t.Fatal(err)
	}
	authorization := projectConversationTestAuthorization{}
	tasks, err := app.NewTaskService(store, authorization, testConfiguredModelCatalog{}, projects)
	if err != nil {
		t.Fatal(err)
	}
	service, err := app.NewProjectConversationService(store, projects, tasks)
	if err != nil {
		t.Fatal(err)
	}
	principal := func(userID string) identity.Principal {
		return identity.Principal{UserID: userID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(userID)}
	}
	conversationValue, replay, err := service.Create(ctx, principal(owner.ID), projectValue.ID, app.CreateProjectConversationCommand{
		Title: "Paged", IdempotencyKey: "project-pages-conversation",
	})
	if err != nil || replay {
		t.Fatalf("create project conversation: value=%+v replay=%t err=%v", conversationValue, replay, err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO model_connections
(id,owner_id,name,base_url,model_name,api_key_cipher,capability_text,checked_at)
VALUES ('p1234567',NULL,'synthetic','http://127.0.0.1','test',decode(repeat('01',29),'hex'),true,clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	for i, actorID := range []string{owner.ID, member.ID, owner.ID} {
		result := routeProjectMessageForTest(t, ctx, store, service, principal(actorID), projectValue.ID, conversationValue.ID,
			fmt.Sprintf("project-pages-task-%d", i), fmt.Sprintf("task %d", i))
		if result.Task.ID == "" || result.Message.TaskID != result.Task.ID {
			t.Fatalf("create project task %d: result=%+v", i, result)
		}
	}
	seen := make(map[string]bool)
	page, err := service.ListTasks(ctx, principal(member.ID), projectValue.ID, conversationValue.ID, app.PageRequest{Limit: 1})
	for err == nil {
		if len(page.Items) != 1 || seen[page.Items[0].ID] {
			t.Fatalf("project active task page duplicated or malformed: %+v", page)
		}
		seen[page.Items[0].ID] = true
		if page.NextCursor == "" {
			break
		}
		page, err = service.ListTasks(ctx, principal(member.ID), projectValue.ID, conversationValue.ID, app.PageRequest{Limit: 1, Cursor: page.NextCursor})
	}
	if err != nil || len(seen) != 3 {
		t.Fatalf("project task pagination returned %d tasks, err=%v; want three", len(seen), err)
	}
	currentProject, err := store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := projects.RemoveMember(ctx, principal(owner.ID), projectValue.ID, member.ID, currentProject.Version, "project-pages-remove-member"); err != nil {
		t.Fatalf("revoke project member: %v", err)
	}
	if _, err := service.ListTasks(ctx, principal(member.ID), projectValue.ID, conversationValue.ID, app.PageRequest{Limit: 1}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked member read project task list = %v, want not found", err)
	}
}
