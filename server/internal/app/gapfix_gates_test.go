package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

func TestCheckBriefGate(t *testing.T) {
	steps := func(n int) []task.PlanStep {
		out := make([]task.PlanStep, n)
		for i := range out {
			out[i] = task.PlanStep{ID: "s" + string(rune('1'+i)), Title: "step", Status: task.PlanStepPending}
		}
		return out
	}
	cases := []struct {
		name       string
		plan       task.Plan
		wantErr    bool
		wantFields []string
	}{
		{
			name:    "simple task without brief passes",
			plan:    task.Plan{Goal: "fix it", Steps: steps(1)},
			wantErr: false,
		},
		{
			name:    "complex task with complete brief passes",
			plan:    task.Plan{Goal: "fix it", Steps: steps(2), Brief: &task.Brief{Scope: "s", Acceptance: "a", VerificationMethod: "v"}},
			wantErr: false,
		},
		{
			name:       "complex task without brief is rejected and names the gaps",
			plan:       task.Plan{Goal: "fix it", Steps: steps(2)},
			wantErr:    true,
			wantFields: []string{"scope", "acceptance", "verification_method"},
		},
		{
			name:       "complex task with partial brief names only the gap",
			plan:       task.Plan{Goal: "fix it", Steps: steps(2), Brief: &task.Brief{Scope: "s"}},
			wantErr:    true,
			wantFields: []string{"acceptance", "verification_method"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckBriefGate(tc.plan)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("CheckBriefGate() returned unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("CheckBriefGate() must reject a complex task without a complete brief")
			}
			if domain.CodeOf(err) != domain.CodeInvalidArgument {
				t.Fatalf("CheckBriefGate() error code = %v, want invalid_argument", domain.CodeOf(err))
			}
			for _, field := range tc.wantFields {
				if !strings.Contains(err.Error(), field) {
					t.Fatalf("CheckBriefGate() error %q does not name missing field %q", err.Error(), field)
				}
			}
		})
	}
}

func TestCheckRootCauseGate(t *testing.T) {
	rootCause := &task.RootCause{Statement: "nil map write on retry", ReproductionOutput: "panic: assignment to entry in nil map",
		CodeLines: []task.RootCauseCodeLine{{File: "server/internal/app/foo.go", StartLine: 42, EndLine: 44, Excerpt: "cache[key] = value"}}}
	cases := []struct {
		name       string
		plan       task.Plan
		wantErr    bool
		wantFields []string
	}{
		{
			name:    "plan without a fix proposal passes",
			plan:    task.Plan{Goal: "add feature", Steps: []task.PlanStep{{ID: "s1", Title: "step", Status: task.PlanStepPending}}},
			wantErr: false,
		},
		{
			name:    "fix plan with evidence-backed root cause passes",
			plan:    task.Plan{Goal: "fix it", Fix: &task.FixProposal{MinimalChange: "one line", RootCause: rootCause}},
			wantErr: false,
		},
		{
			name:       "fix plan without a root cause is rejected and names the evidence",
			plan:       task.Plan{Goal: "fix it", Fix: &task.FixProposal{MinimalChange: "one line"}},
			wantErr:    true,
			wantFields: []string{"statement", "reproduction_output", "code_lines"},
		},
		{
			name:       "fix plan with a statement but no reproduction names only the gap",
			plan:       task.Plan{Goal: "fix it", Fix: &task.FixProposal{MinimalChange: "one line", RootCause: &task.RootCause{Statement: "broken", CodeLines: []task.RootCauseCodeLine{{File: "a.go", StartLine: 1, EndLine: 1, Excerpt: "x"}}}}},
			wantErr:    true,
			wantFields: []string{"reproduction_output"},
		},
		{
			name:       "fix plan with a code line without an excerpt names the line",
			plan:       task.Plan{Goal: "fix it", Fix: &task.FixProposal{MinimalChange: "one line", RootCause: &task.RootCause{Statement: "broken", ReproductionOutput: "FAIL", CodeLines: []task.RootCauseCodeLine{{File: "a.go", StartLine: 1, EndLine: 1}}}}},
			wantErr:    true,
			wantFields: []string{"code_lines[0].excerpt"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckRootCauseGate(tc.plan)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("CheckRootCauseGate() returned unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("CheckRootCauseGate() must reject a fix plan without root-cause evidence")
			}
			if domain.CodeOf(err) != domain.CodeInvalidArgument {
				t.Fatalf("CheckRootCauseGate() error code = %v, want invalid_argument", domain.CodeOf(err))
			}
			for _, field := range tc.wantFields {
				if !strings.Contains(err.Error(), field) {
					t.Fatalf("CheckRootCauseGate() error %q does not name missing evidence %q", err.Error(), field)
				}
			}
		})
	}
}

func TestUnrunCoveringTests(t *testing.T) {
	impact := &task.ImpactMap{ChangedFiles: []string{"a.go"}, Callers: []string{"b.go"}, CoveringTests: []string{"TestA", "TestB"}}
	cases := []struct {
		name   string
		tested []string
		want   []string
	}{
		{name: "all covering tests ran", tested: []string{"TestA", "TestB", "TestOther"}, want: nil},
		{name: "one covering test missing", tested: []string{"TestA"}, want: []string{"TestB"}},
		{name: "no covering test ran", tested: []string{"TestOther"}, want: []string{"TestA", "TestB"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UnrunCoveringTests(impact, tc.tested)
			if len(got) != len(tc.want) {
				t.Fatalf("UnrunCoveringTests() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("UnrunCoveringTests() = %v, want %v", got, tc.want)
				}
			}
		})
	}
	if got := UnrunCoveringTests(nil, nil); len(got) != 0 {
		t.Fatalf("UnrunCoveringTests() without an impact map = %v, want empty", got)
	}
}

func TestQueryTaskImpact(t *testing.T) {
	plan := &task.Plan{Impact: &task.ImpactMap{ChangedFiles: []string{"b.go", "a.go", "b.go"}, Callers: []string{"c.go"}, CoveringTests: []string{"TestB", "TestA"}}}
	got, ok := QueryTaskImpact(plan)
	if !ok {
		t.Fatal("QueryTaskImpact() must find the recorded impact map")
	}
	if len(got.ChangedFiles) != 2 || got.ChangedFiles[0] != "a.go" || got.ChangedFiles[1] != "b.go" {
		t.Fatalf("QueryTaskImpact() changed files = %v, want [a.go b.go]", got.ChangedFiles)
	}
	if _, ok := QueryTaskImpact(nil); ok {
		t.Fatal("QueryTaskImpact() on a nil plan must report nothing")
	}
	if _, ok := QueryTaskImpact(&task.Plan{}); ok {
		t.Fatal("QueryTaskImpact() on a plan without an impact map must report nothing")
	}
	// The query must not mutate the recorded map.
	if len(plan.Impact.ChangedFiles) != 3 {
		t.Fatalf("QueryTaskImpact() mutated the recorded impact map: %v", plan.Impact.ChangedFiles)
	}
}

func TestCheckFailingTestGate(t *testing.T) {
	fix := &task.FixProposal{MinimalChange: "change one line"}
	record := &task.FailingTestRecord{TestName: "TestFoo", FailureExcerpt: "FAIL"}
	cases := []struct {
		name     string
		plan     *task.Plan
		hostless bool
		wantErr  bool
	}{
		{name: "fix task without a failing test is blocked", plan: &task.Plan{Fix: fix}, hostless: false, wantErr: true},
		{name: "fix task with a failing test passes", plan: &task.Plan{Fix: fix, FailingTest: record}, hostless: false, wantErr: false},
		{name: "hostless fix task is exempt (known gap)", plan: &task.Plan{Fix: fix}, hostless: true, wantErr: false},
		{name: "non-fix task without a failing test passes", plan: &task.Plan{}, hostless: false, wantErr: false},
		{name: "no plan passes", plan: nil, hostless: false, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckFailingTestGate(tc.plan, tc.hostless)
			if tc.wantErr && err == nil {
				t.Fatal("CheckFailingTestGate() must block a fix task without a failing test")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("CheckFailingTestGate() returned unexpected error: %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "failing test") {
				t.Fatalf("CheckFailingTestGate() error %q does not name the missing failing test record", err.Error())
			}
		})
	}
}

func TestCheckMinimalDiffGate(t *testing.T) {
	impact := &task.ImpactMap{ChangedFiles: []string{"a.go"}, Callers: []string{"b.go"}, CoveringTests: []string{"TestA"}}
	cases := []struct {
		name    string
		plan    *task.Plan
		files   []string
		wantErr bool
	}{
		{name: "write inside the impact map passes", plan: &task.Plan{Impact: impact}, files: []string{"a.go", "b.go"}, wantErr: false},
		{name: "write outside the impact map is blocked", plan: &task.Plan{Impact: impact}, files: []string{"a.go", "c.go"}, wantErr: true},
		{name: "no recorded impact map passes", plan: &task.Plan{}, files: []string{"a.go", "c.go"}, wantErr: false},
		{name: "no plan passes", plan: nil, files: []string{"a.go"}, wantErr: false},
		{name: "no files passes", plan: &task.Plan{Impact: impact}, files: nil, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckMinimalDiffGate(tc.plan, tc.files)
			if tc.wantErr && err == nil {
				t.Fatal("CheckMinimalDiffGate() must block a write outside the recorded impact map")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("CheckMinimalDiffGate() returned unexpected error: %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "c.go") {
				t.Fatalf("CheckMinimalDiffGate() error %q does not name the out-of-scope file", err.Error())
			}
		})
	}
}

func TestAgentFileEvidencePaths(t *testing.T) {
	evidence := []AgentFileEvidence{{Path: "a.go"}, {Path: ""}, {Path: "b.go"}}
	got := agentFileEvidencePaths(evidence)
	if len(got) != 2 || got[0] != "a.go" || got[1] != "b.go" {
		t.Fatalf("agentFileEvidencePaths() = %v, want [a.go b.go]", got)
	}
	if got := agentFileEvidencePaths(nil); len(got) != 0 {
		t.Fatalf("agentFileEvidencePaths(nil) = %v, want empty", got)
	}
}

func TestEvaluateTestGate(t *testing.T) {
	complete := TestGateReport{
		NewTest: "TestFoo", NewTestRan: true, NewTestPassed: true,
		ExistingSuiteRan: true, ExistingSuitePassed: true,
		CoveringTests: []string{"TestFoo", "TestBar"}, CoveringTestsRan: []string{"TestFoo", "TestBar"},
	}
	cases := []struct {
		name   string
		report TestGateReport
		want   bool
	}{
		{name: "new test and suite both passed", report: complete, want: true},
		{name: "new test never ran blocks", report: TestGateReport{NewTest: "TestFoo", ExistingSuiteRan: true, ExistingSuitePassed: true}, want: false},
		{name: "new test failed blocks", report: TestGateReport{NewTest: "TestFoo", NewTestRan: true, NewTestPassed: false, ExistingSuiteRan: true, ExistingSuitePassed: true}, want: false},
		{name: "suite never ran blocks", report: TestGateReport{NewTest: "TestFoo", NewTestRan: true, NewTestPassed: true}, want: false},
		{name: "suite failed blocks", report: TestGateReport{NewTest: "TestFoo", NewTestRan: true, NewTestPassed: true, ExistingSuiteRan: true, ExistingSuitePassed: false}, want: false},
		{name: "unrun covering test blocks", report: TestGateReport{
			NewTest: "TestFoo", NewTestRan: true, NewTestPassed: true,
			ExistingSuiteRan: true, ExistingSuitePassed: true,
			CoveringTests: []string{"TestFoo", "TestBar"}, CoveringTestsRan: []string{"TestFoo"},
		}, want: false},
		{name: "no covering tests to check passes", report: TestGateReport{
			NewTest: "TestFoo", NewTestRan: true, NewTestPassed: true,
			ExistingSuiteRan: true, ExistingSuitePassed: true,
		}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := EvaluateTestGate(tc.report)
			if decision.Allowed != tc.want {
				t.Fatalf("EvaluateTestGate() allowed = %v, want %v (reason %q)", decision.Allowed, tc.want, decision.Reason)
			}
			if !tc.want && decision.Reason == "" {
				t.Fatal("EvaluateTestGate() must give a reason when it blocks")
			}
		})
	}
}

func shellTestInputs(t *testing.T, calls []struct {
	callID  string
	command string
	output  string
}) []ModelInput {
	t.Helper()
	var inputs []ModelInput
	for _, call := range calls {
		inputs = append(inputs,
			ModelInput{Type: "function_call", CallID: call.callID, Name: runtimeToolShellExec,
				Arguments: fmt.Sprintf(`{"workspace_id":"w1234567","command":%q}`, call.command)},
			ModelInput{Type: "function_call_output", CallID: call.callID, Output: call.output},
		)
	}
	return inputs
}

func TestTestReportFromInputs(t *testing.T) {
	passOutput := `{"exit_code":0,"stdout":"ok","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`
	failOutput := `{"exit_code":1,"stdout":"FAIL","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`
	t.Run("new test and suite both passed", func(t *testing.T) {
		inputs := shellTestInputs(t, []struct {
			callID  string
			command string
			output  string
		}{
			{callID: "c1", command: "go test -run TestFoo ./internal/app/", output: passOutput},
			{callID: "c2", command: "go test ./...", output: passOutput},
		})
		report := TestReportFromInputs(inputs, "TestFoo", &task.ImpactMap{CoveringTests: []string{"TestFoo", "TestBar"}})
		if !report.NewTestRan || !report.NewTestPassed || !report.ExistingSuiteRan || !report.ExistingSuitePassed {
			t.Fatalf("TestReportFromInputs() = %+v, want all ran and passed", report)
		}
		if len(report.CoveringTestsRan) != 2 {
			t.Fatalf("TestReportFromInputs() covering tests ran = %v, want both", report.CoveringTestsRan)
		}
	})
	t.Run("failed new test is not a pass", func(t *testing.T) {
		inputs := shellTestInputs(t, []struct {
			callID  string
			command string
			output  string
		}{
			{callID: "c1", command: "go test -run TestFoo ./internal/app/", output: failOutput},
			{callID: "c2", command: "go test ./...", output: passOutput},
		})
		report := TestReportFromInputs(inputs, "TestFoo", nil)
		if !report.NewTestRan || report.NewTestPassed {
			t.Fatalf("TestReportFromInputs() = %+v, want new test ran but failed", report)
		}
	})
	t.Run("single-test run is not a suite run", func(t *testing.T) {
		inputs := shellTestInputs(t, []struct {
			callID  string
			command string
			output  string
		}{
			{callID: "c1", command: "go test -run TestFoo ./internal/app/", output: passOutput},
		})
		report := TestReportFromInputs(inputs, "TestFoo", &task.ImpactMap{CoveringTests: []string{"TestFoo"}})
		if report.ExistingSuiteRan {
			t.Fatalf("TestReportFromInputs() = %+v, want no suite run", report)
		}
		if len(report.CoveringTestsRan) != 1 || report.CoveringTestsRan[0] != "TestFoo" {
			t.Fatalf("TestReportFromInputs() covering tests ran = %v, want [TestFoo]", report.CoveringTestsRan)
		}
	})
	t.Run("unreadable output is not a pass", func(t *testing.T) {
		inputs := shellTestInputs(t, []struct {
			callID  string
			command string
			output  string
		}{
			{callID: "c1", command: "go test -run TestFoo ./internal/app/", output: "not json"},
			{callID: "c2", command: "go test ./...", output: passOutput},
		})
		report := TestReportFromInputs(inputs, "TestFoo", nil)
		if report.NewTestPassed {
			t.Fatalf("TestReportFromInputs() = %+v, want new test not passed", report)
		}
	})
	t.Run("no test calls means nothing ran", func(t *testing.T) {
		report := TestReportFromInputs(nil, "TestFoo", &task.ImpactMap{CoveringTests: []string{"TestFoo"}})
		if report.NewTestRan || report.ExistingSuiteRan {
			t.Fatalf("TestReportFromInputs() = %+v, want nothing ran", report)
		}
	})
}

func TestRunShellTestCommand(t *testing.T) {
	ctx := context.Background()
	if err := RunShellTestCommand(ctx, "true"); err != nil {
		t.Fatalf("RunShellTestCommand(true) returned unexpected error: %v", err)
	}
	if err := RunShellTestCommand(ctx, "false"); err == nil {
		t.Fatal("RunShellTestCommand(false) must fail on a non-zero exit code")
	}
	if err := RunShellTestCommand(ctx, ""); err == nil {
		t.Fatal("RunShellTestCommand with no command must fail")
	}
	if err := RunShellTestCommand(nil, "true"); err == nil {
		t.Fatal("RunShellTestCommand with a nil context must fail")
	}
}

func TestCheckImpactCoverageGate(t *testing.T) {
	plan := func(impact *task.ImpactMap) *task.Plan { return &task.Plan{Impact: impact} }
	impact := &task.ImpactMap{
		ChangedFiles:  []string{"server/internal/app/foo.go", "server/internal/worker/bar.go"},
		CoveringTests: []string{"TestFoo", "TestBar"},
		Coverage: map[string][]string{
			"server/internal/app/foo.go":    {"TestFoo"},
			"server/internal/worker/bar.go": {"TestBar"},
		},
	}
	for _, tc := range []struct {
		name     string
		plan     *task.Plan
		tested   []string
		wantErr  bool
		wantFile string
	}{
		{name: "uncovered changed file blocks and names it", plan: plan(impact), tested: []string{"TestFoo"}, wantErr: true, wantFile: "server/internal/worker/bar.go"},
		{name: "all changed files covered passes", plan: plan(impact), tested: []string{"TestFoo", "TestBar"}},
		{name: "no plan passes", plan: nil},
		{name: "no impact passes", plan: plan(nil)},
		{name: "no per-file coverage passes, flat check is the only对照", plan: plan(&task.ImpactMap{
			ChangedFiles: []string{"server/internal/app/foo.go"}, CoveringTests: []string{"TestFoo"},
		}), tested: []string{"TestFoo"}},
		{name: "coverage only for a caller still blocks the changed file", plan: plan(&task.ImpactMap{
			ChangedFiles:  []string{"server/internal/app/foo.go"},
			Callers:       []string{"server/internal/app/caller.go"},
			CoveringTests: []string{"TestCaller"},
			Coverage:      map[string][]string{"server/internal/app/caller.go": {"TestCaller"}},
		}), tested: []string{"TestCaller"}, wantErr: true, wantFile: "server/internal/app/foo.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckImpactCoverageGate(tc.plan, tc.tested)
			if tc.wantErr && err == nil {
				t.Fatal("CheckImpactCoverageGate() must block an uncovered changed file")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("CheckImpactCoverageGate() error=%v, want pass", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.wantFile) {
				t.Fatalf("CheckImpactCoverageGate() error=%v, want it to name %s", err, tc.wantFile)
			}
		})
	}
}

func TestTestReportFromInputsSuiteScope(t *testing.T) {
	passOutput := `{"exit_code":0,"stdout":"ok","stderr":"","stdout_truncated":false,"stderr_truncated":false,"timed_out":false}`
	impact := &task.ImpactMap{
		ChangedFiles:  []string{"server/internal/app/foo.go", "server/internal/worker/bar.go"},
		CoveringTests: []string{"TestFoo", "TestBar"},
		Coverage: map[string][]string{
			"server/internal/app/foo.go":    {"TestFoo"},
			"server/internal/worker/bar.go": {"TestBar"},
		},
	}
	inputs := func(commands ...string) []ModelInput {
		calls := make([]struct {
			callID  string
			command string
			output  string
		}, 0, len(commands))
		for i, command := range commands {
			calls = append(calls, struct {
				callID  string
				command string
				output  string
			}{callID: fmt.Sprintf("c%d", i+1), command: command, output: passOutput})
		}
		return shellTestInputs(t, calls)
	}
	// The counterexample: the new test ran, a suite ran and passed — but
	// only in ./internal/app/, while the impact also covers a worker
	// file. The old shortcut counted every covering test as run and let
	// the gate pass; the scope-aware report must not, and both gates
	// must block.
	t.Run("scoped suite outside the impact blocks both gates", func(t *testing.T) {
		report := TestReportFromInputs(inputs("go test -run TestFoo ./internal/app/", "go test ./internal/app/"), "TestFoo", impact)
		if len(report.CoveringTestsRan) != 1 || report.CoveringTestsRan[0] != "TestFoo" {
			t.Fatalf("CoveringTestsRan=%v, want [TestFoo]", report.CoveringTestsRan)
		}
		if decision := EvaluateTestGate(report); decision.Allowed {
			t.Fatal("EvaluateTestGate() allowed a run whose covering test never ran")
		}
		err := CheckImpactCoverageGate(&task.Plan{Impact: impact}, report.CoveringTestsRan)
		if err == nil || !strings.Contains(err.Error(), "server/internal/worker/bar.go") {
			t.Fatalf("CheckImpactCoverageGate() error=%v, want it to name server/internal/worker/bar.go", err)
		}
	})
	t.Run("scoped suite covering every impacted package passes", func(t *testing.T) {
		report := TestReportFromInputs(inputs("go test -run TestFoo ./internal/app/", "go test ./internal/app/ ./internal/worker/"), "TestFoo", impact)
		if len(report.CoveringTestsRan) != 2 {
			t.Fatalf("CoveringTestsRan=%v, want both covering tests", report.CoveringTestsRan)
		}
		if decision := EvaluateTestGate(report); !decision.Allowed {
			t.Fatalf("EvaluateTestGate() blocked a fully covered run: %q", decision.Reason)
		}
		if err := CheckImpactCoverageGate(&task.Plan{Impact: impact}, report.CoveringTestsRan); err != nil {
			t.Fatalf("CheckImpactCoverageGate() error=%v, want pass", err)
		}
	})
	t.Run("repo-root package spelling matches workspace-relative files", func(t *testing.T) {
		report := TestReportFromInputs(inputs("go test -run TestFoo ./server/internal/app/", "go test ./server/internal/app/ ./server/internal/worker/"), "TestFoo", impact)
		if len(report.CoveringTestsRan) != 2 {
			t.Fatalf("CoveringTestsRan=%v, want both covering tests", report.CoveringTestsRan)
		}
	})
	t.Run("scoped suite without per-file coverage proves nothing", func(t *testing.T) {
		flat := &task.ImpactMap{ChangedFiles: impact.ChangedFiles, CoveringTests: impact.CoveringTests}
		report := TestReportFromInputs(inputs("go test -run TestFoo ./internal/app/", "go test ./internal/app/ ./internal/worker/"), "TestFoo", flat)
		if len(report.CoveringTestsRan) != 1 || report.CoveringTestsRan[0] != "TestFoo" {
			t.Fatalf("CoveringTestsRan=%v, want only the named TestFoo", report.CoveringTestsRan)
		}
		if decision := EvaluateTestGate(report); decision.Allowed {
			t.Fatal("EvaluateTestGate() allowed a scoped run with no per-file coverage to stand in for TestBar")
		}
	})
	t.Run("whole-tree suite still covers everything", func(t *testing.T) {
		report := TestReportFromInputs(inputs("go test -run TestFoo ./internal/app/", "go test ./..."), "TestFoo", impact)
		if len(report.CoveringTestsRan) != 2 {
			t.Fatalf("CoveringTestsRan=%v, want both covering tests", report.CoveringTestsRan)
		}
	})
}

func TestTestSuiteScope(t *testing.T) {
	for _, tc := range []struct {
		command   string
		wholeTree bool
		packages  []string
	}{
		{command: "go test ./...", wholeTree: true},
		{command: "go test ./server/...", wholeTree: true},
		{command: "go test ./internal/app/", packages: []string{"internal/app"}},
		{command: "go test -count=1 ./internal/app/ ./internal/worker/", packages: []string{"internal/app", "internal/worker"}},
		{command: "go test ./server/internal/app/", packages: []string{"server/internal/app"}},
		{command: "make test", wholeTree: true},
		{command: "npm test", wholeTree: true},
	} {
		t.Run(tc.command, func(t *testing.T) {
			wholeTree, packages := testSuiteScope(tc.command)
			if wholeTree != tc.wholeTree {
				t.Fatalf("testSuiteScope(%q) wholeTree=%v, want %v", tc.command, wholeTree, tc.wholeTree)
			}
			if len(packages) != len(tc.packages) {
				t.Fatalf("testSuiteScope(%q) packages=%v, want %v", tc.command, packages, tc.packages)
			}
			for i, want := range tc.packages {
				if packages[i] != want {
					t.Fatalf("testSuiteScope(%q) packages[%d]=%q, want %q", tc.command, i, packages[i], want)
				}
			}
		})
	}
}
