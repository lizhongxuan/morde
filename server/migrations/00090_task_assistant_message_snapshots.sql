-- +goose Up
-- Keep a bounded copy of the latest assistant model turn so Task snapshots
-- remain useful after clients have fallen behind the event cursor.
CREATE TABLE task_assistant_message_snapshots (
  task_id text PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
  message_id text NOT NULL CHECK (message_id ~ '^m[a-z0-9]{7}$'),
  turn_id text NOT NULL CHECK (turn_id ~ '^s[a-z0-9]{7}$'),
  content text NOT NULL CHECK (octet_length(content) BETWEEN 1 AND 32768),
  status text NOT NULL CHECK (status IN ('streaming', 'completed', 'interrupted')),
  message_sequence bigint CHECK (message_sequence IS NULL OR message_sequence > 0),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  CHECK ((status = 'completed') = (message_sequence IS NOT NULL))
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM task_assistant_message_snapshots) THEN
    RAISE EXCEPTION 'cannot roll back assistant message snapshots while Task text is retained';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE task_assistant_message_snapshots;
