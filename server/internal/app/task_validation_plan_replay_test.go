package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"morde/server/internal/domain/task"
)

func TestEvaluateTaskValidationPlanReplayUsesTypedChecksAndWorkspaceSnapshot(t *testing.T) {
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRiskSelected,
		Command: "go test ./...", CoveragePaths: []string{"server"}}
	plan := task.Plan{TaskRevision: 4, Revision: 2, Goal: "Fix the bug", CostEstimateState: "unknown",
		FixPlan: &task.FixPlan{}, Steps: []task.PlanStep{
			{ID: "implement", Title: "Implement fix", Status: task.PlanStepCompleted,
				VerificationApplicability: task.PlanVerificationApplicabilityNotApplicable},
			{ID: "verify-unit", Title: "Verify unit tests", Status: task.PlanStepCompleted,
				VerificationApplicability: task.PlanVerificationApplicabilityCheck, VerificationCheck: &check},
			{ID: "verify-integration", Title: "Verify integration tests", Status: task.PlanStepCompleted,
				VerificationApplicability: task.PlanVerificationApplicabilityCheck, VerificationCheck: &check},
		}}
	if err := plan.Validate(); err != nil {
		t.Fatalf("fixture plan is invalid: %v", err)
	}
	first := taskValidationReplayCheckFixture(plan, "verify-unit", check, 1)
	second := taskValidationReplayCheckFixture(plan, "verify-integration", check, 2)
	evidence := &TaskValidationReplayEvidence{RequirementSetVerified: true, FinalWorkspaceSnapshotVerified: true,
		PlanRevision: plan.Revision,
		Checks:       []TaskValidationReplayCheckEvidence{first, second}}
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPass {
		t.Fatalf("same-snapshot passing checks state = %q, want pass", got)
	}

	failed := second
	failed.Status, failed.ExitCode = TaskValidationReplayFail, 1
	evidence.Checks[1] = failed
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayFail {
		t.Fatalf("verified failing check state = %q, want fail", got)
	}

	evidence.Checks[1] = second
	evidence.Checks[1].WorkspaceManifestSHA256After = strings.Repeat("d", 64)
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPending {
		t.Fatalf("changed workspace during check state = %q, want pending", got)
	}

	failed.WorkspaceManifestSHA256After = strings.Repeat("d", 64)
	evidence.Checks[1] = failed
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPending {
		t.Fatalf("failed check that changed the Workspace state = %q, want pending", got)
	}

	evidence.Checks[1] = second
	evidence.Checks[1].WorkspaceViewID = "v" + strings.Repeat("e", 64)
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPending {
		t.Fatalf("checks from different Workspace views state = %q, want pending", got)
	}
}

func TestEvaluateTaskValidationPlanReplayKeepsOmittedRequiredCheckPending(t *testing.T) {
	// The task source requires the full suite, but the plan declares only a
	// focused check. Its successful receipt cannot establish completeness.
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRiskSelected,
		Command: "go test ./internal/app", CoveragePaths: []string{"server/internal/app"}}
	plan := task.Plan{TaskRevision: 4, Revision: 2, Goal: "Fix the bug", CostEstimateState: "unknown",
		FixPlan: &task.FixPlan{}, Steps: []task.PlanStep{{ID: "verify-focused", Title: "Run focused tests", Status: task.PlanStepCompleted,
			VerificationApplicability: task.PlanVerificationApplicabilityCheck, VerificationCheck: &check}}}
	evidence := &TaskValidationReplayEvidence{PlanRevision: plan.Revision,
		Checks: []TaskValidationReplayCheckEvidence{taskValidationReplayCheckFixture(plan, "verify-focused", check, 1)}}
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPending {
		t.Fatalf("focused check passed without a verified complete requirement set: got %q, want pending", got)
	}
}

func TestEvaluateTaskValidationPlanReplayRequiresRequirementSetAndFinalSnapshotFacts(t *testing.T) {
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRiskSelected,
		Command: "go test ./...", CoveragePaths: []string{"server"}}
	plan := task.Plan{TaskRevision: 4, Revision: 2, Goal: "Fix the bug", CostEstimateState: "unknown",
		FixPlan: &task.FixPlan{}, Steps: []task.PlanStep{{ID: "verify", Title: "Run tests", Status: task.PlanStepCompleted,
			VerificationApplicability: task.PlanVerificationApplicabilityCheck, VerificationCheck: &check}}}
	fact := taskValidationReplayCheckFixture(plan, "verify", check, 1)
	for name, evidence := range map[string]*TaskValidationReplayEvidence{
		"incomplete requirement set": {PlanRevision: plan.Revision, FinalWorkspaceSnapshotVerified: true,
			Checks: []TaskValidationReplayCheckEvidence{fact}},
		"final snapshot unknown": {PlanRevision: plan.Revision, RequirementSetVerified: true,
			Checks: []TaskValidationReplayCheckEvidence{fact}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPending {
				t.Fatalf("H07 with %s = %q, want pending", name, got)
			}
		})
	}
}

func TestEvaluateTaskValidationPlanReplayPassesVerifiedRequiredCheckAndSources(t *testing.T) {
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRequired,
		Command: "go test ./...", CoveragePaths: []string{"server"},
		RequirementSource: &task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage,
			MessageID: "m1234567", ContentSHA256: strings.Repeat("a", 64)}}
	plan := task.Plan{TaskRevision: 4, Revision: 2, Goal: "Fix the bug", CostEstimateState: "unknown",
		FixPlan: &task.FixPlan{}, Steps: []task.PlanStep{{ID: "verify", Title: "Run required tests", Status: task.PlanStepCompleted,
			VerificationApplicability: task.PlanVerificationApplicabilityCheck, VerificationCheck: &check}}}
	fact := taskValidationReplayCheckFixture(plan, "verify", check, 1)
	fact.RequirementSourceVerified = true
	evidence := &TaskValidationReplayEvidence{RequirementSetVerified: true, FinalWorkspaceSnapshotVerified: true,
		PlanRevision: plan.Revision, Checks: []TaskValidationReplayCheckEvidence{fact}}
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPass {
		t.Fatalf("fully verified required check replay = %q, want pass", got)
	}
}

func TestEvaluateTaskValidationPlanReplayKeepsMissingAndUnverifiedChecksPending(t *testing.T) {
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRiskSelected,
		Command: "go test ./...", CoveragePaths: []string{"server"}}
	plan := task.Plan{TaskRevision: 4, Revision: 2, Goal: "Fix the bug", CostEstimateState: "unknown",
		FixPlan: &task.FixPlan{}, Steps: []task.PlanStep{
			{ID: "implement", Title: "Implement fix", Status: task.PlanStepCompleted},
			{ID: "verify", Title: "Verify fix", Status: task.PlanStepCompleted,
				VerificationApplicability: task.PlanVerificationApplicabilityCheck, VerificationCheck: &check},
		}}
	evidence := &TaskValidationReplayEvidence{PlanRevision: plan.Revision,
		PlanStepsWithoutChecks: []string{"implement"},
		Checks:                 []TaskValidationReplayCheckEvidence{taskValidationReplayCheckFixture(plan, "verify", check, 1)}}
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPending {
		t.Fatalf("plan with an undeclared step check state = %q, want pending", got)
	}

	required := check
	required.Basis = task.PlanVerificationCheckRequired
	required.RequirementSource = &task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage,
		MessageID: "m1234567", ContentSHA256: strings.Repeat("a", 64)}
	plan.Steps[0].VerificationApplicability = task.PlanVerificationApplicabilityCheck
	plan.Steps[0].VerificationCheck = &required
	plan.Steps[1].VerificationCheck = &required
	first := taskValidationReplayCheckFixture(plan, "implement", required, 1)
	second := taskValidationReplayCheckFixture(plan, "verify", required, 2)
	first.RequirementSourceVerified, second.RequirementSourceVerified = false, true
	evidence = &TaskValidationReplayEvidence{PlanRevision: plan.Revision, Checks: []TaskValidationReplayCheckEvidence{first, second}}
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPending {
		t.Fatalf("unresolved required check source state = %q, want pending", got)
	}
}

func TestAttestTaskValidationShellResultBindsExactWorkspaceAndRequest(t *testing.T) {
	revision, checkpoint := int64(4), int64(9)
	current := task.Task{ID: "q1234567", WorkspaceID: "w1234567", CurrentRevision: &revision,
		WorkspaceCheckpointRevision: &checkpoint}
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRiskSelected,
		Command: "go test ./...", CoveragePaths: []string{"server"}}
	requestDigest := sha256.Sum256([]byte("typed test request"))
	result, err := json.Marshal(taskStepShellOutput{ExitCode: 1, TaskID: current.ID, WorkspaceID: current.WorkspaceID,
		BindingVersion: 3, WorkspaceViewID: "v" + strings.Repeat("a", 64), WorkspaceCheckpointRevision: &checkpoint,
		WorkspaceManifestSHA256Before: strings.Repeat("b", 64), WorkspaceManifestSHA256After: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	output, attested := attestTaskValidationShellResult(current, 2, "verify", check, 3, requestDigest[:], string(result))
	if !attested {
		t.Fatal("valid Runtime Workspace result was not attested")
	}
	var got taskStepShellOutput
	if err := json.Unmarshal([]byte(output), &got); err != nil || got.VerificationCheck == nil {
		t.Fatalf("attested output receipt = %+v, err=%v", got.VerificationCheck, err)
	}
	commandSHA, checkSHA, coverageSHA, ok := taskValidationPlanCheckDigests(check)
	if !ok || got.VerificationCheck.Version != 1 || got.VerificationCheck.TaskRevision != revision ||
		got.VerificationCheck.PlanRevision != 2 || got.VerificationCheck.CheckID != "verify" ||
		got.VerificationCheck.CommandSHA256 != commandSHA || got.VerificationCheck.CheckSHA256 != checkSHA ||
		got.VerificationCheck.CoverageSHA256 != coverageSHA ||
		got.VerificationCheck.RequestSHA256 != hex.EncodeToString(requestDigest[:]) {
		t.Fatalf("receipt does not bind the expected typed check: %+v", got.VerificationCheck)
	}
	got.WorkspaceManifestSHA256After = strings.Repeat("c", 64)
	changed, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if _, attested := attestTaskValidationShellResult(current, 2, "verify", check, 3, requestDigest[:], string(changed)); !attested {
		t.Fatal("definitive Runtime result with a changed Workspace manifest lost its execution receipt")
	}
	fact := taskValidationReplayCheckFixture(task.Plan{TaskRevision: revision, Revision: 2}, "verify", check, 1)
	fact.WorkspaceManifestSHA256After = strings.Repeat("c", 64)
	if got := EvaluateTaskValidationPlanReplay(task.Plan{TaskRevision: revision, Revision: 2, Goal: "Fix", CostEstimateState: "unknown",
		FixPlan: &task.FixPlan{}, Steps: []task.PlanStep{{ID: "verify", Title: "Verify", Status: task.PlanStepCompleted,
			VerificationApplicability: task.PlanVerificationApplicabilityCheck, VerificationCheck: &check}}},
		&TaskValidationReplayEvidence{PlanRevision: 2, Checks: []TaskValidationReplayCheckEvidence{fact}}); got != TaskValidationReplayPending {
		t.Fatalf("replay with a changed Workspace manifest state = %q, want pending", got)
	}
}

func TestAttestReconciledTaskValidationShellResultPreservesReceipt(t *testing.T) {
	revision, checkpoint := int64(4), int64(9)
	current := task.Task{ID: "q1234567", WorkspaceID: "w1234567", CurrentRevision: &revision,
		WorkspaceCheckpointRevision: &checkpoint}
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRiskSelected,
		Command: "go test ./...", CoveragePaths: []string{"server"}}
	requestDigest := sha256.Sum256([]byte("reconciled request"))
	output, err := json.Marshal(taskStepShellOutput{ExitCode: 1, TaskID: current.ID, WorkspaceID: current.WorkspaceID,
		BindingVersion: 3, WorkspaceViewID: "v" + strings.Repeat("a", 64), WorkspaceCheckpointRevision: &checkpoint,
		WorkspaceManifestSHA256Before: strings.Repeat("b", 64), WorkspaceManifestSHA256After: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	operation := task.Operation{ID: "o1234567", TaskID: current.ID, RequestHash: requestDigest[:], BindingVersion: 3,
		TaskRevision: &revision, CallID: "call-verify", ToolName: runtimeToolShellExec}
	request := AgentToolRequest{Task: current, Call: ModelToolCall{CallID: "call-verify", Name: runtimeToolShellExec},
		OperationID: operation.ID, OperationRequestHash: requestDigest[:]}
	ctx := context.WithValue(context.Background(), cloudDSHToolActivityKey{}, cloudDSHToolActivity{
		call: request.Call, current: current,
	})
	ctx = withTaskValidationPlanCheck(ctx, 2, "verify", &check)
	got := attestReconciledTaskValidationShellResult(ctx, request, operation,
		AgentToolResult{Status: AgentToolFailed, Output: string(output)})
	var shell taskStepShellOutput
	if err := json.Unmarshal([]byte(got.Output), &shell); err != nil || shell.VerificationCheck == nil ||
		shell.VerificationCheck.CheckID != "verify" {
		t.Fatalf("reconciled runtime result lost its typed check receipt: result=%+v err=%v", got, err)
	}
	differentRequestDigest := sha256.Sum256([]byte("different request"))
	request.OperationRequestHash = differentRequestDigest[:]
	got = attestReconciledTaskValidationShellResult(ctx, request, operation,
		AgentToolResult{Status: AgentToolFailed, Output: string(output)})
	shell = taskStepShellOutput{}
	if json.Unmarshal([]byte(got.Output), &shell) != nil || shell.VerificationCheck != nil {
		t.Fatalf("receipt was added for a different operation request: %+v", shell.VerificationCheck)
	}
}

func taskValidationReplayCheckFixture(plan task.Plan, checkID string, check task.PlanVerificationCheck,
	runSequence int64) TaskValidationReplayCheckEvidence {
	commandSHA, checkSHA, coverageSHA, _ := taskValidationPlanCheckDigests(check)
	return TaskValidationReplayCheckEvidence{
		CheckID: checkID, TaskID: "q1234567", Kind: check.Kind, Basis: check.Basis,
		CoveragePaths: append([]string(nil), check.CoveragePaths...), RequirementSource: check.RequirementSource,
		RequirementSourceVerified: check.Basis == task.PlanVerificationCheckRiskSelected,
		Status:                    TaskValidationReplayPass, TaskRevision: plan.TaskRevision, PlanRevision: plan.Revision,
		OperationID: "o1234567", CallID: "call-" + checkID, RequestSHA256: strings.Repeat("1", 64),
		CommandSHA256: commandSHA, CheckSHA256: checkSHA, CoverageSHA256: coverageSHA, RunSequence: runSequence,
		WorkspaceID: "w1234567", WorkspaceBindingVersion: 3, WorkspaceCheckpointRevision: 9,
		WorkspaceViewID:               "v" + strings.Repeat("a", 64),
		WorkspaceManifestSHA256Before: strings.Repeat("b", 64), WorkspaceManifestSHA256After: strings.Repeat("b", 64),
	}
}
