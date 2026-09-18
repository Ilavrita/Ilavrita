-- The notification queues as they stood before a worker could claim a row.
-- Every replica read the same rows, so every replica fanned out the same write
-- and posted the same notification.
CREATE TABLE IF NOT EXISTS subscription_backlog (
  project_id TEXT NOT NULL,
  id         TEXT NOT NULL,
  res_type   TEXT NOT NULL,
  res_id     TEXT NOT NULL,
  version_id TEXT NOT NULL,
  at         BIGINT NOT NULL,

  PRIMARY KEY (project_id, id),

  CHECK (substr(id, 1, 4) = 'wrt_'),
  CHECK (res_type <> '' AND res_id <> '' AND version_id <> '')
);

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

  PRIMARY KEY (project_id, id),

  CHECK (substr(id, 1, 4) = 'dlv_'),
  CHECK (res_type <> '' AND res_id <> '' AND version_id <> ''),
  CHECK (attempts >= 0),

  CHECK (state = 'pending' OR settled_at IS NOT NULL),
  CHECK (state <> 'pending' OR settled_at IS NULL),

  FOREIGN KEY (project_id, subscription_id)
    REFERENCES subscription_owners (project_id, subscription_id)
    ON DELETE CASCADE
);

-- The indexes the earlier release created. They are named and shaped the same
-- as the current ones, which is what lets the schema be applied over this table
-- before the rebuild replaces it.
CREATE INDEX IF NOT EXISTS ix_subscription_backlog_at ON subscription_backlog (at, id);

CREATE INDEX IF NOT EXISTS ix_subscription_deliveries_due
  ON subscription_deliveries (state, due_at, id);
