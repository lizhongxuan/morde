-- +goose Up
ALTER TABLE notifications
  ADD COLUMN delivery_sequence bigint,
  ADD CONSTRAINT notifications_delivery_sequence_positive
    CHECK (delivery_sequence IS NULL OR delivery_sequence > 0);

CREATE TABLE notification_account_sequences (
  owner_id       text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  last_sequence  bigint NOT NULL DEFAULT 0 CHECK (last_sequence >= 0),
  cursor_secret  bytea NOT NULL CHECK (octet_length(cursor_secret) = 32)
);

CREATE UNIQUE INDEX notifications_owner_delivery_sequence_unique
  ON notifications(owner_id, delivery_sequence)
  WHERE delivery_sequence IS NOT NULL;

-- +goose Down
-- Stream positions and committed notices are durable; do not discard active history.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM notification_account_sequences WHERE last_sequence > 0)
     OR EXISTS (SELECT 1 FROM notifications WHERE delivery_sequence IS NOT NULL) THEN
    RAISE EXCEPTION 'cannot roll back notification incremental delivery while stream history exists';
  END IF;
END;
$$;
-- +goose StatementEnd

DROP INDEX notifications_owner_delivery_sequence_unique;
DROP TABLE notification_account_sequences;
ALTER TABLE notifications
  DROP CONSTRAINT notifications_delivery_sequence_positive,
  DROP COLUMN delivery_sequence;
