-- +goose Up
-- DSH main-coordinator checkpoints are keyed by the accepted source message,
-- conversation and owner. They never depend on or masquerade as a Task.
CREATE TABLE conversation_coordinator_checkpoint_heads (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  actor_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  conversation_id text NOT NULL,
  message_id text NOT NULL,
  agent_id text NOT NULL CHECK (length(agent_id) BETWEEN 1 AND 128),
  generation bigint NOT NULL CHECK (generation > 0),
  authorization_version bigint NOT NULL CHECK (authorization_version > 0),
  session_id text NOT NULL CHECK (length(session_id) BETWEEN 1 AND 128),
  through_sequence bigint NOT NULL DEFAULT -1 CHECK (through_sequence >= -1),
  digest bytea NOT NULL DEFAULT '' CHECK (octet_length(digest) IN (0,32)),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (service_instance_id, owner_id, actor_id, conversation_id, message_id, session_id),
  FOREIGN KEY (message_id, owner_id, conversation_id, actor_id)
    REFERENCES conversation_messages(id, owner_id, conversation_id, actor_id) ON DELETE RESTRICT,
  CHECK ((through_sequence = -1) = (octet_length(digest) = 0))
);

CREATE TABLE conversation_coordinator_checkpoint_pages (
  service_instance_id uuid NOT NULL,
  owner_id text NOT NULL,
  actor_id text NOT NULL,
  conversation_id text NOT NULL,
  message_id text NOT NULL,
  session_id text NOT NULL,
  from_sequence bigint NOT NULL CHECK (from_sequence >= 0),
  through_sequence bigint NOT NULL CHECK (through_sequence >= from_sequence),
  digest bytea NOT NULL CHECK (octet_length(digest) = 32),
  ciphertext bytea NOT NULL CHECK (octet_length(ciphertext) BETWEEN 1 AND 1500000),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (service_instance_id, owner_id, actor_id, conversation_id, message_id, session_id, from_sequence),
  CONSTRAINT conversation_coordinator_checkpoint_pages_head_fkey
    FOREIGN KEY (service_instance_id, owner_id, actor_id, conversation_id, message_id, session_id)
    REFERENCES conversation_coordinator_checkpoint_heads(service_instance_id, owner_id, actor_id, conversation_id, message_id, session_id)
    ON DELETE RESTRICT
);

-- +goose Down
-- Checkpoints contain recoverable coordination context; dropping them could
-- make an accepted message appear safe to rerun after an uncertain response.
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_coordinator_checkpoint_heads) THEN
    RAISE EXCEPTION 'cannot remove non-empty conversation coordinator checkpoints';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE conversation_coordinator_checkpoint_pages;
DROP TABLE conversation_coordinator_checkpoint_heads;
