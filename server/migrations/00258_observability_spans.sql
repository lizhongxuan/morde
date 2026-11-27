-- +goose Up
CREATE TABLE observability_spans (
  trace_id            text NOT NULL CHECK (trace_id ~ '^[0-9a-f]{32}$'),
  span_id             text NOT NULL CHECK (span_id ~ '^[0-9a-f]{16}$'),
  parent_span_id      text NOT NULL DEFAULT '' CHECK (parent_span_id = '' OR parent_span_id ~ '^[0-9a-f]{16}$'),
  name                text NOT NULL CHECK (octet_length(name) BETWEEN 1 AND 256),
  span_kind           text NOT NULL CHECK (span_kind IN ('unspecified', 'internal', 'server', 'client', 'producer', 'consumer')),
  start_time          timestamptz NOT NULL,
  end_time            timestamptz NOT NULL CHECK (end_time >= start_time),
  status              text NOT NULL CHECK (status IN ('unset', 'ok', 'error')),
  scope_name          text NOT NULL DEFAULT '',
  scope_version       text NOT NULL DEFAULT '',
  attributes          jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(attributes) = 'object'),
  resource            jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(resource) = 'object'),
  links               jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(links) = 'array'),
  events              jsonb NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(events) = 'array'),
  dropped_attributes  integer NOT NULL DEFAULT 0 CHECK (dropped_attributes >= 0),
  dropped_links       integer NOT NULL DEFAULT 0 CHECK (dropped_links >= 0),
  dropped_events      integer NOT NULL DEFAULT 0 CHECK (dropped_events >= 0),
  created_at          timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (trace_id, span_id)
);

CREATE INDEX observability_spans_trace_page
  ON observability_spans(trace_id, start_time, span_id);

-- Retention only deletes spans. Encrypted DSH recovery checkpoints use their
-- existing tables and are never included in this cleanup path.
CREATE INDEX observability_spans_retention
  ON observability_spans(end_time, trace_id, span_id);

-- +goose Down
DROP TABLE observability_spans;
