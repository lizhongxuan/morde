package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
)

const (
	MaxPlanSteps                     = 32
	MaxPlanListItems                 = 8
	MaxPlanVerificationObligations   = 32
	MaxPlanRequirementRefBytes       = 40
	MaxPlanRequirementStatementBytes = 512
	MaxPlanRequirementIDsPerCheck    = 32
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
	// RequirementManifest is the candidate set of explicit verification
	// obligations. Its digest is server-computed; it does not assert completeness.
	RequirementManifest *PlanVerificationRequirementManifest `json:"requirement_manifest,omitempty"`
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

type PlanVerificationScopeMode string

const (
	PlanVerificationScopeFullSuite PlanVerificationScopeMode = "full_suite"
	PlanVerificationScopeTargeted  PlanVerificationScopeMode = "targeted"
)

type PlanVerificationRequirementID string

type PlanVerificationRequirementManifest struct {
	Version     int                          `json:"version"`
	Digest      string                       `json:"digest,omitempty"`
	Obligations []PlanVerificationObligation `json:"obligations"`
}

// PlanVerificationObligation is one candidate explicit verification request.
// Ref only links items within a tool call; the App replaces it with a
// deterministic ID after resolving Source. Neither Ref nor ID proves that the
// candidate manifest contains every user requirement.
type PlanVerificationObligation struct {
	ID            PlanVerificationRequirementID     `json:"id,omitempty"`
	Ref           string                            `json:"ref,omitempty"`
	Statement     string                            `json:"statement"`
	Kind          PlanVerificationCheckKind         `json:"kind"`
	ScopeMode     PlanVerificationScopeMode         `json:"scope_mode"`
	CoveragePaths []string                          `json:"coverage_paths"`
	Selectors     []string                          `json:"selectors,omitempty"`
	Source        PlanVerificationRequirementSource `json:"source"`
}

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
	Kind           PlanVerificationCheckKind       `json:"kind"`
	Basis          PlanVerificationCheckBasis      `json:"basis"`
	Command        string                          `json:"command"`
	ScopeMode      PlanVerificationScopeMode       `json:"scope_mode,omitempty"`
	CoveragePaths  []string                        `json:"coverage_paths"`
	Selectors      []string                        `json:"selectors,omitempty"`
	RequirementIDs []PlanVerificationRequirementID `json:"requirement_ids,omitempty"`
	// RequirementSource is retained for old persisted plans. New candidate
	// manifests keep source refs on obligations and map checks by RequirementIDs.
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
		if step.VerificationCheck != nil && !step.VerificationCheck.valid(p.RequirementManifest != nil) {
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
	if p.RequirementManifest != nil {
		if err := p.validateCanonicalRequirementManifest(); err != nil {
			return err
		}
		if err := p.validateRequirementMappings(); err != nil {
			return err
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
	if p.RequirementManifest != nil {
		return p.validateRequirementManifestInput(input)
	}
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

func (check PlanVerificationCheck) valid(withManifest bool) bool {
	if !validPlanVerificationCheckKind(check.Kind) ||
		(check.Basis != PlanVerificationCheckRequired && check.Basis != PlanVerificationCheckRiskSelected) ||
		!planText(check.Command, 1, 1024) || len(check.CoveragePaths) == 0 || len(check.CoveragePaths) > MaxPlanListItems ||
		!validPlanCoveragePaths(check.CoveragePaths) {
		return false
	}
	if withManifest {
		if check.RequirementSource != nil || !validPlanVerificationScope(check.ScopeMode, check.CoveragePaths, check.Selectors) ||
			len(check.RequirementIDs) > MaxPlanRequirementIDsPerCheck {
			return false
		}
		if check.Basis == PlanVerificationCheckRequired {
			return len(check.RequirementIDs) > 0
		}
		return len(check.RequirementIDs) == 0
	}
	if check.ScopeMode != "" || len(check.Selectors) != 0 || len(check.RequirementIDs) != 0 {
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

func (p Plan) validateRequirementManifestInput(input Plan) error {
	manifest := p.RequirementManifest
	if manifest == nil || manifest.Version != 1 || len(manifest.Obligations) > MaxPlanVerificationObligations || manifest.Digest != "" {
		return errors.New("task plan requirement manifest input is invalid")
	}
	validationManifest := &PlanVerificationRequirementManifest{Version: manifest.Version,
		Obligations: make([]PlanVerificationObligation, 0, len(manifest.Obligations))}
	refs := make(map[string]PlanVerificationRequirementID, len(manifest.Obligations))
	for _, obligation := range manifest.Obligations {
		if obligation.ID != "" || !obligation.validSubmissionInput() {
			return errors.New("task plan requirement obligation input is invalid")
		}
		if _, exists := refs[obligation.Ref]; exists {
			return errors.New("task plan requirement ref is duplicated")
		}
		canonical := obligation
		canonical.Source.Locator = ""
		canonical.Source.HistoryResultToken = ""
		canonical.Source.ContentSHA256 = strings.Repeat("0", 64)
		if canonical.Source.Kind == PlanVerificationRequirementUserMessage && canonical.Source.MessageID == "" {
			canonical.Source.MessageID = "m0000000"
		}
		canonical.Ref = ""
		id, err := canonical.CanonicalID()
		if err != nil {
			return errors.New("task plan requirement obligation cannot be canonicalized")
		}
		canonical.ID = PlanVerificationRequirementID(id)
		refs[obligation.Ref] = canonical.ID
		validationManifest.Obligations = append(validationManifest.Obligations, canonical)
	}
	input.RequirementManifest = validationManifest
	for index := range input.Steps {
		check := input.Steps[index].VerificationCheck
		if check == nil {
			continue
		}
		if check.RequirementSource != nil || len(check.RequirementIDs) > MaxPlanRequirementIDsPerCheck {
			return errors.New("task plan check must reference the requirement manifest")
		}
		checkCopy := *check
		checkCopy.RequirementIDs = make([]PlanVerificationRequirementID, 0, len(check.RequirementIDs))
		seen := make(map[PlanVerificationRequirementID]struct{}, len(check.RequirementIDs))
		for _, ref := range check.RequirementIDs {
			id, exists := refs[string(ref)]
			if !validPlanRequirementRef(string(ref)) || !exists {
				return errors.New("task plan check references an unknown requirement ref")
			}
			if _, exists := seen[id]; exists {
				return errors.New("task plan check requirement ref is duplicated")
			}
			seen[id] = struct{}{}
			checkCopy.RequirementIDs = append(checkCopy.RequirementIDs, id)
		}
		sort.Slice(checkCopy.RequirementIDs, func(i, j int) bool { return checkCopy.RequirementIDs[i] < checkCopy.RequirementIDs[j] })
		input.Steps[index].VerificationCheck = &checkCopy
	}
	digest, err := input.RequirementManifestDigest()
	if err != nil {
		return err
	}
	input.RequirementManifest.Digest = digest
	return input.Validate()
}

func (obligation PlanVerificationObligation) validSubmissionInput() bool {
	return validPlanRequirementRef(obligation.Ref) && planText(obligation.Statement, 1, MaxPlanRequirementStatementBytes) &&
		validPlanVerificationCheckKind(obligation.Kind) &&
		validPlanVerificationScope(obligation.ScopeMode, obligation.CoveragePaths, obligation.Selectors) &&
		obligation.Source.ContentSHA256 == "" && obligation.Source.validSubmissionInput()
}

func (obligation PlanVerificationObligation) CanonicalID() (string, error) {
	if !planText(obligation.Statement, 1, MaxPlanRequirementStatementBytes) || !validPlanVerificationCheckKind(obligation.Kind) ||
		!validPlanVerificationScope(obligation.ScopeMode, obligation.CoveragePaths, obligation.Selectors) || !obligation.Source.valid() {
		return "", errors.New("task plan requirement obligation is not canonical")
	}
	payload := struct {
		Version       int                               `json:"version"`
		Statement     string                            `json:"statement"`
		Kind          PlanVerificationCheckKind         `json:"kind"`
		ScopeMode     PlanVerificationScopeMode         `json:"scope_mode"`
		CoveragePaths []string                          `json:"coverage_paths"`
		Selectors     []string                          `json:"selectors"`
		Source        PlanVerificationRequirementSource `json:"source"`
	}{Version: 1, Statement: obligation.Statement, Kind: obligation.Kind, ScopeMode: obligation.ScopeMode,
		CoveragePaths: canonicalPlanStrings(obligation.CoveragePaths), Selectors: canonicalPlanStrings(obligation.Selectors), Source: obligation.Source}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "rq_" + hex.EncodeToString(digest[:]), nil
}

// RequirementManifestDigest returns the lowercase SHA-256 digest of the
// canonical candidate manifest. It binds manifest version, TaskRevision,
// Plan.Revision, each canonical obligation ID, its statement/type/scope, and
// canonical source identity. It excludes the stored Digest field, local Ref,
// check mapping, TaskID, owner, and authorization context. Callers bind those
// outer identities in their event/confirmation records.
func (p Plan) RequirementManifestDigest() (string, error) {
	manifest := p.RequirementManifest
	if manifest == nil || manifest.Version != 1 || p.TaskRevision < 0 || p.Revision < 1 ||
		len(manifest.Obligations) > MaxPlanVerificationObligations {
		return "", errors.New("task plan requirement manifest is unavailable or invalid")
	}
	obligations := make([]planRequirementManifestDigestObligation, 0, len(manifest.Obligations))
	seen := make(map[PlanVerificationRequirementID]struct{}, len(manifest.Obligations))
	for _, obligation := range manifest.Obligations {
		if obligation.Ref != "" {
			return "", errors.New("task plan requirement ref is not canonical")
		}
		id, err := obligation.CanonicalID()
		if err != nil || string(obligation.ID) != id {
			return "", errors.New("task plan requirement ID is not canonical")
		}
		if _, exists := seen[obligation.ID]; exists {
			return "", errors.New("task plan requirement ID is duplicated")
		}
		seen[obligation.ID] = struct{}{}
		obligations = append(obligations, planRequirementManifestDigestObligation{ID: obligation.ID,
			Statement: obligation.Statement, Kind: obligation.Kind, ScopeMode: obligation.ScopeMode,
			CoveragePaths: canonicalPlanStrings(obligation.CoveragePaths), Selectors: canonicalPlanStrings(obligation.Selectors),
			Source: obligation.Source})
	}
	sort.Slice(obligations, func(i, j int) bool { return obligations[i].ID < obligations[j].ID })
	payload := planRequirementManifestDigestPayload{Version: manifest.Version, TaskRevision: p.TaskRevision,
		PlanRevision: p.Revision, Obligations: obligations}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

type planRequirementManifestDigestPayload struct {
	Version      int                                       `json:"version"`
	TaskRevision int64                                     `json:"task_revision"`
	PlanRevision int64                                     `json:"plan_revision"`
	Obligations  []planRequirementManifestDigestObligation `json:"obligations"`
}

type planRequirementManifestDigestObligation struct {
	ID            PlanVerificationRequirementID     `json:"id"`
	Statement     string                            `json:"statement"`
	Kind          PlanVerificationCheckKind         `json:"kind"`
	ScopeMode     PlanVerificationScopeMode         `json:"scope_mode"`
	CoveragePaths []string                          `json:"coverage_paths"`
	Selectors     []string                          `json:"selectors"`
	Source        PlanVerificationRequirementSource `json:"source"`
}

func (p Plan) validateCanonicalRequirementManifest() error {
	manifest := p.RequirementManifest
	if manifest == nil || manifest.Version != 1 || len(manifest.Obligations) > MaxPlanVerificationObligations ||
		!validPlanSHA256(manifest.Digest) {
		return errors.New("task plan requirement manifest is invalid")
	}
	digest, err := p.RequirementManifestDigest()
	if err != nil || digest != manifest.Digest {
		return errors.New("task plan requirement manifest digest does not match")
	}
	return nil
}

func (p Plan) validateRequirementMappings() error {
	manifest := p.RequirementManifest
	if manifest == nil {
		return nil
	}
	obligations := make(map[PlanVerificationRequirementID]PlanVerificationObligation, len(manifest.Obligations))
	for _, obligation := range manifest.Obligations {
		obligations[obligation.ID] = obligation
	}
	mapped := make(map[PlanVerificationRequirementID]string, len(obligations))
	for _, step := range p.Steps {
		check := step.VerificationCheck
		if check == nil {
			continue
		}
		for _, id := range check.RequirementIDs {
			obligation, exists := obligations[id]
			if !exists {
				return errors.New("task plan check references an unknown requirement ID")
			}
			if prior, exists := mapped[id]; exists {
				return errors.New("task plan requirement is mapped by multiple checks: " + prior)
			}
			if check.Kind != obligation.Kind || check.ScopeMode != obligation.ScopeMode ||
				!samePlanStringSet(check.CoveragePaths, obligation.CoveragePaths) || !samePlanStringSet(check.Selectors, obligation.Selectors) {
				return errors.New("task plan check does not match its requirement type or scope")
			}
			mapped[id] = step.ID
		}
	}
	if len(mapped) != len(obligations) {
		return errors.New("task plan has an unmapped requirement obligation")
	}
	return nil
}

func validPlanVerificationScope(mode PlanVerificationScopeMode, paths, selectors []string) bool {
	if len(paths) == 0 || len(paths) > MaxPlanListItems || !validPlanCoveragePaths(paths) ||
		len(selectors) > MaxPlanListItems || !validPlanTextList(selectors, 256) {
		return false
	}
	seen := make(map[string]struct{}, len(selectors))
	for _, selector := range selectors {
		if _, exists := seen[selector]; exists {
			return false
		}
		seen[selector] = struct{}{}
	}
	switch mode {
	case PlanVerificationScopeFullSuite:
		return len(selectors) == 0
	case PlanVerificationScopeTargeted:
		if len(paths) == 1 && paths[0] == "." && len(selectors) == 0 {
			return false
		}
		return true
	default:
		return false
	}
}

func validPlanRequirementRef(value string) bool {
	if len(value) == 0 || len(value) > MaxPlanRequirementRefBytes || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func canonicalPlanStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	if result == nil {
		return []string{}
	}
	return result
}

func samePlanStringSet(left, right []string) bool {
	left = canonicalPlanStrings(left)
	right = canonicalPlanStrings(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
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
