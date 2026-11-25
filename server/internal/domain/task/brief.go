package task

import (
	"errors"
	"strconv"
	"strings"
)

// Brief is the four-piece execution brief (brief 四件套) the Planner role
// produces before a complex task is dispatched. The objective lives on
// Plan.Goal; the remaining three fields live here.
//
// Each field maps to a blocking dimension, which is what decides whether a
// missing field may be asked about at all:
//
//   - scope: execution range (what the task may touch)
//   - acceptance: acceptance criteria (what "done" means; guards irreversible
//     outcomes, since a fix must not land without a checkable bar)
//   - verification_method: how the result is verified (execution and cost:
//     without it the agent cannot prove the fix, so verification would fall
//     back to re-running the whole task)
//
// A missing field is only ever surfaced for a complex task (see
// Plan.BlockingBriefGaps); simple tasks never ask. The rejection is returned
// to the agent as structured tool-call information, never as a user-visible
// question.
type Brief struct {
	Scope              string `json:"scope"`
	Acceptance         string `json:"acceptance"`
	VerificationMethod string `json:"verification_method"`
}

// MissingFields names the brief fields that are empty, in fixed order.
func (b *Brief) MissingFields() []string {
	if b == nil {
		return []string{"scope", "acceptance", "verification_method"}
	}
	var missing []string
	if strings.TrimSpace(b.Scope) == "" {
		missing = append(missing, "scope")
	}
	if strings.TrimSpace(b.Acceptance) == "" {
		missing = append(missing, "acceptance")
	}
	if strings.TrimSpace(b.VerificationMethod) == "" {
		missing = append(missing, "verification_method")
	}
	return missing
}

// Validate rejects structurally invalid brief content. Completeness is a
// separate concern (MissingFields / Plan.BlockingBriefGaps).
func (b *Brief) Validate() error {
	if b == nil {
		return nil
	}
	if !planText(b.Scope, 0, 2048) || !planText(b.Acceptance, 0, 2048) || !planText(b.VerificationMethod, 0, 2048) {
		return errors.New("task brief fields are invalid")
	}
	return nil
}

// RootCauseCodeLine is one code location a root-cause conclusion points at:
// the file, the line range, and the excerpt the conclusion rests on. The
// excerpt is the evidence itself — a conclusion that names a file without
// quoting the lines it blames cannot be opened and checked.
type RootCauseCodeLine struct {
	File      string `json:"file"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Excerpt   string `json:"excerpt"`
}

func (l RootCauseCodeLine) Validate() error {
	if !validImpactPath(l.File) || l.StartLine < 1 || l.EndLine < l.StartLine || l.EndLine-l.StartLine > 64 ||
		!planText(l.Excerpt, 1, 1024) {
		return errors.New("task root cause code line is invalid")
	}
	return nil
}

// RootCause is the evidence-backed root-cause conclusion for a bug-fix task:
// what is broken, the reproduction output that demonstrates it, and the code
// lines it blames. Every conclusion must carry its evidence — a statement
// without a reproduction output or without a quoted code line is a guess,
// not a conclusion, and the root-cause gate rejects it.
type RootCause struct {
	Statement          string              `json:"statement"`
	ReproductionOutput string              `json:"reproduction_output"`
	CodeLines          []RootCauseCodeLine `json:"code_lines"`
}

// MissingEvidence names the evidence pieces a root-cause conclusion lacks,
// in fixed order: statement, reproduction_output, code_lines. A code line
// that is itself invalid is reported by its index (for example
// "code_lines[0].excerpt").
func (r *RootCause) MissingEvidence() []string {
	if r == nil {
		return []string{"statement", "reproduction_output", "code_lines"}
	}
	var missing []string
	if strings.TrimSpace(r.Statement) == "" {
		missing = append(missing, "statement")
	}
	if strings.TrimSpace(r.ReproductionOutput) == "" {
		missing = append(missing, "reproduction_output")
	}
	if len(r.CodeLines) == 0 {
		missing = append(missing, "code_lines")
	}
	for index, line := range r.CodeLines {
		if err := line.Validate(); err != nil {
			missing = append(missing, "code_lines["+strconv.Itoa(index)+"].excerpt")
		}
	}
	return missing
}

func (r *RootCause) Validate() error {
	if r == nil {
		return nil
	}
	if !planText(r.Statement, 1, 2048) || !planText(r.ReproductionOutput, 1, 2048) ||
		len(r.CodeLines) == 0 || len(r.CodeLines) > 16 {
		return errors.New("task root cause is invalid")
	}
	for _, line := range r.CodeLines {
		if err := line.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// FixProposal is the structured fix proposal (修复方案结构化) for a bug-fix
// task: the smallest change that could fix it, its risks, and the
// alternatives that were considered. It is advisory metadata recorded with
// the plan; it never grants tool access.
type FixProposal struct {
	MinimalChange string     `json:"minimal_change"`
	Risks         []string   `json:"risks"`
	Alternatives  []string   `json:"alternatives"`
	RootCause     *RootCause `json:"root_cause,omitempty"`
}

func (f *FixProposal) Validate() error {
	if f == nil {
		return nil
	}
	if !planText(f.MinimalChange, 1, 2048) || len(f.Risks) > MaxPlanListItems || len(f.Alternatives) > MaxPlanListItems ||
		!validPlanTextList(f.Risks, 512) || !validPlanTextList(f.Alternatives, 512) {
		return errors.New("task fix proposal is invalid")
	}
	return f.RootCause.Validate()
}

// FailingTestRecord is the reproduction of the bug as a failing test: the
// test that demonstrates the bug and the failure output it produced. A fix
// task must record one before any code write (see the failing-test gate);
// the record itself is a claim about a test run, not a re-run.
type FailingTestRecord struct {
	TestName       string `json:"test_name"`
	FailureExcerpt string `json:"failure_excerpt"`
}

func (r *FailingTestRecord) Validate() error {
	if r == nil {
		return nil
	}
	if !planText(r.TestName, 1, 256) || !planText(r.FailureExcerpt, 1, 2048) {
		return errors.New("task failing test record is invalid")
	}
	return nil
}

// ImpactMap is the impact surface (影响面清单) of a fix: the files the fix
// touches, their known callers, and the tests that cover them. It is the
// record the regression step checks against, file by file.
type ImpactMap struct {
	ChangedFiles  []string `json:"changed_files"`
	Callers       []string `json:"callers"`
	CoveringTests []string `json:"covering_tests"`
	// Coverage is the optional per-file coverage map (P3-2): file ->
	// tests covering that file. It is the edge list the regression
	// gate checks per changed file (CheckImpactCoverage); the flat
	// CoveringTests list is the union over the changed files and
	// their callers and cannot say which test covers which file.
	// Absent (nil) means the agent did not record per-file coverage:
	// the per-file gate falls back to the flat list and does not
	// block. Old plans without this field keep validating.
	Coverage map[string][]string `json:"coverage,omitempty"`
}

func (m *ImpactMap) Validate() error {
	if m == nil {
		return nil
	}
	if len(m.ChangedFiles) == 0 || len(m.ChangedFiles) > 64 || len(m.Callers) > 128 || len(m.CoveringTests) > 128 || len(m.Coverage) > 128 {
		return errors.New("task impact map is invalid")
	}
	for _, file := range m.ChangedFiles {
		if !validImpactPath(file) {
			return errors.New("task impact map changed file is invalid")
		}
	}
	if !validPlanTextList(m.Callers, 512) || !validPlanTextList(m.CoveringTests, 256) {
		return errors.New("task impact map callers or covering tests are invalid")
	}
	for file, tests := range m.Coverage {
		if !validImpactPath(file) || len(tests) > 128 || !validPlanTextList(tests, 256) {
			return errors.New("task impact map coverage is invalid")
		}
	}
	return nil
}

func validImpactPath(value string) bool {
	if len(value) == 0 || len(value) > 512 || !planText(value, 1, 512) {
		return false
	}
	if value[0] == '/' || strings.Contains(value, "..") || strings.ContainsRune(value, '\\') {
		return false
	}
	return true
}

// IsComplex reports whether the plan is a complex task: more than one step.
// Only complex tasks must carry a complete brief before dispatch.
func (p Plan) IsComplex() bool {
	return len(p.Steps) > 1
}

// MissingBriefFields names every brief field that is empty, including the
// objective when Plan.Goal is empty. The order is fixed: goal, scope,
// acceptance, verification_method.
func (p Plan) MissingBriefFields() []string {
	var missing []string
	if strings.TrimSpace(p.Goal) == "" {
		missing = append(missing, "goal")
	}
	missing = append(missing, p.Brief.MissingFields()...)
	return missing
}

// BlockingBriefGaps returns the brief fields that block dispatch. It is
// empty for simple tasks: a simple task is never held up for a brief, and no
// user-visible question is ever produced for a gap — the caller returns the
// gap list to the agent as structured information instead.
func (p Plan) BlockingBriefGaps() []string {
	if !p.IsComplex() {
		return nil
	}
	return p.MissingBriefFields()
}
