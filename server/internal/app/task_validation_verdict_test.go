package app

import "testing"

func TestTaskValidationVerdictPassesSevenServerChecks(t *testing.T) {
	if got := SummarizeTaskValidationEvaluations(validTaskValidationEvaluations()); got.State != TaskValidationPass {
		t.Fatalf("SummarizeTaskValidationEvaluations() = %q, want PASS", got.State)
	}
}

func TestTaskValidationVerdictPassesWithExplicitNonBlockingNote(t *testing.T) {
	evaluations := validTaskValidationEvaluations()
	evaluations[1].NonBlockingNote = "One optional detail remains unverified."
	if got := SummarizeTaskValidationEvaluations(evaluations); got.State != TaskValidationPassNotes {
		t.Fatalf("SummarizeTaskValidationEvaluations() = %q, want PASS+NOTES", got.State)
	}
}

func TestTaskValidationVerdictFailsApplicableCheck(t *testing.T) {
	evaluations := validTaskValidationEvaluations()
	evaluations[3].Outcome = TaskValidationCheckFail
	if got := SummarizeTaskValidationEvaluations(evaluations); got.State != TaskValidationFail {
		t.Fatalf("SummarizeTaskValidationEvaluations() = %q, want FAIL", got.State)
	}
}

func TestTaskValidationVerdictFailureTakesPrecedenceOverPending(t *testing.T) {
	evaluations := validTaskValidationEvaluations()
	evaluations[1].Outcome = TaskValidationCheckFail
	evaluations[5].Outcome = TaskValidationCheckPending
	if got := SummarizeTaskValidationEvaluations(evaluations); got.State != TaskValidationFail {
		t.Fatalf("SummarizeTaskValidationEvaluations() = %q, want FAIL", got.State)
	}
}

func TestEvaluateTaskValidationReviewDerivesNAButKeepsUnknownExecutionFactsPending(t *testing.T) {
	report := validTaskValidationReviewReport()
	textOnlyFacts := TaskValidationVerdictFacts{
		TaskExecution:             TaskValidationFactAbsent,
		IrreversibleAction:        TaskValidationFactAbsent,
		RepeatedError:             TaskValidationFactAbsent,
		EvidenceContentVerified:   true,
		ClarificationHistoryKnown: true,
	}
	if got := SummarizeTaskValidationEvaluations(EvaluateTaskValidationReview(report, textOnlyFacts)); got.State != TaskValidationPass {
		t.Fatalf("text-only Task verdict = %q, want PASS", got.State)
	}

	toolFacts := TaskValidationVerdictFacts{TaskExecution: TaskValidationFactPresent}
	evaluations := EvaluateTaskValidationReview(report, toolFacts)
	if evaluations[indexTaskHardCheck(TaskHardCheckApproval)].Outcome != TaskValidationCheckPending ||
		evaluations[indexTaskHardCheck(TaskHardCheckCorrection)].Outcome != TaskValidationCheckPending ||
		evaluations[indexTaskHardCheck(TaskHardCheckReplay)].Outcome != TaskValidationCheckPending {
		t.Fatalf("tool Task trusted reviewer claims without server facts: %+v", evaluations)
	}
	if got := SummarizeTaskValidationEvaluations(evaluations); got.State != TaskValidationPending {
		t.Fatalf("tool Task verdict = %q, want pending", got.State)
	}
}

func TestEvaluateTaskValidationReviewRequiresVerifiedEvidenceContentForH02(t *testing.T) {
	facts := TaskValidationVerdictFacts{
		TaskExecution:             TaskValidationFactAbsent,
		IrreversibleAction:        TaskValidationFactAbsent,
		RepeatedError:             TaskValidationFactAbsent,
		ClarificationHistoryKnown: true,
	}
	evaluations := EvaluateTaskValidationReview(validTaskValidationReviewReport(), facts)
	if evaluations[indexTaskHardCheck(TaskHardCheckEvidence)].Outcome != TaskValidationCheckPending {
		t.Fatalf("H02 accepted a model support claim without verified content: %+v", evaluations)
	}
	if got := SummarizeTaskValidationEvaluations(evaluations); got.State != TaskValidationPending {
		t.Fatalf("unverified H02 verdict = %q, want pending", got.State)
	}
}

func TestEvaluateTaskValidationReviewRequiresEvidenceBoundNotes(t *testing.T) {
	report := validTaskValidationReviewReport()
	report.Recommendation = TaskValidationPassNotes
	facts := TaskValidationVerdictFacts{
		TaskExecution:             TaskValidationFactAbsent,
		IrreversibleAction:        TaskValidationFactAbsent,
		RepeatedError:             TaskValidationFactAbsent,
		EvidenceContentVerified:   true,
		ClarificationHistoryKnown: true,
	}
	if got := SummarizeTaskValidationEvaluations(EvaluateTaskValidationReview(report, facts)); got.State != TaskValidationPending {
		t.Fatalf("unstructured PASS+NOTES review = %q, want pending", got.State)
	}
}

func TestEvaluateTaskValidationReviewFailureSurfacesWithOtherChecksPending(t *testing.T) {
	report := validTaskValidationReviewReport()
	report.Findings[0].Assessment = TaskValidationReviewContradicts
	report.Recommendation = TaskValidationFail
	got := SummarizeTaskValidationEvaluations(EvaluateTaskValidationReview(report, TaskValidationVerdictFacts{
		TaskExecution: TaskValidationFactPresent,
	}))
	if got.State != TaskValidationFail {
		t.Fatalf("failed check with incomplete server facts = %q, want FAIL", got.State)
	}
}

func TestEvaluateTaskValidationReviewRejectsIncompleteReview(t *testing.T) {
	report := validTaskValidationReviewReport()
	report.Findings = report.Findings[:6]
	if got := SummarizeTaskValidationEvaluations(EvaluateTaskValidationReview(report, TaskValidationVerdictFacts{
		TaskExecution:             TaskValidationFactAbsent,
		IrreversibleAction:        TaskValidationFactAbsent,
		RepeatedError:             TaskValidationFactAbsent,
		ClarificationHistoryKnown: true,
	})); got.State != TaskValidationPending {
		t.Fatalf("incomplete review verdict = %q, want pending", got.State)
	}
}

func TestEvaluateTaskValidationReviewRejectsInconsistentRecommendation(t *testing.T) {
	for name, report := range map[string]TaskValidationReviewReport{
		"unknown recommendation": func() TaskValidationReviewReport {
			report := validTaskValidationReviewReport()
			report.Recommendation = "unknown"
			return report
		}(),
		"fail without non-supporting finding": func() TaskValidationReviewReport {
			report := validTaskValidationReviewReport()
			report.Recommendation = TaskValidationFail
			return report
		}(),
		"pass with contradiction": func() TaskValidationReviewReport {
			report := validTaskValidationReviewReport()
			report.Findings[0].Assessment = TaskValidationReviewContradicts
			return report
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if got := SummarizeTaskValidationEvaluations(EvaluateTaskValidationReview(report, TaskValidationVerdictFacts{
				TaskExecution:             TaskValidationFactAbsent,
				IrreversibleAction:        TaskValidationFactAbsent,
				RepeatedError:             TaskValidationFactAbsent,
				EvidenceContentVerified:   true,
				ClarificationHistoryKnown: true,
			})); got.State != TaskValidationPending {
				t.Fatalf("inconsistent report verdict = %q, want pending", got.State)
			}
		})
	}
}

func TestTaskValidationVerdictAcceptsOnlyServerApplicableReasons(t *testing.T) {
	evaluations := validTaskValidationEvaluations()
	evaluations[2] = TaskValidationCheckEvaluation{ID: TaskHardCheckIndependent, Outcome: TaskValidationCheckNotApplicable,
		NotApplicableReason: TaskHardCheckNoTaskExecution}
	evaluations[4] = TaskValidationCheckEvaluation{ID: TaskHardCheckApproval, Outcome: TaskValidationCheckNotApplicable,
		NotApplicableReason: TaskHardCheckNoIrreversibleAction}
	evaluations[5] = TaskValidationCheckEvaluation{ID: TaskHardCheckCorrection, Outcome: TaskValidationCheckNotApplicable,
		NotApplicableReason: TaskHardCheckNoRepeatedError}
	evaluations[6] = TaskValidationCheckEvaluation{ID: TaskHardCheckReplay, Outcome: TaskValidationCheckNotApplicable,
		NotApplicableReason: TaskHardCheckNoTaskExecution}
	if got := SummarizeTaskValidationEvaluations(evaluations); got.State != TaskValidationPass {
		t.Fatalf("SummarizeTaskValidationEvaluations() = %q, want PASS with server-derived N/A checks", got.State)
	}
}

func TestTaskValidationVerdictRejectsForgedNotApplicableReason(t *testing.T) {
	for _, evaluation := range []TaskValidationCheckEvaluation{
		{ID: TaskHardCheckConclusionFirst, Outcome: TaskValidationCheckNotApplicable, NotApplicableReason: TaskHardCheckNoTaskExecution},
		{ID: TaskHardCheckApproval, Outcome: TaskValidationCheckNotApplicable, NotApplicableReason: TaskHardCheckNoTaskExecution},
		{ID: TaskHardCheckReplay, Outcome: TaskValidationCheckNotApplicable, NotApplicableReason: TaskHardCheckNoRepeatedError},
	} {
		evaluations := validTaskValidationEvaluations()
		evaluations[indexTaskHardCheck(evaluation.ID)] = evaluation
		if got := SummarizeTaskValidationEvaluations(evaluations); got.State != TaskValidationPending {
			t.Errorf("SummarizeTaskValidationEvaluations(%+v) = %q, want pending", evaluation, got.State)
		}
	}
}

func TestTaskValidationVerdictRejectsIncompleteOrMalformedChecks(t *testing.T) {
	valid := validTaskValidationEvaluations()
	for name, evaluations := range map[string][]TaskValidationCheckEvaluation{
		"missing":   valid[:6],
		"duplicate": append(append([]TaskValidationCheckEvaluation(nil), valid[:6]...), valid[0]),
		"unknown": func() []TaskValidationCheckEvaluation {
			copy := append([]TaskValidationCheckEvaluation(nil), valid...)
			copy[6].ID = "H99"
			return copy
		}(),
		"out_of_order": func() []TaskValidationCheckEvaluation {
			copy := append([]TaskValidationCheckEvaluation(nil), valid...)
			copy[0], copy[1] = copy[1], copy[0]
			return copy
		}(),
		"invalid outcome": func() []TaskValidationCheckEvaluation {
			copy := append([]TaskValidationCheckEvaluation(nil), valid...)
			copy[0].Outcome = "PASS+NOTES"
			return copy
		}(),
		"unstructured note": func() []TaskValidationCheckEvaluation {
			copy := append([]TaskValidationCheckEvaluation(nil), valid...)
			copy[0].NonBlockingNote = " note "
			return copy
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if got := SummarizeTaskValidationEvaluations(evaluations); got.State != TaskValidationPending {
				t.Fatalf("SummarizeTaskValidationEvaluations() = %q, want pending", got.State)
			}
		})
	}
}

func indexTaskHardCheck(id TaskHardCheckID) int {
	for index, current := range taskHardCheckOrder {
		if current == id {
			return index
		}
	}
	return -1
}

func validTaskValidationEvaluations() []TaskValidationCheckEvaluation {
	evaluations := make([]TaskValidationCheckEvaluation, len(taskHardCheckOrder))
	for index, id := range taskHardCheckOrder {
		evaluations[index] = TaskValidationCheckEvaluation{ID: id, Outcome: TaskValidationCheckPass}
	}
	return evaluations
}

func validTaskValidationReviewReport() TaskValidationReviewReport {
	findings := make([]TaskValidationReviewFinding, len(taskHardCheckOrder))
	for index, id := range taskHardCheckOrder {
		findings[index] = TaskValidationReviewFinding{ID: id, Assessment: TaskValidationReviewSupports}
	}
	return TaskValidationReviewReport{Recommendation: TaskValidationPass, Findings: findings}
}
