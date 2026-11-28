package app

// TaskValidationCheckOutcome is a server-owned evaluation state, not a model
// claim or Task lifecycle state.
type TaskValidationCheckOutcome string

const (
	TaskValidationCheckPending       TaskValidationCheckOutcome = "pending"
	TaskValidationCheckPass          TaskValidationCheckOutcome = "PASS"
	TaskValidationCheckFail          TaskValidationCheckOutcome = "FAIL"
	TaskValidationCheckNotApplicable TaskValidationCheckOutcome = "N/A"
)

type TaskValidationFactState string

const (
	TaskValidationFactUnknown TaskValidationFactState = "unknown"
	TaskValidationFactAbsent  TaskValidationFactState = "absent"
	TaskValidationFactPresent TaskValidationFactState = "present"
)

// TaskValidationVerdictFacts contains server-read applicability and evidence
// completeness facts. Callers must not populate it from a model report.
type TaskValidationVerdictFacts struct {
	TaskExecution             TaskValidationFactState
	IrreversibleAction        TaskValidationFactState
	RepeatedError             TaskValidationFactState
	EvidenceContentVerified   bool
	ClarificationHistoryKnown bool
	ApprovalHistoryVerified   bool
	CorrectionRecordVerified  bool
	ReplayVerified            bool
	ReplayFailed              bool
}

// TaskValidationCheckEvaluation must be built from current server facts and a
// validated independent review; it must never be copied from a model report.
type TaskValidationCheckEvaluation struct {
	ID                  TaskHardCheckID
	Outcome             TaskValidationCheckOutcome
	NotApplicableReason TaskHardCheckNotApplicableReason
	NonBlockingNote     string
}

// SummarizeTaskValidationEvaluations folds the seven ordered server-owned
// checks into the public summary. Invalid or incomplete inputs fail closed.
func SummarizeTaskValidationEvaluations(evaluations []TaskValidationCheckEvaluation) TaskValidationSummary {
	pending := TaskValidationSummary{State: TaskValidationPending}
	if len(evaluations) != len(taskHardCheckOrder) {
		return pending
	}

	hasPending, hasFailure, hasNotes := false, false, false
	for index, evaluation := range evaluations {
		if evaluation.ID != taskHardCheckOrder[index] {
			return pending
		}
		switch evaluation.Outcome {
		case TaskValidationCheckPending:
			if evaluation.NotApplicableReason != "" || evaluation.NonBlockingNote != "" {
				return pending
			}
			hasPending = true
		case TaskValidationCheckPass:
			if evaluation.NotApplicableReason != "" || !validTaskValidationEvaluationNote(evaluation.NonBlockingNote) {
				return pending
			}
			hasNotes = hasNotes || evaluation.NonBlockingNote != ""
		case TaskValidationCheckFail:
			if evaluation.NotApplicableReason != "" || evaluation.NonBlockingNote != "" {
				return pending
			}
			hasFailure = true
		case TaskValidationCheckNotApplicable:
			if evaluation.NonBlockingNote != "" ||
				!validTaskHardCheckNotApplicableReason(evaluation.ID, evaluation.NotApplicableReason) {
				return pending
			}
		default:
			return pending
		}
	}

	if hasFailure {
		return TaskValidationSummary{State: TaskValidationFail}
	}
	if hasPending {
		return pending
	}
	if hasNotes {
		return TaskValidationSummary{State: TaskValidationPassNotes}
	}
	return TaskValidationSummary{State: TaskValidationPass}
}

// EvaluateTaskValidationReview combines a validated independent review with
// server-owned facts. Unavailable facts remain pending; only the server may
// derive task-specific N/A outcomes.
func EvaluateTaskValidationReview(report TaskValidationReviewReport, facts TaskValidationVerdictFacts) []TaskValidationCheckEvaluation {
	evaluations := make([]TaskValidationCheckEvaluation, len(taskHardCheckOrder))
	// shortcut: keep unstructured PASS+NOTES pending; add it only when notes bind to check evidence.
	if (report.Recommendation != TaskValidationPass && report.Recommendation != TaskValidationPassNotes && report.Recommendation != TaskValidationFail) ||
		report.Recommendation == TaskValidationPassNotes || len(report.Findings) != len(taskHardCheckOrder) {
		return pendingTaskValidationEvaluations()
	}
	hasNonSupportingFinding := false
	for index, finding := range report.Findings {
		if finding.ID != taskHardCheckOrder[index] || (finding.Assessment != TaskValidationReviewSupports &&
			finding.Assessment != TaskValidationReviewContradicts && finding.Assessment != TaskValidationReviewInsufficient) {
			return pendingTaskValidationEvaluations()
		}
		hasNonSupportingFinding = hasNonSupportingFinding || finding.Assessment != TaskValidationReviewSupports
	}
	if (report.Recommendation == TaskValidationFail) != hasNonSupportingFinding {
		return pendingTaskValidationEvaluations()
	}

	for index, id := range taskHardCheckOrder {
		evaluation := TaskValidationCheckEvaluation{ID: id, Outcome: TaskValidationCheckPending}
		finding := report.Findings[index]

		switch id {
		case TaskHardCheckConclusionFirst:
			evaluation = taskValidationReviewFindingEvaluation(finding)
		case TaskHardCheckEvidence:
			if finding.Assessment == TaskValidationReviewContradicts || facts.EvidenceContentVerified {
				evaluation = taskValidationReviewFindingEvaluation(finding)
			}
		case TaskHardCheckIndependent, TaskHardCheckReplay:
			switch facts.TaskExecution {
			case TaskValidationFactAbsent:
				evaluation.Outcome = TaskValidationCheckNotApplicable
				evaluation.NotApplicableReason = TaskHardCheckNoTaskExecution
			case TaskValidationFactPresent:
				if id == TaskHardCheckIndependent {
					evaluation = taskValidationReviewFindingEvaluation(finding)
				} else if facts.ReplayFailed {
					evaluation.Outcome = TaskValidationCheckFail
				} else if facts.ReplayVerified {
					evaluation = taskValidationReviewFindingEvaluation(finding)
				}
			}
		case TaskHardCheckClarification:
			if facts.ClarificationHistoryKnown {
				evaluation = taskValidationReviewFindingEvaluation(finding)
			}
		case TaskHardCheckApproval:
			switch facts.IrreversibleAction {
			case TaskValidationFactAbsent:
				evaluation.Outcome = TaskValidationCheckNotApplicable
				evaluation.NotApplicableReason = TaskHardCheckNoIrreversibleAction
			case TaskValidationFactPresent:
				if facts.ApprovalHistoryVerified {
					evaluation = taskValidationReviewFindingEvaluation(finding)
				}
			}
		case TaskHardCheckCorrection:
			switch facts.RepeatedError {
			case TaskValidationFactAbsent:
				evaluation.Outcome = TaskValidationCheckNotApplicable
				evaluation.NotApplicableReason = TaskHardCheckNoRepeatedError
			case TaskValidationFactPresent:
				if facts.CorrectionRecordVerified {
					evaluation = taskValidationReviewFindingEvaluation(finding)
				}
			}
		}
		evaluations[index] = evaluation
	}
	return evaluations
}

func pendingTaskValidationEvaluations() []TaskValidationCheckEvaluation {
	evaluations := make([]TaskValidationCheckEvaluation, len(taskHardCheckOrder))
	for index, id := range taskHardCheckOrder {
		evaluations[index] = TaskValidationCheckEvaluation{ID: id, Outcome: TaskValidationCheckPending}
	}
	return evaluations
}

func taskValidationReviewFindingEvaluation(finding TaskValidationReviewFinding) TaskValidationCheckEvaluation {
	evaluation := TaskValidationCheckEvaluation{ID: finding.ID, Outcome: TaskValidationCheckPending}
	switch finding.Assessment {
	case TaskValidationReviewSupports:
		evaluation.Outcome = TaskValidationCheckPass
	case TaskValidationReviewContradicts:
		evaluation.Outcome = TaskValidationCheckFail
	}
	return evaluation
}

func validTaskValidationEvaluationNote(note string) bool {
	if note == "" {
		return true
	}
	return validTaskValidationReviewText(note, maxTaskValidationReviewRationaleBytes)
}
