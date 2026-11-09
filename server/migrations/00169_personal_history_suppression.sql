-- +goose Up
-- Keep ordinary conversation history intact when a personal memory is deleted,
-- while preventing matching historical messages from being sent back to DSH.
CREATE TABLE personal_history_suppressions (
  service_instance_id uuid NOT NULL,
  owner_id text NOT NULL,
  message_id text NOT NULL REFERENCES conversation_messages(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (service_instance_id, owner_id, message_id),
  FOREIGN KEY (service_instance_id, owner_id)
    REFERENCES account_change_counters(service_instance_id, owner_id) ON DELETE CASCADE
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM personal_history_suppressions) THEN
    RAISE EXCEPTION 'cannot roll back personal history suppression while deleted-memory tombstones exist';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE personal_history_suppressions;
