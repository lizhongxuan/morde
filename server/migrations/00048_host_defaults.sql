-- +goose Up
ALTER TABLE users
  ADD COLUMN default_host_cpu_milli integer NOT NULL DEFAULT 1000 CHECK (default_host_cpu_milli > 0),
  ADD COLUMN default_host_mem_mib integer NOT NULL DEFAULT 2048 CHECK (default_host_mem_mib > 0);

ALTER TABLE users ALTER COLUMN quota_hosts SET DEFAULT 1;

-- Preserve values explicitly saved by an administrator, including an explicit 3.
UPDATE users AS u SET quota_hosts = 1, updated_at = clock_timestamp()
WHERE u.quota_hosts = 3
  AND NOT EXISTS (
    SELECT 1 FROM audit_logs AS a
    WHERE a.action = 'user.update' AND a.target = u.id AND a.detail ? 'quota'
  );

-- +goose Down
ALTER TABLE users ALTER COLUMN quota_hosts SET DEFAULT 3;
ALTER TABLE users DROP COLUMN default_host_cpu_milli, DROP COLUMN default_host_mem_mib;
