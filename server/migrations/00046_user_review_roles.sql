-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM users WHERE lower(btrim(name)) = 'admin' AND name <> 'admin') THEN
    RAISE EXCEPTION 'username admin is reserved; resolve case-insensitive admin name conflicts before migrating';
  END IF;
  IF EXISTS (SELECT 1 FROM users WHERE name = 'admin' AND role <> 'admin') THEN
    RAISE EXCEPTION 'username admin belongs to a non-admin account; resolve the conflict before migrating';
  END IF;
  IF EXISTS (SELECT 1 FROM users)
     AND NOT EXISTS (SELECT 1 FROM users WHERE name = 'admin' AND role = 'admin' AND NOT disabled) THEN
    RAISE EXCEPTION 'an enabled admin account is required before migrating existing users';
  END IF;
END;
$$;
-- +goose StatementEnd

ALTER TABLE users ADD COLUMN status text;
ALTER TABLE users ADD COLUMN must_change_password boolean NOT NULL DEFAULT false;
ALTER TABLE users DROP CONSTRAINT users_role_check;

UPDATE users SET status = CASE WHEN disabled THEN 'disabled' ELSE 'pending' END;
UPDATE users
SET role = 'super_admin', status = 'approved', must_change_password = true
WHERE name = 'admin' AND role = 'admin';

ALTER TABLE users ALTER COLUMN status SET NOT NULL;
ALTER TABLE users ADD CONSTRAINT users_status_check CHECK (status IN ('pending', 'rejected', 'approved', 'disabled'));
ALTER TABLE users ADD CONSTRAINT users_role_check CHECK (role IN ('user', 'admin', 'super_admin'));
ALTER TABLE users ADD CONSTRAINT users_admin_name_check
  CHECK (lower(btrim(name)) <> 'admin' OR (name = 'admin' AND role = 'super_admin'));
ALTER TABLE users DROP COLUMN disabled;
CREATE UNIQUE INDEX users_single_super_admin ON users (role) WHERE role = 'super_admin';

UPDATE tokens SET revoked_at = now()
WHERE revoked_at IS NULL;

-- +goose Down
DROP INDEX users_single_super_admin;
ALTER TABLE users ADD COLUMN disabled boolean NOT NULL DEFAULT false;
ALTER TABLE users DROP CONSTRAINT users_admin_name_check;
UPDATE users SET disabled = status <> 'approved', role = CASE WHEN role = 'super_admin' THEN 'admin' ELSE role END;
ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users DROP COLUMN must_change_password;
ALTER TABLE users DROP COLUMN status;
ALTER TABLE users DROP CONSTRAINT users_role_check;
ALTER TABLE users ADD CONSTRAINT users_role_check CHECK (role IN ('user', 'admin'));
