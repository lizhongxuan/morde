-- +goose Up
CREATE TABLE schedules (
  id                   text PRIMARY KEY CHECK (id ~ '^j[a-z0-9]{7}$'),
  owner_id             text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  version              bigint NOT NULL CHECK (version > 0),
  status               text NOT NULL CHECK (status IN ('enabled', 'disabled', 'completed')),
  execution_epoch      bigint NOT NULL CHECK (execution_epoch > 0),
  host_id              text NOT NULL,
  model_id             text NOT NULL CHECK (model_id <> ''),
  objective            text NOT NULL CHECK (length(btrim(objective)) > 0 AND octet_length(objective) <= 32768),
  timing               jsonb NOT NULL CHECK (jsonb_typeof(timing) = 'object'),
  budget_active_seconds integer NOT NULL CHECK (budget_active_seconds BETWEEN 1 AND 1800),
  budget_tool_calls     integer NOT NULL CHECK (budget_tool_calls BETWEEN 1 AND 100),
  budget_model_tokens   integer NOT NULL CHECK (budget_model_tokens BETWEEN 1 AND 50000),
  auth_kind             text NOT NULL CHECK (auth_kind IN ('session', 'api_key')),
  auth_token_id         text NOT NULL DEFAULT '',
  auth_scopes           text[] NOT NULL DEFAULT '{}',
  misfire_policy        text NOT NULL CHECK (misfire_policy IN ('skip', 'run_once')),
  notification_events  text[] NOT NULL DEFAULT ARRAY['completed', 'failed', 'waiting_user']::text[]
    CHECK (notification_events <@ ARRAY['completed', 'failed', 'waiting_user']::text[]),
  next_occurrence_key  text NOT NULL DEFAULT '',
  next_run_at           timestamptz,
  next_check_at         timestamptz,
  created_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at            timestamptz NOT NULL DEFAULT clock_timestamp(),
  UNIQUE (id, owner_id),
  FOREIGN KEY (host_id, owner_id) REFERENCES hosts(id, owner_id),
  CHECK ((status = 'enabled' AND next_occurrence_key <> '' AND next_check_at IS NOT NULL)
      OR (status <> 'enabled' AND next_occurrence_key = '' AND next_run_at IS NULL AND next_check_at IS NULL)),
  CHECK (next_run_at IS NULL OR next_check_at IS NOT NULL)
);

CREATE INDEX schedules_due ON schedules(next_check_at, id) WHERE status = 'enabled';
CREATE INDEX schedules_owner_created ON schedules(owner_id, created_at DESC, id DESC);

CREATE TABLE schedule_runs (
  id                   text PRIMARY KEY CHECK (id ~ '^y[a-z0-9]{7}$'),
  schedule_id          text NOT NULL,
  owner_id             text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  schedule_version     bigint NOT NULL CHECK (schedule_version > 0),
  occurrence_key       text NOT NULL CHECK (occurrence_key <> ''),
  scheduled_at         timestamptz,
  coalesced_count      bigint NOT NULL DEFAULT 0 CHECK (coalesced_count >= 0),
  status               text NOT NULL CHECK (status IN (
    'queued', 'running', 'waiting_user', 'pausing', 'paused', 'stopping',
    'completed', 'failed', 'cancelled', 'skipped', 'missed'
  )),
  skip_reason          text CHECK (skip_reason IS NULL OR skip_reason IN ('dst_gap', 'overlap')),
  task_id              text UNIQUE REFERENCES tasks(id),
  notification_events text[] NOT NULL DEFAULT ARRAY['completed', 'failed', 'waiting_user']::text[]
    CHECK (notification_events <@ ARRAY['completed', 'failed', 'waiting_user']::text[]),
  created_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
  updated_at           timestamptz NOT NULL DEFAULT clock_timestamp(),
  finished_at          timestamptz,
  UNIQUE (schedule_id, occurrence_key),
  FOREIGN KEY (schedule_id, owner_id) REFERENCES schedules(id, owner_id) ON DELETE CASCADE,
  CHECK ((status = 'skipped') = (skip_reason IS NOT NULL)),
  CHECK ((scheduled_at IS NOT NULL OR skip_reason = 'dst_gap') IS TRUE),
  CHECK ((status IN ('skipped', 'missed') AND task_id IS NULL)
      OR (status NOT IN ('skipped', 'missed') AND task_id IS NOT NULL))
);

CREATE INDEX schedule_runs_owner_history ON schedule_runs(owner_id, schedule_id, created_at DESC, id DESC);
CREATE INDEX schedule_runs_active ON schedule_runs(schedule_id, status) WHERE task_id IS NOT NULL;

-- +goose Down
DROP TABLE schedule_runs;
DROP TABLE schedules;
