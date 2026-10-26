-- +goose Up
CREATE TABLE task_pending_requests (
  id                  text PRIMARY KEY CHECK (id ~ '^r[a-z0-9]{7}$'),
  task_id             text NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  version             bigint NOT NULL CHECK (version > 0),
  kind                text NOT NULL CHECK (kind = 'approval'),
  title               text NOT NULL CHECK (char_length(title) BETWEEN 1 AND 512),
  object              text NOT NULL CHECK (char_length(object) BETWEEN 1 AND 512),
  content             text NOT NULL CHECK (char_length(content) BETWEEN 1 AND 512),
  impact              text NOT NULL CHECK (char_length(impact) BETWEEN 1 AND 512),
  call_id             text NOT NULL CHECK (char_length(call_id) BETWEEN 1 AND 256),
  tool_name           text NOT NULL CHECK (char_length(tool_name) BETWEEN 1 AND 128),
  request_hash        bytea NOT NULL CHECK (octet_length(request_hash) = 32),
  runtime_generation  bigint NOT NULL CHECK (runtime_generation >= 0),
  runtime_instance_id text NOT NULL,
  control_version     bigint NOT NULL CHECK (control_version >= 0),
  status              text NOT NULL CHECK (status IN ('pending', 'approved', 'rejected')),
  decision            text NOT NULL DEFAULT '' CHECK (decision IN ('', 'approve', 'reject')),
  created_at          timestamptz NOT NULL DEFAULT now(),
  responded_at        timestamptz,
  UNIQUE (task_id, version),
  CHECK ((status = 'pending' AND decision = '' AND responded_at IS NULL) OR
         (status = 'approved' AND decision = 'approve' AND responded_at IS NOT NULL) OR
         (status = 'rejected' AND decision = 'reject' AND responded_at IS NOT NULL))
);
CREATE UNIQUE INDEX task_pending_requests_one_active ON task_pending_requests(task_id) WHERE status='pending';
CREATE INDEX task_pending_requests_latest ON task_pending_requests(task_id, version DESC);

-- +goose Down
DROP TABLE task_pending_requests;
