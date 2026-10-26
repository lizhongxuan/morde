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

func TestUnknownOperationMustBeReconciled(t *testing.T) {
	if !CanTransitionOperation(OperationDispatched, OperationUnknown) ||
		!CanTransitionOperation(OperationUnknown, OperationSucceeded) ||
		CanTransitionOperation(OperationUnknown, OperationDispatched) {
		t.Fatal("unknown external operation may be replayed without reconciliation")
	}
}
