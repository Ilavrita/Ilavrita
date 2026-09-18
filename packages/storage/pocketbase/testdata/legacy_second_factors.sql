-- user_second_factors as it stood before a factor could hold a replacement
-- awaiting proof.
--
-- A database created from this is what PrepareSchema has to bring forward: the
-- current declaration is applied with IF NOT EXISTS, so this table would keep
-- its shape, every read of a factor would name a column that is not there, and
-- every login by somebody holding one would fail.
CREATE TABLE IF NOT EXISTS user_second_factors (
  user_id       TEXT NOT NULL PRIMARY KEY
                REFERENCES users (id) ON DELETE CASCADE ON UPDATE RESTRICT,
  state         TEXT NOT NULL CHECK (state IN ('pending', 'active')),
  sealed_secret TEXT NOT NULL,
  last_step     BIGINT NOT NULL DEFAULT 0,
  created_at    BIGINT NOT NULL,
  activated_at  BIGINT,

  CHECK (sealed_secret <> ''),
  CHECK (last_step >= 0),
  CHECK (state <> 'active' OR activated_at IS NOT NULL),
  CHECK (state = 'active' OR activated_at IS NULL)
);
