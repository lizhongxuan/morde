package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"morde/server/internal/domain"
)

type TaskHardCheckID string

const (
	TaskHardCheckConclusionFirst TaskHardCheckID = "H01"
	TaskHardCheckEvidence        TaskHardCheckID = "H02"
	TaskHardCheckIndependent     TaskHardCheckID = "H03"
	TaskHardCheckClarification   TaskHardCheckID = "H04"
	TaskHardCheckApproval        TaskHardCheckID = "H05"
	TaskHardCheckCorrection      TaskHardCheckID = "H06"
	TaskHardCheckReplay          TaskHardCheckID = "H07"
)

type TaskHardCheckOutcome string

const (
	TaskHardCheckPass          TaskHardCheckOutcome = "PASS"
	TaskHardCheckFail          TaskHardCheckOutcome = "FAIL"
	TaskHardCheckNotApplicable TaskHardCheckOutcome = "N/A"
)

type TaskValidationState string

const (
	TaskValidationPending   TaskValidationState = "pending"
	TaskValidationPass      TaskValidationState = "PASS"
	TaskValidationPassNotes TaskValidationState = "PASS+NOTES"
	TaskValidationFail      TaskValidationState = "FAIL"
)

type TaskValidationSummary struct {
	State TaskValidationState `json:"state"`
}

func ValidTaskValidationState(value TaskValidationState) bool {
	switch value {
	case TaskValidationPending, TaskValidationPass, TaskValidationPassNotes, TaskValidationFail:
		return true
	default:
		return false
	}
}

type TaskHardCheckNotApplicableReason string

const (
	TaskHardCheckNoTaskExecution      TaskHardCheckNotApplicableReason = "no_task_execution"
	TaskHardCheckNoIrreversibleAction TaskHardCheckNotApplicableReason = "no_irreversible_action"
	TaskHardCheckNoRepeatedError      TaskHardCheckNotApplicableReason = "no_repeated_error"
)

type TaskValidationEvidenceKind string

const (
	TaskValidationEvidenceSource      TaskValidationEvidenceKind = "source"
	TaskValidationEvidenceBaseline    TaskValidationEvidenceKind = "baseline"
	TaskValidationEvidenceAction      TaskValidationEvidenceKind = "action"
	TaskValidationEvidenceResult      TaskValidationEvidenceKind = "result"
	TaskValidationEvidenceSideEffect  TaskValidationEvidenceKind = "side_effect"
	TaskValidationEvidenceBeforeAfter TaskValidationEvidenceKind = "before_after"
	TaskValidationEvidenceReview      TaskValidationEvidenceKind = "review"
)

// TaskValidationEvidenceRef is a claimed opaque reference to a Task record.
// It contains no evidence body or user text.
type TaskValidationEvidenceRef struct {
	Kind   TaskValidationEvidenceKind `json:"kind"`
	ID     string                     `json:"id,omitempty"`
	CallID string                     `json:"call_id,omitempty"`
}

type TaskResultClaim struct {
	ID           string                      `json:"claim_id"`
	StartByte    int32                       `json:"start_byte"`
	EndByte      int32                       `json:"end_byte"`
	EvidenceRefs []TaskValidationEvidenceRef `json:"evidence_refs"`
	// shortcut: one measurement pair per claim; split multi-metric results until a reviewer needs grouped comparisons.
	PerformanceComparison *TaskResultPerformanceComparison `json:"performance_comparison,omitempty"`
}

// TaskResultPerformanceComparison is an untrusted measurement claim whose
// inputs must resolve to same-revision Task Steps before it is persisted.
type TaskResultPerformanceComparison struct {
	Metric         string  `json:"metric"`
	Unit           string  `json:"unit"`
	Before         float64 `json:"before"`
	After          float64 `json:"after"`
	Environment    string  `json:"environment"`
	BaselineCallID string  `json:"baseline_call_id"`
	AfterCallID    string  `json:"after_call_id"`
}

type TaskValidationPerformanceComparison struct {
	Metric      string                          `json:"metric"`
	Unit        string                          `json:"unit"`
	Before      float64                         `json:"before"`
	After       float64                         `json:"after"`
	Delta       float64                         `json:"delta"`
	Environment string                          `json:"environment"`
	BaselineRef TaskValidationEvidenceRefRecord `json:"baseline_ref"`
	AfterRef    TaskValidationEvidenceRefRecord `json:"after_ref"`
}

type TaskValidationEvidenceRefRecord struct {
	Kind         TaskValidationEvidenceKind `json:"kind"`
	ID           string                     `json:"id"`
	TaskRevision int64                      `json:"task_revision"`
	Sequence     int64                      `json:"sequence"`
	CallID       string                     `json:"call_id,omitempty"`
}

type TaskValidationEvidenceClaimRecord struct {
	ID                    string                               `json:"claim_id"`
	StartByte             int32                                `json:"start_byte"`
	EndByte               int32                                `json:"end_byte"`
	EvidenceRefs          []TaskValidationEvidenceRefRecord    `json:"evidence_refs"`
	PerformanceComparison *TaskValidationPerformanceComparison `json:"performance_comparison,omitempty"`
	SourceLocationState   string                               `json:"source_location_state,omitempty"`
	SourceLocationIDs     []string                             `json:"source_location_ids,omitempty"`
}

const (
	TaskValidationSourceLocationsMapped      = "mapped"
	TaskValidationSourceLocationsPartial     = "partial"
	TaskValidationSourceLocationsUnavailable = "unavailable"
	TaskValidationSourceLocationsRestricted  = "restricted"
	TaskValidationSourceLocationsNone        = "no_locator"
	TaskValidationSourceLocationsTruncated   = "truncated"
)

type TaskValidationEvidenceRecord struct {
	TaskRevision    int64                               `json:"task_revision"`
	EventSequence   int64                               `json:"event_sequence"`
	ResultSHA256    string                              `json:"result_sha256"`
	Claims          []TaskValidationEvidenceClaimRecord `json:"claims"`
	SourceLocations []TaskStepSourceLocation            `json:"source_locations,omitempty"`
}

type TaskHardCheckClaim struct {
	ID                  TaskHardCheckID                  `json:"id"`
	Outcome             TaskHardCheckOutcome             `json:"outcome"`
	NotApplicableReason TaskHardCheckNotApplicableReason `json:"not_applicable_reason,omitempty"`
	EvidenceRefs        []TaskValidationEvidenceRef      `json:"evidence_refs"`
	HasNonBlockingNotes bool                             `json:"has_non_blocking_notes"`
}

type TaskValidationIssue string

const (
	TaskValidationIssueMissingCheck                  TaskValidationIssue = "missing_check"
	TaskValidationIssueTooManyChecks                 TaskValidationIssue = "too_many_checks"
	TaskValidationIssueDuplicateCheck                TaskValidationIssue = "duplicate_check"
	TaskValidationIssueUnknownCheck                  TaskValidationIssue = "unknown_check"
	TaskValidationIssueInvalidOutcome                TaskValidationIssue = "invalid_outcome"
	TaskValidationIssueMissingNotApplicableReason    TaskValidationIssue = "missing_na_reason"
	TaskValidationIssueInvalidNotApplicableReason    TaskValidationIssue = "invalid_na_reason"
	TaskValidationIssueUnexpectedNotApplicableReason TaskValidationIssue = "unexpected_na_reason"
	TaskValidationIssueMissingEvidence               TaskValidationIssue = "missing_evidence"
	TaskValidationIssueInvalidEvidenceReference      TaskValidationIssue = "invalid_evidence_reference"
	TaskValidationIssueDuplicateEvidenceReference    TaskValidationIssue = "duplicate_evidence_reference"
)

// TaskHardCheckClaimResult contains untrusted assertions, not verified outcomes.
type TaskHardCheckClaimResult struct {
	ID                  TaskHardCheckID                  `json:"id"`
	Outcome             TaskHardCheckOutcome             `json:"outcome"`
	NotApplicableReason TaskHardCheckNotApplicableReason `json:"not_applicable_reason,omitempty"`
	EvidenceRefs        []TaskValidationEvidenceRef      `json:"evidence_refs"`
	HasNonBlockingNotes bool                             `json:"has_non_blocking_notes"`
}

type TaskHardCheckShapeValidation struct {
	ValidShape bool
	Checks     []TaskHardCheckClaimResult
	Issues     []TaskValidationIssue
}

var taskHardCheckOrder = [...]TaskHardCheckID{
	TaskHardCheckConclusionFirst,
	TaskHardCheckEvidence,
	TaskHardCheckIndependent,
	TaskHardCheckClarification,
	TaskHardCheckApproval,
	TaskHardCheckCorrection,
	TaskHardCheckReplay,
}

var taskValidationIssueOrder = [...]TaskValidationIssue{
	TaskValidationIssueMissingCheck,
	TaskValidationIssueTooManyChecks,
	TaskValidationIssueDuplicateCheck,
	TaskValidationIssueUnknownCheck,
	TaskValidationIssueInvalidOutcome,
	TaskValidationIssueMissingNotApplicableReason,
	TaskValidationIssueInvalidNotApplicableReason,
	TaskValidationIssueUnexpectedNotApplicableReason,
	TaskValidationIssueMissingEvidence,
	TaskValidationIssueInvalidEvidenceReference,
	TaskValidationIssueDuplicateEvidenceReference,
}

// shortcut: cap input at the seven defined checks and 64 refs per check; raise only with a versioned review contract.
const maxTaskValidationEvidenceRefs = 64
const maxTaskValidationEvidenceIDBytes = 128
const maxTaskResultClaims = 64
const maxTaskResultEvidenceRefs = 64
const maxTaskResultEvidenceRefsTotal = 256
const MaxTaskValidationEvidenceBytes = 16 << 10

func ValidateTaskResultClaims(result string, claims []TaskResultClaim) error {
	invalid := func() error {
		return domain.InvalidArgument("validation_evidence", "task result evidence claims are invalid")
	}
	if len(claims) == 0 || len(claims) > maxTaskResultClaims || len(result) == 0 || len(result) > MaxAssistantMessageBytes ||
		!utf8.ValidString(result) || strings.ContainsRune(result, '\x00') {
		return invalid()
	}
	seenClaims := make(map[string]struct{}, len(claims))
	totalRefs := 0
	for _, claim := range claims {
		if !validTaskResultClaimID(claim.ID) || claim.StartByte < 0 || claim.EndByte <= claim.StartByte || int(claim.EndByte) > len(result) ||
			!utf8.ValidString(result[:claim.StartByte]) || !utf8.ValidString(result[:claim.EndByte]) ||
			len(claim.EvidenceRefs) == 0 || len(claim.EvidenceRefs) > maxTaskResultEvidenceRefs {
			return invalid()
		}
		if _, duplicate := seenClaims[claim.ID]; duplicate {
			return invalid()
		}
		seenClaims[claim.ID] = struct{}{}
		if !validTaskResultPerformanceComparison(claim.PerformanceComparison) {
			return invalid()
		}
		seenRefs := make(map[TaskValidationEvidenceKind]map[string]struct{}, len(claim.EvidenceRefs))
		for _, ref := range claim.EvidenceRefs {
			if !validTaskResultEvidenceRef(ref) {
				return invalid()
			}
			if seenRefs[ref.Kind] == nil {
				seenRefs[ref.Kind] = make(map[string]struct{})
			}
			key := "id:" + ref.ID
			if ref.CallID != "" {
				key = "call:" + ref.CallID
			}
			if _, duplicate := seenRefs[ref.Kind][key]; duplicate {
				return invalid()
			}
			seenRefs[ref.Kind][key] = struct{}{}
			totalRefs++
			if totalRefs > maxTaskResultEvidenceRefsTotal {
				return invalid()
			}
		}
		if comparison := claim.PerformanceComparison; comparison != nil {
			for _, ref := range []TaskValidationEvidenceRef{
				{Kind: TaskValidationEvidenceBaseline, CallID: comparison.BaselineCallID},
				{Kind: TaskValidationEvidenceBeforeAfter, CallID: comparison.AfterCallID},
			} {
				key := "call:" + ref.CallID
				if _, duplicate := seenRefs[ref.Kind][key]; duplicate {
					return invalid()
				}
			}
			totalRefs += 2
			if totalRefs > maxTaskResultEvidenceRefsTotal {
				return invalid()
			}
		}
	}
	encoded, err := json.Marshal(claims)
	if err != nil || len(encoded) > MaxTaskValidationEvidenceBytes {
		return invalid()
	}
	return nil
}

func ValidateTaskValidationEvidenceRecord(result string, record TaskValidationEvidenceRecord) error {
	if record.TaskRevision < 0 || record.EventSequence < 1 || len(record.ResultSHA256) != 64 || strings.ToLower(record.ResultSHA256) != record.ResultSHA256 {
		return domain.InvalidArgument("validation_evidence", "task validation evidence record is invalid")
	}
	if len(record.SourceLocations) > maxTaskValidationSourceLocations {
		return domain.InvalidArgument("validation_evidence", "task validation source locations exceed their limit")
	}
	availableSourceLocations := make(map[string]struct{}, len(record.SourceLocations))
	for _, location := range record.SourceLocations {
		if !validTaskStepSourceLocationDocument(location) {
			return domain.InvalidArgument("validation_evidence", "task validation source location is invalid")
		}
		if _, duplicate := availableSourceLocations[location.ID]; duplicate {
			return domain.InvalidArgument("validation_evidence", "task validation source location is duplicated")
		}
		availableSourceLocations[location.ID] = struct{}{}
	}
	digest := sha256.Sum256([]byte(result))
	if record.ResultSHA256 != hex.EncodeToString(digest[:]) {
		return domain.InvalidArgument("validation_evidence", "task validation evidence result digest does not match")
	}
	claims := make([]TaskResultClaim, 0, len(record.Claims))
	for index, claim := range record.Claims {
		if claim.ID != "c"+strconv.Itoa(index+1) {
			return domain.InvalidArgument("validation_evidence", "task validation evidence claim ID is invalid")
		}
		input := TaskResultClaim{ID: claim.ID, StartByte: claim.StartByte, EndByte: claim.EndByte,
			EvidenceRefs: make([]TaskValidationEvidenceRef, 0, len(claim.EvidenceRefs))}
		if claim.SourceLocationState != "" {
			if !validTaskValidationSourceLocationMapping(claim.SourceLocationState, claim.SourceLocationIDs, availableSourceLocations) {
				return domain.InvalidArgument("validation_evidence", "task validation source location mapping is invalid")
			}
		} else if len(claim.SourceLocationIDs) != 0 {
			return domain.InvalidArgument("validation_evidence", "task validation source location mapping is invalid")
		}
		if comparison := claim.PerformanceComparison; comparison != nil {
			input.PerformanceComparison = &TaskResultPerformanceComparison{
				Metric: comparison.Metric, Unit: comparison.Unit, Before: comparison.Before, After: comparison.After,
				Environment: comparison.Environment, BaselineCallID: comparison.BaselineRef.CallID, AfterCallID: comparison.AfterRef.CallID,
			}
			if comparison.Delta != comparison.After-comparison.Before ||
				comparison.BaselineRef.Kind != TaskValidationEvidenceBaseline || comparison.AfterRef.Kind != TaskValidationEvidenceBeforeAfter ||
				comparison.BaselineRef.TaskRevision != record.TaskRevision || comparison.AfterRef.TaskRevision != record.TaskRevision ||
				comparison.BaselineRef.Sequence < 1 || comparison.AfterRef.Sequence < 1 ||
				!validTaskResultEvidenceID(TaskValidationEvidenceBaseline, comparison.BaselineRef.ID) ||
				!validTaskResultEvidenceID(TaskValidationEvidenceBeforeAfter, comparison.AfterRef.ID) ||
				comparison.BaselineRef.ID == comparison.AfterRef.ID {
				return domain.InvalidArgument("validation_evidence", "task performance evidence reference is invalid")
			}
		}
		for _, ref := range claim.EvidenceRefs {
			if ref.TaskRevision != record.TaskRevision || ref.Sequence < 1 ||
				(ref.CallID != "" && (SafeTaskValidationCallID(ref.CallID) != ref.CallID ||
					(ref.Kind != TaskValidationEvidenceAction && ref.Kind != TaskValidationEvidenceBaseline &&
						ref.Kind != TaskValidationEvidenceBeforeAfter && ref.Kind != TaskValidationEvidenceSideEffect))) ||
				!validTaskResultEvidenceID(ref.Kind, ref.ID) {
				return domain.InvalidArgument("validation_evidence", "task validation evidence reference is invalid")
			}
			input.EvidenceRefs = append(input.EvidenceRefs, TaskValidationEvidenceRef{Kind: ref.Kind, ID: ref.ID})
		}
		claims = append(claims, input)
	}
	if err := ValidateTaskResultClaims(result, claims); err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	maxBytes := MaxTaskValidationEvidenceBytes
	if len(record.SourceLocations) > 0 {
		maxBytes = maxTaskValidationEvidenceResponseBytes
	}
	if err != nil || len(encoded) > maxBytes {
		return domain.InvalidArgument("validation_evidence", "task validation evidence record exceeds the limit")
	}
	return nil
}

const (
	maxTaskValidationSourceLocations       = 32
	maxTaskValidationEvidenceResponseBytes = 256 << 10
)

func validTaskValidationSourceLocationMapping(state string, ids []string, available map[string]struct{}) bool {
	if len(ids) > maxTaskValidationSourceLocations {
		return false
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !validTaskStepSourceLocationID(id) {
			return false
		}
		if _, exists := available[id]; !exists {
			return false
		}
		if _, duplicate := seen[id]; duplicate {
			return false
		}
		seen[id] = struct{}{}
	}
	switch state {
	case TaskValidationSourceLocationsMapped:
		return len(ids) > 0
	case TaskValidationSourceLocationsPartial:
		return len(ids) > 0
	case TaskValidationSourceLocationsUnavailable, TaskValidationSourceLocationsRestricted,
		TaskValidationSourceLocationsNone, TaskValidationSourceLocationsTruncated:
		return len(ids) == 0
	default:
		return false
	}
}

// SafeTaskValidationCallID keeps only bounded identifier-safe characters in evidence details.
func SafeTaskValidationCallID(value string) string {
	if len(value) == 0 || len(value) > 128 {
		return ""
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return ""
		}
	}
	return value
}

func validTaskResultClaimID(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '.' && c != '_' && c != '~' && c != '-' {
			return false
		}
	}
	return true
}

func validTaskResultEvidenceID(kind TaskValidationEvidenceKind, id string) bool {
	switch kind {
	case TaskValidationEvidenceSource:
		return domain.ValidMessageID(id)
	case TaskValidationEvidenceBaseline, TaskValidationEvidenceAction, TaskValidationEvidenceResult, TaskValidationEvidenceBeforeAfter:
		return domain.ValidStepID(id)
	case TaskValidationEvidenceSideEffect:
		return domain.ValidArtifactID(id)
	default:
		return false
	}
}

func validTaskResultEvidenceRef(ref TaskValidationEvidenceRef) bool {
	if ref.ID != "" && ref.CallID != "" {
		return false
	}
	if ref.CallID != "" {
		return (ref.Kind == TaskValidationEvidenceAction || ref.Kind == TaskValidationEvidenceBaseline ||
			ref.Kind == TaskValidationEvidenceBeforeAfter || ref.Kind == TaskValidationEvidenceSideEffect) &&
			SafeTaskValidationCallID(ref.CallID) == ref.CallID
	}
	if ref.Kind == TaskValidationEvidenceSource && ref.ID == "current" {
		return true
	}
	if ref.Kind == TaskValidationEvidenceResult && ref.ID == "current" {
		return true
	}
	return validTaskResultEvidenceID(ref.Kind, ref.ID)
}

func validTaskResultPerformanceComparison(value *TaskResultPerformanceComparison) bool {
	if value == nil {
		return true
	}
	validText := func(text string, maxBytes int, singleLine bool) bool {
		return len(text) > 0 && len(text) <= maxBytes && utf8.ValidString(text) && strings.TrimSpace(text) == text &&
			!strings.ContainsRune(text, '\x00') && (!singleLine || !strings.ContainsAny(text, "\r\n"))
	}
	return validText(value.Metric, 128, true) && validText(value.Unit, 32, true) &&
		validText(value.Environment, 512, true) && finiteTaskValidationNumber(value.Before) && finiteTaskValidationNumber(value.After) &&
		value.BaselineCallID != value.AfterCallID && SafeTaskValidationCallID(value.BaselineCallID) == value.BaselineCallID &&
		SafeTaskValidationCallID(value.AfterCallID) == value.AfterCallID && finiteTaskValidationNumber(value.After-value.Before)
}

func finiteTaskValidationNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// ValidateTaskHardCheckShape validates an untrusted review's structure only.
// Its output cannot establish a trusted task verdict or prove its assertions.
func ValidateTaskHardCheckShape(checks []TaskHardCheckClaim) TaskHardCheckShapeValidation {
	result := TaskHardCheckShapeValidation{
		Checks: make([]TaskHardCheckClaimResult, len(taskHardCheckOrder)),
		Issues: make([]TaskValidationIssue, 0),
	}
	checkIndex := make(map[TaskHardCheckID]int, len(taskHardCheckOrder))
	for i, id := range taskHardCheckOrder {
		checkIndex[id] = i
		result.Checks[i] = TaskHardCheckClaimResult{ID: id, Outcome: TaskHardCheckFail, EvidenceRefs: make([]TaskValidationEvidenceRef, 0)}
	}
	issues := make(map[TaskValidationIssue]struct{})
	addIssue := func(issue TaskValidationIssue) { issues[issue] = struct{}{} }
	seen := make(map[TaskHardCheckID]bool, len(taskHardCheckOrder))
	if len(checks) > len(taskHardCheckOrder) {
		addIssue(TaskValidationIssueTooManyChecks)
		checks = checks[:len(taskHardCheckOrder)]
	}

	for _, check := range checks {
		index, known := checkIndex[check.ID]
		if !known {
			addIssue(TaskValidationIssueUnknownCheck)
			continue
		}
		if seen[check.ID] {
			addIssue(TaskValidationIssueDuplicateCheck)
			result.Checks[index] = failedHardCheckClaim(check.ID)
			continue
		}
		seen[check.ID] = true

		refs, refsValid := validateTaskValidationEvidenceRefs(check.EvidenceRefs, addIssue)
		checkResult := TaskHardCheckClaimResult{
			ID:                  check.ID,
			Outcome:             TaskHardCheckFail,
			EvidenceRefs:        refs,
			HasNonBlockingNotes: check.HasNonBlockingNotes,
		}
		if !refsValid {
			checkResult.HasNonBlockingNotes = false
		}

		switch check.Outcome {
		case TaskHardCheckPass:
			if check.NotApplicableReason != "" {
				addIssue(TaskValidationIssueUnexpectedNotApplicableReason)
				break
			}
			if len(refs) == 0 {
				addIssue(TaskValidationIssueMissingEvidence)
				break
			}
			if !refsValid {
				break
			}
			checkResult.Outcome = TaskHardCheckPass
		case TaskHardCheckFail:
			if check.NotApplicableReason != "" {
				addIssue(TaskValidationIssueUnexpectedNotApplicableReason)
			}
		case TaskHardCheckNotApplicable:
			if check.NotApplicableReason == "" {
				addIssue(TaskValidationIssueMissingNotApplicableReason)
				break
			}
			if !validTaskHardCheckNotApplicableReason(check.ID, check.NotApplicableReason) {
				addIssue(TaskValidationIssueInvalidNotApplicableReason)
				break
			}
			if !refsValid {
				break
			}
			checkResult.Outcome = TaskHardCheckNotApplicable
			checkResult.NotApplicableReason = check.NotApplicableReason
		default:
			addIssue(TaskValidationIssueInvalidOutcome)
		}
		if checkResult.Outcome != TaskHardCheckPass {
			checkResult.HasNonBlockingNotes = false
		}
		result.Checks[index] = checkResult
	}

	for _, id := range taskHardCheckOrder {
		if !seen[id] {
			addIssue(TaskValidationIssueMissingCheck)
		}
	}
	for _, issue := range taskValidationIssueOrder {
		if _, ok := issues[issue]; ok {
			result.Issues = append(result.Issues, issue)
		}
	}

	result.ValidShape = len(result.Issues) == 0
	return result
}

func failedHardCheckClaim(id TaskHardCheckID) TaskHardCheckClaimResult {
	return TaskHardCheckClaimResult{ID: id, Outcome: TaskHardCheckFail, EvidenceRefs: make([]TaskValidationEvidenceRef, 0)}
}

func validTaskHardCheckNotApplicableReason(id TaskHardCheckID, reason TaskHardCheckNotApplicableReason) bool {
	switch id {
	case TaskHardCheckIndependent, TaskHardCheckReplay:
		return reason == TaskHardCheckNoTaskExecution
	case TaskHardCheckApproval:
		return reason == TaskHardCheckNoIrreversibleAction
	case TaskHardCheckCorrection:
		return reason == TaskHardCheckNoRepeatedError
	default:
		return false
	}
}

func validateTaskValidationEvidenceRefs(refs []TaskValidationEvidenceRef, addIssue func(TaskValidationIssue)) ([]TaskValidationEvidenceRef, bool) {
	capacity := min(len(refs), maxTaskValidationEvidenceRefs)
	validated := make([]TaskValidationEvidenceRef, 0, capacity)
	seen := make(map[TaskValidationEvidenceRef]struct{}, capacity)
	valid := true
	if len(refs) > maxTaskValidationEvidenceRefs {
		addIssue(TaskValidationIssueInvalidEvidenceReference)
		valid = false
	}
	for i, ref := range refs {
		if i >= maxTaskValidationEvidenceRefs {
			break
		}
		if !validTaskValidationEvidenceRef(ref) {
			addIssue(TaskValidationIssueInvalidEvidenceReference)
			valid = false
			continue
		}
		if _, exists := seen[ref]; exists {
			addIssue(TaskValidationIssueDuplicateEvidenceReference)
			valid = false
			continue
		}
		seen[ref] = struct{}{}
		validated = append(validated, ref)
	}
	return validated, valid
}

func validTaskValidationEvidenceRef(ref TaskValidationEvidenceRef) bool {
	switch ref.Kind {
	case TaskValidationEvidenceSource, TaskValidationEvidenceBaseline, TaskValidationEvidenceAction,
		TaskValidationEvidenceResult, TaskValidationEvidenceSideEffect, TaskValidationEvidenceBeforeAfter,
		TaskValidationEvidenceReview:
	default:
		return false
	}
	if ref.CallID != "" || ref.ID == "" || len(ref.ID) > maxTaskValidationEvidenceIDBytes || strings.TrimSpace(ref.ID) != ref.ID {
		return false
	}
	for i, r := range ref.ID {
		alphaNumeric := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alphaNumeric && r != '.' && r != '_' && r != ':' && r != '-' || i == 0 && !alphaNumeric {
			return false
		}
	}
	return true
}
