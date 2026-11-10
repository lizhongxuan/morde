-- +goose Up
-- Checkpoints created before this migration have no verifiable route-context
-- binding. Keep those rows, but make them ineligible for fail-open recovery.
ALTER TABLE conversation_coordinator_checkpoint_heads
  ADD COLUMN context_digest bytea
  CHECK (context_digest IS NULL OR octet_length(context_digest) = 32);

ALTER TABLE conversation_coordinator_checkpoint_heads
  ADD CONSTRAINT conversation_coordinator_checkpoint_heads_context_key
  UNIQUE (service_instance_id, owner_id, actor_id, conversation_id, message_id, session_id, context_digest);

ALTER TABLE conversation_coordinator_checkpoint_pages
  ADD COLUMN context_digest bytea
  CHECK (context_digest IS NULL OR octet_length(context_digest) = 32);

ALTER TABLE conversation_coordinator_checkpoint_pages
  ADD CONSTRAINT conversation_coordinator_checkpoint_pages_context_head_fkey
  FOREIGN KEY (service_instance_id, owner_id, actor_id, conversation_id, message_id, session_id, context_digest)
  REFERENCES conversation_coordinator_checkpoint_heads(service_instance_id, owner_id, actor_id, conversation_id, message_id, session_id, context_digest)
  ON DELETE RESTRICT;

-- +goose Down
-- Removing the context binding could make a changed route snapshot appear
-- compatible with a checkpoint. Refuse rollback while any checkpoint exists.
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_coordinator_checkpoint_heads)
     OR EXISTS (SELECT 1 FROM conversation_coordinator_checkpoint_pages) THEN
    RAISE EXCEPTION 'cannot remove coordinator checkpoint context binding while checkpoints exist';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE conversation_coordinator_checkpoint_pages
  DROP CONSTRAINT conversation_coordinator_checkpoint_pages_context_head_fkey;
ALTER TABLE conversation_coordinator_checkpoint_pages DROP COLUMN context_digest;
ALTER TABLE conversation_coordinator_checkpoint_heads DROP CONSTRAINT conversation_coordinator_checkpoint_heads_context_key;
ALTER TABLE conversation_coordinator_checkpoint_heads DROP COLUMN context_digest;
