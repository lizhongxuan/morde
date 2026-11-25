package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

func TestPostgresTaskValidationPlanCheckReplayVerifiesReceiptAndSources(t *testing.T) {
	fixture := createS03RunningAgentTask(t, newRouteActionsTestFixture(t), "h07-plan-check-replay", true)
	ctx := context.Background()
	var workspaceID string
	var bindingVersion int64
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT workspace_id,binding_version FROM workspace_bindings
WHERE host_id=$1 AND owner_id=$2 ORDER BY created_at DESC LIMIT 1`, fixture.host.ID, fixture.task.OwnerID).
		Scan(&workspaceID, &bindingVersion); err != nil {
		t.Fatalf("read existing Host Workspace binding: %v", err)
	}
	workspaceValue, err := fixture.route.store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		t.Fatalf("read existing Host Workspace: %v", err)
	}
	binding, err := fixture.route.store.GetWorkspaceBinding(ctx, fixture.host.ID, workspaceID)
	if err != nil {
		t.Fatalf("read existing Host Workspace binding: %v", err)
	}
	if binding.State == workspace.BindingPreparing {
		binding, err = fixture.route.store.TransitionWorkspaceBinding(ctx, fixture.host.ID, workspaceID, binding.BindingVersion,
			workspace.BindingPreparing, workspace.BindingActive, workspaceValue.Revision, 0)
		if err != nil {
			t.Fatalf("activate existing Host Workspace binding: %v", err)
		}
	}
	bindingVersion = binding.BindingVersion
	inputRevision := workspaceValue.Revision
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE tasks
SET workspace_id=$2,workspace_owner_id=$3,input_revision=$4,workspace_checkpoint_revision=$4,context_digest=$5
WHERE id=$1`, fixture.task.ID, workspaceValue.ID, fixture.task.OwnerID, inputRevision, hashBytes("H07 input manifest")); err != nil {
		t.Fatalf("bind Task Workspace checkpoint: %v", err)
	}
	current, err := fixture.route.store.GetTask(ctx, fixture.task.ID, fixture.task.OwnerID)
	if err != nil || current.Task.WorkspaceID != workspaceValue.ID || current.Task.InputRevision == nil ||
		current.Task.WorkspaceCheckpointRevision == nil {
		t.Fatalf("load bound Task Workspace: task=%+v err=%v", current.Task, err)
	}
	var sourceMessageID, sourceContent string
	if err := fixture.route.store.pool.QueryRow(ctx, `SELECT id,content FROM conversation_messages
WHERE task_id=$1 AND role='user' ORDER BY sequence LIMIT 1`, current.Task.ID).Scan(&sourceMessageID, &sourceContent); err != nil {
		t.Fatalf("read exact user requirement source: %v", err)
	}
	sourceDigest := sha256.Sum256([]byte(sourceContent))
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRequired,
		Command: "go test ./...", CoveragePaths: []string{"server"},
		RequirementSource: &task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage,
			MessageID: sourceMessageID, ContentSHA256: hex.EncodeToString(sourceDigest[:])}}
	plan := task.Plan{TaskRevision: *current.Task.CurrentRevision, Revision: 1, Goal: "Fix the requested issue",
		Scope: "server implementation", VerificationMethod: "run the declared test check", CostEstimateState: "unknown",
		FixPlan: &task.FixPlan{ChangedFiles: []string{"server/internal/app/task.go"}, Risks: []string{"regression"}, Alternatives: []string{"revert"}},
		Steps: []task.PlanStep{{ID: "verify", Title: "Run the required tests", Status: task.PlanStepPending,
			CompletionCriteria:        []string{"the required test check passes"},
			VerificationApplicability: task.PlanVerificationApplicabilityCheck, VerificationCheck: &check}}}
	current.Task = commitTaskPlanForStepBinding(t, fixture, plan, "call_h07_plan")

	callID := "call_h07_shell_check"
	requestDigest := taskPlanStepTestDigest("shell_exec:{\"command\":\"go test ./...\"}")
	toolCall, operation := taskPlanStepOperationFixture(t, fixture, current.Task, callID, "shell_exec", requestDigest)
	operation.WorkspaceID, operation.BindingVersion = workspaceValue.ID, bindingVersion
	operation.InputRevision, operation.WorkspaceCheckpointRevision = current.Task.InputRevision, current.Task.WorkspaceCheckpointRevision
	created, replayed, err := fixture.route.store.CreateOperation(ctx, operation, current.Task.Lease.Owner)
	if err != nil || replayed {
		t.Fatalf("create typed test operation: operation=%+v replayed=%t err=%v", created, replayed, err)
	}
	commitToolCallStepForPlanBinding(t, fixture, current.Task, toolCall, "h07-check-result")
	if _, err := fixture.route.store.UpdateOperation(ctx, taskPlanOperationUpdate(current.Task, fixture.host, created,
		plan.Revision, "verify", task.OperationDispatched)); err != nil {
		t.Fatalf("bind typed test operation to plan step: %v", err)
	}
	if _, err := fixture.route.store.UpdateOperation(ctx, taskPlanOperationUpdate(current.Task, fixture.host, created,
		0, "", task.OperationSucceeded)); err != nil {
		t.Fatalf("settle typed test operation: %v", err)
	}
	commandSHA := sha256.Sum256([]byte(check.Command))
	checkBytes, err := json.Marshal(check)
	if err != nil {
		t.Fatal(err)
	}
	checkSHA := sha256.Sum256(checkBytes)
	coverageBytes, err := json.Marshal(check.CoveragePaths)
	if err != nil {
		t.Fatal(err)
	}
	coverageSHA := sha256.Sum256(coverageBytes)
	checkpointRevision := *current.Task.WorkspaceCheckpointRevision
	manifestDigest := hex.EncodeToString(make([]byte, sha256.Size))
	viewDigest := hex.EncodeToString(make([]byte, sha256.Size))
	viewID := "v" + viewDigest
	receipt := app.TaskValidationCheckExecutionReceipt{Version: 2, TaskRevision: plan.TaskRevision, PlanRevision: plan.Revision,
		CheckID: "verify", Kind: string(check.Kind), Basis: string(check.Basis),
		CommandSHA256: hex.EncodeToString(commandSHA[:]), CheckSHA256: hex.EncodeToString(checkSHA[:]),
		CoverageSHA256: hex.EncodeToString(coverageSHA[:]), RequestSHA256: hex.EncodeToString(requestDigest)}
	runtimeResult, err := json.Marshal(map[string]any{
		"exit_code": 0, "timed_out": false, "stdout": "", "stderr": "", "stdout_truncated": false, "stderr_truncated": false,
		"task_id": current.Task.ID, "workspace_id": workspaceValue.ID, "binding_version": bindingVersion,
		"workspace_view_id": viewID, "workspace_checkpoint_revision": checkpointRevision,
		"workspace_snapshot_sha256_before": manifestDigest, "workspace_snapshot_sha256_after": manifestDigest,
		"verification_check": receipt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_operations SET result=$2 WHERE id=$1`, created.ID, string(runtimeResult)); err != nil {
		t.Fatalf("store typed test operation result: %v", err)
	}
	claim := app.TaskValidationReviewClaim{TaskID: current.Task.ID, OwnerID: current.Task.OwnerID,
		InitiatorID: current.Task.InitiatorID, ConversationID: current.Task.ConversationID,
		TaskRevision: plan.TaskRevision, ResultEventSequence: 1, ResultSHA256: hashBytes("review result"),
		ClaimGeneration: 1, LeaseOwner: "h07-review-worker"}
	loadReplay := func(proofs ...*app.TaskValidationFinalWorkspaceSnapshotProof) *app.TaskValidationReplayEvidence {
		t.Helper()
		tx, err := fixture.route.store.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		storedPlan, err := latestTaskPlan(ctx, tx, claim.TaskID, claim.TaskRevision)
		if err != nil || storedPlan == nil {
			t.Fatalf("read current typed verification plan: plan=%+v err=%v", storedPlan, err)
		}
		taskFacts, err := readTaskValidationReplayTask(ctx, tx, claim)
		if err != nil {
			t.Fatalf("read current Task Workspace facts: %v", err)
		}
		evidence, err := loadTaskValidationPlanCheckReplay(ctx, tx, claim, *storedPlan, taskFacts, proofs...)
		if err != nil {
			t.Fatalf("read typed verification replay: %v", err)
		}
		return evidence
	}
	if got := loadReplay(); got.State != app.TaskValidationReplayPending || got.RequirementSetVerified ||
		got.FinalWorkspaceSnapshotVerified || len(got.Checks) != 1 ||
		got.Checks[0].Status != app.TaskValidationReplayPass || !got.Checks[0].RequirementSourceVerified ||
		got.Checks[0].OperationID != created.ID || got.Checks[0].RunSequence < 1 {
		t.Fatalf("typed plan check replay = %+v; want a receipt bound to the exact message, operation, and step, with H07 pending until a current Workspace snapshot is independently verified", got)
	}
	legacyReceipt := receipt
	legacyReceipt.Version = 1
	legacyResult, err := json.Marshal(map[string]any{
		"exit_code": 0, "timed_out": false, "stdout": "", "stderr": "", "stdout_truncated": false, "stderr_truncated": false,
		"task_id": current.Task.ID, "workspace_id": workspaceValue.ID, "binding_version": bindingVersion,
		"workspace_view_id": viewID, "workspace_checkpoint_revision": checkpointRevision,
		"verification_check": legacyReceipt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_operations SET result=$2 WHERE id=$1`, created.ID, string(legacyResult)); err != nil {
		t.Fatalf("store legacy typed test operation result: %v", err)
	}
	if got := loadReplay(); len(got.Checks) != 1 || got.Checks[0].Status != app.TaskValidationReplayPending || got.State != app.TaskValidationReplayPending {
		t.Fatalf("legacy check receipt without the shared snapshot digest = %+v; want pending", got)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE task_operations SET result=$2 WHERE id=$1`, created.ID, string(runtimeResult)); err != nil {
		t.Fatalf("restore shared-digest typed test operation result: %v", err)
	}
	for _, deliveryStatus := range []string{"pending", "unknown"} {
		if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE conversation_messages SET delivery_status=$2 WHERE id=$1`, sourceMessageID, deliveryStatus); err != nil {
			t.Fatalf("set required user message delivery state %q: %v", deliveryStatus, err)
		}
		got := loadReplay()
		if got.RequirementSetVerified || len(got.Checks) != 1 || got.Checks[0].RequirementSourceVerified ||
			got.State != app.TaskValidationReplayPending {
			t.Fatalf("required message with delivery status %q verified a plan requirement: %+v; want source unverified and H07 pending",
				deliveryStatus, got)
		}
		if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE conversation_messages SET delivery_status='applied' WHERE id=$1`, sourceMessageID); err != nil {
			t.Fatalf("restore applied required user message: %v", err)
		}
		if got := loadReplay(); len(got.Checks) != 1 || !got.Checks[0].RequirementSourceVerified {
			t.Fatalf("restored applied user message did not restore source identity proof: %+v", got)
		}
	}
	checkFact := loadReplay().Checks[0]
	finalProof := &app.TaskValidationFinalWorkspaceSnapshotProof{Claim: claim,
		TaskID: claim.TaskID, TaskRevision: claim.TaskRevision,
		HostID: checkFact.HostID, RuntimeGeneration: checkFact.HostRuntimeGeneration,
		RuntimeInstanceID: checkFact.HostRuntimeInstanceID, ControlVersion: checkFact.HostControlVersion,
		WorkspaceID: checkFact.WorkspaceID, BindingVersion: checkFact.WorkspaceBindingVersion,
		InputRevision: checkFact.InputRevision, WorkspaceCheckpointRevision: checkFact.WorkspaceCheckpointRevision,
		InputManifestSHA256: checkFact.InputManifestSHA256, ViewID: checkFact.WorkspaceViewID,
		SnapshotSHA256: checkFact.WorkspaceSnapshotSHA256After}
	if got := loadReplay(finalProof); got.State != app.TaskValidationReplayPending || !got.FinalWorkspaceSnapshotVerified ||
		got.FinalWorkspaceSnapshotProof == nil {
		t.Fatalf("matching database-fenced final snapshot proof = %+v; final snapshot should verify while source-set completeness remains pending", got)
	}
	changedManifest := *finalProof
	changedManifest.SnapshotSHA256 = hex.EncodeToString(hashBytes("changed final Workspace"))
	if got := loadReplay(&changedManifest); got.FinalWorkspaceSnapshotVerified || got.State != app.TaskValidationReplayPending {
		t.Fatalf("mismatched final manifest proof = %+v; want unverified and pending", got)
	}
	changedFence := *finalProof
	changedFence.RuntimeInstanceID = "runtime-replaced"
	if got := loadReplay(&changedFence); got.FinalWorkspaceSnapshotVerified || got.State != app.TaskValidationReplayPending {
		t.Fatalf("mismatched Host Runtime proof = %+v; want unverified and pending", got)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id='runtime-replaced' WHERE id=$1`, fixture.host.ID); err != nil {
		t.Fatalf("change current Host Runtime fence: %v", err)
	}
	if got := loadReplay(finalProof); got.FinalWorkspaceSnapshotVerified || got.State != app.TaskValidationReplayPending {
		t.Fatalf("proof from prior Host Runtime instance = %+v; want unverified and pending", got)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE hosts SET runtime_instance_id=$2 WHERE id=$1`,
		fixture.host.ID, checkFact.HostRuntimeInstanceID); err != nil {
		t.Fatalf("restore Host Runtime fence: %v", err)
	}
	if _, err := fixture.route.store.pool.Exec(ctx, `UPDATE conversation_messages SET content=content||' changed'
WHERE id=$1`, sourceMessageID); err != nil {
		t.Fatal(err)
	}
	if got := loadReplay(); got.State != app.TaskValidationReplayPending || got.Checks[0].RequirementSourceVerified {
		t.Fatalf("changed required-message source replay = %+v; want pending", got)
	}
}
