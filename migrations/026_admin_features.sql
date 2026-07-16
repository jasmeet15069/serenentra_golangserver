-- Backends for three previously-fake features: System Admin API keys +
-- integrations, and Revenue dynamic-pricing adjustments. No semicolons in comments.

-- Per-tenant API keys. The full key is shown once on create. Only a bcrypt hash and
-- a display prefix are stored.
CREATE TABLE IF NOT EXISTS api_keys (
  id UUID PRIMARY KEY,
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  key_prefix TEXT NOT NULL,
  key_hash TEXT NOT NULL,
  is_active BOOLEAN NOT NULL DEFAULT true,
  last_used_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_api_keys_hotel ON api_keys (hotel_id, created_at DESC);

-- Per-tenant third-party integrations (enable/disable + config + last check).
CREATE TABLE IF NOT EXISTS integrations (
  id UUID PRIMARY KEY,
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  category TEXT,
  is_enabled BOOLEAN NOT NULL DEFAULT false,
  config JSONB NOT NULL DEFAULT '{}'::jsonb,
  status TEXT NOT NULL DEFAULT 'disconnected',
  last_checked_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (hotel_id, provider)
);

-- Revenue rate adjustments applied from the Revenue Management screen (audit trail).
CREATE TABLE IF NOT EXISTS rate_adjustments (
  id UUID PRIMARY KEY,
  hotel_id UUID NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
  percent NUMERIC(6,2) NOT NULL,
  room_type TEXT,
  rooms_affected INT NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
