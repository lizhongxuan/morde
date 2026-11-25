package task

import (
	"encoding/json"
	"strings"
	"testing"
)

func validFixPlan() *FixPlan {
	return &FixPlan{ChangedFiles: []string{"auth/session.go"}, Risks: []string{"session expiry shifts by a day"},
		Alternatives: []string{"restart the service daily"}}
}

func planWithFixPlan(fixPlan *FixPlan) Plan {
	return Plan{TaskRevision: 0, Revision: 1, Goal: "Fix the login crash", CostEstimateState: "unknown",
		Steps: []PlanStep{{ID: "repro", Title: "Reproduce", Status: PlanStepPending,
			CompletionCriteria: []string{"crash reproduces"}}},
		FixPlan: fixPlan}
}

func TestPlanFixPlanValidation(t *testing.T) {
	if err := planWithFixPlan(nil).Validate(); err != nil {
		t.Fatalf("plan without a fix plan rejected: %v", err)
	}
	if err := planWithFixPlan(validFixPlan()).Validate(); err != nil {
		t.Fatalf("plan with a valid fix plan rejected: %v", err)
	}
	if err := planWithFixPlan(&FixPlan{}).Validate(); err != nil {
		t.Fatalf("plan with an empty fix plan rejected: %v", err)
	}
	for name, fixPlan := range map[string]*FixPlan{
		"too many changed files":   {ChangedFiles: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
		"changed file too long":    {ChangedFiles: []string{strings.Repeat("x", 513)}},
		"changed file empty":       {ChangedFiles: []string{""}},
		"changed files duplicated": {ChangedFiles: []string{"a.go", "a.go"}},
		"too many risks":           {Risks: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
		"risk too long":            {Risks: []string{strings.Repeat("x", 513)}},
		"too many alternatives":    {Alternatives: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}},
		"alternative too long":     {Alternatives: []string{strings.Repeat("x", 513)}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := planWithFixPlan(fixPlan).Validate(); err == nil {
				t.Fatal("plan with an invalid fix plan was accepted")
			}
		})
	}
}

func TestPlanFixPlanJSONRoundTrip(t *testing.T) {
	encoded, err := json.Marshal(planWithFixPlan(nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "fix_plan") {
		t.Fatalf("plan without a fix plan encoded a fix_plan field: %s", encoded)
	}
	var decoded Plan
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.FixPlan != nil || decoded.Validate() != nil {
		t.Fatalf("plan without a fix plan did not round-trip: decoded=%+v, err=%v", decoded, err)
	}
	encoded, err = json.Marshal(planWithFixPlan(validFixPlan()))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.FixPlan == nil || decoded.Validate() != nil ||
		len(decoded.FixPlan.ChangedFiles) != 1 || decoded.FixPlan.ChangedFiles[0] != "auth/session.go" ||
		len(decoded.FixPlan.Risks) != 1 || len(decoded.FixPlan.Alternatives) != 1 {
		t.Fatalf("plan with a fix plan did not round-trip: decoded=%+v, err=%v", decoded, err)
	}
}

func TestBriefFromPlanCarriesFixPlan(t *testing.T) {
	plan := planWithFixPlan(validFixPlan())
	plan.Scope = "auth package only"
	plan.VerificationMethod = "run the failing test"
	brief, err := BriefFromPlan(plan)
	if err != nil {
		t.Fatalf("BriefFromPlan rejected a complete plan with a fix plan: %v", err)
	}
	if brief.FixPlan == nil || len(brief.FixPlan.ChangedFiles) != 1 || brief.FixPlan.ChangedFiles[0] != "auth/session.go" {
		t.Fatalf("derived brief lost the fix plan: %+v", brief)
	}
	if err := brief.Validate(); err != nil {
		t.Fatalf("derived brief with a fix plan is invalid: %v", err)
	}
}

func validRootCause() *RootCause {
	return &RootCause{Summary: "session expiry is computed from the wall clock instead of the monotonic clock",
		ReproOutput: "--- FAIL: TestSessionExpiry (0.00s)\n    session_test.go:42: expiry 2026-11-25T00:00:00Z, want 2026-11-26T00:00:00Z",
		CodeLines:   []string{"auth/session.go:42", "auth/session.go:87"}}
}

func TestPlanFixPlanRootCauseValidation(t *testing.T) {
	withRootCause := func(rootCause *RootCause) *FixPlan {
		fixPlan := validFixPlan()
		fixPlan.RootCause = rootCause
		return fixPlan
	}
	if err := planWithFixPlan(withRootCause(validRootCause())).Validate(); err != nil {
		t.Fatalf("plan with an evidenced root cause rejected: %v", err)
	}
	for name, rootCause := range map[string]*RootCause{
		"empty summary":          {Summary: "", ReproOutput: "FAIL", CodeLines: []string{"a.go:1"}},
		"summary too long":       {Summary: strings.Repeat("x", 1025), ReproOutput: "FAIL", CodeLines: []string{"a.go:1"}},
		"blank repro output":     {Summary: "s", ReproOutput: "  \n ", CodeLines: []string{"a.go:1"}},
		"repro output too long":  {Summary: "s", ReproOutput: strings.Repeat("x", MaxRootCauseReproOutputRunes+1), CodeLines: []string{"a.go:1"}},
		"no code lines":          {Summary: "s", ReproOutput: "FAIL", CodeLines: nil},
		"too many code lines":    {Summary: "s", ReproOutput: "FAIL", CodeLines: []string{"a.go:1", "b.go:2", "c.go:3", "d.go:4", "e.go:5", "f.go:6", "g.go:7", "h.go:8", "i.go:9"}},
		"absolute code line":     {Summary: "s", ReproOutput: "FAIL", CodeLines: []string{"/abs/path.go:1"}},
		"escaping code line":     {Summary: "s", ReproOutput: "FAIL", CodeLines: []string{"../escape.go:1"}},
		"code line without line": {Summary: "s", ReproOutput: "FAIL", CodeLines: []string{"file.go"}},
		"code line zero":         {Summary: "s", ReproOutput: "FAIL", CodeLines: []string{"file.go:0"}},
		"code line not a number": {Summary: "s", ReproOutput: "FAIL", CodeLines: []string{"file.go:abc"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := planWithFixPlan(withRootCause(rootCause)).Validate(); err == nil {
				t.Fatal("plan with an unevidenced root cause was accepted")
			}
		})
	}
	for _, line := range []string{"server/internal/app/model_gateway.go:639", "a.go:1", "dir/sub/file.test.go:123"} {
		if err := planWithFixPlan(withRootCause(&RootCause{Summary: "s", ReproOutput: "FAIL", CodeLines: []string{line}})).Validate(); err != nil {
			t.Fatalf("plan with code line %q rejected: %v", line, err)
		}
	}
}

func TestPlanFixPlanRootCauseJSONRoundTrip(t *testing.T) {
	plan := planWithFixPlan(validFixPlan())
	plan.FixPlan.RootCause = validRootCause()
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "root_cause") {
		t.Fatalf("plan with a root cause did not encode a root_cause field: %s", encoded)
	}
	var decoded Plan
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.FixPlan == nil || decoded.FixPlan.RootCause == nil ||
		decoded.FixPlan.RootCause.Summary != validRootCause().Summary ||
		len(decoded.FixPlan.RootCause.CodeLines) != 2 || decoded.Validate() != nil {
		t.Fatalf("plan with a root cause did not round-trip: decoded=%+v, err=%v", decoded, err)
	}
	plan.Scope = "auth package only"
	plan.VerificationMethod = "run the failing test"
	brief, err := BriefFromPlan(plan)
	if err != nil || brief.FixPlan == nil || brief.FixPlan.RootCause == nil ||
		brief.FixPlan.RootCause.ReproOutput != validRootCause().ReproOutput {
		t.Fatalf("derived brief lost the root cause: %+v, %v", brief, err)
	}
}
