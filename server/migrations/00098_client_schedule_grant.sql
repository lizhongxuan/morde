-- +goose Up
ALTER TABLE tokens DROP CONSTRAINT tokens_grants_api_key_only;
ALTER TABLE tokens ADD CONSTRAINT tokens_grants_api_key_only CHECK (
  (kind = 'api_key' AND cardinality(workspace_ids) = 0 AND NOT ('workspace:sync' = ANY(scopes)))
  OR (kind = 'device' AND host_id IS NULL AND cardinality(host_ids) = 0
      AND scopes = ARRAY['workspace:sync']::text[] AND cardinality(workspace_ids) <= 500)
  OR (kind = 'client' AND client_installation_id IS NOT NULL AND host_id IS NULL
      AND cardinality(host_ids) = 0 AND cardinality(workspace_ids) = 0
      AND (cardinality(scopes) = 0 OR scopes = ARRAY['schedule:read','schedule:manage']::text[]))
  OR (kind NOT IN ('api_key', 'device', 'client') AND cardinality(host_ids) = 0
      AND cardinality(workspace_ids) = 0 AND cardinality(scopes) = 0)
);

ALTER TABLE schedules DROP CONSTRAINT schedules_auth_kind_check;
ALTER TABLE schedules ADD CONSTRAINT schedules_auth_kind_check CHECK (auth_kind IN ('session', 'client'));
ALTER TABLE schedules ADD COLUMN auth_client_installation_id text REFERENCES client_installations(id);
ALTER TABLE schedules ADD CONSTRAINT schedules_client_authorization_check CHECK (
  (auth_kind = 'client' AND auth_client_installation_id IS NOT NULL
    AND auth_scopes = ARRAY['schedule:read','schedule:manage']::text[])
  OR (auth_kind <> 'client' AND auth_client_installation_id IS NULL)
);

-- +goose Down
ALTER TABLE schedules DROP CONSTRAINT schedules_client_authorization_check;
ALTER TABLE schedules DROP COLUMN auth_client_installation_id;
ALTER TABLE schedules DROP CONSTRAINT schedules_auth_kind_check;
ALTER TABLE schedules ADD CONSTRAINT schedules_auth_kind_check CHECK (auth_kind IN ('session', 'api_key'));
ALTER TABLE tokens DROP CONSTRAINT tokens_grants_api_key_only;
ALTER TABLE tokens ADD CONSTRAINT tokens_grants_api_key_only CHECK (
  (kind = 'api_key' AND cardinality(workspace_ids) = 0 AND NOT ('workspace:sync' = ANY(scopes)))
  OR (kind = 'device' AND host_id IS NULL AND cardinality(host_ids) = 0
      AND scopes = ARRAY['workspace:sync']::text[] AND cardinality(workspace_ids) <= 500)
  OR (kind = 'client' AND client_installation_id IS NOT NULL AND host_id IS NULL
      AND cardinality(host_ids) = 0 AND cardinality(workspace_ids) = 0 AND cardinality(scopes) = 0)
  OR (kind NOT IN ('api_key', 'device', 'client') AND cardinality(host_ids) = 0
      AND cardinality(workspace_ids) = 0 AND cardinality(scopes) = 0)
);
