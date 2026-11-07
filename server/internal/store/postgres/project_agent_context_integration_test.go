package postgres

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/project"
)

func TestPostgresProjectAgentContextIsSharedBoundedAndMemberScoped(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	member := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	outsider := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Agent context", "Shared project only", "project-agent-context-01")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1,
		"project-context-invite-1", "project-context-accept-1"); err != nil {
		t.Fatal(err)
	}

	contents := []project.Content{
		{ProjectID: projectValue.ID, Kind: project.ContentMemory, ID: "global", Revision: 1, Body: []byte(`{"content":"shared project memory"}`), UpdatedBy: owner.ID},
		{ProjectID: projectValue.ID, Kind: project.ContentGoal, ID: "goal-1", Revision: 1, Body: []byte(`{"title":"Shared goal","status":"active","steps":[]}`), UpdatedBy: owner.ID},
		{ProjectID: projectValue.ID, Kind: project.ContentDocument, ID: "doc-1", Revision: 1, Body: []byte(`{"title":"Shared document","content":"shared project document"}`), UpdatedBy: owner.ID},
		{ProjectID: projectValue.ID, Kind: project.ContentFeedPost, ID: "feed-1", Revision: 1, Body: []byte(`{"title":"Not task context","type":"note","created_at":"2026-01-01T00:00:00Z","body":"feed data"}`), UpdatedBy: owner.ID},
		{ProjectID: projectValue.ID, Kind: project.ContentDocument, ID: "large-doc", Revision: 1,
			Body: []byte(fmt.Sprintf(`{"title":"Large","content":"%s"}`, strings.Repeat("x", 25<<10))), UpdatedBy: owner.ID},
	}
	for _, content := range contents {
		if _, replay, err := store.ApplyProjectContentMutation(ctx, owner.ID, content, 0, false, "agent-context-"+content.ID+"-01"); err != nil || replay {
			t.Fatalf("save project context %s/%s: replay=%t err=%v", content.Kind, content.ID, replay, err)
		}
	}
	personalPrincipal := identity.Principal{UserID: owner.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(owner.ID)}
	if _, err := store.ApplyPersonalMutation(ctx, personalPrincipal, personalObjectMutationForTest(t, owner.ID, "private-memory-001",
		personal.ObjectTypeMemory, "global", 0, `{"content":"private personal canary"}`, false)); err != nil {
		t.Fatalf("save separate personal memory: %v", err)
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
	principal := func(userID string) identity.Principal {
		return identity.Principal{UserID: userID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(userID)}
	}
	conversationValue, replay, err := conversations.Create(ctx, principal(owner.ID), projectValue.ID,
		app.CreateProjectConversationCommand{Title: "Agent context", IdempotencyKey: "project-agent-context-conversation"})
	if err != nil || replay {
		t.Fatalf("create project conversation = %#v replay=%t err=%v", conversationValue, replay, err)
	}
	appendTask := func(actorID, key string) app.TaskSubmissionResult {
		t.Helper()
		result, err := submitIndependentProjectTaskForTest(t, ctx, store, owner.ID, actorID, projectValue.ID, conversationValue.ID,
			"project-test-model", "Read shared project context", key, nil)
		if err != nil {
			t.Fatalf("append project Task as %s: %v", actorID, err)
		}
		return result
	}
	ownerSubmission := appendTask(owner.ID, "project-agent-context-owner-task")
	ownerTask, found, err := store.ClaimNextTask(ctx, "project-agent-context-owner-worker", time.Minute)
	if err != nil || !found || ownerTask.ID != ownerSubmission.Task.ID {
		t.Fatalf("claim owner project Task = %#v found=%t err=%v", ownerTask, found, err)
	}
	ownerPrincipal := principal(owner.ID)
	ownerItems, ownerOmitted, err := projects.AgentContext(ctx, ownerPrincipal, ownerTask)
	if err != nil || len(ownerItems) != 3 || !ownerOmitted {
		t.Fatalf("owner project context = %#v omitted=%t err=%v; want three bounded project records", ownerItems, ownerOmitted, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='failed',finished_at=clock_timestamp() WHERE id=$1`, ownerTask.ID); err != nil {
		t.Fatalf("finish owner context Task fixture: %v", err)
	}

	memberSubmission := appendTask(member.ID, "project-agent-context-member-task")
	memberTask, found, err := store.ClaimNextTask(ctx, "project-agent-context-member-worker", time.Minute)
	if err != nil || !found || memberTask.ID != memberSubmission.Task.ID {
		t.Fatalf("claim member project Task = %#v found=%t err=%v", memberTask, found, err)
	}
	memberItems, memberOmitted, err := projects.AgentContext(ctx, principal(member.ID), memberTask)
	if err != nil || !memberOmitted || !reflect.DeepEqual(ownerItems, memberItems) {
		t.Fatalf("member project context = %#v omitted=%t err=%v; owner context=%#v", memberItems, memberOmitted, err, ownerItems)
	}
	for _, item := range memberItems {
		if item.Kind != project.ContentMemory && item.Kind != project.ContentGoal && item.Kind != project.ContentDocument {
			t.Fatalf("non-context Muse data was included: %#v", item)
		}
		if strings.Contains(string(item.Body), "private personal canary") || item.ID == "large-doc" {
			t.Fatalf("context exposed personal data or exceeded per-object bound: %#v", item)
		}
	}
	if _, _, err := projects.AgentContext(ctx, principal(outsider.ID), memberTask); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("outsider context access error = %v", err)
	}
	currentProject, err := store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveProjectMember(ctx, owner.ID, projectValue.ID, member.ID, currentProject.Version, "project-context-remove-01"); err != nil {
		t.Fatalf("remove project member: %v", err)
	}
	if _, _, err := projects.AgentContext(ctx, principal(member.ID), memberTask); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked member context access error = %v", err)
	}
}
