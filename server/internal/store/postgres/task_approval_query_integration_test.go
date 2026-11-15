package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func TestPostgresTaskApprovalSnapshotsAndConversationPanelPaging(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	other := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	outsider := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	first, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "task-approval-query-first", "First approval"))
	if err != nil {
		t.Fatalf("submit first Task: %v", err)
	}
	submissions := []app.TaskSubmission{
		newTestTaskSubmission(t, owner.ID, "", "task-approval-query-second", "Second approval"),
		newTestTaskSubmission(t, owner.ID, "", "task-approval-query-third", "Third approval"),
	}
	created := []app.TaskSubmissionResult{first}
	for index := range submissions {
		submissions[index].Task.ConversationID = first.Conversation.ID
		submissions[index].Message.ConversationID = first.Conversation.ID
		value, err := store.SubmitTask(ctx, submissions[index])
		if err != nil {
			t.Fatalf("submit related Task %d: %v", index+1, err)
		}
		created = append(created, value)
	}
	separateConversationTask, err := store.SubmitTask(ctx, newTestTaskSubmission(t, owner.ID, "", "task-approval-query-separate", "Separate conversation"))
	if err != nil {
		t.Fatalf("submit separate-conversation Task: %v", err)
	}
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Approval query project", "Approval access fixture", "approval-query-project")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, other.ID, projectValue, project.RoleMember, 1,
		"approval-query-invite", "approval-query-accept"); err != nil {
		t.Fatalf("add approval query project member: %v", err)
	}
	projectConversation, _, err := store.CreateProjectConversation(ctx, owner.ID, conversation.Conversation{
		ID: mustID(t, domain.NewConversationID), OwnerID: owner.ID, ProjectID: projectValue.ID, Title: "Approval query project",
	}, testIdempotency(owner.ID, "project_conversation.create", "approval-query-conversation"))
	if err != nil {
		t.Fatalf("create approval query project conversation: %v", err)
	}
	projectTask, err := submitIndependentProjectTaskForTest(t, ctx, store, owner.ID, other.ID, projectValue.ID,
		projectConversation.ID, "project-test-model", "Project approval", "approval-query-project-task", nil)
	if err != nil {
		t.Fatalf("submit project approval Task: %v", err)
	}

	base := created[len(created)-1].Task.CreatedAt.Add(time.Minute).Truncate(time.Microsecond)
	statuses := []task.Status{task.StatusRunning, task.StatusCompleted, task.StatusRunning}
	updatedAt := []time.Time{base, base.Add(time.Minute), base}
	for index, value := range created {
		leaseOwner, leaseVersion := "", int64(0)
		var leaseExpiresAt *time.Time
		if statuses[index] == task.StatusRunning {
			leaseOwner = "approval-query-test-worker"
			leaseVersion = 1
			expiresAt := time.Now().UTC().Add(time.Hour)
			leaseExpiresAt = &expiresAt
		}
		if _, err := pool.Exec(ctx, `UPDATE tasks SET status=$2, updated_at=$3, lease_owner=$4, lease_version=$5, lease_expires_at=$6 WHERE id=$1`,
			value.Task.ID, statuses[index], updatedAt[index], leaseOwner, leaseVersion, leaseExpiresAt); err != nil {
			t.Fatalf("set Task status %s: %v", value.Task.ID, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='waiting_user' WHERE id = ANY($1::text[])`, []string{separateConversationTask.Task.ID, projectTask.Task.ID}); err != nil {
		t.Fatalf("set scoped approval Task states: %v", err)
	}
	approvalBase := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Microsecond)
	firstApprovalAt, expiredAt := approvalBase.Add(3*time.Minute), approvalBase.Add(4*time.Minute)
	insertApproval := func(taskID string, version int64, status, decision string, createdAt time.Time, expiresAt *time.Time, respondedAt *time.Time) string {
		t.Helper()
		id := mustID(t, domain.NewPendingRequestID)
		if _, err := pool.Exec(ctx, `INSERT INTO task_pending_requests
(id, task_id, version, kind, title, object, content, impact, call_id, tool_name, request_hash, runtime_generation, runtime_instance_id,
 control_version, status, decision, created_at, expires_at, responded_at)
VALUES ($1,$2,$3,'approval','Confirm access','Workspace','Read a selected file','Disclosure risk',$4,'morde_workspace_file_read',$5,0,'test-runtime',0,$6,$7,$8,$9,$10)`,
			id, taskID, version, "call-"+id, hashBytes("approval:"+id), status, decision, createdAt, expiresAt, respondedAt); err != nil {
			t.Fatalf("insert approval %s: %v", id, err)
		}
		return id
	}
	insertApproval(created[0].Task.ID, 1, "pending", "", firstApprovalAt, nil, nil)
	respondedAt := approvalBase.Add(5 * time.Minute)
	insertApproval(created[1].Task.ID, 1, "approved", "approve", approvalBase.Add(2*time.Minute), nil, &respondedAt)
	insertApproval(created[2].Task.ID, 1, "pending", "", approvalBase.Add(time.Minute), &expiredAt, nil)
	insertApproval(created[0].Task.ID, 2, "cancelled", "", approvalBase.Add(4*time.Minute), nil, &respondedAt)
	insertApproval(separateConversationTask.Task.ID, 1, "pending", "", approvalBase.Add(90*time.Second), nil, nil)
	insertApproval(projectTask.Task.ID, 1, "pending", "", approvalBase.Add(30*time.Second), nil, nil)

	page, err := store.ListTaskApprovals(ctx, owner.ID, nil, first.Conversation.ID, app.PageRequest{Limit: 2})
	if err != nil || len(page.Items) != 2 || page.NextCursor == "" || page.Items[0].Request.CreatedAt.Before(page.Items[1].Request.CreatedAt) ||
		page.PendingCountsByHostID[""] != 1 {
		t.Fatalf("first Task approval page = %+v err=%v", page, err)
	}
	lastPage, err := store.ListTaskApprovals(ctx, owner.ID, nil, first.Conversation.ID, app.PageRequest{Limit: 2, Cursor: page.NextCursor})
	if err != nil || len(lastPage.Items) != 2 || lastPage.NextCursor != "" || lastPage.PendingCountsByHostID[""] != 1 {
		t.Fatalf("last Task approval page = %+v err=%v", lastPage, err)
	}
	for _, item := range append(page.Items, lastPage.Items...) {
		if item.TaskID == created[1].Task.ID && (item.Request.Status != task.PendingRequestApproved || item.Request.Decision != task.PendingDecisionApprove || item.Request.RespondedAt == nil) {
			t.Fatalf("handled approval snapshot was not restored: %+v", item)
		}
		if item.TaskID == created[2].Task.ID && (item.Request.ExpiresAt == nil || !item.Request.ExpiresAt.Equal(expiredAt)) {
			t.Fatalf("expired approval context was lost: %+v", item)
		}
		if item.Request.Status == task.PendingRequestCancelled && (item.Request.Decision != "" || item.Request.RespondedAt == nil) {
			t.Fatalf("cancelled approval snapshot was not restored: %+v", item)
		}
	}
	outsiderPage, err := store.ListTaskApprovals(ctx, outsider.ID, nil, "", app.PageRequest{Limit: 10})
	if err != nil || len(outsiderPage.Items) != 0 || len(outsiderPage.PendingCountsByHostID) != 0 {
		t.Fatalf("another account read Task approvals: %+v err=%v", outsiderPage, err)
	}

	panel, err := store.ListConversationTasks(ctx, owner.ID, "", first.Conversation.ID, nil, app.ConversationTaskViewPanel, app.PageRequest{Limit: 2})
	if err != nil || len(panel.Items) != 2 || panel.NextCursor == "" || panel.Items[0].Status != task.StatusRunning || panel.Items[1].Status != task.StatusRunning ||
		panel.Items[0].UpdatedAt.Before(panel.Items[1].UpdatedAt) || (panel.Items[0].UpdatedAt.Equal(panel.Items[1].UpdatedAt) && panel.Items[0].ID <= panel.Items[1].ID) {
		t.Fatalf("first mixed Task panel page = %+v err=%v", panel, err)
	}
	panelNext, err := store.ListConversationTasks(ctx, owner.ID, "", first.Conversation.ID, nil, app.ConversationTaskViewPanel, app.PageRequest{Limit: 2, Cursor: panel.NextCursor})
	if err != nil || len(panelNext.Items) != 1 || panelNext.Items[0].Status != task.StatusCompleted || panelNext.NextCursor != "" {
		t.Fatalf("last mixed Task panel page = %+v err=%v", panelNext, err)
	}
	active, err := store.ListConversationTasks(ctx, owner.ID, "", first.Conversation.ID, nil, app.ConversationTaskViewActive, app.PageRequest{Limit: 10})
	if err != nil || len(active.Items) != 2 {
		t.Fatalf("active Task view changed while adding panel view: %+v err=%v", active, err)
	}
	recent, err := store.ListConversationTasks(ctx, owner.ID, "", first.Conversation.ID, nil, app.ConversationTaskViewRecent, app.PageRequest{Limit: 10})
	if err != nil || len(recent.Items) != 1 || recent.Items[0].Status != task.StatusCompleted {
		t.Fatalf("recent Task view changed while adding panel view: %+v err=%v", recent, err)
	}
	allApprovals, err := store.ListTaskApprovals(ctx, owner.ID, nil, "", app.PageRequest{Limit: 10})
	if err != nil || len(allApprovals.Items) != 6 || allApprovals.PendingCountsByHostID[""] != 3 {
		t.Fatalf("owner cross-conversation/project approval scope = %+v err=%v", allApprovals, err)
	}
	memberPage, err := store.ListTaskApprovals(ctx, other.ID, nil, projectConversation.ID, app.PageRequest{Limit: 10})
	if err != nil || len(memberPage.Items) != 1 || memberPage.Items[0].TaskID != projectTask.Task.ID || memberPage.PendingCountsByHostID[""] != 1 {
		t.Fatalf("current project member approval scope = %+v err=%v", memberPage, err)
	}
	currentProject, err := store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatalf("get current approval project version: %v", err)
	}
	if err := store.RemoveProjectMember(ctx, owner.ID, projectValue.ID, other.ID, currentProject.Version, "approval-query-remove-member"); err != nil {
		t.Fatalf("revoke project approval member: %v", err)
	}
	revokedPage, err := store.ListTaskApprovals(ctx, other.ID, nil, projectConversation.ID, app.PageRequest{Limit: 10})
	if err != nil || len(revokedPage.Items) != 0 || revokedPage.PendingCountsByHostID[""] != 0 {
		t.Fatalf("revoked project member retained approval access: %+v err=%v", revokedPage, err)
	}
}
