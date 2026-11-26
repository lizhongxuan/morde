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

func TestPlanVerificationApplicabilityKeepsLegacyPlansReadable(t *testing.T) {
	check := &PlanVerificationCheck{Kind: PlanVerificationCheckTest, Basis: PlanVerificationCheckRiskSelected,
		Command: "go test ./server/internal/app", CoveragePaths: []string{"server/internal/app"}}
	fix := Plan{TaskRevision: 1, Revision: 1, Goal: "Fix behavior", CostEstimateState: "unknown",
		FixPlan: &FixPlan{}, Steps: []PlanStep{
			{ID: "implement", Title: "Implement fix", Status: PlanStepPending,
				VerificationApplicability: PlanVerificationApplicabilityNotApplicable},
			{ID: "verify", Title: "Run check", Status: PlanStepPending,
				VerificationApplicability: PlanVerificationApplicabilityCheck, VerificationCheck: check},
		}}
	if err := fix.ValidateSubmission(); err != nil {
		t.Fatalf("explicitly classified fix plan rejected: %v", err)
	}

	legacy := fix
	legacy.Steps = append([]PlanStep(nil), fix.Steps...)
	legacy.Steps[0].VerificationApplicability = ""
	legacy.Steps[1].VerificationApplicability = ""
	if err := legacy.Validate(); err != nil {
		t.Fatalf("legacy fix plan was not readable: %v", err)
	}
	if err := legacy.ValidateSubmission(); err == nil {
		t.Fatal("new fix plan submission accepted steps with unknown verification applicability")
	}
}

func TestPlanVerificationApplicabilityMatchesCheckDeclaration(t *testing.T) {
	check := &PlanVerificationCheck{Kind: PlanVerificationCheckTest, Basis: PlanVerificationCheckRiskSelected,
		Command: "go test ./server/internal/app", CoveragePaths: []string{"server/internal/app"}}
	for name, step := range map[string]PlanStep{
		"check requires typed check": {ID: "verify", Title: "Run check", Status: PlanStepPending,
			VerificationApplicability: PlanVerificationApplicabilityCheck},
		"not applicable cannot carry check": {ID: "verify", Title: "Run check", Status: PlanStepPending,
			VerificationApplicability: PlanVerificationApplicabilityNotApplicable, VerificationCheck: check},
		"unknown applicability is invalid": {ID: "verify", Title: "Run check", Status: PlanStepPending,
			VerificationApplicability: "maybe"},
	} {
		t.Run(name, func(t *testing.T) {
			plan := Plan{TaskRevision: 1, Revision: 1, Goal: "Fix behavior", CostEstimateState: "unknown",
				Steps: []PlanStep{step}}
			if err := plan.Validate(); err == nil {
				t.Fatal("inconsistent verification applicability was accepted")
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

func TestPlanSubmissionInputAllowsUnhashedRequirementLocators(t *testing.T) {
	workspaceRevision := int64(9)
	for name, source := range map[string]PlanVerificationRequirementSource{
		"current task input":      {Kind: PlanVerificationRequirementUserMessage, Locator: PlanVerificationRequirementCurrentTaskInputLocator},
		"current task turn input": {Kind: PlanVerificationRequirementUserMessage, Locator: PlanVerificationRequirementCurrentTaskTurnInputLocator},
		"history search result": {Kind: PlanVerificationRequirementUserMessage, Locator: PlanVerificationRequirementHistorySearchResultLocator,
			HistoryResultToken: "signed.history.result.token"},
		"project snapshot locator": {Kind: PlanVerificationRequirementProjectFile, WorkspacePath: "AGENTS.md", WorkspaceRevision: &workspaceRevision},
	} {
		t.Run(name, func(t *testing.T) {
			check := &PlanVerificationCheck{Kind: PlanVerificationCheckTest, Basis: PlanVerificationCheckRequired,
				Command: "go test ./...", CoveragePaths: []string{"server"}, RequirementSource: &source}
			plan := Plan{TaskRevision: 1, Revision: 1, Goal: "Fix behavior", CostEstimateState: "unknown",
				FixPlan: &FixPlan{}, Steps: []PlanStep{{ID: "verify", Title: "Verify behavior", Status: PlanStepPending,
					VerificationApplicability: PlanVerificationApplicabilityCheck, VerificationCheck: check}}}
			if err := plan.ValidateSubmissionInput(); err != nil {
				t.Fatalf("valid locator form rejected: %v", err)
			}
			if err := plan.ValidateSubmission(); err == nil {
				t.Fatal("unresolved source locator was accepted as a persisted submission")
			}
			if source.Locator == PlanVerificationRequirementHistorySearchResultLocator && source.HistoryResultToken != "signed.history.result.token" {
				t.Fatal("input validation mutated the caller's history source token")
			}
		})
	}

	for name, mutate := range map[string]func(*PlanVerificationRequirementSource){
		"current locator with message ID": func(source *PlanVerificationRequirementSource) { source.MessageID = "m1234567" },
		"unknown locator":                 func(source *PlanVerificationRequirementSource) { source.Locator = "latest_message" },
		"raw user message ID": func(source *PlanVerificationRequirementSource) {
			source.Locator, source.MessageID = "", "m1234567"
		},
		"history locator without token": func(source *PlanVerificationRequirementSource) {
			source.Locator = PlanVerificationRequirementHistorySearchResultLocator
		},
		"history locator with raw message ID": func(source *PlanVerificationRequirementSource) {
			source.Locator, source.HistoryResultToken, source.MessageID =
				PlanVerificationRequirementHistorySearchResultLocator, "signed.history.result.token", "m1234567"
		},
		"history token too long": func(source *PlanVerificationRequirementSource) {
			source.Locator, source.HistoryResultToken = PlanVerificationRequirementHistorySearchResultLocator,
				strings.Repeat("x", PlanVerificationRequirementHistoryResultTokenMaxBytes+1)
		},
		"turn locator with message ID": func(source *PlanVerificationRequirementSource) {
			source.Locator, source.MessageID = PlanVerificationRequirementCurrentTaskTurnInputLocator, "m1234567"
		},
		"turn locator with Workspace path": func(source *PlanVerificationRequirementSource) {
			source.Locator, source.WorkspacePath = PlanVerificationRequirementCurrentTaskTurnInputLocator, "AGENTS.md"
		},
		"turn locator with Workspace revision": func(source *PlanVerificationRequirementSource) {
			revision := int64(9)
			source.Locator, source.WorkspaceRevision = PlanVerificationRequirementCurrentTaskTurnInputLocator, &revision
		},
		"turn locator with digest": func(source *PlanVerificationRequirementSource) {
			source.Locator, source.ContentSHA256 = PlanVerificationRequirementCurrentTaskTurnInputLocator, strings.Repeat("a", 64)
		},
		"user message with Workspace path": func(source *PlanVerificationRequirementSource) { source.WorkspacePath = "AGENTS.md" },
		"malformed optional digest":        func(source *PlanVerificationRequirementSource) { source.ContentSHA256 = "not-a-digest" },
	} {
		t.Run("reject "+name, func(t *testing.T) {
			source := PlanVerificationRequirementSource{Kind: PlanVerificationRequirementUserMessage,
				Locator: PlanVerificationRequirementCurrentTaskInputLocator}
			mutate(&source)
			plan := Plan{TaskRevision: 1, Revision: 1, Goal: "Fix behavior", CostEstimateState: "unknown",
				Steps: []PlanStep{{ID: "verify", Title: "Verify behavior", Status: PlanStepPending,
					VerificationCheck: &PlanVerificationCheck{Kind: PlanVerificationCheckTest, Basis: PlanVerificationCheckRequired,
						Command: "go test ./...", CoveragePaths: []string{"server"}, RequirementSource: &source}}}}
			if err := plan.ValidateSubmissionInput(); err == nil {
				t.Fatal("invalid locator form was accepted")
			}
		})
	}

	t.Run("canonical server-resolved source validates", func(t *testing.T) {
		plan := Plan{TaskRevision: 1, Revision: 1, Goal: "Fix behavior", CostEstimateState: "unknown", FixPlan: &FixPlan{},
			Steps: []PlanStep{{ID: "verify", Title: "Verify behavior", Status: PlanStepPending,
				VerificationApplicability: PlanVerificationApplicabilityCheck, VerificationCheck: &PlanVerificationCheck{
					Kind: PlanVerificationCheckTest, Basis: PlanVerificationCheckRequired, Command: "go test ./...", CoveragePaths: []string{"server"},
					RequirementSource: &PlanVerificationRequirementSource{Kind: PlanVerificationRequirementUserMessage,
						MessageID: "m1234567", ContentSHA256: strings.Repeat("a", 64)},
				}}}}
		if err := plan.ValidateSubmission(); err != nil {
			t.Fatalf("canonical server-resolved plan rejected: %v", err)
		}
	})
}
