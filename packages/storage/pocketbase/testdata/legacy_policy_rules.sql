-- access_policy_rules as it stood before a rule could state an element filter.
-- A database created from this is what PrepareSchema has to bring forward: the
-- current declaration is applied with IF NOT EXISTS, so this table would keep
-- its shape and every rule in it would compile to a grant narrowed by nothing.

CREATE TABLE IF NOT EXISTS access_policy_rules (
  project_id        TEXT NOT NULL,
  policy_id         TEXT NOT NULL,
  ordinal           BIGINT NOT NULL,
  kind              TEXT NOT NULL CHECK (kind IN ('fhir', 'platform')),
  res_type          TEXT NOT NULL,
  action            TEXT NOT NULL CHECK (action IN ('read', 'write', 'delete', 'search', 'history')),
  unrestricted      INTEGER NOT NULL DEFAULT 0 CHECK (unrestricted IN (0, 1)),
  compartment_type  TEXT,
  compartment_id    TEXT,
  compartment_param TEXT,

  PRIMARY KEY (project_id, policy_id, ordinal),

  CHECK (ordinal >= 0),
  CHECK (res_type <> ''),
  CHECK (compartment_type IS NULL OR compartment_type <> ''),
  CHECK (compartment_id IS NULL OR compartment_id <> ''),
  CHECK (compartment_param IS NULL OR compartment_param <> ''),

  CHECK (
    (unrestricted = 1 AND compartment_type IS NULL AND compartment_id IS NULL AND compartment_param IS NULL)
    OR (unrestricted = 0 AND compartment_type IS NOT NULL AND compartment_id IS NOT NULL AND compartment_param IS NULL)
    OR (unrestricted = 0 AND compartment_type IS NOT NULL AND compartment_id IS NULL AND compartment_param IS NOT NULL)
  ),

  FOREIGN KEY (project_id, policy_id)
    REFERENCES access_policies (project_id, id) ON DELETE CASCADE ON UPDATE RESTRICT,

  FOREIGN KEY (project_id, policy_id, compartment_param)
    REFERENCES access_policy_parameters (project_id, policy_id, name)
    ON DELETE RESTRICT ON UPDATE RESTRICT
);
