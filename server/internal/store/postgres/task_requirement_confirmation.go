package postgres

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/store/postgres/sqlcgen"
)

// ConfirmTaskRequirementManifest stores only the authenticated user's
// confirmation fact. The canonical candidate manifest remains in task.plan.
func (s *Store) ConfirmTaskRequirementManifest(ctx context.Context,
	command app.TaskRequirementManifestConfirmationCommand) (app.TaskRequirementManifestConfirmation, error) {
	if ctx == nil || !domain.ValidTaskID(command.TaskID) || !domain.ValidUserID(command.ActorUserID) ||
		!validTaskRequirementConfirmationPrincipalKind(command.ActorPrincipalKind) || command.TaskRevision < 0 ||
		command.PlanRevision < 1 || command.PlanEventSequence < 1 || !validTaskRequirementConfirmationDigest(command.ManifestDigest) ||
		!validProjectIdempotencyKey(command.IdempotencyKey) {
		return app.TaskRequirementManifestConfirmation{}, domain.InvalidArgument("task_requirement_manifest_confirmation", "confirmation identity is invalid")
	}
	var result app.TaskRequirementManifestConfirmation
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		// Match Task progress lock order: project Workspace/membership fence first,
		// then the Task row. The Task lock serializes confirmation against plan writes.
		workspaceFence, err := lockProjectTaskWorkspaceFence(ctx, tx, command.TaskID, false)
		if err != nil {
			return err
		}
		ownerID, err := taskOwnerIDForActor(ctx, tx, command.TaskID, command.ActorUserID, true, true)
		if err != nil {
			return err
		}
		if err := verifyProjectTaskWorkspaceFence(ctx, tx, command.TaskID, workspaceFence); err != nil {
			return err
		}
		row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: command.TaskID, OwnerID: ownerID})
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("read Task for requirement manifest confirmation: %w", err)
		}
		if row.CurrentRevision != row.DesiredRevision || row.CurrentRevision != command.TaskRevision || row.OwnerID != ownerID {
			return domain.NewError(domain.CodeConflict, "task requirement confirmation revision is stale")
		}
		if row.Status == "cancelling" || row.Status == "cancelled" {
			return domain.NewError(domain.CodeConflict, "cancelled Task cannot confirm its requirement manifest")
		}
		authorized, err := requireCurrentTaskAuthorization(ctx, tx, row)
		if err != nil {
			return err
		}
		if !authorized {
			return domain.NewError(domain.CodeUnauthenticated, "current Task authorization is unavailable")
		}

		var planEventSequence int64
		var payload []byte
		err = tx.QueryRow(ctx, `SELECT sequence,payload FROM task_events
WHERE task_id=$1 AND event_type='task.plan' AND payload #>> '{plan,task_revision}'=$2
ORDER BY sequence DESC LIMIT 1`, command.TaskID, fmt.Sprint(command.TaskRevision)).Scan(&planEventSequence, &payload)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "current task plan is unavailable for confirmation")
		}
		if err != nil {
			return fmt.Errorf("read current task plan for requirement confirmation: %w", err)
		}
		if planEventSequence != command.PlanEventSequence || len(payload) < 2 || len(payload) > 64<<10 {
			return domain.NewError(domain.CodeConflict, "task plan event changed before confirmation")
		}
		var event appTaskPlanEvent
		if json.Unmarshal(payload, &event) != nil || event.Plan.Validate() != nil || event.TaskRevision != command.TaskRevision ||
			event.PlanRevision != command.PlanRevision || event.Plan.TaskRevision != command.TaskRevision ||
			event.Plan.Revision != command.PlanRevision || event.Detail.AuthorizationVersion != row.AuthVersion ||
			event.Detail.CallID == "" || event.Detail.AgentSessionID == "" ||
			len(event.Detail.FingerprintSHA256) != 64 {
			return domain.NewError(domain.CodeInvalidState, "stored task plan confirmation identity is invalid")
		}
		commit := app.CloudDSHTaskPlanCommit{TaskID: command.TaskID, OwnerID: ownerID,
			AgentSessionID: event.Detail.AgentSessionID, AuthorizationVersion: event.Detail.AuthorizationVersion,
			CallID: event.Detail.CallID, Plan: event.Plan, FingerprintSHA256: event.Detail.FingerprintSHA256}
		if app.ValidateCloudDSHTaskPlanEvent(payload, commit) != nil {
			return domain.NewError(domain.CodeInvalidState, "stored task plan confirmation fingerprint is invalid")
		}
		manifestDigest, err := event.Plan.RequirementManifestDigest()
		if err != nil || !validTaskRequirementConfirmationDigest(manifestDigest) {
			return domain.NewError(domain.CodeInvalidState, "stored task requirement manifest is invalid")
		}
		expectedDigest, _ := hex.DecodeString(command.ManifestDigest)
		actualDigest, _ := hex.DecodeString(manifestDigest)
		if !bytes.Equal(expectedDigest, actualDigest) {
			return domain.NewError(domain.CodeConflict, "task requirement manifest digest changed before confirmation")
		}

		prior, found, err := readTaskRequirementManifestConfirmation(ctx, tx, ownerID, command.TaskID, command.IdempotencyKey)
		if err != nil {
			return err
		}
		if found {
			if sameTaskRequirementManifestConfirmation(prior, command, planEventSequence, manifestDigest, row.AuthVersion) {
				result = prior
				return nil
			}
			return domain.NewError(domain.CodeIdempotencyConflict, "task requirement confirmation idempotency key was reused")
		}
		var existingDigest []byte
		var existingRevision, existingPlanRevision int64
		var existingOwner string
		err = tx.QueryRow(ctx, `SELECT owner_id,task_revision,plan_revision,manifest_digest FROM task_requirement_manifest_confirmations
WHERE task_id=$1 AND plan_event_sequence=$2`, command.TaskID, planEventSequence).
			Scan(&existingOwner, &existingRevision, &existingPlanRevision, &existingDigest)
		if err == nil {
			if existingOwner != ownerID || existingRevision != command.TaskRevision || existingPlanRevision != command.PlanRevision ||
				!bytes.Equal(existingDigest, actualDigest) {
				return domain.NewError(domain.CodeInvalidState, "stored task requirement confirmation does not match its plan")
			}
			return domain.NewError(domain.CodeConflict, "current task plan was already confirmed with another idempotency key")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read existing task requirement confirmation: %w", err)
		}

		var confirmedAt time.Time
		err = tx.QueryRow(ctx, `INSERT INTO task_requirement_manifest_confirmations
  (task_id,owner_id,task_revision,plan_revision,plan_event_sequence,manifest_digest,confirmed_by_user_id,
   confirmed_by_principal_kind,task_authorization_version,idempotency_key)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
RETURNING confirmed_at`, command.TaskID, ownerID, command.TaskRevision, command.PlanRevision, planEventSequence,
			actualDigest, command.ActorUserID, command.ActorPrincipalKind, row.AuthVersion, command.IdempotencyKey).Scan(&confirmedAt)
		if err != nil {
			return fmt.Errorf("persist task requirement manifest confirmation: %w", err)
		}
		result = app.TaskRequirementManifestConfirmation{TaskID: command.TaskID, TaskRevision: command.TaskRevision,
			PlanRevision: command.PlanRevision, PlanEventSequence: planEventSequence, ManifestDigest: manifestDigest,
			ConfirmedByUserID: command.ActorUserID, ConfirmedByPrincipalKind: command.ActorPrincipalKind,
			TaskAuthorizationVersion: row.AuthVersion, ConfirmedAt: confirmedAt}
		return nil
	})
	if err != nil {
		return app.TaskRequirementManifestConfirmation{}, err
	}
	return result, nil
}

func readTaskRequirementManifestConfirmation(ctx context.Context, tx pgx.Tx, ownerID, taskID, idempotencyKey string) (
	app.TaskRequirementManifestConfirmation, bool, error) {
	var result app.TaskRequirementManifestConfirmation
	var digest []byte
	err := tx.QueryRow(ctx, `SELECT task_id,task_revision,plan_revision,plan_event_sequence,manifest_digest,
confirmed_by_user_id,confirmed_by_principal_kind,task_authorization_version,confirmed_at
FROM task_requirement_manifest_confirmations WHERE owner_id=$1 AND task_id=$2 AND idempotency_key=$3`,
		ownerID, taskID, idempotencyKey).Scan(&result.TaskID, &result.TaskRevision, &result.PlanRevision,
		&result.PlanEventSequence, &digest, &result.ConfirmedByUserID, &result.ConfirmedByPrincipalKind,
		&result.TaskAuthorizationVersion, &result.ConfirmedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.TaskRequirementManifestConfirmation{}, false, nil
	}
	if err != nil {
		return app.TaskRequirementManifestConfirmation{}, false, fmt.Errorf("read task requirement confirmation idempotency record: %w", err)
	}
	if len(digest) != 32 {
		return app.TaskRequirementManifestConfirmation{}, false, domain.NewError(domain.CodeInvalidState, "stored task requirement confirmation digest is invalid")
	}
	result.ManifestDigest = hex.EncodeToString(digest)
	return result, true, nil
}

// readTaskRequirementManifestConfirmationForPlan returns a confirmation only
// when every persisted plan fence and the caller's current Task authorization
// version match. Callers must still validate current authorization in their
// transaction; an unchanged version alone does not prove that a credential is
// still active. This helper is intended for same-transaction verification
// consumers and fails closed by returning found=false for stale bindings.
func readTaskRequirementManifestConfirmationForPlan(ctx context.Context, tx pgx.Tx, ownerID, taskID string,
	taskRevision, planRevision, planEventSequence, taskAuthorizationVersion int64, manifestDigest string) (
	app.TaskRequirementManifestConfirmation, bool, error) {
	if ctx == nil || tx == nil || !domain.ValidUserID(ownerID) || !domain.ValidTaskID(taskID) || taskRevision < 0 ||
		planRevision < 1 || planEventSequence < 1 || taskAuthorizationVersion < 1 ||
		!validTaskRequirementConfirmationDigest(manifestDigest) {
		return app.TaskRequirementManifestConfirmation{}, false, domain.InvalidArgument("task_requirement_manifest_confirmation", "confirmation lookup fence is invalid")
	}
	digest, _ := hex.DecodeString(manifestDigest)
	var result app.TaskRequirementManifestConfirmation
	var storedDigest []byte
	err := tx.QueryRow(ctx, `SELECT task_id,task_revision,plan_revision,plan_event_sequence,manifest_digest,
confirmed_by_user_id,confirmed_by_principal_kind,task_authorization_version,confirmed_at
FROM task_requirement_manifest_confirmations
WHERE owner_id=$1 AND task_id=$2 AND task_revision=$3 AND plan_revision=$4 AND plan_event_sequence=$5
  AND task_authorization_version=$6 AND manifest_digest=$7`, ownerID, taskID, taskRevision, planRevision,
		planEventSequence, taskAuthorizationVersion, digest).Scan(&result.TaskID, &result.TaskRevision,
		&result.PlanRevision, &result.PlanEventSequence, &storedDigest, &result.ConfirmedByUserID,
		&result.ConfirmedByPrincipalKind, &result.TaskAuthorizationVersion, &result.ConfirmedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.TaskRequirementManifestConfirmation{}, false, nil
	}
	if err != nil {
		return app.TaskRequirementManifestConfirmation{}, false, fmt.Errorf("read task requirement confirmation for plan: %w", err)
	}
	if len(storedDigest) != 32 || !bytes.Equal(storedDigest, digest) ||
		!domain.ValidUserID(result.ConfirmedByUserID) || !validTaskRequirementConfirmationPrincipalKind(result.ConfirmedByPrincipalKind) ||
		result.TaskAuthorizationVersion != taskAuthorizationVersion || result.ConfirmedAt.IsZero() {
		return app.TaskRequirementManifestConfirmation{}, false, domain.NewError(domain.CodeInvalidState, "stored task requirement confirmation is invalid")
	}
	result.ManifestDigest = hex.EncodeToString(storedDigest)
	return result, true, nil
}

func sameTaskRequirementManifestConfirmation(prior app.TaskRequirementManifestConfirmation,
	command app.TaskRequirementManifestConfirmationCommand, eventSequence int64, digest string,
	authorizationVersion int64) bool {
	return prior.TaskID == command.TaskID && prior.TaskRevision == command.TaskRevision && prior.PlanRevision == command.PlanRevision &&
		prior.PlanEventSequence == eventSequence && prior.ManifestDigest == digest && prior.ConfirmedByUserID == command.ActorUserID &&
		prior.ConfirmedByPrincipalKind == command.ActorPrincipalKind && prior.TaskAuthorizationVersion == authorizationVersion
}

func validTaskRequirementConfirmationDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
}

func validTaskRequirementConfirmationPrincipalKind(value string) bool {
	switch identity.Kind(value) {
	case identity.KindSession, identity.KindAPIKey, identity.KindClient, identity.KindDevice:
		return true
	default:
		return false
	}
}
