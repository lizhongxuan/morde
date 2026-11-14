package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/project"
)

func resolveRouteModelInTx(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) (string, error) {
	selected := claim.Input.ModelID
	if selected == "" && claim.Input.ResultReviewID != "" {
		err := tx.QueryRow(ctx, `SELECT t.model_id
FROM conversation_result_batches b
JOIN conversation_result_sources s ON s.batch_message_id=b.message_id AND s.owner_id=b.owner_id
 AND s.actor_id=b.actor_id AND s.conversation_id=b.conversation_id AND s.state='pending'
JOIN tasks t ON t.id=s.task_id AND t.owner_id=s.owner_id AND t.initiator_id=s.actor_id AND t.conversation_id=s.conversation_id
WHERE b.message_id=$1 AND b.service_instance_id=$2::uuid AND b.owner_id=$3 AND b.actor_id=$4 AND b.conversation_id=$5
ORDER BY s.source_order DESC, s.task_id LIMIT 1`, claim.Input.ResultReviewID, claim.ServiceInstanceID,
			claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID).Scan(&selected)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("read result review model: %w", err)
		}
	}
	if selected == "" {
		if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT selected_model_id FROM user_preferences WHERE user_id=$1),'')`, claim.Message.ActorID).Scan(&selected); err != nil {
			return "", fmt.Errorf("read route model preference: %w", err)
		}
	}
	if selected == "" {
		if err := tx.QueryRow(ctx, `SELECT COALESCE(model_connection_id,'') FROM admin_model_defaults WHERE id=1 FOR SHARE`).Scan(&selected); err != nil {
			return "", fmt.Errorf("read route model default: %w", err)
		}
	}
	if selected == "" {
		rows, err := tx.Query(ctx, `SELECT id FROM model_connections WHERE (owner_id IS NULL OR owner_id=$1)
 AND checked_at IS NOT NULL AND capability_text ORDER BY id LIMIT 2`, claim.Message.ActorID)
		if err != nil {
			return "", fmt.Errorf("find route text model: %w", err)
		}
		defer rows.Close()
		var choices []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return "", fmt.Errorf("read route text model: %w", err)
			}
			choices = append(choices, id)
		}
		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("read route text models: %w", err)
		}
		if len(choices) != 1 {
			return "", domain.NewError(domain.CodeInvalidState, "a route model must be selected")
		}
		selected = choices[0]
	}
	if !domain.ValidModelConnectionID(selected) {
		return "", domain.NewError(domain.CodeInvalidState, "selected route model is unavailable")
	}
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT (owner_id IS NULL OR owner_id=$2) AND checked_at IS NOT NULL AND capability_text
FROM model_connections WHERE id=$1 FOR SHARE`, selected, claim.Message.ActorID).Scan(&allowed); err != nil || !allowed {
		return "", domain.NewError(domain.CodeInvalidState, "selected route model is unavailable")
	}
	return selected, nil
}

func (s *Store) ResolveConversationRouteModel(ctx context.Context, claim app.ConversationRouteClaim) (string, error) {
	var selected string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		var err error
		selected, err = resolveRouteModelInTx(ctx, tx, claim)
		return err
	})
	return selected, err
}

func checkConversationRouteModelFence(ctx context.Context, tx pgx.Tx, fence app.ConversationRouteModelFence, modelID string) error {
	claim, value := fence.Claim, fence.Context
	if value.ActorID != claim.Message.ActorID || value.ReferenceTaskID != claim.Input.ReferenceTaskID ||
		value.ModelID != modelID || len(value.History) > 100 || len(value.Candidates) > 100 {
		return domain.InvalidArgument("route_context", "route model context is invalid")
	}
	if _, err := app.ConversationRouteModelRequest(value); err != nil {
		return err
	}
	if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
		return err
	}
	selected, err := resolveRouteModelInTx(ctx, tx, claim)
	if err != nil || selected != modelID {
		return domain.NewError(domain.CodeInvalidState, "route model selection changed")
	}
	snapshot, err := loadConversationRouteSnapshotInTx(ctx, tx, claim, true)
	if err != nil {
		return err
	}
	expected := snapshot.Context
	expected.ModelID = modelID
	if value.SummaryVersion > 0 {
		if snapshot.Summary == nil || snapshot.Summary.Version != value.SummaryVersion ||
			!bytes.Equal(snapshot.Summary.SummarySHA256, value.SummarySHA256) ||
			!bytes.Equal(snapshot.Summary.SourcesSHA256, value.SummarySourcesSHA256) {
			return domain.NewError(domain.CodeConflict, "authorized route summary changed")
		}
		expected.SummaryText = value.SummaryText
		expected.History = app.ConversationRouteHistoryWithoutSummarySources(expected.History, snapshot.Summary.Sources)
	} else {
		expected.SummaryVersion, expected.SummarySHA256, expected.SummarySourcesSHA256, expected.SummaryText = 0, nil, nil, ""
	}
	if snapshot.PreviousCycle || !reflect.DeepEqual(expected, value) {
		return domain.NewError(domain.CodeConflict, "authorized route context changed")
	}
	if claim.Input.PreviousClientMessageID == "" {
		if value.PreviousUnresolved {
			return domain.InvalidArgument("route_context", "route predecessor status is invalid")
		}
	} else {
		var state string
		err := tx.QueryRow(ctx, `SELECT r.state FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id
WHERE r.service_instance_id=$1::uuid AND r.actor_id=$2 AND r.conversation_id=$3 AND r.client_message_id=$4
  AND r.owner_id=$5 FOR SHARE OF r`, claim.ServiceInstanceID, claim.Message.ActorID, claim.Message.ConversationID,
			claim.Input.PreviousClientMessageID, claim.Message.OwnerID).Scan(&state)
		if value.PreviousMissing {
			if !errors.Is(err, pgx.ErrNoRows) || !value.PreviousUnresolved {
				return domain.NewError(domain.CodeConflict, "route predecessor changed or is unavailable")
			}
		} else if err != nil || (state != "routed") != value.PreviousUnresolved {
			return domain.NewError(domain.CodeConflict, "route predecessor changed or is unavailable")
		}
	}
	for _, item := range value.History {
		var valid bool
		err := tx.QueryRow(ctx, `SELECT m.conversation_id=$2 AND m.owner_id=$3 AND m.sequence=$4 AND m.role=$6 AND m.content=$7
 AND (m.sequence<$5 OR (m.sequence>$5 AND m.actor_id=$8 AND m.role='user' AND EXISTS (
   SELECT 1 FROM conversation_message_routes predecessor WHERE predecessor.message_id=m.id
    AND predecessor.service_instance_id=$9::uuid AND predecessor.owner_id=$3 AND predecessor.actor_id=$8
    AND predecessor.conversation_id=$2 AND predecessor.client_message_id=$11)))
 AND (octet_length(m.content)<=4096 OR (m.sequence>$5 AND m.actor_id=$8 AND m.role='user' AND EXISTS (
   SELECT 1 FROM conversation_message_routes predecessor WHERE predecessor.message_id=m.id
    AND predecessor.service_instance_id=$9::uuid AND predecessor.owner_id=$3 AND predecessor.actor_id=$8
    AND predecessor.conversation_id=$2 AND predecessor.client_message_id=$11))) AND (
  (m.actor_id=$8 AND (m.task_id IS NULL OR t.initiator_id=$8)
   AND (m.role='user' OR (m.role='assistant' AND (
    (m.output_status IS NULL AND (t.status='completed' OR EXISTS (
     SELECT 1 FROM conversation_result_batches batch WHERE batch.message_id=m.id AND batch.published_message_id=m.id
      AND batch.state='published' AND batch.service_instance_id=$9::uuid
      AND batch.owner_id=m.owner_id AND batch.conversation_id=m.conversation_id AND batch.actor_id=m.actor_id
      AND EXISTS (SELECT 1 FROM conversation_result_sources source WHERE source.batch_message_id=batch.message_id
        AND source.owner_id=batch.owner_id AND source.conversation_id=batch.conversation_id AND source.actor_id=batch.actor_id AND source.state='published')
    ))) OR (m.output_status='completed' AND t.initiator_id=m.actor_id AND EXISTS (
     SELECT 1 FROM task_events event WHERE event.task_id=m.task_id AND event.sequence=m.output_result_event_sequence
      AND event.task_revision=m.output_task_revision AND event.event_type='task.result'
      AND event.result_content_sha256=sha256(convert_to(m.content,'UTF8'))
    ))
   ))))
  OR (m.actor_id<>$8 AND $10<>'' AND m.source_kind NOT IN ('history_import','legacy_instant')
   AND EXISTS (SELECT 1 FROM conversations project_scope WHERE project_scope.id=m.conversation_id
    AND project_scope.owner_id=m.owner_id AND project_scope.project_id=$10)
   AND (`+projectSharedHistoryEvidenceSQL+`))
 )
FROM conversation_messages m LEFT JOIN tasks t ON t.id=m.task_id AND t.owner_id=m.owner_id AND t.conversation_id=m.conversation_id
WHERE m.id=$1 FOR SHARE OF m`, item.MessageID, claim.Message.ConversationID, claim.Message.OwnerID,
			item.Sequence, claim.Message.Sequence, string(item.Role), item.Content, claim.Message.ActorID, claim.ServiceInstanceID,
			claim.Input.ProjectID, claim.Input.PreviousClientMessageID).Scan(&valid)
		if err != nil || !valid {
			return domain.NewError(domain.CodeUnauthenticated, "route history is unavailable")
		}
	}
	for _, candidate := range value.Candidates {
		var valid bool
		err := tx.QueryRow(ctx, `SELECT conversation_id=$2 AND owner_id=$3 AND initiator_id=$4 AND status=$5
 AND desired_revision=$6 AND objective=$7 FROM tasks WHERE id=$1 FOR SHARE`, candidate.TaskID,
			claim.Message.ConversationID, claim.Message.OwnerID, claim.Message.ActorID,
			string(candidate.Status), candidate.Revision, candidate.Objective).Scan(&valid)
		if err != nil || !valid {
			return domain.NewError(domain.CodeConflict, "route candidate changed")
		}
	}
	return nil
}

func conversationRouteModelPhase(value app.ConversationRouteContext) string {
	if value.ResultReview != nil && value.ResultReview.RetryAttempt > 0 {
		return fmt.Sprintf("result_review_%d", value.ResultReview.RetryAttempt)
	}
	if value.PreviousMissing {
		return "missing_predecessor"
	}
	return "normal"
}

func (s *Store) PrepareConversationRouteModelCall(ctx context.Context, call app.ModelCallReservation, projectCall *app.ProjectModelReservation) (*app.ConversationRouteDecision, error) {
	if call.Source != app.ModelCallRoute || call.Route == nil || call.OwnerID != call.Route.Claim.Message.ActorID ||
		call.ProjectID != call.Route.Claim.Input.ProjectID || len(call.RequestDigest) != 32 || call.ReservedTokens < 1 ||
		(call.LegacyCallID == "") != (len(call.LegacyDigest) == 0) || (call.LegacyCallID != "" &&
		(!strings.HasPrefix(call.CallID, "mdl_") || !strings.HasPrefix(call.LegacyCallID, "route_") || len(call.LegacyDigest) != 32)) {
		return nil, domain.InvalidArgument("route_model", "route model call is invalid")
	}
	if projectCall != nil {
		if call.ProjectID == "" || call.PersonalModel || projectCall.ProjectID != call.ProjectID || projectCall.ActorID != call.OwnerID ||
			projectCall.Source != app.ModelCallRoute || projectCall.TaskID != "" || projectCall.CallID != call.CallID ||
			projectCall.ModelID != call.ModelID || projectCall.ReservedTokens != call.ReservedTokens ||
			projectCall.AccountCall.Source != call.Source || projectCall.AccountCall.OwnerID != call.OwnerID ||
			projectCall.AccountCall.ProjectID != call.ProjectID || projectCall.AccountCall.ModelID != call.ModelID ||
			projectCall.AccountCall.PersonalModel || projectCall.AccountCall.CallID != call.CallID ||
			!bytes.Equal(projectCall.AccountCall.RequestDigest, call.RequestDigest) ||
			projectCall.Usage.InputTokens < 0 || projectCall.Usage.OutputTokens < 0 ||
			projectCall.Usage.InputTokens > math.MaxInt64-projectCall.Usage.OutputTokens ||
			projectCall.Usage.InputTokens+projectCall.Usage.OutputTokens != call.ReservedTokens ||
			projectCall.Rate.Validate() != nil || projectCall.Rate.ModelID != call.ModelID || projectCall.Rate.Model != call.ProviderModel || len(projectCall.RequestDigest) != 32 {
			return nil, domain.InvalidArgument("route_model", "project route reservation is invalid")
		}
		cost, err := projectCall.Rate.EstimateTokenCost(projectCall.Usage)
		if err != nil || cost < 1 || cost != projectCall.ReservedMicros {
			return nil, domain.InvalidArgument("route_model", "project route cost is invalid")
		}
	}
	var decision *app.ConversationRouteDecision
	unresolved := false
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, call.OwnerID); err != nil {
			return err
		}
		if err := lockConversationRouteClaim(ctx, tx, call.Route.Claim); err != nil {
			return err
		}
		var state, priorCall, priorModel, priorProject, dispatchOwner, priorPhase string
		var digest, result []byte
		var priorVersion time.Time
		var dispatchVersion int64
		phase := conversationRouteModelPhase(call.Route.Context)
		attempt := call.Route.Context.RouteAttempt
		if call.Route.Context.ResultReview != nil {
			attempt = 0
		}
		var priorAttempt int64
		err := tx.QueryRow(ctx, `SELECT state,call_id,model_id,request_digest,decision,model_version,COALESCE(project_id,''),dispatch_lease_owner,dispatch_lease_version,phase,attempt
FROM conversation_route_model_calls WHERE message_id=$1 AND actor_id=$2
 AND (state IN ('reserved','dispatched') OR (phase=$3 AND attempt=$4) OR (state='unknown' AND attempt=$4))
ORDER BY (state IN ('reserved','dispatched')) DESC,(phase=$3 AND attempt=$4) DESC,updated_at DESC LIMIT 1 FOR UPDATE`,
			call.Route.Claim.Message.ID, call.OwnerID, phase, attempt).
			Scan(&state, &priorCall, &priorModel, &digest, &result, &priorVersion, &priorProject, &dispatchOwner, &dispatchVersion, &priorPhase, &priorAttempt)
		if err == nil && state == "reserved" && (priorPhase != phase || priorAttempt != attempt || priorProject != call.ProjectID ||
			priorCall != call.CallID || priorModel != call.ModelID || !bytes.Equal(digest, call.RequestDigest) ||
			!priorVersion.Equal(call.ConnectionVersion)) {
			if err := releaseRouteReservationInTx(ctx, tx, call.OwnerID, call.Route.Claim.Message.ID); err != nil {
				return err
			}
			err = pgx.ErrNoRows
		}
		legacyPrior := err == nil && priorPhase == phase && priorAttempt == 0 && attempt == 0 && call.LegacyCallID != "" && priorCall == call.LegacyCallID &&
			priorProject == call.ProjectID && priorModel == call.ModelID && bytes.Equal(digest, call.LegacyDigest) &&
			priorVersion.Equal(call.ConnectionVersion)
		if legacyPrior && state == "reserved" {
			if err := releaseRouteReservationInTx(ctx, tx, call.OwnerID, call.Route.Claim.Message.ID); err != nil {
				return err
			}
			err = pgx.ErrNoRows
		}
		if err == nil {
			if priorProject != call.ProjectID {
				return domain.NewError(domain.CodeInvalidState, "route project attribution changed")
			}
			if state == "dispatched" || state == "unknown" {
				if state == "dispatched" && call.Route.Claim.LeaseVersion > dispatchVersion {
					if err := markAbandonedRouteCallUnknown(ctx, tx, call.Route.Claim.Message.ID, call.OwnerID, priorProject, priorCall); err != nil {
						return err
					}
				} else if state == "dispatched" && (call.Route.Claim.LeaseVersion != dispatchVersion || call.Route.Claim.LeaseOwner != dispatchOwner) {
					return domain.NewError(domain.CodeConflict, "route dispatch lease is inconsistent")
				}
				unresolved = true
				return nil
			}
			if err := checkModelCaller(ctx, tx, call); err != nil {
				return err
			}
			if legacyPrior && state == "settled" {
				var value app.ConversationRouteDecision
				if err := json.Unmarshal(result, &value); err != nil || !validStoredRouteDecision(value, call.Route.Context) {
					return domain.NewError(domain.CodeInvalidState, "settled legacy route decision is invalid")
				}
				if staleAttemptZeroRouteDecision(value, call.Route.Context) {
					return domain.NewError(domain.CodeConflict, "legacy route result predates current request candidates")
				}
				decision = &value
				return nil
			}
			if state == "settled" && phase == "normal" && attempt == 0 && priorAttempt == 0 && priorPhase == phase &&
				strings.HasPrefix(call.CallID, "mdl_") && call.LegacyCallID != "" && priorCall == call.CallID &&
				priorProject == call.ProjectID && priorModel == call.ModelID && priorVersion.Equal(call.ConnectionVersion) {
				if err := checkModelCaller(ctx, tx, call); err != nil {
					return err
				}
				var value app.ConversationRouteDecision
				if err := json.Unmarshal(result, &value); err != nil || !validStoredRouteDecision(value, call.Route.Context) {
					return domain.NewError(domain.CodeInvalidState, "settled coordinator route decision is invalid")
				}
				if staleAttemptZeroRouteDecision(value, call.Route.Context) {
					return domain.NewError(domain.CodeConflict, "legacy coordinator result predates current request candidates")
				}
				decision = &value
				return nil
			}
			if priorCall != call.CallID || priorModel != call.ModelID || !bytes.Equal(digest, call.RequestDigest) || !priorVersion.Equal(call.ConnectionVersion) {
				return domain.NewError(domain.CodeConflict, "route model context changed and requires clarification")
			}
			switch state {
			case "reserved":
				return nil
			case "settled":
				var value app.ConversationRouteDecision
				if err := json.Unmarshal(result, &value); err != nil {
					return fmt.Errorf("decode route decision: %w", err)
				}
				decision = &value
				return nil
			default:
				return domain.NewError(domain.CodeInvalidState, "route model result is unknown and requires reconciliation")
			}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("find route model call: %w", err)
		}
		if err := checkModelCaller(ctx, tx, call); err != nil {
			return err
		}
		if call.ProjectID != "" && !call.PersonalModel && projectCall == nil {
			return app.ErrRouteProjectReservationRequired
		}
		if err := reserveModelCallInTx(ctx, tx, call, projectCall); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO conversation_route_model_calls
(message_id,service_instance_id,owner_id,actor_id,conversation_id,project_id,source,call_id,request_digest,model_id,model_version,state,phase,attempt)
VALUES ($1,$2::uuid,$3,$4,$5,NULLIF($6,''),'route',$7,$8,$9,$10,'reserved',$11,$12)`,
			call.Route.Claim.Message.ID, call.Route.Claim.ServiceInstanceID, call.Route.Claim.Message.OwnerID,
			call.OwnerID, call.Route.Claim.Message.ConversationID, call.ProjectID, call.CallID,
			call.RequestDigest, call.ModelID, call.ConnectionVersion, phase, attempt)
		if err != nil {
			return fmt.Errorf("reserve route model call: %w", err)
		}
		return nil
	})
	if err == nil && unresolved {
		return nil, domain.NewError(domain.CodeInvalidState, "route model result is unknown and requires reconciliation")
	}
	return decision, err
}

func markAbandonedRouteCallUnknown(ctx context.Context, tx pgx.Tx, messageID, actorID, projectID, callID string) error {
	var state string
	var personal bool
	if err := tx.QueryRow(ctx, `SELECT state,personal_model FROM model_call_ledger WHERE owner_id=$1 AND source='route' AND call_id=$2 FOR UPDATE`,
		actorID, callID).Scan(&state, &personal); err != nil || state != "dispatched" {
		return domain.NewError(domain.CodeInvalidState, "route model account hold is unavailable")
	}
	if projectID != "" && !personal {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM project_budget_ledger WHERE project_id=$1 AND source='route' AND call_id=$2 AND actor_id=$3 FOR UPDATE`,
			projectID, callID, actorID).Scan(&status); err != nil || status != "reserved" {
			return domain.NewError(domain.CodeInvalidState, "project route model hold is unavailable")
		}
		if err := markProjectModelCallUnknown(ctx, tx, projectID, actorID, string(app.ModelCallRoute), callID, ""); err != nil {
			return err
		}
	} else if _, err := tx.Exec(ctx, `UPDATE model_call_ledger SET state='unknown',settled_at=clock_timestamp()
WHERE owner_id=$1 AND source='route' AND call_id=$2 AND state='dispatched'`, actorID, callID); err != nil {
		return fmt.Errorf("hold abandoned route model usage: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE conversation_route_model_calls SET state='unknown',updated_at=clock_timestamp()
WHERE message_id=$1 AND call_id=$2 AND state='dispatched'`, messageID, callID); err != nil {
		return fmt.Errorf("hold abandoned route model result: %w", err)
	}
	return nil
}

// The caller holds the account advisory lock and the route row lock. Only a
// durable pre-dispatch reservation can be discarded; a dispatched call is
// retained for unknown-usage reconciliation.
func releaseRouteReservationInTx(ctx context.Context, tx pgx.Tx, actorID, messageID string) error {
	var state, callID, projectID string
	err := tx.QueryRow(ctx, `SELECT state,call_id,COALESCE(project_id,'') FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND state='reserved' FOR UPDATE`, messageID, actorID).Scan(&state, &callID, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock route reservation for release: %w", err)
	}
	if state != "reserved" {
		return nil
	}
	var ledgerState string
	var personal bool
	if err := tx.QueryRow(ctx, `SELECT state,personal_model FROM model_call_ledger
WHERE owner_id=$1 AND source='route' AND call_id=$2 FOR UPDATE`, actorID, callID).Scan(&ledgerState, &personal); err != nil || ledgerState != "dispatched" {
		return domain.NewError(domain.CodeInvalidState, "route model reservation is inconsistent")
	}
	if projectID != "" && !personal {
		if err := lockProjectByIDInTx(ctx, tx, projectID); err != nil {
			return err
		}
		var projectState string
		if err := tx.QueryRow(ctx, `SELECT status FROM project_budget_ledger WHERE project_id=$1 AND source='route'
AND call_id=$2 AND actor_id=$3 AND task_id IS NULL FOR UPDATE`, projectID, callID, actorID).Scan(&projectState); err != nil || projectState != "reserved" {
			return domain.NewError(domain.CodeInvalidState, "project route model reservation is inconsistent")
		}
	} else {
		var projectHold bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM project_budget_ledger WHERE actor_id=$1 AND source='route' AND call_id=$2)`,
			actorID, callID).Scan(&projectHold); err != nil || projectHold {
			return domain.NewError(domain.CodeInvalidState, "route project reservation attribution is inconsistent")
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM conversation_route_model_calls WHERE message_id=$1 AND actor_id=$2 AND state='reserved'`, messageID, actorID); err != nil {
		return fmt.Errorf("release route model reservation: %w", err)
	}
	if projectID != "" && !personal {
		result, err := tx.Exec(ctx, `DELETE FROM project_budget_ledger WHERE project_id=$1 AND source='route' AND call_id=$2 AND actor_id=$3 AND status='reserved'`,
			projectID, callID, actorID)
		if err != nil || result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "project route reservation changed before release")
		}
	}
	result, err := tx.Exec(ctx, `DELETE FROM model_call_ledger WHERE owner_id=$1 AND source='route' AND call_id=$2 AND state='dispatched'`, actorID, callID)
	if err != nil || result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "route model reservation changed before release")
	}
	return nil
}

func expiredRouteRowsInTx(ctx context.Context, tx pgx.Tx, actorID string, includeDispatched bool) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT r.message_id FROM conversation_message_routes r
JOIN conversation_route_model_calls c ON c.message_id=r.message_id AND c.actor_id=r.actor_id
WHERE c.actor_id=$1 AND (c.state='reserved' OR ($2 AND c.state='dispatched'))
AND (r.state<>'processing' OR r.lease_expires_at IS NULL OR r.lease_expires_at<=clock_timestamp()
  OR (c.state='dispatched' AND (r.lease_version<>c.dispatch_lease_version OR r.lease_owner<>c.dispatch_lease_owner)))
ORDER BY c.created_at,c.message_id LIMIT 100 FOR UPDATE OF r SKIP LOCKED`, actorID, includeDispatched)
	if err != nil {
		return nil, fmt.Errorf("find expired route model calls: %w", err)
	}
	defer rows.Close()
	var messages []string
	for rows.Next() {
		var messageID string
		if err := rows.Scan(&messageID); err != nil {
			return nil, fmt.Errorf("read expired route model call: %w", err)
		}
		messages = append(messages, messageID)
	}
	return messages, rows.Err()
}

func releaseExpiredRouteReservationsInTx(ctx context.Context, tx pgx.Tx, actorID string) error {
	messages, err := expiredRouteRowsInTx(ctx, tx, actorID, false)
	if err != nil {
		return err
	}
	for _, messageID := range messages {
		if err := releaseRouteReservationInTx(ctx, tx, actorID, messageID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) recoverExpiredRouteModelCalls(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT c.actor_id FROM conversation_route_model_calls c
JOIN conversation_message_routes r ON r.message_id=c.message_id AND r.actor_id=c.actor_id
WHERE c.state IN ('reserved','dispatched')
AND (r.state<>'processing' OR r.lease_expires_at IS NULL OR r.lease_expires_at<=clock_timestamp()
  OR (c.state='dispatched' AND (r.lease_version<>c.dispatch_lease_version OR r.lease_owner<>c.dispatch_lease_owner)))
ORDER BY c.actor_id LIMIT 100`)
	if err != nil {
		return 0, fmt.Errorf("list expired route model actors: %w", err)
	}
	var actors []string
	for rows.Next() {
		var actor string
		if err := rows.Scan(&actor); err != nil {
			rows.Close()
			return 0, fmt.Errorf("read expired route model actor: %w", err)
		}
		actors = append(actors, actor)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("iterate expired route model actors: %w", err)
	}
	marked := 0
	for _, actor := range actors {
		count := 0
		err := s.WithTx(ctx, func(tx pgx.Tx) error {
			if err := lockModelLedgerOwner(ctx, tx, actor); err != nil {
				return err
			}
			messages, err := expiredRouteRowsInTx(ctx, tx, actor, true)
			if err != nil {
				return err
			}
			for _, messageID := range messages {
				var state, callID, projectID string
				var personal bool
				err := tx.QueryRow(ctx, `SELECT c.state,c.call_id,COALESCE(c.project_id,''),l.personal_model
FROM conversation_route_model_calls c JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
WHERE c.message_id=$1 AND c.actor_id=$2 AND c.state IN ('reserved','dispatched') FOR UPDATE OF c`, messageID, actor).Scan(&state, &callID, &projectID, &personal)
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				if err != nil {
					return fmt.Errorf("lock expired route model call: %w", err)
				}
				if state == "reserved" {
					if err := releaseRouteReservationInTx(ctx, tx, actor, messageID); err != nil {
						return err
					}
					continue
				}
				if state != "dispatched" {
					continue
				}
				if projectID != "" && !personal {
					if err := lockProjectByIDInTx(ctx, tx, projectID); err != nil {
						return err
					}
				}
				if err := markAbandonedRouteCallUnknown(ctx, tx, messageID, actor, projectID, callID); err != nil {
					return err
				}
				count++
			}
			return nil
		})
		if err != nil {
			return marked, err
		}
		marked += count
	}
	return marked, nil
}

func (s *Store) BeginConversationRouteModelCall(ctx context.Context, call app.ModelCallReservation) error {
	if call.Source != app.ModelCallRoute || call.Route == nil || call.OwnerID != call.Route.Claim.Message.ActorID ||
		call.ProjectID != call.Route.Claim.Input.ProjectID {
		return domain.InvalidArgument("route_model", "route model call is invalid")
	}
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, call.OwnerID); err != nil {
			return err
		}
		if err := checkModelCaller(ctx, tx, call); err != nil {
			return err
		}
		var state, modelID string
		var digest []byte
		var version time.Time
		err := tx.QueryRow(ctx, `SELECT state,model_id,request_digest,model_version FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND call_id=$3 FOR UPDATE`, call.Route.Claim.Message.ID, call.OwnerID, call.CallID).
			Scan(&state, &modelID, &digest, &version)
		if err != nil || state != "reserved" || modelID != call.ModelID || !bytes.Equal(digest, call.RequestDigest) || !version.Equal(call.ConnectionVersion) {
			return domain.NewError(domain.CodeConflict, "route model call is no longer awaiting dispatch")
		}
		var ledgerState, ledgerModel string
		var ledgerDigest []byte
		var ledgerPersonal bool
		err = tx.QueryRow(ctx, `SELECT state,model_id,request_digest,personal_model FROM model_call_ledger
WHERE owner_id=$1 AND source='route' AND call_id=$2 FOR UPDATE`, call.OwnerID, call.CallID).
			Scan(&ledgerState, &ledgerModel, &ledgerDigest, &ledgerPersonal)
		if err != nil || ledgerState != "dispatched" || ledgerModel != call.ModelID || !bytes.Equal(ledgerDigest, call.RequestDigest) || ledgerPersonal != call.PersonalModel {
			return domain.NewError(domain.CodeInvalidState, "route model account reservation is unavailable")
		}
		if call.ProjectID != "" && !call.PersonalModel {
			var reserved bool
			err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM project_budget_ledger WHERE project_id=$1 AND source='route'
 AND call_id=$2 AND actor_id=$3 AND model_id=$4 AND task_id IS NULL AND status='reserved')`,
				call.ProjectID, call.CallID, call.OwnerID, call.ModelID).Scan(&reserved)
			if err != nil || !reserved {
				return domain.NewError(domain.CodeInvalidState, "project route model budget is unavailable")
			}
		}
		result, err := tx.Exec(ctx, `UPDATE conversation_route_model_calls SET state='dispatched',dispatch_lease_owner=$4,
dispatch_lease_version=$5,updated_at=clock_timestamp() WHERE message_id=$1 AND actor_id=$2 AND call_id=$3 AND state='reserved'`,
			call.Route.Claim.Message.ID, call.OwnerID, call.CallID, call.Route.Claim.LeaseOwner, call.Route.Claim.LeaseVersion)
		if err != nil {
			return fmt.Errorf("begin route model dispatch: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "route model call was already dispatched")
		}
		return nil
	})
	if err == nil {
		return nil
	}
	// A failed Begin is safe to release only if the same lease still owns a
	// durable reserved record. A concurrent successful dispatch wins the lock
	// and leaves the record untouched.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	releaseErr := s.WithTx(releaseCtx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(releaseCtx, tx, call.OwnerID); err != nil {
			return err
		}
		var messageID string
		lookupErr := tx.QueryRow(releaseCtx, `SELECT r.message_id FROM conversation_message_routes r
JOIN conversation_route_model_calls c ON c.message_id=r.message_id AND c.actor_id=r.actor_id
WHERE r.message_id=$1 AND r.actor_id=$2 AND r.lease_owner=$3 AND r.lease_version=$4
 AND c.call_id=$5 AND c.request_digest=$6 AND c.state='reserved' FOR UPDATE OF r`,
			call.Route.Claim.Message.ID, call.OwnerID, call.Route.Claim.LeaseOwner, call.Route.Claim.LeaseVersion,
			call.CallID, call.RequestDigest).Scan(&messageID)
		if errors.Is(lookupErr, pgx.ErrNoRows) {
			return nil
		}
		if lookupErr != nil {
			return fmt.Errorf("find failed route reservation: %w", lookupErr)
		}
		return releaseRouteReservationInTx(releaseCtx, tx, call.OwnerID, messageID)
	})
	if releaseErr != nil {
		return fmt.Errorf("%w; release undispatched route reservation: %w", err, releaseErr)
	}
	return err
}

// Settlement locks the route record and both applicable ledgers together. The
// durable decision is committed only with its exact usage, even if the original
// worker's lease expired after dispatch.
func (s *Store) SettleConversationRouteModelCall(ctx context.Context, call app.ModelCallReservation, decision *app.ConversationRouteDecision, usage app.ModelUsage, unknown bool) error {
	if call.Source != app.ModelCallRoute || call.Route == nil || !domain.ValidUserID(call.OwnerID) ||
		(!unknown && (decision == nil || !validStoredRouteDecision(*decision, call.Route.Context) || !usage.Known || usage.InputTokens < 0 || usage.OutputTokens < 0 ||
			usage.InputTokens > math.MaxInt64-usage.OutputTokens)) {
		return domain.InvalidArgument("route_model", "route model settlement is invalid")
	}
	var encoded []byte
	var err error
	if !unknown {
		encoded, err = json.Marshal(decision)
		if err != nil {
			return domain.InvalidArgument("route_model", "route model decision is invalid")
		}
	}
	overBudget := false
	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockModelLedgerOwner(ctx, tx, call.OwnerID); err != nil {
			return err
		}
		if !call.PersonalModel {
			if _, _, err := lockPlatformModelBudget(ctx, tx, call.OwnerID); err != nil {
				return err
			}
		}
		if call.ProjectID != "" && !call.PersonalModel {
			if err := lockProjectByIDInTx(ctx, tx, call.ProjectID); err != nil {
				return err
			}
		}
		var state, projectID, serviceID, ownerID, conversationID, modelID string
		var storedDigest, storedDecision []byte
		err := tx.QueryRow(ctx, `SELECT state,request_digest,decision,COALESCE(project_id,''),service_instance_id::text,owner_id,conversation_id,model_id FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND call_id=$3 FOR UPDATE`, call.Route.Claim.Message.ID, call.OwnerID, call.CallID).
			Scan(&state, &storedDigest, &storedDecision, &projectID, &serviceID, &ownerID, &conversationID, &modelID)
		if err != nil || !bytes.Equal(storedDigest, call.RequestDigest) || projectID != call.ProjectID || modelID != call.ModelID ||
			serviceID != call.Route.Claim.ServiceInstanceID || ownerID != call.Route.Claim.Message.OwnerID || conversationID != call.Route.Claim.Message.ConversationID {
			return domain.NewError(domain.CodeInvalidState, "route model call is unavailable")
		}
		if state == "settled" && !unknown {
			var prior app.ConversationRouteDecision
			var input, output int64
			var ledgerState string
			if json.Unmarshal(storedDecision, &prior) == nil && reflect.DeepEqual(prior, *decision) &&
				tx.QueryRow(ctx, `SELECT state,input_tokens,output_tokens FROM model_call_ledger WHERE owner_id=$1 AND source='route' AND call_id=$2`,
					call.OwnerID, call.CallID).Scan(&ledgerState, &input, &output) == nil && ledgerState == "settled" && input == usage.InputTokens && output == usage.OutputTokens {
				return nil
			}
			return domain.NewError(domain.CodeIdempotencyConflict, "route model settlement was already recorded differently")
		}
		if state == "unknown" && unknown {
			return nil
		}
		if state != "dispatched" && (state != "unknown" || unknown) {
			return domain.NewError(domain.CodeConflict, "route model call is not awaiting settlement")
		}
		var accountState string
		var reserved int64
		var personal bool
		var accountDigest []byte
		err = tx.QueryRow(ctx, `SELECT state,reserved_tokens,personal_model,request_digest FROM model_call_ledger
WHERE owner_id=$1 AND source='route' AND call_id=$2 FOR UPDATE`, call.OwnerID, call.CallID).
			Scan(&accountState, &reserved, &personal, &accountDigest)
		if err != nil || (state == "dispatched" && accountState != "dispatched") || (state == "unknown" && accountState != "unknown") ||
			personal != call.PersonalModel || !bytes.Equal(accountDigest, call.RequestDigest) {
			return domain.NewError(domain.CodeInvalidState, "route model account ledger is unavailable")
		}
		if !unknown && !personal && usage.InputTokens+usage.OutputTokens > reserved {
			unknown = true
			overBudget = true
		}
		var cost int64
		if call.ProjectID != "" && !personal {
			var status, modelID, currency, provider, rateModel string
			var version, inputRate, outputRate, reservedInput, reservedOutput, reservedCost int64
			err = tx.QueryRow(ctx, `SELECT status,model_id,currency,rate_provider,rate_model,rate_version,
input_micros_per_million,output_micros_per_million,reserved_input_tokens,reserved_output_tokens,reserved_micros
FROM project_budget_ledger WHERE project_id=$1 AND source='route' AND call_id=$2 AND actor_id=$3 AND task_id IS NULL FOR UPDATE`,
				call.ProjectID, call.CallID, call.OwnerID).Scan(&status, &modelID, &currency, &provider, &rateModel, &version,
				&inputRate, &outputRate, &reservedInput, &reservedOutput, &reservedCost)
			if err != nil || (state == "dispatched" && status != "reserved") || (state == "unknown" && status != "unknown") || modelID != call.ModelID {
				return domain.NewError(domain.CodeInvalidState, "project route model ledger is unavailable")
			}
			if !unknown {
				rate := project.RateSnapshot{Provider: provider, ModelID: modelID, Model: rateModel, Currency: currency,
					Version: version, InputMicrosPerMillion: inputRate, OutputMicrosPerMillion: outputRate}
				cost, err = rate.EstimateTokenCost(project.Usage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens})
				if err != nil || usage.InputTokens+usage.OutputTokens == 0 || usage.InputTokens > reservedInput ||
					usage.OutputTokens > reservedOutput || cost > reservedCost {
					unknown = true
					overBudget = true
				}
			}
		}
		if unknown {
			if call.ProjectID != "" && !personal {
				if err := markProjectModelCallUnknown(ctx, tx, call.ProjectID, call.OwnerID, string(app.ModelCallRoute), call.CallID, ""); err != nil {
					return err
				}
			} else if _, err := tx.Exec(ctx, `UPDATE model_call_ledger SET state='unknown',settled_at=clock_timestamp()
WHERE owner_id=$1 AND source='route' AND call_id=$2 AND state='dispatched'`, call.OwnerID, call.CallID); err != nil {
				return fmt.Errorf("hold unknown route model usage: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE conversation_route_model_calls SET state='unknown',updated_at=clock_timestamp()
WHERE message_id=$1 AND call_id=$2 AND state='dispatched'`, call.Route.Claim.Message.ID, call.CallID); err != nil {
				return fmt.Errorf("hold unknown route model result: %w", err)
			}
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE model_call_ledger SET state='settled',input_tokens=$3,output_tokens=$4,settled_at=clock_timestamp()
WHERE owner_id=$1 AND source='route' AND call_id=$2 AND state IN ('dispatched','unknown')`, call.OwnerID, call.CallID, usage.InputTokens, usage.OutputTokens); err != nil {
			return fmt.Errorf("settle route account model usage: %w", err)
		}
		if call.ProjectID != "" && !personal {
			if _, err := tx.Exec(ctx, `UPDATE project_budget_ledger SET status='settled',actual_input_tokens=$3,actual_output_tokens=$4,
actual_micros=$5,settled_at=clock_timestamp() WHERE project_id=$1 AND source='route' AND call_id=$2 AND status IN ('reserved','unknown')`,
				call.ProjectID, call.CallID, usage.InputTokens, usage.OutputTokens, cost); err != nil {
				return fmt.Errorf("settle route project model usage: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE conversation_route_model_calls SET state='settled',decision=$2::jsonb,updated_at=clock_timestamp()
WHERE message_id=$1 AND call_id=$3 AND state IN ('dispatched','unknown')`, call.Route.Claim.Message.ID, encoded, call.CallID); err != nil {
			return fmt.Errorf("save route model decision: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if overBudget {
		return domain.NewError(domain.CodeQuotaExceeded, "route model usage exceeded its reservation")
	}
	return nil
}

func validStoredRouteDecision(value app.ConversationRouteDecision, context app.ConversationRouteContext) bool {
	if !app.ValidConversationRouteModelOutput(value.ModelOutput) {
		return false
	}
	if value.InvalidOutput {
		return value.Action == app.ConversationRouteClarify && value.TaskID == "" && value.TargetMessageID == "" &&
			!value.ExplicitNew && value.ExpectedRevision == 0 && value.Reply == "" && len(value.DependencyTaskIDs) == 0 &&
			len(value.AggregateMessageIDs) == 0 &&
			value.Question != "" && utf8.ValidString(value.Question) && utf8.RuneCountInString(value.Question) <= 200
	}
	switch value.Action {
	case app.ConversationRouteNew:
		return value.TaskID == "" && value.TargetMessageID == "" && (!value.ExplicitNew || app.ConversationRouteHasExplicitNewIntent(context.Content)) &&
			value.ExpectedRevision == 0 && value.Question == "" && value.Reply == "" &&
			(!value.ExplicitNew || len(value.AggregateMessageIDs) == 0) &&
			app.ValidConversationRouteDependencyIDs(value.DependencyTaskIDs, context.Candidates) &&
			app.ValidConversationRouteAggregateIDs(value.AggregateMessageIDs, context.AggregationCandidates)
	case app.ConversationRouteJoin:
		return value.TaskID == "" && value.TargetMessageID != "" && !value.ExplicitNew && value.ExpectedRevision == 0 &&
			value.Question == "" && value.Reply == "" && len(value.DependencyTaskIDs) == 0 &&
			len(value.AggregateMessageIDs) == 0 &&
			app.ValidConversationRouteJoinTarget(value.TargetMessageID, context.RequestCandidates)
	case app.ConversationRouteReply:
		return value.TaskID == "" && value.TargetMessageID == "" && !value.ExplicitNew && value.ExpectedRevision == 0 && value.Question == "" && len(value.DependencyTaskIDs) == 0 &&
			len(value.AggregateMessageIDs) == 0 &&
			app.ValidConversationRouteReply(value.Reply)
	case app.ConversationRouteClarify:
		return value.TaskID == "" && value.TargetMessageID == "" && !value.ExplicitNew && value.ExpectedRevision == 0 && value.Reply == "" && len(value.DependencyTaskIDs) == 0 &&
			len(value.AggregateMessageIDs) == 0 && value.Question != "" &&
			utf8.ValidString(value.Question) && utf8.RuneCountInString(value.Question) <= 200
	case app.ConversationRouteUpdate, app.ConversationRouteStatus, app.ConversationRouteStop:
		if value.ExplicitNew || value.Question != "" || value.Reply != "" || len(value.DependencyTaskIDs) != 0 || len(value.AggregateMessageIDs) != 0 {
			return false
		}
		if value.Action == app.ConversationRouteStop && value.TargetMessageID != "" {
			return value.TaskID == "" && value.ExpectedRevision == 0 &&
				app.ValidConversationRoutePendingRootStop(value.TargetMessageID, context.RequestCandidates)
		}
		if value.TargetMessageID != "" {
			return false
		}
		for _, candidate := range context.Candidates {
			if candidate.TaskID == value.TaskID && candidate.Revision == value.ExpectedRevision {
				return true
			}
		}
	}
	return false
}

func staleAttemptZeroRouteDecision(value app.ConversationRouteDecision, context app.ConversationRouteContext) bool {
	if value.Action != app.ConversationRouteNew && value.Action != app.ConversationRouteReply {
		return false
	}
	for _, candidate := range context.RequestCandidates {
		if app.ValidConversationRouteJoinTarget(candidate.MessageID, context.RequestCandidates) {
			return true
		}
	}
	return false
}

func (s *Store) RecoverConversationCoordinatorModelOutput(ctx context.Context, fence app.ConversationRouteModelFence) (string, error) {
	claim := fence.Claim
	value := fence.Context
	if ctx == nil || !domain.ValidModelConnectionID(value.ModelID) || value.ActorID != claim.Message.ActorID ||
		value.ReferenceTaskID != claim.Input.ReferenceTaskID {
		return "", domain.InvalidArgument("route_context", "coordinator model recovery fence is invalid")
	}
	var output string
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := checkConversationRouteModelFence(ctx, tx, fence, value.ModelID); err != nil {
			return err
		}
		var state, modelID string
		var encoded []byte
		attempt := value.RouteAttempt
		if value.ResultReview != nil {
			attempt = 0
		}
		err := tx.QueryRow(ctx, `SELECT state,model_id,decision FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND phase=$3 AND attempt=$4`, claim.Message.ID, claim.Message.ActorID,
			conversationRouteModelPhase(value), attempt).Scan(&state, &modelID, &encoded)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "coordinator model result is unavailable")
		}
		if err != nil {
			return fmt.Errorf("read settled coordinator model result: %w", err)
		}
		var decision app.ConversationRouteDecision
		if state != "settled" || modelID != value.ModelID || json.Unmarshal(encoded, &decision) != nil ||
			!validStoredRouteDecision(decision, value) {
			return domain.NewError(domain.CodeInvalidState, "coordinator model result is not safely recoverable")
		}
		if attempt == 0 && staleAttemptZeroRouteDecision(decision, value) {
			return domain.NewError(domain.CodeConflict, "legacy coordinator result predates current request candidates")
		}
		output = decision.ModelOutput
		if output == "" {
			fallback, err := json.Marshal(struct {
				Action   app.ConversationRouteAction `json:"action"`
				TaskID   string                      `json:"task_id"`
				Question string                      `json:"question"`
				Reply    string                      `json:"reply,omitempty"`
			}{decision.Action, decision.TaskID, decision.Question, decision.Reply})
			if err != nil {
				return fmt.Errorf("encode settled coordinator model result: %w", err)
			}
			output = string(fallback)
		}
		if !app.ValidConversationRouteModelOutput(output) || output == "" {
			return domain.NewError(domain.CodeInvalidState, "coordinator model result is invalid")
		}
		return nil
	})
	return output, err
}
