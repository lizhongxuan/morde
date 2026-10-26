-- +goose Up
CREATE TABLE user_preferences (
  user_id              text PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  assistant_name       text NOT NULL DEFAULT '' CHECK (octet_length(assistant_name) <= 80),
  selected_model_id    text NOT NULL DEFAULT '' CHECK (selected_model_id = '' OR selected_model_id ~ '^p[a-z0-9]{7}$'),
  default_host_id      text NOT NULL DEFAULT '' CHECK (default_host_id = '' OR default_host_id ~ '^h[a-z0-9]{7}$'),
  last_host_issue      text NOT NULL DEFAULT '' CHECK (last_host_issue IN ('', 'capacity_unavailable', 'quota_exceeded')),
  updated_at           timestamptz NOT NULL DEFAULT clock_timestamp()
);

-- +goose Down
DROP TABLE user_preferences;
