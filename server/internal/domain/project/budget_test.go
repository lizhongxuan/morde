package project

import (
	"math"
	"testing"
)

func TestBudgetValidate(t *testing.T) {
	if err := (Budget{Currency: "USD", LimitMicros: 1, Version: 1}).Validate(); err != nil {
		t.Fatalf("valid budget: %v", err)
	}
	for _, value := range []Budget{
		{Currency: "usd", LimitMicros: 1, Version: 1},
		{Currency: "USD", LimitMicros: 0, Version: 1},
		{Currency: "USD", LimitMicros: 1, Version: 0},
	} {
		if err := value.Validate(); err == nil {
			t.Fatalf("invalid budget %#v was accepted", value)
		}
	}
}

func TestEstimateTokenCostRoundsUpWithoutOverflow(t *testing.T) {
	rate := RateSnapshot{
		Provider: "provider", ModelID: "p1234567", Model: "model", Currency: "USD", Version: 1,
		InputMicrosPerMillion: 3, OutputMicrosPerMillion: 5,
	}
	cost, err := rate.EstimateTokenCost(Usage{InputTokens: 1, OutputTokens: 1})
	if err != nil || cost != 2 {
		t.Fatalf("rounded token cost = %d, %v; want 2", cost, err)
	}
	cost, err = rate.EstimateTokenCost(Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if err != nil || cost != 8 {
		t.Fatalf("million-token cost = %d, %v; want 8", cost, err)
	}
	overflowRate := rate
	overflowRate.InputMicrosPerMillion = math.MaxInt64
	if _, err := overflowRate.EstimateTokenCost(Usage{InputTokens: math.MaxInt64}); err == nil {
		t.Fatal("overflowing token cost was accepted")
	}
	if _, err := rate.EstimateTokenCost(Usage{}); err == nil {
		t.Fatal("empty token usage was accepted")
	}
}
