package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
	evidence.FinalWorkspaceSnapshotProof = taskValidationFinalWorkspaceProofFixture(first)
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
	evidence.Checks[1].WorkspaceSnapshotSHA256After = strings.Repeat("d", 64)
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPending {
		t.Fatalf("changed workspace during check state = %q, want pending", got)
	}

	failed.WorkspaceSnapshotSHA256After = strings.Repeat("d", 64)
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
	// The source identity and all declared check/Workspace facts are verified,
	// but the plan declares only a focused check while the requirement asks for
	// the full suite. Those facts cannot establish requirement-set completeness.
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRequired,
		Command: "go test ./internal/app", CoveragePaths: []string{"server/internal/app"},
		RequirementSource: &task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage,
			MessageID: "m1234567", ContentSHA256: strings.Repeat("a", 64)}}
	plan := task.Plan{TaskRevision: 4, Revision: 2, Goal: "Fix the bug", CostEstimateState: "unknown",
		FixPlan: &task.FixPlan{}, Steps: []task.PlanStep{{ID: "verify-focused", Title: "Run focused tests", Status: task.PlanStepCompleted,
			VerificationApplicability: task.PlanVerificationApplicabilityCheck, VerificationCheck: &check}}}
	fact := taskValidationReplayCheckFixture(plan, "verify-focused", check, 1)
	fact.RequirementSourceVerified = true
	evidence := &TaskValidationReplayEvidence{PlanRevision: plan.Revision, FinalWorkspaceSnapshotVerified: true,
		Checks: []TaskValidationReplayCheckEvidence{fact}}
	evidence.FinalWorkspaceSnapshotProof = taskValidationFinalWorkspaceProofFixture(fact)
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPending {
		t.Fatalf("verified focused check and source identity passed without a verified complete requirement set: got %q, want pending", got)
	}
}

func TestEvaluateTaskValidationPlanReplayReportsVerifiedFailureWithIncompleteRequirements(t *testing.T) {
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRequired,
		Command: "go test ./internal/app", CoveragePaths: []string{"server/internal/app"},
		RequirementSource: &task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage,
			MessageID: "m1234567", ContentSHA256: strings.Repeat("a", 64)}}
	plan := task.Plan{TaskRevision: 4, Revision: 2, Goal: "Fix the reported issue", CostEstimateState: "unknown",
		FixPlan: &task.FixPlan{}, Steps: []task.PlanStep{
			{ID: "implement", Title: "Implement fix", Status: task.PlanStepCompleted},
			{ID: "verify-focused", Title: "Run focused check", Status: task.PlanStepCompleted,
				VerificationApplicability: task.PlanVerificationApplicabilityCheck, VerificationCheck: &check},
		}}
	fact := taskValidationReplayCheckFixture(plan, "verify-focused", check, 1)
	fact.RequirementSourceVerified = true
	fact.Status, fact.ExitCode = TaskValidationReplayFail, 1
	evidence := &TaskValidationReplayEvidence{PlanRevision: plan.Revision,
		PlanStepsWithoutChecks: []string{"implement"}, Checks: []TaskValidationReplayCheckEvidence{fact}}
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayFail {
		t.Fatalf("verified failing check with an incomplete requirement set = %q, want fail", got)
	}
	fact.RequirementSourceVerified = false
	evidence.Checks[0] = fact
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPending {
		t.Fatalf("failing required check with an unverified source = %q, want pending", got)
	}
}

func TestEvaluateTaskValidationPlanReplayRequiresRequirementSetAndFinalSnapshotFacts(t *testing.T) {
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRiskSelected,
		Command: "go test ./...", CoveragePaths: []string{"server"}}
	plan := task.Plan{TaskRevision: 4, Revision: 2, Goal: "Fix the bug", CostEstimateState: "unknown",
		FixPlan: &task.FixPlan{}, Steps: []task.PlanStep{{ID: "verify", Title: "Run tests", Status: task.PlanStepCompleted,
			VerificationApplicability: task.PlanVerificationApplicabilityCheck, VerificationCheck: &check}}}
	fact := taskValidationReplayCheckFixture(plan, "verify", check, 1)
	legacyFact := fact
	legacyFact.WorkspaceSnapshotSHA256Before, legacyFact.WorkspaceSnapshotSHA256After = "", ""
	coverageUnknown := fact
	coverageUnknown.ActualCoverageVerified = false
	for name, evidence := range map[string]*TaskValidationReplayEvidence{
		"incomplete requirement set": {PlanRevision: plan.Revision, FinalWorkspaceSnapshotVerified: true,
			Checks: []TaskValidationReplayCheckEvidence{fact}},
		"final snapshot unknown": {PlanRevision: plan.Revision, RequirementSetVerified: true,
			Checks: []TaskValidationReplayCheckEvidence{fact}},
		"final snapshot boolean without proof": {PlanRevision: plan.Revision, RequirementSetVerified: true,
			FinalWorkspaceSnapshotVerified: true, Checks: []TaskValidationReplayCheckEvidence{fact}},
		"runner coverage unknown": {PlanRevision: plan.Revision, RequirementSetVerified: true,
			FinalWorkspaceSnapshotVerified: true, Checks: []TaskValidationReplayCheckEvidence{coverageUnknown}},
		"legacy check receipt without shared snapshot digest": {PlanRevision: plan.Revision,
			RequirementSetVerified: true, FinalWorkspaceSnapshotVerified: true,
			Checks: []TaskValidationReplayCheckEvidence{legacyFact}},
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
	// This synthetic fact represents a future independent Store-validated
	// completeness and semantic attestation, not an exact-digest confirmation.
	evidence := &TaskValidationReplayEvidence{RequirementSetVerified: true, FinalWorkspaceSnapshotVerified: true,
		PlanRevision: plan.Revision, Checks: []TaskValidationReplayCheckEvidence{fact}}
	evidence.FinalWorkspaceSnapshotProof = taskValidationFinalWorkspaceProofFixture(fact)
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPass {
		t.Fatalf("fully verified required check replay = %q, want pass", got)
	}
}

func TestEvaluateTaskValidationPlanReplayCarriesManifestToExactCheckAndKeepsUnknownRunnerScopePending(t *testing.T) {
	source := task.PlanVerificationRequirementSource{Kind: task.PlanVerificationRequirementUserMessage,
		MessageID: "m1234567", ContentSHA256: strings.Repeat("a", 64)}
	obligation := task.PlanVerificationObligation{Statement: "Run the full test suite",
		Kind: task.PlanVerificationCheckTest, ScopeMode: task.PlanVerificationScopeFullSuite,
		CoveragePaths: []string{"server"}, Source: source}
	obligationID, err := obligation.CanonicalID()
	if err != nil {
		t.Fatalf("canonicalize required test obligation: %v", err)
	}
	obligation.ID = task.PlanVerificationRequirementID(obligationID)
	check := task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRequired,
		Command: "go test ./...", ScopeMode: task.PlanVerificationScopeFullSuite, CoveragePaths: []string{"server"},
		RequirementIDs: []task.PlanVerificationRequirementID{obligation.ID}}
	plan := task.Plan{TaskRevision: 4, Revision: 2, Goal: "Fix the bug", CostEstimateState: "unknown",
		FixPlan: &task.FixPlan{}, RequirementManifest: &task.PlanVerificationRequirementManifest{Version: 1,
			Obligations: []task.PlanVerificationObligation{obligation}}, Steps: []task.PlanStep{{ID: "verify-suite",
			Title: "Run the full suite", Status: task.PlanStepCompleted,
			VerificationApplicability: task.PlanVerificationApplicabilityCheck, VerificationCheck: &check}}}
	plan.RequirementManifest.Digest, err = plan.RequirementManifestDigest()
	if err != nil || plan.Validate() != nil {
		t.Fatalf("manifest-backed plan invalid: digest=%q err=%v validate=%v", plan.RequirementManifest.Digest, err, plan.Validate())
	}
	pending := PendingTaskValidationReplayEvidence(&plan)
	if len(pending.Checks) != 1 || pending.Checks[0].ScopeMode != task.PlanVerificationScopeFullSuite ||
		len(pending.Checks[0].RequirementIDs) != 1 || pending.Checks[0].RequirementIDs[0] != obligation.ID ||
		pending.Checks[0].ActualCoverageVerified {
		t.Fatalf("pending evidence lost requirement/check mapping or implied runner scope: %+v", pending)
	}

	fact := taskValidationReplayCheckFixture(plan, "verify-suite", check, 1)
	fact.RequirementSourceVerified = true
	evidence := &TaskValidationReplayEvidence{PlanRevision: plan.Revision, RequirementSetVerified: true,
		FinalWorkspaceSnapshotVerified: true, Checks: []TaskValidationReplayCheckEvidence{fact},
		FinalWorkspaceSnapshotProof: taskValidationFinalWorkspaceProofFixture(fact)}
	fact.ActualCoverageVerified = false
	evidence.Checks[0] = fact
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPending {
		t.Fatalf("confirmed full-suite obligation with only a declared check = %q, want pending", got)
	}
	fact.ActualCoverageVerified = true
	evidence.Checks[0] = fact
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPass {
		t.Fatalf("manifest, exact check mapping, and trusted runner scope = %q, want pass", got)
	}
	fact.RequirementIDs = nil
	evidence.Checks[0] = fact
	if got := EvaluateTaskValidationPlanReplay(plan, evidence); got != TaskValidationReplayPending {
		t.Fatalf("execution receipt detached from required obligation = %q, want pending", got)
	}
}

func TestTaskValidationFinalWorkspaceProofRequiresExactClaimAndOperationFence(t *testing.T) {
	check := taskValidationReplayCheckFixture(task.Plan{TaskRevision: 4, Revision: 2}, "verify",
		task.PlanVerificationCheck{Kind: task.PlanVerificationCheckTest, Basis: task.PlanVerificationCheckRiskSelected,
			Command: "go test ./...", CoveragePaths: []string{"server"}}, 1)
	proof := taskValidationFinalWorkspaceProofFixture(check)
	claim := proof.Claim
	baseline := TaskValidationReplaySnapshotBaseline{OwnerID: claim.OwnerID, InitiatorID: claim.InitiatorID,
		ConversationID: claim.ConversationID, TaskID: claim.TaskID, TaskRevision: claim.TaskRevision,
		HostID: check.HostID, HostRuntimeGeneration: check.HostRuntimeGeneration,
		HostRuntimeInstanceID: check.HostRuntimeInstanceID, HostControlVersion: check.HostControlVersion,
		HostState: "running", HostControlState: "ai", WorkspaceID: check.WorkspaceID,
		InputRevision: check.InputRevision, WorkspaceCheckpointRevision: check.WorkspaceCheckpointRevision,
		InputManifestSHA256: check.InputManifestSHA256}
	checks := []TaskValidationReplayCheckEvidence{check}
	if !TaskValidationFinalWorkspaceSnapshotProofMatchesReplay(claim, baseline, checks, proof) {
		t.Fatal("matching final Workspace proof was rejected")
	}

	changedClaim := claim
	changedClaim.ClaimGeneration++
	if TaskValidationFinalWorkspaceSnapshotProofMatchesReplay(changedClaim, baseline, checks, proof) {
		t.Fatal("proof from a different review claim generation was accepted")
	}
	changedCheck := check
	changedCheck.HostRuntimeGeneration++
	if TaskValidationFinalWorkspaceSnapshotProofMatchesReplay(claim, baseline,
		[]TaskValidationReplayCheckEvidence{changedCheck}, proof) {
		t.Fatal("proof from a different operation Runtime fence was accepted")
	}
	changedBaseline := baseline
	changedBaseline.WorkspaceCheckpointRevision++
	if TaskValidationFinalWorkspaceSnapshotProofMatchesReplay(claim, changedBaseline, checks, proof) {
		t.Fatal("proof for a stale Task checkpoint was accepted")
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
		WorkspaceSnapshotSHA256Before: strings.Repeat("c", 64), WorkspaceSnapshotSHA256After: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	var legacyResult taskStepShellOutput
	if err := json.Unmarshal(result, &legacyResult); err != nil {
		t.Fatal(err)
	}
	legacyResult.WorkspaceSnapshotSHA256Before, legacyResult.WorkspaceSnapshotSHA256After = "", ""
	legacyBytes, err := json.Marshal(legacyResult)
	if err != nil {
		t.Fatal(err)
	}
	if _, attested := attestTaskValidationShellResult(current, 2, "verify", check, 3, requestDigest[:], string(legacyBytes)); attested {
		t.Fatal("legacy Runtime result without a shared snapshot digest received a typed verification receipt")
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
	if !ok || got.VerificationCheck.Version != 2 || got.VerificationCheck.TaskRevision != revision ||
		got.VerificationCheck.PlanRevision != 2 || got.VerificationCheck.CheckID != "verify" ||
		got.VerificationCheck.CommandSHA256 != commandSHA || got.VerificationCheck.CheckSHA256 != checkSHA ||
		got.VerificationCheck.CoverageSHA256 != coverageSHA ||
		got.VerificationCheck.RequestSHA256 != hex.EncodeToString(requestDigest[:]) {
		t.Fatalf("receipt does not bind the expected typed check: %+v", got.VerificationCheck)
	}
	got.WorkspaceSnapshotSHA256After = strings.Repeat("d", 64)
	changed, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if _, attested := attestTaskValidationShellResult(current, 2, "verify", check, 3, requestDigest[:], string(changed)); !attested {
		t.Fatal("definitive Runtime result with a changed Workspace manifest lost its execution receipt")
	}
	fact := taskValidationReplayCheckFixture(task.Plan{TaskRevision: revision, Revision: 2}, "verify", check, 1)
	fact.WorkspaceSnapshotSHA256After = strings.Repeat("d", 64)
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
		WorkspaceManifestSHA256Before: strings.Repeat("b", 64), WorkspaceManifestSHA256After: strings.Repeat("b", 64),
		WorkspaceSnapshotSHA256Before: strings.Repeat("c", 64), WorkspaceSnapshotSHA256After: strings.Repeat("c", 64)})
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
		CheckID: checkID, TaskID: "q1234567", Kind: check.Kind, Basis: check.Basis, ScopeMode: check.ScopeMode,
		CoveragePaths: append([]string(nil), check.CoveragePaths...), Selectors: append([]string(nil), check.Selectors...),
		RequirementIDs: append([]task.PlanVerificationRequirementID(nil), check.RequirementIDs...), RequirementSource: check.RequirementSource,
		RequirementSourceVerified: check.Basis == task.PlanVerificationCheckRiskSelected,
		ActualCoverageVerified:    true,
		Status:                    TaskValidationReplayPass, TaskRevision: plan.TaskRevision, PlanRevision: plan.Revision,
		OperationID: "o1234567", CallID: "call-" + checkID, RequestSHA256: strings.Repeat("1", 64),
		CommandSHA256: commandSHA, CheckSHA256: checkSHA, CoverageSHA256: coverageSHA, RunSequence: runSequence,
		WorkspaceID: "w1234567", WorkspaceBindingVersion: 3, WorkspaceCheckpointRevision: 9,
		WorkspaceViewID:               "v" + strings.Repeat("a", 64),
		WorkspaceManifestSHA256Before: strings.Repeat("b", 64), WorkspaceManifestSHA256After: strings.Repeat("b", 64),
		WorkspaceSnapshotSHA256Before: strings.Repeat("c", 64), WorkspaceSnapshotSHA256After: strings.Repeat("c", 64),
		HostID: "h1234567", HostRuntimeGeneration: 2, HostRuntimeInstanceID: "runtime-instance",
		HostControlVersion: 3, InputRevision: 4, InputManifestSHA256: strings.Repeat("c", 64),
	}
}

func taskValidationFinalWorkspaceProofFixture(check TaskValidationReplayCheckEvidence) *TaskValidationFinalWorkspaceSnapshotProof {
	claim := TaskValidationReviewClaim{OwnerID: "u1234567", InitiatorID: "u1234567", ConversationID: "c1234567",
		TaskID: check.TaskID, TaskRevision: check.TaskRevision, ResultEventSequence: 9,
		ResultSHA256: bytesRepeat(0x45, sha256.Size), ClaimGeneration: 2, LeaseOwner: "review-worker",
		LeaseExpiresAt: time.Unix(1_800_000_000, 0).UTC()}
	return &TaskValidationFinalWorkspaceSnapshotProof{Claim: claim, TaskID: check.TaskID, TaskRevision: check.TaskRevision,
		HostID: check.HostID, RuntimeGeneration: check.HostRuntimeGeneration, RuntimeInstanceID: check.HostRuntimeInstanceID,
		ControlVersion: check.HostControlVersion, WorkspaceID: check.WorkspaceID, BindingVersion: check.WorkspaceBindingVersion,
		InputRevision: check.InputRevision, WorkspaceCheckpointRevision: check.WorkspaceCheckpointRevision,
		InputManifestSHA256: check.InputManifestSHA256, ViewID: check.WorkspaceViewID,
		SnapshotSHA256: check.WorkspaceSnapshotSHA256After}
}

func bytesRepeat(value byte, count int) []byte {
	result := make([]byte, count)
	for index := range result {
		result[index] = value
	}
	return result
}
