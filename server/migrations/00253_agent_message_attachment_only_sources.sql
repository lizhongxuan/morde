-- +goose Up
-- An attachment-only user message has an empty text source but still has a
-- non-empty, server-validated attachment identity. Keep rejecting empty
-- text-only messages by requiring the canonical empty attachment-list digest
-- to differ whenever the source content length is zero.
ALTER TABLE task_agent_message_deliveries
  DROP CONSTRAINT task_agent_message_deliveries_source_content_bytes_check,
  ADD CONSTRAINT task_agent_message_deliveries_source_content_bytes_check CHECK (
    source_content_bytes IS NULL OR source_content_bytes BETWEEN 1 AND 8388608 OR
    (source_content_bytes = 0 AND attachment_ids_sha256 IS NOT NULL AND
      attachment_ids_sha256 <> sha256(convert_to('[]', 'UTF8')))
  );

-- +goose Down
-- Keep the relaxed constraint so persisted attachment-only receipts remain
-- readable after a code rollback. Legacy writers still reject empty text.
SELECT 1;
