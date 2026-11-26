package task

import (
	"encoding/hex"
	"errors"
	"path"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
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
	TaskRevision       int64      `json:"task_revision"`
	Revision           int64      `json:"revision"`
	Goal               string     `json:"goal"`
	Scope              string     `json:"scope,omitempty"`
	VerificationMethod string     `json:"verification_method,omitempty"`
	CostEstimateState  string     `json:"cost_estimate_state"`
	Steps              []PlanStep `json:"steps"`
	// FixPlan is optional: when present it marks the task as a bug fix and
	// carries the structured fix proposal. Plans stored before E03 omit it.
	FixPlan *FixPlan `json:"fix_plan,omitempty"`
}

type PlanStep struct {
	ID                 string         `json:"id"`
	Title              string         `json:"title"`
	Description        string         `json:"description"`
	DependsOn          []string       `json:"depends_on"`
	Outputs            []string       `json:"outputs"`
	CompletionCriteria []string       `json:"completion_criteria"`
	Status             PlanStepStatus `json:"status"`
	// VerificationApplicability identifies whether this step declares an
	// H07 check or has no step-specific check. The empty value is retained for
	// legacy plans and is treated as unknown by validation review.
	VerificationApplicability PlanVerificationApplicability `json:"verification_applicability,omitempty"`
	// VerificationCheck is optional for compatibility with plans saved before
	// H07 check identities were introduced. A missing value means unknown to
	// verification; it does not mean that no checks apply.
	VerificationCheck *PlanVerificationCheck `json:"verification_check,omitempty"`
}

type PlanVerificationApplicability string

const (
	PlanVerificationApplicabilityCheck         PlanVerificationApplicability = "check"
	PlanVerificationApplicabilityNotApplicable PlanVerificationApplicability = "not_applicable"
)

type PlanVerificationCheckKind string

const (
	PlanVerificationCheckTest      PlanVerificationCheckKind = "test"
	PlanVerificationCheckBuild     PlanVerificationCheckKind = "build"
	PlanVerificationCheckLint      PlanVerificationCheckKind = "lint"
	PlanVerificationCheckTypecheck PlanVerificationCheckKind = "typecheck"
	PlanVerificationCheckOther     PlanVerificationCheckKind = "other"
)

type PlanVerificationCheckBasis string

const (
	// PlanVerificationCheckRequired means a user or project instruction
	// explicitly requires this check and must be identified by a source ref.
	PlanVerificationCheckRequired PlanVerificationCheckBasis = "required"
	// PlanVerificationCheckRiskSelected means the Agent selected this check
	// for the task's risk profile; it does not claim the user/project required it.
	PlanVerificationCheckRiskSelected PlanVerificationCheckBasis = "risk_selected"
)

type PlanVerificationRequirementSourceKind string

const (
	PlanVerificationRequirementUserMessage PlanVerificationRequirementSourceKind = "user_message"
	PlanVerificationRequirementProjectFile PlanVerificationRequirementSourceKind = "project_file"
)

const (
	PlanVerificationRequirementCurrentTaskInputLocator     = "current_task_input"
	PlanVerificationRequirementCurrentTaskTurnInputLocator = "current_task_turn_input"
	PlanVerificationRequirementHistorySearchResultLocator  = "history_search_result"
	PlanVerificationRequirementHistoryResultTokenMaxBytes  = 2048
)

// PlanVerificationRequirementSource identifies the source that makes a
// verification check mandatory. The server must independently resolve the
// referenced message or file and verify its digest before treating a required
// check as applicable. A history result token is accepted only at the tool
// boundary and is never part of a persisted canonical source.
type PlanVerificationRequirementSource struct {
	Kind PlanVerificationRequirementSourceKind `json:"kind"`
	// Locator is accepted only on a new plan-tool submission. The Store resolves
	// it to an immutable message ID before the plan is validated or persisted.
	Locator string `json:"locator,omitempty"`
	// HistoryResultToken proves that a later user message came from this Task's
	// authorized, watermark-bounded history-search result. It is tool-input only
	// and must be cleared after App verification, before a plan is persisted.
	HistoryResultToken string `json:"history_result_token,omitempty"`
	MessageID          string `json:"message_id,omitempty"`
	WorkspacePath      string `json:"workspace_path,omitempty"`
	WorkspaceRevision  *int64 `json:"workspace_revision,omitempty"`
	ContentSHA256      string `json:"content_sha256,omitempty"`
}

// PlanVerificationCheck describes one check whose execution may contribute
// evidence to H07. Its identity is the containing PlanStep.ID, which is
// already carried through the durable plan-step execution binding.
type PlanVerificationCheck struct {
	Kind              PlanVerificationCheckKind          `json:"kind"`
	Basis             PlanVerificationCheckBasis         `json:"basis"`
	Command           string                             `json:"command"`
	CoveragePaths     []string                           `json:"coverage_paths"`
	RequirementSource *PlanVerificationRequirementSource `json:"requirement_source,omitempty"`
}

func (p Plan) Validate() error {
	if p.TaskRevision < 0 || p.Revision < 1 || !planText(p.Goal, 1, 2048) || p.CostEstimateState != "unknown" || len(p.Steps) == 0 || len(p.Steps) > MaxPlanSteps {
		return errors.New("task plan fields are invalid")
	}
	// Scope and verification method are optional so plans stored before the
	// brief gate stay readable; when present they must satisfy the brief bounds.
	if (p.Scope != "" && !planText(p.Scope, 1, 2048)) || (p.VerificationMethod != "" && !planText(p.VerificationMethod, 1, 1024)) {
		return errors.New("task plan fields are invalid")
	}
	// A fix plan is optional so plans stored before E03 stay readable; when
	// present it must satisfy its own boundaries.
	if p.FixPlan != nil {
		if err := p.FixPlan.Validate(); err != nil {
			return err
		}
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
		switch step.VerificationApplicability {
		case "":
			// Historical plans omit applicability. Keep them readable, but do
			// not infer that no verification applies.
		case PlanVerificationApplicabilityCheck:
			if step.VerificationCheck == nil {
				return errors.New("task plan verification applicability requires a check")
			}
		case PlanVerificationApplicabilityNotApplicable:
			if step.VerificationCheck != nil {
				return errors.New("task plan not-applicable step cannot carry a check")
			}
		default:
			return errors.New("task plan verification applicability is invalid")
		}
		if step.VerificationCheck != nil && !step.VerificationCheck.valid() {
			return errors.New("task plan verification check is invalid")
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

// ValidateSubmission applies the stricter contract for newly submitted
// plans while Validate keeps historical persisted plans readable. Every
// step of a new fix plan must classify its verification applicability so an
// unchecked subset cannot be mistaken for a complete check list.
func (p Plan) ValidateSubmission() error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.FixPlan == nil {
		return nil
	}
	for _, step := range p.Steps {
		if step.VerificationApplicability != PlanVerificationApplicabilityCheck &&
			step.VerificationApplicability != PlanVerificationApplicabilityNotApplicable {
			return errors.New("fix plan step verification applicability is required")
		}
	}
	return nil
}

// ValidateSubmissionInput validates source locators accepted at the DSH tool
// boundary. It allows server-resolved locators to omit their digest because
// the App and Store resolve and hash the source before ValidateSubmission is
// called. It never makes an unresolved locator valid as a persisted Plan.
func (p Plan) ValidateSubmissionInput() error {
	input := p
	input.Steps = append([]PlanStep(nil), p.Steps...)
	for index := range input.Steps {
		check := input.Steps[index].VerificationCheck
		if check == nil || check.RequirementSource == nil {
			continue
		}
		source := *check.RequirementSource
		if !source.validSubmissionInput() {
			return errors.New("task plan verification requirement source locator is invalid")
		}
		source.Locator = ""
		source.HistoryResultToken = ""
		if source.Kind == PlanVerificationRequirementUserMessage && source.MessageID == "" {
			// This placeholder is confined to the validation copy. The submitted
			// plan still has no message ID until the Store resolves its locator.
			source.MessageID = "m0000000"
		}
		if source.ContentSHA256 == "" {
			source.ContentSHA256 = strings.Repeat("0", 64)
		}
		checkCopy := *check
		checkCopy.RequirementSource = &source
		input.Steps[index].VerificationCheck = &checkCopy
	}
	return input.Validate()
}

func (check PlanVerificationCheck) valid() bool {
	if !validPlanVerificationCheckKind(check.Kind) ||
		(check.Basis != PlanVerificationCheckRequired && check.Basis != PlanVerificationCheckRiskSelected) ||
		!planText(check.Command, 1, 1024) || len(check.CoveragePaths) == 0 || len(check.CoveragePaths) > MaxPlanListItems ||
		!validPlanCoveragePaths(check.CoveragePaths) {
		return false
	}
	if check.Basis == PlanVerificationCheckRequired {
		return check.RequirementSource != nil && check.RequirementSource.valid()
	}
	return check.RequirementSource == nil
}

func validPlanVerificationCheckKind(value PlanVerificationCheckKind) bool {
	switch value {
	case PlanVerificationCheckTest, PlanVerificationCheckBuild, PlanVerificationCheckLint,
		PlanVerificationCheckTypecheck, PlanVerificationCheckOther:
		return true
	default:
		return false
	}
}

func (source PlanVerificationRequirementSource) valid() bool {
	if source.Locator != "" || source.HistoryResultToken != "" || !validPlanSHA256(source.ContentSHA256) {
		return false
	}
	switch source.Kind {
	case PlanVerificationRequirementUserMessage:
		return domain.ValidMessageID(source.MessageID) && source.WorkspacePath == "" && source.WorkspaceRevision == nil
	case PlanVerificationRequirementProjectFile:
		return source.MessageID == "" && validPlanRelativePath(source.WorkspacePath, false) &&
			source.WorkspaceRevision != nil && *source.WorkspaceRevision >= 0
	default:
		return false
	}
}

func (source PlanVerificationRequirementSource) validSubmissionInput() bool {
	if source.ContentSHA256 != "" && !validPlanSHA256(source.ContentSHA256) {
		return false
	}
	switch source.Kind {
	case PlanVerificationRequirementUserMessage:
		if source.WorkspacePath != "" || source.WorkspaceRevision != nil || source.ContentSHA256 != "" {
			return false
		}
		if source.Locator == PlanVerificationRequirementCurrentTaskTurnInputLocator {
			return source.MessageID == "" && source.HistoryResultToken == ""
		}
		if source.Locator == PlanVerificationRequirementCurrentTaskInputLocator {
			return source.MessageID == "" && source.HistoryResultToken == ""
		}
		if source.Locator == PlanVerificationRequirementHistorySearchResultLocator {
			return source.MessageID == "" && planText(source.HistoryResultToken, 1, PlanVerificationRequirementHistoryResultTokenMaxBytes)
		}
		// New submissions never accept model-supplied raw message IDs. Initial
		// and current-turn locators are bound by the server; historical messages
		// must carry a signed token issued with an authorized search result.
		return false
	case PlanVerificationRequirementProjectFile:
		return source.Locator == "" && source.HistoryResultToken == "" && source.MessageID == "" &&
			validPlanRelativePath(source.WorkspacePath, false) && source.WorkspaceRevision != nil && *source.WorkspaceRevision >= 0
	default:
		return false
	}
}

func validPlanCoveragePaths(paths []string) bool {
	seen := make(map[string]struct{}, len(paths))
	for _, value := range paths {
		if !validPlanRelativePath(value, true) {
			return false
		}
		if _, exists := seen[value]; exists {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

// validPlanRelativePath accepts normalized, workspace-relative paths only.
// A coverage scope may use "." to mean the whole workspace; a source file
// reference must name a specific file or directory.
func validPlanRelativePath(value string, allowWorkspaceRoot bool) bool {
	if !planText(value, 1, 512) || strings.ContainsRune(value, '\\') || strings.HasPrefix(value, "/") ||
		strings.ContainsAny(value, "\n\r\t:*?[]{}") || path.Clean(value) != value {
		return false
	}
	if value == "." {
		return allowWorkspaceRoot
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func validPlanSHA256(value string) bool {
	if len(value) != hex.EncodedLen(32) || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
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
