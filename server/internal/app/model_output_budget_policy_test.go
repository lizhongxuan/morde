package app

import "testing"

const testModelBudgetBaseURL = "https://model.example/v1"
const testModelBudgetModel = "budget-test"

// Synthetic values are test data only; production routes and goals require
// their own reviewed strategy/version.
func syntheticModelOutputBudgetStrategy() ModelOutputBudgetStrategyDefinition {
	return ModelOutputBudgetStrategyDefinition{
		Version: "test-policy-v1",
		Models: []ModelOutputBudgetModelDefinition{{
			BaseURL: testModelBudgetBaseURL,
			Model:   testModelBudgetModel,
			Stages: []ModelOutputBudgetStageDefinition{
				{Stage: string(modelOutputBudgetStageRootTextOnly), GoalTokens: 4_096, MinimumTokens: 512},
				{Stage: string(modelOutputBudgetStageToolStep), GoalTokens: 640, MinimumTokens: 256},
				{Stage: string(modelOutputBudgetStageSubtask), GoalTokens: 1_024, MinimumTokens: 512},
				{Stage: string(modelOutputBudgetStageSummary), GoalTokens: 768, MinimumTokens: 256},
				{Stage: string(modelOutputBudgetStageLongForm), GoalTokens: 8_192, MinimumTokens: 2_048},
				{Stage: string(modelOutputBudgetStageTextContinuation), GoalTokens: 4_096, MinimumTokens: 512},
			},
		}},
	}
}

func testModelBudgetRouteKey() string {
	return modelMaxOutputCapacityRouteKey(testModelBudgetBaseURL, testModelBudgetModel)
}

func TestModelOutputBudgetPolicyBindsVersionRouteAndStageToPlanInput(t *testing.T) {
	definition := syntheticModelOutputBudgetStrategy()
	snapshot, err := newModelOutputBudgetStrategySnapshot(definition)
	if err != nil {
		t.Fatal(err)
	}

	definition.Version = "changed-after-snapshot"
	definition.Models[0].Stages[0].GoalTokens = 2_000_000

	stageValues := map[string][2]int64{
		string(modelOutputBudgetStageRootTextOnly):     {4_096, 512},
		string(modelOutputBudgetStageToolStep):         {640, 256},
		string(modelOutputBudgetStageSubtask):          {1_024, 512},
		string(modelOutputBudgetStageSummary):          {768, 256},
		string(modelOutputBudgetStageLongForm):         {8_192, 2_048},
		string(modelOutputBudgetStageTextContinuation): {4_096, 512},
	}
	for stage, want := range stageValues {
		t.Run(stage, func(t *testing.T) {
			input, err := snapshot.inputForModelStage(baseModelOutputBudgetInput(), testModelBudgetRouteKey(), stage)
			if err != nil {
				t.Fatal(err)
			}
			if input.Stage != stage || input.StrategyVersion != "test-policy-v1" ||
				input.GoalTokens != want[0] || input.MinimumTokens != want[1] {
				t.Fatalf("policy was not bound to input: %+v; want stage=%q goal=%d minimum=%d version=test-policy-v1",
					input, stage, want[0], want[1])
			}
			plan, err := buildModelOutputBudgetPlan(input)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Stage != stage || plan.StrategyVersion != "test-policy-v1" ||
				plan.GoalTokens != want[0] || plan.MinimumTokens != want[1] {
				t.Fatalf("plan lost policy binding: %+v", plan)
			}
		})
	}
}

func TestModelOutputBudgetPolicyRejectsInvalidDefinitions(t *testing.T) {
	tests := []struct {
		name string
		edit func(*ModelOutputBudgetStrategyDefinition)
	}{
		{name: "empty version", edit: func(definition *ModelOutputBudgetStrategyDefinition) { definition.Version = "" }},
		{name: "whitespace version", edit: func(definition *ModelOutputBudgetStrategyDefinition) { definition.Version = " test-policy-v1" }},
		{name: "missing model route", edit: func(definition *ModelOutputBudgetStrategyDefinition) { definition.Models = nil }},
		{name: "duplicate model route", edit: func(definition *ModelOutputBudgetStrategyDefinition) {
			definition.Models = append(definition.Models, definition.Models[0])
		}},
		{name: "non HTTPS model route", edit: func(definition *ModelOutputBudgetStrategyDefinition) {
			definition.Models[0].BaseURL = "http://model.example/v1"
		}},
		{name: "unknown stage", edit: func(definition *ModelOutputBudgetStrategyDefinition) {
			definition.Models[0].Stages[0].Stage = "unreviewed_stage"
		}},
		{name: "missing stage", edit: func(definition *ModelOutputBudgetStrategyDefinition) {
			stages := definition.Models[0].Stages
			definition.Models[0].Stages = stages[:len(stages)-1]
		}},
		{name: "duplicate stage", edit: func(definition *ModelOutputBudgetStrategyDefinition) {
			definition.Models[0].Stages[1].Stage = definition.Models[0].Stages[0].Stage
		}},
		{name: "missing goal", edit: func(definition *ModelOutputBudgetStrategyDefinition) {
			definition.Models[0].Stages[0].GoalTokens = 0
		}},
		{name: "missing minimum", edit: func(definition *ModelOutputBudgetStrategyDefinition) {
			definition.Models[0].Stages[0].MinimumTokens = 0
		}},
		{name: "minimum exceeds goal", edit: func(definition *ModelOutputBudgetStrategyDefinition) {
			stage := &definition.Models[0].Stages[0]
			stage.MinimumTokens = stage.GoalTokens + 1
		}},
		{name: "goal exceeds supported capacity range", edit: func(definition *ModelOutputBudgetStrategyDefinition) {
			definition.Models[0].Stages[0].GoalTokens = MaxModelOutputTokens + 1
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			definition := syntheticModelOutputBudgetStrategy()
			testCase.edit(&definition)
			if _, err := newModelOutputBudgetStrategySnapshot(definition); err == nil {
				t.Fatal("invalid strategy definition was accepted")
			}
		})
	}
}

func TestModelOutputBudgetPolicyRejectsUnknownRouteStageAndZeroSnapshot(t *testing.T) {
	snapshot, err := newModelOutputBudgetStrategySnapshot(syntheticModelOutputBudgetStrategy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.inputForModelStage(baseModelOutputBudgetInput(), testModelBudgetRouteKey(), "other_stage"); err == nil {
		t.Fatal("unknown stage was accepted")
	}
	if _, err := snapshot.inputForModelStage(baseModelOutputBudgetInput(), "https://another.example/v1\x00budget-test", string(modelOutputBudgetStageToolStep)); err == nil {
		t.Fatal("unconfigured model route was accepted")
	}
	if _, err := (modelOutputBudgetStrategySnapshot{}).inputForModelStage(baseModelOutputBudgetInput(), testModelBudgetRouteKey(), string(modelOutputBudgetStageToolStep)); err == nil {
		t.Fatal("zero-value snapshot was accepted")
	}
}
