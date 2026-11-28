package postgres

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
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
