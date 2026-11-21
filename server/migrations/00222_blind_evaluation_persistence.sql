-- +goose Up
CREATE TABLE blind_evaluation_packs (
  pack_id text PRIMARY KEY CHECK (pack_id ~ '^[0-9a-f]{32}$'),
  suite_version text NOT NULL,
  suite_json jsonb NOT NULL,
  pack_json jsonb NOT NULL,
  created_by text NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE blind_evaluation_mappings (
  pack_id text PRIMARY KEY REFERENCES blind_evaluation_packs(pack_id) ON DELETE CASCADE,
  mapping_json jsonb NOT NULL
);

CREATE TABLE blind_evaluation_run_manifests (
  pack_id text NOT NULL REFERENCES blind_evaluation_packs(pack_id) ON DELETE CASCADE,
  run_id text NOT NULL,
  manifest_json jsonb NOT NULL,
  PRIMARY KEY (pack_id, run_id)
);

CREATE TABLE blind_evaluation_participants (
  pack_id text NOT NULL REFERENCES blind_evaluation_packs(pack_id) ON DELETE CASCADE,
  user_id text NOT NULL REFERENCES users(id),
  role text NOT NULL CHECK (role IN ('implementer', 'reviewer', 'arbitrator')),
  PRIMARY KEY (pack_id, user_id, role)
);

CREATE INDEX blind_evaluation_participants_user_role
  ON blind_evaluation_participants(user_id, role, pack_id);

CREATE TABLE blind_evaluation_reviews (
  pack_id text NOT NULL,
  candidate_label text NOT NULL,
  reviewer_id text NOT NULL,
  role text NOT NULL DEFAULT 'reviewer' CHECK (role = 'reviewer'),
  review_json jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (pack_id, candidate_label, reviewer_id),
  FOREIGN KEY (pack_id, reviewer_id, role)
    REFERENCES blind_evaluation_participants(pack_id, user_id, role) ON DELETE CASCADE
);

CREATE TABLE blind_evaluation_arbitrations (
  pack_id text NOT NULL,
  candidate_label text NOT NULL,
  arbitrator_id text NOT NULL,
  role text NOT NULL DEFAULT 'arbitrator' CHECK (role = 'arbitrator'),
  arbitration_json jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  PRIMARY KEY (pack_id, candidate_label),
  FOREIGN KEY (pack_id, arbitrator_id, role)
    REFERENCES blind_evaluation_participants(pack_id, user_id, role) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE blind_evaluation_arbitrations;
DROP TABLE blind_evaluation_reviews;
DROP TABLE blind_evaluation_participants;
DROP TABLE blind_evaluation_run_manifests;
DROP TABLE blind_evaluation_mappings;
DROP TABLE blind_evaluation_packs;
