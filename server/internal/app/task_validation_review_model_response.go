package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

const MaxTaskValidationReviewModelResponseCiphertext = 32 << 10

type TaskValidationReviewModelResponseRecord struct {
	Ciphertext []byte
	Digest     []byte
	TextDigest []byte
}

type TaskValidationReviewModelResponseStore interface {
	RecoverTaskValidationReviewModelResponse(context.Context, ModelCallReservation) (TaskValidationReviewModelResponseRecord, bool, error)
}

type taskValidationReviewModelResponseIdentity struct {
	Version             int    `json:"version"`
	OwnerID             string `json:"owner_id"`
	TaskID              string `json:"task_id"`
	TaskRevision        int64  `json:"task_revision"`
	ResultEventSequence int64  `json:"result_event_sequence"`
	ResultSHA256        string `json:"result_sha256"`
	ClaimGeneration     int64  `json:"claim_generation"`
	LeaseOwner          string `json:"lease_owner"`
	SessionID           string `json:"session_id"`
	ThroughSequence     int64  `json:"through_sequence"`
	CallID              string `json:"call_id"`
	RequestDigest       string `json:"request_digest"`
	ModelID             string `json:"model_id"`
	ProviderEndpoint    string `json:"provider_endpoint"`
	ProviderModel       string `json:"provider_model"`
	ConnectionVersion   string `json:"connection_version"`
	ProjectID           string `json:"project_id"`
	PersonalModel       bool   `json:"personal_model"`
}

type taskValidationReviewModelResponseBody struct {
	Text                  string     `json:"text"`
	Usage                 ModelUsage `json:"usage"`
	StopReason            string     `json:"stop_reason"`
	ProviderReportedModel string     `json:"provider_reported_model,omitempty"`
}

type taskValidationReviewModelResponseEnvelope struct {
	Identity taskValidationReviewModelResponseIdentity `json:"identity"`
	Response taskValidationReviewModelResponseBody     `json:"response"`
}

func taskValidationReviewModelResponseAAD(reservation ModelCallReservation) (taskValidationReviewModelResponseIdentity, []byte, error) {
	if reservation.Source != ModelCallReview || reservation.Review == nil ||
		!ValidTaskValidationReviewModelFence(*reservation.Review) || reservation.OwnerID != reservation.Review.InitiatorID ||
		!domain.ValidModelConnectionID(reservation.ModelID) || taskValidationModelIdentityRouteKey(reservation.ProviderEndpoint) == "" ||
		reservation.ProviderModel == "" || len(reservation.ProviderModel) > 256 ||
		reservation.ConnectionVersion.IsZero() || len(reservation.RequestDigest) != sha256.Size ||
		!idempotencyKeyPattern.MatchString(reservation.CallID) ||
		(reservation.ProjectID != "" && !project.ValidID(reservation.ProjectID)) {
		return taskValidationReviewModelResponseIdentity{}, nil, domain.InvalidArgument("task_validation_review_model_response", "review model response identity is invalid")
	}
	callID, err := DSHTaskValidationReviewModelCallID(*reservation.Review)
	if err != nil || callID != reservation.CallID {
		return taskValidationReviewModelResponseIdentity{}, nil, domain.InvalidArgument("task_validation_review_model_response", "review model response call identity is invalid")
	}
	claim := reservation.Review.Claim
	identity := taskValidationReviewModelResponseIdentity{Version: 1, OwnerID: reservation.OwnerID, TaskID: claim.TaskID,
		TaskRevision: claim.TaskRevision, ResultEventSequence: claim.ResultEventSequence, ResultSHA256: hex.EncodeToString(claim.ResultSHA256),
		ClaimGeneration: claim.ClaimGeneration, LeaseOwner: claim.LeaseOwner, SessionID: reservation.Review.SessionID,
		ThroughSequence: reservation.Review.ThroughSequence, CallID: reservation.CallID, RequestDigest: hex.EncodeToString(reservation.RequestDigest),
		ModelID: reservation.ModelID, ProviderEndpoint: reservation.ProviderEndpoint, ProviderModel: reservation.ProviderModel,
		ConnectionVersion: reservation.ConnectionVersion.UTC().Format(time.RFC3339Nano), ProjectID: reservation.ProjectID,
		PersonalModel: reservation.PersonalModel}
	aad, err := json.Marshal(identity)
	if err != nil {
		return taskValidationReviewModelResponseIdentity{}, nil, domain.NewError(domain.CodeInternal, "review model response identity could not be encoded")
	}
	return identity, append([]byte("morde-task-validation-review-model-response-v1\n"), aad...), nil
}

func SealTaskValidationReviewModelResponse(sealer *Sealer, reservation ModelCallReservation, response ModelResponse) (TaskValidationReviewModelResponseRecord, error) {
	identity, aad, err := taskValidationReviewModelResponseAAD(reservation)
	if err != nil || sealer == nil || !validTaskValidationReviewModelResponse(response) {
		return TaskValidationReviewModelResponseRecord{}, domain.InvalidArgument("task_validation_review_model_response", "review model response is invalid")
	}
	plain, err := json.Marshal(taskValidationReviewModelResponseEnvelope{Identity: identity, Response: taskValidationReviewModelResponseBody{
		Text: response.Text, Usage: response.Usage, StopReason: response.StopReason, ProviderReportedModel: response.ProviderReportedModel,
	}})
	if err != nil || len(plain) > MaxTaskValidationReviewModelResponseCiphertext {
		return TaskValidationReviewModelResponseRecord{}, domain.NewError(domain.CodeResultTooLarge, "review model response is too large to store")
	}
	ciphertext, err := sealer.sealWithAAD(plain, aad)
	if err != nil || len(ciphertext) > MaxTaskValidationReviewModelResponseCiphertext {
		return TaskValidationReviewModelResponseRecord{}, domain.NewError(domain.CodeInvalidState, "review model response could not be stored safely")
	}
	digest := sha256.Sum256(plain)
	textDigest := sha256.Sum256([]byte(response.Text))
	return TaskValidationReviewModelResponseRecord{Ciphertext: ciphertext, Digest: append([]byte(nil), digest[:]...),
		TextDigest: append([]byte(nil), textDigest[:]...)}, nil
}

func OpenTaskValidationReviewModelResponse(sealer *Sealer, record TaskValidationReviewModelResponseRecord,
	reservation ModelCallReservation) (ModelResponse, error) {
	identity, aad, err := taskValidationReviewModelResponseAAD(reservation)
	if err != nil || sealer == nil || len(record.Ciphertext) == 0 || len(record.Ciphertext) > MaxTaskValidationReviewModelResponseCiphertext ||
		len(record.Digest) != sha256.Size || len(record.TextDigest) != sha256.Size {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored review model response identity is invalid")
	}
	plain, err := sealer.openWithAAD(record.Ciphertext, aad)
	if err != nil {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored review model response could not be opened")
	}
	digest := sha256.Sum256(plain)
	if !bytes.Equal(digest[:], record.Digest) {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored review model response digest changed")
	}
	var envelope taskValidationReviewModelResponseEnvelope
	decoder := json.NewDecoder(bytes.NewReader(plain))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil || envelope.Identity != identity || !utf8.ValidString(envelope.Response.Text) {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored review model response identity changed")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored review model response has trailing data")
	}
	textDigest := sha256.Sum256([]byte(envelope.Response.Text))
	if !bytes.Equal(textDigest[:], record.TextDigest) {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored review model response text digest changed")
	}
	response := ModelResponse{Text: envelope.Response.Text, Usage: envelope.Response.Usage,
		StopReason: envelope.Response.StopReason, ProviderReportedModel: envelope.Response.ProviderReportedModel}
	if !validTaskValidationReviewModelResponse(response) {
		return ModelResponse{}, domain.NewError(domain.CodeInvalidState, "stored review model response is invalid")
	}
	return response, nil
}

func validTaskValidationReviewModelResponse(response ModelResponse) bool {
	return len(response.ToolCalls) == 0 && response.StopReason == "stop" &&
		strings.TrimSpace(response.Text) != "" && utf8.ValidString(response.Text) && !strings.ContainsRune(response.Text, '\x00') &&
		len(response.Text) <= MaxTaskValidationReviewReportBytes && ValidProviderReportedModel(response.ProviderReportedModel) &&
		response.Usage.Known && response.Usage.InputTokens >= 0 && response.Usage.OutputTokens >= 0 &&
		response.Usage.InputTokens <= 1<<53-1 && response.Usage.OutputTokens <= 1<<53-1
}
