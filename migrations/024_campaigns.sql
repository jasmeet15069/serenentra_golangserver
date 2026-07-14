-- Marketing campaigns for CRM. Previously the CRM "campaigns" tab was a hardcoded
-- demo with fake "launched"/"created" toasts and no persistence. This gives it a
-- real table so campaigns can be created, listed, launched (status change), and
-- deleted per tenant. Metrics (sent/opens/clicks/revenue) default to 0 and are
-- placeholders until a real send integration populates them.
CREATE TABLE IF NOT EXISTS campaigns (
  id UUID PRIMARY KEY,
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  audience TEXT,
  channel TEXT NOT NULL DEFAULT 'email',
  status TEXT NOT NULL DEFAULT 'draft',
  sent INTEGER NOT NULL DEFAULT 0,
  opens INTEGER NOT NULL DEFAULT 0,
  clicks INTEGER NOT NULL DEFAULT 0,
  revenue NUMERIC(12,2) NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_campaigns_hotel ON campaigns (hotel_id, created_at DESC);
