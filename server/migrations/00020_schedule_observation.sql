-- +goose Up
ALTER TABLE schedules
  ADD COLUMN observation_spec jsonb,
  ADD COLUMN observation_config_hash text NOT NULL DEFAULT '',
  ADD COLUMN baseline_fingerprint text NOT NULL DEFAULT '';

ALTER TABLE schedules ADD CONSTRAINT schedules_observation_state_check CHECK (
  (observation_spec IS NULL AND observation_config_hash = '' AND baseline_fingerprint = '')
  OR (observation_spec IS NOT NULL
      AND jsonb_typeof(observation_spec) = 'object'
      AND observation_spec ?& ARRAY['url', 'selector', 'field']
      AND (observation_spec - ARRAY['url', 'selector', 'field']) = '{}'::jsonb
      AND jsonb_typeof(observation_spec->'url') = 'string'
      AND octet_length(observation_spec->>'url') BETWEEN 1 AND 2048
      AND jsonb_typeof(observation_spec->'selector') = 'string'
      AND octet_length(observation_spec->>'selector') BETWEEN 1 AND 1024
      AND jsonb_typeof(observation_spec->'field') = 'string'
      AND observation_spec->>'field' IN ('text', 'value')
      AND observation_config_hash ~ '^[0-9a-f]{64}$'
      AND (baseline_fingerprint = '' OR baseline_fingerprint ~ '^[0-9a-f]{64}$'))
);

ALTER TABLE schedule_runs
  ADD COLUMN observation_spec jsonb,
  ADD COLUMN observation_config_hash text NOT NULL DEFAULT '',
  ADD COLUMN observation_outcome text NOT NULL DEFAULT 'unknown'
    CHECK (observation_outcome IN ('unknown', 'baseline', 'changed', 'no_change')),
  ADD COLUMN observation_fingerprint text NOT NULL DEFAULT '';

ALTER TABLE schedule_runs ADD CONSTRAINT schedule_runs_observation_state_check CHECK (
  (observation_spec IS NULL AND observation_config_hash = '' AND observation_fingerprint = '' AND observation_outcome = 'unknown')
  OR (observation_spec IS NOT NULL
      AND jsonb_typeof(observation_spec) = 'object'
      AND observation_spec ?& ARRAY['url', 'selector', 'field']
      AND (observation_spec - ARRAY['url', 'selector', 'field']) = '{}'::jsonb
      AND jsonb_typeof(observation_spec->'url') = 'string'
      AND octet_length(observation_spec->>'url') BETWEEN 1 AND 2048
      AND jsonb_typeof(observation_spec->'selector') = 'string'
      AND octet_length(observation_spec->>'selector') BETWEEN 1 AND 1024
      AND jsonb_typeof(observation_spec->'field') = 'string'
      AND observation_spec->>'field' IN ('text', 'value')
      AND observation_config_hash ~ '^[0-9a-f]{64}$'
      AND (observation_fingerprint = '' OR observation_fingerprint ~ '^[0-9a-f]{64}$'))
);

-- +goose Down
ALTER TABLE schedule_runs DROP CONSTRAINT schedule_runs_observation_state_check;
ALTER TABLE schedule_runs DROP COLUMN observation_fingerprint, DROP COLUMN observation_outcome,
  DROP COLUMN observation_config_hash, DROP COLUMN observation_spec;
ALTER TABLE schedules DROP CONSTRAINT schedules_observation_state_check;
ALTER TABLE schedules DROP COLUMN baseline_fingerprint, DROP COLUMN observation_config_hash, DROP COLUMN observation_spec;
