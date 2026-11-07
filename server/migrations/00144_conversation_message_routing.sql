-- +goose Up
-- A received message remains the conversation authority. This row holds its
-- idempotency, routing input and recoverable worker lease, without creating a Task.
ALTER TABLE conversation_messages
  ADD CONSTRAINT conversation_messages_id_owner_conversation_actor_unique
  UNIQUE (id, owner_id, conversation_id, actor_id);

CREATE TABLE conversation_message_routes (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  actor_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  conversation_id text NOT NULL,
  message_id text NOT NULL UNIQUE,
  client_message_id text NOT NULL CHECK (client_message_id ~ '^[A-Za-z0-9._~-]{8,128}$'),
  previous_client_message_id text CHECK (previous_client_message_id ~ '^[A-Za-z0-9._~-]{8,128}$'),
  reference_task_id text,
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  request_metadata jsonb NOT NULL CHECK (jsonb_typeof(request_metadata) = 'object'),
  authorization_snapshot jsonb NOT NULL CHECK (jsonb_typeof(authorization_snapshot) = 'object'),
  state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'processing', 'routed', 'waiting_user', 'failed')),
  lease_owner text NOT NULL DEFAULT '',
  lease_version bigint NOT NULL DEFAULT 0 CHECK (lease_version >= 0),
  lease_expires_at timestamptz,
  failure_reason text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (service_instance_id, actor_id, conversation_id, client_message_id),
  FOREIGN KEY (message_id, owner_id, conversation_id, actor_id)
    REFERENCES conversation_messages(id, owner_id, conversation_id, actor_id) ON DELETE RESTRICT,
  CHECK ((lease_owner = '') = (lease_expires_at IS NULL)),
  CHECK ((state = 'processing') = (lease_owner <> ''))
);

CREATE INDEX conversation_message_routes_claimable
  ON conversation_message_routes(state, lease_expires_at, created_at, message_id)
  WHERE state IN ('pending', 'processing');

ALTER TABLE tasks
  ADD COLUMN current_revision bigint NOT NULL DEFAULT 0 CHECK (current_revision >= 0),
  ADD COLUMN desired_revision bigint NOT NULL DEFAULT 0 CHECK (desired_revision >= current_revision);

-- +goose Down
-- Never silently delete accepted messages' routing state or active revisions.
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_message_routes)
     OR EXISTS (SELECT 1 FROM tasks WHERE current_revision <> 0 OR desired_revision <> 0) THEN
    RAISE EXCEPTION 'cannot remove conversation routing while messages or revisions exist';
  END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE tasks DROP COLUMN desired_revision, DROP COLUMN current_revision;
DROP INDEX conversation_message_routes_claimable;
DROP TABLE conversation_message_routes;
ALTER TABLE conversation_messages DROP CONSTRAINT conversation_messages_id_owner_conversation_actor_unique;
