-- +goose Up
ALTER TABLE conversation_messages
  ADD COLUMN variant_group_id text,
  ADD COLUMN variant_version bigint,
  ADD COLUMN variant_action text,
  ADD COLUMN variant_operation_id text;

ALTER TABLE conversation_messages
  ADD CONSTRAINT conversation_messages_variant_projection_check CHECK (
    (variant_group_id IS NULL AND variant_version IS NULL AND variant_action IS NULL AND variant_operation_id IS NULL)
    OR (variant_group_id IS NOT NULL AND variant_version IS NOT NULL AND variant_version >= 1 AND
        variant_action IS NOT NULL AND
        variant_action IN ('original','edit_resend','regenerate_text','continue_generation'))
  );

CREATE TABLE conversation_message_variants (
  operation_id text PRIMARY KEY CHECK (operation_id ~ '^o[a-z0-9]{7}$'),
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  actor_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  project_id text REFERENCES projects(id) ON DELETE RESTRICT,
  conversation_id text NOT NULL,
  source_message_id text NOT NULL REFERENCES conversation_messages(id) ON DELETE CASCADE,
  input_source_message_id text NOT NULL REFERENCES conversation_messages(id) ON DELETE CASCADE,
  attachment_source_message_id text REFERENCES conversation_messages(id) ON DELETE CASCADE,
  variant_group_id text NOT NULL REFERENCES conversation_messages(id) ON DELETE CASCADE,
  expected_version bigint NOT NULL CHECK (expected_version >= 1),
  variant_version bigint NOT NULL CHECK (variant_version > 1),
  action text NOT NULL CHECK (action IN ('edit_resend','regenerate_text','continue_generation')),
  state text NOT NULL CHECK (state IN ('accepted','running','completed','failed','unknown')),
  idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9._~-]{8,128}$'),
  request_hash bytea NOT NULL CHECK (octet_length(request_hash)=32),
  task_id text REFERENCES tasks(id) ON DELETE SET NULL,
  user_message_id text,
  assistant_message_id text,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  FOREIGN KEY (conversation_id, owner_id) REFERENCES conversations(id, owner_id) ON DELETE CASCADE,
  UNIQUE (service_instance_id, actor_id, conversation_id, idempotency_key),
  UNIQUE (variant_group_id, variant_version)
);

CREATE UNIQUE INDEX conversation_message_variant_one_active_task
  ON conversation_message_variants(task_id)
  WHERE task_id IS NOT NULL AND state IN ('accepted','running','unknown');
CREATE INDEX conversation_message_variant_task
  ON conversation_message_variants(task_id, variant_version DESC)
  WHERE task_id IS NOT NULL;

ALTER TABLE conversation_messages
  ADD CONSTRAINT conversation_messages_variant_operation_fk
  FOREIGN KEY (variant_operation_id) REFERENCES conversation_message_variants(operation_id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE conversation_messages DROP CONSTRAINT conversation_messages_variant_operation_fk;
DROP INDEX conversation_message_variant_task;
DROP INDEX conversation_message_variant_one_active_task;
DROP TABLE conversation_message_variants;
ALTER TABLE conversation_messages DROP CONSTRAINT conversation_messages_variant_projection_check;
ALTER TABLE conversation_messages
  DROP COLUMN variant_operation_id,
  DROP COLUMN variant_action,
  DROP COLUMN variant_version,
  DROP COLUMN variant_group_id;
