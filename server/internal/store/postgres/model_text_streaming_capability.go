package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
	"morde/server/internal/store/postgres/sqlcgen"
)

func (s *Store) BeginModelTextStreamingProbe(ctx context.Context, request app.ModelCallReservation) (bool, error) {
	if err := validateModelTextStreamingProbe(request); err != nil {
		return false, err
	}
	var replay bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, request.OwnerID); err != nil {
			return err
		}
		if err := checkModelCaller(ctx, tx, request); err != nil {
			return err
		}
		var checkedText bool
		if err := tx.QueryRow(ctx, `SELECT checked_at IS NOT NULL AND capability_text FROM model_connections WHERE id=$1`, request.ModelID).Scan(&checkedText); err != nil || !checkedText {
			return domain.NewError(domain.CodeUnsupported, "model text capability must be verified before streaming")
		}
		var priorAttempt, state, actor string
		var sameVersion bool
		err := tx.QueryRow(ctx, `SELECT attempt_id,state,actor_id,connection_version=$2 FROM model_text_streaming_capabilities
WHERE model_id=$1 FOR UPDATE`, request.ModelID, request.ConnectionVersion).Scan(&priorAttempt, &state, &actor, &sameVersion)
		if err == nil && sameVersion && actor == request.OwnerID && priorAttempt == request.CallID && state == "verified" {
			replay = true
			return nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read model streaming probe: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO model_text_streaming_capabilities(model_id,connection_version,actor_id,attempt_id,state,verified_at)
VALUES ($1,$2,$3,$4,'checking',NULL) ON CONFLICT (model_id) DO UPDATE SET connection_version=EXCLUDED.connection_version,
actor_id=EXCLUDED.actor_id,attempt_id=EXCLUDED.attempt_id,state='checking',verified_at=NULL,updated_at=clock_timestamp()`,
			request.ModelID, request.ConnectionVersion, request.OwnerID, request.CallID); err != nil {
			return fmt.Errorf("begin model streaming probe: %w", err)
		}
		return nil
	})
	return replay, err
}

func (s *Store) FinishModelTextStreamingProbe(ctx context.Context, request app.ModelCallReservation) error {
	if err := validateModelTextStreamingProbe(request); err != nil {
		return err
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, request.OwnerID); err != nil {
			return err
		}
		if err := checkModelCaller(ctx, tx, request); err != nil {
			return err
		}
		var settled bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_call_ledger WHERE owner_id=$1 AND source='probe'
 AND call_id=$2 AND model_id=$3 AND provider_model=$4 AND personal_model=$5 AND state='settled')`, request.OwnerID,
			request.CallID, request.ModelID, request.ProviderModel, request.PersonalModel).Scan(&settled); err != nil || !settled {
			return domain.NewError(domain.CodeInvalidState, "model streaming probe usage is not settled")
		}
		result, err := tx.Exec(ctx, `UPDATE model_text_streaming_capabilities SET state='verified',verified_at=clock_timestamp(),updated_at=clock_timestamp()
WHERE model_id=$1 AND connection_version=$2 AND actor_id=$3 AND attempt_id=$4 AND state='checking'`,
			request.ModelID, request.ConnectionVersion, request.OwnerID, request.CallID)
		if err != nil {
			return fmt.Errorf("finish model streaming probe: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "model streaming probe was superseded")
		}
		return nil
	})
}

func validateModelTextStreamingProbe(request app.ModelCallReservation) error {
	if request.Source != app.ModelCallProbe || request.Task != nil || request.OwnerID != request.Principal.UserID ||
		!domain.ValidUserID(request.OwnerID) || !domain.ValidModelConnectionID(request.ModelID) || request.ProviderModel == "" ||
		request.ConnectionVersion.IsZero() || len(request.CallID) != 70 || request.CallID[:6] != "probe_" {
		return domain.InvalidArgument("model_probe", "model streaming probe identity is invalid")
	}
	return nil
}

func (s *Store) TaskTextStreamingCapability(ctx context.Context, claimed task.Task) (bool, error) {
	if ctx == nil || !domain.ValidTaskID(claimed.ID) || !domain.ValidUserID(claimed.OwnerID) || !domain.ValidUserID(claimed.InitiatorID) ||
		!domain.ValidConversationID(claimed.ConversationID) || !domain.ValidModelConnectionID(claimed.ModelID) ||
		claimed.Lease.Owner == "" || claimed.Lease.Version < 1 || claimed.Authorization.Version < 1 {
		return false, domain.InvalidArgument("task", "task streaming identity is invalid")
	}
	var available bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		projectID, err := lockTaskInitiatorInTx(ctx, tx, claimed.ID, claimed.OwnerID, claimed.InitiatorID)
		if err != nil || projectID != claimed.ProjectID {
			return domain.NewError(domain.CodeUnauthenticated, "task streaming authorization is unavailable")
		}
		gate, err := requireOwnerExecutionEnabled(ctx, tx, claimed.InitiatorID, nil)
		if err != nil {
			return err
		}
		var id string
		err = tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 AND owner_id=$2 AND initiator_id=$3 AND conversation_id=$4
 AND project_id IS NOT DISTINCT FROM NULLIF($5,'') AND model_id=$6 AND status='running' AND execution_location='server'
 AND lease_owner=$7 AND lease_version=$8 AND lease_expires_at>clock_timestamp() AND auth_version=$9 AND execution_epoch=$10
 AND current_revision=desired_revision FOR UPDATE`, claimed.ID, claimed.OwnerID, claimed.InitiatorID, claimed.ConversationID,
			claimed.ProjectID, claimed.ModelID, claimed.Lease.Owner, claimed.Lease.Version, claimed.Authorization.Version, gate.epoch).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeUnauthenticated, "task streaming lease is unavailable")
		}
		if err != nil {
			return fmt.Errorf("lock task streaming authorization: %w", err)
		}
		stored, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: claimed.ID, OwnerID: claimed.OwnerID})
		if err != nil {
			return fmt.Errorf("read task streaming authorization: %w", err)
		}
		storedAuthorization := taskDomain(stored).Authorization
		claimedAuthorization := claimed.Authorization
		sameExpiry := storedAuthorization.ExpiresAt == nil && claimedAuthorization.ExpiresAt == nil ||
			storedAuthorization.ExpiresAt != nil && claimedAuthorization.ExpiresAt != nil && storedAuthorization.ExpiresAt.Equal(*claimedAuthorization.ExpiresAt)
		if storedAuthorization.Kind != claimedAuthorization.Kind || storedAuthorization.TokenID != claimedAuthorization.TokenID ||
			storedAuthorization.ClientInstallationID != claimedAuthorization.ClientInstallationID || storedAuthorization.Version != claimedAuthorization.Version ||
			!sameStringSlices(storedAuthorization.Scopes, claimedAuthorization.Scopes) || !sameExpiry {
			return domain.NewError(domain.CodeUnauthenticated, "task streaming authorization changed")
		}
		allowed, err := requireCurrentTaskAuthorization(ctx, tx, stored)
		if err != nil {
			return err
		}
		if !allowed {
			return domain.NewError(domain.CodeUnauthenticated, "task streaming authorization is unavailable")
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_connections model
JOIN model_text_streaming_capabilities proof ON proof.model_id=model.id AND proof.connection_version=model.updated_at AND proof.state='verified'
WHERE model.id=$1 AND model.checked_at IS NOT NULL AND model.capability_text AND (model.owner_id IS NULL OR model.owner_id=$2))`,
			claimed.ModelID, claimed.InitiatorID).Scan(&available); err != nil {
			return fmt.Errorf("read task streaming capability: %w", err)
		}
		return nil
	})
	return available, err
}

var _ app.ModelTextStreamingProbeStore = (*Store)(nil)
var _ app.ModelTaskTextStreamingCapabilityStore = (*Store)(nil)
var _ app.ModelTaskStreamingDispatchStore = (*Store)(nil)
