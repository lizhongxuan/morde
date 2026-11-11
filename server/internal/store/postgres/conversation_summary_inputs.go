package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/conversation"
)

const conversationSummaryInputSourceLimit = 32

type conversationSummaryInputRow struct {
	sequence    int64
	kindOrder   int
	id          string
	sourceOrder int
	source      app.ConversationSummarySource
	role        string
	contentSize int64
	content     []byte
}

func (s *Store) LoadConversationSummaryInputPage(ctx context.Context, request app.ConversationSummaryInputPageRequest) (app.ConversationSummaryInputPage, error) {
	var page app.ConversationSummaryInputPage
	if ctx == nil {
		return page, domain.InvalidArgument("conversation_summary_source", "conversation summary input context is required")
	}
	budget, err := app.ConversationSummaryInputPageBudget(request.ContextWindowTokens)
	if err != nil {
		return page, err
	}
	boundary := request.Claim.Message.Sequence - 1
	if request.Claim.Message.Sequence < 1 {
		return page, domain.InvalidArgument("conversation_summary_source", "conversation summary input boundary is invalid")
	}
	if request.Cursor != nil && (request.Cursor.BoundarySequence != boundary || request.Cursor.Sequence < 1 ||
		request.Cursor.KindOrder < 0 || request.Cursor.KindOrder > 1 || request.Cursor.ByteOffset < 1 || request.Cursor.SummaryVersion < 0 ||
		(request.Cursor.KindOrder == 0 && (!domain.ValidMessageID(request.Cursor.ID) || request.Cursor.SourceOrder != 0)) ||
		(request.Cursor.KindOrder == 1 && (!domain.ValidTaskID(request.Cursor.ID) || request.Cursor.SourceOrder < 1)) ||
		(request.Cursor.SummaryVersion == 0 && len(request.Cursor.SummarySHA256) != 0) ||
		(request.Cursor.SummaryVersion > 0 && len(request.Cursor.SummarySHA256) != sha256.Size)) {
		return page, domain.InvalidArgument("conversation_summary_source", "conversation summary input cursor is invalid")
	}

	err = s.WithTx(ctx, func(tx pgx.Tx) error {
		identity, err := lockConversationSummaryScope(ctx, tx, request.Claim)
		if err != nil {
			return err
		}
		head, err := ensureConversationSummaryHead(ctx, tx, identity, true)
		if err != nil {
			return err
		}
		if request.Cursor != nil && request.Cursor.Identity != identity {
			return domain.NewError(domain.CodeConflict, "conversation authorization changed while reading its sources")
		}
		if request.Cursor != nil && (head.Version != request.Cursor.SummaryVersion || !bytes.Equal(head.Digest, request.Cursor.SummarySHA256)) {
			return domain.NewError(domain.CodeConflict, "conversation summary changed while reading its sources")
		}
		page.Identity, page.BoundarySequence, page.Head = identity, boundary, head
		if request.Cursor == nil && head.Version > 0 {
			stored, err := readConversationSummaryForInputPage(ctx, tx, head)
			if err != nil {
				return err
			}
			page.Summary = &stored
		}

		previousMessageID, err := conversationSummaryPredecessorMessageID(ctx, tx, request.Claim, identity)
		if err != nil {
			return err
		}
		descendants, err := conversationRouteDescendantIDs(ctx, tx, request.Claim)
		if err != nil {
			return err
		}
		if page.Summary != nil {
			if err := validateConversationSummarySources(ctx, tx, request.Claim, identity, previousMessageID, descendants,
				page.Summary.Sources); err != nil {
				return err
			}
		}
		rows, err := tx.Query(ctx, conversationSummaryInputPageSQL, identity.OwnerID, identity.ConversationID, boundary,
			identity.ActorID, identity.ServiceInstanceID, identity.ScopeID, descendants, previousMessageID, head.Version,
			budget, request.Cursor != nil, cursorSequence(request.Cursor), cursorKindOrder(request.Cursor),
			cursorID(request.Cursor), cursorSourceOrder(request.Cursor), cursorByteOffset(request.Cursor), conversationSummaryInputSourceLimit+1)
		if err != nil {
			return fmt.Errorf("read authorized conversation summary sources: %w", err)
		}
		defer rows.Close()
		var fetched []conversationSummaryInputRow
		for rows.Next() {
			var item conversationSummaryInputRow
			var kind string
			if err := rows.Scan(&item.sequence, &item.kindOrder, &item.id, &item.sourceOrder, &kind,
				&item.source.ID, &item.source.ObjectType, &item.source.Version, &item.source.Sequence,
				&item.role, &item.contentSize, &item.source.SHA256, &item.content); err != nil {
				return fmt.Errorf("scan authorized conversation summary source: %w", err)
			}
			item.source.Kind = kind
			if item.sequence < 1 || item.id == "" || item.sourceOrder < 0 || item.contentSize < 1 ||
				len(item.source.SHA256) != sha256.Size || len(item.content) == 0 || len(item.content) > budget ||
				(item.role != string(conversation.RoleUser) && item.role != string(conversation.RoleAssistant)) ||
				(item.kindOrder == 0 && (item.source.Kind != app.ConversationSummarySourceMessage ||
					!domain.ValidMessageID(item.id) || item.source.ID != item.id || item.sourceOrder != 0 || item.source.Sequence != item.sequence)) ||
				(item.kindOrder == 1 && (item.source.Kind != app.ConversationSummarySourceTask ||
					!domain.ValidTaskID(item.id) || item.source.ID != item.id || item.sourceOrder < 1)) {
				return domain.NewError(domain.CodeInvalidState, "conversation summary source page contains an invalid row")
			}
			fetched = append(fetched, item)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate authorized conversation summary sources: %w", err)
		}

		used := 0
		for index, item := range fetched {
			offset := int64(0)
			if sameSummaryInputCursor(request.Cursor, item) {
				offset = request.Cursor.ByteOffset
			}
			if offset >= item.contentSize || offset > int64(^uint(0)>>1) {
				return domain.NewError(domain.CodeInvalidArgument, "conversation summary source offset is invalid")
			}
			chunk := item.content
			if sameSummaryInputCursor(request.Cursor, item) && offset > 0 {
				chunk = item.content
			}
			if !utf8.Valid(chunk) {
				chunk = validConversationSummaryUTF8Prefix(chunk)
			}
			if len(chunk) == 0 {
				return domain.NewError(domain.CodeInvalidState, "conversation summary source chunk is not valid UTF-8")
			}
			remaining := budget - used
			if len(chunk) > remaining {
				chunk = chunk[:conversationSummaryUTF8PrefixLength(chunk, remaining)]
			}
			if len(chunk) == 0 {
				return domain.NewError(domain.CodeInvalidState, "conversation summary source page cannot fit a UTF-8 character")
			}
			if !utf8.Valid(chunk) {
				return domain.NewError(domain.CodeInvalidState, "conversation summary source chunk is not valid UTF-8")
			}
			page.Sources = append(page.Sources, app.ConversationSummaryPromptSource{Source: item.source,
				Role: item.role, Content: string(chunk), ByteOffset: offset})
			used += len(chunk)
			nextOffset := offset + int64(len(chunk))
			page.Next = &app.ConversationSummaryInputCursor{Identity: identity, BoundarySequence: boundary, SummaryVersion: head.Version,
				SummarySHA256: append([]byte(nil), head.Digest...), Sequence: item.sequence, KindOrder: item.kindOrder,
				ID: item.id, SourceOrder: item.sourceOrder, ByteOffset: nextOffset}
			if used >= budget {
				if nextOffset == item.contentSize && index == len(fetched)-1 && len(fetched) <= conversationSummaryInputSourceLimit {
					page.Next = nil
				}
				return nil
			}
			if nextOffset < item.contentSize || index == conversationSummaryInputSourceLimit-1 {
				return nil
			}
		}
		if len(fetched) <= conversationSummaryInputSourceLimit {
			page.Next = nil
		}
		return nil
	})
	return page, err
}

func conversationSummaryPredecessorMessageID(ctx context.Context, tx pgx.Tx, claim app.ConversationRouteClaim,
	identity app.ConversationSummaryIdentity) (string, error) {
	if claim.Input.PreviousClientMessageID == "" {
		return "", nil
	}
	var state, messageID string
	err := tx.QueryRow(ctx, `SELECT route.state,message.id FROM conversation_message_routes route
JOIN conversation_messages message ON message.id=route.message_id AND message.owner_id=route.owner_id
 AND message.actor_id=route.actor_id AND message.conversation_id=route.conversation_id
 AND message.role='user' AND message.delivery_status='applied'
WHERE route.service_instance_id=$1::uuid AND route.owner_id=$2 AND route.actor_id=$3 AND route.conversation_id=$4
 AND route.client_message_id=$5`, identity.ServiceInstanceID, identity.OwnerID, identity.ActorID,
		identity.ConversationID, claim.Input.PreviousClientMessageID).Scan(&state, &messageID)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && state != "routed" {
		return "", domain.NewError(domain.CodeConflict, "conversation predecessor is not ready for summary input")
	}
	if err != nil {
		return "", fmt.Errorf("read predecessor for conversation summary: %w", err)
	}
	return messageID, nil
}

func readConversationSummaryForInputPage(ctx context.Context, tx pgx.Tx, head app.ConversationSummaryHead) (app.StoredConversationSummary, error) {
	var summary app.StoredConversationSummary
	var previous *int64
	summary.Identity = head.Identity
	err := tx.QueryRow(ctx, `SELECT version,previous_version,operation_id,summary_digest,source_digest,ciphertext
FROM conversation_summary_versions WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
 AND scope_kind=$5 AND scope_id=$6 AND version=$7 AND authorization_version=$8 AND scope_version=$9 AND state='valid'`,
		head.Identity.ServiceInstanceID, head.Identity.OwnerID, head.Identity.ActorID, head.Identity.ConversationID,
		head.Identity.ScopeKind, head.Identity.ScopeID, head.Version, head.Identity.AuthorizationVersion, head.Identity.ScopeVersion).
		Scan(&summary.Version, &previous, &summary.OperationID, &summary.SummarySHA256, &summary.SourcesSHA256, &summary.Ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.StoredConversationSummary{}, domain.NewError(domain.CodeConflict, "conversation summary head changed while reading its sources")
	}
	if err != nil {
		return app.StoredConversationSummary{}, fmt.Errorf("read prior conversation summary: %w", err)
	}
	summary.Previous = previous
	if summary.Version != head.Version || !bytes.Equal(summary.SummarySHA256, head.Digest) {
		return app.StoredConversationSummary{}, domain.NewError(domain.CodeInvalidState, "conversation summary head proof is invalid")
	}
	rows, err := tx.Query(ctx, `SELECT source_kind,source_id,source_object_type,source_version,source_sequence,source_digest
FROM conversation_summary_sources WHERE service_instance_id=$1::uuid AND owner_id=$2 AND actor_id=$3 AND conversation_id=$4
 AND scope_kind=$5 AND scope_id=$6 AND summary_version=$7 ORDER BY source_order`, head.Identity.ServiceInstanceID,
		head.Identity.OwnerID, head.Identity.ActorID, head.Identity.ConversationID, head.Identity.ScopeKind, head.Identity.ScopeID, head.Version)
	if err != nil {
		return app.StoredConversationSummary{}, fmt.Errorf("read prior conversation summary source proofs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var source app.ConversationSummarySource
		if err := rows.Scan(&source.Kind, &source.ID, &source.ObjectType, &source.Version, &source.Sequence, &source.SHA256); err != nil {
			return app.StoredConversationSummary{}, fmt.Errorf("scan prior conversation summary source proof: %w", err)
		}
		summary.Sources = append(summary.Sources, source)
	}
	if err := rows.Err(); err != nil {
		return app.StoredConversationSummary{}, fmt.Errorf("iterate prior conversation summary source proofs: %w", err)
	}
	sourceDigest, err := app.ConversationSummarySourcesDigest(summary.Sources)
	if err != nil || !bytes.Equal(sourceDigest, summary.SourcesSHA256) {
		return app.StoredConversationSummary{}, domain.NewError(domain.CodeInvalidState, "prior conversation summary source proof is invalid")
	}
	return summary, nil
}

func cursorSequence(cursor *app.ConversationSummaryInputCursor) int64 {
	if cursor == nil {
		return 0
	}
	return cursor.Sequence
}

func cursorKindOrder(cursor *app.ConversationSummaryInputCursor) int {
	if cursor == nil {
		return 0
	}
	return cursor.KindOrder
}

func cursorID(cursor *app.ConversationSummaryInputCursor) string {
	if cursor == nil {
		return ""
	}
	return cursor.ID
}

func cursorSourceOrder(cursor *app.ConversationSummaryInputCursor) int {
	if cursor == nil {
		return 0
	}
	return cursor.SourceOrder
}

func cursorByteOffset(cursor *app.ConversationSummaryInputCursor) int64 {
	if cursor == nil {
		return 0
	}
	return cursor.ByteOffset
}

func sameSummaryInputCursor(cursor *app.ConversationSummaryInputCursor, row conversationSummaryInputRow) bool {
	return cursor != nil && cursor.Sequence == row.sequence && cursor.KindOrder == row.kindOrder && cursor.ID == row.id && cursor.SourceOrder == row.sourceOrder
}

func validConversationSummaryUTF8Prefix(value []byte) []byte {
	for len(value) > 0 && !utf8.Valid(value) {
		value = value[:len(value)-1]
	}
	return value
}

func conversationSummaryUTF8PrefixLength(value []byte, maxBytes int) int {
	if maxBytes > len(value) {
		maxBytes = len(value)
	}
	for maxBytes > 0 && !utf8.Valid(value[:maxBytes]) {
		maxBytes--
	}
	return maxBytes
}

const conversationSummaryInputPageSQL = `WITH source_rows AS (
  SELECT m.sequence AS sort_sequence,0 AS kind_order,m.id AS sort_id,0 AS source_order,
    'message'::text AS source_kind,m.id AS source_id,''::text AS source_object_type,0::bigint AS source_version,
    m.sequence AS source_sequence,m.role,m.content,sha256(convert_to(m.content,'UTF8')) AS source_digest
  FROM conversation_messages m
  LEFT JOIN tasks t ON t.id=m.task_id AND t.owner_id=m.owner_id AND t.conversation_id=m.conversation_id
  WHERE m.owner_id=$1 AND m.conversation_id=$2 AND (m.sequence<=$3 OR m.id=$8)
    AND m.source_kind<>'task_result_review' AND m.delivery_status='applied' AND m.id<>ALL($7::text[])
    AND NOT EXISTS (SELECT 1 FROM personal_history_suppressions suppressed
      WHERE suppressed.service_instance_id=$5::uuid AND suppressed.owner_id=m.owner_id AND suppressed.message_id=m.id)
    AND ($9=0 OR NOT EXISTS (SELECT 1 FROM conversation_summary_sources covered
      WHERE covered.service_instance_id=$5::uuid AND covered.owner_id=$1 AND covered.actor_id=$4 AND covered.conversation_id=$2
        AND covered.scope_kind=CASE WHEN $6='' THEN 'personal' ELSE 'project' END AND covered.scope_id=$6
        AND covered.summary_version=$9 AND covered.source_kind='message' AND covered.source_id=m.id
        AND covered.source_sequence=m.sequence))
    AND (
      (m.actor_id=$4 AND (m.task_id IS NULL OR t.initiator_id=$4) AND (m.role='user' OR (m.role='assistant' AND (
        (m.output_status IS NULL AND ((m.source_kind='conversation' AND m.task_id IS NULL) OR t.status='completed' OR EXISTS (
          SELECT 1 FROM conversation_result_batches batch
          WHERE batch.message_id=m.id AND batch.published_message_id=m.id AND batch.state='published'
            AND batch.service_instance_id=$5::uuid AND batch.owner_id=m.owner_id AND batch.actor_id=m.actor_id
            AND batch.conversation_id=m.conversation_id AND EXISTS (SELECT 1 FROM conversation_result_sources source
              WHERE source.batch_message_id=batch.message_id AND source.owner_id=batch.owner_id AND source.actor_id=batch.actor_id
                AND source.conversation_id=batch.conversation_id AND source.state='published')
        ))) OR (m.output_status='completed' AND t.id=m.task_id AND t.initiator_id=m.actor_id AND EXISTS (
          SELECT 1 FROM task_events result WHERE result.task_id=m.task_id AND result.sequence=m.output_result_event_sequence
            AND result.task_revision=m.output_task_revision AND result.event_type='task.result'
            AND result.result_content_sha256=sha256(convert_to(m.content,'UTF8'))
        ))))))
      OR ($6<>'' AND m.actor_id<>$4 AND m.source_kind NOT IN ('history_import','legacy_instant')
        AND EXISTS (SELECT 1 FROM conversations project_scope WHERE project_scope.id=m.conversation_id
          AND project_scope.owner_id=m.owner_id AND project_scope.project_id=$6)
        AND (` + projectSharedHistoryEvidenceSQL + `))
    )
  UNION ALL
  SELECT published.sequence AS sort_sequence,1 AS kind_order,result.task_id AS sort_id,result.source_order,
    'task_result'::text AS source_kind,result.task_id AS source_id,''::text AS source_object_type,
    result.task_revision AS source_version,result.event_sequence AS source_sequence,'assistant'::text AS role,
    result.content, result.payload_sha256 AS source_digest
  FROM conversation_result_sources result
  JOIN conversation_result_batches batch ON batch.message_id=result.batch_message_id AND batch.owner_id=result.owner_id
    AND batch.actor_id=result.actor_id AND batch.conversation_id=result.conversation_id AND batch.state='published'
    AND batch.service_instance_id=$5::uuid
  JOIN conversation_messages published ON published.id=batch.published_message_id AND published.owner_id=batch.owner_id
    AND published.actor_id=batch.actor_id AND published.conversation_id=batch.conversation_id
  JOIN conversation_messages origin ON origin.id=result.user_message_id AND origin.owner_id=result.owner_id
    AND origin.actor_id=result.actor_id AND origin.conversation_id=result.conversation_id
  JOIN tasks t ON t.id=result.task_id AND t.owner_id=result.owner_id AND t.initiator_id=result.actor_id
    AND t.conversation_id=result.conversation_id
  WHERE result.owner_id=$1 AND result.conversation_id=$2 AND published.sequence<=$3 AND result.state='published'
    AND origin.role='user' AND origin.delivery_status='applied' AND origin.source_kind<>'task_result_review'
    AND (origin.sequence<=$3 OR origin.id=$8) AND origin.id<>ALL($7::text[])
    AND published.delivery_status='applied' AND t.status='completed' AND t.current_revision=result.task_revision
    AND t.desired_revision=result.task_revision AND t.result IS NOT DISTINCT FROM result.content
    AND t.current_step IS NOT DISTINCT FROM result.turn_id AND t.auth_version=result.authorization_version
    AND result.payload_sha256=sha256(convert_to(result.content,'UTF8'))
    AND NOT EXISTS (SELECT 1 FROM personal_history_suppressions suppressed
      WHERE suppressed.service_instance_id=$5::uuid AND suppressed.owner_id=result.owner_id
        AND suppressed.message_id IN (origin.id,published.id))
    AND (($6='' AND result.actor_id=$4 AND t.project_id IS NULL) OR ($6<>'' AND t.project_id=$6 AND EXISTS (
      SELECT 1 FROM project_members member JOIN users member_user ON member_user.id=member.user_id AND member_user.status='approved'
      WHERE member.project_id=$6 AND member.user_id=result.actor_id AND member.role IN ('owner','member')
    )))
    AND ($9=0 OR NOT EXISTS (SELECT 1 FROM conversation_summary_sources covered
      WHERE covered.service_instance_id=$5::uuid AND covered.owner_id=$1 AND covered.actor_id=$4 AND covered.conversation_id=$2
        AND covered.scope_kind=CASE WHEN $6='' THEN 'personal' ELSE 'project' END AND covered.scope_id=$6
        AND covered.summary_version=$9 AND covered.source_kind='task_result' AND covered.source_id=result.task_id
        AND covered.source_version=result.task_revision AND covered.source_sequence=result.event_sequence))
)
SELECT sort_sequence,kind_order,sort_id,source_order,source_kind,source_id,source_object_type,source_version,source_sequence,
  role,octet_length(content),source_digest,
  substring(convert_to(content,'UTF8') from
    (CASE WHEN $11 AND sort_sequence=$12 AND kind_order=$13 AND sort_id=$14 AND source_order=$15 THEN $16 ELSE 0 END)+1 FOR $10)
FROM source_rows
WHERE NOT $11 OR sort_sequence>$12 OR (sort_sequence=$12 AND kind_order>$13) OR
  (sort_sequence=$12 AND kind_order=$13 AND sort_id>$14) OR
  (sort_sequence=$12 AND kind_order=$13 AND sort_id=$14 AND source_order>$15) OR
  (sort_sequence=$12 AND kind_order=$13 AND sort_id=$14 AND source_order=$15 AND $16<octet_length(content))
ORDER BY sort_sequence,kind_order,sort_id,source_order LIMIT $17`
