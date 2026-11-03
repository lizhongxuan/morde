-- Read-only inventory before enforcing one non-detached Workspace per Host.
-- Preserve every Host, Workspace, and legacy volume until owners resolve any ambiguity.
SELECT h.id AS host_id,
       h.owner_id,
       h.state AS host_state,
       COALESCE((
         SELECT array_agg(b.workspace_id ORDER BY b.workspace_id, b.state)
         FROM workspace_bindings b
         WHERE b.host_id = h.id
           AND b.state IN ('preparing', 'active', 'unbinding', 'failed')
       ), '{}'::text[]) AS non_detached_workspace_ids,
       (
         SELECT count(*)
         FROM workspace_bindings b
         WHERE b.host_id = h.id
           AND b.state IN ('preparing', 'active', 'unbinding', 'failed')
       ) AS non_detached_binding_count,
       COALESCE((
         SELECT array_agg(v.workspace_id ORDER BY v.workspace_id)
         FROM workspace_volumes v
         WHERE v.legacy_host_id = h.id
           AND v.state = 'legacy'
       ), '{}'::text[]) AS legacy_workspace_ids,
       (
         SELECT count(*)
         FROM workspace_volumes v
         WHERE v.legacy_host_id = h.id
           AND v.state = 'legacy'
       ) AS legacy_workspace_count
FROM hosts h
ORDER BY h.owner_id, h.id;

-- Legacy Workspace volumes without a matching Host owner must be assigned manually.
SELECT v.workspace_id,
       v.owner_id,
       v.legacy_host_id
FROM workspace_volumes v
LEFT JOIN hosts h ON h.id = v.legacy_host_id
WHERE v.state = 'legacy'
  AND (v.legacy_host_id IS NULL OR h.id IS NULL OR v.owner_id <> h.owner_id)
ORDER BY v.owner_id, v.workspace_id;
