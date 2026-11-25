package task

import (
	"strings"
	"testing"
)

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

func TestPlanVerificationCheckValidation(t *testing.T) {
	workspaceRevision := int64(7)
	validRequired := &PlanVerificationCheck{
		Kind: PlanVerificationCheckTest, Basis: PlanVerificationCheckRequired,
		Command: "go test ./server/internal/app -run TestFix", CoveragePaths: []string{"server/internal/app"},
		RequirementSource: &PlanVerificationRequirementSource{
			Kind: PlanVerificationRequirementUserMessage, MessageID: "m1234567",
			ContentSHA256: strings.Repeat("a", 64),
		},
	}
	validProjectRequired := &PlanVerificationCheck{
		Kind: PlanVerificationCheckLint, Basis: PlanVerificationCheckRequired,
		Command: "golangci-lint run ./server/internal/app", CoveragePaths: []string{"server/internal/app"},
		RequirementSource: &PlanVerificationRequirementSource{
			Kind: PlanVerificationRequirementProjectFile, WorkspacePath: "AGENTS.md", WorkspaceRevision: &workspaceRevision,
			ContentSHA256: strings.Repeat("b", 64),
		},
	}
	validRiskSelected := &PlanVerificationCheck{
		Kind: PlanVerificationCheckTest, Basis: PlanVerificationCheckRiskSelected,
		Command: "go test ./server/internal/app", CoveragePaths: []string{"server/internal/app"},
	}
	for name, check := range map[string]*PlanVerificationCheck{
		"required user message": validRequired,
		"required project file": validProjectRequired,
		"risk selected":         validRiskSelected,
	} {
		t.Run(name, func(t *testing.T) {
			plan := Plan{TaskRevision: 1, Revision: 1, Goal: "Fix behavior", CostEstimateState: "unknown", Steps: []PlanStep{
				{ID: "verify", Title: "Verify change", Status: PlanStepPending, VerificationCheck: check},
			}}
			if err := plan.Validate(); err != nil {
				t.Fatalf("valid verification check rejected: %v", err)
			}
		})
	}

	for name, mutate := range map[string]func(*PlanVerificationCheck){
		"unknown kind":                func(check *PlanVerificationCheck) { check.Kind = "benchmark" },
		"unknown basis":               func(check *PlanVerificationCheck) { check.Basis = "optional" },
		"empty command":               func(check *PlanVerificationCheck) { check.Command = "  " },
		"command too long":            func(check *PlanVerificationCheck) { check.Command = strings.Repeat("x", 1025) },
		"missing coverage":            func(check *PlanVerificationCheck) { check.CoveragePaths = nil },
		"duplicate coverage":          func(check *PlanVerificationCheck) { check.CoveragePaths = []string{"server", "server"} },
		"absolute coverage":           func(check *PlanVerificationCheck) { check.CoveragePaths = []string{"/server"} },
		"escaping coverage":           func(check *PlanVerificationCheck) { check.CoveragePaths = []string{"../server"} },
		"glob coverage":               func(check *PlanVerificationCheck) { check.CoveragePaths = []string{"server/**"} },
		"required missing source":     func(check *PlanVerificationCheck) { check.RequirementSource = nil },
		"risk selected with source":   func(check *PlanVerificationCheck) { check.Basis = PlanVerificationCheckRiskSelected },
		"user source with path":       func(check *PlanVerificationCheck) { check.RequirementSource.WorkspacePath = "AGENTS.md" },
		"user source with bad digest": func(check *PlanVerificationCheck) { check.RequirementSource.ContentSHA256 = strings.Repeat("A", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			check := *validRequired
			check.CoveragePaths = append([]string(nil), validRequired.CoveragePaths...)
			if validRequired.RequirementSource != nil {
				source := *validRequired.RequirementSource
				check.RequirementSource = &source
			}
			mutate(&check)
			plan := Plan{TaskRevision: 1, Revision: 1, Goal: "Fix behavior", CostEstimateState: "unknown", Steps: []PlanStep{
				{ID: "verify", Title: "Verify change", Status: PlanStepPending, VerificationCheck: &check},
			}}
			if err := plan.Validate(); err == nil {
				t.Fatal("invalid verification check was accepted")
			}
		})
	}

	t.Run("project source requires revision and normalized path", func(t *testing.T) {
		for name, mutate := range map[string]func(*PlanVerificationRequirementSource){
			"missing revision": func(source *PlanVerificationRequirementSource) { source.WorkspaceRevision = nil },
			"negative revision": func(source *PlanVerificationRequirementSource) {
				revision := int64(-1)
				source.WorkspaceRevision = &revision
			},
			"traversal path": func(source *PlanVerificationRequirementSource) { source.WorkspacePath = "../AGENTS.md" },
			"missing digest": func(source *PlanVerificationRequirementSource) { source.ContentSHA256 = "" },
		} {
			t.Run(name, func(t *testing.T) {
				check := *validProjectRequired
				source := *validProjectRequired.RequirementSource
				check.RequirementSource = &source
				mutate(check.RequirementSource)
				plan := Plan{TaskRevision: 1, Revision: 1, Goal: "Fix behavior", CostEstimateState: "unknown", Steps: []PlanStep{
					{ID: "verify", Title: "Verify change", Status: PlanStepPending, VerificationCheck: &check},
				}}
				if err := plan.Validate(); err == nil {
					t.Fatal("invalid project requirement source was accepted")
				}
			})
		}
	})
}
