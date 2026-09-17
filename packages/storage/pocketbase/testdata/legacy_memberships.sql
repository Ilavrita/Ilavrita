-- The project_memberships table exactly as it stood at ecb9a05, before the
-- client application and bot registries existed. It is the shape every
-- database created before them still carries, and what the rebuild adopts
-- the principal foreign keys onto.

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
