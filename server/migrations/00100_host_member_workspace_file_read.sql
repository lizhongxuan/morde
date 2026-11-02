-- +goose Up
ALTER TABLE host_members DROP CONSTRAINT host_members_permissions_check;
ALTER TABLE host_members ADD CONSTRAINT host_members_permissions_check
  CHECK (cardinality(permissions) BETWEEN 1 AND 5);
ALTER TABLE host_members DROP CONSTRAINT host_members_permissions_check1;
ALTER TABLE host_members ADD CONSTRAINT host_members_permission_values_check
  CHECK (permissions <@ ARRAY['desktop:view', 'desktop:control', 'terminal:control', 'mcp:control', 'workspace:file:read']::text[]);

-- +goose Down
-- Rolling back this grant removes it from mixed grants and deletes rows that
-- would otherwise have no permissions. Existing Host access is not widened.
DELETE FROM host_members WHERE permissions <@ ARRAY['workspace:file:read']::text[];
UPDATE host_members SET permissions = array_remove(permissions, 'workspace:file:read')
WHERE 'workspace:file:read' = ANY(permissions);
ALTER TABLE host_members DROP CONSTRAINT host_members_permission_values_check;
ALTER TABLE host_members ADD CONSTRAINT host_members_permissions_check1
  CHECK (permissions <@ ARRAY['desktop:view', 'desktop:control', 'terminal:control', 'mcp:control']::text[]);
ALTER TABLE host_members DROP CONSTRAINT host_members_permissions_check;
ALTER TABLE host_members ADD CONSTRAINT host_members_permissions_check
  CHECK (cardinality(permissions) BETWEEN 1 AND 4);
