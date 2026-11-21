package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func (s *Store) RecoverTaskValidationReviewModelResponse(ctx context.Context,
	request app.ModelCallReservation) (app.TaskValidationReviewModelResponseRecord, bool, error) {
	if ctx == nil || request.Source != app.ModelCallReview || request.Review == nil || request.OwnerID != request.Review.InitiatorID ||
		len(request.RequestDigest) != 32 || request.CallID == "" {
		return app.TaskValidationReviewModelResponseRecord{}, false,
			domain.InvalidArgument("task_validation_review_model_response", "review model response recovery is invalid")
	}
	var record app.TaskValidationReviewModelResponseRecord
	found := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, request.OwnerID); err != nil {
			return err
		}
		if err := checkModelCaller(ctx, tx, request); err != nil {
			return err
		}
		var state, modelID, providerModel string
		var taskID *string
		var digest []byte
		err := tx.QueryRow(ctx, `SELECT state,task_id,model_id,provider_model,request_digest
FROM model_call_ledger WHERE owner_id=$1 AND source='review' AND call_id=$2 FOR UPDATE`, request.OwnerID, request.CallID).
			Scan(&state, &taskID, &modelID, &providerModel, &digest)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read Task validation review model ledger: %w", err)
		}
		if taskID == nil || *taskID != request.Review.Claim.TaskID || modelID != request.ModelID ||
			providerModel != request.ProviderModel || !bytes.Equal(digest, request.RequestDigest) {
			return domain.NewError(domain.CodeIdempotencyConflict, "review model response call identity changed")
		}
		if state != "settled" {
			return domain.NewError(domain.CodeInvalidState, "review model call has no recoverable settled response")
		}
		var storedTaskID, leaseOwner, sessionID, providerEndpoint, providerReportedModel string
		var taskRevision, eventSequence, claimGeneration, through int64
		var resultSHA, requestDigest, responseCiphertext, responseDigest, responseTextDigest []byte
		var connectionVersion time.Time
		var personalModel bool
		var projectID string
		if err := tx.QueryRow(ctx, `SELECT task_id,task_revision,result_event_sequence,result_sha256,claim_generation,lease_owner,
session_id,through_sequence,model_id,provider_model,COALESCE(provider_endpoint,''),COALESCE(provider_reported_model,''),connection_version,project_id,personal_model,request_digest,
response_ciphertext,response_digest,response_text_sha256
FROM task_validation_review_model_responses WHERE owner_id=$1 AND source='review' AND call_id=$2 FOR UPDATE`,
			request.OwnerID, request.CallID).Scan(&storedTaskID, &taskRevision, &eventSequence, &resultSHA, &claimGeneration,
			&leaseOwner, &sessionID, &through, &modelID, &providerModel, &providerEndpoint, &providerReportedModel, &connectionVersion, &projectID, &personalModel,
			&requestDigest, &responseCiphertext, &responseDigest, &responseTextDigest); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeInvalidState, "settled review model response is unavailable and requires reconciliation")
			}
			return fmt.Errorf("read settled Task validation review model response: %w", err)
		}
		claim := request.Review.Claim
		if storedTaskID != claim.TaskID || taskRevision != claim.TaskRevision || eventSequence != claim.ResultEventSequence ||
			!bytes.Equal(resultSHA, claim.ResultSHA256) || claimGeneration != claim.ClaimGeneration || leaseOwner != claim.LeaseOwner ||
			sessionID != request.Review.SessionID || through != request.Review.ThroughSequence || modelID != request.ModelID ||
			providerModel != request.ProviderModel || providerEndpoint != request.ProviderEndpoint ||
			!app.ValidProviderReportedModel(providerReportedModel) || !connectionVersion.Equal(request.ConnectionVersion) ||
			projectID != request.ProjectID || personalModel != request.PersonalModel || !bytes.Equal(requestDigest, request.RequestDigest) {
			return domain.NewError(domain.CodeIdempotencyConflict, "review model response recovery fence changed")
		}
		if len(responseCiphertext) == 0 || len(responseCiphertext) > app.MaxTaskValidationReviewModelResponseCiphertext ||
			len(responseDigest) != 32 || len(responseTextDigest) != 32 {
			return domain.NewError(domain.CodeInvalidState, "stored review model response is malformed")
		}
		record = app.TaskValidationReviewModelResponseRecord{Ciphertext: append([]byte(nil), responseCiphertext...),
			Digest: append([]byte(nil), responseDigest...), TextDigest: append([]byte(nil), responseTextDigest...)}
		found = true
		return nil
	})
	return record, found, err
}

func recordTaskValidationReviewModelResponseInTx(ctx context.Context, tx pgx.Tx,
	settlement app.ModelCallSettlement) (bool, error) {
	request := settlement.ReviewCall
	if request == nil || request.Source != app.ModelCallReview || request.Review == nil || request.OwnerID != settlement.OwnerID ||
		request.CallID != settlement.CallID || len(request.RequestDigest) != 32 || request.ProviderEndpoint == "" ||
		!app.ValidProviderReportedModel(settlement.ProviderReportedModel) || len(settlement.ResponseCiphertext) == 0 ||
		len(settlement.ResponseCiphertext) > app.MaxTaskValidationReviewModelResponseCiphertext || len(settlement.ResponseDigest) != 32 ||
		len(settlement.ResponseTextDigest) != 32 ||
		!settlement.Usage.Known {
		return false, domain.InvalidArgument("task_validation_review_model_response", "review model response settlement is invalid")
	}
	var state, taskID, modelID, providerModel string
	var requestDigest []byte
	if err := tx.QueryRow(ctx, `SELECT state,COALESCE(task_id,''),model_id,provider_model,request_digest
FROM model_call_ledger WHERE owner_id=$1 AND source='review' AND call_id=$2 FOR UPDATE`, request.OwnerID, request.CallID).
		Scan(&state, &taskID, &modelID, &providerModel, &requestDigest); err != nil {
		return false, fmt.Errorf("verify review model response ledger: %w", err)
	}
	if state != "settled" || taskID != request.Review.Claim.TaskID || modelID != request.ModelID ||
		providerModel != request.ProviderModel || !bytes.Equal(requestDigest, request.RequestDigest) {
		return false, domain.NewError(domain.CodeIdempotencyConflict, "review model response settlement identity changed")
	}
	claim := request.Review.Claim
	_, err := tx.Exec(ctx, `INSERT INTO task_validation_review_model_responses
(owner_id,source,call_id,task_id,task_revision,result_event_sequence,result_sha256,claim_generation,lease_owner,
 session_id,through_sequence,model_id,provider_model,provider_endpoint,provider_reported_model,connection_version,project_id,personal_model,request_digest,response_ciphertext,response_digest,response_text_sha256)
VALUES ($1,'review',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`, request.OwnerID, request.CallID,
		claim.TaskID, claim.TaskRevision, claim.ResultEventSequence, claim.ResultSHA256, claim.ClaimGeneration, claim.LeaseOwner,
		request.Review.SessionID, request.Review.ThroughSequence, request.ModelID, request.ProviderModel, request.ProviderEndpoint,
		settlement.ProviderReportedModel, request.ConnectionVersion,
		request.ProjectID, request.PersonalModel, request.RequestDigest, settlement.ResponseCiphertext, settlement.ResponseDigest,
		settlement.ResponseTextDigest)
	if err != nil {
		return false, fmt.Errorf("persist Task validation review model response: %w", err)
	}
	return true, nil
}

func verifySettledTaskValidationReviewModelResponseInTx(ctx context.Context, tx pgx.Tx,
	settlement app.ModelCallSettlement) error {
	if settlement.ReviewCall != nil && (settlement.ReviewCall.Review == nil || settlement.ReviewCall.Source != app.ModelCallReview ||
		settlement.ReviewCall.OwnerID != settlement.OwnerID || settlement.ReviewCall.CallID != settlement.CallID) {
		return domain.InvalidArgument("task_validation_review_model_response", "review model response settlement identity is invalid")
	}
	if settlement.ReviewCall == nil {
		if settlement.ResponseInvalid {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_validation_review_model_responses
WHERE owner_id=$1 AND source='review' AND call_id=$2)`, settlement.OwnerID, settlement.CallID).Scan(&exists); err != nil {
				return fmt.Errorf("check settled review model response identity: %w", err)
			}
			if exists {
				return domain.NewError(domain.CodeIdempotencyConflict, "review model response was already stored")
			}
		}
		return nil
	}
	request := settlement.ReviewCall
	var taskID, leaseOwner, sessionID, modelID, providerModel, providerEndpoint, providerReportedModel, projectID string
	var taskRevision, eventSequence, claimGeneration, through int64
	var resultSHA, requestDigest, ciphertext, digest, textDigest []byte
	var connectionVersion time.Time
	var personalModel bool
	err := tx.QueryRow(ctx, `SELECT task_id,task_revision,result_event_sequence,result_sha256,claim_generation,lease_owner,
session_id,through_sequence,model_id,provider_model,provider_endpoint,provider_reported_model,connection_version,project_id,personal_model,request_digest,
response_ciphertext,response_digest,response_text_sha256 FROM task_validation_review_model_responses
WHERE owner_id=$1 AND source='review' AND call_id=$2 FOR UPDATE`, settlement.OwnerID, settlement.CallID).
		Scan(&taskID, &taskRevision, &eventSequence, &resultSHA, &claimGeneration, &leaseOwner, &sessionID, &through,
			&modelID, &providerModel, &providerEndpoint, &providerReportedModel, &connectionVersion, &projectID, &personalModel, &requestDigest, &ciphertext, &digest, &textDigest)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) && settlement.ResponseInvalid {
			return nil
		}
		return storeError("verify settled review response", "Task validation review model response", err)
	}
	claim := request.Review.Claim
	if taskID != claim.TaskID || taskRevision != claim.TaskRevision || eventSequence != claim.ResultEventSequence ||
		!bytes.Equal(resultSHA, claim.ResultSHA256) || claimGeneration != claim.ClaimGeneration || leaseOwner != claim.LeaseOwner ||
		sessionID != request.Review.SessionID || through != request.Review.ThroughSequence || modelID != request.ModelID ||
		providerModel != request.ProviderModel || providerEndpoint != request.ProviderEndpoint ||
		providerReportedModel != settlement.ProviderReportedModel || !connectionVersion.Equal(request.ConnectionVersion) || projectID != request.ProjectID ||
		personalModel != request.PersonalModel || !bytes.Equal(requestDigest, request.RequestDigest) ||
		!bytes.Equal(ciphertext, settlement.ResponseCiphertext) || !bytes.Equal(digest, settlement.ResponseDigest) ||
		!bytes.Equal(textDigest, settlement.ResponseTextDigest) {
		return domain.NewError(domain.CodeIdempotencyConflict, "settled review model response identity changed")
	}
	return nil
}

var _ app.TaskValidationReviewModelResponseStore = (*Store)(nil)
