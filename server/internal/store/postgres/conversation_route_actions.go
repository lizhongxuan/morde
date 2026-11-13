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

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
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
	var createdAt time.Time
	var metadata, authorization []byte
	err := tx.QueryRow(ctx, `SELECT m.content,m.role,m.source_kind,m.sequence,m.created_at,r.client_message_id,r.request_metadata,r.authorization_snapshot,r.route_attempt
FROM conversation_message_routes r
JOIN service_identity i ON i.singleton=true AND i.instance_id=r.service_instance_id
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id
 AND m.conversation_id=r.conversation_id AND m.actor_id=r.actor_id
WHERE r.service_instance_id=$1::uuid AND r.message_id=$2 AND r.owner_id=$3 AND r.actor_id=$4 AND r.conversation_id=$5
 AND r.state='processing' AND r.lease_owner=$6 AND r.lease_version=$7 AND r.lease_expires_at>clock_timestamp()
FOR UPDATE OF r`, claim.ServiceInstanceID, claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID,
		claim.Message.ConversationID, claim.LeaseOwner, claim.LeaseVersion).Scan(&content, &role, &sourceKind, &sequence, &createdAt, &clientID,
		&metadata, &authorization, &routeAttempt)
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
		ReferenceTaskID: claim.Input.ReferenceTaskID,
		History:         []app.ConversationRouteHistory{}, Candidates: []app.ConversationRouteCandidate{},
		RequestCandidates: []app.ConversationRouteRequestCandidate{}}}
	scopeDigest, err := app.ConversationRouteExecutionScopeDigest(claim.Input)
	if err != nil {
		return app.ConversationRouteSnapshot{}, err
	}
	value.Context.ExecutionScopeDigest = scopeDigest
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
			value.PreviousRoutingState, value.PreviousTaskID = state, previousTaskID
			value.PreviousMessageID, value.PreviousSequence = previousMessageID, previousSequence
		}
		err = tx.QueryRow(ctx, `WITH RECURSIVE chain(client_id,previous_id,seen,cycle,depth) AS (
 SELECT r.client_message_id,r.previous_client_message_id,ARRAY[r.client_message_id],r.client_message_id=$5,1
 FROM conversation_message_routes r WHERE r.service_instance_id=$1::uuid AND r.actor_id=$2 AND r.conversation_id=$3
  AND r.client_message_id=$4 AND r.owner_id=$6
 UNION ALL
 SELECT r.client_message_id,r.previous_client_message_id,c.seen||r.client_message_id,
  r.client_message_id=ANY(c.seen) OR r.client_message_id=$5,c.depth+1
 FROM chain c JOIN conversation_message_routes r ON r.service_instance_id=$1::uuid AND r.actor_id=$2
  AND r.conversation_id=$3 AND r.owner_id=$6 AND r.client_message_id=c.previous_id
 WHERE NOT c.cycle AND c.depth<128
)
SELECT COALESCE(bool_or(cycle OR (depth=128 AND previous_id IS NOT NULL)),false) FROM chain`,
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
COALESCE(m.output_result_event_sequence,0),m.role,m.content FROM conversation_messages m
LEFT JOIN tasks t ON t.id=m.task_id AND t.owner_id=m.owner_id AND t.conversation_id=m.conversation_id
WHERE m.owner_id=$1 AND m.conversation_id=$2 AND m.sequence<$3 AND m.source_kind<>'task_result_review'
 AND octet_length(m.content)<=4096 AND m.id<>ALL($7::text[])
 AND (
   (m.actor_id=$4 AND (m.task_id IS NULL OR t.initiator_id=$4)
    AND (m.role='user' OR (m.role='assistant' AND (
      (m.output_status IS NULL AND (t.status='completed' OR EXISTS (
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
		claim.ServiceInstanceID, claim.Input.ProjectID, descendants)
	if err != nil {
		return app.ConversationRouteSnapshot{}, fmt.Errorf("read route history: %w", err)
	}
	for rows.Next() {
		var item app.ConversationRouteHistory
		var role string
		if err := rows.Scan(&item.MessageID, &item.Sequence, &item.TaskID, &item.TaskRevision, &item.EventSequence, &role, &item.Content); err != nil {
			rows.Close()
			return app.ConversationRouteSnapshot{}, fmt.Errorf("scan route history: %w", err)
		}
		item.Role = conversation.Role(role)
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
COALESCE(m.output_result_event_sequence,0),m.role,m.content FROM conversation_messages m
WHERE m.owner_id=$1 AND m.conversation_id=$2 AND m.sequence<$3 AND m.role='assistant'
 AND m.source_kind NOT IN ('history_import','legacy_instant') AND m.delivery_status='applied'
 AND octet_length(m.content)<=4096 AND m.id<>ALL($7::text[])
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
		claim.Message.ActorID, claim.ServiceInstanceID, claim.Input.ProjectID, descendants)
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
		if err := resultRows.Scan(&item.MessageID, &item.Sequence, &item.TaskID, &item.TaskRevision, &item.EventSequence, &role, &item.Content); err != nil {
			resultRows.Close()
			return app.ConversationRouteSnapshot{}, fmt.Errorf("scan recent published Task result: %w", err)
		}
		publishedResultIDs = append(publishedResultIDs, item.MessageID)
		if _, ok := seenHistory[item.MessageID]; ok {
			continue
		}
		item.Role = conversation.Role(role)
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
		sourceRows, err := tx.Query(ctx, `SELECT DISTINCT origin.id,origin.sequence,source.task_id,origin.content
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
 AND octet_length(origin.content)<=4096 AND origin.id<>ALL($6::text[])
 AND (origin.id=source.user_message_id OR origin.sequence=(
   SELECT min(initial.sequence) FROM conversation_messages initial
   WHERE initial.task_id=source.task_id AND initial.owner_id=source.owner_id
    AND initial.actor_id=source.actor_id AND initial.conversation_id=source.conversation_id
    AND initial.role='user' AND initial.delivery_status='applied'
    AND initial.source_kind NOT IN ('history_import','legacy_instant')))
ORDER BY origin.sequence`, publishedResultIDs, claim.Message.OwnerID, claim.Message.ActorID,
			claim.Message.ConversationID, claim.ServiceInstanceID, descendants)
		if err != nil {
			return app.ConversationRouteSnapshot{}, fmt.Errorf("read published Task source requests: %w", err)
		}
		for sourceRows.Next() {
			var item app.ConversationRouteHistory
			if err := sourceRows.Scan(&item.MessageID, &item.Sequence, &item.TaskID, &item.Content); err != nil {
				sourceRows.Close()
				return app.ConversationRouteSnapshot{}, fmt.Errorf("scan published Task source request: %w", err)
			}
			if _, ok := seenHistory[item.MessageID]; ok {
				continue
			}
			item.Role = conversation.RoleUser
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
COALESCE(output_result_event_sequence,0),role,content FROM conversation_messages
WHERE id=$1 AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4 AND role='user'`,
			value.PreviousMessageID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID).
			Scan(&item.MessageID, &item.Sequence, &item.TaskID, &item.TaskRevision, &item.EventSequence, &role, &item.Content)
		if err == nil {
			item.Role = conversation.Role(role)
			value.Context.History = append(value.Context.History, item)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return app.ConversationRouteSnapshot{}, fmt.Errorf("read late predecessor: %w", err)
		}
	}
	query := `SELECT id,initiator_id,objective,status,desired_revision FROM tasks
WHERE owner_id=$1 AND conversation_id=$2 AND initiator_id=$3
 AND status IN ('queued','running','waiting_user','pausing','paused','cancelling')
 AND waiting_reason<>'project_recovery_unavailable'
 AND NOT EXISTS (SELECT 1 FROM conversation_messages future WHERE future.task_id=tasks.id AND future.id=ANY($7::text[]))
 AND (created_at<=$4 OR EXISTS (SELECT 1 FROM conversation_messages origin
   WHERE origin.task_id=tasks.id AND origin.owner_id=tasks.owner_id AND origin.conversation_id=tasks.conversation_id
     AND origin.actor_id=tasks.initiator_id AND origin.role='user' AND (origin.sequence<$5 OR origin.id=$6)))
ORDER BY id LIMIT 65`
	if lockTasks {
		query += ` FOR UPDATE`
	}
	rows, err = tx.Query(ctx, query, claim.Message.OwnerID, claim.Message.ConversationID, claim.Message.ActorID,
		claim.Message.CreatedAt, claim.Message.Sequence, value.PreviousMessageID, descendants)
	if err != nil {
		return app.ConversationRouteSnapshot{}, fmt.Errorf("read route task candidates: %w", err)
	}
	for rows.Next() {
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
	if len(value.Context.Candidates) > 64 {
		return app.ConversationRouteSnapshot{}, domain.NewError(domain.CodeInvalidState, "too many active tasks to route safely")
	}
	value.Context.Content, err = joinedConversationRouteContent(ctx, tx, claim)
	if err != nil {
		return app.ConversationRouteSnapshot{}, err
	}
	requestRows, err := tx.Query(ctx, `SELECT r.message_id,m.sequence,m.content,r.state,COALESCE(m.task_id,''),
COALESCE(t.status,''),EXISTS(SELECT 1 FROM conversation_route_model_calls call
 WHERE call.message_id=r.message_id AND call.actor_id=r.actor_id AND call.phase='normal'
  AND call.attempt=r.route_attempt AND call.state='settled' AND call.decision->>'Action'='reply'),r.request_metadata
FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.actor_id=r.actor_id
 AND m.conversation_id=r.conversation_id
LEFT JOIN tasks t ON t.id=m.task_id AND t.owner_id=m.owner_id AND t.conversation_id=m.conversation_id
WHERE r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
 AND r.message_id<>$5 AND r.duplicate_of_message_id IS NULL
 AND (m.sequence<$6 OR r.client_message_id=$7 OR r.message_id=(
   SELECT prior.duplicate_of_message_id FROM conversation_message_routes prior
   WHERE prior.service_instance_id=$1::uuid AND prior.owner_id=$2 AND prior.actor_id=$3
    AND prior.conversation_id=$4 AND prior.client_message_id=$7))
 AND (r.state IN ('pending','processing','waiting_user','waiting_adjustment','failed')
   OR t.status IN ('queued','running','waiting_user','pausing','paused','cancelling')
   OR (r.state='routed' AND m.created_at >= $8::timestamptz-interval '30 minutes'))
 AND $9=''
ORDER BY CASE WHEN r.state IN ('pending','processing','waiting_user','waiting_adjustment','failed')
 OR t.status IN ('queued','running','waiting_user','pausing','paused','cancelling') THEN 0 ELSE 1 END,
		m.sequence DESC LIMIT 101`, claim.ServiceInstanceID, claim.Message.OwnerID, claim.Message.ActorID,
		claim.Message.ConversationID, claim.Message.ID, claim.Message.Sequence, claim.Input.PreviousClientMessageID,
		claim.Message.CreatedAt, claim.Input.ResultReviewID)
	if err != nil {
		return app.ConversationRouteSnapshot{}, fmt.Errorf("read prior conversation route candidates: %w", err)
	}
	for requestRows.Next() {
		var candidate app.ConversationRouteRequestCandidate
		var routeStatus, taskStatus string
		var directReply bool
		var metadata []byte
		if err := requestRows.Scan(&candidate.MessageID, &candidate.Sequence, &candidate.Content, &routeStatus,
			&candidate.TaskID, &taskStatus, &directReply, &metadata); err != nil {
			requestRows.Close()
			return app.ConversationRouteSnapshot{}, fmt.Errorf("scan prior conversation route candidate: %w", err)
		}
		if len(value.Context.RequestCandidates) == 100 {
			requestRows.Close()
			return app.ConversationRouteSnapshot{}, domain.NewError(domain.CodeInvalidState, "too many prior requests to route safely")
		}
		if len(candidate.Content) > 4096 {
			requestRows.Close()
			return app.ConversationRouteSnapshot{}, domain.NewError(domain.CodeResultTooLarge, "prior request exceeds routing context limit")
		}
		candidate.RoutingStatus = conversation.RoutingStatus(routeStatus)
		candidate.DirectReply = candidate.TaskID == "" && directReply
		if candidate.TaskID != "" {
			candidate.TaskStatus = task.Status(taskStatus)
		}
		if err := json.Unmarshal(metadata, &candidate.RequestInput); err != nil {
			requestRows.Close()
			return app.ConversationRouteSnapshot{}, fmt.Errorf("decode prior route candidate input: %w", err)
		}
		candidate.ExecutionScopeDigest, err = app.ConversationRouteExecutionScopeDigest(candidate.RequestInput)
		if err != nil {
			requestRows.Close()
			return app.ConversationRouteSnapshot{}, err
		}
		candidate.SameExecutionScope = bytes.Equal(candidate.ExecutionScopeDigest, value.Context.ExecutionScopeDigest)
		value.Context.RequestCandidates = append(value.Context.RequestCandidates, candidate)
	}
	err = requestRows.Err()
	requestRows.Close()
	if err != nil {
		return app.ConversationRouteSnapshot{}, fmt.Errorf("read prior conversation route candidates: %w", err)
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
	return s.WithTx(ctx, func(tx pgx.Tx) error {
		if decision.Action == app.ConversationRouteNew {
			if err := lockConversationRouteNewScope(ctx, tx, claim); err != nil {
				return err
			}
		}
		if err := lockConversationRouteClaim(ctx, tx, claim); err != nil {
			return err
		}
		current, err := loadConversationRouteSnapshotInTx(ctx, tx, claim, true)
		if err != nil {
			return err
		}
		if current.ResultReviewSuperseded {
			return finishConversationRoute(ctx, tx, claim, "routed", "", "", true)
		}
		if !app.ValidConversationRouteModelOutput(decision.ModelOutput) {
			return domain.InvalidArgument("route_decision", "conversation route model output is invalid")
		}
		if decision.Action == app.ConversationRouteReply {
			if decision.TaskID != "" || decision.ExpectedRevision != 0 || decision.Question != "" ||
				!app.ValidConversationRouteReply(decision.Reply) {
				return domain.InvalidArgument("route_decision", "direct conversation reply is invalid")
			}
			decision.Reply = strings.TrimSpace(decision.Reply)
		} else if decision.Reply != "" {
			return domain.InvalidArgument("route_decision", "reply text is only valid for a direct reply")
		}
		modelID := snapshot.Context.ModelID
		snapshot.Context.ModelID = ""
		if !reflect.DeepEqual(snapshot, current) {
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
		if (decision.Action == app.ConversationRouteNew || decision.Action == app.ConversationRouteReply) &&
			app.ConversationRouteHasExplicitControlIntent(current.Context.Content) {
			if current.Context.ReferenceUnavailable {
				decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: app.UnavailableRouteReferenceMessage}
			} else if current.PreviousMissing {
				decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: "前一条消息尚未确认，请先处理或重试。"}
			}
		}
		if current.PreviousMissing && decision.Action == app.ConversationRouteReply {
			decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify,
				Question: "前一条消息尚未确认，请先处理或重试。", ModelOutput: decision.ModelOutput}
		}
		if current.PreviousMissing && decision.Action == app.ConversationRouteNew {
			var independent bool
			err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND phase='missing_predecessor' AND state='settled' AND model_id=$3
 AND decision->>'Action'='new' AND COALESCE(decision->>'TaskID','')='')`, claim.Message.ID, claim.Message.ActorID, modelID).Scan(&independent)
			if err != nil || !independent || strings.TrimSpace(claim.Message.Content) == "" {
				return domain.NewError(domain.CodeConflict, "independent conversation request was not verified")
			}
		} else if current.PreviousMissing && decision.Action != app.ConversationRouteClarify {
			return domain.NewError(domain.CodeConflict, "previous conversation message is unavailable")
		}
		if current.Context.ResultReview != nil && decision.Action != app.ConversationRouteReply {
			return domain.NewError(domain.CodeInvalidState, "task result review must produce a reply")
		}
		var candidate *app.ConversationRouteCandidate
		for index := range current.Context.Candidates {
			item := &current.Context.Candidates[index]
			if item.TaskID == decision.TaskID && item.InitiatorID == claim.Message.ActorID && item.Revision == decision.ExpectedRevision {
				candidate = item
				break
			}
		}
		if decision.Action == app.ConversationRouteUpdate || decision.Action == app.ConversationRouteStatus || decision.Action == app.ConversationRouteStop {
			if candidate == nil || (claim.Input.ReferenceTaskID != "" && claim.Input.ReferenceTaskID != decision.TaskID) {
				question := "无法确认目标任务，请说明要处理哪个任务。"
				if current.Context.ReferenceUnavailable {
					question = app.UnavailableRouteReferenceMessage
				}
				decision = app.ConversationRouteDecision{Action: app.ConversationRouteClarify, Question: question}
			}
		}
		switch decision.Action {
		case app.ConversationRouteJoin:
			return linkDuplicateConversationRouteInTx(ctx, tx, claim, current, decision.TargetMessageID)
		case app.ConversationRouteNew:
			if !decision.ExplicitNew {
				if duplicate := exactConversationRouteDuplicate(current.Context.RequestCandidates, claim); duplicate != "" {
					return linkDuplicateConversationRouteInTx(ctx, tx, claim, current, duplicate)
				}
			}
			return createConversationRouteTask(ctx, tx, claim, current.Context.Content, modelID, decision.DependencyTaskIDs,
				current.Context.Candidates, current.Summary)
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
			return finishConversationRoute(ctx, tx, claim, "routed", decision.Reply, "", true)
		case app.ConversationRouteUpdate:
			if err := requireConversationRouteScope(ctx, tx, claim, identity.ScopeTaskControl); err != nil {
				return err
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
				return finishConversationRoute(ctx, tx, claim, "waiting_user", "前一条消息尚未确认，请说明这次要修改哪个任务。", "", false)
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
			if _, err := tx.Exec(ctx, `UPDATE task_mcp_connector_operations SET cancel_requested=true,updated_at=clock_timestamp()
WHERE task_id=$1 AND status='dispatched' AND cancel_requested=false`, candidate.TaskID); err != nil {
				return fmt.Errorf("fence task operations before stop: %w", err)
			}
			if err := requestTaskCancellationForAuthorization(ctx, tx, candidate.TaskID, claim.Message.OwnerID); err != nil {
				return err
			}
			return finishConversationRoute(ctx, tx, claim, "routed", "已请求停止该任务，停止完成后会更新状态。", candidate.TaskID, true)
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

func requeueConversationRouteForReclassification(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) error {
	const maxAutomaticConflicts = 3
	result, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET
state=CASE WHEN auto_retry_count+1 >= $9 THEN 'failed' ELSE 'pending' END,
failure_reason=CASE WHEN auto_retry_count+1 >= $9 THEN 'routing_context_unavailable' ELSE '' END,
route_attempt=CASE WHEN auto_retry_count+1 >= $9 THEN route_attempt ELSE route_attempt+1 END,
auto_retry_count=LEAST(auto_retry_count+1,$9),lease_owner='',lease_expires_at=NULL,
available_after=clock_timestamp(),updated_at=clock_timestamp()
WHERE service_instance_id=$1::uuid AND message_id=$2 AND owner_id=$3 AND actor_id=$4 AND conversation_id=$5
 AND state='processing' AND lease_owner=$6 AND lease_version=$7 AND route_attempt=$8 AND lease_expires_at>clock_timestamp()`,
		claim.ServiceInstanceID, claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID,
		claim.LeaseOwner, claim.LeaseVersion, claim.RouteAttempt, maxAutomaticConflicts)
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
	content := strings.TrimSpace(claim.Message.Content)
	var target string
	var targetSequence int64
	for _, candidate := range candidates {
		if candidate.SameExecutionScope && strings.TrimSpace(candidate.Content) == content &&
			(target == "" || candidate.Sequence < targetSequence) &&
			app.ValidConversationRouteJoinTarget(candidate.MessageID, candidates) {
			target, targetSequence = candidate.MessageID, candidate.Sequence
		}
	}
	return target
}

func joinedConversationRouteContent(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) (string, error) {
	var rootMetadata []byte
	err := tx.QueryRow(ctx, `SELECT r.request_metadata FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.actor_id=r.actor_id
 AND m.conversation_id=r.conversation_id
WHERE r.service_instance_id=$1::uuid AND r.message_id=$2 AND r.owner_id=$3 AND r.actor_id=$4
 AND r.conversation_id=$5 AND r.duplicate_of_message_id IS NULL AND m.task_id IS NULL`,
		claim.ServiceInstanceID, claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID).Scan(&rootMetadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return claim.Message.Content, nil
	}
	if err != nil {
		return "", fmt.Errorf("read conversation root before merging joined input: %w", err)
	}
	var rootInput app.ConversationRoutingInput
	if err := json.Unmarshal(rootMetadata, &rootInput); err != nil {
		return "", fmt.Errorf("decode conversation root input before merging joined input: %w", err)
	}
	rootDigest, err := app.ConversationRouteExecutionScopeDigest(rootInput)
	if err != nil {
		return "", err
	}
	currentDigest, err := app.ConversationRouteExecutionScopeDigest(claim.Input)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(rootDigest, currentDigest) {
		return "", domain.NewError(domain.CodeConflict, "conversation root execution scope changed")
	}
	rows, err := tx.Query(ctx, `SELECT m.content,r.request_metadata FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.actor_id=r.actor_id
 AND m.conversation_id=r.conversation_id
WHERE r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
 AND r.duplicate_of_message_id=$5 AND r.state='routed' AND m.role='user' AND m.task_id IS NULL
ORDER BY m.sequence,m.id`, claim.ServiceInstanceID, claim.Message.OwnerID, claim.Message.ActorID,
		claim.Message.ConversationID, claim.Message.ID)
	if err != nil {
		return "", fmt.Errorf("read joined conversation inputs: %w", err)
	}
	defer rows.Close()
	contents := []string{claim.Message.Content}
	seen := map[string]struct{}{strings.TrimSpace(claim.Message.Content): {}}
	totalBytes := len(claim.Message.Content)
	for rows.Next() {
		var content string
		var metadata []byte
		if err := rows.Scan(&content, &metadata); err != nil {
			return "", fmt.Errorf("scan joined conversation input: %w", err)
		}
		var input app.ConversationRoutingInput
		if err := json.Unmarshal(metadata, &input); err != nil {
			return "", fmt.Errorf("decode joined conversation input: %w", err)
		}
		digest, err := app.ConversationRouteExecutionScopeDigest(input)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(digest, currentDigest) {
			return "", domain.NewError(domain.CodeConflict, "joined conversation input changed execution scope")
		}
		key := strings.TrimSpace(content)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		contents = append(contents, content)
		totalBytes += len(content) + len("\n\n--- 用户补充 ---\n\n")
		if totalBytes > 32768 {
			return "", domain.NewError(domain.CodeResultTooLarge, "joined conversation input exceeds the routing limit")
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read joined conversation inputs: %w", err)
	}
	if len(contents) == 1 {
		return claim.Message.Content, nil
	}
	var merged strings.Builder
	merged.WriteString("用户为同一项工作提交了以下消息，请按顺序作为一个请求处理：\n\n")
	for index, content := range contents {
		if index > 0 {
			merged.WriteString("\n\n--- 用户补充 ---\n\n")
		}
		merged.WriteString(content)
	}
	if merged.Len() > 32768 {
		return "", domain.NewError(domain.CodeResultTooLarge, "joined conversation input exceeds the routing limit")
	}
	return merged.String(), nil
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
	if candidate == nil || !app.ValidConversationRouteJoinTarget(targetMessageID, snapshot.Context.RequestCandidates) {
		return domain.NewError(domain.CodeConflict, "conversation join target is no longer authorized")
	}
	var rootSequence int64
	var rootContent, rootClientID, rootState, rootTaskID, rootDelivery, rootSourceKind string
	var rootMetadata []byte
	var rootAttempt int64
	err := tx.QueryRow(ctx, `SELECT m.sequence,m.content,r.client_message_id,r.state,COALESCE(m.task_id,''),m.delivery_status,
r.request_metadata,r.route_attempt,m.source_kind
FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.actor_id=r.actor_id
 AND m.conversation_id=r.conversation_id
WHERE r.service_instance_id=$1::uuid AND r.owner_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
 AND r.message_id=$5 AND r.duplicate_of_message_id IS NULL
FOR UPDATE OF r,m`, claim.ServiceInstanceID, claim.Message.OwnerID, claim.Message.ActorID,
		claim.Message.ConversationID, targetMessageID).Scan(&rootSequence, &rootContent, &rootClientID, &rootState,
		&rootTaskID, &rootDelivery, &rootMetadata, &rootAttempt, &rootSourceKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.NewError(domain.CodeConflict, "conversation join target changed")
	}
	if err != nil {
		return fmt.Errorf("lock conversation join target: %w", err)
	}
	var rootInput app.ConversationRoutingInput
	if json.Unmarshal(rootMetadata, &rootInput) != nil || rootSequence != candidate.Sequence || rootContent != candidate.Content ||
		rootState != string(candidate.RoutingStatus) || rootTaskID != candidate.TaskID ||
		!reflect.DeepEqual(rootInput, candidate.RequestInput) {
		return domain.NewError(domain.CodeConflict, "conversation join target changed")
	}
	rootDigest, err := app.ConversationRouteExecutionScopeDigest(rootInput)
	if err != nil || !bytes.Equal(rootDigest, snapshot.Context.ExecutionScopeDigest) {
		return domain.NewError(domain.CodeConflict, "conversation join execution scope changed")
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
			return finishConversationRoute(ctx, tx, claim, "waiting_user", "前一条消息尚未确认，请说明这次要修改哪个任务。", "", false)
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
	dependencyTaskIDs []string, candidates []app.ConversationRouteCandidate, storedSummary *app.StoredConversationSummary) error {
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
	if strings.TrimSpace(objective) == "" {
		objective = app.ConversationAttachmentOnlyObjective
	}
	fileIntent := task.FileDeliveryNone
	if directive, specified := app.RoutedFileDeliveryDirective(claim.Message.Content, claim.Input.WorkspaceID != ""); specified {
		fileIntent = directive
	}
	value := task.Task{ID: taskID, OwnerID: claim.Message.OwnerID, InitiatorID: claim.Message.ActorID,
		ProjectID: claim.Input.ProjectID, ConversationID: claim.Message.ConversationID, ModelID: modelID,
		Objective: objective, Status: task.StatusQueued, Authorization: claim.Authorization, Budget: claim.Input.Budget,
		ExecutionLocation: "server", FileDeliveryIntent: fileIntent, CheckpointThroughSequence: -1}
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
	ids := make([]string, 0, len(contextValue.Messages))
	for _, message := range contextValue.Messages {
		ids = append(ids, message.ID)
	}
	manifest := taskConversationContextManifest{OwnerID: value.OwnerID, ProjectID: value.ProjectID, InitiatorID: value.InitiatorID,
		ConversationID: value.ConversationID, Scope: contextValue.Scope, ThroughSequence: through,
		MessageCount: len(ids), MessageIDs: ids, DigestSHA256: hex.EncodeToString(contextValue.Digest), Summary: contextValue.Summary}
	payload, err := attachTaskConversationContextManifest([]byte(`{"status":"queued"}`), manifest)
	if err != nil {
		return err
	}
	if err := insertTask(ctx, tx, value, 1, gate.epoch); err != nil {
		return err
	}
	for _, dependencyTaskID := range dependencyTaskIDs {
		var requiredRevision int64
		err := tx.QueryRow(ctx, `SELECT desired_revision FROM tasks
WHERE id=$1 AND owner_id=$2 AND conversation_id=$3 AND initiator_id=$4
 AND status IN ('queued','running','waiting_user','pausing','paused','cancelling')
 AND waiting_reason<>'project_recovery_unavailable' FOR UPDATE`, dependencyTaskID, value.OwnerID, value.ConversationID, value.InitiatorID).
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
	if err := insertTaskEvent(ctx, tx, task.Event{TaskID: value.ID, Sequence: 1, Type: "task.status", Payload: payload}); err != nil {
		return err
	}
	var reclassifyingDirectReply bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((request_metadata->>'reclassify_direct_reply')::boolean,false)
FROM conversation_message_routes WHERE message_id=$1 AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4`,
		claim.Message.ID, claim.Message.OwnerID, claim.Message.ActorID, claim.Message.ConversationID).Scan(&reclassifyingDirectReply); err != nil {
		return fmt.Errorf("read direct reply reclassification state: %w", err)
	}
	return finishConversationRoute(ctx, tx, claim, "routed", "", taskID,
		len(claim.Input.ClientAttachmentIDs) == 0 && !reclassifyingDirectReply)
}

func validateInitialRouteAttachments(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim) error {
	if len(claim.Input.ClientAttachmentIDs) == 0 {
		return nil
	}
	var total, matching, distinctAttachments int
	if claim.Input.ProjectID != "" {
		err := tx.QueryRow(ctx, `SELECT count(*),
 count(*) FILTER (WHERE client_attachment_id=ANY($7::text[]) AND state='committed'),count(DISTINCT attachment_id)
FROM project_conversation_attachments
WHERE message_id=$1 AND service_instance_id=$2::uuid AND owner_id=$3 AND actor_id=$4
 AND conversation_id=$5 AND project_id=$6`, claim.Message.ID, claim.ServiceInstanceID,
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
		claim.Message.ID, claim.ServiceInstanceID, claim.Message.ActorID, claim.Input.ClientAttachmentIDs).Scan(&total, &matching, &distinctAttachments)
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
		if err := insertMessage(ctx, tx, conversation.Message{ID: messageID, OwnerID: claim.Message.OwnerID,
			ActorID: claim.Message.ActorID, ConversationID: claim.Message.ConversationID, Role: conversation.RoleAssistant,
			Content: reply, SourceKind: "conversation", DeliveryStatus: conversation.DeliveryApplied}); err != nil {
			return err
		}
		if err := recordAccountObjectChanges(ctx, tx, accountObjectChange{OwnerID: claim.Message.OwnerID,
			ObjectType: "message", ObjectID: messageID, Operation: "upsert", MutationID: claim.Message.ClientMessageID}); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE conversation_message_routes SET state=$2,lease_owner='',lease_expires_at=NULL,
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
		var active bool
		var expiresAt time.Time
		var tokenScopes []string
		err = tx.QueryRow(ctx, `SELECT u.status='approved' AND t.revoked_at IS NULL AND t.expires_at>clock_timestamp()
 AND t.kind=$3 AND COALESCE(t.client_installation_id,'')=$4,t.expires_at,t.scopes
FROM users u JOIN tokens t ON t.owner_id=u.id WHERE u.id=$1 AND t.id=$2 FOR SHARE OF u,t`,
			principal.UserID, principal.TokenID, string(principal.Kind), principal.ClientInstallationID).Scan(&active, &expiresAt, &tokenScopes)
		if err != nil || !active {
			return domain.NewError(domain.CodeUnauthenticated, "conversation route retry credential is unavailable")
		}
		if principal.Kind == identity.KindClient && !domain.ValidClientInstallationID(principal.ClientInstallationID) {
			return domain.NewError(domain.CodeUnauthenticated, "conversation route retry installation is unavailable")
		}
		if principal.ClientInstallationID != "" {
			if err := checkModelInstallation(ctx, tx, principal.ClientInstallationID, principal.UserID); err != nil {
				return err
			}
		}
		current := identity.Principal{Kind: principal.Kind}
		for _, value := range tokenScopes {
			current.Scopes = append(current.Scopes, identity.Scope(value))
		}
		if !current.HasScope(identity.ScopeTaskSubmit) {
			return domain.NewError(domain.CodeForbidden, "conversation route retry is not authorized")
		}
		var requestHash []byte
		var state, sourceKind, resultReviewID string
		var routeAttempt int64
		var taskID *string
		err = tx.QueryRow(ctx, `SELECT r.request_hash,r.state,m.task_id,m.source_kind,COALESCE(r.request_metadata->>'result_review_id',''),r.route_attempt FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.actor_id=r.actor_id AND m.conversation_id=r.conversation_id AND m.owner_id=r.owner_id
WHERE r.service_instance_id=$1::uuid AND r.message_id=$2 AND r.actor_id=$3 AND r.conversation_id=$4
FOR UPDATE OF r,m`, instanceID, messageID, principal.UserID, conversationID).Scan(&requestHash, &state, &taskID, &sourceKind, &resultReviewID, &routeAttempt)
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
		var unknownSharedUsage bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_route_model_calls c
JOIN model_call_ledger l ON l.owner_id=c.actor_id AND l.source=c.source AND l.call_id=c.call_id
WHERE c.message_id=$1 AND c.actor_id=$2 AND c.state='unknown' AND l.state='unknown' AND NOT l.personal_model)`,
			messageID, principal.UserID).Scan(&unknownSharedUsage); err != nil {
			return fmt.Errorf("check unknown shared model usage before retry: %w", err)
		}
		if unknownSharedUsage {
			return domain.NewError(domain.CodeInvalidState, "shared model usage is unknown and must be reconciled before retry")
		}
		var unresolved bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_route_model_calls
WHERE message_id=$1 AND actor_id=$2 AND (state='dispatched' OR (state='unknown' AND $3::boolean)))`,
			messageID, principal.UserID, sourceKind == "task_result_review").Scan(&unresolved); err != nil {
			return fmt.Errorf("check unresolved route call before retry: %w", err)
		}
		if unresolved {
			return domain.NewError(domain.CodeConflict, "conversation route is still processing and cannot be retried")
		}
		if state != "failed" || taskID != nil {
			return domain.NewError(domain.CodeConflict, "only a failed message without a Task can be retried")
		}
		auth := task.Authorization{Kind: string(principal.Kind), TokenID: principal.TokenID,
			ClientInstallationID: principal.ClientInstallationID, Version: 1, ExpiresAt: &expiresAt}
		for _, scope := range principal.Scopes {
			auth.Scopes = append(auth.Scopes, string(scope))
		}
		if sourceKind == "task_result_review" {
			if !domain.ValidMessageID(resultReviewID) {
				return domain.NewError(domain.CodeConflict, "task result review route is no longer retryable")
			}
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
		if sourceKind != "task_result_review" && routeAttempt == int64(^uint64(0)>>1) {
			return domain.NewError(domain.CodeInvalidState, "conversation route retry limit has been exhausted")
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
