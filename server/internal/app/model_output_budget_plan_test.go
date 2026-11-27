package app

import (
	"math"
	"reflect"
	"testing"
)

func modelBudgetInt64(value int64) *int64 { return &value }

func baseModelOutputBudgetInput() modelOutputBudgetInput {
	return modelOutputBudgetInput{
		Stage:                   "tool_step",
		StrategyVersion:         "shadow-v1",
		GoalTokens:              12_000,
		MinimumTokens:           512,
		ModelMaxOutputTokens:    modelBudgetInt64(16_000),
		ContextWindowTokens:     modelBudgetInt64(64_000),
		ContextInputUpperBound:  modelBudgetInt64(20_000),
		ContextSafetyMargin:     2_000,
		ProtocolMaxOutputTokens: 32_768,
	}
}

func TestBuildModelOutputBudgetPlanUsesTheSmallestApplicableLimit(t *testing.T) {
	input := baseModelOutputBudgetInput()
	input.TokenBudgetApplicable = true
	input.TokenBudgetRemaining = modelBudgetInt64(7_000)
	input.InputTokenReservation = modelBudgetInt64(2_000)
	input.AdditionalOutputTokenLimits = []modelOutputTokenLimit{
		{Source: "project_amount", Applicable: true, OutputTokens: modelBudgetInt64(6_000)},
		{Source: "schedule_budget", Applicable: false},
	}

	plan, err := buildModelOutputBudgetPlan(input)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Dispatchable || plan.BlockReason != "" || plan.SafeTokens != 5_000 || plan.SendTokens != 5_000 {
		t.Fatalf("unexpected budget plan: %+v", plan)
	}
	if !reflect.DeepEqual(plan.LimitingDimensions, []string{"token_budget"}) {
		t.Fatalf("unexpected limiting dimensions: %v", plan.LimitingDimensions)
	}
}

func TestBuildModelOutputBudgetPlanReportsTheBindingLimit(t *testing.T) {
	tests := []struct {
		name      string
		edit      func(*modelOutputBudgetInput)
		wantSafe  int64
		wantSend  int64
		wantLimit []string
	}{
		{
			name:     "model output capability",
			edit:     func(input *modelOutputBudgetInput) { input.ModelMaxOutputTokens = modelBudgetInt64(4_096) },
			wantSafe: 4_096, wantSend: 4_096, wantLimit: []string{"model_max_output"},
		},
		{
			name: "context remainder",
			edit: func(input *modelOutputBudgetInput) {
				input.ContextWindowTokens = modelBudgetInt64(22_000)
				input.ContextInputUpperBound = modelBudgetInt64(20_000)
				input.ContextSafetyMargin = 1_000
			},
			wantSafe: 1_000, wantSend: 1_000, wantLimit: []string{"context_window"},
		},
		{
			name: "protocol ceiling",
			edit: func(input *modelOutputBudgetInput) {
				input.GoalTokens = 40_000
				input.MinimumTokens = 512
				input.ProtocolMaxOutputTokens = 4_096
			},
			wantSafe: 4_096, wantSend: 4_096, wantLimit: []string{"protocol_max_output"},
		},
		{
			name: "stage goal",
			edit: func(input *modelOutputBudgetInput) {
				input.GoalTokens = 2_048
				input.MinimumTokens = 512
			},
			wantSafe: 16_000, wantSend: 2_048, wantLimit: []string{"stage_goal"},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			input := baseModelOutputBudgetInput()
			testCase.edit(&input)
			plan, err := buildModelOutputBudgetPlan(input)
			if err != nil {
				t.Fatal(err)
			}
			if !plan.Dispatchable || plan.SafeTokens != testCase.wantSafe || plan.SendTokens != testCase.wantSend {
				t.Fatalf("unexpected budget plan: %+v", plan)
			}
			if !reflect.DeepEqual(plan.LimitingDimensions, testCase.wantLimit) {
				t.Fatalf("limiting dimensions = %v, want %v", plan.LimitingDimensions, testCase.wantLimit)
			}
		})
	}
}

func TestBuildModelOutputBudgetPlanFailsClosedOnUnknownApplicableData(t *testing.T) {
	tests := []struct {
		name       string
		edit       func(*modelOutputBudgetInput)
		wantReason string
	}{
		{
			name:       "model capability",
			edit:       func(input *modelOutputBudgetInput) { input.ModelMaxOutputTokens = nil },
			wantReason: "model_capability_unknown",
		},
		{
			name:       "context window",
			edit:       func(input *modelOutputBudgetInput) { input.ContextWindowTokens = nil },
			wantReason: "model_capability_unknown",
		},
		{
			name:       "input context bound",
			edit:       func(input *modelOutputBudgetInput) { input.ContextInputUpperBound = nil },
			wantReason: "context_input_bound_unknown",
		},
		{
			name:       "token budget",
			edit:       func(input *modelOutputBudgetInput) { input.TokenBudgetApplicable = true },
			wantReason: "token_budget_unknown",
		},
		{
			name: "applicable amount bound",
			edit: func(input *modelOutputBudgetInput) {
				input.AdditionalOutputTokenLimits = []modelOutputTokenLimit{{Source: "project_amount", Applicable: true}}
			},
			wantReason: "budget_limit_unknown",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			input := baseModelOutputBudgetInput()
			testCase.edit(&input)
			plan, err := buildModelOutputBudgetPlan(input)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Dispatchable || plan.BlockReason != testCase.wantReason || plan.SendTokens != 0 {
				t.Fatalf("expected fail-closed reason %q, got %+v", testCase.wantReason, plan)
			}
		})
	}
}

func TestBuildModelOutputBudgetPlanPausesBelowMinimumAndHandlesIntegerBoundaries(t *testing.T) {
	t.Run("below minimum", func(t *testing.T) {
		input := baseModelOutputBudgetInput()
		input.MinimumTokens = 512
		input.ContextWindowTokens = modelBudgetInt64(21_000)
		input.ContextInputUpperBound = modelBudgetInt64(20_000)
		input.ContextSafetyMargin = 900
		plan, err := buildModelOutputBudgetPlan(input)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Dispatchable || plan.BlockReason != "below_stage_minimum" || plan.SafeTokens != 100 || plan.SendTokens != 100 {
			t.Fatalf("unexpected below-minimum plan: %+v", plan)
		}
	})

	t.Run("zero remaining tokens", func(t *testing.T) {
		input := baseModelOutputBudgetInput()
		input.TokenBudgetApplicable = true
		input.TokenBudgetRemaining = modelBudgetInt64(100)
		input.InputTokenReservation = modelBudgetInt64(100)
		plan, err := buildModelOutputBudgetPlan(input)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Dispatchable || plan.SafeTokens != 0 || plan.SendTokens != 0 || plan.BlockReason != "below_stage_minimum" {
			t.Fatalf("unexpected exhausted-budget plan: %+v", plan)
		}
	})

	t.Run("maximum integer values do not overflow", func(t *testing.T) {
		input := modelOutputBudgetInput{
			Stage: "long_reply", StrategyVersion: "shadow-v1", GoalTokens: math.MaxInt64,
			MinimumTokens: 1, ModelMaxOutputTokens: modelBudgetInt64(math.MaxInt64),
			ContextWindowTokens: modelBudgetInt64(math.MaxInt64), ContextInputUpperBound: modelBudgetInt64(0),
			ProtocolMaxOutputTokens: math.MaxInt64,
		}
		plan, err := buildModelOutputBudgetPlan(input)
		if err != nil {
			t.Fatal(err)
		}
		if !plan.Dispatchable || plan.SafeTokens != math.MaxInt64 || plan.SendTokens != math.MaxInt64 {
			t.Fatalf("unexpected integer-boundary plan: %+v", plan)
		}
	})

	t.Run("negative budget values are rejected", func(t *testing.T) {
		input := baseModelOutputBudgetInput()
		input.TokenBudgetApplicable = true
		input.TokenBudgetRemaining = modelBudgetInt64(1)
		input.InputTokenReservation = modelBudgetInt64(-1)
		if _, err := buildModelOutputBudgetPlan(input); err == nil {
			t.Fatal("negative input reservation should be rejected")
		}
	})
}
