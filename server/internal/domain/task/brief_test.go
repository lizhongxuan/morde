package task

import (
	"strings"
	"testing"
)

func validBrief() Brief {
	return Brief{Goal: "Fix the login crash", Scope: "auth package only",
		AcceptanceCriteria: []string{"login succeeds after restart"}, VerificationMethod: "run the failing test, then the auth suite"}
}

func TestBriefValidate(t *testing.T) {
	if err := validBrief().Validate(); err != nil {
		t.Fatalf("valid brief rejected: %v", err)
	}
	withFixPlan := validBrief()
	withFixPlan.FixPlan = &FixPlan{ChangedFiles: []string{"auth/session.go"}, Risks: []string{"session expiry shifts by a day"}, Alternatives: []string{"restart the service daily"}}
	if err := withFixPlan.Validate(); err != nil {
		t.Fatalf("valid brief with fix plan rejected: %v", err)
	}

	long := strings.Repeat("x", 2049)
	cases := map[string]func(*Brief){
		"empty goal":             func(value *Brief) { value.Goal = "" },
		"goal too long":          func(value *Brief) { value.Goal = long },
		"goal with NUL":          func(value *Brief) { value.Goal = "fix\x00it" },
		"goal invalid utf-8":     func(value *Brief) { value.Goal = "fix\xffit" },
		"empty scope":            func(value *Brief) { value.Scope = "" },
		"scope too long":         func(value *Brief) { value.Scope = long },
		"no acceptance criteria": func(value *Brief) { value.AcceptanceCriteria = nil },
		"too many acceptance criteria": func(value *Brief) {
			value.AcceptanceCriteria = []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
		},
		"acceptance criterion too long": func(value *Brief) { value.AcceptanceCriteria = []string{strings.Repeat("x", 513)} },
		"acceptance criterion empty":    func(value *Brief) { value.AcceptanceCriteria = []string{""} },
		"acceptance criteria duplicated": func(value *Brief) {
			value.AcceptanceCriteria = []string{"same", "same"}
		},
		"empty verification method":    func(value *Brief) { value.VerificationMethod = "" },
		"verification method too long": func(value *Brief) { value.VerificationMethod = strings.Repeat("x", 1025) },
		"fix plan too many files": func(value *Brief) {
			value.FixPlan = &FixPlan{ChangedFiles: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}}
		},
		"fix plan empty risk": func(value *Brief) { value.FixPlan = &FixPlan{Risks: []string{""}} },
		"fix plan alternative too long": func(value *Brief) {
			value.FixPlan = &FixPlan{Alternatives: []string{strings.Repeat("x", 513)}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			value := validBrief()
			mutate(&value)
			if err := value.Validate(); err == nil {
				t.Fatal("invalid brief was accepted")
			}
		})
	}
}

func TestBriefFromPlan(t *testing.T) {
	plan := Plan{TaskRevision: 2, Revision: 1, Goal: "Fix the login crash", Scope: "auth package only",
		VerificationMethod: "run the failing test, then the auth suite", CostEstimateState: "unknown", Steps: []PlanStep{
			{ID: "repro", Title: "Reproduce", Status: PlanStepPending,
				CompletionCriteria: []string{"crash reproduces", "login succeeds after restart"}},
			{ID: "fix", Title: "Fix", DependsOn: []string{"repro"}, Status: PlanStepPending,
				CompletionCriteria: []string{"login succeeds after restart", "no regression in auth suite"}},
		}}
	brief, err := BriefFromPlan(plan)
	if err != nil {
		t.Fatalf("BriefFromPlan rejected a complete plan: %v", err)
	}
	want := []string{"crash reproduces", "login succeeds after restart", "no regression in auth suite"}
	if brief.Goal != plan.Goal || brief.Scope != plan.Scope || brief.VerificationMethod != plan.VerificationMethod ||
		len(brief.AcceptanceCriteria) != len(want) {
		t.Fatalf("derived brief = %+v, want criteria %v", brief, want)
	}
	for index, criterion := range want {
		if brief.AcceptanceCriteria[index] != criterion {
			t.Fatalf("derived criteria[%d] = %q, want %q", index, brief.AcceptanceCriteria[index], criterion)
		}
	}

	for name, mutate := range map[string]func(*Plan){
		"missing scope":               func(value *Plan) { value.Scope = "" },
		"missing verification method": func(value *Plan) { value.VerificationMethod = "" },
		"no acceptance criteria": func(value *Plan) {
			for index := range value.Steps {
				value.Steps[index].CompletionCriteria = nil
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			value := plan
			value.Steps = append([]PlanStep(nil), plan.Steps...)
			mutate(&value)
			if _, err := BriefFromPlan(value); err == nil {
				t.Fatal("BriefFromPlan accepted an incomplete plan")
			}
		})
	}

	t.Run("missing scope names the field", func(t *testing.T) {
		value := plan
		value.Scope = ""
		_, err := BriefFromPlan(value)
		if err == nil || !strings.Contains(err.Error(), "scope") {
			t.Fatalf("missing-scope error = %v, want it to name scope", err)
		}
	})
	t.Run("missing verification method names the field", func(t *testing.T) {
		value := plan
		value.VerificationMethod = ""
		_, err := BriefFromPlan(value)
		if err == nil || !strings.Contains(err.Error(), "verification_method") {
			t.Fatalf("missing-verification error = %v, want it to name verification_method", err)
		}
	})
	t.Run("no acceptance criteria names the field", func(t *testing.T) {
		value := plan
		value.Steps = append([]PlanStep(nil), plan.Steps...)
		for index := range value.Steps {
			value.Steps[index].CompletionCriteria = nil
		}
		_, err := BriefFromPlan(value)
		if err == nil || !strings.Contains(err.Error(), "acceptance_criteria") {
			t.Fatalf("missing-criteria error = %v, want it to name acceptance_criteria", err)
		}
	})
}
