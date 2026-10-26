-- name: AcquireHostActivityLease :execrows
WITH expired_leases AS (
  DELETE FROM host_activity_leases AS lease
  WHERE lease.host_id = sqlc.arg(host_id)
    AND lease.lease_id <> sqlc.arg(lease_id)
    AND lease.expires_at <= clock_timestamp()
),
current_host AS (
  SELECT id
  FROM hosts
  WHERE id = sqlc.arg(host_id)
    AND state = 'running'
    AND deleted_at IS NULL
    AND runtime_generation = sqlc.arg(runtime_generation)
    AND control_version = sqlc.arg(control_version)
  FOR SHARE
)
INSERT INTO host_activity_leases (
  host_id, lease_id, runtime_generation, control_version, kind, expires_at
)
SELECT current_host.id, sqlc.arg(lease_id), sqlc.arg(runtime_generation), sqlc.arg(control_version), sqlc.arg(kind),
       clock_timestamp() + sqlc.arg(ttl_seconds)::double precision * interval '1 second'
FROM current_host
ON CONFLICT (host_id, lease_id) DO UPDATE
SET expires_at = EXCLUDED.expires_at,
    updated_at = clock_timestamp()
WHERE host_activity_leases.runtime_generation = EXCLUDED.runtime_generation
  AND host_activity_leases.control_version = EXCLUDED.control_version
  AND host_activity_leases.kind = EXCLUDED.kind;

-- name: RenewHostActivityLease :execrows
WITH current_host AS (
  SELECT id
  FROM hosts
  WHERE id = sqlc.arg(host_id)
    AND state = 'running'
    AND deleted_at IS NULL
    AND runtime_generation = sqlc.arg(runtime_generation)
    AND control_version = sqlc.arg(control_version)
  FOR SHARE
)
UPDATE host_activity_leases AS lease
SET expires_at = clock_timestamp() + sqlc.arg(ttl_seconds)::double precision * interval '1 second',
    updated_at = clock_timestamp()
FROM current_host
WHERE lease.host_id = current_host.id
  AND lease.lease_id = sqlc.arg(lease_id)
  AND lease.runtime_generation = sqlc.arg(runtime_generation)
  AND lease.control_version = sqlc.arg(control_version)
  AND lease.kind = sqlc.arg(kind)
  AND lease.expires_at > clock_timestamp();

-- name: ReleaseHostActivityLease :exec
DELETE FROM host_activity_leases
WHERE host_id = sqlc.arg(host_id)
  AND lease_id = sqlc.arg(lease_id)
  AND runtime_generation = sqlc.arg(runtime_generation)
  AND control_version = sqlc.arg(control_version)
  AND kind = sqlc.arg(kind);

-- name: HasActiveHostActivityLease :one
SELECT EXISTS (
  SELECT 1
  FROM host_activity_leases AS lease
  JOIN hosts AS host ON host.id = lease.host_id
  WHERE lease.host_id = sqlc.arg(host_id)
    AND lease.runtime_generation = sqlc.arg(runtime_generation)
    AND lease.runtime_generation = host.runtime_generation
    AND lease.control_version = host.control_version
    AND lease.expires_at > clock_timestamp()
    AND host.state IN ('running','stopping','deleting')
    AND host.deleted_at IS NULL
);

-- name: HasOutstandingControlTransitionLeases :one
SELECT EXISTS (
  SELECT 1
  FROM host_activity_leases AS lease
  JOIN hosts AS host ON host.id = lease.host_id
  WHERE lease.host_id = sqlc.arg(host_id)
    AND lease.runtime_generation = host.runtime_generation
    AND lease.control_version = sqlc.arg(expected_control_version)
    AND host.control_version = sqlc.arg(expected_control_version)::bigint + 1
    AND host.control_state IN ('taking_over', 'returning')
    AND lease.expires_at > clock_timestamp()
    AND host.state = 'running'
    AND host.deleted_at IS NULL
);
