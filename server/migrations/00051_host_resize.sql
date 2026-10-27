-- +goose Up
ALTER TABLE hosts
  ADD COLUMN resource_version bigint NOT NULL DEFAULT 1 CHECK (resource_version > 0),
  ADD COLUMN resource_update_state text NOT NULL DEFAULT 'idle'
    CHECK (resource_update_state IN ('idle', 'applying', 'unknown')),
  ADD COLUMN resource_target_cpu_milli integer CHECK (resource_target_cpu_milli > 0),
  ADD COLUMN resource_target_mem_mib integer CHECK (resource_target_mem_mib > 0),
  ADD CONSTRAINT hosts_resource_target_state_check CHECK (
    (resource_update_state = 'idle' AND resource_target_cpu_milli IS NULL AND resource_target_mem_mib IS NULL)
    OR (resource_update_state <> 'idle' AND resource_target_cpu_milli IS NOT NULL AND resource_target_mem_mib IS NOT NULL)
  );

-- +goose Down
ALTER TABLE hosts
  DROP CONSTRAINT hosts_resource_target_state_check,
  DROP COLUMN resource_target_mem_mib,
  DROP COLUMN resource_target_cpu_milli,
  DROP COLUMN resource_update_state,
  DROP COLUMN resource_version;
