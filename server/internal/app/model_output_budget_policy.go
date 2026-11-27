package app

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// modelOutputBudgetStage identifies the part of an Agent execution for which
// a separately reviewed output target and minimum are configured.
type modelOutputBudgetStage string

const (
	modelOutputBudgetStageShortAnswer      modelOutputBudgetStage = "short_answer"
	modelOutputBudgetStageToolStep         modelOutputBudgetStage = "tool_step"
	modelOutputBudgetStageSubtask          modelOutputBudgetStage = "subtask"
	modelOutputBudgetStageSummary          modelOutputBudgetStage = "summary"
	modelOutputBudgetStageLongForm         modelOutputBudgetStage = "long_form"
	modelOutputBudgetStageTextContinuation modelOutputBudgetStage = "text_continuation"
)

var (
	modelOutputBudgetStrategyVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	modelOutputBudgetRequiredStages         = [...]modelOutputBudgetStage{
		modelOutputBudgetStageShortAnswer,
		modelOutputBudgetStageToolStep,
		modelOutputBudgetStageSubtask,
		modelOutputBudgetStageSummary,
		modelOutputBudgetStageLongForm,
		modelOutputBudgetStageTextContinuation,
	}
)

// ModelOutputBudgetStageDefinition is supplied by a reviewed per-model
// strategy table. Values are intentionally not defaulted here: a policy must
// choose each stage's target and minimum explicitly.
type ModelOutputBudgetStageDefinition struct {
	Stage         string
	GoalTokens    int64
	MinimumTokens int64
}

// ModelOutputBudgetModelDefinition binds stage goals to one exact HTTPS
// endpoint/model route. Other routes remain ineligible for adaptive admission.
type ModelOutputBudgetModelDefinition struct {
	BaseURL string
	Model   string
	Stages  []ModelOutputBudgetStageDefinition
}

// ModelOutputBudgetStrategyDefinition is the mutable input to the snapshot
// constructor. The constructor validates and copies it so later caller edits
// cannot change a strategy after it has been bound to a plan.
type ModelOutputBudgetStrategyDefinition struct {
	Version string
	Models  []ModelOutputBudgetModelDefinition
}

type modelOutputBudgetStagePolicy struct {
	GoalTokens    int64
	MinimumTokens int64
}

// modelOutputBudgetStrategySnapshot is immutable through its API. Its map and
// values are private, and no method returns an alias to internal storage.
type modelOutputBudgetStrategySnapshot struct {
	version string
	models  map[string]map[modelOutputBudgetStage]modelOutputBudgetStagePolicy
}

func newModelOutputBudgetStrategySnapshot(definition ModelOutputBudgetStrategyDefinition) (modelOutputBudgetStrategySnapshot, error) {
	if !modelOutputBudgetStrategyVersionPattern.MatchString(definition.Version) || strings.TrimSpace(definition.Version) != definition.Version {
		return modelOutputBudgetStrategySnapshot{}, errors.New("model output budget strategy version is invalid")
	}
	if len(definition.Models) == 0 {
		return modelOutputBudgetStrategySnapshot{}, errors.New("model output budget strategy must define at least one exact model route")
	}

	models := make(map[string]map[modelOutputBudgetStage]modelOutputBudgetStagePolicy, len(definition.Models))
	for _, model := range definition.Models {
		routeKey := modelMaxOutputCapacityRouteKey(model.BaseURL, model.Model)
		if routeKey == "" {
			return modelOutputBudgetStrategySnapshot{}, errors.New("model output budget strategy route must use an exact HTTPS endpoint and model")
		}
		if _, exists := models[routeKey]; exists {
			return modelOutputBudgetStrategySnapshot{}, fmt.Errorf("duplicate model output budget route %q", model.Model)
		}
		if len(model.Stages) != len(modelOutputBudgetRequiredStages) {
			return modelOutputBudgetStrategySnapshot{}, fmt.Errorf("model output budget route %q must define every supported stage exactly once", model.Model)
		}
		stages := make(map[modelOutputBudgetStage]modelOutputBudgetStagePolicy, len(model.Stages))
		for _, entry := range model.Stages {
			stage := modelOutputBudgetStage(entry.Stage)
			if !supportedModelOutputBudgetStage(stage) {
				return modelOutputBudgetStrategySnapshot{}, fmt.Errorf("unknown model output budget stage %q", entry.Stage)
			}
			if _, exists := stages[stage]; exists {
				return modelOutputBudgetStrategySnapshot{}, fmt.Errorf("duplicate model output budget stage %q for route %q", entry.Stage, model.Model)
			}
			if entry.GoalTokens < 1 || entry.GoalTokens > MaxModelOutputTokens ||
				entry.MinimumTokens < 1 || entry.MinimumTokens > entry.GoalTokens {
				return modelOutputBudgetStrategySnapshot{}, fmt.Errorf("model output budget stage %q has values outside the allowed range", entry.Stage)
			}
			stages[stage] = modelOutputBudgetStagePolicy{GoalTokens: entry.GoalTokens, MinimumTokens: entry.MinimumTokens}
		}
		for _, stage := range modelOutputBudgetRequiredStages {
			if _, exists := stages[stage]; !exists {
				return modelOutputBudgetStrategySnapshot{}, fmt.Errorf("model output budget route %q is missing stage %q", model.Model, stage)
			}
		}
		models[routeKey] = stages
	}
	return modelOutputBudgetStrategySnapshot{version: definition.Version, models: models}, nil
}

func supportedModelOutputBudgetStage(stage modelOutputBudgetStage) bool {
	for _, supported := range modelOutputBudgetRequiredStages {
		if stage == supported {
			return true
		}
	}
	return false
}

func (snapshot modelOutputBudgetStrategySnapshot) policyForModelStage(routeKey, stage string) (modelOutputBudgetStagePolicy, error) {
	if snapshot.version == "" || len(snapshot.models) == 0 {
		return modelOutputBudgetStagePolicy{}, errors.New("model output budget strategy snapshot is invalid")
	}
	stages, routeExists := snapshot.models[routeKey]
	if !routeExists {
		return modelOutputBudgetStagePolicy{}, errors.New("model output budget strategy is not configured for this route")
	}
	value, ok := stages[modelOutputBudgetStage(stage)]
	if !ok || !supportedModelOutputBudgetStage(modelOutputBudgetStage(stage)) {
		return modelOutputBudgetStagePolicy{}, fmt.Errorf("unknown model output budget stage %q", stage)
	}
	return value, nil
}

// inputForStage binds a stage policy and its version to the pure plan input.
// It preserves all capability and budget measurements already collected by the
// caller while making it impossible to mix this strategy's goal/minimum with a
// different version label.
func (snapshot modelOutputBudgetStrategySnapshot) inputForModelStage(input modelOutputBudgetInput, routeKey, stage string) (modelOutputBudgetInput, error) {
	policy, err := snapshot.policyForModelStage(routeKey, stage)
	if err != nil {
		return modelOutputBudgetInput{}, err
	}
	input.Stage = stage
	input.StrategyVersion = snapshot.version
	input.GoalTokens = policy.GoalTokens
	input.MinimumTokens = policy.MinimumTokens
	return input, nil
}
