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

  -- Whether this registration keeps a secret, which decides what it must present
  -- to redeem an authorization code. It is recorded rather than derived from
  -- whether a live credential exists, because those differ exactly when it
  -- matters: revoking the last credential of a confidential client would
  -- otherwise turn it into one that redeems codes presenting nothing.
  --
  -- The default is for the rebuild alone. Every registration that predates this
  -- column was issued a secret when it was created, so confidential is what it
  -- already was. Go refuses a registration naming no kind, so nothing new
  -- reaches this default.
  kind        TEXT NOT NULL DEFAULT 'confidential'
                CHECK (kind IN ('public', 'confidential')),

  -- The public keys a backend service signs its client assertions with, as a
  -- JWK Set, NULL for a registration that signs none.
  --
  -- Inline rather than a jwks_uri, deliberately. A URL would make the token
  -- endpoint fetch a client-controlled address on every authentication: an
  -- outbound request this server otherwise never makes, an availability
  -- dependency on somebody else's host, and a request-forgery surface aimed at
  -- whatever the deployment can reach.
  jwks        TEXT,

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

-- The addresses an authorization code may be handed back to.

-- They are rows rather than a delimited column because the comparison that
-- matters is exact equality against one of them, and a delimited column makes
-- that a scan over a parsed list — which is where a separator inside an address
-- becomes two addresses. One row is one address, and the primary key says the
-- same address cannot be registered twice.

-- The check is deliberately weak: what an address must be is stated in Go, by
-- ParseRedirectURI, and restating a URL grammar in SQL would be a second
-- definition that drifts. What SQL states is what SQL can hold honestly — an
-- address is not empty, and it carries no fragment, because a fragment never
-- reaches a server and an address carrying one means something it cannot do.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS client_redirect_uris (
  project_id            TEXT NOT NULL,
  client_application_id TEXT NOT NULL,
  uri                   TEXT NOT NULL,
  created_at            BIGINT NOT NULL,

  PRIMARY KEY (project_id, client_application_id, uri),

  CHECK (uri <> ''),
  CHECK (instr(uri, '#') = 0),

  FOREIGN KEY (project_id, client_application_id)
    REFERENCES client_applications (project_id, id) ON DELETE CASCADE ON UPDATE RESTRICT
);

-- One person's approval, waiting to be redeemed once.

-- Everything the token endpoint re-checks is bound here rather than re-read from
-- the request that redeems it: which client it was issued to, the address it is
-- returned at, the challenge it is bound to, and what was approved. A row that
-- carried less would be one the token endpoint had to trust its presenter about.

-- The code itself is never written. Only its SHA-256 is, so a stolen database
-- yields nothing a caller could present — the same reason a session token is
-- stored as a digest.

-- Redemption deletes the row rather than marking it spent, which is how single
-- use is enforced without any state something must remember to read: a code that
-- cannot be replayed because it no longer exists needs no flag.

-- tenant: project_id
-- tenant-exempt: ux_authorization_codes_hash, because the digest is the lookup
-- key and the row it finds is what names the Project
CREATE TABLE IF NOT EXISTS authorization_codes (
  project_id            TEXT NOT NULL,
  id                    TEXT NOT NULL,

  code_hash             TEXT NOT NULL,

  client_application_id TEXT NOT NULL,
  user_id               TEXT NOT NULL REFERENCES users (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  membership_id         TEXT NOT NULL,

  redirect_uri          TEXT NOT NULL,
  code_challenge        TEXT NOT NULL,

  -- What was approved, in the shape a session carries it, so the token endpoint
  -- moves the value across rather than deriving a second one.
  launch_patient        TEXT,
  granted_scopes        TEXT NOT NULL,

  created_at            BIGINT NOT NULL,
  expires_at            BIGINT NOT NULL,

  PRIMARY KEY (project_id, id),

  CHECK (substr(id, 1, 4) = 'acd_'),

  -- A code is redeemed on the round trip the client is already making. Sixty
  -- seconds is generous for that; NOT NULL alone would permit the year 3000.
  CHECK (expires_at > created_at AND expires_at <= created_at + 60000),

  -- A code holding nothing to compare against would answer every presenter.
  CHECK (code_hash <> ''),

  -- PKCE is required of every client, so a row with no challenge is one nothing
  -- binds to the client that asked for it.
  CHECK (code_challenge <> ''),

  -- A code granting nothing would mint a session narrowed by nothing.
  CHECK (granted_scopes <> ''),

  CHECK (redirect_uri <> '' AND instr(redirect_uri, '#') = 0),

  FOREIGN KEY (project_id, client_application_id)
    REFERENCES client_applications (project_id, id) ON DELETE CASCADE ON UPDATE RESTRICT,

  FOREIGN KEY (project_id, membership_id)
    REFERENCES project_memberships (project_id, id) ON DELETE RESTRICT ON UPDATE RESTRICT
);

-- The digest is the lookup key, so it is unique across the install.
CREATE UNIQUE INDEX IF NOT EXISTS ux_authorization_codes_hash
  ON authorization_codes (code_hash);

-- One grant, refreshable until its chain expires.

-- A refresh token is rotated on every use: redeeming one writes its successor
-- and marks it spent. Its row is kept rather than deleted, because a spent token
-- presented again is the one signal that a copy of it exists somewhere it should
-- not — and a deleted row would answer "unknown", which is exactly what a guess
-- answers. The digest of a spent token is therefore retained, and is no longer a
-- credential: it authorizes nothing and exists only to be recognised.

-- Every rotation carries the same chain_id, which is what makes them one grant.
-- Detecting a replay revokes the chain, and the sessions it minted along with
-- it: sessions.refresh_chain is what makes that reachable.

-- tenant: project_id
-- tenant-exempt: ux_refresh_tokens_hash, because the digest is the lookup key
-- and the row it finds is what names the Project
CREATE TABLE IF NOT EXISTS refresh_tokens (
  project_id            TEXT NOT NULL,
  id                    TEXT NOT NULL,
  chain_id              TEXT NOT NULL,

  token_hash            TEXT NOT NULL,

  client_application_id TEXT NOT NULL,
  user_id               TEXT NOT NULL REFERENCES users (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  membership_id         TEXT NOT NULL,

  launch_patient        TEXT,
  granted_scopes        TEXT NOT NULL,

  state                 TEXT NOT NULL CHECK (state IN ('active', 'spent')),

  created_at            BIGINT NOT NULL,
  expires_at            BIGINT NOT NULL,

  PRIMARY KEY (project_id, id),

  CHECK (substr(id, 1, 4) = 'rft_'),
  CHECK (substr(chain_id, 1, 4) = 'rch_'),

  -- A grant that outlives the month it was approved in is one nobody re-approved.
  CHECK (expires_at > created_at AND expires_at <= created_at + 2592000000),

  -- A token holding nothing to compare against would answer every presenter,
  -- and a spent one holding nothing could not be recognised as a replay.
  CHECK (token_hash <> ''),

  -- A grant exchangeable for nothing would mint a session narrowed by nothing.
  CHECK (granted_scopes <> ''),

  FOREIGN KEY (project_id, client_application_id)
    REFERENCES client_applications (project_id, id) ON DELETE CASCADE ON UPDATE RESTRICT,

  FOREIGN KEY (project_id, membership_id)
    REFERENCES project_memberships (project_id, id) ON DELETE RESTRICT ON UPDATE RESTRICT
);

-- The digest is the lookup key, so it is unique across the install. It covers
-- spent rows too, which is what makes a replay resolve to the chain it belongs
-- to rather than to nothing.
CREATE UNIQUE INDEX IF NOT EXISTS ux_refresh_tokens_hash
  ON refresh_tokens (token_hash);

-- One chain's rotations, for revoking all of them at once.
CREATE INDEX IF NOT EXISTS ix_refresh_tokens_chain
  ON refresh_tokens (project_id, chain_id);

-- For sweeping what expired without anyone refreshing it.
CREATE INDEX IF NOT EXISTS ix_refresh_tokens_expiry
  ON refresh_tokens (expires_at);

-- sessions.refresh_chain carries no index of its own, deliberately. This file is
-- applied before any rebuild runs, so an index over a column a rebuild is about
-- to add would fail on exactly the installs that need the rebuild. The one query
-- that reads it runs when a replay is detected, which is rare enough that a scan
-- bounded by project_id costs nothing worth this risk.

-- For sweeping what expired without anyone redeeming it.
CREATE INDEX IF NOT EXISTS ix_authorization_codes_expiry
  ON authorization_codes (expires_at);

-- The client assertions already spent, so none authenticates twice.

-- A client assertion carries a jti its client chose, and SMART requires a server
-- to refuse a second use of one. The row is kept until the assertion it names
-- would have expired anyway: after that the expiry refuses it, and remembering
-- it longer would be remembering something nothing can present.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS client_assertion_jtis (
  project_id            TEXT NOT NULL,
  client_application_id TEXT NOT NULL,
  jti                   TEXT NOT NULL,
  expires_at            BIGINT NOT NULL,

  -- The key is what makes a replay a conflict rather than a second row. It is
  -- per client, because a jti is unique within the client that chose it and two
  -- clients picking the same string are not replaying each other.
  PRIMARY KEY (project_id, client_application_id, jti),

  CHECK (jti <> ''),

  FOREIGN KEY (project_id, client_application_id)
    REFERENCES client_applications (project_id, id) ON DELETE CASCADE ON UPDATE RESTRICT
);

-- For sweeping what expired.
CREATE INDEX IF NOT EXISTS ix_client_assertion_jtis_expiry
  ON client_assertion_jtis (expires_at);

-- The keys this install signs identity tokens with. Install-scoped rather than
-- per Project, because the issuer a token names is this server: one key set is
-- published at one jwks_uri, and a reader that had to know which Project a
-- token came from before it could find the key would be a reader SMART does not
-- describe.
--
-- The private half is sealed, not hashed. A password can be hashed because the
-- server only has to recognise it; this one has to be used, so whatever holds
-- it can sign an identity for anybody.

-- tenant: none, the keys this install signs identity tokens with
CREATE TABLE IF NOT EXISTS identity_signing_keys (
  -- The kid a token names and a reader resolves against the published set.
  id          TEXT NOT NULL,

  private_key TEXT NOT NULL,
  state       TEXT NOT NULL CHECK (state IN ('active', 'retired')),
  created_at  BIGINT NOT NULL,
  retired_at  BIGINT,

  PRIMARY KEY (id),

  CHECK (id <> ''),
  CHECK (private_key <> ''),

  -- A retired key carries when, and an active one cannot: the pair is what says
  -- whether a key is still signing or only still verifying.
  CHECK ((state = 'retired') = (retired_at IS NOT NULL))
);

-- One key signs at a time. A second active key would mean two answers to which
-- one a token came from, and the partial index makes that unrepresentable
-- rather than merely avoided.
CREATE UNIQUE INDEX IF NOT EXISTS ux_identity_signing_keys_active
  ON identity_signing_keys (state) WHERE state = 'active';

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
-- Super jobs. The install-wide work this server does to itself.
-- ===========================================================================

-- A migration, a seed or a backfill belongs to no Project: it is done to the
-- install. Each one decides for itself whether there is anything to do — by
-- looking at the database, or by comparing a fingerprint of what it would
-- apply — so running one twice does nothing the second time. What this table
-- adds is the record, because an operator asking what a database has been
-- through should not have to infer it from the shape of the tables.
--
-- A row is written when a job actually did something, or when one failed. A
-- start that changed nothing writes nothing, because a server starts far more
-- often than its schema changes and a row per start per job would bury the ones
-- that matter.

-- tenant: none, one row per run of one install-wide job
CREATE TABLE IF NOT EXISTS super_jobs (
  id          TEXT NOT NULL,
  name        TEXT NOT NULL,
  kind        TEXT NOT NULL CHECK (kind IN ('migration', 'seed', 'backfill')),
  subject     TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  outcome     TEXT NOT NULL CHECK (outcome IN ('applied', 'failed')),
  detail      TEXT NOT NULL DEFAULT '',
  started_at  BIGINT NOT NULL,
  finished_at BIGINT NOT NULL,

  PRIMARY KEY (id),

  CHECK (substr(id, 1, 4) = 'job_'),
  CHECK (name <> '' AND subject <> ''),
  CHECK (finished_at >= started_at)
);

-- What has been done to one table, newest first. This is the question the
-- record exists to answer.
CREATE INDEX IF NOT EXISTS ix_super_jobs_subject
  ON super_jobs (subject, started_at DESC, id);

-- And the same question asked of one job.
CREATE INDEX IF NOT EXISTS ix_super_jobs_name
  ON super_jobs (name, started_at DESC, id);

-- Nothing revises a job record, for the same reason nothing revises an audit
-- row: a record of what a server did to itself that the server can edit
-- afterwards is not a record.
CREATE TRIGGER IF NOT EXISTS super_jobs_no_update
BEFORE UPDATE ON super_jobs
BEGIN
  SELECT RAISE(ABORT, 'super_jobs rows are immutable');
END;

-- ===========================================================================
-- Canonical resources. The FHIR specification's own definitions, seeded from
-- what this build embeds.
-- ===========================================================================

-- They belong to no Project: they are the specification, identical in every one
-- of them, and a copy per Project would be the same bytes written as many times
-- as there are tenants. Nothing writes here over the API — a seed is the only
-- thing that does — so there is no version, no history and no tombstone.

-- tenant-exempt: the base specification, the same for every Project
CREATE TABLE IF NOT EXISTS canonical_resource (
  res_type TEXT NOT NULL,
  res_id   TEXT NOT NULL,
  url      TEXT NOT NULL,
  version  TEXT NOT NULL,
  content  TEXT NOT NULL,

  PRIMARY KEY (res_type, res_id),

  CHECK (res_type <> '' AND res_id <> ''),
  CHECK (url <> '' AND content <> '')
);

-- A canonical url is how a reference to a definition is resolved, which is the
-- one lookup that is not by id.
CREATE UNIQUE INDEX IF NOT EXISTS ux_canonical_resource_url
  ON canonical_resource (url, version);

-- What was seeded, so a start that would change nothing reads one row and stops
-- rather than parsing the whole specification to find that out.

-- tenant: none, one row describing what this install holds
CREATE TABLE IF NOT EXISTS canonical_seed (
  id        TEXT NOT NULL CHECK (id = 'canonical'),
  digest    TEXT NOT NULL,
  release   TEXT NOT NULL,
  held      BIGINT NOT NULL,
  seeded_at BIGINT NOT NULL,

  PRIMARY KEY (id),

  CHECK (digest <> '' AND release <> ''),
  CHECK (held >= 0)
);

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

  -- Exactly one of these names the principal. A person's session names a user;
  -- a backend service's names the registration whose key signed for it. Both are
  -- nullable because only one is ever set, and the CHECK below is what makes
  -- "neither" and "both" unrepresentable rather than merely unusual.
  user_id       TEXT REFERENCES users (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  client_application_id TEXT,

  membership_id TEXT NOT NULL,
  state         TEXT NOT NULL CHECK (state IN ('active', 'revoked')),

  -- What a SMART app's session was launched with, both NULL for an ordinary
  -- login. They live in this row rather than a table beside it so that a
  -- session an app holds cannot be read without reading what that app was
  -- granted: a launch context that went missing would read as a login nobody's
  -- app holds, and that one is narrowed by nothing.
  launch_patient TEXT,

  -- The granted scopes as the token response reported them, space-delimited.
  -- Stored verbatim so what the app was told it holds and what this server
  -- narrows by are the same text rather than two renderings of it.
  granted_scopes TEXT,

  -- The refresh chain this session was minted from, NULL when it was not minted
  -- from one. It is recorded so that detecting a replayed refresh token can
  -- revoke the access tokens that grant already produced: revoking only the
  -- chain would stop the next refresh while leaving whatever the replayer
  -- already obtained alive until it expired on its own.
  refresh_chain TEXT,

  created_at    BIGINT NOT NULL,
  expires_at    BIGINT NOT NULL,
  revoked_at    BIGINT,

  PRIMARY KEY (project_id, id),

  CHECK (substr(id, 1, 4) = 'ses_'),

  -- One principal, never none and never two. A session naming nobody authorizes
  -- nothing and would be served as whatever a nil principal reads as; one naming
  -- both is two principals sharing a token.
  CHECK ((user_id IS NULL) <> (client_application_id IS NULL)),

  -- A patient without scopes is a session that was launched and granted
  -- nothing. It would rebuild as an ordinary login while looking like an app's
  -- session to anyone reading the table, so it is unrepresentable.
  CHECK (launch_patient IS NULL OR granted_scopes IS NOT NULL),

  -- Absence is NULL. The empty string would be a second way to say it, and the
  -- two would not narrow alike.
  CHECK (granted_scopes IS NULL OR granted_scopes <> ''),

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
    REFERENCES project_memberships (project_id, id) ON DELETE RESTRICT ON UPDATE RESTRICT,

  FOREIGN KEY (project_id, client_application_id)
    REFERENCES client_applications (project_id, id) ON DELETE RESTRICT ON UPDATE RESTRICT
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

-- ===========================================================================
-- Audit. What happened, independently of the FHIR AuditEvent resource, so a
-- client cannot edit the record of its own actions.
-- ===========================================================================

-- project_id holds the literal 'system' for an event outside every Project, so
-- it carries no foreign key to projects — the same reason platform_resource
-- carries none. A login naming a slug that resolves to nothing still happened,
-- and the row must not record the caller's own spelling of that slug.

-- No foreign key names the principal or the membership either. An audit row
-- outlives the standing it records: a key would either block withdrawing a
-- membership or erase the evidence that it once acted.

-- detail is drawn from a closed vocabulary rather than being free text, so
-- nothing a caller supplied can reach the record. A row that quoted a request
-- would hold the very content this table is kept clear of, and one that quoted
-- a login would hold the password (AUD-2).

-- tenant: project_id
CREATE TABLE IF NOT EXISTS audit_events (
  project_id     TEXT NOT NULL,
  id             TEXT NOT NULL,
  at             BIGINT NOT NULL,
  principal_kind TEXT NOT NULL,
  principal_id   TEXT NOT NULL,
  membership_id  TEXT,
  action         TEXT NOT NULL CHECK (action IN (
    'read', 'write', 'delete', 'search', 'history', 'authenticate'
  )),
  res_type       TEXT,
  res_id         TEXT,
  outcome        TEXT NOT NULL CHECK (outcome IN ('allowed', 'refused', 'failed')),
  detail         TEXT NOT NULL DEFAULT '' CHECK (detail IN (
    '', 'not-authorized', 'not-found', 'deleted', 'version-conflict',
    'already-exists', 'malformed', 'unidentified', 'throttled', 'unavailable'
  )),

  PRIMARY KEY (project_id, id),

  CHECK (project_id <> ''),
  CHECK (substr(id, 1, 4) = 'aud_'),
  CHECK (principal_kind <> '' AND principal_id <> ''),
  CHECK (membership_id IS NULL OR membership_id <> ''),

  -- An id belongs to a type. A search names a type and no id, which is what it
  -- acted on; an id with no type names nothing at all.
  --
  -- The NULL tests are explicit because "res_id <> ''" is NULL when res_id is,
  -- and a CHECK passes on NULL: the emptiness tests alone would admit a row
  -- this one is written to refuse.
  CHECK (res_type IS NULL OR res_type <> ''),
  CHECK (res_id IS NULL OR (res_type IS NOT NULL AND res_id <> ''))
);

-- An incident reads one Project's events in the order they happened.
CREATE INDEX IF NOT EXISTS ix_audit_events_at ON audit_events (project_id, at, id);

-- Nothing revises an audit row. A record whoever acted can edit afterwards is
-- not a record (AUD-4).
CREATE TRIGGER IF NOT EXISTS audit_events_no_update
BEFORE UPDATE ON audit_events
BEGIN
  SELECT RAISE(ABORT, 'audit_events is append-only');
END;

-- Deletion is confined to the purge worker, which only runs against a Project
-- the database itself reports as deleting. This is the fhir_resource_history
-- rule, for the same reason: erasing a Project has to be possible, and erasing
-- what it did while it existed has to not be.
CREATE TRIGGER IF NOT EXISTS audit_events_no_delete
BEFORE DELETE ON audit_events
WHEN (SELECT state FROM projects WHERE id = OLD.project_id) IS NOT 'deleting'
BEGIN
  SELECT RAISE(ABORT, 'audit_events is append-only');
END;

-- ===========================================================================
-- Search index. Derived from a resource's own content when it is written, in
-- the transaction that writes it, exactly as its compartments are. A predicate
-- with no writer is decorative, so the projection and the reader land together.
-- ===========================================================================

-- ===========================================================================
-- Reindex backlog. Types whose index no longer matches the parameters.
-- ===========================================================================

-- A parameter defined today says nothing about resources written yesterday: the
-- index is built on write, so everything already stored is invisible to a new
-- parameter until it is walked again.
--
-- One row per Project and type rather than per resource. Defining three
-- parameters on Organization is one reindex of Organization, and a row that is
-- already there is left alone — the work is "make this type's index match the
-- parameters", which is the same work however many times it is asked for.
--
-- Claimed the way the subscription backlog is claimed, so several replicas
-- share the work without two of them walking the same type at once.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS reindex_backlog (
  project_id    TEXT NOT NULL REFERENCES projects (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  res_type      TEXT NOT NULL,
  at            BIGINT NOT NULL,
  claimed_by    TEXT,
  claimed_until BIGINT,

  PRIMARY KEY (project_id, res_type),

  CHECK (res_type <> '')
);

-- ===========================================================================
-- Custom search parameters. What a Project added to the built-in registry.
-- ===========================================================================

-- A SearchParameter resource a Project stored, compiled into the shape the
-- index and the query compiler both read. It is a projection of that resource,
-- maintained in the transaction that writes it, exactly as fhir_search_index is
-- a projection of the resource it indexes: a definition kept anywhere else is
-- one a write can skip.
--
-- Per Project, because a parameter one tenant defined must not change what
-- another tenant's query means. The built-in registry is the floor every
-- Project stands on and none of them can move: a custom code that shadowed one
-- is refused when it is compiled, not here.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS search_parameter (
  project_id    TEXT NOT NULL REFERENCES projects (id) ON DELETE RESTRICT ON UPDATE RESTRICT,
  res_type      TEXT NOT NULL,
  code          TEXT NOT NULL,
  kind          TEXT NOT NULL CHECK (kind IN ('token', 'string', 'reference', 'date')),

  -- The dotted element path a write projects from, with the type stripped.
  path          TEXT NOT NULL,

  -- A token's two halves, within the element the path names. Empty when the
  -- element is itself the code.
  code_member   TEXT NOT NULL DEFAULT '',
  system_member TEXT NOT NULL DEFAULT '',

  -- The SearchParameter resource this was compiled from, so removing that
  -- resource removes what it defined and nothing else.
  source_id     TEXT NOT NULL,

  defined_at    BIGINT NOT NULL,

  PRIMARY KEY (project_id, res_type, code),

  CHECK (res_type <> '' AND code <> '' AND path <> '' AND source_id <> '')
);

-- Removing one source's definitions, which is what a delete or a replacement
-- does before writing what the resource now says.
CREATE INDEX IF NOT EXISTS ix_search_parameter_source
  ON search_parameter (project_id, source_id);

-- One row per indexed value. A resource carrying three categories has three
-- token rows for that parameter, and a search naming any of them finds it.

-- The columns a kind does not use are NULL, and the CHECK below makes a
-- half-formed row unrepresentable rather than leaving each reader to decide
-- what a token with a date bound means.

-- Nothing here is version-dimensioned. Search answers over current state, and a
-- tombstone is excluded by the fhir_resource row it joins rather than by
-- clearing its index, so a delete stays one statement.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS fhir_search_index (
  project_id TEXT NOT NULL,
  res_type   TEXT NOT NULL,
  res_id     TEXT NOT NULL,
  param      TEXT NOT NULL,
  kind       TEXT NOT NULL CHECK (kind IN ('token', 'string', 'reference', 'date')),

  -- token: the code. reference: the "Type/id" it names.
  code       TEXT,

  -- token: the system the code was recorded under, NULL when it carried none.
  system     TEXT,

  -- string: the value case-folded, so a prefix match need not guess how it was
  -- capitalised.
  folded     TEXT,

  -- date: the span the value names, inclusive at both ends.
  lower      BIGINT,
  upper      BIGINT,

  CHECK (project_id <> '' AND res_type <> '' AND res_id <> '' AND param <> ''),

  -- Each kind fills its own columns and no others.
  CHECK (
    (kind IN ('token', 'reference')
      AND code IS NOT NULL AND code <> ''
      AND folded IS NULL AND lower IS NULL AND upper IS NULL)
    OR (kind = 'string'
      AND folded IS NOT NULL AND folded <> ''
      AND code IS NOT NULL AND code <> ''
      AND system IS NULL AND lower IS NULL AND upper IS NULL)
    OR (kind = 'date'
      AND lower IS NOT NULL AND upper IS NOT NULL AND upper >= lower
      AND code IS NULL AND system IS NULL AND folded IS NULL)
  ),

  -- Only a token is qualified by a system.
  CHECK (system IS NULL OR kind = 'token'),

  FOREIGN KEY (project_id, res_type, res_id)
    REFERENCES fhir_resource (project_id, res_type, res_id) ON DELETE CASCADE ON UPDATE RESTRICT
);

-- The lookup a token or a reference compiles to: one parameter's values within
-- one Project and type.
CREATE INDEX IF NOT EXISTS ix_fhir_search_index_code
  ON fhir_search_index (project_id, res_type, param, code);

-- The prefix scan a string compiles to.
CREATE INDEX IF NOT EXISTS ix_fhir_search_index_folded
  ON fhir_search_index (project_id, res_type, param, folded);

-- The range scan a date compiles to.
CREATE INDEX IF NOT EXISTS ix_fhir_search_index_span
  ON fhir_search_index (project_id, res_type, param, lower, upper);

-- What a rewrite clears before it projects again.
CREATE INDEX IF NOT EXISTS ix_fhir_search_index_resource
  ON fhir_search_index (project_id, res_type, res_id);

-- ===========================================================================
-- Login throttle. Counted across the install rather than within one process,
-- because a deployment running three replicas would otherwise allow three
-- times the guesses the limit states.
-- ===========================================================================

-- These rows sit outside every Project. A login names a Project by slug, and a
-- slug resolving to nothing is still an attempt worth counting, so there is no
-- tenant column to put it under; the key carries the Project it was derived
-- from.

-- key is a digest, never the identity or the address behind it. A table of who
-- tried to log in and failed is a list of this install's users and where they
-- were, kept somewhere nobody thinks to look.
CREATE TABLE IF NOT EXISTS login_attempts (
  key TEXT NOT NULL,
  at  BIGINT NOT NULL,

  -- A SHA-256 digest in hex, which is the only thing that reaches this column.
  CHECK (length(key) = 64)
);

-- The count one attempt asks for: one key's failures inside the window.
CREATE INDEX IF NOT EXISTS ix_login_attempts_key ON login_attempts (key, at);

-- ===========================================================================
-- Second factors. One per identity, because a person proves they are
-- themselves once rather than choosing which of several ways to.
-- ===========================================================================

-- The secret is sealed, not hashed. A password can be hashed because the server
-- only ever has to recognise it; this one the server has to compute with, so
-- whatever holds it holds the factor. Sealing means a database read on its own
-- — a leaked backup, a replica, a stolen file — does not hand over anyone's
-- second factor, because the key lives in the deployment's environment.

-- last_step is the counter of the last code accepted. A code at or before it is
-- a replay: without this a code watched over someone's shoulder is good for the
-- rest of its thirty seconds.

-- No tenant column. A user is an identity in the install, and the realm its
-- membership resolves in is the users row's own business.
CREATE TABLE IF NOT EXISTS user_second_factors (
  user_id        TEXT NOT NULL PRIMARY KEY
                 REFERENCES users (id) ON DELETE CASCADE ON UPDATE RESTRICT,
  state          TEXT NOT NULL CHECK (state IN ('pending', 'active')),
  sealed_secret  TEXT NOT NULL,

  -- A replacement awaiting proof. The secret above stays in force until a code
  -- from the new phone arrives, so moving to one never leaves a window in
  -- which the account has no second factor at all.
  pending_secret TEXT,

  last_step      BIGINT NOT NULL DEFAULT 0,
  created_at     BIGINT NOT NULL,
  activated_at   BIGINT,

  CHECK (sealed_secret <> ''),
  CHECK (last_step >= 0),

  -- A factor is active only once a code proved the person holds it. Enrolling
  -- one and never proving it must not lock anyone out of their own account.
  CHECK (state <> 'active' OR activated_at IS NOT NULL),
  CHECK (state = 'active' OR activated_at IS NULL),

  -- Only a factor in force can be being replaced. A replacement beside a
  -- pending one would be a second unproved secret, and nothing could say which
  -- of them a code was meant to prove.
  CHECK (pending_secret IS NULL OR (state = 'active' AND pending_secret <> ''))
);

-- ===========================================================================
-- Subscriptions. What a write owes a subscriber, kept as rows so a restart
-- between the write and the notification loses neither.
-- ===========================================================================

-- Who a Subscription delivers as.
--
-- A notification carries what a resource says, so what a subscriber may be told
-- is what the standing that created the subscription may read — never what the
-- subscription asked for. The principal is recorded here because that is what a
-- Scope is built from, and it is recorded when the Subscription is created
-- because a client must not be able to state it.
--
-- The key cascades: standing withdrawn is a subscription that stops delivering,
-- rather than one that goes on delivering as somebody who is no longer there.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS subscription_owners (
  project_id      TEXT NOT NULL,
  subscription_id TEXT NOT NULL,
  membership_id   TEXT NOT NULL,
  principal_kind  TEXT NOT NULL,
  principal_id    TEXT NOT NULL,
  created_at      BIGINT NOT NULL,

  PRIMARY KEY (project_id, subscription_id),

  CHECK (subscription_id <> '' AND principal_kind <> '' AND principal_id <> ''),

  FOREIGN KEY (project_id, membership_id)
    REFERENCES project_memberships (project_id, id) ON DELETE CASCADE ON UPDATE RESTRICT
);

-- What was written, before anybody has worked out who should hear about it.
--
-- A write records one row here and nothing more. Matching every subscription
-- inside the write would make every write cost as much as the subscription list
-- is long, and would hold the transaction open while it did.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS subscription_backlog (
  project_id TEXT NOT NULL,
  id         TEXT NOT NULL,
  res_type   TEXT NOT NULL,
  res_id     TEXT NOT NULL,
  version_id TEXT NOT NULL,
  at         BIGINT NOT NULL,

  -- Which worker is working through this row, and until when. Several replicas
  -- read the same backlog, so without a claim each one would fan the same write
  -- out again. The lease is what makes a worker that died give its rows back.
  claimed_by    TEXT,
  claimed_until BIGINT,

  PRIMARY KEY (project_id, id),

  CHECK (substr(id, 1, 4) = 'wrt_'),
  CHECK (res_type <> '' AND res_id <> '' AND version_id <> ''),

  -- A claim is a worker and a lease together. Half of one names a holder that
  -- never expires, or an expiry belonging to nobody.
  CHECK ((claimed_by IS NULL AND claimed_until IS NULL)
      OR (claimed_by IS NOT NULL AND claimed_until IS NOT NULL AND claimed_by <> ''))
);

-- The order a backlog is worked through: oldest first, so a subscriber is told
-- about writes in the order they happened.
CREATE INDEX IF NOT EXISTS ix_subscription_backlog_at ON subscription_backlog (at, id);

-- One thing one subscriber is owed.
--
-- It carries the resource's key and not its content. The resource is read at
-- delivery time under the owner's own Scope, so access withdrawn between the
-- write and the notification is access the notification does not have: a queue
-- holding the body would deliver what the subscriber could no longer read.

-- tenant: project_id
CREATE TABLE IF NOT EXISTS subscription_deliveries (
  project_id      TEXT NOT NULL,
  id              TEXT NOT NULL,
  subscription_id TEXT NOT NULL,
  res_type        TEXT NOT NULL,
  res_id          TEXT NOT NULL,
  version_id      TEXT NOT NULL,
  state           TEXT NOT NULL CHECK (state IN ('pending', 'delivered', 'abandoned')),
  attempts        BIGINT NOT NULL DEFAULT 0,
  due_at          BIGINT NOT NULL,
  created_at      BIGINT NOT NULL,
  settled_at      BIGINT,

  -- Which worker is making this delivery, and until when. See the backlog: the
  -- consequence here is a subscriber posted to twice for one write.
  claimed_by      TEXT,
  claimed_until   BIGINT,

  PRIMARY KEY (project_id, id),

  CHECK (substr(id, 1, 4) = 'dlv_'),
  CHECK ((claimed_by IS NULL AND claimed_until IS NULL)
      OR (claimed_by IS NOT NULL AND claimed_until IS NOT NULL AND claimed_by <> '')),
  CHECK (res_type <> '' AND res_id <> '' AND version_id <> ''),
  CHECK (attempts >= 0),

  -- A settled delivery says when, and a pending one has not settled.
  CHECK (state = 'pending' OR settled_at IS NOT NULL),
  CHECK (state <> 'pending' OR settled_at IS NULL),

  FOREIGN KEY (project_id, subscription_id)
    REFERENCES subscription_owners (project_id, subscription_id)
    ON DELETE CASCADE ON UPDATE RESTRICT
);

-- What a worker asks for: the pending deliveries that are due.
CREATE INDEX IF NOT EXISTS ix_subscription_deliveries_due
  ON subscription_deliveries (state, due_at, id);
