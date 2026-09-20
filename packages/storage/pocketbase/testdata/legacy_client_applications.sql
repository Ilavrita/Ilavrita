-- The client_applications table as it stood before this server had OAuth
-- endpoints. Every column and check is the release's own; the only difference
-- from the current declaration is that it cannot say which proof a registration
-- offers.
CREATE TABLE client_applications (
  project_id  TEXT NOT NULL REFERENCES projects (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  id          TEXT NOT NULL,
  name        TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  state       TEXT NOT NULL CHECK (state IN ('active', 'suspended', 'revoked')),
  created_at  BIGINT NOT NULL,
  updated_at  BIGINT NOT NULL,
  revoked_at  BIGINT,
  version     BIGINT NOT NULL DEFAULT 1,

  PRIMARY KEY (project_id, id),

  CHECK (substr(id, 1, 4) = 'cli_'),

  CHECK (project_id <> '' AND project_id <> 'system'),
  CHECK (name <> ''),
  CHECK (state <> 'revoked' OR revoked_at IS NOT NULL)
);

CREATE UNIQUE INDEX ux_client_applications_name
  ON client_applications (project_id, name);

CREATE INDEX ix_client_applications_state
  ON client_applications (project_id, state, id);
