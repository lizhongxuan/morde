package task

import "testing"

func TestPlanValidate(t *testing.T) {
	valid := Plan{TaskRevision: 2, Revision: 1, Goal: "Prepare a report", CostEstimateState: "unknown", Steps: []PlanStep{
		{ID: "collect", Title: "Collect inputs", Status: PlanStepCompleted, Outputs: []string{"source notes"}, CompletionCriteria: []string{"sources are recorded"}},
		{ID: "write", Title: "Write report", DependsOn: []string{"collect"}, Status: PlanStepPending, CompletionCriteria: []string{"report is reviewed"}},
	}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}

	for name, mutate := range map[string]func(*Plan){
		"cycle":                       func(value *Plan) { value.Steps[0].DependsOn = []string{"write"} },
		"missing dependency":          func(value *Plan) { value.Steps[1].DependsOn = []string{"missing"} },
		"duplicate ID":                func(value *Plan) { value.Steps[1].ID = value.Steps[0].ID },
		"known cost without evidence": func(value *Plan) { value.CostEstimateState = "estimated" },
	} {
		t.Run(name, func(t *testing.T) {
			value := valid
			value.Steps = append([]PlanStep(nil), valid.Steps...)
			mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatal("invalid plan was accepted")
			}
		})
	}
}
