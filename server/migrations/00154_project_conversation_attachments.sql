-- +goose Up
-- Project attachments are not personal library objects. The original sender,
-- message and declared client slot remain their permanent identity.
CREATE TABLE project_conversation_attachments (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  project_id text NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  actor_id text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  conversation_id text NOT NULL,
  message_id text NOT NULL REFERENCES conversation_message_routes(message_id) ON DELETE RESTRICT,
  client_attachment_id text NOT NULL CHECK (client_attachment_id ~ '^[A-Za-z0-9._~-]{8,128}$'),
  attachment_id text NOT NULL CHECK (attachment_id ~ '^pca_[a-f0-9]{32}$'),
  name text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 255),
  media_type text NOT NULL CHECK (octet_length(media_type) BETWEEN 1 AND 128),
  size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 0 AND 314572800),
  sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
  state text NOT NULL CHECK (state IN ('uploading','committed','expired')),
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (message_id, client_attachment_id),
  UNIQUE (service_instance_id, project_id, attachment_id),
  UNIQUE (service_instance_id, project_id, message_id, client_attachment_id),
  FOREIGN KEY (message_id, owner_id, conversation_id, actor_id)
    REFERENCES conversation_messages(id, owner_id, conversation_id, actor_id) ON DELETE RESTRICT
);

CREATE INDEX project_conversation_attachments_budget
  ON project_conversation_attachments(service_instance_id, project_id, state);

CREATE TABLE project_conversation_attachment_chunks (
  service_instance_id uuid NOT NULL,
  project_id text NOT NULL,
  message_id text NOT NULL,
  client_attachment_id text NOT NULL,
  sequence integer NOT NULL CHECK (sequence BETWEEN 0 AND 74),
  size_bytes integer NOT NULL CHECK (size_bytes BETWEEN 1 AND 4194304),
  sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
  body bytea NOT NULL CHECK (octet_length(body)=size_bytes),
  PRIMARY KEY (message_id, client_attachment_id, sequence),
  FOREIGN KEY (service_instance_id, project_id, message_id, client_attachment_id)
    REFERENCES project_conversation_attachments(service_instance_id, project_id, message_id, client_attachment_id) ON DELETE CASCADE
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM project_conversation_attachments) THEN
    RAISE EXCEPTION 'cannot remove project conversation attachments while data exists';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE project_conversation_attachment_chunks;
DROP TABLE project_conversation_attachments;
