package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func TestPostgresTaskPlanInternalFileEvidenceReadRequiresCurrentBoundSources(t *testing.T) {
	ctx := context.Background()
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "plan-file-evidence-read", true)
	if len(fixture.host.Workspaces) != 1 {
		t.Fatalf("test Host workspace bindings = %d; want one", len(fixture.host.Workspaces))
	}
	binding := fixture.host.Workspaces[0]
	workspaceValue := binding.Workspace
	binding, err := fixture.route.store.TransitionWorkspaceBinding(ctx, fixture.host.ID, workspaceValue.ID,
		binding.BindingVersion, workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, workspaceValue.Revision)
	if err != nil {
		t.Fatal(err)
	}
	manifestSHA, err := workspace.ManifestSHA256([]workspace.File{})
	if err != nil {
		t.Fatal(err)
	}
	contextDigest, err := hex.DecodeString(manifestSHA)
	if err != nil {
		t.Fatal(err)
	}
	revision := workspaceValue.Revision
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE tasks SET workspace_id=$2,workspace_owner_id=$3,
input_revision=$4,workspace_checkpoint_revision=$4,context_digest=$5 WHERE id=$1`,
		fixture.task.ID, workspaceValue.ID, fixture.task.OwnerID, revision, contextDigest); err != nil {
		t.Fatal(err)
	}
	loaded, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	current := loaded.Task
	plan := task.Plan{TaskRevision: *current.CurrentRevision, Revision: 1, Goal: "Write and verify a report",
		Scope: "Write the confirmed Workspace file", VerificationMethod: "Read back the file", CostEstimateState: "unknown",
		Steps: []task.PlanStep{{ID: "write", Title: "Write report", Status: task.PlanStepCompleted,
			CompletionCriteria: []string{"the file is written and verified"}}}}
	current = commitTaskPlanForStepBinding(t, fixture, plan, "file-evidence-plan-1")

	content := "hello evidence"
	contentDigest := sha256.Sum256([]byte(content))
	callID := "call_file_write_evidence"
	callArguments, err := json.Marshal(map[string]string{"workspace_id": workspaceValue.ID, "path": "notes/result.txt", "content": content})
	if err != nil {
		t.Fatal(err)
	}
	requestDigest := sha256.Sum256(append([]byte("file_write:"), callArguments...))
	toolCall, sourceOperation := taskPlanStepOperationFixture(t, fixture, current, callID, "file_write", requestDigest[:])
	sourceOperation.WorkspaceID = workspaceValue.ID
	sourceOperation.BindingVersion = binding.BindingVersion
	sourceOperation.InputRevision = current.InputRevision
	sourceOperation.WorkspaceCheckpointRevision = current.WorkspaceCheckpointRevision
	preparedSource, replayed, err := fixture.route.store.CreateOperation(ctx, sourceOperation, current.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("create file_write source operation = %#v replayed=%t err=%v", preparedSource, replayed, err)
	}
	sourceStepID := commitToolCallStepForPlanBindingWithStatus(t, fixture, current, toolCall,
		"file-evidence-source-running", task.StepRunning, "")
	dispatchSource := taskPlanOperationUpdate(current, fixture.host, sourceOperation, 1, "write", task.OperationDispatched)
	if _, err := fixture.route.store.UpdateOperation(ctx, dispatchSource); err != nil {
		t.Fatalf("dispatch plan-bound file_write source: %v", err)
	}
	writeResult, err := json.Marshal(map[string]any{"path": "notes/result.txt", "bytes_written": len(content), "sha256": fmt.Sprintf("%x", contentDigest[:])})
	if err != nil {
		t.Fatal(err)
	}
	settleSource := taskPlanOperationUpdate(current, fixture.host, preparedSource, 0, "", task.OperationSucceeded)
	settleSource.Result = string(writeResult)
	if _, err := fixture.route.store.UpdateOperation(ctx, settleSource); err != nil {
		t.Fatalf("settle file_write source: %v", err)
	}
	commitToolCallStepForPlanBindingWithStatus(t, fixture, current, toolCall,
		"file-evidence-source-settled", task.StepSucceeded, sourceStepID)
	settledSource, err := fixture.route.store.GetOperation(ctx, current.OwnerID, current.ID, sourceOperation.Key)
	if err != nil {
		t.Fatal(err)
	}
	source := app.OperationEvidenceSource{OperationKey: settledSource.Key, CallID: callID,
		WorkspaceID: workspaceValue.ID, BindingVersion: binding.BindingVersion,
		RequestHash: append([]byte(nil), settledSource.RequestHash...), Result: settledSource.Result,
		Path: "notes/result.txt", Size: int64(len(content)), SHA256: fmt.Sprintf("%x", contentDigest[:])}
	sources := []app.OperationEvidenceSource{source}

	makeReadOperation := func(stepID string, proof []app.OperationEvidenceSource) task.Operation {
		t.Helper()
		key := fmt.Sprintf("agent:verify:v2:%s:%s:%d", stepID, workspaceValue.ID, binding.BindingVersion)
		requestHash, err := app.FileEvidenceReadRequestHash(key, proof)
		if err != nil {
			t.Fatalf("build internal file evidence request hash: %v", err)
		}
		operation := task.Operation{ID: mustID(t, domain.NewOperationID), TaskID: current.ID, Key: key,
			RequestHash: requestHash[:], HostID: current.HostID, WorkspaceID: workspaceValue.ID,
			BindingVersion: binding.BindingVersion, InputRevision: current.InputRevision,
			WorkspaceCheckpointRevision: current.WorkspaceCheckpointRevision, RuntimeGeneration: fixture.host.RuntimeGeneration,
			RuntimeInstanceID: fixture.host.RuntimeInstanceID, ControlVersion: fixture.host.ControlVersion,
			LeaseVersion: current.Lease.Version, RuntimeLeaseVersion: current.Lease.Version, Status: task.OperationPrepared}
		created, replayed, err := fixture.route.store.CreateOperation(ctx, operation, current.Lease.Owner)
		if err != nil || replayed {
			t.Fatalf("create internal file evidence read = %#v replayed=%t err=%v", created, replayed, err)
		}
		return created
	}
	dispatchRead := func(operation task.Operation, proof []app.OperationEvidenceSource) error {
		t.Helper()
		_, err := fixture.route.store.UpdateOperation(ctx, app.OperationUpdate{
			TaskID: current.ID, OperationKey: operation.Key, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
			RuntimeGeneration: fixture.host.RuntimeGeneration, RuntimeInstanceID: fixture.host.RuntimeInstanceID,
			ControlVersion: fixture.host.ControlVersion, RuntimeLeaseVersion: current.Lease.Version,
			Status: task.OperationDispatched, InternalPurpose: app.InternalOperationDispatchPurposeFileEvidenceRead,
			EvidenceSources: proof,
		})
		return err
	}

	eligible := makeReadOperation("s1234501", sources)
	if err := dispatchRead(eligible, sources); err != nil {
		t.Fatalf("dispatch exact plan-bound file evidence read: %v", err)
	}
	if err := dispatchRead(eligible, sources); err != nil {
		t.Fatalf("replay exact file evidence read proof: %v", err)
	}
	var verifyBindings int
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT count(*) FROM task_plan_step_execution_bindings WHERE task_id=$1 AND execution_id=$2`,
		current.ID, eligible.ID).Scan(&verifyBindings); err != nil || verifyBindings != 0 {
		t.Fatalf("internal verification operation has %d plan bindings, err=%v; want zero", verifyBindings, err)
	}

	missing := makeReadOperation("s1234502", sources)
	if err := dispatchRead(missing, nil); domain.CodeOf(err) != domain.CodeInvalidArgument {
		t.Fatalf("dispatch without source proof error=%v; want invalid argument", err)
	}
	changedSources := []app.OperationEvidenceSource{source}
	changedSources[0].RequestHash = append([]byte(nil), source.RequestHash...)
	changedSources[0].RequestHash[0] ^= 0xff
	changed := makeReadOperation("s1234503", sources)
	if err := dispatchRead(changed, changedSources); domain.CodeOf(err) != domain.CodeIdempotencyConflict {
		t.Fatalf("dispatch with changed source proof error=%v; want idempotency conflict", err)
	}

	failed := makeReadOperation("s1234504", sources)
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_operations SET status='failed' WHERE id=$1`, settledSource.ID); err != nil {
		t.Fatal(err)
	}
	if err := dispatchRead(failed, sources); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("dispatch with failed file_write source error=%v; want conflict", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_operations SET status='succeeded' WHERE id=$1`, settledSource.ID); err != nil {
		t.Fatal(err)
	}

	var savedPlanStepID, savedSessionID, savedCallID, savedToolName string
	var savedRequestDigest []byte
	var savedAuthorizationVersion int64
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT plan_step_id,agent_session_id,call_id,tool_name,request_digest,authorization_version
FROM task_plan_step_execution_bindings WHERE task_id=$1 AND execution_id=$2`, current.ID, settledSource.ID).Scan(
		&savedPlanStepID, &savedSessionID, &savedCallID, &savedToolName, &savedRequestDigest, &savedAuthorizationVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `DELETE FROM task_plan_step_execution_bindings WHERE task_id=$1 AND execution_id=$2`,
		current.ID, settledSource.ID); err != nil {
		t.Fatal(err)
	}
	unbound := makeReadOperation("s1234505", sources)
	if err := dispatchRead(unbound, sources); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("dispatch with unbound file_write source error=%v; want conflict", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `INSERT INTO task_plan_step_execution_bindings
(owner_id,task_id,task_revision,plan_revision,plan_step_id,execution_kind,execution_id,agent_session_id,call_id,tool_name,
connector_id,child_step_id,parent_step_id,request_digest,authorization_version)
VALUES ($1,$2,$3,1,$4,'operation',$5,$6,$7,$8,'','','',$9,$10)`, current.OwnerID, current.ID, *current.CurrentRevision,
		savedPlanStepID, settledSource.ID, savedSessionID, savedCallID, savedToolName, savedRequestDigest, savedAuthorizationVersion); err != nil {
		t.Fatal(err)
	}

	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_plan_step_execution_bindings SET authorization_version=$3
WHERE task_id=$1 AND execution_id=$2`, current.ID, settledSource.ID, current.Authorization.Version+1); err != nil {
		t.Fatal(err)
	}
	unauthorized := makeReadOperation("s1234506", sources)
	if err := dispatchRead(unauthorized, sources); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("dispatch with stale authorization binding error=%v; want conflict", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_plan_step_execution_bindings SET authorization_version=$3
WHERE task_id=$1 AND execution_id=$2`, current.ID, settledSource.ID, current.Authorization.Version); err != nil {
		t.Fatal(err)
	}

	modelCall, modelOperation := taskPlanStepOperationFixture(t, fixture, current, "call_model_without_plan_selector",
		"terminal_exec", taskPlanStepTestDigest("model call arguments"))
	modelOperation.WorkspaceID = workspaceValue.ID
	modelOperation.BindingVersion = binding.BindingVersion
	modelOperation.InputRevision = current.InputRevision
	modelOperation.WorkspaceCheckpointRevision = current.WorkspaceCheckpointRevision
	if _, _, err := fixture.route.store.CreateOperation(ctx, modelOperation, current.Lease.Owner); err != nil {
		t.Fatalf("create ordinary model operation: %v", err)
	}
	commitToolCallStepForPlanBinding(t, fixture, current, modelCall, "model-call-without-selector")
	if _, err := fixture.route.store.UpdateOperation(ctx, taskPlanOperationUpdate(current, fixture.host, modelOperation,
		0, "", task.OperationDispatched)); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("ordinary model operation without plan selector error=%v; want conflict", err)
	}

	plan.Revision = 2
	plan.Goal = "Write and verify the revised report"
	current = commitTaskPlanForStepBinding(t, fixture, plan, "file-evidence-plan-2")
	stalePlan := makeReadOperation("s1234507", sources)
	if err := dispatchRead(stalePlan, sources); domain.CodeOf(err) != domain.CodeConflict {
		t.Fatalf("dispatch with source bound only to stale plan revision error=%v; want conflict", err)
	}
}
