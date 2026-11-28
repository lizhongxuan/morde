package app

import (
	"math/rand"
	"reflect"
	"testing"
)

// This fixed-seed differential check compares the production calculator with
// a deliberately small reference formula over a broad set of budget envelopes.
// It is an offline contract check; it does not enable or change live sends.
func TestBuildModelOutputBudgetPlanMatchesOfflineReference(t *testing.T) {
	rng := rand.New(rand.NewSource(20261127))
	for index := 0; index < 5000; index++ {
		goal := int64(1 + rng.Intn(20_000))
		minimum := int64(1 + rng.Intn(int(goal)))
		modelMax := int64(1 + rng.Intn(30_000))
		window := int64(4_096 + rng.Intn(100_000))
		inputBound := int64(rng.Intn(int(window + 5_000)))
		margin := int64(rng.Intn(10_000))
		protocolMax := int64(1 + rng.Intn(32_768))
		inputReserve := int64(rng.Intn(10_000))
		budgetRemaining := int64(rng.Intn(50_000))
		projectLimit := int64(rng.Intn(25_000))
		scheduleLimit := int64(rng.Intn(25_000))
		budgetApplicable := rng.Intn(2) == 1
		projectApplicable := rng.Intn(2) == 1
		scheduleApplicable := rng.Intn(2) == 1

		input := modelOutputBudgetInput{
			Stage: "tool_step", StrategyVersion: "offline-reference-v1", GoalTokens: goal, MinimumTokens: minimum,
			ModelMaxOutputTokens: &modelMax, ContextWindowTokens: &window, ContextInputUpperBound: &inputBound,
			ContextSafetyMargin: margin, TokenBudgetApplicable: budgetApplicable,
			TokenBudgetRemaining: &budgetRemaining, InputTokenReservation: &inputReserve, ProtocolMaxOutputTokens: protocolMax,
			AdditionalOutputTokenLimits: []modelOutputTokenLimit{
				{Source: "project_amount", Applicable: projectApplicable, OutputTokens: &projectLimit},
				{Source: "schedule_budget", Applicable: scheduleApplicable, OutputTokens: &scheduleLimit},
			},
		}
		plan, err := buildModelOutputBudgetPlan(input)
		if err != nil {
			t.Fatalf("case %d: production calculator rejected valid input: %v", index, err)
		}

		contextRemainder := window - inputBound - margin
		if contextRemainder < 0 {
			contextRemainder = 0
		}
		limits := []struct {
			name       string
			value      int64
			applicable bool
		}{
			{name: "model_max_output", value: modelMax, applicable: true},
			{name: "protocol_max_output", value: protocolMax, applicable: true},
			{name: "context_window", value: contextRemainder, applicable: true},
			{name: "token_budget", value: max(int64(0), budgetRemaining-inputReserve), applicable: budgetApplicable},
			{name: "project_amount", value: projectLimit, applicable: projectApplicable},
			{name: "schedule_budget", value: scheduleLimit, applicable: scheduleApplicable},
		}
		expectedSafe := int64(^uint64(0) >> 1)
		for _, limit := range limits {
			if limit.applicable && limit.value < expectedSafe {
				expectedSafe = limit.value
			}
		}
		expectedSend := min(goal, expectedSafe)
		expectedDispatchable := expectedSend >= minimum
		expectedLimits := make([]string, 0, len(limits)+1)
		if expectedSafe <= goal {
			for _, limit := range limits {
				if limit.applicable && limit.value == expectedSafe {
					expectedLimits = append(expectedLimits, limit.name)
				}
			}
		}
		if goal <= expectedSafe {
			expectedLimits = append(expectedLimits, "stage_goal")
		}
		expectedReason := ""
		if !expectedDispatchable {
			expectedReason = "below_stage_minimum"
		}
		if plan.SafeTokens != expectedSafe || plan.SendTokens != expectedSend || plan.Dispatchable != expectedDispatchable ||
			plan.BlockReason != expectedReason || !reflect.DeepEqual(plan.LimitingDimensions, expectedLimits) {
			t.Fatalf("case %d differs from offline reference: input=%+v plan=%+v expected safe/send/dispatch/reason/limits=%d/%d/%t/%q/%v",
				index, input, plan, expectedSafe, expectedSend, expectedDispatchable, expectedReason, expectedLimits)
		}
	}
}
