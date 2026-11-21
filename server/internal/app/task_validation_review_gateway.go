package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"morde/server/internal/domain"
)

type TaskValidationReviewModelFence struct {
	Claim           TaskValidationReviewClaim
	InitiatorID     string
	SessionID       string
	ThroughSequence int64
}

func ValidTaskValidationReviewModelFence(value TaskValidationReviewModelFence) bool {
	return ValidTaskValidationReviewClaim(value.Claim) && domain.ValidUserID(value.InitiatorID) &&
		dshModelIdentityPattern.MatchString(value.SessionID) && value.ThroughSequence >= 0 && value.ThroughSequence <= 1<<53-1
}

// DSHTaskValidationReviewModelCallID binds a metered DSH turn to one leased,
// revision-bound review claim and makes provider-response recovery idempotent.
func DSHTaskValidationReviewModelCallID(fence TaskValidationReviewModelFence) (string, error) {
	if !ValidTaskValidationReviewModelFence(fence) {
		return "", domain.InvalidArgument("model_call", "invalid Task validation review model identity")
	}
	claim := fence.Claim
	seed := "morde-dsh-task-validation-review-v1\n" + claim.OwnerID + "\n" + claim.TaskID + "\n" +
		strconv.FormatInt(claim.TaskRevision, 10) + "\n" + strconv.FormatInt(claim.ResultEventSequence, 10) + "\n" +
		hex.EncodeToString(claim.ResultSHA256) + "\n" + strconv.FormatInt(claim.ClaimGeneration, 10) + "\n" +
		fence.SessionID + "\n" + strconv.FormatInt(fence.ThroughSequence, 10)
	digest := sha256.Sum256([]byte(seed))
	return "review_" + hex.EncodeToString(digest[:]), nil
}

func (s *ModelGatewayService) CompleteTaskValidationReviewDSHModel(ctx context.Context, fence TaskValidationReviewModelFence,
	modelID string, raw json.RawMessage) (ModelResponse, error) {
	if ctx == nil || !ValidTaskValidationReviewModelFence(fence) || !domain.ValidModelConnectionID(modelID) || len(raw) == 0 || len(raw) > 1<<20 {
		return ModelResponse{}, domain.InvalidArgument("task_validation_review_model", "review model request is invalid")
	}
	request, err := parseDSHModelRequest(ctx, ModelTaskFence{}, modelID, raw, nil)
	if err != nil || len(request.Tools) != 0 || request.RequiredTool != "" {
		return ModelResponse{}, domain.InvalidArgument("task_validation_review_model", "review model request must not contain tools")
	}
	callID, err := DSHTaskValidationReviewModelCallID(fence)
	if err != nil {
		return ModelResponse{}, err
	}
	response, err := s.Complete(ctx, ModelCall{Source: ModelCallReview, Review: &fence, ModelID: modelID,
		CallID: callID, RawRequest: raw, Request: request})
	if err == nil && response.TaskValidationReview != nil {
		response.TaskValidationReview.SessionID = fence.SessionID
		response.TaskValidationReview.ThroughSequence = fence.ThroughSequence
	}
	return response, err
}

type TaskValidationReviewExecutionModelResponse struct {
	Reservation ModelCallReservation
	Response    TaskModelResponseRecord
}

type TaskValidationReviewExecutionModelStore interface {
	RecoverTaskValidationReviewExecutionModelResponse(context.Context, TaskValidationReviewClaim) (TaskValidationReviewExecutionModelResponse, bool, error)
}

func (s *ModelGatewayService) ResolveTaskValidationReviewExecutionModel(ctx context.Context,
	claim TaskValidationReviewClaim) (TaskValidationReviewModel, error) {
	if ctx == nil || !ValidTaskValidationReviewClaim(claim) {
		return TaskValidationReviewModel{}, domain.InvalidArgument("task_validation_review_model", "execution model claim is invalid")
	}
	store, ok := s.store.(TaskValidationReviewExecutionModelStore)
	if !ok {
		return TaskValidationReviewModel{}, domain.NewError(domain.CodeUnsupported, "Task execution model response is unavailable")
	}
	stored, found, err := store.RecoverTaskValidationReviewExecutionModelResponse(ctx, claim)
	if err != nil || !found {
		if err != nil {
			return TaskValidationReviewModel{}, err
		}
		return TaskValidationReviewModel{}, domain.NewError(domain.CodeConflict, "Task execution model response is unavailable")
	}
	reservation := stored.Reservation
	response, err := OpenTaskModelResponse(s.responseSealer, stored.Response, reservation)
	if err != nil || response.ProviderReportedModel == "" {
		return TaskValidationReviewModel{}, domain.NewError(domain.CodeInvalidState, "Task execution model identity is unavailable")
	}
	model, err := taskValidationReviewModelFromResponse(reservation.ModelID, reservation.ConnectionVersion,
		reservation.ProviderEndpoint, reservation.ProviderModel, response.ProviderReportedModel, reservation.CallID, stored.Response.Digest)
	if err != nil {
		return TaskValidationReviewModel{}, err
	}
	model.SessionID = reservation.Task.AgentSessionID
	model.ThroughSequence = reservation.Task.ThroughSequence
	return model, nil
}

func (s *ModelGatewayService) ResolveTaskValidationReviewModel(ctx context.Context, ownerID string,
	execution TaskValidationReviewModel) (string, error) {
	if ctx == nil || !domain.ValidUserID(ownerID) || !validTaskValidationReviewModel(execution) {
		return "", domain.InvalidArgument("task_validation_review_model", "execution model identity is invalid")
	}
	selector, ok := s.models.(interface {
		SelectIndependentTaskValidationReviewModel(context.Context, string, TaskValidationReviewModel) (string, error)
	})
	if !ok {
		return "", domain.NewError(domain.CodeUnsupported, "Task validation review model catalog is unavailable")
	}
	return selector.SelectIndependentTaskValidationReviewModel(ctx, ownerID, execution)
}

func taskValidationReviewModelFromResponse(connectionID string, connectionVersion time.Time, endpoint, configured, reported string,
	callID string, responseDigest []byte) (TaskValidationReviewModel, error) {
	identity, ok := ResolveTaskValidationModelIdentity(endpoint, configured, reported)
	if !ok || !domain.ValidModelConnectionID(connectionID) || connectionVersion.IsZero() {
		return TaskValidationReviewModel{}, domain.NewError(domain.CodeUnsupported, "Task validation model family could not be verified")
	}
	return TaskValidationReviewModel{ConnectionID: connectionID, ConnectionVersion: connectionVersion, Endpoint: endpoint,
		ConfiguredModel: configured, ReportedModel: reported, Identity: identity, CallID: callID,
		ResponseDigest: append([]byte(nil), responseDigest...)}, nil
}

type TaskValidationReviewModelProjectResolver interface {
	ModelTaskProjectIDForReview(context.Context, TaskValidationReviewModelFence) (string, error)
}
