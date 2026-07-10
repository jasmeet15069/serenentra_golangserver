-- Demo request leads captured from the Serenentra landing page.
CREATE TABLE IF NOT EXISTS demo_requests (
  id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  name         TEXT        NOT NULL,
  email        TEXT        NOT NULL,
  phone        TEXT,
  property_name TEXT       NOT NULL,
  rooms        TEXT        NOT NULL,
  country      TEXT,
  message      TEXT,
  status       TEXT        NOT NULL DEFAULT 'new', -- new | contacted | converted | closed
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS demo_requests_status_idx    ON demo_requests (status);
CREATE INDEX IF NOT EXISTS demo_requests_created_at_idx ON demo_requests (created_at DESC);
