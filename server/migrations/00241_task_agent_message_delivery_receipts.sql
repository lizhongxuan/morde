-- +goose Up
-- R21 separates the immutable user source from the representation accepted by
-- DSH. Existing rows remain nullable and continue through the legacy inline
-- path; a receipt is complete only when every digest and fence is present.
ALTER TABLE task_agent_message_deliveries
  ADD COLUMN enqueued_checkpoint_through_seq bigint CHECK (enqueued_checkpoint_through_seq IS NULL OR enqueued_checkpoint_through_seq >= 0),
  ADD COLUMN conversation_id text,
  ADD COLUMN message_sequence bigint CHECK (message_sequence IS NULL OR message_sequence > 0),
  ADD COLUMN task_revision bigint CHECK (task_revision IS NULL OR task_revision >= 0),
  ADD COLUMN receipt_agent_generation bigint CHECK (receipt_agent_generation IS NULL OR receipt_agent_generation > 0),
  ADD COLUMN source_content_bytes bigint CHECK (source_content_bytes IS NULL OR source_content_bytes BETWEEN 1 AND 8388608),
  ADD COLUMN source_sha256 bytea CHECK (source_sha256 IS NULL OR octet_length(source_sha256)=32),
  ADD COLUMN manifest_version integer CHECK (manifest_version IS NULL OR manifest_version = 1),
  ADD COLUMN manifest_sha256 bytea CHECK (manifest_sha256 IS NULL OR octet_length(manifest_sha256)=32),
  ADD COLUMN representation_kind text CHECK (representation_kind IS NULL OR representation_kind IN ('inline','reference')),
  ADD COLUMN representation_version integer CHECK (representation_version IS NULL OR representation_version = 1),
  ADD COLUMN representation_sha256 bytea CHECK (representation_sha256 IS NULL OR octet_length(representation_sha256)=32),
  ADD COLUMN attachment_ids_sha256 bytea CHECK (attachment_ids_sha256 IS NULL OR octet_length(attachment_ids_sha256)=32),
  ADD COLUMN applied_agent_generation bigint CHECK (applied_agent_generation IS NULL OR applied_agent_generation > 0),
  ADD COLUMN applied_checkpoint_through_seq bigint CHECK (applied_checkpoint_through_seq IS NULL OR applied_checkpoint_through_seq >= 0),
  ADD CONSTRAINT task_agent_message_delivery_receipt_complete CHECK (
    (conversation_id IS NULL AND message_sequence IS NULL AND task_revision IS NULL AND receipt_agent_generation IS NULL AND source_content_bytes IS NULL
      AND source_sha256 IS NULL AND manifest_version IS NULL AND manifest_sha256 IS NULL AND representation_kind IS NULL
      AND representation_version IS NULL AND representation_sha256 IS NULL AND attachment_ids_sha256 IS NULL
      AND applied_agent_generation IS NULL AND applied_checkpoint_through_seq IS NULL)
    OR
    (conversation_id IS NOT NULL AND message_sequence IS NOT NULL AND task_revision IS NOT NULL AND receipt_agent_generation IS NOT NULL AND source_content_bytes IS NOT NULL
      AND source_sha256 IS NOT NULL AND manifest_version IS NOT NULL AND manifest_sha256 IS NOT NULL AND representation_kind IS NOT NULL
      AND representation_version IS NOT NULL AND representation_sha256 IS NOT NULL AND attachment_ids_sha256 IS NOT NULL)
  ),
  ADD CONSTRAINT task_agent_message_delivery_receipt_applied_checkpoint CHECK (
    applied_checkpoint_through_seq IS NULL OR
      (representation_sha256 IS NOT NULL AND applied_agent_generation IS NOT NULL AND
        (claimed_checkpoint_through_seq=applied_checkpoint_through_seq OR enqueued_checkpoint_through_seq=applied_checkpoint_through_seq))
  );

COMMENT ON COLUMN task_agent_message_deliveries.source_sha256 IS
  'SHA-256 of the immutable original UTF-8 user message, never of the DSH representation.';
COMMENT ON COLUMN task_agent_message_deliveries.representation_sha256 IS
  'SHA-256 of the canonical inline or source-reference envelope accepted by the root DSH checkpoint.';
COMMENT ON COLUMN task_agent_message_deliveries.applied_checkpoint_through_seq IS
  'Root checkpoint tail sequence that atomically accepted the exact receipt representation; this does not prove full source reading.';

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_agent_message_deliveries WHERE source_sha256 IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot remove persisted R21 message delivery receipt evidence';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE task_agent_message_deliveries
  DROP CONSTRAINT task_agent_message_delivery_receipt_applied_checkpoint,
  DROP CONSTRAINT task_agent_message_delivery_receipt_complete,
  DROP COLUMN applied_checkpoint_through_seq,
  DROP COLUMN applied_agent_generation,
  DROP COLUMN enqueued_checkpoint_through_seq,
  DROP COLUMN attachment_ids_sha256,
  DROP COLUMN representation_sha256,
  DROP COLUMN representation_version,
  DROP COLUMN representation_kind,
  DROP COLUMN manifest_sha256,
  DROP COLUMN manifest_version,
  DROP COLUMN source_sha256,
  DROP COLUMN source_content_bytes,
  DROP COLUMN receipt_agent_generation,
  DROP COLUMN task_revision,
  DROP COLUMN message_sequence,
  DROP COLUMN conversation_id;
