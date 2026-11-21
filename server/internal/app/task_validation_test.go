package app

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"testing"

	"morde/server/internal/domain/task"
)

func TestValidTaskValidationState(t *testing.T) {
	for _, state := range []TaskValidationState{TaskValidationPending, TaskValidationPass, TaskValidationPassNotes, TaskValidationFail} {
		if !ValidTaskValidationState(state) {
			t.Errorf("ValidTaskValidationState(%q) = false", state)
		}
	}
	for _, state := range []TaskValidationState{"", "verified", "pass", "N/A"} {
		if ValidTaskValidationState(state) {
			t.Errorf("ValidTaskValidationState(%q) = true", state)
		}
	}
}

func TestValidateTaskHardCheckShapeAcceptsSevenChecks(t *testing.T) {
	result := ValidateTaskHardCheckShape(validTaskHardChecks())
	if !result.ValidShape || len(result.Checks) != 7 || len(result.Issues) != 0 {
		t.Fatalf("ValidateTaskHardCheckShape() = %+v, want a valid shape", result)
	}
	if result.Checks[2].Outcome != TaskHardCheckNotApplicable || result.Checks[2].NotApplicableReason != TaskHardCheckNoTaskExecution ||
		result.Checks[4].Outcome != TaskHardCheckNotApplicable || result.Checks[4].NotApplicableReason != TaskHardCheckNoIrreversibleAction ||
		result.Checks[5].Outcome != TaskHardCheckNotApplicable || result.Checks[5].NotApplicableReason != TaskHardCheckNoRepeatedError ||
		result.Checks[6].Outcome != TaskHardCheckNotApplicable || result.Checks[6].NotApplicableReason != TaskHardCheckNoTaskExecution {
		t.Fatalf("N/A checks lost their server reason codes: %+v", result.Checks)
	}
}

func TestValidateTaskHardCheckShapePreservesNonBlockingNotesClaim(t *testing.T) {
	checks := validTaskHardChecks()
	checks[1].HasNonBlockingNotes = true
	result := ValidateTaskHardCheckShape(checks)
	if !result.ValidShape || !result.Checks[1].HasNonBlockingNotes {
		t.Fatalf("ValidateTaskHardCheckShape() = %+v, want a valid shape with the supplied notes claim", result)
	}
}

func TestValidateTaskHardCheckShapeDoesNotConvertReviewClaimsIntoVerdict(t *testing.T) {
	checks := validTaskHardChecks()
	checks[3].Outcome = TaskHardCheckFail
	result := ValidateTaskHardCheckShape(checks)
	if !result.ValidShape || result.Checks[3].Outcome != TaskHardCheckFail {
		t.Fatalf("ValidateTaskHardCheckShape() = %+v, want a structurally valid candidate FAIL without a final verdict", result)
	}
}

func TestValidateTaskHardCheckShapeRejectsMissingCheck(t *testing.T) {
	checks := validTaskHardChecks()[:6]
	result := ValidateTaskHardCheckShape(checks)
	if result.ValidShape || result.Checks[6].Outcome != TaskHardCheckFail ||
		!hasTaskValidationIssue(result, TaskValidationIssueMissingCheck) {
		t.Fatalf("ValidateTaskHardCheckShape() = %+v, want missing-check issue", result)
	}
}

func TestValidateTaskHardCheckShapeRejectsInvalidNotApplicableReason(t *testing.T) {
	checks := validTaskHardChecks()
	checks[2].NotApplicableReason = TaskHardCheckNotApplicableReason("user supplied reason")
	result := ValidateTaskHardCheckShape(checks)
	if result.ValidShape || result.Checks[2].Outcome != TaskHardCheckFail ||
		!hasTaskValidationIssue(result, TaskValidationIssueInvalidNotApplicableReason) {
		t.Fatalf("ValidateTaskHardCheckShape() = %+v, want invalid N/A issue", result)
	}
}

func TestValidateTaskHardCheckShapeRejectsInvalidOutcome(t *testing.T) {
	checks := validTaskHardChecks()
	checks[0].Outcome = TaskHardCheckOutcome("unknown")
	result := ValidateTaskHardCheckShape(checks)
	if result.ValidShape || result.Checks[0].Outcome != TaskHardCheckFail ||
		!hasTaskValidationIssue(result, TaskValidationIssueInvalidOutcome) {
		t.Fatalf("ValidateTaskHardCheckShape() = %+v, want invalid-outcome issue", result)
	}
}

func TestValidateTaskHardCheckShapeRejectsRepeatedCheck(t *testing.T) {
	checks := validTaskHardChecks()
	checks[6] = checks[0]
	result := ValidateTaskHardCheckShape(checks)
	if result.ValidShape || result.Checks[0].Outcome != TaskHardCheckFail ||
		!hasTaskValidationIssue(result, TaskValidationIssueDuplicateCheck) {
		t.Fatalf("ValidateTaskHardCheckShape() = %+v, want duplicate-check issue", result)
	}
}

func TestValidateTaskHardCheckShapeRejectsPassedCheckWithoutEvidence(t *testing.T) {
	checks := validTaskHardChecks()
	checks[0].EvidenceRefs = nil
	result := ValidateTaskHardCheckShape(checks)
	if result.ValidShape || result.Checks[0].Outcome != TaskHardCheckFail ||
		!hasTaskValidationIssue(result, TaskValidationIssueMissingEvidence) {
		t.Fatalf("ValidateTaskHardCheckShape() = %+v, want missing-evidence issue", result)
	}
}

func TestValidateTaskHardCheckShapeRejectsInvalidEvidenceReferenceWithoutEchoingIt(t *testing.T) {
	checks := validTaskHardChecks()
	checks[0].EvidenceRefs = append(checks[0].EvidenceRefs, TaskValidationEvidenceRef{
		Kind: TaskValidationEvidenceSource, ID: "private user content",
	})
	result := ValidateTaskHardCheckShape(checks)
	if result.ValidShape || result.Checks[0].Outcome != TaskHardCheckFail ||
		!hasTaskValidationIssue(result, TaskValidationIssueInvalidEvidenceReference) {
		t.Fatalf("ValidateTaskHardCheckShape() = %+v, want invalid-evidence issue", result)
	}
	for _, ref := range result.Checks[0].EvidenceRefs {
		if ref.ID == "private user content" {
			t.Fatal("invalid evidence reference was returned")
		}
	}
}

func TestValidateTaskHardCheckShapeRejectsRepeatedEvidenceReference(t *testing.T) {
	checks := validTaskHardChecks()
	checks[0].EvidenceRefs = append(checks[0].EvidenceRefs, checks[0].EvidenceRefs[0])
	result := ValidateTaskHardCheckShape(checks)
	if result.ValidShape || result.Checks[0].Outcome != TaskHardCheckFail ||
		!hasTaskValidationIssue(result, TaskValidationIssueDuplicateEvidenceReference) {
		t.Fatalf("ValidateTaskHardCheckShape() = %+v, want duplicate-evidence issue", result)
	}
}

func TestValidateTaskHardCheckShapeRejectsNotApplicableForRequiredCheck(t *testing.T) {
	checks := validTaskHardChecks()
	checks[0].Outcome = TaskHardCheckNotApplicable
	checks[0].NotApplicableReason = TaskHardCheckNoTaskExecution
	result := ValidateTaskHardCheckShape(checks)
	if result.ValidShape || result.Checks[0].Outcome != TaskHardCheckFail ||
		!hasTaskValidationIssue(result, TaskValidationIssueInvalidNotApplicableReason) {
		t.Fatalf("ValidateTaskHardCheckShape() = %+v, want forbidden N/A issue", result)
	}
}

func TestValidateTaskHardCheckShapeBoundsCheckCount(t *testing.T) {
	checks := validTaskHardChecks()
	checks = append(checks, TaskHardCheckClaim{ID: TaskHardCheckConclusionFirst, Outcome: TaskHardCheckPass})
	result := ValidateTaskHardCheckShape(checks)
	if result.ValidShape || !hasTaskValidationIssue(result, TaskValidationIssueTooManyChecks) {
		t.Fatalf("ValidateTaskHardCheckShape() = %+v, want oversized-input issue", result)
	}
}

func TestValidateTaskResultClaimsRequiresBoundedUTF8RangesAndEvidence(t *testing.T) {
	result := "已完成"
	valid := []TaskResultClaim{{ID: "claim-1", StartByte: 0, EndByte: int32(len(result)), EvidenceRefs: []TaskValidationEvidenceRef{
		{Kind: TaskValidationEvidenceSource, ID: "m1234567"},
	}}}
	if err := ValidateTaskResultClaims(result, valid); err != nil {
		t.Fatalf("valid Task result claims error = %v", err)
	}
	modelVisible := []TaskResultClaim{{ID: "claim-1", StartByte: 0, EndByte: int32(len(result)), EvidenceRefs: []TaskValidationEvidenceRef{
		{Kind: TaskValidationEvidenceSource, ID: "current"},
		{Kind: TaskValidationEvidenceAction, CallID: "tool-call-4"},
		{Kind: TaskValidationEvidenceSideEffect, CallID: "write-1"},
		{Kind: TaskValidationEvidenceResult, ID: "current"},
	}}}
	if err := ValidateTaskResultClaims(result, modelVisible); err != nil {
		t.Fatalf("model-visible Task result evidence references error = %v", err)
	}
	for _, claims := range [][]TaskResultClaim{
		{{ID: "claim-1", StartByte: 1, EndByte: 3, EvidenceRefs: valid[0].EvidenceRefs}},
		{{ID: "claim-1", StartByte: 0, EndByte: 3, EvidenceRefs: nil}},
		{{ID: "claim-1", StartByte: 0, EndByte: int32(len(result)), EvidenceRefs: []TaskValidationEvidenceRef{{Kind: TaskValidationEvidenceReview, ID: "s1234567"}}}},
		{{ID: "claim-1", StartByte: 0, EndByte: int32(len(result)), EvidenceRefs: []TaskValidationEvidenceRef{{Kind: TaskValidationEvidenceSource, ID: "m1234567", CallID: "tool-call-4"}}}},
		{{ID: "claim-1", StartByte: 0, EndByte: int32(len(result)), EvidenceRefs: []TaskValidationEvidenceRef{{Kind: TaskValidationEvidenceSource, CallID: "tool-call-4"}}}},
		{{ID: "claim-1", StartByte: 0, EndByte: int32(len(result)), EvidenceRefs: []TaskValidationEvidenceRef{{Kind: TaskValidationEvidenceAction, CallID: "private call id"}}}},
		{valid[0], valid[0]},
	} {
		if err := ValidateTaskResultClaims(result, claims); err == nil {
			t.Fatalf("invalid Task result claims accepted: %+v", claims)
		}
	}
}

func TestValidateTaskResultClaimsRequiresComparablePerformanceEvidence(t *testing.T) {
	result := "Reduced response time from 120 to 90 ms."
	newClaim := func() []TaskResultClaim {
		return []TaskResultClaim{{ID: "claim-1", StartByte: 0, EndByte: int32(len(result)),
			EvidenceRefs: []TaskValidationEvidenceRef{{Kind: TaskValidationEvidenceSource, ID: "current"}},
			PerformanceComparison: &TaskResultPerformanceComparison{
				Metric: "response time", Unit: "ms", Before: 120, After: 90, Environment: "same test machine",
				BaselineCallID: "baseline_1", AfterCallID: "after_1",
			},
		}}
	}
	if err := ValidateTaskResultClaims(result, newClaim()); err != nil {
		t.Fatalf("valid performance evidence error = %v", err)
	}
	for name, mutate := range map[string]func(*TaskResultPerformanceComparison){
		"missing unit":          func(value *TaskResultPerformanceComparison) { value.Unit = "" },
		"non-finite value":      func(value *TaskResultPerformanceComparison) { value.Before = math.NaN() },
		"same call":             func(value *TaskResultPerformanceComparison) { value.AfterCallID = value.BaselineCallID },
		"invalid call id":       func(value *TaskResultPerformanceComparison) { value.AfterCallID = "private value" },
		"multiline environment": func(value *TaskResultPerformanceComparison) { value.Environment = "machine\nwith extra text" },
	} {
		claims := newClaim()
		mutate(claims[0].PerformanceComparison)
		if err := ValidateTaskResultClaims(result, claims); err == nil {
			t.Errorf("invalid performance evidence accepted: %s", name)
		}
	}
}

func TestValidateTaskHardCheckShapeDoesNotAcceptModelVisibleCallIDs(t *testing.T) {
	checks := validTaskHardChecks()
	checks[0].EvidenceRefs = []TaskValidationEvidenceRef{{Kind: TaskValidationEvidenceAction, CallID: "tool-call-4"}}
	result := ValidateTaskHardCheckShape(checks)
	if result.ValidShape || !hasTaskValidationIssue(result, TaskValidationIssueInvalidEvidenceReference) {
		t.Fatalf("ValidateTaskHardCheckShape() = %+v, want call ID rejected", result)
	}
}

func TestValidateTaskValidationEvidenceRecordBindsResultRevisionAndSequence(t *testing.T) {
	result := "Changed file and verified it."
	digest := sha256.Sum256([]byte(result))
	record := TaskValidationEvidenceRecord{
		TaskRevision: 2, EventSequence: 7, ResultSHA256: hex.EncodeToString(digest[:]),
		Claims: []TaskValidationEvidenceClaimRecord{{ID: "c1", StartByte: 0, EndByte: int32(len(result)), EvidenceRefs: []TaskValidationEvidenceRefRecord{
			{Kind: TaskValidationEvidenceAction, ID: "s1234567", TaskRevision: 2, Sequence: 4, CallID: "tool-call-4"},
			{Kind: TaskValidationEvidenceSideEffect, ID: "a1234567", TaskRevision: 2, Sequence: 5, CallID: "write-1"},
		}, PerformanceComparison: &TaskValidationPerformanceComparison{Metric: "elapsed", Unit: "ms", Before: 120, After: 90, Delta: -30,
			Environment: "same machine", BaselineRef: TaskValidationEvidenceRefRecord{Kind: TaskValidationEvidenceBaseline,
				ID: "s2345678", TaskRevision: 2, Sequence: 6, CallID: "baseline-1"},
			AfterRef: TaskValidationEvidenceRefRecord{Kind: TaskValidationEvidenceBeforeAfter,
				ID: "s3456789", TaskRevision: 2, Sequence: 7, CallID: "after-1"}},
		}},
	}
	if err := ValidateTaskValidationEvidenceRecord(result, record); err != nil {
		t.Fatalf("valid Task validation evidence record error = %v", err)
	}
	record.Claims[0].EvidenceRefs[0].CallID = "private call id"
	if err := ValidateTaskValidationEvidenceRecord(result, record); err == nil {
		t.Fatal("non-opaque tool call ID was accepted")
	}
	record.Claims[0].EvidenceRefs[0].CallID = "tool-call-4"
	record.Claims[0].EvidenceRefs[0].TaskRevision = 1
	if err := ValidateTaskValidationEvidenceRecord(result, record); err == nil {
		t.Fatal("evidence from another Task revision was accepted")
	}
	record.Claims[0].EvidenceRefs[0].TaskRevision = 2
	record.Claims[0].EvidenceRefs[1].CallID = "private call id"
	if err := ValidateTaskValidationEvidenceRecord(result, record); err == nil {
		t.Fatal("non-opaque artifact source call ID was accepted")
	}
	record.Claims[0].EvidenceRefs[1].CallID = "write-1"
	record.Claims[0].PerformanceComparison.Delta = -29
	if err := ValidateTaskValidationEvidenceRecord(result, record); err == nil {
		t.Fatal("client-supplied performance delta was accepted")
	}
}

func TestRestrictTaskValidationEvidenceHidesRevokedWorkspaceReferences(t *testing.T) {
	record := &TaskValidationEvidenceRecord{Claims: []TaskValidationEvidenceClaimRecord{
		{ID: "c1", EvidenceRefs: []TaskValidationEvidenceRefRecord{
			{Kind: TaskValidationEvidenceSource, ID: "m1234567"},
			{Kind: TaskValidationEvidenceAction, ID: "s1234567"},
			{Kind: TaskValidationEvidenceAction, ID: "s2345678"},
			{Kind: TaskValidationEvidenceSideEffect, ID: "a1234567"},
		}, PerformanceComparison: &TaskValidationPerformanceComparison{Metric: "latency", Unit: "ms", Before: 100, After: 90, Delta: -10,
			Environment: "private workspace", BaselineRef: TaskValidationEvidenceRefRecord{Kind: TaskValidationEvidenceBaseline, ID: "s1234567"},
			AfterRef: TaskValidationEvidenceRefRecord{Kind: TaskValidationEvidenceBeforeAfter, ID: "s2345678"}}},
		{ID: "c2", EvidenceRefs: []TaskValidationEvidenceRefRecord{
			{Kind: TaskValidationEvidenceSideEffect, ID: "a2345678"},
		}},
	}}
	filtered := restrictTaskValidationEvidence(record, []task.Step{
		{ID: "s1234567", ToolName: "morde_workspace_file_read"}, {ID: "s2345678", ToolName: "browser_open"},
	}, true, true)
	if filtered == nil || len(filtered.Claims) != 1 || filtered.Claims[0].PerformanceComparison != nil || len(filtered.Claims[0].EvidenceRefs) != 2 ||
		filtered.Claims[0].EvidenceRefs[0].ID != "m1234567" || filtered.Claims[0].EvidenceRefs[1].ID != "s2345678" {
		t.Fatalf("restricted validation evidence = %#v", filtered)
	}
}

func validTaskHardChecks() []TaskHardCheckClaim {
	ref := func(kind TaskValidationEvidenceKind, id string) []TaskValidationEvidenceRef {
		return []TaskValidationEvidenceRef{{Kind: kind, ID: id}}
	}
	return []TaskHardCheckClaim{
		{ID: TaskHardCheckConclusionFirst, Outcome: TaskHardCheckPass, EvidenceRefs: ref(TaskValidationEvidenceResult, "m1234567")},
		{ID: TaskHardCheckEvidence, Outcome: TaskHardCheckPass, EvidenceRefs: []TaskValidationEvidenceRef{
			{Kind: TaskValidationEvidenceBaseline, ID: "s1234567"},
			{Kind: TaskValidationEvidenceAction, ID: "s2345678"},
			{Kind: TaskValidationEvidenceResult, ID: "s3456789"},
			{Kind: TaskValidationEvidenceSideEffect, ID: "o1234567"},
		}},
		{ID: TaskHardCheckIndependent, Outcome: TaskHardCheckNotApplicable, NotApplicableReason: TaskHardCheckNoTaskExecution},
		{ID: TaskHardCheckClarification, Outcome: TaskHardCheckPass, EvidenceRefs: ref(TaskValidationEvidenceSource, "m2345678")},
		{ID: TaskHardCheckApproval, Outcome: TaskHardCheckNotApplicable, NotApplicableReason: TaskHardCheckNoIrreversibleAction},
		{ID: TaskHardCheckCorrection, Outcome: TaskHardCheckNotApplicable, NotApplicableReason: TaskHardCheckNoRepeatedError},
		{ID: TaskHardCheckReplay, Outcome: TaskHardCheckNotApplicable, NotApplicableReason: TaskHardCheckNoTaskExecution},
	}
}

func hasTaskValidationIssue(result TaskHardCheckShapeValidation, want TaskValidationIssue) bool {
	for _, issue := range result.Issues {
		if issue == want {
			return true
		}
	}
	return false
}
