package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

func validTaskTerminalDeliveryModelCallReservation(request app.ModelCallReservation) bool {
	fence := request.TerminalDelivery
	if request.Source != app.ModelCallTerminalDelivery || fence == nil || request.Task != nil || request.Review != nil ||
		request.Summary != nil || request.Route != nil || request.Principal.UserID != "" || request.Principal.TokenID != "" ||
		request.Principal.ClientInstallationID != "" || request.OwnerID != fence.InitiatorID || request.NeedsTools || request.NeedsVision ||
		!app.ValidTaskTerminalDeliveryModelDispatchFence(*fence) {
		return false
	}
	expected, err := app.DSHTerminalDeliveryModelCallID(*fence)
	return err == nil && request.CallID == expected
}

func taskTerminalDeliveryClaimFromModelFence(fence app.TaskTerminalDeliveryModelFence) TaskTerminalDeliveryClaim {
	return TaskTerminalDeliveryClaim{
		ID: fence.DeliveryID, TaskID: fence.TaskID, OwnerID: fence.OwnerID,
		ConversationID: fence.ConversationID, InitiatorID: fence.InitiatorID,
		TaskRevision: fence.TaskRevision, AuthorizationVersion: fence.AuthorizationVersion,
		SourceKind: fence.SourceKind, SourceEventSequence: fence.SourceEventSequence,
		SourceEventType: fence.SourceEventType, SourceSHA256: append([]byte(nil), fence.SourceSHA256...),
		ResultSHA256:              append([]byte(nil), fence.ResultSHA256...),
		ValidationClaimGeneration: cloneNullableInt64(fence.ValidationClaimGeneration),
		VerdictSHA256:             append([]byte(nil), fence.VerdictSHA256...),
		ClaimGeneration:           fence.ClaimGeneration, LeaseOwner: fence.LeaseOwner,
		LeaseExpiresAt: fence.LeaseExpiresAt, ModelCallID: fence.ModelCallID,
		State: TaskTerminalDeliveryStateClaimed,
	}
}

func cloneNullableInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func taskTerminalDeliveryFenceMatches(stored TaskTerminalDeliveryClaim, fence app.TaskTerminalDeliveryModelFence,
	matchGeneration bool) bool {
	expected := taskTerminalDeliveryClaimFromModelFence(fence)
	if !sameTaskTerminalDeliveryModelIdentity(stored, expected) ||
		stored.ModelCallID != expected.ModelCallID {
		return false
	}
	return !matchGeneration || stored.ClaimGeneration == fence.ClaimGeneration
}

func lockTaskTerminalDeliveryModelSourceInTx(ctx context.Context, tx pgx.Tx,
	fence app.TaskTerminalDeliveryModelFence) (TaskTerminalDeliveryClaim, bool, error) {
	if ctx == nil || tx == nil || !app.ValidTaskTerminalDeliveryModelFence(fence) {
		return TaskTerminalDeliveryClaim{}, false,
			domain.InvalidArgument("task_terminal_delivery_model", "terminal delivery model source is invalid")
	}
	stored, _, found, err := loadTaskTerminalDeliveryClaimInTx(ctx, tx, fence.DeliveryID)
	if err != nil || !found {
		return TaskTerminalDeliveryClaim{}, false, err
	}
	if !taskTerminalDeliveryFenceMatches(stored, fence, false) {
		return stored, false, nil
	}
	current, err := taskTerminalDeliverySourceCurrentInTx(ctx, tx, stored)
	return stored, current, err
}

func validTerminalDeliverySettlementCall(call *app.ModelCallReservation, ownerID, callID string) bool {
	return call != nil && call.Source == app.ModelCallTerminalDelivery && call.TerminalDelivery != nil &&
		call.OwnerID == ownerID && call.CallID == callID && len(call.RequestDigest) == 32 &&
		app.ValidTaskTerminalDeliveryModelDispatchFence(*call.TerminalDelivery) &&
		call.TerminalDelivery.ModelCallID == callID && call.OwnerID == call.TerminalDelivery.InitiatorID
}

func verifyTerminalDeliveryLedgerAttribution(ctx context.Context, tx pgx.Tx, ownerID, callID, taskID string,
	digest []byte, storedDeliveryID *int64, storedClaimGeneration *int64, dispatchStarted bool,
	call *app.ModelCallReservation) error {
	if !validTerminalDeliverySettlementCall(call, ownerID, callID) || storedDeliveryID == nil ||
		storedClaimGeneration == nil || !dispatchStarted || *storedDeliveryID != call.TerminalDelivery.DeliveryID ||
		*storedClaimGeneration != call.TerminalDelivery.ClaimGeneration || taskID != call.TerminalDelivery.TaskID ||
		!bytes.Equal(digest, call.RequestDigest) {
		return domain.NewError(domain.CodeInvalidState, "terminal delivery model call attribution changed")
	}
	stored, _, err := lockTaskTerminalDeliveryModelSourceInTx(ctx, tx, *call.TerminalDelivery)
	if err != nil {
		return err
	}
	if !taskTerminalDeliveryFenceMatches(stored, *call.TerminalDelivery, false) {
		return domain.NewError(domain.CodeInvalidState, "terminal delivery model source attribution changed")
	}
	return nil
}

// storeTaskTerminalDeliveryModelResponseInTx writes an already encrypted
// response to the current outbox generation. The provider call is bound to
// the immutable delivery ID; its result may arrive after a lease reclaim, but
// it can never overwrite a different result or a published/invalidated row.
func storeTaskTerminalDeliveryModelResponseInTx(ctx context.Context, tx pgx.Tx,
	call *app.ModelCallReservation, ciphertext, ciphertextDigest []byte) (bool, error) {
	if call == nil || !validTerminalDeliverySettlementCall(call, call.OwnerID, call.CallID) || len(ciphertext) == 0 ||
		len(ciphertext) > app.MaxTaskTerminalDeliveryModelResponseCiphertext || len(ciphertextDigest) != 32 {
		return false, domain.InvalidArgument("task_terminal_delivery_model", "terminal delivery response storage is invalid")
	}
	digest := sha256.Sum256(ciphertext)
	if !bytes.Equal(digest[:], ciphertextDigest) {
		return false, domain.NewError(domain.CodeInvalidState, "terminal delivery encrypted response digest changed")
	}
	stored, current, err := lockTaskTerminalDeliveryModelSourceInTx(ctx, tx, *call.TerminalDelivery)
	if err != nil || !current {
		return false, err
	}
	switch stored.State {
	case TaskTerminalDeliveryStateReplyReady, TaskTerminalDeliveryStatePublished:
		if !bytes.Equal(stored.ResponseCiphertext, ciphertext) || !bytes.Equal(stored.ResponseSHA256, ciphertextDigest) {
			return false, domain.NewError(domain.CodeIdempotencyConflict, "terminal delivery response was settled differently")
		}
		return true, nil
	case TaskTerminalDeliveryStateClaimed, TaskTerminalDeliveryStateUnknown, "pending":
		result, err := tx.Exec(ctx, `UPDATE task_terminal_delivery_outbox
SET state='reply_ready',lease_owner=NULL,lease_expires_at=NULL,response_ciphertext=$3,response_sha256=$4,
    updated_at=clock_timestamp()
WHERE id=$1 AND claim_generation=$2 AND state IN ('claimed','unknown','pending')
  AND model_call_id=$5 AND response_ciphertext IS NULL AND response_sha256 IS NULL`,
			stored.ID, stored.ClaimGeneration, ciphertext, ciphertextDigest, stored.ModelCallID)
		if err != nil {
			return false, fmt.Errorf("persist terminal delivery settled model response: %w", err)
		}
		return result.RowsAffected() == 1, nil
	default:
		return false, nil
	}
}

func verifySettledTerminalDeliveryModelResponseInTx(ctx context.Context, tx pgx.Tx,
	call *app.ModelCallReservation, ciphertext, ciphertextDigest []byte, responseInvalid bool) error {
	if responseInvalid {
		if len(ciphertext) != 0 || len(ciphertextDigest) != 0 {
			return domain.NewError(domain.CodeIdempotencyConflict, "terminal delivery invalid response has ciphertext")
		}
		return nil
	}
	ok, err := storeTaskTerminalDeliveryModelResponseInTx(ctx, tx, call, ciphertext, ciphertextDigest)
	if err != nil {
		return err
	}
	if !ok {
		return domain.NewError(domain.CodeConflict, "terminal delivery model response source is no longer current")
	}
	return nil
}

func sameTaskTerminalDeliveryModelIdentity(left, right TaskTerminalDeliveryClaim) bool {
	return left.ID == right.ID && left.TaskID == right.TaskID && left.OwnerID == right.OwnerID &&
		left.ConversationID == right.ConversationID && left.InitiatorID == right.InitiatorID &&
		left.TaskRevision == right.TaskRevision && left.AuthorizationVersion == right.AuthorizationVersion &&
		left.SourceKind == right.SourceKind && left.SourceEventSequence == right.SourceEventSequence &&
		left.SourceEventType == right.SourceEventType && bytes.Equal(left.SourceSHA256, right.SourceSHA256) &&
		bytes.Equal(left.ResultSHA256, right.ResultSHA256) &&
		nullableInt64Equal(left.ValidationClaimGeneration, right.ValidationClaimGeneration) &&
		bytes.Equal(left.VerdictSHA256, right.VerdictSHA256)
}

func checkTaskTerminalDeliveryModelFenceInTx(ctx context.Context, tx pgx.Tx,
	fence app.TaskTerminalDeliveryModelFence) (string, bool, error) {
	if ctx == nil || tx == nil || !app.ValidTaskTerminalDeliveryModelDispatchFence(fence) {
		return "", false, domain.NewError(domain.CodeUnauthenticated, "terminal delivery model fence is invalid")
	}
	stored, leaseActive, found, err := loadTaskTerminalDeliveryClaimInTx(ctx, tx, fence.DeliveryID)
	if err != nil || !found {
		return "", false, err
	}
	if !taskTerminalDeliveryFenceMatches(stored, fence, true) || stored.State != TaskTerminalDeliveryStateClaimed ||
		!leaseActive || stored.LeaseOwner != fence.LeaseOwner || !stored.LeaseExpiresAt.Equal(fence.LeaseExpiresAt) {
		return "", false, nil
	}
	current, err := taskTerminalDeliverySourceCurrentInTx(ctx, tx, stored)
	if err != nil || !current {
		if err == nil {
			if _, updateErr := tx.Exec(ctx, `UPDATE task_terminal_delivery_outbox
SET state='invalidated',lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE id=$1 AND state='claimed' AND claim_generation=$2 AND lease_owner=$3`,
				stored.ID, stored.ClaimGeneration, stored.LeaseOwner); updateErr != nil {
				return "", false, fmt.Errorf("invalidate stale terminal delivery model claim: %w", updateErr)
			}
		}
		return "", false, err
	}
	var projectID string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(project_id,'') FROM tasks WHERE id=$1 AND owner_id=$2 AND initiator_id=$3`,
		fence.TaskID, fence.OwnerID, fence.InitiatorID).Scan(&projectID); err != nil {
		return "", false, domain.NewError(domain.CodeUnauthenticated, "terminal delivery Task project scope is unavailable")
	}
	return projectID, true, nil
}

func (s *Store) TaskTerminalDeliveryModelID(ctx context.Context,
	fence app.TaskTerminalDeliveryModelFence) (string, error) {
	if ctx == nil || !app.ValidTaskTerminalDeliveryModelDispatchFence(fence) {
		return "", domain.NewError(domain.CodeUnauthenticated, "terminal delivery model is unavailable")
	}
	var modelID string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if _, current, err := checkTaskTerminalDeliveryModelFenceInTx(ctx, tx, fence); err != nil {
			return err
		} else if !current {
			return domain.NewError(domain.CodeConflict, "terminal delivery model claim is no longer current")
		}
		if err := tx.QueryRow(ctx, `SELECT model_id FROM tasks WHERE id=$1 AND owner_id=$2 AND initiator_id=$3`,
			fence.TaskID, fence.OwnerID, fence.InitiatorID).Scan(&modelID); err != nil || !domain.ValidModelConnectionID(modelID) {
			return domain.NewError(domain.CodeNotFound, "terminal delivery Task model is unavailable")
		}
		return nil
	})
	return modelID, err
}

func (s *Store) ModelTaskProjectIDForTerminalDelivery(ctx context.Context,
	fence app.TaskTerminalDeliveryModelFence) (string, error) {
	if ctx == nil || !app.ValidTaskTerminalDeliveryModelDispatchFence(fence) {
		return "", domain.NewError(domain.CodeUnauthenticated, "terminal delivery model project scope is unavailable")
	}
	var projectID string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		var current bool
		var err error
		projectID, current, err = checkTaskTerminalDeliveryModelFenceInTx(ctx, tx, fence)
		if err != nil {
			return err
		}
		if !current {
			return domain.NewError(domain.CodeConflict, "terminal delivery model claim is no longer current")
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return projectID, nil
}

func (s *Store) RecoverTaskTerminalDeliveryModelResponse(ctx context.Context,
	fence app.TaskTerminalDeliveryModelFence) (app.TaskTerminalDeliveryModelResponseRecord, bool, error) {
	if ctx == nil || !app.ValidTaskTerminalDeliveryModelFence(fence) {
		return app.TaskTerminalDeliveryModelResponseRecord{}, false,
			domain.InvalidArgument("task_terminal_delivery_model", "terminal delivery recovery fence is invalid")
	}
	var record app.TaskTerminalDeliveryModelResponseRecord
	found := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, _, exists, err := loadTaskTerminalDeliveryClaimInTx(ctx, tx, fence.DeliveryID)
		if err != nil || !exists {
			return err
		}
		if !taskTerminalDeliveryFenceMatches(stored, fence, true) || fence.LeaseOwner != "" ||
			!fence.LeaseExpiresAt.IsZero() || stored.State != TaskTerminalDeliveryStateReplyReady ||
			stored.LeaseOwner != "" || !stored.LeaseExpiresAt.IsZero() || len(stored.ResponseCiphertext) == 0 ||
			len(stored.ResponseSHA256) != 32 {
			return nil
		}
		current, err := taskTerminalDeliverySourceCurrentInTx(ctx, tx, stored)
		if err != nil || !current {
			return err
		}
		var state string
		var modelID string
		var requestDigest []byte
		var deliveryID int64
		var dispatched bool
		if err := tx.QueryRow(ctx, `SELECT state,model_id,request_digest,terminal_delivery_id,terminal_delivery_dispatch_started
FROM model_call_ledger WHERE owner_id=$1 AND source='terminal_delivery' AND call_id=$2 FOR SHARE`,
			fence.InitiatorID, fence.ModelCallID).Scan(&state, &modelID, &requestDigest, &deliveryID, &dispatched); errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("read terminal delivery model response ledger: %w", err)
		}
		cipherDigest := sha256Sum(stored.ResponseCiphertext)
		if state != "settled" || !dispatched || deliveryID != fence.DeliveryID ||
			!domain.ValidModelConnectionID(modelID) || len(requestDigest) != 32 ||
			!bytes.Equal(cipherDigest, stored.ResponseSHA256) {
			return domain.NewError(domain.CodeInvalidState, "terminal delivery reply-ready response has no matching settled call")
		}
		record = app.TaskTerminalDeliveryModelResponseRecord{ModelID: modelID,
			RequestDigest: append([]byte(nil), requestDigest...), Ciphertext: append([]byte(nil), stored.ResponseCiphertext...),
			CiphertextSHA256: append([]byte(nil), stored.ResponseSHA256...)}
		found = true
		return nil
	})
	return record, found, err
}

func (s *Store) TaskTerminalDeliveryModelCallDisposition(ctx context.Context,
	fence app.TaskTerminalDeliveryModelFence) (app.TaskTerminalDeliveryModelCallDisposition, error) {
	if ctx == nil || !app.ValidTaskTerminalDeliveryModelFence(fence) {
		return app.TaskTerminalDeliveryModelStale,
			domain.InvalidArgument("task_terminal_delivery_model", "terminal delivery call-state fence is invalid")
	}
	disposition := app.TaskTerminalDeliveryModelStale
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, leaseActive, found, err := loadTaskTerminalDeliveryClaimInTx(ctx, tx, fence.DeliveryID)
		if err != nil || !found {
			return err
		}
		if !taskTerminalDeliveryFenceMatches(stored, fence, false) || stored.State == TaskTerminalDeliveryStatePublished ||
			stored.State == TaskTerminalDeliveryStateInvalidated {
			return nil
		}
		current, err := taskTerminalDeliverySourceCurrentInTx(ctx, tx, stored)
		if err != nil || !current {
			return err
		}
		activeFence := app.ValidTaskTerminalDeliveryModelDispatchFence(fence) && stored.State == TaskTerminalDeliveryStateClaimed &&
			leaseActive && stored.ClaimGeneration == fence.ClaimGeneration && stored.LeaseOwner == fence.LeaseOwner &&
			stored.LeaseExpiresAt.Equal(fence.LeaseExpiresAt)
		if fence.LeaseOwner == "" && stored.State == TaskTerminalDeliveryStateReplyReady && !leaseActive {
			activeFence = false
		}
		var state string
		var requestDigest []byte
		var terminalDeliveryID int64
		var dispatchStarted bool
		err = tx.QueryRow(ctx, `SELECT state,request_digest,terminal_delivery_id,terminal_delivery_dispatch_started
FROM model_call_ledger WHERE owner_id=$1 AND source='terminal_delivery' AND call_id=$2 FOR SHARE`,
			fence.InitiatorID, fence.ModelCallID).Scan(&state, &requestDigest, &terminalDeliveryID, &dispatchStarted)
		if errors.Is(err, pgx.ErrNoRows) {
			if activeFence {
				disposition = app.TaskTerminalDeliveryModelNotDispatched
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("read terminal delivery model call state: %w", err)
		}
		if terminalDeliveryID != fence.DeliveryID || len(requestDigest) != 32 {
			return domain.NewError(domain.CodeInvalidState, "terminal delivery model call attribution changed")
		}
		switch state {
		case "unknown":
			disposition = app.TaskTerminalDeliveryModelUnknown
		case "dispatched":
			if !dispatchStarted && activeFence {
				disposition = app.TaskTerminalDeliveryModelNotDispatched
			} else if dispatchStarted {
				disposition = app.TaskTerminalDeliveryModelUnknown
			}
		case "settled":
			if stored.State == TaskTerminalDeliveryStateReplyReady {
				disposition = app.TaskTerminalDeliveryModelReplyReady
			} else if stored.State == TaskTerminalDeliveryStateClaimed || stored.State == TaskTerminalDeliveryStateUnknown {
				disposition = app.TaskTerminalDeliveryModelFallbackSafe
			}
		default:
			return domain.NewError(domain.CodeInvalidState, "terminal delivery model call state is invalid")
		}
		return nil
	})
	return disposition, err
}

func (s *Store) beginTerminalDeliveryModelDispatchInTx(ctx context.Context, tx pgx.Tx,
	request app.ModelCallReservation) error {
	if !validTaskTerminalDeliveryModelCallReservation(request) {
		return domain.InvalidArgument("model_call", "terminal delivery model dispatch is invalid")
	}
	if _, current, err := checkTaskTerminalDeliveryModelFenceInTx(ctx, tx, *request.TerminalDelivery); err != nil {
		return err
	} else if !current {
		return domain.NewError(domain.CodeConflict, "terminal delivery model claim is no longer current")
	}
	var state, modelID string
	var requestDigest []byte
	var terminalDeliveryID int64
	var dispatchStarted bool
	err := tx.QueryRow(ctx, `SELECT state,model_id,request_digest,terminal_delivery_id,terminal_delivery_dispatch_started
FROM model_call_ledger WHERE owner_id=$1 AND source='terminal_delivery' AND call_id=$2 FOR UPDATE`,
		request.OwnerID, request.CallID).Scan(&state, &modelID, &requestDigest, &terminalDeliveryID, &dispatchStarted)
	if err != nil {
		return storeError("find terminal delivery model dispatch", "model call", err)
	}
	if state != "dispatched" || modelID != request.ModelID || terminalDeliveryID != request.TerminalDelivery.DeliveryID ||
		!bytes.Equal(requestDigest, request.RequestDigest) || dispatchStarted {
		return domain.NewError(domain.CodeConflict, "terminal delivery model call may already have been dispatched")
	}
	if _, err := tx.Exec(ctx, `UPDATE model_call_ledger SET terminal_delivery_claim_generation=$3,
terminal_delivery_dispatch_started=true,created_at=clock_timestamp()
WHERE owner_id=$1 AND source='terminal_delivery' AND call_id=$2 AND state='dispatched'
  AND terminal_delivery_dispatch_started=false`, request.OwnerID, request.CallID, request.TerminalDelivery.ClaimGeneration); err != nil {
		return fmt.Errorf("record terminal delivery model dispatch: %w", err)
	}
	return nil
}

func sha256Sum(value []byte) []byte {
	sum := sha256.Sum256(value)
	return sum[:]
}
