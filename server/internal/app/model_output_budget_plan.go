package app

import "errors"

// modelOutputBudgetInput is a pure, shadow-only snapshot of one prospective
// model call. Currency-based limits must be converted using a pinned rate
// outside this function; this function never mixes tokens and money.
type modelOutputBudgetInput struct {
	Stage                       string
	StrategyVersion             string
	GoalTokens                  int64
	MinimumTokens               int64
	ModelMaxOutputTokens        *int64
	ContextWindowTokens         *int64
	ContextInputUpperBound      *int64
	ContextSafetyMargin         int64
	TokenBudgetApplicable       bool
	TokenBudgetRemaining        *int64
	InputTokenReservation       *int64
	ProtocolMaxOutputTokens     int64
	AdditionalOutputTokenLimits []modelOutputTokenLimit
}

type modelOutputTokenLimit struct {
	Source       string
	Applicable   bool
	OutputTokens *int64
}

type modelOutputBudgetPlan struct {
	Stage              string
	StrategyVersion    string
	SafeTokens         int64
	GoalTokens         int64
	MinimumTokens      int64
	SendTokens         int64
	Dispatchable       bool
	BlockReason        string
	LimitingDimensions []string
}

// buildModelOutputBudgetPlan computes O_safe and O_send without changing any
// live request. Unknown applicable limits fail closed so callers can record a
// shadow reason instead of treating missing capability or budget data as free
// capacity.
func buildModelOutputBudgetPlan(input modelOutputBudgetInput) (modelOutputBudgetPlan, error) {
	plan := modelOutputBudgetPlan{
		Stage:           input.Stage,
		StrategyVersion: input.StrategyVersion,
		GoalTokens:      input.GoalTokens,
		MinimumTokens:   input.MinimumTokens,
	}
	if input.Stage == "" || input.StrategyVersion == "" || input.GoalTokens <= 0 || input.MinimumTokens <= 0 ||
		input.MinimumTokens > input.GoalTokens || input.ContextSafetyMargin < 0 || input.ProtocolMaxOutputTokens <= 0 {
		return plan, errors.New("invalid model output budget strategy")
	}

	if input.ModelMaxOutputTokens == nil || input.ContextWindowTokens == nil {
		plan.BlockReason = "model_capability_unknown"
		return plan, nil
	}
	if *input.ModelMaxOutputTokens <= 0 || *input.ContextWindowTokens <= 0 {
		return plan, errors.New("model output capability must be positive")
	}
	if input.ContextInputUpperBound == nil {
		plan.BlockReason = "context_input_bound_unknown"
		return plan, nil
	}
	if *input.ContextInputUpperBound < 0 {
		return plan, errors.New("context input bound must be non-negative")
	}

	limits := []modelOutputTokenLimit{
		{Source: "model_max_output", Applicable: true, OutputTokens: input.ModelMaxOutputTokens},
		{Source: "protocol_max_output", Applicable: true, OutputTokens: &input.ProtocolMaxOutputTokens},
	}
	if *input.ContextInputUpperBound >= *input.ContextWindowTokens {
		zero := int64(0)
		limits = append(limits, modelOutputTokenLimit{Source: "context_window", Applicable: true, OutputTokens: &zero})
	} else {
		available := *input.ContextWindowTokens - *input.ContextInputUpperBound
		if input.ContextSafetyMargin >= available {
			available = 0
		} else {
			available -= input.ContextSafetyMargin
		}
		limits = append(limits, modelOutputTokenLimit{Source: "context_window", Applicable: true, OutputTokens: &available})
	}
	if input.TokenBudgetApplicable {
		if input.TokenBudgetRemaining == nil || input.InputTokenReservation == nil {
			plan.BlockReason = "token_budget_unknown"
			return plan, nil
		}
		if *input.TokenBudgetRemaining < 0 || *input.InputTokenReservation < 0 {
			return plan, errors.New("token budget values must be non-negative")
		}
		available := int64(0)
		if *input.TokenBudgetRemaining > *input.InputTokenReservation {
			available = *input.TokenBudgetRemaining - *input.InputTokenReservation
		}
		limits = append(limits, modelOutputTokenLimit{Source: "token_budget", Applicable: true, OutputTokens: &available})
	}
	for _, limit := range input.AdditionalOutputTokenLimits {
		if !limit.Applicable {
			continue
		}
		if limit.Source == "" {
			return plan, errors.New("applicable output limit requires a source")
		}
		if limit.OutputTokens == nil {
			plan.BlockReason = "budget_limit_unknown"
			return plan, nil
		}
		if *limit.OutputTokens < 0 {
			return plan, errors.New("output token limits must be non-negative")
		}
		limits = append(limits, limit)
	}

	safe := limits[0].OutputTokens
	for _, limit := range limits[1:] {
		if *limit.OutputTokens < *safe {
			safe = limit.OutputTokens
		}
	}
	plan.SafeTokens = *safe
	plan.SendTokens = min(input.GoalTokens, plan.SafeTokens)
	if plan.SafeTokens <= input.GoalTokens {
		for _, limit := range limits {
			if *limit.OutputTokens == plan.SafeTokens {
				plan.LimitingDimensions = append(plan.LimitingDimensions, limit.Source)
			}
		}
	}
	if input.GoalTokens <= plan.SafeTokens {
		plan.LimitingDimensions = append(plan.LimitingDimensions, "stage_goal")
	}
	if plan.SendTokens < input.MinimumTokens {
		plan.BlockReason = "below_stage_minimum"
		return plan, nil
	}
	plan.Dispatchable = true
	return plan, nil
}
