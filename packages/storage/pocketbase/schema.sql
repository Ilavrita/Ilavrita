-- Ilavrita database schema. A Project is the isolation boundary, so the tenant
-- column leads every primary key and every index that carries Project-scoped or
-- link-derived data.

-- Machine-readable markers for the schema-derived isolation test: "tenant:"
-- names a table's tenant column ("none" for registry tables), "tenant-exempt:"
-- justifies an index whose first column is not that column.

-- Portability: plain SQL that runs on SQLite and PostgreSQL. Every construct
-- that is not portable carries a "sqlite-only:" comment naming the PostgreSQL
-- equivalent. BIGINT is used for epoch milliseconds because PostgreSQL INTEGER is 32-bit.

-- Booleans are INTEGER 0/1 with a CHECK because SQLite has no BOOLEAN type.
-- Exclusive-or CHECKs are written as explicit disjunctions because PostgreSQL
-- cannot add booleans, so the shorter "(a IS NOT NULL) + (b IS NOT NULL) = 1" form is unportable.

-- ===========================================================================
-- Projects and instance
-- ===========================================================================

-- tenant: id
CREATE TABLE IF NOT EXISTS projects (
  id                  TEXT NOT NULL,
  kind                TEXT NOT NULL CHECK (kind IN ('standard', 'super')),
  slug                TEXT NOT NULL,
  name                TEXT NOT NULL,
  state               TEXT NOT NULL CHECK (state IN ('active', 'suspended', 'archived', 'deleting')),
  environment         TEXT NOT NULL DEFAULT 'production',
  allow_clinical_data INTEGER NOT NULL DEFAULT 1 CHECK (allow_clinical_data IN (0, 1)),
  created_at          BIGINT NOT NULL,
  updated_at          BIGINT NOT NULL,
  state_changed_at    BIGINT NOT NULL,
  version             BIGINT NOT NULL DEFAULT 1,

  PRIMARY KEY (id),

  -- 'system' is the platform_resource sentinel for a system-scoped row, so no
  -- Project may claim it as an id and collide with system scope.
  CHECK (id <> '' AND id <> 'system'),

  -- The Super Project administers; it never stores patient data.
  CHECK (kind <> 'super' OR allow_clinical_data = 0),

  -- Parent key for the project_memberships composite foreign key that confines
  -- super_admin to a Super Project.
  UNIQUE (id, kind)
);

-- tenant-exempt: a slug resolves which Project a request means, so it is global by definition
CREATE UNIQUE INDEX IF NOT EXISTS ux_projects_slug ON projects (slug);

-- tenant-exempt: enforces at most one Super Project across the whole instance
CREATE UNIQUE INDEX IF NOT EXISTS ux_projects_single_super ON projects (kind) WHERE kind = 'super';

-- tenant-exempt: the Super Admin project roster and the purge worker both scan by state
CREATE INDEX IF NOT EXISTS ix_projects_state ON projects (state, id);

-- tenant: none, one row describing the whole install
CREATE TABLE IF NOT EXISTS instance (
  id                         TEXT NOT NULL CHECK (id = 'instance'),
  schema_version             BIGINT NOT NULL,
  super_project_id           TEXT NOT NULL REFERENCES projects (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  bootstrap_state            TEXT NOT NULL CHECK (bootstrap_state IN ('pending', 'complete')),
  bootstrap_token_hash       TEXT,
  bootstrap_token_expires_at BIGINT,
  created_at                 BIGINT NOT NULL,
  updated_at                 BIGINT NOT NULL,

  PRIMARY KEY (id),

  -- A token without an expiry never dies, and a claimed instance keeps no
  -- bootstrap material that could claim it a second time.
  CHECK (bootstrap_token_hash IS NULL OR bootstrap_token_expires_at IS NOT NULL),
  CHECK (bootstrap_state <> 'complete' OR (bootstrap_token_hash IS NULL AND bootstrap_token_expires_at IS NULL))
);

-- ===========================================================================
-- Users
-- ===========================================================================

-- tenant: none, an identity registry; a server-scoped user has no owning Project
CREATE TABLE IF NOT EXISTS users (
  id               TEXT NOT NULL,
  scope            TEXT NOT NULL CHECK (scope IN ('server', 'project')),
  home_project_id  TEXT REFERENCES projects (id) ON DELETE RESTRICT ON UPDATE RESTRICT,

  -- The email uniqueness realm: a Project id for a Project-scoped user, the
  -- literal 'system' for a server-scoped one. Stored so it can lead an index.
  identity_realm   TEXT NOT NULL GENERATED ALWAYS AS (COALESCE(home_project_id, 'system')) STORED,

  email_normalized TEXT NOT NULL,
  email_display    TEXT NOT NULL,
  password_hash    TEXT,
  state            TEXT NOT NULL CHECK (state IN ('invited', 'active', 'disabled')),
  mfa_required     INTEGER NOT NULL DEFAULT 0 CHECK (mfa_required IN (0, 1)),
  created_at       BIGINT NOT NULL,
  updated_at       BIGINT NOT NULL,
  version          BIGINT NOT NULL DEFAULT 1,

  PRIMARY KEY (id),

  CHECK (id <> ''),
  CHECK (
    (scope = 'project' AND home_project_id IS NOT NULL)
    OR (scope = 'server' AND home_project_id IS NULL)
  )
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_users_realm_email ON users (identity_realm, email_normalized);

CREATE INDEX IF NOT EXISTS ix_users_realm_state ON users (identity_realm, state, id);

-- ===========================================================================
-- Client applications and bots
-- ===========================================================================

-- A programmatic principal belongs to exactly one Project, so project_id leads
-- the key and project_memberships can carry it into a composite foreign key: a
-- caller registered in one Project cannot hold standing in another. That is the
-- containment users cannot express, because a user may be server-scoped.

-- Two tables rather than one with a kind column. project_memberships carries a
-- column per family, and principal_kind is generated from which one is set, so
-- a per-family foreign key is what keeps that generated value honest. A merged
-- table would accept a bot's id in the client column and principal_kind would
-- then name the wrong family with every constraint still satisfied.

-- Ids carry a family prefix because ux_pm_active_principal is unique on the
-- generated principal_id alone: without disjoint spaces a client application
-- and a user would be one principal to it. substr, not LIKE, whose ASCII
-- case-insensitivity accepts 'CLI_x'.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS client_applications (
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

  -- Subsumes the non-emptiness guard: 'cli' is not 'cli_'.
  CHECK (substr(id, 1, 4) = 'cli_'),

  CHECK (project_id <> '' AND project_id <> 'system'),
  CHECK (name <> ''),
  CHECK (state <> 'revoked' OR revoked_at IS NOT NULL)
);

-- One name per Project, so a second integration cannot register under a name an
-- operator already trusts.
CREATE UNIQUE INDEX IF NOT EXISTS ux_client_applications_name
  ON client_applications (project_id, name);

CREATE INDEX IF NOT EXISTS ix_client_applications_state
  ON client_applications (project_id, state, id);

-- A bot is invoked by this server rather than authenticated by it. No credential
-- table names this one and no column here holds a hash, so a bot secret is
-- unrepresentable rather than merely unissued.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS bots (
  project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  id         TEXT NOT NULL,
  name       TEXT NOT NULL,
  state      TEXT NOT NULL CHECK (state IN ('active', 'suspended', 'revoked')),
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL,
  revoked_at BIGINT,
  version    BIGINT NOT NULL DEFAULT 1,

  PRIMARY KEY (project_id, id),

  CHECK (substr(id, 1, 4) = 'bot_'),
  CHECK (project_id <> '' AND project_id <> 'system'),
  CHECK (name <> ''),
  CHECK (state <> 'revoked' OR revoked_at IS NOT NULL)
);

CREATE UNIQUE INDEX IF NOT EXISTS ux_bots_name ON bots (project_id, name);

CREATE INDEX IF NOT EXISTS ix_bots_state ON bots (project_id, state, id);

-- A credential is one secret's whole life, owned by one client application. The
-- plaintext is never a column; the hash is, and a revoked row holds neither.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS client_application_credentials (
  project_id            TEXT NOT NULL,
  client_application_id TEXT NOT NULL,
  id                    TEXT NOT NULL,

  -- SHA-256 over 32 bytes this server minted. ClientSecret admits nothing else,
  -- so a guesser has nothing to shorten and a work factor would buy latency.
  secret_hash           TEXT,

  state                 TEXT NOT NULL CHECK (state IN ('active', 'superseded', 'revoked')),
  created_at            BIGINT NOT NULL,
  expires_at            BIGINT NOT NULL,
  revoked_at            BIGINT,

  PRIMARY KEY (project_id, client_application_id, id),

  CHECK (substr(id, 1, 4) = 'cac_'),

  -- A secret that outlives the quarter it was issued in is one nobody rotated.
  -- NOT NULL alone permits the year 3000, so the ceiling is stated: 90 days.
  CHECK (expires_at > created_at AND expires_at <= created_at + 7776000000),

  -- Revocation destroys the material, which is stronger than a state something
  -- must remember to read: a revoked row matches no secret because it holds none.
  CHECK (state <> 'revoked' OR (secret_hash IS NULL AND revoked_at IS NOT NULL)),

  -- The mirror, so a live credential answering nothing is unrepresentable too.
  CHECK (state = 'revoked' OR (secret_hash IS NOT NULL AND revoked_at IS NULL)),

  FOREIGN KEY (project_id, client_application_id)
    REFERENCES client_applications (project_id, id) ON DELETE CASCADE ON UPDATE RESTRICT
);

-- At most one credential answers for an application, and at most one outgoing
-- credential overlaps it. A third live secret is a constraint violation rather
-- than a count an application check reads and then races.
CREATE UNIQUE INDEX IF NOT EXISTS ux_cac_active
  ON client_application_credentials (project_id, client_application_id) WHERE state = 'active';

CREATE UNIQUE INDEX IF NOT EXISTS ux_cac_superseded
  ON client_application_credentials (project_id, client_application_id) WHERE state = 'superseded';

-- One application's credentials, live ones first.
CREATE INDEX IF NOT EXISTS ix_cac_application
  ON client_application_credentials (project_id, client_application_id, state, expires_at);

-- ===========================================================================
-- FHIR resources
-- ===========================================================================

-- Physically separate from platform resources, so a forgotten predicate cannot
-- cross between the two families.

-- The resource identity is the natural key (project_id, res_type, res_id) and a
-- version is that key plus version_seq. There is no global surrogate row id, so
-- no value can be reused across Projects and none can leak an instance-wide insert rate.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS fhir_resource (
  project_id     TEXT NOT NULL REFERENCES projects (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  res_type       TEXT NOT NULL,
  res_id         TEXT NOT NULL,
  version_id     TEXT NOT NULL,
  version_seq    BIGINT NOT NULL,

  -- Bumped when a soft-deleted logical id is written again. A history read
  -- binds the current epoch, so a new owner never inherits the old owner's versions.
  identity_epoch BIGINT NOT NULL DEFAULT 0,

  last_updated   BIGINT NOT NULL,
  deleted        INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1)),
  content        TEXT,

  PRIMARY KEY (project_id, res_type, res_id),

  CHECK (project_id <> '' AND project_id <> 'system'),
  CHECK (res_type <> '' AND res_id <> ''),
  CHECK (version_seq > 0),
  CHECK (identity_epoch >= 0),

  -- A tombstone carries no body; every live version does.
  CHECK (deleted = 1 OR content IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS ix_fhir_resource_type_updated
  ON fhir_resource (project_id, res_type, last_updated DESC, res_id);

CREATE INDEX IF NOT EXISTS ix_fhir_resource_system_updated
  ON fhir_resource (project_id, last_updated DESC, res_type, res_id);

-- History is append-only and authorized in its own right. Every column a
-- version read needs to authorize is on the version row or on
-- fhir_resource_history_compartment, never resolved through the current row.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS fhir_resource_history (
  project_id     TEXT NOT NULL REFERENCES projects (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  res_type       TEXT NOT NULL,
  res_id         TEXT NOT NULL,
  version_seq    BIGINT NOT NULL,
  version_id     TEXT NOT NULL,
  identity_epoch BIGINT NOT NULL DEFAULT 0,
  last_updated   BIGINT NOT NULL,
  deleted        INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1)),
  content        TEXT,

  PRIMARY KEY (project_id, res_type, res_id, version_seq),

  CHECK (project_id <> '' AND project_id <> 'system'),
  CHECK (res_type <> '' AND res_id <> ''),
  CHECK (version_seq > 0),
  CHECK (identity_epoch >= 0),
  CHECK (deleted = 1 OR content IS NOT NULL),

  UNIQUE (project_id, res_type, res_id, version_id)
);

-- Serves history-instance with the identity_epoch and history_from floors bound
-- in the index rather than filtered after the fetch.
CREATE INDEX IF NOT EXISTS ix_fhir_history_instance
  ON fhir_resource_history (project_id, res_type, res_id, identity_epoch, last_updated DESC, version_seq DESC);

-- history-type: GET /fhir/R4/{Type}/_history
CREATE INDEX IF NOT EXISTS ix_fhir_history_type
  ON fhir_resource_history (project_id, res_type, last_updated DESC, res_id, version_seq);

-- history-system: GET /fhir/R4/_history
CREATE INDEX IF NOT EXISTS ix_fhir_history_system
  ON fhir_resource_history (project_id, last_updated DESC, res_type, res_id, version_seq);

-- ===========================================================================
-- Compartment projections
-- ===========================================================================

-- A Grant's Compartment is the one structural restriction it carries, so the
-- history path gets its own version-dimensioned relation instead of inheriting
-- the current row's answer.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS fhir_resource_compartment (
  project_id TEXT NOT NULL,
  comp_type  TEXT NOT NULL,
  comp_id    TEXT NOT NULL,
  res_type   TEXT NOT NULL,
  res_id     TEXT NOT NULL,

  PRIMARY KEY (project_id, comp_type, comp_id, res_type, res_id),

  FOREIGN KEY (project_id, res_type, res_id)
    REFERENCES fhir_resource (project_id, res_type, res_id) ON DELETE CASCADE ON UPDATE RESTRICT
);

-- tenant: project_id
CREATE TABLE IF NOT EXISTS fhir_resource_history_compartment (
  project_id  TEXT NOT NULL,
  comp_type   TEXT NOT NULL,
  comp_id     TEXT NOT NULL,
  res_type    TEXT NOT NULL,
  res_id      TEXT NOT NULL,
  version_seq BIGINT NOT NULL,

  PRIMARY KEY (project_id, comp_type, comp_id, res_type, res_id, version_seq),

  FOREIGN KEY (project_id, res_type, res_id, version_seq)
    REFERENCES fhir_resource_history (project_id, res_type, res_id, version_seq)
    ON DELETE CASCADE ON UPDATE RESTRICT
);

-- Drives the join from a history row to the compartments that version belonged
-- to, which is what makes a per-version compartment check possible.
CREATE INDEX IF NOT EXISTS ix_fhir_history_compartment_version
  ON fhir_resource_history_compartment (project_id, res_type, res_id, version_seq, comp_type, comp_id);

-- ===========================================================================
-- Platform resources. Same document shape as FHIR resources, separate tables.
-- ===========================================================================

-- project_id holds the literal 'system' for a system-scoped row, so it carries
-- no foreign key to projects. NULL is rejected because NULLs do not compare
-- equal and would let two system rows share an id.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS platform_resource (
  project_id   TEXT NOT NULL,
  res_type     TEXT NOT NULL,
  res_id       TEXT NOT NULL,
  version_id   TEXT NOT NULL,
  version_seq  BIGINT NOT NULL,
  last_updated BIGINT NOT NULL,
  deleted      INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1)),
  content      TEXT,

  PRIMARY KEY (project_id, res_type, res_id),

  CHECK (project_id <> ''),
  CHECK (res_type <> '' AND res_id <> ''),
  CHECK (version_seq > 0),
  CHECK (deleted = 1 OR content IS NOT NULL),

  -- System scope holds only types that genuinely have no owning Project.
  CHECK (
    project_id <> 'system'
    OR res_type IN ('Project', 'User', 'IdentityProvider', 'SystemSetting')
  )
);

CREATE INDEX IF NOT EXISTS ix_platform_resource_type_updated
  ON platform_resource (project_id, res_type, last_updated DESC, res_id);

-- tenant: project_id
CREATE TABLE IF NOT EXISTS platform_resource_history (
  project_id   TEXT NOT NULL,
  res_type     TEXT NOT NULL,
  res_id       TEXT NOT NULL,
  version_seq  BIGINT NOT NULL,
  version_id   TEXT NOT NULL,
  last_updated BIGINT NOT NULL,
  deleted      INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1)),
  content      TEXT,

  PRIMARY KEY (project_id, res_type, res_id, version_seq),

  CHECK (project_id <> ''),
  CHECK (res_type <> '' AND res_id <> ''),
  CHECK (version_seq > 0),
  CHECK (deleted = 1 OR content IS NOT NULL),
  CHECK (
    project_id <> 'system'
    OR res_type IN ('Project', 'User', 'IdentityProvider', 'SystemSetting')
  ),

  UNIQUE (project_id, res_type, res_id, version_id)
);

CREATE INDEX IF NOT EXISTS ix_platform_history_instance
  ON platform_resource_history (project_id, res_type, res_id, last_updated DESC, version_seq DESC);

CREATE INDEX IF NOT EXISTS ix_platform_history_type
  ON platform_resource_history (project_id, res_type, last_updated DESC, res_id, version_seq);

-- ===========================================================================
-- Access policies
-- ===========================================================================

-- An AccessPolicy is the only thing a membership binding or a data link points
-- at. One Project owns it, and (project_id, id) is the parent key that keeps a
-- restriction resolvable only in its owner (LNK-6).

-- tenant: project_id
CREATE TABLE IF NOT EXISTS access_policies (
  project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  id         TEXT NOT NULL,
  name       TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL,
  version    BIGINT NOT NULL DEFAULT 1,

  PRIMARY KEY (project_id, id),

  CHECK (id <> ''),
  CHECK (project_id <> '' AND project_id <> 'system')
);

-- A policy declares the parameters it takes, so a rule can only name one a
-- binding is asked for, and a binding that omits one is refused rather than
-- resolved to a default subject (FR-055).

-- tenant: project_id
CREATE TABLE IF NOT EXISTS access_policy_parameters (
  project_id TEXT NOT NULL,
  policy_id  TEXT NOT NULL,
  name       TEXT NOT NULL,

  PRIMARY KEY (project_id, policy_id, name),

  CHECK (name <> ''),

  FOREIGN KEY (project_id, policy_id)
    REFERENCES access_policies (project_id, id) ON DELETE CASCADE ON UPDATE RESTRICT
);

-- One row per rule, each naming exactly one (kind, resource type, action), the
-- same shape as project_link_types: a triple absent here mints no Grant, and no
-- rule carries a type list a later read could widen against (LNK-3).

-- The restriction is one of three shapes; a rule with neither a compartment nor
-- an explicit unrestricted marker is unrepresentable (LNK-5). Which types may
-- carry an unrestricted rule is validated in packages/authz, which owns the list.

-- tenant: project_id
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
  compartment_ids   TEXT,
  compartment_param TEXT,
  filter_path       TEXT,
  filter_comparator TEXT,
  filter_values     TEXT,
  returns           TEXT,

  PRIMARY KEY (project_id, policy_id, ordinal),

  CHECK (ordinal >= 0),
  CHECK (res_type <> ''),
  CHECK (compartment_type IS NULL OR compartment_type <> ''),
  CHECK (compartment_id IS NULL OR compartment_id <> ''),
  CHECK (compartment_param IS NULL OR compartment_param <> ''),

  -- A filter narrows the rule to the resources whose named element matches. It
  -- is stated in full or not at all: a path with no comparator is a restriction
  -- nothing can apply, and a restriction nothing applies is a rule that reaches
  -- further than it says it does.
  CHECK (
    (filter_path IS NULL AND filter_comparator IS NULL AND filter_values IS NULL)
    OR (filter_path IS NOT NULL AND filter_comparator IS NOT NULL AND filter_values IS NOT NULL)
  ),
  CHECK (filter_path IS NULL OR filter_path <> ''),
  CHECK (filter_comparator IS NULL OR filter_comparator IN ('eq', 'in')),

  -- sqlite-only: json_valid, json_type and json_array_length. PostgreSQL holds
  -- the values in a jsonb column and uses jsonb_typeof and jsonb_array_length.
  CHECK (filter_values IS NULL OR (
    json_valid(filter_values) AND json_type(filter_values) = 'array'
    AND json_array_length(filter_values) >= 1
  )),

  -- Equality names exactly one value, which is what makes it a different
  -- statement from membership over a set that happens to hold one.
  CHECK (filter_comparator <> 'eq' OR json_array_length(filter_values) = 1),

  -- The elements the rule hands back. NULL returns the whole resource; a list
  -- returns those members and the ones every projection carries. An empty list
  -- is refused because a rule returning nothing is one nobody meant to write,
  -- and is indistinguishable from a list nothing ever bound.

  -- sqlite-only: json_valid, json_type and json_array_length, as above.
  CHECK (returns IS NULL OR (
    json_valid(returns) AND json_type(returns) = 'array' AND json_array_length(returns) >= 1
  )),

  -- An unrestricted rule says so and names no subject; a restricted one names a
  -- subject type plus exactly one of a literal id, a set of literal ids, or one
  -- parameter. A set is the same restriction as one rule per id, written once.
  CHECK (
    (unrestricted = 1
      AND compartment_type IS NULL AND compartment_id IS NULL
      AND compartment_ids IS NULL AND compartment_param IS NULL)
    OR (unrestricted = 0 AND compartment_type IS NOT NULL
      AND compartment_id IS NOT NULL AND compartment_ids IS NULL AND compartment_param IS NULL)
    OR (unrestricted = 0 AND compartment_type IS NOT NULL
      AND compartment_id IS NULL AND compartment_ids IS NOT NULL AND compartment_param IS NULL)
    OR (unrestricted = 0 AND compartment_type IS NOT NULL
      AND compartment_id IS NULL AND compartment_ids IS NULL AND compartment_param IS NOT NULL)
  ),

  -- sqlite-only: json_valid, json_type and json_array_length, as elsewhere here.
  CHECK (compartment_ids IS NULL OR (
    json_valid(compartment_ids) AND json_type(compartment_ids) = 'array'
    AND json_array_length(compartment_ids) >= 1
  )),

  FOREIGN KEY (project_id, policy_id)
    REFERENCES access_policies (project_id, id) ON DELETE CASCADE ON UPDATE RESTRICT,

  -- A rule names only a parameter the policy declares. NULL is a rule that takes
  -- none, the one case this key does not constrain.
  FOREIGN KEY (project_id, policy_id, compartment_param)
    REFERENCES access_policy_parameters (project_id, policy_id, name)
    ON DELETE RESTRICT ON UPDATE RESTRICT
);

-- Resolution asks one policy for one exact triple, so the probe follows the key.
CREATE INDEX IF NOT EXISTS ix_access_policy_rules_probe
  ON access_policy_rules (project_id, policy_id, kind, res_type, action);

-- ===========================================================================
-- Project links
-- ===========================================================================

-- The authorization probe reads this table directly, so lifecycle lives here:
-- a proposed, suspended, revoked or expired link authorizes nothing, and there
-- is no derived copy that can hold a stale answer after a revoke.

-- The key is natural, (grantee, grantor, kind), not a surrogate id. A child row
-- therefore cannot name a link without naming both Projects, and it carries the
-- kind, which is what makes an administrative link with data scopes unrepresentable.

-- tenant: grantee_project
CREATE TABLE IF NOT EXISTS project_links (
  grantee_project          TEXT NOT NULL REFERENCES projects (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  grantor_project          TEXT NOT NULL REFERENCES projects (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  kind                     TEXT NOT NULL CHECK (kind IN ('data', 'administrative')),
  status                   TEXT NOT NULL CHECK (status IN ('proposed', 'active', 'suspended', 'revoked')),
  all_resource_types       INTEGER NOT NULL DEFAULT 0 CHECK (all_resource_types IN (0, 1)),

  -- LNK-6: the restriction is always the grantor's policy. The name says so, so
  -- no lookup can be parameterized by the consuming Project by accident.
  grantor_access_policy_id TEXT,
  policy_params            TEXT NOT NULL DEFAULT '{}',

  -- Floor on how far back a grantee may read history, defaulted to activated_at
  -- by the writer. HIST-3 requires it bound as a predicate on every history
  -- read; no read path binds it yet, so it restricts nothing today.
  history_from             BIGINT,

  expires_at               BIGINT,
  activated_at             BIGINT,
  grantor_approved_by      TEXT,
  grantor_approved_at      BIGINT,
  grantee_approved_by      TEXT,
  grantee_approved_at      BIGINT,
  created_at               BIGINT NOT NULL,
  updated_at               BIGINT NOT NULL,
  version                  BIGINT NOT NULL DEFAULT 1,

  PRIMARY KEY (grantee_project, grantor_project, kind),

  CHECK (grantor_project <> grantee_project),

  -- Administrative containment carries no data privilege.
  CHECK (kind <> 'administrative' OR all_resource_types = 0),

  -- Two-sided approval as a constraint: an active link has both approvals and
  -- an activation instant, so a mere proposal can never authorize anything.
  CHECK (status <> 'active' OR (grantor_approved_at IS NOT NULL AND grantee_approved_at IS NOT NULL)),
  CHECK (status <> 'active' OR activated_at IS NOT NULL),

  CHECK ((grantor_approved_by IS NULL AND grantor_approved_at IS NULL)
      OR (grantor_approved_by IS NOT NULL AND grantor_approved_at IS NOT NULL)),
  CHECK ((grantee_approved_by IS NULL AND grantee_approved_at IS NULL)
      OR (grantee_approved_by IS NOT NULL AND grantee_approved_at IS NOT NULL)),

  CHECK (expires_at IS NULL OR expires_at > created_at),
  CHECK (history_from IS NULL OR activated_at IS NOT NULL),

  -- A data link's restriction is the grantor's own policy; an administrative
  -- link names none, so administrative reach has no policy to widen (FR-052).
  CHECK (kind <> 'data' OR grantor_access_policy_id IS NOT NULL),
  CHECK (kind <> 'administrative' OR grantor_access_policy_id IS NULL),

  -- The policy is looked up in the grantor, so a grantee's same-named policy
  -- cannot lower the restriction it reaches through (LNK-6).
  FOREIGN KEY (grantor_project, grantor_access_policy_id)
    REFERENCES access_policies (project_id, id) ON DELETE RESTRICT ON UPDATE RESTRICT
);

-- The approver columns hold membership ids with no foreign key: a Super Admin
-- may approve both sides from the Super Project, and a links/memberships
-- foreign key cycle is not expressible in portable DDL.

-- Layer-1 probe: grantee, kind, then the lifecycle columns the predicate binds.
CREATE INDEX IF NOT EXISTS ix_project_links_active
  ON project_links (grantee_project, kind, expires_at, grantor_project) WHERE status = 'active';

CREATE INDEX IF NOT EXISTS ix_project_links_grantee
  ON project_links (grantee_project, kind, status, grantor_project);

-- tenant-exempt: grantor_project is also a tenant column; this is the grantor's
-- own view of who may reach into it
CREATE INDEX IF NOT EXISTS ix_project_links_grantor
  ON project_links (grantor_project, kind, status, grantee_project);

-- tenant-exempt: at most one administrative parent per Project is a statement
-- about the grantor, so grantor_project is the only column that can enforce it
CREATE UNIQUE INDEX IF NOT EXISTS ux_project_links_one_parent
  ON project_links (grantor_project) WHERE kind = 'administrative' AND status = 'active';

-- A data link covering N types is N rows, so the authorizer mints one Grant per
-- (type, action) pair and a type absent here produces no Grant at all. The kind
-- column plus its CHECK make a data scope on an administrative link impossible.

-- tenant: grantee_project
CREATE TABLE IF NOT EXISTS project_link_types (
  grantee_project TEXT NOT NULL,
  grantor_project TEXT NOT NULL,
  kind            TEXT NOT NULL CHECK (kind = 'data'),
  res_type        TEXT NOT NULL,

  -- A link never confers write. Omitting 'write' and 'delete' makes
  -- cross-Project write unrepresentable rather than merely rejected.
  action          TEXT NOT NULL CHECK (action IN ('read', 'search', 'history')),

  PRIMARY KEY (grantee_project, grantor_project, kind, res_type, action),

  FOREIGN KEY (grantee_project, grantor_project, kind)
    REFERENCES project_links (grantee_project, grantor_project, kind)
    ON DELETE CASCADE ON UPDATE RESTRICT
);

CREATE INDEX IF NOT EXISTS ix_project_link_types_probe
  ON project_link_types (grantee_project, res_type, action, grantor_project);

-- ===========================================================================
-- Memberships
-- ===========================================================================

-- Super Admin is super_admin = 1 on an active membership whose Project is
-- kind='super'. The composite foreign key to projects(id, kind) plus the CHECK
-- make any other shape a constraint violation rather than a policy promise.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS project_memberships (
  project_id               TEXT NOT NULL,
  id                       TEXT NOT NULL,

  -- Denormalized so the CHECK below can see it; the composite foreign key keeps
  -- it equal to the Project's own kind.
  project_kind             TEXT NOT NULL,

  user_id                  TEXT REFERENCES users (id) ON DELETE RESTRICT ON UPDATE RESTRICT,

  client_application_id    TEXT,
  bot_id                   TEXT,

  principal_kind           TEXT NOT NULL GENERATED ALWAYS AS (
    CASE
      WHEN user_id IS NOT NULL THEN 'user'
      WHEN client_application_id IS NOT NULL THEN 'client_application'
      ELSE 'bot'
    END
  ) STORED,
  principal_id             TEXT NOT NULL GENERATED ALWAYS AS (
    COALESCE(user_id, client_application_id, bot_id)
  ) STORED,

  profile_type             TEXT,
  profile_id               TEXT,
  state                    TEXT NOT NULL CHECK (state IN ('invited', 'active', 'suspended', 'revoked')),
  admin                    INTEGER NOT NULL DEFAULT 0 CHECK (admin IN (0, 1)),
  super_admin              INTEGER NOT NULL DEFAULT 0 CHECK (super_admin IN (0, 1)),
  user_configuration_id    TEXT,
  invitation_source        TEXT NOT NULL CHECK (invitation_source IN (
    'bootstrap', 'invite', 'idp_jit', 'scim', 'api', 'migration', 'link'
  )),
  invited_by_membership_id TEXT,

  -- Set when this membership exists only because an administrative link
  -- conferred it. The link's grantor is this Project, pinned by the foreign key.
  via_link_grantee_project TEXT,
  via_link_kind            TEXT,

  -- Denormalized so a policy binding's composite foreign key can see it: a
  -- link-minted membership must never carry a binding (CP-1).
  link_sourced             INTEGER NOT NULL GENERATED ALWAYS AS (
    CASE WHEN via_link_grantee_project IS NULL THEN 0 ELSE 1 END
  ) STORED,

  created_at               BIGINT NOT NULL,
  updated_at               BIGINT NOT NULL,
  activated_at             BIGINT,
  revoked_at               BIGINT,
  version                  BIGINT NOT NULL DEFAULT 1,

  -- Bumped only by state, admin, super_admin and policy-binding changes, so an
  -- unrelated edit does not invalidate every cached authorization.
  authz_version            BIGINT NOT NULL DEFAULT 1,

  PRIMARY KEY (project_id, id),

  CHECK (id <> ''),

  -- Exactly one principal, written as a disjunction because PostgreSQL cannot
  -- add booleans.
  CHECK (
    (user_id IS NOT NULL AND client_application_id IS NULL AND bot_id IS NULL)
    OR (user_id IS NULL AND client_application_id IS NOT NULL AND bot_id IS NULL)
    OR (user_id IS NULL AND client_application_id IS NULL AND bot_id IS NOT NULL)
  ),

  -- The three principal namespaces are disjoint, so the generated principal_id
  -- that ux_pm_active_principal keys on can never name two principals at once.
  CHECK (client_application_id IS NULL OR substr(client_application_id, 1, 4) = 'cli_'),
  CHECK (bot_id IS NULL OR substr(bot_id, 1, 4) = 'bot_'),
  CHECK (user_id IS NULL OR (substr(user_id, 1, 4) <> 'cli_' AND substr(user_id, 1, 4) <> 'bot_')),

  -- Super Admin administers the install and answers for it. A machine principal
  -- is one leaked secret away, with no second factor and no person behind it.
  CHECK (super_admin = 0 OR user_id IS NOT NULL),

  -- A bot runs code this server invokes, so administrative standing on one is a
  -- control-plane write reachable from whatever that code is made to do.
  CHECK (bot_id IS NULL OR (admin = 0 AND super_admin = 0)),

  -- A machine principal's authority is its AccessPolicy, never a compartment it
  -- occupies: a profile would collect compartment grants with no person in the
  -- chain that leads to them.
  CHECK (profile_id IS NULL OR user_id IS NOT NULL),

  -- The load-bearing one: Super Admin cannot exist outside a Super Project.
  CHECK (super_admin = 0 OR project_kind = 'super'),
  CHECK (super_admin = 0 OR admin = 1),

  CHECK (
    (profile_type IS NULL AND profile_id IS NULL)
    OR (profile_type IS NOT NULL AND profile_id IS NOT NULL)
  ),

  CHECK (
    (via_link_grantee_project IS NULL AND via_link_kind IS NULL)
    OR (via_link_grantee_project IS NOT NULL AND via_link_kind IS NOT NULL)
  ),

  -- A link-minted membership is never privileged, and it is only ever minted by
  -- an administrative link, which is what stops a link being an escalation path.
  CHECK (via_link_grantee_project IS NULL OR (admin = 0 AND super_admin = 0)),
  CHECK (via_link_kind IS NULL OR via_link_kind = 'administrative'),
  CHECK (
    (via_link_grantee_project IS NULL AND invitation_source <> 'link')
    OR (via_link_grantee_project IS NOT NULL AND invitation_source = 'link')
  ),

  CHECK (state <> 'active' OR activated_at IS NOT NULL),
  CHECK (state <> 'revoked' OR revoked_at IS NOT NULL),

  FOREIGN KEY (project_id, project_kind)
    REFERENCES projects (id, kind) ON DELETE RESTRICT ON UPDATE RESTRICT,

  -- A membership's profile can never point into another Project.
  FOREIGN KEY (project_id, profile_type, profile_id)
    REFERENCES fhir_resource (project_id, res_type, res_id) ON DELETE RESTRICT ON UPDATE RESTRICT,

  -- An inviter is a member of the same Project, so invitation lineage cannot
  -- cross a Project boundary.
  FOREIGN KEY (project_id, invited_by_membership_id)
    REFERENCES project_memberships (project_id, id) ON DELETE RESTRICT ON UPDATE RESTRICT,

  -- A client application is a member only of the Project that registered it, so
  -- a membership cannot name one belonging to another. The key is unenforced
  -- when the column is NULL, which is every user and bot membership.
  FOREIGN KEY (project_id, client_application_id)
    REFERENCES client_applications (project_id, id) ON DELETE RESTRICT ON UPDATE RESTRICT,

  -- The same containment for a bot, which is also what makes a bot id in the
  -- client application column a constraint violation rather than a principal
  -- kind that lies.
  FOREIGN KEY (project_id, bot_id)
    REFERENCES bots (project_id, id) ON DELETE RESTRICT ON UPDATE RESTRICT,

  -- This Project is the link's grantor, so a link into one Project cannot mint a
  -- membership in another.
  FOREIGN KEY (via_link_grantee_project, project_id, via_link_kind)
    REFERENCES project_links (grantee_project, grantor_project, kind)
    ON DELETE RESTRICT ON UPDATE RESTRICT
);

-- One membership per principal per Project, which is what makes membership
-- resolution at token issuance deterministic.
CREATE UNIQUE INDEX IF NOT EXISTS ux_pm_active_principal
  ON project_memberships (project_id, principal_id) WHERE state <> 'revoked';

CREATE UNIQUE INDEX IF NOT EXISTS ux_pm_profile
  ON project_memberships (project_id, profile_type, profile_id)
  WHERE state <> 'revoked' AND profile_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS ix_pm_project_state ON project_memberships (project_id, state, id);

-- Answers the Super Admin roster count as an index-only scan.
CREATE INDEX IF NOT EXISTS ix_pm_super ON project_memberships (project_id, state) WHERE super_admin = 1;

-- tenant-exempt: login resolves which Projects a principal may address, so the
-- principal is the only thing known at probe time; every query it feeds re-pins
-- the Project
CREATE INDEX IF NOT EXISTS ix_pm_principal ON project_memberships (principal_id, state, project_id);

-- membershipQuery resolves one principal in one Project including its revoked
-- rows, which the partial uniqueness index above cannot serve.
CREATE INDEX IF NOT EXISTS ix_pm_project_principal
  ON project_memberships (project_id, principal_id, state);

-- Parent key for the policy binding's composite foreign key. It is an index
-- rather than a table constraint because link_sourced is a generated column.
CREATE UNIQUE INDEX IF NOT EXISTS ux_pm_link_sourced
  ON project_memberships (project_id, id, link_sourced);

-- A binding is one AccessPolicy on one membership, in the membership's own
-- Project: project_id leads the key and both foreign keys carry it, so a
-- membership cannot bind a policy another Project owns.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS project_membership_policies (
  project_id              TEXT NOT NULL,
  membership_id           TEXT NOT NULL,
  policy_id               TEXT NOT NULL,
  ordinal                 BIGINT NOT NULL DEFAULT 0,

  -- The values this binding supplies to the policy's declared parameters (FR-055).
  policy_params           TEXT NOT NULL DEFAULT '{}',

  -- Always 0, so the composite foreign key below makes a binding on a
  -- link-minted membership a constraint violation, not a promise (CP-1).
  membership_link_sourced INTEGER NOT NULL DEFAULT 0 CHECK (membership_link_sourced = 0),

  PRIMARY KEY (project_id, membership_id, policy_id),

  CHECK (ordinal >= 0),

  FOREIGN KEY (project_id, membership_id, membership_link_sourced)
    REFERENCES project_memberships (project_id, id, link_sourced)
    ON DELETE CASCADE ON UPDATE RESTRICT,

  FOREIGN KEY (project_id, policy_id)
    REFERENCES access_policies (project_id, id) ON DELETE RESTRICT ON UPDATE RESTRICT
);

-- One membership's bindings resolve in ordinal order.
CREATE INDEX IF NOT EXISTS ix_pmp_membership_ordinal
  ON project_membership_policies (project_id, membership_id, ordinal);

-- The link-conferrable capability set is a closed allowlist enforced by a CHECK,
-- so no second write path can confer membership or policy writes, and each row
-- names one membership rather than the ambient role "the grantee's admins".

-- tenant: grantee_project
CREATE TABLE IF NOT EXISTS project_link_capabilities (
  grantee_project              TEXT NOT NULL,
  grantor_project              TEXT NOT NULL,
  kind                         TEXT NOT NULL CHECK (kind = 'administrative'),
  capability                   TEXT NOT NULL CHECK (capability IN (
    'project.quota.write',
    'project.lifecycle.write',
    'project.settings.write',
    'project.membership.read'
  )),
  principal_membership_project TEXT NOT NULL,
  principal_membership_id      TEXT NOT NULL,

  PRIMARY KEY (grantee_project, grantor_project, kind, capability, principal_membership_id),

  -- The holder is a member of the grantee Project, named individually.
  CHECK (principal_membership_project = grantee_project),

  FOREIGN KEY (grantee_project, grantor_project, kind)
    REFERENCES project_links (grantee_project, grantor_project, kind)
    ON DELETE CASCADE ON UPDATE RESTRICT,

  FOREIGN KEY (principal_membership_project, principal_membership_id)
    REFERENCES project_memberships (project_id, id) ON DELETE RESTRICT ON UPDATE RESTRICT
);

-- The CHECK above pins principal_membership_project to grantee_project, so the
-- tenant column leads this index too.
CREATE INDEX IF NOT EXISTS ix_project_link_capabilities_holder
  ON project_link_capabilities (grantee_project, principal_membership_id, capability);

-- ===========================================================================
-- Sessions
-- ===========================================================================

-- A session is what turns a later request into a principal. It pins the Project
-- and the membership at issuance, so a request never has to resolve which
-- membership was meant: FR-048 requires that choice to be deterministic, and the
-- only instant it can be made honestly is when the credential was presented.

-- The token is 32 bytes this server minted, stored as a single SHA-256. Unlike a
-- client credential, a session token names itself: it is the identifier, so the
-- unique index below is keyed on the hash and carries no tenant column. That is
-- the one place a caller-presented value selects a row on its own, and it is
-- sound only because the row it selects is what states the Project.

-- tenant: project_id
-- tenant-exempt: ux_sessions_token, because the token is the lookup key and the
-- row it finds is what names the Project every later query is bound by
CREATE TABLE IF NOT EXISTS sessions (
  project_id    TEXT NOT NULL,
  id            TEXT NOT NULL,

  -- NULL once revoked: a revoked session matches no token because it holds none.
  token_hash    TEXT,

  user_id       TEXT NOT NULL REFERENCES users (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  membership_id TEXT NOT NULL,
  state         TEXT NOT NULL CHECK (state IN ('active', 'revoked')),
  created_at    BIGINT NOT NULL,
  expires_at    BIGINT NOT NULL,
  revoked_at    BIGINT,

  PRIMARY KEY (project_id, id),

  CHECK (substr(id, 1, 4) = 'ses_'),

  -- A session that outlives the day it was issued on is one nobody re-proved a
  -- credential for. NOT NULL alone permits the year 3000, so the ceiling is
  -- stated: 12 hours.
  CHECK (expires_at > created_at AND expires_at <= created_at + 43200000),

  -- Revocation destroys the material rather than labelling it, and the mirror
  -- makes a live session holding nothing unrepresentable too.
  CHECK (state <> 'revoked' OR (token_hash IS NULL AND revoked_at IS NOT NULL)),
  CHECK (state = 'revoked' OR (token_hash IS NOT NULL AND revoked_at IS NULL)),

  -- The membership is pinned in the session's own Project, so a token issued for
  -- one Project cannot name standing in another.
  FOREIGN KEY (project_id, membership_id)
    REFERENCES project_memberships (project_id, id) ON DELETE RESTRICT ON UPDATE RESTRICT
);

-- The token is the lookup key, so it is unique across the install. A revoked
-- session holds no token and falls out of the index.
CREATE UNIQUE INDEX IF NOT EXISTS ux_sessions_token
  ON sessions (token_hash) WHERE token_hash IS NOT NULL;

-- One identity's live sessions, for a "sign out everywhere" that names no token.
CREATE INDEX IF NOT EXISTS ix_sessions_user ON sessions (project_id, user_id, state, expires_at);

-- ===========================================================================
-- Append-only history
-- ===========================================================================

-- sqlite-only: RAISE(ABORT, ...) triggers. On PostgreSQL, revoke UPDATE and
-- DELETE on the history tables from the application role, or port these to a
-- PL/pgSQL trigger function.

-- sqlite-only: PostgreSQL has no CREATE TRIGGER IF NOT EXISTS; use CREATE OR
-- REPLACE TRIGGER (PostgreSQL 14+).

-- sqlite-only: "x IS NOT <literal>" is SQLite's null-safe inequality, so a
-- missing projects row aborts rather than comparing to NULL. On PostgreSQL
-- write "x IS DISTINCT FROM <literal>".

CREATE TRIGGER IF NOT EXISTS fhir_resource_history_no_update
BEFORE UPDATE ON fhir_resource_history
BEGIN
  SELECT RAISE(ABORT, 'fhir_resource_history is append-only');
END;

-- Deletion is confined to the purge worker, which only runs against a Project
-- the database itself reports as deleting.
CREATE TRIGGER IF NOT EXISTS fhir_resource_history_no_delete
BEFORE DELETE ON fhir_resource_history
WHEN (SELECT state FROM projects WHERE id = OLD.project_id) IS NOT 'deleting'
BEGIN
  SELECT RAISE(ABORT, 'fhir_resource_history is append-only outside a project purge');
END;

CREATE TRIGGER IF NOT EXISTS platform_resource_history_no_update
BEFORE UPDATE ON platform_resource_history
BEGIN
  SELECT RAISE(ABORT, 'platform_resource_history is append-only');
END;

CREATE TRIGGER IF NOT EXISTS platform_resource_history_no_delete
BEFORE DELETE ON platform_resource_history
WHEN OLD.project_id = 'system'
  OR (SELECT state FROM projects WHERE id = OLD.project_id) IS NOT 'deleting'
BEGIN
  SELECT RAISE(ABORT, 'platform_resource_history is append-only outside a project purge');
END;

-- A renamed slug strands links and authorization caches, and a changed kind
-- would move the Super Admin guard's parent key.
CREATE TRIGGER IF NOT EXISTS projects_identity_immutable
BEFORE UPDATE ON projects
WHEN NEW.id IS NOT OLD.id OR NEW.slug IS NOT OLD.slug OR NEW.kind IS NOT OLD.kind
BEGIN
  SELECT RAISE(ABORT, 'projects.id, projects.slug and projects.kind are immutable');
END;

-- sqlite-only: an AFTER UPDATE trigger. On PostgreSQL, the same body as a
-- PL/pgSQL trigger function.

-- Revoking a registration destroys the secrets issued under it. The foreign key
-- cannot express this, because ON DELETE CASCADE fires on deletion and a
-- revocation is deliberately not a deletion: the row survives for the audit.
CREATE TRIGGER IF NOT EXISTS client_application_revocation_destroys_secrets
AFTER UPDATE OF state ON client_applications
WHEN new.state = 'revoked' AND old.state <> 'revoked'
BEGIN
  UPDATE client_application_credentials
     SET state = 'revoked', secret_hash = NULL, revoked_at = new.updated_at
   WHERE project_id = new.project_id
     AND client_application_id = new.id
     AND state <> 'revoked';
END;

-- A registration's state gates its memberships' standing, so changing it changes
-- effective authorization. authz_version is what a cache keys on, and nothing
-- else would bump it for a change that happens in another table.
CREATE TRIGGER IF NOT EXISTS client_application_state_bumps_authz
AFTER UPDATE OF state ON client_applications
WHEN new.state <> old.state
BEGIN
  UPDATE project_memberships SET authz_version = authz_version + 1
   WHERE project_id = new.project_id AND client_application_id = new.id;
END;

CREATE TRIGGER IF NOT EXISTS bot_state_bumps_authz
AFTER UPDATE OF state ON bots
WHEN new.state <> old.state
BEGIN
  UPDATE project_memberships SET authz_version = authz_version + 1
   WHERE project_id = new.project_id AND bot_id = new.id;
END;
