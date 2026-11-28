-- +goose Up
CREATE TABLE observability_trace_links (
  trace_id   text NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$'),
  span_id    text NOT NULL CHECK (span_id ~ '^[0-9a-f]{16}$'),
  request_id text NOT NULL CHECK (octet_length(request_id) BETWEEN 1 AND 255),
  message_id text NOT NULL REFERENCES conversation_messages(id) ON DELETE CASCADE,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (trace_id,span_id,message_id)
);

CREATE INDEX observability_trace_links_message ON observability_trace_links(message_id,created_at DESC);
CREATE INDEX observability_trace_links_retention ON observability_trace_links(created_at,trace_id,span_id);

-- +goose Down
DROP INDEX observability_trace_links_retention;
DROP INDEX observability_trace_links_message;
DROP TABLE observability_trace_links;
