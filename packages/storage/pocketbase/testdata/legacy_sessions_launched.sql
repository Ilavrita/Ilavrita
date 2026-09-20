-- The sessions table as it stood after SMART launch contexts landed and before
-- the refresh grant did. It is the release an install upgrading one step behind
-- is actually on, and the only shape that reaches the refresh_chain migration:
-- a table predating both is rebuilt once, from the current declaration, and
-- adopts every column at the same time.
CREATE TABLE sessions (
  project_id     TEXT NOT NULL,
  id             TEXT NOT NULL,
  token_hash     TEXT,
  user_id        TEXT NOT NULL REFERENCES users (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  membership_id  TEXT NOT NULL,
  state          TEXT NOT NULL CHECK (state IN ('active', 'revoked')),

  launch_patient TEXT,
  granted_scopes TEXT,

  created_at     BIGINT NOT NULL,
  expires_at     BIGINT NOT NULL,
  revoked_at     BIGINT,

  PRIMARY KEY (project_id, id),

  CHECK (substr(id, 1, 4) = 'ses_'),

  CHECK (expires_at > created_at AND expires_at <= created_at + 43200000),

  CHECK (state <> 'revoked' OR (token_hash IS NULL AND revoked_at IS NOT NULL)),
  CHECK (state = 'revoked' OR (token_hash IS NOT NULL AND revoked_at IS NULL)),

  CHECK (launch_patient IS NULL OR granted_scopes IS NOT NULL),
  CHECK (granted_scopes IS NULL OR granted_scopes <> ''),

  FOREIGN KEY (project_id, membership_id)
    REFERENCES project_memberships (project_id, id) ON DELETE RESTRICT ON UPDATE RESTRICT
);

CREATE UNIQUE INDEX ux_sessions_token
  ON sessions (token_hash) WHERE token_hash IS NOT NULL;

CREATE INDEX ix_sessions_user ON sessions (project_id, user_id, state, expires_at);
