-- +goose Up
CREATE TABLE host_usage_samples (
  id bigserial PRIMARY KEY,
  host_id text NOT NULL REFERENCES hosts(id) ON DELETE RESTRICT,
  runtime_generation bigint NOT NULL CHECK (runtime_generation > 0),
  sandbox_id text NOT NULL DEFAULT '',
  owner_id text NOT NULL,
  observed_at timestamptz NOT NULL,
  sampled_at timestamptz,
  state text NOT NULL CHECK (state IN ('sampled', 'unknown')),
  reason text CHECK (reason IS NULL OR reason IN (
    'no_previous_sample', 'sampling_failed', 'stale_sample', 'sampling_gap',
    'generation_changed', 'host_not_running', 'counter_reset', 'container_replaced'
  )),
  source text NOT NULL DEFAULT 'docker-engine-cgroup' CHECK (source = 'docker-engine-cgroup'),
  container_id text,
  cpu_usage_total_ns bigint,
  memory_usage_bytes bigint,
  raw_payload bytea,
  attribution_state text NOT NULL CHECK (attribution_state IN ('unknown', 'project')),
  attribution_reason text CHECK (attribution_reason IS NULL OR attribution_reason IN (
    'no_active_workspace_binding', 'multiple_active_workspace_bindings',
    'no_active_project_resource', 'multiple_active_project_resources', 'generation_changed'
  )),
  workspace_id text,
  workspace_binding_id text,
  workspace_binding_version bigint,
  project_id text,
  project_resource_version bigint,
  actor_id text,
  CHECK (
    (sampled_at IS NULL AND container_id IS NULL AND cpu_usage_total_ns IS NULL AND memory_usage_bytes IS NULL AND raw_payload IS NULL)
    OR
    (sampled_at IS NOT NULL AND container_id IS NOT NULL AND container_id ~ '^[a-f0-9]{12,64}$' AND
     cpu_usage_total_ns IS NOT NULL AND cpu_usage_total_ns >= 0 AND
     memory_usage_bytes IS NOT NULL AND memory_usage_bytes >= 0 AND raw_payload IS NOT NULL)
  ),
  CHECK (
    (state = 'sampled' AND sandbox_id <> '' AND sampled_at IS NOT NULL AND reason IS NULL)
    OR
    (state = 'unknown' AND reason IS NOT NULL)
  ),
  CHECK (
    (attribution_state = 'project' AND owner_id <> '' AND workspace_id IS NOT NULL AND
     workspace_binding_id IS NOT NULL AND workspace_binding_version > 0 AND project_id IS NOT NULL AND
     project_resource_version > 0 AND attribution_reason IS NULL AND actor_id IS NULL)
    OR
    (attribution_state = 'unknown' AND attribution_reason IS NOT NULL AND project_id IS NULL AND
     project_resource_version IS NULL AND actor_id IS NULL)
  ),
  CHECK ((workspace_binding_id IS NULL) = (workspace_binding_version IS NULL))
);
CREATE INDEX host_usage_samples_generation_time
  ON host_usage_samples(host_id, runtime_generation, observed_at, id);
CREATE INDEX host_usage_samples_latest
  ON host_usage_samples(host_id, id DESC);

CREATE TABLE host_usage_intervals (
  id bigserial PRIMARY KEY,
  host_id text NOT NULL REFERENCES hosts(id) ON DELETE RESTRICT,
  runtime_generation bigint NOT NULL CHECK (runtime_generation > 0),
  sandbox_id text NOT NULL DEFAULT '',
  start_observation_id bigint REFERENCES host_usage_samples(id) ON DELETE RESTRICT,
  end_observation_id bigint NOT NULL REFERENCES host_usage_samples(id) ON DELETE RESTRICT,
  started_at timestamptz NOT NULL,
  ended_at timestamptz NOT NULL,
  state text NOT NULL CHECK (state IN ('measured', 'unknown')),
  unknown_reason text CHECK (unknown_reason IS NULL OR unknown_reason IN (
    'no_previous_sample', 'sampling_failed', 'stale_sample', 'sampling_gap',
    'generation_changed', 'host_not_running', 'counter_reset', 'container_replaced'
  )),
  calculation_method text NOT NULL CHECK (calculation_method = 'docker_cgroup_cpu_counter_diff_and_memory_trapezoid_v1'),
  cpu_seconds double precision,
  estimated_memory_mib_seconds double precision,
  owner_id text NOT NULL,
  attribution_state text NOT NULL CHECK (attribution_state IN ('unknown', 'project')),
  attribution_reason text CHECK (attribution_reason IS NULL OR attribution_reason IN (
    'no_active_workspace_binding', 'multiple_active_workspace_bindings',
    'no_active_project_resource', 'multiple_active_project_resources', 'generation_changed', 'association_changed'
  )),
  workspace_id text,
  workspace_binding_id text,
  workspace_binding_version bigint,
  project_id text,
  project_resource_version bigint,
  actor_id text,
  CHECK (
    (state = 'measured' AND start_observation_id IS NOT NULL AND ended_at > started_at AND unknown_reason IS NULL AND
     cpu_seconds IS NOT NULL AND cpu_seconds >= 0 AND
     estimated_memory_mib_seconds IS NOT NULL AND estimated_memory_mib_seconds >= 0)
    OR
    (state = 'unknown' AND unknown_reason IS NOT NULL AND cpu_seconds IS NULL AND estimated_memory_mib_seconds IS NULL)
  ),
  CHECK (
    (attribution_state = 'project' AND owner_id <> '' AND workspace_id IS NOT NULL AND
     workspace_binding_id IS NOT NULL AND workspace_binding_version > 0 AND project_id IS NOT NULL AND
     project_resource_version > 0 AND attribution_reason IS NULL AND actor_id IS NULL)
    OR
    (attribution_state = 'unknown' AND attribution_reason IS NOT NULL AND project_id IS NULL AND
     project_resource_version IS NULL AND actor_id IS NULL)
  ),
  CHECK ((workspace_binding_id IS NULL) = (workspace_binding_version IS NULL))
);
CREATE INDEX host_usage_intervals_generation_time
  ON host_usage_intervals(host_id, runtime_generation, ended_at, id);

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM host_usage_samples) OR EXISTS (SELECT 1 FROM host_usage_intervals) THEN
    RAISE EXCEPTION 'cannot roll back Host usage metering after observations have been recorded';
  END IF;
END $$;
-- +goose StatementEnd

DROP TABLE host_usage_intervals;
DROP TABLE host_usage_samples;
