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
    OR res_type IN ('Project', 'User', 'ClientApplication', 'IdentityProvider', 'SystemSetting')
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
    OR res_type IN ('Project', 'User', 'ClientApplication', 'IdentityProvider', 'SystemSetting')
  ),

  UNIQUE (project_id, res_type, res_id, version_id)
);

CREATE INDEX IF NOT EXISTS ix_platform_history_instance
  ON platform_resource_history (project_id, res_type, res_id, last_updated DESC, version_seq DESC);

CREATE INDEX IF NOT EXISTS ix_platform_history_type
  ON platform_resource_history (project_id, res_type, last_updated DESC, res_id, version_seq);

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
  CHECK (history_from IS NULL OR activated_at IS NOT NULL)
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

  -- client_applications and bots are separate tables that do not exist yet;
  -- their foreign keys land with them.
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
