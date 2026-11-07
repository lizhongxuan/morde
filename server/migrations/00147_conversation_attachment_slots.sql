-- +goose Up
-- A stable client attachment slot binds to the existing, owner-scoped message
-- attachment reference after its upload has been verified and committed.
CREATE TABLE conversation_attachment_slots (
  message_id text NOT NULL REFERENCES conversation_message_routes(message_id) ON DELETE RESTRICT,
  client_attachment_id text NOT NULL CHECK (client_attachment_id ~ '^[A-Za-z0-9._~-]{8,128}$'),
  service_instance_id uuid NOT NULL,
  owner_id text NOT NULL,
  attachment_id text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (message_id, client_attachment_id),
  FOREIGN KEY (service_instance_id, owner_id, message_id, attachment_id)
    REFERENCES conversation_message_attachments(service_instance_id, owner_id, message_id, attachment_id) ON DELETE RESTRICT
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM conversation_attachment_slots) THEN
    RAISE EXCEPTION 'cannot remove linked conversation attachment identities';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE conversation_attachment_slots;
