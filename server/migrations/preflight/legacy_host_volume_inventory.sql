-- Read-only candidate inventory for old independent Host volumes.
-- Candidate names and database bindings are not proof of Docker volume identity.
-- Compare these rows with a local Docker volume listing and require an explicit
-- owner, destination, credential, and rollback decision before migrating data.
WITH host_inventory AS (
  SELECT h.id AS host_id,
         h.owner_id,
         h.state AS host_state,
         COALESCE((
           SELECT array_agg(b.workspace_id ORDER BY b.workspace_id)
           FROM workspace_bindings b
           WHERE b.host_id = h.id
             AND b.state IN ('preparing', 'active', 'unbinding', 'failed')
         ), '{}'::text[]) AS bound_workspace_ids,
         COALESCE((
           SELECT array_agg(v.workspace_id ORDER BY v.workspace_id)
           FROM workspace_volumes v
           WHERE v.legacy_host_id = h.id AND v.state = 'legacy'
         ), '{}'::text[]) AS legacy_workspace_ids,
         (
           SELECT count(*)
           FROM workspace_bindings b
           WHERE b.host_id = h.id
             AND b.state IN ('preparing', 'active', 'unbinding', 'failed')
         ) AS binding_count
  FROM hosts h
), host_volumes AS (
  SELECT h.host_id,
         h.owner_id,
         h.host_state,
         v.volume_kind,
         v.volume_name,
         h.bound_workspace_ids,
         h.legacy_workspace_ids,
         h.binding_count
  FROM host_inventory h
  CROSS JOIN LATERAL (VALUES
    ('workspace_root'::text, 'workspaces-' || h.host_id),
    ('profile'::text, 'profile-' || h.host_id),
    ('runtime'::text, 'runtime-' || h.host_id),
    ('apps'::text, 'apps-' || h.host_id)
  ) AS v(volume_kind, volume_name)
), compose_volumes AS (
  SELECT NULL::text AS host_id,
         NULL::text AS owner_id,
         NULL::text AS host_state,
         v.volume_kind,
         v.volume_name,
         '{}'::text[] AS bound_workspace_ids,
         '{}'::text[] AS legacy_workspace_ids,
         0::bigint AS binding_count
  FROM (VALUES
    ('compose_workspace_root'::text, 'workspace-root'::text),
    ('compose_profile'::text, 'profile-data'::text),
    ('compose_runtime'::text, 'runtime-data'::text),
    ('compose_apps'::text, 'host-apps'::text)
  ) AS v(volume_kind, volume_name)
)
SELECT volume_kind, volume_name, owner_id, host_id, host_state,
       bound_workspace_ids, legacy_workspace_ids, binding_count
FROM (
  SELECT * FROM host_volumes
  UNION ALL
  SELECT * FROM compose_volumes
) AS candidates
ORDER BY owner_id NULLS LAST, host_id NULLS LAST, volume_kind;
