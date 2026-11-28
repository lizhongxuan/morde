package task

import "testing"

func TestBudgetValidate(t *testing.T) {
	if err := (Budget{ActiveSeconds: 1800, ToolCalls: 100, ModelTokens: 50000}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, budget := range []Budget{
		{ActiveSeconds: 1801, ToolCalls: 100, ModelTokens: 50000},
		{ActiveSeconds: 1, ToolCalls: 101, ModelTokens: 50000},
		{ActiveSeconds: 1, ToolCalls: 1, ModelTokens: 50001},
		{ActiveSeconds: 0, ToolCalls: 1, ModelTokens: 1},
	} {
		if err := budget.Validate(); err == nil {
			t.Errorf("budget %#v accepted", budget)
		}
	}
}

func TestTaskTransitions(t *testing.T) {
	for _, tc := range []struct {
		from, to Status
		want     bool
	}{
		{StatusQueued, StatusRunning, true},
		{StatusRunning, StatusPausing, true},
		{StatusPausing, StatusPaused, true},
		{StatusCancelling, StatusCancelled, true},
		{StatusCompleted, StatusRunning, false},
	} {
		if got := CanTransition(tc.from, tc.to); got != tc.want {
			t.Errorf("CanTransition(%q, %q) = %t, want %t", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestTaskTransitionsWithReasonOnlyRequeuesOrPausesCapacityWaits(t *testing.T) {
	for _, tc := range []struct {
		from, to Status
		reason   string
		want     bool
	}{
		{StatusWaitingUser, StatusQueued, "model_budget_capacity", true},
		{StatusWaitingUser, StatusPaused, "model_budget_capacity", true},
		{StatusWaitingUser, StatusQueued, "approval_required", false},
		{StatusWaitingUser, StatusPaused, "clarification_required", false},
		{StatusWaitingUser, StatusQueued, "", false},
	} {
		if got := CanTransitionWithReason(tc.from, tc.to, tc.reason); got != tc.want {
			t.Errorf("CanTransitionWithReason(%q, %q, %q) = %t, want %t", tc.from, tc.to, tc.reason, got, tc.want)
		}
	}
}

func TestStepCountsExcludeBudgetSteps(t *testing.T) {
	total, completed := StepCounts([]Step{
		{Kind: "plan", Status: StepSucceeded},
		{Kind: "budget", Status: StepSucceeded},
		{Kind: "model", Status: StepRunning},
		{Kind: "tool", Status: StepFailed},
	})
	if total != 3 || completed != 1 {
		t.Fatalf("StepCounts = (%d, %d), want (3, 1)", total, completed)
	}
}

func TestUnknownOperationMustBeReconciled(t *testing.T) {
	if !CanTransitionOperation(OperationDispatched, OperationUnknown) ||
		!CanTransitionOperation(OperationUnknown, OperationSucceeded) ||
		CanTransitionOperation(OperationUnknown, OperationDispatched) {
		t.Fatal("unknown external operation may be replayed without reconciliation")
	}
}

func TestStructuredClarificationValidatesStableQuestionAndAnswerIDs(t *testing.T) {
	questions := []ClarificationQuestion{
		{ID: "environment", Prompt: "Where should this run?", Type: ClarificationSingleChoice, Required: true,
			Options: []ClarificationQuestionOption{{ID: "staging", Label: "Staging"}, {ID: "production", Label: "Production"}}},
		{ID: "channels", Prompt: "Which channels?", Type: ClarificationMultiChoice,
			Options: []ClarificationQuestionOption{{ID: "web", Label: "Web"}, {ID: "mobile", Label: "Mobile"}}},
		{ID: "notes", Prompt: "Additional notes", Type: ClarificationFreeText, AllowSkip: true, Options: []ClarificationQuestionOption{}},
	}
	if err := ValidateClarificationQuestions(questions); err != nil {
		t.Fatalf("valid question group rejected: %v", err)
	}
	answers := []ClarificationAnswer{
		{QuestionID: "environment", OptionIDs: []string{"staging"}},
		{QuestionID: "channels", OptionIDs: []string{"web", "mobile"}},
		{QuestionID: "notes", Skipped: true, OptionIDs: []string{}},
	}
	if err := ValidateClarificationAnswers(questions, answers); err != nil {
		t.Fatalf("valid answer group rejected: %v", err)
	}
	invalidAnswers := []ClarificationAnswer{answers[0], answers[1]}
	if err := ValidateClarificationAnswers(questions, invalidAnswers); err == nil {
		t.Fatal("missing explicit answer or skip was accepted")
	}
	unknownOption := []ClarificationAnswer{
		{QuestionID: "environment", OptionIDs: []string{"live"}}, answers[1], answers[2],
	}
	if err := ValidateClarificationAnswers(questions, unknownOption); err == nil {
		t.Fatal("unknown choice was accepted")
	}
	requiredSkip := []ClarificationAnswer{
		{QuestionID: "environment", Skipped: true, OptionIDs: []string{}}, answers[1], answers[2],
	}
	if err := ValidateClarificationAnswers(questions, requiredSkip); err == nil {
		t.Fatal("required clarification question was skipped")
	}
	implicitSkip := []ClarificationAnswer{answers[0], answers[1], {QuestionID: "notes"}}
	if err := ValidateClarificationAnswers(questions, implicitSkip); err == nil {
		t.Fatal("empty non-skip answer was accepted")
	}
}

func TestStructuredClarificationRejectsAmbiguousQuestionGroups(t *testing.T) {
	for _, questions := range [][]ClarificationQuestion{
		{},
		{{ID: "environment", Prompt: "Where?", Type: ClarificationSingleChoice,
			Options: []ClarificationQuestionOption{{ID: "same", Label: "A"}, {ID: "same", Label: "B"}}}},
		{{ID: "bad id", Prompt: "Where?", Type: ClarificationFreeText}},
		{{ID: "required", Prompt: "Required", Type: ClarificationFreeText, Required: true, AllowSkip: true}},
		{{ID: "notes", Prompt: "Notes", Type: ClarificationFreeText,
			Options: []ClarificationQuestionOption{{ID: "unused", Label: "Unused"}}}},
	} {
		if err := ValidateClarificationQuestions(questions); err == nil {
			t.Errorf("invalid question group accepted: %#v", questions)
		}
	}
}
