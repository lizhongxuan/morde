-- +goose Up
CREATE TABLE sites (
  id                  text PRIMARY KEY CHECK (id ~ '^site_[a-f0-9]{16}$'),
  owner_id            text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  host_id             text NOT NULL,
  workspace_id        text NOT NULL,
  state               text NOT NULL CHECK (state IN ('active', 'draining', 'stopped')),
  current_release_id  text,
  rollback_release_id text,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  UNIQUE (id, workspace_id),
  CONSTRAINT sites_host_owner_fk FOREIGN KEY (host_id, owner_id) REFERENCES hosts(id, owner_id) ON DELETE RESTRICT,
  CONSTRAINT sites_workspace_owner_fk FOREIGN KEY (workspace_id, owner_id) REFERENCES workspaces(id, owner_id) ON DELETE RESTRICT,
  CHECK (current_release_id IS NULL OR rollback_release_id IS NULL OR current_release_id <> rollback_release_id)
);
CREATE INDEX sites_host_state ON sites(host_id, state);

CREATE TABLE site_releases (
  site_id            text NOT NULL,
  id                 text NOT NULL UNIQUE CHECK (id ~ '^rel_[a-f0-9]{16}$'),
  workspace_id       text NOT NULL,
  source_revision    bigint NOT NULL CHECK (source_revision > 0),
  manifest_sha256    text NOT NULL CHECK (manifest_sha256 ~ '^[a-f0-9]{64}$'),
  service_port       integer NOT NULL CHECK (service_port BETWEEN 1 AND 65535),
  state              text NOT NULL CHECK (state IN ('candidate', 'healthy', 'failed', 'cancelled')),
  created_at         timestamptz NOT NULL DEFAULT now(),
  updated_at         timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (site_id, id),
  CONSTRAINT site_releases_site_workspace_fk FOREIGN KEY (site_id, workspace_id) REFERENCES sites(id, workspace_id) ON DELETE RESTRICT,
  CONSTRAINT site_releases_source_revision_fk FOREIGN KEY (workspace_id, source_revision) REFERENCES workspace_revisions(workspace_id, revision) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX site_releases_one_candidate_per_site ON site_releases(site_id) WHERE state = 'candidate';
CREATE INDEX site_releases_source_revision ON site_releases(workspace_id, source_revision);

ALTER TABLE sites
  ADD CONSTRAINT sites_current_release_fk FOREIGN KEY (id, current_release_id)
    REFERENCES site_releases(site_id, id) ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
  ADD CONSTRAINT sites_rollback_release_fk FOREIGN KEY (id, rollback_release_id)
    REFERENCES site_releases(site_id, id) ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED;

CREATE TABLE host_site_drains (
  host_id     text PRIMARY KEY REFERENCES hosts(id) ON DELETE RESTRICT,
  created_at  timestamptz NOT NULL DEFAULT now(),
  updated_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
-- +goose StatementBegin
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM site_releases) OR EXISTS (SELECT 1 FROM sites) OR EXISTS (SELECT 1 FROM host_site_drains) THEN
    RAISE EXCEPTION 'cannot roll back site release persistence while site data exists';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE host_site_drains;
ALTER TABLE sites DROP CONSTRAINT sites_current_release_fk, DROP CONSTRAINT sites_rollback_release_fk;
DROP TABLE site_releases;
DROP TABLE sites;
