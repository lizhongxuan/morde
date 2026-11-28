package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/project"
	"morde/server/internal/domain/task"
	"morde/server/internal/domain/workspace"
)

// lockConversationRouteClaim makes the persisted receipt, current service
// instance, lease, membership, account and original credential authoritative.
// Callers must hold the transaction until their reservation or action commits.
func lockConversationRouteClaim(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) error {
	if !domain.ValidMessageID(claim.Message.ID) || !domain.ValidConversationID(claim.Message.ConversationID) ||
		!domain.ValidUserID(claim.Message.OwnerID) || !domain.ValidUserID(claim.Message.ActorID) ||
		!domain.ValidServiceInstanceID(claim.ServiceInstanceID) || claim.LeaseOwner == "" || claim.LeaseVersion < 1 ||
		claim.Message.Sequence < 1 || claim.Authorization.Version < 1 {
		return domain.InvalidArgument("route_lease", "conversation route identity is invalid")
	}
	receipt := app.ConversationMessageReceipt{Message: claim.Message, ServiceInstanceID: claim.ServiceInstanceID, Input: claim.Input}
	if err := lockConversationReceiptScopeInTx(ctx, tx, receipt); err != nil {
		return err
	}
	var content, role, sourceKind, clientID string
	var sequence, routeAttempt int64
	var createdAt, aggregationStartedAt, aggregationDeadline time.Time
	var aggregationState string
	var metadata, authorization []byte
	err := tx.QueryRow(ctx, `SELECT m.content,m.role,m.source_kind,m.sequence,m.created_at,r.client_message_id,r.request_metadata,r.authorization_snapshot,r.route_attempt,
r.aggregation_started_at,r.aggregation_deadline,r.aggregation_state
FROM conversation_message_routes r
JOIN service_identity i ON i.singleton=true AND i.instance_id=r.service_instance_id
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id
 AND m.conversation_id=r.conversation_id AND m.actor_id=r.actor_id
WHERE r.service_instance_id=$1::uuid AND r.message_id=$2 AND r.owner_id=$3 AND r.actor_id=$4 AND r.conversation_id=$5
 AND r.state='processing' AND r.lease_owner=$6 AND r.lease_version=$7 AND r.lease_expires_at>clock_timestamp()
FOR UPDATE OF r`, claim.ServiceInstanceID, claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID,
		claim.Message.ConversationID, claim.LeaseOwner, claim.LeaseVersion).Scan(&content, &role, &sourceKind, &sequence, &createdAt, &clientID,
		&metadata, &authorization, &routeAttempt, &aggregationStartedAt, &aggregationDeadline, &aggregationState)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeConflict, "conversation route lease is no longer current")
	}
	if err != nil {
		return fmt.Errorf("lock conversation route claim: %w", err)
	}
	var input app.ConversationRoutingInput
	var auth task.Authorization
	if json.Unmarshal(metadata, &input) != nil || json.Unmarshal(authorization, &auth) != nil ||
		!reflect.DeepEqual(input, claim.Input) || !reflect.DeepEqual(auth, claim.Authorization) || routeAttempt != claim.RouteAttempt || routeAttempt < 0 ||
		!aggregationStartedAt.Equal(claim.AggregationStartedAt) || !aggregationDeadline.Equal(claim.AggregationDeadline) ||
		aggregationState != claim.AggregationState ||
		content != claim.Message.Content || role != string(conversation.RoleUser) ||
		(claim.Message.SourceKind != "" && sourceKind != claim.Message.SourceKind) || sequence != claim.Message.Sequence ||
		clientID != claim.Message.ClientMessageID || !createdAt.Equal(claim.Message.CreatedAt) {
		return domain.NewError(domain.CodeConflict, "conversation route receipt has changed")
	}
	if claim.Input.ResultReviewID != "" {
		return lockTaskResultReviewRouteClaim(ctx, tx, claim, sourceKind)
	}
	if sourceKind == "task_result_review" {
		return domain.NewError(domain.CodeConflict, "internal result review route is missing its review identity")
	}
	if auth.Kind != string(identity.KindSession) && auth.Kind != string(identity.KindClient) ||
		!domain.ValidTokenID(auth.TokenID) || auth.ExpiresAt == nil || !auth.ExpiresAt.After(time.Now()) {
		return domain.NewError(domain.CodeUnauthenticated, "conversation route authorization is unavailable")
	}
	principal := identity.Principal{Kind: identity.Kind(auth.Kind)}
	for _, scope := range auth.Scopes {
		principal.Scopes = append(principal.Scopes, identity.Scope(scope))
	}
	if !principal.HasScope(identity.ScopeTaskSubmit) || (auth.Kind == string(identity.KindClient) && !domain.ValidClientInstallationID(auth.ClientInstallationID)) {
		return domain.NewError(domain.CodeUnauthenticated, "conversation route authorization is unavailable")
	}
	var active bool
	var currentScopes []string
	err = tx.QueryRow(ctx, `SELECT u.status='approved' AND t.revoked_at IS NULL AND t.expires_at>clock_timestamp()
 AND t.kind=$3 AND COALESCE(t.client_installation_id,'')=$4,t.scopes
FROM users u JOIN tokens t ON t.owner_id=u.id WHERE u.id=$1 AND t.id=$2 FOR SHARE OF u,t`,
		claim.Message.ActorID, auth.TokenID, auth.Kind, auth.ClientInstallationID).Scan(&active, &currentScopes)
	if err != nil || !active {
		return domain.NewError(domain.CodeUnauthenticated, "conversation route credential is unavailable")
	}
	if auth.ClientInstallationID != "" {
		if err := checkModelInstallation(ctx, tx, auth.ClientInstallationID, claim.Message.ActorID); err != nil {
			return err
		}
	}
	currentPrincipal := identity.Principal{Kind: identity.Kind(auth.Kind)}
	for _, scope := range currentScopes {
		currentPrincipal.Scopes = append(currentPrincipal.Scopes, identity.Scope(scope))
	}
	if !currentPrincipal.HasScope(identity.ScopeTaskSubmit) {
		return domain.NewError(domain.CodeUnauthenticated, "conversation route authorization is unavailable")
	}
	return nil
}

func requireConversationRouteScope(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim, scope identity.Scope) error {
	principal := identity.Principal{Kind: identity.Kind(claim.Authorization.Kind)}
	for _, value := range claim.Authorization.Scopes {
		principal.Scopes = append(principal.Scopes, identity.Scope(value))
	}
	if !principal.HasScope(scope) {
		return domain.NewError(domain.CodeForbidden, "conversation task action is not authorized")
	}
	var scopes []string
	if err := tx.QueryRow(ctx, `SELECT scopes FROM tokens WHERE id=$1 AND owner_id=$2 FOR SHARE`,
		claim.Authorization.TokenID, claim.Message.ActorID).Scan(&scopes); err != nil {
		return domain.NewError(domain.CodeUnauthenticated, "conversation route credential is unavailable")
	}
	current := identity.Principal{Kind: identity.Kind(claim.Authorization.Kind)}
	for _, value := range scopes {
		current.Scopes = append(current.Scopes, identity.Scope(value))
	}
	if !current.HasScope(scope) {
		return domain.NewError(domain.CodeForbidden, "conversation task action is not authorized")
	}
	return nil
}

// LoadConversationRouteSnapshot excludes other initiators' unfinished Tasks
// and keeps the original IDs and sequence numbers for gateway revalidation.
func (s *Store) LoadConversationRouteSnapshot(ctx context.Context, claim app.ConversationRouteClaim) (app.ConversationRouteSnapshot, error) {
	var snapshot app.ConversationRouteSnapshot
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		var err error
		snapshot, err = loadConversationRouteSnapshotInTx(ctx, tx, claim, false)
		if err == nil && snapshot.ResultReviewSuperseded {
			err = finishConversationRoute(ctx, tx, claim, "routed", "", "", true)
		}
		return err
	})
	return snapshot, err
}

// BeginConversationRouteAggregation schedules final classification without
// extending the receipt deadline and fences the first classifier lease.
func (s *Store) BeginConversationRouteAggregation(ctx context.Context, claim app.ConversationRouteClaim,
	_ app.ConversationRouteSnapshot, decision app.ConversationRouteDecision) (bool, error) {
	if decision.Action != app.ConversationRouteNew || decision.ExplicitNew || len(decision.AggregateMessageIDs) != 0 ||
		claim.Input.ResultReviewID != "" || len(claim.Input.ClientAttachmentIDs) != 0 {
		return false, nil
	}
	var started bool
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		if claim.AggregationState != "open" ||
			claim.AggregationDeadline.Sub(claim.AggregationStartedAt) != app.ConversationRouteAggregationWindow {
			return nil
		}
		progress, err := conversationRouteAggregationProgress(ctx, tx, claim.Message.ID)
		if err != nil {
			return err
		}
		if progress.deadlinePassed && progress.candidates == 0 {
			return nil
		}
		if claim.RouteAttempt >= int64(^uint64(0)>>1) {
			return domain.NewError(domain.CodeInvalidState, "conversation route attempt limit reached")
		}
		result, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET aggregation_state='waiting',
state='pending',route_attempt=route_attempt+1,lease_owner='',lease_expires_at=NULL,
lease_version=lease_version+1,updated_at=clock_timestamp()
WHERE message_id=$1 AND state='processing' AND aggregation_state='open'
 AND lease_owner=$2 AND lease_version=$3 AND route_attempt=$4`, claim.Message.ID, claim.LeaseOwner, claim.LeaseVersion, claim.RouteAttempt)
		if err != nil {
			return fmt.Errorf("start fixed conversation aggregation window: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "conversation aggregation window changed")
		}
		if err := syncConversationDuplicateMessages(ctx, tx, claim.Message.ID); err != nil {
			return err
		}
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: claim.Message.OwnerID,
			ObjectType: "message", ObjectID: claim.Message.ID, Operation: "upsert", MutationID: claim.Message.ClientMessageID}); err != nil {
			return err
		}
		started = true
		return nil
	})
	return started, err
}

// ResolveConversationRouteAggregation uses the conversation lock to decide
// whether a candidate is still unresolved. A reply or independent task may
// have committed after the worker read its snapshot and must not fail the root.
func (s *Store) ResolveConversationRouteAggregation(ctx context.Context, claim app.ConversationRouteClaim) error {
	if claim.AggregationState != "waiting" {
		return domain.InvalidArgument("route_aggregation", "conversation route is not waiting for aggregation")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		progress, err := conversationRouteAggregationProgress(ctx, tx, claim.Message.ID)
		if err != nil {
			return err
		}
		state, reason := "pending", ""
		if progress.unresolved > 0 && progress.clarification == 0 && (progress.attention > 0 || progress.waitExpired) {
			state, reason = "failed", "routing_context_unavailable"
		}
		result, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET state=$2,failure_reason=$3,
lease_owner='',lease_expires_at=NULL,available_after=clock_timestamp(),updated_at=clock_timestamp()
WHERE message_id=$1 AND state='processing' AND lease_owner=$4 AND lease_version=$5 AND lease_expires_at>clock_timestamp()`,
			claim.Message.ID, state, reason, claim.LeaseOwner, claim.LeaseVersion)
		if err != nil {
			return fmt.Errorf("resolve conversation aggregation wait: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "conversation route lease is no longer current")
		}
		if err := syncConversationDuplicateMessages(ctx, tx, claim.Message.ID); err != nil {
			return err
		}
		return recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: claim.Message.OwnerID,
			ObjectType: "message", ObjectID: claim.Message.ID, Operation: "upsert", MutationID: claim.Message.ClientMessageID})
	})
}

// A later client message can have a smaller server sequence when uploads arrive
// out of order. Do not expose any causal descendant as this message's history.
func conversationRouteDescendantIDs(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) ([]string, error) {
	rows, err := tx.Query(ctx, `WITH RECURSIVE descendants(message_id,client_id,path,depth) AS (
 SELECT r.message_id,r.client_message_id,ARRAY[r.client_message_id],1 FROM conversation_message_routes r
 WHERE r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4 AND r.previous_client_message_id=$5
 UNION ALL
 SELECT r.message_id,r.client_message_id,d.path||r.client_message_id,d.depth+1
 FROM descendants d JOIN conversation_message_routes r ON r.service_instance_id=$1::uuid AND r.owner_id=$2
  AND r.actor_id=$3 AND r.conversation_id=$4 AND r.previous_client_message_id=d.client_id
 WHERE d.depth<128 AND NOT r.client_message_id=ANY(d.path)
)
SELECT message_id,depth FROM descendants LIMIT 1025`, claim.ServiceInstanceID, claim.Message.OwnerID,
		claim.Message.ActorID, claim.Message.ConversationID, claim.Message.ClientMessageID)
	if err != nil {
		return nil, fmt.Errorf("read conversation causal descendants: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		var depth int
		if err := rows.Scan(&id, &depth); err != nil {
			return nil, fmt.Errorf("scan conversation causal descendant: %w", err)
		}
		if depth >= 128 || len(ids) >= 1024 {
			return nil, domain.NewError(domain.CodeInvalidState, "conversation causal chain is too large to route safely")
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read conversation causal descendants: %w", err)
	}
	rows.Close()
	if len(ids) == 0 {
		return ids, nil
	}
	// A completed result or a merged result is also future context when any
	// of its source messages is a causal descendant.
	resultRows, err := tx.Query(ctx, `SELECT m.id FROM conversation_messages m
WHERE m.owner_id=$1 AND m.conversation_id=$2 AND m.role='assistant' AND (
  (m.task_id IS NOT NULL AND EXISTS (SELECT 1 FROM conversation_messages origin
    WHERE origin.owner_id=m.owner_id AND origin.conversation_id=m.conversation_id AND origin.task_id=m.task_id
      AND origin.role='user' AND origin.id=ANY($3::text[])))
  OR EXISTS (SELECT 1 FROM conversation_result_batches batch JOIN conversation_result_sources source
    ON source.batch_message_id=batch.message_id AND source.owner_id=batch.owner_id AND source.actor_id=batch.actor_id
      AND source.conversation_id=batch.conversation_id
    WHERE batch.message_id=m.id AND batch.owner_id=m.owner_id AND batch.conversation_id=m.conversation_id
      AND batch.service_instance_id=$4::uuid AND source.user_message_id=ANY($3::text[]))
) LIMIT 1025`, claim.Message.OwnerID, claim.Message.ConversationID, ids, claim.ServiceInstanceID)
	if err != nil {
		return nil, fmt.Errorf("read causal descendant results: %w", err)
	}
	defer resultRows.Close()
	resultCount := 0
	for resultRows.Next() {
		var id string
		if err := resultRows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan causal descendant result: %w", err)
		}
		resultCount++
		if resultCount > 1024 || len(ids) >= 2048 {
			return nil, domain.NewError(domain.CodeInvalidState, "conversation causal history is too large to route safely")
		}
		ids = append(ids, id)
	}
	if err := resultRows.Err(); err != nil {
		return nil, fmt.Errorf("read causal descendant results: %w", err)
	}
	return ids, nil
}

func loadConversationRouteSnapshotInTx(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim, lockTasks bool) (app.ConversationRouteSnapshot, error) {
	value := app.ConversationRouteSnapshot{Context: app.ConversationRouteContext{ActorID: claim.Message.ActorID, RouteAttempt: claim.RouteAttempt,
		Content: claim.Message.Content, TimeZone: claim.Input.TimeZone, ServerReceivedAt: claim.Message.CreatedAt,
		ReferenceTaskID: claim.Input.ReferenceTaskID, CurrentMessageHasAttachments: len(claim.Input.ClientAttachmentIDs) != 0,
		CurrentRequestModelID: claim.Input.ModelID,
		History:               []app.ConversationRouteHistory{}, Candidates: []app.ConversationRouteCandidate{},
		RequestCandidates: []app.ConversationRouteRequestCandidate{}, AggregationCandidates: []app.ConversationRouteAggregationCandidate{}}}
	scopeDigest, err := app.ConversationRouteExecutionScopeDigest(claim.Input)
	if err != nil {
		return app.ConversationRouteSnapshot{}, err
	}
	value.Context.ExecutionScopeDigest = scopeDigest
	controlScopeDigest, err := app.ConversationRouteControlScopeDigest(claim.Input)
	if err != nil {
		return app.ConversationRouteSnapshot{}, err
	}
	value.Context.ControlScopeDigest = controlScopeDigest
	if claim.Input.ResultReviewID != "" {
		resultReview, err := loadConversationTaskResultReviewInTx(ctx, tx, claim)
		if err != nil {
			return app.ConversationRouteSnapshot{}, err
		}
		value.Context.ResultReview = resultReview
		value.ResultReviewSuperseded = resultReview == nil
	}
	if claim.Input.ProjectID != "" && claim.Input.ReferenceTaskID != "" {
		query := `SELECT waiting_reason FROM tasks WHERE id=$1 AND owner_id=$2 AND conversation_id=$3
 AND project_id=$4 AND initiator_id=$5`
		if lockTasks {
			query += ` FOR UPDATE`
		}
		var reason string
		err := tx.QueryRow(ctx, query, claim.Input.ReferenceTaskID, claim.Message.OwnerID, claim.Message.ConversationID,
			claim.Input.ProjectID, claim.Message.ActorID).Scan(&reason)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return app.ConversationRouteSnapshot{}, fmt.Errorf("read referenced Task recovery state: %w", err)
		}
		value.Context.ReferenceUnavailable = err == nil && reason == "project_recovery_unavailable"
	}
	if claim.Input.PreviousClientMessageID != "" {
		var state, previousTaskID, previousMessageID string
		var previousSequence int64
		err := tx.QueryRow(ctx, `SELECT r.state,COALESCE(revision.task_id,m.task_id,''),m.id,m.sequence FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id
LEFT JOIN task_revision_intents revision ON revision.message_id=m.id AND revision.initiator_id=r.actor_id AND revision.conversation_id=r.conversation_id
WHERE r.service_instance_id=$1::uuid AND r.actor_id=$2 AND r.conversation_id=$3 AND r.client_message_id=$4 AND r.owner_id=$5`,
			claim.ServiceInstanceID, claim.Message.ActorID, claim.Message.ConversationID,
			claim.Input.PreviousClientMessageID, claim.Message.OwnerID).Scan(&state, &previousTaskID, &previousMessageID, &previousSequence)
		if errors.Is(err, pgx.ErrNoRows) {
			value.PreviousMissing = true
			value.Context.PreviousMissing = true
			value.Context.PreviousUnresolved = true
		} else if err != nil {
			return app.ConversationRouteSnapshot{}, fmt.Errorf("read previous conversation route: %w", err)
		} else {
			value.Context.PreviousUnresolved = state != "routed"
			value.Context.PreviousMessageID, value.Context.PreviousTaskID = previousMessageID, previousTaskID
			value.PreviousRoutingState, value.PreviousTaskID = state, previousTaskID
			value.PreviousMessageID, value.PreviousSequence = previousMessageID, previousSequence
		}
		err = tx.QueryRow(ctx, `WITH RECURSIVE chain(client_id,previous_id) AS (
	 SELECT r.client_message_id,r.previous_client_message_id
 FROM conversation_message_routes r WHERE r.service_instance_id=$1::uuid AND r.actor_id=$2 AND r.conversation_id=$3
  AND r.client_message_id=$4 AND r.owner_id=$6
	 UNION
	 SELECT r.client_message_id,r.previous_client_message_id
 FROM chain c JOIN conversation_message_routes r ON r.service_instance_id=$1::uuid AND r.actor_id=$2
  AND r.conversation_id=$3 AND r.owner_id=$6 AND r.client_message_id=c.previous_id
)
SELECT COALESCE(bool_or(c.client_id=$5),false) OR COALESCE(bool_and(c.previous_id IS NOT NULL AND EXISTS (
	SELECT 1 FROM chain ancestor WHERE ancestor.client_id=c.previous_id
)),false) FROM chain c`,
			claim.ServiceInstanceID, claim.Message.ActorID, claim.Message.ConversationID,
			claim.Input.PreviousClientMessageID, claim.Message.ClientMessageID, claim.Message.OwnerID).Scan(&value.PreviousCycle)
		if err != nil {
			return app.ConversationRouteSnapshot{}, fmt.Errorf("verify conversation predecessor chain: %w", err)
		}
	}
	descendants, err := conversationRouteDescendantIDs(ctx, tx, claim)
	if err != nil {
		return app.ConversationRouteSnapshot{}, err
	}
	rows, err := tx.Query(ctx, `SELECT m.id,m.sequence,COALESCE(m.task_id,''),COALESCE(m.output_task_revision,0),
COALESCE(m.output_result_event_sequence,0),m.role,
 CASE WHEN octet_length(m.content)<=4096 THEN m.content ELSE left(m.content,511)||E'\n…\n'||right(m.content,511) END,
 octet_length(m.content),sha256(convert_to(m.content,'UTF8')),octet_length(m.content)>4096 FROM conversation_messages m
LEFT JOIN tasks t ON t.id=m.task_id AND t.owner_id=m.owner_id AND t.conversation_id=m.conversation_id
WHERE m.owner_id=$1 AND m.conversation_id=$2 AND m.sequence<$3 AND m.source_kind NOT IN ('task_result_review','conversation_variant_input')
 AND m.id<>ALL($7::text[])
 AND ($8::text='' OR COALESCE(m.variant_group_id,CASE WHEN m.role='user' THEN m.id ELSE (
   SELECT COALESCE(origin.variant_group_id,origin.id) FROM conversation_messages origin
   WHERE origin.owner_id=m.owner_id AND origin.conversation_id=m.conversation_id AND origin.actor_id=m.actor_id
    AND origin.role='user' AND origin.source_kind<>'conversation_variant_input'
    AND ((m.task_id IS NOT NULL AND origin.task_id=m.task_id) OR (m.task_id IS NULL AND origin.sequence<m.sequence))
   ORDER BY CASE WHEN m.task_id IS NOT NULL THEN origin.sequence END ASC,
    CASE WHEN m.task_id IS NULL THEN origin.sequence END DESC LIMIT 1
 ) END,'')<>$8)
 AND (
   (m.actor_id=$4 AND (m.task_id IS NULL OR t.initiator_id=$4)
    AND (m.role='user' OR (m.role='assistant' AND (
      ($9::text='' AND m.task_id IS NULL AND m.source_kind='conversation' AND m.delivery_status='applied')
      OR (m.output_status IS NULL AND (t.status='completed' OR EXISTS (
        SELECT 1 FROM conversation_result_batches batch
        WHERE batch.message_id=m.id AND batch.published_message_id=m.id AND batch.state='published'
          AND batch.service_instance_id=$5::uuid
          AND batch.owner_id=m.owner_id AND batch.actor_id=m.actor_id AND batch.conversation_id=m.conversation_id
          AND EXISTS (SELECT 1 FROM conversation_result_sources source
            WHERE source.batch_message_id=batch.message_id AND source.owner_id=batch.owner_id
              AND source.actor_id=batch.actor_id AND source.conversation_id=batch.conversation_id AND source.state='published')
      ))) OR (m.output_status='completed' AND t.id=m.task_id AND t.initiator_id=m.actor_id AND EXISTS (
        SELECT 1 FROM task_events result WHERE result.task_id=m.task_id
          AND result.sequence=m.output_result_event_sequence AND result.task_revision=m.output_task_revision
          AND result.event_type='task.result' AND result.result_content_sha256=sha256(convert_to(m.content,'UTF8'))
      ))
    ))))
   OR (m.actor_id<>$4 AND $6<>'' AND m.source_kind NOT IN ('history_import','legacy_instant')
    AND EXISTS (SELECT 1 FROM conversations project_scope WHERE project_scope.id=m.conversation_id
      AND project_scope.owner_id=m.owner_id AND project_scope.project_id=$6)
    AND (`+projectSharedHistoryEvidenceSQL+`))
 )
ORDER BY m.sequence DESC LIMIT 8`, claim.Message.OwnerID, claim.Message.ConversationID, claim.Message.Sequence, claim.Message.ActorID,
		claim.ServiceInstanceID, claim.Input.ProjectID, descendants, claim.Input.VariantGroupID, claim.Input.ResultReviewID)
	if err != nil {
		return app.ConversationRouteSnapshot{}, fmt.Errorf("read route history: %w", err)
	}
	for rows.Next() {
		var item app.ConversationRouteHistory
		var role string
		if err := rows.Scan(&item.MessageID, &item.Sequence, &item.TaskID, &item.TaskRevision, &item.EventSequence, &role,
			&item.Content, &item.ContentBytes, &item.SourceSHA256, &item.Truncated); err != nil {
			rows.Close()
			return app.ConversationRouteSnapshot{}, fmt.Errorf("scan route history: %w", err)
		}
		item.Role = conversation.Role(role)
		item.ContentSHA256 = hex.EncodeToString(item.SourceSHA256)
		value.Context.History = append(value.Context.History, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return app.ConversationRouteSnapshot{}, fmt.Errorf("read route history: %w", err)
	}
	for i, j := 0, len(value.Context.History)-1; i < j; i, j = i+1, j-1 {
		value.Context.History[i], value.Context.History[j] = value.Context.History[j], value.Context.History[i]
	}
	// Keep recent published Task results reachable after unrelated turns roll them out of the newest-message window.
	resultRows, err := tx.Query(ctx, `SELECT m.id,m.sequence,COALESCE(m.task_id,''),COALESCE(m.output_task_revision,0),
COALESCE(m.output_result_event_sequence,0),m.role,
 CASE WHEN octet_length(m.content)<=4096 THEN m.content ELSE left(m.content,511)||E'\n…\n'||right(m.content,511) END,
 octet_length(m.content),sha256(convert_to(m.content,'UTF8')),octet_length(m.content)>4096 FROM conversation_messages m
WHERE m.owner_id=$1 AND m.conversation_id=$2 AND m.sequence<$3 AND m.role='assistant'
 AND m.source_kind NOT IN ('history_import','legacy_instant') AND m.delivery_status='applied'
 AND m.id<>ALL($7::text[])
 AND ($8::text='' OR COALESCE(m.variant_group_id,(
   SELECT COALESCE(origin.variant_group_id,origin.id) FROM conversation_messages origin
   WHERE origin.owner_id=m.owner_id AND origin.conversation_id=m.conversation_id AND origin.actor_id=m.actor_id
    AND origin.role='user' AND origin.source_kind<>'conversation_variant_input'
    AND ((m.task_id IS NOT NULL AND origin.task_id=m.task_id) OR (m.task_id IS NULL AND origin.sequence<m.sequence))
   ORDER BY CASE WHEN m.task_id IS NOT NULL THEN origin.sequence END ASC,
    CASE WHEN m.task_id IS NULL THEN origin.sequence END DESC LIMIT 1
 ),'')<>$8)
 AND EXISTS (SELECT 1 FROM conversation_result_batches batch JOIN conversation_result_sources source
   ON source.batch_message_id=batch.message_id AND source.owner_id=batch.owner_id
    AND source.actor_id=batch.actor_id AND source.conversation_id=batch.conversation_id AND source.state='published'
   WHERE batch.message_id=m.id AND batch.published_message_id=m.id AND batch.state='published'
    AND batch.owner_id=m.owner_id AND batch.actor_id=m.actor_id AND batch.conversation_id=m.conversation_id)
 AND ((m.actor_id=$4 AND EXISTS (SELECT 1 FROM conversation_result_batches batch
    WHERE batch.message_id=m.id AND batch.published_message_id=m.id AND batch.state='published'
     AND batch.service_instance_id=$5::uuid AND batch.owner_id=m.owner_id AND batch.actor_id=m.actor_id
     AND batch.conversation_id=m.conversation_id))
   OR (m.actor_id<>$4 AND $6<>'' AND EXISTS (SELECT 1 FROM conversations project_scope
    WHERE project_scope.id=m.conversation_id AND project_scope.owner_id=m.owner_id AND project_scope.project_id=$6)
    AND (`+projectSharedHistoryEvidenceSQL+`)))
ORDER BY m.sequence DESC LIMIT 4`, claim.Message.OwnerID, claim.Message.ConversationID, claim.Message.Sequence,
		claim.Message.ActorID, claim.ServiceInstanceID, claim.Input.ProjectID, descendants, claim.Input.VariantGroupID)
	if err != nil {
		return app.ConversationRouteSnapshot{}, fmt.Errorf("read recent published Task results: %w", err)
	}
	seenHistory := make(map[string]struct{}, len(value.Context.History))
	for _, item := range value.Context.History {
		seenHistory[item.MessageID] = struct{}{}
	}
	publishedResultIDs := make([]string, 0, 4)
	for resultRows.Next() {
		var item app.ConversationRouteHistory
		var role string
		if err := resultRows.Scan(&item.MessageID, &item.Sequence, &item.TaskID, &item.TaskRevision, &item.EventSequence, &role,
			&item.Content, &item.ContentBytes, &item.SourceSHA256, &item.Truncated); err != nil {
			resultRows.Close()
			return app.ConversationRouteSnapshot{}, fmt.Errorf("scan recent published Task result: %w", err)
		}
		publishedResultIDs = append(publishedResultIDs, item.MessageID)
		if _, ok := seenHistory[item.MessageID]; ok {
			continue
		}
		item.Role = conversation.Role(role)
		item.ContentSHA256 = hex.EncodeToString(item.SourceSHA256)
		value.Context.History = append(value.Context.History, item)
		seenHistory[item.MessageID] = struct{}{}
	}
	err = resultRows.Err()
	resultRows.Close()
	if err != nil {
		return app.ConversationRouteSnapshot{}, fmt.Errorf("read recent published Task results: %w", err)
	}
	if len(publishedResultIDs) != 0 {
		// Reload source requests only for the current actor; project sharing rules for recent history remain unchanged.
		sourceRows, err := tx.Query(ctx, `SELECT DISTINCT origin.id,origin.sequence,source.task_id,
 CASE WHEN octet_length(origin.content)<=4096 THEN origin.content ELSE left(origin.content,511)||E'\n…\n'||right(origin.content,511) END,
 octet_length(origin.content),sha256(convert_to(origin.content,'UTF8')),octet_length(origin.content)>4096
FROM conversation_result_sources source
JOIN conversation_result_batches batch ON batch.message_id=source.batch_message_id
 AND batch.owner_id=source.owner_id AND batch.actor_id=source.actor_id AND batch.conversation_id=source.conversation_id
JOIN tasks t ON t.id=source.task_id AND t.owner_id=source.owner_id
 AND t.conversation_id=source.conversation_id AND t.initiator_id=source.actor_id AND t.status='completed'
JOIN conversation_messages origin ON origin.task_id=source.task_id AND origin.owner_id=source.owner_id
 AND origin.actor_id=source.actor_id AND origin.conversation_id=source.conversation_id AND origin.role='user'
WHERE source.batch_message_id=ANY($1::text[]) AND source.owner_id=$2 AND source.actor_id=$3
 AND source.conversation_id=$4 AND source.state='published'
 AND batch.state='published' AND batch.published_message_id=source.batch_message_id
 AND batch.service_instance_id=$5::uuid
 AND origin.delivery_status='applied' AND origin.source_kind NOT IN ('history_import','legacy_instant')
 AND ($7::text='' OR COALESCE(origin.variant_group_id,origin.id)<>$7)
 AND origin.id<>ALL($6::text[])
 AND (origin.id=source.user_message_id OR origin.sequence=(
   SELECT min(initial.sequence) FROM conversation_messages initial
   WHERE initial.task_id=source.task_id AND initial.owner_id=source.owner_id
    AND initial.actor_id=source.actor_id AND initial.conversation_id=source.conversation_id
    AND initial.role='user' AND initial.delivery_status='applied'
    AND initial.source_kind NOT IN ('history_import','legacy_instant')))
ORDER BY origin.sequence`, publishedResultIDs, claim.Message.OwnerID, claim.Message.ActorID,
			claim.Message.ConversationID, claim.ServiceInstanceID, descendants, claim.Input.VariantGroupID)
		if err != nil {
			return app.ConversationRouteSnapshot{}, fmt.Errorf("read published Task source requests: %w", err)
		}
		for sourceRows.Next() {
			var item app.ConversationRouteHistory
			if err := sourceRows.Scan(&item.MessageID, &item.Sequence, &item.TaskID, &item.Content,
				&item.ContentBytes, &item.SourceSHA256, &item.Truncated); err != nil {
				sourceRows.Close()
				return app.ConversationRouteSnapshot{}, fmt.Errorf("scan published Task source request: %w", err)
			}
			if _, ok := seenHistory[item.MessageID]; ok {
				continue
			}
			item.Role = conversation.RoleUser
			item.ContentSHA256 = hex.EncodeToString(item.SourceSHA256)
			value.Context.History = append(value.Context.History, item)
			seenHistory[item.MessageID] = struct{}{}
		}
		err = sourceRows.Err()
		sourceRows.Close()
		if err != nil {
			return app.ConversationRouteSnapshot{}, fmt.Errorf("read published Task source requests: %w", err)
		}
	}
	sort.Slice(value.Context.History, func(i, j int) bool {
		return value.Context.History[i].Sequence < value.Context.History[j].Sequence
	})
	if value.PreviousSequence > claim.Message.Sequence && !value.PreviousCycle {
		var item app.ConversationRouteHistory
		var role string
		err := tx.QueryRow(ctx, `SELECT id,sequence,COALESCE(task_id,''),COALESCE(output_task_revision,0),
COALESCE(output_result_event_sequence,0),role,
 CASE WHEN octet_length(content)<=4096 THEN content ELSE left(content,511)||E'\n…\n'||right(content,511) END,
 octet_length(content),sha256(convert_to(content,'UTF8')),octet_length(content)>4096 FROM conversation_messages
WHERE id=$1 AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND role='user'`,
			value.PreviousMessageID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID).
			Scan(&item.MessageID, &item.Sequence, &item.TaskID, &item.TaskRevision, &item.EventSequence, &role,
				&item.Content, &item.ContentBytes, &item.SourceSHA256, &item.Truncated)
		if err == nil {
			item.Role = conversation.Role(role)
			item.ContentSHA256 = hex.EncodeToString(item.SourceSHA256)
			value.Context.History = append(value.Context.History, item)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return app.ConversationRouteSnapshot{}, fmt.Errorf("read late predecessor: %w", err)
		}
	}
	query := `SELECT id,initiator_id,objective,status,desired_revision FROM tasks
WHERE owner_id=$1 AND conversation_id=$2 AND initiator_id=$3
 AND status IN ('queued','running','waiting_user','pausing','paused','cancelling')
 AND project_id IS NOT DISTINCT FROM NULLIF($11,'')
 AND waiting_reason<>'project_recovery_unavailable'
 AND NOT EXISTS (SELECT 1 FROM conversation_route_internal_tool_calls internal_tool WHERE internal_tool.task_id=tasks.id)
 AND ($8::text='' OR NOT EXISTS (SELECT 1 FROM conversation_messages origin
   WHERE origin.task_id=tasks.id AND origin.owner_id=tasks.owner_id AND origin.conversation_id=tasks.conversation_id
    AND origin.actor_id=tasks.initiator_id AND origin.role='user'
    AND COALESCE(origin.variant_group_id,origin.id)=$8))
 AND NOT EXISTS (SELECT 1 FROM conversation_messages future WHERE future.task_id=tasks.id AND future.id=ANY($7::text[]))
 AND (created_at<=$4 OR EXISTS (SELECT 1 FROM conversation_messages origin
   WHERE origin.task_id=tasks.id AND origin.owner_id=tasks.owner_id AND origin.conversation_id=tasks.conversation_id
     AND origin.actor_id=tasks.initiator_id AND origin.role='user' AND (origin.sequence<$5 OR origin.id=$6)))
ORDER BY CASE WHEN id=$9 AND $9<>'' THEN 0 WHEN id=$10 AND $10<>'' THEN 1 ELSE 2 END,created_at DESC,id LIMIT 65`
	if lockTasks {
		query += ` FOR UPDATE`
	}
	rows, err = tx.Query(ctx, query, claim.Message.OwnerID, claim.Message.ConversationID, claim.Message.ActorID,
		claim.Message.CreatedAt, claim.Message.Sequence, value.PreviousMessageID, descendants, claim.Input.VariantGroupID,
		claim.Input.ReferenceTaskID, value.PreviousTaskID, claim.Input.ProjectID)
	if err != nil {
		return app.ConversationRouteSnapshot{}, fmt.Errorf("read route task candidates: %w", err)
	}
	for rows.Next() {
		if len(value.Context.Candidates) == 64 {
			value.Context.CandidateSetIncomplete = true
			value.Context.TaskCandidateSetIncomplete = true
			break
		}
		var item app.ConversationRouteCandidate
		var status string
		if err := rows.Scan(&item.TaskID, &item.InitiatorID, &item.Objective, &status, &item.Revision); err != nil {
			rows.Close()
			return app.ConversationRouteSnapshot{}, fmt.Errorf("scan route task candidate: %w", err)
		}
		item.Status = task.Status(status)
		value.Context.Candidates = append(value.Context.Candidates, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return app.ConversationRouteSnapshot{}, fmt.Errorf("read route task candidates: %w", err)
	}
	if claim.AggregationState == "waiting" && claim.Message.TaskID == "" {
		progress, err := conversationRouteAggregationProgress(ctx, tx, claim.Message.ID)
		if err != nil {
			return app.ConversationRouteSnapshot{}, err
		}
		value.AggregationUnresolved = progress.unresolved > 0
		value.AggregationNeedsClarification = progress.clarification > 0
		value.AggregationCanWait = progress.unresolved > 0 && progress.attention == 0 && !progress.waitExpired
		value.Context.Content = claim.Message.Content
	} else {
		value.Context.Content, value.Context.ContentSources, err = joinedConversationRouteContent(ctx, tx, claim)
		if err != nil {
			return app.ConversationRouteSnapshot{}, err
		}
		if len(value.Context.ContentSources) > 0 {
			digest := sha256.Sum256([]byte(value.Context.Content))
			value.Context.ContentBytes = int64(len(value.Context.Content))
			value.Context.ContentSHA256 = digest[:]
		}
	}
	if value.Context.Content == claim.Message.Content {
		value.Context.CurrentMessageID = claim.Message.ID
	}
	requestRows, err := tx.Query(ctx, `SELECT r.message_id,m.sequence,
 CASE WHEN octet_length(m.content)<=4096 THEN m.content ELSE left(m.content,1023)||E'\n…\n'||right(m.content,1023) END,
 CASE WHEN octet_length(m.content)>4096 THEN sha256(convert_to(m.content,'UTF8')) END,octet_length(m.content)>4096,
 r.state,COALESCE(m.task_id,''),
COALESCE(t.status,''),r.aggregation_state IN ('open','waiting'),
r.aggregation_state IN ('open','waiting') AND $10::timestamptz>r.aggregation_deadline,EXISTS(SELECT 1 FROM conversation_route_model_calls call
 WHERE call.message_id=r.message_id AND call.actor_id=r.actor_id AND call.phase='normal'
  AND call.attempt=r.route_attempt AND call.state='settled' AND call.decision->>'Action'='reply'),r.request_metadata,r.authorization_snapshot
FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.actor_id=r.actor_id
 AND m.conversation_id=r.conversation_id
LEFT JOIN tasks t ON t.id=m.task_id AND t.owner_id=m.owner_id AND t.conversation_id=m.conversation_id
WHERE r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
 AND r.message_id<>$5 AND r.duplicate_of_message_id IS NULL
 AND ($11::text='' OR COALESCE(m.variant_group_id,m.id)<>$11)
 AND (m.sequence<$6 OR r.client_message_id=$7 OR r.message_id=(
   SELECT prior.duplicate_of_message_id FROM conversation_message_routes prior
   WHERE prior.service_instance_id=$1::uuid AND prior.owner_id=$2 AND prior.actor_id=$3
    AND prior.conversation_id=$4 AND prior.client_message_id=$7))
 AND (r.state IN ('pending','processing','waiting_user','waiting_adjustment','failed')
   OR t.status IN ('queued','running','waiting_user','pausing','paused','cancelling')
   OR (r.state='routed' AND m.created_at >= $8::timestamptz-interval '30 minutes'))
 AND $9=''
ORDER BY CASE WHEN r.client_message_id=$7 THEN 0 ELSE 1 END,
 CASE WHEN r.state IN ('pending','processing','waiting_user','waiting_adjustment','failed')
 OR t.status IN ('queued','running','waiting_user','pausing','paused','cancelling') THEN 0 ELSE 1 END,
		m.sequence DESC LIMIT 101`, claim.ServiceInstanceID, claim.Message.OwnerID, claim.Message.ActorID,
		claim.Message.ConversationID, claim.Message.ID, claim.Message.Sequence, claim.Input.PreviousClientMessageID,
		claim.Message.CreatedAt, claim.Input.ResultReviewID, claim.AggregationStartedAt, claim.Input.VariantGroupID)
	if err != nil {
		return app.ConversationRouteSnapshot{}, fmt.Errorf("read prior conversation route candidates: %w", err)
	}
	for requestRows.Next() {
		if len(value.Context.RequestCandidates) == 100 {
			value.Context.CandidateSetIncomplete = true
			value.Context.RequestCandidateSetIncomplete = true
			break
		}
		var candidate app.ConversationRouteRequestCandidate
		var routeStatus, taskStatus string
		var directReply, aggregationWaiting, aggregationClosed bool
		var metadata, authorization []byte
		if err := requestRows.Scan(&candidate.MessageID, &candidate.Sequence, &candidate.Content, &candidate.ContentSHA256,
			&candidate.ContentTruncated, &routeStatus,
			&candidate.TaskID, &taskStatus, &aggregationWaiting, &aggregationClosed, &directReply, &metadata, &authorization); err != nil {
			requestRows.Close()
			return app.ConversationRouteSnapshot{}, fmt.Errorf("scan prior conversation route candidate: %w", err)
		}
		candidate.RoutingStatus = conversation.RoutingStatus(routeStatus)
		candidate.DirectReply = candidate.TaskID == "" && directReply
		candidate.AggregationWaiting = candidate.TaskID == "" && aggregationWaiting
		candidate.AggregationClosed = candidate.TaskID == "" && aggregationClosed
		if candidate.TaskID != "" {
			candidate.TaskStatus = task.Status(taskStatus)
		}
		if err := json.Unmarshal(metadata, &candidate.RequestInput); err != nil {
			requestRows.Close()
			return app.ConversationRouteSnapshot{}, fmt.Errorf("decode prior route candidate input: %w", err)
		}
		if err := json.Unmarshal(authorization, &candidate.Authorization); err != nil {
			requestRows.Close()
			return app.ConversationRouteSnapshot{}, fmt.Errorf("decode prior route authorization: %w", err)
		}
		candidate.SameAuthorization = reflect.DeepEqual(candidate.Authorization, claim.Authorization)
		candidate.ExecutionScopeDigest, err = app.ConversationRouteExecutionScopeDigest(candidate.RequestInput)
		if err != nil {
			requestRows.Close()
			return app.ConversationRouteSnapshot{}, err
		}
		candidate.SameExecutionScope = bytes.Equal(candidate.ExecutionScopeDigest, value.Context.ExecutionScopeDigest)
		candidate.ControlScopeDigest, err = app.ConversationRouteControlScopeDigest(candidate.RequestInput)
		if err != nil {
			requestRows.Close()
			return app.ConversationRouteSnapshot{}, err
		}
		candidate.SameControlScope = bytes.Equal(candidate.ControlScopeDigest, value.Context.ControlScopeDigest)
		value.Context.RequestCandidates = append(value.Context.RequestCandidates, candidate)
	}
	err = requestRows.Err()
	requestRows.Close()
	if err != nil {
		return app.ConversationRouteSnapshot{}, fmt.Errorf("read prior conversation route candidates: %w", err)
	}
	if claim.AggregationState == "waiting" && claim.Message.TaskID == "" && len(claim.Input.ClientAttachmentIDs) == 0 {
		linkedRows, err := tx.Query(ctx, `SELECT m.id,m.sequence,
 CASE WHEN octet_length(m.content)<=4096 THEN m.content ELSE left(m.content,1023)||E'\n…\n'||right(m.content,1023) END,
 CASE WHEN octet_length(m.content)>4096 THEN sha256(convert_to(m.content,'UTF8')) END,octet_length(m.content)>4096,
 r.request_metadata,r.authorization_snapshot
FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.actor_id=r.actor_id
 AND m.conversation_id=r.conversation_id
WHERE r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
 AND r.duplicate_of_message_id=$5 AND r.state='routed' AND m.role='user' AND m.task_id IS NULL
 AND r.aggregation_started_at<=$6 AND COALESCE(r.request_metadata->'client_attachment_ids','[]'::jsonb)='[]'::jsonb
ORDER BY m.sequence,m.id LIMIT $7`, claim.ServiceInstanceID, claim.Message.OwnerID, claim.Message.ActorID,
			claim.Message.ConversationID, claim.Message.ID, claim.AggregationDeadline, app.MaxConversationRouteAggregateMessages+1)
		if err != nil {
			return app.ConversationRouteSnapshot{}, fmt.Errorf("read linked conversation aggregation candidates: %w", err)
		}
		for linkedRows.Next() {
			var candidate app.ConversationRouteAggregationCandidate
			var metadata, authorization []byte
			if err := linkedRows.Scan(&candidate.MessageID, &candidate.Sequence, &candidate.Content, &candidate.ContentSHA256,
				&candidate.Truncated, &metadata, &authorization); err != nil {
				linkedRows.Close()
				return app.ConversationRouteSnapshot{}, fmt.Errorf("scan linked conversation aggregation candidate: %w", err)
			}
			var input app.ConversationRoutingInput
			var auth task.Authorization
			if json.Unmarshal(metadata, &input) != nil || json.Unmarshal(authorization, &auth) != nil ||
				!reflect.DeepEqual(auth, claim.Authorization) || len(input.ClientAttachmentIDs) != 0 {
				continue
			}
			digest, err := app.ConversationRouteExecutionScopeDigest(input)
			if err != nil {
				linkedRows.Close()
				return app.ConversationRouteSnapshot{}, err
			}
			if !bytes.Equal(digest, value.Context.ExecutionScopeDigest) {
				continue
			}
			if len(value.Context.AggregationCandidates) == app.MaxConversationRouteAggregateMessages {
				continue
			}
			value.Context.AggregationCandidates = append(value.Context.AggregationCandidates, candidate)
		}
		err = linkedRows.Err()
		linkedRows.Close()
		if err != nil {
			return app.ConversationRouteSnapshot{}, fmt.Errorf("read linked conversation aggregation candidates: %w", err)
		}
	}
	if value.Context.ResultReview == nil {
		stored, err := loadConversationRouteSummaryInTx(ctx, tx, claim, value.PreviousMessageID, descendants)
		if err != nil {
			return app.ConversationRouteSnapshot{}, err
		}
		if stored != nil {
			value.Summary = stored
			value.Context.SummaryVersion = stored.Version
			value.Context.SummarySHA256 = append([]byte(nil), stored.SummarySHA256...)
			value.Context.SummarySourcesSHA256 = append([]byte(nil), stored.SourcesSHA256...)
		}
	}
	return value, nil
}

func loadConversationRouteSummaryInTx(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim,
	previousMessageID string, descendants []string) (*app.StoredConversationSummary, error) {
	identity, err := lockConversationSummaryScope(ctx, tx, claim)
	if err != nil {
		return nil, err
	}
	head, err := ensureConversationSummaryHead(ctx, tx, identity, false)
	if err != nil {
		return nil, err
	}
	if head.Version < 1 || head.Identity.AuthorizationVersion != identity.AuthorizationVersion || head.Identity.ScopeVersion != identity.ScopeVersion {
		return nil, nil
	}
	// shortcut: only the 32 newest valid versions are searched; raise the bound if causal gaps can span more compactions.
	rows, err := tx.Query(ctx, `SELECT version,summary_digest FROM conversation_summary_versions
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
 AND scope_kind=$5 AND scope_id=$6 AND authorization_version=$7 AND scope_version=$8 AND state='valid'
 AND version<=$9 ORDER BY version DESC LIMIT 32`, identity.ServiceInstanceID, identity.OwnerID, identity.ActorID,
		identity.ConversationID, identity.ScopeKind, identity.ScopeID, identity.AuthorizationVersion, identity.ScopeVersion, head.Version)
	if err != nil {
		return nil, fmt.Errorf("read candidate conversation route summaries: %w", err)
	}
	type summaryCandidate struct {
		version int64
		digest  []byte
	}
	var candidates []summaryCandidate
	for rows.Next() {
		var candidate summaryCandidate
		if err := rows.Scan(&candidate.version, &candidate.digest); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan candidate conversation route summary: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate candidate conversation route summaries: %w", err)
	}
	rows.Close()
	for _, value := range candidates {
		candidate := head
		candidate.Version, candidate.Digest = value.version, value.digest
		stored, err := readConversationSummaryForInputPage(ctx, tx, candidate)
		if err != nil {
			if domain.CodeOf(err) != "" {
				continue
			}
			return nil, err
		}
		if err := validateConversationSummarySources(ctx, tx, claim, identity, previousMessageID, descendants, stored.Sources); err != nil {
			if domain.CodeOf(err) != "" {
				continue
			}
			return nil, err
		}
		return &stored, nil
	}
	return nil, nil
}

func (s *Store) ApplyConversationRoute(ctx context.Context, claim app.ConversationRouteClaim,
	snapshot app.ConversationRouteSnapshot, decision app.ConversationRouteDecision) error {
	if !app.ValidConversationRouteFileDeliveryIntent(decision.Action, decision.FileDeliveryIntent) {
		return domain.InvalidArgument("route_decision", "file delivery intent is invalid for this route action")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if decision.Action == app.ConversationRouteNew {
			if err := lockConversationRouteNewScope(ctx, tx, claim); err != nil {
				return err
			}
		}
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		var cancelRequested bool
		if err := tx.QueryRow(ctx, `SELECT cancel_requested_at IS NOT NULL FROM conversation_message_routes
WHERE service_instance_id=$1::uuid AND message_id=$2 AND owner_id=$3 AND actor_id=$4 AND conversation_id=$5 FOR UPDATE`,
			claim.ServiceInstanceID, claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID).Scan(&cancelRequested); err != nil {
			return fmt.Errorf("read long-form conversation cancellation: %w", err)
		}
		if cancelRequested {
			if claim.Input.ResponseMode != app.ConversationResponseLongForm {
				return domain.NewError(domain.CodeInvalidState, "a short conversation route cannot be cancelled as a long reply")
			}
			return finishConversationRoute(ctx, tx, claim, "cancelled", "", "", true)
		}
		if decision.Cancelled {
			return domain.NewError(domain.CodeConflict, "long-form cancellation has not been recorded")
		}
		current, err := loadConversationRouteSnapshotInTx(ctx, tx, claim, true)
		if err != nil {
			return err
		}
		if current.ResultReviewSuperseded {
			return finishConversationRoute(ctx, tx, claim, "routed", "", "", true)
		}
		if current.AggregationUnresolved {
			if current.AggregationNeedsClarification && decision.Action == app.ConversationRouteClarify &&
				strings.TrimSpace(decision.Question) != "" && utf8.ValidString(decision.Question) &&
				!strings.ContainsRune(decision.Question, '\x00') && utf8.RuneCountInString(decision.Question) <= 200 {
				return finishConversationRoute(ctx, tx, claim, "waiting_user", strings.TrimSpace(decision.Question), "", false)
			}
			return requeueConversationRouteForReclassification(ctx, tx, claim)
		}
		if !app.ValidConversationRouteModelOutput(decision.ModelOutput) {
			return domain.InvalidArgument("route_decision", "conversation route model output is invalid")
		}
		if !app.ValidConversationRouteAgentToolProfile(decision, current.Context) {
			return domain.InvalidArgument("route_decision", "conversation route Agent tool profile is invalid")
		}
		if decision.Action != app.ConversationRouteNew && len(decision.AggregateMessageIDs) != 0 {
			return domain.InvalidArgument("route_decision", "aggregation candidates are only valid for a new Task")
		}
		if claim.Input.ResponseMode == app.ConversationResponseLongForm {
			if decision.ModelOutput != "" || decision.InvalidOutput || decision.TaskID != "" || decision.TargetMessageID != "" ||
				decision.Acknowledge || decision.ExplicitNew || decision.ExpectedRevision != 0 || len(decision.DependencyTaskIDs) != 0 ||
				len(decision.AggregateMessageIDs) != 0 || decision.ToolName != "" || decision.ToolArguments != "" || decision.AgentToolProfile != "" {
				return domain.InvalidArgument("route_decision", "long-form conversation decision contains unsupported fields")
			}
			switch decision.Action {
			case app.ConversationRouteReply:
				if !decision.LongReply || decision.Question != "" || !app.ValidConversationLongReply(decision.Reply) {
					return domain.InvalidArgument("route_decision", "long-form conversation reply is invalid")
				}
				decision.Reply = strings.TrimSpace(decision.Reply)
			case app.ConversationRouteClarify:
				if !decision.LongReply || decision.Partial || decision.Reply != "" || strings.TrimSpace(decision.Question) == "" ||
					!utf8.ValidString(decision.Question) || strings.ContainsRune(decision.Question, '\x00') || utf8.RuneCountInString(decision.Question) > 200 {
					return domain.InvalidArgument("route_decision", "long-form clarification is invalid")
				}
				decision.Question = strings.TrimSpace(decision.Question)
			default:
				return domain.InvalidArgument("route_decision", "long-form replies cannot route Tasks or tools")
			}
		} else {
			if decision.LongReply || decision.Partial {
				return domain.InvalidArgument("route_decision", "long-form reply fields are unavailable for this route")
			}
			if decision.Action == app.ConversationRouteReply {
				if decision.TaskID != "" || decision.ExpectedRevision != 0 || decision.Question != "" || !app.ValidConversationRouteReply(decision.Reply) {
					return domain.InvalidArgument("route_decision", "direct conversation reply is invalid")
				}
				decision.Reply = strings.TrimSpace(decision.Reply)
			} else if decision.Reply != "" {
				return domain.InvalidArgument("route_decision", "reply text is only valid for a direct reply")
			}
		}
		if decision.Action == app.ConversationRouteTool {
			if decision.Acknowledge || decision.TaskID != "" || decision.TargetMessageID != "" || decision.ExplicitNew ||
				decision.ExpectedRevision != 0 || decision.Question != "" || len(decision.DependencyTaskIDs) != 0 ||
				len(decision.AggregateMessageIDs) != 0 || !app.ValidConversationRouteDirectToolCall(decision.ToolName, []byte(decision.ToolArguments)) {
				return domain.InvalidArgument("route_decision", "direct route tool call is invalid")
			}
		} else if decision.ToolName != "" || decision.ToolArguments != "" {
			return domain.InvalidArgument("route_decision", "direct tool fields are only valid for a tool action")
		}
		if (decision.Action == app.ConversationRouteStop || decision.Action == app.ConversationRouteStopAll) &&
			!app.ValidConversationRouteStopDecision(current.Context, decision) {
			decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
				Question: "我无法安全确认要停止的任务，请重新说明停止范围。"}
		}
		modelID := snapshot.Context.ModelID
		snapshot.Context.ModelID = ""
		if !conversationRouteSnapshotsMatch(snapshot, current) {
			if current.Context.ResultReview != nil {
				return domain.NewError(domain.CodeConflict, "task result review context changed")
			}
			if decision.Action == app.ConversationRouteNew && !decision.ExplicitNew {
				if duplicate := exactConversationRouteDuplicate(current.Context.RequestCandidates, claim); duplicate != "" {
					return linkDuplicateConversationRouteInTx(ctx, tx, claim, current, duplicate)
				}
			}
			return requeueConversationRouteForReclassification(ctx, tx, claim)
		}
		if current.PreviousCycle && decision.Action != app.ConversationRouteClarify {
			return domain.NewError(domain.CodeConflict, "conversation predecessor chain is invalid")
		}
		if current.PreviousMissing && (decision.Action == app.ConversationRouteNew || decision.Action == app.ConversationRouteReply) {
			var independent bool
			err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND phase='missing_predecessor' AND state='settled' AND model_id=$3
 AND attempt=$4 AND decision->>'Action'=$5 AND COALESCE(decision->>'TaskID','')=''
 AND COALESCE(decision->>'Reply','')=$6)`, claim.Message.ID, claim.Message.ActorID, modelID,
				claim.RouteAttempt, string(decision.Action), decision.Reply).Scan(&independent)
			if err != nil || !independent || strings.TrimSpace(claim.Message.Content) == "" {
				return domain.NewError(domain.CodeConflict, "independent conversation request was not verified")
			}
		} else if current.PreviousMissing && decision.Action != app.ConversationRouteClarify {
			return domain.NewError(domain.CodeConflict, "previous conversation message is unavailable")
		}
		if current.Context.ResultReview != nil && decision.Action != app.ConversationRouteReply {
			return domain.NewError(domain.CodeInvalidState, "task result review must produce a reply")
		}
		if decision.Action == app.ConversationRouteTool && (!app.ConversationRouteDirectToolContextEligible(current.Context) ||
			len(claim.Input.ClientAttachmentIDs) != 0 || claim.AggregationState == "waiting") {
			return domain.NewError(domain.CodeConflict, "direct route tool context is no longer eligible")
		}
		if decision.Acknowledge && decision.Action != app.ConversationRouteNew &&
			decision.Action != app.ConversationRouteUpdate && decision.Action != app.ConversationRouteJoin {
			return domain.InvalidArgument("route_decision", "only background task actions can acknowledge a message")
		}
		if decision.Action == app.ConversationRouteSteer && (decision.TaskID == "" || decision.TargetMessageID != "" ||
			decision.ExpectedRevision < 0 || decision.Question != "" || decision.Reply != "" || decision.ExplicitNew ||
			len(decision.DependencyTaskIDs) != 0 || len(decision.AggregateMessageIDs) != 0) {
			return domain.InvalidArgument("route_decision", "task supplement decision is invalid")
		}
		if err := recordConversationMessageAcknowledgementDecisionInTx(ctx, tx, claim, decision.Acknowledge); err != nil {
			return err
		}
		var candidate *app.ConversationRouteCandidate
		for index := range current.Context.Candidates {
			item := &current.Context.Candidates[index]
			if item.TaskID == decision.TaskID && item.InitiatorID == claim.Message.ActorID && item.Revision == decision.ExpectedRevision {
				candidate = item
				break
			}
		}
		pendingRootStop := decision.Action == app.ConversationRouteStop && decision.TargetMessageID != "" && decision.TaskID == "" &&
			app.ValidConversationRoutePendingRootStop(decision.TargetMessageID, current.Context.RequestCandidates)
		if decision.Action == app.ConversationRouteSteer && (current.PreviousCycle ||
			!app.ValidConversationRouteSteerTarget(decision.TaskID, current.Context)) {
			decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
				Question: "我无法安全地把这条补充加入该任务。请指定正在运行的任务，或重新发送修改后的完整要求。"}
		}
		if decision.Action == app.ConversationRouteUpdate || decision.Action == app.ConversationRouteSteer ||
			decision.Action == app.ConversationRouteStatus || decision.Action == app.ConversationRouteStop {
			if !pendingRootStop && (candidate == nil || (claim.Input.ReferenceTaskID != "" && claim.Input.ReferenceTaskID != decision.TaskID)) {
				question := "无法确认目标任务，请说明要处理哪个任务。"
				if current.Context.ReferenceUnavailable {
					question = app.UnavailableRouteReferenceMessage
				}
				decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: question}
			}
		}
		switch decision.Action {
		case app.ConversationRouteJoin:
			if err := linkDuplicateConversationRouteInTx(ctx, tx, claim, current, decision.TargetMessageID); err != nil {
				return err
			}
			return acknowledgeConversationMessageForRunningTaskInTx(ctx, tx, claim)
		case app.ConversationRouteNew:
			if !app.ValidConversationRouteAggregateIDs(decision.AggregateMessageIDs, current.Context.AggregationCandidates) {
				return domain.NewError(domain.CodeConflict, "conversation aggregation candidates changed")
			}
			if !decision.ExplicitNew {
				if duplicate := exactConversationRouteDuplicate(current.Context.RequestCandidates, claim); duplicate != "" {
					if err := linkDuplicateConversationRouteInTx(ctx, tx, claim, current, duplicate); err != nil {
						return err
					}
					return acknowledgeConversationMessageForRunningTaskInTx(ctx, tx, claim)
				}
			}
			routedContent := current.Context.Content
			if claim.AggregationState == "waiting" {
				routedContent, err = prepareConversationRouteAggregation(ctx, tx, claim, current, decision.AggregateMessageIDs)
				if err != nil {
					return err
				}
			}
			return createConversationRouteTask(ctx, tx, claim, routedContent, modelID, decision.DependencyTaskIDs,
				decision.FileDeliveryIntent, current.Context.Candidates, current.Summary, decision.AgentToolProfile, nil)
		case app.ConversationRouteTool:
			return createConversationRouteTask(ctx, tx, claim, current.Context.Content, modelID, nil,
				task.FileDeliveryNone, current.Context.Candidates, current.Summary, "", &decision)
		case app.ConversationRouteReply:
			if current.Context.ResultReview != nil {
				return publishConversationResultReviewInTx(ctx, tx, claim, decision.Reply)
			}
			result, err := tx.Exec(ctx, `UPDATE conversation_messages SET delivery_status='applied'
WHERE id=$1 AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND role='user' AND task_id IS NULL AND delivery_status='pending'`,
				claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID)
			if err != nil {
				return fmt.Errorf("mark directly answered conversation message as applied: %w", err)
			}
			if result.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "conversation message is no longer awaiting a direct reply")
			}
			routeState := "routed"
			if decision.Partial {
				routeState = "partial"
			}
			return finishConversationRoute(ctx, tx, claim, routeState, decision.Reply, "", true)
		case app.ConversationRouteSteer:
			if candidate == nil || candidate.Status != task.StatusRunning {
				return domain.NewError(domain.CodeConflict, "conversation supplement Task is no longer running")
			}
			if err := requireConversationRouteScope(ctx, tx, claim, identity.ScopeTaskSubmit); err != nil {
				return err
			}
			result, err := tx.Exec(ctx, `UPDATE conversation_messages SET delivery_mode='steer'
WHERE id=$1 AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND role='user' AND task_id IS NULL
 AND delivery_status='pending' AND delivery_mode='followup'`, claim.Message.ID, claim.Message.OwnerID,
				claim.Message.ActorID, claim.Message.ConversationID)
			if err != nil {
				return fmt.Errorf("set conversation supplement delivery mode: %w", err)
			}
			if result.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "conversation supplement changed before routing")
			}
			return finishConversationRoute(ctx, tx, claim, "routed", "", candidate.TaskID, false)
		case app.ConversationRouteUpdate:
			if err := requireConversationRouteScope(ctx, tx, claim, identity.ScopeTaskControl); err != nil {
				return err
			}
			pendingClarification, err := conversationRouteHasPendingFileDeliveryClarification(ctx, tx, candidate.TaskID)
			if err != nil {
				return err
			}
			if pendingClarification {
				return finishConversationRoute(ctx, tx, claim, "waiting_user",
					"上一条修改仍在等待文件交付确认，请先处理该确认。 / The previous update is still waiting for a file delivery answer; resolve it first.", "", false)
			}
			var waitingReason string
			var ownStop bool
			if err := tx.QueryRow(ctx, `SELECT waiting_reason,EXISTS(SELECT 1 FROM task_revision_intents i WHERE i.task_id=tasks.id AND i.state='stopping')
FROM tasks WHERE id=$1`, candidate.TaskID).Scan(&waitingReason, &ownStop); err != nil {
				return fmt.Errorf("verify task adjustment state: %w", err)
			}
			if candidate.Status == task.StatusCancelling || candidate.Status == task.StatusPaused && waitingReason != "" ||
				candidate.Status == task.StatusPausing && !ownStop {
				return finishConversationRoute(ctx, tx, claim, "waiting_user", "该任务正在停止或等待其他问题处理，请处理后重新发送修改。", "", false)
			}
			if current.Context.PreviousUnresolved && (current.PreviousRoutingState != "waiting_adjustment" || current.PreviousTaskID != candidate.TaskID) {
				return finishConversationRoute(ctx, tx, claim, "waiting_user", "我无法确认这次修改对应的任务。请指定目标任务。", "", false)
			}
			var lastRevisionSequence, lastExpectedRevision int64
			var lastRevisionMessage string
			if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT message_sequence FROM task_revision_intents WHERE task_id=$1 ORDER BY expected_revision DESC LIMIT 1),0),
COALESCE((SELECT message_id FROM task_revision_intents WHERE task_id=$1 ORDER BY expected_revision DESC LIMIT 1),''),
COALESCE((SELECT expected_revision FROM task_revision_intents WHERE task_id=$1 ORDER BY expected_revision DESC LIMIT 1),-1)`, candidate.TaskID).
				Scan(&lastRevisionSequence, &lastRevisionMessage, &lastExpectedRevision); err != nil {
				return fmt.Errorf("verify task revision order: %w", err)
			}
			if lastRevisionSequence >= claim.Message.Sequence && (current.PreviousSequence <= claim.Message.Sequence ||
				current.PreviousMessageID != lastRevisionMessage || lastExpectedRevision+1 != candidate.Revision) {
				return finishConversationRoute(ctx, tx, claim, "waiting_user", "另一条较新的修改已收到，请确认这条修改是否仍要执行。", "", false)
			}
			if err := validateInitialRouteAttachments(ctx, tx, claim); err != nil {
				return err
			}
			if decision.FileDeliveryIntent == task.FileDeliveryUnclear {
				question, err := beginConversationRouteFileDeliveryClarification(ctx, tx, claim, *candidate)
				if err != nil {
					return err
				}
				return finishConversationRoute(ctx, tx, claim, "waiting_user", question, candidate.TaskID, false)
			}
			if err := validateConversationRouteTaskFileDeliverySnapshot(ctx, tx, claim, candidate.TaskID, decision.FileDeliveryIntent); err != nil {
				return err
			}
			if err := beginConversationTaskRevision(ctx, tx, claim, *candidate); err != nil {
				return err
			}
			return finishConversationRoute(ctx, tx, claim, "waiting_adjustment", "已收到修改，等待调整。", "", false)
		case app.ConversationRouteStatus:
			if err := requireConversationRouteScope(ctx, tx, claim, identity.ScopeTaskRead); err != nil {
				return err
			}
			return finishConversationRoute(ctx, tx, claim, "routed", conversationRouteStatusText(candidate.Status), "", true)
		case app.ConversationRouteStop:
			if err := requireConversationRouteScope(ctx, tx, claim, identity.ScopeTaskControl); err != nil {
				return err
			}
			if pendingRootStop {
				return cancelPendingConversationRouteRoot(ctx, tx, claim, current, decision.TargetMessageID)
			}
			if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations SET cancel_requested=true,updated_at=clock_timestamp()
WHERE task_id=$1 AND status='dispatched' AND cancel_requested=false`, candidate.TaskID); err != nil {
				return fmt.Errorf("fence task operations before stop: %w", err)
			}
			if err := requestTaskCancellationForAuthorization(ctx, tx, candidate.TaskID, claim.Message.OwnerID); err != nil {
				return err
			}
			return finishConversationRoute(ctx, tx, claim, "routed", "已请求停止该任务，停止完成后会更新状态。", candidate.TaskID, true)
		case app.ConversationRouteStopAll:
			response, err := stopAllConversationRouteTasksInTx(ctx, tx, claim)
			if err != nil {
				return err
			}
			return finishConversationRoute(ctx, tx, claim, "routed", response, "", true)
		case app.ConversationRouteClarify:
			if strings.TrimSpace(decision.Question) == "" || !utf8.ValidString(decision.Question) ||
				strings.ContainsRune(decision.Question, '\x00') || len([]rune(decision.Question)) > 200 {
				return domain.InvalidArgument("route_decision", "clarification question is invalid")
			}
			if err := finishConversationRoute(ctx, tx, claim, "waiting_user", strings.TrimSpace(decision.Question), "", false); err != nil {
				return err
			}
			if current.PreviousMissing && !current.PreviousCycle {
				_, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET awaiting_predecessor=true WHERE message_id=$1`, claim.Message.ID)
				return err
			}
			return nil
		default:
			return domain.InvalidArgument("route_decision", "conversation route action is invalid")
		}
	})
}

func stopAllConversationRouteTasksInTx(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) (string, error) {
	if err := requireConversationRouteScope(ctx, tx, claim, identity.ScopeTaskControl); err != nil {
		return "", err
	}
	rows, err := tx.Query(ctx, `SELECT id,status FROM tasks
WHERE owner_id=$1 AND initiator_id=$2 AND status IN ('queued','running','waiting_user','pausing','paused','cancelling')
 AND NOT EXISTS (SELECT 1 FROM conversation_route_internal_tool_calls internal_tool WHERE internal_tool.task_id=tasks.id)
ORDER BY created_at,id LIMIT 257 FOR UPDATE`, claim.Message.OwnerID, claim.Message.ActorID)
	if err != nil {
		return "", fmt.Errorf("lock tasks for explicit stop-all request: %w", err)
	}
	type activeTask struct {
		id     string
		status task.Status
	}
	active := make([]activeTask, 0, 16)
	for rows.Next() {
		var item activeTask
		var status string
		if err := rows.Scan(&item.id, &status); err != nil {
			rows.Close()
			return "", fmt.Errorf("read task for explicit stop-all request: %w", err)
		}
		item.status = task.Status(status)
		active = append(active, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", fmt.Errorf("iterate tasks for explicit stop-all request: %w", err)
	}
	rows.Close()
	if len(active) > 256 {
		return "当前活动任务较多，我无法一次安全停止全部；没有任务被停止。请说明要停止的任务范围。", nil
	}
	if len(active) == 0 {
		return "当前没有由你发起且仍在执行的任务。", nil
	}
	requested, alreadyStopping := 0, 0
	for _, item := range active {
		if item.status == task.StatusCancelling {
			alreadyStopping++
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations SET cancel_requested=true,updated_at=clock_timestamp()
WHERE task_id=$1 AND status='dispatched' AND cancel_requested=false`, item.id); err != nil {
			return "", fmt.Errorf("fence task operation before stop-all cancellation: %w", err)
		}
		if err := requestTaskCancellationForAuthorization(ctx, tx, item.id, claim.Message.OwnerID); err != nil {
			return "", err
		}
		requested++
	}
	return fmt.Sprintf("已向你发起的 %d 个活动任务提交停止请求；%d 个任务已在停止处理中。我会根据服务端确认结果更新状态。", requested, alreadyStopping), nil
}

func recordConversationMessageAcknowledgementDecisionInTx(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim, acknowledge bool) error {
	result, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET
acknowledgement_requested=$8 AND NOT agent_acknowledged,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND message_id=$2 AND owner_id=$3 AND actor_id=$4 AND conversation_id=$5
 AND state='processing' AND lease_owner=$6 AND lease_version=$7 AND lease_expires_at>clock_timestamp()`, claim.ServiceInstanceID, claim.Message.ID,
		claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID, claim.LeaseOwner, claim.LeaseVersion, acknowledge)
	if err != nil {
		return fmt.Errorf("record main Agent message acknowledgement decision: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "conversation acknowledgement decision is no longer current")
	}
	return nil
}

func acknowledgeConversationMessageForRunningTaskInTx(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) error {
	rows, err := tx.Query(ctx, `UPDATE conversation_message_routes r SET acknowledgement_requested=false,agent_acknowledged=true,updated_at=clock_timestamp()
FROM conversation_messages m
WHERE r.message_id=m.id AND r.message_id=$1 AND r.owner_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
 AND r.acknowledgement_requested AND NOT r.agent_acknowledged
 AND EXISTS (
  SELECT 1 FROM tasks t WHERE t.id=COALESCE(m.task_id,
    (SELECT root.task_id FROM conversation_messages root WHERE root.id=r.duplicate_of_message_id AND root.owner_id=m.owner_id),
    (SELECT intent.task_id FROM task_revision_intents intent WHERE intent.message_id=m.id ORDER BY intent.expected_revision DESC LIMIT 1))
   AND t.owner_id=m.owner_id AND t.conversation_id=m.conversation_id AND t.status='running'
   AND t.lease_expires_at>clock_timestamp() AND t.current_revision=t.desired_revision
   AND EXISTS (SELECT 1 FROM task_revision_starts started WHERE started.task_id=t.id AND started.revision=t.current_revision)
 )
RETURNING m.id,m.owner_id,r.client_message_id`, claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID)
	if err != nil {
		return fmt.Errorf("confirm conversation acknowledgement for running Task: %w", err)
	}
	type acknowledgedMessage struct{ id, ownerID, clientMessageID string }
	var messages []acknowledgedMessage
	for rows.Next() {
		var message acknowledgedMessage
		if err := rows.Scan(&message.id, &message.ownerID, &message.clientMessageID); err != nil {
			rows.Close()
			return fmt.Errorf("read confirmed conversation acknowledgement: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate confirmed conversation acknowledgements: %w", err)
	}
	rows.Close()
	for _, message := range messages {
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: message.ownerID,
			ObjectType: "message", ObjectID: message.id, Operation: "upsert", MutationID: message.clientMessageID}); err != nil {
			return err
		}
	}
	return nil
}

const maxAutomaticRouteConflicts = 16 // Matches the maximum route-worker concurrency so one independent batch can settle.

func requeueConversationRouteForReclassification(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) error {
	result, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET
state=CASE WHEN auto_retry_count+1 >= $9 THEN 'failed' ELSE 'pending' END,
failure_reason=CASE WHEN auto_retry_count+1 >= $9 THEN 'routing_context_unavailable' ELSE '' END,
route_attempt=CASE WHEN auto_retry_count+1 >= $9 THEN route_attempt ELSE route_attempt+1 END,
auto_retry_count=LEAST(auto_retry_count+1,$9),lease_owner='',lease_expires_at=NULL,
acknowledgement_requested=false,available_after=clock_timestamp(),updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND message_id=$2 AND owner_id=$3 AND actor_id=$4 AND conversation_id=$5
 AND state='processing' AND lease_owner=$6 AND lease_version=$7 AND route_attempt=$8 AND lease_expires_at>clock_timestamp()`,
		claim.ServiceInstanceID, claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID,
		claim.LeaseOwner, claim.LeaseVersion, claim.RouteAttempt, maxAutomaticRouteConflicts)
	if err != nil {
		return fmt.Errorf("requeue conversation route after context change: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "conversation route lease changed before reclassification")
	}
	if err := syncConversationDuplicateMessages(ctx, tx, claim.Message.ID); err != nil {
		return err
	}
	return recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: claim.Message.OwnerID,
		ObjectType: "message", ObjectID: claim.Message.ID, Operation: "upsert", MutationID: claim.Message.ClientMessageID})
}

func syncConversationDuplicateMessages(ctx context.Context, tx pgx.Tx, rootMessageID string) error {
	rows, err := tx.Query(ctx, `UPDATE conversation_messages duplicate SET delivery_status=original.delivery_status
FROM conversation_message_routes duplicate_route
JOIN conversation_message_routes original_route ON original_route.service_instance_id=duplicate_route.service_instance_id
 AND original_route.owner_id=duplicate_route.owner_id AND original_route.actor_id=duplicate_route.actor_id
 AND original_route.conversation_id=duplicate_route.conversation_id
JOIN conversation_messages original ON original.id=original_route.message_id AND original.owner_id=original_route.owner_id
 AND original.actor_id=original_route.actor_id AND original.conversation_id=original_route.conversation_id
WHERE duplicate_route.message_id=duplicate.id AND duplicate_route.duplicate_of_message_id=original_route.message_id
 AND original_route.message_id=$1
RETURNING duplicate.id,duplicate.owner_id,original_route.client_message_id`, rootMessageID)
	if err != nil {
		return fmt.Errorf("synchronize duplicate conversation message delivery: %w", err)
	}
	type changedMessage struct{ id, owner, mutation string }
	var messageIDs []changedMessage
	for rows.Next() {
		var message changedMessage
		if err := rows.Scan(&message.id, &message.owner, &message.mutation); err != nil {
			rows.Close()
			return fmt.Errorf("read synchronized duplicate conversation message: %w", err)
		}
		messageIDs = append(messageIDs, message)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read synchronized duplicate conversation messages: %w", err)
	}
	rows.Close()
	for _, message := range messageIDs {
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: message.owner,
			ObjectType: "message", ObjectID: message.id, Operation: "upsert", MutationID: message.mutation}); err != nil {
			return err
		}
	}
	return nil
}

func lockConversationRouteNewScope(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) error {
	scopeDigest, err := app.ConversationRouteExecutionScopeDigest(claim.Input)
	if err != nil {
		return err
	}
	key := strings.Join([]string{"conversation-route-new", claim.Message.OwnerID, claim.Message.ActorID,
		claim.Message.ConversationID, hex.EncodeToString(scopeDigest)}, ":")
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
		return fmt.Errorf("serialize same-scope conversation task creation: %w", err)
	}
	return nil
}

func exactConversationRouteDuplicate(candidates []app.ConversationRouteRequestCandidate, claim app.ConversationRouteClaim) string {
	var target string
	var targetSequence int64
	for _, candidate := range candidates {
		if (target == "" || candidate.Sequence < targetSequence) &&
			app.ValidConversationRouteExactDuplicateTarget(candidate.MessageID, claim.Message.Content, candidates) {
			target, targetSequence = candidate.MessageID, candidate.Sequence
		}
	}
	return target
}

const (
	joinedConversationRouteContentBudget           = 32768
	joinedConversationRouteContentNoteReserveBytes = 128
)

const joinedConversationRouteContentPrefix = "用户为同一项工作提交了以下消息，请按顺序作为一个请求处理：\n\n"

const joinedConversationRouteContentSeparator = "\n\n--- 用户补充 ---\n\n"

func joinedConversationRouteContent(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) (string, []app.ConversationRouteMessageSource, error) {
	if len(claim.Input.ClientAttachmentIDs) != 0 {
		return claim.Message.Content, nil, nil
	}
	var rootMetadata []byte
	err := tx.QueryRow(ctx, `SELECT r.request_metadata FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.actor_id=r.actor_id
 AND m.conversation_id=r.conversation_id
WHERE r.service_instance_id=$1::uuid AND r.message_id=$2 AND r.owner_id=$3 AND r.actor_id=$4
 AND r.conversation_id=$5 AND r.duplicate_of_message_id IS NULL AND m.task_id IS NULL`,
		claim.ServiceInstanceID, claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID).Scan(&rootMetadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return claim.Message.Content, nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("read conversation root before merging joined input: %w", err)
	}
	var rootInput app.ConversationRoutingInput
	if err := json.Unmarshal(rootMetadata, &rootInput); err != nil {
		return "", nil, fmt.Errorf("decode conversation root input before merging joined input: %w", err)
	}
	rootDigest, err := app.ConversationRouteExecutionScopeDigest(rootInput)
	if err != nil {
		return "", nil, err
	}
	currentDigest, err := app.ConversationRouteExecutionScopeDigest(claim.Input)
	if err != nil {
		return "", nil, err
	}
	if !bytes.Equal(rootDigest, currentDigest) {
		return "", nil, domain.NewError(domain.CodeConflict, "conversation root execution scope changed")
	}
	rows, err := tx.Query(ctx, `SELECT m.id,m.sequence,m.content,r.request_metadata,r.authorization_snapshot FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.actor_id=r.actor_id
 AND m.conversation_id=r.conversation_id
WHERE r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
 AND r.duplicate_of_message_id=$5 AND r.state='routed' AND m.role='user' AND m.task_id IS NULL
ORDER BY m.sequence,m.id`, claim.ServiceInstanceID, claim.Message.OwnerID, claim.Message.ActorID,
		claim.Message.ConversationID, claim.Message.ID)
	if err != nil {
		return "", nil, fmt.Errorf("read joined conversation inputs: %w", err)
	}
	defer rows.Close()
	contents := []string{claim.Message.Content}
	seen := map[string]struct{}{strings.TrimSpace(claim.Message.Content): {}}
	rootSHA := sha256.Sum256([]byte(claim.Message.Content))
	sources := []app.ConversationRouteMessageSource{{MessageID: claim.Message.ID, Sequence: claim.Message.Sequence,
		ContentBytes: int64(len(claim.Message.Content)), ContentSHA256: rootSHA[:]}}
	// Reserve room for the merged prefix and the omitted-supplements note so
	// the merged input stays within budget even after they are added.
	budget := joinedConversationRouteContentBudget - len(joinedConversationRouteContentPrefix) -
		joinedConversationRouteContentNoteReserveBytes
	totalBytes := len(claim.Message.Content)
	var omitted []string
	for rows.Next() {
		var messageID, content string
		var sequence int64
		var metadata, authorization []byte
		if err := rows.Scan(&messageID, &sequence, &content, &metadata, &authorization); err != nil {
			return "", nil, fmt.Errorf("scan joined conversation input: %w", err)
		}
		var input app.ConversationRoutingInput
		var auth task.Authorization
		if err := json.Unmarshal(metadata, &input); err != nil {
			return "", nil, fmt.Errorf("decode joined conversation input: %w", err)
		}
		if err := json.Unmarshal(authorization, &auth); err != nil || !reflect.DeepEqual(auth, claim.Authorization) ||
			len(input.ClientAttachmentIDs) != 0 || sequence < 1 {
			return "", nil, domain.NewError(domain.CodeConflict, "joined conversation authorization changed")
		}
		digest, err := app.ConversationRouteExecutionScopeDigest(input)
		if err != nil {
			return "", nil, err
		}
		if !bytes.Equal(digest, currentDigest) {
			return "", nil, domain.NewError(domain.CodeConflict, "joined conversation input changed execution scope")
		}
		key := strings.TrimSpace(content)
		if _, ok := seen[key]; ok {
			continue
		}
		// Keep the message limit and source manifest limit aligned. Anything
		// that cannot be represented safely is detached and routed separately.
		if len(sources) >= app.MaxConversationRouteAggregateMessages+1 ||
			totalBytes+len(content)+len(joinedConversationRouteContentSeparator) > budget {
			omitted = append(omitted, messageID)
			continue
		}
		seen[key] = struct{}{}
		contents = append(contents, content)
		totalBytes += len(content) + len(joinedConversationRouteContentSeparator)
		contentSHA := sha256.Sum256([]byte(content))
		sources = append(sources, app.ConversationRouteMessageSource{MessageID: messageID, Sequence: sequence,
			ContentBytes: int64(len(content)), ContentSHA256: contentSHA[:]})
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("read joined conversation inputs: %w", err)
	}
	if len(omitted) > 0 {
		if err := detachJoinedConversationRouteInputs(ctx, tx, claim, omitted); err != nil {
			return "", nil, err
		}
	}
	if len(contents) == 1 {
		return claim.Message.Content, nil, nil
	}
	var merged strings.Builder
	merged.WriteString(joinedConversationRouteContentPrefix)
	for index, content := range contents {
		if index > 0 {
			merged.WriteString(joinedConversationRouteContentSeparator)
		}
		merged.WriteString(content)
	}
	if len(omitted) > 0 {
		note := fmt.Sprintf("\n\n【注：另有 %d 条补充因过长未合并，将单独处理。】", len(omitted))
		if len(note) > joinedConversationRouteContentNoteReserveBytes {
			return "", nil, domain.NewError(domain.CodeResultTooLarge, "joined conversation omission note exceeds its reserved budget")
		}
		merged.WriteString(note)
	}
	if merged.Len() > joinedConversationRouteContentBudget {
		return "", nil, domain.NewError(domain.CodeResultTooLarge, "joined conversation input exceeds the routing limit")
	}
	return merged.String(), sources, nil
}

// detachJoinedConversationRouteInputs releases joined supplements that did
// not fit into the merged routing input so the route worker picks them up as
// independent messages instead of losing them.
func detachJoinedConversationRouteInputs(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim, messageIDs []string) error {
	rows, err := tx.Query(ctx, `UPDATE conversation_message_routes SET duplicate_of_message_id=NULL,state='pending',
aggregation_state='settled',available_after=clock_timestamp(),lease_owner='',lease_expires_at=NULL,
lease_version=lease_version+1,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
 AND duplicate_of_message_id=$5 AND state='routed' AND message_id=ANY($6::text[])
RETURNING message_id,client_message_id`,
		claim.ServiceInstanceID, claim.Message.OwnerID, claim.Message.ActorID,
		claim.Message.ConversationID, claim.Message.ID, messageIDs)
	if err != nil {
		return fmt.Errorf("detach oversized joined conversation inputs: %w", err)
	}
	type detachedMessage struct{ id, clientMessageID string }
	detached := make([]detachedMessage, 0, len(messageIDs))
	for rows.Next() {
		var item detachedMessage
		if err := rows.Scan(&item.id, &item.clientMessageID); err != nil {
			rows.Close()
			return fmt.Errorf("read detached joined conversation input: %w", err)
		}
		detached = append(detached, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read detached joined conversation inputs: %w", err)
	}
	rows.Close()
	if len(detached) != len(messageIDs) {
		return domain.NewError(domain.CodeConflict, "joined conversation input changed while merging")
	}
	result, err := tx.Exec(ctx, `UPDATE conversation_messages SET delivery_status='pending'
WHERE id=ANY($1::text[]) AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND role='user'
 AND task_id IS NULL AND delivery_status IN ('pending','applied','unknown')`, messageIDs,
		claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID)
	if err != nil {
		return fmt.Errorf("mark detached joined conversation inputs pending: %w", err)
	}
	if result.RowsAffected() != int64(len(messageIDs)) {
		return domain.NewError(domain.CodeConflict, "detached conversation message changed while merging")
	}
	for _, item := range detached {
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: claim.Message.OwnerID,
			ObjectType: "message", ObjectID: item.id, Operation: "upsert", MutationID: item.clientMessageID}); err != nil {
			return err
		}
	}
	return nil
}

func prepareConversationRouteAggregation(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim,
	snapshot app.ConversationRouteSnapshot, selectedIDs []string) (string, error) {
	if !app.ValidConversationRouteAggregateIDs(selectedIDs, snapshot.Context.AggregationCandidates) {
		return "", domain.NewError(domain.CodeConflict, "conversation aggregation candidate is no longer available")
	}
	selected := make(map[string]app.ConversationRouteAggregationCandidate, len(selectedIDs))
	for _, id := range selectedIDs {
		for _, candidate := range snapshot.Context.AggregationCandidates {
			if candidate.MessageID == id {
				selected[id] = candidate
				break
			}
		}
	}
	if len(selected) != len(selectedIDs) {
		return "", domain.NewError(domain.CodeConflict, "conversation aggregation candidate set changed")
	}
	var contents []string
	rows, err := tx.Query(ctx, `SELECT m.id,m.sequence,m.content,r.request_metadata,r.authorization_snapshot,r.aggregation_started_at
FROM conversation_message_routes r JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id
 AND m.actor_id=r.actor_id AND m.conversation_id=r.conversation_id
WHERE r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
 AND r.duplicate_of_message_id=$5 AND r.state='routed' AND m.role='user' AND m.task_id IS NULL
 AND m.id=ANY($6::text[])
ORDER BY m.sequence,m.id FOR UPDATE OF r,m`, claim.ServiceInstanceID, claim.Message.OwnerID, claim.Message.ActorID,
		claim.Message.ConversationID, claim.Message.ID, selectedIDs)
	if err != nil {
		return "", fmt.Errorf("lock selected conversation aggregation candidates: %w", err)
	}
	for rows.Next() {
		var id, content string
		var sequence int64
		var metadata, authorization []byte
		var acceptedAt time.Time
		if err := rows.Scan(&id, &sequence, &content, &metadata, &authorization, &acceptedAt); err != nil {
			rows.Close()
			return "", fmt.Errorf("scan selected conversation aggregation candidate: %w", err)
		}
		candidate, ok := selected[id]
		var input app.ConversationRoutingInput
		var auth task.Authorization
		if !ok || sequence != candidate.Sequence || !conversationRouteAggregationCandidateMatchesContent(candidate, content) || acceptedAt.After(claim.AggregationDeadline) ||
			json.Unmarshal(metadata, &input) != nil || json.Unmarshal(authorization, &auth) != nil ||
			!reflect.DeepEqual(auth, claim.Authorization) || len(input.ClientAttachmentIDs) != 0 {
			rows.Close()
			return "", domain.NewError(domain.CodeConflict, "selected conversation aggregation candidate changed")
		}
		digest, err := app.ConversationRouteExecutionScopeDigest(input)
		if err != nil || !bytes.Equal(digest, snapshot.Context.ExecutionScopeDigest) {
			rows.Close()
			return "", domain.NewError(domain.CodeConflict, "selected conversation aggregation scope changed")
		}
		contents = append(contents, content)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", fmt.Errorf("read selected conversation aggregation candidates: %w", err)
	}
	if len(contents) != len(selectedIDs) {
		return "", domain.NewError(domain.CodeConflict, "selected conversation aggregation candidate disappeared")
	}
	content := claim.Message.Content
	if len(contents) > 0 {
		var merged strings.Builder
		merged.WriteString("用户为同一项工作提交了以下消息，请按顺序作为一个请求处理：\n\n")
		merged.WriteString(content)
		for _, supplement := range contents {
			merged.WriteString("\n\n--- 用户补充 ---\n\n")
			merged.WriteString(supplement)
		}
		if merged.Len() > 32768 {
			return "", domain.NewError(domain.CodeResultTooLarge, "conversation aggregation exceeds the routing limit")
		}
		content = merged.String()
	}
	rows, err = tx.Query(ctx, `WITH detached AS (
 UPDATE conversation_message_routes SET duplicate_of_message_id=NULL,state='pending',aggregation_state='settled',
  available_after=clock_timestamp(),lease_owner='',lease_expires_at=NULL,lease_version=lease_version+1,updated_at=clock_timestamp()
 WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
  AND duplicate_of_message_id=$5 AND state='routed' AND message_id<>ALL($6::text[])
 RETURNING message_id,owner_id,client_message_id
), reset_messages AS (
 UPDATE conversation_messages m SET delivery_status='pending' FROM detached d
 WHERE m.id=d.message_id AND m.owner_id=d.owner_id RETURNING m.id,d.owner_id,d.client_message_id
)
SELECT id,owner_id,client_message_id FROM reset_messages`, claim.ServiceInstanceID, claim.Message.OwnerID,
		claim.Message.ActorID, claim.Message.ConversationID, claim.Message.ID, selectedIDs)
	if err != nil {
		return "", fmt.Errorf("release unselected conversation aggregation candidates: %w", err)
	}
	type releasedMessage struct{ id, owner, clientID string }
	var released []releasedMessage
	for rows.Next() {
		var item releasedMessage
		if err := rows.Scan(&item.id, &item.owner, &item.clientID); err != nil {
			rows.Close()
			return "", fmt.Errorf("read released conversation aggregation candidate: %w", err)
		}
		released = append(released, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return "", fmt.Errorf("read released conversation aggregation candidates: %w", err)
	}
	for _, item := range released {
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: item.owner,
			ObjectType: "message", ObjectID: item.id, Operation: "upsert", MutationID: item.clientID}); err != nil {
			return "", err
		}
	}
	return content, nil
}

func cancelPendingConversationRouteRoot(ctx context.Context, tx pgx.Tx, stopClaim app.ConversationRouteClaim,
	snapshot app.ConversationRouteSnapshot, targetMessageID string) error {
	if !app.ValidConversationRoutePendingRootStop(targetMessageID, snapshot.Context.RequestCandidates) {
		return domain.NewError(domain.CodeConflict, "pending conversation route target is no longer authorized")
	}
	var root app.ConversationRouteClaim
	var role, delivery, sourceKind, routeState, aggregationState string
	var taskID string
	var metadata, authorization []byte
	err := tx.QueryRow(ctx, `SELECT m.id,m.conversation_id,m.owner_id,m.actor_id,m.role,m.content,COALESCE(m.task_id,''),
m.model_id,m.sequence,m.delivery_status,m.created_at,m.source_kind,r.client_message_id,r.request_metadata,
r.authorization_snapshot,r.route_attempt,r.aggregation_started_at,r.aggregation_deadline,r.aggregation_state,r.state
FROM conversation_message_routes r JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id
 AND m.actor_id=r.actor_id AND m.conversation_id=r.conversation_id
WHERE r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
 AND r.message_id=$5 AND r.duplicate_of_message_id IS NULL
FOR UPDATE OF r,m`, stopClaim.ServiceInstanceID, stopClaim.Message.OwnerID, stopClaim.Message.ActorID,
		stopClaim.Message.ConversationID, targetMessageID).Scan(&root.Message.ID, &root.Message.ConversationID,
		&root.Message.OwnerID, &root.Message.ActorID, &role, &root.Message.Content, &taskID, &root.Message.ModelID,
		&root.Message.Sequence, &delivery, &root.Message.CreatedAt, &sourceKind, &root.Message.ClientMessageID,
		&metadata, &authorization, &root.RouteAttempt, &root.AggregationStartedAt, &root.AggregationDeadline,
		&aggregationState, &routeState)
	if errors.Is(err, pgx.ErrNoRows) {
		return requeueConversationRouteForReclassification(ctx, tx, stopClaim)
	}
	if err != nil {
		return fmt.Errorf("lock pending conversation route target: %w", err)
	}
	if json.Unmarshal(metadata, &root.Input) != nil || json.Unmarshal(authorization, &root.Authorization) != nil {
		return requeueConversationRouteForReclassification(ctx, tx, stopClaim)
	}
	if taskID != "" || (routeState != "pending" && routeState != "processing") ||
		(aggregationState != "open" && aggregationState != "waiting") ||
		delivery != string(conversation.DeliveryPending) {
		return requeueConversationRouteForReclassification(ctx, tx, stopClaim)
	}
	if !reflect.DeepEqual(root.Authorization, stopClaim.Authorization) {
		return finishConversationRoute(ctx, tx, stopClaim, "waiting_user", "这条请求的授权范围已变化，请确认目标后重试。", "", false)
	}
	rootControlScopeDigest, err := app.ConversationRouteControlScopeDigest(root.Input)
	if err != nil || !bytes.Equal(rootControlScopeDigest, snapshot.Context.ControlScopeDigest) {
		return finishConversationRoute(ctx, tx, stopClaim, "waiting_user", "这条请求的执行范围已变化，请确认目标后重试。", "", false)
	}
	var candidate *app.ConversationRouteRequestCandidate
	for index := range snapshot.Context.RequestCandidates {
		item := &snapshot.Context.RequestCandidates[index]
		if item.MessageID == targetMessageID {
			candidate = item
			break
		}
	}
	if candidate == nil || candidate.Sequence != root.Message.Sequence || !conversationRouteRequestCandidateMatchesContent(*candidate, root.Message.Content) ||
		candidate.RoutingStatus != conversation.RoutingStatus(routeState) || !candidate.SameControlScope || !candidate.SameAuthorization {
		return requeueConversationRouteForReclassification(ctx, tx, stopClaim)
	}
	root.ServiceInstanceID = stopClaim.ServiceInstanceID
	root.Message.Role = conversation.Role(role)
	root.Message.SourceKind = sourceKind
	root.Message.DeliveryStatus = conversation.DeliveryStatus(delivery)
	root.Message.RoutingStatus = conversation.RoutingStatus(routeState)
	result, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET aggregation_state='cancelled',updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND message_id=$2 AND state=$3 AND aggregation_state IN ('open','waiting')
 AND route_attempt=$4`,
		root.ServiceInstanceID, root.Message.ID, routeState, root.RouteAttempt)
	if err != nil {
		return fmt.Errorf("mark pending conversation route cancelled: %w", err)
	}
	if result.RowsAffected() != 1 {
		return requeueConversationRouteForReclassification(ctx, tx, stopClaim)
	}
	if err := finishConversationRoute(ctx, tx, root, "routed", "这条请求已在任务创建前取消；未创建 Task，也未执行。", "", true); err != nil {
		return err
	}
	linkedRows, err := tx.Query(ctx, `UPDATE conversation_message_routes SET aggregation_state='cancelled',
lease_owner='',lease_expires_at=NULL,lease_version=lease_version+1,updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
 AND duplicate_of_message_id=$5 AND state='routed'
RETURNING message_id,owner_id,client_message_id`, root.ServiceInstanceID, root.Message.OwnerID,
		root.Message.ActorID, root.Message.ConversationID, root.Message.ID)
	if err != nil {
		return fmt.Errorf("mark linked conversation supplements cancelled: %w", err)
	}
	type linkedMessage struct{ id, owner, clientID string }
	var linked []linkedMessage
	for linkedRows.Next() {
		var item linkedMessage
		if err := linkedRows.Scan(&item.id, &item.owner, &item.clientID); err != nil {
			linkedRows.Close()
			return fmt.Errorf("read cancelled conversation supplement: %w", err)
		}
		linked = append(linked, item)
	}
	err = linkedRows.Err()
	linkedRows.Close()
	if err != nil {
		return fmt.Errorf("read cancelled conversation supplements: %w", err)
	}
	for _, item := range linked {
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: item.owner,
			ObjectType: "message", ObjectID: item.id, Operation: "upsert", MutationID: item.clientID}); err != nil {
			return err
		}
	}
	return finishConversationRoute(ctx, tx, stopClaim, "routed", "已取消这条仍在等待的请求；未创建 Task，也未执行。", "", true)
}

func markPendingTaskConversationMessagesUnknown(ctx context.Context, tx pgx.Tx, taskID, ownerID, conversationID string) error {
	rows, err := tx.Query(ctx, `UPDATE conversation_messages message SET delivery_status='unknown'
FROM tasks task
WHERE task.id=$1 AND task.owner_id=$2 AND message.task_id=task.id AND message.owner_id=task.owner_id
 AND message.conversation_id=task.conversation_id AND ($3='' OR message.conversation_id=$3)
 AND message.role='user' AND message.delivery_status='pending'
RETURNING message.id`, taskID, ownerID, conversationID)
	if err != nil {
		return fmt.Errorf("mark pending task messages unknown: %w", err)
	}
	var messageIDs []string
	for rows.Next() {
		var messageID string
		if err := rows.Scan(&messageID); err != nil {
			rows.Close()
			return fmt.Errorf("read pending task message: %w", err)
		}
		messageIDs = append(messageIDs, messageID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("read pending task messages: %w", err)
	}
	rows.Close()
	for _, messageID := range messageIDs {
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: ownerID,
			ObjectType: "message", ObjectID: messageID, Operation: "upsert", MutationID: messageID}); err != nil {
			return err
		}
		if err := syncConversationDuplicateMessages(ctx, tx, messageID); err != nil {
			return err
		}
	}
	return nil
}

func linkDuplicateConversationRouteInTx(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim,
	snapshot app.ConversationRouteSnapshot, targetMessageID string) error {
	var candidate *app.ConversationRouteRequestCandidate
	for index := range snapshot.Context.RequestCandidates {
		item := &snapshot.Context.RequestCandidates[index]
		if item.MessageID == targetMessageID && item.SameExecutionScope {
			candidate = item
			break
		}
	}
	validTarget := candidate != nil && app.ValidConversationRouteJoinTarget(targetMessageID, snapshot.Context.RequestCandidates)
	if candidate != nil && conversationRouteRequestCandidateMatchesContent(*candidate, claim.Message.Content) {
		validTarget = app.ValidConversationRouteExactDuplicateTarget(targetMessageID, claim.Message.Content, snapshot.Context.RequestCandidates)
	}
	if candidate == nil || !validTarget {
		return domain.NewError(domain.CodeConflict, "conversation join target is no longer authorized")
	}
	var rootSequence int64
	var rootContent, rootClientID, rootState, rootTaskID, rootDelivery, rootSourceKind, rootAggregationState string
	var rootMetadata, rootAuthorization []byte
	var rootAttempt int64
	var rootAggregationEligible bool
	err := tx.QueryRow(ctx, `SELECT m.sequence,m.content,r.client_message_id,r.state,COALESCE(m.task_id,''),m.delivery_status,
r.request_metadata,r.authorization_snapshot,r.route_attempt,m.source_kind,r.aggregation_state,
$6::timestamptz BETWEEN r.aggregation_started_at AND r.aggregation_deadline
FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.actor_id=r.actor_id
 AND m.conversation_id=r.conversation_id
WHERE r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
 AND r.message_id=$5 AND r.duplicate_of_message_id IS NULL
FOR UPDATE OF r,m`, claim.ServiceInstanceID, claim.Message.OwnerID, claim.Message.ActorID,
		claim.Message.ConversationID, targetMessageID, claim.AggregationStartedAt).Scan(&rootSequence, &rootContent, &rootClientID, &rootState,
		&rootTaskID, &rootDelivery, &rootMetadata, &rootAuthorization, &rootAttempt, &rootSourceKind,
		&rootAggregationState, &rootAggregationEligible)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeConflict, "conversation join target changed")
	}
	if err != nil {
		return fmt.Errorf("lock conversation join target: %w", err)
	}
	var rootInput app.ConversationRoutingInput
	if json.Unmarshal(rootMetadata, &rootInput) != nil || rootSequence != candidate.Sequence || !conversationRouteRequestCandidateMatchesContent(*candidate, rootContent) ||
		rootState != string(candidate.RoutingStatus) || rootTaskID != candidate.TaskID ||
		!reflect.DeepEqual(rootInput, candidate.RequestInput) {
		return domain.NewError(domain.CodeConflict, "conversation join target changed")
	}
	rootDigest, err := app.ConversationRouteExecutionScopeDigest(rootInput)
	if err != nil || !bytes.Equal(rootDigest, snapshot.Context.ExecutionScopeDigest) {
		return domain.NewError(domain.CodeConflict, "conversation join execution scope changed")
	}
	var rootAuthorizationValue task.Authorization
	if json.Unmarshal(rootAuthorization, &rootAuthorizationValue) != nil ||
		!reflect.DeepEqual(rootAuthorizationValue, candidate.Authorization) ||
		!reflect.DeepEqual(rootAuthorizationValue, claim.Authorization) {
		return requeueConversationRouteForReclassification(ctx, tx, claim)
	}
	if rootAggregationState == "open" || rootAggregationState == "waiting" {
		if (!rootAggregationEligible && !sameConversationRouteContent(rootContent, claim.Message.Content)) ||
			len(rootInput.ClientAttachmentIDs) != 0 {
			return requeueConversationRouteForReclassification(ctx, tx, claim)
		}
	}
	if rootTaskID != "" {
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM tasks WHERE id=$1 AND owner_id=$2 AND conversation_id=$3
 AND initiator_id=$4 FOR UPDATE`, rootTaskID, claim.Message.OwnerID, claim.Message.ConversationID, claim.Message.ActorID).Scan(&status); err != nil {
			return domain.NewError(domain.CodeConflict, "conversation join Task is no longer available")
		}
		if task.Status(status) != candidate.TaskStatus {
			return domain.NewError(domain.CodeConflict, "conversation join Task status changed")
		}
	} else if rootState == "routed" {
		var directReply bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND phase='normal' AND attempt=$3 AND state='settled'
 AND decision->>'Action'='reply')`, targetMessageID, claim.Message.ActorID, rootAttempt).Scan(&directReply); err != nil {
			return fmt.Errorf("verify direct reply before joining: %w", err)
		}
		if !directReply {
			return domain.NewError(domain.CodeConflict, "routed conversation is not a direct reply")
		}
	}
	causal := rootSequence < claim.Message.Sequence
	if !causal && claim.Input.PreviousClientMessageID != "" {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM conversation_message_routes root WHERE root.service_instance_id=$1::uuid AND root.owner_id=$2
  AND root.actor_id=$3 AND root.conversation_id=$4 AND root.message_id=$5 AND root.client_message_id=$6
 UNION ALL
 SELECT 1 FROM conversation_message_routes prior WHERE prior.service_instance_id=$1::uuid AND prior.owner_id=$2
  AND prior.actor_id=$3 AND prior.conversation_id=$4 AND prior.client_message_id=$6
   AND prior.duplicate_of_message_id=$5
)`, claim.ServiceInstanceID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID,
			targetMessageID, claim.Input.PreviousClientMessageID).Scan(&causal); err != nil {
			return fmt.Errorf("verify conversation join predecessor: %w", err)
		}
	}
	if !causal {
		return domain.NewError(domain.CodeConflict, "conversation join target is not causally earlier")
	}
	revisionTaskCandidate := (*app.ConversationRouteCandidate)(nil)
	revisionInput := !sameConversationRouteContent(rootContent, claim.Message.Content)
	if rootTaskID != "" && revisionInput {
		if err := requireConversationRouteScope(ctx, tx, claim, identity.ScopeTaskControl); err != nil {
			return err
		}
		for index := range snapshot.Context.Candidates {
			item := &snapshot.Context.Candidates[index]
			if item.TaskID == rootTaskID && item.InitiatorID == claim.Message.ActorID && item.Status == candidate.TaskStatus {
				revisionTaskCandidate = item
				break
			}
		}
		if revisionTaskCandidate == nil {
			return domain.NewError(domain.CodeConflict, "conversation join Task revision is no longer available")
		}
		if snapshot.Context.PreviousUnresolved && (snapshot.PreviousRoutingState != "waiting_adjustment" || snapshot.PreviousTaskID != rootTaskID) {
			return finishConversationRoute(ctx, tx, claim, "waiting_user", "我无法确认这次修改对应的任务。请指定目标任务。", "", false)
		}
		// A validated `join` decision is a fresh, same-scope statement that this
		// message adds to the target Task. Multiple messages can be classified in
		// parallel and commit in a different order than their sequence, so the
		// Task revision CAS below is the ordering fence here. Applying the older
		// message as a later revision preserves both inputs; explicit Task updates
		// still use the stricter causal-order check in the update path.
		if err := validateInitialRouteAttachments(ctx, tx, claim); err != nil {
			return err
		}
		pendingClarification, err := conversationRouteHasPendingFileDeliveryClarification(ctx, tx, rootTaskID)
		if err != nil {
			return err
		}
		if pendingClarification {
			return finishConversationRoute(ctx, tx, claim, "waiting_user",
				"目标任务仍在等待文件交付确认，请先处理该确认。 / The target Task is still waiting for a file delivery answer; resolve it first.", "", false)
		}
		if err := beginConversationTaskRevision(ctx, tx, claim, *revisionTaskCandidate); err != nil {
			return err
		}
	}
	if rootState == "routed" && rootTaskID == "" {
		if rootSourceKind != "conversation" || rootDelivery != "applied" {
			return domain.NewError(domain.CodeConflict, "routed conversation message cannot be safely joined")
		}
		requeued, err := requeueDirectConversationReplyForTask(ctx, tx, claim, targetMessageID, rootAttempt)
		if err != nil {
			return err
		}
		if !requeued {
			return domain.NewError(domain.CodeConflict, "direct conversation reply cannot be safely reclassified")
		}
		rootState, rootDelivery = "pending", "pending"
	}
	duplicateDelivery := rootDelivery
	routeState, routeReply := "routed", ""
	if revisionTaskCandidate != nil {
		duplicateDelivery = "pending"
		routeState, routeReply = "waiting_adjustment", "已收到补充，等待调整。"
	}
	messageUpdate, err := tx.Exec(ctx, `UPDATE conversation_messages SET delivery_status=$2
WHERE id=$1 AND owner_id=$3 AND actor_id=$4 AND conversation_id=$5 AND role='user' AND task_id IS NULL
	 AND delivery_status IN ('pending','applied','unknown')`, claim.Message.ID, duplicateDelivery, claim.Message.OwnerID,
		claim.Message.ActorID, claim.Message.ConversationID)
	if err != nil {
		return fmt.Errorf("copy original request delivery state to duplicate: %w", err)
	}
	if messageUpdate.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "duplicate conversation message is no longer available")
	}
	updated, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET duplicate_of_message_id=$2
WHERE service_instance_id=$1::uuid AND owner_id=$3 AND actor_id=$4 AND conversation_id=$5 AND message_id=$6
 AND state='processing' AND lease_owner=$7 AND lease_version=$8 AND duplicate_of_message_id IS NULL`,
		claim.ServiceInstanceID, targetMessageID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID,
		claim.Message.ID, claim.LeaseOwner, claim.LeaseVersion)
	if err != nil {
		return fmt.Errorf("associate duplicate conversation request: %w", err)
	}
	if updated.RowsAffected() != 1 {
		return domain.NewError(domain.CodeConflict, "duplicate conversation request changed")
	}
	return finishConversationRoute(ctx, tx, claim, routeState, routeReply, "", false)
}

func sameConversationRouteContent(first, second string) bool {
	return strings.TrimSpace(first) == strings.TrimSpace(second)
}

func conversationRouteRequestCandidateMatchesContent(candidate app.ConversationRouteRequestCandidate, content string) bool {
	if candidate.ContentTruncated {
		digest := sha256.Sum256([]byte(content))
		return len(candidate.ContentSHA256) == sha256.Size && bytes.Equal(candidate.ContentSHA256, digest[:])
	}
	return candidate.Content == content
}

func conversationRouteAggregationCandidateMatchesContent(candidate app.ConversationRouteAggregationCandidate, content string) bool {
	if candidate.Truncated {
		digest := sha256.Sum256([]byte(content))
		return len(candidate.ContentSHA256) == sha256.Size && bytes.Equal(candidate.ContentSHA256, digest[:])
	}
	return candidate.Content == content
}

func requeueDirectConversationReplyForTask(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim,
	rootMessageID string, rootAttempt int64) (bool, error) {
	if rootAttempt == int64(^uint64(0)>>1) {
		return false, nil
	}
	var directReply, unresolved bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND phase='normal' AND attempt=$3 AND state='settled' AND decision->>'Action'='reply'),
EXISTS (SELECT 1 FROM conversation_route_model_calls WHERE message_id=$1 AND actor_id=$2
 AND (state IN ('reserved','dispatched') OR (state='unknown' AND attempt=$3)))`, rootMessageID, claim.Message.ActorID,
		rootAttempt).Scan(&directReply, &unresolved); err != nil {
		return false, fmt.Errorf("verify prior direct conversation reply: %w", err)
	}
	if !directReply || unresolved {
		return false, nil
	}
	authorization, err := json.Marshal(claim.Authorization)
	if err != nil {
		return false, fmt.Errorf("encode current authorization for conversation reclassification: %w", err)
	}
	result, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET state='pending',route_attempt=route_attempt+1,
auto_retry_count=0,failure_reason='',authorization_snapshot=$2::jsonb,
request_metadata=jsonb_set(request_metadata,'{reclassify_direct_reply}','true'::jsonb,true),
available_after=clock_timestamp(),updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND message_id=$3 AND owner_id=$4 AND actor_id=$5 AND conversation_id=$6
 AND state='routed' AND route_attempt=$7 AND route_attempt<9223372036854775807`,
		claim.ServiceInstanceID, authorization, rootMessageID, claim.Message.OwnerID, claim.Message.ActorID,
		claim.Message.ConversationID, rootAttempt)
	if err != nil {
		return false, fmt.Errorf("requeue direct conversation reply: %w", err)
	}
	if result.RowsAffected() != 1 {
		return false, domain.NewError(domain.CodeConflict, "direct conversation reply changed before reclassification")
	}
	result, err = tx.Exec(ctx, `UPDATE conversation_messages SET delivery_status='pending'
WHERE id=$1 AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND role='user' AND task_id IS NULL AND delivery_status='applied'`,
		rootMessageID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID)
	if err != nil {
		return false, fmt.Errorf("mark direct conversation request pending for reclassification: %w", err)
	}
	if result.RowsAffected() != 1 {
		return false, domain.NewError(domain.CodeConflict, "direct conversation message changed before reclassification")
	}
	if err := syncConversationDuplicateMessages(ctx, tx, rootMessageID); err != nil {
		return false, err
	}
	if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: claim.Message.OwnerID,
		ObjectType: "message", ObjectID: rootMessageID, Operation: "upsert", MutationID: claim.Message.ClientMessageID}); err != nil {
		return false, err
	}
	return true, nil
}

func createConversationRouteTask(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim, routedContent string, modelID string,
	dependencyTaskIDs []string, fileDeliveryIntent task.FileDeliveryIntent,
	candidates []app.ConversationRouteCandidate, storedSummary *app.StoredConversationSummary,
	agentToolProfile task.AgentToolProfile, internalTool *app.ConversationRouteDecision) error {
	if err := validateInitialRouteAttachments(ctx, tx, claim); err != nil {
		return err
	}
	if !app.ValidConversationRouteDependencyIDs(dependencyTaskIDs, candidates) {
		return domain.NewError(domain.CodeConflict, "task dependency selection is no longer current")
	}
	if !domain.ValidModelConnectionID(modelID) || claim.Input.Budget.Validate() != nil {
		return domain.InvalidArgument("route_model", "conversation route model or budget is invalid")
	}
	if claim.Input.ModelID != "" && claim.Input.ModelID != modelID {
		return domain.NewError(domain.CodeConflict, "selected model changed during routing")
	}
	var allowed bool
	err := tx.QueryRow(ctx, `SELECT checked_at IS NOT NULL AND capability_text AND (owner_id IS NULL OR owner_id=$2)
FROM model_connections WHERE id=$1 FOR SHARE`, modelID, claim.Message.ActorID).Scan(&allowed)
	if err != nil || !allowed {
		return domain.NewError(domain.CodeNotFound, "model connection not found")
	}
	gate, err := requireOwnerExecutionEnabled(ctx, tx, claim.Message.ActorID, nil)
	if err != nil {
		return err
	}
	taskID, err := domain.NewTaskID()
	if err != nil {
		return err
	}
	objective := routedContent
	if routedContent == claim.Message.Content {
		objective = app.ConversationRouteTaskMessageObjective(claim.Message.ID, routedContent)
	}
	if strings.TrimSpace(objective) == "" {
		objective = app.ConversationAttachmentOnlyObjective
	}
	fileIntent := fileDeliveryIntent
	if fileIntent == "" {
		fileIntent = task.FileDeliveryNone
	}
	if !fileIntent.Valid() {
		return domain.InvalidArgument("route_decision", "file delivery intent is invalid")
	}
	var workspaceOwnerID string
	var workspaceProjectResourceVersion *int64
	if fileIntent == task.FileDeliveryRequired {
		workspaceOwnerID, workspaceProjectResourceVersion, err = validateConversationRouteWorkspaceInputSnapshot(ctx, tx, claim)
		if err != nil {
			return err
		}
	}
	initialStatus, initialEventType := task.StatusQueued, "task.status"
	initialWaitingReason, initialCurrentStep := "", ""
	var initialSteps []task.Step
	var initialClarification *task.ClarificationRequest
	if fileIntent == task.FileDeliveryUnclear {
		workspaceSnapshotAvailable := false
		if claim.Input.WorkspaceID != "" && (claim.Input.WorkspaceInputRevision != nil || len(claim.Input.WorkspaceContextDigest) != 0) {
			workspaceOwnerID, workspaceProjectResourceVersion, err = validateConversationRouteWorkspaceInputSnapshot(ctx, tx, claim)
			if err != nil {
				return err
			}
			workspaceSnapshotAvailable = true
		}
		initialSteps, initialClarification, err = newConversationRouteFileDeliveryClarification(taskID, workspaceSnapshotAvailable)
		if err != nil {
			return err
		}
		initialStatus, initialEventType = task.StatusWaitingUser, "task.waiting_user"
		initialWaitingReason, initialCurrentStep = "clarification_required", initialSteps[1].ID
	}
	var workspaceID string
	var inputRevision, workspaceCheckpointRevision *int64
	var contextDigest []byte
	if fileIntent == task.FileDeliveryRequired || (fileIntent == task.FileDeliveryUnclear && claim.Input.WorkspaceID != "" &&
		claim.Input.WorkspaceInputRevision != nil && len(claim.Input.WorkspaceContextDigest) == 32) {
		workspaceID = claim.Input.WorkspaceID
		if workspaceOwnerID == "" {
			workspaceOwnerID = claim.Message.ActorID
		}
		revision := *claim.Input.WorkspaceInputRevision
		inputRevision = &revision
		checkpointRevision := revision
		workspaceCheckpointRevision = &checkpointRevision
		contextDigest = append([]byte(nil), claim.Input.WorkspaceContextDigest...)
	}
	value := task.Task{ID: taskID, OwnerID: claim.Message.OwnerID, InitiatorID: claim.Message.ActorID,
		ProjectID: claim.Input.ProjectID, ConversationID: claim.Message.ConversationID, ModelID: modelID,
		WorkspaceID: workspaceID, WorkspaceOwnerID: workspaceOwnerID, InputRevision: inputRevision,
		WorkspaceProjectResourceVersion: workspaceProjectResourceVersion,
		WorkspaceCheckpointRevision:     workspaceCheckpointRevision, ContextDigest: contextDigest,
		Objective: objective, Status: initialStatus, WaitingReason: initialWaitingReason, CurrentStep: initialCurrentStep,
		Authorization: claim.Authorization, Budget: claim.Input.Budget,
		ExecutionLocation: "server", FileDeliveryIntent: fileIntent, CheckpointThroughSequence: -1}
	value = normalizeNewTask(value)
	if err := validateTaskConversationScope(value, value.ProjectID); err != nil {
		return err
	}
	if err := validateTask(value); err != nil {
		return err
	}
	through := claim.Message.Sequence - 1
	prior, err := readTaskConversationContextMessages(ctx, tx, value.OwnerID, value.ConversationID, through, nil, true)
	if err != nil {
		return err
	}
	prior, err = filterConversationRouteTaskHistory(ctx, tx, value, prior)
	if err != nil {
		return err
	}
	descendants, err := conversationRouteDescendantIDs(ctx, tx, claim)
	if err != nil {
		return err
	}
	if len(descendants) != 0 {
		future := make(map[string]struct{}, len(descendants))
		for _, id := range descendants {
			future[id] = struct{}{}
		}
		kept := prior[:0]
		for _, message := range prior {
			if _, isFuture := future[message.ID]; !isFuture {
				kept = append(kept, message)
			}
		}
		prior = kept
	}
	var summary *task.ConversationContextSummary
	if storedSummary != nil {
		summary = taskConversationSummaryReference(*storedSummary)
		prior = taskConversationContextWithoutSummarySources(prior, taskConversationSummarySources(storedSummary.Sources))
	}
	contextValue := buildTaskConversationContext(value.OwnerID, value.ProjectID, value.InitiatorID, value.ConversationID, through, prior, summary)
	contextValue.AgentToolProfile = agentToolProfile
	ids := make([]string, 0, len(contextValue.Messages))
	for _, message := range contextValue.Messages {
		ids = append(ids, message.ID)
	}
	manifest := taskConversationContextManifest{OwnerID: value.OwnerID, ProjectID: value.ProjectID, InitiatorID: value.InitiatorID,
		ConversationID: value.ConversationID, Scope: contextValue.Scope, ThroughSequence: through,
		MessageCount: len(ids), MessageIDs: ids, DigestSHA256: hex.EncodeToString(contextValue.Digest),
		AgentToolProfile: agentToolProfile, Summary: contextValue.Summary}
	if internalTool == nil && claim.Message.Content != "" && routedContent == claim.Message.Content && objective != routedContent {
		manifest.CurrentMessageSource, err = app.BuildConversationMessageSourceManifest(claim.Message.ID, claim.Message.Sequence, claim.Message.Content)
		if err != nil {
			return err
		}
	}
	eventPayload := map[string]string{"status": string(initialStatus)}
	if initialClarification != nil {
		eventPayload["reason"] = initialWaitingReason
		eventPayload["clarification_id"] = initialClarification.ID
		eventPayload["clarification_version"] = "1"
	}
	encodedEventPayload, err := json.Marshal(eventPayload)
	if err != nil {
		return fmt.Errorf("encode initial routed task event: %w", err)
	}
	payload, err := attachTaskConversationContextManifest(encodedEventPayload, manifest)
	if err != nil {
		return err
	}
	initialSubmission := app.TaskSubmission{Task: value, InitialEvent: task.Event{TaskID: taskID, Sequence: 1, Type: initialEventType, Payload: payload},
		InitialSteps: initialSteps, InitialClarificationRequest: initialClarification}
	if err := validateInitialFileDeliveryClarification(initialSubmission); err != nil {
		return err
	}
	if err := insertTask(ctx, tx, value, 1, gate.epoch); err != nil {
		return err
	}
	if err := insertInitialFileDeliveryClarification(ctx, tx, initialSubmission); err != nil {
		return err
	}
	if internalTool != nil {
		replyMessageID, err := domain.NewMessageID()
		if err != nil {
			return err
		}
		callID := "morde_route_tool_" + claim.Message.ID
		if _, err := tx.Exec(ctx, `INSERT INTO conversation_route_internal_tool_calls
(task_id,owner_id,message_id,call_id,tool_name,arguments,reply_message_id)
VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7)`, value.ID, value.OwnerID, claim.Message.ID, callID,
			internalTool.ToolName, internalTool.ToolArguments, replyMessageID); err != nil {
			return fmt.Errorf("persist direct route tool call: %w", err)
		}
	}
	for _, dependencyTaskID := range dependencyTaskIDs {
		var requiredRevision int64
		err := tx.QueryRow(ctx, `SELECT desired_revision FROM tasks
WHERE id=$1 AND owner_id=$2 AND conversation_id=$3 AND initiator_id=$4
 AND project_id IS NOT DISTINCT FROM NULLIF($5,'')
 AND status IN ('queued','running','waiting_user','pausing','paused','cancelling')
 AND waiting_reason<>'project_recovery_unavailable' FOR UPDATE`, dependencyTaskID, value.OwnerID, value.ConversationID, value.InitiatorID, value.ProjectID).
			Scan(&requiredRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeConflict, "task dependency changed before submission")
		}
		if err != nil {
			return fmt.Errorf("lock task dependency for submission: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_dependencies
(task_id,owner_id,conversation_id,dependency_task_id,required_revision) VALUES ($1,$2,$3,$4,$5)`,
			value.ID, value.OwnerID, value.ConversationID, dependencyTaskID, requiredRevision); err != nil {
			return fmt.Errorf("persist task dependency: %w", err)
		}
	}
	if err := insertTaskEvent(ctx, tx, task.Event{TaskID: value.ID, Sequence: 1, Type: initialEventType, Payload: payload}); err != nil {
		return err
	}
	var reclassifyingDirectReply bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((request_metadata->>'reclassify_direct_reply')::boolean,false)
FROM conversation_message_routes WHERE message_id=$1 AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4`,
		claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID).Scan(&reclassifyingDirectReply); err != nil {
		return fmt.Errorf("read direct reply reclassification state: %w", err)
	}
	if internalTool != nil {
		return finishConversationRoute(ctx, tx, claim, "routed", "", "", true)
	}
	return finishConversationRoute(ctx, tx, claim, "routed", "", taskID,
		len(claim.Input.ClientAttachmentIDs) == 0 && !reclassifyingDirectReply)
}

func validateConversationRouteWorkspaceInputSnapshot(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) (string, *int64, error) {
	input := claim.Input
	if !domain.ValidWorkspaceID(input.WorkspaceID) || input.WorkspaceInputRevision == nil || *input.WorkspaceInputRevision < 0 ||
		len(input.WorkspaceContextDigest) != 32 {
		if input.ProjectID != "" {
			return "", nil, domain.InvalidArgument("workspace_id", "a project Workspace requires an authorized project selection")
		}
		return "", nil, domain.InvalidArgument("workspace_id", "file delivery requires a valid Workspace input revision and context digest")
	}
	workspaceOwnerID := claim.Message.ActorID
	var projectResourceVersion *int64
	if input.ProjectID != "" {
		if !domain.ValidUserID(input.WorkspaceOwnerID) || input.WorkspaceProjectResourceVersion == nil || *input.WorkspaceProjectResourceVersion < 1 {
			return "", nil, domain.NewError(domain.CodeWorkspaceConflict, "project Workspace receipt is missing its accepted authorization snapshot")
		}
		ownerID, version, err := lockProjectTaskWorkspaceAccess(contextWithoutProjectWorkspacePermit(ctx), tx,
			input.ProjectID, claim.Message.ActorID, input.WorkspaceID, false)
		if err != nil {
			return "", nil, err
		}
		if ownerID != input.WorkspaceOwnerID || version != *input.WorkspaceProjectResourceVersion {
			return "", nil, domain.NewError(domain.CodeWorkspaceConflict, "project Workspace owner or resource changed after the conversation receipt")
		}
		workspaceOwnerID = ownerID
		projectResourceVersion = &version
	} else if claim.Message.ActorID != claim.Message.OwnerID {
		return "", nil, domain.InvalidArgument("workspace_id", "a personal Workspace must belong to the message owner")
	}
	var ownerID, state string
	var revision int64
	var deleted bool
	err := tx.QueryRow(ctx, `SELECT owner_id,state,revision,deleted_at IS NOT NULL FROM workspaces WHERE id=$1 FOR SHARE`,
		input.WorkspaceID).Scan(&ownerID, &state, &revision, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if err != nil {
		return "", nil, fmt.Errorf("lock selected conversation Workspace: %w", err)
	}
	if ownerID != workspaceOwnerID {
		return "", nil, domain.NewError(domain.CodeNotFound, "workspace not found")
	}
	if state != string(workspace.StateReady) || deleted {
		return "", nil, domain.NewError(domain.CodeWorkspaceConflict, "selected Workspace is not ready for a file delivery Task")
	}
	if revision != *input.WorkspaceInputRevision {
		return "", nil, domain.NewError(domain.CodeWorkspaceConflict, "selected Workspace changed after the conversation receipt")
	}
	actualDigest, err := conversationRouteWorkspaceManifestDigest(ctx, tx, input.WorkspaceID, revision)
	if err != nil {
		return "", nil, domain.NewError(domain.CodeWorkspaceConflict, "selected Workspace input manifest is unavailable or stale")
	}
	if !bytes.Equal(actualDigest, input.WorkspaceContextDigest) {
		return "", nil, domain.NewError(domain.CodeWorkspaceConflict, "selected Workspace context digest is stale")
	}
	return workspaceOwnerID, projectResourceVersion, nil
}

func conversationRouteWorkspaceManifestDigest(ctx context.Context, tx pgx.Tx, workspaceID string, revision int64) ([]byte, error) {
	files := make([]workspace.File, 0)
	emptyDirectories := make([]string, 0)
	directoriesByPath := make(map[string]workspace.Directory)
	afterPath := ""
	for {
		page, err := getWorkspaceManifestPageTx(ctx, tx, workspaceID, revision, "", afterPath, maxWorkspaceSyncPage)
		if err != nil {
			return nil, err
		}
		if page.WorkspaceID != workspaceID || page.Revision != revision {
			return nil, errors.New("selected Workspace manifest page changed revision")
		}
		if len(files)+len(emptyDirectories)+len(page.Files)+len(page.EmptyDirectories) > workspace.MaxSyncFiles {
			return nil, errors.New("selected Workspace manifest exceeds item limit")
		}
		files = append(files, page.Files...)
		emptyDirectories = append(emptyDirectories, page.EmptyDirectories...)
		pagePaths := make([]string, 0, len(page.Files)+len(page.EmptyDirectories))
		for _, directory := range page.Directories {
			if err := workspace.ValidateDirectory(directory, false); err != nil {
				return nil, err
			}
			key := strings.ToLower(directory.Path)
			if previous, exists := directoriesByPath[key]; exists {
				if !workspace.SameDirectoryMetadata(previous, directory) {
					return nil, errors.New("selected Workspace directory metadata changed between pages")
				}
				continue
			}
			directoriesByPath[key] = directory
		}
		for _, file := range page.Files {
			pagePaths = append(pagePaths, file.Path)
		}
		pagePaths = append(pagePaths, page.EmptyDirectories...)
		sort.Strings(pagePaths)
		previousPath := afterPath
		for _, path := range pagePaths {
			if path <= previousPath || strings.EqualFold(path, previousPath) {
				return nil, errors.New("selected Workspace manifest paths are not in cursor order")
			}
			previousPath = path
		}
		if page.NextCursor == nil {
			break
		}
		cursor, err := workspace.DecodeManifestCursor(*page.NextCursor)
		if err != nil || cursor.Revision != revision || cursor.Prefix != "" || len(pagePaths) == 0 ||
			cursor.After != pagePaths[len(pagePaths)-1] || cursor.After <= afterPath {
			return nil, errors.New("selected Workspace manifest cursor is invalid")
		}
		afterPath = cursor.After
	}
	directories := make([]workspace.Directory, 0, len(directoriesByPath))
	for _, directory := range directoriesByPath {
		directories = append(directories, directory)
	}
	sort.Slice(directories, func(i, j int) bool { return directories[i].Path < directories[j].Path })
	digest, err := workspace.ManifestTreeWithMetadataSHA256(files, emptyDirectories, directories)
	if err != nil {
		return nil, err
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		return nil, errors.New("selected Workspace manifest digest is invalid")
	}
	return decoded, nil
}

func newConversationRouteFileDeliveryClarification(taskID string, workspaceSnapshotAvailable bool) ([]task.Step, *task.ClarificationRequest, error) {
	classificationStepID, err := domain.NewStepID()
	if err != nil {
		return nil, nil, fmt.Errorf("generate routed file intent classification step ID: %w", err)
	}
	clarificationStepID, err := domain.NewStepID()
	if err != nil {
		return nil, nil, fmt.Errorf("generate routed file intent clarification step ID: %w", err)
	}
	requestID, err := domain.NewPendingRequestID()
	if err != nil {
		return nil, nil, fmt.Errorf("generate routed file intent clarification ID: %w", err)
	}
	question := "当前任务尚未绑定工作区。你需要交付文件，还是只要在聊天中回答？如需文件，请重新提交并选择工作区。 / This Task has no bound Workspace. Do you need a file, or is a chat answer enough? For file delivery, resubmit with a Workspace selected."
	if workspaceSnapshotAvailable {
		question = "你需要交付文件，还是只要在聊天中回答？如需文件，将使用本任务已绑定的工作区。 / Do you need a file, or is a chat answer enough? If you need a file, this Task will use its bound Workspace."
	}
	steps := []task.Step{
		{ID: classificationStepID, TaskID: taskID, Sequence: 1, Kind: "verification", Status: task.StepSucceeded,
			Title: "Check file delivery intent", Summary: "The router could not determine whether a file is required."},
		{ID: clarificationStepID, TaskID: taskID, Sequence: 2, Kind: "clarification", Status: task.StepWaitingUser,
			Title: "Confirm file delivery", Summary: question},
	}
	request := &task.ClarificationRequest{ID: requestID, TaskID: taskID, Version: 1, ModelStepID: classificationStepID,
		CallID: app.FileDeliveryIntentClarificationCallID, Question: question, Status: task.ClarificationPending}
	return steps, request, nil
}

func conversationRouteHasPendingFileDeliveryClarification(ctx context.Context, tx pgx.Tx, taskID string) (bool, error) {
	var pending bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM conversation_route_file_delivery_clarifications
 WHERE task_id=$1 AND status='pending'
)`, taskID).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("check pending route file delivery clarification: %w", err)
	}
	return pending, nil
}

func beginConversationRouteFileDeliveryClarification(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim,
	candidate app.ConversationRouteCandidate) (string, error) {
	if err := beginConversationTaskRevision(ctx, tx, claim, candidate); err != nil {
		return "", err
	}
	var nextSequence, version int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM task_steps WHERE task_id=$1`, candidate.TaskID).Scan(&nextSequence); err != nil {
		return "", fmt.Errorf("read next Task clarification step sequence: %w", err)
	}
	if nextSequence < 1 || nextSequence == int64(^uint64(0)>>1) {
		return "", domain.NewError(domain.CodeInvalidState, "Task step sequence is exhausted")
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM task_clarification_requests WHERE task_id=$1`, candidate.TaskID).Scan(&version); err != nil {
		return "", fmt.Errorf("read next Task clarification version: %w", err)
	}
	if version < 1 || version == int64(^uint64(0)>>1) {
		return "", domain.NewError(domain.CodeInvalidState, "Task clarification version is exhausted")
	}
	classificationStepID, err := domain.NewStepID()
	if err != nil {
		return "", fmt.Errorf("generate route file intent classification step ID: %w", err)
	}
	clarificationStepID, err := domain.NewStepID()
	if err != nil {
		return "", fmt.Errorf("generate route file intent clarification step ID: %w", err)
	}
	requestID, err := domain.NewPendingRequestID()
	if err != nil {
		return "", fmt.Errorf("generate route file intent clarification ID: %w", err)
	}
	question := "这次修改需要交付文件，还是只需在聊天中回答？文件交付将使用当前任务绑定的工作区；如果任务没有可用工作区，无法选择文件交付。 / Does this update need a file, or is a chat answer enough? File delivery uses the Workspace already bound to this Task; it cannot be selected if no usable Workspace is bound."
	steps := []task.Step{
		{ID: classificationStepID, TaskID: candidate.TaskID, Sequence: nextSequence, Kind: "verification", Status: task.StepSucceeded,
			Title: "Check file delivery intent", Summary: "The route could not determine whether this Task update needs a file."},
		{ID: clarificationStepID, TaskID: candidate.TaskID, Sequence: nextSequence + 1, Kind: "clarification", Status: task.StepWaitingUser,
			Title: "Confirm file delivery", Summary: question},
	}
	for _, step := range steps {
		if _, err := tx.Exec(ctx, `INSERT INTO task_steps (id,task_id,sequence,kind,status,title,summary)
VALUES ($1,$2,$3,$4,$5,$6,$7)`, step.ID, step.TaskID, step.Sequence, step.Kind, string(step.Status), step.Title, step.Summary); err != nil {
			return "", fmt.Errorf("persist route file delivery clarification step: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO task_clarification_requests
(id,task_id,version,model_step_id,call_id,question,status)
VALUES ($1,$2,$3,$4,$5,$6,'pending')`, requestID, candidate.TaskID, version, classificationStepID,
		task.RouteFileDeliveryIntentClarificationCallID, question); err != nil {
		return "", fmt.Errorf("persist route file delivery clarification request: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO conversation_route_file_delivery_clarifications
(message_id,task_id,owner_id,initiator_id,conversation_id,expected_revision,clarification_id,clarification_version,status)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'pending')`, claim.Message.ID, candidate.TaskID, claim.Message.OwnerID,
		claim.Message.ActorID, claim.Message.ConversationID, candidate.Revision, requestID, version); err != nil {
		return "", fmt.Errorf("persist route file delivery clarification binding: %w", err)
	}
	return question, nil
}

func validateConversationRouteTaskFileDeliverySnapshot(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim,
	taskID string, decisionIntent task.FileDeliveryIntent) error {
	var storedIntent, workspaceID, workspaceOwnerID, initiatorID string
	var hasWorkspaceInputSnapshot bool
	var inputRevision, checkpointRevision pgtype.Int8
	var contextDigest []byte
	err := tx.QueryRow(ctx, `SELECT file_delivery_intent,COALESCE(workspace_id,''),COALESCE(workspace_owner_id,''),initiator_id,
input_revision,workspace_checkpoint_revision,context_digest,
input_revision IS NOT NULL AND workspace_checkpoint_revision IS NOT NULL
AND workspace_checkpoint_revision=input_revision AND COALESCE(octet_length(context_digest)=32,false)
FROM tasks WHERE id=$1 AND owner_id=$2 AND initiator_id=$3 AND conversation_id=$4 FOR UPDATE`,
		taskID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID).
		Scan(&storedIntent, &workspaceID, &workspaceOwnerID, &initiatorID, &inputRevision, &checkpointRevision, &contextDigest, &hasWorkspaceInputSnapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeNotFound, "task not found")
	}
	if err != nil {
		return fmt.Errorf("read task Workspace snapshot for route update: %w", err)
	}
	currentIntent := task.FileDeliveryIntent(storedIntent)
	if !currentIntent.Valid() {
		return domain.NewError(domain.CodeInvalidState, "task file delivery intent is invalid")
	}
	effectiveIntent := decisionIntent
	if effectiveIntent == "" {
		effectiveIntent = currentIntent
	}
	if effectiveIntent == task.FileDeliveryUnclear {
		return domain.NewError(domain.CodeConflict, "file delivery intent must be clarified before updating this Task")
	}
	if effectiveIntent == task.FileDeliveryRequired && (workspaceID == "" || !hasWorkspaceInputSnapshot) {
		return domain.InvalidArgument("workspace_id", "file delivery requires a Workspace input snapshot bound to the Task")
	}
	if effectiveIntent == task.FileDeliveryRequired {
		if !domain.ValidWorkspaceID(workspaceID) || !inputRevision.Valid || !checkpointRevision.Valid ||
			inputRevision.Int64 < 0 || checkpointRevision.Int64 != inputRevision.Int64 || len(contextDigest) != 32 || workspaceOwnerID == "" {
			return domain.InvalidArgument("workspace_id", "file delivery requires a complete Workspace input snapshot bound to the Task")
		}
		var ownerID, state string
		var revision int64
		var deleted bool
		err := tx.QueryRow(ctx, `SELECT owner_id,state,revision,deleted_at IS NOT NULL FROM workspaces WHERE id=$1 FOR SHARE`, workspaceID).
			Scan(&ownerID, &state, &revision, &deleted)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if err != nil {
			return fmt.Errorf("check Task Workspace snapshot for file delivery: %w", err)
		}
		if ownerID != workspaceOwnerID {
			return domain.NewError(domain.CodeNotFound, "workspace not found")
		}
		if state != string(workspace.StateReady) || deleted || revision != checkpointRevision.Int64 {
			return domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace snapshot is no longer current and ready for file delivery")
		}
		actualDigest, err := conversationRouteWorkspaceManifestDigest(ctx, tx, workspaceID, revision)
		if err != nil || !bytes.Equal(actualDigest, contextDigest) {
			return domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace input manifest is unavailable or stale")
		}
		if workspaceOwnerID != initiatorID {
			return domain.NewError(domain.CodeWorkspaceConflict, "Task Workspace owner is no longer authorized for this file delivery")
		}
	}
	return nil
}

func validateInitialRouteAttachments(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) error {
	if len(claim.Input.ClientAttachmentIDs) == 0 {
		return nil
	}
	var total, matching, distinctAttachments int
	messageID := claim.Message.ID
	if domain.ValidMessageID(claim.Input.VariantSourceMessageID) {
		messageID = claim.Input.VariantSourceMessageID
	}
	if claim.Input.ProjectID != "" {
		err := tx.QueryRow(ctx, `SELECT count(*),
 count(*) FILTER (WHERE client_attachment_id=ANY($7::text[]) AND state='committed'),count(DISTINCT attachment_id)
FROM project_conversation_attachments
WHERE message_id=$1 AND service_instance_id=$2::uuid AND owner_id=$3 AND actor_id=$4
 AND conversation_id=$5 AND project_id=$6`, messageID, claim.ServiceInstanceID,
			claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID, claim.Input.ProjectID,
			claim.Input.ClientAttachmentIDs).Scan(&total, &matching, &distinctAttachments)
		if err != nil {
			return fmt.Errorf("check initial project conversation attachments: %w", err)
		}
		if total != len(claim.Input.ClientAttachmentIDs) || matching != total || distinctAttachments != total {
			return domain.NewError(domain.CodeInvalidState, "initial project conversation attachments are not ready")
		}
		return nil
	}
	err := tx.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE slot.client_attachment_id=ANY($4::text[]) AND upload.state='committed'),count(DISTINCT slot.attachment_id)
FROM conversation_attachment_slots slot
JOIN conversation_message_attachments link ON link.service_instance_id=slot.service_instance_id AND link.owner_id=slot.owner_id
 AND link.message_id=slot.message_id AND link.attachment_id=slot.attachment_id
JOIN personal_attachment_uploads upload ON upload.service_instance_id=link.service_instance_id AND upload.owner_id=link.owner_id
 AND upload.upload_id=link.upload_id AND upload.attachment_id=link.attachment_id
WHERE slot.message_id=$1 AND slot.service_instance_id=$2::uuid AND slot.owner_id=$3`,
		messageID, claim.ServiceInstanceID, claim.Message.ActorID, claim.Input.ClientAttachmentIDs).Scan(&total, &matching, &distinctAttachments)
	if err != nil {
		return fmt.Errorf("check initial conversation attachments: %w", err)
	}
	if total != len(claim.Input.ClientAttachmentIDs) || matching != total || distinctAttachments != total {
		return domain.NewError(domain.CodeInvalidState, "initial conversation attachments are not ready")
	}
	return nil
}

// Pending messages from unrelated Tasks are not instructions for a newly
// created Task. Only completed routed Tasks can contribute routed history.
func filterConversationRouteTaskHistory(ctx context.Context, tx pgx.Tx, value task.Task,
	messages []task.ConversationContextMessage) ([]task.ConversationContextMessage, error) {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	if len(ids) == 0 {
		return messages, nil
	}
	rows, err := tx.Query(ctx, `SELECT m.id FROM conversation_messages m
LEFT JOIN tasks t ON t.id=m.task_id AND t.owner_id=m.owner_id AND t.conversation_id=m.conversation_id
WHERE m.id=ANY($1::text[]) AND m.owner_id=$2 AND m.conversation_id=$3 AND (
 ($4::text='' AND (NOT EXISTS (SELECT 1 FROM conversation_message_routes r WHERE r.message_id=m.id)
    OR (m.task_id IS NOT NULL AND t.status='completed')))
 OR ($4::text<>'' AND m.delivery_status='applied'
  AND NOT EXISTS (SELECT 1 FROM conversation_message_routes r WHERE r.message_id=m.id AND r.state<>'routed')
  AND ((m.actor_id=$5 AND (NOT EXISTS (SELECT 1 FROM conversation_message_routes r WHERE r.message_id=m.id)
         OR (m.task_id IS NOT NULL AND t.status='completed')))
    OR (m.actor_id<>$5 AND EXISTS (SELECT 1 FROM conversations project_scope
      WHERE project_scope.id=m.conversation_id AND project_scope.owner_id=m.owner_id AND project_scope.project_id=$4)
      AND (`+projectSharedHistoryEvidenceSQL+`)))
 )
)`, ids, value.OwnerID, value.ConversationID, value.ProjectID, value.InitiatorID)
	if err != nil {
		return nil, fmt.Errorf("filter routed task history: %w", err)
	}
	defer rows.Close()
	allowed := make(map[string]struct{}, len(ids))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan routed task history: %w", err)
		}
		allowed[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read routed task history: %w", err)
	}
	filtered := make([]task.ConversationContextMessage, 0, len(messages))
	for _, message := range messages {
		if _, ok := allowed[message.ID]; ok {
			filtered = append(filtered, message)
		}
	}
	return filtered, nil
}

func conversationRouteStatusText(status task.Status) string {
	switch status {
	case task.StatusQueued:
		return "该任务正在等待执行。"
	case task.StatusRunning:
		return "该任务正在执行。"
	case task.StatusWaitingUser:
		return "该任务正在等待你的处理。"
	case task.StatusPausing:
		return "该任务正在暂停。"
	case task.StatusPaused:
		return "该任务已暂停。"
	case task.StatusCancelling:
		return "该任务正在停止。"
	default:
		return "任务状态已变化，请刷新后查看。"
	}
}

func finishConversationRoute(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim,
	state, reply, taskID string, applied bool) error {
	if taskID != "" {
		status := "pending"
		if applied {
			status = "applied"
		}
		if _, err := tx.Exec(ctx, `UPDATE conversation_messages SET task_id=$2,delivery_status=$3 WHERE id=$1`, claim.Message.ID, taskID, status); err != nil {
			return fmt.Errorf("associate routed message with task: %w", err)
		}
		if claim.Input.VariantOperationID != "" {
			updated, err := tx.Exec(ctx, `UPDATE conversation_message_variants SET task_id=$2,state='running',updated_at=clock_timestamp()
WHERE operation_id=$1 AND actor_id=$3 AND state='accepted'`, claim.Input.VariantOperationID, taskID, claim.Message.ActorID)
			if err != nil {
				return fmt.Errorf("associate variant operation with Task: %w", err)
			}
			if updated.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "conversation variant operation changed during routing")
			}
		}
	} else if applied {
		result, err := tx.Exec(ctx, `UPDATE conversation_messages SET delivery_status='applied'
WHERE id=$1 AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND role='user' AND task_id IS NULL AND delivery_status IN ('pending','applied')`,
			claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID)
		if err != nil {
			return fmt.Errorf("mark completed conversation action as applied: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "conversation message is no longer awaiting a completed action")
		}
	}
	if reply != "" {
		messageID, err := domain.NewMessageID()
		if err != nil {
			return err
		}
		variantGroupID, variantVersion, variantAction, operationID := claim.Input.VariantGroupID, claim.Input.VariantVersion, claim.Input.VariantAction, claim.Input.VariantOperationID
		if variantGroupID == "" {
			variantGroupID, variantVersion, variantAction = claim.Message.ID, 1, "original"
		}
		if err := insertMessage(ctx, tx, conversation.Message{ID: messageID, OwnerID: claim.Message.OwnerID,
			ActorID: claim.Message.ActorID, ConversationID: claim.Message.ConversationID, Role: conversation.RoleAssistant,
			Content: reply, SourceKind: "conversation", DeliveryStatus: conversation.DeliveryApplied,
			VariantGroupID: variantGroupID, VariantVersion: variantVersion, VariantAction: variantAction, VariantOperationID: operationID}); err != nil {
			return err
		}
		if operationID != "" {
			if _, err := tx.Exec(ctx, `UPDATE conversation_message_variants SET state='completed',assistant_message_id=$2,updated_at=clock_timestamp()
WHERE operation_id=$1 AND actor_id=$3 AND state='accepted'`, operationID, messageID, claim.Message.ActorID); err != nil {
				return fmt.Errorf("complete direct conversation variant: %w", err)
			}
		}
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: claim.Message.OwnerID,
			ObjectType: "message", ObjectID: messageID, Operation: "upsert", MutationID: claim.Message.ClientMessageID}); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET state=$2,
aggregation_state=CASE WHEN aggregation_state='cancelled' THEN 'cancelled' ELSE 'settled' END,
lease_owner='',lease_expires_at=NULL,lease_version=lease_version+1,
failure_reason='',awaiting_predecessor=false,updated_at=clock_timestamp() WHERE message_id=$1`, claim.Message.ID, state); err != nil {
		return fmt.Errorf("complete conversation route: %w", err)
	}
	if err := syncConversationDuplicateMessages(ctx, tx, claim.Message.ID); err != nil {
		return err
	}
	if state == "routed" || state == "waiting_adjustment" {
		rows, err := tx.Query(ctx, `UPDATE conversation_message_routes SET state='pending',awaiting_predecessor=false,
available_after=clock_timestamp(),updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
 AND previous_client_message_id=$5 AND state='waiting_user' AND awaiting_predecessor RETURNING message_id`,
			claim.ServiceInstanceID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID, claim.Message.ClientMessageID)
		if err != nil {
			return fmt.Errorf("resume dependent conversation routes: %w", err)
		}
		var resumed []string
		for rows.Next() {
			var messageID string
			if err := rows.Scan(&messageID); err != nil {
				rows.Close()
				return fmt.Errorf("read resumed conversation routes: %w", err)
			}
			resumed = append(resumed, messageID)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("read resumed conversation routes: %w", err)
		}
		for _, messageID := range resumed {
			if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: claim.Message.OwnerID,
				ObjectType: "message", ObjectID: messageID, Operation: "upsert", MutationID: claim.Message.ClientMessageID}); err != nil {
				return err
			}
		}
	}
	changes := []accountObjectChange{{OwnerID: claim.Message.OwnerID, ObjectType: "conversation",
		ObjectID: claim.Message.ConversationID, Operation: "upsert", MutationID: claim.Message.ClientMessageID}}
	if claim.Input.ResultReviewID == "" {
		changes = append(changes, accountObjectChange{OwnerID: claim.Message.OwnerID, ObjectType: "message",
			ObjectID: claim.Message.ID, Operation: "upsert", MutationID: claim.Message.ClientMessageID})
	}
	return recordAccountObjectChanges(ctx, tx, changes...)
}

func (s *Store) FailConversationRoute(ctx context.Context, claim app.ConversationRouteClaim, reason string) error {
	if reason != "model_unavailable" && reason != "routing_context_unavailable" && reason != "routing_action_unavailable" && reason != "message_too_long" {
		return domain.InvalidArgument("route_failure", "invalid conversation route failure")
	}
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		var taskIDs []string
		if claim.Input.ResultReviewID != "" {
			var reviewState string
			err := tx.QueryRow(ctx, `SELECT state FROM conversation_result_batches
WHERE message_id=$1 AND trigger_message_id=$2 FOR UPDATE`, claim.Input.ResultReviewID, claim.Message.ID).Scan(&reviewState)
			if err != nil {
				return domain.NewError(domain.CodeConflict, "task result review route is no longer current")
			}
			if reviewState != "processing" && reviewState != "superseded" {
				return domain.NewError(domain.CodeConflict, "task result review route is no longer current")
			}
			taskIDs, err = conversationResultReviewTaskIDsInTx(ctx, tx, claim.Input.ResultReviewID)
			if err != nil {
				return err
			}
			if reviewState == "superseded" {
				result, err := tx.Exec(ctx, `UPDATE conversation_message_routes r SET state='routed',failure_reason='',
lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
FROM service_identity i WHERE i.singleton=true AND i.instance_id=r.service_instance_id AND r.service_instance_id=$1::uuid
 AND r.message_id=$2 AND r.state='processing' AND r.lease_owner=$3 AND r.lease_version=$4 AND r.lease_expires_at>clock_timestamp()
 AND r.owner_id=$5 AND r.actor_id=$6 AND r.conversation_id=$7`, claim.ServiceInstanceID, claim.Message.ID, claim.LeaseOwner,
					claim.LeaseVersion, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID)
				if err != nil {
					return fmt.Errorf("finish superseded task result review route: %w", err)
				}
				if result.RowsAffected() != 1 {
					return domain.NewError(domain.CodeConflict, "conversation route lease is no longer current")
				}
				if _, err := tx.Exec(ctx, `UPDATE conversation_messages SET delivery_status='applied' WHERE id=$1`, claim.Message.ID); err != nil {
					return fmt.Errorf("finish superseded task result review trigger: %w", err)
				}
				return recordTaskResultReviewChanges(ctx, tx, claim, taskIDs)
			}
		}
		result, err := tx.Exec(ctx, `UPDATE conversation_message_routes r SET state='failed',failure_reason=$5,
lease_owner='',lease_expires_at=NULL,updated_at=clock_timestamp()
FROM service_identity i WHERE i.singleton=true AND i.instance_id=r.service_instance_id AND r.service_instance_id=$1::uuid
 AND r.message_id=$2 AND r.state='processing' AND r.lease_owner=$3 AND r.lease_version=$4 AND r.lease_expires_at>clock_timestamp()
 AND r.owner_id=$6 AND r.actor_id=$7 AND r.conversation_id=$8`,
			claim.ServiceInstanceID, claim.Message.ID, claim.LeaseOwner, claim.LeaseVersion, reason,
			claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID)
		if err != nil {
			return fmt.Errorf("record conversation route failure: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "conversation route lease is no longer current")
		}
		if err := syncConversationDuplicateMessages(ctx, tx, claim.Message.ID); err != nil {
			return err
		}
		if claim.Input.ResultReviewID != "" {
			result, err := tx.Exec(ctx, `UPDATE conversation_result_batches SET state='failed',updated_at=clock_timestamp()
WHERE message_id=$1 AND trigger_message_id=$2 AND state='processing'`, claim.Input.ResultReviewID, claim.Message.ID)
			if err != nil {
				return fmt.Errorf("fail task result review batch: %w", err)
			}
			if result.RowsAffected() != 1 {
				return domain.NewError(domain.CodeConflict, "task result review changed before failure was recorded")
			}
			if _, err := tx.Exec(ctx, `UPDATE conversation_messages SET delivery_status='applied' WHERE id=$1`, claim.Message.ID); err != nil {
				return fmt.Errorf("finish failed task result review trigger: %w", err)
			}
			return recordTaskResultReviewChanges(ctx, tx, claim, taskIDs)
		}
		if claim.Input.VariantOperationID != "" {
			if _, err := tx.Exec(ctx, `UPDATE conversation_message_variants SET state='failed',updated_at=clock_timestamp()
WHERE operation_id=$1 AND actor_id=$2 AND state IN ('accepted','running')`, claim.Input.VariantOperationID, claim.Message.ActorID); err != nil {
				return fmt.Errorf("fail conversation variant route: %w", err)
			}
		}
		return recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: claim.Message.OwnerID,
			ObjectType: "message", ObjectID: claim.Message.ID, Operation: "upsert", MutationID: claim.Message.ClientMessageID})
	})
}

func recordTaskResultReviewChanges(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim, taskIDs []string) error {
	changes := make([]accountObjectChange, 0, len(taskIDs)+1)
	for _, taskID := range taskIDs {
		changes = append(changes, accountObjectChange{OwnerID: claim.Message.OwnerID, ObjectType: "task", ObjectID: taskID, Operation: "upsert"})
	}
	changes = append(changes, accountObjectChange{OwnerID: claim.Message.OwnerID, ObjectType: "conversation",
		ObjectID: claim.Message.ConversationID, Operation: "upsert"})
	return recordAccountObjectChanges(ctx, tx, changes...)
}

// RetryConversationRoute reuses the accepted message and requires a fresh,
// authorized client action. Replaying the same key never starts a second retry.
func (s *Store) RetryConversationRoute(ctx context.Context, principal identity.Principal,
	projectID, conversationID, messageID, instanceID, idempotencyKey string) error {
	if ctx == nil || !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		(principal.Kind != identity.KindSession && principal.Kind != identity.KindClient) ||
		!principal.HasScope(identity.ScopeTaskSubmit) || !domain.ValidConversationID(conversationID) ||
		!domain.ValidMessageID(messageID) || !domain.ValidServiceInstanceID(instanceID) || !validProjectIdempotencyKey(idempotencyKey) {
		return domain.InvalidArgument("route_retry", "conversation route retry identity is invalid")
	}
	requestedMessageID := messageID
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireConversationServiceInstance(ctx, tx, instanceID); err != nil {
			return err
		}
		var ownerID, duplicateOf string
		err := tx.QueryRow(ctx, `SELECT r.owner_id,COALESCE(r.duplicate_of_message_id,'') FROM conversation_message_routes r
JOIN conversations c ON c.id=r.conversation_id AND c.owner_id=r.owner_id
WHERE r.service_instance_id=$1::uuid AND r.message_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
 AND c.project_id IS NOT DISTINCT FROM NULLIF($5,'')`, instanceID, requestedMessageID, principal.UserID, conversationID, projectID).Scan(&ownerID, &duplicateOf)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "conversation message not found")
		}
		if err != nil {
			return fmt.Errorf("find conversation route to retry: %w", err)
		}
		if duplicateOf != "" {
			messageID = duplicateOf
		}
		receipt := app.ConversationMessageReceipt{ServiceInstanceID: instanceID,
			Message: conversation.Message{ID: messageID, OwnerID: ownerID, ActorID: principal.UserID, ConversationID: conversationID},
			Input:   app.ConversationRoutingInput{ProjectID: projectID}}
		if err := lockConversationReceiptScopeInTx(ctx, tx, receipt); err != nil {
			return err
		}
		expiresAt, tokenScopes, err := authorizeConversationRouteRetryInTx(ctx, tx, principal)
		if err != nil {
			return err
		}
		var requestHash []byte
		var state, failureReason, sourceKind, resultReviewID string
		var routeAttempt int64
		var taskID *string
		err = tx.QueryRow(ctx, `SELECT r.request_hash,r.state,r.failure_reason,m.task_id,m.source_kind,COALESCE(r.request_metadata->>'result_review_id',''),r.route_attempt FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.actor_id=r.actor_id AND m.conversation_id=r.conversation_id AND m.owner_id=r.owner_id
WHERE r.service_instance_id=$1::uuid AND r.message_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
FOR UPDATE OF r,m`, instanceID, messageID, principal.UserID, conversationID).Scan(&requestHash, &state, &failureReason, &taskID, &sourceKind, &resultReviewID, &routeAttempt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "conversation message not found")
		}
		if err != nil {
			return fmt.Errorf("lock conversation route to retry: %w", err)
		}
		encoded, err := json.Marshal(struct {
			InstanceID     string `json:"instance_id"`
			ActorID        string `json:"actor_id"`
			ConversationID string `json:"conversation_id"`
			MessageID      string `json:"message_id"`
			RequestHash    []byte `json:"request_hash"`
		}{instanceID, principal.UserID, conversationID, messageID, requestHash})
		if err != nil {
			return err
		}
		digest := sha256.Sum256(encoded)
		record := app.IdempotencyRecord{OwnerID: principal.UserID, Operation: "conversation.route.retry",
			Key: idempotencyKey, RequestHash: digest[:]}
		stored, replayed, err := reserveIdempotency(ctx, tx, record, messageID, conversationID)
		if err != nil {
			return err
		}
		if replayed {
			if stored.ResourceID != messageID || stored.RelatedID != conversationID {
				return domain.NewError(domain.CodeIdempotencyConflict, "retry key was used for another message")
			}
			return nil
		}
		eligibility, err := conversationRouteRetryEligibilityInTx(ctx, tx, messageID, principal.UserID,
			state, failureReason, taskID, sourceKind, resultReviewID, routeAttempt)
		if err != nil {
			return err
		}
		if !eligibility.Eligible {
			return conversationRouteRetryEligibilityError(eligibility.Reason)
		}
		auth := task.Authorization{Kind: string(principal.Kind), TokenID: principal.TokenID,
			ClientInstallationID: principal.ClientInstallationID, Version: 1, ExpiresAt: &expiresAt}
		auth.Scopes = append(auth.Scopes, tokenScopes...)
		if sourceKind == "task_result_review" {
			var authorizationVersion int64
			err := tx.QueryRow(ctx, `UPDATE conversation_result_batches SET state='processing',retry_attempt=retry_attempt+1,updated_at=clock_timestamp()
WHERE message_id=$1 AND trigger_message_id=$2 AND state='failed' AND retry_attempt<9223372036854775807
RETURNING authorization_version`, resultReviewID, messageID).Scan(&authorizationVersion)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeConflict, "task result review route is no longer retryable")
			}
			if err != nil {
				return fmt.Errorf("retry task result review batch: %w", err)
			}
			auth = task.Authorization{Kind: string(identity.KindSession), Version: authorizationVersion,
				Scopes: []string{string(identity.ScopeTaskSubmit)}}
		}
		authorization, err := json.Marshal(auth)
		if err != nil {
			return err
		}
		query := `UPDATE conversation_message_routes SET state='pending',failure_reason='',
authorization_snapshot=$2::jsonb,available_after=clock_timestamp(),updated_at=clock_timestamp() WHERE message_id=$1 AND state='failed'`
		args := []any{messageID, authorization}
		if sourceKind != "task_result_review" {
			query = `UPDATE conversation_message_routes SET state='pending',failure_reason='',route_attempt=route_attempt+1,auto_retry_count=0,
authorization_snapshot=$2::jsonb,available_after=clock_timestamp(),updated_at=clock_timestamp() WHERE message_id=$1 AND state='failed' AND route_attempt=$3`
			args = append(args, routeAttempt)
		}
		result, err := tx.Exec(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("retry conversation route: %w", err)
		}
		if result.RowsAffected() != 1 {
			return domain.NewError(domain.CodeConflict, "conversation route changed before retry")
		}
		root := app.ConversationRouteClaim{ServiceInstanceID: instanceID, Message: conversation.Message{ID: messageID,
			OwnerID: ownerID, ActorID: principal.UserID, ConversationID: conversationID, ClientMessageID: idempotencyKey}}
		if err := syncConversationDuplicateMessages(ctx, tx, root.Message.ID); err != nil {
			return err
		}
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: ownerID, ObjectType: "message",
			ObjectID: messageID, Operation: "upsert", MutationID: idempotencyKey}); err != nil {
			return err
		}
		if requestedMessageID != messageID {
			return recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: ownerID, ObjectType: "message",
				ObjectID: requestedMessageID, Operation: "upsert", MutationID: idempotencyKey})
		}
		return nil
	})
}

func authorizeConversationRouteRetryInTx(ctx context.Context, tx pgx.Tx, principal identity.Principal) (time.Time, []string, error) {
	var active bool
	var expiresAt time.Time
	var tokenScopes []string
	err := tx.QueryRow(ctx, `SELECT u.status='approved' AND t.revoked_at IS NULL AND t.expires_at>clock_timestamp()
 AND t.kind=$3 AND COALESCE(t.client_installation_id,'')=$4,t.expires_at,t.scopes
FROM users u JOIN tokens t ON t.owner_id=u.id WHERE u.id=$1 AND t.id=$2 FOR SHARE OF u,t`,
		principal.UserID, principal.TokenID, string(principal.Kind), principal.ClientInstallationID).Scan(&active, &expiresAt, &tokenScopes)
	if err != nil || !active {
		return time.Time{}, nil, domain.NewError(domain.CodeUnauthenticated, "conversation route retry credential is unavailable")
	}
	if principal.Kind == identity.KindClient && !domain.ValidClientInstallationID(principal.ClientInstallationID) {
		return time.Time{}, nil, domain.NewError(domain.CodeUnauthenticated, "conversation route retry installation is unavailable")
	}
	if principal.ClientInstallationID != "" {
		if err := checkModelInstallation(ctx, tx, principal.ClientInstallationID, principal.UserID); err != nil {
			return time.Time{}, nil, err
		}
	}
	current := identity.Principal{Kind: principal.Kind}
	for _, value := range tokenScopes {
		current.Scopes = append(current.Scopes, identity.Scope(value))
	}
	if !current.HasScope(identity.ScopeTaskSubmit) {
		return time.Time{}, nil, domain.NewError(domain.CodeForbidden, "conversation route retry is not authorized")
	}
	return expiresAt, tokenScopes, nil
}

func conversationRouteRetryEligibilityInTx(ctx context.Context, tx pgx.Tx, messageID, actorID, state, failureReason string,
	taskID *string, sourceKind, resultReviewID string, routeAttempt int64) (app.ConversationMessageRetryEligibility, error) {
	ineligible := func(reason string) app.ConversationMessageRetryEligibility {
		return app.ConversationMessageRetryEligibility{Eligible: false, Reason: reason}
	}
	if failureReason == "message_too_long" {
		return ineligible(app.ConversationMessageRetryReasonMessageTooLong), nil
	}
	var unknownSharedUsage bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_route_model_calls c
JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
WHERE c.message_id=$1 AND c.actor_id=$2 AND c.state='unknown' AND l.state='unknown' AND NOT l.personal_model)`,
		messageID, actorID).Scan(&unknownSharedUsage); err != nil {
		return app.ConversationMessageRetryEligibility{}, fmt.Errorf("check unknown shared model usage before retry: %w", err)
	}
	if unknownSharedUsage {
		return ineligible(app.ConversationMessageRetryReasonReconciliationRequired), nil
	}
	var dispatched bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND state='dispatched')`, messageID, actorID).Scan(&dispatched); err != nil {
		return app.ConversationMessageRetryEligibility{}, fmt.Errorf("check dispatched route call before retry: %w", err)
	}
	if dispatched {
		return ineligible(app.ConversationMessageRetryReasonProcessing), nil
	}
	if sourceKind == "task_result_review" {
		var unknownReviewCall bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND state='unknown')`, messageID, actorID).Scan(&unknownReviewCall); err != nil {
			return app.ConversationMessageRetryEligibility{}, fmt.Errorf("check unknown result review call before retry: %w", err)
		}
		if unknownReviewCall {
			return ineligible(app.ConversationMessageRetryReasonReviewOutcomeUnknown), nil
		}
	}
	if state != "failed" {
		return ineligible(app.ConversationMessageRetryReasonNotFailed), nil
	}
	if taskID != nil {
		return ineligible(app.ConversationMessageRetryReasonTaskAttached), nil
	}
	if sourceKind == "task_result_review" {
		if !domain.ValidMessageID(resultReviewID) {
			return ineligible(app.ConversationMessageRetryReasonReviewNotRetryable), nil
		}
		var reviewState string
		var retryAttempt int64
		err := tx.QueryRow(ctx, `SELECT state,retry_attempt FROM conversation_result_batches
WHERE message_id=$1 AND trigger_message_id=$2`, resultReviewID, messageID).Scan(&reviewState, &retryAttempt)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && reviewState != "failed" {
			return ineligible(app.ConversationMessageRetryReasonReviewNotRetryable), nil
		}
		if err != nil {
			return app.ConversationMessageRetryEligibility{}, fmt.Errorf("check task result review retry eligibility: %w", err)
		}
		if retryAttempt == int64(^uint64(0)>>1) {
			return ineligible(app.ConversationMessageRetryReasonLimitReached), nil
		}
	} else if routeAttempt == int64(^uint64(0)>>1) {
		return ineligible(app.ConversationMessageRetryReasonLimitReached), nil
	}
	var unknownPersonalUsage bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_route_model_calls c
JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
WHERE c.message_id=$1 AND c.actor_id=$2 AND c.state='unknown' AND l.state='unknown' AND l.personal_model)`,
		messageID, actorID).Scan(&unknownPersonalUsage); err != nil {
		return app.ConversationMessageRetryEligibility{}, fmt.Errorf("check unknown personal model usage before retry: %w", err)
	}
	result := app.ConversationMessageRetryEligibility{Eligible: true, Reason: app.ConversationMessageRetryReasonAvailable}
	if unknownPersonalUsage {
		result.Warning = app.ConversationMessageRetryWarningPersonalOutcomeUnknown
	}
	return result, nil
}

func conversationRouteRetryEligibilityError(reason string) error {
	switch reason {
	case app.ConversationMessageRetryReasonMessageTooLong:
		return domain.NewError(domain.CodeInvalidState, "conversation message exceeded processing limits and cannot be retried unchanged")
	case app.ConversationMessageRetryReasonReconciliationRequired:
		return domain.NewError(domain.CodeInvalidState, "model call outcome or shared usage must be reconciled before retry")
	case app.ConversationMessageRetryReasonReviewOutcomeUnknown:
		return domain.NewError(domain.CodeConflict, "conversation route is still processing and cannot be retried")
	case app.ConversationMessageRetryReasonProcessing:
		return domain.NewError(domain.CodeConflict, "conversation route is still processing and cannot be retried")
	case app.ConversationMessageRetryReasonLimitReached:
		return domain.NewError(domain.CodeInvalidState, "conversation route retry limit has been exhausted")
	case app.ConversationMessageRetryReasonNotFailed, app.ConversationMessageRetryReasonTaskAttached,
		app.ConversationMessageRetryReasonReviewNotRetryable:
		return domain.NewError(domain.CodeConflict, "only a retryable failed message without a Task can be retried")
	default:
		return domain.NewError(domain.CodeInvalidState, "conversation route retry eligibility is unavailable")
	}
}

// GetConversationMessageRetryEligibility reports the current retry gate for a
// persisted message. RetryConversationRoute re-evaluates the same gate while
// holding its write transaction, so this result is advisory under races.
func (s *Store) GetConversationMessageRetryEligibility(ctx context.Context, principal identity.Principal,
	projectID, conversationID, messageID, instanceID string) (app.ConversationMessageRetryEligibility, error) {
	if ctx == nil || !domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) ||
		(principal.Kind != identity.KindSession && principal.Kind != identity.KindClient) || !principal.HasScope(identity.ScopeTaskSubmit) ||
		!domain.ValidConversationID(conversationID) || !domain.ValidMessageID(messageID) || !domain.ValidServiceInstanceID(instanceID) ||
		(projectID != "" && !project.ValidID(projectID)) {
		return app.ConversationMessageRetryEligibility{}, domain.InvalidArgument("route_retry", "conversation route retry identity is invalid")
	}
	var eligibility app.ConversationMessageRetryEligibility
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		if err := requireConversationServiceInstance(ctx, tx, instanceID); err != nil {
			return err
		}
		requestedMessageID := messageID
		var ownerID, duplicateOf string
		err := tx.QueryRow(ctx, `SELECT r.owner_id,COALESCE(r.duplicate_of_message_id,'') FROM conversation_message_routes r
JOIN conversations c ON c.id=r.conversation_id AND c.owner_id=r.owner_id
WHERE r.service_instance_id=$1::uuid AND r.message_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
 AND c.project_id IS NOT DISTINCT FROM NULLIF($5,'')`, instanceID, requestedMessageID, principal.UserID, conversationID, projectID).
			Scan(&ownerID, &duplicateOf)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "conversation message not found")
		}
		if err != nil {
			return fmt.Errorf("find conversation route retry eligibility: %w", err)
		}
		if duplicateOf != "" {
			messageID = duplicateOf
		}
		receipt := app.ConversationMessageReceipt{ServiceInstanceID: instanceID,
			Message: conversation.Message{ID: messageID, OwnerID: ownerID, ActorID: principal.UserID, ConversationID: conversationID},
			Input:   app.ConversationRoutingInput{ProjectID: projectID}}
		if err := lockConversationReceiptScopeInTx(ctx, tx, receipt); err != nil {
			return err
		}
		if _, _, err := authorizeConversationRouteRetryInTx(ctx, tx, principal); err != nil {
			return err
		}
		var state, failureReason, sourceKind, resultReviewID string
		var routeAttempt int64
		var taskID *string
		err = tx.QueryRow(ctx, `SELECT r.state,r.failure_reason,m.task_id,m.source_kind,
COALESCE(r.request_metadata->>'result_review_id',''),r.route_attempt
FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.actor_id=r.actor_id AND m.conversation_id=r.conversation_id AND m.owner_id=r.owner_id
WHERE r.service_instance_id=$1::uuid AND r.message_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
FOR SHARE OF r,m`, instanceID, messageID, principal.UserID, conversationID).
			Scan(&state, &failureReason, &taskID, &sourceKind, &resultReviewID, &routeAttempt)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.CodeNotFound, "conversation message not found")
		}
		if err != nil {
			return fmt.Errorf("read conversation route retry eligibility: %w", err)
		}
		currentEligibility, err := conversationRouteRetryEligibilityInTx(ctx, tx, messageID, principal.UserID,
			state, failureReason, taskID, sourceKind, resultReviewID, routeAttempt)
		if err != nil {
			return err
		}
		eligibility = currentEligibility
		return nil
	})
	if err != nil {
		return app.ConversationMessageRetryEligibility{}, err
	}
	return eligibility, nil
}
