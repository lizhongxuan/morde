package worker

import (
	"context"
	"testing"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

// pauseModelRecoveryToolsTest records which pause path
// pauseCloudDSHModelRecovery takes: the generic pause, the diagnostic
// pause, or the terminal ladder-exhaustion record.
type pauseModelRecoveryToolsTest struct {
	pauseRecoveryCalls   int
	pauseModelCalls      []string
	pauseDiagnosticCalls []app.AgentModelFailureDiagnostic
	exhaustionCalls      []app.FailoverReason
}

func (f *pauseModelRecoveryToolsTest) Reconcile(context.Context, task.Task, app.ModelToolCall, *string) (app.AgentToolResult, error) {
	return app.AgentToolResult{}, nil
}

func (f *pauseModelRecoveryToolsTest) PauseRecovery(context.Context, task.Task) error {
	f.pauseRecoveryCalls++
	return nil
}

func (f *pauseModelRecoveryToolsTest) PauseModelRecovery(_ context.Context, _ task.Task, category string) error {
	f.pauseModelCalls = append(f.pauseModelCalls, category)
	return nil
}

func (f *pauseModelRecoveryToolsTest) PauseModelRecoveryDiagnostic(_ context.Context, _ task.Task, diagnostic app.AgentModelFailureDiagnostic) error {
	f.pauseDiagnosticCalls = append(f.pauseDiagnosticCalls, diagnostic)
	return nil
}

func (f *pauseModelRecoveryToolsTest) PauseModelRecoveryAfterLadderExhaustion(_ context.Context, _ task.Task, reason app.FailoverReason) error {
	f.exhaustionCalls = append(f.exhaustionCalls, reason)
	return nil
}

// ladderDiagnosticCauseTest is a DSH model failure carrying a diagnostic
// with spent ladder attempts, as PauseModelRecoveryAfterLadderExhaustion
// records them.
type ladderDiagnosticCauseTest struct {
	diagnostic app.AgentModelFailureDiagnostic
}

func (e ladderDiagnosticCauseTest) Error() string { return "model result unavailable" }

func (e ladderDiagnosticCauseTest) ModelFailureDiagnostic() app.AgentModelFailureDiagnostic {
	return e.diagnostic
}

func TestPauseCloudDSHModelRecoveryLadderDecision(t *testing.T) {
	for _, test := range []struct {
		name           string
		cause          error
		wantExhaustion []app.FailoverReason
		wantDiagnostic int
		wantGeneric    int
	}{
		{
			name:           "terminal category exhausts at once",
			cause:          ladderDiagnosticCauseTest{diagnostic: app.AgentModelFailureDiagnostic{Category: "authentication_rejected"}},
			wantExhaustion: []app.FailoverReason{app.FailoverReasonAuthenticationRejected},
		},
		{
			name:           "fresh rate limit pauses instead of exhausting",
			cause:          ladderDiagnosticCauseTest{diagnostic: app.AgentModelFailureDiagnostic{Category: "rate_limited"}},
			wantDiagnostic: 1,
		},
		{
			name:           "rate limit with three attempts spent exhausts",
			cause:          ladderDiagnosticCauseTest{diagnostic: app.AgentModelFailureDiagnostic{Category: "rate_limited", LadderAttempts: 3}},
			wantExhaustion: []app.FailoverReason{app.FailoverReasonRateLimited},
		},
		{
			name:           "rate limit with two attempts spent still pauses",
			cause:          ladderDiagnosticCauseTest{diagnostic: app.AgentModelFailureDiagnostic{Category: "rate_limited", LadderAttempts: 2}},
			wantDiagnostic: 1,
		},
		{
			name:           "invalid response with two attempts spent exhausts",
			cause:          ladderDiagnosticCauseTest{diagnostic: app.AgentModelFailureDiagnostic{Category: "invalid_response", LadderAttempts: 2}},
			wantExhaustion: []app.FailoverReason{app.FailoverReasonInvalidResponse},
		},
		{
			name:           "context length never exhausts",
			cause:          ladderDiagnosticCauseTest{diagnostic: app.AgentModelFailureDiagnostic{Category: "context_length_exceeded", LadderAttempts: 3}},
			wantDiagnostic: 1,
		},
		{
			name:           "unmapped category falls back to classification and pauses",
			cause:          ladderDiagnosticCauseTest{diagnostic: app.AgentModelFailureDiagnostic{Category: "unauthorized_tool"}},
			wantDiagnostic: 1,
		},
		{
			name:        "plain rate limited error pauses generically",
			cause:       domain.NewError(domain.CodeRateLimited, "model concurrency limit reached"),
			wantGeneric: 1,
		},
		{
			name:           "plain authentication error exhausts",
			cause:          domain.NewError(domain.CodeUnauthenticated, "model authentication rejected"),
			wantExhaustion: []app.FailoverReason{app.FailoverReasonAuthenticationRejected},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			tools := &pauseModelRecoveryToolsTest{}
			if err := pauseCloudDSHModelRecovery(context.Background(), tools, task.Task{}, test.cause); err != nil {
				t.Fatalf("pauseCloudDSHModelRecovery() error=%v", err)
			}
			if len(tools.exhaustionCalls) != len(test.wantExhaustion) {
				t.Fatalf("exhaustion=%v, want %v", tools.exhaustionCalls, test.wantExhaustion)
			}
			for i, want := range test.wantExhaustion {
				if tools.exhaustionCalls[i] != want {
					t.Fatalf("exhaustion[%d]=%s, want %s", i, tools.exhaustionCalls[i], want)
				}
			}
			if len(tools.pauseDiagnosticCalls) != test.wantDiagnostic {
				t.Fatalf("diagnostic pauses=%d, want %d", len(tools.pauseDiagnosticCalls), test.wantDiagnostic)
			}
			if tools.pauseRecoveryCalls != test.wantGeneric {
				t.Fatalf("generic pauses=%d, want %d", tools.pauseRecoveryCalls, test.wantGeneric)
			}
		})
	}
}

func TestCloudDSHModelFailureCategoryReason(t *testing.T) {
	for _, test := range []struct {
		category string
		want     app.FailoverReason
		mapped   bool
	}{
		{category: "rate_limited", want: app.FailoverReasonRateLimited, mapped: true},
		{category: "provider_rate_limited", want: app.FailoverReasonRateLimited, mapped: true},
		{category: "timeout", want: app.FailoverReasonTimeout, mapped: true},
		{category: "context_length_exceeded", want: app.FailoverReasonContextLengthExceeded, mapped: true},
		{category: "task_budget_exceeded", want: app.FailoverReasonBudgetExceeded, mapped: true},
		{category: "unauthorized_tool", mapped: false},
		{category: "something_new", mapped: false},
		{category: "", mapped: false},
	} {
		t.Run(test.category, func(t *testing.T) {
			got, ok := cloudDSHModelFailureCategoryReason(test.category)
			if ok != test.mapped || got != test.want {
				t.Fatalf("reason=%q mapped=%v, want %q mapped=%v", got, ok, test.want, test.mapped)
			}
		})
	}
}
