package postgres

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func TestPostgresTaskStepOutputCapturePersistsPagesAndRechecksAuthorization(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "output-capture-receipt", true)
	ctx := context.Background()
	var workspaceID string
	var bindingVersion int64
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT workspace_id,binding_version FROM workspace_bindings
WHERE host_id=$1 AND owner_id=$2 ORDER BY created_at DESC LIMIT 1`, fixture.host.ID, fixture.task.OwnerID).
		Scan(&workspaceID, &bindingVersion); err != nil {
		t.Fatalf("read Host Workspace binding: %v", err)
	}
	workspaceValue, err := fixture.route.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		t.Fatalf("read Host Workspace: %v", err)
	}
	binding, err := fixture.route.store.GetWorkspaceBinding(ctx, fixture.host.ID, workspaceID)
	if err != nil {
		t.Fatalf("read Host Workspace binding: %v", err)
	}
	if binding.State == workspace.BindingPreparing {
		binding, err = fixture.route.store.TransitionWorkspaceBinding(ctx, fixture.host.ID, workspaceID, binding.BindingVersion,
			workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0)
		if err != nil {
			t.Fatalf("activate Host Workspace binding: %v", err)
		}
	}
	bindingVersion = binding.BindingVersion
	checkpointRevision := workspaceValue.Revision
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE tasks SET workspace_id=$2,workspace_owner_id=$3,
input_revision=$4,workspace_checkpoint_revision=$4,context_digest=$5 WHERE id=$1`,
		fixture.task.ID, workspaceID, fixture.task.OwnerID, checkpointRevision, hashBytes("output-capture input manifest")); err != nil {
		t.Fatalf("bind Task to Host Workspace: %v", err)
	}
	loaded, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil || loaded.Task.WorkspaceID != workspaceID || loaded.Task.CurrentRevision == nil ||
		loaded.Task.WorkspaceCheckpointRevision == nil || *loaded.Task.WorkspaceCheckpointRevision != checkpointRevision {
		t.Fatalf("load Workspace-bound Task: task=%+v err=%v", loaded.Task, err)
	}
	current := loaded.Task
	viewID := "v" + strings.Repeat("a", 64)
	sealer, err := app.NewSealer(bytesOf(32, 0x31))
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: current.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession}
	tasks, err := app.NewTaskService(fixture.route.store, k4FixtureAuthorization{}, k4FixtureModelCatalog{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}

	const callID = "call_output_capture_complete"
	stepID, operation := createTerminalShellOutputCaptureExecution(t, fixture, current, callID, workspaceID,
		bindingVersion, checkpointRevision)
	identityValue := app.TaskStepOutputCaptureIdentity{
		OwnerID: current.OwnerID, TaskID: current.ID, AuthorizationVersion: current.Authorization.Version,
		TaskRevision: *current.CurrentRevision, WorkspaceID: workspaceID, WorkspaceAuthorizationVersion: 0,
		WorkspaceBindingVersion: bindingVersion, WorkspaceViewID: viewID, WorkspaceCheckpointRevision: checkpointRevision,
		StepID: stepID, CallID: callID, OperationID: operation.ID,
	}
	stdout := strings.Repeat("x", app.TaskStepOutputCapturePageBytes+19) + " api_key=canary-secret"
	stderr := "safe stderr"
	record, err := app.SealTaskStepOutputCapture(sealer, identityValue, []byte(stdout), []byte(stderr))
	if err != nil {
		t.Fatalf("seal complete Task step output: %v", err)
	}
	if err := fixture.route.store.SaveTaskStepOutputCapture(ctx, record); err != nil {
		t.Fatalf("save complete Task step output: %v", err)
	}
	var receiptCount, pageCount int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_step_output_captures WHERE task_id=$1 AND step_id=$2`,
		current.ID, stepID).Scan(&receiptCount); err != nil || receiptCount != 1 {
		t.Fatalf("complete receipt count=%d err=%v; want one", receiptCount, err)
	}
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_step_output_capture_pages WHERE task_id=$1 AND step_id=$2`,
		current.ID, stepID).Scan(&pageCount); err != nil || pageCount < 3 {
		t.Fatalf("encrypted page count=%d err=%v; want stdout pagination plus stderr", pageCount, err)
	}
	rows, err := fixture.route.store.pool.Query(ctx, `SELECT payload_ciphertext FROM task_step_output_capture_pages
WHERE task_id=$1 AND step_id=$2`, current.ID, stepID)
	if err != nil {
		t.Fatalf("read encrypted page ciphertext: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ciphertext []byte
		if err := rows.Scan(&ciphertext); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(ciphertext), "canary-secret") {
			t.Fatal("encrypted page ciphertext contains the source credential")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("scan encrypted page ciphertext: %v", err)
	}

	page, err := tasks.GetStepOutputCapturePage(ctx, principal, current.ID, stepID, "stdout", 0)
	if err != nil || page.State != app.TaskStepOutputCaptureStateComplete || !page.Complete || page.EOF ||
		page.NextOffset != app.TaskStepOutputCapturePageBytes || len(page.Data) != app.TaskStepOutputCapturePageBytes {
		t.Fatalf("first authorized stdout page=%+v err=%v", page, err)
	}
	second, err := tasks.GetStepOutputCapturePage(ctx, principal, current.ID, stepID, "stdout", page.NextOffset)
	if err != nil || second.State != app.TaskStepOutputCaptureStateComplete || !second.Complete || !second.EOF {
		t.Fatalf("second authorized stdout page=%+v err=%v", second, err)
	}
	allOutput := string(page.Data) + string(second.Data)
	if strings.Contains(allOutput, "canary-secret") || !strings.Contains(allOutput, "[已隐藏]") {
		t.Fatalf("authorized output was not redacted: contains_secret=%t tail=%q", strings.Contains(allOutput, "canary-secret"), allOutput[len(allOutput)-64:])
	}
	status, err := fixture.route.store.GetTaskStepOutputCaptureStatus(ctx, current.OwnerID, current.ID, stepID)
	if err != nil || status.State != app.TaskStepOutputCaptureStateComplete || status.Identity != identityValue {
		t.Fatalf("complete capture status=%+v err=%v", status, err)
	}

	truncatedCallID := "call_output_capture_incomplete"
	truncatedStepID, truncatedOperation := createTerminalShellOutputCaptureExecution(t, fixture, current, truncatedCallID,
		workspaceID, bindingVersion, checkpointRevision)
	statusRecord := app.TaskStepOutputCaptureStatusRecord{Identity: app.TaskStepOutputCaptureIdentity{
		OwnerID: current.OwnerID, TaskID: current.ID, AuthorizationVersion: current.Authorization.Version,
		TaskRevision: *current.CurrentRevision, WorkspaceID: workspaceID, WorkspaceAuthorizationVersion: 0,
		WorkspaceBindingVersion: bindingVersion, WorkspaceViewID: viewID, WorkspaceCheckpointRevision: checkpointRevision,
		StepID: truncatedStepID, CallID: truncatedCallID, OperationID: truncatedOperation.ID,
	}, State: app.TaskStepOutputCaptureStateSourceTruncated}
	if err := fixture.route.store.SaveTaskStepOutputCaptureStatus(ctx, statusRecord); err != nil {
		t.Fatalf("persist incomplete output status: %v", err)
	}
	truncatedStatus, err := fixture.route.store.GetTaskStepOutputCaptureStatus(ctx, current.OwnerID, current.ID, truncatedStepID)
	if err != nil || truncatedStatus.State != app.TaskStepOutputCaptureStateSourceTruncated ||
		truncatedStatus.Identity != statusRecord.Identity {
		t.Fatalf("incomplete capture status=%+v err=%v", truncatedStatus, err)
	}

	stranger := createPreRetirementTestUser(t, fixture.route.store, 1, 2000, 4096)
	if _, err := fixture.route.store.GetTaskStepOutputCapturePage(ctx, stranger.ID, current.ID, stepID, "stdout", 0); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("foreign user read captured output: %v; want not found", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE tasks SET auth_version=auth_version+1 WHERE id=$1`, current.ID); err != nil {
		t.Fatalf("revoke Task authorization version: %v", err)
	}
	if _, err := fixture.route.store.GetTaskStepOutputCapturePage(ctx, current.OwnerID, current.ID, stepID, "stdout", 0); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("read captured output after authorization revocation: %v; want not found", err)
	}
	if _, err := fixture.route.store.GetTaskStepOutputCaptureStatus(ctx, current.OwnerID, current.ID, truncatedStepID); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("read incomplete output status after authorization revocation: %v; want not found", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `WITH stamp AS (SELECT clock_timestamp() AS now)
UPDATE task_step_output_captures SET created_at=stamp.now-interval '25 hours',expires_at=stamp.now-interval '1 hour'
FROM stamp WHERE task_id=$1 AND step_id=$2`, current.ID, stepID); err != nil {
		t.Fatalf("expire encrypted output receipt: %v", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `WITH stamp AS (SELECT clock_timestamp() AS now)
UPDATE task_step_output_capture_statuses SET created_at=stamp.now-interval '25 hours',expires_at=stamp.now-interval '1 hour'
FROM stamp WHERE task_id=$1 AND step_id=$2`, current.ID, truncatedStepID); err != nil {
		t.Fatalf("expire incomplete output status: %v", err)
	}
	removed, err := fixture.route.store.CleanupExpiredTaskStepOutputCaptures(ctx, 10)
	if err != nil || removed != 2 {
		t.Fatalf("clean expired output records removed=%d err=%v; want receipt and status", removed, err)
	}
	var remainingReceipts, remainingPages, remainingStatuses int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT
(SELECT count(*) FROM task_step_output_captures WHERE task_id=$1),
(SELECT count(*) FROM task_step_output_capture_pages WHERE task_id=$1),
(SELECT count(*) FROM task_step_output_capture_statuses WHERE task_id=$1)`, current.ID).
		Scan(&remainingReceipts, &remainingPages, &remainingStatuses); err != nil ||
		remainingReceipts != 0 || remainingPages != 0 || remainingStatuses != 0 {
		t.Fatalf("expired output rows remain: receipts=%d pages=%d statuses=%d err=%v", remainingReceipts,
			remainingPages, remainingStatuses, err)
	}
}

func TestPostgresTaskStepOutputCaptureRechecksProjectMemberRevocation(t *testing.T) {
	ctx := context.Background()
	route := newRouteActionsTestFixture(t)
	projectValue := createProjectStoreFixture(t, ctx, route.store, route.ownerID,
		"Output capture access", "Project member revocation fences", "output-capture-project-create")
	member := createPreRetirementTestUser(t, route.store, 1, 2000, 4096)
	if _, err := inviteAndAcceptProjectStoreMember(t, ctx, route.store, route.ownerID, member.ID,
		projectValue, project.RoleMember, projectValue.Version, "output-capture-project-invite", "output-capture-project-accept"); err != nil {
		t.Fatalf("add Project member: %v", err)
	}
	projectValue, err := route.store.GetProject(ctx, route.ownerID, projectValue.ID)
	if err != nil {
		t.Fatalf("reload Project after adding member: %v", err)
	}
	projectConversationID := mustID(t, domain.NewConversationID)
	projectConversation, _, err := route.store.CreateProjectConversation(ctx, route.ownerID, conversation.Conversation{
		ID: projectConversationID, OwnerID: route.ownerID, ProjectID: projectValue.ID, Title: "Output capture access",
	}, testIdempotency(route.ownerID, "project_conversation.create", "output-capture-project-conversation"))
	if err != nil {
		t.Fatalf("create Project conversation fixture: %v", err)
	}
	route.projectID = projectValue.ID
	route.conversation = projectConversation
	fixture := createS03RunningAgentTask(t, route, "output-capture-project-revocation", true)

	var workspaceID string
	var bindingVersion int64
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT workspace_id,binding_version FROM workspace_bindings
WHERE host_id=$1 AND owner_id=$2 ORDER BY created_at DESC LIMIT 1`, fixture.host.ID, fixture.task.OwnerID).
		Scan(&workspaceID, &bindingVersion); err != nil {
		t.Fatalf("read Host Workspace binding: %v", err)
	}
	workspaceValue, err := fixture.route.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		t.Fatalf("read Host Workspace: %v", err)
	}
	binding, err := fixture.route.store.GetWorkspaceBinding(ctx, fixture.host.ID, workspaceID)
	if err != nil {
		t.Fatalf("read Host Workspace binding: %v", err)
	}
	if binding.State == workspace.BindingPreparing {
		binding, err = fixture.route.store.TransitionWorkspaceBinding(ctx, fixture.host.ID, workspaceID, binding.BindingVersion,
			workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0)
		if err != nil {
			t.Fatalf("activate Host Workspace binding: %v", err)
		}
	}
	bindingVersion = binding.BindingVersion
	checkpointRevision := workspaceValue.Revision
	if _, err := fixture.route.store.UpsertHostMember(ctx, host.Member{HostID: fixture.host.ID,
		OwnerID: fixture.task.OwnerID, UserID: member.ID, Permissions: []host.Permission{host.PermissionWorkspaceFileRead}}); err != nil {
		t.Fatalf("grant Project member Workspace read access: %v", err)
	}
	projectValue, err = fixture.route.store.GetProject(ctx, fixture.task.OwnerID, projectValue.ID)
	if err != nil {
		t.Fatalf("reload Project before adding Workspace resource: %v", err)
	}
	projectResource, replay, err := fixture.route.store.AddProjectResource(ctx, fixture.task.OwnerID, project.Resource{
		ProjectID: projectValue.ID, Kind: project.ResourceWorkspace, ResourceID: workspaceID, Version: 1,
	}, projectValue.Version, "output-capture-project-workspace")
	if err != nil || replay || projectResource.Version != 1 {
		t.Fatalf("add Project Workspace resource = %#v replay=%t err=%v", projectResource, replay, err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE tasks SET workspace_id=$2,workspace_owner_id=$3,
	workspace_project_resource_version=$4,input_revision=$5,workspace_checkpoint_revision=$5,context_digest=$6 WHERE id=$1`,
		fixture.task.ID, workspaceID, fixture.task.OwnerID, projectResource.Version, checkpointRevision,
		hashBytes("output-capture project input manifest")); err != nil {
		t.Fatalf("bind Task to Project Workspace: %v", err)
	}
	loaded, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil || loaded.Task.ProjectID != projectValue.ID || loaded.Task.WorkspaceProjectResourceVersion == nil ||
		*loaded.Task.WorkspaceProjectResourceVersion != projectResource.Version {
		t.Fatalf("load Project-bound Task: task=%+v err=%v", loaded.Task, err)
	}
	current := loaded.Task
	viewID := "v" + strings.Repeat("b", 64)
	sealer, err := app.NewSealer(bytesOf(32, 0x32))
	if err != nil {
		t.Fatal(err)
	}
	projects, err := app.NewProjectService(fixture.route.store)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := app.NewTaskService(fixture.route.store, k4FixtureAuthorization{}, k4FixtureModelCatalog{}, projects)
	if err != nil {
		t.Fatal(err)
	}
	if err := tasks.SetTaskStepDetailSealer(sealer); err != nil {
		t.Fatal(err)
	}
	callID := "call_output_capture_project"
	stepID, operation := createTerminalShellOutputCaptureExecution(t, fixture, current, callID, workspaceID,
		bindingVersion, checkpointRevision)
	initiatorMember, err := fixture.route.store.GetProjectMember(ctx, projectValue.ID, current.InitiatorID)
	if err != nil {
		t.Fatalf("read Project task initiator membership: %v", err)
	}
	identityValue := app.TaskStepOutputCaptureIdentity{
		OwnerID: current.OwnerID, TaskID: current.ID, AuthorizationVersion: current.Authorization.Version,
		TaskRevision: *current.CurrentRevision, WorkspaceID: workspaceID, WorkspaceAuthorizationVersion: initiatorMember.Version,
		WorkspaceBindingVersion: bindingVersion, WorkspaceViewID: viewID, WorkspaceCheckpointRevision: checkpointRevision,
		StepID: stepID, CallID: callID, OperationID: operation.ID,
	}
	record, err := app.SealTaskStepOutputCapture(sealer, identityValue, []byte("project output page"), nil)
	if err != nil {
		t.Fatalf("seal Project Task output: %v", err)
	}
	if err := fixture.route.store.SaveTaskStepOutputCapture(ctx, record); err != nil {
		t.Fatalf("save Project Task output: %v", err)
	}
	principal := identity.Principal{UserID: member.ID, Role: identity.RoleUser, Kind: identity.KindSession, TokenID: testSessionTokenID(member.ID)}
	page, err := tasks.GetStepOutputCapturePage(ctx, principal, current.ID, stepID, "stdout", 0)
	if err != nil || string(page.Data) != "project output page" || !page.Complete {
		t.Fatalf("Project member initial page=%+v err=%v; want complete output", page, err)
	}

	currentProject, err := fixture.route.store.GetProject(ctx, fixture.task.OwnerID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.route.store.UpdateProjectMemberRole(ctx, fixture.task.OwnerID, projectValue.ID, member.ID,
		project.RoleViewer, currentProject.Version, "output-capture-project-demote"); err != nil {
		t.Fatalf("downgrade Project member: %v", err)
	}
	if _, err := tasks.GetStepOutputCapturePage(ctx, principal, current.ID, stepID, "stdout", 0); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("Project viewer read captured output: %v; want not found", err)
	}
	currentProject, err = fixture.route.store.GetProject(ctx, fixture.task.OwnerID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.route.store.UpdateProjectMemberRole(ctx, fixture.task.OwnerID, projectValue.ID, member.ID,
		project.RoleMember, currentProject.Version, "output-capture-project-restore"); err != nil {
		t.Fatalf("restore Project member role: %v", err)
	}
	if _, err := tasks.GetStepOutputCapturePage(ctx, principal, current.ID, stepID, "stdout", 0); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("re-authorized Project member read stale captured output: %v; want not found", err)
	}
	ownerPrincipal := identity.Principal{UserID: fixture.task.OwnerID, Role: identity.RoleUser, Kind: identity.KindSession,
		TokenID: testSessionTokenID(fixture.task.OwnerID)}
	if _, err := tasks.GetStepOutputCapturePage(ctx, ownerPrincipal, current.ID, stepID, "stdout", 0); err != nil {
		t.Fatalf("Project owner lost access after member role change: %v", err)
	}

	currentProject, err = fixture.route.store.GetProject(ctx, fixture.task.OwnerID, projectValue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.route.store.RemoveProjectResource(ctx, fixture.task.OwnerID, projectValue.ID,
		project.ResourceWorkspace, workspaceID, currentProject.Version, "output-capture-project-remove-workspace"); err != nil {
		t.Fatalf("remove Project Workspace resource: %v", err)
	}
	if _, err := tasks.GetStepOutputCapturePage(ctx, ownerPrincipal, current.ID, stepID, "stdout", 0); domain.CodeOf(err) != domain.CodeNotFound {
		t.Fatalf("read captured output after Workspace resource removal: %v; want not found", err)
	}
}

func TestPostgresTaskStepOutputCaptureSerializesConcurrentOwnerQuotaWrites(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "output-capture-quota-pressure", true)
	ctx := context.Background()
	const seedRecords = 30
	var workspaceID string
	var bindingVersion int64
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT workspace_id,binding_version FROM workspace_bindings
WHERE host_id=$1 AND owner_id=$2 ORDER BY created_at DESC LIMIT 1`, fixture.host.ID, fixture.task.OwnerID).
		Scan(&workspaceID, &bindingVersion); err != nil {
		t.Fatalf("read Host Workspace binding: %v", err)
	}
	workspaceValue, err := fixture.route.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		t.Fatalf("read Host Workspace: %v", err)
	}
	binding, err := fixture.route.store.GetWorkspaceBinding(ctx, fixture.host.ID, workspaceID)
	if err != nil {
		t.Fatalf("read Host Workspace binding: %v", err)
	}
	if binding.State == workspace.BindingPreparing {
		binding, err = fixture.route.store.TransitionWorkspaceBinding(ctx, fixture.host.ID, workspaceID, binding.BindingVersion,
			workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0)
		if err != nil {
			t.Fatalf("activate Host Workspace binding: %v", err)
		}
	}
	bindingVersion = binding.BindingVersion
	checkpointRevision := workspaceValue.Revision
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE tasks SET budget_tool_calls=100,workspace_id=$2,workspace_owner_id=$3,
input_revision=$4,workspace_checkpoint_revision=$4,context_digest=$5 WHERE id=$1`,
		fixture.task.ID, workspaceID, fixture.task.OwnerID, checkpointRevision, hashBytes("output-capture quota pressure manifest")); err != nil {
		t.Fatalf("bind Task to Host Workspace: %v", err)
	}
	loaded, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil || loaded.Task.Budget.ToolCalls < seedRecords+2 || loaded.Task.CurrentRevision == nil || loaded.Task.WorkspaceCheckpointRevision == nil ||
		*loaded.Task.WorkspaceCheckpointRevision != checkpointRevision {
		t.Fatalf("load Workspace-bound Task: task=%+v err=%v", loaded.Task, err)
	}
	sealer, err := app.NewSealer(bytesOf(32, 0x31))
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(strings.Repeat("x", app.TaskStepOutputCaptureMaxBytes))
	viewID := "v" + strings.Repeat("a", 64)
	buildRecord := func(callID string) app.TaskStepOutputCaptureRecord {
		current, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
		if err != nil {
			t.Fatalf("reload Task for output receipt %s: %v", callID, err)
		}
		stepID, operation := createTerminalShellOutputCaptureExecution(t, fixture, current.Task, callID,
			workspaceID, bindingVersion, checkpointRevision)
		identityValue := app.TaskStepOutputCaptureIdentity{
			OwnerID: current.Task.OwnerID, TaskID: current.Task.ID, AuthorizationVersion: current.Task.Authorization.Version,
			TaskRevision: *current.Task.CurrentRevision, WorkspaceID: workspaceID, WorkspaceAuthorizationVersion: 0,
			WorkspaceBindingVersion: bindingVersion, WorkspaceViewID: viewID, WorkspaceCheckpointRevision: checkpointRevision,
			StepID: stepID, CallID: callID, OperationID: operation.ID,
		}
		record, err := app.SealTaskStepOutputCapture(sealer, identityValue, payload, nil)
		if err != nil {
			t.Fatalf("seal quota-pressure output %s: %v", callID, err)
		}
		return record
	}
	first := buildRecord("call_output_capture_quota_seed_00")
	perRecordBytes := taskStepOutputCaptureStorageBytes(first)
	remainingBytes := app.TaskStepOutputCaptureOwnerQuotaBytes - seedRecords*perRecordBytes
	if perRecordBytes < 1 || remainingBytes < perRecordBytes || remainingBytes >= 2*perRecordBytes {
		t.Fatalf("quota pressure fixture does not leave room for exactly one record: record=%d remaining=%d quota=%d",
			perRecordBytes, remainingBytes, app.TaskStepOutputCaptureOwnerQuotaBytes)
	}
	if err := fixture.route.store.SaveTaskStepOutputCapture(ctx, first); err != nil {
		t.Fatalf("save first quota seed receipt: %v", err)
	}
	for index := 1; index < seedRecords; index++ {
		record := buildRecord(fmt.Sprintf("call_output_capture_quota_seed_%02d", index))
		if storageBytes := taskStepOutputCaptureStorageBytes(record); storageBytes != perRecordBytes {
			t.Fatalf("quota seed %d storage bytes=%d, want %d", index, storageBytes, perRecordBytes)
		}
		if err := fixture.route.store.SaveTaskStepOutputCapture(ctx, record); err != nil {
			t.Fatalf("save quota seed receipt %d: %v", index, err)
		}
	}
	candidates := []app.TaskStepOutputCaptureRecord{
		buildRecord("call_output_capture_quota_contender_a"),
		buildRecord("call_output_capture_quota_contender_b"),
	}
	start := make(chan struct{})
	results := make(chan error, len(candidates))
	for _, candidate := range candidates {
		go func(record app.TaskStepOutputCaptureRecord) {
			<-start
			results <- fixture.route.store.SaveTaskStepOutputCapture(ctx, record)
		}(candidate)
	}
	close(start)
	succeeded, rejected := 0, 0
	for range candidates {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case domain.CodeOf(err) == domain.CodeQuotaExceeded:
			rejected++
		default:
			t.Fatalf("concurrent output receipt failed outside quota: %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("concurrent quota results: succeeded=%d quota_rejected=%d; want 1 each", succeeded, rejected)
	}
	var ownerRows int
	var ownerBytes int64
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*),COALESCE(sum(storage_bytes),0) FROM task_step_output_captures
WHERE owner_id=$1 AND expires_at>clock_timestamp()`, fixture.task.OwnerID).Scan(&ownerRows, &ownerBytes); err != nil {
		t.Fatalf("read post-race owner quota: %v", err)
	}
	if ownerRows != seedRecords+1 || ownerBytes > app.TaskStepOutputCaptureOwnerQuotaBytes {
		t.Fatalf("owner quota after concurrent writes: rows=%d bytes=%d cap=%d; want %d rows within cap",
			ownerRows, ownerBytes, app.TaskStepOutputCaptureOwnerQuotaBytes, seedRecords+1)
	}
	for _, candidate := range candidates {
		var receiptExists bool
		if err := fixture.route.store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_step_output_captures
WHERE task_id=$1 AND step_id=$2)`, candidate.Identity.TaskID, candidate.Identity.StepID).Scan(&receiptExists); err != nil {
			t.Fatalf("check concurrent candidate receipt: %v", err)
		}
		if !receiptExists {
			var pageCount int
			if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_step_output_capture_pages
WHERE task_id=$1 AND step_id=$2`, candidate.Identity.TaskID, candidate.Identity.StepID).Scan(&pageCount); err != nil {
				t.Fatalf("check rejected candidate pages: %v", err)
			}
			if pageCount != 0 {
				t.Fatalf("rejected candidate left %d encrypted pages without a receipt", pageCount)
			}
		}
	}
}

func createTerminalShellOutputCaptureExecution(t *testing.T, fixture s03AgentTaskFixture, current task.Task,
	callID, workspaceID string, bindingVersion, checkpointRevision int64) (string, task.Operation) {
	t.Helper()
	ctx := context.Background()
	digest := sha256.Sum256([]byte("shell_exec:" + callID))
	toolCall, operation := taskPlanStepOperationFixture(t, fixture, current, callID, "shell_exec", digest[:])
	toolCall.Phase = "execute"
	operation.WorkspaceID = workspaceID
	operation.BindingVersion = bindingVersion
	operation.InputRevision = current.InputRevision
	operation.WorkspaceCheckpointRevision = &checkpointRevision
	created, replayed, err := fixture.route.store.CreateOperation(ctx, operation, current.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("create Shell output capture operation: operation=%+v replayed=%t err=%v", created, replayed, err)
	}
	for _, status := range []task.OperationStatus{task.OperationDispatched, task.OperationSucceeded} {
		update := taskPlanOperationUpdate(current, fixture.host, created, 0, "", status)
		update.Result = `{"stdout":"redacted","stderr":"","exit_code":0,"timed_out":false}`
		if _, err := fixture.route.store.UpdateOperation(ctx, update); err != nil {
			t.Fatalf("settle Shell output capture operation to %s: %v", status, err)
		}
	}
	stepID := commitToolCallStepForPlanBindingWithStatus(t, fixture, current, toolCall,
		"output-capture-"+callID, task.StepSucceeded, "")
	return stepID, created
}
