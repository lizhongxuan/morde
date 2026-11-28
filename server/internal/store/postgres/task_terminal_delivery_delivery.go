package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/store/postgres/sqlcgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Store) ClaimTaskTerminalDeliverySources(ctx context.Context, workerID string,
	lease time.Duration, limit int32) ([]app.TaskTerminalDeliveryClaim, error) {
	claims, err := s.ClaimTaskTerminalDeliveryOutbox(ctx, workerID, lease, limit)
	if err != nil {
		return nil, err
	}
	result := make([]app.TaskTerminalDeliveryClaim, 0, len(claims))
	for _, claim := range claims {
		result = append(result, appTaskTerminalDeliveryClaim(claim))
	}
	return result, nil
}

func (s *Store) ListTaskTerminalDeliveryReplyReady(ctx context.Context, limit int32) ([]app.TaskTerminalDeliveryClaim, error) {
	claims, err := s.ListTaskTerminalDeliveryRepliesReady(ctx, limit)
	if err != nil {
		return nil, err
	}
	result := make([]app.TaskTerminalDeliveryClaim, 0, len(claims))
	for _, claim := range claims {
		result = append(result, appTaskTerminalDeliveryClaim(claim))
	}
	return result, nil
}

func (s *Store) GetTaskTerminalDeliveryReplyReady(ctx context.Context, deliveryID int64) (app.TaskTerminalDeliveryClaim, bool, error) {
	if ctx == nil || deliveryID < 1 {
		return app.TaskTerminalDeliveryClaim{}, false,
			domain.InvalidArgument("task_terminal_delivery_claim", "reply-ready delivery ID is invalid")
	}
	var claim TaskTerminalDeliveryClaim
	found := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := invalidateStaleTaskTerminalDeliveriesInTx(ctx, tx); err != nil {
			return err
		}
		query := `SELECT ` + taskTerminalDeliverySelectColumns + `,COALESCE(delivery.lease_expires_at>clock_timestamp(),false)
FROM task_terminal_delivery_outbox delivery
JOIN tasks task ON task.id=delivery.task_id AND task.owner_id=delivery.owner_id
JOIN task_events event ON event.task_id=delivery.task_id AND event.sequence=delivery.source_event_sequence
WHERE delivery.id=$1 AND delivery.state='reply_ready' AND ` + taskTerminalDeliverySourceCurrentPredicate + `
FOR UPDATE OF delivery,task,event`
		loaded, leaseActive, err := scanTaskTerminalDeliveryClaim(tx.QueryRow(ctx, query, deliveryID), true)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read reply-ready Task terminal delivery: %w", err)
		}
		if !validTaskTerminalDeliveryClaim(loaded) || leaseActive || loaded.State != TaskTerminalDeliveryStateReplyReady {
			return domain.NewError(domain.CodeInvalidState, "stored reply-ready Task terminal delivery is invalid")
		}
		current, err := taskTerminalDeliveryCurrentAndAuthorizedInTx(ctx, tx, loaded)
		if err != nil {
			return err
		}
		if !current {
			if err := invalidateTaskTerminalDeliveryInTx(ctx, tx, loaded); err != nil {
				return err
			}
			return nil
		}
		claim, found = loaded, true
		return nil
	})
	if err != nil || !found {
		return app.TaskTerminalDeliveryClaim{}, found, err
	}
	return appTaskTerminalDeliveryClaim(claim), true, nil
}

func (s *Store) ReadTaskTerminalDeliverySourceFacts(ctx context.Context,
	claim app.TaskTerminalDeliveryClaim) (app.TaskTerminalDeliverySourceFacts, bool, error) {
	if ctx == nil || !app.ValidTaskTerminalDeliveryClaim(claim) ||
		(claim.State != app.TaskTerminalDeliveryStateClaimed && claim.State != app.TaskTerminalDeliveryStateReplyReady) {
		return app.TaskTerminalDeliverySourceFacts{}, false,
			domain.InvalidArgument("task_terminal_delivery_facts", "terminal delivery claim is invalid")
	}
	storedClaim := postgresTaskTerminalDeliveryClaim(claim)
	var facts app.TaskTerminalDeliverySourceFacts
	found := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, leaseActive, exists, err := loadTaskTerminalDeliveryClaimInTx(ctx, tx, storedClaim.ID)
		if err != nil || !exists {
			return err
		}
		if !sameTaskTerminalDeliverySource(stored, storedClaim) || stored.State != claim.State ||
			stored.State == TaskTerminalDeliveryStateClaimed && (!leaseActive || stored.LeaseOwner != storedClaim.LeaseOwner ||
				!stored.LeaseExpiresAt.Equal(storedClaim.LeaseExpiresAt)) ||
			stored.State == TaskTerminalDeliveryStateReplyReady && (stored.ResponseKind != storedClaim.ResponseKind ||
				!bytes.Equal(stored.ResponseCiphertext, storedClaim.ResponseCiphertext) ||
				!bytes.Equal(stored.ResponseSHA256, storedClaim.ResponseSHA256)) {
			return nil
		}
		current, err := taskTerminalDeliveryCurrentAndAuthorizedInTx(ctx, tx, stored)
		if err != nil {
			return err
		}
		if !current {
			if err := invalidateTaskTerminalDeliveryInTx(ctx, tx, stored); err != nil {
				return err
			}
			return nil
		}
		facts, err = readTaskTerminalDeliveryFactsInTx(ctx, tx, stored)
		if err != nil {
			return err
		}
		if !app.ValidTaskTerminalDeliverySourceFacts(claim, facts) {
			return domain.NewError(domain.CodeInvalidState, "verified Task terminal delivery facts are invalid")
		}
		found = true
		return nil
	})
	if err != nil {
		return app.TaskTerminalDeliverySourceFacts{}, false, err
	}
	return facts, found, nil
}

func (s *Store) MarkTaskTerminalDeliveryUnknownClaim(ctx context.Context, claim app.TaskTerminalDeliveryClaim) (bool, error) {
	if ctx == nil || !app.ValidTaskTerminalDeliveryClaim(claim) || claim.State != app.TaskTerminalDeliveryStateClaimed {
		return false, domain.InvalidArgument("task_terminal_delivery_claim", "active terminal delivery claim is invalid")
	}
	return s.MarkTaskTerminalDeliveryUnknown(ctx, postgresTaskTerminalDeliveryClaim(claim))
}

func (s *Store) PersistTaskTerminalDeliveryReplyReady(ctx context.Context, claim app.TaskTerminalDeliveryClaim,
	ciphertext []byte) (app.TaskTerminalDeliveryClaim, bool, error) {
	if ctx == nil || !app.ValidTaskTerminalDeliveryClaim(claim) || claim.State != app.TaskTerminalDeliveryStateClaimed {
		return app.TaskTerminalDeliveryClaim{}, false,
			domain.InvalidArgument("task_terminal_delivery_claim", "active terminal delivery claim is invalid")
	}
	ready, changed, err := s.MarkTaskTerminalDeliveryReplyReady(ctx, postgresTaskTerminalDeliveryClaim(claim), ciphertext)
	if err != nil {
		return app.TaskTerminalDeliveryClaim{}, false, err
	}
	return appTaskTerminalDeliveryClaim(ready), changed, nil
}

func (s *Store) InvalidateTaskTerminalDeliveryClaim(ctx context.Context, claim app.TaskTerminalDeliveryClaim) (bool, error) {
	if ctx == nil || !app.ValidTaskTerminalDeliveryClaim(claim) ||
		(claim.State != app.TaskTerminalDeliveryStateClaimed && claim.State != app.TaskTerminalDeliveryStateReplyReady) {
		return false, domain.InvalidArgument("task_terminal_delivery_claim", "terminal delivery claim is invalid")
	}
	requested := postgresTaskTerminalDeliveryClaim(claim)
	var invalidated bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, leaseActive, found, err := loadTaskTerminalDeliveryClaimInTx(ctx, tx, requested.ID)
		if err != nil || !found {
			return err
		}
		if !sameTaskTerminalDeliverySource(stored, requested) || stored.State != requested.State {
			return nil
		}
		if stored.State == TaskTerminalDeliveryStateClaimed && (!leaseActive || stored.LeaseOwner != requested.LeaseOwner ||
			!stored.LeaseExpiresAt.Equal(requested.LeaseExpiresAt)) ||
			stored.State == TaskTerminalDeliveryStateReplyReady && (stored.ResponseKind != requested.ResponseKind ||
				!bytes.Equal(stored.ResponseCiphertext, requested.ResponseCiphertext) ||
				!bytes.Equal(stored.ResponseSHA256, requested.ResponseSHA256)) {
			return nil
		}
		if err := invalidateTaskTerminalDeliveryInTx(ctx, tx, stored); err != nil {
			return err
		}
		invalidated = true
		return nil
	})
	return invalidated, err
}

// PublishTaskTerminalDeliveryReply inserts the assistant message and marks its
// durable source published in one transaction. The outbox row is locked before
// the insert, so concurrent restart recovery returns the first message ID.
func (s *Store) PublishTaskTerminalDeliveryReply(ctx context.Context, ready app.TaskTerminalDeliveryClaim,
	content string) (bool, string, error) {
	if ctx == nil || !app.ValidTaskTerminalDeliveryClaim(ready) || ready.State != app.TaskTerminalDeliveryStateReplyReady ||
		!validTerminalDeliveryMessage(content) {
		return false, "", domain.InvalidArgument("task_terminal_delivery_publish", "reply-ready terminal delivery is invalid")
	}
	requested := postgresTaskTerminalDeliveryClaim(ready)
	var published bool
	var messageID string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		stored, leaseActive, found, err := loadTaskTerminalDeliveryClaimInTx(ctx, tx, requested.ID)
		if err != nil || !found {
			return err
		}
		if !sameTaskTerminalDeliverySource(stored, requested) {
			return nil
		}
		if stored.State == TaskTerminalDeliveryStatePublished {
			if stored.PublishedMessageID == "" || stored.ResponseKind != requested.ResponseKind ||
				!bytes.Equal(stored.ResponseCiphertext, requested.ResponseCiphertext) ||
				!bytes.Equal(stored.ResponseSHA256, requested.ResponseSHA256) {
				return domain.NewError(domain.CodeIdempotencyConflict, "Task terminal delivery was published with another response")
			}
			matches, err := taskTerminalDeliveryPublishedMessageMatchesInTx(ctx, tx, stored, content)
			if err != nil {
				return err
			}
			if !matches {
				return domain.NewError(domain.CodeIdempotencyConflict, "published Task terminal delivery message changed")
			}
			messageID = stored.PublishedMessageID
			return nil
		}
		if stored.State != TaskTerminalDeliveryStateReplyReady || leaseActive || stored.LeaseOwner != "" ||
			stored.ResponseKind != requested.ResponseKind ||
			!bytes.Equal(stored.ResponseCiphertext, requested.ResponseCiphertext) ||
			!bytes.Equal(stored.ResponseSHA256, requested.ResponseSHA256) {
			return nil
		}
		var conversationID string
		if err := tx.QueryRow(ctx, `SELECT id FROM conversations WHERE id=$1 AND owner_id=$2 FOR UPDATE`,
			stored.ConversationID, stored.OwnerID).Scan(&conversationID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return invalidateTaskTerminalDeliveryInTx(ctx, tx, stored)
			}
			return fmt.Errorf("lock conversation before Task terminal delivery: %w", err)
		}
		current, err := taskTerminalDeliveryCurrentAndAuthorizedInTx(ctx, tx, stored)
		if err != nil {
			return err
		}
		if !current {
			return invalidateTaskTerminalDeliveryInTx(ctx, tx, stored)
		}
		steered, err := taskTerminalDeliveryHasLaterConversationInputInTx(ctx, tx, stored)
		if err != nil {
			return err
		}
		if steered {
			return invalidateTaskTerminalDeliveryInTx(ctx, tx, stored)
		}
		messageID, err = domain.NewMessageID()
		if err != nil {
			return fmt.Errorf("generate Task terminal delivery message ID: %w", err)
		}
		message := conversation.Message{ID: messageID, OwnerID: stored.OwnerID, ActorID: stored.InitiatorID,
			ConversationID: stored.ConversationID, Role: conversation.RoleAssistant, Content: content,
			TaskID: stored.TaskID, SourceKind: "task", DeliveryStatus: conversation.DeliveryApplied}
		if err := insertMessage(ctx, tx, message); err != nil {
			return err
		}
		changed, err := s.MarkTaskTerminalDeliveryPublishedInTx(ctx, tx, stored, messageID)
		if err != nil {
			return err
		}
		if !changed {
			return domain.NewError(domain.CodeConflict, "Task terminal delivery changed before message publication")
		}
		if err := recordAccountObjectChanges(ctx, tx,
			accountObjectChange{OwnerID: stored.OwnerID, ObjectType: "message", ObjectID: messageID, Operation: "upsert"},
			accountObjectChange{OwnerID: stored.OwnerID, ObjectType: "conversation", ObjectID: stored.ConversationID, Operation: "upsert"}); err != nil {
			return err
		}
		published = true
		return nil
	})
	if err != nil {
		return false, "", err
	}
	return published, messageID, nil
}

func appTaskTerminalDeliveryClaim(claim TaskTerminalDeliveryClaim) app.TaskTerminalDeliveryClaim {
	return app.TaskTerminalDeliveryClaim{TaskTerminalDeliveryIdentity: app.TaskTerminalDeliveryIdentity{
		DeliveryID: claim.ID, TaskID: claim.TaskID, OwnerID: claim.OwnerID, ConversationID: claim.ConversationID,
		InitiatorID: claim.InitiatorID, TaskRevision: claim.TaskRevision, AuthorizationVersion: claim.AuthorizationVersion,
		SourceKind: claim.SourceKind, SourceEventSequence: claim.SourceEventSequence, SourceEventType: claim.SourceEventType,
		SourceSHA256: append([]byte(nil), claim.SourceSHA256...), ResultSHA256: append([]byte(nil), claim.ResultSHA256...),
		ValidationClaimGeneration: cloneTerminalDeliveryGeneration(claim.ValidationClaimGeneration),
		VerdictSHA256:             append([]byte(nil), claim.VerdictSHA256...),
	}, State: claim.State, ClaimGeneration: claim.ClaimGeneration, LeaseOwner: claim.LeaseOwner,
		LeaseExpiresAt: claim.LeaseExpiresAt, ModelCallID: claim.ModelCallID,
		ResponseKind:       claim.ResponseKind,
		ResponseCiphertext: append([]byte(nil), claim.ResponseCiphertext...), ResponseSHA256: append([]byte(nil), claim.ResponseSHA256...),
		PublishedMessageID: claim.PublishedMessageID}
}

func postgresTaskTerminalDeliveryClaim(claim app.TaskTerminalDeliveryClaim) TaskTerminalDeliveryClaim {
	return TaskTerminalDeliveryClaim{ID: claim.DeliveryID, TaskID: claim.TaskID, OwnerID: claim.OwnerID,
		ConversationID: claim.ConversationID, InitiatorID: claim.InitiatorID, TaskRevision: claim.TaskRevision,
		AuthorizationVersion: claim.AuthorizationVersion, SourceKind: claim.SourceKind,
		SourceEventSequence: claim.SourceEventSequence, SourceEventType: claim.SourceEventType,
		SourceSHA256: append([]byte(nil), claim.SourceSHA256...), ResultSHA256: append([]byte(nil), claim.ResultSHA256...),
		ValidationClaimGeneration: cloneTerminalDeliveryGeneration(claim.ValidationClaimGeneration),
		VerdictSHA256:             append([]byte(nil), claim.VerdictSHA256...), State: claim.State,
		ClaimGeneration: claim.ClaimGeneration, LeaseOwner: claim.LeaseOwner, LeaseExpiresAt: claim.LeaseExpiresAt,
		ModelCallID: claim.ModelCallID, ResponseKind: claim.ResponseKind,
		ResponseCiphertext: append([]byte(nil), claim.ResponseCiphertext...),
		ResponseSHA256:     append([]byte(nil), claim.ResponseSHA256...), PublishedMessageID: claim.PublishedMessageID}
}

func cloneTerminalDeliveryGeneration(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func taskTerminalDeliveryCurrentAndAuthorizedInTx(ctx context.Context, tx pgx.Tx,
	claim TaskTerminalDeliveryClaim) (bool, error) {
	current, err := taskTerminalDeliverySourceCurrentInTx(ctx, tx, claim)
	if err != nil || !current {
		return current, err
	}
	row, err := sqlcgen.New(tx).GetTask(ctx, sqlcgen.GetTaskParams{ID: claim.TaskID, OwnerID: claim.OwnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read current Task authorization for terminal delivery: %w", err)
	}
	if row.AuthVersion != claim.AuthorizationVersion || row.ConversationID != claim.ConversationID || row.InitiatorID != claim.InitiatorID {
		return false, nil
	}
	return requireTaskTerminalDeliveryAuthorization(ctx, tx, row)
}

// A terminal explanation only writes a bounded assistant message to the
// conversation already bound to the stopped Task. It does not resume Task
// work or dispatch tools, so expiration of the original token alone must not
// suppress that explanation. Keep account, project, token identity, revocation,
// installation, and the immutable delivery fences checked as usual.
func requireTaskTerminalDeliveryAuthorization(ctx context.Context, tx pgx.Tx, value sqlcgen.Task) (bool, error) {
	if value.AuthTokenID == "" || value.AuthVersion < 1 || !domain.ValidUserID(value.InitiatorID) {
		return false, nil
	}
	if projectWorkspaceAuthorizationPermit(ctx, value) {
		return requireTaskTerminalDeliveryCredentialAuthorization(ctx, tx, value, false)
	}
	projectID, err := lockTaskInitiatorInTx(ctx, tx, value.ID, value.OwnerID, value.InitiatorID)
	if err != nil {
		switch domain.CodeOf(err) {
		case domain.CodeUnauthenticated, domain.CodeForbidden, domain.CodeNotFound:
			return false, nil
		default:
			return false, err
		}
	}
	if value.ProjectID.Valid != (projectID != "") || value.ProjectID.Valid && value.ProjectID.String != projectID {
		return false, nil
	}
	return requireTaskTerminalDeliveryCredentialAuthorization(ctx, tx, value, true)
}

func requireTaskTerminalDeliveryCredentialAuthorization(ctx context.Context, tx pgx.Tx, value sqlcgen.Task,
	lockRows bool) (bool, error) {
	// Scheduled Tasks are bound to a durable schedule run, not a user token
	// expiry. Reuse the standard check for their run linkage and account state.
	if value.AuthKind == string(identity.KindScheduleTask) {
		return requireCurrentTaskCredentialAuthorization(ctx, tx, value, lockRows)
	}
	lock := ""
	if lockRows {
		lock = " FOR SHARE"
	}
	var approved bool
	err := tx.QueryRow(ctx, `SELECT status='approved' FROM users WHERE id=$1`+lock, value.InitiatorID).Scan(&approved)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !approved {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check terminal delivery account authorization: %w", err)
	}
	var kind string
	var installationID pgtype.Text
	var revokedAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `SELECT kind,client_installation_id,revoked_at
FROM tokens WHERE id=$1 AND owner_id=$2`+lock, value.AuthTokenID, value.InitiatorID).
		Scan(&kind, &installationID, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check terminal delivery source token: %w", err)
	}
	if kind != value.AuthKind || kind == string(identity.KindClient) && !installationID.Valid ||
		installationID.Valid != (value.AuthClientInstallationID.String != "") ||
		installationID.Valid && installationID.String != value.AuthClientInstallationID.String {
		return false, nil
	}
	if revokedAt.Valid && (!installationID.Valid || !value.CreatedAt.Time.Before(revokedAt.Time)) {
		return false, nil
	}
	if installationID.Valid {
		var installationActive bool
		err := tx.QueryRow(ctx, `SELECT COALESCE(expires_at>clock_timestamp(),false) AND revoked_at IS NULL
FROM client_installations WHERE id=$1 AND owner_id=$2`+lock, installationID.String, value.InitiatorID).
			Scan(&installationActive)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !installationActive {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("check terminal delivery client installation: %w", err)
		}
	}
	return true, nil
}

func invalidateTaskTerminalDeliveryInTx(ctx context.Context, tx pgx.Tx, claim TaskTerminalDeliveryClaim) error {
	result, err := tx.Exec(ctx, `UPDATE task_terminal_delivery_outbox
SET state='invalidated',lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
WHERE id=$1 AND state IN ('claimed','reply_ready') AND claim_generation=$2`, claim.ID, claim.ClaimGeneration)
	if err != nil {
		return fmt.Errorf("invalidate stale Task terminal delivery: %w", err)
	}
	_ = result
	return nil
}

func readTaskTerminalDeliveryFactsInTx(ctx context.Context, tx pgx.Tx,
	claim TaskTerminalDeliveryClaim) (app.TaskTerminalDeliverySourceFacts, error) {
	facts := app.TaskTerminalDeliverySourceFacts{Identity: appTaskTerminalDeliveryClaim(claim).TaskTerminalDeliveryIdentity}
	var objective, result, status, waitingReason, modelID string
	var eventPayload []byte
	err := tx.QueryRow(ctx, `SELECT task.objective,task.result,task.status,task.waiting_reason,task.model_id,event.payload,
       step_counts.total,step_counts.succeeded,step_counts.failed,step_counts.cancelled,step_counts.unconfirmed
FROM tasks task JOIN task_events event ON event.task_id=task.id AND event.sequence=$3
JOIN LATERAL (
  SELECT count(*) AS total,
         count(*) FILTER (WHERE step.status='succeeded') AS succeeded,
         count(*) FILTER (WHERE step.status='failed') AS failed,
         count(*) FILTER (WHERE step.status='cancelled') AS cancelled,
         count(*) FILTER (WHERE step.status IN ('queued','running','waiting_user','unknown')) AS unconfirmed
  FROM task_steps step WHERE step.task_id=task.id
    AND (step.task_revision=task.current_revision OR (task.current_revision=0 AND step.task_revision IS NULL))
) step_counts ON TRUE
WHERE task.id=$1 AND task.owner_id=$2 AND task.conversation_id=$4 AND task.initiator_id=$5
  AND task.current_revision=$6 AND task.desired_revision=$6 AND task.auth_version=$7
  AND event.event_type=$8 AND sha256(convert_to(event.payload::text,'UTF8'))=$9
FOR SHARE OF task,event`, claim.TaskID, claim.OwnerID, claim.SourceEventSequence, claim.ConversationID,
		claim.InitiatorID, claim.TaskRevision, claim.AuthorizationVersion, claim.SourceEventType, claim.SourceSHA256).
		Scan(&objective, &result, &status, &waitingReason, &modelID, &eventPayload,
			&facts.StepCounts.Total, &facts.StepCounts.Succeeded, &facts.StepCounts.Failed,
			&facts.StepCounts.Cancelled, &facts.StepCounts.Unconfirmed)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.TaskTerminalDeliverySourceFacts{}, domain.NewError(domain.CodeConflict, "Task terminal delivery source changed")
	}
	if err != nil {
		return app.TaskTerminalDeliverySourceFacts{}, fmt.Errorf("read authorized Task terminal delivery source facts: %w", err)
	}
	facts.TaskStatus = status
	facts.ModelID = modelID
	facts.ObjectiveSummary, facts.ObjectiveTruncated = boundedTerminalDeliveryObjective(objective)
	objectiveDigest := sha256.Sum256([]byte(objective))
	facts.ObjectiveSHA256 = objectiveDigest[:]
	switch claim.SourceKind {
	case TaskTerminalDeliverySourceFailure:
		facts.FailureCode = safeTaskTerminalFailureCode(eventPayload)
	case TaskTerminalDeliverySourceCancellation:
	case TaskTerminalDeliverySourceWaiting:
		facts.WaitingReason = safeTaskTerminalWaitingReason(waitingReason)
		facts.RecoveryDiagnosis = app.TaskTerminalDeliveryRecoveryDiagnosisFromEvent(eventPayload)
	case TaskTerminalDeliverySourceValidation:
		validation, err := readTaskTerminalDeliveryValidationFactsInTx(ctx, tx, claim, result)
		if err != nil {
			return app.TaskTerminalDeliverySourceFacts{}, err
		}
		facts.ValidationState = validation.State
		facts.ValidationFindings = validation.Findings
	default:
		return app.TaskTerminalDeliverySourceFacts{}, domain.NewError(domain.CodeInvalidState, "Task terminal delivery source kind is invalid")
	}
	digest, err := app.TaskTerminalDeliveryFactsDigest(facts)
	if err != nil {
		return app.TaskTerminalDeliverySourceFacts{}, err
	}
	facts.FactsSHA256 = digest
	return facts, nil
}

func boundedTerminalDeliveryObjective(objective string) (string, bool) {
	if len(objective) <= app.MaxTaskTerminalDeliveryObjectiveSummaryBytes {
		return objective, false
	}
	preview := objective[:app.MaxTaskTerminalDeliveryObjectiveSummaryBytes]
	for !utf8.ValidString(preview) && len(preview) > 0 {
		preview = preview[:len(preview)-1]
	}
	preview = strings.TrimRight(preview, " \t\r\n")
	if preview == "" {
		return "user request unavailable", true
	}
	return preview, true
}

func safeTaskTerminalFailureCode(payload []byte) string {
	var event struct {
		Reason    string `json:"reason"`
		ErrorCode string `json:"error_code"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return "unknown"
	}
	for _, code := range []string{event.ErrorCode, event.Reason} {
		switch code {
		case "provider_unavailable", "provider_timeout", "provider_rate_limited", "model_unavailable",
			"model_output_invalid", "model_budget_exhausted", "tool_failed", "task_execution_failed":
			return code
		}
	}
	return "unknown"
}

func safeTaskTerminalWaitingReason(reason string) string {
	if category, ok := taskTerminalDeliveryPublicWaitingReason(reason); ok {
		return category
	}
	return app.SafeTaskTerminalDeliveryWaitingReason(reason)
}

type taskTerminalDeliveryValidationFacts struct {
	State    app.TaskValidationState
	Findings []app.TaskTerminalDeliveryFinding
}

func readTaskTerminalDeliveryValidationFactsInTx(ctx context.Context, tx pgx.Tx,
	claim TaskTerminalDeliveryClaim, result string) (taskTerminalDeliveryValidationFacts, error) {
	var state string
	var reportRaw, artifactProofsRaw []byte
	err := tx.QueryRow(ctx, `SELECT validation_state,review_report,artifact_content_evidence
FROM task_validation_reviews
WHERE task_id=$1 AND owner_id=$2 AND task_revision=$3 AND result_event_sequence=$4
  AND result_sha256=$5 AND claim_generation=$6 AND state='reviewed' FOR SHARE`,
		claim.TaskID, claim.OwnerID, claim.TaskRevision, claim.SourceEventSequence,
		claim.ResultSHA256, claim.ValidationClaimGeneration).Scan(&state, &reportRaw, &artifactProofsRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return taskTerminalDeliveryValidationFacts{}, domain.NewError(domain.CodeConflict, "Task validation source changed")
	}
	if err != nil {
		return taskTerminalDeliveryValidationFacts{}, fmt.Errorf("read Task terminal delivery validation source: %w", err)
	}
	validationState := app.TaskValidationState(state)
	if !app.ValidTaskValidationState(validationState) || len(reportRaw) == 0 || len(reportRaw) > app.MaxTaskValidationReviewReportBytes ||
		len(artifactProofsRaw) == 0 || len(artifactProofsRaw) > app.MaxTaskValidationReviewArtifactProofBytes {
		return taskTerminalDeliveryValidationFacts{}, domain.NewError(domain.CodeInvalidState, "stored Task validation delivery facts are invalid")
	}
	evidence, err := loadTaskValidationEvidence(ctx, tx, claim.TaskID, claim.TaskRevision, result)
	if err != nil {
		return taskTerminalDeliveryValidationFacts{}, err
	}
	report, err := app.ParseTaskValidationReviewReport(reportRaw, evidence)
	if err != nil {
		return taskTerminalDeliveryValidationFacts{}, domain.NewError(domain.CodeInvalidState, "stored Task validation report is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(artifactProofsRaw))
	decoder.DisallowUnknownFields()
	var proofs []app.TaskValidationReviewArtifactContentProof
	if decoder.Decode(&proofs) != nil || proofs == nil {
		return taskTerminalDeliveryValidationFacts{}, domain.NewError(domain.CodeInvalidState, "stored Task validation artifact evidence is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return taskTerminalDeliveryValidationFacts{}, domain.NewError(domain.CodeInvalidState, "stored Task validation artifact evidence has trailing data")
	}
	facts, err := loadTaskValidationVerdictFacts(ctx, tx,
		app.TaskValidationReviewClaim{OwnerID: claim.OwnerID, InitiatorID: claim.InitiatorID, ConversationID: claim.ConversationID,
			TaskID: claim.TaskID, TaskRevision: claim.TaskRevision, ResultEventSequence: claim.SourceEventSequence,
			ResultSHA256: append([]byte(nil), claim.ResultSHA256...), ClaimGeneration: *claim.ValidationClaimGeneration,
			LeaseOwner: "terminal-delivery-facts-reader"}, result, evidence, report, proofs)
	if err != nil {
		return taskTerminalDeliveryValidationFacts{}, err
	}
	evaluations := app.EvaluateTaskValidationReview(report, facts)
	verdict := app.SummarizeTaskValidationEvaluations(evaluations)
	if verdict.State != validationState {
		return taskTerminalDeliveryValidationFacts{}, domain.NewError(domain.CodeConflict, "Task validation verdict facts changed")
	}
	canonicalReport, err := json.Marshal(report)
	if err != nil {
		return taskTerminalDeliveryValidationFacts{}, fmt.Errorf("encode Task terminal delivery review report: %w", err)
	}
	canonicalProofs, err := json.Marshal(proofs)
	if err != nil {
		return taskTerminalDeliveryValidationFacts{}, fmt.Errorf("encode Task terminal delivery artifact evidence: %w", err)
	}
	reviewClaim := app.TaskValidationReviewClaim{OwnerID: claim.OwnerID, InitiatorID: claim.InitiatorID, ConversationID: claim.ConversationID,
		TaskID: claim.TaskID, TaskRevision: claim.TaskRevision, ResultEventSequence: claim.SourceEventSequence,
		ResultSHA256: append([]byte(nil), claim.ResultSHA256...), ClaimGeneration: *claim.ValidationClaimGeneration,
		LeaseOwner: "terminal-delivery-facts-reader"}
	verdictDigest, err := taskValidationDeliveryVerdictDigest(reviewClaim, verdict, evaluations, canonicalReport, canonicalProofs)
	if err != nil || !bytes.Equal(verdictDigest, claim.VerdictSHA256) {
		return taskTerminalDeliveryValidationFacts{}, domain.NewError(domain.CodeConflict, "Task validation verdict digest changed")
	}
	findings := make([]app.TaskTerminalDeliveryFinding, len(report.Findings))
	for index, finding := range report.Findings {
		refs := append([]app.TaskValidationEvidenceRefRecord(nil), finding.EvidenceRefs...)
		findings[index] = app.TaskTerminalDeliveryFinding{ID: finding.ID, Outcome: evaluations[index].Outcome,
			Assessment: finding.Assessment, Rationale: finding.Rationale, EvidenceRefs: refs}
	}
	return taskTerminalDeliveryValidationFacts{State: validationState, Findings: findings}, nil
}

func taskTerminalDeliveryHasLaterConversationInputInTx(ctx context.Context, tx pgx.Tx, claim TaskTerminalDeliveryClaim) (bool, error) {
	var found bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM task_events source
 JOIN conversation_messages message ON message.owner_id=$2 AND message.actor_id=$5
   AND message.conversation_id=$6 AND message.role='user'
 LEFT JOIN conversation_message_routes route ON route.message_id=message.id
 WHERE source.task_id=$1 AND source.sequence=$3 AND source.event_type=$4
   AND GREATEST(message.created_at,COALESCE(route.updated_at,message.created_at))>source.created_at
)`, claim.TaskID, claim.OwnerID, claim.SourceEventSequence, claim.SourceEventType, claim.InitiatorID,
		claim.ConversationID).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("check later conversation input before terminal delivery publish: %w", err)
	}
	return found, nil
}

func taskTerminalDeliveryPublishedMessageMatchesInTx(ctx context.Context, tx pgx.Tx,
	claim TaskTerminalDeliveryClaim, content string) (bool, error) {
	var matches bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM conversation_messages message
 WHERE message.id=$1 AND message.task_id=$2 AND message.owner_id=$3 AND message.actor_id=$4
   AND message.conversation_id=$5 AND message.role='assistant' AND message.content=$6
   AND message.source_kind='task' AND message.delivery_status='applied'
)`, claim.PublishedMessageID, claim.TaskID, claim.OwnerID, claim.InitiatorID, claim.ConversationID, content).Scan(&matches)
	if err != nil {
		return false, fmt.Errorf("verify published Task terminal delivery message: %w", err)
	}
	return matches, nil
}

func validTerminalDeliveryMessage(content string) bool {
	return content != "" && strings.TrimSpace(content) == content && utf8.ValidString(content) &&
		len(content) <= app.MaxTaskTerminalDeliveryReplyBytes && !strings.ContainsRune(content, '\x00')
}
