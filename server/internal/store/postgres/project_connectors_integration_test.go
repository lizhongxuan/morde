package postgres

import (
	"context"
	"testing"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
)

func TestPostgresProjectMCPConnectorOwnerInitiatorAndRevocationFences(t *testing.T) {
	store, pool := newTemporaryStore(t)
	ctx := context.Background()
	projectOwner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	connectorOwner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	outsider := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	projectValue := createProjectStoreFixture(t, ctx, store, projectOwner.ID,
		"Connector authorization", "Explicit project authorization", "project-mcp-connectors-01")
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, store, projectOwner.ID, connectorOwner.ID, projectValue,
		project.RoleMember, 1, "project-mcp-invite-01", "project-mcp-accept-01"); err != nil {
		t.Fatal(err)
	}
	connector := createEnabledMCPGrantConnectors(t, store, connectorOwner.ID)[0]
	grant, err := store.GrantProjectMCPConnector(ctx, connectorOwner.ID, projectValue.ID, connector.ID, 0)
	if err != nil || !grant.Grant.Active || grant.Grant.Version != 1 || grant.Grant.ConnectorOwnerID != connectorOwner.ID {
		t.Fatalf("connector owner explicitly grants project access = %#v, err=%v", grant, err)
	}
	for _, actorID := range []string{projectOwner.ID, connectorOwner.ID} {
		listed, err := store.ListProjectMCPConnectors(ctx, actorID, projectValue.ID)
		if err != nil || len(listed) != 1 || listed[0].Grant.ConnectorOwnerID != connectorOwner.ID || listed[0].Name == "" {
			t.Fatalf("list project connectors as %s = %#v, err=%v", actorID, listed, err)
		}
	}
	if listed, err := store.ListProjectMCPConnectors(ctx, outsider.ID, projectValue.ID); err != nil || len(listed) != 0 {
		t.Fatalf("nonmember project connector rows = %#v, err=%v", listed, err)
	}
	if err := store.AuthorizeProjectMCPConnector(ctx, projectValue.ID, projectOwner.ID, connectorOwner.ID, connector.ID, 1); err != nil {
		t.Fatalf("current project owner cannot use explicitly granted connector: %v", err)
	}
	if err := store.AuthorizeProjectMCPConnector(ctx, projectValue.ID, outsider.ID, connectorOwner.ID, connector.ID, 1); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("outsider authorization error = %v, want forbidden", err)
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
	conversationValue, replay, err := conversations.Create(ctx, principal(projectOwner.ID), projectValue.ID,
		app.CreateProjectConversationCommand{Title: "Project MCP", IdempotencyKey: "project-mcp-conversation"})
	if err != nil || replay {
		t.Fatalf("create project conversation = %#v replay=%t err=%v", conversationValue, replay, err)
	}

	appendTask := func(actorID, key string) app.TaskSubmissionResult {
		t.Helper()
		command := app.AppendMessageCommand{ModelID: "project-test-model", Content: "Use the granted MCP connector", IdempotencyKey: key,
			MCPConnectorIDs: []string{connector.ID}}
		result, err := conversations.AppendMessage(ctx, principal(actorID), projectValue.ID, conversationValue.ID,
			command)
		if err != nil {
			t.Fatalf("append project Task as %s: %v", actorID, err)
		}
		return result
	}
	firstTask := appendTask(connectorOwner.ID, "project-mcp-member-task-01")
	if firstTask.Task.OwnerID != projectOwner.ID || firstTask.Task.InitiatorID != connectorOwner.ID {
		t.Fatalf("project task identity = owner %s initiator %s", firstTask.Task.OwnerID, firstTask.Task.InitiatorID)
	}
	var taskOwner, taskInitiator, taskConnectorOwner, taskProjectID string
	var taskProjectGrantVersion int64
	if err := pool.QueryRow(ctx, `SELECT g.owner_id,t.initiator_id,g.connector_owner_id,g.project_id,g.project_grant_version
FROM task_mcp_connector_grants g JOIN tasks t ON t.id=g.task_id AND t.owner_id=g.owner_id
WHERE g.task_id=$1 AND g.connector_id=$2`, firstTask.Task.ID, connector.ID).
		Scan(&taskOwner, &taskInitiator, &taskConnectorOwner, &taskProjectID, &taskProjectGrantVersion); err != nil {
		t.Fatalf("read project Task connector snapshot: %v", err)
	}
	if taskOwner != projectOwner.ID || taskInitiator != connectorOwner.ID || taskConnectorOwner != connectorOwner.ID ||
		taskProjectID != projectValue.ID || taskProjectGrantVersion != 1 {
		t.Fatalf("project Task connector snapshot owner=%s initiator=%s connectorOwner=%s project=%s projectVersion=%d",
			taskOwner, taskInitiator, taskConnectorOwner, taskProjectID, taskProjectGrantVersion)
	}

	running, found, err := store.ClaimNextTask(ctx, "project-mcp-worker-01", time.Minute)
	if err != nil || !found || running.ID != firstTask.Task.ID {
		t.Fatalf("claim first project Task = %#v found=%t err=%v", running, found, err)
	}
	firstGrant := readProjectTaskMCPConnectorGrant(t, ctx, store, projectOwner.ID, running.ID, connector.ID)
	prepared, dispatched := prepareProjectMCPOperation(t, ctx, store, running, firstGrant, "project-owner-revoke")
	if _, err := store.RevokeProjectMCPConnector(ctx, projectOwner.ID, projectValue.ID, connector.ID, 1); err != nil {
		t.Fatalf("project owner revokes connector authorization: %v", err)
	}
	preparedAfterRevoke, err := store.GetMCPConnectorOperation(ctx, projectOwner.ID, running.ID, connector.ID, prepared.Key)
	if err != nil || preparedAfterRevoke.Status != task.OperationCancelled {
		t.Fatalf("prepared operation after project revoke = %#v, err=%v", preparedAfterRevoke, err)
	}
	dispatchedAfterRevoke, err := store.GetMCPConnectorOperation(ctx, projectOwner.ID, running.ID, connector.ID, dispatched.Key)
	if err != nil || dispatchedAfterRevoke.Status != task.OperationDispatched || !dispatchedAfterRevoke.CancelRequested {
		t.Fatalf("in-flight operation after project revoke = %#v, err=%v", dispatchedAfterRevoke, err)
	}
	if _, err := store.CompleteMCPConnectorOperation(ctx, dispatched, running.Lease.Owner, task.OperationSucceeded, "late result"); err == nil {
		t.Fatal("result commit succeeded after project authorization was revoked")
	}
	if err := store.MarkMCPConnectorOperationUnknown(ctx, dispatched, running.Lease.Owner, "call outcome unknown after project revoke"); err != nil {
		t.Fatalf("settle revoked in-flight operation as unknown: %v", err)
	}
	unknown, err := store.GetMCPConnectorOperation(ctx, projectOwner.ID, running.ID, connector.ID, dispatched.Key)
	if err != nil || unknown.Status != task.OperationUnknown {
		t.Fatalf("revoked operation recovery state = %#v, err=%v", unknown, err)
	}
	if err := store.AuthorizeProjectMCPConnector(ctx, projectValue.ID, projectOwner.ID, connectorOwner.ID, connector.ID, 1); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("stale project grant authorization error = %v, want forbidden", err)
	}

	// Regrant, then run a project-owner-initiated Task with another member's
	// connector. Removing that connector owner must fence both the snapshot and
	// a dispatched operation while preserving unknown side-effect handling.
	if _, err := store.GrantProjectMCPConnector(ctx, connectorOwner.ID, projectValue.ID, connector.ID, 2); err != nil {
		t.Fatalf("connector owner regrants project access: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET status='failed',finished_at=clock_timestamp() WHERE id=$1`, running.ID); err != nil {
		t.Fatalf("finish first project Task fixture: %v", err)
	}
	secondTask := appendTask(projectOwner.ID, "project-mcp-owner-task-02")
	if secondTask.Task.OwnerID != projectOwner.ID || secondTask.Task.InitiatorID != projectOwner.ID {
		t.Fatalf("second project task identity = owner %s initiator %s", secondTask.Task.OwnerID, secondTask.Task.InitiatorID)
	}
	secondRunning, found, err := store.ClaimNextTask(ctx, "project-mcp-worker-02", time.Minute)
	if err != nil || !found || secondRunning.ID != secondTask.Task.ID {
		t.Fatalf("claim second project Task = %#v found=%t err=%v", secondRunning, found, err)
	}
	secondGrant := readProjectTaskMCPConnectorGrant(t, ctx, store, projectOwner.ID, secondRunning.ID, connector.ID)
	_, secondDispatched := prepareProjectMCPOperation(t, ctx, store, secondRunning, secondGrant, "connector-owner-removal")
	currentProject, err := store.GetProject(ctx, projectOwner.ID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveProjectMember(ctx, projectOwner.ID, projectValue.ID, connectorOwner.ID, currentProject.Version,
		"project-mcp-remove-connector-owner"); err != nil {
		t.Fatalf("remove connector owner from project: %v", err)
	}
	var active bool
	var grantVersion int64
	if err := pool.QueryRow(ctx, `SELECT active,version FROM project_mcp_connector_grants WHERE project_id=$1 AND connector_id=$2`,
		projectValue.ID, connector.ID).Scan(&active, &grantVersion); err != nil || active || grantVersion != 4 {
		t.Fatalf("member removal left project connector grant active=%t version=%d err=%v", active, grantVersion, err)
	}
	if err := store.AuthorizeProjectMCPConnector(ctx, projectValue.ID, projectOwner.ID, connectorOwner.ID, connector.ID, 3); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("connector owner removal authorization error = %v, want forbidden", err)
	}
	removedOwnerOperation, err := store.GetMCPConnectorOperation(ctx, projectOwner.ID, secondRunning.ID, connector.ID, secondDispatched.Key)
	if err != nil || removedOwnerOperation.Status != task.OperationDispatched || !removedOwnerOperation.CancelRequested {
		t.Fatalf("in-flight operation after connector owner removal = %#v err=%v", removedOwnerOperation, err)
	}
	if _, err := store.CompleteMCPConnectorOperation(ctx, secondDispatched, secondRunning.Lease.Owner, task.OperationSucceeded, "late result"); err == nil {
		t.Fatal("result commit succeeded after connector owner left the project")
	}
	if err := store.MarkMCPConnectorOperationUnknown(ctx, secondDispatched, secondRunning.Lease.Owner, "call outcome unknown after owner removal"); err != nil {
		t.Fatalf("settle removed-owner operation as unknown: %v", err)
	}
}

func TestPostgresProjectMCPConnectorDeletionRevokesGrant(t *testing.T) {
	store, _ := newTemporaryStore(t)
	ctx := context.Background()
	owner := createPreRetirementTestUser(t, store, 4, 20_000, 40_000)
	projectValue := createProjectStoreFixture(t, ctx, store, owner.ID, "Connector delete", "Delete fence", "project-mcp-delete-01")
	connector := createEnabledMCPGrantConnectors(t, store, owner.ID)[0]
	if _, err := store.GrantProjectMCPConnector(ctx, owner.ID, projectValue.ID, connector.ID, 0); err != nil {
		t.Fatalf("grant project connector: %v", err)
	}
	if err := store.DeleteMCPConnector(ctx, owner.ID, connector.ID, connector.Revision, audit.Entry{
		ActorID: owner.ID, ActorKind: audit.ActorSession, Action: audit.ActionMCPConnectorDelete, Target: connector.ID,
	}); err != nil {
		t.Fatalf("delete project connector: %v", err)
	}
	var active bool
	var version int64
	var revokedAt *time.Time
	if err := store.pool.QueryRow(ctx, `SELECT active,version,revoked_at FROM project_mcp_connector_grants
WHERE project_id=$1 AND connector_id=$2`, projectValue.ID, connector.ID).Scan(&active, &version, &revokedAt); err != nil || active || version != 2 || revokedAt == nil {
		t.Fatalf("connector deletion grant active=%t version=%d revoked_at=%v err=%v", active, version, revokedAt, err)
	}
	if err := store.AuthorizeProjectMCPConnector(ctx, projectValue.ID, owner.ID, owner.ID, connector.ID, 1); domain.CodeOf(err) != domain.CodeForbidden {
		t.Fatalf("deleted connector authorization error = %v, want forbidden", err)
	}
}

func readProjectTaskMCPConnectorGrant(t *testing.T, ctx context.Context, store *Store, ownerID, taskID, connectorID string) app.TaskMCPConnectorGrant {
	t.Helper()
	grants, err := store.ListTaskMCPConnectorGrants(ctx, ownerID, taskID)
	if err != nil || len(grants) != 1 || grants[0].ConnectorID != connectorID || grants[0].RevokedAt != nil {
		t.Fatalf("read active project Task connector grant = %#v, err=%v", grants, err)
	}
	return grants[0]
}

func prepareProjectMCPOperation(t *testing.T, ctx context.Context, store *Store, running task.Task,
	grant app.TaskMCPConnectorGrant, key string) (app.MCPConnectorOperation, app.MCPConnectorOperation) {
	t.Helper()
	operation := app.MCPConnectorOperation{
		ID: mustID(t, domain.NewOperationID), TaskID: running.ID, OwnerID: running.OwnerID, ConnectorID: grant.ConnectorID,
		Key: key + "-prepared", ToolName: "search", ConnectorRevision: grant.ConnectorRevision,
		CredentialVersion: grant.CredentialVersion, GrantVersion: grant.GrantVersion, LeaseVersion: running.Lease.Version,
		HostID: running.HostID, WorkspaceID: running.WorkspaceID, InputRevision: running.InputRevision,
		WorkspaceCheckpointRevision: running.WorkspaceCheckpointRevision, RequestHash: hashBytes("request:" + key + "-prepared"),
	}
	if running.WorkspaceID != "" {
		binding, err := store.GetWorkspaceBinding(ctx, running.HostID, running.WorkspaceID)
		if err != nil {
			t.Fatalf("read project Task Workspace binding: %v", err)
		}
		operation.BindingVersion = binding.BindingVersion
	}
	prepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, operation, running.Lease.Owner)
	if err != nil || replayed || prepared.Status != task.OperationPrepared {
		t.Fatalf("prepare project MCP operation = %#v replay=%t err=%v", prepared, replayed, err)
	}
	dispatchedInput := operation
	dispatchedInput.ID = mustID(t, domain.NewOperationID)
	dispatchedInput.Key = key + "-dispatched"
	dispatchedInput.RequestHash = hashBytes("request:" + dispatchedInput.Key)
	dispatchedPrepared, replayed, err := store.PrepareMCPConnectorOperation(ctx, dispatchedInput, running.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("prepare dispatched project MCP operation = %#v replay=%t err=%v", dispatchedPrepared, replayed, err)
	}
	dispatched, err := store.MarkMCPConnectorOperationDispatched(ctx, dispatchedPrepared, running.Lease.Owner)
	if err != nil || dispatched.Status != task.OperationDispatched {
		t.Fatalf("dispatch project MCP operation = %#v err=%v", dispatched, err)
	}
	return prepared, dispatched
}
