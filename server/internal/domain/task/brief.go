package task

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// MaxBriefAcceptanceCriteria bounds the task-level acceptance list derived
	// from plan step completion criteria.
	MaxBriefAcceptanceCriteria = 8
	// MaxBriefFixPlanItems bounds each list of a structured fix plan.
	MaxBriefFixPlanItems = 8
	// MaxRootCauseCodeLines bounds the code lines cited as root-cause evidence.
	MaxRootCauseCodeLines = 8
	// MaxRootCauseReproOutputRunes bounds the failing reproduction output
	// carried as root-cause evidence.
	MaxRootCauseReproOutputRunes = 4096
)

// Brief is the four-piece planner brief for a complex task: goal, scope,
// acceptance criteria and verification method. It is derived from a submitted
// Plan, never from a model-filled step status: acceptance criteria are the
// deduplicated union of the plan steps' completion criteria.
type Brief struct {
	Goal               string   `json:"goal"`
	Scope              string   `json:"scope"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	VerificationMethod string   `json:"verification_method"`
	// FixPlan is optional: a structured fix proposal carried with the brief.
	FixPlan *FixPlan `json:"fix_plan,omitempty"`
}

// FixPlan is the structured form of a fix proposal: the minimal change set,
// its risks and the alternatives considered.
type FixPlan struct {
	ChangedFiles []string `json:"changed_files"`
	Risks        []string `json:"risks"`
	Alternatives []string `json:"alternatives"`
	// RootCause is optional: when present it states the located root cause
	// together with the evidence that backs it, so a fix conclusion can be
	// opened and checked instead of taken on the agent's word. Plans stored
	// before E04 omit it.
	RootCause *RootCause `json:"root_cause,omitempty"`
}

// RootCause is a located root-cause conclusion with its evidence: the
// failing reproduction output and the code lines the conclusion points at.
// Either piece missing means the conclusion is unevidenced and the whole
// root cause is rejected.
type RootCause struct {
	Summary     string   `json:"summary"`
	ReproOutput string   `json:"repro_output"`
	CodeLines   []string `json:"code_lines"`
}

// Validate checks the root-cause evidence boundaries. All three pieces are
// required when a root cause is present.
func (r RootCause) Validate() error {
	if !planText(r.Summary, 1, 1024) {
		return errors.New("root cause summary is invalid")
	}
	if !utf8.ValidString(r.ReproOutput) || strings.ContainsRune(r.ReproOutput, '\x00') ||
		utf8.RuneCountInString(strings.TrimSpace(r.ReproOutput)) < 1 ||
		utf8.RuneCountInString(r.ReproOutput) > MaxRootCauseReproOutputRunes {
		return errors.New("root cause repro output is invalid")
	}
	if len(r.CodeLines) < 1 || len(r.CodeLines) > MaxRootCauseCodeLines {
		return errors.New("root cause code lines are invalid")
	}
	for _, line := range r.CodeLines {
		if !validRootCauseCodeLine(line) {
			return errors.New("root cause code line is invalid")
		}
	}
	return nil
}

// validRootCauseCodeLine accepts a workspace-relative "path:line" reference:
// a relative path without a parent-directory escape and a 1-based line
// number. Absolute paths, escapes and bare paths without a line are not
// evidence a reader can open.
func validRootCauseCodeLine(value string) bool {
	if len(value) == 0 || len(value) > 512 || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return false
	}
	separator := strings.LastIndex(value, ":")
	if separator <= 0 || separator == len(value)-1 {
		return false
	}
	path, lineText := value[:separator], value[separator+1:]
	if strings.HasPrefix(path, "/") {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." {
			return false
		}
	}
	if len(lineText) > 10 {
		return false
	}
	line, err := strconv.Atoi(lineText)
	return err == nil && line >= 1
}

// Validate checks the brief boundaries. All four brief pieces are required;
// a FixPlan, when present, must satisfy its own boundaries.
func (b Brief) Validate() error {
	if !planText(b.Goal, 1, 2048) {
		return errors.New("brief goal is invalid")
	}
	if !planText(b.Scope, 1, 2048) {
		return errors.New("brief scope is invalid")
	}
	if len(b.AcceptanceCriteria) < 1 || len(b.AcceptanceCriteria) > MaxBriefAcceptanceCriteria ||
		!validPlanTextList(b.AcceptanceCriteria, 512) {
		return errors.New("brief acceptance criteria are invalid")
	}
	if !planText(b.VerificationMethod, 1, 1024) {
		return errors.New("brief verification method is invalid")
	}
	if b.FixPlan != nil {
		if err := b.FixPlan.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks the fix plan boundaries. Empty lists are allowed: a fix
// may touch no file, carry no known risk or have no alternative. A root
// cause, when present, must carry its full evidence.
func (f FixPlan) Validate() error {
	if len(f.ChangedFiles) > MaxBriefFixPlanItems || !validPlanTextList(f.ChangedFiles, 512) ||
		len(f.Risks) > MaxBriefFixPlanItems || !validPlanTextList(f.Risks, 512) ||
		len(f.Alternatives) > MaxBriefFixPlanItems || !validPlanTextList(f.Alternatives, 512) {
		return errors.New("fix plan is invalid")
	}
	if f.RootCause != nil {
		if err := f.RootCause.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// BriefFromPlan derives the brief of a submitted plan. It names the missing
// piece when the plan cannot yield a complete brief.
func BriefFromPlan(plan Plan) (Brief, error) {
	if plan.Scope == "" {
		return Brief{}, errors.New("brief is missing scope")
	}
	if plan.VerificationMethod == "" {
		return Brief{}, errors.New("brief is missing verification_method")
	}
	seen := make(map[string]struct{})
	criteria := make([]string, 0, MaxBriefAcceptanceCriteria)
	for _, step := range plan.Steps {
		for _, criterion := range step.CompletionCriteria {
			if _, exists := seen[criterion]; exists {
				continue
			}
			seen[criterion] = struct{}{}
			criteria = append(criteria, criterion)
		}
	}
	if len(criteria) == 0 {
		return Brief{}, errors.New("brief is missing acceptance_criteria")
	}
	brief := Brief{Goal: plan.Goal, Scope: plan.Scope, AcceptanceCriteria: criteria, VerificationMethod: plan.VerificationMethod,
		FixPlan: plan.FixPlan}
	if err := brief.Validate(); err != nil {
		return Brief{}, fmt.Errorf("brief from plan is invalid: %w", err)
	}
	return brief, nil
}
