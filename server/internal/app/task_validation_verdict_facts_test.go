package app

import (
	"testing"

	"morde/server/internal/domain/task"
)

// E09 independent verifier: the server-attested facts that let a review
// verdict resolve instead of staying pending for every executed task.
// These tests use memory fakes only: no DSH, no database, no model.

func e09Revision(value int64) *int64 { return &value }

func TestTaskToolRequiresPerCallApproval(t *testing.T) {
	for _, test := range []struct {
		name     string
		toolName string
		want     bool
	}{
		{name: "shell exec is sensitive", toolName: runtimeToolShellExec, want: true},
		{name: "browser type is sensitive", toolName: runtimeToolBrowserType, want: true},
		{name: "desktop click is sensitive", toolName: runtimeToolDesktopClick, want: true},
		{name: "press key is sensitive without arguments to classify", toolName: runtimeToolBrowserPressKey, want: true},
		{name: "workspace write needs approval", toolName: cloudDSHWorkspaceWriteToolName, want: true},
		{name: "unknown tool name fails closed", toolName: "some_unknown_tool", want: true},
		{name: "external MCP tool name fails closed", toolName: "github__create_issue", want: true},
		{name: "file read is safe", toolName: runtimeToolFileRead},
		{name: "file write is task-scope only", toolName: runtimeToolFileWrite},
		{name: "browser click is task-scope only", toolName: runtimeToolBrowserClick},
		{name: "workspace read is read-only", toolName: cloudDSHWorkspaceReadToolName},
		{name: "web search is read-only", toolName: cloudDSHWebSearchToolName},
		{name: "skill read is read-only", toolName: cloudDSHSkillReadToolName},
		{name: "history search is read-only", toolName: cloudDSHCurrentHistorySearchToolName},
		{name: "plan update is harness-internal", toolName: cloudDSHTaskPlanToolName},
		{name: "goal create is harness-internal", toolName: cloudDSHGoalCreateToolName},
		{name: "capability probe is read-only", toolName: "morde_capability_probe"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := TaskToolRequiresPerCallApproval(test.toolName); got != test.want {
				t.Fatalf("TaskToolRequiresPerCallApproval(%q) = %v, want %v", test.toolName, got, test.want)
			}
		})
	}
}

func TestEvaluateTaskApprovalCoverage(t *testing.T) {
	digest := testGateDigest("shell_exec go test ./...")
	otherDigest := testGateDigest("shell_exec go test ./other")
	rev1 := e09Revision(1)
	rev2 := e09Revision(2)
	action := TaskApprovalAction{CallID: "c1234567", ToolName: runtimeToolShellExec, RequestDigest: digest, TaskRevision: rev1}
	approval := TaskApprovalRecord{CallID: "c1234567", ToolName: runtimeToolShellExec, RequestHash: digest, TaskRevision: rev1}
	for _, test := range []struct {
		name           string
		actions        []TaskApprovalAction
		approvals      []TaskApprovalRecord
		unclassifiable bool
		wantFact       TaskValidationFactState
		wantVerified   bool
	}{
		{name: "no actions means no irreversible action", wantFact: TaskValidationFactAbsent},
		{name: "matched approval verifies",
			actions: []TaskApprovalAction{action}, approvals: []TaskApprovalRecord{approval},
			wantFact: TaskValidationFactPresent, wantVerified: true},
		{name: "no approval row leaves the action unverified",
			actions:  []TaskApprovalAction{action},
			wantFact: TaskValidationFactPresent},
		{name: "wrong call id does not match",
			actions:   []TaskApprovalAction{action},
			approvals: []TaskApprovalRecord{{CallID: "c9999999", ToolName: runtimeToolShellExec, RequestHash: digest, TaskRevision: rev1}},
			wantFact:  TaskValidationFactPresent},
		{name: "wrong digest does not match",
			actions:   []TaskApprovalAction{action},
			approvals: []TaskApprovalRecord{{CallID: "c1234567", ToolName: runtimeToolShellExec, RequestHash: otherDigest, TaskRevision: rev1}},
			wantFact:  TaskValidationFactPresent},
		{name: "wrong revision does not match",
			actions:   []TaskApprovalAction{action},
			approvals: []TaskApprovalRecord{{CallID: "c1234567", ToolName: runtimeToolShellExec, RequestHash: digest, TaskRevision: rev2}},
			wantFact:  TaskValidationFactPresent},
		{name: "one unverified action blocks the whole history",
			actions:   []TaskApprovalAction{action, {CallID: "c7654321", ToolName: runtimeToolShellExec, RequestDigest: otherDigest, TaskRevision: rev1}},
			approvals: []TaskApprovalRecord{approval},
			wantFact:  TaskValidationFactPresent},
		{name: "unclassifiable action keeps the fact unknown",
			actions: []TaskApprovalAction{action}, approvals: []TaskApprovalRecord{approval}, unclassifiable: true,
			wantFact: TaskValidationFactUnknown},
		{name: "action without identity never matches",
			actions:   []TaskApprovalAction{{CallID: "", ToolName: runtimeToolShellExec, RequestDigest: digest, TaskRevision: rev1}},
			approvals: []TaskApprovalRecord{approval},
			wantFact:  TaskValidationFactPresent},
	} {
		t.Run(test.name, func(t *testing.T) {
			fact, verified := EvaluateTaskApprovalCoverage(test.actions, test.approvals, test.unclassifiable)
			if fact != test.wantFact || verified != test.wantVerified {
				t.Fatalf("EvaluateTaskApprovalCoverage() = (%q, %v), want (%q, %v)", fact, verified, test.wantFact, test.wantVerified)
			}
		})
	}
}

func TestEvaluateRepeatedErrorCorrections(t *testing.T) {
	digest := testGateDigest("shell_exec go test ./...")
	other := testGateDigest("shell_exec go vet ./...")
	failed := func(sequence int64, d []byte) task.Step {
		return task.Step{ID: "red", Kind: "tool", Status: task.StepFailed, Sequence: sequence,
			ToolName: runtimeToolShellExec, RequestDigest: d}
	}
	passed := func(sequence int64, d []byte) task.Step {
		return task.Step{ID: "green", Kind: "tool", Status: task.StepSucceeded, Sequence: sequence,
			ToolName: runtimeToolShellExec, RequestDigest: d}
	}
	for _, test := range []struct {
		name         string
		steps        []task.Step
		wantFact     TaskValidationFactState
		wantVerified bool
	}{
		{name: "no failures", wantFact: TaskValidationFactAbsent},
		{name: "one failure is not repeated",
			steps:    []task.Step{failed(1, digest), passed(2, digest)},
			wantFact: TaskValidationFactAbsent},
		{name: "same call failing twice then passing is a verified correction",
			steps:    []task.Step{failed(1, digest), failed(2, digest), passed(3, digest)},
			wantFact: TaskValidationFactPresent, wantVerified: true},
		{name: "same call failing twice with no later pass is unverified",
			steps:    []task.Step{failed(1, digest), failed(2, digest)},
			wantFact: TaskValidationFactPresent},
		{name: "different calls failing once each is not repeated",
			steps:    []task.Step{failed(1, digest), failed(2, other)},
			wantFact: TaskValidationFactAbsent},
		{name: "a pass before the second failure does not verify the correction",
			steps:    []task.Step{failed(1, digest), passed(2, digest), failed(3, digest)},
			wantFact: TaskValidationFactPresent},
		{name: "steps without a digest cannot be grouped",
			steps: []task.Step{
				{ID: "red", Kind: "tool", Status: task.StepFailed, Sequence: 1, ToolName: runtimeToolShellExec},
				{ID: "red", Kind: "tool", Status: task.StepFailed, Sequence: 2, ToolName: runtimeToolShellExec},
			},
			wantFact: TaskValidationFactAbsent},
	} {
		t.Run(test.name, func(t *testing.T) {
			fact, verified := EvaluateRepeatedErrorCorrections(test.steps)
			if fact != test.wantFact || verified != test.wantVerified {
				t.Fatalf("EvaluateRepeatedErrorCorrections() = (%q, %v), want (%q, %v)", fact, verified, test.wantFact, test.wantVerified)
			}
		})
	}
}

func TestEvaluateTaskValidationReplay(t *testing.T) {
	passingGate := TestGateResult{NewTestPassed: true, FullSuitePassed: true}
	halfGate := TestGateResult{NewTestPassed: true}
	for _, test := range []struct {
		name            string
		hasFixPlan      bool
		gate            TestGateResult
		evidenceChecked bool
		want            bool
	}{
		{name: "fix task with both gate halves", hasFixPlan: true, gate: passingGate, want: true},
		{name: "fix task with only one gate half", hasFixPlan: true, gate: halfGate},
		{name: "fix task with no gate", hasFixPlan: true},
		{name: "fix task ignores evidence coverage", hasFixPlan: true, gate: passingGate, evidenceChecked: false, want: true},
		{name: "other task replays by read-back compare", evidenceChecked: true, want: true},
		{name: "other task without verified content cannot replay", evidenceChecked: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := EvaluateTaskValidationReplay(test.hasFixPlan, test.gate, test.evidenceChecked); got != test.want {
				t.Fatalf("EvaluateTaskValidationReplay() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestReplayEvidenceForSteps(t *testing.T) {
	suite := testGateDigest("go test ./...")
	revision := int64(1)
	steps := []task.Step{
		{ID: "red", Kind: "tool", Status: task.StepFailed, Sequence: 1, ToolName: runtimeToolShellExec,
			FailureReason: taskToolTestFailureReason, RequestDigest: suite, TaskRevision: &revision, Summary: directTestEvidenceSummary},
		{ID: "green", Kind: "tool", Status: task.StepSucceeded, Sequence: 2, ToolName: runtimeToolShellExec,
			FailureReason: taskToolTestSuitePassedReason, RequestDigest: suite, TaskRevision: &revision, Summary: directTestEvidenceSummary},
	}
	got := ReplayEvidenceForSteps(steps, &revision, true)
	if got == nil {
		t.Fatal("ReplayEvidenceForSteps(fix task) = nil, want a replay section")
	}
	want := &TaskValidationReplayEvidence{NewTestPassed: true, FullSuitePassed: true,
		NewTestFailedSequence: 1, NewTestPassedSequence: 2, FullSuitePassedSequence: 2}
	if got.NewTestPassed != want.NewTestPassed || got.FullSuitePassed != want.FullSuitePassed ||
		got.NewTestFailedSequence != want.NewTestFailedSequence || got.NewTestPassedSequence != want.NewTestPassedSequence ||
		got.FullSuitePassedSequence != want.FullSuitePassedSequence || got.PlanRevision != want.PlanRevision || len(got.Checks) != 0 {
		t.Fatalf("ReplayEvidenceForSteps() = %+v, want %+v", got, want)
	}
	if got := ReplayEvidenceForSteps(steps, &revision, false); got != nil {
		t.Fatalf("ReplayEvidenceForSteps(non-fix task) = %+v, want nil", got)
	}
	// The section is included even when the gate fails, so the model can
	// see the re-run did not happen.
	if got := ReplayEvidenceForSteps(nil, &revision, true); got == nil || got.NewTestPassed || got.FullSuitePassed {
		t.Fatalf("ReplayEvidenceForSteps(fix task, no runs) = %+v, want an all-false section", got)
	}
}

// TestIndependentVerifierResolvesFixTaskVerdict is the E09 acceptance in
// miniature: a fix task whose server facts all resolve (approval coverage
// verified, no repeated error, replay verified, clarification history
// known, evidence content verified) and whose review supports every check
// reaches PASS instead of staying pending, as it did before E09.
func TestIndependentVerifierResolvesFixTaskVerdict(t *testing.T) {
	suite := testGateDigest("go test ./...")
	rev := e09Revision(1)
	steps := []task.Step{
		{ID: "red", Kind: "tool", Status: task.StepFailed, Sequence: 1, ToolName: runtimeToolShellExec,
			FailureReason: taskToolTestFailureReason, RequestDigest: suite, TaskRevision: rev, Summary: directTestEvidenceSummary},
		{ID: "green", Kind: "tool", Status: task.StepSucceeded, Sequence: 2, ToolName: runtimeToolShellExec,
			FailureReason: taskToolTestSuitePassedReason, RequestDigest: suite, TaskRevision: rev, Summary: directTestEvidenceSummary},
		{ID: "read", Kind: "tool", Status: task.StepSucceeded, Sequence: 3, ToolName: runtimeToolFileRead},
	}
	approvalFact, approvalVerified := EvaluateTaskApprovalCoverage(
		[]TaskApprovalAction{{CallID: "c1234567", ToolName: runtimeToolShellExec, RequestDigest: suite, TaskRevision: rev}},
		[]TaskApprovalRecord{{CallID: "c1234567", ToolName: runtimeToolShellExec, RequestHash: suite, TaskRevision: rev}},
		false)
	repeatedFact, correctionVerified := EvaluateRepeatedErrorCorrections(steps)
	facts := TaskValidationVerdictFacts{
		TaskExecution:             TaskValidationFactPresent,
		IrreversibleAction:        approvalFact,
		ApprovalHistoryVerified:   approvalVerified,
		RepeatedError:             repeatedFact,
		CorrectionRecordVerified:  correctionVerified,
		EvidenceContentVerified:   true,
		ClarificationHistoryKnown: true,
		ReplayVerified:            EvaluateTaskValidationReplay(true, EvaluateTestGate(steps, rev), true),
	}
	evaluations := EvaluateTaskValidationReview(validTaskValidationReviewReport(), facts)
	if got := SummarizeTaskValidationEvaluations(evaluations); got.State != TaskValidationPass {
		t.Fatalf("fix task verdict = %q, want PASS (evaluations %+v)", got.State, evaluations)
	}

	// The same task with the test gate unproven stays pending: the
	// verifier must not pass what it could not re-derive.
	unproven := EvaluateTestGate(steps[:1], rev)
	facts.ReplayVerified = EvaluateTaskValidationReplay(true, unproven, true)
	if got := SummarizeTaskValidationEvaluations(EvaluateTaskValidationReview(validTaskValidationReviewReport(), facts)); got.State != TaskValidationPending {
		t.Fatalf("fix task without a replay verdict = %q, want pending", got.State)
	}
}
