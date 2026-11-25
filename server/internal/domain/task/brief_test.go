package task

import (
	"reflect"
	"testing"
)

func TestBriefMissingFields(t *testing.T) {
	cases := []struct {
		name  string
		brief *Brief
		want  []string
	}{
		{name: "nil brief misses all three", brief: nil, want: []string{"scope", "acceptance", "verification_method"}},
		{name: "empty brief misses all three", brief: &Brief{}, want: []string{"scope", "acceptance", "verification_method"}},
		{name: "blank fields count as missing", brief: &Brief{Scope: "  ", Acceptance: "x", VerificationMethod: "y"}, want: []string{"scope"}},
		{name: "complete brief misses nothing", brief: &Brief{Scope: "s", Acceptance: "a", VerificationMethod: "v"}, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.brief.MissingFields(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("MissingFields() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBriefValidate(t *testing.T) {
	if err := (&Brief{Scope: "s", Acceptance: "a", VerificationMethod: "v"}).Validate(); err != nil {
		t.Fatalf("complete brief should validate: %v", err)
	}
	if err := (&Brief{}).Validate(); err != nil {
		t.Fatalf("empty brief is structurally valid (completeness is separate): %v", err)
	}
	if err := (&Brief{Scope: string(make([]byte, 2049))}).Validate(); err == nil {
		t.Fatal("oversized scope must fail validation")
	}
}

func TestPlanBlockingBriefGaps(t *testing.T) {
	steps := func(n int) []PlanStep {
		out := make([]PlanStep, n)
		for i := range out {
			out[i] = PlanStep{ID: "s" + string(rune('1'+i)), Title: "step", Status: PlanStepPending}
		}
		return out
	}
	cases := []struct {
		name string
		plan Plan
		want []string
	}{
		{
			name: "simple task never blocks",
			plan: Plan{Goal: "fix it", Steps: steps(1)},
			want: nil,
		},
		{
			name: "complex task with complete brief does not block",
			plan: Plan{Goal: "fix it", Steps: steps(2), Brief: &Brief{Scope: "s", Acceptance: "a", VerificationMethod: "v"}},
			want: nil,
		},
		{
			name: "complex task without brief names all missing fields",
			plan: Plan{Goal: "fix it", Steps: steps(2)},
			want: []string{"scope", "acceptance", "verification_method"},
		},
		{
			name: "complex task with partial brief names only the gap",
			plan: Plan{Goal: "fix it", Steps: steps(3), Brief: &Brief{Scope: "s", VerificationMethod: "v"}},
			want: []string{"acceptance"},
		},
		{
			name: "complex task without goal names goal first",
			plan: Plan{Steps: steps(2)},
			want: []string{"goal", "scope", "acceptance", "verification_method"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.plan.BlockingBriefGaps(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("BlockingBriefGaps() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFixProposalValidate(t *testing.T) {
	valid := &FixProposal{MinimalChange: "change one line", Risks: []string{"may break x"}, Alternatives: []string{"rewrite"}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid fix proposal should validate: %v", err)
	}
	if err := (&FixProposal{}).Validate(); err == nil {
		t.Fatal("fix proposal without a minimal change must fail validation")
	}
	if err := (&FixProposal{MinimalChange: "x", Risks: []string{"a", "a"}}).Validate(); err == nil {
		t.Fatal("duplicated risks must fail validation")
	}
	valid.RootCause = validRootCause()
	if err := valid.Validate(); err != nil {
		t.Fatalf("fix proposal with an evidence-backed root cause should validate: %v", err)
	}
	valid.RootCause = &RootCause{Statement: "broken", ReproductionOutput: "FAIL", CodeLines: []RootCauseCodeLine{{File: "a.go", StartLine: 1, EndLine: 1}}}
	if err := valid.Validate(); err == nil {
		t.Fatal("fix proposal with a root cause code line without an excerpt must fail validation")
	}
}

func validRootCause() *RootCause {
	return &RootCause{Statement: "nil map write on retry", ReproductionOutput: "panic: assignment to entry in nil map",
		CodeLines: []RootCauseCodeLine{{File: "server/internal/app/foo.go", StartLine: 42, EndLine: 44, Excerpt: "cache[key] = value"}}}
}

func TestRootCauseValidate(t *testing.T) {
	if err := validRootCause().Validate(); err != nil {
		t.Fatalf("valid root cause should validate: %v", err)
	}
	if err := (*RootCause)(nil).Validate(); err != nil {
		t.Fatalf("nil root cause should validate (absent is allowed): %v", err)
	}
	cases := []struct {
		name  string
		cause *RootCause
	}{
		{name: "no statement", cause: &RootCause{ReproductionOutput: "FAIL", CodeLines: []RootCauseCodeLine{{File: "a.go", StartLine: 1, EndLine: 1, Excerpt: "x"}}}},
		{name: "no reproduction output", cause: &RootCause{Statement: "broken", CodeLines: []RootCauseCodeLine{{File: "a.go", StartLine: 1, EndLine: 1, Excerpt: "x"}}}},
		{name: "no code lines", cause: &RootCause{Statement: "broken", ReproductionOutput: "FAIL"}},
		{name: "absolute file path", cause: &RootCause{Statement: "broken", ReproductionOutput: "FAIL", CodeLines: []RootCauseCodeLine{{File: "/etc/passwd", StartLine: 1, EndLine: 1, Excerpt: "x"}}}},
		{name: "end line before start line", cause: &RootCause{Statement: "broken", ReproductionOutput: "FAIL", CodeLines: []RootCauseCodeLine{{File: "a.go", StartLine: 5, EndLine: 2, Excerpt: "x"}}}},
		{name: "line range too wide", cause: &RootCause{Statement: "broken", ReproductionOutput: "FAIL", CodeLines: []RootCauseCodeLine{{File: "a.go", StartLine: 1, EndLine: 100, Excerpt: "x"}}}},
		{name: "empty excerpt", cause: &RootCause{Statement: "broken", ReproductionOutput: "FAIL", CodeLines: []RootCauseCodeLine{{File: "a.go", StartLine: 1, EndLine: 1}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cause.Validate(); err == nil {
				t.Fatal("invalid root cause must fail validation")
			}
		})
	}
}

func TestRootCauseMissingEvidence(t *testing.T) {
	if got := (*RootCause)(nil).MissingEvidence(); len(got) != 3 {
		t.Fatalf("nil root cause must miss all three evidence pieces, got %v", got)
	}
	cause := validRootCause()
	if got := cause.MissingEvidence(); len(got) != 0 {
		t.Fatalf("evidence-backed root cause must miss nothing, got %v", got)
	}
	cause.CodeLines[0].Excerpt = ""
	if got := cause.MissingEvidence(); len(got) != 1 || got[0] != "code_lines[0].excerpt" {
		t.Fatalf("invalid code line must be named by index, got %v", got)
	}
}

func TestFailingTestRecordValidate(t *testing.T) {
	if err := (&FailingTestRecord{TestName: "TestFoo", FailureExcerpt: "FAIL"}).Validate(); err != nil {
		t.Fatalf("valid failing test record should validate: %v", err)
	}
	if err := (&FailingTestRecord{TestName: "TestFoo"}).Validate(); err == nil {
		t.Fatal("failing test record without failure output must fail validation")
	}
	if err := (&FailingTestRecord{FailureExcerpt: "FAIL"}).Validate(); err == nil {
		t.Fatal("failing test record without a test name must fail validation")
	}
}

func TestImpactMapValidate(t *testing.T) {
	valid := &ImpactMap{ChangedFiles: []string{"server/internal/app/foo.go"}, Callers: []string{"server/internal/app/bar.go"}, CoveringTests: []string{"TestFoo"},
		Coverage: map[string][]string{"server/internal/app/foo.go": {"TestFoo"}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid impact map should validate: %v", err)
	}
	cases := []struct {
		name   string
		impact *ImpactMap
	}{
		{name: "no changed files", impact: &ImpactMap{}},
		{name: "absolute path", impact: &ImpactMap{ChangedFiles: []string{"/etc/passwd"}}},
		{name: "parent traversal", impact: &ImpactMap{ChangedFiles: []string{"../secret.go"}}},
		{name: "duplicated covering test", impact: &ImpactMap{ChangedFiles: []string{"a.go"}, CoveringTests: []string{"TestA", "TestA"}}},
		{name: "coverage absolute path", impact: &ImpactMap{ChangedFiles: []string{"a.go"}, CoveringTests: []string{"TestA"},
			Coverage: map[string][]string{"/etc/passwd": {"TestA"}}}},
		{name: "coverage duplicated test", impact: &ImpactMap{ChangedFiles: []string{"a.go"}, CoveringTests: []string{"TestA"},
			Coverage: map[string][]string{"a.go": {"TestA", "TestA"}}}},
		{name: "coverage empty test name", impact: &ImpactMap{ChangedFiles: []string{"a.go"}, CoveringTests: []string{"TestA"},
			Coverage: map[string][]string{"a.go": {""}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.impact.Validate(); err == nil {
				t.Fatal("invalid impact map must fail validation")
			}
		})
	}
}

func TestPlanValidateAcceptsOptionalPlannerRecords(t *testing.T) {
	plan := Plan{
		TaskRevision: 1, Revision: 1, Goal: "fix it", CostEstimateState: "unknown",
		Steps:       []PlanStep{{ID: "s1", Title: "step", Status: PlanStepPending}},
		Brief:       &Brief{Scope: "s", Acceptance: "a", VerificationMethod: "v"},
		Fix:         &FixProposal{MinimalChange: "one line"},
		Impact:      &ImpactMap{ChangedFiles: []string{"a.go"}, CoveringTests: []string{"TestA"}},
		FailingTest: &FailingTestRecord{TestName: "TestA", FailureExcerpt: "FAIL"},
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("plan with planner records should validate: %v", err)
	}
	plan.Impact = &ImpactMap{ChangedFiles: []string{"/abs.go"}}
	if err := plan.Validate(); err == nil {
		t.Fatal("plan with an invalid impact map must fail validation")
	}
}
