-- +goose Up
CREATE TABLE observability_logs (
  id          bigserial PRIMARY KEY,
  trace_id    text NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$'),
  span_id     text NOT NULL DEFAULT '' CHECK (span_id = '' OR span_id ~ '^[0-9a-f]{16}$'),
  request_id  text NOT NULL DEFAULT '' CHECK (octet_length(request_id) <= 255),
  task_id     text NOT NULL DEFAULT '' CHECK (octet_length(task_id) <= 128),
  service     text NOT NULL CHECK (octet_length(service) BETWEEN 1 AND 128),
  severity    text NOT NULL CHECK (severity IN ('debug', 'info', 'warn', 'error', 'dpanic', 'panic', 'fatal')),
  message     text NOT NULL CHECK (octet_length(message) BETWEEN 1 AND 2048),
  attributes  jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(attributes) = 'object' AND pg_column_size(attributes) <= 8192),
  occurred_at timestamptz NOT NULL
);

CREATE INDEX observability_logs_trace_page ON observability_logs(trace_id, occurred_at DESC, id DESC);
CREATE INDEX observability_logs_retention ON observability_logs(occurred_at, id);

-- +goose Down
DROP INDEX observability_logs_retention;
DROP INDEX observability_logs_trace_page;
DROP TABLE observability_logs;
