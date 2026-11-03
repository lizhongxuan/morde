package postgres

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/notification"
	"morde/server/internal/domain/personal"
	"morde/server/internal/domain/project"
)

func TestPostgresGoalAndProjectSourceNotificationsAreDurableAndRechecked(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 4, 20_000, 40_000)
	member := createTestUser(t, store, 4, 20_000, 40_000)
	principal := createPersonalReadSessionPrincipal(t, store, owner.ID)

	goalID := "goal-reminders-1"
	goalCreate := personalObjectMutationForTest(t, owner.ID, "goal-create-001", personal.ObjectTypeGoal, goalID, 0,
		`{"title":"private goal title","status":"active","steps":[{"id":"step_1","title":"private step","status":"pending"}]}`, false)
	if _, err := store.ApplyPersonalMutation(ctx, principal, goalCreate); err != nil {
		t.Fatalf("create goal: %v", err)
	}
	goalUpdate := personalObjectMutationForTest(t, owner.ID, "goal-update-001", personal.ObjectTypeGoal, goalID, 1,
		`{"title":"private goal title","status":"done","steps":[{"id":"step_1","title":"private step","status":"blocked","note":"private blocker"}]}`, false)
	if _, err := store.ApplyPersonalMutation(ctx, principal, goalUpdate); err != nil {
		t.Fatalf("complete and block goal step: %v", err)
	}
	if receipt, err := store.ApplyPersonalMutation(ctx, principal, goalUpdate); err != nil || !receipt.Replay {
		t.Fatalf("replay goal update: receipt=%#v err=%v", receipt, err)
	}

	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Notification project", "private project", "source-notice-project-01")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1,
		"source-notice-invite-1", "source-notice-accept-1"); err != nil {
		t.Fatalf("add project member: %v", err)
	}
	workItemID, err := project.NewID()
	if err != nil {
		t.Fatal(err)
	}
	item := project.WorkItem{
		ID: workItemID, ProjectID: projectValue.ID, Title: "private work item", Description: "private description",
		AcceptanceCriteria: []string{"private acceptance"}, AssigneeID: owner.ID, State: project.WorkItemOpen, Version: 1,
	}
	if _, _, err := store.CreateProjectWorkItem(ctx, owner.ID, item, "source-notice-work-create-01"); err != nil {
		t.Fatalf("create project work item: %v", err)
	}
	blocked := item
	blocked.State, blocked.BlockedReason, blocked.Version = project.WorkItemBlocked, "private blocked reason", 2
	digest := sha256.Sum256([]byte("project work item blocked"))
	mutation := app.ProjectWorkItemMutation{
		Operation: "update_work_item", WorkItemID: item.ID, IdempotencyKey: "source-notice-work-update-01", RequestDigest: digest[:],
	}
	if _, err := store.UpdateProjectWorkItem(ctx, owner.ID, blocked, 1, mutation); err != nil {
		t.Fatalf("block project work item: %v", err)
	}
	if _, err := store.UpdateProjectWorkItem(ctx, owner.ID, blocked, 1, mutation); err != nil {
		t.Fatalf("replay project work item update: %v", err)
	}

	service, err := app.NewNotificationService(store)
	if err != nil {
		t.Fatal(err)
	}
	processed, err := service.ProcessPending(ctx, 100)
	if err != nil || processed != 4 {
		t.Fatalf("ProcessPending() = %d, %v; want 2 Goal and 2 project recipient events", processed, err)
	}
	ownerPage, err := store.ListNotifications(ctx, owner.ID, false, app.PageRequest{Limit: 10})
	if err != nil || len(ownerPage.Items) != 3 {
		t.Fatalf("owner notifications = %#v, %v; want 3", ownerPage.Items, err)
	}
	memberPage, err := store.ListNotifications(ctx, member.ID, false, app.PageRequest{Limit: 10})
	if err != nil || len(memberPage.Items) != 1 || memberPage.Items[0].Kind != notification.KindProjectItemBlocked {
		t.Fatalf("member notifications = %#v, %v; want project notice only", memberPage.Items, err)
	}
	for _, value := range append(ownerPage.Items, memberPage.Items...) {
		if value.TaskID != "" || value.EventSequence != 0 || strings.Contains(value.Summary, "private") {
			t.Fatalf("source notification leaked Task or private content: %#v", value)
		}
	}
	memberNoticeID := memberPage.Items[0].ID
	if _, err := store.MarkNotificationRead(ctx, member.ID, memberNoticeID); err != nil {
		t.Fatalf("mark project notice read: %v", err)
	}

	currentProject, err := store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveProjectMember(ctx, owner.ID, projectValue.ID, member.ID, currentProject.Version, "source-notice-remove-01"); err != nil {
		t.Fatalf("remove project member: %v", err)
	}
	memberPage, err = store.ListNotifications(ctx, member.ID, false, app.PageRequest{Limit: 10})
	if err != nil || len(memberPage.Items) != 0 {
		t.Fatalf("revoked project member notifications = %#v, %v; want none", memberPage.Items, err)
	}
	if _, err := store.MarkNotificationRead(ctx, member.ID, memberNoticeID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked project member marked a project notification read: %v", err)
	}
	if pending, err := store.ListUnprocessedNotificationSourceEvents(ctx, 10); err != nil || len(pending) != 0 {
		t.Fatalf("unprocessed source events = %#v, %v; want none", pending, err)
	}
}

func TestNotificationSourceOutboxMigrationDownPreservesHistory(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createTestUser(t, store, 1, 1000, 2048)
	if _, err := pool.Exec(ctx, `INSERT INTO notification_source_events
(source_kind,source_id,source_version,event_kind,recipient_id)
VALUES ('goal','goal-down-guard',1,'goal_completed',$1)`, owner.ID); err != nil {
		t.Fatalf("insert source event history: %v", err)
	}
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 123); err == nil || !strings.Contains(err.Error(), "cannot roll back notification source events") {
		t.Fatalf("notification source migration rollback = %v, want data protection rejection", err)
	}
	var tableExists bool
	var eventCount int
	if err := pool.QueryRow(ctx, `SELECT to_regclass('notification_source_events') IS NOT NULL`).Scan(&tableExists); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_source_events`).Scan(&eventCount); err != nil || !tableExists || eventCount != 1 {
		t.Fatalf("source event history after rejected rollback = table %t, rows %d, err %v", tableExists, eventCount, err)
	}
}

func TestNotificationSourceOutboxMigrationDownUpRoundTrip(t *testing.T) {
	_, pool := newTemporaryStore(t)
	ctx := context.Background()
	provider, closeProvider, err := newMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProvider()
	if _, err := provider.DownTo(ctx, 123); err != nil {
		t.Fatalf("roll back empty notification source migration: %v", err)
	}
	if _, err := provider.UpTo(ctx, 124); err != nil {
		t.Fatalf("reapply notification source migration: %v", err)
	}
}
