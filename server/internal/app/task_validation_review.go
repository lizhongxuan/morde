package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"morde/server/internal/domain"
)

type TaskValidationReviewClaim struct {
	OwnerID             string
	InitiatorID         string
	ConversationID      string
	TaskID              string
	TaskRevision        int64
	ResultEventSequence int64
	ResultSHA256        []byte
	ClaimGeneration     int64
	LeaseOwner          string
	LeaseExpiresAt      time.Time
}

type TaskValidationReviewAssessment string

const (
	TaskValidationReviewSupports     TaskValidationReviewAssessment = "supports"
	TaskValidationReviewContradicts  TaskValidationReviewAssessment = "contradicts"
	TaskValidationReviewInsufficient TaskValidationReviewAssessment = "insufficient"
)

// TaskValidationReviewFinding is a short, evidence-linked reviewer assessment;
// it is a recommendation and never a trusted verdict by itself.
type TaskValidationReviewFinding struct {
	ID           TaskHardCheckID                   `json:"id"`
	Assessment   TaskValidationReviewAssessment    `json:"assessment"`
	Rationale    string                            `json:"rationale"`
	EvidenceRefs []TaskValidationEvidenceRefRecord `json:"evidence_refs"`
}

type TaskValidationReviewReport struct {
	Recommendation TaskValidationState           `json:"recommendation"`
	Summary        string                        `json:"summary"`
	Findings       []TaskValidationReviewFinding `json:"findings"`
}

type TaskValidationReviewModel struct {
	CallID            string
	ConnectionID      string
	ConnectionVersion time.Time
	Endpoint          string
	ConfiguredModel   string
	ReportedModel     string
	Identity          TaskValidationModelIdentity
	ResponseDigest    []byte
	SessionID         string
	ThroughSequence   int64
}

type TaskValidationReviewCompletion struct {
	Claim          TaskValidationReviewClaim
	SessionID      string
	ResponseText   string
	ExecutionModel TaskValidationReviewModel
	ReviewerModel  TaskValidationReviewModel
	Report         TaskValidationReviewReport
}

type TaskValidationReviewInput struct {
	Objective       string                               `json:"objective"`
	Result          string                               `json:"result"`
	Evidence        *TaskValidationEvidenceRecord        `json:"evidence"`
	EvidenceDetails []TaskValidationReviewEvidenceDetail `json:"evidence_details"`
}

type TaskValidationReviewEvidenceDetail struct {
	Ref      TaskValidationEvidenceRefRecord `json:"ref"`
	State    string                          `json:"state"`
	Step     *TaskValidationReviewStepDetail `json:"step,omitempty"`
	Artifact *TaskValidationReviewArtifact   `json:"artifact,omitempty"`
}

type TaskValidationReviewStepDetail struct {
	ToolName        string                          `json:"tool_name"`
	Status          string                          `json:"status"`
	Title           string                          `json:"title"`
	Summary         string                          `json:"summary"`
	DetailState     string                          `json:"detail_state"`
	Object          *string                         `json:"object,omitempty"`
	Input           *string                         `json:"input,omitempty"`
	Result          *string                         `json:"result,omitempty"`
	Truncated       bool                            `json:"truncated"`
	ShellExecution  *TaskStepShellExecutionEvidence `json:"shell_execution,omitempty"`
	ContentEvidence *TaskStepContentEvidence        `json:"content_evidence,omitempty"`
}

type TaskValidationReviewArtifact struct {
	SourceCallID string `json:"source_call_id"`
	Label        string `json:"label"`
	Path         string `json:"path"`
	MediaType    string `json:"media_type"`
	SizeBytes    int64  `json:"size_bytes"`
	Revision     int64  `json:"revision"`
	SHA256       string `json:"sha256"`
}

const (
	MaxTaskValidationReviewReportBytes    = 16 << 10
	maxTaskValidationReviewSummaryBytes   = 1024
	maxTaskValidationReviewRationaleBytes = 512
	maxTaskValidationReviewEvidenceRefs   = 16
)

// ParseTaskValidationReviewReport rejects duplicate and unknown JSON fields
// before accepting a bounded structured reviewer recommendation.
func ParseTaskValidationReviewReport(raw []byte, evidence *TaskValidationEvidenceRecord) (TaskValidationReviewReport, error) {
	invalid := func() (TaskValidationReviewReport, error) {
		return TaskValidationReviewReport{}, domain.InvalidArgument("task_validation_review", "task validation review report is invalid")
	}
	if !validAgentJSONObject(raw, MaxTaskValidationReviewReportBytes) {
		return invalid()
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var report TaskValidationReviewReport
	if decoder.Decode(&report) != nil {
		return invalid()
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || ValidateTaskValidationReviewReport(report, evidence) != nil {
		return invalid()
	}
	return report, nil
}

func ValidateTaskValidationReviewReport(report TaskValidationReviewReport, evidence *TaskValidationEvidenceRecord) error {
	if report.Recommendation != TaskValidationPass && report.Recommendation != TaskValidationPassNotes && report.Recommendation != TaskValidationFail ||
		!validTaskValidationReviewText(report.Summary, maxTaskValidationReviewSummaryBytes) || len(report.Findings) != len(taskHardCheckOrder) {
		return domain.InvalidArgument("task_validation_review", "task validation review report is invalid")
	}
	allowed := make(map[TaskValidationEvidenceRefRecord]struct{})
	if evidence != nil {
		for _, claim := range evidence.Claims {
			for _, ref := range claim.EvidenceRefs {
				allowed[ref] = struct{}{}
			}
			if comparison := claim.PerformanceComparison; comparison != nil {
				allowed[comparison.BaselineRef] = struct{}{}
				allowed[comparison.AfterRef] = struct{}{}
			}
		}
	}
	hasNonSupportingFinding := false
	for index, finding := range report.Findings {
		if finding.ID != taskHardCheckOrder[index] ||
			(finding.Assessment != TaskValidationReviewSupports && finding.Assessment != TaskValidationReviewContradicts &&
				finding.Assessment != TaskValidationReviewInsufficient) ||
			!validTaskValidationReviewText(finding.Rationale, maxTaskValidationReviewRationaleBytes) ||
			len(finding.EvidenceRefs) > maxTaskValidationReviewEvidenceRefs ||
			finding.Assessment != TaskValidationReviewInsufficient && len(finding.EvidenceRefs) == 0 {
			return domain.InvalidArgument("task_validation_review", "task validation review report is invalid")
		}
		hasNonSupportingFinding = hasNonSupportingFinding || finding.Assessment != TaskValidationReviewSupports
		seen := make(map[TaskValidationEvidenceRefRecord]struct{}, len(finding.EvidenceRefs))
		for _, ref := range finding.EvidenceRefs {
			if _, ok := allowed[ref]; !ok {
				return domain.InvalidArgument("task_validation_review", "task validation review cites unavailable evidence")
			}
			if _, duplicate := seen[ref]; duplicate {
				return domain.InvalidArgument("task_validation_review", "task validation review repeats an evidence reference")
			}
			seen[ref] = struct{}{}
		}
	}
	if (report.Recommendation == TaskValidationFail) != hasNonSupportingFinding {
		return domain.InvalidArgument("task_validation_review", "task validation review recommendation conflicts with its findings")
	}
	encoded, err := json.Marshal(report)
	if err != nil || len(encoded) > MaxTaskValidationReviewReportBytes {
		return domain.InvalidArgument("task_validation_review", "task validation review report exceeds its limit")
	}
	return nil
}

func ValidateTaskValidationReviewCompletion(value TaskValidationReviewCompletion, evidence *TaskValidationEvidenceRecord) error {
	if !ValidTaskValidationReviewClaim(value.Claim) || !dshModelIdentityPattern.MatchString(value.SessionID) ||
		len(value.ResponseText) == 0 || len(value.ResponseText) > MaxTaskValidationReviewReportBytes ||
		!validTaskValidationReviewModel(value.ExecutionModel) || !validTaskValidationReviewModel(value.ReviewerModel) ||
		len(value.ExecutionModel.ResponseDigest) != 32 ||
		value.ExecutionModel.Identity.Family == value.ReviewerModel.Identity.Family ||
		value.ExecutionModel.Identity.CatalogVersion != value.ReviewerModel.Identity.CatalogVersion {
		return domain.InvalidArgument("task_validation_review", "task validation review identity is invalid")
	}
	parsed, err := ParseTaskValidationReviewReport([]byte(value.ResponseText), evidence)
	providedJSON, providedErr := json.Marshal(value.Report)
	parsedJSON, parsedErr := json.Marshal(parsed)
	if err != nil || providedErr != nil || parsedErr != nil || !bytes.Equal(providedJSON, parsedJSON) {
		return domain.InvalidArgument("task_validation_review", "task validation review report does not match its model response")
	}
	return nil
}

func validTaskValidationReviewModel(model TaskValidationReviewModel) bool {
	if !idempotencyKeyPattern.MatchString(model.CallID) || !domain.ValidModelConnectionID(model.ConnectionID) || model.ConnectionVersion.IsZero() ||
		len(model.ResponseDigest) != 32 || !dshModelIdentityPattern.MatchString(model.SessionID) ||
		model.ThroughSequence < 0 || model.ThroughSequence > 1<<53-1 {
		return false
	}
	identity, ok := ResolveTaskValidationModelIdentity(model.Endpoint, model.ConfiguredModel, model.ReportedModel)
	return ok && identity == model.Identity
}

func ValidTaskValidationReviewClaim(claim TaskValidationReviewClaim) bool {
	return domain.ValidUserID(claim.OwnerID) && domain.ValidUserID(claim.InitiatorID) && domain.ValidConversationID(claim.ConversationID) &&
		domain.ValidTaskID(claim.TaskID) && claim.TaskRevision >= 0 &&
		claim.ResultEventSequence > 0 && len(claim.ResultSHA256) == 32 && claim.ClaimGeneration > 0 &&
		claim.LeaseOwner != "" && strings.TrimSpace(claim.LeaseOwner) == claim.LeaseOwner && len(claim.LeaseOwner) <= 128
}

func validTaskValidationReviewText(value string, limit int) bool {
	return value != "" && strings.TrimSpace(value) == value && len(value) <= limit && utf8.ValidString(value) &&
		!strings.ContainsRune(value, '\x00') && !strings.ContainsFunc(value, unicode.IsControl)
}

type TaskValidationReviewQueue interface {
	ClaimTaskValidationReviews(context.Context, string, time.Duration, int32) ([]TaskValidationReviewClaim, error)
	RetryTaskValidationReview(context.Context, TaskValidationReviewClaim, time.Duration) (bool, error)
	CompleteTaskValidationReview(context.Context, TaskValidationReviewCompletion) (bool, error)
}

type TaskValidationReviewSnapshotReader interface {
	GetTaskValidationReviewSnapshot(context.Context, TaskValidationReviewClaim) (TaskDetails, error)
	GetTaskValidationReviewStepDetail(context.Context, TaskValidationReviewClaim, string) (TaskStepDetail, error)
}
