-- +goose Up
-- Derived representations are committed as one immutable set beneath the
-- original message slot. Existing clients and records retain the none mode.
ALTER TABLE project_conversation_attachments
  ADD COLUMN derivative_mode text NOT NULL DEFAULT 'none'
    CHECK (derivative_mode IN ('none','pdf','image')),
  ADD COLUMN derivative_state text NOT NULL DEFAULT 'none'
    CHECK (derivative_state IN ('none','awaiting','completed','skipped')),
  ADD COLUMN derivative_fingerprint bytea,
  ADD CONSTRAINT project_attachment_derivative_state_check CHECK (
    (derivative_mode='none' AND derivative_state='none' AND derivative_fingerprint IS NULL) OR
    (derivative_mode<>'none' AND derivative_state='awaiting' AND derivative_fingerprint IS NULL) OR
    (derivative_mode<>'none' AND derivative_state IN ('completed','skipped') AND derivative_fingerprint IS NOT NULL AND octet_length(derivative_fingerprint)=32)
  );

CREATE TABLE project_conversation_attachment_derivatives (
  service_instance_id uuid NOT NULL,
  project_id text NOT NULL,
  message_id text NOT NULL,
  client_attachment_id text NOT NULL,
  attachment_id text NOT NULL CHECK (attachment_id ~ '^pca_[a-f0-9]{32}$'),
  position integer NOT NULL CHECK (position BETWEEN 0 AND 10),
  kind text NOT NULL CHECK (kind IN ('extracted_text','page_preview','image_conversion')),
  page integer NOT NULL CHECK (page BETWEEN 0 AND 10),
  name text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 255),
  media_type text NOT NULL CHECK (media_type IN ('text/plain','image/jpeg')),
  size_bytes integer NOT NULL CHECK (size_bytes BETWEEN 0 AND 524288),
  sha256 text NOT NULL CHECK (sha256 ~ '^[a-f0-9]{64}$'),
  body bytea NOT NULL CHECK (octet_length(body)=size_bytes),
  PRIMARY KEY (message_id,client_attachment_id,position),
  UNIQUE (service_instance_id,project_id,attachment_id),
  UNIQUE (message_id,client_attachment_id,kind,page),
  FOREIGN KEY (service_instance_id,project_id,message_id,client_attachment_id)
    REFERENCES project_conversation_attachments(service_instance_id,project_id,message_id,client_attachment_id) ON DELETE CASCADE
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM project_conversation_attachments WHERE derivative_mode<>'none') THEN
    RAISE EXCEPTION 'cannot remove project attachment derivatives while data exists';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE project_conversation_attachment_derivatives;
ALTER TABLE project_conversation_attachments
  DROP CONSTRAINT project_attachment_derivative_state_check,
  DROP COLUMN derivative_fingerprint,
  DROP COLUMN derivative_state,
  DROP COLUMN derivative_mode;
