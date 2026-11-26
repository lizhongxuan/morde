package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

type agentMessageDeliveryReceipt struct {
	Source           app.AgentMessageSourceReference
	AttachmentIDsSHA []byte
}

func applyAgentMessageDeliveryReceipt(message *app.AgentConversationMessage, receipt agentMessageDeliveryReceipt) {
	if message == nil {
		return
	}
	envelope := receipt.Source.Envelope
	message.SourceSequence = envelope.MessageSequence
	message.SourceContentBytes = envelope.ContentBytes
	message.SourceSHA256 = mustDecodeAgentMessageDigest(envelope.SourceSHA256)
	message.ManifestVersion = envelope.ManifestVersion
	message.ManifestSHA256 = mustDecodeAgentMessageDigest(envelope.ManifestSHA256)
	message.RepresentationKind = receipt.Source.RepresentationKind
	message.RepresentationVersion = receipt.Source.RepresentationVersion
	message.RepresentationSHA256 = append([]byte(nil), receipt.Source.RepresentationSHA256...)
	message.SourceAgentGeneration = envelope.AgentGeneration
	message.TaskRevision = envelope.TaskRevision
}

// buildAgentMessageReference builds the canonical, immutable source metadata
// from the message row locked by the caller. Its page boundaries match the
// bounded message source reader used by Cloud DSH.
func buildAgentMessageReference(fence app.AgentCheckpointFence, conversationID, messageID string,
	messageSequence, taskRevision int64, mode app.AgentMessageDeliveryMode, attachmentIDs []string,
	content string) (app.AgentMessageReferenceEnvelope, []byte, error) {
	if fence.AgentRootSessionID == "" {
		fence.AgentRootSessionID = fence.AgentSessionID
	}
	if !domain.ValidMessageID(messageID) || !domain.ValidConversationID(conversationID) || messageSequence < 1 || taskRevision < 0 ||
		(mode != app.AgentMessageDeliverySteer && mode != app.AgentMessageDeliveryFollowup) ||
		len(content) > app.MaxConversationMessageBytes || len(content) == 0 && len(attachmentIDs) == 0 ||
		!utf8.ValidString(content) || strings.ContainsRune(content, '\x00') {
		return app.AgentMessageReferenceEnvelope{}, nil, domain.NewError(domain.CodeInvalidState, "message source is invalid")
	}
	if attachmentIDs == nil {
		attachmentIDs = []string{}
	}
	attachmentJSON, err := json.Marshal(attachmentIDs)
	if err != nil {
		return app.AgentMessageReferenceEnvelope{}, nil, fmt.Errorf("encode message source attachment identity: %w", err)
	}
	attachmentDigest := sha256.Sum256(attachmentJSON)
	contentDigest := sha256.Sum256([]byte(content))
	pages := agentMessageSourcePages(content)
	pageDigests := make([]app.AgentMessageSourcePage, len(pages))
	for index, page := range pages {
		pageDigests[index] = app.AgentMessageSourcePage{RuneOffset: page.RuneOffset,
			NextRuneOffset: page.NextRuneOffset, SHA256: hex.EncodeToString(page.SHA256)}
	}
	manifestDigest, err := app.ComputeAgentMessageManifestSHA256(messageID, messageSequence, int64(len(content)),
		contentDigest[:], int64(utf8.RuneCountInString(content)), pageDigests)
	if err != nil {
		return app.AgentMessageReferenceEnvelope{}, nil, err
	}
	envelope := app.AgentMessageReferenceEnvelope{
		Version: app.AgentMessageReferenceEnvelopeVersion, TaskID: fence.TaskID, TaskRevision: taskRevision,
		OwnerID: fence.OwnerID, ConversationID: conversationID, MessageID: messageID,
		MessageSequence: messageSequence, AuthorizationVersion: fence.AuthorizationVersion,
		AgentGeneration: fence.AgentGeneration, RootSessionID: fence.AgentRootSessionID, Mode: mode,
		AttachmentIDs: append([]string{}, attachmentIDs...), ContentBytes: int64(len(content)),
		SourceSHA256: hex.EncodeToString(contentDigest[:]), ManifestVersion: app.AgentMessageManifestVersion,
		ManifestSHA256: hex.EncodeToString(manifestDigest), TotalRunes: int64(utf8.RuneCountInString(content)), Pages: pageDigests,
	}
	return envelope, append([]byte(nil), attachmentDigest[:]...), nil
}

type agentMessageSourcePage struct {
	RuneOffset     int64
	NextRuneOffset int64
	SHA256         []byte
}

func agentMessageSourcePages(content string) []agentMessageSourcePage {
	if content == "" {
		return []agentMessageSourcePage{}
	}
	pages := make([]agentMessageSourcePage, 0, len(content)/app.MaxConversationMessageChunkBytes+1)
	pageStartByte, pageBytes := 0, 0
	pageStartRune, pageRunes := int64(0), 0
	appendPage := func(endByte int, endRune int64) {
		digest := sha256.Sum256([]byte(content[pageStartByte:endByte]))
		pages = append(pages, agentMessageSourcePage{RuneOffset: pageStartRune, NextRuneOffset: endRune,
			SHA256: append([]byte(nil), digest[:]...)})
		pageStartByte, pageBytes = endByte, 0
		pageStartRune, pageRunes = endRune, 0
	}
	for byteOffset, value := range content {
		width := utf8.RuneLen(value)
		if pageRunes > 0 && (pageBytes+width > app.MaxConversationMessageChunkBytes || pageRunes >= app.MaxConversationMessageChunkRunes) {
			appendPage(byteOffset, pageStartRune+int64(pageRunes))
		}
		pageBytes += width
		pageRunes++
	}
	appendPage(len(content), pageStartRune+int64(pageRunes))
	return pages
}

func prepareAgentMessageDeliveryReceipt(fence app.AgentCheckpointFence, conversationID string, message app.AgentConversationMessage,
	messageSequence, taskRevision int64, attachmentIDs []string, sourceContent string) (agentMessageDeliveryReceipt, error) {
	receiptFence := fence
	if message.Reference != nil {
		if message.Reference.AgentGeneration > fence.AgentGeneration || message.Reference.AgentGeneration < 1 ||
			message.Reference.RootSessionID != fence.AgentRootSessionID {
			return agentMessageDeliveryReceipt{}, domain.NewError(domain.CodeInvalidState, "checkpoint reference generation changed")
		}
		if message.SourceAgentGeneration > 0 && message.SourceAgentGeneration != message.Reference.AgentGeneration {
			return agentMessageDeliveryReceipt{}, domain.NewError(domain.CodeInvalidState, "checkpoint source generation changed")
		}
		receiptFence.AgentGeneration = message.Reference.AgentGeneration
	} else if message.SourceAgentGeneration > 0 {
		if message.SourceAgentGeneration > fence.AgentGeneration {
			return agentMessageDeliveryReceipt{}, domain.NewError(domain.CodeInvalidState, "checkpoint source generation changed")
		}
		receiptFence.AgentGeneration = message.SourceAgentGeneration
	}
	envelope, attachmentDigest, err := buildAgentMessageReference(receiptFence, conversationID, message.ID,
		messageSequence, taskRevision, message.Mode, attachmentIDs, sourceContent)
	if err != nil {
		return agentMessageDeliveryReceipt{}, err
	}
	if message.SourceSequence != 0 && message.SourceSequence != envelope.MessageSequence ||
		message.SourceContentBytes != 0 && message.SourceContentBytes != envelope.ContentBytes ||
		len(message.SourceSHA256) != 0 && !bytes.Equal(message.SourceSHA256, mustDecodeAgentMessageDigest(envelope.SourceSHA256)) ||
		message.ManifestVersion != 0 && message.ManifestVersion != envelope.ManifestVersion ||
		len(message.ManifestSHA256) != 0 && !bytes.Equal(message.ManifestSHA256, mustDecodeAgentMessageDigest(envelope.ManifestSHA256)) ||
		message.TaskRevision != 0 && message.TaskRevision != envelope.TaskRevision {
		return agentMessageDeliveryReceipt{}, domain.NewError(domain.CodeInvalidState, "checkpoint source metadata changed")
	}
	if message.RepresentationVersion != 0 && message.RepresentationVersion != app.AgentMessageReferenceEnvelopeVersion {
		return agentMessageDeliveryReceipt{}, domain.NewError(domain.CodeInvalidState, "checkpoint representation version changed")
	}

	kind := message.RepresentationKind
	if message.Reference != nil {
		if kind != "" && kind != "reference" {
			return agentMessageDeliveryReceipt{}, domain.NewError(domain.CodeInvalidState, "checkpoint representation kind changed")
		}
		kind = "reference"
		actual := *message.Reference
		if err := app.ValidateAgentMessageReferenceEnvelope(actual); err != nil {
			return agentMessageDeliveryReceipt{}, domain.NewError(domain.CodeInvalidState, "checkpoint message reference is invalid")
		}
		envelope.RepresentationSHA256 = actual.RepresentationSHA256
		actualJSON, actualErr := json.Marshal(actual)
		expectedJSON, expectedErr := json.Marshal(envelope)
		if actualErr != nil || expectedErr != nil || !bytes.Equal(actualJSON, expectedJSON) {
			return agentMessageDeliveryReceipt{}, domain.NewError(domain.CodeInvalidState, "checkpoint message reference changed")
		}
		marker, err := app.FormatAgentMessageReferenceEnvelope(actual)
		if err != nil || message.Content != marker {
			return agentMessageDeliveryReceipt{}, domain.NewError(domain.CodeInvalidState, "checkpoint message reference representation changed")
		}
	} else {
		if kind != "" && kind != "inline" || message.Content != sourceContent {
			return agentMessageDeliveryReceipt{}, domain.NewError(domain.CodeInvalidState, "checkpoint inline message changed")
		}
		digest, err := app.ComputeAgentMessageInlineRepresentationSHA256(envelope)
		if err != nil {
			return agentMessageDeliveryReceipt{}, err
		}
		envelope.RepresentationSHA256 = hex.EncodeToString(digest)
		kind = "inline"
	}
	representationDigest, err := hex.DecodeString(envelope.RepresentationSHA256)
	if err != nil || len(representationDigest) != sha256.Size ||
		len(message.RepresentationSHA256) != 0 && !bytes.Equal(message.RepresentationSHA256, representationDigest) {
		return agentMessageDeliveryReceipt{}, domain.NewError(domain.CodeInvalidState, "checkpoint representation digest changed")
	}
	return agentMessageDeliveryReceipt{Source: app.AgentMessageSourceReference{
		Envelope: envelope, RepresentationKind: kind, RepresentationVersion: app.AgentMessageReferenceEnvelopeVersion,
		RepresentationSHA256: append([]byte(nil), representationDigest...),
	}, AttachmentIDsSHA: attachmentDigest}, nil
}

func mustDecodeAgentMessageDigest(value string) []byte {
	digest, _ := hex.DecodeString(value)
	return digest
}

func persistAgentMessageDeliveryReceipt(ctx context.Context, tx pgx.Tx, fence app.AgentCheckpointFence,
	message app.AgentConversationMessage, receipt agentMessageDeliveryReceipt, eventSequence, checkpointThrough int64) error {
	envelope := receipt.Source.Envelope
	result, err := tx.Exec(ctx, `UPDATE task_agent_message_deliveries SET
 conversation_id=$2,message_sequence=$3,task_revision=$4,receipt_agent_generation=$5,source_content_bytes=$6,source_sha256=$7,
 manifest_version=$8,manifest_sha256=$9,representation_kind=$10,representation_version=$11,
 representation_sha256=$12,attachment_ids_sha256=$13,claimed_through_seq=$14,
 claimed_checkpoint_through_seq=$15,applied_agent_generation=COALESCE(applied_agent_generation,$24),
 applied_checkpoint_through_seq=COALESCE(applied_checkpoint_through_seq,$15),updated_at=clock_timestamp()
WHERE message_id=$1 AND task_id=$16 AND owner_id=$17 AND initiator_id=$18 AND agent_instance_id=$19
 AND agent_generation=$20 AND authorization_version=$21 AND session_id=$22 AND mode=$23
 AND claimed_through_seq IS NULL AND claimed_checkpoint_through_seq IS NULL
 AND (applied_checkpoint_through_seq IS NULL OR applied_checkpoint_through_seq=enqueued_checkpoint_through_seq)
 AND (source_sha256 IS NULL OR (conversation_id=$2 AND message_sequence=$3 AND task_revision=$4
  AND receipt_agent_generation=$5 AND source_content_bytes=$6 AND source_sha256=$7 AND manifest_version=$8
  AND manifest_sha256=$9 AND representation_kind=$10 AND representation_version=$11
  AND representation_sha256=$12 AND attachment_ids_sha256=$13))
 AND (enqueued_through_seq IS NULL OR enqueued_through_seq<$14)`,
		message.ID, envelope.ConversationID, envelope.MessageSequence, envelope.TaskRevision, envelope.AgentGeneration,
		envelope.ContentBytes, mustDecodeAgentMessageDigest(envelope.SourceSHA256), envelope.ManifestVersion,
		mustDecodeAgentMessageDigest(envelope.ManifestSHA256), receipt.Source.RepresentationKind,
		receipt.Source.RepresentationVersion, receipt.Source.RepresentationSHA256, receipt.AttachmentIDsSHA,
		eventSequence, checkpointThrough, fence.TaskID, fence.OwnerID, fence.InitiatorID, fence.AgentInstanceID,
		fence.AgentGeneration, fence.AuthorizationVersion, fence.AgentRootSessionID, string(message.Mode), fence.AgentGeneration)
	if err != nil {
		return fmt.Errorf("persist agent message delivery receipt: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeInvalidState, "agent message delivery receipt changed before checkpoint commit")
	}
	return nil
}

func persistAgentMessageEnqueuedReceipt(ctx context.Context, tx pgx.Tx, fence app.AgentCheckpointFence,
	message app.AgentConversationMessage, receipt agentMessageDeliveryReceipt, eventSequence, checkpointThrough int64) error {
	envelope := receipt.Source.Envelope
	result, err := tx.Exec(ctx, `UPDATE task_agent_message_deliveries SET
 conversation_id=$2,message_sequence=$3,task_revision=$4,receipt_agent_generation=$5,source_content_bytes=$6,source_sha256=$7,
 manifest_version=$8,manifest_sha256=$9,representation_kind=$10,representation_version=$11,
 representation_sha256=$12,attachment_ids_sha256=$13,enqueued_through_seq=$14,
 enqueued_checkpoint_through_seq=$15,applied_agent_generation=$24,applied_checkpoint_through_seq=$15,updated_at=clock_timestamp()
WHERE message_id=$1 AND task_id=$16 AND owner_id=$17 AND initiator_id=$18 AND agent_instance_id=$19
 AND agent_generation=$20 AND authorization_version=$21 AND session_id=$22 AND mode=$23
 AND enqueued_through_seq IS NULL AND claimed_through_seq IS NULL AND claimed_checkpoint_through_seq IS NULL
 AND applied_checkpoint_through_seq IS NULL AND source_sha256 IS NULL`,
		message.ID, envelope.ConversationID, envelope.MessageSequence, envelope.TaskRevision, envelope.AgentGeneration,
		envelope.ContentBytes, mustDecodeAgentMessageDigest(envelope.SourceSHA256), envelope.ManifestVersion,
		mustDecodeAgentMessageDigest(envelope.ManifestSHA256), receipt.Source.RepresentationKind,
		receipt.Source.RepresentationVersion, receipt.Source.RepresentationSHA256, receipt.AttachmentIDsSHA,
		eventSequence, checkpointThrough, fence.TaskID, fence.OwnerID, fence.InitiatorID, fence.AgentInstanceID,
		fence.AgentGeneration, fence.AuthorizationVersion, fence.AgentRootSessionID, string(message.Mode), fence.AgentGeneration)
	if err != nil {
		return fmt.Errorf("persist enqueued agent message receipt: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeInvalidState, "enqueued agent message receipt changed before checkpoint commit")
	}
	return nil
}

func recordAgentMessageEnqueueEvidence(ctx context.Context, tx pgx.Tx, fence app.AgentCheckpointFence,
	messageID string, eventSequence, checkpointThrough int64) error {
	result, err := tx.Exec(ctx, `UPDATE task_agent_message_deliveries SET enqueued_through_seq=$3,
 enqueued_checkpoint_through_seq=$4,updated_at=clock_timestamp()
WHERE message_id=$1 AND task_id=$2 AND owner_id=$5 AND initiator_id=$6 AND agent_instance_id=$7
 AND agent_generation=$8 AND authorization_version=$9 AND session_id=$10
 AND enqueued_through_seq IS NULL AND claimed_checkpoint_through_seq=$4 AND applied_checkpoint_through_seq=$4
 AND claimed_through_seq>$3`, messageID, fence.TaskID, eventSequence, checkpointThrough, fence.OwnerID,
		fence.InitiatorID, fence.AgentInstanceID, fence.AgentGeneration, fence.AuthorizationVersion, fence.AgentRootSessionID)
	if err != nil {
		return fmt.Errorf("record checkpointed message enqueue evidence: %w", err)
	}
	if result.RowsAffected() != 1 {
		return domain.NewError(domain.CodeInvalidState, "checkpointed message enqueue evidence changed")
	}
	return nil
}

func agentMessageSourceFence(ctx context.Context, tx pgx.Tx, fence app.AgentCheckpointFence) (agentTaskLock, int64, error) {
	if fence.AgentRootSessionID == "" {
		fence.AgentRootSessionID = fence.AgentSessionID
	}
	if !domain.ValidTaskID(fence.TaskID) || !domain.ValidUserID(fence.OwnerID) || !domain.ValidUserID(fence.InitiatorID) ||
		fence.AgentSessionID == "" || fence.AgentSessionID != fence.AgentRootSessionID || fence.AgentInstanceID == "" ||
		fence.AgentGeneration < 1 || fence.AuthorizationVersion < 1 || fence.LeaseOwner == "" || fence.LeaseVersion < 1 ||
		fence.EngineVersion == "" || fence.ProfileVersion == "" {
		return agentTaskLock{}, 0, domain.InvalidArgument("message_source", "message source fence is invalid")
	}
	current, err := lockAuthorizedAgentTask(ctx, tx, agentCheckpointInstanceFence(fence, fence.AgentRootSessionID))
	if err != nil {
		return agentTaskLock{}, 0, err
	}
	if current.InstanceID != fence.AgentInstanceID || current.Generation != fence.AgentGeneration ||
		current.SessionID != fence.AgentRootSessionID || current.EngineVersion != fence.EngineVersion || current.ProfileVersion != fence.ProfileVersion {
		return agentTaskLock{}, 0, domain.NewError(domain.CodeUnauthenticated, "message source authorization is unavailable")
	}
	if err := lockActiveAgentLease(ctx, tx, agentCheckpointInstanceFence(fence, fence.AgentRootSessionID)); err != nil {
		return agentTaskLock{}, 0, err
	}
	var parentSessionID string
	var sessionThrough int64
	var sessionDigest []byte
	if err := tx.QueryRow(ctx, `SELECT COALESCE(parent_session_id,''),checkpoint_through_seq,checkpoint_digest
FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2 AND session_id=$3 FOR UPDATE`,
		fence.TaskID, fence.OwnerID, fence.AgentRootSessionID).Scan(&parentSessionID, &sessionThrough, &sessionDigest); errors.Is(err, pgx.ErrNoRows) {
		return agentTaskLock{}, 0, domain.NewError(domain.CodeUnauthenticated, "message source session is unavailable")
	} else if err != nil {
		return agentTaskLock{}, 0, fmt.Errorf("lock message source session: %w", err)
	}
	if parentSessionID != "" || sessionThrough != current.CheckpointThrough || !bytes.Equal(sessionDigest, current.CheckpointDigest) {
		return agentTaskLock{}, 0, domain.NewError(domain.CodeInvalidState, "message source root checkpoint changed")
	}
	var taskRevision int64
	if err := tx.QueryRow(ctx, `SELECT current_revision FROM tasks WHERE id=$1 AND owner_id=$2`, fence.TaskID, fence.OwnerID).Scan(&taskRevision); err != nil {
		return agentTaskLock{}, 0, fmt.Errorf("read message source task revision: %w", err)
	}
	return current, taskRevision, nil
}

func (s *Store) ReadAgentMessageSourceReference(ctx context.Context, fence app.AgentCheckpointFence,
	messageID string) (app.AgentMessageSourceReference, error) {
	if fence.AgentRootSessionID == "" {
		fence.AgentRootSessionID = fence.AgentSessionID
	}
	if !domain.ValidMessageID(messageID) {
		return app.AgentMessageSourceReference{}, domain.InvalidArgument("message_source", "message source identity is invalid")
	}
	var output app.AgentMessageSourceReference
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, taskRevision, err := agentMessageSourceFence(ctx, tx, fence)
		if err != nil {
			return err
		}
		output, _, err = readAgentMessageSourceReceipt(ctx, tx, fence, current, taskRevision, messageID)
		return err
	})
	return output, err
}

func (s *Store) ReadAgentMessageSourcePage(ctx context.Context, fence app.AgentCheckpointFence, messageID string,
	expectedSourceSHA256 []byte, runeOffset int64) (app.AgentMessageSourcePageRead, error) {
	if fence.AgentRootSessionID == "" {
		fence.AgentRootSessionID = fence.AgentSessionID
	}
	if !domain.ValidMessageID(messageID) || len(expectedSourceSHA256) != sha256.Size || runeOffset < 0 {
		return app.AgentMessageSourcePageRead{}, domain.InvalidArgument("message_source_page", "message source page identity is invalid")
	}
	var output app.AgentMessageSourcePageRead
	err := s.WithTx(ctx, func(tx pgx.Tx) error {
		current, taskRevision, err := agentMessageSourceFence(ctx, tx, fence)
		if err != nil {
			return err
		}
		reference, content, err := readAgentMessageSourceReceipt(ctx, tx, fence, current, taskRevision, messageID)
		if err != nil {
			return err
		}
		returnAgentMessageSourcePage(reference.Envelope, content, expectedSourceSHA256, runeOffset, &output)
		if output.Content == "" || output.NextRuneOffset <= output.RuneOffset {
			return domain.NewError(domain.CodeNotFound, "message source page is unavailable")
		}
		return nil
	})
	return output, err
}

func readAgentMessageSourceReceipt(ctx context.Context, tx pgx.Tx, fence app.AgentCheckpointFence, current agentTaskLock,
	taskRevision int64, messageID string) (app.AgentMessageSourceReference, string, error) {
	var taskID, ownerID, initiatorID, agentID, sessionID, mode, conversationID, representationKind, deliveryStatus string
	var generation, authorizationVersion, messageSequence, receiptRevision, sourceContentBytes int64
	var receiptGeneration pgtype.Int8
	var manifestVersion, representationVersion int
	var sourceDigest, manifestDigest, representationDigest, attachmentDigest []byte
	var appliedAgentGeneration pgtype.Int8
	var enqueuedThrough, enqueuedCheckpoint, claimedThrough, claimedCheckpoint, appliedCheckpoint pgtype.Int8
	var content, actorID string
	var rowSequence int64
	err := tx.QueryRow(ctx, `SELECT delivery.task_id,delivery.owner_id,delivery.initiator_id,delivery.agent_instance_id,
 delivery.agent_generation,delivery.receipt_agent_generation,delivery.authorization_version,delivery.session_id,delivery.mode,
 delivery.conversation_id,delivery.message_sequence,delivery.task_revision,delivery.source_content_bytes,delivery.source_sha256,
 delivery.manifest_version,delivery.manifest_sha256,delivery.representation_kind,delivery.representation_version,
 delivery.representation_sha256,delivery.attachment_ids_sha256,delivery.applied_agent_generation,
 delivery.enqueued_through_seq,delivery.enqueued_checkpoint_through_seq,
 delivery.claimed_through_seq,delivery.claimed_checkpoint_through_seq,delivery.applied_checkpoint_through_seq,
 message.content,message.sequence,message.delivery_status,message.actor_id
FROM task_agent_message_deliveries delivery
JOIN conversation_messages message ON message.id=delivery.message_id AND message.task_id=delivery.task_id
JOIN tasks task ON task.id=delivery.task_id AND task.owner_id=delivery.owner_id
WHERE delivery.message_id=$1 AND delivery.task_id=$2 AND delivery.owner_id=$3
FOR UPDATE OF delivery,message`, messageID, fence.TaskID, fence.OwnerID).Scan(
		&taskID, &ownerID, &initiatorID, &agentID, &generation, &receiptGeneration, &authorizationVersion, &sessionID, &mode,
		&conversationID, &messageSequence, &receiptRevision, &sourceContentBytes, &sourceDigest, &manifestVersion,
		&manifestDigest, &representationKind, &representationVersion, &representationDigest, &attachmentDigest, &appliedAgentGeneration,
		&enqueuedThrough, &enqueuedCheckpoint, &claimedThrough, &claimedCheckpoint, &appliedCheckpoint,
		&content, &rowSequence, &deliveryStatus, &actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.AgentMessageSourceReference{}, "", domain.NewError(domain.CodeNotFound, "message source receipt is unavailable")
	}
	if err != nil {
		return app.AgentMessageSourceReference{}, "", fmt.Errorf("read agent message source receipt: %w", err)
	}
	if taskID != fence.TaskID || ownerID != fence.OwnerID || initiatorID != fence.InitiatorID || actorID != fence.InitiatorID ||
		sessionID != fence.AgentRootSessionID || authorizationVersion != fence.AuthorizationVersion || generation < 1 ||
		generation > fence.AgentGeneration || !receiptGeneration.Valid || receiptGeneration.Int64 < 1 || receiptGeneration.Int64 > generation ||
		!appliedAgentGeneration.Valid || appliedAgentGeneration.Int64 < 1 || appliedAgentGeneration.Int64 > fence.AgentGeneration ||
		(mode != string(app.AgentMessageDeliverySteer) && mode != string(app.AgentMessageDeliveryFollowup)) ||
		conversationID != current.ConversationID || messageSequence != rowSequence || receiptRevision != taskRevision || receiptRevision < 0 ||
		messageSequence < 1 || !validAgentMessageSourceDigestFields(sourceContentBytes, sourceDigest, manifestVersion, manifestDigest,
		representationVersion, representationDigest, attachmentDigest, representationKind) || !appliedCheckpoint.Valid ||
		appliedCheckpoint.Int64 < 0 || appliedCheckpoint.Int64 > current.CheckpointThrough {
		return app.AgentMessageSourceReference{}, "", domain.NewError(domain.CodeInvalidState, "message source receipt is incomplete or stale")
	}
	claimProof := claimedThrough.Valid && claimedCheckpoint.Valid && claimedCheckpoint.Int64 == appliedCheckpoint.Int64
	enqueueProof := enqueuedThrough.Valid && enqueuedCheckpoint.Valid && enqueuedCheckpoint.Int64 == appliedCheckpoint.Int64
	if claimProof && deliveryStatus != "applied" ||
		!claimProof && (!enqueueProof || deliveryStatus != "pending" && deliveryStatus != "applied") {
		return app.AgentMessageSourceReference{}, "", domain.NewError(domain.CodeInvalidState, "message source has no applied checkpoint proof")
	}
	var proofEventSequence int64
	if enqueueProof {
		proofEventSequence = enqueuedThrough.Int64
	} else {
		proofEventSequence = claimedThrough.Int64
	}
	var checkpointed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_agent_checkpoints
WHERE task_id=$1 AND owner_id=$2 AND agent_session_id=$3 AND agent_root_session_id=$3
 AND agent_generation=$4 AND auth_version=$5 AND from_seq<=$6 AND through_seq=$7 AND through_seq>=$6)`,
		fence.TaskID, fence.OwnerID, fence.AgentRootSessionID, appliedAgentGeneration.Int64, authorizationVersion,
		proofEventSequence, appliedCheckpoint.Int64).Scan(&checkpointed); err != nil {
		return app.AgentMessageSourceReference{}, "", fmt.Errorf("verify applied message source checkpoint: %w", err)
	}
	if !checkpointed {
		return app.AgentMessageSourceReference{}, "", domain.NewError(domain.CodeInvalidState, "message source checkpoint receipt is unavailable")
	}
	serviceInstanceID, err := serviceInstanceIDInTx(ctx, tx)
	if err != nil {
		return app.AgentMessageSourceReference{}, "", err
	}
	var attachmentIDs []string
	if current.ProjectID != "" {
		attachmentIDs, err = loadProjectCheckpointAttachmentIDs(ctx, tx, serviceInstanceID, current.ProjectID,
			fence.OwnerID, fence.InitiatorID, conversationID, messageID)
	} else {
		attachmentIDs, err = loadConversationMessageAttachmentIDs(ctx, tx, serviceInstanceID, fence.OwnerID, conversationID, messageID)
	}
	if err != nil {
		return app.AgentMessageSourceReference{}, "", err
	}
	historicalFence := fence
	historicalFence.AgentGeneration = receiptGeneration.Int64
	expected, expectedAttachmentDigest, err := buildAgentMessageReference(historicalFence, conversationID, messageID,
		messageSequence, receiptRevision, app.AgentMessageDeliveryMode(mode), attachmentIDs, content)
	if err != nil {
		return app.AgentMessageSourceReference{}, "", domain.NewError(domain.CodeInvalidState, "message source changed after delivery")
	}
	if !bytes.Equal(mustDecodeAgentMessageDigest(expected.SourceSHA256), sourceDigest) || expected.ContentBytes != sourceContentBytes ||
		expected.ManifestVersion != manifestVersion || !bytes.Equal(mustDecodeAgentMessageDigest(expected.ManifestSHA256), manifestDigest) ||
		!bytes.Equal(expectedAttachmentDigest, attachmentDigest) {
		return app.AgentMessageSourceReference{}, "", domain.NewError(domain.CodeInvalidState, "message source manifest changed after delivery")
	}
	var representationSHA256 []byte
	if representationKind == "inline" {
		representationSHA256, err = app.ComputeAgentMessageInlineRepresentationSHA256(expected)
	} else {
		representationSHA256, err = app.ComputeAgentMessageReferenceRepresentationSHA256(expected)
	}
	if err != nil || representationVersion != app.AgentMessageReferenceEnvelopeVersion || !bytes.Equal(representationSHA256, representationDigest) {
		return app.AgentMessageSourceReference{}, "", domain.NewError(domain.CodeInvalidState, "message representation receipt changed")
	}
	expected.RepresentationSHA256 = hex.EncodeToString(representationDigest)
	reference := app.AgentMessageSourceReference{Envelope: expected, RepresentationKind: representationKind,
		RepresentationVersion: representationVersion, RepresentationSHA256: append([]byte(nil), representationDigest...),
		AppliedCheckpointThrough: appliedCheckpoint.Int64}
	if err := app.ValidateAgentMessageSourceReference(reference); err != nil {
		return app.AgentMessageSourceReference{}, "", domain.NewError(domain.CodeInvalidState, "message source receipt integrity check failed")
	}
	reference.AuthorizedAgentGeneration = fence.AgentGeneration
	if generation != fence.AgentGeneration || agentID != fence.AgentInstanceID {
		result, err := tx.Exec(ctx, `UPDATE task_agent_message_deliveries SET agent_instance_id=$4,agent_generation=$5,updated_at=clock_timestamp()
WHERE message_id=$1 AND task_id=$2 AND owner_id=$3 AND agent_generation=$6 AND receipt_agent_generation=$7
 AND authorization_version=$8 AND session_id=$9`, messageID, fence.TaskID, fence.OwnerID, fence.AgentInstanceID,
			fence.AgentGeneration, generation, receiptGeneration.Int64, fence.AuthorizationVersion, fence.AgentRootSessionID)
		if err != nil {
			return app.AgentMessageSourceReference{}, "", fmt.Errorf("rebind recovered message source receipt: %w", err)
		}
		if result.RowsAffected() != 1 {
			return app.AgentMessageSourceReference{}, "", domain.NewError(domain.CodeConflict, "message source receipt changed during recovery")
		}
	}
	return reference, content, nil
}

func validAgentMessageSourceDigestFields(contentBytes int64, sourceDigest []byte, manifestVersion int, manifestDigest []byte,
	representationVersion int, representationDigest, attachmentDigest []byte, representationKind string) bool {
	emptyAttachmentListDigest := sha256.Sum256([]byte("[]"))
	validContentSize := contentBytes > 0 && contentBytes <= app.MaxConversationMessageBytes ||
		contentBytes == 0 && len(attachmentDigest) == sha256.Size && !bytes.Equal(attachmentDigest, emptyAttachmentListDigest[:])
	return validContentSize && len(sourceDigest) == sha256.Size &&
		manifestVersion == app.AgentMessageManifestVersion && len(manifestDigest) == sha256.Size &&
		representationVersion == app.AgentMessageReferenceEnvelopeVersion && len(representationDigest) == sha256.Size &&
		len(attachmentDigest) == sha256.Size && (representationKind == "inline" || representationKind == "reference")
}

func returnAgentMessageSourcePage(envelope app.AgentMessageReferenceEnvelope, content string, expectedSourceSHA256 []byte,
	runeOffset int64, output *app.AgentMessageSourcePageRead) {
	if len(expectedSourceSHA256) != sha256.Size || !bytes.Equal(mustDecodeAgentMessageDigest(envelope.SourceSHA256), expectedSourceSHA256) {
		return
	}
	pages := agentMessageSourcePages(content)
	for _, page := range pages {
		if runeOffset < page.RuneOffset || runeOffset >= page.NextRuneOffset {
			continue
		}
		start := agentMessageRuneByteOffset(content, runeOffset)
		end := agentMessageRuneByteOffset(content, page.NextRuneOffset)
		if start < 0 || end < start || end > len(content) {
			return
		}
		fragment := content[start:end]
		fragmentDigest := sha256.Sum256([]byte(fragment))
		output.MessageID = envelope.MessageID
		output.SourceSHA256 = mustDecodeAgentMessageDigest(envelope.SourceSHA256)
		output.PageSHA256 = append([]byte(nil), page.SHA256...)
		output.PageRuneOffset, output.PageNextRuneOffset = page.RuneOffset, page.NextRuneOffset
		output.FragmentSHA256 = append([]byte(nil), fragmentDigest[:]...)
		output.RuneOffset, output.NextRuneOffset = runeOffset, page.NextRuneOffset
		output.TotalRunes = envelope.TotalRunes
		output.Content = fragment
		return
	}
}

func agentMessageRuneByteOffset(content string, runeOffset int64) int {
	if runeOffset < 0 {
		return -1
	}
	if runeOffset == 0 {
		return 0
	}
	var runes int64
	for offset := range content {
		if runes == runeOffset {
			return offset
		}
		runes++
	}
	if runes == runeOffset {
		return len(content)
	}
	return -1
}
