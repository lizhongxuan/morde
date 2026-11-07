-- +goose Up
-- Uploads retain immutable message metadata and exact UTF-8 bytes until the
-- completed text and routing receipt can be committed as one message.
CREATE TABLE conversation_message_uploads (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  actor_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  conversation_id text NOT NULL,
  client_message_id text NOT NULL CHECK (client_message_id ~ '^[A-Za-z0-9._~-]{8,128}$'),
  total_bytes integer NOT NULL CHECK (total_bytes BETWEEN 1 AND 8388608),
  total_chunks integer NOT NULL CHECK (total_chunks BETWEEN 1 AND 512 AND total_chunks = (total_bytes + 16383) / 16384),
  content_sha256 bytea NOT NULL CHECK (octet_length(content_sha256) = 32),
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  request_metadata jsonb NOT NULL CHECK (jsonb_typeof(request_metadata) = 'object'),
  authorization_snapshot jsonb NOT NULL CHECK (jsonb_typeof(authorization_snapshot) = 'object'),
  state text NOT NULL DEFAULT 'uploading' CHECK (state IN ('uploading', 'committed')),
  message_id text,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (service_instance_id, actor_id, conversation_id, client_message_id),
  FOREIGN KEY (conversation_id, owner_id) REFERENCES conversations(id, owner_id) ON DELETE RESTRICT,
  FOREIGN KEY (message_id, owner_id, conversation_id, actor_id)
    REFERENCES conversation_messages(id, owner_id, conversation_id, actor_id) ON DELETE RESTRICT,
  CHECK ((state = 'committed') = (message_id IS NOT NULL))
);

CREATE TABLE conversation_message_upload_chunks (
  service_instance_id uuid NOT NULL,
  actor_id text NOT NULL,
  conversation_id text NOT NULL,
  client_message_id text NOT NULL,
  sequence integer NOT NULL CHECK (sequence BETWEEN 0 AND 511),
  content bytea NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 16384),
  content_sha256 bytea NOT NULL CHECK (octet_length(content_sha256) = 32),
  PRIMARY KEY (service_instance_id, actor_id, conversation_id, client_message_id, sequence),
  FOREIGN KEY (service_instance_id, actor_id, conversation_id, client_message_id)
    REFERENCES conversation_message_uploads(service_instance_id, actor_id, conversation_id, client_message_id) ON DELETE RESTRICT
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_message_uploads) THEN
    RAISE EXCEPTION 'cannot remove conversation uploads while message data exists';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE conversation_message_upload_chunks;
DROP TABLE conversation_message_uploads;
