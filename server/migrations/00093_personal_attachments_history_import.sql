-- +goose Up
LOCK TABLE conversations, conversation_messages IN SHARE ROW EXCLUSIVE MODE;

ALTER TABLE conversation_messages
  ADD CONSTRAINT conversation_messages_id_owner_conversation_unique
    UNIQUE (id, owner_id, conversation_id);

CREATE TABLE personal_attachment_uploads (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  upload_id text NOT NULL CHECK (upload_id ~ '^pu_[a-f0-9]{32}$'),
  attachment_id text CHECK (attachment_id IS NULL OR attachment_id ~ '^pa_[a-f0-9]{32}$'),
  object_type text NOT NULL CHECK (object_type = 'library_item'),
  object_id text NOT NULL CHECK (octet_length(object_id) BETWEEN 1 AND 128),
  base_revision bigint NOT NULL CHECK (base_revision >= 0),
  size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 0 AND 9007199250546688),
  sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
  media_type text NOT NULL CHECK (octet_length(media_type) BETWEEN 1 AND 128),
  request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 8 AND 128),
  state text NOT NULL CHECK (state IN ('pending','active','committed','cancelled','expired')),
  reservation_id text,
  reservation_released boolean NOT NULL DEFAULT false,
  commit_idempotency_key text,
  result_revision bigint,
  change_sequence bigint,
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_instance_id, owner_id, upload_id),
  UNIQUE (service_instance_id, owner_id, idempotency_key),
  UNIQUE (service_instance_id, owner_id, upload_id, attachment_id),
  CHECK (reservation_id IS NULL OR octet_length(reservation_id) BETWEEN 8 AND 128),
  CHECK (state NOT IN ('active','committed') OR reservation_id IS NOT NULL),
  CHECK (NOT reservation_released OR (reservation_id IS NOT NULL AND state IN ('cancelled','expired'))),
  CHECK (commit_idempotency_key IS NULL OR octet_length(commit_idempotency_key) BETWEEN 8 AND 128),
  CHECK (
    (state = 'committed' AND attachment_id IS NOT NULL AND commit_idempotency_key IS NOT NULL
      AND result_revision IS NOT NULL AND result_revision > 0
      AND change_sequence IS NOT NULL AND change_sequence > 0)
    OR
    (state <> 'committed' AND attachment_id IS NULL AND commit_idempotency_key IS NULL
      AND result_revision IS NULL AND change_sequence IS NULL)
  )
);

CREATE UNIQUE INDEX personal_attachment_uploads_owner_attachment
  ON personal_attachment_uploads(service_instance_id, owner_id, attachment_id)
  WHERE attachment_id IS NOT NULL;
CREATE INDEX personal_attachment_uploads_expiry
  ON personal_attachment_uploads(service_instance_id, expires_at, upload_id)
  WHERE state IN ('pending','active');
CREATE INDEX personal_attachment_uploads_release_retry
  ON personal_attachment_uploads(service_instance_id, expires_at, upload_id)
  WHERE state IN ('cancelled','expired') AND reservation_id IS NOT NULL AND NOT reservation_released;

CREATE TABLE personal_attachment_chunks (
  service_instance_id uuid NOT NULL,
  owner_id text NOT NULL,
  upload_id text NOT NULL,
  sequence integer NOT NULL CHECK (sequence >= 0),
  size_bytes bigint NOT NULL CHECK (size_bytes BETWEEN 1 AND 4194304),
  sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
  body bytea NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_instance_id, owner_id, upload_id, sequence),
  FOREIGN KEY (service_instance_id, owner_id, upload_id)
    REFERENCES personal_attachment_uploads(service_instance_id, owner_id, upload_id) ON DELETE CASCADE,
  CHECK (octet_length(body) = size_bytes)
);

CREATE TABLE personal_history_import_mappings (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  source_id text NOT NULL CHECK (octet_length(source_id) BETWEEN 1 AND 128),
  device_account_id text NOT NULL CHECK (octet_length(device_account_id) BETWEEN 1 AND 128),
  source_conversation_id text NOT NULL CHECK (octet_length(source_conversation_id) BETWEEN 1 AND 128),
  source_message_id text NOT NULL DEFAULT '',
  requested_conversation_id text NOT NULL DEFAULT '',
  canonical_conversation_id text NOT NULL,
  canonical_message_id text,
  content_hash bytea,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_instance_id, owner_id, source_id, device_account_id, source_conversation_id, source_message_id),
  FOREIGN KEY (canonical_conversation_id, owner_id)
    REFERENCES conversations(id, owner_id) ON DELETE CASCADE,
  FOREIGN KEY (canonical_message_id, owner_id, canonical_conversation_id)
    REFERENCES conversation_messages(id, owner_id, conversation_id) ON DELETE CASCADE,
  CHECK (source_message_id = '' OR octet_length(source_message_id) BETWEEN 1 AND 128),
  CHECK (requested_conversation_id = '' OR requested_conversation_id ~ '^c[a-z0-9]{7}$'),
  CHECK (
    (source_message_id = '' AND canonical_message_id IS NULL AND content_hash IS NULL)
    OR
    (octet_length(source_message_id) BETWEEN 1 AND 128 AND canonical_message_id IS NOT NULL
      AND content_hash IS NOT NULL AND octet_length(content_hash) = 32)
  )
);

CREATE TABLE conversation_message_attachments (
  service_instance_id uuid NOT NULL REFERENCES service_identity(instance_id) ON DELETE RESTRICT,
  owner_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  conversation_id text NOT NULL,
  message_id text NOT NULL,
  upload_id text NOT NULL,
  attachment_id text NOT NULL CHECK (attachment_id ~ '^pa_[a-f0-9]{32}$'),
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_instance_id, owner_id, message_id, attachment_id),
  FOREIGN KEY (service_instance_id, owner_id, upload_id, attachment_id)
    REFERENCES personal_attachment_uploads(service_instance_id, owner_id, upload_id, attachment_id) ON DELETE RESTRICT,
  FOREIGN KEY (message_id, owner_id, conversation_id)
    REFERENCES conversation_messages(id, owner_id, conversation_id) ON DELETE CASCADE,
  FOREIGN KEY (conversation_id, owner_id)
    REFERENCES conversations(id, owner_id) ON DELETE CASCADE
);
CREATE INDEX conversation_message_attachments_owner_attachment
  ON conversation_message_attachments(service_instance_id, owner_id, attachment_id);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_message_attachments)
     OR EXISTS (SELECT 1 FROM personal_history_import_mappings)
     OR EXISTS (SELECT 1 FROM personal_attachment_chunks)
     OR EXISTS (SELECT 1 FROM personal_attachment_uploads) THEN
    RAISE EXCEPTION 'cannot roll back personal attachments while attachment or history data exists';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE conversation_message_attachments;
DROP TABLE personal_history_import_mappings;
DROP TABLE personal_attachment_chunks;
DROP TABLE personal_attachment_uploads;
ALTER TABLE conversation_messages
  DROP CONSTRAINT conversation_messages_id_owner_conversation_unique;
