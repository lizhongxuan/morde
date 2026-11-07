package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func TestPostgresProjectTaskRevocationRequiresAgentStopEvidence(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	member := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	outsider := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Revoked Task", "In-flight project Task history", "project-revoked-task")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, owner.ID, member.ID, projectValue, project.RoleMember, 1,
		"project-revoked-task-invite", "project-revoked-task-accept"); err != nil {
		t.Fatalf("accept project member: %v", err)
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
	conversationValue, replay, err := conversations.Create(ctx, principal(owner.ID), projectValue.ID, app.CreateProjectConversationCommand{
		Title: "Revocation test", IdempotencyKey: "project-revoked-task-conversation",
	})
	if err != nil || replay {
		t.Fatalf("create project conversation = %#v replay=%t err=%v", conversationValue, replay, err)
	}
	submission, err := submitIndependentProjectTaskForTest(t, ctx, store, owner.ID, member.ID, projectValue.ID, conversationValue.ID,
		"project-test-model", "summarize the project plan", "project-revoked-task-message", nil)
	if err != nil || submission.Task.OwnerID != owner.ID || submission.Task.InitiatorID != member.ID ||
		submission.Task.ProjectID != projectValue.ID || submission.Message.ActorID != member.ID {
		t.Fatalf("append member project Task = %#v err=%v", submission, err)
	}

	running, found, err := store.ClaimNextTask(ctx, "project-revocation-task-worker", time.Minute)
	if err != nil || !found || running.ID != submission.Task.ID || running.Status != task.StatusRunning {
		t.Fatalf("claim project Task = %#v found=%t err=%v", running, found, err)
	}
	supervisorID := "project-revocation-supervisor"
	stopKey := make([]byte, 32)
	stopKey[0] = 41
	authenticator, err := app.NewAgentStopAuthenticator(supervisorID, stopKey)
	if err != nil {
		t.Fatal(err)
	}
	agentService, err := app.NewAgentInstanceService(store, authenticator)
	if err != nil {
		t.Fatal(err)
	}
	supervisorNonce := make([]byte, 32)
	supervisorNonce[0] = 1
	start := app.AgentInstanceStart{
		Fence: app.AgentInstanceFence{
			TaskID: running.ID, OwnerID: owner.ID, InitiatorID: member.ID,
			AgentInstanceID: "project-agent-1", AgentSessionID: "project-session-1", SupervisorID: supervisorID,
			LeaseOwner: running.Lease.Owner, LeaseVersion: running.Lease.Version,
			AuthorizationVersion: running.Authorization.Version, EngineVersion: "0.2.0-test", ProfileVersion: "v1",
		},
		SupervisorNonce: supervisorNonce, LeaseDuration: 20 * time.Second,
	}
	agentLease, replayed, err := agentService.Start(ctx, start)
	if err != nil || replayed || agentLease.Fence.TaskID != running.ID {
		t.Fatalf("start project Task Agent = %#v replayed=%t err=%v", agentLease, replayed, err)
	}

	currentProject, err := store.GetProject(ctx, owner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveProjectMember(ctx, owner.ID, projectValue.ID, member.ID, currentProject.Version, "project-revoked-task-remove-member"); err != nil {
		t.Fatalf("revoke active Task initiator: %v", err)
	}

	_, err = store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: running.ID, LeaseOwner: running.Lease.Owner, LeaseVersion: running.Lease.Version,
		Status: task.StatusCompleted, Result: "unauthorized completion",
		Event: task.Event{TaskID: running.ID, Type: "task.result", Payload: []byte(`{"result":"unauthorized completion"}`)},
	})
	if domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("revoked member Task progress error = %v, want stale lease rejection", err)
	}

	var status, result string
	var leaseOwner string
	var leaseVersion int64
	if err := store.pool.QueryRow(ctx, `SELECT status,result,lease_owner,lease_version FROM tasks WHERE id=$1`, running.ID).
		Scan(&status, &result, &leaseOwner, &leaseVersion); err != nil {
		t.Fatal(err)
	}
	if status != string(task.StatusCancelling) || result != "" || leaseOwner != "" || leaseVersion != running.Lease.Version+1 {
		t.Fatalf("Task after revoked completion attempt = status %q result %q lease %q/%d; want fenced cancellation", status, result, leaseOwner, leaseVersion)
	}

	stopClaim, found, err := store.ClaimNextTaskStop(ctx, "project-revocation-stop-worker", time.Minute)
	if err != nil || !found || stopClaim.Task.ID != running.ID || stopClaim.Request.Action != task.ActionCancel {
		t.Fatalf("claim authorization cancellation = %#v found=%t err=%v", stopClaim, found, err)
	}
	if _, err := store.CompleteTaskStop(ctx, running.ID, stopClaim.Request.ID, stopClaim.Request.LeaseOwner,
		stopClaim.Request.LeaseVersion); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("task stop completed before safe_after: %v", err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE task_stop_requests SET safe_after=clock_timestamp()-interval '1 second' WHERE id=$1`, stopClaim.Request.ID); err != nil {
		t.Fatalf("advance stop safe_after for evidence check: %v", err)
	}
	if _, err := store.CompleteTaskStop(ctx, running.ID, stopClaim.Request.ID, stopClaim.Request.LeaseOwner,
		stopClaim.Request.LeaseVersion); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("task stop completed without Agent stop evidence: %v", err)
	}
	var agentState, stopState string
	if err := store.pool.QueryRow(ctx, `SELECT state FROM task_agent_instances WHERE task_id=$1 AND agent_generation=$2`,
		running.ID, agentLease.Fence.AgentGeneration).Scan(&agentState); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT status FROM task_stop_requests WHERE task_id=$1 AND id=$2`,
		running.ID, stopClaim.Request.ID).Scan(&stopState); err != nil {
		t.Fatal(err)
	}
	if agentState != "active" || stopState != string(task.StopRequestPending) {
		t.Fatalf("stop without evidence changed Agent/stop state: agent=%q stop=%q", agentState, stopState)
	}
	if _, err := store.CommitTaskProgress(ctx, app.TaskProgress{
		TaskID: running.ID, LeaseOwner: running.Lease.Owner, LeaseVersion: running.Lease.Version,
		Status: task.StatusCompleted, Result: "late unauthorized completion",
		Event: task.Event{TaskID: running.ID, Type: "task.result", Payload: []byte(`{"result":"late unauthorized completion"}`)},
	}); domain.CodeOf(err) != domain.CodeInvalidState {
		t.Fatalf("fenced Task accepted late completion = %v", err)
	}

	if _, err := conversations.ListMessages(ctx, principal(member.ID), projectValue.ID, conversationValue.ID, app.PageRequest{Limit: 20}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked member retained project history access: %v", err)
	}
	if _, err := conversations.ListMessages(ctx, principal(outsider.ID), projectValue.ID, conversationValue.ID, app.PageRequest{Limit: 20}); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("outsider read project history: %v", err)
	}
	if _, err := store.GetTask(ctx, running.ID, member.ID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked member retained Task access: %v", err)
	}
	ownerHistory, err := conversations.ListMessages(ctx, principal(owner.ID), projectValue.ID, conversationValue.ID, app.PageRequest{Limit: 20})
	if err != nil || len(ownerHistory.Items) != 1 || ownerHistory.Items[0].ActorID != member.ID || ownerHistory.Items[0].Content != "summarize the project plan" {
		t.Fatalf("remaining project owner history = %#v err=%v", ownerHistory, err)
	}
	ownerTask, err := store.GetTask(ctx, running.ID, owner.ID)
	if err != nil || ownerTask.Task.OwnerID != owner.ID || ownerTask.Task.InitiatorID != member.ID || ownerTask.Task.ProjectID != projectValue.ID ||
		ownerTask.Task.Status != task.StatusCancelling || ownerTask.Task.Result != "" {
		t.Fatalf("project Task attribution/state after revocation = %#v err=%v", ownerTask.Task, err)
	}
	eventPage, err := store.GetTaskEventPage(ctx, owner.ID, running.ID, 0, 100)
	if err != nil || len(eventPage.Items) == 0 {
		t.Fatalf("remaining owner Task events = %#v err=%v", eventPage, err)
	}
	for _, event := range eventPage.Items {
		if event.Type == "task.result" {
			t.Fatalf("revoked Task recorded a completion result: %#v", event)
		}
	}
	if _, err := store.GetTaskEventPage(ctx, member.ID, running.ID, 0, 100); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("revoked member retained Task event access: %v", err)
	}

	receipt := signAgentLifecycleReceipt(t, stopKey, agentLease.Fence, supervisorNonce)
	if _, err := agentService.ConfirmStopped(ctx, receipt); err != nil {
		t.Fatalf("record verified Agent stop after project revocation: %v", err)
	}
	cancelled, err := store.CompleteTaskStop(ctx, running.ID, stopClaim.Request.ID, stopClaim.Request.LeaseOwner,
		stopClaim.Request.LeaseVersion)
	if err != nil || cancelled.Status != task.StatusCancelled || cancelled.Result != "" {
		t.Fatalf("finalize project Task cancellation after stop evidence = %#v err=%v", cancelled, err)
	}
}
