package task

import (
	"errors"
	"strings"
	"unicode/utf8"
)

const (
	MaxPlanSteps     = 32
	MaxPlanListItems = 8
)

type PlanStepStatus string

const (
	PlanStepPending   PlanStepStatus = "pending"
	PlanStepRunning   PlanStepStatus = "in_progress"
	PlanStepCompleted PlanStepStatus = "completed"
	PlanStepBlocked   PlanStepStatus = "blocked"
)

// Plan is an advisory, revisioned execution outline. It never grants tool access.
type Plan struct {
	TaskRevision      int64      `json:"task_revision"`
	Revision          int64      `json:"revision"`
	Goal              string     `json:"goal"`
	CostEstimateState string     `json:"cost_estimate_state"`
	Steps             []PlanStep `json:"steps"`
}

type PlanStep struct {
	ID                 string         `json:"id"`
	Title              string         `json:"title"`
	Description        string         `json:"description"`
	DependsOn          []string       `json:"depends_on"`
	Outputs            []string       `json:"outputs"`
	CompletionCriteria []string       `json:"completion_criteria"`
	Status             PlanStepStatus `json:"status"`
}

func (p Plan) Validate() error {
	if p.TaskRevision < 0 || p.Revision < 1 || !planText(p.Goal, 1, 2048) || p.CostEstimateState != "unknown" || len(p.Steps) == 0 || len(p.Steps) > MaxPlanSteps {
		return errors.New("task plan fields are invalid")
	}
	steps := make(map[string]PlanStep, len(p.Steps))
	for _, step := range p.Steps {
		if !planID(step.ID) || !planText(step.Title, 1, 160) || !planText(step.Description, 0, 1024) ||
			len(step.DependsOn) > MaxPlanListItems || len(step.Outputs) > MaxPlanListItems || len(step.CompletionCriteria) > MaxPlanListItems ||
			(step.Status != PlanStepPending && step.Status != PlanStepRunning && step.Status != PlanStepCompleted && step.Status != PlanStepBlocked) {
			return errors.New("task plan step is invalid")
		}
		if _, exists := steps[step.ID]; exists {
			return errors.New("task plan step ID is duplicated")
		}
		if !validPlanTextList(step.Outputs, 256) || !validPlanTextList(step.CompletionCriteria, 512) {
			return errors.New("task plan step lists are invalid")
		}
		steps[step.ID] = step
	}
	for _, step := range p.Steps {
		seen := make(map[string]struct{}, len(step.DependsOn))
		for _, dependency := range step.DependsOn {
			if _, exists := steps[dependency]; !exists || dependency == step.ID {
				return errors.New("task plan dependency is invalid")
			}
			if _, exists := seen[dependency]; exists {
				return errors.New("task plan dependency is duplicated")
			}
			seen[dependency] = struct{}{}
		}
	}
	state := make(map[string]uint8, len(steps))
	var visit func(string) bool
	visit = func(id string) bool {
		if state[id] == 1 {
			return false
		}
		if state[id] == 2 {
			return true
		}
		state[id] = 1
		for _, dependency := range steps[id].DependsOn {
			if !visit(dependency) {
				return false
			}
		}
		state[id] = 2
		return true
	}
	for id := range steps {
		if !visit(id) {
			return errors.New("task plan dependencies contain a cycle")
		}
	}
	return nil
}

func planID(value string) bool {
	if len(value) == 0 || len(value) > 40 {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || (index > 0 && (char == '-' || char == '_')) {
			continue
		}
		return false
	}
	return true
}

func planText(value string, minimum, maximum int) bool {
	return len(value) >= minimum && len(value) <= maximum && utf8.ValidString(value) && !strings.ContainsRune(value, '\x00') &&
		(minimum == 0 || strings.TrimSpace(value) != "")
}

func validPlanTextList(values []string, maximum int) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !planText(value, 1, maximum) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}
