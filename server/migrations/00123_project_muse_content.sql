-- +goose Up
ALTER TABLE project_content_revisions
  DROP CONSTRAINT project_content_revisions_kind_check;
ALTER TABLE project_content_revisions
  ADD CONSTRAINT project_content_revisions_kind_check
  CHECK (kind IN ('memory', 'goal', 'document', 'feed_post', 'idea', 'library_item', 'brief'));

CREATE INDEX project_content_muse_search_scope
  ON project_content_revisions(project_id, created_at DESC, kind, content_id)
  WHERE current AND NOT deleted AND kind IN ('feed_post', 'idea', 'library_item', 'brief');

CREATE TABLE project_content_comments (
  project_id        text NOT NULL,
  content_kind      text NOT NULL CHECK (content_kind IN ('feed_post', 'idea', 'library_item', 'brief')),
  content_id        text NOT NULL,
  content_revision  bigint NOT NULL CHECK (content_revision >= 1),
  row_id            text NOT NULL CHECK (char_length(row_id) BETWEEN 1 AND 128 AND octet_length(row_id) <= 512),
  comment_id        text NOT NULL CHECK (comment_id ~ '^c[a-z0-9]{7}$'),
  version           bigint NOT NULL CHECK (version >= 1),
  body              text,
  deleted           boolean NOT NULL DEFAULT false,
  current           boolean NOT NULL DEFAULT true,
  updated_by        text NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  idempotency_key   text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9._~-]{8,128}$'),
  request_digest    bytea NOT NULL CHECK (octet_length(request_digest) = 32),
  created_at        timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (project_id, content_kind, content_id, content_revision, row_id, comment_id, version),
  FOREIGN KEY (project_id, content_kind, content_id, content_revision)
    REFERENCES project_content_revisions(project_id, kind, content_id, revision) ON DELETE CASCADE,
  UNIQUE (project_id, updated_by, idempotency_key),
  CHECK ((deleted AND body IS NULL) OR (NOT deleted AND body IS NOT NULL AND char_length(body) BETWEEN 1 AND 500))
);
CREATE UNIQUE INDEX project_content_comments_one_current
  ON project_content_comments(project_id, content_kind, content_id, content_revision, row_id, comment_id)
  WHERE current;
CREATE INDEX project_content_comments_row
  ON project_content_comments(project_id, content_kind, content_id, content_revision, row_id, created_at, comment_id)
  WHERE current AND NOT deleted;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM project_content_comments) THEN
    RAISE EXCEPTION 'cannot roll back project content comments while comment history exists';
  END IF;
  IF EXISTS (
    SELECT 1 FROM project_content_revisions
    WHERE kind IN ('feed_post', 'idea', 'library_item', 'brief')
  ) THEN
    RAISE EXCEPTION 'cannot roll back project Muse content while its history exists';
  END IF;
END $$;
-- +goose StatementEnd

DROP INDEX project_content_comments_row;
DROP INDEX project_content_comments_one_current;
DROP TABLE project_content_comments;
DROP INDEX project_content_muse_search_scope;
ALTER TABLE project_content_revisions
  DROP CONSTRAINT project_content_revisions_kind_check;
ALTER TABLE project_content_revisions
  ADD CONSTRAINT project_content_revisions_kind_check
  CHECK (kind IN ('memory', 'goal', 'document'));
