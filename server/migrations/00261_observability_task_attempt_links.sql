-- +goose Up
CREATE TABLE observability_task_attempt_links (
  task_id         text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  attempt         bigint NOT NULL CHECK (attempt > 0),
  origin_trace_id text NOT NULL CHECK (origin_trace_id ~ '^[0-9a-f]{32}$'),
  origin_span_id  text NOT NULL CHECK (origin_span_id ~ '^[0-9a-f]{16}$'),
  task_trace_id   text NOT NULL CHECK (task_trace_id ~ '^[0-9a-f]{32}$'),
  task_span_id    text NOT NULL CHECK (task_span_id ~ '^[0-9a-f]{16}$'),
  request_id      text NOT NULL CHECK (octet_length(request_id) BETWEEN 1 AND 255),
  message_id      text NOT NULL REFERENCES conversation_messages(id) ON DELETE CASCADE,
  created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (task_id, attempt),
  UNIQUE (task_trace_id, task_span_id)
);

CREATE INDEX observability_task_attempt_links_origin ON observability_task_attempt_links(origin_trace_id, created_at DESC);
CREATE INDEX observability_task_attempt_links_retention ON observability_task_attempt_links(created_at, task_id, attempt);

-- +goose Down
DROP INDEX observability_task_attempt_links_retention;
DROP INDEX observability_task_attempt_links_origin;
DROP TABLE observability_task_attempt_links;
