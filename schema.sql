CREATE TABLE IF NOT EXISTS score_events (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  season_id  TEXT NOT NULL,
  user_id    TEXT NOT NULL,
  delta      BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_score_events_season_created
  ON score_events (season_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_score_events_season_user_created
  ON score_events (season_id, user_id, created_at DESC);

CREATE TABLE IF NOT EXISTS outbox (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  event_type   TEXT NOT NULL,
  payload      JSONB NOT NULL,
  status       TEXT NOT NULL DEFAULT 'pending', -- pending/processing/done/failed
  attempts     INT NOT NULL DEFAULT 0,
  last_error   TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  processed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_outbox_pending
  ON outbox (status, id);